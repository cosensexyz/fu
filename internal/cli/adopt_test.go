package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

// TestAdoptCommand adopts a pre-existing skill entry and switches it to a
// store link, leaving the agent's non-skill content alone.
func TestAdoptCommand(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, skillsDir, "pdf-tools")
	if err := os.WriteFile(filepath.Join(skillsDir, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "adopt")
	if err != nil {
		t.Fatalf("adopt: %v (%s)", err, out)
	}
	if !strings.Contains(out, "adopted pdf-tools (from claude)") {
		t.Fatalf("output missing adoption line: %s", out)
	}
	info, err := os.Lstat(filepath.Join(skillsDir, "pdf-tools"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("entry must become a store link: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "notes.txt")); err != nil {
		t.Fatalf("non-skill content touched: %v", err)
	}
	// The config records the adoption.
	cfg := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "pdf-tools") {
		t.Fatalf("adopted skill missing from fu.yaml:\n%s", raw)
	}
}

// TestAdoptRepeatedIsIdle pins that a second adopt run has nothing to do:
// everything is already a fu link.
func TestAdoptRepeatedIsIdle(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, skillsDir, "pdf-tools")
	if _, err := runCmd(t, "adopt"); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, "adopt")
	if err != nil {
		t.Fatalf("second adopt must not fail: %v (%s)", err, out)
	}
	if strings.Contains(out, "adopted") {
		t.Fatalf("second adopt must adopt nothing:\n%s", out)
	}
}

// TestAdoptCommandWarnsOnDisagreeingSymlinkTargets pins the CLI's warning
// line: two detected agents whose symlink entries point at different
// targets holding identical content are adopted, and the missing local
// source record is reported as "warning: ..." on stderr (cli/adopt.go:39-41).
func TestAdoptCommandWarnsOnDisagreeingSymlinkTargets(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	for _, d := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runCmd(t, "init")

	targetA, targetB := t.TempDir(), t.TempDir()
	writeSkill(t, targetA, "pdf-tools")
	writeSkill(t, targetB, "pdf-tools")
	for _, d := range []string{".claude", ".codex"} {
		skillsDir := filepath.Join(home, d, "skills")
		if err := os.MkdirAll(skillsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := targetA
		if d == ".codex" {
			target = targetB
		}
		if err := os.Symlink(filepath.Join(target, "pdf-tools"), filepath.Join(skillsDir, "pdf-tools")); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runCmd(t, "adopt")
	if err != nil {
		t.Fatalf("adopt: %v (%s)", err, out)
	}
	if !strings.Contains(out, "adopted pdf-tools (from claude, codex)") {
		t.Fatalf("output missing adoption line: %s", out)
	}
	if !strings.Contains(out, "warning: skill pdf-tools: symlink targets differ across agents") {
		t.Fatalf("output missing disagreeing-targets warning:\n%s", out)
	}
}

// TestAdoptCommandInvalidCandidateExitsZero pins round 7 finding M2: an
// adopt-level per-candidate failure (here: a tree containing a FIFO, which
// the digest projection refuses) is reported as "invalid:" -- the same
// class as add's invalid candidates -- and the command still exits 0,
// because the remaining candidates were adopted. Reconcile-level failures
// keep the "failed:" label and exit 1.
func TestAdoptCommandInvalidCandidateExitsZero(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, skillsDir, "pdf-tools")
	// A candidate whose tree contains a FIFO: the projection refuses it at
	// digest time, landing in res.Failed.
	if err := os.MkdirAll(filepath.Join(skillsDir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: broken\ndescription: d\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(skillsDir, "broken", "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(skillsDir, "broken", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "adopt")
	if err != nil {
		t.Fatalf("adopt with an invalid candidate must exit 0: %v (%s)", err, out)
	}
	if !strings.Contains(out, "adopted pdf-tools (from claude)") {
		t.Fatalf("output missing adoption line: %s", out)
	}
	if !strings.Contains(out, "invalid: broken:") {
		t.Fatalf("output missing invalid line for the FIFO candidate:\n%s", out)
	}
}

func TestAdoptPrintsDurableSuccessBeforeReconcileFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claudeSkills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(claudeSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, claudeSkills, "alpha")
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "skills"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	out, err := runCmd(t, "adopt")
	if !errors.Is(err, engine.ErrOperationFailed) {
		t.Fatalf("adopt error = %v, want ErrOperationFailed; output=%s", err, out)
	}
	if !strings.Contains(out, "adopted alpha (from claude)") {
		t.Fatalf("output must report the durable adoption before failure:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(fuHome, "store", "skills", "alpha", "SKILL.md")); err != nil {
		t.Fatalf("durable skill missing: %v", err)
	}
}

// TestAdoptCommandEmptyEnvironmentPinsNothingToAdopt pins round 8 finding
// M1: with nothing to adopt (an empty environment, or every entry already a
// fu link), the command must say so instead of silently succeeding.
func TestAdoptCommandEmptyEnvironmentPinsNothingToAdopt(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	out, err := runCmd(t, "adopt")
	if err != nil {
		t.Fatalf("adopt with nothing to do must exit 0: %v (%s)", err, out)
	}
	if !strings.Contains(out, "nothing to adopt") {
		t.Fatalf("output missing nothing-to-adopt hint: %q", out)
	}
}

func TestAdoptCommandRejectsExplicitEmptyAgentWithoutMutation(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, skillsDir, "alpha")
	runCmd(t, "init")

	out, err := runCmd(t, "adopt", "--agent", "")
	if err == nil || !strings.Contains(err.Error(), "agent scope cannot be empty") {
		t.Fatalf("explicit empty --agent must be rejected, err=%v output=%s", err, out)
	}
	info, statErr := os.Lstat(filepath.Join(skillsDir, "alpha"))
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("explicit empty scope must leave the candidate untouched: %v, %v", info, statErr)
	}
}

func TestAdoptCommandExplicitEmptyAgentIsUsageError(t *testing.T) {
	// Isolated like every sibling test. This was the one test in the file
	// without the pair, so it relied entirely on the guard it is testing to
	// keep `fu adopt` -- the most destructive command in the tool -- away from
	// the developer's real ~/.claude, ~/.codex and ~/.fu. A test whose safety
	// depends on the code under test turns a regression into data loss, and it
	// also weakened the assertion: exit 2 could have come from anywhere
	// (round 18 finding I15).
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	code, output := runExitCode(t, "adopt", "--agent", "")
	if code != 2 {
		t.Fatalf("explicit empty --agent must exit 2, got %d; output=%s", code, output)
	}
	if !strings.Contains(output, "agent scope cannot be empty") {
		t.Fatalf("the usage error must name the cause: %s", output)
	}
}

// TestAdoptCommandAgentScopeEndToEnd closes one of round 18 finding M25's
// gaps: a valid --agent scope was covered only in internal/engine, so nothing
// proved the CLI passes AdoptScope{Agent: x} through rather than silently
// adopting from every agent.
func TestAdoptCommandAgentScopeEndToEnd(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claudeSkills := filepath.Join(home, ".claude", "skills")
	codexSkills := filepath.Join(home, ".codex", "skills")
	for _, dir := range []string{claudeSkills, codexSkills} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runCmd(t, "init")
	writeSkill(t, claudeSkills, "alpha")
	writeSkill(t, codexSkills, "beta")

	stdout, stderr, err := runCmdSplit(t, "adopt", "--agent", "claude")
	if err != nil {
		t.Fatalf("scoped adopt: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "adopted alpha (from claude)") {
		t.Fatalf("the scoped agent's skill must be adopted and named:\n%s", stdout)
	}
	if strings.Contains(stdout, "beta") {
		t.Fatalf("the out-of-scope agent's skill must not be adopted:\n%s", stdout)
	}
	// codex's own copy is untouched and still a real directory.
	info, statErr := os.Lstat(filepath.Join(codexSkills, "beta"))
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("out-of-scope agent must stay untouched: %v %v", info, statErr)
	}
}

// TestAdoptCommandRendersIncompletePhase proves that newAdoptCmd routes an
// OperationOutcome carrying an incomplete phase through printDurableOutcome
// while still confirming the adopt on stdout.
//
// It does not prove the engine and the CLI renderer are wired together, and an
// earlier version of this test claimed it did. It is fake-driven, touches no
// store, and feeds ReconcileComplete=false -- a value run() cannot produce, so
// the vector itself is unreachable in production. That engine-to-CLI gap is
// still open: reaching it needs a phase the engine really can leave incomplete,
// and the only one available is the canonical-path failure, which needs
// FU_HOME's resolution to change mid-command and has no reliable test seam.
// Naming the test for what it does keeps that gap visible instead of recording
// it as closed.
func TestAdoptCommandRendersIncompletePhase(t *testing.T) {
	operation := engine.OperationOutcome{
		Name: "alpha", Committed: true, PostCommitComplete: true,
		WALComplete: true, CanonicalChecked: true, ReconcileComplete: false,
	}
	cmd := newAdoptCmd(fakeAdoptApplication{
		outcome: engine.AdoptResult{
			Adopted: []engine.AdoptSummary{{Name: "alpha", Agents: []string{"claude"}, Operation: operation}},
		},
	})
	stdout, stderr, err := executeSplitForTest(cmd)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !strings.Contains(stdout, "adopted alpha (from claude)") {
		t.Fatalf("the confirmation must still reach stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "agent reconciliation did not complete") {
		t.Fatalf("the engine's incomplete phase must reach the user: %q", stderr)
	}
}

func TestAdoptCommandRendersEveryResultBranch(t *testing.T) {
	tests := []struct {
		name    string
		outcome engine.AdoptResult
		want    string
	}{
		{
			name: "pending durable operation",
			outcome: engine.AdoptResult{Pending: []engine.AdoptSummary{{
				Name: "alpha",
				Operation: engine.OperationOutcome{
					Name: "alpha", Committed: true, RecoveryPending: true,
				},
			}}},
			want: "adopt alpha committed",
		},
		{
			name:    "cross-agent content conflict",
			outcome: engine.AdoptResult{Conflicts: []string{"alpha"}},
			want:    "conflict: alpha: content differs across agents; left untouched",
		},
		{
			name:    "already managed skip",
			outcome: engine.AdoptResult{Skipped: []string{"alpha"}},
			want:    "skipped alpha: already managed",
		},
		{
			name: "unlocated candidate failure",
			outcome: engine.AdoptResult{Failed: []engine.FailedAction{{
				Err: errors.New("unlocated failure"),
			}}},
			want: "invalid: unlocated failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newAdoptCmd(fakeAdoptApplication{outcome: tt.outcome})
			stdout, stderr, err := executeSplitForTest(cmd)
			if err != nil {
				t.Fatal(err)
			}
			if output := stdout + stderr; !strings.Contains(output, tt.want) {
				t.Fatalf("adopt output %q must contain %q", output, tt.want)
			}
		})
	}
}

func executeSplitForTest(cmd *cobra.Command) (stdout, stderr string, err error) {
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{})
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestExecuteSplitForTestUsesExplicitEmptyArgs(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"cli.test", "--agent", "codex"}
	cmd := newAdoptCmd(fakeAdoptApplication{})

	if _, _, err := executeSplitForTest(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Flags().Changed("agent") {
		t.Fatal("helper inherited the process --agent flag")
	}
}
