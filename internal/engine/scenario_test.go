// internal/engine/scenario_test.go
package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
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
	// Revert now writes through applyTreeToWorktree's checked worktree
	// updater (internal/store/worktree_apply.go), which -- like
	// ResetWorktreeToHead -- refuses to run outside a checked write session;
	// this is the same BeginWrite/Close scaffolding Restore already wraps its
	// own worktree reset in (restore.go).
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Store.Revert(1); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(s, agents); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(codexDir, "writer")); err != nil {
		t.Fatal("revert + reconcile must restore the codex link")
	}
}

// TestScenarioBrokenLinksAreReportedThenRepaired walks SPEC §10's acceptance
// criterion end to end: break the links by hand, require status to name every
// break, repair with restore, and require status to come back clean of
// everything restore is actually able to fix.
//
// alpha and beta start alike -- both desired, enabled skills whose links are
// deleted by hand -- but beta's deleted link is then replaced with a symlink
// to unrelated content, making beta's path KindForeign (scan.go) rather than
// merely absent. Diff (diff.go:16) classifies that ReportConflict, not
// ReportForeign (diff.go:17): fu.yaml has an opinion on beta -- it wants the
// skill on for this agent -- so this is not the "no opinion at all" case
// ReportForeign is reserved for. SPEC rule 2 forbids fu from ever touching
// content it did not create, so Restore rebuilds alpha's link but must leave
// beta's foreign occupant exactly as found. The ReportConflict that survives
// Restore for beta is therefore the correct outcome of that rule, not a
// repair left undone -- do not "fix" this test by making the conflict go
// away.
//
// Restore is called with hard=false: this walkthrough exercises only the
// link-repair layer scenario 5 covers above, and beta's surviving
// ReportConflict is precisely what must happen when the store worktree itself
// has nothing to do with the break -- see Restore's own doc comment
// (restore.go) for what hard=true does instead.
func TestScenarioBrokenLinksAreReportedThenRepaired(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	for _, name := range []string{"alpha", "beta"} {
		if _, err := NewSkill(s, agents, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "beta")); err != nil {
		t.Fatal(err)
	}
	foreignTarget := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(foreignTarget, filepath.Join(dir, "beta")); err != nil {
		t.Fatal(err)
	}

	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Agents) != 1 || len(report.Agents[0].Drift) < 2 {
		t.Fatalf("status must name both breaks: %+v", report.Agents)
	}

	if _, err := Restore(s, agents, false); err != nil {
		t.Fatal(err)
	}

	// alpha had nothing standing in its way but an absent fu link, so restore
	// must rebuild it pointing back into the store, same as any other
	// CreateLink.
	alphaTarget, err := os.Readlink(filepath.Join(dir, "alpha"))
	if err != nil {
		t.Fatalf("restore must rebuild alpha's deleted link: %v", err)
	}
	if want := filepath.Join(s.SkillsDir(), "alpha"); alphaTarget != want {
		t.Fatalf("alpha link target = %q, want %q", alphaTarget, want)
	}

	// beta's foreign occupant must be untouched -- same symlink, same target.
	// Restore is forbidden from replacing, removing, or repointing content it
	// did not create (SPEC rule 2), no matter how badly fu.yaml wants beta
	// linked.
	betaTarget, err := os.Readlink(filepath.Join(dir, "beta"))
	if err != nil {
		t.Fatalf("restore must leave beta's foreign symlink in place: %v", err)
	}
	if betaTarget != foreignTarget {
		t.Fatalf("beta target = %q, want the untouched foreign target %q", betaTarget, foreignTarget)
	}

	cfg, err = store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	after, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	var sawBetaConflict bool
	for _, agentStatus := range after.Agents {
		for _, action := range agentStatus.Drift {
			// ReportConflict and ReportForeign are exactly the drift fu must
			// never act on (diff.go:16-17): a desired path occupied by
			// foreign content, or a name fu.yaml has no opinion on at all.
			// Anything else surviving restore is drift restore should have
			// repaired and did not -- a genuine bug, not a policy outcome.
			if action.Type != ReportConflict && action.Type != ReportForeign {
				t.Fatalf("restore left drift behind that it should have repaired: %+v", action)
			}
			if action.Type == ReportConflict && action.AgentName == "claude" && action.Skill == "beta" {
				sawBetaConflict = true
			}
		}
	}
	if !sawBetaConflict {
		t.Fatal("beta's foreign occupant must still be reported as a conflict after restore")
	}
}
