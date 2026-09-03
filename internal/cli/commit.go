// internal/cli/commit.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type commitApplication interface {
	Commit(name, message string) (engine.CommitOutcome, error)
}

func newCommitCmd(app commitApplication) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   "commit [name] [-m <message>]",
		Short: "Record pending hand edits as one operation",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			if len(args) == 1 {
				name = args[0]
				// An explicitly empty positional is a usage error, not a
				// store-wide commit -- the same guard `fu update ""` has.
				if name == "" {
					return &UsageError{errors.New(
						"commit needs a skill name: run `fu commit <name>` for one skill, " +
							"or `fu commit` with no argument for the whole store")}
				}
			}
			messageGiven := cmd.Flags().Changed("message")
			if messageGiven && message == "" {
				return &UsageError{errors.New("-m needs a non-empty message")}
			}
			outcome, err := app.Commit(name, message)
			printResult(cmd, outcome.Result)
			out := cmd.OutOrStdout()
			// Reported before the error is returned, like `fu revert`: a
			// failure arriving after either commit is durable must not read
			// as though nothing was recorded. Written and ExternalWritten are
			// independent facts (engine.CommitOutcome's doc comment), not
			// alternatives, and both are reported here whenever both are
			// true. The external snapshot is reported first because it lands
			// first on HEAD (store.Sweep's own two layers, the same order
			// applied to a store-wide `fu commit`), matching the order it
			// will appear in `git log`.
			if outcome.ExternalWritten {
				fmt.Fprintf(out, "recorded a snapshot staged with git as %q\n", engine.ExternalCommitMessage)
			}
			if outcome.Written {
				fmt.Fprintf(out, "recorded %q:\n", outcome.Subject)
				for _, path := range outcome.Changed {
					fmt.Fprintf(out, "  %s\n", path)
				}
			}
			if err != nil {
				return err
			}
			switch {
			case !outcome.Written && !outcome.ExternalWritten:
				fmt.Fprintln(out, "nothing to commit")
			case outcome.ExternalWritten && !outcome.Written && messageGiven:
				// The derived store-wide candidate turned out empty -- e.g.
				// `git add -A` followed by `fu commit -m "reason"`, where the
				// external layer above already consumed the whole
				// difference -- so the user's -m text was never used for
				// anything. This is said only on the no-error path: under an
				// error we cannot tell whether the second candidate was
				// genuinely empty or simply failed to prepare, so claiming
				// "there was nothing left to record" would be a guess.
				fmt.Fprintln(out, "your -m message was not recorded: there was nothing left to commit under it")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "message recorded as the commit body; the subject line is generated")
	return cmd
}
