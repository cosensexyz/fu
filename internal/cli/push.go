package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type pushApplication interface {
	Push() (engine.PushOutcome, error)
}

func newPushCmd(app pushApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "push",
		Short: "Record pending hand edits, then push the store's branch to its remote",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := app.Push()
			printResult(cmd, outcome.Result)
			if err != nil && !outcome.Completed {
				return err
			}
			out := cmd.OutOrStdout()
			// push reconciles before it pushes -- its prologue is a full
			// write-command prologue -- so it projects the links an agent
			// detected since the last write command was owed, and does so on
			// a run whose own output is entirely about the remote. Printed on
			// the up-to-date arm too: nothing went to the remote there, and
			// the local projection may still have moved.
			if outcome.UpToDate {
				fmt.Fprintf(out, "already up to date with %s\n", outcome.URL)
				printDeliveryHint(out, outcome.Result)
				return err
			}
			fmt.Fprintf(out, "pushed %s (%s) to %s\n", outcome.Branch, outcome.Head, outcome.URL)
			printDeliveryHint(out, outcome.Result)
			return err
		},
	}
}
