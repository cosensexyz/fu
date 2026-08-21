// internal/cli/revert.go
package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type revertApplication interface {
	Revert(n int) (engine.RevertOutcome, error)
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
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "reverted %d operation(s)\n", n)
			return nil
		},
	}
}
