package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// uninspectableFuLink plants a fu-shaped link whose target cannot be
// inspected: the store-side target is a (relative, so the store's sweep can
// record it) symlink to itself, so stat through the link fails with ELOOP --
// the case DESIGN's known gap names.
func uninspectableFuLink(t *testing.T, agentDir, storeSkills, name string) {
	t.Helper()
	target := filepath.Join(storeSkills, name)
	if err := os.Symlink(name, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(agentDir, name)); err != nil {
		t.Fatal(err)
	}
}

func entryByName(t *testing.T, st AgentState, name string) Entry {
	t.Helper()
	for _, e := range st.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("entry %q not found in %+v", name, st.Entries)
	return Entry{}
}

// One entry that cannot be inspected no longer aborts the agent: it is
// recorded as unknown, with its error, and the healthy entries are still
// classified.
func TestScanAgentRecordsAnUninspectableEntryAndKeepsScanning(t *testing.T) {
	root := t.TempDir()
	storeSkills := filepath.Join(root, "store", "skills")
	agentDir := filepath.Join(root, "agent")
	for _, dir := range []string{filepath.Join(storeSkills, "beta"), agentDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(storeSkills, "beta"), filepath.Join(agentDir, "beta")); err != nil {
		t.Fatal(err)
	}
	uninspectableFuLink(t, agentDir, storeSkills, "loop")

	st, err := ScanAgent(fakeAgent{"claude", agentDir}, storeSkills)
	if err != nil {
		t.Fatalf("one bad entry must not fail the scan: %v", err)
	}
	loop := entryByName(t, st, "loop")
	if loop.Kind != KindUnknown || loop.Err == nil {
		t.Fatalf("loop = %+v, want KindUnknown with its error", loop)
	}
	beta := entryByName(t, st, "beta")
	if beta.Kind != KindFuLink || beta.Broken {
		t.Fatalf("beta = %+v, want a healthy fu link", beta)
	}
}

// An unknown entry is neither missing, nor foreign, nor a removable fu link:
// whatever fu.yaml says about the name, Diff reports it as failed and plans
// no creation, removal or conflict for it.
func TestDiffReportsAnUninspectableEntryOnlyAsFailed(t *testing.T) {
	boom := errors.New("stat fu-owned symlink: too many levels of symbolic links")
	state := AgentState{Agent: fakeAgent{"claude", "/agent"}, Entries: []Entry{{Name: "loop", Kind: KindUnknown, Err: boom}}}
	for _, tc := range []struct {
		name    string
		desired map[string]bool
	}{
		{"enabled", map[string]bool{"loop": true}},
		{"disabled", map[string]bool{"loop": false}},
		{"unregistered", map[string]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acts := Diff(tc.desired, state, "/store/skills")
			if len(acts) != 1 || acts[0].Type != ReportFailed || acts[0].Skill != "loop" || !errors.Is(acts[0].Err, boom) {
				t.Fatalf("actions = %+v, want one ReportFailed for loop carrying its error", acts)
			}
		})
	}
}

// Within one agent, a bad entry is reported as a failure of that entry
// while the healthy ones are reconciled, and the bad entry itself is never
// touched.
func TestReconcileIsolatesAnUninspectableEntryWithinAnAgent(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	agentDir := t.TempDir()
	uninspectableFuLink(t, agentDir, s.SkillsDir(), "loop")

	res, err := Reconcile(s, []agent.Agent{fakeAgent{"claude", agentDir}})
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("an entry that cannot be inspected is an operation failure, got %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.AgentName != "claude" || res.Failed[0].Action.Skill != "loop" || res.Failed[0].Err == nil {
		t.Fatalf("failed = %+v, want the one entry named with its error", res.Failed)
	}
	if target, err := os.Readlink(filepath.Join(agentDir, "alpha")); err != nil || target != filepath.Join(s.SkillsDir(), "alpha") {
		t.Fatalf("the healthy skill must still be linked: %v %q", err, target)
	}
	if target, err := os.Readlink(filepath.Join(agentDir, "loop")); err != nil || target != filepath.Join(s.SkillsDir(), "loop") {
		t.Fatalf("the bad entry must be left exactly as found: %v %q", err, target)
	}
}

// fu status lists the bad entry as drift of its own kind, with no scan error
// on the agent and no error from the command.
func TestStatusListsAnUninspectableEntry(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	uninspectableFuLink(t, agentDir, s.SkillsDir(), "loop")

	report, err := Status(s, cfg, []agent.Agent{fakeAgent{"claude", agentDir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Agents) != 1 || report.Agents[0].ScanErr != "" {
		t.Fatalf("agents = %+v, want claude without a scan error", report.Agents)
	}
	found := false
	for _, drift := range report.Agents[0].Drift {
		if drift.Skill == "loop" {
			found = true
			if drift.Type != ReportFailed || drift.Err == nil {
				t.Fatalf("loop drift = %+v, want ReportFailed with its error", drift)
			}
		}
	}
	if !found {
		t.Fatalf("drift = %+v, want the uninspectable entry listed", report.Agents[0].Drift)
	}
}

// adopt leaves the bad entry to the closing reconcile's report and still
// adopts the agent's other skills; the agent is not excluded wholesale.
func TestAdoptSkipsAnUninspectableEntryAndAdoptsTheRest(t *testing.T) {
	s, _ := setupStore(t)
	agentDir := t.TempDir()
	writeSkillTree(t, agentDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	uninspectableFuLink(t, agentDir, s.SkillsDir(), "loop")

	res, err := Adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "")
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("the bad entry must surface as an operation failure of the closing reconcile, got %v", err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("adopted = %+v, want pdf-tools despite the bad entry", res.Adopted)
	}
	for _, f := range res.Failed {
		if f.Action.Skill == "loop" {
			t.Fatalf("the bad entry is an I/O failure, not an invalid candidate; adopt must not report it itself: %+v", res.Failed)
		}
	}
	reported := false
	for _, f := range res.Reconcile.Failed {
		if f.Action.AgentName == "claude" && f.Action.Skill == "loop" && f.Err != nil {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("reconcile failed = %+v, want the bad entry named with its agent and error", res.Reconcile.Failed)
	}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, "agent claude: skills scan failed") {
			t.Fatalf("the agent must not be reported as unscannable: %q", w)
		}
	}
	if target, err := os.Readlink(filepath.Join(agentDir, "loop")); err != nil || target != filepath.Join(s.SkillsDir(), "loop") {
		t.Fatalf("the bad entry must be left exactly as found: %v %q", err, target)
	}
}

// When the bad entry is the agent's only entry, adoption finds no candidate
// at all, so nothing runs a later reconcile: the prologue's own pass is the
// one that must still name the entry and make the command exit 1.
func TestAdoptReportsAnUninspectableEntryWhenItIsTheOnlyEntry(t *testing.T) {
	s, _ := setupStore(t)
	agentDir := t.TempDir()
	uninspectableFuLink(t, agentDir, s.SkillsDir(), "loop")

	res, err := Adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "")
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("the sole bad entry must still surface as an operation failure, got %v", err)
	}
	if len(res.Adopted) != 0 || len(res.Pending) != 0 || len(res.Failed) != 0 {
		t.Fatalf("nothing is adoptable here: adopted=%+v pending=%+v failed=%+v", res.Adopted, res.Pending, res.Failed)
	}
	reported := false
	for _, f := range res.Reconcile.Failed {
		if f.Action.AgentName == "claude" && f.Action.Skill == "loop" && f.Err != nil {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("reconcile failed = %+v, want the prologue's report of the bad entry", res.Reconcile.Failed)
	}
}

// An entry that vanished between the listing and its inspection is not an
// entry: it is skipped, as os.ReadDir skips a dirent that vanished under it,
// rather than reported as a failure of the tool.
func TestScanAgentSkipsAnEntryThatVanishedAfterTheListing(t *testing.T) {
	root := t.TempDir()
	storeSkills := filepath.Join(root, "store", "skills")
	agentDir := filepath.Join(root, "agent")
	for _, dir := range []string{filepath.Join(storeSkills, "beta"), agentDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"beta", "gone"} {
		if err := os.Symlink(filepath.Join(storeSkills, name), filepath.Join(agentDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	st, err := scanAgentWithHooks(fakeAgent{"claude", agentDir}, storeSkills, func() {
		if err := os.Remove(filepath.Join(agentDir, "gone")); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range st.Entries {
		if e.Name == "gone" {
			t.Fatalf("a vanished entry must not be recorded, got %+v", e)
		}
	}
	if beta := entryByName(t, st, "beta"); beta.Kind != KindFuLink {
		t.Fatalf("beta = %+v, want a healthy fu link", beta)
	}
}

// A readlink that fails for any reason but absence -- here the directory
// losing its search permission after the listing -- records the entry as
// uninspectable rather than failing the scan.
func TestScanAgentRecordsAnEntryWhoseReadlinkFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	root := t.TempDir()
	storeSkills := filepath.Join(root, "store", "skills")
	agentDir := filepath.Join(root, "agent")
	for _, dir := range []string{storeSkills, agentDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(storeSkills, "alpha"), filepath.Join(agentDir, "alpha")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(agentDir, 0o755) })

	st, err := scanAgentWithHooks(fakeAgent{"claude", agentDir}, storeSkills, func() {
		if err := os.Chmod(agentDir, 0o000); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("one unreadable entry must not fail the scan: %v", err)
	}
	alpha := entryByName(t, st, "alpha")
	if alpha.Kind != KindUnknown || alpha.Err == nil {
		t.Fatalf("alpha = %+v, want KindUnknown with the readlink error", alpha)
	}
}
