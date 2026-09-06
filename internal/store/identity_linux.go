//go:build linux

package store

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// fileHandleAt returns the kernel file handle of name beneath dirFD without
// following a final symlink, or "" when the filesystem exports no handle.
func fileHandleAt(dirFD int, name string) (string, error) {
	handle, _, err := unix.NameToHandleAt(dirFD, name, 0)
	return encodeFileHandle(handle, err)
}

// fileHandleOf is fileHandleAt for an open descriptor.
func fileHandleOf(fd int) (string, error) {
	handle, _, err := unix.NameToHandleAt(fd, "", unix.AT_EMPTY_PATH)
	return encodeFileHandle(handle, err)
}

func encodeFileHandle(handle unix.FileHandle, err error) (string, error) {
	if err != nil {
		if fileHandleUnsupported(err) {
			return "", nil
		}
		return "", err
	}
	return fmt.Sprintf("%d:%x", handle.Type(), handle.Bytes()), nil
}

// fileHandleUnsupported classifies the errors that mean "no handle here"
// rather than "the entry changed". The kept set is narrow on purpose:
// EOPNOTSUPP (which ENOTSUP aliases on Linux) is the documented result of a
// filesystem with no export operations, and ENOSYS is a kernel built without
// the syscall.
//
// EPERM and EINVAL were tried here and dropped. EPERM is documented for
// open_by_handle_at, not for name_to_handle_at; it was added on the theory
// that a seccomp profile would deny the syscall that way, but testing showed
// Docker's default profile blocks open_by_handle_at while still allowing
// name_to_handle_at, so the theory had no case to support it. EINVAL is not a
// kernel signal for "no handle" either: x/sys already retries EOVERFLOW
// internally before returning, so an EINVAL that survives is more likely a
// real bug than a benign unsupported filesystem.
//
// The strict direction is the safe default: misclassifying an error as
// "unsupported" here degrades that entry's identity to device and inode
// silently, which on ext4 is exactly the defect this file exists to close.
// Leaving an errno unclassified instead fails the calling write command
// loudly, with the entry named in the error, where it can be seen and
// reported. Anything outside the kept set, ENOENT and EOVERFLOW above all, is
// reported to the caller.
func fileHandleUnsupported(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS)
}

func identityHandleSupported(dir string) (bool, error) {
	return identityHandleSupportedWith(dir, unix.NameToHandleAt)
}

type nameToHandleAtFunc func(int, string, int) (unix.FileHandle, int, error)

func identityHandleSupportedWith(dir string, lookup nameToHandleAtFunc) (bool, error) {
	_, _, err := lookup(unix.AT_FDCWD, dir, unix.AT_SYMLINK_FOLLOW)
	if err == nil {
		return true, nil
	}
	if fileHandleUnsupported(err) {
		return false, nil
	}
	return false, err
}
