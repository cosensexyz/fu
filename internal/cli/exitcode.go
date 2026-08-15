// internal/cli/exitcode.go
package cli

import (
	"errors"
	"fmt"
	"strings"

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
	args = normalizeGoTestStyleFlags(root, args)
	initUsageClassifiedCompletion(root, args)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	fmt.Fprintln(root.ErrOrStderr(), "error:", err)
	var uerr *UsageError
	if errors.As(err, &uerr) {
		// Print usage for the command that was actually dispatched. Only the
		// exit code was fixed the first time round; the missing usage text --
		// the other half of the same defect, and the half that tells the user
		// what to type instead -- stayed missing, so `fu add` emitted
		// "error: accepts 1 arg(s), received 0" and nothing else (round 18
		// finding M23).
		target := root
		if found, _, findErr := root.Find(args); findErr == nil && found != nil {
			target = found
		}
		fmt.Fprintln(root.ErrOrStderr(), target.UsageString())
		return 2
	}
	if isUnknownCommand(root, args) {
		// The same usage text the UsageError branch prints. Both paths exit 2,
		// so printing it for one and not the other made exit 2 sometimes carry
		// the answer to "what should I have typed" and sometimes not. Root's
		// usage is the right one here: the command the user named does not
		// exist, so there is no more specific usage to show.
		fmt.Fprintln(root.ErrOrStderr(), root.UsageString())
		return 2
	}
	return 1
}

// initUsageClassifiedCompletion lets Cobra build its own completion command
// tree, then applies Fu's typed argument-error boundary to that generated
// tree. Pre-registering a hand-written replacement would duplicate Cobra's
// shell generators and flags; initializing here also preserves the command's
// configured output writer, which tests and alternate front ends may replace
// after constructing the root.
func initUsageClassifiedCompletion(root *cobra.Command, args []string) {
	root.InitDefaultCompletionCmd(args...)
	completion, _, err := root.Find([]string{"completion"})
	if err != nil || completion == nil || completion == root {
		return
	}
	wrapCommandArgsAsUsage(completion)
}

func wrapCommandArgsAsUsage(cmd *cobra.Command) {
	if cmd.Args != nil {
		cmd.Args = usageArgs(cmd.Args)
	}
	for _, child := range cmd.Commands() {
		wrapCommandArgsAsUsage(child)
	}
}

// normalizeGoTestStyleFlags compensates for pflag silently skipping tokens in
// Go's single-dash -test.* namespace. Only tokens parsed as options are
// rewritten: values of known flags, arguments after --, and completion input
// are user data and must remain byte-for-byte unchanged.
func normalizeGoTestStyleFlags(root *cobra.Command, args []string) []string {
	normalized := make([]string, len(args))
	copy(normalized, args)
	if len(normalized) != 0 && (normalized[0] == "__complete" || normalized[0] == "__completeNoDesc") {
		return normalized
	}
	command, _, _ := root.Find(normalized)
	if command == nil {
		command = root
	}

	skipValue := false
	for i, arg := range normalized {
		if arg == "--" {
			break
		}
		if skipValue {
			skipValue = false
			continue
		}
		if flagTakesSeparateValue(command, arg) {
			skipValue = true
			continue
		}
		if strings.HasPrefix(arg, "-test.") {
			normalized[i] = "-" + arg
		}
	}
	return normalized
}

func flagTakesSeparateValue(command *cobra.Command, arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	var flagName string
	var shorthand bool
	switch {
	case strings.HasPrefix(arg, "--"):
		flagName = strings.TrimPrefix(arg, "--")
	case len(arg) == 2 && strings.HasPrefix(arg, "-"):
		flagName, shorthand = arg[1:], true
	default:
		return false
	}
	var flagNoOptDefVal string
	var found bool
	if shorthand {
		if flag := command.Flags().ShorthandLookup(flagName); flag != nil {
			flagNoOptDefVal, found = flag.NoOptDefVal, true
		}
	} else if flag := command.Flag(flagName); flag != nil {
		flagNoOptDefVal, found = flag.NoOptDefVal, true
	}
	return found && flagNoOptDefVal == ""
}

// isUnknownCommand reports whether args fail to resolve to a real command,
// re-running cobra's own read-only Find lookup now that root.Execute() has
// already run once (see execute's doc comment for why this timing is what
// makes the check safe and accurate).
func isUnknownCommand(root *cobra.Command, args []string) bool {
	_, _, err := root.Find(args)
	return err != nil
}
