// internal/cli/delivery_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"

	"github.com/cosensexyz/fu/internal/engine"
)

// The hint answers one question -- will restarting an agent look different --
// and must not claim so after a pass that changed nothing.
func TestDeliveryHintDependsOnWhatActuallyChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result engine.Result
		want   string
	}{
		{"nothing changed", engine.Result{}, ""},
		{"only diagnostics", engine.Result{Conflicts: []engine.Action{{Skill: "alpha"}}}, ""},
		{"a link created", engine.Result{Created: 1}, "takes effect in new agent sessions"},
		{"a link removed", engine.Result{Removed: 1}, "takes effect in new agent sessions"},
		{
			"changed with a conflict",
			engine.Result{Created: 1, Conflicts: []engine.Action{{Skill: "beta"}}},
			"takes effect in new agent sessions for the links that changed; see diagnostics",
		},
		{
			"changed with a failure",
			engine.Result{Removed: 1, Failed: []engine.FailedAction{{}}},
			"takes effect in new agent sessions for the links that changed; see diagnostics",
		},
		{
			"changed with an agent skipped",
			engine.Result{Created: 1, Skipped: []string{"codex"}},
			"takes effect in new agent sessions for the links that changed; see diagnostics",
		},
		{
			"changed with a store-side gap",
			engine.Result{Created: 1, Missing: []engine.Action{{Skill: "gamma"}}},
			"takes effect in new agent sessions for the links that changed; see diagnostics",
		},
		{
			// A standing fu.yaml property, not something this run met and
			// declined: qualifying on it would qualify every command from
			// then on, over a name none of them touched.
			"changed with an unrelated bad name in fu.yaml",
			engine.Result{Created: 1, Invalid: []engine.Action{{Skill: "Bad_Name"}}},
			"takes effect in new agent sessions",
		},
		{
			"changed with a reserved name in fu.yaml",
			engine.Result{Created: 1, Reserved: []engine.Action{{Skill: ".system"}}},
			"takes effect in new agent sessions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliveryHint(tc.result); got != tc.want {
				t.Fatalf("deliveryHint = %q, want %q", got, tc.want)
			}
		})
	}
}

// A command that projected a new link says so; SPEC rule 8's promise is not
// the switch commands' alone.
func TestNewCommandReportsWhenTheLinkTakesEffect(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")

	out, err := runCmd(t, "new", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "created alpha") {
		t.Fatalf("confirmation missing:\n%s", out)
	}
	if !strings.Contains(out, "takes effect in new agent sessions") {
		t.Fatalf("want the delivery hint after a command that projected a link:\n%s", out)
	}
}

// With no agent installed there is nothing to project, so the same command
// must not promise a restart will change anything.
func TestNewCommandDoesNotClaimEffectWithNoAgent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")

	out, err := runCmd(t, "new", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "created alpha") {
		t.Fatalf("confirmation missing:\n%s", out)
	}
	if strings.Contains(out, "takes effect") {
		t.Fatalf("nothing was projected; the hint must not appear:\n%s", out)
	}
}

// rm removes the link as well as the skill, and says so.
func TestRmCommandReportsWhenTheRemovalTakesEffect(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "rm", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed alpha") || !strings.Contains(out, "takes effect in new agent sessions") {
		t.Fatalf("want the removal confirmed with its hint:\n%s", out)
	}
}

// restore is the command whose whole purpose is re-delivery, so it reports
// what it rebuilt -- and stays quiet when the projection was already correct.
func TestRestoreCommandReportsOnlyWhenItRebuiltSomething(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	mustMkdirAll(t, claude)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "restore")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "takes effect") {
		t.Fatalf("the projection was already correct; nothing takes effect:\n%s", out)
	}

	if err := os.Remove(filepath.Join(claude, "skills", "alpha")); err != nil {
		t.Fatal(err)
	}
	out, err = runCmd(t, "restore")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "takes effect in new agent sessions") {
		t.Fatalf("want the rebuild reported:\n%s", out)
	}
}

// Enabling a skill that is already on changes no link, so the switch commands
// stop claiming a restart will show something new (SPEC rule 8 is a promise
// about a change).
func TestToggleDoesNotClaimEffectWhenNoLinkChanged(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "enable", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "enabled alpha globally") {
		t.Fatalf("confirmation missing:\n%s", out)
	}
	if strings.Contains(out, "takes effect") {
		t.Fatalf("alpha was already on and linked; nothing takes effect:\n%s", out)
	}
}

// The same command still reports effect when the switch really does change a
// link, which is the case SPEC rule 8 was written for.
func TestToggleStillReportsEffectWhenALinkChanged(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "disable", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled alpha globally; takes effect in new agent sessions") {
		t.Fatalf("want the effect reported for a real change:\n%s", out)
	}
}

// fu commit reconciles like every other write command, so recording a hand
// edit to fu.yaml can take a link away in the same run -- and that is exactly
// the case where the user has no other signal that their agents changed.
func TestCommitReportsALinkItsReconcileRemoved(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	mustMkdirAll(t, claude)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	config := filepath.Join(fuHome, "store", "fu.yaml")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "enabled: true", "enabled: false", 1)
	if edited == string(body) {
		t.Fatalf("precondition: fu.yaml must hold an enabled skill:\n%s", body)
	}
	if err := os.WriteFile(config, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "commit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(claude, "skills", "alpha")); !os.IsNotExist(err) {
		t.Fatalf("precondition: the reconcile must have removed the link: %v", err)
	}
	if !strings.Contains(out, "takes effect in new agent sessions") {
		t.Fatalf("want the removal reported:\n%s", out)
	}
}

// push reconciles before it pushes -- its prologue is a full write-command
// prologue -- so it projects the links an agent detected since the last write
// command was owed, on a run whose own output is entirely about the remote.
func TestPushReportsTheLinksItsPrologueProjected(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	bare := filepath.Join(t.TempDir(), "store.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "remote", bare)
	// codex appears only now, so its link is owed and nothing but this push
	// will project it.
	mustMkdirAll(t, filepath.Join(home, ".codex"))

	out, err := runCmd(t, "push")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(home, ".codex", "skills", "alpha")); err != nil {
		t.Fatalf("precondition: the prologue must have projected codex's link: %v", err)
	}
	if !strings.Contains(out, "takes effect in new agent sessions") {
		t.Fatalf("want the projection reported:\n%s", out)
	}

	// A second push has nothing left to project and must not say otherwise.
	out, err = runCmd(t, "push")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "takes effect") {
		t.Fatalf("nothing was projected; the hint must not appear:\n%s", out)
	}
}

// add's abort exits carry the prologue's findings out with them; they must
// carry its effect too. The prologue is a full write-command prologue, so it
// has already projected what an agent detected since the last write command
// was owed by the time the user aborts -- and "nothing selected; nothing
// installed" is true about the source and false about the agent directories.
func TestAddReportsWhatItsPrologueProjectedWhenNothingIsSelected(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	// codex appears only now, so its link is owed and the prologue projects it.
	mustMkdirAll(t, filepath.Join(home, ".codex"))

	// Two candidates, so add prompts rather than auto-selecting the only one.
	source := t.TempDir()
	writeSkill(t, source, "pdf-tools")
	writeSkill(t, source, "writer")

	// An empty selection: the user aborts, nothing is installed.
	out, _ := runCmdWithInput(t, "\n", "add", source)
	if _, err := os.Readlink(filepath.Join(home, ".codex", "skills", "alpha")); err != nil {
		t.Fatalf("precondition: the prologue must have projected codex's link: %v", err)
	}
	if !strings.Contains(out, "nothing selected; nothing installed") {
		t.Fatalf("precondition: the run must have aborted:\n%s", out)
	}
	if !strings.Contains(out, "takes effect in new agent sessions") {
		t.Fatalf("want the projection the prologue made reported:\n%s", out)
	}
}
