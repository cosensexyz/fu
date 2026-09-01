// internal/cli/update_acceptance_test.go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateNamesTheRepairPathForAnInvalidSkillName pins round 4's finding
// that `fu update` was the odd one out. A name LoadConfig excluded for failing
// validation is not "unknown" -- it is right there in fu.yaml, unreachable
// until the file is edited -- and both `fu show` and `fu rm` have always said
// so and named the file. `fu update` answered `unknown skill %q`, sending the
// user to look for a skill the config plainly shows.
//
// Driven through the compiled command against a real hand-edited fu.yaml,
// like its `fu list` / `fu show` sibling above (list_test.go), because the
// point is what a user meets, not what one function returns.
func TestUpdateNamesTheRepairPathForAnInvalidSkillName(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	if out, err := runCmd(t, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, out)
	}
	if out, err := runCmd(t, "new", "alpha"); err != nil {
		t.Fatalf("new: %v (%s)", err, out)
	}

	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "skills:\n  alpha:",
		"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: true\n  alpha:", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected seed content to edit")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "update", "Beta")
	if err == nil {
		t.Fatalf("updating a name fu.yaml holds but cannot manage must fail:\n%s", out)
	}
	for _, want := range []string{"fails validation", "fu.yaml"} {
		if !strings.Contains(out+err.Error(), want) {
			t.Fatalf("the refusal must name %q so the user can repair it: %v\n%s", want, err, out)
		}
	}
	if strings.Contains(out+err.Error(), `unknown skill "Beta"`) {
		t.Fatalf("a name present in fu.yaml is not unknown: %v\n%s", err, out)
	}
}

// TestUpdateDoesNotAccumulateResidue is this batch's acceptance measurement,
// in the shape TestRecoveryDoesNotGrowWithWriteCommandCount (gc_test.go)
// established: what staging/ and recovery/ hold after a round of updates must
// be bounded by what is pending, not by how many updates have ever run. Two
// rounds are compared against ten more so any per-update leak shows as growth
// rather than as a constant this test would have to be taught.
//
// The two directories are checked differently, and deliberately so:
//
//   - staging/ is asserted empty outright, every checkpoint. update's own
//     afterTxnCleared reclaim (reclaimExchangedUpdatePayload, update.go) runs
//     inline, synchronously, as part of the update command itself -- no `fu
//     gc` is needed to clear it in the ordinary, uncrashed path. This is the
//     half that fails if that reclaim is removed: the tree an update replaces
//     would then sit at staging/<name> forever, and the very next update of
//     the same skill would refuse outright ("staging already holds unmatched
//     content"), since checkUpdateAvailable (update.go) will not stage a
//     second tree on top of an unreclaimed one.
//   - recovery/ is never asserted empty: a completed transaction's own
//     journal family stays there until a separate `fu gc` prunes it, true of
//     every write command and not particular to update (one `fu new` alone
//     leaves nine entries). What is asserted instead is that every entry
//     found there is a txn- journal file: unlike `fu rm`, update quarantines
//     no content into recovery/ at all -- SPEC rule 3 leaves the tree it
//     replaces recoverable from the store's own git history instead -- so any
//     other name would mean update had started archiving something of its
//     own.
//
// Each round is also confirmed to have taken the content shape (the exchange)
// rather than the lock-only shape (a source digest advancing with the tree
// underneath it unchanged): a lock-only round touches nothing under staging/
// or recovery/ beyond its own config-only journal, so a suite that drifted
// into running lock-only rounds by accident would stay green whether or not
// the reclaim under test still worked -- exactly the failure mode this
// acceptance test exists to rule out for itself.
func TestUpdateDoesNotAccumulateResidue(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	if _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}

	srcDir := t.TempDir()
	writeSkill(t, srcDir, "kit")
	if out, err := runCmd(t, "add", srcDir); err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}

	edit := 0
	countAfter := func(rounds int) int {
		t.Helper()
		for range rounds {
			edit++
			body := fmt.Sprintf("---\nname: kit\ndescription: d\n---\n\nbody %d\n", edit)
			if err := os.WriteFile(filepath.Join(srcDir, "kit", "SKILL.md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := runCmd(t, "update", "kit")
			if err != nil {
				t.Fatalf("update: %v (%s)", err, out)
			}
			if !strings.Contains(out, "updated kit") || strings.Contains(out, "source lock") {
				t.Fatalf("round %d did not take the content shape, nothing to reclaim would be measured: %q", edit, out)
			}
			// The exchange's product-level promise, read the way an agent
			// reads it: through the symlink, not through a store path. Design
			// §4.2 chose the exchange over retire-then-publish precisely so an
			// agent starting a session in the window never sees a broken or
			// partial link, and every other assertion about that is made on
			// the store side. Symlinks resolve by path, so this holds by
			// construction today -- which is exactly why pinning it costs two
			// lines and would catch the day it stops holding.
			linked, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "kit", "SKILL.md"))
			if err != nil {
				t.Fatalf("round %d left the agent's link unreadable: %v", edit, err)
			}
			if string(linked) != body {
				t.Fatalf("round %d: the agent reads %q through its link, want the updated body %q", edit, linked, body)
			}
		}
		staging, err := os.ReadDir(filepath.Join(fuHome, "staging"))
		if err != nil {
			t.Fatal(err)
		}
		if len(staging) != 0 {
			t.Fatalf("staging/ must be empty after a settled update, holds %d entries", len(staging))
		}
		if out, err := runCmd(t, "gc"); err != nil {
			t.Fatalf("gc: %v (%s)", err, out)
		}
		recovery, err := os.ReadDir(filepath.Join(fuHome, "recovery"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range recovery {
			if !strings.HasPrefix(entry.Name(), "txn-") {
				t.Fatalf("update must archive nothing into recovery/, found %q", entry.Name())
			}
		}
		return len(staging) + len(recovery)
	}
	few := countAfter(2)
	many := countAfter(10)
	if many > few {
		t.Fatalf("staging/+recovery/ grew with update round count: %d entries after 2 rounds, %d after 10 more", few, many)
	}
}
