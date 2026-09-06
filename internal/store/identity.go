package store

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func identityPathError(path string, err error) error {
	return &os.PathError{Op: "identify", Path: path, Err: err}
}

// FileIdentity is a persistent identity for one filesystem entry on the
// supported POSIX platforms. Renames preserve it; replacement does not.
//
// Device and inode alone are not a persistent identity on Linux: ext4 hands a
// freed inode number to the next entry created, so an entry removed and
// recreated under the same name keeps both numbers. Handle carries the kernel
// file handle from name_to_handle_at(2) where the filesystem exports one; it
// embeds the inode generation, which every reallocation changes, and survives
// rename and rename-exchange like the inode does. It is empty on macOS (APFS
// never reuses an inode number) and on Linux filesystems that export no
// handle (overlayfs, for example), where Same degrades to device and inode.
//
// Identities are captured only through entryIdentityAt and openIdentity below.
// identity_guard_test.go rejects any other code in this package that reads a
// Stat_t inode directly; internal/engine/identity_test.go enforces the same
// rule for internal/engine, exempting reconcile.go, currently only for its
// sameCheckedEntry recheck.
type FileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	// Handle is "<type>:<hex>" of the kernel file handle, or empty.
	Handle string `json:"handle,omitempty"`
}

// Valid reports whether the identity was captured at all.
func (id FileIdentity) Valid() bool {
	return id.Inode != 0
}

// Same reports whether both identities name the same filesystem object. A
// missing handle on either side degrades the comparison to device and inode:
// persisted identities captured without a handle, and filesystems that export
// none, still reconcile. When both sides carry a handle, a mismatch is
// decisive. This fallback makes Same deliberately non-transitive; callers must
// compare every required pair directly rather than infer equality through a
// chain of comparisons.
func (id FileIdentity) Same(other FileIdentity) bool {
	if id.Device != other.Device || id.Inode != other.Inode {
		return false
	}
	return id.Handle == "" || other.Handle == "" || id.Handle == other.Handle
}

// unixAtFDCWD keeps pathname-mode identity captures explicit at call sites.
const unixAtFDCWD = unix.AT_FDCWD

// identityHooks lets tests act between the two halves of a sealed capture.
type identityHooks struct {
	afterFirstStat  func(name string) error
	afterHandle     func(name string) error
	afterSecondStat func(name string) error
	lookupHandle    func(parentFD int, name string) (string, error)
}

// entryIdentityAt captures the identity of name beneath parentFD without
// following a final symlink, and returns the stat it was captured from. On a
// filesystem that exports handles, the two stat and two handle lookups seal a
// capture across at most one same-name replacement. Repeated alternating
// replacements can make both sample pairs appear stable while pairing one
// generation's stat with another generation's handle; callers must not treat
// the seal as protection against an adversarial replacement loop. A replacement
// before the first handle lookup which reuses the inode is returned as the
// replacement itself, so callers must use the returned stat for type and mode
// decisions. Where handles are unavailable, the seal compares only device,
// inode and type and cannot detect every reuse.
//
// parentFD may be unix.AT_FDCWD, in which case name may be a full path.
func entryIdentityAt(parentFD int, name string) (FileIdentity, unix.Stat_t, error) {
	return entryIdentityAtWithHooks(parentFD, name, identityHooks{})
}

func entryIdentityAtWithHooks(parentFD int, name string, hooks identityHooks) (FileIdentity, unix.Stat_t, error) {
	var first unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &first, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return FileIdentity{}, unix.Stat_t{}, err
	}
	if hooks.afterFirstStat != nil {
		if err := hooks.afterFirstStat(name); err != nil {
			return FileIdentity{}, unix.Stat_t{}, err
		}
	}
	handleAt := fileHandleAt
	if hooks.lookupHandle != nil {
		handleAt = hooks.lookupHandle
	}
	lookupHandle := func() (string, error) {
		handle, err := handleAt(parentFD, name)
		if err == nil {
			return handle, nil
		}
		if errors.Is(err, unix.ENOENT) {
			// Bare errno, not wrapped: os.IsNotExist unwraps only
			// *PathError, *LinkError and *SyscallError, not an arbitrary
			// %w chain, so wrapping ENOENT here would defeat it for
			// callers that use it -- go-git's dotgit layer among them, for
			// packed-refs and config.
			return "", err
		}
		return "", fmt.Errorf("file handle of %q: %w", name, err)
	}
	firstHandle, err := lookupHandle()
	if err != nil {
		return FileIdentity{}, unix.Stat_t{}, err
	}
	if hooks.afterHandle != nil {
		if err := hooks.afterHandle(name); err != nil {
			return FileIdentity{}, unix.Stat_t{}, err
		}
	}
	var second unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &second, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return FileIdentity{}, unix.Stat_t{}, err
	}
	if hooks.afterSecondStat != nil {
		if err := hooks.afterSecondStat(name); err != nil {
			return FileIdentity{}, unix.Stat_t{}, err
		}
	}
	secondHandle, err := lookupHandle()
	if err != nil {
		return FileIdentity{}, unix.Stat_t{}, err
	}
	if identitySealChanged(first, firstHandle, second, secondHandle) {
		return FileIdentity{}, unix.Stat_t{}, fmt.Errorf("%w: %q was replaced while being identified", ErrOwnedTreeChanged, name)
	}
	return FileIdentity{Device: uint64(second.Dev), Inode: uint64(second.Ino), Handle: secondHandle}, second, nil
}

func identitySealChanged(first unix.Stat_t, firstHandle string, second unix.Stat_t, secondHandle string) bool {
	return first.Dev != second.Dev || first.Ino != second.Ino ||
		first.Mode&unix.S_IFMT != second.Mode&unix.S_IFMT || firstHandle != secondHandle
}

// openIdentity captures the identity of the object behind an open descriptor
// and returns its stat. Stat and handle come from the same open object, so no
// seal is needed. For the same object, its encoded handle must equal the handle
// returned by entryIdentityAt; whole-directory adoption relies on that invariant.
func openIdentity(fd int) (FileIdentity, unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return FileIdentity{}, unix.Stat_t{}, err
	}
	handle, err := fileHandleOf(fd)
	if err != nil {
		return FileIdentity{}, unix.Stat_t{}, fmt.Errorf("file handle of descriptor %d: %w", fd, err)
	}
	return FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Handle: handle}, stat, nil
}

// EntryIdentityAt is entryIdentityAt for other packages.
func EntryIdentityAt(parentFD int, name string) (FileIdentity, unix.Stat_t, error) {
	return entryIdentityAt(parentFD, name)
}

// OpenIdentity is openIdentity for other packages.
func OpenIdentity(fd int) (FileIdentity, unix.Stat_t, error) {
	return openIdentity(fd)
}

// IdentityHandleSupported reports whether the filesystem holding dir exports
// file handles. It is false everywhere but Linux.
func IdentityHandleSupported(dir string) (bool, error) {
	return identityHandleSupported(dir)
}
