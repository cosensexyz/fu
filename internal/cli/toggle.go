// internal/cli/toggle.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type toggleApplication interface {
	SetGlobal(string, bool) (engine.ToggleOutcome, error)
	SetAgent(string, string, bool) (engine.ToggleOutcome, error)
}

func newToggleCmd(app toggleApplication, use string, on bool) *cobra.Command {
	var agentName string
	verb := "disabled"
	if on {
		verb = "enabled"
	}
	cmd := &cobra.Command{
		Use:   use + " <name>",
		Short: use + " a skill globally, or for one agent with --agent",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --agent must distinguish "not given" from "given as empty"
			// (finding I8): agentName == "" conflates the two, so
			// `--agent ""` used to fall into the global branch and perform
			// a broader change than requested. Changed() reflects only
			// whether the flag was actually passed on the command line. An
			// explicit empty value is a usage error, the same shell mistake
			// `fu adopt --agent ""` already exits 2 for: routing it through
			// SetAgentSwitch instead made one construct produce two exit-code
			// classes across the CLI (round 18 finding M20).
			//
			if cmd.Flags().Changed("agent") && agentName == "" {
				return &UsageError{engine.ErrEmptyAgentScope}
			}
			var outcome engine.ToggleOutcome
			var err error
			if !cmd.Flags().Changed("agent") {
				outcome, err = app.SetGlobal(args[0], on)
			} else {
				outcome, err = app.SetAgent(args[0], agentName, on)
			}
			// Same class as the empty --agent above, so the same exit code
			// (DESIGN §7): a name no adapter answers to is a malformed flag
			// value. A known but currently undetected agent is not malformed;
			// toggle intentionally persists its override for future sessions.
			if errors.Is(err, engine.ErrUnknownAgent) {
				return &UsageError{err}
			}
			// engine.ErrOperationFailed (finding 4) means fu.yaml was
			// already updated and committed -- only reconcile-side link
			// delivery hit a genuine per-agent failure -- so the
			// confirmation below is still durably true even though the
			// command must still exit non-zero. Any other error means
			// nothing was changed at all.
			if err != nil && !outcome.Operation.DurablyStarted() {
				return err
			}
			// Diagnostics before confirmation (round 2 finding 5): printed
			// first so a reader watching both streams together (the
			// ordinary terminal case) has the caveat lead into the claim it
			// may qualify, rather than contradicting it a line later.
			//
			// The rule holds for the commands that confirm one object --
			// enable, disable, new, rm -- which all follow it. add and adopt
			// deliberately do not: both report per item, and a batch's
			// diagnostics belong next to the item that produced them, not
			// hoisted ahead of every confirmation in the run. Stating the
			// scope here keeps that from reading as three commands ignoring
			// the rule.
			printDurableOutcome(cmd, use, outcome.Operation)
			printResult(cmd, outcome.Operation.Reconcile)
			// Confirm what changed and when it takes effect (finding I9):
			// SPEC rule 8 makes this output fu's only means of conveying
			// that a switch change applies to the next agent session, not
			// to whatever is already running. But that claim must not
			// stand unqualified when this same pass recorded a conflict or
			// a failure against this exact skill (round 2 finding 5):
			// "takes effect" is not durably true for at least one target
			// agent then, and a caller who captures only stdout would
			// never see the stderr diagnostics that say so. Softening the
			// wording keeps the confirmation honest on its own, whether or
			// not the diagnostics above are visible to whoever reads it.
			out := cmd.OutOrStdout()
			switch {
			case cmd.Flags().Changed("agent") && outcome.DeliveryBlocked:
				fmt.Fprintf(out, "%s %s for %s; may not take effect -- see diagnostics\n", verb, args[0], agentName)
			case cmd.Flags().Changed("agent"):
				fmt.Fprintf(out, "%s %s for %s; takes effect in new agent sessions\n", verb, args[0], agentName)
			case outcome.DeliveryBlocked:
				fmt.Fprintf(out, "%s %s globally; may not take effect for every agent -- see diagnostics\n", verb, args[0])
			default:
				fmt.Fprintf(out, "%s %s globally; takes effect in new agent sessions\n", verb, args[0])
			}
			return err
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "limit to one agent (claude|codex)")
	return cmd
}
