// internal/engine/scenario_test.go
package engine

import (
	"os"
	"path/filepath"
	"testing"

	"fu/internal/agent"
)

// Walks SPEC scenarios covered by plan 1: create (7), toggle (2), and
// the store layer of mistake recovery (5).
func TestScenarioWalkthrough(t *testing.T) {
	s, _ := setupStore(t)
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}, fakeAgent{"codex", codexDir}}

	// Scenario 7: author a skill, edit it, record via next write op sweep.
	if _, err := NewSkill(s, agents, "writer"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.SkillsDir(), "writer", "notes.md"), []byte("draft"), 0o644)

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
