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
				// The two counts are reported as separate clauses because they
				// no longer describe one thing: the file count covers the
				// reclaimed config-exchange bookkeeping as well as the pruned
				// journal entries, so a run that swept only residue has files
				// to its name and no transactions. Parenthesising the files
				// after the transactions reads as "0 transactions, which are 3
				// files".
				//
				// The clause names journal and bookkeeping files specifically
				// because that is all Files counts. A reclaimed rm payload is
				// a directory tree removed whole against its manifest, and it
				// is not tallied -- counting it would mean returning a file
				// count from ReclaimRecoveryPayloadOwned, which is a deletion
				// primitive whose signature is not worth widening for one line
				// of output. An unqualified "recovery files" would have
				// undercounted against a promise this makes explicit instead.
				fmt.Fprintf(cmd.OutOrStdout(), "pruned %d completed transactions; removed %d recovery journal and bookkeeping files\n", outcome.Transactions, outcome.Files)
			}
			return err
		},
	}
}
