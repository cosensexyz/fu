package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"golang.org/x/sys/unix"
)

// rootFilesystem adapts session-pinned logical roots to go-billy. Each
// instance retains its primary descriptor and optional mounted descriptors;
// Chroot only narrows the virtual relative base.
type rootFilesystem struct {
	checked *checkedRoot
	base    string
	mounts  map[string]*checkedRoot
}

func newRootFilesystem(root *checkedRoot, base string) (*rootFilesystem, error) {
	fsys := &rootFilesystem{checked: root}
	clean, err := fsys.clean(base)
	if err != nil {
		return nil, err
	}
	fsys.base = clean
	return fsys, nil
}

// Mount replaces one virtual subtree with a separately pinned logical root.
// The worktree uses this for skills, so a pathname replacement beneath the
// pinned store cannot change what go-git reads or stages.
func (f *rootFilesystem) Mount(name string, root *checkedRoot) error {
	name, err := f.clean(name)
	if err != nil {
		return err
	}
	if name == "." || root == nil {
		return fmt.Errorf("invalid logical-root mount %q", name)
	}
	if f.mounts == nil {
		f.mounts = make(map[string]*checkedRoot)
	}
	f.mounts[name] = root
	return nil
}

func (f *rootFilesystem) clean(name string) (string, error) {
	name = filepath.Clean(name)
	if name == "." || name == string(filepath.Separator) {
		return ".", nil
	}
	if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return "", billy.ErrCrossedBoundary
	}
	return name, nil
}

func (f *rootFilesystem) path(name string) (string, error) {
	name, err := f.clean(name)
	if err != nil {
		return "", err
	}
	if f.base == "." {
		return name, nil
	}
	if name == "." {
		return f.base, nil
	}
	return f.clean(filepath.Join(f.base, name))
}

func (f *rootFilesystem) resolve(name string) (*checkedRoot, string, string, error) {
	virtual, err := f.path(name)
	if err != nil {
		return nil, "", "", err
	}
	selectedRoot := f.checked
	selectedPath := virtual
	best := ""
	for mount, root := range f.mounts {
		if virtual != mount && !strings.HasPrefix(virtual, mount+string(filepath.Separator)) {
			continue
		}
		if len(mount) <= len(best) {
			continue
		}
		best = mount
		selectedRoot = root
		selectedPath = strings.TrimPrefix(virtual, mount)
		selectedPath = strings.TrimPrefix(selectedPath, string(filepath.Separator))
		if selectedPath == "" {
			selectedPath = "."
		}
	}
	return selectedRoot, selectedPath, virtual, nil
}

func (f *rootFilesystem) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

func (f *rootFilesystem) Open(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

func (f *rootFilesystem) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	root, p, _, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	if readOnlyRootOpen(flag) {
		// readOnlyRootOpen already excludes O_CREATE, so a read never has a
		// directory to create on the way.
		return openReadOnlyRootFile(root, p, name, flag, perm)
	}
	return openWritableRootFile(root, p, name, flag, perm)
}

// openRootDirNoFollow walks to the directory named by dir one component at a
// time from the pinned root descriptor, refusing a symlink at every step and
// creating missing components when the caller is about to create a file.
//
// os.Root is not usable for this. It keeps resolution inside the root but
// follows contained links on the way, so a single `refs/heads -> tags` link
// silently retargets every write beneath it -- containment says the write
// stayed in .git, not that it reached the file it named. MkdirAll has the same
// property, which is why directory creation happens here rather than through
// os.Root before the walk.
func openRootDirNoFollow(root *checkedRoot, dir string, create bool) (*os.File, error) {
	return openRootDirNoFollowPerm(root, dir, create, 0o755)
}

func openRootDirNoFollowPerm(root *checkedRoot, dir string, create bool, perm os.FileMode) (*os.File, error) {
	if root == nil || root.dir == nil {
		return nil, fmt.Errorf("open directory %s: checked root is unavailable", dir)
	}
	current, err := reopenDirNoFollow(int(root.dir.Fd()), ".", root.display, false, perm)
	if err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	if dir == "." || dir == string(filepath.Separator) {
		return current, nil
	}
	for component := range strings.SplitSeq(dir, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			_ = current.Close()
			return nil, billy.ErrCrossedBoundary
		}
		next, err := reopenDirNoFollow(int(current.Fd()), component, component, create, perm)
		closeErr := current.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			_ = next.Close()
			return nil, closeErr
		}
		current = next
	}
	return current, nil
}

func reopenDirNoFollow(parentFD int, name, display string, create bool, perm os.FileMode) (*os.File, error) {
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(parentFD, name, flags, 0)
	if err != nil && create && errors.Is(err, unix.ENOENT) {
		if mkErr := unix.Mkdirat(parentFD, name, uint32(perm.Perm())); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
			return nil, &os.PathError{Op: "mkdirat", Path: display, Err: mkErr}
		}
		fd, err = unix.Openat(parentFD, name, flags, 0)
	}
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: err}
	}
	file := os.NewFile(uintptr(fd), display)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open directory %s: invalid file descriptor", display)
	}
	return file, nil
}

// openCheckedParent walks to the directory holding path's final component, so
// the caller can address that component relative to a descriptor instead of by
// a name something else may reinterpret between calls.
func openCheckedParent(root *checkedRoot, path string, create bool) (*os.File, string, error) {
	dir, err := openRootDirNoFollow(root, filepath.Dir(path), create)
	if err != nil {
		return nil, "", err
	}
	return dir, filepath.Base(path), nil
}

// statEntryNoFollow describes one directory entry without following it and
// without blocking on it: classified by fstatat first, then reopened with
// O_NONBLOCK|O_NOFOLLOW and rechecked, so a type change between the two is
// reported rather than acted on. Symlinks are described as themselves; FIFOs,
// sockets and devices are refused (DESIGN §6).
func statEntryNoFollow(parentFD int, name, display string) (os.FileInfo, error) {
	observed, err := statAt(parentFD, name)
	if err != nil {
		return nil, &os.PathError{Op: "fstatat", Path: display, Err: err}
	}
	_, observedKind, err := modeAndKind(&observed)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", display, err)
	}
	if observedKind == ownedSymlink {
		// st_size on a symlink is the target length, already in hand from the
		// fstatat above -- no second syscall and no chance of describing a
		// different link than the one just classified.
		return symlinkFileInfo{
			name:    filepath.Base(display),
			size:    observed.Size,
			modTime: time.Unix(observed.Mtim.Sec, observed.Mtim.Nsec),
		}, nil
	}
	if observedKind == ownedFile {
		// Described from the stat already in hand. Opening a regular file here
		// bought nothing -- the FIFO/socket/device refusal this design exists
		// for happened at the fstatat above, and the guarantee is re-established
		// on the descriptor that actually reads the bytes, in
		// openReadOnlyRootFile and hashFileAt. What it did buy was EACCES: one
		// unreadable file aborted the whole walk, and with it IsDirty, Sweep and
		// every write command, on a worktree plain `git status` handles.
		return regularFileInfo{
			name:    filepath.Base(display),
			size:    observed.Size,
			mode:    os.FileMode(observed.Mode & 0o777),
			modTime: time.Unix(observed.Mtim.Sec, observed.Mtim.Nsec),
			sys:     observed,
		}, nil
	}
	flags := unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_DIRECTORY
	fd, err := unix.Openat(parentFD, name, flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: err}
	}
	file := os.NewFile(uintptr(fd), filepath.Base(display))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inspect %s: invalid file descriptor", display)
	}
	var opened unix.Stat_t
	statErr := unix.Fstat(fd, &opened)
	if statErr == nil {
		_, openedKind, kindErr := modeAndKind(&opened)
		switch {
		case kindErr != nil:
			statErr = fmt.Errorf("inspect opened %s: %w", display, kindErr)
		case openedKind != observedKind:
			statErr = fmt.Errorf("%s changed type from %s to %s while being inspected", display, observedKind, openedKind)
		}
	}
	info, infoErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return nil, statErr
	}
	if infoErr != nil {
		return nil, infoErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return info, nil
}

func lstatCheckedPath(root *checkedRoot, path string) (os.FileInfo, error) {
	dir, base, err := openCheckedParent(root, path, false)
	if err != nil {
		return nil, err
	}
	info, statErr := statEntryNoFollow(int(dir.Fd()), base, filepath.Join(root.display, path))
	closeErr := dir.Close()
	if statErr != nil {
		return nil, statErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return info, nil
}

func readOnlyRootOpen(flag int) bool {
	const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_EXCL | os.O_TRUNC
	return flag&writeFlags == 0
}

// openWritableRootFile opens a control file for writing without following a
// link at any component, and without ever addressing something that is not a
// regular file.
//
// os.Root cannot supply either property. It contains the resolution but does
// not stop at a link: it opens the final component with O_NOFOLLOW, and on
// ELOOP reads the link and keeps resolving as long as the target stays inside
// the root -- passing O_NOFOLLOW in from here does not change that. Containment
// says the write stayed in .git; it does not say the write reached the file it
// named. go-git rewrites .git/index through Create and a loose reference
// through OpenFile(O_RDWR|O_CREATE), so a direct Git writer racing fu could
// drop an in-root relative link at either name -- or one component above it --
// after fu read and validated it, and the write would land on a different file
// inside .git, corrupted while the operation reported success.
//
// The type check is the other half. A FIFO left at one of these names is worse
// than a wrong write: go-git's next read of it blocks forever, and it blocks
// holding fu.lock, so every later fu process queues behind it. O_NONBLOCK makes
// the open itself safe, and O_TRUNC is withheld until the descriptor is proven
// regular so a special file is never emptied on the way to being rejected.
func openWritableRootFile(root *checkedRoot, path, display string, flag int, perm os.FileMode) (billy.File, error) {
	base := filepath.Base(path)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("open %s for writing: %q does not name a file", display, path)
	}
	dir, err := openRootDirNoFollow(root, filepath.Dir(path), flag&os.O_CREATE != 0)
	if err != nil {
		return nil, err
	}
	openFlags := flag&^os.O_TRUNC | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, openErr := unix.Openat(int(dir.Fd()), base, openFlags, uint32(perm.Perm()))
	closeErr := dir.Close()
	if openErr != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: openErr}
	}
	if closeErr != nil {
		_ = unix.Close(fd)
		return nil, closeErr
	}
	// Checked on the raw descriptor, before os.NewFile hands a special file to
	// the runtime poller.
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: display, Err: err}
	}
	if err := requireRegularStat(display, &opened); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if flag&os.O_TRUNC != 0 {
		if err := unix.Ftruncate(fd, 0); err != nil {
			_ = unix.Close(fd)
			return nil, &os.PathError{Op: "ftruncate", Path: display, Err: err}
		}
	}
	file := os.NewFile(uintptr(fd), display)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open %s for writing: invalid file descriptor", display)
	}
	return &rootFile{File: file, name: display}, nil
}

// openReadOnlyRootFile reads the file the caller named, and not whatever a link
// on the way happens to point at.
//
// This used to go through os.Root, where the no-follow guarantee came from the
// identity comparison rather than from the O_NOFOLLOW flag -- os.Root resolves
// a contained final symlink regardless of that flag. But the comparison was
// made against an os.Root Lstat of the same path, so both sides followed the
// same intermediate links: `refs/heads -> tags` made the check agree that fu
// had opened exactly the file it asked for, while the bytes came from a
// different reference. Walking the parent descriptor-relative is what makes the
// logical path and the opened object the same thing; the identity comparison
// then does what it always claimed to, catching a replacement between the stat
// and the open. O_NONBLOCK is what keeps a FIFO at this name from blocking.
func openReadOnlyRootFile(root *checkedRoot, path, display string, flag int, perm os.FileMode) (billy.File, error) {
	dir, base, err := openCheckedParent(root, path, false)
	if err != nil {
		return nil, err
	}
	parentFD := int(dir.Fd())
	observed, statErr := statAt(parentFD, base)
	fd := -1
	var openErr error
	if statErr == nil {
		fd, openErr = unix.Openat(parentFD, base, flag|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	}
	closeErr := dir.Close()
	if statErr != nil {
		return nil, &os.PathError{Op: "fstatat", Path: display, Err: statErr}
	}
	if openErr != nil {
		return nil, &os.PathError{Op: "openat", Path: display, Err: openErr}
	}
	if closeErr != nil {
		_ = unix.Close(fd)
		return nil, closeErr
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: display, Err: err}
	}
	if err := requireRegularStat(display, &opened); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if identityFromStat(&observed) != identityFromStat(&opened) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: %q changed identity while go-git opened it", errRegularFileChanged, display)
	}
	file := os.NewFile(uintptr(fd), display)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open %s: invalid file descriptor", display)
	}
	stamp := stampRegularFile(&opened)
	return &rootFile{File: file, name: display, readStamp: &stamp}, nil
}

// maxSymlinkHops bounds Stat's resolution of a final link, matching the spirit
// of the kernel's own ELOOP limit.
const maxSymlinkHops = 8

// Stat follows a link at the final component -- that is what separates it from
// Lstat -- but resolves it one hop at a time back through resolve, so every hop
// is re-checked against the mount table and the root boundary, and no
// intermediate component is ever followed.
func (f *rootFilesystem) Stat(name string) (os.FileInfo, error) {
	current := name
	for range maxSymlinkHops {
		root, p, virtual, err := f.resolve(current)
		if err != nil {
			return nil, err
		}
		info, err := lstatCheckedPath(root, p)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if p == "." && virtual != "." {
				return mountedFileInfo{FileInfo: info, name: filepath.Base(virtual)}, nil
			}
			return info, nil
		}
		target, err := f.Readlink(current)
		if err != nil {
			return nil, err
		}
		if filepath.IsAbs(target) {
			return nil, billy.ErrCrossedBoundary
		}
		current = filepath.Join(filepath.Dir(current), target)
	}
	return nil, &os.PathError{Op: "stat", Path: name, Err: unix.ELOOP}
}

func (f *rootFilesystem) Rename(oldName, newName string) error {
	oldRoot, oldPath, _, err := f.resolve(oldName)
	if err != nil {
		return err
	}
	newRoot, newPath, _, err := f.resolve(newName)
	if err != nil {
		return err
	}
	if oldRoot != newRoot {
		return billy.ErrCrossedBoundary
	}
	oldDir, oldBase, err := openCheckedParent(oldRoot, oldPath, false)
	if err != nil {
		return err
	}
	newDir, newBase, err := openCheckedParent(newRoot, newPath, true)
	if err != nil {
		_ = oldDir.Close()
		return err
	}
	renameErr := unix.Renameat(int(oldDir.Fd()), oldBase, int(newDir.Fd()), newBase)
	closeErr := errors.Join(oldDir.Close(), newDir.Close())
	if renameErr != nil {
		return &os.LinkError{Op: "rename", Old: oldName, New: newName, Err: renameErr}
	}
	return closeErr
}

func (f *rootFilesystem) Remove(name string) error {
	root, p, _, err := f.resolve(name)
	if err != nil {
		return err
	}
	dir, base, err := openCheckedParent(root, p, false)
	if err != nil {
		return err
	}
	removeErr := unix.Unlinkat(int(dir.Fd()), base, 0)
	// Linux reports EISDIR here, Darwin EPERM; both mean "use AT_REMOVEDIR".
	if errors.Is(removeErr, unix.EISDIR) || errors.Is(removeErr, unix.EPERM) {
		removeErr = unix.Unlinkat(int(dir.Fd()), base, unix.AT_REMOVEDIR)
	}
	closeErr := dir.Close()
	if removeErr != nil {
		return &os.PathError{Op: "unlinkat", Path: name, Err: removeErr}
	}
	return closeErr
}

func (f *rootFilesystem) Join(elem ...string) string { return filepath.Join(elem...) }

// TempFile creates through the same no-follow walk as OpenFile: it is the
// other path that creates a file for writing, and a symlinked component
// retargets it identically.
func (f *rootFilesystem) TempFile(dir, prefix string) (billy.File, error) {
	root, rootDir, virtualDir, err := f.resolve(dir)
	if err != nil {
		return nil, err
	}
	parent, err := openRootDirNoFollow(root, rootDir, true)
	if err != nil {
		return nil, err
	}
	file, base, err := createTempAt(parent, prefix)
	closeErr := parent.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, closeErr
	}
	virtualName := filepath.Join(virtualDir, base)
	name, err := filepath.Rel(f.base, virtualName)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &rootFile{File: file, name: name}, nil
}

func createTempAt(parent *os.File, prefix string) (*os.File, string, error) {
	for range 100 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := prefix + hex.EncodeToString(random[:])
		fd, err := unix.Openat(int(parent.Fd()), name,
			unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return nil, "", &os.PathError{Op: "openat", Path: name, Err: err}
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			return nil, "", fmt.Errorf("create temporary file %s: invalid file descriptor", name)
		}
		return file, name, nil
	}
	return nil, "", fmt.Errorf("create temporary file in %s: exhausted unique names", parent.Name())
}

func createTempRoot(root *os.Root, dir, prefix string) (*os.File, string, error) {
	for range 100 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := filepath.Join(dir, prefix+hex.EncodeToString(random[:]))
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("create temporary file in %s: exhausted unique names", dir)
}

func openDirFresh(root *checkedRoot, name string) (*os.File, error) {
	if root == nil || root.dir == nil {
		return nil, fmt.Errorf("open directory %s: checked root is unavailable", name)
	}
	fd, err := unix.Openat(int(root.dir.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root.display, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open directory %s: invalid file descriptor", name)
	}
	return file, nil
}

func readDirInfosFresh(root *checkedRoot, name string) ([]os.FileInfo, error) {
	dir, err := openRootDirNoFollow(root, name, false)
	if err != nil {
		return nil, err
	}
	entries, readErr := dir.ReadDir(-1)
	if readErr != nil {
		_ = dir.Close()
		return nil, readErr
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		display := filepath.Join(root.display, name, entry.Name())
		info, err := statEntryNoFollow(int(dir.Fd()), entry.Name(), display)
		if err != nil {
			_ = dir.Close()
			return nil, err
		}
		infos = append(infos, info)
	}
	if err := dir.Close(); err != nil {
		return nil, err
	}
	return infos, nil
}

// regularFileInfo describes a regular file from the fstatat that classified it,
// so describing an entry never requires permission to read it.
type regularFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	sys     unix.Stat_t
}

func (i regularFileInfo) Name() string       { return i.name }
func (i regularFileInfo) Size() int64        { return i.size }
func (i regularFileInfo) Mode() os.FileMode  { return i.mode }
func (i regularFileInfo) ModTime() time.Time { return i.modTime }
func (regularFileInfo) IsDir() bool          { return false }
func (i regularFileInfo) Sys() any           { return &i.sys }

// symlinkFileInfo describes a link without opening it. size is the length of
// the target, which is what st_size means for a symlink and what go-git's
// worktree noder hashes with: NewHasher(BlobObject, size) followed by the target
// bytes. Reporting zero produced a hash over a length the content contradicts,
// so Status called the entry modified on every pass, IsDirty never went false,
// and Sweep could not converge.
type symlinkFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (i symlinkFileInfo) Name() string       { return i.name }
func (i symlinkFileInfo) Size() int64        { return i.size }
func (symlinkFileInfo) Mode() os.FileMode    { return os.ModeSymlink | 0o777 }
func (i symlinkFileInfo) ModTime() time.Time { return i.modTime }
func (symlinkFileInfo) IsDir() bool          { return false }
func (symlinkFileInfo) Sys() any             { return nil }

func (f *rootFilesystem) ReadDir(name string) ([]os.FileInfo, error) {
	root, p, virtual, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	infos, err := readDirInfosFresh(root, p)
	if err != nil {
		return nil, err
	}
	if root == f.checked {
		byName := make(map[string]int, len(infos))
		for i, info := range infos {
			byName[info.Name()] = i
		}
		for mount, mountedRoot := range f.mounts {
			if filepath.Dir(mount) != virtual {
				continue
			}
			info, err := mountedRoot.root.Stat(".")
			if err != nil {
				return nil, err
			}
			name := filepath.Base(mount)
			mounted := mountedFileInfo{FileInfo: info, name: name}
			if i, ok := byName[name]; ok {
				infos[i] = mounted
			} else {
				infos = append(infos, mounted)
			}
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
	return infos, nil
}

func (f *rootFilesystem) MkdirAll(name string, perm os.FileMode) error {
	root, p, _, err := f.resolve(name)
	if err != nil {
		return err
	}
	dir, err := openRootDirNoFollowPerm(root, p, true, perm)
	if err != nil {
		return err
	}
	return dir.Close()
}

func (f *rootFilesystem) Lstat(name string) (os.FileInfo, error) {
	root, p, virtual, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	info, err := lstatCheckedPath(root, p)
	if err != nil {
		return nil, err
	}
	if p == "." && virtual != "." {
		return mountedFileInfo{FileInfo: info, name: filepath.Base(virtual)}, nil
	}
	return info, nil
}

func (f *rootFilesystem) Symlink(target, link string) error {
	root, p, _, err := f.resolve(link)
	if err != nil {
		return err
	}
	dir, base, err := openCheckedParent(root, p, true)
	if err != nil {
		return err
	}
	symlinkErr := unix.Symlinkat(target, int(dir.Fd()), base)
	closeErr := dir.Close()
	if symlinkErr != nil {
		return &os.LinkError{Op: "symlink", Old: target, New: link, Err: symlinkErr}
	}
	return closeErr
}

func (f *rootFilesystem) Readlink(link string) (string, error) {
	root, p, _, err := f.resolve(link)
	if err != nil {
		return "", err
	}
	dir, base, err := openCheckedParent(root, p, false)
	if err != nil {
		return "", err
	}
	target, readErr := readlinkAt(int(dir.Fd()), base)
	closeErr := dir.Close()
	if readErr != nil {
		return "", &os.PathError{Op: "readlinkat", Path: link, Err: readErr}
	}
	if closeErr != nil {
		return "", closeErr
	}
	return target, nil
}

func (f *rootFilesystem) Chroot(name string) (billy.Filesystem, error) {
	p, err := f.path(name)
	if err != nil {
		return nil, err
	}
	return &rootFilesystem{checked: f.checked, base: p, mounts: f.mounts}, nil
}

func (f *rootFilesystem) Root() string { return f.base }

func (f *rootFilesystem) Capabilities() billy.Capability { return billy.AllCapabilities }

type mountedFileInfo struct {
	os.FileInfo
	name string
}

func (i mountedFileInfo) Name() string { return i.name }

type rootStandardFS struct{ billy *rootFilesystem }

func (f rootStandardFS) Open(name string) (fs.File, error) {
	file, err := f.billy.Open(name)
	if err != nil {
		return nil, err
	}
	standard, ok := file.(fs.File)
	if !ok {
		_ = file.Close()
		return nil, fmt.Errorf("opened %s through go-billy without fs.File capabilities", name)
	}
	return standard, nil
}

func (f rootStandardFS) ReadDir(name string) ([]fs.DirEntry, error) {
	infos, err := f.billy.ReadDir(name)
	if err != nil {
		return nil, err
	}
	entries := make([]fs.DirEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, fs.FileInfoToDirEntry(info))
	}
	return entries, nil
}

func (f rootStandardFS) ReadLink(name string) (string, error) {
	return f.billy.Readlink(name)
}

// Lstat is required, not optional. fs.ReadLinkFS is satisfied by ReadLink and
// Lstat together, so omitting it made the interface assertion in fs.ReadLink
// fail and every symlink in the store return ErrInvalid -- through Sweep, which
// every write command runs first. One hand-made link disabled all of them, and
// the absolute-target refusal that sits behind the same call could never fire.
func (f rootStandardFS) Lstat(name string) (fs.FileInfo, error) {
	return f.billy.Lstat(name)
}

func (f rootStandardFS) Stat(name string) (fs.FileInfo, error) {
	return f.billy.Stat(name)
}

// Asserted here so a missing method is a build failure rather than a runtime
// ErrInvalid from deep inside a walk.
var _ fs.ReadLinkFS = rootStandardFS{}

type rootFile struct {
	*os.File
	name      string
	readStamp *regularFileStamp
}

func (f *rootFile) Name() string { return f.name }

func (f *rootFile) validateReadStamp() error {
	if f.readStamp == nil {
		return nil
	}
	var current unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &current); err != nil {
		return err
	}
	if err := requireRegularStat(f.name, &current); err != nil {
		return fmt.Errorf("%w: %q changed type while go-git read it: %v", errRegularFileChanged, f.name, err)
	}
	if stampRegularFile(&current) != *f.readStamp {
		return fmt.Errorf("%w: %q metadata changed while go-git read it", errRegularFileChanged, f.name)
	}
	return nil
}

func (f *rootFile) Read(p []byte) (int, error) {
	n, readErr := f.File.Read(p)
	return n, stableReadError(readErr, f.validateReadStamp())
}

func (f *rootFile) ReadAt(p []byte, off int64) (int, error) {
	n, readErr := f.File.ReadAt(p, off)
	return n, stableReadError(readErr, f.validateReadStamp())
}

func (f *rootFile) Close() error {
	validationErr := f.validateReadStamp()
	closeErr := f.File.Close()
	if validationErr == nil {
		return closeErr
	}
	return errors.Join(validationErr, closeErr)
}

func stableReadError(readErr, validationErr error) error {
	if validationErr == nil {
		return readErr
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		return validationErr
	}
	return errors.Join(readErr, validationErr)
}

func (f *rootFile) Lock() error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

func (f *rootFile) Unlock() error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
