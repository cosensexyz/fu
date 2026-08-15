// internal/engine/scenario_test.go
package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
)

// Walks the SPEC scenarios this file covers: create (7), toggle (2), and the
// store layer of mistake recovery (5). Scenarios 1 and 6 are exercised
// functionally by add_test.go, adopt_test.go and adopt_whole_test.go rather
// than here; folding them into this walkthrough is tracked in DESIGN §8.
func TestScenarioWalkthrough(t *testing.T) {
	s, _ := setupStore(t)
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}, fakeAgent{"codex", codexDir}}

	// Scenario 7: author a skill, edit it, record via next write op sweep.
	if _, err := NewSkill(s, agents, "writer"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "writer", "notes.md"), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Scenario 2: temporarily disable for one agent only.
	if _, err := SetAgentSwitch(s, agents, "writer", "codex", false); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Log(2)
	if err != nil {
		t.Fatal(err)
	}
	if entries[1].Message != "external: manual modifications" {
		t.Fatal("manual edit must be swept before the toggle operation")
	}
	if _, err := os.Lstat(filepath.Join(codexDir, "writer")); !os.IsNotExist(err) {
		t.Fatal("codex must lose the link")
	}
	if _, err := os.Readlink(filepath.Join(claudeDir, "writer")); err != nil {
		t.Fatal("claude keeps the link")
	}

	// Scenario 5 (store layer): revert the toggle, reconcile restores links.
	if err := s.Revert(1); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(s, agents); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(codexDir, "writer")); err != nil {
		t.Fatal("revert + reconcile must restore the codex link")
	}
}
