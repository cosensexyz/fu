// internal/cli/update.go
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type updateApplication interface {
	UpdateSkills(name string, force bool) (engine.UpdateBatchOutcome, error)
}

func newUpdateCmd(app updateApplication) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "update [name]",
		Short: "Pull the latest content for one skill, or every updatable skill",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			if len(args) == 1 {
				name = args[0]
			}
			// An explicitly empty positional is a usage error, not a batch.
			// `application.go` resolves an empty name to "every updatable
			// skill", so without this `fu update ""` silently escalated a
			// request that named one skill into a store-wide write -- while
			// `fu show ""` answers `unknown skill ""`. Refused before the
			// --force guard below, because the empty name is the mistake in
			// `fu update "" --force` too, and it is the one that would still
			// be a mistake with the flag removed.
			if len(args) == 1 && name == "" {
				return &UsageError{errors.New(
					"update needs a skill name: run `fu update <name>` for one skill, " +
						"or `fu update` with no argument for every updatable skill")}
			}
			// --force is only meaningful against a named skill: a batch
			// force would overwrite several skills' local modifications
			// with nothing on the command line naming any of them.
			if force && name == "" {
				return &UsageError{errors.New(
					"--force needs a skill name: a batch update would overwrite several skills' " +
						"local modifications without naming any of them; run `fu update <name> --force` instead")}
			}
			outcome, err := app.UpdateSkills(name, force)
			// The application refuses the same combination on its own
			// (engine.ErrUpdateForceNeedsName) for a caller that reaches it
			// without going through the guard above -- unreachable from this
			// command today, but still a usage error, not an operation
			// failure, if it were ever hit.
			if errors.Is(err, engine.ErrUpdateForceNeedsName) {
				err = &UsageError{err}
			}
			// Printed first, and regardless of how the run turned out, the
			// order `fu outdated` and `fu status` use (outdated.go,
			// status.go); `fu list` and `fu show` print theirs after the
			// report instead. First is right here for the reason those two
			// give: a name this run silently excluded, or a fu.yaml newer than
			// this build, is context for everything below it. `fu update`
			// was the one command that printed neither -- when at least one
			// transaction runs the `invalid:` line arrives through the
			// reconcile channel, but a store with nothing to update returns
			// before any transaction and so said nothing at all, while `fu
			// outdated` on the same store warned about both (round 4).
			printVersionWarning(cmd, outcome.Diagnostics)
			printInvalidNames(cmd, outcome.Diagnostics)
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			// Silence would be the output of a command that never ran, the
			// same reasoning `fu outdated` and `fu status` state for their own
			// clean-result lines (outdated.go, status.go). Every loop below is
			// empty when UpdateSkills found no target, which is the ordinary
			// result of `fu update` against a current store. Not claimed when
			// err is set: the outcome is then empty because the run failed,
			// not because there was nothing to do.
			if err == nil && len(outcome.Updated) == 0 && len(outcome.LockOnly) == 0 &&
				len(outcome.Skipped) == 0 && len(outcome.Unjudged) == 0 && len(outcome.Unattempted) == 0 {
				fmt.Fprintln(out, "nothing to update")
			}
			for _, updated := range outcome.Updated {
				fmt.Fprintf(out, "updated %s\n", updated)
			}
			// Not "updated": this shape writes nothing but fu.yaml's source
			// record, and on an already-current skill it writes the same bytes
			// back and produces no commit at all (round 2, Minor #7 -- found by
			// two lenses). Both cases are honestly described by what did
			// happen, and the genuine advance is still distinguishable from the
			// content shape above.
			for _, lockOnly := range outcome.LockOnly {
				fmt.Fprintf(out, "%s: recorded the source lock, content unchanged\n", lockOnly)
			}
			// No --force hint, unlike a skip: --force waives the
			// local-modification refusal and nothing else, so it does not help a
			// row that could not be judged at all (round 3).
			for _, unjudged := range outcome.Unjudged {
				fmt.Fprintf(errOut, "could not judge %s: %s\n", unjudged.Name, singleLine(unjudged.Reason))
			}
			for _, skip := range outcome.Skipped {
				fmt.Fprintf(errOut, "skipped %s: %s; run `fu update %s --force` to update it\n", skip.Name, singleLine(skip.Reason), skip.Name)
			}
			if len(outcome.Unattempted) != 0 {
				fmt.Fprintf(errOut, "not attempted: %s\n", strings.Join(outcome.Unattempted, ", "))
			}
			for _, operation := range outcome.Operations {
				printDurableOutcome(cmd, "update", operation)
			}
			printResult(cmd, outcome.Reconcile)
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a skill's local modifications (requires a skill name)")
	return cmd
}
