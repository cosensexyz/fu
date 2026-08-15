// internal/cli/adopt.go
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type adoptApplication interface {
	Adopt(engine.AdoptScope) (engine.AdoptResult, error)
}

func newAdoptCmd(app adoptApplication) *cobra.Command {
	var agentName string
	cmd := &cobra.Command{
		Use:   "adopt [--agent <a>]",
		Short: "Adopt existing skill entries into the store, switching them to fu links",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := engine.AdoptScope{All: true}
			if cmd.Flags().Changed("agent") {
				if agentName == "" {
					return &UsageError{engine.ErrEmptyAgentScope}
				}
				scope = engine.AdoptScope{Agent: agentName}
			}
			res, err := app.Adopt(scope)
			// A name no adapter answers to is a malformed flag value, the same
			// class as the empty one above, so it takes the same exit code
			// (DESIGN §7). `--agent nosuch` exiting 1 while `--agent ""` exited
			// 2 made one construct produce two exit-code classes. An agent that
			// is known but not installed here is not malformed and stays exit 1.
			if errors.Is(err, engine.ErrUnknownAgent) {
				return &UsageError{err}
			}
			for _, s := range res.Adopted {
				fmt.Fprintf(cmd.OutOrStdout(), "adopted %s (from %s)\n", s.Name, strings.Join(s.Agents, ", "))
				printDurableOutcome(cmd, "adopt", s.Operation)
			}
			for _, s := range res.Pending {
				printDurableOutcome(cmd, "adopt", s.Operation)
			}
			for _, name := range res.Conflicts {
				fmt.Fprintf(cmd.ErrOrStderr(), "conflict: %s: content differs across agents; left untouched\n", name)
			}
			for _, conflict := range res.PreflightConflicts {
				fmt.Fprintf(cmd.ErrOrStderr(), "conflict: %s: %v\n", conflict.Action.Skill, conflict.Err)
			}
			for _, name := range res.Skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %s: already managed\n", name)
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			for _, f := range res.Failed {
				// Adopt-level per-candidate failures are reported as
				// "invalid:" -- the same class as add's invalid candidates:
				// the rest of the run completed, so the command exits 0
				// (round 7 finding M2). Reconcile-level failures keep the
				// "failed:" label and exit 1 via ErrOperationFailed.
				switch {
				case f.Action.Skill != "":
					fmt.Fprintf(cmd.ErrOrStderr(), "invalid: %s: %v\n", f.Action.Skill, f.Err)
				case f.Action.AgentName != "":
					// An agent-level refusal carries no skill name, so the agent
					// is the only thing that locates it. Today's messages happen
					// to embed it themselves; relying on that makes the label
					// depend on how each message was worded.
					fmt.Fprintf(cmd.ErrOrStderr(), "invalid: %s: %v\n", f.Action.AgentName, f.Err)
				default:
					fmt.Fprintf(cmd.ErrOrStderr(), "invalid: %v\n", f.Err)
				}
			}
			printResult(cmd, res.Reconcile)
			// Nothing was found or reported at all: say so, so an empty
			// environment (or one where everything is already a fu link) is
			// not indistinguishable from "never checked" (round 8 finding M1).
			// The verdict itself is the engine's -- res.Empty() -- so a second
			// front end cannot disagree with this one about whether a run was
			// a no-op (round 18 finding I20).
			if err == nil && res.Empty() {
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing to adopt")
			}
			return err
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "limit adoption to one agent (claude|codex)")
	return cmd
}
