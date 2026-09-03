// internal/cli/commit_test.go
package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakeCommitApplication struct {
	name, message string
	outcome       engine.CommitOutcome
	err           error
}

func (f *fakeCommitApplication) Commit(name, message string) (engine.CommitOutcome, error) {
	f.name, f.message = name, message
	return f.outcome, f.err
}

func runCommit(t *testing.T, app *fakeCommitApplication, args ...string) (string, error) {
	t.Helper()
	cmd := newCommitCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCommitCommandPassesNameAndMessageAndReportsWhatItRecorded(t *testing.T) {
	app := &fakeCommitApplication{outcome: engine.CommitOutcome{
		Written: true, Subject: "commit: alpha", Changed: []string{"skills/alpha/SKILL.md"},
	}}
	out, err := runCommit(t, app, "alpha", "-m", "why")
	if err != nil {
		t.Fatal(err)
	}
	if app.name != "alpha" || app.message != "why" {
		t.Fatalf("app got (%q, %q)", app.name, app.message)
	}
	for _, want := range []string{"commit: alpha", "skills/alpha/SKILL.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output must name %q:\n%s", want, out)
		}
	}
}

func TestCommitCommandWithoutANameRecordsTheWholeStore(t *testing.T) {
	app := &fakeCommitApplication{outcome: engine.CommitOutcome{Written: true, Subject: "commit: alpha, fu.yaml", Changed: []string{"fu.yaml", "skills/alpha/SKILL.md"}}}
	if _, err := runCommit(t, app); err != nil {
		t.Fatal(err)
	}
	if app.name != "" {
		t.Fatalf("name = %q, want empty for a store-wide commit", app.name)
	}
}

func TestCommitCommandSaysNothingToCommitAndExitsZero(t *testing.T) {
	app := &fakeCommitApplication{}
	out, err := runCommit(t, app, "alpha")
	if err != nil {
		t.Fatalf("nothing to commit is not a failure: %v", err)
	}
	if !strings.Contains(out, "nothing to commit") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestCommitCommandUsageErrors(t *testing.T) {
	for _, args := range [][]string{{""}, {"alpha", "-m", ""}, {"alpha", "beta"}} {
		_, err := runCommit(t, &fakeCommitApplication{}, args...)
		var usage *UsageError
		if !errors.As(err, &usage) {
			t.Errorf("`fu commit %q` must be a usage error, got %T %v", args, err, err)
		}
	}
}

func TestCommitCommandReportsAWrittenCommitBeforeAnError(t *testing.T) {
	app := &fakeCommitApplication{
		outcome: engine.CommitOutcome{Written: true, Subject: "commit: alpha", Changed: []string{"skills/alpha/SKILL.md"}},
		err:     errors.New("reconcile failed"),
	}
	out, err := runCommit(t, app, "alpha")
	if err == nil {
		t.Fatal("the error must propagate")
	}
	if !strings.Contains(out, "commit: alpha") {
		t.Fatalf("a durable commit must be reported even when the command fails afterwards:\n%s", out)
	}
}

// The brief's rendering (Written == false -> "nothing to commit") predates
// engine.CommitOutcome.ExternalWritten. When a no-name run finds nothing
// staged directly with git and yet the derived store-wide candidate is
// still empty (`git add -A` followed by `fu commit -m "reason"`: the
// external layer consumes the whole difference, leaving nothing for the
// second candidate), a commit was still written to the store -- just not
// the one the user's -m text described. "nothing to commit" would be false
// in that case, so this must exit 0 without that line and instead name the
// external commit that was actually recorded.
func TestCommitCommandReportsExternalWrittenInsteadOfNothingToCommit(t *testing.T) {
	app := &fakeCommitApplication{outcome: engine.CommitOutcome{ExternalWritten: true}}
	out, err := runCommit(t, app)
	if err != nil {
		t.Fatalf("an external-only commit is not a failure: %v", err)
	}
	if strings.Contains(out, "nothing to commit") {
		t.Fatalf("a commit was durably recorded; must not say nothing to commit:\n%s", out)
	}
	if !strings.Contains(out, "external: manual modifications") {
		t.Fatalf("output must name the external commit that was actually recorded:\n%s", out)
	}
}

// Same shape, but the user did supply -m: their message must not be
// silently swallowed -- the output must say plainly that it was not
// recorded, and why.
func TestCommitCommandReportsExternalWrittenMessageWasNotRecorded(t *testing.T) {
	app := &fakeCommitApplication{outcome: engine.CommitOutcome{ExternalWritten: true}}
	out, err := runCommit(t, app, "-m", "reason")
	if err != nil {
		t.Fatalf("an external-only commit is not a failure: %v", err)
	}
	if !strings.Contains(out, "external: manual modifications") {
		t.Fatalf("output must name the external commit that was actually recorded:\n%s", out)
	}
	if !strings.Contains(out, "-m") || !strings.Contains(out, "not recorded") {
		t.Fatalf("output must say plainly that the -m message was not recorded:\n%s", out)
	}
}

// Written and ExternalWritten are independent facts, not alternatives
// (engine.CommitOutcome's doc comment): a store-wide `fu commit` where some
// paths were staged directly with git and other hand edits were left
// pending records both an external snapshot and the derived candidate as
// separate commits on HEAD. Both must be reported, the external one first
// since it lands first chronologically -- the same order `git log` shows.
func TestCommitCommandReportsBothWrittenAndExternalWritten(t *testing.T) {
	app := &fakeCommitApplication{outcome: engine.CommitOutcome{
		Written: true, ExternalWritten: true,
		Subject: "commit: alpha", Changed: []string{"skills/alpha/SKILL.md"},
	}}
	out, err := runCommit(t, app)
	if err != nil {
		t.Fatal(err)
	}
	external := strings.Index(out, "external: manual modifications")
	written := strings.Index(out, "commit: alpha")
	if external == -1 || written == -1 {
		t.Fatalf("output must mention both durable commits:\n%s", out)
	}
	if external > written {
		t.Fatalf("the external commit lands first on HEAD and must be reported first:\n%s", out)
	}
}

// A durable external commit must be reported even when the run then fails,
// on the same reasoning TestCommitCommandReportsAWrittenCommitBeforeAnError
// already establishes for the Written case. The -m note must not appear
// here even though -m was given: under an error we cannot tell whether the
// second candidate was genuinely empty or simply failed to prepare, so
// claiming "nothing was left to record" would be a guess.
func TestCommitCommandReportsExternalWrittenBeforeAnError(t *testing.T) {
	app := &fakeCommitApplication{
		outcome: engine.CommitOutcome{ExternalWritten: true},
		err:     errors.New("reconcile failed"),
	}
	out, err := runCommit(t, app, "-m", "reason")
	if err == nil {
		t.Fatal("the error must propagate")
	}
	if !strings.Contains(out, "external: manual modifications") {
		t.Fatalf("a durable external commit must be reported even when the command fails afterwards:\n%s", out)
	}
	if strings.Contains(out, "not recorded") {
		t.Fatalf("the -m note must not appear under an error, since the second candidate's emptiness is not actually known:\n%s", out)
	}
}
