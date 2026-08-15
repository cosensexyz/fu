// internal/cli/rm.go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type rmApplication interface {
	RemoveSkill(string) (engine.RemoveOutcome, error)
}

func newRmCmd(app rmApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a skill from the store and every agent",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			outcome, err := app.RemoveSkill(name)
			if err != nil && !outcome.Operation.DurablyStarted() {
				return err
			}
			// Diagnostics before confirmation, as toggle.go states the rule: a
			// reader watching both streams together gets the caveat leading
			// into the claim it may qualify, rather than contradicting it a
			// line later.
			printDurableOutcome(cmd, "remove", outcome.Operation)
			printResult(cmd, outcome.Operation.Reconcile)
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", name)
			return err
		},
	}
}
