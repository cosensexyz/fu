package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectAndDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := len(Detected()); got != 0 {
		t.Fatalf("nothing installed, want 0 detected, got %d", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	det := Detected()
	if len(det) != 2 {
		t.Fatalf("want 2 detected, got %d", len(det))
	}
	c, ok := ByName("claude")
	if !ok || c.SkillsDir() != filepath.Join(home, ".claude", "skills") {
		t.Fatalf("claude skills dir wrong: %q", c.SkillsDir())
	}
	x, _ := ByName("codex")
	if x.Reserved()[0] != ".system" {
		t.Fatal("codex must reserve .system (SPEC rule 11)")
	}
}

// Finding I6: with HOME unset, Detect must report false and SkillsDir
// must return "" for every known adapter, rather than degrading to
// ".claude/skills" or ".codex/skills" -- a path resolved relative to the
// process's current working directory. Detected must then report zero
// agents, so an adapter with no resolvable home directory never reaches
// the scan/reconcile paths through the normal flow. Every other adapter
// test in this file sets HOME via t.Setenv and never covers this case.
func TestDetectAndSkillsDirWithHomeUnset(t *testing.T) {
	t.Setenv("HOME", "")
	for _, a := range All() {
		if a.Detect() {
			t.Fatalf("%s: Detect must report false when HOME is unset", a.Name())
		}
		if got := a.SkillsDir(); got != "" {
			t.Fatalf("%s: SkillsDir must be empty when HOME is unset, got %q", a.Name(), got)
		}
	}
	if got := len(Detected()); got != 0 {
		t.Fatalf("HOME unset: want 0 detected agents, got %d", got)
	}
}

// Round 5 finding: the HOME==""" guard above covers only half of what its
// own comment claims to prevent. A *relative* HOME is not empty, so it
// sailed past and got joined into ".claude/skills" all the same -- the
// very cwd-relative path the guard exists to refuse. Reproduced against
// the compiled binary: run from a project directory that has its own
// ./.claude and ./.codex, `env HOME=. fu new alpha` wrote links into the
// project's own agent config directories and treated them as a global
// installation.
//
// Detect is guarded for the same reason and in the same breath: it stats
// filepath.Join(home, ".claude"), so a relative HOME makes it report a
// project's own directory as an installed agent. Its existing doc comment
// already states that refusing detection is what keeps an agent with no
// resolvable home out of the scan/reconcile paths entirely; a relative
// HOME is exactly such a home.
func TestDetectAndSkillsDirWithRelativeHome(t *testing.T) {
	// A directory that really does contain both agents' marker
	// directories, so nothing here fails merely for want of content: if
	// these adapters resolved a relative HOME at all, they would find
	// something.
	project := t.TempDir()
	for _, marker := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(project, marker), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(project)

	for _, home := range []string{".", "./", "relative/home", ".."} {
		t.Run(home, func(t *testing.T) {
			t.Setenv("HOME", home)
			for _, a := range All() {
				if a.Detect() {
					t.Errorf("%s: Detect must report false for a relative HOME %q -- it would "+
						"otherwise report a project's own directory as an installed agent", a.Name(), home)
				}
				if got := a.SkillsDir(); got != "" {
					t.Errorf("%s: SkillsDir must be empty for a relative HOME %q, got %q -- a path "+
						"resolved against the process's own working directory", a.Name(), home, got)
				}
			}
			if got := len(Detected()); got != 0 {
				t.Errorf("relative HOME %q: want 0 detected agents, got %d", home, got)
			}
		})
	}
}
