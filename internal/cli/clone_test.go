package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakeCloneApplication struct {
	url     string
	outcome engine.CloneOutcome
	err     error
}

func (f *fakeCloneApplication) Clone(url string) (engine.CloneOutcome, error) {
	f.url = url
	return f.outcome, f.err
}

func runClone(t *testing.T, app *fakeCloneApplication, args ...string) (string, error) {
	t.Helper()
	cmd := newCloneCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCloneCommandPassesTheURLAndReportsTheStore(t *testing.T) {
	home := t.TempDir()
	app := &fakeCloneApplication{outcome: engine.CloneOutcome{Home: home, URL: "/srv/s.git", Branch: "master", Skills: 3}}
	out, err := runClone(t, app, "/srv/s.git")
	if err != nil || app.url != "/srv/s.git" {
		t.Fatalf("clone must pass the url through, got %q err=%v", app.url, err)
	}
	want := "cloned store to " + filepath.Join(home, "store") + " from /srv/s.git (3 skill(s))"
	if !strings.Contains(out, want) {
		t.Fatalf("clone output = %q, want %q", out, want)
	}
}

func TestCloneCommandArgumentErrorsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {""}, {"a", "b"}} {
		_, err := runClone(t, &fakeCloneApplication{}, args...)
		var uerr *UsageError
		if !errors.As(err, &uerr) {
			t.Fatalf("fu clone %q must be a usage error, got %v", args, err)
		}
	}
}

// Pins that a failed clone still surfaces whatever reconcile report the
// engine returned alongside the error, the same guarantee push and pull give.
func TestCloneCommandSurfacesTheErrorAndTheReconcileReport(t *testing.T) {
	app := &fakeCloneApplication{
		outcome: engine.CloneOutcome{Result: engine.Result{Warnings: []string{"recovered something"}}},
		err:     errors.New("clone failed"),
	}
	out, err := runClone(t, app, "/srv/s.git")
	if err == nil || err.Error() != "clone failed" || !strings.Contains(out, "warning: recovered something") {
		t.Fatalf("clone must print the report and return the error, out=%q err=%v", out, err)
	}
}

// Pins the ordering rule TestDiagnosticsPrecedeConfirmation enshrines
// elsewhere in this package: on success, the reconcile report must precede
// the confirmation line in the merged output stream, not follow it.
func TestCloneCommandPrintsTheReconcileReportBeforeTheConfirmation(t *testing.T) {
	home := t.TempDir()
	app := &fakeCloneApplication{outcome: engine.CloneOutcome{
		Result: engine.Result{Warnings: []string{"recovered something"}},
		Home:   home, URL: "/srv/s.git", Branch: "master", Skills: 1,
	}}
	out, err := runClone(t, app, "/srv/s.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	warning := strings.Index(out, "warning: recovered something")
	confirmation := strings.Index(out, "cloned store to")
	if warning < 0 || confirmation < 0 {
		t.Fatalf("test setup: need both a warning and a confirmation, got %q", out)
	}
	if confirmation < warning {
		t.Fatalf("diagnostics must precede the confirmation:\n%s", out)
	}
}
