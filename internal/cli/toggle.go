// internal/cli/toggle.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/engine"
)

func newToggleCmd(use string, on bool) *cobra.Command {
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
			st, err := openStore()
			if err != nil {
				return err
			}
			// --agent must distinguish "not given" from "given as empty"
			// (finding I8): agentName == "" conflates the two, so
			// `--agent ""` used to fall into the global branch and perform
			// a broader change than requested. Changed() reflects only
			// whether the flag was actually passed on the command line; an
			// explicit empty value then falls through to SetAgentSwitch and
			// is rejected there as an unknown agent, like any other bogus
			// name.
			//
			// targetAgents records which agent(s) this invocation actually
			// scoped itself to (round 3 finding 1): a global toggle can
			// affect any detected agent (every one of them follows the
			// global unless it holds its own override), but an --agent
			// toggle only ever touches that one agent's switch. Reconcile
			// itself always runs over every detected agent regardless of
			// this command's scope (a write command's reconcile also picks
			// up e.g. a newly-detected agent), so skillBlocked below must
			// filter *its* diagnostics down to this narrower set itself --
			// otherwise a problem on some agent this command never touched
			// would still soften a confirmation that names a different one.
			detected := agent.Detected()
			var res engine.Result
			var targetAgents []string
			if !cmd.Flags().Changed("agent") {
				res, err = engine.SetGlobal(st, detected, args[0], on)
				for _, a := range detected {
					targetAgents = append(targetAgents, a.Name())
				}
			} else {
				res, err = engine.SetAgentSwitch(st, detected, args[0], agentName, on)
				targetAgents = []string{agentName}
			}
			// engine.ErrOperationFailed (finding 4) means fu.yaml was
			// already updated and committed -- only reconcile-side link
			// delivery hit a genuine per-agent failure -- so the
			// confirmation below is still durably true even though the
			// command must still exit non-zero. Any other error means
			// nothing was changed at all.
			if err != nil && !errors.Is(err, engine.ErrOperationFailed) {
				return err
			}
			// Diagnostics before confirmation (round 2 finding 5): printed
			// first so a reader watching both streams together (the
			// ordinary terminal case) has the caveat lead into the claim it
			// may qualify, rather than contradicting it a line later.
			printResult(cmd, res)
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
			switch blocked := skillBlocked(res, args[0], targetAgents); {
			case cmd.Flags().Changed("agent") && blocked:
				fmt.Fprintf(out, "%s %s for %s; may not take effect -- see diagnostics\n", verb, args[0], agentName)
			case cmd.Flags().Changed("agent"):
				fmt.Fprintf(out, "%s %s for %s; takes effect in new agent sessions\n", verb, args[0], agentName)
			case blocked:
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

// skillBlocked reports whether res records, for name and for one of
// targetAgents specifically, a conflict, a disabled-foreign report, a
// missing-store-content report, a failure, or a skipped agent: the
// diagnostics that directly contradict a "takes effect" claim (round 2
// finding 5, extended to DisabledForeign, and now to Missing/Failed's
// agent-level form/Skipped -- round 3 finding 1). Reserved/Invalid are
// about other cells of the state matrix (a name that collides with a
// reserved entry, or fails naming validation) and are not what either
// finding's fix asks to soften against.
//
// Two corrections over the previous version, both from the same round 3
// finding: matching must be scoped to the (skill, agent) pair, not skill
// alone -- Reconcile always runs over every detected agent regardless of
// this command's own --agent scope, so an unrelated agent's conflict (e.g.
// a `disable alpha --agent codex` call, with claude's alpha link disturbed
// by foreign content) used to soften a confirmation about an agent the
// command never touched. targetAgents is exactly the set this invocation
// scoped itself to (every detected agent for a global toggle, or the one
// named by --agent), computed by the caller.
//
// Second, a Failed or Skipped entry can be agent-level rather than
// skill-level: Reconcile records a broken ScanAgent (e.g. the agent's
// skills dir being a non-directory) as a placeholder Action carrying only
// AgentName, Skill == "" -- so the old `f.Action.Skill == name` comparison
// could never match it, letting the strongest form of the bug (an agent
// that received literally nothing) through with an entirely unqualified
// confirmation. Skipped is agent-level by construction (SPEC rule 10: the
// agent's own skills dir is a symlink, so its entry is a bare agent name,
// not an Action at all) and is treated the same way: either kind blocks
// every skill on that agent, matched or not.
//
// Foreign is still not surfaced by printResult at all -- it is
// informational inventory for a name fu.yaml has no opinion on at all,
// deferred to a future `fu status` -- so it still cannot contradict
// anything here.
func skillBlocked(res engine.Result, name string, targetAgents []string) bool {
	targeted := make(map[string]bool, len(targetAgents))
	for _, a := range targetAgents {
		targeted[a] = true
	}
	for _, c := range res.Conflicts {
		if c.Skill == name && targeted[c.AgentName] {
			return true
		}
	}
	for _, d := range res.DisabledForeign {
		if d.Skill == name && targeted[d.AgentName] {
			return true
		}
	}
	for _, m := range res.Missing {
		if m.Skill == name && targeted[m.AgentName] {
			return true
		}
	}
	for _, f := range res.Failed {
		if targeted[f.Action.AgentName] && (f.Action.Skill == "" || f.Action.Skill == name) {
			return true
		}
	}
	for _, a := range res.Skipped {
		if targeted[a] {
			return true
		}
	}
	return false
}
