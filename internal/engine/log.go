// internal/engine/log.go
package engine

import (
	"fmt"
	"strings"
	"time"
)

// LogEntry is one row of `fu log`. Ordinal is the number `fu revert n` would
// use for this commit, 0 for a commit that is not an operation (a sweep, a
// recovery compensation and the operation it cancels, `init: store`).
// Subject is the message's first line; Body its first non-empty line after
// that, or empty.
type LogEntry struct {
	Ordinal int
	Hash    string
	When    time.Time
	Subject string
	Body    string
}

// LogOutcome carries the rows and the config-level diagnostics every read
// command owes its caller (ReadDiagnostics).
type LogOutcome struct {
	Entries     []LogEntry
	Diagnostics ReadDiagnostics
}

// splitLogMessage separates a commit message into its subject line and the
// first line of its body. fu's own messages are one line; `fu commit -m`
// appends the user's text after a blank line, and a direct-git commit may
// hold anything.
//
// "Blank" is decided by TrimSpace rather than by the line-ending bytes,
// because those are not the only thing a blank line can be made of. Skipping
// over "\r" and "\n" alone stopped at a line of spaces or of a lone tab: that
// line became the body, the CLI's own trim then rendered it away, and the
// real body below it was never reached -- a silent drop, against LogEntry's
// promise of "its first non-empty line after that" (review 2026-09-03,
// Minor). It also subsumes the CRLF case an earlier fix handled separately: a
// remainder beginning "\r\n" is a blank line under TrimSpace like any other.
//
// Leading whitespace on a line that has content is left alone. Only wholly
// blank lines are passed over; an indented body line is the body.
func splitLogMessage(message string) (subject, bodyFirstLine string) {
	subject, rest, _ := strings.Cut(message, "\n")
	subject = strings.TrimRight(subject, "\r")
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		if strings.TrimSpace(line) != "" {
			return subject, strings.TrimRight(line, "\r")
		}
	}
	return subject, ""
}

// Log lists up to count commits of the store's first-parent history, newest
// first. It is a read-only command: no lock, no sweep, nothing written
// (SPEC §9). count must be positive; the CLI refuses anything else as a
// usage error before reaching here.
func (a *Application) Log(count int) (LogOutcome, error) {
	if count < 1 {
		return LogOutcome{}, fmt.Errorf("log count must be >= 1, got %d", count)
	}
	st, cfg, err := a.readStore()
	if err != nil {
		return LogOutcome{}, err
	}
	entries, err := st.Log(count)
	if err != nil {
		return LogOutcome{}, fmt.Errorf("read store history: %w", err)
	}
	outcome := LogOutcome{Entries: make([]LogEntry, 0, len(entries)), Diagnostics: readDiagnostics(st, cfg, nil)}
	for _, e := range entries {
		subject, body := splitLogMessage(e.Message)
		outcome.Entries = append(outcome.Entries, LogEntry{
			Ordinal: e.Ordinal,
			Hash:    e.Hash,
			When:    e.When,
			Subject: subject,
			Body:    body,
		})
	}
	return outcome, nil
}
