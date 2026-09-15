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

// `fu revert` must tell the user that reverting an adopt did not put back what
// adopt moved aside, at the moment they run it.
//
// SPEC scenario 5 points at revert as the remedy for a committed mistake, and
// adopting the wrong directory is such a mistake. Before this, the command
// printed "reverted 1 operation(s)" and the user's skills directory was simply
// empty -- the content safe under recovery/ and nothing on screen connecting
// the two.
func TestRevertNamesAnAdoptItCouldNotPutBack(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	skills := filepath.Join(home, ".claude", "skills", "notes")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"),
		[]byte("---\nname: notes\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	if _, err := runCmd(t, "adopt"); err != nil {
		t.Fatal(err)
	}

	_, errOut, err := runCmdSplit(t, "revert", "1")
	if err != nil {
		t.Fatal(err)
	}
	// Both archive forms, because which one exists depends on what adopt
	// displaced and a user sent after the wrong one finds nothing. Naming only
	// the directory copy left the symlink cases -- including rule 10's, where
	// the agent's whole skills directory is the link -- hunting for a file
	// that was never written.
	for _, want := range []string{
		"notes", "does not put back", "by hand",
		"adopt-archive-*", "adopt-link-*.json", "skills directory",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("revert must say what it could not restore and where it is; missing %q in:\n%s", want, errOut)
		}
	}
}

// A revert that refused to act must say nothing at all.
//
// The names are read before the revert runs, so a refusal that never reaches
// the worktree leaves them on the outcome. Printing them then tells a user
// whose count was out of range that their adopted skill was rolled back and
// should be copied out of recovery/ by hand -- while the link and the store
// copy are both still there, so following that advice overwrites fu's own
// link. That is the same false statement the warning exists to prevent, with
// the sign flipped.
func TestRevertSaysNothingWhenItRefusedToAct(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	skills := filepath.Join(home, ".claude", "skills", "notes")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"),
		[]byte("---\nname: notes\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	if _, err := runCmd(t, "adopt"); err != nil {
		t.Fatal(err)
	}

	_, errOut, err := runCmdSplit(t, "revert", "5")
	if err == nil {
		t.Fatal("reverting past the start of history must fail")
	}
	if strings.Contains(errOut, "does not put back") {
		t.Fatalf("a refused revert undid nothing and must claim nothing:\n%s", errOut)
	}
	// And the adopt really is intact, which is what makes the claim wrong.
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "notes")); err != nil {
		t.Fatalf("the adopt must be untouched after a refused revert: %v", err)
	}
}

// And it says nothing when no adopt was undone, or the warning becomes noise
// on every revert and stops being read.
func TestRevertSaysNothingAboutAdoptWhenNoneWasUndone(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")
	if _, err := runCmd(t, "new", "writer"); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runCmdSplit(t, "revert", "1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errOut, "adopt") {
		t.Fatalf("reverting a `new` displaced nothing and must not mention adopt:\n%s", errOut)
	}
}

// A revert that fails partway must still say which adopt it undid.
//
// The per-name judgement in the engine is what makes that report *correct* on
// an error path; this placement -- above the error return -- is the only thing
// that makes it *visible* there, and nothing asserted it. Moving the call back
// below the return left every package green while the user, mid-failure, got
// the changed paths and no word that one of them was an adopt whose original
// is now only in recovery/. That is the silent direction, and it is this
// change's own subject.
//
// The failure is injected the way the engine test injects it: two adopted
// skills, the first-sorted one's directory made unwritable so the deletion
// pass fails on it after removing the other.
func TestRevertStillNamesAnAdoptWhenItFailsPartway(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not refused the unlink")
	}
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	for _, name := range []string{"note", "notes"} {
		dir := filepath.Join(home, ".claude", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
			[]byte("---\nname: "+name+"\ndescription: d\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runCmd(t, "init")
	if _, err := runCmd(t, "adopt"); err != nil {
		t.Fatal(err)
	}

	stuck := filepath.Join(fuHome, "store", "skills", "note")
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	_, errOut, err := runCmdSplit(t, "revert", "2")
	if err == nil {
		t.Fatal("the unlink of an unwritable directory's file must fail the revert")
	}
	if !strings.Contains(errOut, "does not put back") {
		t.Fatalf("a revert that undid an adopt before failing must still say so:\n%s", errOut)
	}
	if !strings.Contains(errOut, "notes") {
		t.Fatalf("the adopt it did undo must be named:\n%s", errOut)
	}
	// And the one it never reached must not be, since its link and store copy
	// are both still live.
	if strings.Contains(errOut, "note,") || strings.Contains(errOut, ", note") {
		t.Fatalf("the untouched adopt must not be named:\n%s", errOut)
	}
}
