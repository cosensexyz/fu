// internal/cli/new.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"fu/internal/agent"
	"fu/internal/engine"
)

func newNewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new <name>",
		Short: "Scaffold a new skill in the store, enabled by default",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			res, err := engine.NewSkill(st, agent.Detected(), args[0])
			// engine.ErrOperationFailed (finding 4) means the skill itself
			// was scaffolded, registered, and committed -- Mutate/Save/Commit
			// all ran -- and only the reconcile-side link delivery hit a
			// genuine per-agent failure. That is durably true and worth
			// confirming even though the command must still exit non-zero,
			// unlike every other error here, which means nothing durable
			// happened at all.
			if err != nil && !errors.Is(err, engine.ErrOperationFailed) {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", args[0])
			printResult(cmd, res)
			return err
		},
	}
}
