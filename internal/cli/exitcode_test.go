// internal/cli/exitcode_test.go
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

// runExitCode is exitcode_test.go's own small helper (distinct from the
// shared runCmd, which reports success/failure but not the process exit
// code DESIGN §7 actually specifies) for driving execute() with captured
// output.
func runExitCode(t *testing.T, args ...string) (int, string) {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	code := execute(root, args)
	return code, out.String()
}

func isolateExitCodeEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("FU_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// Finding I7, case 1: an unrecognized subcommand must exit 2, not 1.
// Reproduced against the compiled binary pre-fix: `fu bogus-command`
// printed "error: unknown command ..." and exited 1, indistinguishable
// from an operation failure.
func TestExitCodeUnknownSubcommand(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "bogus-command")
	if code != 2 {
		t.Fatalf("unknown subcommand must exit 2, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "error:") {
		t.Fatalf("output must still report the error, got %q", out)
	}
}

// Finding I7, case 2: a missing required positional argument must exit
// 2. Reproduced against the compiled binary pre-fix: `fu show` printed
// "error: accepts 1 arg(s), received 0" and exited 1.
func TestExitCodeMissingArgument(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "show")
	if code != 2 {
		t.Fatalf("missing argument must exit 2, got %d; output: %s", code, out)
	}
}

// Finding I7, case 3: an unknown flag must exit 2.
func TestExitCodeUnknownFlag(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "list", "--bogus-flag")
	if code != 2 {
		t.Fatalf("unknown flag must exit 2, got %d; output: %s", code, out)
	}
}

func TestExitCodeRejectsGoTestStyleUnknownFlag(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "list", "-test.not-a-fu-flag")
	if code != 2 {
		t.Fatalf("-test.* unknown flag must exit 2, got %d; output: %s", code, out)
	}
}

func TestExitCodePreservesGoTestStylePositionalAfterDoubleDash(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if code, out := runExitCode(t, "init"); code != 0 {
		t.Fatalf("init: exit=%d out=%q", code, out)
	}
	code, out := runExitCode(t, "show", "--", "-test.name")
	if code != 1 {
		t.Fatalf("a preserved unknown skill is an operation failure, got exit=%d out=%q", code, out)
	}
	if !strings.Contains(out, `unknown skill "-test.name"`) || strings.Contains(out, `unknown skill "--test.name"`) {
		t.Fatalf("double-dash positional was rewritten: %q", out)
	}
}

func TestExitCodePreservesGoTestStyleFlagValue(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "add", "--ref", "-test.x", "https://example.invalid/repo.git")
	if code != 2 {
		t.Fatalf("an invalid ref remains a usage error, got exit=%d out=%q", code, out)
	}
	if !strings.Contains(out, `invalid ref "-test.x"`) || strings.Contains(out, `invalid ref "--test.x"`) {
		t.Fatalf("flag value was rewritten: %q", out)
	}
}

func TestNormalizeGoTestStyleFlagsLeavesCompletionFragmentAlone(t *testing.T) {
	root := NewRootCmd()
	args := []string{"__complete", "add", "-test."}
	got := normalizeGoTestStyleFlags(root, args)
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("completion arguments = %q, want %q", got, args)
	}
}

func TestNormalizeGoTestStyleFlagsLeavesNoDescriptionCompletionFragmentAlone(t *testing.T) {
	root := NewRootCmd()
	args := []string{"__completeNoDesc", "add", "-test."}
	got := normalizeGoTestStyleFlags(root, args)
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("completion arguments = %q, want %q", got, args)
	}
}

func TestNormalizeGoTestStyleFlagsDoesNotConsumeValueAfterBoolFlag(t *testing.T) {
	got := normalizeGoTestStyleFlags(NewRootCmd(), []string{"add", "--all", "-test.name"})
	want := []string{"add", "--all", "--test.name"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized arguments = %q, want %q", got, want)
	}
}

func TestNormalizeGoTestStyleFlagsPreservesEqualsValue(t *testing.T) {
	got := normalizeGoTestStyleFlags(NewRootCmd(), []string{"list", "-test.x=value-with-=bytes"})
	want := []string{"list", "--test.x=value-with-=bytes"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized arguments = %q, want %q", got, want)
	}
}

func TestNormalizeGoTestStyleFlagsUsesDispatchedCommandAndShorthand(t *testing.T) {
	root := &cobra.Command{Use: "fu"}
	stringCommand := &cobra.Command{Use: "string"}
	stringCommand.Flags().StringP("value", "v", "", "string value")
	boolCommand := &cobra.Command{Use: "bool"}
	boolCommand.Flags().BoolP("value", "v", false, "boolean value")
	otherCommand := &cobra.Command{Use: "other"}
	root.AddCommand(stringCommand, boolCommand, otherCommand)

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "string shorthand consumes separate value",
			args: []string{"string", "-v", "-test.name"},
			want: []string{"string", "-v", "-test.name"},
		},
		{
			name: "bool shorthand does not consume separate value",
			args: []string{"bool", "-v", "-test.name"},
			want: []string{"bool", "-v", "--test.name"},
		},
		{
			name: "another command local flag does not consume value",
			args: []string{"other", "--value", "-test.name"},
			want: []string{"other", "--value", "--test.name"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGoTestStyleFlags(root, tc.args)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("normalized arguments = %q, want %q", got, tc.want)
			}
		})
	}
}

// Finding I7, case 4: a well-formed command that fails to run (an
// operation failure, as opposed to a usage error) must exit 1, not 2.
func TestExitCodeOperationFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	// Store not initialized: `fu new` is well-formed but fails to run.
	code, out := runExitCode(t, "new", "alpha")
	if code != 1 {
		t.Fatalf("operation failure must exit 1, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "error:") {
		t.Fatalf("output must still report the error, got %q", out)
	}
}

// Round 2 finding 4, at the actual process-exit-code level (not just the
// error value TestNewCommandReportsPerAgentReconcileFailure in new_test.go
// checks): a per-agent reconcile failure (engine.Result.Failed) must make
// the real execute() int return 1. Reproduced against the compiled binary
// pre-fix: `fu new gamma` over a broken claude skills dir printed both
// "created gamma" and "failed: claude: ..." to stdout and exited 0 --
// `echo $?` showed success despite claude receiving nothing at all.
func TestExitCodeReconcileFailedAgentIsOperationFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// claude is detected, but its skills dir is a plain file: ScanAgent
	// fails for it with a genuine (non-precondition) error.
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runExitCode(t, "init"); code != 0 {
		t.Fatalf("init: exit=%d out=%q", code, out)
	}

	code, out := runExitCode(t, "new", "gamma")
	if code != 1 {
		t.Fatalf("a per-agent reconcile failure must exit 1, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "created gamma") {
		t.Fatalf("the durable part (config entry + commit) must still be confirmed: %q", out)
	}
	if !strings.Contains(out, "failed:") {
		t.Fatalf("the failure diagnostic must still be printed: %q", out)
	}
}

// The flip side of the test above, and the decision finding 4 asked this
// pass to make explicit: Conflicts and Missing are expected, actionable
// states fu is correctly refusing to resolve on its own, not operation
// failures, so they must NOT push the exit code to 1. Reproduced here with
// a real (non-fu) directory already occupying a skill's path, which makes
// `fu new` report a conflict rather than a failure.
func TestExitCodeConflictDoesNotCauseOperationFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := runExitCode(t, "init"); code != 0 {
		t.Fatalf("init: exit=%d out=%q", code, out)
	}

	code, out := runExitCode(t, "new", "alpha")
	if code != 0 {
		t.Fatalf("a reported conflict must not be an operation failure, got exit %d; output: %s", code, out)
	}
	if !strings.Contains(out, "conflict:") {
		t.Fatalf("the conflict must still be reported: %q", out)
	}
}

// The disabled-plus-foreign report (Result.DisabledForeign) must actually
// reach the user, on stderr specifically -- these are diagnostics, not the
// command's own output (see printResult's doc comment) -- and must not
// push the exit code to 1: fu correctly left unmanaged content alone,
// the same "expected, actionable state" class as
// Conflicts/Missing/Reserved/Invalid (see engine.ErrOperationFailed's doc
// comment), not a mechanical failure.
//
// Reproduced against the compiled binary pre-fix: disabling a skill whose
// path was already occupied by real, unmanaged content printed only the
// toggle confirmation and exited 0, with no record anywhere -- on either
// stream -- that the path was occupied or that the skill might still be
// loaded.
//
// This test keeps stdout and stderr in separate buffers, unlike the
// shared runCmd/runExitCode helpers (which merge them for convenience --
// see runExitCode's own doc comment), specifically to prove the message
// lands on the stream printResult actually writes to, not merely that it
// appears somewhere in combined output.
func TestExitCodeDisabledForeignReachesStderrNotStdout(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (code int, stdout, stderr string) {
		t.Helper()
		root := NewRootCmd()
		var outBuf, errBuf bytes.Buffer
		root.SetOut(&outBuf)
		root.SetErr(&errBuf)
		code = execute(root, args)
		return code, outBuf.String(), errBuf.String()
	}

	if code, _, stderr := run("init"); code != 0 {
		t.Fatalf("init: exit=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := run("new", "alpha"); code != 0 {
		t.Fatalf("new alpha: exit=%d stderr=%q", code, stderr)
	}
	// alpha is fu-owned and enabled here; disable it while the link is
	// still fu's own, so it is cleanly removed rather than leaving a race
	// between this setup step and the assertions below.
	if code, _, stderr := run("disable", "alpha"); code != 0 {
		t.Fatalf("disable alpha: exit=%d stderr=%q", code, stderr)
	}

	// The user's own, real content now occupies the exact path fu just
	// cleared -- not fu's to touch, and not the same as the still-fu-owned
	// link that was there a moment ago.
	link := filepath.Join(home, ".claude", "skills", "alpha")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	// alpha is already off; this second disable is the exact reproduction
	// this fix targets. Before the fix, this command printed only the
	// confirmation and exited 0, identical to a completely uneventful run.
	code, stdout, stderr := run("disable", "alpha")
	if code != 0 {
		t.Fatalf("a disabled-foreign report must not be an operation failure, got exit %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "disabled-foreign:") || !strings.Contains(stderr, "claude/alpha") {
		t.Fatalf("the disabled-foreign report must reach stderr naming claude/alpha, got stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "occupied by unmanaged content") || !strings.Contains(stderr, "may still be loaded") {
		t.Fatalf("the message must say the user's content occupies the path and the skill may still load, got stderr=%q", stderr)
	}
	if strings.Contains(stdout, "disabled-foreign:") {
		t.Fatalf("the diagnostic must not also leak onto stdout, got stdout=%q", stdout)
	}
	got, err := os.ReadFile(filepath.Join(link, "notes.txt"))
	if err != nil || string(got) != "mine" {
		t.Fatalf("foreign content must never be touched, got %q err=%v", got, err)
	}
}

// Success must still exit 0, unaffected by the usage-error classification
// added elsewhere in execute().
func TestExitCodeSuccess(t *testing.T) {
	fuHome := t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", t.TempDir())
	code, out := runExitCode(t, "init")
	if code != 0 {
		t.Fatalf("success must exit 0, got %d; output: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(fuHome, "store", "fu.yaml")); err != nil {
		t.Fatalf("init must still have actually run: %v", err)
	}
}

// --help must still exit 0 and print help, unaffected by root gaining a
// FlagErrorFunc: this is the path that used to (and must still) go
// through cobra's own flag.ErrHelp handling before any RunE runs.
func TestExitCodeHelpStillSucceeds(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "--help")
	if code != 0 {
		t.Fatalf("--help must exit 0, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "skill manager") {
		t.Fatalf("help output missing description: %q", out)
	}
}

// Round 2 finding 1: root.Find(args) used to run as a pre-check *before*
// root.Execute(), inspecting a command tree that does not yet hold cobra's
// own help/completion/__complete subcommands -- those are only registered
// inside Execute() itself (InitDefaultHelpCmd/InitDefaultCompletionCmd/
// initCompleteCmd, all called at the top of ExecuteC(), before ExecuteC()
// does its own, equivalent Find). Reproduced against the compiled binary
// pre-fix: `fu help`, `fu help new`, `fu completion bash`, and `fu
// __complete new` (with a trailing empty argument) all printed `error:
// unknown command ... for "fu"` and exited 2 -- breaking one of the two
// documented ways to get help and killing shell completion outright,
// since __complete is the exact entry point every generated completion
// script calls.
//
// Round 2 finding 2: this whole class had no process-level coverage.
// exitcode_test.go already drives execute() -- the process-level entry
// point; see Run's and execute's doc comments for why cmd/fu/main.go is
// one line specifically so this stays testable without spawning a process
// -- through runExitCode below, unlike the rest of internal/cli, which
// calls cmd.Execute() directly through the shared runCmd helper and so
// never reaches execute()'s classification at all. But the existing cases
// here never happened to exercise help/completion/__complete, so the
// regression above shipped with the whole suite green. The cases below
// close that gap at the same execute()-level runExitCode already reaches;
// cmd/fu/main_test.go's TestBinarySmoke separately confirms a sample of
// them end to end against the real compiled binary.
func TestExitCodeHelpSucceeds(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "help")
	if code != 0 {
		t.Fatalf("fu help must exit 0, got %d; output: %s", code, out)
	}
}

func TestExitCodeHelpForSubcommandSucceeds(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "help", "new")
	if code != 0 {
		t.Fatalf("fu help new must exit 0, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "new") {
		t.Fatalf("fu help new must print help for the named subcommand, got %q", out)
	}
}

func TestExitCodeCompletionSucceeds(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "completion", "bash")
	if code != 0 {
		t.Fatalf("fu completion bash must exit 0, got %d; output: %s", code, out)
	}
}

func TestExitCodeCompletionExcessArgumentsIsUsageError(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "completion", "bash", "zsh")
	if code != 2 {
		t.Fatalf("completion argument-count error must exit 2, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "fu completion bash") || !strings.Contains(out, "Usage:") {
		t.Fatalf("completion argument-count error must show dispatched usage, got %q", out)
	}
}

// __complete is not a convenience alias -- it is the literal command every
// generated shell completion script (bash/zsh/fish/powershell) invokes on
// every Tab press, so this exiting non-zero means interactive completion
// is entirely dead, not merely degraded.
func TestExitCodeDunderCompleteSucceeds(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "__complete", "new", "")
	if code != 0 {
		t.Fatalf("fu __complete must exit 0, got %d; output: %s", code, out)
	}
}

func TestExitCodeBareInvocationSucceeds(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t)
	if code != 0 {
		t.Fatalf("bare fu must exit 0, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "skill manager") {
		t.Fatalf("bare fu must still print help, got %q", out)
	}
}

// Finding 2's own "too many arguments" case: unaffected by finding 1's bug
// (excess positional args are classified via usageArgs/UsageError, not the
// unknown-command path), but was likewise missing from process-level
// coverage -- every existing arg-count case here tested too few, not too
// many.
func TestExitCodeExcessArguments(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "show", "alpha", "extra")
	if code != 2 {
		t.Fatalf("excess positional arguments must exit 2, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "fu show <name>") {
		t.Fatalf("usage errors must show the dispatched command's usage, got %q", out)
	}
}

// UsageError must unwrap to the underlying cobra-generated error, so
// errors.Is/errors.As chains through it like any other wrapped error.
func TestUsageErrorUnwraps(t *testing.T) {
	inner := errors.New("boom")
	var err error = &UsageError{inner}
	if !errors.Is(err, inner) {
		t.Fatal("UsageError must unwrap to the error it wraps")
	}
	if err.Error() != "boom" {
		t.Fatalf("UsageError.Error() must delegate to the wrapped error, got %q", err.Error())
	}
}

// Run is the exported entry point DESIGN §9 expects another front end to sit
// beside, so a nil argument slice must mean "no arguments" -- not "whatever
// this process was invoked with". SetArgs(nil) leaves cobra's args nil and
// ExecuteC falls back to os.Args[1:], which also made isUnknownCommand inspect
// a different argument list than the one cobra dispatched on.
func TestRunWithNilArgsIgnoresTheProcessCommandLine(t *testing.T) {
	isolateExitCodeEnvironment(t)
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = []string{"fu", "bogus-extra-arg"}

	code, out := runExitCode(t)
	if code != 0 {
		t.Fatalf("nil args must behave as a bare invocation, got %d; output: %s", code, out)
	}
	if strings.Contains(out, "bogus-extra-arg") {
		t.Fatalf("nil args consumed the process command line: %s", out)
	}
}

// TestExitCodeUnknownAgentValueIsUsageError pins the two halves of one
// inconsistency. `--agent ""` exited 2 while `--agent nosuch` exited 1, though
// both are malformed flag values typed on the command line and DESIGN §7
// defines exit 2 as the usage class -- one construct producing two exit-code
// classes across the CLI. An agent that is known but simply not installed on
// this machine is a different thing and stays exit 1.
func TestExitCodeUnknownAgentValueIsUsageError(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	for _, args := range [][]string{
		{"adopt", "--agent", "nosuch"},
		{"adopt", "--agent", ""},
		{"enable", "alpha", "--agent", "nosuch"},
		{"enable", "alpha", "--agent", ""},
	} {
		code, out := runExitCode(t, args...)
		if code != 2 {
			t.Fatalf("%v: a malformed --agent value must exit 2, got %d; output: %s", args, code, out)
		}
		if !strings.Contains(out, "Usage:") {
			t.Fatalf("%v: exit 2 must tell the user what to type instead: %q", args, out)
		}
	}

	// codex is a real adapter; it is simply not installed under this HOME.
	// That is an environment fact, not a typo, so it stays an operation
	// failure.
	if code, out := runExitCode(t, "adopt", "--agent", "codex"); code != 1 {
		t.Fatalf("a known but undetected agent must exit 1, got %d; output: %s", code, out)
	}
}

func TestExitCodeInvalidAddRefsAreUsageErrors(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	const gitURL = "https://example.invalid/repo.git"
	for _, ref := range []string{
		"",
		"refs/heads/main",
		strings.Repeat("0", 40),
		"bad ref",
	} {
		code, out := runExitCode(t, "add", "--ref", ref, gitURL)
		if code != 2 {
			t.Fatalf("--ref %q: malformed flag value must exit 2, got %d; output: %s", ref, code, out)
		}
		if !strings.Contains(out, "Usage:") {
			t.Fatalf("--ref %q: usage failure must show the valid command shape: %q", ref, out)
		}
	}
}

// TestExitCodeUnknownSubcommandPrintsUsage is the other half of round 18
// finding M23. Usage text was added for the UsageError branch but not for the
// unknown-command path, which also exits 2 -- so exit 2 sometimes printed
// usage and sometimes did not.
func TestExitCodeUnknownSubcommandPrintsUsage(t *testing.T) {
	isolateExitCodeEnvironment(t)
	code, out := runExitCode(t, "bogus-command")
	if code != 2 {
		t.Fatalf("unknown subcommand must exit 2, got %d", code)
	}
	if !strings.Contains(out, "Usage:") {
		t.Fatalf("exit 2 must carry usage text on every path that produces it: %q", out)
	}
	if !strings.Contains(out, "adopt") {
		t.Fatalf("root usage must list the real commands: %q", out)
	}
}

// TestPrintResultNamesRetiredPathOnConflict is the CLI half of round 18's M10.
// The engine half is pinned by TestReconcileConflictNamesRetiredPathWhenRestoreFails,
// but the sentence the *user* reads had no test: changing the `Target != ""`
// branch to `false` left the suite green, and that string appears exactly once
// in the repo, in production code. It is the only pointer the user gets to
// where fu parked their content.
func TestPrintResultNamesRetiredPathOnConflict(t *testing.T) {
	const retired = "/home/u/.claude/skills/.fu-retired-abc123"
	cmd := &cobra.Command{}
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	printResult(cmd, engine.Result{Conflicts: []engine.Action{
		{AgentName: "claude", Skill: "alpha", Target: retired},
		{AgentName: "codex", Skill: "beta"},
	}})
	got := errBuf.String()
	if !strings.Contains(got, "fu's own link was moved to "+retired) {
		t.Fatalf("a conflict carrying a retired path must name it: %q", got)
	}
	// The bare conflict keeps its shorter wording, with no dangling "moved to".
	if !strings.Contains(got, "conflict: codex/beta occupied by unmanaged content\n") {
		t.Fatalf("a conflict with no retired path must not gain one: %q", got)
	}
	if outBuf.Len() != 0 {
		t.Fatalf("diagnostics belong on stderr: %q", outBuf.String())
	}
}

// TestDiagnosticsPrecedeConfirmation pins the ordering rule toggle.go states
// and the four single-object commands follow. Swapping the confirmation back
// ahead of the diagnostics in new.go and rm.go left the suite green, though a
// reader watching both streams together -- the ordinary terminal case -- is
// exactly who the rule is for: the caveat should lead into the claim it may
// qualify rather than contradict it a line later.
func TestDiagnosticsPrecedeConfirmation(t *testing.T) {
	// A merged buffer is what makes the ordering observable at all, which is
	// why runCmd rather than runCmdSplit is the right harness here.
	cases := map[string]struct {
		setup        func(t *testing.T, fuHome, home string)
		args         []string
		confirmation string
		diagnostic   string
	}{
		"new": {
			setup: func(t *testing.T, _, home string) {
				occupied := filepath.Join(home, ".claude", "skills", "alpha")
				if err := os.MkdirAll(occupied, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			args:         []string{"new", "alpha"},
			confirmation: "created alpha",
			diagnostic:   "conflict:",
		},
		"rm": {
			setup: func(t *testing.T, _, home string) {
				src := t.TempDir()
				writeSkill(t, src, "alpha")
				if _, err := runCmd(t, "add", src); err != nil {
					t.Fatal(err)
				}
				// codex's skills directory becomes a plain file, so reconcile
				// isolates that agent into Failed while the removal itself is
				// already durable.
				codex := filepath.Join(home, ".codex", "skills")
				if err := os.RemoveAll(codex); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(codex, []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			args:         []string{"rm", "alpha"},
			confirmation: "removed alpha",
			diagnostic:   "failed:",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fuHome, home := t.TempDir(), t.TempDir()
			t.Setenv("FU_HOME", fuHome)
			t.Setenv("HOME", home)
			for _, dir := range []string{".claude", ".codex"} {
				if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			runCmd(t, "init")
			tc.setup(t, fuHome, home)

			out, _ := runCmd(t, tc.args...)
			confirmation := strings.Index(out, tc.confirmation)
			diagnostic := strings.Index(out, tc.diagnostic)
			if confirmation < 0 || diagnostic < 0 {
				t.Fatalf("test setup: need both a confirmation and a diagnostic, got %q", out)
			}
			if diagnostic > confirmation {
				t.Fatalf("diagnostics must precede the confirmation:\n%s", out)
			}
		})
	}
}
