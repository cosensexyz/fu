// internal/cli/log.go
package cli

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cosensexyz/fu/internal/engine"
)

type logApplication interface {
	Log(count int) (engine.LogOutcome, error)
}

func newLogCmd(app logApplication) *cobra.Command {
	var count int
	cmd := &cobra.Command{
		Use:   "log [-n <count>]",
		Short: "Show the store's history, numbered the way `fu revert` counts",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if count < 1 {
				return &UsageError{fmt.Errorf("log takes a positive entry count, got %d", count)}
			}
			outcome, err := app.Log(count)
			if err != nil {
				return err
			}
			// Rendered into a buffer rather than straight to the writer
			// so each line can be trimmed after the flush; see below.
			var table bytes.Buffer
			w := tabwriter.NewWriter(&table, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "#\tWHEN\tHASH\tOPERATION")
			for _, e := range outcome.Entries {
				ordinal := ""
				if e.Ordinal != 0 {
					ordinal = strconv.Itoa(e.Ordinal)
				}
				operation := sanitizeCell(e.Subject, e.Body)
				when := e.When.Local().Format("2006-01-02 15:04")
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ordinal, when, e.Hash, operation)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			// Four cells on every row, padding trimmed afterwards.
			//
			// A commit with no message still needs a fourth cell, because
			// tabwriter excludes a *trailing* cell from its column-width
			// computation: ending such a row at the hash made that hash
			// trailing, collapsed the HASH block to the header's own width,
			// and left the header's OPERATION three columns left of every
			// subject below it (review 2026-09-03, Minor). Emitting the empty
			// cell keeps the block intact and ends the row in padding
			// instead, which TrimRight then removes -- so both properties
			// hold at once rather than trading off.
			out := cmd.OutOrStdout()
			for _, line := range strings.Split(strings.TrimSuffix(table.String(), "\n"), "\n") {
				if _, err := fmt.Fprintln(out, strings.TrimRight(line, " ")); err != nil {
					return err
				}
			}
			printVersionWarning(cmd, outcome.Diagnostics)
			printInvalidNames(cmd, outcome.Diagnostics)
			return nil
		},
	}
	cmd.Flags().IntVarP(&count, "count", "n", 20, "number of commits to show")
	return cmd
}

// sanitizeCell renders a commit's subject and first body line as one table
// cell that cannot forge or distort a row.
//
// A commit message is arbitrary bytes: a direct-git user writes it and fu does
// not control it. The rule is a class, not a list -- every C0 control and DEL
// becomes a space -- because enumerating the bytes observed to fail is what
// made this the same finding three review rounds running (a tab, then a
// carriage return, then a vertical tab and a form feed).
//
// The class is the right unit because text/tabwriter gives meaning to four of
// those bytes, not one: a tab and a vertical tab each terminate a cell, and a
// line feed and a form feed each end a line. Either of the latter two splits
// one commit into two rendered rows, and the reader sees a whole entry that
// does not exist -- ordinal, timestamp and hash all invented -- on the one
// command whose numbers feed `fu revert n`. Unlike a carriage return, which
// only redraws a terminal, they put that fabrication in the bytes, where it
// survives redirection and a pasted transcript. The remaining controls are
// worth mapping for the terminal's sake alone: ESC opens a cursor-movement
// sequence (`ESC [ G` is the carriage return's effect verbatim).
//
// Deliberately not extended to C1 (U+0080-U+009F), U+2028, U+2029, U+202E or
// ZWJ, so that "a class, not a list" is not read as covering more than it
// does. None of them means anything to text/tabwriter, so none can terminate
// a cell or end a line: the fabrication class -- a forged row that survives
// `| cat`, redirection and a pasted transcript, because the damage is in the
// bytes -- stays fully closed. What they can still do is act on a terminal
// that decodes UTF-8 C1, where U+009B G renders as ESC [ G. That exposure is
// shared with `fu show` and `fu list`, which render the same untrusted text
// through singleLine, so closing it belongs there rather than in making one
// command silently stricter than its neighbours (review 2026-09-03 round 4,
// Minor).
//
// singleLine (show.go) still trims, which collapses a whitespace-only message
// to the empty cell the caller pads out.
func sanitizeCell(subject, body string) string {
	cell := subject
	if body != "" {
		cell += "  " + body
	}
	return singleLine(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, cell))
}
