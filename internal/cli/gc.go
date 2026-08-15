package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type gcApplication interface {
	PruneRecovery() (engine.PruneOutcome, error)
}

func newGCCmd(app gcApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Safely prune completed recovery journals",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			outcome, err := app.PruneRecovery()
			if err == nil && outcome.Transactions == 0 && outcome.Files == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing to prune")
			} else if outcome.Transactions != 0 || outcome.Files != 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "pruned %d completed transactions (%d files)\n", outcome.Transactions, outcome.Files)
			}
			return err
		},
	}
}
