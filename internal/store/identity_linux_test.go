//go:build linux

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosensexyz/fu/internal/testenv"
	"golang.org/x/sys/unix"
)

// ext4 hands a freed inode number to the next entry created in the same
// directory, so device and inode alone cannot tell a replacement from the
// original. The handle carries the inode generation and can.
func TestLinuxHandleDistinguishesAReusedInode(t *testing.T) {
	dir := t.TempDir()
	skipUnlessReplacementDetectable(t, dir)
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	path := filepath.Join(dir, "entry")

	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, _, err := entryIdentityAt(int(parent.Fd()), "entry")
	if err != nil {
		t.Fatal(err)
	}
	if first.Handle == "" {
		t.Fatal("a filesystem that reports handle support must yield a handle")
	}
	second := captureSameNameReplacement(t, first, func() FileIdentity {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("two"), 0o644); err != nil {
			t.Fatal(err)
		}
		identity, _, err := entryIdentityAt(int(parent.Fd()), "entry")
		if err != nil {
			t.Fatal(err)
		}
		return identity
	})
	if first.Same(second) {
		t.Fatalf("replacement must not be Same as the original (inode reused: %v): %+v vs %+v",
			first.Inode == second.Inode, first, second)
	}
	t.Logf("replacement inode reused: %v; handles differ: %v", first.Inode == second.Inode, first.Handle != second.Handle)

	// A symlink replaced by a directory of the same name, the adopt shape.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", path); err != nil {
		t.Fatal(err)
	}
	link, _, err := entryIdentityAt(int(parent.Fd()), "entry")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	directory, _, err := entryIdentityAt(int(parent.Fd()), "entry")
	if err != nil {
		t.Fatal(err)
	}
	if link.Same(directory) {
		t.Fatalf("a directory replacing a symlink must not be Same: %+v vs %+v", link, directory)
	}
}

func TestLinuxOpenIdentityCarriesTheSameHandle(t *testing.T) {
	dir := t.TempDir()
	skipUnlessReplacementDetectable(t, dir)
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	byFD, _, err := openIdentity(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if byFD.Handle == "" {
		t.Fatal("descriptor capture must carry a handle where the filesystem exports one")
	}
	byPath, _, err := entryIdentityAt(unixAtFDCWD, path)
	if err != nil {
		t.Fatal(err)
	}
	if byFD != byPath {
		t.Fatalf("descriptor capture %+v must equal name capture %+v", byFD, byPath)
	}
}

func TestLinuxSymlinkHandleIsStableAcrossRename(t *testing.T) {
	dir := t.TempDir()
	skipUnlessReplacementDetectable(t, dir)
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.Symlink("missing-target", filepath.Join(dir, "before")); err != nil {
		t.Fatal(err)
	}

	before, _, err := entryIdentityAt(int(parent.Fd()), "before")
	if err != nil {
		t.Fatal(err)
	}
	if before.Handle == "" {
		t.Fatal("a filesystem that reports handle support must yield a handle")
	}
	if err := os.Rename(filepath.Join(dir, "before"), filepath.Join(dir, "after")); err != nil {
		t.Fatal(err)
	}
	after, _, err := entryIdentityAt(int(parent.Fd()), "after")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("symlink identity changed across rename: %+v vs %+v", before, after)
	}
}

// TestFileHandleUnsupportedClassifiesKernelErrnos pins the classifier
// directly, independent of any filesystem: EOPNOTSUPP and ENOSYS are the only
// errnos the kernel contract supports as "no handle here" (see the doc
// comment on fileHandleUnsupported for EPERM and EINVAL, which look
// plausible but are not). A stray EOVERFLOW -- which x/sys already retries
// internally before returning -- must not be swallowed either.
func TestFileHandleUnsupportedClassifiesKernelErrnos(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"EOPNOTSUPP", unix.EOPNOTSUPP, true},
		{"ENOSYS", unix.ENOSYS, true},
		{"EPERM", unix.EPERM, false},
		{"EINVAL", unix.EINVAL, false},
		{"ENOENT", unix.ENOENT, false},
		{"EOVERFLOW", unix.EOVERFLOW, false},
		{"EACCES", unix.EACCES, false},
		{"non-errno error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := fileHandleUnsupported(tc.err); got != tc.want {
			t.Errorf("%s: fileHandleUnsupported(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestIdentityHandleSupportedFollowsTheFinalSymlink(t *testing.T) {
	gotDirFD := 0
	gotPath := ""
	gotFlags := 0
	supported, err := identityHandleSupportedWith("/tmp/link", func(dirFD int, path string, flags int) (unix.FileHandle, int, error) {
		gotDirFD = dirFD
		gotPath = path
		gotFlags = flags
		return unix.FileHandle{}, 0, nil
	})
	if err != nil || !supported {
		t.Fatalf("identityHandleSupportedWith = %v, %v; want true, nil", supported, err)
	}
	if gotDirFD != unix.AT_FDCWD || gotPath != "/tmp/link" || gotFlags != unix.AT_SYMLINK_FOLLOW {
		t.Fatalf("NameToHandleAt arguments = (%d, %q, %#x), want (%d, %q, %#x)",
			gotDirFD, gotPath, gotFlags, unix.AT_FDCWD, "/tmp/link", unix.AT_SYMLINK_FOLLOW)
	}
}

// handleExportingFilesystemMagics are the filesystems this branch relies on
// exporting file handles: ext4, XFS and btrfs on a real disk, tmpfs for
// unprivileged tests that cannot loop-mount one. t.TempDir() lands on
// whatever TMPDIR is, which is not guaranteed to be one of these -- a plain
// Docker devcontainer, a 9p bind mount and some CI sandboxes may put it on
// overlayfs or 9p instead, neither of which is in this branch's promise list.
var handleExportingFilesystemMagics = map[int64]string{
	int64(unix.EXT4_SUPER_MAGIC):  "EXT4_SUPER_MAGIC",
	int64(unix.XFS_SUPER_MAGIC):   "XFS_SUPER_MAGIC",
	int64(unix.BTRFS_SUPER_MAGIC): "BTRFS_SUPER_MAGIC",
	int64(unix.TMPFS_MAGIC):       "TMPFS_MAGIC",
}

// inodeReusingFilesystemMagics are the handle-exporting filesystems on which
// CI can exercise the original defect rather than merely the handle path.
var inodeReusingFilesystemMagics = map[int64]string{
	int64(unix.EXT4_SUPER_MAGIC): "EXT4_SUPER_MAGIC",
	int64(unix.XFS_SUPER_MAGIC):  "XFS_SUPER_MAGIC",
}

// TestLinuxHandleSupportedWhereThisBranchPromisesHandles asserts the positive
// path through identityHandleSupported directly, with no skip -- but only on
// a filesystem this branch actually claims handles for. Every other Linux
// test in this file also depends on that path returning true, but only
// through skipUnlessReplacementDetectable, which turns "supported came back
// false" into a silent skip rather than a failure; that is the right call for
// t.TempDir() in general, since nothing here promises TMPDIR is on a
// handle-exporting filesystem.
//
// This test used to assert unconditionally (named TestLinuxHandleSupportedOnExt4),
// which failed on a Linux TMPDIR that is not one of the filesystems below --
// overlayfs and 9p among them -- even though `go test ./...` was green there
// before this branch. It now gates on the directory's filesystem magic
// (see the test workflow and DESIGN.md's Linux-filesystem row): on a match, a
// regression that made identityHandleSupported stop reporting true here is
// meant to fail loudly instead of being absorbed as a skip elsewhere; on
// anything else it skips and names the magic in hex, so a future reader can
// identify the filesystem instead of the test failing where nothing was
// promised.
func TestLinuxHandleSupportedWhereThisBranchPromisesHandles(t *testing.T) {
	dir := t.TempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	if testenv.FileHandlesRequired() {
		if _, reusesInodes := inodeReusingFilesystemMagics[int64(stat.Type)]; !reusesInodes {
			t.Fatalf("%s is on filesystem magic %#x; required CI coverage needs an inode-reusing filesystem", dir, stat.Type)
		}
	}
	name, promised := handleExportingFilesystemMagics[int64(stat.Type)]
	if !promised {
		t.Skipf("%s is on filesystem magic %#x, not one this branch claims handles for", dir, stat.Type)
	}
	supported, err := identityHandleSupported(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		t.Fatalf("%s: identityHandleSupported = false, want true; %s (magic %#x) is supposed to export file handles", dir, name, stat.Type)
	}
}

func TestEntryIdentityAtRejectsARealReplacementBetweenSecondStatAndSecondHandle(t *testing.T) {
	dir := t.TempDir()
	skipUnlessReplacementDetectable(t, dir)
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	original, _, err := entryIdentityAt(int(parent.Fd()), "entry")
	if err != nil {
		t.Fatal(err)
	}

	var replacement FileIdentity
	_, _, err = entryIdentityAtWithHooks(int(parent.Fd()), "entry", identityHooks{
		afterSecondStat: func(string) error {
			replacement = captureSameNameReplacement(t, original, func() FileIdentity {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
					t.Fatal(err)
				}
				identity, _, err := entryIdentityAt(int(parent.Fd()), "entry")
				if err != nil {
					t.Fatal(err)
				}
				return identity
			})
			return nil
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("replacement between the second stat and handle must report ErrOwnedTreeChanged, got %v", err)
	}
	if testenv.FileHandlesRequired() {
		if original.Device != replacement.Device || original.Inode != replacement.Inode ||
			original.Handle == "" || replacement.Handle == "" || original.Handle == replacement.Handle {
			t.Fatalf("replacement coverage did not reuse the inode with a new handle: original=%+v replacement=%+v", original, replacement)
		}
	}
}
