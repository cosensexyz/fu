// internal/engine/scenario_closeout_test.go
package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// The three SPEC §3 scenarios this file adds, and why they are here rather
// than counted as covered by the unit tests that touch the same code.
//
// Scenarios 2, 4, 5 and 7 already had walkthroughs (scenario_test.go): each
// drives the entry point a user drives and asserts the outcome SPEC promises,
// not an internal step. Scenarios 1, 3 and 6 were described as "exercised
// functionally by add_test.go, adopt_test.go and adopt_whole_test.go" -- true,
// and not the same thing. Those files assert what a function returns; a
// scenario has to assert what the user was promised, which for these three is
// a claim about *all managed agents*, about *a recorded commit moving*, and
// about *a pre-existing environment being left alone*. None of those is any
// one function's postcondition, which is exactly why copying a unit test up
// here would pad the count without closing the gap.
//
// What each adds beyond its unit coverage is named at the test.

// scenarioEnv is applicationEnv with every v1 agent detectable, because these
// scenarios promise things about the whole managed set rather than about one
// agent. Returns the store and the two agents' skills directories.
func scenarioEnv(t *testing.T) (*Application, *store.Store, map[string]string) {
	t.Helper()
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	// The marker each adapter detects on is its skills directory's parent, and
	// the skills directory itself is the adapter's to name -- asking it rather
	// than rebuilding `~/.<name>/skills` here keeps a third adapter with a
	// different layout from quietly dropping out of Detected() while still
	// appearing in this map.
	dirs := map[string]string{}
	for _, a := range agent.All() {
		if err := os.MkdirAll(filepath.Dir(a.SkillsDir()), 0o755); err != nil {
			t.Fatal(err)
		}
		dirs[a.Name()] = a.SkillsDir()
	}
	app := NewApplication()
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}
	st, err := app.openStore()
	if err != nil {
		t.Fatal(err)
	}
	// Every adapter must actually be detected, or a scenario asserting
	// something about "every managed agent" would be asserting it about fewer
	// than it looks.
	if len(agent.Detected()) != len(dirs) || len(dirs) < 2 {
		t.Fatalf("scenarios about every managed agent need all of them detected; built %v, detected %d",
			dirs, len(agent.Detected()))
	}
	return app, st, dirs
}

// makeGitSourceWithSkills is makeGitSourceNamed for a repository holding more
// than one skill, which is what scenario 1 is about: the scan finds everything
// the repository offers and the user picks from it.
func makeGitSourceWithSkills(t *testing.T, names ...string) string {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		writeSkillTree(t, work, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("seed", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}}); err != nil {
		t.Fatal(err)
	}
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	headRef, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + headRef.Name().Short() + ":refs/heads/" + headRef.Name().Short())},
	}); err != nil {
		t.Fatal(err)
	}
	return "file://" + bare
}

// linkedSkills lists the entries an agent's skills directory offers.
func linkedSkills(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// TestScenarioInstallFromAGitRepository is SPEC §3 scenario 1: `fu add
// <git-url>` scans out every skill the repository holds, the user selects from
// them, and the selected ones are available to every managed agent next
// session.
//
// Beyond add_test.go, which installs one skill and checks it landed: the
// repository here holds three, only one is selected, and the assertion is
// about the whole managed set -- every agent offers the chosen skill and none
// offers the two that were passed over. "All managed agents" and "only what
// was selected" are the scenario's actual promise and neither is any single
// function's postcondition.
func TestScenarioInstallFromAGitRepository(t *testing.T) {
	app, st, dirs := scenarioEnv(t)
	url := makeGitSourceWithSkills(t, "alpha", "beta", "gamma")

	preparation, err := app.PrepareAdd(url, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := preparation.Session.(*AddPlan)
	defer plan.Close()

	offered := []string{}
	for _, candidate := range plan.Candidates() {
		offered = append(offered, candidate.Name)
	}
	sort.Strings(offered)
	if strings.Join(offered, ",") != "alpha,beta,gamma" {
		t.Fatalf("the scan must offer every skill the repository holds, got %v", offered)
	}

	var chosen []Candidate
	for _, candidate := range plan.Candidates() {
		if candidate.Name == "beta" {
			chosen = append(chosen, candidate)
		}
	}
	if _, err := plan.Install(chosen); err != nil {
		t.Fatal(err)
	}

	for name, dir := range dirs {
		got := linkedSkills(t, dir)
		if len(got) != 1 || got[0] != "beta" {
			t.Fatalf("agent %s offers %v; the selected skill must reach every managed agent and the unselected must reach none", name, got)
		}
		resolved, err := os.Stat(filepath.Join(dir, "beta", "SKILL.md"))
		if err != nil || resolved.Size() == 0 {
			t.Fatalf("agent %s's link does not resolve to the skill: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(st.SkillsDir(), "beta")); err != nil {
		t.Fatalf("the selected skill must be in the store: %v", err)
	}
	for _, passed := range []string{"alpha", "gamma"} {
		if _, err := os.Stat(filepath.Join(st.SkillsDir(), passed)); !os.IsNotExist(err) {
			t.Fatalf("%s was not selected and must not be installed: %v", passed, err)
		}
	}
}

// TestScenarioOutdatedThenUpdateRecordsTheNewCommit is SPEC §3 scenario 3:
// `fu outdated` lists what can move, `fu update <name>` upgrades it and
// records the new commit.
//
// Beyond update_test.go, which drives UpdateSkills directly: the entry point
// here is `outdated` -- the command the scenario says a user runs first -- and
// the assertions are that it says nothing before the upstream moves, names the
// skill after it does, and that the commit fu.yaml records afterwards is the
// one the upstream actually advanced to. Content changing and a lock advancing
// are separately tested; that the *reported* work is the work that then
// happens is the scenario.
func TestScenarioOutdatedThenUpdateRecordsTheNewCommit(t *testing.T) {
	app, st, dirs := scenarioEnv(t)
	url := makeGitSourceNamed(t, false, "pdf-tools")
	installedFromGit(t, st, url)

	// app.Outdated, not the free Outdated: that is what newOutdatedCmd binds
	// to, so it is the layer a user's `fu outdated` actually reaches.
	before, err := app.Outdated()
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range before.Rows {
		if status.Updatable {
			t.Fatalf("nothing has moved upstream yet, so nothing may be reported outdated: %+v", status)
		}
	}
	installed := recordedSourceFields(t, st, "pdf-tools")["commit"]

	advanced := advanceGitSource(t, url, func(work string) {
		writeSkillBody(t, filepath.Join(work, "pdf-tools"), "pdf-tools", "version two")
	})
	if advanced == installed {
		t.Fatal("the fixture did not advance the upstream")
	}

	after, err := app.Outdated()
	if err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, status := range after.Rows {
		if status.Name == "pdf-tools" && status.Updatable {
			named = true
		}
	}
	if !named {
		t.Fatalf("`fu outdated` must name the skill whose upstream moved: %+v", after.Rows)
	}

	if _, err := app.UpdateSkills("pdf-tools", false); err != nil {
		t.Fatal(err)
	}
	if got := recordedSourceFields(t, st, "pdf-tools")["commit"]; got != advanced {
		t.Fatalf("recorded commit = %q, want the commit the upstream advanced to %q", got, advanced)
	}
	// And the upgrade is what every managed agent now offers.
	for name, dir := range dirs {
		body, err := os.ReadFile(filepath.Join(dir, "pdf-tools", "SKILL.md"))
		if err != nil {
			t.Fatalf("agent %s: %v", name, err)
		}
		if !strings.Contains(string(body), "version two") {
			t.Fatalf("agent %s still offers the pre-update content", name)
		}
	}
}

// TestScenarioAdoptExistingSkillsKeepsTheEnvironmentAsItWas is SPEC §3
// scenario 6: an environment with loose skills already in it -- real
// directories under one agent, a symlink under another -- is taken into the
// store by `fu adopt`, links are left in place, the switch matrix keeps its
// pre-adopt shape, and the original target of an existing symlink is not
// disturbed.
//
// Beyond adopt_test.go and adopt_whole_test.go, which assert what one adopt
// call did to one entry: this asserts what the environment looks like as a
// whole afterwards. Stated precisely, since the standard this batch set itself
// is not to copy tests upward for the count:
//
//   - The switch clause is *not* new on its own. TestAdoptWritesFalseOverrides
//     ForMissingAgents already pins it for a real directory under one agent,
//     and pins it more tightly -- it asserts the explicit false override, where
//     this asserts only the effective value. What is new is the shape: two
//     forms and two agents in one adopt, with the symlink-sourced direction
//     (enabled for codex, off for claude) that the existing test does not run.
//   - The link assertion is new, and it is what "原地留下链接" actually says:
//     an entry that is a symlink resolving into the store. Reading content
//     through the path passes even against an adopt that touched nothing.
//   - The external-target invariance is new and strictly stronger than
//     TestAdoptSymlinkEntry's check that the target is still a directory: this
//     compares the tree byte for byte. It is the promise a user would
//     otherwise verify by losing work.
func TestScenarioAdoptExistingSkillsKeepsTheEnvironmentAsItWas(t *testing.T) {
	_, st, dirs := scenarioEnv(t)

	claudeDir, codexDir := dirs["claude"], dirs["codex"]
	if claudeDir == "" || codexDir == "" {
		t.Fatalf("this scenario needs both v1 agents: %v", dirs)
	}
	for _, dir := range []string{claudeDir, codexDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// The environment as the user left it: a real directory under claude only,
	// and a symlink under codex only, pointing into a repository of theirs.
	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: d\n---\n")
	external := t.TempDir()
	target := writeSkillTree(t, external, "linked", "---\nname: linked\ndescription: d\n---\n")
	if err := os.Symlink(target, filepath.Join(codexDir, "linked")); err != nil {
		t.Fatal(err)
	}
	externalBefore := treeSnapshot(t, external)

	if _, err := Adopt(st, agent.Detected(), ""); err != nil {
		t.Fatal(err)
	}

	// Taken into the store.
	for _, name := range []string{"notes", "linked"} {
		if _, err := os.Stat(filepath.Join(st.SkillsDir(), name, "SKILL.md")); err != nil {
			t.Fatalf("%s was not taken into the store: %v", name, err)
		}
	}

	// The switch matrix keeps its pre-adopt shape: each skill stays on for the
	// agent that had it and off for the one that did not. Anything newly added
	// would be on for both, so this is the clause that distinguishes adopt.
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		skill, on, off string
	}{
		{"notes", "claude", "codex"},
		{"linked", "codex", "claude"},
	} {
		if !cfg.Effective(want.skill, want.on) {
			t.Fatalf("%s was present for %s before adopt and must stay enabled there", want.skill, want.on)
		}
		if cfg.Effective(want.skill, want.off) {
			t.Fatalf("%s was absent for %s before adopt and must not be switched on by it", want.skill, want.off)
		}
	}

	// A link is left in place, and it points into the store. Both halves
	// matter and only together: reading through the agent path succeeds
	// identically whether adopt relinked the entry or never touched the agent
	// directory at all, so the content check alone passes against an adopt
	// that did nothing. That is not hypothetical -- neutering the switch call
	// leaves this scenario green if the link is not asserted.
	for dir, name := range map[string]string{claudeDir: "notes", codexDir: "linked"} {
		entry := filepath.Join(dir, name)
		info, err := os.Lstat(entry)
		if err != nil {
			t.Fatalf("%s is gone from %s: %v", name, dir, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s must be left as a link after adopt, got mode %v", entry, info.Mode())
		}
		resolved, err := filepath.EvalSymlinks(entry)
		if err != nil {
			t.Fatalf("%s does not resolve: %v", entry, err)
		}
		wantPrefix, err := filepath.EvalSymlinks(st.SkillsDir())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(resolved, wantPrefix) {
			t.Fatalf("%s resolves to %s, which is outside the store at %s: adopt must leave a link into what it took in",
				entry, resolved, wantPrefix)
		}
		// And the experience is unchanged: the same content, now through the
		// store rather than from wherever it used to live.
		body, err := os.ReadFile(filepath.Join(entry, "SKILL.md"))
		if err != nil {
			t.Fatalf("%s no longer resolves under %s: %v", name, dir, err)
		}
		if !strings.Contains(string(body), "name: "+name) {
			t.Fatalf("%s resolves to unexpected content: %q", name, body)
		}
	}

	// And the repository the symlink pointed at is untouched.
	if after := treeSnapshot(t, external); after != externalBefore {
		t.Fatalf("adopt modified the external target it only had permission to read:\n before %s\n after  %s",
			externalBefore, after)
	}
}

// treeSnapshot renders a directory tree as a comparable string: every path,
// its type, and the bytes of every regular file.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			dest, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			lines = append(lines, rel+" -> "+dest)
		case info.IsDir():
			lines = append(lines, rel+"/")
		default:
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			lines = append(lines, rel+" "+string(body))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
