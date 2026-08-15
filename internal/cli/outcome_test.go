package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakeAddSession struct {
	outcome      engine.AddOutcome
	err          error
	candidates   []engine.Candidate
	invalid      map[string]error
	noCandidates error
	noSelection  error
	prologue     engine.Result
}

func (f *fakeAddSession) Candidates() []engine.Candidate {
	if f.candidates != nil {
		return f.candidates
	}
	return []engine.Candidate{{Name: "alpha", Description: "d", Subdir: ".", Digest: "digest"}}
}
func (f *fakeAddSession) Invalid() map[string]error { return f.invalid }
func (f *fakeAddSession) SourceArg() string         { return "SRC" }
func (f *fakeAddSession) NoCandidates() error       { return f.noCandidates }
func (f *fakeAddSession) NoSelection() error        { return f.noSelection }
func (f *fakeAddSession) Prologue() engine.Result   { return f.prologue }
func (f *fakeAddSession) Install([]engine.Candidate) (engine.AddOutcome, error) {
	return f.outcome, f.err
}
func (f *fakeAddSession) Close() error { return nil }

type fakeAddApplication struct {
	session  engine.AddSession
	prologue engine.Result
	err      error
}

func (f fakeAddApplication) PrepareAdd(string, string) (engine.AddPreparation, error) {
	return engine.AddPreparation{Session: f.session, Prologue: f.prologue}, f.err
}

type fakeAdoptApplication struct {
	outcome engine.AdoptResult
	err     error
}

func (f fakeAdoptApplication) Adopt(engine.AdoptScope) (engine.AdoptResult, error) {
	return f.outcome, f.err
}

type fakeRemoveApplication struct {
	outcome engine.RemoveOutcome
	err     error
}

type fakeNewApplication struct {
	outcome engine.OperationOutcome
	err     error
}

func (f fakeNewApplication) NewSkill(string) (engine.OperationOutcome, error) {
	return f.outcome, f.err
}

type fakeToggleApplication struct {
	outcome engine.ToggleOutcome
	err     error
}

func (f fakeToggleApplication) SetGlobal(string, bool) (engine.ToggleOutcome, error) {
	return f.outcome, f.err
}

func (f fakeToggleApplication) SetAgent(string, string, bool) (engine.ToggleOutcome, error) {
	return f.outcome, f.err
}

func (f fakeRemoveApplication) RemoveSkill(string) (engine.RemoveOutcome, error) {
	return f.outcome, f.err
}

func TestRenderDurableOutcomeAfterCommitInterruption(t *testing.T) {
	output := renderOutcomeForTest(t, engine.OperationOutcome{
		Name:            "alpha",
		Committed:       true,
		RecoveryPending: true,
	})
	for _, want := range []string{"remove alpha committed", "post-commit work", "recovery pending"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q missing %q", output, want)
		}
	}
}

func TestRenderDurableOutcomeAtWALCompletionFailure(t *testing.T) {
	output := renderOutcomeForTest(t, engine.OperationOutcome{
		Name:               "alpha",
		Committed:          true,
		PostCommitComplete: true,
		RecoveryPending:    true,
	})
	if !strings.Contains(output, "WAL completion") || !strings.Contains(output, "recovery pending") {
		t.Fatalf("output must identify the incomplete WAL phase: %q", output)
	}
}

func TestAddCommandRendersCommittedAfterCommitFailure(t *testing.T) {
	interrupted := errors.New("afterCommit failed")
	operation := engine.OperationOutcome{Name: "alpha", Committed: true, RecoveryPending: true}
	cmd := newAddCmd(fakeAddApplication{session: &fakeAddSession{
		outcome: engine.AddOutcome{Operations: []engine.OperationOutcome{operation}},
		err:     interrupted,
	}})
	output, err := executeCommandForOutcomeTest(cmd, "source")
	if !errors.Is(err, interrupted) {
		t.Fatalf("error = %v, want %v", err, interrupted)
	}
	if strings.Contains(output, "added alpha") {
		t.Fatalf("rollback-pending install must not be reported as added: %q", output)
	}
	for _, want := range []string{"add alpha committed", "will roll back"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q missing %q", output, want)
		}
	}
}

func TestSingleObjectCommandsReportEitherDurableBoundary(t *testing.T) {
	failure := errors.New("durable boundary failure")
	tests := []struct {
		name string
		cmd  *cobra.Command
		args []string
		want string
	}{
		{
			name: "new after post-commit",
			cmd: newNewCmd(fakeNewApplication{outcome: engine.OperationOutcome{
				Name: "alpha", PostCommitComplete: true,
			}, err: failure}),
			args: []string{"alpha"}, want: "created alpha",
		},
		{
			name: "rm after post-commit",
			cmd: newRmCmd(fakeRemoveApplication{outcome: engine.RemoveOutcome{Operation: engine.OperationOutcome{
				Name: "alpha", PostCommitComplete: true,
			}}, err: failure}),
			args: []string{"alpha"}, want: "removed alpha",
		},
		{
			name: "toggle after commit write",
			cmd: newToggleCmd(fakeToggleApplication{outcome: engine.ToggleOutcome{Operation: engine.OperationOutcome{
				Name: "alpha", Committed: true,
			}}, err: failure}, "enable", true),
			args: []string{"alpha"}, want: "enabled alpha globally",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output, err := executeCommandForOutcomeTest(tc.cmd, tc.args...)
			if !errors.Is(err, failure) {
				t.Fatalf("error = %v, want %v", err, failure)
			}
			if !strings.Contains(output, tc.want) {
				t.Fatalf("durable outcome %q missing %q", output, tc.want)
			}
		})
	}
}

func TestAdoptCommandRendersPreflightConflictSeparately(t *testing.T) {
	cmd := newAdoptCmd(fakeAdoptApplication{outcome: engine.AdoptResult{
		PreflightConflicts: []engine.FailedAction{{Action: engine.Action{Skill: "alpha"}, Err: errors.New("entry changed during capture")}},
	}})
	output, err := executeCommandForOutcomeTest(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "conflict: alpha") || strings.Contains(output, "invalid: alpha") {
		t.Fatalf("capture race must render as conflict, not invalid content: %q", output)
	}
}

func TestPrintResultRendersReservedReport(t *testing.T) {
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetErr(&output)
	printResult(cmd, engine.Result{Reserved: []engine.Action{{AgentName: "codex", Skill: ".system"}}})
	if !strings.Contains(output.String(), "reserved: codex/.system") {
		t.Fatalf("reserved report was not rendered: %q", output.String())
	}
}

func TestPrintResultRendersCandidateFailureWithoutAgentSeparator(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	printResult(cmd, engine.Result{Failed: []engine.FailedAction{{
		Action: engine.Action{Skill: "beta"},
		Err:    errors.New("candidate failed"),
	}}})
	if got := out.String(); !strings.Contains(got, "failed: beta: candidate failed") || strings.Contains(got, "failed: /beta") {
		t.Fatalf("candidate failure rendering = %q", got)
	}
}

func TestAdoptCommandRendersCommittedWALCompletionFailure(t *testing.T) {
	walFailure := errors.New("WAL completion failed")
	operation := engine.OperationOutcome{
		Name: "alpha", Committed: true, PostCommitComplete: true, RecoveryPending: true,
	}
	cmd := newAdoptCmd(fakeAdoptApplication{
		outcome: engine.AdoptResult{Adopted: []engine.AdoptSummary{{Name: "alpha", Agents: []string{"claude"}, Operation: operation}}},
		err:     walFailure,
	})
	output, err := executeCommandForOutcomeTest(cmd)
	if !errors.Is(err, walFailure) {
		t.Fatalf("error = %v, want %v", err, walFailure)
	}
	for _, want := range []string{"adopted alpha", "WAL completion", "recovery pending"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q missing %q", output, want)
		}
	}
}

func TestRemoveCommandRendersWrittenCommitErrorAsDurable(t *testing.T) {
	verificationFailure := errors.New("commit written plus verification error")
	operation := engine.OperationOutcome{Name: "alpha", Committed: true, RecoveryPending: true}
	cmd := newRmCmd(fakeRemoveApplication{
		outcome: engine.RemoveOutcome{Name: "alpha", Operation: operation},
		err:     verificationFailure,
	})
	output, err := executeCommandForOutcomeTest(cmd, "alpha")
	if !errors.Is(err, verificationFailure) {
		t.Fatalf("error = %v, want %v", err, verificationFailure)
	}
	for _, want := range []string{"removed alpha", "remove alpha committed", "recovery pending"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q missing %q", output, want)
		}
	}
}

func executeCommandForOutcomeTest(cmd *cobra.Command, args ...string) (string, error) {
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	if args == nil {
		args = []string{}
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	return output.String(), err
}

func TestExecuteCommandForOutcomeTestUsesExplicitEmptyArgs(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"cli.test", "ambient"}
	var got []string
	cmd := &cobra.Command{Use: "test", Run: func(_ *cobra.Command, args []string) { got = args }}

	if _, err := executeCommandForOutcomeTest(cmd); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("helper inherited process arguments: %v", got)
	}
}

func renderOutcomeForTest(t *testing.T, outcome engine.OperationOutcome) string {
	t.Helper()
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetErr(&output)
	printDurableOutcome(cmd, "remove", outcome)
	return output.String()
}

func TestRenderDurableOutcomeIgnoresUncommittedOperation(t *testing.T) {
	output := renderOutcomeForTest(t, engine.OperationOutcome{Name: "alpha"})
	if output != "" {
		t.Fatalf("an uncommitted operation has no durable outcome to warn about: %q", output)
	}
}

// TestRenderNonTransactionCommitFailureNamesTheCommit pins round 18 finding
// I4. enable/disable carry no transaction record, so a written-but-failed
// commit yields Committed=true, RecoveryPending=false, PostCommitComplete=false.
// Branching on RecoveryPending sent that state past both post-commit cases and
// into the canonical-path case, which reported "canonical-path verification did
// not complete" -- a phase that was never reached, for a failure that was the
// commit itself.
func TestRenderNonTransactionCommitFailureNamesTheCommit(t *testing.T) {
	output := renderOutcomeForTest(t, engine.OperationOutcome{
		Name:      "alpha",
		Committed: true,
	})
	if strings.Contains(output, "canonical-path") {
		t.Fatalf("a commit that never reached the canonical-path check must not blame it: %q", output)
	}
	if !strings.Contains(output, "post-commit") {
		t.Fatalf("output must name the earliest incomplete phase: %q", output)
	}
	if strings.Contains(output, "recovery pending") {
		t.Fatalf("an operation with no transaction record has nothing pending: %q", output)
	}
}

func TestRenderDurableOutcomeDoesNotCallNoOpCommitted(t *testing.T) {
	output := renderOutcomeForTest(t, engine.OperationOutcome{
		Name:               "alpha",
		PostCommitComplete: true,
		WALComplete:        true,
	})
	if strings.Contains(output, "alpha committed") {
		t.Fatalf("no-op outcome must not claim a commit: %q", output)
	}
	if !strings.Contains(output, "without a commit") {
		t.Fatalf("no-op incomplete outcome must state that no commit was made: %q", output)
	}
}

// TestRenderReconcileIncompleteWarning covers the branch round 18 finding I3
// made unreachable: ReconcileComplete was set before its error was examined,
// so CanonicalChecked && !ReconcileComplete could never occur and this arm of
// printDurableOutcome was dead code.
func TestRenderReconcileIncompleteWarning(t *testing.T) {
	output := renderOutcomeForTest(t, engine.OperationOutcome{
		Name:               "alpha",
		Committed:          true,
		PostCommitComplete: true,
		WALComplete:        true,
		CanonicalChecked:   true,
	})
	if !strings.Contains(output, "agent reconciliation did not complete") {
		t.Fatalf("output must report the incomplete reconcile phase: %q", output)
	}
}
