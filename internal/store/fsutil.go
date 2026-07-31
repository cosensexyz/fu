// internal/store/fsutil.go
package store

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes via temp file + fsync + rename inside the target
// directory: readers never see partial content and the rename cannot
// cross filesystems (DESIGN §6).
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// WriteFileAtomicRoot is WriteFileAtomic relative to an already checked root.
func WriteFileAtomicRoot(root *os.Root, path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, tmpName, err := createTempRoot(root, dir, ".tmp-")
	if err != nil {
		return err
	}
	defer root.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Chmod(tmpName, perm); err != nil {
		return err
	}
	return root.Rename(tmpName, path)
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
	dir := filepath.Dir(path)
	tmp, tmpName, err := createTempRoot(root, dir, ".tmp-")
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Chmod(tmpName, perm); err != nil {
		return err
	}
	parent, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return renameNoReplace(
		int(parent.Fd()), filepath.Base(tmpName),
		int(parent.Fd()), filepath.Base(path),
	)
}
