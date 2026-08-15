package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// checkedRoot holds two descriptors for one validated logical directory. The
// os.Root supplies the ordinary descriptor-relative filesystem API; dir is a
// raw directory descriptor used where Go has no cross-root operation, such as
// renameat from staging into skills.
type checkedRoot struct {
	root    *os.Root
	dir     *os.File
	display string
}

type checkedRoots struct {
	home     *checkedRoot
	store    *checkedRoot
	skills   *checkedRoot
	git      *checkedRoot
	staging  *checkedRoot
	recovery *checkedRoot
}

// keepDescriptorOwnersAlive is deferred by functions that pass a raw Fd to a
// syscall without otherwise retaining the owning Go object until that syscall
// and all follow-up checks finish. The defer stores the owners until function
// return; the KeepAlive calls then establish the required liveness boundary.
func keepDescriptorOwnersAlive(owners ...any) {
	for _, owner := range owners {
		runtime.KeepAlive(owner)
	}
}

func openPinnedTop(path string) (*checkedRoot, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open logical root %s without following its final component: %w", path, err)
	}
	dir := os.NewFile(uintptr(fd), path)
	if dir == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open logical root %s: invalid file descriptor", path)
	}
	return pairPinnedRoot(dir, path)
}

func pairPinnedRoot(dir *os.File, path string) (*checkedRoot, error) {
	fail := func(err error) (*checkedRoot, error) {
		_ = dir.Close()
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return fail(fmt.Errorf("open validated logical root %s: %w", path, err))
	}
	opened, err := dir.Stat()
	if err != nil {
		_ = root.Close()
		return fail(fmt.Errorf("stat validated logical root %s: %w", path, err))
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return fail(fmt.Errorf("stat rooted logical directory %s: %w", path, err))
	}
	if !os.SameFile(opened, rootInfo) {
		_ = root.Close()
		return fail(fmt.Errorf("%s changed while its logical root was being pinned", path))
	}
	return &checkedRoot{root: root, dir: dir, display: path}, nil
}

func openCheckedTop(path string, want os.FileInfo) (*checkedRoot, error) {
	root, err := openPinnedTop(path)
	if err != nil {
		return nil, fmt.Errorf("open validated logical root %s: %w", path, err)
	}
	opened, err := root.dir.Stat()
	if err != nil {
		_ = root.close()
		return nil, fmt.Errorf("stat validated logical root %s: %w", path, err)
	}
	if want == nil || !os.SameFile(want, opened) {
		_ = root.close()
		return nil, fmt.Errorf("%s no longer names the logical root validated when the store was opened", path)
	}
	return root, nil
}

func openPinnedChild(parent *checkedRoot, name, display string) (*checkedRoot, error) {
	defer keepDescriptorOwnersAlive(parent)
	if parent == nil || parent.dir == nil || parent.root == nil {
		return nil, fmt.Errorf("open logical root %s: parent root is unavailable", display)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, fmt.Errorf("open logical root %s: invalid child name %q", display, name)
	}
	fd, err := unix.Openat(int(parent.dir.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open logical root %s without following links: %w", display, err)
	}
	dir := os.NewFile(uintptr(fd), display)
	if dir == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open logical root %s: invalid file descriptor", display)
	}
	return pairPinnedRoot(dir, display)
}

func openCheckedChild(parent *checkedRoot, name, display string, want os.FileInfo) (*checkedRoot, error) {
	root, err := openPinnedChild(parent, name, display)
	if err != nil {
		return nil, fmt.Errorf("open validated logical root %s: %w", display, err)
	}
	opened, err := root.dir.Stat()
	if err != nil {
		_ = root.close()
		return nil, fmt.Errorf("stat validated logical root %s: %w", display, err)
	}
	if want == nil || !os.SameFile(want, opened) {
		_ = root.close()
		return nil, fmt.Errorf("%s no longer names the logical root validated when the store was opened", display)
	}
	return root, nil
}

func openOrCreatePinnedChild(parent *checkedRoot, name, display string, mode uint32) (*checkedRoot, error) {
	defer keepDescriptorOwnersAlive(parent)
	root, err := openPinnedChild(parent, name, display)
	if err == nil {
		return root, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return nil, err
	}
	if err := unix.Mkdirat(int(parent.dir.Fd()), name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("create logical root %s relative to its pinned parent: %w", display, err)
	}
	return openPinnedChild(parent, name, display)
}

func (r *checkedRoot) close() error {
	if r == nil {
		return nil
	}
	var errs []error
	if r.root != nil {
		if err := r.root.Close(); err != nil {
			errs = append(errs, err)
		}
		r.root = nil
	}
	if r.dir != nil {
		if err := r.dir.Close(); err != nil {
			errs = append(errs, err)
		}
		r.dir = nil
	}
	return errors.Join(errs...)
}

func (r *checkedRoots) close() error {
	if r == nil {
		return nil
	}
	var errs []error
	for _, root := range []*checkedRoot{r.git, r.skills, r.recovery, r.staging, r.store, r.home} {
		if err := root.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validLogicalEntry(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!filepath.IsAbs(name) && !strings.ContainsRune(name, filepath.Separator)
}

// validPublicLogicalEntry applies the stricter grammar for names that callers
// can publish into a fu-managed namespace. Internal transaction artifacts use
// the .fu- prefix, so user-visible names must never be allowed to collide with
// it; low-level helpers still use validLogicalEntry for those artifacts.
func validPublicLogicalEntry(name string) bool {
	return validLogicalEntry(name) && !strings.HasPrefix(name, ".fu-")
}

func renameChecked(src *checkedRoot, srcName string, dst *checkedRoot, dstName string) error {
	return renameCheckedExclusive(src, srcName, dst, dstName, nil)
}

func renameCheckedExclusive(src *checkedRoot, srcName string, dst *checkedRoot, dstName string, beforeRename func()) error {
	defer keepDescriptorOwnersAlive(src, dst)
	if src == nil || src.dir == nil || dst == nil || dst.dir == nil {
		return errors.New("checked logical root is unavailable")
	}
	if !validLogicalEntry(srcName) || !validLogicalEntry(dstName) {
		return fmt.Errorf("rename between checked roots requires single-component names: %q -> %q", srcName, dstName)
	}
	if beforeRename != nil {
		beforeRename()
	}
	if err := renameNoReplace(int(src.dir.Fd()), srcName, int(dst.dir.Fd()), dstName); err != nil {
		return fmt.Errorf("rename %s/%s to unoccupied %s/%s: %w", src.display, srcName, dst.display, dstName, err)
	}
	return nil
}
