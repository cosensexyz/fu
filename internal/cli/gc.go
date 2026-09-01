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
				// because that is all Files counts. Neither tree a run can
				// reclaim is tallied -- an rm family's quarantined payload
				// under recovery/, and the tree an update family replaced
				// under staging/ -- because each is a directory removed whole
				// against its manifest by a deletion primitive whose signature
				// is not worth widening to return a count for one line of
				// output. The staging one was briefly counted here, which is
				// how this line came to report a directory tree as a "recovery
				// journal and bookkeeping file"; PruneOutcome's own doc carries
				// the rest of that reasoning. An unqualified "recovery files"
				// would have undercounted against a promise this makes
				// explicit instead, and neither reclaim goes unmentioned: both
				// happen only on the way to pruning the family that describes
				// them, so the transaction count always moves with them.
				fmt.Fprintf(cmd.OutOrStdout(), "pruned %d completed transactions; removed %d recovery journal and bookkeeping files\n", outcome.Transactions, outcome.Files)
			}
			return err
		},
	}
}
