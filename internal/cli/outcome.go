package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

// printDurableOutcome explains incomplete work after a mutation was already
// committed. A non-zero exit status must never make durable state look as if
// it was not applied.
func printDurableOutcome(cmd *cobra.Command, action string, outcome engine.OperationOutcome) {
	if !outcome.DurablyStarted() {
		return
	}
	// Branch on the earliest incomplete phase, never on RecoveryPending.
	// Gating the two post-commit cases on RecoveryPending sent an operation
	// with no transaction record -- enable/disable, whose commit was written
	// but failed -- straight past them into the canonical-path case, which
	// blamed a phase the run never reached for a failure that was the commit
	// itself (round 18 finding I4). Recovery is a separate axis: whether a
	// later write command will finish the work, not which phase stopped.
	var phase string
	switch {
	case !outcome.PostCommitComplete:
		phase = "post-commit work did not complete"
	case !outcome.WALComplete:
		phase = "WAL completion failed"
	case !outcome.CanonicalChecked:
		phase = "canonical-path verification did not complete"
	case !outcome.ReconcileComplete:
		phase = "agent reconciliation did not complete"
	default:
		return
	}
	recovery := ""
	if outcome.RecoveryPending {
		switch action {
		case "add", "create":
			recovery = "; recovery pending: the next write command will roll back this incomplete install"
		default:
			recovery = "; recovery pending for the next write command"
		}
	}
	durableState := "committed"
	if !outcome.Committed {
		durableState = "completed without a commit"
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s %s %s; %s%s\n", action, outcome.Name, durableState, phase, recovery)
}
