// internal/cli/show.go
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

// singleLine keeps untrusted frontmatter inside one output line so it cannot
// impersonate a later field.
func singleLine(s string) string {
	replaced := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	return strings.TrimSpace(replaced)
}

type showApplication interface {
	ShowSkill(string) (engine.ShowOutcome, error)
}

func newShowCmd(app showApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one skill's details",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			outcome, err := app.ShowSkill(name)
			// Warn before any schema-dependent lookup can return early: a
			// newer schema may change how both visible and missing entries are
			// interpreted.
			printVersionWarning(cmd, outcome.Diagnostics)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// The identity line is the store's own name, never the
			// frontmatter's. A frontmatter name that disagrees violates SPEC
			// rule 1 and was previously printed as though it were fu's answer,
			// contradicting the name fu links, toggles and reports under.
			fmt.Fprintf(out, "name:        %s\n", name)
			switch {
			case outcome.MetadataError != nil:
				fmt.Fprintf(out, "description: (SKILL.md unreadable: %v)\n", outcome.MetadataError)
			default:
				// One line, always. The output format is line-oriented
				// `key: value`, so a multi-line description could forge any
				// field below it -- a description ending in "global: off" made
				// `grep '^global:'` read the forged line first.
				fmt.Fprintf(out, "description: %s\n", singleLine(outcome.Description))
				if outcome.MetadataWarning != nil {
					fmt.Fprintf(out, "warning:     SKILL.md frontmatter is invalid: %v\n", outcome.MetadataWarning)
				}
			}
			fmt.Fprintf(out, "digest:      %s\n", outcome.Digest)
			// The source record is fu.yaml's own, rendered one scalar per
			// line with a shared alignment so a value cannot forge a field
			// (the same discipline singleLine applies to the description).
			// The commit is shortened to the length fu shows everywhere; the
			// full value stays in fu.yaml.
			if src := outcome.Source; len(src) != 0 {
				fmt.Fprintf(out, "source:\n")
				for _, key := range []string{"type", "url", "path", "ref", "ref_kind", "commit", "subdir"} {
					v, ok := src[key]
					if !ok {
						continue
					}
					if key == "commit" && len(v) > 12 {
						v = v[:12]
					}
					// singleLine like the description (round 15 finding M2):
					// a subdir from a repo directory name or a local source
					// path can carry newlines that would forge a field line.
					fmt.Fprintf(out, "  %-9s %s\n", key+":", singleLine(v))
				}
			}
			fmt.Fprintf(out, "global:      %s\n", onOff(outcome.Global))
			for _, agentState := range outcome.Agents {
				line := onOff(agentState.Enabled)
				if agentState.Override {
					line += " (override)"
				} else {
					line += " (follows global)"
				}
				fmt.Fprintf(out, "%s: %s\n", agentState.Name, line)
			}
			// name itself is confirmed valid above (HasSkill would have
			// refused it otherwise); a different, invalid entry may still
			// coexist elsewhere in the same config (round 4 finding 2) --
			// report it rather than leaving it silently unmentioned.
			printInvalidNames(cmd, outcome.Diagnostics)
			return nil
		},
	}
}
