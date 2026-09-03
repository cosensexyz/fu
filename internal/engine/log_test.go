package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitLogMessage(t *testing.T) {
	cases := []struct{ in, subject, body string }{
		{"new: alpha", "new: alpha", ""},
		{"commit: alpha\n\nwhy\nmore", "commit: alpha", "why"},
		{"commit: alpha\n", "commit: alpha", ""},
		{"commit: alpha\n\n\nlate body", "commit: alpha", "late body"},
		// CRLF, as a direct-git commit made on Windows or through an editor
		// that writes CRLF carries it (final review, finding 4). Trimming
		// only "\n" off the remainder no-opped, because the leading byte is
		// "\r": the first body line came out as "\r" and then as "".
		{"commit: alpha\r\n\r\nwhy\r\nmore\r\n", "commit: alpha", "why"},
		{"commit: alpha\r\n", "commit: alpha", ""},
		// A blank line is not always empty. Trimming line-ending bytes off
		// the remainder skips "\n\n" but stops at a line of spaces or of a
		// lone tab, which then becomes the body and is rendered away by the
		// CLI's own trim -- so the real body was silently dropped, though
		// LogEntry's doc promises "its first non-empty line after that"
		// (review 2026-09-03, Minor).
		{"commit: alpha\n\n   \nwhy", "commit: alpha", "why"},
		{"commit: alpha\n\n\t\nwhy", "commit: alpha", "why"},
		{"commit: alpha\n\n \r\n\r\nwhy\r\n", "commit: alpha", "why"},
		// Whitespace to the end is still no body, not a cell of spaces.
		{"commit: alpha\n\n   \n  ", "commit: alpha", ""},
		// Leading whitespace on a real body line is content, and kept: only
		// wholly blank lines are skipped over.
		{"commit: alpha\n\n  indented", "commit: alpha", "  indented"},
	}
	for _, c := range cases {
		subject, body := splitLogMessage(c.in)
		if subject != c.subject || body != c.body {
			t.Errorf("splitLogMessage(%q) = (%q, %q), want (%q, %q)", c.in, subject, body, c.subject, c.body)
		}
	}
}

func TestApplicationLogNumbersOperationsAndStaysReadOnly(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	app := NewApplication()
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.NewSkill("alpha"); err != nil {
		t.Fatal(err)
	}
	st, err := app.openStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.SkillsDir(), "alpha", "SKILL.md"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Commit("alpha", "first draft"); err != nil {
		t.Fatal(err)
	}
	// A pending edit must survive a log call untouched: log never sweeps.
	if err := os.WriteFile(filepath.Join(st.SkillsDir(), "alpha", "SKILL.md"), []byte("pending"), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := app.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Entries) != 3 {
		t.Fatalf("got %d entries, want commit, new, init: %+v", len(outcome.Entries), outcome.Entries)
	}
	head := outcome.Entries[0]
	if head.Ordinal != 1 || head.Subject != "commit: alpha" || head.Body != "first draft" || len(head.Hash) != 7 {
		t.Fatalf("head = %+v", head)
	}
	if outcome.Entries[1].Ordinal != 2 || outcome.Entries[1].Subject != "new: alpha" {
		t.Fatalf("second = %+v", outcome.Entries[1])
	}
	if outcome.Entries[2].Ordinal != 0 || outcome.Entries[2].Subject != "init: store" {
		t.Fatalf("third = %+v", outcome.Entries[2])
	}
	dirty, err := st.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("log must not sweep the pending edit")
	}
	if outcome.Diagnostics.ConfigPath != st.ConfigPath() {
		t.Fatalf("diagnostics must name the config: %+v", outcome.Diagnostics)
	}
}

func TestApplicationLogRejectsANonPositiveCount(t *testing.T) {
	if _, err := NewApplication().Log(0); err == nil {
		t.Fatal("count 0 must be refused before the store is opened")
	}
}
