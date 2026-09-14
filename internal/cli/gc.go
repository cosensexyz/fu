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
			if err == nil && outcome.Transactions == 0 && outcome.Files == 0 && outcome.Leases == 0 {
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
			printReclaimedPayloads(cmd, outcome)
			return err
		},
	}
}

// printReclaimedPayloads reports the staging half of a run: the abandoned
// temporaries whose leases showed their holders were gone, and the two kinds
// the run deliberately left alone.
//
// The kept kinds are said out loud rather than left as a silent difference,
// because each has a different meaning for the reader. Something in use will
// go on its own once its holder finishes; something fu cannot account for will
// not go at all, and is a thing for a person to look at.
func printReclaimedPayloads(cmd *cobra.Command, outcome engine.PruneOutcome) {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if outcome.StoreSkipped {
		// Said first, so the counts below are read as the whole of what this
		// run could do rather than as all there was.
		fmt.Fprintln(errOut, "no store here yet; swept the staging area only")
	}
	if outcome.Payloads != 0 {
		fmt.Fprintf(out, "reclaimed %d abandoned temporary %s\n",
			outcome.Payloads, plural(outcome.Payloads, "payload", "payloads"))
	}
	if outcome.Leases != 0 {
		// Said separately because it is a separate fact, and because saying
		// nothing was worse than saying it clumsily: a run that settled only
		// records printed no line at all and exited 0, right after `fu status`
		// had counted them as collectable and told the user to run this. That
		// is the convergent crash boundary the design names -- a holder
		// removed its object and died before dropping its lease -- so it is
		// the ordinary case, not an exotic one.
		//
		// It also keeps this command's arithmetic equal to status's: the
		// staging inventory counts entries, and a reclaimed payload is two of
		// them, its record and its object.
		fmt.Fprintf(out, "settled %d abandoned temporary payload %s\n",
			outcome.Leases, plural(outcome.Leases, "record", "records"))
	}
	if outcome.PayloadsClaimed != 0 {
		fmt.Fprintf(errOut, "left %d temporary %s an unfinished write still claims; `fu restore` settles those\n",
			outcome.PayloadsClaimed, plural(outcome.PayloadsClaimed, "payload", "payloads"))
	}
	if outcome.PayloadsInUse != 0 {
		fmt.Fprintf(errOut, "left %d temporary %s in use by another process\n",
			outcome.PayloadsInUse, plural(outcome.PayloadsInUse, "payload", "payloads"))
	}
	if outcome.PayloadsUnaccountable != 0 {
		// Entries, and counted from the list rather than from the sweep's own
		// tally, which is one per refused lease. Several of the shapes below
		// are a record and nothing else -- a lease whose name or token fu will
		// not believe never resolves to an object -- while others leave the
		// record and the object both. Counting leases and calling them entries
		// made `fu gc` say 1 where `fu status` said 2 about the same
		// directory, which is precisely what sharing a classifier is meant to
		// rule out.
		fmt.Fprintf(errOut, "left %d staging %s fu cannot account for\n",
			len(outcome.PayloadNotes), plural(len(outcome.PayloadNotes), "entry", "entries"))
		// Named here rather than by a pointer at `fu status`. That pointer
		// was false in the one home this command was given a storeless mode
		// for: with no store yet, `fu status` exits 1, so a user following
		// the advice got an error instead of the explanation. gc computed
		// the reasons itself, so gc says them.
		//
		// Fewer lines than the count whenever a refusal came with nothing to
		// say -- those keep the silence rather than getting a manufactured
		// sentence.
		for _, note := range outcome.PayloadNotes {
			fmt.Fprintf(errOut, "  %s: %s\n", note.Name, note.Reason)
		}
	}
}
