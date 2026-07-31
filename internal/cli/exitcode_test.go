// internal/cli/exitcode_test.go
package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// Finding I7, case 1: an unrecognized subcommand must exit 2, not 1.
// Reproduced against the compiled binary pre-fix: `fu bogus-command`
// printed "error: unknown command ..." and exited 1, indistinguishable
// from an operation failure.
func TestExitCodeUnknownSubcommand(t *testing.T) {
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
	code, out := runExitCode(t, "show")
	if code != 2 {
		t.Fatalf("missing argument must exit 2, got %d; output: %s", code, out)
	}
}

// Finding I7, case 3: an unknown flag must exit 2.
func TestExitCodeUnknownFlag(t *testing.T) {
	code, out := runExitCode(t, "list", "--bogus-flag")
	if code != 2 {
		t.Fatalf("unknown flag must exit 2, got %d; output: %s", code, out)
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
	code, out := runExitCode(t, "help")
	if code != 0 {
		t.Fatalf("fu help must exit 0, got %d; output: %s", code, out)
	}
}

func TestExitCodeHelpForSubcommandSucceeds(t *testing.T) {
	code, out := runExitCode(t, "help", "new")
	if code != 0 {
		t.Fatalf("fu help new must exit 0, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "new") {
		t.Fatalf("fu help new must print help for the named subcommand, got %q", out)
	}
}

func TestExitCodeCompletionSucceeds(t *testing.T) {
	code, out := runExitCode(t, "completion", "bash")
	if code != 0 {
		t.Fatalf("fu completion bash must exit 0, got %d; output: %s", code, out)
	}
}

// __complete is not a convenience alias -- it is the literal command every
// generated shell completion script (bash/zsh/fish/powershell) invokes on
// every Tab press, so this exiting non-zero means interactive completion
// is entirely dead, not merely degraded.
func TestExitCodeDunderCompleteSucceeds(t *testing.T) {
	code, out := runExitCode(t, "__complete", "new", "")
	if code != 0 {
		t.Fatalf("fu __complete must exit 0, got %d; output: %s", code, out)
	}
}

func TestExitCodeBareInvocationSucceeds(t *testing.T) {
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
	code, out := runExitCode(t, "show", "alpha", "extra")
	if code != 2 {
		t.Fatalf("excess positional arguments must exit 2, got %d; output: %s", code, out)
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
