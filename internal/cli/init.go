package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"fu/internal/store"
)

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize an empty store under $FU_HOME/store",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := store.Home()
			if err != nil {
				return err
			}
			if _, err := store.Init(home); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialized store at %s\n", home)
			return nil
		},
	}
}
