// internal/cli/delivery.go
package cli

import (
	"fmt"
	"io"

	"github.com/cosensexyz/fu/internal/engine"
)

// deliveryHint states when a change to the agent directories becomes visible,
// or says nothing at all.
//
// SPEC rule 8 requires fu to tell the user that a switch change applies to
// the next agent session rather than to whatever is already running, and CLI
// output is fu's only means of doing so. Every command that changes what is
// projected owes the same sentence -- not just enable and disable -- because
// the user's question ("do I need to restart my agent?") does not depend on
// which command moved the link.
//
// The claim is conditioned on the counts rather than printed unconditionally,
// for the same reason toggle's wording is softened when a conflict names the
// same skill: a pass that projected nothing gives a restart nothing to show,
// and saying otherwise sends the user to reopen an agent for no reason. A
// pass that changed some links while refusing others gets the qualified form,
// so the sentence stays true on its own even for a caller that captures only
// stdout and never sees the stderr diagnostics.
func deliveryHint(res engine.Result) string {
	if res.Created == 0 && res.Removed == 0 {
		return ""
	}
	// Exactly the five findings toggleDeliveryBlocked consults, minus its
	// per-skill targeting, which does not apply to a command whose scope is
	// the whole projection.
	//
	// Reserved and Invalid are deliberately not among them, though Diff
	// produces them alongside the rest. Both are standing properties of
	// fu.yaml rather than of this run: one badly spelled key would qualify
	// the sentence for every write command from then on, pointing the user at
	// a diagnostic about a name the run never touched. The five below all
	// describe something this pass met and declined to do.
	blocked := len(res.Conflicts) != 0 || len(res.DisabledForeign) != 0 || len(res.Missing) != 0 ||
		len(res.Skipped) != 0 || len(res.Failed) != 0
	if blocked {
		return "takes effect in new agent sessions for the links that changed; see diagnostics"
	}
	return "takes effect in new agent sessions"
}

// printDeliveryHint writes the hint on its own line, or nothing. Callers that
// confirm one object append it to their confirmation instead; this is for the
// batch commands, whose confirmations are per item while the hint describes
// the run.
func printDeliveryHint(out io.Writer, res engine.Result) {
	if hint := deliveryHint(res); hint != "" {
		fmt.Fprintln(out, hint)
	}
}

// hintSuffix is deliveryHint for a command that names one skill in its
// confirmation: `created alpha; takes effect in new agent sessions`, the
// shape enable and disable have always printed. Commands whose confirmation
// names no skill -- restore, revert, clone, pull -- or names several -- add,
// adopt, update -- use printDeliveryHint instead, so the sentence stands
// beside the run rather than one arbitrary item of it.
func hintSuffix(res engine.Result) string {
	if hint := deliveryHint(res); hint != "" {
		return "; " + hint
	}
	return ""
}
