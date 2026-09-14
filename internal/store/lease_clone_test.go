package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
)

// A completed clone leaves staging empty: the scratch became the store, and
// its lease was released once it covered nothing.
func TestCloneLeavesNoLeaseBehind(t *testing.T) {
	home := t.TempDir()
	source, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "store.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Push(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := Clone(context.Background(), home, bare); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "staging"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a completed clone must leave staging empty, found %v", entries)
	}
}

// The state that had no remedy at all: a clone killed mid-transfer, in a home
// where no store exists yet, so nothing store-shaped could ever have recorded
// the leftover. The lease is what makes it reclaimable.
func TestInterruptedCloneLeavesReclaimableEvidence(t *testing.T) {
	if os.Getenv("FU_TEST_CLONE_ABANDON_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CLONE_ABANDON_HOME")
		url := os.Getenv("FU_TEST_CLONE_ABANDON_URL")
		_, _ = cloneWithHooks(context.Background(), home, url, cloneHooks{
			beforeRename: func() error {
				// Verified, not yet renamed into place: the window the
				// clone-scratch residue comes from.
				os.Exit(93)
				return nil
			},
		})
		panic("clone crash hook did not run")
	}

	home := t.TempDir()
	source, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "store.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Push(context.Background()); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptedCloneLeavesReclaimableEvidence$")
	cmd.Env = append(os.Environ(),
		"FU_TEST_CLONE_ABANDON_HELPER=1",
		"FU_TEST_CLONE_ABANDON_HOME="+home,
		"FU_TEST_CLONE_ABANDON_URL="+bare,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 93 {
		t.Fatalf("the helper must die mid-clone: err=%v output=%s", err, output)
	}

	staging := filepath.Join(home, "staging")
	if _, err := os.Lstat(filepath.Join(home, "store")); !os.IsNotExist(err) {
		t.Fatalf("precondition: an interrupted clone must leave no store: %v", err)
	}
	before, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("want the abandoned clone scratch and its lease, found %v", before)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 1 || outcome.Leases != 1 {
		t.Fatalf("outcome = %+v, want the clone scratch and its lease reclaimed", outcome)
	}
	after, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("staging must be empty after reclamation, still holds %v", after)
	}
}

// And at the clone's own unwinding site: when the removal of the scratch
// fails, the scratch is still there, so its lease has to stay with it.
//
// Every unwinding path in the branch asks "is the object actually gone?"
// before dropping the record, because dropping it beside a survivor
// manufactures the unaccounted residue the lease exists to prevent. Reaching
// the answer "no" here takes a scratch that refuses to be removed -- and the
// refusal has to be confined to the scratch. Making staging itself read-only
// stops the lease being unlinked too, so the record survives either way and
// the test cannot tell the two behaviours apart.
func TestCloneKeepsItsLeaseWhenTheScratchCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not refused the removal")
	}
	_, bare := pushedStore(t)
	home := t.TempDir()
	staging := filepath.Join(home, "staging")
	boom := errors.New("boom")

	var locked string
	t.Cleanup(func() {
		if locked != "" {
			_ = os.Chmod(locked, 0o700)
		}
	})
	_, err := cloneWithHooks(context.Background(), home, bare, cloneHooks{
		beforeRename: func() error {
			entries, err := os.ReadDir(staging)
			if err != nil {
				return err
			}
			var scratch string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".fu-clone-") {
					scratch = filepath.Join(staging, entry.Name())
				}
			}
			if scratch == "" {
				return errors.New("no clone scratch to make undeletable")
			}
			locked = filepath.Join(scratch, "locked")
			if err := os.Mkdir(locked, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(locked, "stuck"), []byte("x"), 0o600); err != nil {
				return err
			}
			// Nothing inside can be unlinked, so RemoveAll cannot finish --
			// while staging itself stays writable, so the lease could be
			// removed if the code chose to.
			if err := os.Chmod(locked, 0o500); err != nil {
				return err
			}
			return boom
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the hook's error must surface, got %v", err)
	}

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Name == "" {
		t.Fatalf("findings = %+v, want the surviving clone scratch named by its own lease", findings)
	}
	if _, err := os.Lstat(filepath.Join(staging, findings[0].Name)); err != nil {
		t.Fatalf("the object %q the lease accounts for is not there: %v", findings[0].Name, err)
	}
}
