// internal/cli/restore_test.go
package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
	"github.com/cosensexyz/fu/internal/store"
)

// fakeRestoreApplication needs a pointer receiver on Restore, not the value
// receiver a fake with no arguments to record could get away with: the hard
// flag it reaches must still be visible to the test's own *fakeRestoreApplication
// after cmd.Execute() returns, and a value-receiver method could only ever
// mutate its own copy.
type fakeRestoreApplication struct {
	outcome engine.RestoreOutcome
	err     error
	hard    bool
}

func (f *fakeRestoreApplication) Restore(hard bool) (engine.RestoreOutcome, error) {
	f.hard = hard
	return f.outcome, f.err
}

func TestRestoreCommandReportsSuccess(t *testing.T) {
	cmd := newRestoreCmd(&fakeRestoreApplication{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "restored agent links\n" {
		t.Fatalf("restore stdout = %q, want the confirmation line", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("a clean restore must print no diagnostics: %q", got)
	}
}

// TestRestoreCommandPrintsFindingsBeforeReturningError pins the requirement
// stated explicitly in this task's own instructions: printResult must run
// before RunE returns the Application's error, so reconcile findings reach
// the user even on a failing pass, not only on success.
func TestRestoreCommandPrintsFindingsBeforeReturningError(t *testing.T) {
	failure := errors.New("boom")
	outcome := engine.RestoreOutcome{Result: engine.Result{
		Failed: []engine.FailedAction{{Action: engine.Action{AgentName: "claude"}, Err: errors.New("scan failed")}},
	}}
	cmd := newRestoreCmd(&fakeRestoreApplication{outcome: outcome, err: failure})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); !errors.Is(err, failure) {
		t.Fatalf("restore error = %v, want %v", err, failure)
	}
	if !strings.Contains(stderr.String(), "failed: claude") {
		t.Fatalf("reconcile findings must reach stderr even when the pass fails: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "restored agent links") {
		t.Fatalf("a failing pass must not claim success: %q", stdout.String())
	}
}

func TestRestoreCommandDoesNotClaimSuccessOnApplicationError(t *testing.T) {
	failure := errors.New("store unavailable")
	cmd := newRestoreCmd(&fakeRestoreApplication{err: failure})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); !errors.Is(err, failure) {
		t.Fatalf("restore error = %v, want %v", err, failure)
	}
	if strings.Contains(output.String(), "restored agent links") {
		t.Fatalf("a failed restore must not print success: %q", output.String())
	}
}

// TestRestoreCommandReportsConflictsWithoutFailing mirrors
// TestExitCodeConflictDoesNotCauseOperationFailure (exitcode_test.go):
// Conflicts/Missing/Reserved/Invalid/Skipped are expected, actionable states,
// not operation failures (engine.ErrOperationFailed's doc comment) -- only a
// non-nil error withholds the confirmation.
func TestRestoreCommandReportsConflictsWithoutFailing(t *testing.T) {
	outcome := engine.RestoreOutcome{Result: engine.Result{
		Conflicts: []engine.Action{{AgentName: "claude", Skill: "alpha"}},
	}}
	cmd := newRestoreCmd(&fakeRestoreApplication{outcome: outcome})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("a reported conflict must not be an operation failure: %v", err)
	}
	if !strings.Contains(stderr.String(), "conflict: claude/alpha") {
		t.Fatalf("the conflict must still be reported: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "restored agent links") {
		t.Fatalf("a conflict alone must not withhold the confirmation: %q", stdout.String())
	}
}

// TestRestoreCommandReportsRefusedPaths covers the RunE branch for a restore
// blocked by uncommitted store content: every refused path must reach
// stderr, along with what the user can do instead -- record it with a write
// command (which commits pending hand edits first, SPEC §5.3) or discard the
// change with `fu restore --hard`. This test's outcome carries no Reset, the
// shape a fake Application produces when the caller ran plain `restore`
// (hard=false): the wording changed from "handle it directly with git" to
// naming `--hard` once this task gave that option a real implementation, so
// the refusal itself is now the one place a user who hits it learns the
// escape hatch exists. The link-layer confirmation must still print on
// stdout -- refusing to touch the store worktree must not read as refusing
// the whole command (mirrors TestRestoreCommandReportsConflictsWithoutFailing's
// principle for Reconcile's own findings).
func TestRestoreCommandReportsRefusedPaths(t *testing.T) {
	outcome := engine.RestoreOutcome{Refused: []string{"skills/alpha/SKILL.md", "skills/alpha/NOTES.md"}}
	cmd := newRestoreCmd(&fakeRestoreApplication{outcome: outcome})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "restored agent links\n" {
		t.Fatalf("a refusal must not withhold the link-layer confirmation: %q", got)
	}
	errOut := stderr.String()
	for _, want := range []string{
		"the store worktree was left alone; these changes are not committed:",
		"  skills/alpha/SKILL.md\n",
		"  skills/alpha/NOTES.md\n",
		"record them with a write command",
		"discard them with `fu restore --hard`",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("refusal stderr missing %q, got %q", want, errOut)
		}
	}
}

// TestRestoreCommandHardFlagReachesTheApplication pins the wiring and the
// wording. The reset line names how many paths moved, because a command that
// silently discarded work would give the user nothing to check.
func TestRestoreCommandHardFlagReachesTheApplication(t *testing.T) {
	app := &fakeRestoreApplication{outcome: engine.RestoreOutcome{Reset: []string{"skills/alpha/SKILL.md"}}}
	cmd := newRestoreCmd(app)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"--hard"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !app.hard {
		t.Fatal("--hard must reach the application")
	}
	out := stdout.String()
	for _, want := range []string{"reset 1 path", "skills/alpha/SKILL.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("restore output missing %q:\n%s", want, out)
		}
	}
}

// TestRestoreCommandRejectsPositionalArguments pins Args: usageArgs(cobra.NoArgs)
// at the exit-code boundary, the same discipline exitcode_test.go's
// TestExitCodeExcessArguments applies to show.
func TestRestoreCommandRejectsPositionalArguments(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "restore", "alpha")
	if code != 2 {
		t.Fatalf("a positional argument must be a usage error, got exit %d; output: %s", code, out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Fatalf("exit 2 must show usage: %q", out)
	}
}

// TestRestoreCommandIntegrationRebuildsLinkAndIsHistoryFree is the CLI-level
// counterpart of TestRestoreRebuildsADeletedLinkWithoutWritingHistory
// (internal/engine/restore_test.go), the same escalation status_test.go's own
// TestStatusCommandIntegrationReportsDriftAndIsReadOnly applies over the
// brief's engine-only test: it drives the real Application through
// NewRootCmd/runCmd rather than a fake, so it is the only test that exercises
// Application.Restore and the newRestoreCmd/root.go wiring against a real
// store rather than a hand-built RestoreOutcome.
func TestRestoreCommandIntegrationRebuildsLinkAndIsHistoryFree(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	mustMkdirAll(t, claudeDir)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	link := filepath.Join(claudeDir, "skills", "alpha")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "restore")
	if err != nil {
		t.Fatalf("restore must succeed rebuilding a deleted link: %v (%s)", err, out)
	}
	if !strings.Contains(out, "restored agent links") {
		t.Fatalf("restore output missing confirmation: %q", out)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("restore must rebuild the deleted link: %v", err)
	}

	after, err := st.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatalf("restore must not produce history: HEAD moved %s -> %s", before.Hash(), after.Hash())
	}
}

// TestRestoreCommandIntegrationSurfacesReconcileFailureAsExitOne mirrors
// exitcode_test.go's TestExitCodeReconcileFailedAgentIsOperationFailure for
// `new`, driven through `restore` instead: proves the production wiring this
// task adds (Application.Restore -> engine.Restore -> engine.Reconcile)
// propagates a genuine per-agent failure (engine.Result.Failed, not merely a
// Conflict/Missing/etc. finding) all the way to exit code 1, the same
// DESIGN §7 boundary every other write command is held to.
func TestRestoreCommandIntegrationSurfacesReconcileFailureAsExitOne(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := runExitCode(t, "init"); code != 0 {
		t.Fatalf("init: exit=%d out=%q", code, out)
	}
	if code, out := runExitCode(t, "new", "alpha"); code != 0 {
		t.Fatalf("new alpha: exit=%d out=%q", code, out)
	}
	// claude's skills dir becomes a plain file: ScanAgent fails for it with a
	// genuine (non-precondition) error, the same fixture
	// TestExitCodeReconcileFailedAgentIsOperationFailure uses for `new`.
	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.RemoveAll(skillsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := runExitCode(t, "restore")
	if code != 1 {
		t.Fatalf("a per-agent reconcile failure must exit 1, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "failed:") {
		t.Fatalf("the failure diagnostic must still be printed: %q", out)
	}
	if strings.Contains(out, "restored agent links") {
		t.Fatalf("a failing pass must not also claim success: %q", out)
	}
}

// TestRestoreCommandSeparatesUntrackedContentFromWhatHardCanDiscard pins the
// remedy split. Refused holds paths inside union(index, HEAD), which `--hard`
// resets; Left holds untracked and ignored paths, which it never touches.
// Offering `--hard` for the second group produced advice that could not be
// followed: the flag reset nothing, the file stayed, and the next `fu restore`
// printed the same suggestion again.
func TestRestoreCommandSeparatesUntrackedContentFromWhatHardCanDiscard(t *testing.T) {
	outcome := engine.RestoreOutcome{
		Refused: []string{"skills/alpha/SKILL.md"},
		Left:    []string{"skills/alpha/scratch.md"},
	}
	cmd := newRestoreCmd(&fakeRestoreApplication{outcome: outcome})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	errOut := stderr.String()
	hard := strings.Index(errOut, "`fu restore --hard`")
	scratch := strings.Index(errOut, "skills/alpha/scratch.md")
	if hard < 0 || scratch < 0 {
		t.Fatalf("both groups must be reported, got %q", errOut)
	}
	// The untracked path must be described after, and separately from, the
	// group --hard applies to -- never inside it.
	if scratch < hard {
		t.Fatalf("untracked content must not be listed under the --hard remedy:\n%s", errOut)
	}
	for _, want := range []string{
		"skills/alpha/SKILL.md",
		"record them with a write command",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr missing %q, got %q", want, errOut)
		}
	}
	// A section number is not something a user without the repository can look
	// up, and no other string in this CLI carries one.
	if strings.Contains(errOut, "SPEC §") {
		t.Fatalf("user-facing output must not cite SPEC section numbers: %q", errOut)
	}
}

// TestRestoreCommandHardAccountsForContentItDeliberatelyKept pins the silent
// case: `--hard` over a worktree dirty only with untracked or ignored content
// resets nothing, and used to print just "restored agent links" -- no answer
// at all for a user who had explicitly asked to discard.
func TestRestoreCommandHardAccountsForContentItDeliberatelyKept(t *testing.T) {
	outcome := engine.RestoreOutcome{Left: []string{"skills/alpha/scratch.md"}}
	cmd := newRestoreCmd(&fakeRestoreApplication{outcome: outcome})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--hard"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	errOut := stderr.String()
	for _, want := range []string{"skills/alpha/scratch.md", "record them with a write command"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("--hard must account for what it kept, missing %q in %q", want, errOut)
		}
	}
}

// TestRestoreCommandPrintsResetPathsBeforeReturningError is
// TestRestoreCommandPrintsFindingsBeforeReturningError's rule applied to the
// half that actually destroys something. engine.Restore fills Reset before it
// assigns the error, so the list exists; returning early threw it away, and a
// user whose reset succeeded but whose follow-up step failed was never told
// which of their edits had just been discarded.
func TestRestoreCommandPrintsResetPathsBeforeReturningError(t *testing.T) {
	failure := errors.New("another Git process holds .git/index.lock")
	outcome := engine.RestoreOutcome{Reset: []string{"skills/alpha/SKILL.md"}}
	cmd := newRestoreCmd(&fakeRestoreApplication{outcome: outcome, err: failure})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--hard"})
	if err := cmd.Execute(); !errors.Is(err, failure) {
		t.Fatalf("the error must still reach the caller, got %v", err)
	}
	if !strings.Contains(stdout.String(), "skills/alpha/SKILL.md") {
		t.Fatalf("discarded paths must be printed even when the run fails:\n%s", stdout.String())
	}
}

// TestRestoreHardFlagUsageIsNotReadAsAValuePlaceholder pins the help text.
// pflag's UnquoteUsage treats a backquoted span in a usage string as the
// flag's argument name, which overrides the bool special case: the backticks
// around `git reset --hard` made `fu restore --help` render the line as
// "--hard git reset --hard", implying the flag takes a value, and stripped the
// comparison out of the description at the same time.
func TestRestoreHardFlagUsageIsNotReadAsAValuePlaceholder(t *testing.T) {
	cmd := newRestoreCmd(&fakeRestoreApplication{})
	usage := cmd.Flags().FlagUsages()
	if strings.Contains(usage, "--hard git reset") {
		t.Fatalf("--hard must not render as taking a value:\n%s", usage)
	}
	if !strings.Contains(usage, "--hard ") && !strings.Contains(usage, "--hard\n") {
		t.Fatalf("--hard must still appear in the usage:\n%s", usage)
	}
}

// TestRestoreCommandReportsRefusedPathsWhenTheRunAlsoFails pairs the two
// things no other case in this file pairs: a non-nil error and a non-empty
// Refused.
//
// engine.Restore fills Refused and *then* returns errors.Join(reconcileErr,
// err) (restore.go), so "one agent failed and the store worktree is dirty" is
// an ordinary combination, not a contrived one. Printing Refused only in the
// success branch inverted the ordering the code above it argues for: the user
// was told the group `--hard` provably cannot help with, and not the group it
// can.
func TestRestoreCommandReportsRefusedPathsWhenTheRunAlsoFails(t *testing.T) {
	app := &fakeRestoreApplication{
		outcome: engine.RestoreOutcome{
			Refused: []string{"skills/alpha/SKILL.md"},
			Left:    []string{"skills/scratch.md"},
		},
		err: engine.ErrOperationFailed,
	}
	cmd := newRestoreCmd(app)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); !errors.Is(err, engine.ErrOperationFailed) {
		t.Fatalf("the failure must still reach the caller, got %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "skills/alpha/SKILL.md") || !strings.Contains(got, "fu restore --hard") {
		t.Fatalf("the resettable group and its remedy must be printed on the error path too:\n%s", got)
	}
	if !strings.Contains(got, "skills/scratch.md") {
		t.Fatalf("the untracked group must still be printed:\n%s", got)
	}
	// The actionable group first, the same order the success path uses.
	if strings.Index(got, "skills/alpha/SKILL.md") > strings.Index(got, "skills/scratch.md") {
		t.Fatalf("the group `--hard` can act on must come first:\n%s", got)
	}
}
