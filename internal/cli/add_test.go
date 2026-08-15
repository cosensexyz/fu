package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestMergeCloseErrorReportsCleanupFailure(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	var result error
	mergeCloseError(&result, failingCloser{err: cleanupErr})
	if !errors.Is(result, cleanupErr) {
		t.Fatalf("result = %v, want cleanup error", result)
	}
}

func TestAddRefRequiresGitSource(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")

	out, err := runCmd(t, "add", "--ref", "feature/foo", srcDir)
	if err == nil || !strings.Contains(err.Error(), "--ref can only be used with a git URL") {
		t.Fatalf("add output/error = %q, %v; want explicit local-source refusal", out, err)
	}
	var usageErr *UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("an inapplicable --ref is a usage error, got %T: %v", err, err)
	}
}

func TestAddExplicitEmptyRefIsUsageError(t *testing.T) {
	cmd := newAddCmd(fakeAddApplication{session: &fakeAddSession{}})
	cmd.SetArgs([]string{"--ref", "", "SRC"})
	err := cmd.Execute()
	var usageErr *UsageError
	if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("explicit empty --ref must be a usage error, got %T: %v", err, err)
	}
}

// TestAddLocalSingleSkill runs the whole `fu add <dir>` command against a
// local source and asserts store content, config source fields, and the
// agent link.
func TestAddLocalSingleSkill(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	srcDir := t.TempDir()
	writeSkill(t, srcDir, "pdf-tools")
	out, err := runCmd(t, "add", srcDir)
	if err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "added pdf-tools") {
		t.Fatalf("output missing confirmation: %s", out)
	}
	if _, err := os.Stat(filepath.Join(fuHome, "store", "skills", "pdf-tools", "SKILL.md")); err != nil {
		t.Fatalf("store content missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "pdf-tools")); err != nil {
		t.Fatalf("agent link missing: %v", err)
	}
	// The source record is written and visible to show.
	out, err = runCmd(t, "show", "pdf-tools")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, out)
	}
	if !strings.Contains(out, "type:     local") {
		t.Fatalf("show missing local source:\n%s", out)
	}
}

// TestAddInteractiveSelection exercises the prompt path with a multi-skill
// source: only the selected skill is installed.
func TestAddInteractiveSelection(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")
	writeSkill(t, srcDir, "beta")
	cmd := newAddCmd(engine.NewApplication())
	out := captureOutput(t, cmd, "2\n", srcDir)
	if !strings.Contains(out, "added beta") || strings.Contains(out, "added alpha") {
		t.Fatalf("only the selected skill must be installed:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "beta")); err != nil {
		t.Fatalf("beta link missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha must not be installed: %v", err)
	}
}

// TestAddAllFlag installs every candidate without prompting.
func TestAddAllFlag(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")
	writeSkill(t, srcDir, "beta")
	out, err := runCmd(t, "add", "--all", srcDir)
	if err != nil {
		t.Fatalf("add --all: %v (%s)", err, out)
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", name)); err != nil {
			t.Fatalf("%s link missing: %v", name, err)
		}
	}
}

// TestAddEmptySelection aborts cleanly when the prompt receives nothing.
func TestAddEmptySelection(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")
	writeSkill(t, srcDir, "beta")
	out, err := runCmdWithInput(t, "\n", "add", srcDir)
	if err != nil {
		t.Fatalf("empty selection must not fail the command: %v (%s)", err, out)
	}
	if !strings.Contains(out, "nothing selected") {
		t.Fatalf("empty selection must say so:\n%s", out)
	}
}

// TestAddGitURL runs add against a git URL end to end through the CLI.
func TestAddGitURL(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	url := makeGitSourceCLI(t)
	out, err := runCmd(t, "add", url)
	if err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "added pdf-tools") {
		t.Fatalf("output missing confirmation: %s", out)
	}
	cfg := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "type: git") || !strings.Contains(string(raw), "commit:") {
		t.Fatalf("git source record missing from fu.yaml:\n%s", raw)
	}
}

// TestAddDuplicateSkipped reports a re-install inside a batch as skipped, not
// an error -- SPEC rule 1's 批量安装时重名项跳过并提示. The batch is now a
// genuine one: a single candidate takes the other half of the same rule
// (遇同名即拒绝安装, TestAddSingleAlreadyInstalledFails), so a one-skill source
// no longer exercises this path (round 18 finding I18). The skip also names
// the way out, which DESIGN §6 requires of any report the user cannot
// otherwise act on.
func TestAddDuplicateSkipped(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")
	if _, err := runCmd(t, "add", srcDir); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, srcDir, "beta")

	stdout, stderr, err := runCmdSplit(t, "add", "--all", srcDir)
	if err != nil {
		t.Fatalf("a batch must skip the duplicate, not fail: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "added beta") {
		t.Fatalf("stdout must confirm the new skill:\n%s", stdout)
	}
	if !strings.Contains(stderr, "skipped alpha") || !strings.Contains(stderr, "fu rm alpha") {
		t.Fatalf("the skip must be reported and name the way out:\n%s", stderr)
	}
}

// TestAddCommandReportsReconcileConflict pins that fu add surfaces the
// trailing reconcile's findings (round 6 finding I1): a foreign entry
// occupying the new skill's agent slot was previously installed silently --
// the reconcile Result was discarded and the "conflict:" line never
// printed. It must appear on stderr like every other diagnostic.
func TestAddCommandReportsReconcileConflict(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	// A foreign directory occupies the slot the new skill's link would take.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "pdf-tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills", "pdf-tools", "README"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "pdf-tools")

	out, err := runCmd(t, "add", srcDir)
	if err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "added pdf-tools") {
		t.Fatalf("output missing add line: %s", out)
	}
	if !strings.Contains(out, "conflict: claude/pdf-tools occupied by unmanaged content") {
		t.Fatalf("output missing reconcile conflict line:\n%s", out)
	}
}

func TestAddPrintsDurableSuccessBeforeReconcileFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "skills"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")

	out, err := runCmd(t, "add", srcDir)
	if !errors.Is(err, engine.ErrOperationFailed) {
		t.Fatalf("add error = %v, want ErrOperationFailed; output=%s", err, out)
	}
	if !strings.Contains(out, "added alpha") {
		t.Fatalf("output must report the durable install before failure:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(fuHome, "store", "skills", "alpha", "SKILL.md")); err != nil {
		t.Fatalf("durable skill missing: %v", err)
	}
}

func TestAddCommandEmptySelectionEOFRequiresAll(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	srcDir := t.TempDir()
	// Two candidates force the interactive selection prompt.
	writeSkill(t, srcDir, "alpha")
	writeSkill(t, srcDir, "beta")

	out, err := runCmdWithInput(t, "", "add", srcDir)
	if err == nil {
		t.Fatalf("unavailable selection input must fail instead of exiting 0: %s", out)
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Fatalf("error must name the non-interactive alternative: %v", err)
	}
	if !strings.Contains(out, "select skills to install (comma-separated numbers, or `all`): \n") {
		t.Fatalf("EOF must terminate the prompt line: %q", out)
	}
}

// addEnv builds an initialized FU_HOME/HOME pair with one detected agent.
func addEnv(t *testing.T) (fuHome, home string) {
	t.Helper()
	fuHome, home = t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	return fuHome, home
}

// TestAddSingleAlreadyInstalledFails pins round 18 finding I18. SPEC rule 1
// distinguishes the two duplicate-name cases -- fu add 遇同名即拒绝安装，提示先
// fu rm 旧项；批量安装时重名项跳过并提示 -- but the batch rule was applied
// unconditionally, so a targeted single-skill add printed one stderr line,
// nothing to stdout, and exited 0. A script could not tell that from success.
func TestAddSingleAlreadyInstalledFails(t *testing.T) {
	addEnv(t)
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")
	if _, err := runCmd(t, "add", srcDir); err != nil {
		t.Fatalf("first install: %v", err)
	}

	stdout, stderr, err := runCmdSplit(t, "add", srcDir)
	if err == nil {
		t.Fatalf("a single-source add of an installed skill must fail; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "fu rm") {
		t.Fatalf("the refusal must name the way out (SPEC rule 1): %v", err)
	}
}

// TestAddStreamPlacement pins round 18 finding I17. Confirmations belong on
// stdout and diagnostics on stderr; every existing assertion used runCmd,
// which merges the two, so moving `added X` to stderr would have left the
// suite green. init_test.go:24 records the same regression class from round 5.
func TestAddStreamPlacement(t *testing.T) {
	addEnv(t)
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")
	if err := os.MkdirAll(filepath.Join(srcDir, "notaskill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "notaskill", "SKILL.md"), []byte("---\nname: mismatched\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCmdSplit(t, "add", "--all", srcDir)
	if err != nil {
		t.Fatalf("add: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stdout, "added alpha") {
		t.Fatalf("the confirmation belongs on stdout: %q", stdout)
	}
	if strings.Contains(stderr, "added alpha") {
		t.Fatalf("the confirmation must not go to stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "invalid:") {
		t.Fatalf("the diagnostic belongs on stderr: %q", stderr)
	}
	if strings.Contains(stdout, "invalid:") {
		t.Fatalf("the diagnostic must not go to stdout: %q", stdout)
	}
}

// TestAddPromptGoesToStderr pins round 18 minor finding M21: `fu add $SRC >
// out.txt` used to put the numbered candidate list and the prompt into the
// file, so the terminal showed nothing and the command looked hung.
func TestAddPromptGoesToStderr(t *testing.T) {
	addEnv(t)
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "alpha")
	writeSkill(t, srcDir, "beta")

	cmd := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader("\n"))
	cmd.SetArgs([]string{"add", srcDir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(errBuf.String(), "select skills to install") {
		t.Fatalf("the prompt belongs on stderr: %q", errBuf.String())
	}
	if strings.Contains(outBuf.String(), "select skills to install") || strings.Contains(outBuf.String(), "1. alpha") {
		t.Fatalf("the prompt and candidate list must not go to stdout: %q", outBuf.String())
	}
}

func TestAddBatchContinuesAfterIsolatedCandidateFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	srcDir := t.TempDir()
	for _, name := range []string{"aaa", "bbb", "ccc"} {
		writeSkill(t, srcDir, name)
	}
	// Unregistered content already sits at the first candidate's store
	// position, so checkAddAvailable refuses it -- a hard failure, not the
	// skip a duplicate name would produce. The failure is isolated to aaa.
	if err := os.MkdirAll(filepath.Join(fuHome, "store", "skills", "aaa"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCmdSplit(t, "add", "--all", srcDir)
	if err == nil {
		t.Fatalf("the blocked candidate must fail the command: %q / %q", stdout, stderr)
	}
	if strings.Contains(stderr, "not attempted:") {
		t.Fatalf("all safe candidates must be attempted: %q", stderr)
	}
	for _, name := range []string{"bbb", "ccc"} {
		if !strings.Contains(stdout, "added "+name) {
			t.Fatalf("candidate %s after the isolated failure was not added: stdout=%q stderr=%q", name, stdout, stderr)
		}
	}
}

// TestAddEmptySourceIsAnError pins the other half of round 18 finding I20's
// boundary fix, which also shipped untested: only the CLI fake mentioned
// NoCandidates, and it returned nil. A source the user named expecting to
// install from is an error when it holds nothing installable -- exiting 0
// having done nothing is indistinguishable from a successful install.
func TestAddEmptySourceIsAnError(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	empty := t.TempDir()
	stdout, stderr, err := runCmdSplit(t, "add", "--all", empty)
	if err == nil {
		t.Fatalf("an empty source must not exit 0: %q / %q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "no valid skills found") {
		t.Fatalf("the refusal must name the cause: %v", err)
	}
	if !strings.Contains(err.Error(), empty) {
		t.Fatalf("the refusal must name the source the user typed: %v", err)
	}
	if stdout != "" {
		t.Fatalf("nothing was installed, so stdout must stay empty: %q", stdout)
	}
}

// TestAddInvalidNamesTheSourceNotTheScratchDir pins that `invalid:` points at
// something the user can act on. The engine keyed these on the absolute path
// it read from, which for a git source is the clone directory under staging --
// deleted the moment the command exits.
func TestAddInvalidNamesTheSourceNotTheScratchDir(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "good")
	broken := filepath.Join(srcDir, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "SKILL.md"),
		[]byte("---\nname: MISMATCH\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCmdSplit(t, "add", "--all", srcDir)
	if err != nil {
		t.Fatalf("one invalid candidate must not fail the run: %v (%s)", err, stderr)
	}
	if !strings.Contains(stderr, "invalid: "+srcDir+": broken:") {
		t.Fatalf("invalid must name the source as typed plus the path within it: %q", stderr)
	}
	if strings.Contains(stderr, ".fu-src-") || strings.Contains(stderr, "staging") {
		t.Fatalf("invalid must not name an ephemeral scratch path: %q", stderr)
	}
}

// TestSelectCandidatesInputBranches covers the interactive selection's refusal
// and edge branches, none of which had a test. A trailing comma used to yield
// `invalid selection ""` -- a message naming nothing the user typed -- and
// neither the non-numeric nor the out-of-range branch was exercised at all.
func TestSelectCandidatesInputBranches(t *testing.T) {
	cands := []engine.Candidate{
		{Name: "alpha", Description: "a", Subdir: "tools/alpha"},
		{Name: "beta", Description: "b", Subdir: "vendor/beta"},
		{Name: "gamma", Description: "c", Subdir: "gamma"},
	}
	selectWith := func(input string) ([]engine.Candidate, error) {
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetIn(strings.NewReader(input))
		return selectCandidates(cmd, cands, false)
	}
	names := func(sel []engine.Candidate) []string {
		out := make([]string, 0, len(sel))
		for _, c := range sel {
			out = append(out, c.Name)
		}
		return out
	}

	t.Run("trailing comma is a typo, not a selection", func(t *testing.T) {
		sel, err := selectWith("1,3,\n")
		if err != nil {
			t.Fatalf("a trailing comma must not be refused: %v", err)
		}
		if got := names(sel); len(got) != 2 || got[0] != "alpha" || got[1] != "gamma" {
			t.Fatalf("selection = %v, want alpha and gamma in presentation order", got)
		}
	})
	t.Run("non-numeric names what was typed", func(t *testing.T) {
		_, err := selectWith("1,two\n")
		if err == nil {
			t.Fatal("a non-numeric selection must be refused")
		}
		if !strings.Contains(err.Error(), `"two"`) {
			t.Fatalf("the refusal must quote what the user typed: %v", err)
		}
	})
	t.Run("out of range names the range", func(t *testing.T) {
		for _, input := range []string{"0\n", "4\n", "-1\n"} {
			_, err := selectWith(input)
			if err == nil {
				t.Fatalf("%q must be refused", input)
			}
			if !strings.Contains(err.Error(), "1-3") {
				t.Fatalf("%q: the refusal must name the valid range: %v", input, err)
			}
		}
	})
	t.Run("only separators selects nothing", func(t *testing.T) {
		sel, err := selectWith(",,\n")
		if err != nil {
			t.Fatalf("separators alone must not be an error: %v", err)
		}
		if len(sel) != 0 {
			t.Fatalf("separators alone must select nothing, got %v", names(sel))
		}
	})
	t.Run("all selects everything", func(t *testing.T) {
		sel, err := selectWith("all\n")
		if err != nil || len(sel) != len(cands) {
			t.Fatalf("`all` must select every candidate: %v, %v", names(sel), err)
		}
	})
	t.Run("all cannot be mixed with numeric selections", func(t *testing.T) {
		for _, input := range []string{"all,1\n", "2, all\n"} {
			_, err := selectWith(input)
			if err == nil || !strings.Contains(err.Error(), "cannot mix `all`") {
				t.Fatalf("%q: mixed all selection error = %v", input, err)
			}
		}
	})
	t.Run("prompt identifies source subdirectories", func(t *testing.T) {
		cmd := &cobra.Command{}
		var prompt bytes.Buffer
		cmd.SetErr(&prompt)
		cmd.SetIn(strings.NewReader("\n"))
		if _, err := selectCandidates(cmd, cands, false); err != nil {
			t.Fatal(err)
		}
		for _, subdir := range []string{"tools/alpha", "vendor/beta", "gamma"} {
			if !strings.Contains(prompt.String(), subdir) {
				t.Fatalf("prompt %q does not identify candidate subdir %q", prompt.String(), subdir)
			}
		}
	})
}

type partialSelectionErrorReader struct {
	done bool
}

func (r *partialSelectionErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("injected read failure")
	}
	r.done = true
	p[0] = '1'
	return 1, errors.New("injected read failure")
}

func TestSelectCandidatesReportsNonEOFReadErrorWithPartialInput(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(&partialSelectionErrorReader{})
	_, err := selectCandidates(cmd, []engine.Candidate{{Name: "alpha"}, {Name: "beta"}}, false)
	if err == nil || !strings.Contains(err.Error(), "injected read failure") {
		t.Fatalf("non-EOF read error must be reported, got %v", err)
	}
}

func TestAddCommandRefusesNilPreparedSession(t *testing.T) {
	cmd := newAddCmd(fakeAddApplication{})
	cmd.SetArgs([]string{"SRC"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("nil prepared session must return a diagnostic error, got %v", err)
	}
}

// TestAddAbortExitsReportPrologueFindings pins the fix that carries the
// mandatory recovery prologue's findings out of add's abort exits. All three
// are covered -- no candidates, selection refused, nothing selected -- because
// each is a path where the user walked away and the prologue's findings are
// the only thing the run has to say. Deleting the printResult calls left the
// suite green: the only mention was a fake returning a zero Result.
func TestAddAbortExitsReportPrologueFindings(t *testing.T) {
	prologue := engine.Result{Conflicts: []engine.Action{{AgentName: "claude", Skill: "alpha"}}}
	cases := map[string]struct {
		session *fakeAddSession
		stdin   string
		wantErr bool
	}{
		"no installable candidates": {
			session: &fakeAddSession{prologue: prologue, noCandidates: errors.New("no valid skills found in SRC")},
			wantErr: true,
		},
		"selection refused": {
			session: &fakeAddSession{prologue: prologue, candidates: []engine.Candidate{
				{Name: "alpha", Subdir: "alpha"}, {Name: "beta", Subdir: "beta"},
			}},
			stdin:   "nonsense\n",
			wantErr: true,
		},
		"nothing selected": {
			session: &fakeAddSession{prologue: prologue, candidates: []engine.Candidate{
				{Name: "alpha", Subdir: "alpha"}, {Name: "beta", Subdir: "beta"},
			}},
			stdin: "\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := newAddCmd(fakeAddApplication{session: tc.session})
			var outBuf, errBuf bytes.Buffer
			cmd.SetOut(&outBuf)
			cmd.SetErr(&errBuf)
			cmd.SetIn(strings.NewReader(tc.stdin))
			cmd.SetArgs([]string{"SRC"})
			err := cmd.Execute()
			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
			if !strings.Contains(errBuf.String(), "conflict: claude/alpha") {
				t.Fatalf("the prologue's findings must reach the user on an abort exit: %q", errBuf.String())
			}
		})
	}
}

func TestAddPreparationFailureReportsPrologueFindings(t *testing.T) {
	boom := errors.New("clone failed")
	cmd := newAddCmd(fakeAddApplication{
		err: boom,
		prologue: engine.Result{
			Conflicts: []engine.Action{{AgentName: "claude", Skill: "alpha"}},
		},
	})
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"SRC"})
	err := cmd.Execute()
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if !strings.Contains(errBuf.String(), "conflict: claude/alpha") {
		t.Fatalf("the recovery prologue must survive preparation failure: %q", errBuf.String())
	}
}
