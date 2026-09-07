package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakePullApplication struct {
	outcome engine.PullOutcome
	err     error
}

func (f *fakePullApplication) Pull() (engine.PullOutcome, error) { return f.outcome, f.err }

func runPull(t *testing.T, app *fakePullApplication, args ...string) (string, error) {
	t.Helper()
	cmd := newPullCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestPullCommandReportsEachOutcome(t *testing.T) {
	cases := []struct {
		name    string
		outcome engine.PullOutcome
		want    string
	}{
		{"fast-forward", engine.PullOutcome{URL: "/srv/s.git", Branch: "master", From: "abc1234", To: "def5678", Changed: []string{"fu.yaml", "skills/x/SKILL.md"}}, "fast-forwarded master abc1234..def5678, 2 path(s) changed"},
		{"up to date", engine.PullOutcome{URL: "/srv/s.git", UpToDate: true}, "already up to date with /srv/s.git"},
		{"empty remote", engine.PullOutcome{URL: "/srv/s.git", EmptyRemote: true}, "remote /srv/s.git has no commits yet; nothing to pull"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPull(t, &fakePullApplication{outcome: tc.outcome})
			if err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("output = %q err=%v, want %q", out, err, tc.want)
			}
		})
	}
}

func TestPullCommandSurfacesTheError(t *testing.T) {
	_, err := runPull(t, &fakePullApplication{err: engine.ErrNoRemoteBranch})
	if !errors.Is(err, engine.ErrNoRemoteBranch) {
		t.Fatalf("pull must return the engine error, got %v", err)
	}
}

// Pins that a failed pull still surfaces whatever reconcile report the
// engine returned alongside the error, the same guarantee push gives.
func TestPullCommandSurfacesTheErrorAndTheReconcileReport(t *testing.T) {
	app := &fakePullApplication{
		outcome: engine.PullOutcome{Result: engine.Result{Warnings: []string{"recovered something"}}},
		err:     engine.ErrNoRemoteBranch,
	}
	out, err := runPull(t, app)
	if !errors.Is(err, engine.ErrNoRemoteBranch) || !strings.Contains(out, "warning: recovered something") {
		t.Fatalf("pull must print the report and return the error, out=%q err=%v", out, err)
	}
}

func TestPullCommandRejectsArguments(t *testing.T) {
	_, err := runPull(t, &fakePullApplication{}, "extra")
	var uerr *UsageError
	if !errors.As(err, &uerr) {
		t.Fatalf("fu pull extra must be a usage error, got %v", err)
	}
}

func TestPullCommandDoesNotConfirmAnIncompleteFastForward(t *testing.T) {
	for _, failure := range []error{errors.New("advance branch: permission denied"), engine.ErrOperationFailed} {
		app := &fakePullApplication{
			outcome: engine.PullOutcome{Branch: "master", From: "abc1234", To: "def5678"},
			err:     failure,
		}
		out, err := runPull(t, app)
		if !errors.Is(err, failure) || strings.Contains(out, "fast-forwarded ") || strings.Contains(out, "already up to date") {
			t.Fatalf("branch metadata must not confirm an incomplete pull: out=%q err=%v", out, err)
		}
	}
}
