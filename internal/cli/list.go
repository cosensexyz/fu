// internal/cli/list.go
package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"fu/internal/agent"
)

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List skills and their switch matrix",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cfg, err := openStoreAndConfig()
			if err != nil {
				return err
			}
			agents := agent.Detected()
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprint(w, "SKILL\tGLOBAL")
			for _, a := range agents {
				fmt.Fprintf(w, "\t%s", a.Name())
			}
			fmt.Fprintln(w)
			for _, name := range cfg.SkillNames() {
				fmt.Fprintf(w, "%s\t%s", name, onOff(cfg.Enabled(name)))
				for _, a := range agents {
					cell := onOff(cfg.Effective(name, a.Name()))
					if _, isOverride := cfg.Override(name, a.Name()); isOverride {
						cell += "*" // override marker
					}
					fmt.Fprintf(w, "\t%s", cell)
				}
				fmt.Fprintln(w)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			// DESIGN §3: a version newer than this build supports must still
			// warn on the read side, even though list proceeds best-effort.
			printVersionWarning(cmd, st, cfg)
			// A name store.LoadConfig found invalid never appears in the
			// matrix above (SkillNames excludes it, round 4 finding 2) --
			// report it separately rather than leaving it silently missing.
			printInvalidNames(cmd, st, cfg)
			return nil
		},
	}
}
