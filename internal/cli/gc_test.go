package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakeGCApplication struct {
	outcome engine.PruneOutcome
	err     error
}

func (f fakeGCApplication) PruneRecovery() (engine.PruneOutcome, error) {
	return f.outcome, f.err
}

func TestGCCommandReportsPrunedTransactions(t *testing.T) {
	cmd := newGCCmd(fakeGCApplication{outcome: engine.PruneOutcome{Transactions: 2, Files: 17}})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pruned 2", "17 recovery journal and bookkeeping files"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("gc output %q lacks %q", output.String(), want)
		}
	}
}

// TestGCCommandDoesNotAttributeReclaimedFilesToTransactions pins that the
// summary stops presenting the file count as belonging to the transactions.
// Files now counts reclaimed bookkeeping alongside journal entries, so a run
// that swept only residue reports Transactions: 0 with a non-zero Files -- and
// "pruned 0 completed transactions (3 files)" asserted both halves of a
// contradiction in one line.
func TestGCCommandDoesNotAttributeReclaimedFilesToTransactions(t *testing.T) {
	cmd := newGCCmd(fakeGCApplication{outcome: engine.PruneOutcome{Files: 3}})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "transactions (3 files)") {
		t.Fatalf("the file count must not be attributed to the transactions: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "pruned 0 completed transactions") {
		t.Fatalf("a residue-only run must still report the transaction count: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "removed 3 recovery journal and bookkeeping files") {
		t.Fatalf("a residue-only run must report what it did remove: %q", stdout.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("a run that removed something is not a no-op: %q", got)
	}
}

func TestGCCommandReportsNothingToPrune(t *testing.T) {
	cmd := newGCCmd(fakeGCApplication{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("empty gc stdout = %q, want no result output", got)
	}
	if got := stderr.String(); got != "nothing to prune\n" {
		t.Fatalf("empty gc stderr = %q, want explicit no-op diagnostic", got)
	}
}

func TestGCCommandDoesNotClaimSuccessOnFailure(t *testing.T) {
	failure := errors.New("prune failed")
	cmd := newGCCmd(fakeGCApplication{err: failure})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); !errors.Is(err, failure) {
		t.Fatalf("gc error = %v, want %v", err, failure)
	}
	if strings.Contains(output.String(), "pruned ") {
		t.Fatalf("failed gc must not print success: %q", output.String())
	}
}

func TestGCCommandReportsHealthyPrunesBeforeReturningDamagedFamilyError(t *testing.T) {
	want := errors.New("damaged transaction family")
	app := &fakeGCApplication{outcome: engine.PruneOutcome{Transactions: 2, Files: 7}, err: want}
	cmd := newGCCmd(app)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if !errors.Is(err, want) {
		t.Fatalf("gc error = %v, want %v", err, want)
	}
	if got := stdout.String(); !strings.Contains(got, "pruned 2 completed transactions; removed 7 recovery journal and bookkeeping files") {
		t.Fatalf("gc did not report healthy work before error: %q", got)
	}
}

// TestRecoveryDoesNotGrowWithWriteCommandCount is this batch's acceptance
// measurement, and the only test that states the property the whole batch
// exists for: what recovery/ holds must be bounded by what is still pending,
// not by how many write commands have ever run. Before the batch every write
// command left exactly three permanent entries behind -- a config exchange
// record, its terminal marker, and the archived fu.yaml inode -- so this
// scenario measured 15 entries after 5 write commands and 75 after 25, with
// recovery/ at 300K against a 164K store holding one trivial skill. `fu gc`
// could not help: it pruned journals only, and by the time it ran a completed
// toggle's journal was the one thing already gone.
//
// The two rounds are compared rather than checked against a constant so that
// the assertion is about growth rather than about today's steady-state size:
// ten further rounds are five times the writes of the first two, so any
// per-command residue that survives gc shows up as a larger count.
//
// Each round drives both kinds of bookkeeping this batch made collectable, so
// that the name's general claim is the one actually measured. A toggle rewrites
// fu.yaml and nothing else, so on its own it pins only the config exchange
// line: with rm payload reclamation removed, a toggle-only round still settles
// at the same count and leaves this test green. A `new` + `rm` pair adds the
// other line -- the payload rm quarantines under recovery/ before it removes
// the skill -- and the pair's own two commits move HEAD every round, so the
// `removed-<name>-<StartHead>` names never collide and residue accumulates
// round over round instead of overwriting itself.
func TestRecoveryDoesNotGrowWithWriteCommandCount(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	if _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "new", "alpha"); err != nil {
		t.Fatal(err)
	}
	countAfter := func(rounds int) int {
		t.Helper()
		for range rounds {
			if out, err := runCmd(t, "disable", "alpha"); err != nil {
				t.Fatalf("disable: %v (%s)", err, out)
			}
			if out, err := runCmd(t, "enable", "alpha"); err != nil {
				t.Fatalf("enable: %v (%s)", err, out)
			}
			if out, err := runCmd(t, "new", "beta"); err != nil {
				t.Fatalf("new: %v (%s)", err, out)
			}
			if out, err := runCmd(t, "rm", "beta"); err != nil {
				t.Fatalf("rm: %v (%s)", err, out)
			}
		}
		if out, err := runCmd(t, "gc"); err != nil {
			t.Fatalf("gc: %v (%s)", err, out)
		}
		entries, err := os.ReadDir(filepath.Join(fuHome, "recovery"))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	few := countAfter(2)
	many := countAfter(10)
	if many > few {
		t.Fatalf("recovery/ grew with write count: %d entries after 2 rounds, %d after 10 more", few, many)
	}
}
