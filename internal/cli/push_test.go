package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakePushApplication struct {
	outcome engine.PushOutcome
	err     error
}

func (f *fakePushApplication) Push() (engine.PushOutcome, error) { return f.outcome, f.err }

func runPush(t *testing.T, app *fakePushApplication, args ...string) (string, error) {
	t.Helper()
	cmd := newPushCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestPushCommandReportsWhatWasPushed(t *testing.T) {
	out, err := runPush(t, &fakePushApplication{outcome: engine.PushOutcome{URL: "/srv/s.git", Branch: "master", Head: "abc1234"}})
	if err != nil || !strings.Contains(out, "pushed master (abc1234) to /srv/s.git") {
		t.Fatalf("push output = %q err=%v", out, err)
	}
	out, err = runPush(t, &fakePushApplication{outcome: engine.PushOutcome{URL: "/srv/s.git", UpToDate: true}})
	if err != nil || !strings.Contains(out, "already up to date with /srv/s.git") {
		t.Fatalf("up-to-date output = %q err=%v", out, err)
	}
}

func TestPushCommandSurfacesTheErrorAndTheReconcileReport(t *testing.T) {
	app := &fakePushApplication{
		outcome: engine.PushOutcome{Result: engine.Result{Warnings: []string{"recovered something"}}},
		err:     engine.ErrNoRemote,
	}
	out, err := runPush(t, app)
	if !errors.Is(err, engine.ErrNoRemote) || !strings.Contains(out, "warning: recovered something") {
		t.Fatalf("push must print the report and return the error, out=%q err=%v", out, err)
	}
}

func TestPushCommandRejectsArguments(t *testing.T) {
	_, err := runPush(t, &fakePushApplication{}, "extra")
	var uerr *UsageError
	if !errors.As(err, &uerr) {
		t.Fatalf("fu push extra must be a usage error, got %v", err)
	}
}

func TestPushCommandDoesNotConfirmAnIncompleteTransfer(t *testing.T) {
	for _, failure := range []error{errors.New("connection refused"), engine.ErrOperationFailed} {
		app := &fakePushApplication{
			outcome: engine.PushOutcome{URL: "/srv/s.git", Branch: "master", Head: "abc1234"},
			err:     failure,
		}
		out, err := runPush(t, app)
		if !errors.Is(err, failure) || strings.Contains(out, "pushed ") || strings.Contains(out, "already up to date") {
			t.Fatalf("branch metadata must not confirm an incomplete push: out=%q err=%v", out, err)
		}
	}
}
