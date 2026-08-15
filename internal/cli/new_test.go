// internal/cli/new_test.go
package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

func TestNewCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FU_HOME", home)
	t.Setenv("HOME", t.TempDir()) // no agents detected; command still works
	runCmd(t, "init")
	if _, err := runCmd(t, "new", "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "store", "skills", "alpha", "SKILL.md")); err != nil {
		t.Fatal("skill not scaffolded via CLI")
	}
	if _, err := runCmd(t, "new", "alpha"); err == nil {
		t.Fatal("duplicate must fail")
	}
}

// Finding I3: a per-agent reconcile failure must not abort `fu new`
// outright -- the config entry and commit are already durable, and a
// healthy agent listed alongside the broken one must still be reconciled
// -- and must be surfaced through the CLI's shared result printer
// (root.go's printResult), not silently dropped.
//
// Round 2 finding 4 reversed this test's exit-code expectation. Before,
// the command returned no error here (exit 0) despite claude getting
// nothing at all: a script running `fu new alpha >/dev/null` saw a clean
// exit with claude silently unserved, because the failure diagnostic also
// went to stdout, right alongside it. The command must still confirm what
// *did* durably happen ("created alpha") and still surface the per-agent
// diagnostic, but the process must now report failure: Result.Failed is a
// genuine operation failure per finding 4's decision, not an expected,
// actionable state like Conflicts/Missing/Reserved/Invalid.
func TestNewCommandReportsPerAgentReconcileFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	// claude is "detected" (its marker directory exists) but its skills
	// dir is a plain file, not a directory, so ScanAgent must fail for it.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	out, err := runCmd(t, "new", "alpha")
	if !errors.Is(err, engine.ErrOperationFailed) {
		t.Fatalf("a per-agent reconcile failure must surface as engine.ErrOperationFailed so the process exits 1 (finding 4), got %v", err)
	}
	if !strings.Contains(out, "created alpha") {
		t.Fatalf("the config entry and commit are durable despite the reconcile-side failure; command must still confirm creation, got %q", out)
	}
	if !strings.Contains(out, "failed:") || !strings.Contains(out, "claude") {
		t.Fatalf("the per-agent failure must be surfaced by the result printer, got %q", out)
	}
}

func TestNewCommandSuppressesCreatedForRollbackPendingInstall(t *testing.T) {
	interrupted := errors.New("WAL completion failed")
	cmd := newNewCmd(fakeNewApplication{
		outcome: engine.OperationOutcome{
			Name:               "alpha",
			Committed:          true,
			PostCommitComplete: true,
			RecoveryPending:    true,
		},
		err: interrupted,
	})
	output, err := executeCommandForOutcomeTest(cmd, "alpha")
	if !errors.Is(err, interrupted) {
		t.Fatalf("new error = %v, want %v", err, interrupted)
	}
	if strings.Contains(output, "created alpha") {
		t.Fatalf("rollback-pending scaffold must not be reported as created: %q", output)
	}
	if !strings.Contains(output, "will roll back") {
		t.Fatalf("rollback-pending scaffold must explain recovery: %q", output)
	}
}

// Finding I6, end to end: with HOME unset, fu must never write into the
// process's current working directory. Reproduced against the compiled
// binary pre-fix: running `env -u HOME FU_HOME=... fu new alpha` from
// inside a project directory containing its own ./.claude created a
// real link at <cwd>/.claude/skills/alpha, treating the project's own
// Claude Code config as if it were a global agent installation.
func TestNewCommandWithHomeUnsetNeverWritesIntoCWD(t *testing.T) {
	fuHome := t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", "")

	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	if _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "new", "alpha"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(project, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatal("HOME unset must never cause fu to write into the project's own ./.claude")
	}
}
