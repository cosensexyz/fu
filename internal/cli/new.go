// internal/cli/new.go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type newApplication interface {
	NewSkill(string) (engine.OperationOutcome, error)
}

func newNewCmd(app newApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "new <name>",
		Short: "Scaffold a new skill in the store, enabled by default",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := app.NewSkill(args[0])
			// engine.ErrOperationFailed (finding 4) means the skill itself
			// was scaffolded, registered, and committed -- Mutate/Save/Commit
			// all ran -- and only the reconcile-side link delivery hit a
			// genuine per-agent failure. That is durably true and worth
			// confirming even though the command must still exit non-zero,
			// unlike every other error here, which means nothing durable
			// happened at all.
			if err != nil && !outcome.DurablyStarted() {
				return err
			}
			// Diagnostics before confirmation, as toggle.go states the rule.
			printDurableOutcome(cmd, "create", outcome)
			printResult(cmd, outcome.Reconcile)
			if !outcome.RecoveryPending {
				fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", args[0])
			}
			return err
		},
	}
}
