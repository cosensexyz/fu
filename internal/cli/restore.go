// internal/cli/restore.go
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type restoreApplication interface {
	Restore(hard bool) (engine.RestoreOutcome, error)
}

func newRestoreCmd(app restoreApplication) *cobra.Command {
	var hard bool
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Rebuild agent links from fu.yaml",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			outcome, err := app.Restore(hard)
			printResult(cmd, outcome.Result)
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			// Printed before the error is returned, for the reason
			// printResult is: engine.Restore fills Reset before it assigns the
			// error, so by the time a failure arrives these paths have already
			// been discarded. Returning first told a user their run had failed
			// and never told them what it had destroyed on the way.
			printResetPaths(out, outcome.Reset)
			// The actionable group first, then the one with no flag behind it,
			// so a reader meets `--hard` beside the paths it actually resets
			// and never beside the paths it provably will not.
			if err != nil {
				printRefusedPaths(errOut, outcome.Refused)
				printKeptPaths(errOut, outcome.Left)
				return err
			}
			fmt.Fprintln(out, "restored agent links")
			printRefusedPaths(errOut, outcome.Refused)
			printKeptPaths(errOut, outcome.Left)
			return nil
		},
	}
	// Plain quotes, not backquotes: pflag's UnquoteUsage reads a backquoted
	// span as the flag's argument name, which overrides the bool special case
	// and made `fu restore --help` print "--hard git reset --hard" -- a flag
	// that appears to take a value, with the comparison stripped out of the
	// description it was meant to be part of.
	cmd.Flags().BoolVar(&hard, "hard", false,
		"discard uncommitted changes to tracked files in the store worktree, like 'git reset --hard'; untracked files are left alone, but note fu commits .gitignored content too, so an ignored file it has already recorded is tracked")
	return cmd
}

func printResetPaths(out io.Writer, reset []string) {
	if len(reset) == 0 {
		return
	}
	fmt.Fprintf(out, "reset %d path(s) in the store worktree to the last commit:\n", len(reset))
	for _, path := range reset {
		fmt.Fprintf(out, "  %s\n", path)
	}
}

// printRefusedPaths reports the tracked, uncommitted content this run left
// alone -- the half of the store worktree that is inside the reset's
// union(index, HEAD) path set, and therefore the half `--hard` can act on.
//
// A function, and called from both branches of RunE, because engine.Restore
// fills Refused before it decides whether to return an error: it splits the
// dirty paths and only then joins the reconcile failure (restore.go). Printing
// this group only on the success path meant a run that hit one unreadable
// agent named the untracked group -- the one no flag can remove -- and stayed
// silent about the group that has a remedy, exactly inverting the ordering
// argument above.
func printRefusedPaths(errOut io.Writer, refused []string) {
	if len(refused) == 0 {
		return
	}
	fmt.Fprintln(errOut, "the store worktree was left alone; these changes are not committed:")
	for _, path := range refused {
		fmt.Fprintf(errOut, "  %s\n", path)
	}
	fmt.Fprintln(errOut, "record them with a write command, which commits pending hand edits first, or discard them with `fu restore --hard`")
}

// printKeptPaths reports the uncommitted content no invocation of this command
// touches -- untracked and ignored paths, which fall outside the reset's
// union(index, HEAD) path set.
//
// It prints on both paths deliberately. On the default path it keeps these
// names out of the group `--hard` is offered for, since suggesting a flag that
// provably will not remove them is advice with no terminating state. On the
// hard path it is the only account a user gets: the reset reports nothing when
// every dirty path is untracked, and a bare "restored agent links" is not an
// answer to an explicit request to discard.
func printKeptPaths(errOut io.Writer, left []string) {
	if len(left) == 0 {
		return
	}
	fmt.Fprintln(errOut, "these are untracked or ignored, so no restore touches them:")
	for _, path := range left {
		fmt.Fprintf(errOut, "  %s\n", path)
	}
	// The same remedy the tracked group is offered, because it applies here
	// too: fu's stageAll deliberately commits untracked *and* gitignored
	// content (DESIGN §4, README), so the next write command's sweep records
	// these exactly as it records a tracked edit. An earlier version said to
	// handle them "by hand", which was simply false -- and withheld from this
	// group the very remedy printed four lines above it.
	fmt.Fprintln(errOut, "record them with a write command, which commits them too, or delete them yourself; `--hard` will not")
}
