// internal/cli/exitcode.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// UsageError marks err as a usage error -- bad arguments or flags -- as
// opposed to a well-formed command that failed to run (finding I7;
// DESIGN §7's exit code 2 vs 1). Constructed at the point cobra itself
// generates the underlying error (see usageArgs below and
// NewRootCmd's SetFlagErrorFunc); Run detects it with errors.As to pick
// the process exit code.
type UsageError struct {
	err error
}

func (e *UsageError) Error() string { return e.err.Error() }
func (e *UsageError) Unwrap() error { return e.err }

// usageArgs wraps a cobra.PositionalArgs validator so any error it
// returns (wrong argument count) is classified as a UsageError.
func usageArgs(fn cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := fn(cmd, args); err != nil {
			return &UsageError{err}
		}
		return nil
	}
}

// Run builds the command tree, executes it with args, and returns the
// process exit code (DESIGN §7): 0 success, 1 operation failure, 2 usage
// error. cmd/fu/main.go is kept to a single line so this stays testable
// without spawning a process (cmd/fu/main_test.go's TestBinarySmoke
// additionally exercises the real binary end to end).
//
// Before the first exit-code fix, all four of unknown subcommand, missing
// argument, unknown flag, and operation failure returned exit code 1, and
// SilenceUsage also suppressed cobra's own usage text for its argument
// errors -- `fu show` printed only "error: accepts 1 arg(s), received 0"
// with no usage text, so neither the exit code nor the output
// distinguished the two classes. That fix's own shape then broke `fu
// help`, `fu help <cmd>`, `fu completion <shell>`, and `fu __complete ...`
// (round 2 finding 1) -- see execute's doc comment for the current shape.
func Run(args []string) int {
	return execute(NewRootCmd(), args)
}

// execute is Run's testable core: it takes an already-configured command
// (tests redirect Out/Err on it before calling in) instead of always
// building a fresh one.
//
// Classification happens from root.Execute()'s own returned error, not
// from a pre-check run before it (round 2 finding 1). The previous shape
// classified an unrecognized subcommand with a read-only call to
// root.Find(args) made *before* root.Execute(): root has subcommands but
// no Args of its own, so Find's internal legacyArgs check is what
// produces "unknown command ... for ...". The problem is that cobra
// registers its own help, completion, and hidden __complete subcommands
// only *inside* ExecuteC() itself (InitDefaultHelpCmd,
// InitDefaultCompletionCmd, and initCompleteCmd, all called at its top,
// before ExecuteC() runs this very same Find internally) -- so a Find
// call made ahead of time inspects a tree missing all three, and
// misclassifies every one of them as an unknown command. `fu help`, `fu
// help <cmd>`, and `fu completion <shell>` broke one of the two
// documented ways to get help; `fu __complete ...` -- the exact command
// every generated shell completion script invokes -- broke Tab completion
// outright.
//
// Two fixes were considered. Calling InitDefaultHelpCmd and
// InitDefaultCompletionCmd ourselves before the pre-check handles help
// and completion, but not __complete: that one is registered by
// initCompleteCmd, an unexported cobra method this package cannot call,
// so the pre-check would still need its own special case for it -- a sign
// that re-implementing cobra's own dispatch here will keep diverging from
// it as cobra evolves. Classifying only after Execute() has already run
// sidesteps the whole problem: by the time it returns, cobra has already
// registered (and, for help/completion/__complete specifically, correctly
// dispatched to) all three, so nothing needs re-implementing.
//
// What Execute()'s returned error cannot do by itself is distinguish a
// genuinely unknown command from a real operation failure: cobra reports
// the former as a plain, unwrapped fmt.Errorf, indistinguishable by type
// or sentinel from whatever error a command's own RunE returns. isUnknownCommand
// re-runs cobra's own lookup to tell the two apart, safely: Find is
// side-effect-free, Execute() already calls it internally regardless, and
// -- unlike the old pre-check -- it now runs after the same registration
// Execute() itself just did, so it sees help/completion/__complete
// exactly as Execute() did.
func execute(root *cobra.Command, args []string) int {
	// A nil slice is not "no arguments" to cobra: SetArgs(nil) leaves the
	// command's args nil, and ExecuteC then falls back to os.Args[1:]. Run is
	// the exported entry point another front end sits beside, so passing nil
	// would silently execute this process's own command line -- and
	// isUnknownCommand, which inspects the caller's args, would disagree with
	// what cobra actually dispatched.
	if args == nil {
		args = []string{}
	}
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	fmt.Fprintln(root.ErrOrStderr(), "error:", err)
	var uerr *UsageError
	if errors.As(err, &uerr) {
		return 2
	}
	if isUnknownCommand(root, args) {
		return 2
	}
	return 1
}

// isUnknownCommand reports whether args fail to resolve to a real command,
// re-running cobra's own read-only Find lookup now that root.Execute() has
// already run once (see execute's doc comment for why this timing is what
// makes the check safe and accurate).
func isUnknownCommand(root *cobra.Command, args []string) bool {
	_, _, err := root.Find(args)
	return err != nil
}
