package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

func TestAdoptRealDirSingleAgent(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	writeSkillTree(t, dir, "linter", "---\nname: linter\ndescription: d\n---\n")
	// A non-skill entry must never be touched.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 2 {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pdf-tools", "linter"} {
		if !cfg.HasSkill(name) || !cfg.Enabled(name) {
			t.Fatalf("%s not registered or not enabled", name)
		}
		// The entry became a store link.
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s entry is not a link: %v %v; adopt result: %+v", name, info, err, res)
		}
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || !strings.HasPrefix(target, s.SkillsDir()) {
			t.Fatalf("%s link target %q not into the store", name, target)
		}
		// The original content is preserved in recovery.
		archives, err := os.ReadDir(s.RecoveryDir())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range archives {
			if strings.HasPrefix(a.Name(), "adopt-archive-claude-"+name) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no recovery archive for %s", name)
		}
	}
	// The non-skill entry is untouched.
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("non-skill entry touched: %v", err)
	}
}

// TestAdoptSkipsNonSkillDirectory pins the per-entry scan's skill filter
// (finding I3): a directory that is not a valid skill (no SKILL.md) must be
// skipped, not abort the whole run at install time. Before the filter it
// passed phase 1 and failed inside adoptOne's ValidateSkillDir, returning an
// error that stopped every other candidate.
func TestAdoptSkipsNonSkillDirectory(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	if err := os.MkdirAll(filepath.Join(dir, "junkdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junkdir", "README.md"), []byte("not a skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatalf("a non-skill directory must not abort the run: %v", err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	// The non-skill directory is untouched, never archived.
	info, err := os.Lstat(filepath.Join(dir, "junkdir"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("junkdir must stay a real directory: %v %v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "junkdir", "README.md")); err != nil || string(got) != "not a skill" {
		t.Fatalf("junkdir content = %q, err %v", got, err)
	}
}

// TestAdoptWarnsOnDisagreeingSymlinkTargets pins finding I5: entries whose
// symlink targets disagree across agents are still adopted, but the missing
// local source record is reported in AdoptResult.Warnings.
func TestAdoptWarnsOnDisagreeingSymlinkTargets(t *testing.T) {
	s, _ := setupStore(t)
	targetA, targetB := t.TempDir(), t.TempDir()
	// Identical content in both targets so the entries merge by digest; only
	// the symlink targets differ.
	writeSkillTree(t, targetA, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	writeSkillTree(t, targetB, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := os.Symlink(filepath.Join(targetA, "pdf-tools"), filepath.Join(dirA, "pdf-tools")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(targetB, "pdf-tools"), filepath.Join(dirB, "pdf-tools")); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", dirA}, fakeAgent{"codex", dirB}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || len(res.Adopted[0].Agents) != 2 {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "symlink targets differ") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("disagreeing symlink targets must be warned about, warnings = %v", res.Warnings)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SourceFields("pdf-tools")) != 0 {
		t.Fatalf("no local source may be recorded for disagreeing targets, got %v", cfg.SourceFields("pdf-tools"))
	}
	records := readAdoptLinkArchives(t, s)
	if len(records) != 2 {
		t.Fatalf("each removed symlink must have a durable archive record, got %+v", records)
	}
	wantTargets := map[string]bool{
		filepath.Join(targetA, "pdf-tools"): true,
		filepath.Join(targetB, "pdf-tools"): true,
	}
	for _, record := range records {
		if !wantTargets[record.RawTarget] || record.Skill != "pdf-tools" || record.OriginalPath == "" {
			t.Fatalf("unexpected symlink archive record: %+v", record)
		}
		delete(wantTargets, record.RawTarget)
	}
	if len(wantTargets) != 0 {
		t.Fatalf("missing archived symlink targets: %v", wantTargets)
	}
	if _, err := PruneCompletedTransactions(s); err != nil {
		t.Fatal(err)
	}
	if after := readAdoptLinkArchives(t, s); len(after) != 2 {
		t.Fatalf("gc removed durable symlink archives: %+v", after)
	}
}

func readAdoptLinkArchives(t *testing.T, s *store.Store) []adoptLinkArchiveRecord {
	t.Helper()
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var records []adoptLinkArchiveRecord
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), adoptLinkArchivePrefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.RecoveryDir(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var record adoptLinkArchiveRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("decode %s: %v", entry.Name(), err)
		}
		records = append(records, record)
	}
	return records
}

// TestAdoptWarnsOnBrokenAgentScan pins finding M4 at the scan phase: a
// per-agent scan failure is reported in the warnings instead of silently
// skipping the agent. (The trailing reconcile still refuses a genuinely
// broken agent with ErrOperationFailed -- by design -- so the scan-level
// warning is asserted on scanAdoptEntries directly.)
func TestAdoptWarnsOnBrokenAgentScan(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	// An agent whose home directory is not resolvable: ScanAgent refuses it,
	// which must not stop the healthy agent's candidates.
	agents := []agent.Agent{fakeAgent{"claude", dir}, fakeAgent{"broken", ""}}

	scan, err := scanAdoptEntries(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	entries, warnings := scan.entries, scan.warnings
	if len(entries) != 1 || entries[0].name != "pdf-tools" {
		t.Fatalf("entries = %+v", entries)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "agent broken: skills scan failed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("broken agent scan must be warned about, warnings = %v", warnings)
	}
}

func TestScanAdoptEntriesRetainsAgentStateForSwitchClassification(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	a := fakeAgent{"claude", dir}

	scan, err := scanAdoptEntries(s, []agent.Agent{a})
	if err != nil {
		t.Fatal(err)
	}
	state, ok := scan.states[a.Name()]
	if !ok || state.Agent.Name() != a.Name() || state.ParentIsSymlink {
		t.Fatalf("retained state = %+v, present %v", state, ok)
	}
}

func TestAdoptScansEachAgentInventoryOnceForMultipleSkills(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	writeSkillTree(t, dir, "beta", "---\nname: beta\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	scans := 0
	h := hooks{scanAdoptAgent: func(a agent.Agent, storeSkillsDir string) (AgentState, error) {
		scans++
		return ScanAgent(a, storeSkillsDir)
	}}

	res, err := adopt(s, agents, "", h)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 2 {
		t.Fatalf("adopted = %+v, want both skills", res.Adopted)
	}
	if scans != 1 {
		t.Fatalf("agent inventory scans = %d, want one scan for the whole batch", scans)
	}
}

func TestAdoptMergesIdenticalAcrossAgents(t *testing.T) {
	s, _ := setupStore(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	writeSkillTree(t, dir2, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || len(res.Adopted[0].Agents) != 2 {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	for _, dir := range []string{dir1, dir2} {
		if _, err := os.Lstat(filepath.Join(dir, "pdf-tools")); err != nil {
			t.Fatalf("switch missing in %s: %v", dir, err)
		}
	}
}

func TestAdoptConflictAcrossAgents(t *testing.T) {
	s, _ := setupStore(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: one\n---\n")
	writeSkillTree(t, dir2, "pdf-tools", "---\nname: pdf-tools\ndescription: two\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "pdf-tools" {
		t.Fatalf("conflicts = %+v", res.Conflicts)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("pdf-tools") {
		t.Fatal("conflicted skill must not be registered")
	}
	// Both originals untouched.
	for _, dir := range []string{dir1, dir2} {
		info, err := os.Lstat(filepath.Join(dir, "pdf-tools"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("conflicted entry must stay untouched: %v %v", info, err)
		}
	}
}

// TestAdoptSwitchIsolationSurfacesReconcile pins findings I1/M4 of round 6:
// when one agent's switch is isolated (its original changed between
// inventory and install, so the archive refuses it), the reconcile findings
// must reach the caller (AdoptResult.Reconcile) and the summary must not
// name the agent whose switch failed.
func TestAdoptSwitchIsolationSurfacesReconcile(t *testing.T) {
	s, _ := setupStore(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	writeSkillTree(t, dir2, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}
	// After the first agent's switch, mutate the second agent's original so
	// its archive refuses (digest mismatch) and the switch is isolated.
	h := hooks{afterAdoptSwitch: func() error {
		body := "---\nname: pdf-tools\ndescription: changed\n---\n\nbody\n"
		return os.WriteFile(filepath.Join(dir2, "pdf-tools", "SKILL.md"), []byte(body), 0o644)
	}}

	res, err := adopt(s, agents, "", h)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || len(res.Adopted[0].Agents) != 1 || res.Adopted[0].Agents[0] != "claude" {
		t.Fatalf("summary must name only the successfully switched agent, got %+v", res.Adopted)
	}
	found := false
	for _, c := range res.Reconcile.Conflicts {
		if c.AgentName == "codex" && c.Skill == "pdf-tools" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reconcile must report the isolated agent's foreign entry, conflicts = %+v", res.Reconcile.Conflicts)
	}
}

// TestAdoptRefusesSwitchWhenParentFormUndeterminable pins round 6 finding
// I2: when ScanAgent cannot classify an agent's skills directory, the
// per-entry switch must be refused rather than defaulted to. The broken
// fixture uses a directory with write+exec but no read permission (0o300):
// Lstat -- the hasEntry check -- succeeds while ScanAgent's ReadDir fails.
// (A per-entry switch would still archive the entry -- os.RemoveAll needs
// the same read permission and fails, leaving the original -- so the
// guard's observable effect is that no archive is created at all.) A
// healthy second agent holding the identical skill goes through the normal
// adopt. (The trailing reconcile reports the unreadable agent as failed by
// design, so the command returns ErrOperationFailed; the guard's effect is
// the untouched entry with no recovery archive.)
func TestAdoptRefusesSwitchWhenParentFormUndeterminable(t *testing.T) {
	s, _ := setupStore(t)
	healthyDir := t.TempDir()
	writeSkillTree(t, healthyDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	brokenDir := t.TempDir()
	writeSkillTree(t, brokenDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	if err := os.Chmod(brokenDir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(brokenDir, 0o755) })
	agents := []agent.Agent{fakeAgent{"claude", healthyDir}, fakeAgent{"broken", brokenDir}}

	res, err := Adopt(s, agents, "")
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("reconcile must report the unreadable agent, got %v", err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("adopted = %+v, want durable pdf-tools despite reconcile failure", res.Adopted)
	}
	// The healthy agent's entry was switched.
	info, lerr := os.Lstat(filepath.Join(healthyDir, "pdf-tools"))
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("healthy agent must be switched: %v %v", info, lerr)
	}
	// The guard's effect: the broken agent's entry was never archived and
	// never touched.
	info, lerr = os.Lstat(filepath.Join(brokenDir, "pdf-tools"))
	if lerr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("entry must stay untouched when the parent form is undeterminable: %v %v", info, lerr)
	}
	archives, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range archives {
		if strings.HasPrefix(a.Name(), "adopt-archive-broken-") {
			t.Fatalf("no archive may be created for the unclassifiable agent, found %s", a.Name())
		}
	}
	// The failure is reported once, by the scan phase (round 9 finding M2
	// deduped the per-candidate repeat); the guard's own effect is the
	// untouched entry with no archive.
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "agent broken: skills scan failed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("must warn about the broken agent, warnings = %v", res.Warnings)
	}
}

func TestAdoptContinuesAfterReconcileFailure(t *testing.T) {
	s, _ := setupStore(t)
	holdingDir := t.TempDir()
	writeSkillTree(t, holdingDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	writeSkillTree(t, holdingDir, "beta", "---\nname: beta\ndescription: d\n---\n")
	unusableDir := t.TempDir()
	agents := []agent.Agent{
		fakeAgent{"claude", holdingDir},
		fakeAgent{"broken", unusableDir},
	}
	becameUnusable := false
	h := hooks{afterAdoptSwitch: func() error {
		if becameUnusable {
			return nil
		}
		becameUnusable = true
		return os.Chmod(unusableDir, 0o300)
	}}
	t.Cleanup(func() { _ = os.Chmod(unusableDir, 0o755) })

	res, err := adopt(s, agents, "", h)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("adopt error = %v, want accumulated reconcile failure", err)
	}
	if !becameUnusable {
		t.Fatal("test hook did not make the second agent unusable")
	}
	if len(res.Adopted) != 2 || res.Adopted[0].Name != "alpha" || res.Adopted[1].Name != "beta" {
		t.Fatalf("a reconcile failure after alpha must not drop beta: %+v", res.Adopted)
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, statErr := os.Stat(filepath.Join(s.SkillsDir(), name, "SKILL.md")); statErr != nil {
			t.Fatalf("%s was not installed: %v", name, statErr)
		}
	}
}

func TestAdoptScopeRejectsAllPlusAgent(t *testing.T) {
	_, err := AdoptScoped(nil, nil, AdoptScope{All: true, Agent: "claude"})
	if err == nil || !strings.Contains(err.Error(), "cannot select all agents and one named agent") {
		t.Fatalf("All plus Agent must be refused, got %v", err)
	}
}

// TestAdoptSkippedDedupesAcrossAgents pins round 6 finding M3: a managed
// name held by several agents is reported once, not once per holder.
func TestAdoptSkippedDedupesAcrossAgents(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	writeSkillTree(t, dir2, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "alpha" {
		t.Fatalf("skipped = %+v, want exactly one alpha", res.Skipped)
	}
}

func TestAdoptLocalSourceClassificationIsAgentOrderIndependent(t *testing.T) {
	run := func(t *testing.T, wholeFirst bool) (map[string]string, []string) {
		s, _ := setupStore(t)
		wholeParent := t.TempDir()
		wholeTarget := t.TempDir()
		writeSkillTree(t, wholeTarget, "alpha", "---\nname: alpha\ndescription: d\n---\n")
		wholeSkills := filepath.Join(wholeParent, "skills")
		if err := os.Symlink(wholeTarget, wholeSkills); err != nil {
			t.Fatal(err)
		}
		realSkills := t.TempDir()
		writeSkillTree(t, realSkills, "alpha", "---\nname: alpha\ndescription: d\n---\n")
		whole := fakeAgent{"whole", wholeSkills}
		real := fakeAgent{"real", realSkills}
		agents := []agent.Agent{whole, real}
		if !wholeFirst {
			agents = []agent.Agent{real, whole}
		}
		res, err := Adopt(s, agents, "")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := store.LoadConfig(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		return cfg.SourceFields("alpha"), res.Warnings
	}
	fieldsA, warningsA := run(t, true)
	fieldsB, warningsB := run(t, false)
	if fmt.Sprint(fieldsA) != fmt.Sprint(fieldsB) || fmt.Sprint(warningsA) != fmt.Sprint(warningsB) {
		t.Fatalf("agent order changed source classification:\nwhole first: fields=%v warnings=%v\nreal first: fields=%v warnings=%v", fieldsA, warningsA, fieldsB, warningsB)
	}
}

// TestAdoptConflictSkipsEntireName pins that a conflicted name is skipped
// for the whole run even when a later agent matches the first digest
// (digests d1/d2/d1): DESIGN §6 says "不同则报冲突、该项整体跳过", and the
// "left untouched" conflict report must be true. Before the fix the third
// agent re-created the merge entry and the name appeared in both Conflicts
// and Adopted.
func TestAdoptConflictSkipsEntireName(t *testing.T) {
	s, _ := setupStore(t)
	dir1, dir2, dir3 := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: one\n---\n")
	writeSkillTree(t, dir2, "pdf-tools", "---\nname: pdf-tools\ndescription: two\n---\n")
	writeSkillTree(t, dir3, "pdf-tools", "---\nname: pdf-tools\ndescription: one\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}, fakeAgent{"gemini", dir3}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "pdf-tools" {
		t.Fatalf("conflicts = %+v", res.Conflicts)
	}
	for _, adopted := range res.Adopted {
		if adopted.Name == "pdf-tools" {
			t.Fatalf("conflicted name must not be adopted, adopted = %+v", res.Adopted)
		}
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("pdf-tools") {
		t.Fatal("conflicted skill must not be registered")
	}
	// All three originals untouched: the conflict line's promise holds.
	for _, dir := range []string{dir1, dir2, dir3} {
		info, err := os.Lstat(filepath.Join(dir, "pdf-tools"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("conflicted entry must stay untouched: %v %v", info, err)
		}
	}
}

func TestAdoptSkipsAlreadyManaged(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir := t.TempDir()
	writeSkillTree(t, dir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	writeSkillTree(t, dir, "beta", "---\nname: beta\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "alpha" {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "beta" {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	// alpha's entry stays untouched (it is not a fu link yet, and adopt must
	// not touch a managed name).
	info, err := os.Lstat(filepath.Join(dir, "alpha"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("managed name must stay untouched: %v %v", info, err)
	}
	// The unadopted copy is reported with the way out (round 8 finding I1):
	// a managed name some agent still holds as an unmanaged copy can never
	// be adopted again otherwise.
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "alpha") && strings.Contains(w, "claude") && strings.Contains(w, "unadopted copy") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("must warn about the unadopted copy with the holding agent, warnings = %v", res.Warnings)
	}
}

func TestAdoptWritesFalseOverridesForMissingAgents(t *testing.T) {
	s, _ := setupStore(t)
	dir1 := t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	// codex is detected but holds no skill.
	dir2 := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// claude had it: follows global (on). codex did not: explicit off.
	if !cfg.Effective("pdf-tools", "claude") {
		t.Fatal("claude must be on (it held the skill)")
	}
	if cfg.Effective("pdf-tools", "codex") {
		t.Fatal("codex must be off (it did not hold the skill)")
	}
	if v, ok := cfg.Override("pdf-tools", "codex"); !ok || v {
		t.Fatal("codex needs an explicit false override")
	}
}

func TestAdoptScopeAgentOnly(t *testing.T) {
	s, _ := setupStore(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	writeSkillTree(t, dir2, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}

	res, err := Adopt(s, agents, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || len(res.Adopted[0].Agents) != 1 || res.Adopted[0].Agents[0] != "claude" {
		t.Fatalf("scoped adopt must only switch claude: %+v", res.Adopted)
	}
	// codex's original is untouched.
	info, err := os.Lstat(filepath.Join(dir2, "pdf-tools"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("out-of-scope agent must stay untouched: %v %v", info, err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// SPEC rules 6 and 9 state the adopt exception as 初始开关保持收编前现状
	// (原本在哪些 agent 生效即保持哪些), and DESIGN §6 spells out that a
	// scoped --agent still read-only inventories the other detected agents.
	// codex holds its own copy of this skill, so it was already in effect
	// there before the adopt: writing a false override made `fu list` report
	// codex off while codex's directory kept loading the skill every session
	// (round 18 finding I10). The out-of-scope agent is left alone; the
	// trailing reconcile reports its unmanaged copy as a conflict rather than
	// delivering over it.
	if _, ok := cfg.Override("pdf-tools", "codex"); ok {
		t.Fatal("an out-of-scope agent that already holds the skill must not be overridden off")
	}
	if !cfg.Effective("pdf-tools", "codex") {
		t.Fatal("codex held the skill before the adopt, so it must stay in effect")
	}
}

// TestAdoptScopeOverridesOutOfScopeAgentWithoutTheSkill is the other half of
// the same rule: an out-of-scope detected agent that does *not* hold the skill
// still gets an explicit false override, so the trailing reconcile never
// delivers a skill to an agent that never had it.
func TestAdoptScopeOverridesOutOfScopeAgentWithoutTheSkill(t *testing.T) {
	s, _ := setupStore(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}

	if _, err := Adopt(s, agents, "claude"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cfg.Override("pdf-tools", "codex"); !ok || v {
		t.Fatal("an out-of-scope agent without the skill needs an explicit false override")
	}
}

func TestAdoptScopeRejectsExplicitEmptyAgent(t *testing.T) {
	s, _ := setupStore(t)
	_, err := AdoptScoped(s, nil, AdoptScope{})
	if err == nil || !strings.Contains(err.Error(), "agent scope cannot be empty") {
		t.Fatalf("explicit empty scope must be rejected, got %v", err)
	}
}

func TestAdoptScopeRejectsUnknownAgent(t *testing.T) {
	s, _ := setupStore(t)
	_, err := AdoptScoped(s, []agent.Agent{fakeAgent{"claude", t.TempDir()}}, AdoptScope{Agent: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown scope must be rejected, got %v", err)
	}
}

func TestAdoptSymlinkEntry(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	targetDir := t.TempDir()
	writeSkillTree(t, targetDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	// The agent skills dir holds a symlink into the target directory.
	realTarget := filepath.Join(targetDir, "pdf-tools")
	if err := os.Symlink(realTarget, filepath.Join(dir, "pdf-tools")); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	// The original target content is untouched (read-only adoption).
	info, err := os.Lstat(realTarget)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("symlink target must be untouched: %v %v", info, err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// A unique symlink target is recorded as a local source.
	src := cfg.SourceFields("pdf-tools")
	resolvedTarget, err := filepath.EvalSymlinks(realTarget)
	if err != nil {
		t.Fatal(err)
	}
	if src["type"] != "local" || src["path"] != resolvedTarget {
		t.Fatalf("source fields = %v", src)
	}
	// The agent's entry is now a store link.
	if _, err := os.Lstat(filepath.Join(dir, "pdf-tools")); err != nil {
		t.Fatalf("switch missing: %v", err)
	}
}

// TestAdoptRecoversAfterProcessInterruption crashes an adopt at each
// durable boundary: uncommitted stages roll back like an install, the
// committed stage and a mid-switching crash resume the agent-side
// switching. The agents are the real adapters resolved through HOME, because
// recovery maps transaction agent names through agent.ByName -- the same
// global registry production uses.
func TestAdoptRecoversAfterProcessInterruption(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_ADOPT_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_ADOPT_HOME")
		stage := os.Getenv("FU_TEST_CRASH_ADOPT_STAGE")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		var h hooks
		switch stage {
		case "after-txn-start":
			// Crash between the pipeline's "started" WAL and adopt's Mutate:
			// nothing has been created.
			h.afterTxnStart = crash
		case "after-staging-create":
			// Crash after the empty staging root was created but before the
			// declared revision: payload still nil, stage still "started".
			h.afterStagingCreate = crash
		case "after-declared":
			h.afterDeclaredTxn = crash
		case "after-copy":
			h.afterCopy = crash
		case "after-save":
			h.afterSave = crash
		case "after-publish":
			h.afterPublish = crash
		case "after-commit":
			h.afterCommit = crash
		case "after-first-switch":
			h.afterAdoptSwitch = crash
		case "after-second-retire":
			retired := 0
			h.afterAdoptRetire = func() error {
				retired++
				if retired == 2 {
					return crash()
				}
				return nil
			}
		default:
			panic("unknown crash stage " + stage)
		}
		agents := []agent.Agent{agent.Claude{}, agent.Codex{}}
		_, _ = adopt(s, agents, "", h)
		panic("crash hook did not run")
	}

	for _, stage := range []string{"after-txn-start", "after-staging-create", "after-declared", "after-copy", "after-save", "after-publish", "after-commit", "after-first-switch", "after-second-retire"} {
		t.Run(stage, func(t *testing.T) {
			// A HOME holding two detected agents, each with an identical
			// skill entry; the store sits at FU_HOME.
			fuHome := t.TempDir()
			t.Setenv("FU_HOME", fuHome)
			if _, err := store.Init(fuHome); err != nil {
				t.Fatal(err)
			}
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)
			if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeSkillTree(t, filepath.Join(homeDir, ".claude", "skills"), "alpha", "---\nname: alpha\ndescription: d\n---\n")
			writeSkillTree(t, filepath.Join(homeDir, ".codex", "skills"), "alpha", "---\nname: alpha\ndescription: d\n---\n")
			cmd := exec.Command(os.Args[0], "-test.run=^TestAdoptRecoversAfterProcessInterruption$")
			cmd.Env = append(os.Environ(),
				"FU_TEST_CRASH_ADOPT_HELPER=1",
				"FU_TEST_CRASH_ADOPT_HOME="+fuHome,
				"FU_HOME="+fuHome,
				"HOME="+homeDir,
				"FU_TEST_CRASH_ADOPT_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child must terminate at %s with code 86, err=%v output=%s", stage, err, output)
			}

			s, err := store.Open(fuHome)
			if err != nil {
				t.Fatal(err)
			}
			agents := []agent.Agent{agent.Claude{}, agent.Codex{}}
			switch stage {
			case "after-commit", "after-first-switch", "after-second-retire":
				// The adopt commit is already durable and inventory will find no
				// candidate. Adopt itself must still run the write-command
				// recovery prologue and finish the recorded agent switches.
				if _, err := Adopt(s, agents, ""); err != nil {
					t.Fatalf("adopt retry after %s must recover: %v", stage, err)
				}
			case "after-save", "after-publish":
				// The prologue rolls the uncommitted config/store state back
				// before inventory, so the same Adopt call can see and re-adopt
				// the untouched originals.
				if _, err := Adopt(s, agents, ""); err != nil {
					t.Fatalf("adopt retry after %s must recover: %v", stage, err)
				}
			default:
				// Uncommitted stages: the retry's own pipeline runs the
				// interrupted transaction's rollback, then re-adopts.
				if _, err := Adopt(s, agents, ""); err != nil {
					t.Fatalf("retry after %s must recover: %v", stage, err)
				}
			}
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("successful recovery must clear its WAL, got %+v", pending)
			}
			if !cfg.HasSkill("alpha") {
				t.Fatalf("adopt must complete after %s", stage)
			}
			// After an uncommitted crash the retry re-adopts; after a
			// committed one the entries are already links and the retry
			// skips them. Either way the end state is links on both agents.
			for _, agentName := range []string{".claude", ".codex"} {
				entry := filepath.Join(homeDir, agentName, "skills", "alpha")
				info, err := os.Lstat(entry)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("agent %s: alpha must be a store link after %s: %v %v", agentName, stage, info, err)
				}
			}
			entries, err := s.Log(10)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Message == "external: manual modifications" {
					t.Fatalf("interrupted Adopt state must not be swept as external work: %+v", entries)
				}
			}
		})
	}
}

// testAgentDir is a test-only agent adapter whose skills directory lives
// under a fixed HOME; Detect follows the directory's existence so an
// unregistered test HOME never surfaces it through agent.Detected.
type testAgentDir struct{ name, home string }

func (t testAgentDir) Name() string { return t.name }
func (t testAgentDir) Detect() bool {
	_, err := os.Stat(filepath.Join(t.home, "."+t.name, "skills"))
	return err == nil
}
func (t testAgentDir) SkillsDir() string {
	return filepath.Join(t.home, "."+t.name, "skills")
}
func (t testAgentDir) Reserved() []string { return nil }

// TestAdoptOverridesRecoversAfterProcessInterruption crashes a committed
// adopt whose transaction writes three false overrides (one holder, three
// non-holding detected agents) and asserts the next write recovers: the
// reconstructed expected config must match the committed one byte for byte
// even though the transaction record JSON-round-tripped the override map
// (review finding C1). Before the canonical-ordering fix this stays pending
// with "committed adopt transaction has unexpected config" forever.
func TestAdoptOverridesRecoversAfterProcessInterruption(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_ADOPT_OVERRIDES_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_ADOPT_OVERRIDES_HOME")
		homeDir := os.Getenv("HOME")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		h := hooks{afterCommit: crash}
		agents := []agent.Agent{
			agent.Claude{}, agent.Codex{},
			testAgentDir{"gemini", homeDir}, testAgentDir{"peridot", homeDir},
		}
		_, _ = adopt(s, agents, "", h)
		panic("crash hook did not run")
	}

	fuHome := t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	for _, dir := range []string{".claude", ".codex", ".gemini", ".peridot"} {
		if err := os.MkdirAll(filepath.Join(homeDir, dir, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeSkillTree(t, filepath.Join(homeDir, ".claude", "skills"), "alpha", "---\nname: alpha\ndescription: d\n---\n")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdoptOverridesRecoversAfterProcessInterruption$")
	cmd.Env = append([]string{}, os.Environ()...)
	cmd.Env = append(cmd.Env,
		"FU_TEST_CRASH_ADOPT_OVERRIDES_HELPER=1",
		"FU_TEST_CRASH_ADOPT_OVERRIDES_HOME="+fuHome,
		"FU_HOME="+fuHome,
		"HOME="+homeDir,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate with code 86, err=%v output=%s", err, output)
	}

	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{
		agent.Claude{}, agent.Codex{},
		testAgentDir{"gemini", homeDir}, testAgentDir{"peridot", homeDir},
	}
	if _, err := NewSkill(s, agents, "beta"); err != nil {
		t.Fatalf("next write after committed multi-override adopt must recover: %v", err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") {
		t.Fatal("adopt must have committed alpha")
	}
	if cfg.Effective("alpha", "codex") || cfg.Effective("alpha", "gemini") || cfg.Effective("alpha", "peridot") {
		t.Fatal("non-holding agents must stay off")
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("recovery must clear its WAL, got %+v", pending)
	}
}

// TestAdoptRecoveryRefusesSwitchThroughNewSymlink pins round 7 finding I1:
// the recovery per-entry pass must re-verify the parent's form at execution
// time. A skills directory that was real at scan time and became a symlink
// before the next write must not be switched through. The target entry must
// survive and the now-unactionable agent is isolated so recovery terminates.
func TestAdoptRecoveryRefusesSwitchThroughNewSymlink(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_ADOPT_SWAP_HELPER") == "1" {
		fuHome := os.Getenv("FU_TEST_CRASH_ADOPT_SWAP_HOME")
		s, err := store.Open(fuHome)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		h := hooks{afterCommit: crash}
		agents := []agent.Agent{agent.Claude{}}
		_, _ = adopt(s, agents, "", h)
		panic("crash hook did not run")
	}

	fuHome := t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, filepath.Join(homeDir, ".claude", "skills"), "alpha", "---\nname: alpha\ndescription: d\n---\n")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdoptRecoveryRefusesSwitchThroughNewSymlink$")
	cmd.Env = append([]string{}, os.Environ()...)
	cmd.Env = append(cmd.Env,
		"FU_TEST_CRASH_ADOPT_SWAP_HELPER=1",
		"FU_TEST_CRASH_ADOPT_SWAP_HOME="+fuHome,
		"FU_HOME="+fuHome,
		"HOME="+homeDir,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate with code 86, err=%v output=%s", err, output)
	}

	// The skills dir was real at scan time; before the next write it becomes
	// a symlink to a target holding a copy of the entry.
	target := t.TempDir()
	writeSkillTree(t, target, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	if err := os.RemoveAll(filepath.Join(homeDir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(homeDir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, []agent.Agent{agent.Claude{}}, "beta"); err != nil {
		t.Fatalf("a replaced skills parent must be isolated during recovery: %v", err)
	}
	// The target's original survives: the recovery must not switch through
	// the new symlink.
	if _, err := os.Stat(filepath.Join(target, "alpha", "SKILL.md")); err != nil {
		t.Fatalf("target entry must survive recovery: %v", err)
	}
	// No recovery archive may have been created for the refused switch.
	archives, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range archives {
		if strings.HasPrefix(a.Name(), "adopt-archive-claude-alpha") {
			t.Fatalf("no archive may be created for a refused switch, found %s", a.Name())
		}
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("the isolated recovery must reach a terminal state, got %+v", pending)
	}
}

// TestAdoptRecoveryRejectsDifferentHome proves that an adopt transaction is
// bound to the absolute agent directory inventoried before the commit. Agent
// names alone are not recovery authority: resolving "claude" under a later
// HOME could archive and replace an unrelated installation with identical
// content.
func TestAdoptRecoveryRejectsDifferentHome(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_ADOPT_HOME_BINDING_HELPER") == "1" {
		fuHome := os.Getenv("FU_TEST_CRASH_ADOPT_HOME_BINDING_FU_HOME")
		s, err := store.Open(fuHome)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		_, _ = adopt(s, []agent.Agent{agent.Claude{}}, "", hooks{afterCommit: crash})
		panic("crash hook did not run")
	}

	fuHome := t.TempDir()
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	originalHome := t.TempDir()
	originalSkills := filepath.Join(originalHome, ".claude", "skills")
	if err := os.MkdirAll(originalSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, originalSkills, "alpha", "---\nname: alpha\ndescription: d\n---\n")

	cmd := exec.Command(os.Args[0], "-test.run=^TestAdoptRecoveryRejectsDifferentHome$")
	cmd.Env = append([]string{}, os.Environ()...)
	cmd.Env = append(cmd.Env,
		"FU_TEST_CRASH_ADOPT_HOME_BINDING_HELPER=1",
		"FU_TEST_CRASH_ADOPT_HOME_BINDING_FU_HOME="+fuHome,
		"FU_HOME="+fuHome,
		"HOME="+originalHome,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate with code 86, err=%v output=%s", err, output)
	}

	replacementHome := t.TempDir()
	replacementSkills := filepath.Join(replacementHome, ".claude", "skills")
	if err := os.MkdirAll(replacementSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, replacementSkills, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", replacementHome)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, []agent.Agent{agent.Claude{}}, "beta"); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("recovery under a different HOME must stop with ErrTxnConflict, got %v", err)
	}
	for _, entry := range []string{
		filepath.Join(originalSkills, "alpha"),
		filepath.Join(replacementSkills, "alpha"),
	} {
		info, err := os.Lstat(entry)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("agent entry %s must remain the original directory, info=%v err=%v", entry, info, err)
		}
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("the conflicting adopt WAL must remain pending, got %+v", pending)
	}
}

// reservedNameAgent is a fakeAgent whose Reserved() list contains a *legal*
// skill name (the adapters' real reserved names are all invalid skill names
// today, which is why the per-entry gap stayed dormant; rule 11 promises
// reserved entries are never adopted regardless of shape).
type reservedNameAgent struct{ name, dir string }

func (r reservedNameAgent) Name() string       { return r.name }
func (r reservedNameAgent) Detect() bool       { return true }
func (r reservedNameAgent) SkillsDir() string  { return r.dir }
func (r reservedNameAgent) Reserved() []string { return []string{"system"} }

// TestAdoptSkipsReservedLegalName pins round 8 finding I2: the per-entry
// scan must apply the rule-11 reserved check like the whole-directory
// branch does, even when the reserved name is a valid skill name.
func TestAdoptSkipsReservedLegalName(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "system", "---\nname: system\ndescription: d\n---\n")
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{reservedNameAgent{"claude", dir}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	// The reserved entry is untouched, never archived.
	info, lerr := os.Lstat(filepath.Join(dir, "system"))
	if lerr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("reserved entry must stay a real untouched directory: %v %v", info, lerr)
	}
	if _, err := os.Stat(filepath.Join(dir, "system", "SKILL.md")); err != nil {
		t.Fatalf("reserved entry content must survive: %v", err)
	}
	cfg, cerr := store.LoadConfig(s.ConfigPath())
	if cerr != nil {
		t.Fatal(cerr)
	}
	if cfg.HasSkill("system") {
		t.Fatal("reserved name must never be registered")
	}
}

// TestAdoptIsolatesPerSkillInstallFailure pins round 11 finding I1: an
// install-stage failure of one candidate (here: an escape symlink that
// passes the inventory but fails ValidateLinks at install) must not abort
// the whole run -- DESIGN §6 阶段三 step 7's "任一 skill 失败不影响其他项"
// covers the install stage. The failing candidate is reported in
// res.Failed, the remaining candidates are still adopted.
func TestAdoptIsolatesPerSkillInstallFailure(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	// A valid-looking skill whose tree carries an escaping symlink: the
	// inventory (SKILL.md meta only) passes it; ValidateLinks refuses it at
	// install time.
	writeSkillTree(t, dir, "evil", "---\nname: evil\ndescription: d\n---\n")
	if err := os.Symlink("../../../../../../outside", filepath.Join(dir, "evil", "escape")); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatalf("one failing candidate must not abort the run: %v", err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("the healthy candidate must still be adopted, adopted = %+v", res.Adopted)
	}
	found := false
	for _, f := range res.Failed {
		if f.Action.Skill == "evil" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failing candidate must be reported, failed = %+v", res.Failed)
	}
	cfg, cerr := store.LoadConfig(s.ConfigPath())
	if cerr != nil {
		t.Fatal(cerr)
	}
	if cfg.HasSkill("evil") {
		t.Fatal("the failing candidate must not be registered")
	}
}

// TestAdoptCommittedSwitchFailureIsFailureClass pins round 13 finding I4:
// when the skill was committed and registered but the agent-side switching
// fails (here: the afterAdoptSwitch hook errors), the run must report the
// post-commit class -- a warning naming the state and exit-1 semantics
// (ErrOperationFailed) -- never an install-class "invalid" entry, and the
// transaction must stay open so the next write finishes the switching.
func TestAdoptCommittedSwitchFailureIsFailureClass(t *testing.T) {
	s, _ := setupStore(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeSkillTree(t, dir1, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	writeSkillTree(t, dir2, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir1}, fakeAgent{"codex", dir2}}
	h := hooks{afterAdoptSwitch: func() error { return errors.New("simulated switch failure") }}

	res, err := adopt(s, agents, "", h)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("committed-but-unswitched must be exit-1 class, got %v", err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "installed but its agents could not be switched") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("must warn about the installed-but-unswitched state, warnings = %v", res.Warnings)
	}
	// The skill was committed and registered.
	cfg, cerr := store.LoadConfig(s.ConfigPath())
	if cerr != nil {
		t.Fatal(cerr)
	}
	if !cfg.HasSkill("pdf-tools") {
		t.Fatal("the skill must be registered despite the switch failure")
	}
	// The transaction stays open for the next write to finish the switching.
	pending, perr := PendingTxns(s)
	if perr != nil {
		t.Fatal(perr)
	}
	if len(pending) != 1 {
		t.Fatalf("the transaction must stay open, pending = %+v", pending)
	}
}

func TestAdoptGenericCommittedFailureIsNotInvalidCandidate(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	interrupted := errors.New("interrupted after commit")

	res, err := adopt(s, []agent.Agent{fakeAgent{"claude", dir}}, "", hooks{
		afterCommit: func() error { return interrupted },
	})
	if !errors.Is(err, ErrOperationFailed) || !errors.Is(err, interrupted) {
		t.Fatalf("adopt error = %v, want operation failure carrying %v", err, interrupted)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("a committed candidate must not be classified as invalid: %+v", res.Failed)
	}
	if len(res.Adopted) != 0 || len(res.Pending) != 1 || res.Pending[0].Name != "alpha" {
		t.Fatalf("durable but incomplete adoption must be pending: adopted=%+v pending=%+v", res.Adopted, res.Pending)
	}
	outcome := res.Pending[0].Operation
	if !outcome.Committed || !outcome.RecoveryPending || outcome.PostCommitComplete || outcome.WALComplete {
		t.Fatalf("adopt outcome must identify the pending post-commit work: %+v", outcome)
	}
}

// TestAdoptRejectsByteIdenticalEntryReplacement binds switching to the
// filesystem entry that was inventoried. Matching type and bytes are not
// ownership: another writer can replace a directory after the commit. Adopt
// must preserve that replacement, isolate that agent, and finish the WAL.
func TestAdoptRejectsByteIdenticalEntryReplacement(t *testing.T) {
	s, _ := setupStore(t)
	firstDir, secondDir := t.TempDir(), t.TempDir()
	body := "---\nname: alpha\ndescription: d\n---\n"
	writeSkillTree(t, firstDir, "alpha", body)
	writeSkillTree(t, secondDir, "alpha", body)
	agents := []agent.Agent{fakeAgent{"first", firstDir}, fakeAgent{"second", secondDir}}

	var replacement os.FileInfo
	h := hooks{afterAdoptSwitch: func() error {
		original := filepath.Join(secondDir, "alpha")
		if err := os.Rename(original, original+"-original"); err != nil {
			return err
		}
		writeSkillTree(t, secondDir, "alpha", body)
		var err error
		replacement, err = os.Lstat(original)
		return err
	}}

	res, err := adopt(s, agents, "", h)
	if err != nil {
		t.Fatalf("a byte-identical entry replacement must isolate its agent: %v", err)
	}
	if len(res.Adopted) != 1 || len(res.Adopted[0].Agents) != 1 || res.Adopted[0].Agents[0] != "first" {
		t.Fatalf("only the successfully switched agent may be reported: %+v", res.Adopted)
	}
	current, statErr := os.Lstat(filepath.Join(secondDir, "alpha"))
	if statErr != nil || replacement == nil || !os.SameFile(current, replacement) || !current.IsDir() {
		t.Fatalf("the replacement must remain at the agent path, current=%v replacement=%v err=%v", current, replacement, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(secondDir, "alpha-original", "SKILL.md")); statErr != nil {
		t.Fatalf("the inventoried original must also remain available: %v", statErr)
	}
	pending, pendingErr := PendingTxns(s)
	if pendingErr != nil {
		t.Fatal(pendingErr)
	}
	if len(pending) != 0 {
		t.Fatalf("the isolated adopt must close its WAL, got %+v", pending)
	}
}

func TestCaptureAdoptTargetResolvesRecordedRawLink(t *testing.T) {
	agentDir := t.TempDir()
	sourceA, sourceB := t.TempDir(), t.TempDir()
	pathA := writeSkillTree(t, sourceA, "alpha", "---\nname: alpha\ndescription: a\n---\n")
	pathB := writeSkillTree(t, sourceB, "alpha", "---\nname: alpha\ndescription: b\n---\n")
	link := filepath.Join(agentDir, "alpha")
	if err := os.Symlink(pathA, link); err != nil {
		t.Fatal(err)
	}
	hookRan := false
	target, err := captureAdoptTargetWithHooks(fakeAgent{"claude", agentDir}, "alpha", "sha256:test", false, nil, func() error {
		hookRan = true
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink(pathB, link)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hookRan {
		t.Fatal("link-read race hook did not run")
	}
	canonicalA, err := filepath.EvalSymlinks(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if target.LinkTarget != pathA || target.SourcePath != canonicalA {
		t.Fatalf("capture mixed link generations: raw=%q source=%q, want raw=%q source=%q", target.LinkTarget, target.SourcePath, pathA, canonicalA)
	}
}

func TestAbandonPlannedAdoptArchiveRequiresMatchingAgent(t *testing.T) {
	txn := &TxnRecord{Archive: &AdoptArchive{Agent: "codex", Stage: "planned"}}
	err := abandonPlannedAdoptArchive(txn, "claude")
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("mismatched archive owner must conflict, got %v", err)
	}
	if txn.Archive == nil {
		t.Fatal("a different agent's archive plan must not be cleared")
	}
}

// TestAdoptRejectsReplacementAfterArchiveStarts closes the longer race after
// retirement. A new occupant of the now-vacant live name is unrelated to the
// retired inode and must survive, while the exact recovery copy preserves the
// inventoried original.
//
// Hook-driven, not timing-driven (round 18 finding I16). This test used to
// spawn a goroutine polling the recovery directory every millisecond up to
// 10,000 times, racing a 24 MB rename against the archive copy. If the copy
// won -- which it does on a loaded machine under -race -- the rename merely
// replaced the finished fu symlink and every assertion still passed. Nothing
// asserted the window had been hit, so a data-loss-adjacent path could
// silently degrade into a no-op while reporting green. afterAdoptRetire fires
// exactly once, in exactly the window the test is about.
func TestAdoptRejectsReplacementAfterArchiveStarts(t *testing.T) {
	s, _ := setupStore(t)
	agentDir := t.TempDir()
	body := "---\nname: alpha\ndescription: d\n---\n"
	original := writeSkillTree(t, agentDir, "alpha", body)
	if err := os.WriteFile(filepath.Join(original, "original.txt"), []byte("inventoried"), 0o644); err != nil {
		t.Fatal(err)
	}
	replacement := writeSkillTree(t, agentDir, "alpha-replacement", body)
	if err := os.WriteFile(filepath.Join(replacement, "replacement.txt"), []byte("newcomer"), 0o644); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Lstat(replacement)
	if err != nil {
		t.Fatal(err)
	}

	// The original has been retired and the archive copy has not run yet: the
	// live name is vacant and a new occupant takes it.
	fired := 0
	h := hooks{afterAdoptRetire: func() error {
		fired++
		if fired > 1 {
			return nil
		}
		return os.Rename(replacement, original)
	}}

	_, err = adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "", h)
	if fired == 0 {
		t.Fatal("the post-retirement window was never entered; the test would prove nothing")
	}
	if err != nil {
		t.Fatalf("a post-retirement occupant is reported by reconcile, not a transaction conflict: %v", err)
	}
	info, statErr := os.Lstat(original)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, replacementInfo) {
		t.Fatalf("the replacement must remain at the agent path, info=%v err=%v", info, statErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(original, "replacement.txt")); readErr != nil || string(got) != "newcomer" {
		t.Fatalf("replacement content must survive: %q %v", got, readErr)
	}
	archivedOriginal := false
	entries, readErr := os.ReadDir(s.RecoveryDir())
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "adopt-archive-claude-alpha-") {
			continue
		}
		// The archive must hold the inventoried original, never the newcomer.
		if _, statErr := os.Stat(filepath.Join(s.RecoveryDir(), entry.Name(), "replacement.txt")); statErr == nil {
			t.Fatalf("the archive captured the replacement instead of the original: %s", entry.Name())
		}
		if _, statErr := os.Stat(filepath.Join(s.RecoveryDir(), entry.Name(), "original.txt")); statErr == nil {
			archivedOriginal = true
		}
	}
	if !archivedOriginal {
		t.Fatal("the exact recovery archive must preserve the inventoried original")
	}
	pending, pendingErr := PendingTxns(s)
	if pendingErr != nil {
		t.Fatal(pendingErr)
	}
	if len(pending) != 0 {
		t.Fatalf("post-retirement name reuse must not strand the completed WAL, got %+v", pending)
	}
}

func TestAdoptRetirementPreservesReplacementAfterFinalApproval(t *testing.T) {
	s, _ := setupStore(t)
	agentDir := t.TempDir()
	body := "---\nname: alpha\ndescription: d\n---\n"
	original := writeSkillTree(t, agentDir, "alpha", body)
	foreign := filepath.Join(agentDir, "foreign-alpha")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "mine.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	replaced := false
	h := hooks{beforeAdoptRetire: func() error {
		if err := os.Rename(original, original+"-approved"); err != nil {
			return err
		}
		if err := os.Rename(foreign, original); err != nil {
			return err
		}
		replaced = true
		return nil
	}}
	_, err := adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("replacement at the retirement boundary must conflict, got %v", err)
	}
	if !replaced {
		t.Fatal("test setup error: retirement hook did not run")
	}
	got, readErr := os.ReadFile(filepath.Join(original, "mine.txt"))
	if readErr != nil || string(got) != "mine" {
		t.Fatalf("foreign replacement must survive at the original name: %q, %v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(original+"-approved", "SKILL.md")); statErr != nil {
		t.Fatalf("approved original must also survive the conflict: %v", statErr)
	}
}

func TestAdoptRecoveryArchivePreservesExactTree(t *testing.T) {
	s, _ := setupStore(t)
	agentDir := t.TempDir()
	original := writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	if err := os.Chmod(original, os.ModeSetgid|0o711); err != nil {
		t.Fatal(err)
	}
	specialDir := filepath.Join(original, "private")
	if err := os.Mkdir(specialDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(specialDir, os.ModeSetgid|os.ModeSticky|0o751); err != nil {
		t.Fatal(err)
	}
	specialFile := filepath.Join(specialDir, "run.sh")
	if err := os.WriteFile(specialFile, []byte("#!/bin/sh\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(specialFile, os.ModeSetuid|0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(original, ".git", "objects", "aa"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, ".git", "config"), []byte("[core]\n\tbare = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, ".git", "objects", "aa", "object"), []byte("git-object"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "adopt-archive-claude-alpha-") {
			archive = filepath.Join(s.RecoveryDir(), entry.Name())
			break
		}
	}
	if archive == "" {
		t.Fatal("exact adopt recovery archive was not retained")
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o711 {
		t.Fatalf("archive root mode = %#o, want 0711", info.Mode().Perm())
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("archive root retained unsupported special bits: %v", info.Mode())
	}
	for _, path := range []string{
		filepath.Join(archive, "private"),
		filepath.Join(archive, "private", "run.sh"),
		filepath.Join(s.SkillsDir(), "alpha", "private"),
		filepath.Join(s.SkillsDir(), "alpha", "private", "run.sh"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Fatalf("copied path %s retained unsupported special bits: %v", path, info.Mode())
		}
	}
	got, err := os.ReadFile(filepath.Join(archive, ".git", "config"))
	if err != nil || string(got) != "[core]\n\tbare = false\n" {
		t.Fatalf("archive must preserve .git/config exactly: %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(archive, ".git", "objects", "aa", "object"))
	if err != nil || string(got) != "git-object" {
		t.Fatalf("archive must preserve .git objects exactly: %q, %v", got, err)
	}
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatalf("following write must not wedge on normalized recovery modes: %v", err)
	}
	if pending, err := PendingTxns(s); err != nil || len(pending) != 0 {
		t.Fatalf("adopt must leave no pending WAL: pending=%+v err=%v", pending, err)
	}
}

func TestAdoptSymlinkArchivesLinkWithoutCopyingTarget(t *testing.T) {
	s, _ := setupStore(t)
	external := t.TempDir()
	target := writeSkillTree(t, external, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(target, ".git", "index")
	if err := os.WriteFile(index, []byte("external-index"), 0o600); err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(agentDir, "alpha")); err != nil {
		t.Fatal(err)
	}

	archiveHookFired := false
	res, err := adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "", hooks{
		afterAdoptArchiveCopy: func() error {
			archiveHookFired = true
			return errors.New("symlink form must not copy an archive")
		},
	})
	if err != nil {
		t.Fatalf("symlink-form adopt must not depend on an archive copy: %v", err)
	}
	if archiveHookFired {
		t.Fatal("symlink-form adopt entered the tree-archive path")
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "alpha" {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	got, err := os.ReadFile(index)
	if err != nil || string(got) != "external-index" {
		t.Fatalf("external target must remain untouched: %q, %v", got, err)
	}
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "adopt-archive-claude-alpha-") {
			t.Fatalf("symlink-form adopt must not retain a copy of the external target: %s", entry.Name())
		}
	}
	records := readAdoptLinkArchives(t, s)
	if len(records) != 1 || records[0].RawTarget != target || records[0].OriginalPath != filepath.Join(agentDir, "alpha") {
		t.Fatalf("removed symlink was not durably archived: %+v", records)
	}
}

func TestAdoptSymlinkRecoveryRequiresDurableArchiveBeforeUnlink(t *testing.T) {
	s, _ := setupStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	external := t.TempDir()
	target := writeSkillTree(t, external, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	agentDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(agentDir, "alpha")); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after symlink retirement")
	_, _ = adopt(s, []agent.Agent{agent.Claude{}}, "", hooks{
		afterAdoptRetire: func() error { return stop },
	})
	pending, err := PendingTxns(s)
	if err != nil || len(pending) != 1 || pending[0].Archive == nil {
		t.Fatalf("pending symlink adopt = %+v, %v", pending, err)
	}
	archive := pending[0].Archive
	retiredPath := filepath.Join(agentDir, archive.Retired)
	if got, err := os.Readlink(retiredPath); err != nil || got != target {
		t.Fatalf("retired symlink = %q, %v; want %q", got, err, target)
	}
	archivePath := filepath.Join(s.RecoveryDir(), archive.LinkArchive)
	if err := os.WriteFile(archivePath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) ||
		!strings.Contains(err.Error(), archive.LinkArchive) ||
		!strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("changed durable link archive must stop recovery, got %v", err)
	}
	if got, err := os.Readlink(retiredPath); err != nil || got != target {
		t.Fatalf("recovery removed the symlink without a valid durable archive: %q, %v", got, err)
	}
	if pending, err := PendingTxns(s); err != nil || len(pending) != 1 {
		t.Fatalf("invalid durable archive must keep its WAL open: pending=%+v err=%v", pending, err)
	}
}

func TestAdoptRefusesOversizedExactArchiveBeforeRetire(t *testing.T) {
	s, _ := setupStore(t)
	agentDir := t.TempDir()
	original := writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	packDir := filepath.Join(original, ".git", "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(packDir, "pack-large.pack")
	f, err := os.Create(pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((64 << 20) + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	res, _ := adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "", hooks{})
	if _, err := os.Stat(filepath.Join(original, "SKILL.md")); err != nil {
		t.Fatalf("oversized exact archive must be refused before retiring the visible skill: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "alpha" {
		t.Fatalf("the installed but unswitched skill must be reported pending, got %+v", res.Pending)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("an isolatable pre-retire refusal must not leave a recovery wedge: %+v", pending)
	}
	if _, err := NewSkill(s, nil, "later"); err != nil {
		t.Fatalf("a later write must remain usable after the refusal: %v", err)
	}
}

func TestAdoptResumeValidatesArchiveBeforeClearingAbsentOriginal(t *testing.T) {
	s, _ := setupStore(t)
	agentDir := t.TempDir()
	writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	stop := errors.New("stop after exact archive")
	h := hooks{afterAdoptArchiveCopy: func() error { return stop }}
	_, err := adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "", h)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("adopt must stop at the archive boundary, got %v", err)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Archive == nil {
		t.Fatalf("archive state must remain durable, got %+v", pending)
	}
	record := pending[0]
	payload := filepath.Join(s.RecoveryDir(), record.Archive.Payload)
	if err := os.Remove(filepath.Join(payload, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	session, sessionErr := s.BeginWrite()
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	defer session.Close()
	err = switchAdoptEntry(session.Store, fakeAgent{"claude", agentDir}, "alpha", &record)
	if !errors.Is(err, store.ErrOwnedTreeChanged) && !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("resume must reject a changed archive, got %v", err)
	}
	if record.Archive == nil {
		t.Fatal("changed archive must not be cleared from the WAL state")
	}
	_, err = switchAdoptedEntriesReporting(session.Store, []agent.Agent{fakeAgent{"claude", agentDir}}, "alpha", &record, hooks{})
	if !errors.Is(err, store.ErrOwnedTreeChanged) || !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("isolation refusal must retain both the archive failure and abandon conflict, got %v", err)
	}
}

func TestAdoptRecoveryEscapesOrphanedUnjournalledArchiveRoot(t *testing.T) {
	s, _ := setupStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	stop := errors.New("stop after retirement")
	_, err := adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "", hooks{
		afterAdoptRetire: func() error { return stop },
	})
	if err == nil {
		t.Fatal("adopt must leave the retired archive pending")
	}
	pending, err := PendingTxns(s)
	if err != nil || len(pending) != 1 || pending[0].Archive == nil {
		t.Fatalf("pending adopt = %+v, %v", pending, err)
	}
	orphan := filepath.Join(s.RecoveryDir(), pending[0].Archive.Payload)
	if err := os.Mkdir(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatalf("recovery must select a fresh archive name after an unjournalled mkdir: %v", err)
	}
	if info, err := os.Stat(orphan); err != nil || !info.IsDir() {
		t.Fatalf("the unowned orphan must be left untouched for explicit pruning: info=%v err=%v", info, err)
	}
	if pending, err := PendingTxns(s); err != nil || len(pending) != 0 {
		t.Fatalf("recovery must complete after escaping the orphan: pending=%+v err=%v", pending, err)
	}
}

func TestAdoptRecoveryDropsAgentWhenRecordedObjectIsUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  string
		mutate func(*testing.T, string, TxnRecord)
	}{
		{
			name:  "planned original deleted",
			stage: "planned",
			mutate: func(t *testing.T, agentDir string, _ TxnRecord) {
				t.Helper()
				if err := os.RemoveAll(filepath.Join(agentDir, "alpha")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "retired entry deleted",
			stage: "retired",
			mutate: func(t *testing.T, agentDir string, record TxnRecord) {
				t.Helper()
				if err := os.RemoveAll(filepath.Join(agentDir, record.Archive.Retired)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupStore(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			agentDir := filepath.Join(home, ".claude", "skills")
			if err := os.MkdirAll(agentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
			stop := errors.New("stop at archive boundary")
			h := hooks{}
			if tc.stage == "planned" {
				h.beforeAdoptRetire = func() error { return stop }
			} else {
				h.afterAdoptRetire = func() error { return stop }
			}
			_, _ = adopt(s, []agent.Agent{agent.Claude{}}, "", h)
			pending, err := PendingTxns(s)
			if err != nil || len(pending) != 1 || pending[0].Archive == nil || pending[0].Archive.Stage != tc.stage {
				t.Fatalf("pending %s adopt = %+v, %v", tc.stage, pending, err)
			}
			tc.mutate(t, agentDir, pending[0])

			outcome, err := NewSkill(s, nil, "beta")
			if err != nil {
				t.Fatalf("unreachable recorded object must be isolated during recovery: %v", err)
			}
			warning := strings.Join(outcome.Warnings, "\n")
			for _, want := range []string{
				"claude", "alpha", "could not switch", "fu rm alpha", "fu adopt",
				filepath.Join(agentDir, pending[0].Archive.Retired),
			} {
				if !strings.Contains(warning, want) {
					t.Fatalf("recovery isolation warning %q does not contain %q", warning, want)
				}
			}
			if pending, err := PendingTxns(s); err != nil || len(pending) != 0 {
				t.Fatalf("isolated adopt must clear its WAL: pending=%+v err=%v", pending, err)
			}
		})
	}
}

func TestAdoptRecoveryKeepsWALWhenRecordedParentIsUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string, TxnRecord) string
	}{
		{
			name: "ancestor replaced",
			mutate: func(t *testing.T, agentDir string, record TxnRecord) string {
				t.Helper()
				ancestor := filepath.Dir(agentDir)
				moved := ancestor + "-old"
				if err := os.Rename(ancestor, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(agentDir, 0o755); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(moved, "skills", record.Archive.Retired)
			},
		},
		{
			name: "skills directory replaced",
			mutate: func(t *testing.T, agentDir string, record TxnRecord) string {
				t.Helper()
				moved := agentDir + ".bak"
				if err := os.Rename(agentDir, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(agentDir, 0o755); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(moved, record.Archive.Retired)
			},
		},
		{
			name: "ancestor missing",
			mutate: func(t *testing.T, agentDir string, record TxnRecord) string {
				t.Helper()
				ancestor := filepath.Dir(agentDir)
				moved := ancestor + "-old"
				if err := os.Rename(ancestor, moved); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(moved, "skills", record.Archive.Retired)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupStore(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			agentDir := filepath.Join(home, ".claude", "skills")
			original := writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
			if err := os.MkdirAll(filepath.Join(original, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(original, ".git", "config"), []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("stop after retirement")
			_, _ = adopt(s, []agent.Agent{agent.Claude{}}, "", hooks{
				afterAdoptRetire: func() error { return stop },
			})
			pending, err := PendingTxns(s)
			if err != nil || len(pending) != 1 || pending[0].Archive == nil || pending[0].Archive.Stage != "retired" {
				t.Fatalf("pending retired adopt = %+v, %v", pending, err)
			}
			retiredPath := tc.mutate(t, agentDir, pending[0])

			_, err = NewSkill(s, nil, "beta")
			if !errors.Is(err, ErrTxnConflict) {
				t.Fatalf("unaddressable recorded parent must keep recovery pending, got %v", err)
			}
			for _, want := range []string{agentDir, pending[0].Archive.Retired} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("recovery conflict %q does not name %q", err, want)
				}
			}
			if got, readErr := os.ReadFile(filepath.Join(retiredPath, ".git", "config")); readErr != nil || string(got) != "preserve" {
				t.Fatalf("retired user tree was not preserved at %s: %q, %v", retiredPath, got, readErr)
			}
			stillPending, pendingErr := PendingTxns(s)
			if pendingErr != nil || len(stillPending) != 1 || stillPending[0].Archive == nil {
				t.Fatalf("unaddressable parent must retain its WAL: pending=%+v err=%v", stillPending, pendingErr)
			}
		})
	}
}

func TestAdoptRecoveryConflictNamesOriginalAndRetiredPaths(t *testing.T) {
	s, _ := setupStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	stop := errors.New("stop after retirement")
	_, _ = adopt(s, []agent.Agent{agent.Claude{}}, "", hooks{
		afterAdoptRetire: func() error { return stop },
	})
	pending, err := PendingTxns(s)
	if err != nil || len(pending) != 1 || pending[0].Archive == nil {
		t.Fatalf("pending adopt = %+v, %v", pending, err)
	}
	original := filepath.Join(agentDir, "alpha")
	retired := filepath.Join(agentDir, pending[0].Archive.Retired)
	if err := os.Mkdir(original, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = NewSkill(s, nil, "beta")
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("dual adopt entries must conflict, got %v", err)
	}
	for _, want := range []string{original, retired, "move one aside", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("dual-entry conflict %q lacks %q", err, want)
		}
	}
}

func TestFilteredAdoptSummaryAgentsDoesNotAliasTransactionAgents(t *testing.T) {
	txnAgents := []string{"failed-whole", "kept"}
	got := filteredAdoptSummaryAgents(txnAgents, map[string]bool{"failed-whole": true}, map[string]bool{})
	if strings.Join(got, ",") != "kept" {
		t.Fatalf("filtered agents = %v, want kept", got)
	}
	got[0] = "changed"
	if strings.Join(txnAgents, ",") != "failed-whole,kept" {
		t.Fatalf("summary result aliases transaction agents: %v", txnAgents)
	}
}

// TestAdoptKeepsWALOpenWhenArchiveFailsAfterRetirement pins round 18 finding
// C1: isolation is only safe while nothing of the user's has been moved.
// Once ensureAdoptOriginalRetired has renamed the original to
// .fu-adopt-retired-<hex>, a plain (non-conflict) failure must propagate
// instead of dropping the agent -- dropping it lets PostCommit succeed,
// ClearTxn close the WAL, and the command exit 0 with the user's tree hidden
// under a random dot-name and an empty recovery archive.
func TestAdoptKeepsWALOpenWhenArchiveFailsAfterRetirement(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	fired := false
	h := hooks{afterAdoptRetire: func() error {
		if fired {
			return nil
		}
		fired = true
		return errors.New("simulated recovery payload failure")
	}}

	_, err := adopt(s, agents, "", h)
	if err == nil {
		t.Fatal("a post-retirement archive failure must not be reported as success")
	}
	pending, perr := PendingTxns(s)
	if perr != nil {
		t.Fatalf("journal must stay readable: %v", perr)
	}
	if len(pending) == 0 {
		t.Fatal("the WAL must stay open so a later write command can finish or unwind the retired original")
	}
}

func TestAdoptSymlinkKeepsWALOpenAfterRetirementFailure(t *testing.T) {
	s, _ := setupStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	target := writeSkillTree(t, targetRoot, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	original := filepath.Join(agentDir, "alpha")
	if err := os.Symlink(target, original); err != nil {
		t.Fatal(err)
	}
	fired := false
	h := hooks{afterAdoptRetire: func() error {
		fired = true
		return errors.New("simulated failure after symlink retirement")
	}}

	_, err := adopt(s, []agent.Agent{fakeAgent{"claude", agentDir}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("post-retirement symlink failure must remain a transaction conflict, got %v", err)
	}
	if !fired {
		t.Fatal("the symlink retirement boundary was never reached")
	}
	pending, perr := PendingTxns(s)
	if perr != nil {
		t.Fatal(perr)
	}
	if len(pending) != 1 || pending[0].Archive == nil || pending[0].Archive.Stage != "retired" {
		t.Fatalf("retired symlink must remain recoverable in the WAL: %+v", pending)
	}
	if _, statErr := os.Lstat(original); !os.IsNotExist(statErr) {
		t.Fatalf("the original name must be vacant after retirement, got %v", statErr)
	}
	retired := filepath.Join(agentDir, pending[0].Archive.Retired)
	if got, readErr := os.Readlink(retired); readErr != nil || got != target {
		t.Fatalf("the inventoried symlink must remain at its recorded retired name, got %q err=%v", got, readErr)
	}

	// Recovery is the prologue of the next write command. Merely proving that
	// the WAL exists would miss a validator that rejects a stage the handler
	// itself persists.
	if _, nextErr := NewSkill(s, nil, "beta"); nextErr != nil {
		t.Fatalf("a following write command must resume the retired symlink stage: %v", nextErr)
	}
	if pending, pendingErr := PendingTxns(s); pendingErr != nil || len(pending) != 0 {
		t.Fatalf("following write must finish and clear the adopt WAL: pending=%+v err=%v", pending, pendingErr)
	}
}

// TestAdoptWithNoSwitchedAgentIsNotReportedAsAdopted pins round 18 finding
// C2: when every holding agent is isolated, the destructive half of adopt did
// not happen, so the run must not be filed under Adopted -- which renders as
// `adopted <name> (from )` with an empty parenthetical at exit 0.
func TestAdoptWithNoSwitchedAgentIsNotReportedAsAdopted(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	// Change the original after the commit but before the switch: the archive
	// plan refuses with a plain "changed since the inventory" error, so the
	// only holding agent is isolated with nothing moved.
	h := hooks{afterCommit: func() error {
		body := "---\nname: pdf-tools\ndescription: changed\n---\n\nbody\n"
		return os.WriteFile(filepath.Join(dir, "pdf-tools", "SKILL.md"), []byte(body), 0o644)
	}}

	res, _ := adopt(s, agents, "", h)
	for _, summary := range res.Adopted {
		if len(summary.Agents) == 0 {
			t.Fatalf("a skill with no switched agent must not be reported as adopted: %+v", summary)
		}
	}
	if len(res.Adopted) != 0 {
		t.Fatalf("Adopted = %+v; want none", res.Adopted)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "pdf-tools" {
		t.Fatalf("Pending = %+v; want the unswitched skill", res.Pending)
	}
}

func TestAdoptIsolationSurfacesArchiveFailureAndRemedy(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	skillDir := writeSkillTree(t, dir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	objectDir := filepath.Join(skillDir, ".git", "objects")
	if err := os.MkdirAll(objectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	largePath := filepath.Join(objectDir, "oversized")
	large, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(65 << 20); err != nil {
		_ = large.Close()
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := Adopt(s, []agent.Agent{fakeAgent{"claude", dir}}, "")
	if err != nil {
		t.Fatalf("an isolatable archive refusal must be reported in the result, got %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "alpha" {
		t.Fatalf("the installed but unswitched skill must remain pending: %+v", res.Pending)
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"oversized", "copy limit", "fu rm alpha", "fu adopt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("isolation warning %q must contain %q", joined, want)
		}
	}
}

// TestPerEntrySwitchIsolatesRecreatedParent pins the per-entry form of the
// same ownership rule as the whole-directory building test. No archive plan
// exists yet, so replacing the user-owned skills directory must isolate this
// agent and close the transaction rather than retain an unrecoverable inode
// comparison forever.
func TestPerEntrySwitchIsolatesRecreatedParent(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	a := fakeAgent{"claude", dir}
	digest, err := digestDir(filepath.Join(dir, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, nil, "alpha", digest)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{
		Op: "adopt", Name: "alpha", AdoptTargets: targets, Agents: []string{"claude"},
	}
	replaceDirectoryWithCopy(t, dir)

	if err := switchAdoptedEntriesWithHooks(s, []agent.Agent{a}, "alpha", txn, hooks{}); err != nil {
		t.Fatalf("a recreated user parent before retirement must isolate the agent: %v", err)
	}
	if txn.Archive != nil || len(txn.Agents) != 0 {
		t.Fatalf("the failed agent must reach a terminal isolated state: %+v", txn)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha", "SKILL.md")); err != nil {
		t.Fatalf("the recreated user entry must remain untouched: %v", err)
	}
}

// TestAdoptReportsInvalidCandidates pins round 18 finding I11. SPEC rule 7
// requires rule-7 validation 在 add 与 adopt 时 and ends with 不合规拒绝并说明
// 原因. add honours this; adopt dropped the candidate with a bare `continue`,
// so a SKILL.md whose name disagrees with its directory produced exactly one
// line -- "nothing to adopt" -- with no hint that anything was seen, rejected,
// or fixable.
func TestAdoptReportsInvalidCandidates(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: wrong-name\ndescription: d\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 0 {
		t.Fatalf("an invalid candidate must not be adopted: %+v", res.Adopted)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("the rejected candidate must be reported with a reason, got %+v", res.Failed)
	}
	if res.Failed[0].Action.Skill != "pdf-tools" {
		t.Fatalf("the report must name the entry: %+v", res.Failed[0])
	}
	if !strings.Contains(res.Failed[0].Err.Error(), "must match directory name") {
		t.Fatalf("the report must give the reason: %v", res.Failed[0].Err)
	}
}
