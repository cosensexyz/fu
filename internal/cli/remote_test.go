package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakeRemoteApplication struct {
	current engine.RemoteOutcome
	set     string
	setErr  error
}

func (f *fakeRemoteApplication) Remote() (engine.RemoteOutcome, error) { return f.current, nil }

func (f *fakeRemoteApplication) SetRemote(url string) (engine.RemoteOutcome, error) {
	f.set = url
	return engine.RemoteOutcome{URL: url, Configured: true, Previous: f.current.URL}, f.setErr
}

func runRemote(t *testing.T, app *fakeRemoteApplication, args ...string) (string, error) {
	t.Helper()
	cmd := newRemoteCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRemoteCommandPrintsTheURLOrSaysNoneIsSet(t *testing.T) {
	out, err := runRemote(t, &fakeRemoteApplication{})
	if err != nil || !strings.Contains(out, "no remote configured; set one with fu remote <url>") {
		t.Fatalf("unset remote output = %q err=%v", out, err)
	}
	out, err = runRemote(t, &fakeRemoteApplication{current: engine.RemoteOutcome{URL: "ssh://h/s.git", Configured: true}})
	if err != nil || strings.TrimSpace(out) != "ssh://h/s.git" {
		t.Fatalf("configured remote output = %q err=%v", out, err)
	}
}

func TestRemoteCommandSetsTheURLAndNamesTheReplacedOne(t *testing.T) {
	app := &fakeRemoteApplication{}
	out, err := runRemote(t, app, "/srv/one.git")
	if err != nil || app.set != "/srv/one.git" || !strings.Contains(out, "remote set to /srv/one.git") || strings.Contains(out, "was") {
		t.Fatalf("first set output = %q set=%q err=%v", out, app.set, err)
	}
	app = &fakeRemoteApplication{current: engine.RemoteOutcome{URL: "/srv/one.git", Configured: true}}
	out, err = runRemote(t, app, "/srv/two.git")
	if err != nil || !strings.Contains(out, "remote set to /srv/two.git (was /srv/one.git)") {
		t.Fatalf("overwrite output = %q err=%v", out, err)
	}
}

func TestRemoteCommandArgumentErrorsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{""}, {"a", "b"}} {
		_, err := runRemote(t, &fakeRemoteApplication{}, args...)
		var uerr *UsageError
		if !errors.As(err, &uerr) {
			t.Fatalf("fu remote %q must be a usage error, got %v", args, err)
		}
	}
}
