package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakeLogApplication struct {
	count   int
	outcome engine.LogOutcome
	err     error
}

func (f *fakeLogApplication) Log(count int) (engine.LogOutcome, error) {
	f.count = count
	return f.outcome, f.err
}

func TestLogCommandRendersOrdinalsAndBodies(t *testing.T) {
	when := time.Date(2026, 9, 2, 10, 12, 0, 0, time.UTC)
	app := &fakeLogApplication{outcome: engine.LogOutcome{Entries: []engine.LogEntry{
		{Ordinal: 1, Hash: "a1b2c3d", When: when, Subject: "commit: alpha", Body: "first draft"},
		{Ordinal: 0, Hash: "9f8e7d6", When: when, Subject: "external: manual modifications"},
		{Ordinal: 2, Hash: "5c4b3a2", When: when, Subject: "new: alpha"},
	}}}
	cmd := newLogCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if app.count != 20 {
		t.Fatalf("default count = %d, want 20", app.count)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and three rows:\n%s", out.String())
	}
	if !strings.HasPrefix(lines[0], "#") || !strings.Contains(lines[0], "OPERATION") {
		t.Fatalf("header:\n%s", lines[0])
	}
	stamp := when.Local().Format("2006-01-02 15:04")
	if !strings.HasPrefix(lines[1], "1 ") || !strings.Contains(lines[1], stamp) || !strings.Contains(lines[1], "a1b2c3d") || !strings.HasSuffix(lines[1], "commit: alpha  first draft") {
		t.Fatalf("row 1:\n%s", lines[1])
	}
	if strings.HasPrefix(strings.TrimSpace(lines[2]), "0") || !strings.HasSuffix(lines[2], "external: manual modifications") {
		t.Fatalf("an uncounted commit shows no ordinal and no trailing blanks:\n%q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "2 ") {
		t.Fatalf("row 3:\n%s", lines[3])
	}
}

func TestLogCommandPassesTheCount(t *testing.T) {
	app := &fakeLogApplication{}
	cmd := newLogCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-n", "3"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if app.count != 3 {
		t.Fatalf("count = %d, want 3", app.count)
	}
}

func TestLogCommandUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"-n", "0"}, {"-n", "-2"}, {"extra"}} {
		cmd := newLogCmd(&fakeLogApplication{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		var usage *UsageError
		if !errors.As(err, &usage) {
			t.Errorf("`fu log %q` must be a usage error, got %T %v", args, err, err)
		}
	}
	// A malformed flag value is classified by the root's FlagErrorFunc.
	var out bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	if code := execute(root, []string{"log", "-n", "abc"}); code != 2 {
		t.Fatalf("`fu log -n abc` exit = %d, want 2:\n%s", code, out.String())
	}
}

// TestLogCommandSanitisesCommitMessageText pins that nothing a commit message
// carries can forge or distort a row of this table.
//
// The rendering fix shipped without a test, and reverting it left every suite
// green (review 2026-09-02 round 2, Important). The assertion is over a class,
// not a list: no C0 control byte and no DEL may reach the rendered table,
// because enumerating the bytes seen to fail is what made this the same
// finding three rounds running -- the tab, then the carriage return, then the
// vertical tab and form feed (review 2026-09-03, Important).
//
// Why each byte in the fixture is one a whole row can be forged with: a tab
// and a vertical tab both terminate a tabwriter cell, so either one opens a
// column the data has no right to; a line feed and a form feed both end a
// tabwriter line, so either one splits a commit into two rows -- and unlike a
// carriage return, which only redraws on a terminal, those two put the
// fabrication in the *bytes*, where it survives `| cat`, redirection and a
// pasted transcript. A carriage return returns a terminal's cursor to column
// 0 so the rest of the message overwrites the row; ESC does the same through
// `ESC [ G`. A message that is empty or only whitespace is included for a
// different reason: it must not end the row in the hash column's padding.
func TestLogCommandSanitisesCommitMessageText(t *testing.T) {
	when := time.Date(2026, 9, 2, 10, 12, 0, 0, time.UTC)
	app := &fakeLogApplication{outcome: engine.LogOutcome{Entries: []engine.LogEntry{
		{Ordinal: 1, Hash: "a1b2c3d", When: when, Subject: "subject\twith a tab"},
		{Ordinal: 2, Hash: "b2c3d4e", When: when, Subject: "carriage\rreturn", Body: "body\rtoo"},
		{Ordinal: 3, Hash: "c3d4e5f", When: when, Subject: "kept", Body: "   "},
		{Ordinal: 4, Hash: "e5f6a7b", When: when, Subject: "vertical\vtab", Body: "form\ffeed"},
		{Ordinal: 5, Hash: "f6a7b8c", When: when, Subject: "escape\x1b[Gand bell\a"},
		{Ordinal: 0, Hash: "d4e5f6a", When: when},
	}}}
	cmd := newLogCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	rendered := out.String()
	for i, r := range rendered {
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			t.Fatalf("byte %d is a control character (%q); the row separator is the only one this table may emit:\n%q", i, r, rendered)
		}
	}
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("want a header and six rows, got %d lines:\n%q", len(lines), rendered)
	}
	for i, line := range lines {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line %d ends in padding, which an empty or whitespace-only message must not produce: %q", i, line)
		}
	}
	if !strings.HasSuffix(lines[1], "subject with a tab") {
		t.Errorf("a tab must become a space, not a column: %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], "carriage return  body too") {
		t.Errorf("a carriage return must become a space: %q", lines[2])
	}
	if !strings.HasSuffix(lines[3], "kept") {
		t.Errorf("a whitespace-only body must leave no residue: %q", lines[3])
	}
	if !strings.HasSuffix(lines[4], "vertical tab  form feed") {
		t.Errorf("a vertical tab must not open a column and a form feed must not open a row: %q", lines[4])
	}
	if !strings.HasSuffix(lines[5], "escape [Gand bell") {
		t.Errorf("an escape sequence must reach the table as text, not as cursor movement: %q", lines[5])
	}
	if !strings.HasSuffix(lines[6], "d4e5f6a") {
		t.Errorf("an empty message must end the row at the hash: %q", lines[6])
	}
}

// TestLogCommandKeepsColumnsAlignedAcrossAnEmptyMessage pins that a commit
// carrying no message cannot move the other rows' columns.
//
// text/tabwriter excludes a *trailing* cell from its column-width computation,
// so emitting three cells for an empty message -- which is what keeping the
// row out of the hash column's padding used to require -- made that row's hash
// a trailing cell and collapsed the HASH block to the header's own width. The
// header then started OPERATION three columns left of every subject beneath it
// (review 2026-09-03, Minor). Four cells always, and the padding is trimmed
// after the flush instead.
func TestLogCommandKeepsColumnsAlignedAcrossAnEmptyMessage(t *testing.T) {
	when := time.Date(2026, 9, 3, 10, 20, 0, 0, time.UTC)
	app := &fakeLogApplication{outcome: engine.LogOutcome{Entries: []engine.LogEntry{
		{Ordinal: 0, Hash: "5c26217", When: when},
		{Ordinal: 1, Hash: "88a6dd8", When: when, Subject: "new: alpha"},
		{Ordinal: 0, Hash: "938bc0c", When: when, Subject: "init: store"},
	}}}
	cmd := newLogCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	rendered := out.String()
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and three rows, got %d lines:\n%q", len(lines), rendered)
	}
	want := strings.Index(lines[0], "OPERATION")
	if want < 0 {
		t.Fatalf("no OPERATION header:\n%q", rendered)
	}
	for _, subject := range []string{"new: alpha", "init: store"} {
		line := ""
		for _, candidate := range lines[1:] {
			if strings.Contains(candidate, subject) {
				line = candidate
			}
		}
		if line == "" {
			t.Fatalf("no row carries %q:\n%q", subject, rendered)
		}
		if got := strings.Index(line, subject); got != want {
			t.Errorf("%q starts at column %d, header OPERATION at %d:\n%q", subject, got, want, rendered)
		}
	}
	for i, line := range lines {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line %d ends in padding: %q", i, line)
		}
	}
}
