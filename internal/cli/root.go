package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"fu/internal/engine"
	"fu/internal/store"
)

// NewRootCmd builds the fu command tree. Tests construct a fresh tree
// per case, so no package-level command state is kept.
func NewRootCmd() *cobra.Command {
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
	root.AddCommand(newInitCmd(), newNewCmd())
	root.AddCommand(newToggleCmd("enable", true), newToggleCmd("disable", false))
	root.AddCommand(newListCmd(), newShowCmd())
	return root
}

// openStore is the shared preamble of every command needing a store.
func openStore() (*store.Store, error) {
	home, err := store.Home()
	if err != nil {
		return nil, err
	}
	return store.Open(home)
}

// openStoreAndConfig is the shared preamble of every read command (list,
// show, and more arriving in the next plan): open the store and load its
// config together, so each command's RunE does not repeat both calls.
func openStoreAndConfig() (*store.Store, *store.Config, error) {
	st, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		return nil, nil, err
	}
	return st, cfg, nil
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
func printVersionWarning(cmd *cobra.Command, st *store.Store, cfg *store.Config) {
	if !cfg.VersionTooNew() {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s has a version newer than this build supports; some content may not be understood\n", st.ConfigPath())
}

// printInvalidNames reports any skill name store.LoadConfig found invalid
// and excluded from the config's skill set (round 4 finding 2). list and
// show are read-only and never run Reconcile, so they never go through
// printResult's own Invalid case below -- without a call of their own,
// they would simply say nothing about an invalid entry, silently omitting
// it from the matrix/detail view rather than surfacing it. Printed to
// stderr like every other diagnostic in this file, and names the config's
// own path explicitly: an invalid name is recoverable only by hand-editing
// fu.yaml today (this plan ships no `fu rm`), and list/show are exactly
// the two commands a user confused by a missing skill reaches for first.
func printInvalidNames(cmd *cobra.Command, st *store.Store, cfg *store.Config) {
	out := cmd.ErrOrStderr()
	for _, inv := range cfg.InvalidNames() {
		fmt.Fprintf(out, "invalid: skill name %q fails validation (%s) and is ignored; edit %s to fix or remove it\n",
			inv.Name, inv.Reason, st.ConfigPath())
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
	for _, c := range res.Conflicts {
		fmt.Fprintf(out, "conflict: %s/%s occupied by unmanaged content\n", c.AgentName, c.Skill)
	}
	// Result.Foreign (a name fu.yaml has no opinion on at all) is
	// deliberately not printed here: informational inventory reserved for a
	// future `fu status`, and noise on every write command otherwise.
	// DisabledForeign is the other half of that same state-matrix cell --
	// a name fu.yaml does track, currently off, whose path is occupied by
	// the user's own content -- and it is actionable, so it is printed.
	for _, d := range res.DisabledForeign {
		fmt.Fprintf(out, "disabled-foreign: %s/%s is off but occupied by unmanaged content; fu left it alone, so the skill may still be loaded every session\n", d.AgentName, d.Skill)
	}
	for _, m := range res.Missing {
		fmt.Fprintf(out, "missing: %s/%s is enabled but the store no longer holds its content\n", m.AgentName, m.Skill)
	}
	for _, r := range res.Reserved {
		fmt.Fprintf(out, "reserved: %s/%s is a reserved name and will never be linked\n", r.AgentName, r.Skill)
	}
	for _, i := range res.Invalid {
		// An empty AgentName marks a config-level finding: a bad key in
		// fu.yaml is one fact about one file, reported once for the pass
		// (engine.configInvalidNames), not once per agent. The per-agent
		// form below is reached only by engine.Desired's defence-in-depth
		// check, for a Config built without going through LoadConfig.
		if i.AgentName == "" {
			fmt.Fprintf(out, "invalid: skill name %q fails validation and will never be linked\n", i.Skill)
			continue
		}
		fmt.Fprintf(out, "invalid: %s: skill name %q fails validation and will never be linked\n", i.AgentName, i.Skill)
	}
	for _, a := range res.Skipped {
		fmt.Fprintf(out, "skipped agent %s: its skills dir is a symlink (run `fu adopt` in a later version, or unlink manually)\n", a)
	}
	for _, f := range res.Failed {
		if f.Action.Skill != "" {
			fmt.Fprintf(out, "failed: %s/%s: %v\n", f.Action.AgentName, f.Action.Skill, f.Err)
		} else {
			fmt.Fprintf(out, "failed: %s: %v\n", f.Action.AgentName, f.Err)
		}
	}
}
