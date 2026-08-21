package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
	"github.com/cosensexyz/fu/internal/store"
)

type fakeRevertApplication struct {
	n       int
	result  engine.Result
	changed []string
	err     error
}

func (f *fakeRevertApplication) Revert(n int) (engine.RevertOutcome, error) {
	f.n = n
	return engine.RevertOutcome{Result: f.result, Changed: f.changed}, f.err
}

func TestRevertCommandPassesTheOperationCount(t *testing.T) {
	app := &fakeRevertApplication{}
	cmd := newRevertCmd(app)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if app.n != 2 {
		t.Fatalf("revert count = %d, want 2", app.n)
	}
	if !strings.Contains(stdout.String(), "reverted 2 operation") {
		t.Fatalf("revert output must say what it did:\n%s", stdout.String())
	}
}

func TestRevertCommandRejectsANonPositiveCount(t *testing.T) {
	cmd := newRevertCmd(&fakeRevertApplication{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"0"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("a non-positive count must be refused")
	}
}

// TestRevertMalformedCountIsAUsageError pins the exit code. `fu revert` with
// no argument exits 2 with usage (usageArgs handles it), while `fu revert abc`,
// `fu revert 0` and `fu revert -1` exited 1 with no usage at all -- the same
// class of mistake reported two different ways by one command. DESIGN §7's
// exit-2 enumeration covers malformed flag *values* and does not reach
// positional arguments, so this was not a strict violation; it was still one
// command contradicting itself.
//
// "-1" is not in this table: pflag claims it as an unknown shorthand flag
// before RunE is reached, and the root command's SetFlagErrorFunc already
// classifies that as a usage error, so it exits 2 by a different route that
// this per-command test cannot exercise.
func TestRevertMalformedCountIsAUsageError(t *testing.T) {
	for _, arg := range []string{"abc", "0"} {
		t.Run(arg, func(t *testing.T) {
			cmd := newRevertCmd(&fakeRevertApplication{})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{arg})
			err := cmd.Execute()
			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("`fu revert %s` must be a usage error, got %T %v", arg, err, err)
			}
		})
	}
}

// TestRevertCommandReportsThePathsItChanged pins the report `fu restore --hard`
// has always given and revert did not. "reverted 2 operation(s)" is not an
// account a user can check anything against; Store.Revert has had this list in
// hand at its applyTreeToWorktree call all along and threw it away. It is also
// the line that makes a revert which rewrote fu.yaml visible at all.
func TestRevertCommandReportsThePathsItChanged(t *testing.T) {
	app := &fakeRevertApplication{changed: []string{"fu.yaml", "skills/alpha/SKILL.md"}}
	cmd := newRevertCmd(app)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{"changed 2 path(s)", "fu.yaml", "skills/alpha/SKILL.md", "reverted 1 operation(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("revert output missing %q:\n%s", want, out)
		}
	}
}

// TestRevertCommandIntegrationRollsTheStoreBack drives the real command tree,
// not a fake, and is the only case in this package that does.
//
// Every other test here constructs newRevertCmd(&fakeRevertApplication{})
// directly, so nothing exercised NewRootCmd's registration or
// Application.Revert's wiring: deleting newRevertCmd(app) from root.go left
// ./internal/cli, ./cmd and ./internal/engine entirely green. Both sibling
// commands delivered in this batch already have this layer
// (TestRestoreCommandIntegration..., TestStatusCommandIntegration...); revert
// was the one exception.
func TestRevertCommandIntegrationRollsTheStoreBack(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	if _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "new", "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "new", "beta"); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "revert", "1")
	if err != nil {
		t.Fatalf("revert must succeed: %v (%s)", err, out)
	}
	if !strings.Contains(out, "reverted 1 operation(s)") {
		t.Fatalf("revert output missing its confirmation:\n%s", out)
	}
	// The store actually moved: beta's content is gone from the worktree and
	// its link with it, while alpha survives.
	if _, err := os.Stat(filepath.Join(st.SkillsDir(), "beta")); !os.IsNotExist(err) {
		t.Fatalf("the reverted operation's content must be gone from the store worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.SkillsDir(), "alpha")); err != nil {
		t.Fatalf("the operation before it must survive: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "beta")); !os.IsNotExist(err) {
		t.Fatalf("the link layer must be rebuilt after the revert: %v", err)
	}
	// A revert is itself an operation, so it publishes a commit of its own.
	after, err := st.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() == after.Hash() {
		t.Fatal("revert must publish its result as a new commit")
	}
}
