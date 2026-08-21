// internal/cli/status.go
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type statusApplication interface {
	Status() (engine.StatusOutcome, error)
}

// driftLabel names a drift action in the user's terms rather than the engine's.
// The engine's ActionType says what reconcile would do; the user needs to know
// what is wrong.
func driftLabel(action engine.Action) string {
	switch action.Type {
	case engine.CreateLink:
		return "missing link"
	case engine.RemoveLink:
		return "stale link"
	case engine.ReportConflict:
		return "occupied by unmanaged content"
	case engine.ReportDisabledForeign:
		return "off, but occupied by unmanaged content"
	case engine.ReportMissing:
		return "enabled, but the store no longer holds it"
	case engine.ReportReserved:
		return "reserved name, never linked"
	case engine.ReportInvalid:
		return "invalid name, never linked"
	case engine.ReportForeign:
		return "unmanaged"
	}
	return "unknown"
}

// Each section prints its own heading and reports whether it printed
// anything, so the three read alike and the clean-report confirmation has one
// thing to test. A heading with nothing under it is scaffold, not a report, so
// every gate is computed before its heading is written.

func printStoreSection(out io.Writer, status engine.StoreStatus) bool {
	if len(status.DirtyPaths) == 0 && len(status.Pending) == 0 {
		return false
	}
	fmt.Fprintln(out, "store")
	for _, path := range status.DirtyPaths {
		fmt.Fprintf(out, "  uncommitted  %s\n", path)
	}
	for _, pending := range status.Pending {
		// Hedged, because a pending transaction is not always something the
		// next write command carries to a conclusion: recovery can also hit a
		// safe conflict, and then every write command stops at the recovery
		// entry point with a remedy until it is repaired by hand. Telling the
		// two apart takes running recovery, which a read-only command must
		// not do.
		fmt.Fprintf(out, "  unfinished   %s %s (the next write command settles it, or says what needs repairing)\n", pending.Op, pending.Name)
	}
	return true
}

func printAgentSection(out io.Writer, agents []engine.AgentStatus) bool {
	reported := false
	for _, agentStatus := range agents {
		if agentStatus.ScanErr != "" || agentStatus.DirIsSymlink || agentStatus.DirMissing || len(agentStatus.Drift) != 0 {
			reported = true
			break
		}
	}
	if !reported {
		return false
	}
	fmt.Fprintln(out, "agents")
	width := driftColumnWidth(agents)
	for _, agentStatus := range agents {
		if agentStatus.ScanErr != "" {
			fmt.Fprintf(out, "  %s: could not be inspected: %s\n", agentStatus.Name, agentStatus.ScanErr)
			continue
		}
		// Both preconditions print their own line and then suppress the
		// per-skill projection drift beneath it -- but only that drift.
		//
		// Suppressing it is right: every enabled skill is missing its link for
		// the single reason the line above already gives, so a store with
		// fifty skills produced fifty-one lines saying the same thing, and
		// SPEC rule 4 asks a read-only command to report a newly detected
		// agent as awaiting projection and stop.
		//
		// Suppressing the rest is not. Status puts Desired's own
		// ReportReserved and ReportInvalid findings in this same slice, and
		// readDiagnostics suppresses the stderr `invalid:` line on the
		// explicit premise that the per-agent finding covers it -- so dropping
		// the slice wholesale left a reserved, invalid name in fu.yaml
		// reported on neither stream, for this command alone. Rule 4 is about
		// projection, not about configuration diagnostics.
		//
		// ReportMissing is left unsuppressed as a store-side fact, and that
		// reading is unconditional only for DirMissing. For DirIsSymlink it
		// holds because engine.Status refuses to compute per-entry drift for
		// such an agent at all (status.go): ScanAgent reports no Entries for a
		// symlinked directory, so Diff would answer every enabled skill with
		// CreateLink and storeSideMissing would upgrade the ones whose store
		// content is absent to ReportMissing -- a store-side claim caused
		// entirely by the agent's own directory. That guard is what keeps this
		// argument true, and
		// TestStatusDescribesNoPerEntryDriftForASymlinkedAgentDir pins it.
		suppressProjection := false
		switch {
		case agentStatus.DirIsSymlink:
			fmt.Fprintf(out, "  %s: skills dir is a symlink; run `fu adopt` to convert it\n", agentStatus.Name)
			suppressProjection = true
		case agentStatus.DirMissing:
			fmt.Fprintf(out, "  %s: detected, nothing projected yet\n", agentStatus.Name)
			suppressProjection = true
		}
		for _, action := range agentStatus.Drift {
			if suppressProjection && action.Type == engine.CreateLink {
				continue
			}
			fmt.Fprintf(out, "  %-*s %s/%s\n", width, driftLabel(action), agentStatus.Name, action.Skill)
		}
	}
	return true
}

// driftColumnWidth sizes the label column to the widest label this report
// actually prints, with the historical 14 as a floor.
//
// A fixed 14 fitted three of the eight labels; "enabled, but the store no
// longer holds it" is 41 characters, so any report containing one came out
// ragged. Sizing to the report rather than to the longest label that exists
// keeps the common case looking exactly as it did -- the floor is what does
// that -- while an unusual finding widens only the report it appears in.
//
// It deliberately does not mirror printAgentSection's own two skips. That
// mirror was here, and both halves of it were provably inert: the only label
// the projection suppression removes is CreateLink's "missing link", which is
// twelve characters and can never beat the floor, and engine.Status fills no
// Drift at all for an agent it could not scan. What the mirror did produce was
// a second place that had to be kept in step with the printing loop by hand,
// for a column width that could not change either way.
func driftColumnWidth(agents []engine.AgentStatus) int {
	width := 14
	for _, agentStatus := range agents {
		for _, action := range agentStatus.Drift {
			if n := len(driftLabel(action)); n > width {
				width = n
			}
		}
	}
	return width
}

func printRecoverySection(out io.Writer, inventory engine.RecoveryInventory) bool {
	if inventory == (engine.RecoveryInventory{}) {
		return false
	}
	fmt.Fprintln(out, "recovery")
	if inventory.Collectable != 0 {
		fmt.Fprintf(out, "  %d collectable (run `fu gc`)\n", inventory.Collectable)
	}
	if inventory.Blocked != 0 {
		// Hedged the way printStoreSection hedges its unfinished line, and for
		// the same reason: recovery can hit a safe conflict instead of settling,
		// and then `fu restore` exits 1 with a repair remedy while this count
		// does not move. A damaged exchange record does exactly that.
		fmt.Fprintf(out, "  %d waiting on an unfinished write (run `fu restore`, which settles it or says what needs repairing, then `fu gc`)\n", inventory.Blocked)
	}
	// Retained and Uncollectable have no remedy between them -- gc keeps the
	// first by design and has never had a plan for the second -- so both stay
	// deliberately inactionable: they say what is accumulating and stop there.
	if inventory.Retained != 0 {
		fmt.Fprintf(out, "  %d kept on purpose (what `fu adopt` set aside)\n", inventory.Retained)
	}
	if inventory.Uncollectable != 0 {
		fmt.Fprintf(out, "  %d that no command collects yet\n", inventory.Uncollectable)
	}
	return true
}

// plural picks a singular or plural suffix for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// printStagingSection reads like the recovery section with one line's worth of
// difference, which is the whole point of keeping them apart. `fu gc` never
// looks at staging, so the waiting line stops at `fu restore` rather than
// repeating recovery's "then `fu gc`"; and there is no collectable line at all,
// because nothing under staging carries the ownership evidence a later run
// would need to claim it.
func printStagingSection(out io.Writer, inventory engine.StagingInventory) bool {
	if inventory == (engine.StagingInventory{}) {
		return false
	}
	fmt.Fprintln(out, "staging")
	if inventory.Blocked != 0 {
		fmt.Fprintf(out, "  %d waiting on an unfinished write (run `fu restore`)\n", inventory.Blocked)
	}
	// Same deliberately inactionable wording as the recovery section's: this
	// residue has no remedy today, so the line says what is accumulating and
	// stops, rather than implying the reader may go delete it.
	if inventory.Uncollectable != 0 {
		fmt.Fprintf(out, "  %d that no command collects yet\n", inventory.Uncollectable)
	}
	// This one does have a remedy, and it is not fu's to perform: the content
	// belongs to whoever put it there. The line exists so a user whom `fu new`
	// or `fu add` has just refused can find out what is occupying the name
	// without having to trigger the refusal again to read it.
	//
	// Worded narrowly on purpose. Those commands do not refuse because staging
	// holds something -- they refuse when it holds the name of the skill being
	// installed (ops.go, add.go), so `fu new bar` runs happily past a staging
	// entry called foo. And the bucket catches every name that is neither
	// claimed nor recognised residue, which need not be a public name at all.
	// Saying more than "fu has no pending record for these, and they will
	// block reuse of their own names" would promise a refusal that may not
	// come.
	if inventory.Unmatched != 0 {
		fmt.Fprintf(out, "  %d staging entr%s fu has no pending record for (`fu new` and `fu add` refuse to reuse these names)\n",
			inventory.Unmatched, plural(inventory.Unmatched, "y", "ies"))
	}
	return true
}

func newStatusCmd(app statusApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report differences between what fu.yaml asks for and what is on disk",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Printed before the error is returned, not instead of it:
			// Application.Status hands back whatever it assembled alongside a
			// failure, and a user reaching for `fu status` because the store
			// looks damaged needs that much. The error still reaches the
			// caller below, so the exit code is unchanged.
			outcome, err := app.Status()
			printVersionWarning(cmd, outcome.Diagnostics)
			printInvalidNames(cmd, outcome.Diagnostics)
			out := cmd.OutOrStdout()
			report := outcome.Report

			reported := printStoreSection(out, report.Store)
			reported = printAgentSection(out, report.Agents) || reported
			reported = printRecoverySection(out, report.Recovery) || reported
			reported = printStagingSection(out, report.Staging) || reported
			// Silence would be the same output a command that did nothing at
			// all produces, leaving a reader unable to tell "everything
			// matches" from "this never looked". Not claimed when err is set:
			// some section was then never assembled, so there is nothing to
			// vouch for.
			if !reported && err == nil {
				fmt.Fprintln(out, "nothing to report: what fu.yaml asks for is what is on disk")
			}
			return err
		},
	}
}
