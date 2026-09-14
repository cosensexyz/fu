// internal/cli/agent.go
package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type agentApplication interface {
	Agents() (engine.AgentsOutcome, error)
}

// agentState names the row's condition. The words are the table's own, kept
// short enough for a column: `fu status` says "detected, nothing projected
// yet" where this says "dir missing", and reserves "cannot inspect" for an
// entry rather than an agent. The notes under the table are where the two
// commands do share wording.
func agentState(overview engine.AgentOverview) string {
	switch {
	case !overview.Detected:
		return "not detected"
	case overview.ScanErr != "":
		return "cannot inspect"
	case overview.DirIsSymlink:
		return "dir is a symlink"
	case overview.DirMissing:
		return "dir missing"
	}
	return "ok"
}

// printAgentNotes writes what a count cannot: what the state means and which
// command changes it. Only rows with something to say produce a line.
func printAgentNotes(out io.Writer, overviews []engine.AgentOverview) {
	for _, overview := range overviews {
		switch {
		case !overview.Detected:
			continue
		case overview.ScanErr != "":
			fmt.Fprintf(out, "%s: could not be inspected: %s\n", overview.Name, overview.ScanErr)
			continue
		case overview.DirIsSymlink:
			// SPEC rule 10: fu never writes through a symlinked skills
			// directory, and adopt is the one command that converts it.
			fmt.Fprintf(out, "%s: skills dir is a symlink; run `fu adopt` to convert it to fu-managed links\n", overview.Name)
			continue
		case overview.DirMissing:
			// SPEC rule 4 requires naming what creates it, since this command
			// deliberately does not.
			fmt.Fprintf(out, "%s: nothing projected yet; the next write command or `fu restore` creates the directory\n", overview.Name)
		}
		if overview.Broken > 0 {
			fmt.Fprintf(out, "%s: %d %s; run `fu status` for the names\n", overview.Name, overview.Broken,
				plural(overview.Broken, "broken link", "broken links"))
		}
		if overview.Uninspectable > 0 {
			fmt.Fprintf(out, "%s: %d %s; run `fu status` for the names\n", overview.Name, overview.Uninspectable,
				plural(overview.Uninspectable, "entry cannot be inspected", "entries cannot be inspected"))
		}
		// Neither of these is pending -- fu will make no change for either --
		// and neither shows in the columns, so without a line the row reads
		// as up to date while the user still has something to do.
		if overview.Blocked > 0 {
			// "name", not "link": the count includes the disabled form, where
			// fu.yaml wants no link at all and the obstruction is that
			// something else holds the name.
			fmt.Fprintf(out, "%s: %d %s blocked by unmanaged content; run `fu status` for the names\n", overview.Name,
				overview.Blocked, plural(overview.Blocked, "name", "names"))
		}
		if overview.Unavailable > 0 {
			fmt.Fprintf(out, "%s: %d enabled %s the store no longer holds; run `fu status` for the names\n", overview.Name,
				overview.Unavailable, plural(overview.Unavailable, "skill", "skills"))
		}
	}
}

func newAgentCmd(app agentApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "List the agents fu supports and what each has been given",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := app.Agents()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "AGENT\tSTATE\tDELIVERED\tPENDING\tUNMANAGED\tSKILLS DIR")
			for _, overview := range outcome.Agents {
				if !overview.Detected {
					// Counts of a directory belonging to no one would read as
					// facts about this machine. The name and the state are
					// the whole row.
					fmt.Fprintf(w, "%s\t%s\t\t\t\t\n", overview.Name, agentState(overview))
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\n", overview.Name, agentState(overview),
					overview.Delivered, overview.Pending, overview.Unmanaged, overview.SkillsDir)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			printAgentNotes(out, outcome.Agents)
			printVersionWarning(cmd, outcome.Diagnostics)
			printInvalidNames(cmd, outcome.Diagnostics)
			return nil
		},
	}
}
