package engine

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/store"
	"github.com/cosensexyz/fu/internal/testenv"
)

func requireReusedWithNewHandle(t *testing.T, before, after store.FileIdentity, reuse testenv.InodeReuse) {
	t.Helper()
	if !testenv.FileHandlesRequired() {
		return
	}
	if !reuse.Reused || before.Device != after.Device || before.Inode != after.Inode || before.Handle == "" || after.Handle == "" || before.Handle == after.Handle {
		t.Fatalf("replacement coverage did not reuse the inode with a new handle: reuse=%+v before=%+v after=%+v", reuse, before, after)
	}
}

// The skills directory replaced on the same inode between the scan and the
// apply must be refused by OpenCheckedDir: the scan's identity carries the
// handle, and the replacement's handle differs.
func TestOpenCheckedDirRefusesASameInodeReplacement(t *testing.T) {
	base := t.TempDir()
	skipUnlessReplacementDetectable(t, base)
	skillsDir := filepath.Join(base, "cfgroot", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	storeSkills := filepath.Join(base, "store-skills")
	if err := os.MkdirAll(storeSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := ScanAgent(fakeAgent{"claude", skillsDir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := store.EntryIdentityAt(unix.AT_FDCWD, skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	reuse, err := testenv.ReplaceOnSameInode(filepath.Dir(skillsDir), "skills", true)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := store.EntryIdentityAt(unix.AT_FDCWD, skillsDir)
	if err != nil {
		t.Fatal(err)
	}

	if dir, err := st.OpenCheckedDir(); err == nil {
		_ = dir.Close()
		t.Fatal("a skills directory replaced since the scan must be refused")
	}
	requireReusedWithNewHandle(t, before, after, reuse)
}

// The directory fu just created, replaced on the same inode before the
// verification scan, must be refused.
func TestCreateAndScanAgentDirRefusesASameInodeReplacement(t *testing.T) {
	base := t.TempDir()
	skipUnlessReplacementDetectable(t, base)
	agentParent := filepath.Join(base, "cfgroot")
	agentSkills := filepath.Join(agentParent, "skills")
	storeSkills := filepath.Join(base, "store-skills")
	if err := os.MkdirAll(storeSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	var before, after store.FileIdentity
	var reuse testenv.InodeReuse
	_, err := createAndScanAgentDir(fakeAgent{"claude", agentSkills}, storeSkills, func() {
		var err error
		if before, _, err = store.EntryIdentityAt(unix.AT_FDCWD, agentSkills); err != nil {
			t.Fatal(err)
		}
		if reuse, err = testenv.ReplaceOnSameInode(agentParent, "skills", true); err != nil {
			t.Fatal(err)
		}
		if after, _, err = store.EntryIdentityAt(unix.AT_FDCWD, agentSkills); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("a directory replaced before the verification scan must be refused")
	}
	requireReusedWithNewHandle(t, before, after, reuse)
}

// A symlink planted at the retired name on the same inode with the very same
// target is not the link fu approved: it must be reported as a conflict and
// left in place, never unlinked.
func TestRetireFuLinkRefusesASameInodeSymlinkAtTheRetiredName(t *testing.T) {
	base := t.TempDir()
	skipUnlessReplacementDetectable(t, base)
	skillsDir := filepath.Join(base, "cfgroot", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	storeSkills := filepath.Join(base, "store", "skills")
	if err := os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(storeSkills, "alpha")
	if err := os.Symlink(target, filepath.Join(skillsDir, "alpha")); err != nil {
		t.Fatal(err)
	}
	st, err := ScanAgent(fakeAgent{"claude", skillsDir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	root, err := st.OpenCheckedDir()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var before, after store.FileIdentity
	var reuse testenv.InodeReuse
	outcome, _, err := retireFuLink(root, "alpha", storeSkills, nil, func(retired string) {
		var err error
		if before, _, err = store.EntryIdentityAt(int(root.file.Fd()), retired); err != nil {
			t.Fatal(err)
		}
		if reuse, err = testenv.ReplaceOnSameInodeWith(skillsDir, retired, testenv.Replacement{SymlinkTarget: target}); err != nil {
			t.Fatal(err)
		}
		if after, _, err = store.EntryIdentityAt(int(root.file.Fd()), retired); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != linkRetireConflict {
		t.Fatalf("outcome = %v, want a conflict: the object at the retired name is not the approved link", outcome)
	}
	// Restored to its live name, the planted link must survive.
	if _, err := os.Lstat(filepath.Join(skillsDir, "alpha")); err != nil {
		t.Fatalf("the planted link must not be unlinked: %v", err)
	}
	requireReusedWithNewHandle(t, before, after, reuse)
}

// The anchor renamed away between inspection and creation, with a new
// directory at its pathname: creation must go through the descriptor held
// since inspection and land in the moved original, never in the pathname's
// new occupant, and the verification rescan must then refuse -- whether the
// occupant is empty or already carries a skills directory of its own.
func TestCreateAndScanAgentDirRefusesAnAnchorRenamedAwayBeforeCreation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		occupantSkill bool
	}{
		{"empty occupant", false},
		{"occupant with its own skills directory", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			agentParent := filepath.Join(base, "cfgroot")
			if err := os.Mkdir(agentParent, 0o755); err != nil {
				t.Fatal(err)
			}
			agentSkills := filepath.Join(agentParent, "skills")
			storeSkills := filepath.Join(base, "store-skills")
			if err := os.MkdirAll(storeSkills, 0o755); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(base, "moved-away")
			_, err := createAndScanAgentDirWithHooks(fakeAgent{"claude", agentSkills}, storeSkills, func() {
				if err := os.Rename(agentParent, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(agentParent, 0o755); err != nil {
					t.Fatal(err)
				}
				if tc.occupantSkill {
					if err := os.Mkdir(agentSkills, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}, nil)
			if err == nil {
				t.Fatal("a pathname whose inspected directory was renamed away must be refused")
			}
			if _, err := os.Lstat(filepath.Join(moved, "skills")); err != nil {
				t.Fatalf("creation must have gone through the held descriptor into the moved original: %v", err)
			}
			if tc.occupantSkill {
				return
			}
			if _, err := os.Lstat(agentSkills); !os.IsNotExist(err) {
				t.Fatalf("nothing may be created in the pathname's new occupant, got %v", err)
			}
		})
	}
}
