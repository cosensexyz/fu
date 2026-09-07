package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type pullApplication interface {
	Pull() (engine.PullOutcome, error)
}

func newPullCmd(app pullApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "pull",
		Short: "Fetch and fast-forward the store's branch, then rebuild agent links",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := app.Pull()
			printResult(cmd, outcome.Result)
			if err != nil && !outcome.Completed {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case outcome.EmptyRemote:
				fmt.Fprintf(out, "remote %s has no commits yet; nothing to pull\n", outcome.URL)
			case outcome.UpToDate:
				fmt.Fprintf(out, "already up to date with %s\n", outcome.URL)
			default:
				fmt.Fprintf(out, "fast-forwarded %s %s..%s, %d path(s) changed\n", outcome.Branch, outcome.From, outcome.To, len(outcome.Changed))
			}
			return err
		},
	}
}
