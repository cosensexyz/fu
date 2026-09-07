package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type remoteApplication interface {
	Remote() (engine.RemoteOutcome, error)
	SetRemote(url string) (engine.RemoteOutcome, error)
}

func newRemoteCmd(app remoteApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "remote [url]",
		Short: "Show the store's remote, or set it",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				outcome, err := app.Remote()
				if err != nil {
					return err
				}
				if !outcome.Configured {
					fmt.Fprintln(out, "no remote configured; set one with fu remote <url>")
					return nil
				}
				fmt.Fprintln(out, outcome.URL)
				return nil
			}
			if args[0] == "" {
				return &UsageError{errors.New("remote takes a non-empty url")}
			}
			outcome, err := app.SetRemote(args[0])
			if err != nil {
				return err
			}
			if outcome.Previous != "" && outcome.Previous != outcome.URL {
				fmt.Fprintf(out, "remote set to %s (was %s)\n", outcome.URL, outcome.Previous)
				return nil
			}
			fmt.Fprintf(out, "remote set to %s\n", outcome.URL)
			return nil
		},
	}
}
