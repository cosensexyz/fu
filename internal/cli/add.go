// internal/cli/add.go
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type addApplication interface {
	PrepareAdd(string, string) (engine.AddPreparation, error)
}

// newAddCmd takes its application as a required parameter, like every sibling
// constructor. It used to be variadic with an engine.NewApplication() default,
// so a test that forgot the argument compiled and quietly ran against the real
// Application -- the compiler could not help -- and every call built a
// throwaway Application even when one was supplied.
func newAddCmd(app addApplication) *cobra.Command {
	var all bool
	var ref string
	cmd := &cobra.Command{
		Use:   "add [--ref <ref>] <git-url> | <local-dir>",
		Short: "Install skills from a git repository or local directory",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			if cmd.Flags().Changed("ref") && ref == "" {
				return &UsageError{engine.ErrEmptyAddRef}
			}
			preparation, err := app.PrepareAdd(args[0], ref)
			if err != nil {
				printResult(cmd, preparation.Prologue)
				if errors.Is(err, engine.ErrInvalidAddRef) {
					return &UsageError{err}
				}
				return err
			}
			plan := preparation.Session
			if plan == nil {
				return errors.New("prepare add returned no session")
			}
			defer mergeCloseError(&retErr, plan)

			cands, invalid := plan.Candidates(), plan.Invalid()
			// Deterministic order: invalid is a map, and repeated runs must
			// print the same sequence (round 8 finding M3).
			invalidPaths := make([]string, 0, len(invalid))
			for path := range invalid {
				invalidPaths = append(invalidPaths, path)
			}
			sort.Strings(invalidPaths)
			for _, path := range invalidPaths {
				// The source as typed plus the path within it. The engine used
				// to key these on the absolute location it read from, which for
				// a git source is a clone directory deleted when the command
				// exits -- a path the user could neither recognise nor look at.
				fmt.Fprintf(cmd.ErrOrStderr(), "invalid: %s: %s: %v\n", plan.SourceArg(), path, invalid[path])
			}
			// All three early returns below carry the mandatory recovery
			// prologue's findings out with them. Install folds the prologue into
			// its own outcome, so these are the only exits that would otherwise
			// drop it -- and they are the exits where the user aborted, which
			// is exactly when a recovery-boundary finding matters most (round
			// 18 finding M18, same defect class in adopt).
			if err := plan.NoCandidates(); err != nil {
				printResult(cmd, plan.Prologue())
				return err
			}
			selected, err := selectCandidates(cmd, cands, all)
			if err != nil {
				// The third abort exit, and the same reasoning as the two
				// around it: the user's selection was refused, nothing was
				// installed, and the prologue's recovery-boundary findings
				// would otherwise be lost.
				printResult(cmd, plan.Prologue())
				return err
			}
			if len(selected) == 0 {
				printResult(cmd, plan.Prologue())
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing selected; nothing installed")
				return plan.NoSelection()
			}
			outcome, err := plan.Install(selected)
			// The batch may have installed some skills before a mid-batch
			// failure: always report what completed before returning the
			// error (round 13 finding M4).
			for _, name := range outcome.Added {
				fmt.Fprintf(cmd.OutOrStdout(), "added %s\n", name)
			}
			for _, name := range outcome.Skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %s: already installed; run `fu rm %s` then add again to replace it\n", name, name)
			}
			if len(outcome.Unattempted) != 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "not attempted: %s\n", strings.Join(outcome.Unattempted, ", "))
			}
			for _, operation := range outcome.Operations {
				printDurableOutcome(cmd, "add", operation)
			}
			printResult(cmd, outcome.Reconcile)
			return err
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "install every valid skill without prompting")
	cmd.Flags().StringVar(&ref, "ref", "", "branch or tag to install (git sources only)")
	return cmd
}

func mergeCloseError(result *error, closer io.Closer) {
	if err := closer.Close(); err != nil {
		*result = errors.Join(*result, fmt.Errorf("clean prepared source: %w", err))
	}
}

// selectCandidates picks the skills to install: all of them with --all, the
// single candidate without one, otherwise an interactive selection read from
// stdin. The prompt is presentation; the choice is the user's, passed back
// to the engine untouched.
func selectCandidates(cmd *cobra.Command, cands []engine.Candidate, all bool) ([]engine.Candidate, error) {
	if all || len(cands) == 1 {
		return cands, nil
	}
	// The candidate list and the prompt are interaction, not result: on
	// stdout, `fu add $SRC > out.txt` swallowed both, so the terminal showed
	// nothing and the command looked hung (round 18 finding M21). Every other
	// non-result line in this CLI already goes to stderr.
	out := cmd.ErrOrStderr()
	for i, c := range cands {
		fmt.Fprintf(out, "%2d. %s (%s) — %s\n", i+1, c.Name, c.Subdir, singleLine(c.Description))
	}
	fmt.Fprint(out, "select skills to install (comma-separated numbers, or `all`): ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(out)
		if line == "" {
			return nil, errors.New("selection input ended before a choice was provided; use --all for non-interactive installation")
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read selection: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	if line == "all" {
		return cands, nil
	}
	parts := strings.Split(line, ",")
	nonempty := 0
	hasAll := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		nonempty++
		hasAll = hasAll || part == "all"
	}
	if hasAll {
		if nonempty == 1 {
			return cands, nil
		}
		return nil, errors.New("cannot mix `all` with numeric selections")
	}
	var selected []engine.Candidate
	chosen := map[int]bool{}
	for _, part := range parts {
		// A trailing or doubled comma is a typo, not a selection: skipping the
		// empty field beats refusing with `invalid selection ""`, which names
		// nothing the user typed.
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid selection %q: expected a number between 1 and %d", part, len(cands))
		}
		if n < 1 || n > len(cands) {
			return nil, fmt.Errorf("selection %d is out of range 1-%d", n, len(cands))
		}
		chosen[n] = true
	}
	if len(chosen) == 0 {
		return nil, nil
	}
	// Keep the presentation order.
	for i, c := range cands {
		if chosen[i+1] {
			selected = append(selected, c)
		}
	}
	return selected, nil
}
