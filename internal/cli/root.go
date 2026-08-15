package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

// NewRootCmd builds the fu command tree. Tests construct a fresh tree
// per case, so no package-level command state is kept.
func NewRootCmd() *cobra.Command {
	app := engine.NewApplication()
	root := &cobra.Command{
		Use:           "fu",
		Short:         "fu (符) — local agent skill manager",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// An unknown flag is a usage error (finding I7, DESIGN §7 exit code 2),
	// classified here at the one place cobra itself generates it; child
	// commands inherit this via FlagErrorFunc's own parent walk.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &UsageError{err}
	})
	root.AddCommand(newInitCmd(app), newNewCmd(app))
	root.AddCommand(newToggleCmd(app, "enable", true), newToggleCmd(app, "disable", false))
	root.AddCommand(newListCmd(app), newShowCmd(app))
	root.AddCommand(newAddCmd(app), newRmCmd(app), newAdoptCmd(app), newGCCmd(app))
	return root
}

// printVersionWarning warns when the loaded fu.yaml version exceeds what
// this build supports (DESIGN §3). A write command refuses to touch such a
// config at all (store.Config.CheckWritable, enforced by engine.Run before
// Sweep); list and show have no mutation to refuse and proceed best-effort
// instead, so they must say so explicitly -- some content in a schema
// version newer than store.SupportedVersion may not be something this
// build's accessors know how to interpret. Printed to stderr like every
// other diagnostic in this file, and names the config's own path
// explicitly, the same way printInvalidNames does just below.
func printVersionWarning(cmd *cobra.Command, diagnostics engine.ReadDiagnostics) {
	if !diagnostics.VersionTooNew {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s has a version newer than this build supports; some content may not be understood\n", diagnostics.ConfigPath)
}

// printInvalidNames reports any skill name store.LoadConfig found invalid
// and excluded from the config's skill set (round 4 finding 2). list and
// show are read-only and never run Reconcile, so they never go through
// printResult's own Invalid case below -- without a call of their own,
// they would simply say nothing about an invalid entry, silently omitting
// it from the matrix/detail view rather than surfacing it. Printed to
// stderr like every other diagnostic in this file, and names the config's
// own path explicitly: an invalid name is recoverable only by hand-editing
// fu.yaml today. `fu rm` now reports the same repair path when asked for the
// isolated name, while list/show remain the read commands a user confused by
// a missing skill reaches for first.
func printInvalidNames(cmd *cobra.Command, diagnostics engine.ReadDiagnostics) {
	out := cmd.ErrOrStderr()
	for _, inv := range diagnostics.InvalidNames {
		fmt.Fprintf(out, "invalid: skill name %q fails validation (%s) and is ignored; edit %s to fix or remove it\n",
			inv.Name, inv.Reason, diagnostics.ConfigPath)
	}
}

// printResult surfaces reconcile findings after a write command. These are
// diagnostics, not the command's own output, so they go to stderr (finding
// 4) -- printed regardless of whether the command's overall confirmation
// line went out already, and regardless of whether the pass as a whole
// exits 0 (Conflicts/DisabledForeign/Missing/Reserved/Invalid/Skipped) or
// 1 (Failed; see engine.ErrOperationFailed's doc comment for that
// decision). Before this, every line below went to stdout, so `fu new x
// >/dev/null` silently swallowed a "failed:" line a script had no other
// way to notice.
func printResult(cmd *cobra.Command, res engine.Result) {
	out := cmd.ErrOrStderr()
	for _, warning := range res.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
	for _, report := range res.UserReports() {
		action := report.Action
		switch report.Kind {
		case engine.ReportConflict:
			// Target is set only when fu moved its own link aside and could
			// not put it back because the name was reoccupied; the user needs
			// to be told where their content went (round 18 finding M10).
			if action.Target != "" {
				fmt.Fprintf(out, "conflict: %s/%s occupied by unmanaged content; fu's own link was moved to %s\n", action.AgentName, action.Skill, action.Target)
			} else {
				fmt.Fprintf(out, "conflict: %s/%s occupied by unmanaged content\n", action.AgentName, action.Skill)
			}
		case engine.ReportDisabledForeign:
			fmt.Fprintf(out, "disabled-foreign: %s/%s is off but occupied by unmanaged content; fu left it alone, so the skill may still be loaded every session\n", action.AgentName, action.Skill)
		case engine.ReportMissing:
			fmt.Fprintf(out, "missing: %s/%s is enabled but the store no longer holds its content\n", action.AgentName, action.Skill)
		case engine.ReportReserved:
			fmt.Fprintf(out, "reserved: %s/%s is a reserved name and will never be linked\n", action.AgentName, action.Skill)
		case engine.ReportInvalid:
			// An empty AgentName marks a config-level finding: a bad key in
			// fu.yaml is one fact about one file, reported once for the pass
			// (engine.configInvalidNames), not once per agent. The per-agent
			// form below is reached only by engine.Desired's defence-in-depth
			// check, for a Config built without going through LoadConfig.
			if action.AgentName == "" {
				fmt.Fprintf(out, "invalid: skill name %q fails validation and will never be linked\n", action.Skill)
				continue
			}
			fmt.Fprintf(out, "invalid: %s: skill name %q fails validation and will never be linked\n", action.AgentName, action.Skill)
		case engine.ReportSkipped:
			fmt.Fprintf(out, "skipped agent %s: its skills dir is a symlink; run `fu adopt` to convert it, or unlink manually\n", action.AgentName)
		case engine.ReportFailed:
			if action.AgentName != "" && action.Skill != "" {
				fmt.Fprintf(out, "failed: %s/%s: %v\n", action.AgentName, action.Skill, report.Err)
			} else if action.Skill != "" {
				fmt.Fprintf(out, "failed: %s: %v\n", action.Skill, report.Err)
			} else {
				fmt.Fprintf(out, "failed: %s: %v\n", action.AgentName, report.Err)
			}
		}
	}
}
