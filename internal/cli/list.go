// internal/cli/list.go
package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

type listApplication interface {
	ListSkills() (engine.ListOutcome, error)
}

func newListCmd(app listApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List skills and their switch matrix",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := app.ListSkills()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprint(w, "SKILL\tGLOBAL")
			for _, agentName := range outcome.Agents {
				fmt.Fprintf(w, "\t%s", agentName)
			}
			fmt.Fprintln(w)
			for _, listed := range outcome.Skills {
				fmt.Fprintf(w, "%s\t%s", listed.Name, onOff(listed.Global))
				for _, agentState := range listed.Agents {
					cell := onOff(agentState.Enabled)
					if agentState.Override {
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
			printVersionWarning(cmd, outcome.Diagnostics)
			// A name store.LoadConfig found invalid never appears in the
			// matrix above (SkillNames excludes it, round 4 finding 2) --
			// report it separately rather than leaving it silently missing.
			printInvalidNames(cmd, outcome.Diagnostics)
			return nil
		},
	}
}
