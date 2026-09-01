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

type fakeUpdateApp struct {
	gotName  string
	gotForce bool
	outcome  engine.UpdateBatchOutcome
	err      error
}

func (f *fakeUpdateApp) UpdateSkills(name string, force bool) (engine.UpdateBatchOutcome, error) {
	f.gotName, f.gotForce = name, force
	return f.outcome, f.err
}

func runUpdate(t *testing.T, app updateApplication, args ...string) (string, error) {
	t.Helper()
	cmd := newUpdateCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// --force is only meaningful against a named skill: a batch force would
// overwrite several skills' hand edits with nothing on the command line saying
// which (spec §2.4).
func TestUpdateCommandRejectsForceWithoutASkillName(t *testing.T) {
	app := &fakeUpdateApp{}
	_, err := runUpdate(t, app, "--force")
	if err == nil {
		t.Fatal("expected a usage error")
	}
	var uerr *UsageError
	if !errors.As(err, &uerr) {
		t.Fatalf("error = %v, want a UsageError so the exit code is 2", err)
	}
	if app.gotName != "" || app.gotForce {
		t.Fatal("a usage error must not reach the application")
	}
}

func TestUpdateCommandPassesForceForANamedSkill(t *testing.T) {
	app := &fakeUpdateApp{outcome: engine.UpdateBatchOutcome{Updated: []string{"kit"}}}
	if _, err := runUpdate(t, app, "kit", "--force"); err != nil {
		t.Fatal(err)
	}
	if app.gotName != "kit" || !app.gotForce {
		t.Fatalf("name = %q force = %v, want kit true", app.gotName, app.gotForce)
	}
}

// A batch that skips locally modified skills must list them and point at the
// per-skill escape hatch.
func TestUpdateCommandListsSkippedSkillsAndNamesTheWayOut(t *testing.T) {
	app := &fakeUpdateApp{outcome: engine.UpdateBatchOutcome{
		Updated: []string{"pdf-tools"},
		Skipped: []engine.UpdateSkip{{Name: "api-docs", Reason: "locally modified"}},
	}}
	out, err := runUpdate(t, app)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "api-docs") || !strings.Contains(out, "locally modified") {
		t.Fatalf("output must name the skipped skill and why:\n%s", out)
	}
	if !strings.Contains(out, "fu update api-docs --force") {
		t.Fatalf("output must name the way out:\n%s", out)
	}
}

func TestUpdateCommandReportsLockOnlyUpdatesDistinctly(t *testing.T) {
	app := &fakeUpdateApp{outcome: engine.UpdateBatchOutcome{LockOnly: []string{"kit"}}}
	out, err := runUpdate(t, app)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kit") {
		t.Fatalf("output must name the skill:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "lock") {
		t.Fatalf("a lock-only advance must be distinguishable from a content update:\n%s", out)
	}
}

// TestUpdateCommandRejectsForceWithAnExplicitEmptyName pins fix round 1's
// minor finding: `fu update "" --force` passes len(args)==1, so a guard
// keyed on len(args)==0 alone would let it slip through to the batch case
// with force silently ignored. The guard must instead key on the resolved
// name being empty.
func TestUpdateCommandRejectsForceWithAnExplicitEmptyName(t *testing.T) {
	app := &fakeUpdateApp{}
	_, err := runUpdate(t, app, "", "--force")
	if err == nil {
		t.Fatal("expected a usage error")
	}
	var uerr *UsageError
	if !errors.As(err, &uerr) {
		t.Fatalf("error = %v, want a UsageError so the exit code is 2", err)
	}
	if app.gotName != "" || app.gotForce {
		t.Fatal("a usage error must not reach the application")
	}
}

// TestUpdateCommandRejectsAnExplicitlyEmptySkillName pins round 4's finding
// that `fu update ""` was not merely useless but actively wrong: an explicit
// empty positional resolves to name == "", which the application treats as
// "every skill", so a command line naming one skill silently escalated into a
// store-wide write. `fu show ""` answers `unknown skill ""`; this is the same
// class of input and owes the same refusal. The --force guard beside it was
// already written to catch `fu update "" --force`, so the empty positional had
// been thought about -- just not without the flag.
func TestUpdateCommandRejectsAnExplicitlyEmptySkillName(t *testing.T) {
	app := &fakeUpdateApp{}
	out, err := runUpdate(t, app, "")
	if err == nil {
		t.Fatalf("expected a usage error, output: %s", out)
	}
	var uerr *UsageError
	if !errors.As(err, &uerr) {
		t.Fatalf("error = %v, want a UsageError so the exit code is 2", err)
	}
	if app.gotName != "" || app.gotForce {
		t.Fatal("a usage error must not reach the application")
	}
	// The message has to name both real forms, because the user meant one of
	// them and the empty string is neither.
	for _, want := range []string{"fu update <name>", "every updatable skill"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name %q: %v", want, err)
		}
	}
}

// TestUpdateCommandPrintsConfigDiagnostics pins round 4's finding that
// `fu update` was the one command printing neither half of ReadDiagnostics.
// The store that exposed it is the ordinary one: nothing to update, so
// UpdateSkills returns before any transaction runs and the reconcile channel
// -- which carries the `invalid:` line when one does -- is never reached. The
// command said "nothing to update" and exited 0 over a fu.yaml this build may
// not understand and over names it had silently excluded from the report,
// while `fu outdated` on the same store warned about both.
func TestUpdateCommandPrintsConfigDiagnostics(t *testing.T) {
	app := &fakeUpdateApp{outcome: engine.UpdateBatchOutcome{
		Diagnostics: engine.ReadDiagnostics{
			ConfigPath:    "/somewhere/fu.yaml",
			VersionTooNew: true,
			InvalidNames:  []engine.InvalidConfigName{{Name: "Bad Name", Reason: "invalid characters"}},
		},
	}}
	out, err := runUpdate(t, app)
	if err != nil {
		t.Fatalf("diagnostics are not a failure: %v", err)
	}
	for _, want := range []string{
		"version newer than this build supports",
		`invalid: skill name "Bad Name"`,
		"/somewhere/fu.yaml",
		"nothing to update",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output must contain %q:\n%s", want, out)
		}
	}
}

// TestUpdateCommandReportsReconcileConflictOnAnUnrelatedSkill pins fix round
// 1 Important finding 1: `fu update` is the only write command that never
// called printResult, so a trailing reconcile finding -- even one about a
// completely unrelated skill, since reconcileChecked reconciles the whole
// config on every write command (pipeline.go:464) -- had nowhere to surface.
// "other" is never touched by this update at all; its link is torn out and
// replaced with unmanaged content before the run, the same technique
// TestAddCommandReportsReconcileConflict (add_test.go) uses to pin the same
// class of finding for `fu add`.
func TestUpdateCommandReportsReconcileConflictOnAnUnrelatedSkill(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	otherSrc := t.TempDir()
	writeSkill(t, otherSrc, "other")
	if _, err := runCmd(t, "add", otherSrc); err != nil {
		t.Fatal(err)
	}
	otherLink := filepath.Join(home, ".claude", "skills", "other")
	if err := os.Remove(otherLink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherLink, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherLink, "README"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "kit" is the skill actually being updated; its own source content
	// moves so there is something for `fu update kit` to pull.
	kitSrc := t.TempDir()
	writeSkill(t, kitSrc, "kit")
	if _, err := runCmd(t, "add", kitSrc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kitSrc, "kit", "SKILL.md"),
		[]byte("---\nname: kit\ndescription: d\n---\n\nbody v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "update", "kit")
	if err != nil {
		t.Fatalf("update: %v (%s)", err, out)
	}
	if !strings.Contains(out, "updated kit") {
		t.Fatalf("output missing the update confirmation: %s", out)
	}
	if !strings.Contains(out, "conflict: claude/other occupied by unmanaged content") {
		t.Fatalf("output missing the unrelated skill's reconcile conflict:\n%s", out)
	}
}

// TestUpdateCommandSaysWhenThereWasNothingToUpdate pins fix round 2's Minor
// #2. With no updatable target, UpdateSkills returns early and every print
// loop here is empty, so the command used to exit 0 having written nothing at
// all -- indistinguishable from a command that never ran. `fu outdated` and
// `fu status` both state their clean result explicitly and say why in their
// own comments ("Silence would read the same as 'outdated never looked'");
// update was the one command in the trio that stayed silent.
func TestUpdateCommandSaysWhenThereWasNothingToUpdate(t *testing.T) {
	out, err := runUpdate(t, &fakeUpdateApp{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to update") {
		t.Fatalf("an update with no targets must say so rather than print nothing:\n%q", out)
	}
}

// The other half: the line is a statement about this run having found nothing
// to do, so it must not appear next to work that was actually reported.
func TestUpdateCommandOmitsTheNothingToUpdateLineWhenSomethingHappened(t *testing.T) {
	for _, tc := range []struct {
		label   string
		outcome engine.UpdateBatchOutcome
	}{
		{"updated", engine.UpdateBatchOutcome{Updated: []string{"kit"}}},
		{"lock only", engine.UpdateBatchOutcome{LockOnly: []string{"kit"}}},
		{"skipped", engine.UpdateBatchOutcome{Skipped: []engine.UpdateSkip{{Name: "kit", Reason: "locally modified"}}}},
		{"unattempted", engine.UpdateBatchOutcome{Unattempted: []string{"kit"}}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			out, err := runUpdate(t, &fakeUpdateApp{outcome: tc.outcome})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "nothing to update") {
				t.Fatalf("a run that reported %s must not also claim it had nothing to do:\n%q", tc.label, out)
			}
		})
	}
}

// A failed run must not claim it had nothing to do either: the outcome is
// empty because the command failed, not because the store was current.
func TestUpdateCommandStaysSilentAboutNothingToUpdateOnFailure(t *testing.T) {
	out, err := runUpdate(t, &fakeUpdateApp{err: errors.New("store unreadable")})
	if err == nil {
		t.Fatal("expected the application failure to reach the caller")
	}
	if strings.Contains(out, "nothing to update") {
		t.Fatalf("a failed run must not report a clean pass:\n%q", out)
	}
}

// TestUpdateCommandReportsRowsItCouldNotJudge is the CLI half of round 3's
// Important #8: the rows must reach the user, on stderr, without the --force
// hint a skip carries, and they must stop the "nothing to update" line from
// claiming a clean pass.
func TestUpdateCommandReportsRowsItCouldNotJudge(t *testing.T) {
	out, err := runUpdate(t, &fakeUpdateApp{outcome: engine.UpdateBatchOutcome{
		Unjudged: []engine.UpdateSkip{{Name: "delta", Reason: "unreachable: dial tcp: i/o timeout"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "could not judge delta: unreachable: dial tcp: i/o timeout") {
		t.Fatalf("an unjudged row must be reported with its reason:\n%s", out)
	}
	if strings.Contains(out, "nothing to update") {
		t.Fatalf("a run that could not judge a skill has not shown the store to be current:\n%s", out)
	}
	if strings.Contains(out, "--force") {
		t.Fatalf("--force does not help a row that could not be judged:\n%s", out)
	}
}

// The three reporting channels round 3's Important #10 found unguarded: the
// "not attempted" line after a batch abort, the durable-outcome warning, and
// the argument-count guard. All three mutations survived the whole package.

func TestUpdateCommandReportsUnattemptedSkillsAfterAnAbort(t *testing.T) {
	out, err := runUpdate(t, &fakeUpdateApp{outcome: engine.UpdateBatchOutcome{
		Updated:     []string{"a-first"},
		Unattempted: []string{"b-second", "c-third"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not attempted: b-second, c-third") {
		t.Fatalf("after an abort the user must be told which skills were never reached:\n%s", out)
	}
}

func TestUpdateCommandWarnsWhenAnOperationLeftRecoveryPending(t *testing.T) {
	out, err := runUpdate(t, &fakeUpdateApp{outcome: engine.UpdateBatchOutcome{
		Updated: []string{"kit"},
		Operations: []engine.OperationOutcome{{
			Name: "kit", Committed: true, PostCommitComplete: true, RecoveryPending: true,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "recovery") {
		t.Fatalf("a committed operation with recovery still pending must say so:\n%s", out)
	}
}

func TestUpdateCommandRejectsASecondPositionalArgument(t *testing.T) {
	app := &fakeUpdateApp{}
	_, err := runUpdate(t, app, "alpha", "beta")
	if err == nil {
		t.Fatal("expected a usage error")
	}
	var uerr *UsageError
	if !errors.As(err, &uerr) {
		t.Fatalf("error = %v, want a UsageError so the exit code is 2", err)
	}
	if app.gotName != "" {
		t.Fatal("a usage error must not reach the application")
	}
}
