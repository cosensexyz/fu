package source

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/store"
)

type scratchIdentity struct {
	device uint64
	inode  uint64
}

type ownedScratch struct {
	parent     *os.File
	rootDir    *os.File
	root       *os.Root
	parentPath string
	name       string
	path       string
	parentID   scratchIdentity
	identity   scratchIdentity
	quarantine string
	closed     bool
}

type scratchCleanupHooks struct {
	beforeContentsCleanup func() error
	beforeRootRetire      func() error
}

type scratchCreateHooks struct {
	afterMkdir     func(parentFD int, name string) error
	inspectCreated func(parentFD int, name string, stat *unix.Stat_t) error
}

func keepScratchDescriptorOwnersAlive(owners ...any) {
	for _, owner := range owners {
		runtime.KeepAlive(owner)
	}
}

func newOwnedScratch(stagingDir string) (_ *ownedScratch, retErr error) {
	return newOwnedScratchWithIdentityHooks(stagingDir, store.FileIdentity{}, scratchCreateHooks{})
}

func newOwnedScratchWithHooks(stagingDir string, hooks scratchCreateHooks) (_ *ownedScratch, retErr error) {
	return newOwnedScratchWithIdentityHooks(stagingDir, store.FileIdentity{}, hooks)
}

func newOwnedScratchChecked(stagingDir string, expected store.FileIdentity) (_ *ownedScratch, retErr error) {
	if expected.Inode == 0 {
		return nil, errors.New("validated staging directory identity is missing")
	}
	return newOwnedScratchWithIdentityHooks(stagingDir, expected, scratchCreateHooks{})
}

func newOwnedScratchWithIdentityHooks(stagingDir string, expected store.FileIdentity, hooks scratchCreateHooks) (_ *ownedScratch, retErr error) {
	parentPath, err := filepath.Abs(stagingDir)
	if err != nil {
		return nil, err
	}
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: parentPath, Err: err}
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	if parent == nil {
		_ = unix.Close(parentFD)
		return nil, fmt.Errorf("open scratch parent %s: invalid descriptor", parentPath)
	}
	defer func() {
		if retErr != nil {
			_ = parent.Close()
		}
	}()
	var parentStat unix.Stat_t
	if err := unix.Fstat(parentFD, &parentStat); err != nil {
		return nil, err
	}
	parentIdentity := sourceScratchIdentity(&parentStat)
	if expected.Inode != 0 && (parentIdentity.device != expected.Device || parentIdentity.inode != expected.Inode) {
		return nil, fmt.Errorf("%s no longer names the validated staging directory", parentPath)
	}

	name, err := newScratchName(".fu-src-")
	if err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
		return nil, fmt.Errorf("create source scratch directory: %w", err)
	}
	created := true
	var createdIdentity scratchIdentity
	defer func() {
		if retErr == nil || !created {
			return
		}
		if createdIdentity.inode != 0 {
			_ = cleanupCreatedScratch(parent, parentPath, name, createdIdentity, nil)
			return
		}
		_ = cleanupUnidentifiedEmptyScratch(parent, parentPath, name)
	}()
	var createdStat unix.Stat_t
	var inspectErr error
	if hooks.inspectCreated != nil {
		inspectErr = hooks.inspectCreated(parentFD, name, &createdStat)
	} else {
		inspectErr = unix.Fstatat(parentFD, name, &createdStat, unix.AT_SYMLINK_NOFOLLOW)
	}
	if inspectErr != nil {
		return nil, fmt.Errorf("inspect created source scratch directory: %w", inspectErr)
	}
	createdIdentity = sourceScratchIdentity(&createdStat)
	if hooks.afterMkdir != nil {
		if err := hooks.afterMkdir(parentFD, name); err != nil {
			return nil, err
		}
	}

	rootFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open source scratch directory: %w", err)
	}
	rootDir := os.NewFile(uintptr(rootFD), filepath.Join(parentPath, name))
	if rootDir == nil {
		_ = unix.Close(rootFD)
		return nil, errors.New("open source scratch directory: invalid descriptor")
	}
	defer func() {
		if retErr != nil {
			_ = rootDir.Close()
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(rootFD, &stat); err != nil {
		return nil, err
	}
	identity := sourceScratchIdentity(&stat)
	if identity != createdIdentity {
		return nil, errors.New("source scratch directory was replaced while opening it")
	}
	path := filepath.Join(parentPath, name)
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = root.Close()
		}
	}()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	dirInfo, err := rootDir.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(rootInfo, dirInfo) {
		return nil, errors.New("source scratch directory was replaced while opening it")
	}
	created = false
	return &ownedScratch{
		parent: parent, rootDir: rootDir, root: root,
		parentPath: parentPath, name: name, path: path, parentID: parentIdentity, identity: identity,
	}, nil
}

// cleanupUnidentifiedEmptyScratch closes the narrow post-Mkdirat window in
// which the created inode could not be inspected. The random private name and
// an atomic directory-only unlink are the available authority: any non-empty
// directory, symlink, file, or other replacement is preserved.
func cleanupUnidentifiedEmptyScratch(parent *os.File, parentPath, name string) error {
	defer keepScratchDescriptorOwnersAlive(parent)
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove unidentified empty source scratch %s: %w", filepath.Join(parentPath, name), err)
	}
	return nil
}

// cleanupCreatedScratch removes an empty directory created by a failed
// constructor. It first retires the original name without replacement and
// post-validates the moved inode, so a new occupant at the public name is
// never the object removed by cleanup.
func cleanupCreatedScratch(parent *os.File, parentPath, name string, expected scratchIdentity, beforeRemove func(string) error) error {
	defer keepScratchDescriptorOwnersAlive(parent)
	retired, err := store.RetireNameAt(parent, name, ".fu-src-orphan-")
	if err != nil {
		return fmt.Errorf("retire failed source scratch %s: %w", filepath.Join(parentPath, name), err)
	}
	var moved unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), retired, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		sourceScratchIdentity(&moved) != expected || uint32(moved.Mode)&uint32(unix.S_IFMT) != uint32(unix.S_IFDIR) {
		mismatch := fmt.Errorf("failed source scratch %s changed during retirement", filepath.Join(parentPath, name))
		if err != nil {
			mismatch = fmt.Errorf("inspect retired source scratch %s: %w", filepath.Join(parentPath, retired), err)
		}
		if restoreErr := store.RestoreRetiredAt(parent, retired, name); restoreErr != nil {
			return errors.Join(mismatch, restoreErr)
		}
		return mismatch
	}
	if beforeRemove != nil {
		if err := beforeRemove(retired); err != nil {
			if restoreErr := store.RestoreRetiredAt(parent, retired, name); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
			return err
		}
	}
	if err := unix.Unlinkat(int(parent.Fd()), retired, unix.AT_REMOVEDIR); err != nil {
		if restoreErr := store.RestoreRetiredAt(parent, retired, name); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return err
	}
	return nil
}

func (s *ownedScratch) Path() string { return s.path }

// Reset removes clone output through the pinned scratch root while retaining
// the root directory for a second branch/tag attempt.
func (s *ownedScratch) Reset() error {
	return s.resetWithHooks(scratchCleanupHooks{})
}

func (s *ownedScratch) resetWithHooks(h scratchCleanupHooks) error {
	if s.closed {
		return errors.New("source scratch directory is closed")
	}
	if err := s.validateNamed(s.name); err != nil {
		return err
	}
	if err := s.removeContents(h); err != nil {
		return err
	}
	return s.validateNamed(s.name)
}

// Close quarantines the exact scratch root with a descriptor-relative,
// no-replace rename, verifies the moved identity, and only then removes its
// contents through the pinned root descriptor.
func (s *ownedScratch) Close() (retErr error) {
	return s.closeWithHooks(scratchCleanupHooks{})
}

func (s *ownedScratch) closeWithHooks(h scratchCleanupHooks) (retErr error) {
	defer keepScratchDescriptorOwnersAlive(s)
	if s.closed {
		return nil
	}
	if s.quarantine == "" {
		if err := s.validateNamed(s.name); err != nil {
			return s.closeDescriptors(err)
		}
		quarantine, err := newScratchName(".fu-src-clean-")
		if err != nil {
			return s.closeDescriptors(err)
		}
		if h.beforeRootRetire != nil {
			if err := h.beforeRootRetire(); err != nil {
				return s.closeDescriptors(err)
			}
		}
		if err := store.RenameNoReplaceAt(s.parent, s.name, s.parent, quarantine); err != nil {
			return s.closeDescriptors(fmt.Errorf("quarantine source scratch directory: %w", err))
		}
		if err := s.validateNamed(quarantine); err != nil {
			if restoreErr := store.RenameNoReplaceAt(s.parent, quarantine, s.parent, s.name); restoreErr != nil {
				return s.closeDescriptors(errors.Join(err, fmt.Errorf("restore unexpected source scratch entry: %w", restoreErr)))
			}
			return s.closeDescriptors(err)
		}
		s.quarantine = quarantine
	}
	manifest, err := store.SnapshotRootOwned(s.root, ".")
	if err != nil {
		return fmt.Errorf("snapshot source scratch before cleanup: %w", err)
	}
	if h.beforeContentsCleanup != nil {
		if err := h.beforeContentsCleanup(); err != nil {
			return err
		}
	}
	if err := store.RemoveOwnedContents(s.rootDir, manifest); err != nil {
		return err
	}
	if err := s.validateNamed(s.quarantine); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(s.parent.Fd()), s.quarantine, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove quarantined source scratch directory: %w", err)
	}
	return s.closeDescriptors(nil)
}

func (s *ownedScratch) closeDescriptors(err error) error {
	s.closed = true
	return errors.Join(err, s.root.Close(), s.rootDir.Close(), s.parent.Close())
}

func (s *ownedScratch) validateNamed(name string) error {
	defer keepScratchDescriptorOwnersAlive(s)
	if err := s.validateParentPath(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(s.parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("source scratch entry %s cannot be verified: %w", filepath.Join(s.parentPath, name), err)
	}
	if sourceScratchIdentity(&stat) != s.identity || uint32(stat.Mode)&uint32(unix.S_IFMT) != uint32(unix.S_IFDIR) {
		return fmt.Errorf("source scratch entry %s was replaced; preserving it", filepath.Join(s.parentPath, name))
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(s.rootDir.Fd()), &opened); err != nil {
		return err
	}
	if sourceScratchIdentity(&opened) != s.identity {
		return errors.New("pinned source scratch descriptor changed identity")
	}
	return nil
}

func (s *ownedScratch) validateParentPath() error {
	fd, err := unix.Open(s.parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("source scratch parent %s cannot be verified: %w", s.parentPath, err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if sourceScratchIdentity(&stat) != s.parentID {
		return fmt.Errorf("source scratch parent %s was replaced; preserving scratch content", s.parentPath)
	}
	return nil
}

func (s *ownedScratch) removeContents(h scratchCleanupHooks) error {
	manifest, err := store.SnapshotRootOwned(s.root, ".")
	if err != nil {
		return err
	}
	if h.beforeContentsCleanup != nil {
		if err := h.beforeContentsCleanup(); err != nil {
			return err
		}
	}
	return store.RemoveOwnedContents(s.rootDir, manifest)
}

func newScratchName(prefix string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func sourceScratchIdentity(stat *unix.Stat_t) scratchIdentity {
	return scratchIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
}
