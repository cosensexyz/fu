package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/testenv"
	"golang.org/x/sys/unix"
)

func TestFileIdentitySameDegradesWithoutHandles(t *testing.T) {
	base := FileIdentity{Device: 7, Inode: 42}
	cases := []struct {
		name string
		a, b FileIdentity
		want bool
	}{
		{"both without handle", base, base, true},
		{"one side without handle", base, FileIdentity{Device: 7, Inode: 42, Handle: "1:aa"}, true},
		{"other side without handle", FileIdentity{Device: 7, Inode: 42, Handle: "1:aa"}, base, true},
		{"equal handles", FileIdentity{Device: 7, Inode: 42, Handle: "1:aa"}, FileIdentity{Device: 7, Inode: 42, Handle: "1:aa"}, true},
		{"different handles", FileIdentity{Device: 7, Inode: 42, Handle: "1:aa"}, FileIdentity{Device: 7, Inode: 42, Handle: "1:bb"}, false},
		{"different inode", base, FileIdentity{Device: 7, Inode: 43}, false},
		{"different device", base, FileIdentity{Device: 8, Inode: 42}, false},
	}
	for _, tc := range cases {
		if got := tc.a.Same(tc.b); got != tc.want {
			t.Errorf("%s: Same = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDirectoryEntryAndDescriptorCapturesHaveTheSameIdentity(t *testing.T) {
	parentPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(parentPath, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	entryIdentity, _, err := EntryIdentityAt(int(parent.Fd()), "child")
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.Open(filepath.Join(parentPath, "child"))
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	descriptorIdentity, _, err := OpenIdentity(int(child.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !entryIdentity.Same(descriptorIdentity) {
		t.Fatalf("entry identity %+v differs from descriptor identity %+v", entryIdentity, descriptorIdentity)
	}
	if testenv.FileHandlesRequired() && (entryIdentity.Handle == "" || descriptorIdentity.Handle == "") {
		t.Fatalf("required directory captures lack handles: entry=%+v descriptor=%+v", entryIdentity, descriptorIdentity)
	}
}

func TestFileIdentityValid(t *testing.T) {
	if (FileIdentity{}).Valid() {
		t.Fatal("zero identity must not be valid")
	}
	if !(FileIdentity{Device: 1, Inode: 2}).Valid() {
		t.Fatal("captured identity must be valid")
	}
}

func TestIdentityPathErrorUsesANeutralOperation(t *testing.T) {
	cause := unix.EPERM
	err := identityPathError("/store/fu.yaml", cause)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("identity error = %T, want *os.PathError", err)
	}
	if pathErr.Op != "identify" || !errors.Is(err, cause) {
		t.Fatalf("identity error = %v, want neutral operation preserving cause", err)
	}
}

func TestFileIdentityJSONRoundTrip(t *testing.T) {
	withHandle := FileIdentity{Device: 1, Inode: 2, Handle: "1:0e0000006820363a"}
	raw, err := json.Marshal(withHandle)
	if err != nil {
		t.Fatal(err)
	}
	var back FileIdentity
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != withHandle {
		t.Fatalf("round trip = %+v, want %+v", back, withHandle)
	}
	rawNoHandle, err := json.Marshal(FileIdentity{Device: 1, Inode: 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(rawNoHandle) != `{"device":1,"inode":2}` {
		t.Fatalf("an empty handle must be omitted, got %s", rawNoHandle)
	}
	var legacy FileIdentity
	if err := json.Unmarshal([]byte(`{"device":1,"inode":2}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Handle != "" || !legacy.Same(withHandle) {
		t.Fatalf("a record written before handles existed must still reconcile: %+v", legacy)
	}
}

func captureSameNameReplacement(t *testing.T, original FileIdentity, replace func() FileIdentity) FileIdentity {
	t.Helper()
	attempts := 1
	if testenv.FileHandlesRequired() {
		attempts = 32
	}
	var replacement FileIdentity
	for range attempts {
		replacement = replace()
		if replacement.Device == original.Device && replacement.Inode == original.Inode {
			return replacement
		}
	}
	if testenv.FileHandlesRequired() {
		t.Fatalf("required regression coverage did not reuse inode %d after %d same-name replacements", original.Inode, attempts)
	}
	return replacement
}

// The seal: the entry is stat'ed before and after the handle lookup. A
// replacement in between must never come back as a mixed identity (stat of
// the original, handle of the replacement). On APFS the replacement has a new
// inode and the seal reports it; on ext4 the replacement reuses the inode, so
// the capture describes the replacement -- which is what is on disk.
func TestEntryIdentityAtNeverReturnsAMixedIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	identity, capturedStat, err := entryIdentityAtWithHooks(int(parent.Fd()), "entry", identityHooks{
		afterFirstStat: func(name string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0o644)
		},
	})
	replacement, replacementStat, replacementErr := entryIdentityAt(int(parent.Fd()), "entry")
	if replacementErr != nil {
		t.Fatal(replacementErr)
	}
	switch {
	case err == nil && !identity.Same(replacement):
		t.Fatalf("capture returned an identity that is neither an error nor the replacement: %+v vs %+v", identity, replacement)
	case err != nil && !errors.Is(err, ErrOwnedTreeChanged):
		t.Fatalf("seal must report ErrOwnedTreeChanged, got %v", err)
	}
	if err == nil && (capturedStat.Size != replacementStat.Size || capturedStat.Mode != replacementStat.Mode) {
		t.Fatalf("successful capture returned stale stat %+v, want replacement stat %+v", capturedStat, replacementStat)
	}
	if runtime.GOOS == "darwin" && err == nil {
		t.Fatal("APFS never reuses an inode number, so the seal must have reported the replacement")
	}
}

func TestEntryIdentityAtKeepsTheStatSealWithoutFileHandles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	_, _, err = entryIdentityAtWithHooks(int(parent.Fd()), "entry", identityHooks{
		lookupHandle: func(int, string) (string, error) { return "", nil },
		afterFirstStat: func(string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0o755)
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("type replacement without handles must trip the stat seal, got %v", err)
	}
}

func TestEntryIdentityAtRejectsAHandleChangeBetweenLookups(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	handleCalls := 0
	_, _, err = entryIdentityAtWithHooks(int(parent.Fd()), "entry", identityHooks{
		lookupHandle: func(_ int, _ string) (string, error) {
			handleCalls++
			if handleCalls == 1 {
				return "1:aa", nil
			}
			return "1:bb", nil
		},
		afterHandle: func(name string) error {
			if name != "entry" || handleCalls != 1 {
				t.Fatalf("afterHandle = (%q, calls %d), want (entry, calls 1)", name, handleCalls)
			}
			return nil
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("a handle change between lookups must report ErrOwnedTreeChanged, got %v", err)
	}
	if !strings.Contains(err.Error(), "filesystem entry changed externally") {
		t.Fatalf("seal error must describe a generic filesystem entry, got %v", err)
	}
	if handleCalls != 2 {
		t.Fatalf("handle lookup calls = %d, want 2", handleCalls)
	}
}

func TestIdentitySealChangedChecksEveryIdentityComponent(t *testing.T) {
	base := unix.Stat_t{Dev: 1, Ino: 2, Mode: unix.S_IFREG | 0o600}
	cases := []struct {
		name         string
		first        unix.Stat_t
		second       unix.Stat_t
		firstHandle  string
		secondHandle string
		want         bool
	}{
		{name: "equal", first: base, second: base, firstHandle: "1:aa", secondHandle: "1:aa"},
		{name: "device", first: base, second: unix.Stat_t{Dev: 2, Ino: 2, Mode: unix.S_IFREG | 0o600}, firstHandle: "1:aa", secondHandle: "1:aa", want: true},
		{name: "inode", first: base, second: unix.Stat_t{Dev: 1, Ino: 3, Mode: unix.S_IFREG | 0o600}, firstHandle: "1:aa", secondHandle: "1:aa", want: true},
		{name: "type", first: base, second: unix.Stat_t{Dev: 1, Ino: 2, Mode: unix.S_IFDIR | 0o700}, firstHandle: "1:aa", secondHandle: "1:aa", want: true},
		{name: "handle", first: base, second: base, firstHandle: "1:aa", secondHandle: "1:bb", want: true},
		{name: "permissions only", first: base, second: unix.Stat_t{Dev: 1, Ino: 2, Mode: unix.S_IFREG | 0o644}, firstHandle: "1:aa", secondHandle: "1:aa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identitySealChanged(tc.first, tc.firstHandle, tc.second, tc.secondHandle); got != tc.want {
				t.Fatalf("identitySealChanged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEntryIdentityAtRejectsAReplacementBetweenFirstHandleAndSecondStat(t *testing.T) {
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

	_, _, err = entryIdentityAtWithHooks(int(parent.Fd()), "entry", identityHooks{
		afterHandle: func(string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0o644)
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("replacement after the first handle must report ErrOwnedTreeChanged, got %v (original %+v)", err, original)
	}
}

func TestEntryIdentityAtKeepsENOENTVisibleToOsIsNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	handleCalls := 0
	_, _, err = entryIdentityAtWithHooks(int(parent.Fd()), "entry", identityHooks{
		lookupHandle: func(int, string) (string, error) {
			handleCalls++
			return "", unix.ENOENT
		},
	})
	if !os.IsNotExist(err) {
		t.Fatalf("identity lookup error = %v, want os.IsNotExist", err)
	}
	if handleCalls != 1 {
		t.Fatalf("handle lookups = %d, want the first lookup to return ENOENT", handleCalls)
	}
}

func TestEntryIdentityAtAndOpenIdentityAgree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	byName, stat, err := entryIdentityAt(int(parent.Fd()), "entry")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(dir, "entry"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	byFD, _, err := openIdentity(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !byName.Valid() || !byFD.Valid() || !byName.Same(byFD) || byName != byFD {
		t.Fatalf("name capture %+v and descriptor capture %+v must be identical", byName, byFD)
	}
	if stat.Size != 1 {
		t.Fatalf("returned stat must describe the entry, size = %d", stat.Size)
	}
}

func TestEntryIdentityAtDoesNotFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	link, _, err := entryIdentityAt(int(parent.Fd()), "link")
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := entryIdentityAt(int(parent.Fd()), "target")
	if err != nil {
		t.Fatal(err)
	}
	if link.Same(target) {
		t.Fatal("the link's identity must be the link's own, not its target's")
	}
	if link.Handle != "" && target.Handle != "" && link.Handle == target.Handle {
		t.Fatalf("link and target handles must differ: %+v vs %+v", link, target)
	}
}

func TestEntryIdentityAtCapturesADanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("missing-target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	identity, stat, err := entryIdentityAt(int(parent.Fd()), "link")
	if err != nil {
		t.Fatalf("capture dangling symlink: %v", err)
	}
	if !identity.Valid() || stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("dangling symlink capture = identity %+v mode %#o", identity, stat.Mode)
	}
}

func TestEntryIdentityAtAcceptsAPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	byPath, _, err := entryIdentityAt(unixAtFDCWD, path)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	byName, _, err := entryIdentityAt(int(parent.Fd()), "entry")
	if err != nil {
		t.Fatal(err)
	}
	if byPath != byName {
		t.Fatalf("path capture %+v must equal name capture %+v", byPath, byName)
	}
}

// skipUnlessReplacementDetectable skips a test whose assertion depends on a
// same-name replacement being distinguishable from the original. On Linux that
// needs the filesystem to export file handles: ext4 reuses a freed inode
// immediately, so without a handle the replacement carries the original's
// whole identity. APFS never reuses inode numbers, so macOS needs no check.
func skipUnlessReplacementDetectable(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	supported, err := identityHandleSupported(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		if testenv.FileHandlesRequired() {
			t.Fatalf("%s does not export file handles; Linux CI must exercise replacement detection", dir)
		}
		t.Skipf("%s does not export file handles; a same-name replacement is not detectable here", dir)
	}
}
