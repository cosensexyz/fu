// internal/cli/revert.go
package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type revertApplication interface {
	Revert(n int) (engine.RevertOutcome, error)
}

// reportRevertedAdopts says that reverting an adopt did not put back what
// adopt moved aside, at the moment the user has just asked for a mistake to be
// undone.
//
// adopt is the only operation that displaces pre-existing user content. Every
// other one removes what it created or restores from history what it deleted,
// so their reverts really do return things to how they were; this one leaves
// the agent entry empty -- which is what the engine checks before putting a
// name here, rather than assuming it follows from the revert. Silent when no adopt was undone, because a line
// printed after every revert stops being read.
//
// Both of adopt's forms are named, because what the user has to do differs. A
// real directory adopt had to move was copied to recovery/ as adopt-archive-*,
// and that copy is the only one outside the store. A symlink was never moved:
// recovery/ holds adopt-link-*.json naming the original target, and the
// content is still there.
//
// The symlink arm says "the entry's own, or the agent's whole skills
// directory" because SPEC rule 10's shape is the second, and it is exactly
// what a two-way split keyed on the entry gets wrong: there the entry adopt
// took in *is* a real directory, yet nothing was copied -- what was displaced
// is the skills symlink itself, and the content never moved. Keyed on the
// entry alone, that user reads the adopt-archive-* arm and goes hunting for a
// directory that was never written.
func reportRevertedAdopts(cmd *cobra.Command, outcome engine.RevertOutcome) {
	names := outcome.DisplacedByRevertedAdopt
	if len(names) == 0 {
		return
	}
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "%d adopted %s among what was reverted, and reverting an adopt does not put back what it moved aside: %s\n",
		len(names), plural(len(names), "skill was", "skills were"), strings.Join(names, ", "))
	fmt.Fprintln(errOut, "  what was there before is recorded under $FU_HOME/recovery/: an adopt-archive-* copy")
	fmt.Fprintln(errOut, "  where adopt had to move a real directory, or an adopt-link-*.json naming the original")
	fmt.Fprintln(errOut, "  target where what it replaced was a symlink -- the entry's own, or the agent's whole")
	fmt.Fprintln(errOut, "  skills directory -- in which case that target is untouched and still holds the content.")
	fmt.Fprintln(errOut, "  No command restores either yet, so put it back by hand if you want it")
}

func newRevertCmd(app revertApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "revert <count>",
		Short: "Roll the store back the given number of operations",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 1 {
				// A usage error, like every other malformed-argument case:
				// `fu revert` with no argument already exits 2 through
				// usageArgs, and a count that is not a positive integer is the
				// same class of mistake. Without this the one command answered
				// two spellings of "you used me wrongly" with two different
				// exit codes.
				return &UsageError{fmt.Errorf("revert takes a positive operation count, got %q", args[0])}
			}
			outcome, err := app.Revert(n)
			printResult(cmd, outcome.Result)
			out := cmd.OutOrStdout()
			// Before the error is returned, on the same reasoning as
			// `fu restore --hard`: the worktree update runs ahead of the
			// commit, so a failure arriving now is a failure that already
			// moved these paths.
			if len(outcome.Changed) != 0 {
				fmt.Fprintf(out, "changed %d path(s) in the store worktree:\n", len(outcome.Changed))
				for _, path := range outcome.Changed {
					fmt.Fprintf(out, "  %s\n", path)
				}
			}
			// Before the error return, on the same reasoning as the changed
			// paths above: the worktree update runs ahead of the commit, so a
			// failure arriving now is a failure that has already dropped the
			// store copy of an adopted skill. Naming the paths that moved
			// while withholding that one of them was an adopt is the worse
			// half of both.
			reportRevertedAdopts(cmd, outcome)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "reverted %d operation(s)\n", n)
			printDeliveryHint(out, outcome.Result)
			return nil
		},
	}
}
