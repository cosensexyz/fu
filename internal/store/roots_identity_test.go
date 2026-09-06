package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cosensexyz/fu/internal/testenv"
)

func TestPairPinnedRootRejectsAReplacementWhileTheOriginalIsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	original, err := dir.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// An unlinked directory remains allocated while this descriptor is open.
	// Recreating its name cannot reuse this live inode, including on ext4.
	for range 32 {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		replacement, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(original, replacement) {
			t.Fatal("replacement reused an inode still held by an open descriptor")
		}
	}
	root, err := pairPinnedRoot(dir, path)
	if root != nil {
		root.close()
	}
	if err == nil {
		t.Fatal("separately resolved replacement must not be paired with the original descriptor")
	}
	// Positive control: the same filesystem must recycle an unpinned inode
	// under the required Linux coverage setting, unlike the pinned one above.
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	// The failed pairing closed the old descriptor. Recreate once before
	// sampling so the allocator can select that newly released inode instead
	// of asking it to reuse the alternate inode from the pinned phase.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	originalIdentity, _, err := EntryIdentityAt(int(parent.Fd()), "root")
	if err != nil {
		t.Fatal(err)
	}
	replacementIdentity := captureSameNameReplacement(t, originalIdentity, func() FileIdentity {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		identity, _, err := EntryIdentityAt(int(parent.Fd()), "root")
		if err != nil {
			t.Fatal(err)
		}
		return identity
	})
	// Only required Linux coverage proves allocator reuse. An optional local
	// run still checks pinned-root rejection but need not observe inode reuse.
	if testenv.FileHandlesRequired() {
		if originalIdentity.Handle == "" || replacementIdentity.Handle == "" || originalIdentity.Handle == replacementIdentity.Handle {
			t.Fatal("positive control requires distinct real handles for the reused inode")
		}
		t.Logf("unpinned inode %d reused with distinct handles; the open descriptor prevented reuse", originalIdentity.Inode)
	}
}
