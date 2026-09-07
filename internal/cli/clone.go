package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type cloneApplication interface {
	Clone(url string) (engine.CloneOutcome, error)
}

func newCloneCmd(app cloneApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "clone <url>",
		Short: "Clone a remote store into $FU_HOME/store and rebuild every agent link",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == "" {
				return &UsageError{errors.New("clone takes a non-empty url")}
			}
			outcome, err := app.Clone(args[0])
			printResult(cmd, outcome.Result)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cloned store to %s from %s (%d skill(s))\n",
				filepath.Join(outcome.Home, "store"), outcome.URL, outcome.Skills)
			return nil
		},
	}
}
