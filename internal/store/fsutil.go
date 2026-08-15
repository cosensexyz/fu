// internal/store/fsutil.go
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// RenameNoReplaceAt atomically renames one descriptor-relative entry without
// replacing an existing destination. Callers must validate both parent
// descriptors before invoking it.
func RenameNoReplaceAt(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	err := renameNoReplace(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName)
	runtime.KeepAlive(oldDir)
	runtime.KeepAlive(newDir)
	return err
}

// WriteFileAtomic writes via temp file + fsync + rename inside the target
// directory: readers never see partial content and the rename cannot
// cross filesystems (DESIGN §6).
//
// This is the *replacing* writer. It has no production caller: the bootstrap
// write it existed for now uses WriteFileAtomicNoReplace, since that branch
// runs only when the destination is absent (round 18 finding M4). It is kept
// as the shape a future replacing write should take -- installing bytes over a
// destination that may already exist and whose current occupant is not fu's to
// prove -- not as something currently in use. Everything on a durable path uses
// WriteFileAtomicNoReplaceRoot instead, which holds the descriptor through
// publication and validates the installed object. Two shapes round 18 finding
// M4 called out are fixed here rather than left to differ from the hardened
// variant: the mode is applied with fchmod on the open descriptor (a
// path-based Chmod after Close could follow a symlink raced into the temp
// name), and the deferred cleanup stops at publication instead of unlinking
// whatever later occupies the freed source name.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmpName)
		}
	}()
	if err := fillRegularFile(tmp, data, perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	published = true
	return nil
}

// WriteFileAtomicRoot is WriteFileAtomic relative to an already checked root,
// with the same two hardenings.
func WriteFileAtomicRoot(root *os.Root, path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, tmpName, err := createTempRoot(root, dir, ".tmp-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = root.Remove(tmpName)
		}
	}()
	if err := fillRegularFile(tmp, data, perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmpName, path); err != nil {
		return err
	}
	published = true
	return nil
}

// WriteFileAtomicNoReplace writes a complete file and atomically installs it
// only when the destination name is absent.
func WriteFileAtomicNoReplace(path string, data []byte, perm os.FileMode) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	return WriteFileAtomicNoReplaceRoot(root, filepath.Base(path), data, perm)
}

// WriteFileAtomicNoReplaceRoot is WriteFileAtomicNoReplace relative to an
// already checked root.
func WriteFileAtomicNoReplaceRoot(root *os.Root, path string, data []byte, perm os.FileMode) error {
	return writeFileAtomicNoReplaceRoot(root, path, data, perm, atomicWriteHooks{})
}

type atomicWriteHooks struct {
	beforeRename func(string) error
	afterRename  func(string) error
	closeTemp    func(*os.File) error
}

func writeFileAtomicNoReplaceRoot(root *os.Root, path string, data []byte, perm os.FileMode, hooks atomicWriteHooks) (retErr error) {
	dir := filepath.Dir(path)
	tmp, tmpName, err := createTempRoot(root, dir, ".tmp-")
	if err != nil {
		return err
	}
	tmpOpen := true
	defer func() {
		if tmpOpen {
			retErr = errors.Join(retErr, tmp.Close())
		}
	}()
	// Until both the temporary's identity and the parent descriptor are in
	// hand, the identity-proven cleanup below cannot run -- and the two early
	// returns between here and there used to leave a .tmp-<hex> file behind
	// forever in the recovery or archive root (round 18 finding M5). Removing
	// it by name is safe in exactly this window: nothing has been published,
	// and the name is freshly generated and unpredictable.
	earlyCleanup := true
	defer func() {
		if !earlyCleanup {
			return
		}
		if err := root.Remove(tmpName); err != nil && !errors.Is(err, unix.ENOENT) && !os.IsNotExist(err) {
			retErr = errors.Join(retErr, fmt.Errorf("remove unpublished atomic temporary %q: %w", tmpName, err))
		}
	}()
	var created unix.Stat_t
	if err := unix.Fstat(int(tmp.Fd()), &created); err != nil {
		runtime.KeepAlive(tmp)
		return err
	}
	runtime.KeepAlive(tmp)
	expected := identityFromStat(&created)
	tempOwned := true
	parent, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, parent.Close())
	}()
	earlyCleanup = false
	defer func() {
		if !tempOwned {
			return
		}
		stat, err := statAt(int(parent.Fd()), filepath.Base(tmpName))
		runtime.KeepAlive(parent)
		if errors.Is(err, unix.ENOENT) {
			return
		}
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("inspect atomic temporary during cleanup: %w", err))
			return
		}
		if identityFromStat(&stat) != expected || uint32(stat.Mode)&uint32(unix.S_IFMT) != uint32(unix.S_IFREG) {
			retErr = errors.Join(retErr, fmt.Errorf("%w: atomic temporary %q was replaced; preserving it", ErrOwnedTreeChanged, tmpName))
			return
		}
		retErr = errors.Join(retErr, retireOwnedLeafAt(parent, filepath.Base(tmpName), ".tmp-retired-", expected, uint32(unix.S_IFREG)))
	}()
	if err := fillRegularFile(tmp, data, perm); err != nil {
		return err
	}
	closeTemp := tmp.Close
	if hooks.closeTemp != nil {
		closeTemp = func() error { return hooks.closeTemp(tmp) }
	}
	closeErr := closeTemp()
	tmpOpen = false
	if closeErr != nil {
		return fmt.Errorf("close atomic temporary before publication: %w", closeErr)
	}
	// The descriptor is closed before the namespace mutation. A close failure
	// therefore cannot report a published file as failed, and the deferred
	// close remains harmless for every earlier return.
	if hooks.beforeRename != nil {
		if err := hooks.beforeRename(tmpName); err != nil {
			return err
		}
	}
	current, err := statAt(int(parent.Fd()), filepath.Base(tmpName))
	runtime.KeepAlive(parent)
	if err != nil || identityFromStat(&current) != expected || uint32(current.Mode)&uint32(unix.S_IFMT) != uint32(unix.S_IFREG) {
		return fmt.Errorf("%w: atomic temporary %q was replaced before publication", ErrOwnedTreeChanged, tmpName)
	}
	if err := renameNoReplace(
		int(parent.Fd()), filepath.Base(tmpName),
		int(parent.Fd()), filepath.Base(path),
	); err != nil {
		runtime.KeepAlive(parent)
		return err
	}
	runtime.KeepAlive(parent)
	// The source name is free after rename. Never run name-based deferred
	// cleanup against a later occupant of that name.
	tempOwned = false
	installed, err := statAt(int(parent.Fd()), filepath.Base(path))
	runtime.KeepAlive(parent)
	if err != nil || identityFromStat(&installed) != expected || uint32(installed.Mode)&uint32(unix.S_IFMT) != uint32(unix.S_IFREG) {
		restoreErr := renameNoReplace(int(parent.Fd()), filepath.Base(path), int(parent.Fd()), filepath.Base(tmpName))
		runtime.KeepAlive(parent)
		mismatch := fmt.Errorf("%w: installed atomic file %q does not match its live descriptor", ErrOwnedTreeChanged, path)
		if restoreErr != nil {
			return errors.Join(mismatch, fmt.Errorf("restore mismatched installed file: %w", restoreErr))
		}
		return mismatch
	}
	if hooks.afterRename != nil {
		if err := hooks.afterRename(tmpName); err != nil {
			return err
		}
	}
	return nil
}
