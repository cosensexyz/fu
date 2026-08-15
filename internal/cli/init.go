package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type initApplication interface {
	Initialize() (engine.InitOutcome, error)
}

func newInitCmd(app initApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize an empty store under $FU_HOME/store",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := app.Initialize()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialized store at %s\n", outcome.Home)
			return nil
		},
	}
}
