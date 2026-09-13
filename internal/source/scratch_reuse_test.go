package source

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
	"github.com/cosensexyz/fu/internal/testenv"
)

func skipUnlessReplacementDetectable(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	supported, err := store.IdentityHandleSupported(dir)
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

func requireReusedWithNewHandle(t *testing.T, before, after store.FileIdentity, reuse testenv.InodeReuse) {
	t.Helper()
	if !testenv.FileHandlesRequired() {
		return
	}
	if !reuse.Reused || before.Device != after.Device || before.Inode != after.Inode || before.Handle == "" || after.Handle == "" || before.Handle == after.Handle {
		t.Fatalf("replacement coverage did not reuse the inode with a new handle: reuse=%+v before=%+v after=%+v", reuse, before, after)
	}
}

// The scratch root replaced on the same inode between its creation and its
// opening must be refused, and the replacement must survive the failed
// constructor's cleanup.
func TestOwnedScratchRefusesASameInodeReplacementBeforeOpening(t *testing.T) {
	parent := t.TempDir()
	skipUnlessReplacementDetectable(t, parent)
	var before, after store.FileIdentity
	var reuse testenv.InodeReuse
	var scratchName string
	_, err := newOwnedScratchWithHooks(parent, scratchCreateHooks{afterMkdir: func(parentFD int, name string) error {
		scratchName = name
		var err error
		if before, _, err = store.EntryIdentityAt(parentFD, name); err != nil {
			return err
		}
		if reuse, err = testenv.ReplaceOnSameInode(parent, name, true); err != nil {
			return err
		}
		after, _, err = store.EntryIdentityAt(parentFD, name)
		return err
	}})
	if err == nil {
		t.Fatal("a scratch root replaced before it was opened must be refused")
	}
	if _, statErr := os.Lstat(filepath.Join(parent, scratchName)); statErr != nil {
		t.Fatalf("the replacement must survive the failed constructor's cleanup: %v", statErr)
	}
	requireReusedWithNewHandle(t, before, after, reuse)
}

// The object cleanup retires and rechecks is compared against the identity
// captured at creation. A directory planted at the scratch name on the same
// inode before cleanup runs -- the constructor's deferred cleanup follows a
// failure, and the name is public -- is not that object: it must be
// restored to its name, never removed.
func TestCleanupCreatedScratchPreservesASameInodeReplacement(t *testing.T) {
	parentPath := t.TempDir()
	skipUnlessReplacementDetectable(t, parentPath)
	parent, err := os.Open(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	const name = ".fu-src-created"
	if err := os.Mkdir(filepath.Join(parentPath, name), 0o700); err != nil {
		t.Fatal(err)
	}
	expected, _, err := store.EntryIdentityAt(int(parent.Fd()), name)
	if err != nil {
		t.Fatal(err)
	}
	reuse, err := testenv.ReplaceOnSameInode(parentPath, name, true)
	if err != nil {
		t.Fatal(err)
	}
	planted, _, err := store.EntryIdentityAt(int(parent.Fd()), name)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(parentPath, name, "foreign")
	if err := os.WriteFile(marker, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cleanupCreatedScratch(parent, parentPath, name, expected, nil); err == nil {
		t.Fatal("cleanup must not remove a directory planted at the scratch name")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "foreign" {
		t.Fatalf("the planted directory must be restored to its name intact: %q, %v", got, err)
	}
	requireReusedWithNewHandle(t, expected, planted, reuse)
}
