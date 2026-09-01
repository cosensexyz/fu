// internal/cli/outdated.go
package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type outdatedApplication interface {
	Outdated() (engine.OutdatedOutcome, error)
}

// splitOutdatedRows partitions Outdated's judged rows into the three groups
// `fu outdated` reports under (SPEC scenario 3): rows whose source comparison
// itself could not be judged, rows with an update available, and rows that
// are current but still carry a Reason -- one judgeLocalModification folded
// onto an otherwise fully comparable row (internal/engine/outdated.go) when
// its own store-snapshot check failed.
//
// A folded Reason must never be dropped just because the row's own
// source-side verdict was Comparable and not Updatable (the Task 2 review's
// binding finding, carried forward to this command): the row still needs a
// line to carry it, and "not comparable" is the wrong group for a row whose
// source comparison succeeded.
//
// That row used to share the updatable group, which is where fix round 2's
// Important #3 landed: the group is also the "updatable" heading and the
// "%d updatable" count, so a row rendering as "no update available" was
// announced and counted as updatable. The grouping argument above was never
// the problem -- the row does belong apart from "not comparable" -- so it
// keeps its own group here instead of being folded into a neighbour's.
//
// A current row is also reported when `fu update` would refuse it
// (UpdateRefusesWithoutForce). Round 2, Important #1: such a row -- store copy
// hand edited, nothing new upstream -- was Comparable, not Updatable and
// carried no Reason, so it landed in no group, was counted nowhere, and `fu
// update <name>` then refused it. Design §3.4 names that broken chain
// explicitly: 若 update 注定拒绝而 outdated 不说，这条链就断了.
//
// A row that is Comparable, not Updatable, carries no Reason and faces no
// refusal has nothing to report and belongs to no slice at all -- it is simply
// current.
func splitOutdatedRows(rows []engine.UpdateStatus) (updatable, upToDate, notComparable []engine.UpdateStatus) {
	for _, row := range rows {
		switch {
		case !row.Comparable:
			notComparable = append(notComparable, row)
		case row.Updatable:
			updatable = append(updatable, row)
		case row.Reason != "" || row.UpdateRefusesWithoutForce():
			upToDate = append(upToDate, row)
		}
	}
	return updatable, upToDate, notComparable
}

// notComparableRowBody renders a row whose source comparison could not be
// made. The Reason is the whole of why it is here; the local-modification
// clause is appended because otherwise that fact is computed and then
// unobtainable anywhere in fu.
//
// judgeLocalModification runs for every row, comparable or not (SPEC rule 9's
// two questions are independent), but splitOutdatedRows files a non-comparable
// row here first, and `fu status` deliberately does not make this comparison
// at all -- it compares the worktree against git, never cfg.Digest against
// store content (internal/engine/outdated.go). So for a tag-pinned skill whose
// store copy was hand edited, LocallyModified was computed, discarded, and
// reported by no command at all.
//
// The clause states the fact and stops. The refusal warning the updatable and
// up-to-date sections carry would be redundant here: this row already says
// update cannot work on it, and for a reason --force does not answer.
func notComparableRowBody(row engine.UpdateStatus) string {
	body := singleLine(row.Reason)
	if row.LocallyModified {
		return body + "; locally modified"
	}
	return body
}

// upToDateRowBody renders what a current row still has to say: that `fu
// update` will refuse it, and whatever Reason a half-failed judgement folded
// on. Either can be present alone, and both can be present together -- SPEC
// rule 9's two questions are independent, so this joins whichever answers
// exist rather than picking one.
func upToDateRowBody(row engine.UpdateStatus) string {
	var parts []string
	if row.UpdateRefusesWithoutForce() {
		parts = append(parts, "locally modified — `fu update` will refuse")
	}
	if row.Reason != "" {
		parts = append(parts, singleLine(row.Reason))
	}
	return strings.Join(parts, "; ")
}

// shortCommit shortens a git commit hash to the length fu shows everywhere
// (internal/cli/show.go's own source-field rendering: "the commit is
// shortened to the length fu shows everywhere"); the full value stays on
// UpdateStatus for anything that needs it.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// localSourcePath addresses the skill's own directory inside its recorded
// local source, rather than the source root. One directory commonly serves
// several skills (that is what the subdir field is for), and the root alone
// renders identically for every one of them -- while the path is the one
// thing a local row gives a reader to act on.
//
// A root-level skill records "" or "." and keeps the recorded path exactly as
// it is: filepath.Join would clean "." away anyway, but saying so here is
// what makes the two cases obviously the same one.
func localSourcePath(row engine.UpdateStatus) string {
	if row.Subdir == "" || row.Subdir == "." {
		return row.Path
	}
	return filepath.Join(row.Path, row.Subdir)
}

// nameColumnWidth sizes the name column to the widest name in the whole
// report, the way `fu status` sizes its own (status.go). Taken across every
// group rather than per group: three sections each padded to their own widest
// name would line up internally and step in and out against each other down
// the page.
func nameColumnWidth(groups ...[]engine.UpdateStatus) int {
	width := 0
	for _, group := range groups {
		for _, row := range group {
			if n := len(row.Name); n > width {
				width = n
			}
		}
	}
	return width
}

// printUpdatableRow renders one row of the updatable section: the
// kind-specific body from updatableRowBody, then the two things every kind
// shares.
//
// The refusal warning is gated on the engine's own predicate
// (UpdateRefusesWithoutForce), not on a rule restated here. Round 2,
// Important #1: this gate used to read `row.Updatable && row.LocallyModified`,
// justified by a comment asserting that `fu update` refuses only when both
// sides moved and that `fu status` reports local modification anyway. Both
// were false -- selectUpdateTargets (application.go) refuses on store-side
// drift alone, and `fu status` compares the worktree against git, never
// cfg.Digest against store content, which is exactly why DESIGN §4's
// 基线三态判定 says sweep 使 worktree 常态干净 and that the two judgements
// 各自独立判定、互不参照.
func printUpdatableRow(out io.Writer, row engine.UpdateStatus, nameWidth int) {
	fmt.Fprintf(out, "  %-*s  %s", nameWidth, row.Name, updatableRowBody(row))
	if row.UpdateRefusesWithoutForce() {
		fmt.Fprint(out, " (locally modified — `fu update` will refuse)")
	}
	// singleLine for the same reason every other field on this line carries it
	// (show.go): a recorded path can hold a newline -- `fu add` on a directory
	// whose name contains one records it verbatim -- and Reason quotes those
	// paths back. Printed raw, one row could fabricate a heading, a second row
	// and a count that disagrees with the rows above it.
	if row.Reason != "" {
		fmt.Fprintf(out, "; %s", singleLine(row.Reason))
	}
	fmt.Fprintln(out)
}

// updatableRowBody renders the part of an updatable-section row that
// depends on whether there is an update and, for a real update, what kind of
// source produced it (design spec §3.4).
//
// Fix round 1, Important #1: a uniform "Current -> Latest" template used to
// run for every kind. A git row's Current/Latest are commit hashes with no
// meaning without the ref they resolve against, so this shows the
// (shortened) commit pair beside the recorded ref. A local row's
// Current/Latest are raw "sha256:"+64-hex content digests
// (skill.DigestManifest, via internal/engine/outdated.go's judgeLocalUpdate)
// with no meaning to a reader at all -- printing them directly, with an
// arrow between them and no path, was the finding. This shows the recorded
// source path instead, the one thing a reader can actually act on.
func updatableRowBody(row engine.UpdateStatus) string {
	switch {
	case !row.Updatable:
		// Not reachable through the command since fix round 2's Important #3
		// gave the current-but-reported row its own section (splitOutdatedRows),
		// which renders its Reason directly and never calls this. Kept because
		// UpdateStatus is a plain struct any caller can hand to this function in
		// any shape, and the alternative to a row that names its own state is a
		// row rendering as an empty arrow between two empty commit fields.
		return "no update available"
	case row.Kind == "git":
		return fmt.Sprintf("%s → %s   %s",
			singleLine(shortCommit(row.Current)), singleLine(shortCommit(row.Latest)), singleLine(row.Ref))
	case row.Kind == "local":
		return fmt.Sprintf("source content differs from the install baseline   %s", singleLine(localSourcePath(row)))
	default:
		// Defensive: Updatable only ever comes true through judgeGitUpdate or
		// judgeLocalUpdate (internal/engine/outdated.go), which only run for
		// Kind "git" or "local" respectively -- but UpdateStatus is a plain
		// struct any caller can construct in any shape, and a row with an
		// update to report must never render as a blank field just because
		// its Kind matches neither.
		return fmt.Sprintf("update available (unrecognized source kind %q)", row.Kind)
	}
}

func newOutdatedCmd(app outdatedApplication) *cobra.Command {
	return &cobra.Command{
		Use:   "outdated",
		Short: "List skills with an update available upstream",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			outcome, err := app.Outdated()
			// Printed before the error is returned and before the report,
			// which is `fu status`'s order rather than every read command's:
			// `fu list` and `fu show` print theirs after their report. Ahead
			// is the right end for this one because a name this report
			// silently omits, or a fu.yaml newer than this build, is context
			// for everything below it (round 2, Important #6).
			printVersionWarning(cmd, outcome.Diagnostics)
			printInvalidNames(cmd, outcome.Diagnostics)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			updatable, upToDate, notComparable := splitOutdatedRows(outcome.Rows)
			// Silence would read the same as "outdated never looked" -- say so
			// explicitly, the same discipline `fu status` uses for a clean report.
			if len(updatable) == 0 && len(upToDate) == 0 && len(notComparable) == 0 {
				fmt.Fprintln(out, "everything is up to date")
				return nil
			}
			width := nameColumnWidth(updatable, upToDate, notComparable)
			if len(updatable) != 0 {
				fmt.Fprintln(out, "updatable")
				for _, row := range updatable {
					printUpdatableRow(out, row, width)
				}
			}
			// These rows are current; what they carry is the other,
			// independent half of SPEC rule 9 -- a hand-edited store copy `fu
			// update` will refuse, a half-failed judgement's Reason, or both.
			// The body says only that, since under this heading restating "no
			// update available" per row would say the heading twice.
			if len(upToDate) != 0 {
				fmt.Fprintln(out, "up to date")
				for _, row := range upToDate {
					fmt.Fprintf(out, "  %-*s  %s\n", width, row.Name, upToDateRowBody(row))
				}
			}
			if len(notComparable) != 0 {
				fmt.Fprintln(out, "not comparable")
				for _, row := range notComparable {
					fmt.Fprintf(out, "  %-*s  %s\n", width, row.Name, notComparableRowBody(row))
				}
			}
			// The two outer counts are printed even at zero, the way this line
			// always has: "0 not comparable" states that nothing failed
			// judgement, which is true. "0 up to date" would not be -- the rows
			// that are simply current are in no group and are counted nowhere,
			// so that number is only ever the count of current rows with
			// something to report, and printing it at zero would read as a
			// claim that nothing is current.
			counts := []string{fmt.Sprintf("%d updatable", len(updatable))}
			if len(upToDate) != 0 {
				counts = append(counts, fmt.Sprintf("%d up to date", len(upToDate)))
			}
			counts = append(counts, fmt.Sprintf("%d not comparable", len(notComparable)))
			fmt.Fprintln(out, strings.Join(counts, ", "))
			return nil
		},
	}
}
