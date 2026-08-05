// internal/cli/show.go
package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
)

// singleLine keeps untrusted frontmatter inside one output line so it cannot
// impersonate a later field.
func singleLine(s string) string {
	replaced := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	return strings.TrimSpace(replaced)
}

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one skill's details",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			st, cfg, err := openStoreAndConfig()
			if err != nil {
				return err
			}
			// Warn before any schema-dependent lookup can return early: a
			// newer schema may change how both visible and missing entries are
			// interpreted.
			printVersionWarning(cmd, st, cfg)
			if !cfg.HasSkill(name) {
				// HasSkill also answers false for a name LoadConfig isolated
				// as invalid, and "unknown skill" is the wrong thing to say
				// about an entry the user can see sitting in fu.yaml (round 7
				// finding). Answer the question actually asked: why it is
				// being ignored, and which file to edit. This is the same
				// diagnostic printInvalidNames gives for other entries -- it
				// was simply unreachable for the one entry the user named,
				// because this return came first.
				for _, inv := range cfg.InvalidNames() {
					if inv.Name == name {
						return fmt.Errorf("skill name %q fails validation (%s) and is ignored; "+
							"edit %s to fix or remove it", inv.Name, inv.Reason, st.ConfigPath())
					}
				}
				return fmt.Errorf("unknown skill %q", name)
			}
			out := cmd.OutOrStdout()
			// The identity line is the store's own name, never the
			// frontmatter's. A frontmatter name that disagrees violates SPEC
			// rule 1 and was previously printed as though it were fu's answer,
			// contradicting the name fu links, toggles and reports under.
			fmt.Fprintf(out, "name:        %s\n", name)
			m, err := skill.ParseMeta(filepath.Join(st.SkillsDir(), name))
			switch {
			case err != nil:
				fmt.Fprintf(out, "description: (SKILL.md unreadable: %v)\n", err)
			default:
				// One line, always. The output format is line-oriented
				// `key: value`, so a multi-line description could forge any
				// field below it -- a description ending in "global: off" made
				// `grep '^global:'` read the forged line first.
				fmt.Fprintf(out, "description: %s\n", singleLine(m.Description))
				if verr := skill.Validate(m, name); verr != nil {
					fmt.Fprintf(out, "warning:     SKILL.md frontmatter is invalid: %v\n", verr)
				}
			}
			fmt.Fprintf(out, "digest:      %s\n", cfg.Digest(name))
			fmt.Fprintf(out, "global:      %s\n", onOff(cfg.Enabled(name)))
			for _, a := range agent.Detected() {
				line := onOff(cfg.Effective(name, a.Name()))
				if _, isOverride := cfg.Override(name, a.Name()); isOverride {
					line += " (override)"
				} else {
					line += " (follows global)"
				}
				fmt.Fprintf(out, "%s: %s\n", a.Name(), line)
			}
			// name itself is confirmed valid above (HasSkill would have
			// refused it otherwise); a different, invalid entry may still
			// coexist elsewhere in the same config (round 4 finding 2) --
			// report it rather than leaving it silently unmentioned.
			printInvalidNames(cmd, st, cfg)
			return nil
		},
	}
}
