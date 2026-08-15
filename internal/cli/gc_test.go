package cli

import (
	"bytes"
	"errors"
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
	for _, want := range []string{"pruned 2", "17 files"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("gc output %q lacks %q", output.String(), want)
		}
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
	if got := stdout.String(); !strings.Contains(got, "pruned 2 completed transactions (7 files)") {
		t.Fatalf("gc did not report healthy work before error: %q", got)
	}
}
