// internal/engine/ops_test.go
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

func TestNewSkillScaffoldsAndLinks(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"))
	if err != nil {
		t.Fatal("SKILL.md not scaffolded")
	}
	if len(raw) == 0 {
		t.Fatal("empty scaffold")
	}
	cfg, _ := store.LoadConfig(s.ConfigPath())
	if !cfg.HasSkill("alpha") || !cfg.Enabled("alpha") {
		t.Fatal("config entry missing or not enabled by default")
	}
	if cfg.Digest("alpha") == "" {
		t.Fatal("digest baseline must be recorded")
	}
	if _, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal("link not materialized")
	}
	// duplicate name is refused (SPEC rule 1)
	if _, err := NewSkill(s, agents, "alpha"); err == nil {
		t.Fatal("duplicate name must be refused")
	}
	// invalid name is refused before any mutation
	if _, err := NewSkill(s, agents, "Bad--Name"); err == nil {
		t.Fatal("invalid name must be refused")
	}
}

// ---- self-review additions ----

// An invalid name must be refused by ValidateName before Run is ever
// invoked, so a rejected NewSkill call must leave the store exactly as a
// fresh init left it: no skill directory, no config entry, and no new
// commit -- not merely "the command returned an error".
func TestNewSkillInvalidNameWritesNothing(t *testing.T) {
	s, _ := setupStore(t)
	before, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "Bad--Name"); err == nil {
		t.Fatal("invalid name must be refused")
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("Bad--Name") || len(cfg.SkillNames()) != 0 {
		t.Fatal("invalid name must not reach the config")
	}
	ents, err := os.ReadDir(s.SkillsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("invalid name must not create a skill directory, found: %+v", ents)
	}
	after, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("invalid name must not create a commit: before=%d after=%d", len(before), len(after))
	}
}

// Critical finding 1: the duplicate-name guard used to consult only
// cfg.HasSkill (fu.yaml), so a store directory that already held content
// but was never registered there -- e.g. a hand-authored skill (a
// documented workflow), or one left behind by a `fu.yaml` revert, or an
// interrupted operation -- was not a "duplicate" as far as the guard
// could see. MkdirAll and WriteFile then ran unconditionally and
// silently truncated whatever was already on disk. Reproduced against
// the compiled binary: `fu new precious` over a hand-written
// skills/precious/SKILL.md exited 0, printed "created precious", and
// replaced the file's content with the scaffold template. NewSkill must
// Lstat the target directory and refuse before writing, regardless of
// what fu.yaml says.
func TestNewSkillRefusesExistingUnregisteredDirectory(t *testing.T) {
	s, _ := setupStore(t)
	dir := filepath.Join(s.SkillsDir(), "precious")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := []byte("MY PRECIOUS HAND-WRITTEN CONTENT")
	skillFile := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillFile, precious, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "precious"); err == nil {
		t.Fatal("must refuse to scaffold over an existing unregistered store directory")
	}

	got, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(precious) {
		t.Fatalf("existing unregistered content must survive untouched, got %q", got)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("precious") {
		t.Fatal("a refused NewSkill must not register the skill in fu.yaml")
	}
}

func TestNewSkillChecksExistingTargetBeforeStartingTransaction(t *testing.T) {
	s, _ := setupStore(t)
	dir := filepath.Join(s.SkillsDir(), "precious")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(dir, "SKILL.md")
	want := []byte("pre-existing content")
	if err := os.WriteFile(skillFile, want, 0o644); err != nil {
		t.Fatal(err)
	}
	reachedWAL := errors.New("transaction WAL was written")

	if _, err := newSkill(s, nil, "precious", hooks{
		afterTxnStart: func() error { return reachedWAL },
	}); err == nil || errors.Is(err, reachedWAL) {
		t.Fatalf("pre-existing target must be refused before the initial WAL, got %v", err)
	}
	if pending, err := PendingTxns(s); err != nil {
		t.Fatal(err)
	} else if len(pending) != 0 {
		t.Fatalf("preflight refusal must leave no transaction record, got %+v", pending)
	}
	got, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("pre-existing target changed: got %q want %q", got, want)
	}
}

func TestNewSkillPreservesUnmatchedStagingEntry(t *testing.T) {
	s, _ := setupStore(t)
	staged := filepath.Join(s.StagingDir(), "alpha")
	nested := filepath.Join(staged, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(nested, "keep.txt")
	wantBytes := []byte("unmatched staging content")
	if err := os.WriteFile(keepPath, wantBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(staged, "keep-link")
	wantTarget := filepath.Join("nested", "keep.txt")
	if err := os.Symlink(wantTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "alpha"); err == nil {
		t.Error("unmatched staging content must make NewSkill refuse")
	}
	gotBytes, err := os.ReadFile(keepPath)
	if err != nil {
		t.Fatalf("unmatched nested file must survive: %v", err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("unmatched nested file changed: got %q want %q", gotBytes, wantBytes)
	}
	gotTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("unmatched symlink must survive: %v", err)
	}
	if gotTarget != wantTarget {
		t.Fatalf("unmatched symlink changed: got %q want %q", gotTarget, wantTarget)
	}
	if pending, err := PendingTxns(s); err != nil {
		t.Fatal(err)
	} else if len(pending) != 0 {
		t.Fatalf("preflight refusal must leave no transaction record, got %+v", pending)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("alpha") {
		t.Fatal("preflight refusal must not register the staged name")
	}
	if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight refusal must not publish a skill, got %v", err)
	}
}

// A duplicate name is refused inside Mutate before any filesystem write
// (the cfg.HasSkill check is its first statement), so the pipeline never
// reaches cfg.Save/Commit for that call. This must hold observably: the
// existing skill's content, recorded digest, materialized link, and the
// store's commit count must all be byte-for-byte unchanged by the failed
// attempt.
//
// The content is hand-edited between the two `new` calls specifically so
// an overwrite is observable: the scaffold template is a pure function of
// the name, so re-running it over an untouched fixture reproduces
// byte-identical output, and a destructive overwrite would hide behind
// that coincidence. Without the hand edit, this test previously kept
// passing even with cfg.HasSkill's duplicate check deleted outright,
// because cfg.AddSkill's own internal duplicate check still turned up a
// non-nil error at the end of Mutate -- after MkdirAll/WriteFile had
// already run and silently reproduced the same (byte-identical) scaffold.
func TestNewSkillDuplicateLeavesStoreUnchanged(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")
	link := filepath.Join(dir, "alpha")

	handEdit := []byte("hand-edited content that must never be scaffolded over")
	if err := os.WriteFile(skillFile, handEdit, 0o644); err != nil {
		t.Fatal(err)
	}
	// Sweep the hand edit into history now, same as any earlier fu command
	// would have, so the commit-count assertion below isolates the effect
	// of the *duplicate attempt* itself rather than conflating it with the
	// legitimate "external: manual modifications" commit the pipeline's
	// own Sweep step would otherwise take on the second NewSkill call.
	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}

	before, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	cfgBefore, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	digestBefore := cfgBefore.Digest("alpha")
	contentBefore, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contentBefore) != string(handEdit) {
		t.Fatal("sanity check: hand edit must be on disk before the second new")
	}
	linkBefore, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, agents, "alpha"); err == nil {
		t.Fatal("duplicate name must be refused")
	}

	after, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("duplicate attempt must not create a commit: before=%d after=%d", len(before), len(after))
	}
	cfgAfter, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfgAfter.Digest("alpha"); got != digestBefore {
		t.Fatalf("duplicate attempt must not alter the recorded digest: got %q want %q", got, digestBefore)
	}
	contentAfter, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contentAfter) != string(contentBefore) {
		t.Fatal("duplicate attempt must not rewrite the existing SKILL.md")
	}
	linkAfter, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkAfter != linkBefore {
		t.Fatalf("duplicate attempt must not disturb the existing link: got %q want %q", linkAfter, linkBefore)
	}
	ents, err := os.ReadDir(s.SkillsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("duplicate attempt must not leave stray entries under skills dir: %+v", ents)
	}
}

// The scaffold is only useful if fu itself would accept it: this exercises
// the exact parse/validate path `add`/`adopt` run at install time (SPEC
// rule 7), rather than just checking that some non-empty bytes exist.
// Table-driven over a single- and multi-hyphen name, since ValidateName's
// hyphen-joining rule is exactly where a hand-built template could most
// plausibly diverge from what Validate demands.
func TestNewSkillScaffoldPassesOwnValidation(t *testing.T) {
	for _, name := range []string{"alpha", "my-cool-skill"} {
		s, _ := setupStore(t)
		if _, err := NewSkill(s, nil, name); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(s.SkillsDir(), name)
		meta, err := skill.ParseMeta(dir)
		if err != nil {
			t.Fatalf("%s: scaffolded SKILL.md must parse: %v", name, err)
		}
		if err := skill.Validate(meta, name); err != nil {
			t.Fatalf("%s: scaffolded skill must pass fu's own validation: %v", name, err)
		}
	}
}

// The digest recorded in fu.yaml is the baseline `update`/local-modification
// detection compares against later; it must be exactly what Digest computes
// over the scaffolded directory, not an approximation, a pre-write value, or
// something that drifts once the pipeline's Commit step touches the tree.
func TestNewSkillDigestMatchesScaffoldedContent(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	want, err := skill.Digest(filepath.Join(s.SkillsDir(), "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Digest("alpha"); got != want {
		t.Fatalf("recorded digest %q does not match freshly computed digest %q", got, want)
	}
}

// No agents detected is the common case on a fresh machine (SPEC rule 4):
// NewSkill must still scaffold and record the skill rather than failing
// for lack of anywhere to link it, and Reconcile over an empty agent list
// must report nothing.
func TestNewSkillWorksWithNoAgents(t *testing.T) {
	s, _ := setupStore(t)
	res, err := NewSkill(s, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 || len(res.Foreign) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("no agents means nothing to reconcile, want an empty Result, got %+v", res)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") || !cfg.Enabled("alpha") {
		t.Fatal("skill must still be scaffolded and enabled even with zero agents detected")
	}
}

// Finding I3's ordering angle, exercised end to end: Reconcile runs
// after Store.Commit in the write pipeline, so before isolation, a
// failing agent's scan error aborted Reconcile with a hard error that
// NewSkill then propagated to its caller -- even though the config entry
// and the commit were already durable. That told the user the command
// had failed when it had, in fact, mostly succeeded. With isolation, the
// failure is reported in Result.Failed instead of aborting the pass, and
// the healthy agent is still reconciled.
//
// Round 2 finding 4 renamed this test (from
// TestNewSkillSucceedsDespiteOneBrokenAgent) and reversed its error
// expectation. "Succeeds" was the wrong word for what finding 4 fixed:
// NewSkill returning no error here made `fu new`'s process exit 0 even
// though claude (say) got nothing, so a script piping only stdout to
// /dev/null saw a clean run. The durability this test is actually about --
// config entry and commit both surviving, the healthy agent still linked
// -- is unchanged and still asserted below; what changed is that NewSkill
// now surfaces the broken agent's failure as an error (wrapping
// engine.ErrOperationFailed) so the CLI's exit code is 1, per finding 4's
// documented decision that Result.Failed means a genuine operation failure.
func TestNewSkillIsolatesBrokenAgentButReportsOperationFailure(t *testing.T) {
	s, _ := setupStore(t)
	base := t.TempDir()
	brokenPath := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(brokenPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	okDir := t.TempDir()
	agents := []agent.Agent{
		fakeAgent{"broken-agent", brokenPath},
		fakeAgent{"claude", okDir},
	}

	res, err := NewSkill(s, agents, "alpha")
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("a broken agent's scan failure must surface as ErrOperationFailed so the CLI exits 1 (finding 4), got %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.AgentName != "broken-agent" {
		t.Fatalf("want the broken agent's failure isolated in Result.Failed, got %+v", res.Failed)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") {
		t.Fatal("the config entry must be durable despite the reconcile-side failure")
	}
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 1 || entries[0].Message != "new: alpha" {
		t.Fatalf("the commit must be durable despite the reconcile-side failure, got %+v", entries)
	}
	want := filepath.Join(s.SkillsDir(), "alpha")
	if target, err := os.Readlink(filepath.Join(okDir, "alpha")); err != nil || target != want {
		t.Fatalf("the healthy agent must still be reconciled despite the other agent's failure: %v %q", err, target)
	}
}

func TestSetGlobalAndAgentSwitch(t *testing.T) {
	s, _ := setupStore(t)
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}, fakeAgent{"codex", codexDir}}
	NewSkill(s, agents, "alpha")

	// agent-level off: codex link removed, claude link stays
	if _, err := SetAgentSwitch(s, agents, "alpha", "codex", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(codexDir, "alpha")); !os.IsNotExist(err) {
		t.Fatal("codex link must be removed")
	}
	if _, err := os.Readlink(filepath.Join(claudeDir, "alpha")); err != nil {
		t.Fatal("claude link must remain")
	}

	// global off: remaining follow-global links removed; override survives
	if _, err := SetGlobal(s, agents, "alpha", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(claudeDir, "alpha")); !os.IsNotExist(err) {
		t.Fatal("claude follows global off")
	}

	// unknown skill / unknown agent are refused
	if _, err := SetGlobal(s, agents, "ghost", true); err == nil {
		t.Fatal("unknown skill must be refused")
	}
	if _, err := SetAgentSwitch(s, agents, "alpha", "gemini", true); err == nil {
		t.Fatal("unknown agent must be refused")
	}
}

// ---- self-review additions: SetGlobal / SetAgentSwitch ----

// Setting an agent's switch to the same value the global already has must
// not be recorded as an override (the config-level rule this relies on is
// TestSameValueNormalization in store/config_test.go); end to end through
// the engine, the redundant call must also leave the materialized link
// untouched rather than removing and recreating it.
func TestSetAgentSwitchSameAsGlobalIsNotStored(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil { // global enabled=true, claude linked
		t.Fatal(err)
	}

	if _, err := SetAgentSwitch(s, agents, "alpha", "claude", true); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Override("alpha", "claude"); ok {
		t.Fatal("a value equal to global must not be stored as an override")
	}
	if _, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal("link must remain present when the agent value matches global")
	}
}

// Flipping the global switch must move only the agents that follow it; an
// agent holding an override stays exactly where it is -- indefinitely, not
// just across the one flip that happens to make it equal to the new
// global value (round 3 finding 4). SPEC §4.1 scopes normalization to an
// agent-level switch write and separately says a global toggle does not
// clear overrides. Therefore a global toggle never
// touches overrides at all -- an override recorded once survives any
// number of later global flips until an agent-level write explicitly
// changes or clears it.
//
// This test used to assert the opposite for a second flip in the
// override's own direction, reasoning that the override was "normalized
// away" by the first flip since it happened to become equal to the new
// global value -- that was SPEC §4.1's other, now-rejected normalization
// rule. Under the decision this build implements, codex's override must
// stay in force across both flips below, moving only if a later
// agent-level SetAgentSwitch touches it directly.
func TestSetGlobalLeavesOverriddenAgentUndisturbed(t *testing.T) {
	s, _ := setupStore(t)
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}, fakeAgent{"codex", codexDir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil { // global on, both linked
		t.Fatal(err)
	}
	claudeLink := filepath.Join(claudeDir, "alpha")
	codexLink := filepath.Join(codexDir, "alpha")

	if _, err := SetAgentSwitch(s, agents, "alpha", "codex", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(codexLink); !os.IsNotExist(err) {
		t.Fatal("codex must be off via its override before the global flip")
	}

	if _, err := SetGlobal(s, agents, "alpha", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(claudeLink); !os.IsNotExist(err) {
		t.Fatal("claude (follow-global) must move to off")
	}
	if _, err := os.Lstat(codexLink); !os.IsNotExist(err) {
		t.Fatal("codex (already off via its override) must stay put, undisturbed by the global flip")
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cfg.Override("alpha", "codex"); !ok || v {
		t.Fatal("codex's override must survive a global flip that makes it equal to the new global value")
	}

	// A second flip, back to the value codex's override already opposes,
	// must carry claude (follow-global) back on while leaving codex exactly
	// where its override put it -- the override was never cleared, so it
	// is still in force.
	if _, err := SetGlobal(s, agents, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(claudeLink); err != nil {
		t.Fatal("claude (follow-global) must move back on")
	}
	if _, err := os.Lstat(codexLink); !os.IsNotExist(err) {
		t.Fatal("codex's override must still be in force after a second, unrelated global flip")
	}
}

// An unknown agent name is checked against the static registry
// (agent.ByName) before Run is invoked, so the pipeline never takes the
// lock or sweeps for a doomed call; that must hold observably. To prove
// the check happens before the lock (not inside Mutate), we dirty the
// store first: if the rejection were deferred into the pipeline, the dirty
// file would be swept into a commit before Mutate runs and fails.
func TestSetAgentSwitchUnknownAgentRejectedBeforeLock(t *testing.T) {
	s, _ := setupStore(t)
	agents := []agent.Agent{fakeAgent{"claude", t.TempDir()}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	// Dirty the store with a manual edit.
	if err := os.WriteFile(filepath.Join(s.Dir(), "manual.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reject the unknown agent before Run is invoked.
	if _, err := SetAgentSwitch(s, agents, "alpha", "gemini", true); err == nil {
		t.Fatal("unknown agent must be refused")
	}
	// No sweep or op commit must have happened: the dirty file is still
	// uncommitted, and the log still shows only init + the NewSkill commit.
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unknown agent must be rejected before the pipeline lock; no sweep must happen, got %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Message == "external: manual modifications" {
			t.Fatal("unknown agent must be rejected before sweep; no sweep commit must appear")
		}
	}
}

// TestSetGlobalAndAgentSwitch only exercises the unknown-skill guard
// through SetGlobal; SetAgentSwitch has its own identical guard
// (cfg.HasSkill inside Mutate) that needs its own coverage with a
// known agent, so a bug in one guard cannot hide behind the other's test.
func TestSetAgentSwitchUnknownSkillRefused(t *testing.T) {
	s, _ := setupStore(t)
	agents := []agent.Agent{fakeAgent{"claude", t.TempDir()}}
	if _, err := SetAgentSwitch(s, agents, "ghost", "claude", true); err == nil {
		t.Fatal("unknown skill must be refused even through the per-agent form")
	}
}

// Toggling a skill to the value it already has runs the full write
// pipeline -- Run/SetGlobal have no before/after diff of their own that
// would short-circuit a no-op mutation -- but produces no new commit
// anyway: cfg.Save() writes byte-identical YAML, so the worktree stays
// clean and Store.Commit's git.ErrEmptyCommit handling (already covered by
// TestCommitNoOpOnCleanWorktree in store) swallows it lower down. Recorded
// here because it is the opposite of what a reader of SetGlobal alone
// would expect, not because SetGlobal contains any dedup logic itself.
func TestSetGlobalRedundantToggleDoesNotCommit(t *testing.T) {
	s, _ := setupStore(t)
	agents := []agent.Agent{fakeAgent{"claude", t.TempDir()}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil { // global already true
		t.Fatal(err)
	}
	before, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetGlobal(s, agents, "alpha", true); err != nil { // redundant
		t.Fatal(err)
	}
	after, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a redundant toggle should leave the worktree clean and add no commit: before=%d after=%d", len(before), len(after))
	}
}

// TestNewSkillLeavesNothingInTheStoreWhenItFails is round 7's atomicity
// finding. NewSkill created its directory and wrote SKILL.md directly into
// the live store, before the config save and the commit. A failure at
// either of those left content in the store that fu.yaml did not know
// about, and this plan ships no `log`, `revert` or `restore` to clean it
// up: the next write swept the residue in as an "external modification",
// while retrying `fu new` refused outright ("store already holds content
// at ...") -- so the command could neither complete nor be repeated.
//
// DESIGN §6 already specifies the shape of the answer: prepare in staging,
// then publish into the store. Staging is a sibling of the repository under
// the same $FU_HOME, so the publish is a same-filesystem rename and cannot
// half-happen.
func TestNewSkillLeavesNothingInTheStoreWhenItFails(t *testing.T) {
	s, _ := setupStore(t)
	boom := errors.New("disk full")

	_, err := newSkill(s, nil, "alpha", hooks{afterMutate: func() error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatalf("want the injected failure propagated, got %v", err)
	}

	// Nothing unregistered may be left in the store.
	if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !os.IsNotExist(err) {
		t.Errorf("a failed `fu new` must leave no content in the store: %v", err)
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Error("a failed `fu new` must leave the store clean, not dirt for the next command to sweep")
	}

	// And the operation must be repeatable, which is the property the
	// leftover directory actually destroyed.
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("retrying after a failure must work: %v", err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") {
		t.Fatal("the retry must actually register the skill")
	}
	if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
		t.Fatalf("the retry must materialize the skill's content: %v", err)
	}
}

// The staging root's own provenance, one boundary earlier than the test below.
//
// Creating the root and recording what it holds used to be two steps addressed
// by pathname: exclusive Mkdir, then a snapshot that reopened the name and
// enumerated whatever was there. Nothing proved the enumerated directory was
// the one Mkdir created, and nothing required the first manifest to be empty,
// so a same-user writer arriving in that window could hand fu either a foreign
// root or a foreign descendant -- and fu would record it as transaction-owned,
// then validate, digest, publish and commit it as its own.
func TestNewSkillDoesNotAdoptForeignStagingRootsOrDescendants(t *testing.T) {
	// Deliberately sorts before the scaffold fu creates. A foreign name that
	// sorts after it is caught by accident, because appending the scaffold
	// entry to an already-populated manifest leaves it out of order and
	// OwnedTree.Validate refuses that; a name that sorts before it produces a
	// well-formed manifest and sails through to the commit.
	const foreignStagedName = "AAA-foreign.md"
	tests := []struct {
		name    string
		arrange func(t *testing.T, staged string) string
	}{
		{
			name: "replaced root",
			arrange: func(t *testing.T, staged string) string {
				t.Helper()
				if err := os.Remove(staged); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(staged, 0o755); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(staged, foreignStagedName)
			},
		},
		{
			name: "inserted descendant",
			arrange: func(t *testing.T, staged string) string {
				t.Helper()
				return filepath.Join(staged, foreignStagedName)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := setupStore(t)
			foreign := []byte("content fu never created")
			staged := filepath.Join(s.StagingDir(), "alpha")
			var foreignPath string
			h := hooks{afterStagingCreate: func() error {
				foreignPath = tt.arrange(t, staged)
				return os.WriteFile(foreignPath, foreign, 0o644)
			}}

			if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, ErrTxnConflict) {
				t.Fatalf("a staging root fu did not create must cause a safe conflict, got %v", err)
			}
			got, err := os.ReadFile(foreignPath)
			if err != nil {
				t.Fatalf("foreign staging content must survive: %v", err)
			}
			if !bytes.Equal(got, foreign) {
				t.Fatalf("foreign staging content changed: got %q want %q", got, foreign)
			}
			if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !os.IsNotExist(err) {
				t.Fatalf("foreign staging content must not be published: %v", err)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].Payload == nil {
				t.Fatalf("safe conflict must retain its ownership WAL, got %+v", pending)
			}
			if entries := pending[0].Payload.Entries; len(entries) != 0 {
				t.Fatalf("the first persisted manifest must claim the created root only, got %+v", entries)
			}
		})
	}
}

// An unrecorded private name has no ownership proof. Even emptiness does not
// identify its inode, so later operations preserve it.
func TestNewSkillPreservesUnrecordedPrivateStagingRoots(t *testing.T) {
	s, _ := setupStore(t)
	abandoned := filepath.Join(s.StagingDir(), ".fu-new-0123456789abcdef0123456789abcdef")
	if err := os.Mkdir(abandoned, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(abandoned)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(abandoned)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("unrecorded private staging root must be preserved: %v, %v", after, err)
	}
}

// Reclamation is rmdir and nothing else, so a private name that somehow holds
// content is preserved rather than cleared: fu never removes what it cannot
// prove it owns.
func TestNewSkillNeverClearsANonEmptyPrivateStagingRoot(t *testing.T) {
	s, _ := setupStore(t)
	occupied := filepath.Join(s.StagingDir(), ".fu-new-0123456789abcdef0123456789abcdef")
	if err := os.Mkdir(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("content fu cannot prove it owns")
	foreignPath := filepath.Join(occupied, "foreign.txt")
	if err := os.WriteFile(foreignPath, foreign, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatalf("occupied private staging root must survive: %v", err)
	}
	if !bytes.Equal(got, foreign) {
		t.Fatalf("occupied private staging root changed: got %q want %q", got, foreign)
	}
}

func TestNewSkillDoesNotAdoptDescendantsAddedAfterStagingOwnership(t *testing.T) {
	tests := []struct {
		name       string
		foreignRel string
	}{
		{name: "unexpected sibling", foreignRel: "foreign.txt"},
		{name: "claimed scaffold name", foreignRel: "SKILL.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := setupStore(t)
			foreign := []byte("externally introduced staging content")
			foreignPath := filepath.Join(s.StagingDir(), "alpha", tt.foreignRel)
			h := hooks{afterStagingOwnership: func() error {
				return os.WriteFile(foreignPath, foreign, 0o644)
			}}

			if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, ErrTxnConflict) {
				t.Fatalf("unexpected staged descendant must cause a safe conflict, got %v", err)
			}
			got, err := os.ReadFile(foreignPath)
			if err != nil {
				t.Fatalf("foreign staged content must survive: %v", err)
			}
			if !bytes.Equal(got, foreign) {
				t.Fatalf("foreign staged content changed: got %q want %q", got, foreign)
			}
			if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !os.IsNotExist(err) {
				t.Fatalf("conflicting staging content must not be published: %v", err)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].Payload == nil {
				t.Fatalf("safe conflict must retain its ownership WAL, got %+v", pending)
			}
			for _, entry := range pending[0].Payload.Entries {
				if entry.Path == tt.foreignRel && tt.foreignRel != "SKILL.md" {
					t.Fatalf("foreign descendant %q was absorbed into transaction authority", entry.Path)
				}
			}
		})
	}
}

func TestObservedNewRollbackPreservesSameContentReplacements(t *testing.T) {
	for _, phase := range []string{"staged", "published"} {
		t.Run(phase, func(t *testing.T) {
			s, _ := setupStore(t)
			var target string
			switch phase {
			case "staged":
				target = filepath.Join(s.StagingDir(), "alpha")
			case "published":
				target = filepath.Join(s.SkillsDir(), "alpha")
			default:
				t.Fatalf("unknown phase %q", phase)
			}
			var replacementBytes []byte
			replace := func() error {
				var err error
				replacementBytes, err = os.ReadFile(filepath.Join(target, "SKILL.md"))
				if err != nil {
					return err
				}
				if err := os.Rename(target, target+"-owned"); err != nil {
					return err
				}
				if err := os.Mkdir(target, 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(target, "SKILL.md"), replacementBytes, 0o644)
			}
			boom := errors.New("stop after replacement")
			var h hooks
			if phase == "staged" {
				h.afterMutate = func() error {
					if err := replace(); err != nil {
						t.Fatal(err)
					}
					return boom
				}
			} else {
				h.afterPublish = func() error {
					if err := replace(); err != nil {
						t.Fatal(err)
					}
					return boom
				}
			}

			if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, ErrTxnConflict) {
				t.Fatalf("same-content %s replacement must stop rollback with ErrTxnConflict, got %v", phase, err)
			}
			got, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
			if err != nil {
				t.Fatalf("same-content %s replacement must survive: %v", phase, err)
			}
			if !bytes.Equal(got, replacementBytes) {
				t.Fatalf("same-content %s replacement changed: got %q want %q", phase, got, replacementBytes)
			}
			if pending, err := PendingTxns(s); err != nil {
				t.Fatal(err)
			} else if len(pending) != 1 || pending[0].Payload == nil {
				t.Fatalf("safe conflict must retain the ownership WAL, got %+v", pending)
			}
		})
	}
}

func TestNewSkillRejectsLateExternalChanges(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*testing.T, *store.Store)
		change     func(*testing.T, *store.Store)
		assertKept func(*testing.T, *store.Store)
		sameSkill  bool
	}{
		{
			name: "tracked unrelated modification",
			prepare: func(t *testing.T, s *store.Store) {
				path := filepath.Join(s.Dir(), "external.txt")
				if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Commit("test: tracked late-change fixture"); err != nil {
					t.Fatal(err)
				}
			},
			change: func(t *testing.T, s *store.Store) {
				if err := os.WriteFile(filepath.Join(s.Dir(), "external.txt"), []byte("late tracked"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			assertKept: func(t *testing.T, s *store.Store) {
				got, err := os.ReadFile(filepath.Join(s.Dir(), "external.txt"))
				if err != nil || string(got) != "late tracked" {
					t.Fatalf("late tracked edit changed: got %q err=%v", got, err)
				}
			},
		},
		{
			name: "ignored unrelated addition",
			prepare: func(t *testing.T, s *store.Store) {
				if err := os.WriteFile(filepath.Join(s.Dir(), ".gitignore"), []byte("late-ignored.txt\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Commit("test: ignored late-change fixture"); err != nil {
					t.Fatal(err)
				}
			},
			change: func(t *testing.T, s *store.Store) {
				if err := os.WriteFile(filepath.Join(s.Dir(), "late-ignored.txt"), []byte("late ignored"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			assertKept: func(t *testing.T, s *store.Store) {
				got, err := os.ReadFile(filepath.Join(s.Dir(), "late-ignored.txt"))
				if err != nil || string(got) != "late ignored" {
					t.Fatalf("late ignored addition changed: got %q err=%v", got, err)
				}
			},
		},
		{
			name: "tracked unrelated deletion",
			prepare: func(t *testing.T, s *store.Store) {
				path := filepath.Join(s.Dir(), "delete-me.txt")
				if err := os.WriteFile(path, []byte("tracked"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Commit("test: deletion late-change fixture"); err != nil {
					t.Fatal(err)
				}
			},
			change: func(t *testing.T, s *store.Store) {
				if err := os.Remove(filepath.Join(s.Dir(), "delete-me.txt")); err != nil {
					t.Fatal(err)
				}
			},
			assertKept: func(t *testing.T, s *store.Store) {
				if _, err := os.Lstat(filepath.Join(s.Dir(), "delete-me.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("late deletion must remain deleted, got %v", err)
				}
			},
		},
		{
			name:    "same skill modification",
			prepare: func(*testing.T, *store.Store) {},
			change: func(t *testing.T, s *store.Store) {
				if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("late same-skill edit"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			assertKept: func(t *testing.T, s *store.Store) {
				got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"))
				if err != nil || string(got) != "late same-skill edit" {
					t.Fatalf("late same-skill edit changed: got %q err=%v", got, err)
				}
			},
			sameSkill: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := setupStore(t)
			tt.prepare(t, s)
			before, err := s.Repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			_, err = newSkill(s, nil, "alpha", hooks{afterPublish: func() error {
				tt.change(t, s)
				return nil
			}})
			if err == nil {
				t.Fatal("late external store change must stop the operation commit")
			}
			after, headErr := s.Repo.Head()
			if headErr != nil {
				t.Fatal(headErr)
			}
			if after.Hash() != before.Hash() {
				t.Fatalf("late external change must not be attributed to new: before=%s after=%s", before.Hash(), after.Hash())
			}
			tt.assertKept(t, s)
			pending, pendingErr := PendingTxns(s)
			if pendingErr != nil {
				t.Fatal(pendingErr)
			}
			if tt.sameSkill {
				if !errors.Is(err, ErrTxnConflict) {
					t.Fatalf("same-skill mutation must stop at a safe ownership conflict, got %v", err)
				}
				if len(pending) != 1 {
					t.Fatalf("same-skill ownership conflict must retain the WAL, got %+v", pending)
				}
				return
			}
			if len(pending) != 0 {
				t.Fatalf("unrelated change must allow transaction rollback and WAL clearing, got %+v", pending)
			}
			cfg, cfgErr := store.LoadConfig(s.ConfigPath())
			if cfgErr != nil {
				t.Fatal(cfgErr)
			}
			if cfg.HasSkill("alpha") {
				t.Fatal("rolled-back new must not remain in config")
			}
			if _, statErr := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rolled-back new content must be absent, got %v", statErr)
			}
			if sweepErr := s.Sweep(); sweepErr != nil {
				t.Fatal(sweepErr)
			}
			log, logErr := s.Log(1)
			if logErr != nil {
				t.Fatal(logErr)
			}
			if len(log) != 1 || log[0].Message != "external: manual modifications" {
				t.Fatalf("preserved dirt must receive its own external commit, got %+v", log)
			}
			if _, retryErr := NewSkill(s, nil, "alpha"); retryErr != nil {
				t.Fatalf("operation must be retryable after the external sweep: %v", retryErr)
			}
		})
	}
}

func TestWriteCommandRefusesFIFOWithoutBlockingAndReleasesLock(t *testing.T) {
	s, _ := setupStore(t)
	fifo := filepath.Join(s.Dir(), "blocking.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := NewSkill(s, nil, "alpha")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "blocking.fifo") {
			t.Fatalf("FIFO must fail promptly with a path-wrapped unsupported-type error, got %v", err)
		}
	case <-time.After(time.Second):
		// Pair any blocking FIFO readers repeatedly so the pre-fix command can
		// unwind and the RED test never strands its goroutine or store lock.
		stopRescue := make(chan struct{})
		rescueDone := make(chan struct{})
		go func() {
			defer close(rescueDone)
			for {
				select {
				case <-stopRescue:
					return
				default:
				}
				fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				if err == nil {
					_ = unix.Close(fd)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		select {
		case <-result:
		case <-time.After(3 * time.Second):
		}
		close(stopRescue)
		<-rescueDone
		t.Fatal("write command blocked while inspecting a FIFO")
	}

	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("a following write must acquire the released lock and succeed: %v", err)
	}
}

func TestWriteCommandRefusesConfigFIFOWithoutBlockingAndReleasesLock(t *testing.T) {
	s, _ := setupStore(t)
	configPath := s.ConfigPath()
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(configPath, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := NewSkill(s, nil, "alpha")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "fu.yaml") {
			t.Fatalf("config FIFO must fail promptly with a path-wrapped unsupported-type error, got %v", err)
		}
	case <-time.After(time.Second):
		fd, openErr := unix.Open(configPath, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if openErr == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-result:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("write command blocked while loading a FIFO config")
	}

	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("a following write must acquire the released lock and succeed: %v", err)
	}
}

func TestWriteCommandRefusesGitIndexFIFOWithoutBlockingAndReleasesLock(t *testing.T) {
	s, _ := setupStore(t)
	indexPath := filepath.Join(s.Dir(), ".git", "index")
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(indexPath, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := NewSkill(s, nil, "alpha")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "index") {
			t.Fatalf("Git index FIFO must fail promptly with a path-wrapped unsupported-type error, got %v", err)
		}
	case <-time.After(time.Second):
		fd, openErr := unix.Open(indexPath, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if openErr == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-result:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("write command blocked while go-git opened a FIFO index")
	}

	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, indexBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("a following write must acquire the released lock and succeed: %v", err)
	}
}

func TestWriteCommandRefusesGitHEADSymlinkReplacement(t *testing.T) {
	s, _ := setupStore(t)
	gitDir := filepath.Join(s.Dir(), ".git")
	headPath := filepath.Join(gitDir, "HEAD")
	headBytes, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatal(err)
	}
	const decoyName = "HEAD.fu-test-decoy"
	if err := os.WriteFile(filepath.Join(gitDir, decoyName), headBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(headPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoyName, headPath); err != nil {
		t.Fatal(err)
	}

	_, opErr := NewSkill(s, nil, "alpha")
	if err := os.Remove(headPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headPath, headBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if opErr == nil || !strings.Contains(opErr.Error(), "HEAD") {
		t.Fatalf("write command must refuse a Git HEAD symlink introduced after Store.Open, got %v", opErr)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("a following write with the real HEAD restored must succeed: %v", err)
	}
}

func TestWriteCommandRefusesWALFIFOWithoutBlockingAndReleasesLock(t *testing.T) {
	s, _ := setupStore(t)
	name := txnRecordName(TxnRecord{
		Op:       "new",
		TxnID:    "00000000000000000000000000000001",
		Sequence: 1,
	})
	fifo := filepath.Join(s.RecoveryDir(), name)
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := NewSkill(s, nil, "alpha")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), name) {
			t.Fatalf("WAL FIFO must fail promptly with a path-wrapped unsupported-type error, got %v", err)
		}
	case <-time.After(time.Second):
		// Pair a pre-fix blocking reader so the test can unwind without
		// stranding the goroutine or the process-wide write lock.
		stopRescue := make(chan struct{})
		rescueDone := make(chan struct{})
		go func() {
			defer close(rescueDone)
			for {
				select {
				case <-stopRescue:
					return
				default:
				}
				fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				if err == nil {
					_ = unix.Close(fd)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		select {
		case <-result:
		case <-time.After(3 * time.Second):
		}
		close(stopRescue)
		<-rescueDone
		t.Fatal("write command blocked while reading a FIFO transaction record")
	}

	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("a following write must acquire the released lock and succeed: %v", err)
	}
}

// TestNewSkillNeverFollowsIndirectionAtMachineLocalRoots is round 8's first
// Critical, and a regression introduced by round 7's own staging fix. That
// fix moved scaffolding into $FU_HOME/staging and cleared any leftover
// there with os.RemoveAll before building -- without asking whether
// "staging" was still a directory fu created. os.MkdirAll accepts an
// existing symlink to a directory, so a redirected staging root made
// `fu new alpha` recursively delete <target>/alpha: content outside the
// store entirely, that fu never created.
//
// Reproduced against the compiled binary before the fix: with
// ~/precious/alpha/notes.md present and staging -> ~/precious, `fu new
// alpha` printed "created alpha" and notes.md was gone.
func TestNewSkillNeverFollowsIndirectionAtMachineLocalRoots(t *testing.T) {
	for _, root := range []string{"staging", "recovery"} {
		t.Run(root+" is a symlink", func(t *testing.T) {
			home := t.TempDir()
			if _, err := store.Init(home); err != nil {
				t.Fatal(err)
			}
			// The user's own content, at a name the operation will match.
			precious := filepath.Join(t.TempDir(), "precious")
			if err := os.MkdirAll(filepath.Join(precious, "alpha"), 0o755); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(precious, "alpha", "notes.md")
			if err := os.WriteFile(keep, []byte("irreplaceable"), 0o644); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(home, root)
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(precious, target); err != nil {
				t.Fatal(err)
			}

			// Whether this refuses or proceeds, it must not reach through the
			// link. Re-opening is what a real command does, and is where the
			// refusal belongs.
			if reopened, err := store.Open(home); err == nil {
				_, _ = NewSkill(reopened, nil, "alpha")
			}

			if got, err := os.ReadFile(keep); err != nil || string(got) != "irreplaceable" {
				t.Fatalf("content outside the store must never be touched, however %s is spelled: %q %v",
					root, got, err)
			}
		})
	}
}

// Store.Open validates one FU_HOME layout. NewSkill must keep addressing that
// identity even if the pathname is later replaced with a second, valid-looking
// store. Otherwise staging cleanup, publish, config/Git writes, or rollback can
// mutate the replacement despite it never having passed Store.Open.
func TestNewSkillUsesTheStoreIdentityThatOpenValidated(t *testing.T) {
	type fixture struct {
		store       *store.Store
		home        string
		moved       string
		decoy       string
		preciousRel string
		decoyConfig []byte
		decoyHead   string
	}
	setup := func(t *testing.T, preciousRel string) fixture {
		t.Helper()
		base := t.TempDir()
		home := filepath.Join(base, "home")
		if _, err := store.Init(home); err != nil {
			t.Fatal(err)
		}
		opened, err := store.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		decoy := filepath.Join(base, "decoy")
		if _, err := store.Init(decoy); err != nil {
			t.Fatal(err)
		}
		decoyStore, err := store.Open(decoy)
		if err != nil {
			t.Fatal(err)
		}
		decoyConfig, err := os.ReadFile(decoyStore.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		decoyHead, err := decoyStore.Repo.Head()
		if err != nil {
			t.Fatal(err)
		}
		precious := filepath.Join(decoy, preciousRel)
		if err := os.MkdirAll(filepath.Dir(precious), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(precious, []byte("irreplaceable"), 0o644); err != nil {
			t.Fatal(err)
		}
		return fixture{
			store:       opened,
			home:        home,
			moved:       filepath.Join(base, "opened-store-moved"),
			decoy:       decoy,
			preciousRel: preciousRel,
			decoyConfig: decoyConfig,
			decoyHead:   decoyHead.Hash().String(),
		}
	}
	swap := func(t *testing.T, f fixture) {
		t.Helper()
		if err := os.Rename(f.home, f.moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.decoy, f.home); err != nil {
			t.Fatal(err)
		}
	}
	assertPrecious := func(t *testing.T, f fixture) {
		t.Helper()
		path := filepath.Join(f.home, f.preciousRel)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "irreplaceable" {
			t.Fatalf("replacement-store content must remain untouched at %s: %q %v", path, got, err)
		}
		config, err := os.ReadFile(filepath.Join(f.home, "store", "fu.yaml"))
		if err != nil || string(config) != string(f.decoyConfig) {
			t.Fatalf("replacement-store config changed: %q %v", config, err)
		}
		decoyStore, err := store.Open(f.home)
		if err != nil {
			t.Fatalf("replacement store no longer opens: %v", err)
		}
		head, err := decoyStore.Repo.Head()
		if err != nil || head.Hash().String() != f.decoyHead {
			t.Fatalf("replacement-store history changed: %v %v", head, err)
		}
	}

	t.Run("replacement before the write is refused", func(t *testing.T) {
		f := setup(t, filepath.Join("staging", "alpha", "notes.md"))
		swap(t, f)
		if _, err := NewSkill(f.store, nil, "alpha"); err == nil {
			t.Fatal("a write must refuse FU_HOME when it no longer names the layout Store.Open validated")
		}
		assertPrecious(t, f)
	})

	t.Run("publish keeps using the checked identity", func(t *testing.T) {
		f := setup(t, filepath.Join("staging", "alpha", "notes.md"))
		h := hooks{afterMutate: func() error {
			swap(t, f)
			return nil
		}}
		if _, err := newSkill(f.store, nil, "alpha", h); err == nil {
			t.Fatal("replacement during a write must be reported before the command claims success")
		}
		assertPrecious(t, f)
	})

	t.Run("rollback keeps using the checked identity", func(t *testing.T) {
		f := setup(t, filepath.Join("store", "skills", "alpha", "notes.md"))
		boom := errors.New("injected failure after publish")
		h := hooks{afterPublish: func() error {
			swap(t, f)
			return boom
		}}
		if _, err := newSkill(f.store, nil, "alpha", h); !errors.Is(err, boom) {
			t.Fatalf("want injected failure, got %v", err)
		}
		assertPrecious(t, f)
	})
}

func TestNewSkillKeepsEveryLogicalRootPinnedDuringWrite(t *testing.T) {
	openStore := func(t *testing.T) *store.Store {
		t.Helper()
		home := t.TempDir()
		if _, err := store.Init(home); err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	swap := func(t *testing.T, path, original, decoy string) {
		t.Helper()
		if err := os.Rename(path, original); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(filepath.Dir(path), decoy)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.IsAbs(rel) {
			t.Fatalf("setup: replacement target must stay relative, got %q", rel)
		}
		if err := os.Symlink(rel, path); err != nil {
			t.Fatal(err)
		}
	}
	assertFile := func(t *testing.T, path string, want []byte) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(want) {
			t.Fatalf("decoy content changed at %s: got %q, want %q, err=%v", path, got, want, err)
		}
	}

	t.Run("staging cleanup", func(t *testing.T) {
		s := openStore(t)
		decoy := filepath.Join(s.Home, "decoy-staging")
		keep := filepath.Join(decoy, "alpha", "notes.md")
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			t.Fatal(err)
		}
		want := []byte("staging sentinel")
		if err := os.WriteFile(keep, want, 0o644); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("stop after replacing staging")
		h := hooks{afterMutate: func() error {
			swap(t, s.StagingDir(), filepath.Join(s.Home, "pinned-staging"), decoy)
			return boom
		}}
		if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, boom) {
			t.Fatalf("want injected failure, got %v", err)
		}
		assertFile(t, keep, want)
	})

	t.Run("recovery WAL", func(t *testing.T) {
		s := openStore(t)
		decoy := filepath.Join(s.Home, "decoy-recovery")
		if err := os.Mkdir(decoy, 0o755); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(decoy, "txn-new.json")
		want := []byte("recovery sentinel")
		if err := os.WriteFile(keep, want, 0o644); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("stop after replacing recovery")
		h := hooks{afterMutate: func() error {
			swap(t, s.RecoveryDir(), filepath.Join(s.Home, "pinned-recovery"), decoy)
			return boom
		}}
		if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, boom) {
			t.Fatalf("want injected failure, got %v", err)
		}
		assertFile(t, keep, want)
	})

	t.Run("store config", func(t *testing.T) {
		s := openStore(t)
		decoy := filepath.Join(s.Home, "decoy-store")
		if err := os.Mkdir(decoy, 0o755); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(decoy, "fu.yaml")
		want := []byte("store sentinel")
		if err := os.WriteFile(keep, want, 0o644); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("stop after replacing store")
		h := hooks{afterMutate: func() error {
			swap(t, s.Dir(), filepath.Join(s.Home, "pinned-store"), decoy)
			return boom
		}}
		if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, boom) {
			t.Fatalf("want injected failure, got %v", err)
		}
		assertFile(t, keep, want)
	})

	t.Run("skills publication", func(t *testing.T) {
		s := openStore(t)
		decoy := filepath.Join(s.Home, "decoy-skills")
		if err := os.Mkdir(decoy, 0o755); err != nil {
			t.Fatal(err)
		}
		redirected := false
		boom := errors.New("stop after publication")
		h := hooks{
			afterMutate: func() error {
				swap(t, s.SkillsDir(), filepath.Join(s.Dir(), "pinned-skills"), decoy)
				return nil
			},
			afterPublish: func() error {
				_, err := os.Stat(filepath.Join(decoy, "alpha", "SKILL.md"))
				redirected = err == nil
				return boom
			},
		}
		if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, boom) {
			t.Fatalf("want injected failure, got %v", err)
		}
		if redirected {
			t.Fatal("publication reached the replacement skills directory")
		}
		ents, err := os.ReadDir(decoy)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("replacement skills directory was mutated: %v", ents)
		}
	})

	t.Run("git metadata", func(t *testing.T) {
		s := openStore(t)
		decoyWorktree := filepath.Join(s.Home, "decoy-repository")
		decoyRepo, err := git.PlainClone(decoyWorktree, false, &git.CloneOptions{URL: s.Dir()})
		if err != nil {
			t.Fatal(err)
		}
		before, err := decoyRepo.Head()
		if err != nil {
			t.Fatal(err)
		}
		h := hooks{afterMutate: func() error {
			swap(t, filepath.Join(s.Dir(), git.GitDirName), filepath.Join(s.Home, "pinned-git"), filepath.Join(decoyWorktree, git.GitDirName))
			return nil
		}}
		_, _ = newSkill(s, nil, "alpha", h)
		after, err := decoyRepo.Head()
		if err != nil {
			t.Fatal(err)
		}
		if after.Hash() != before.Hash() {
			t.Fatalf("replacement Git history changed from %s to %s", before.Hash(), after.Hash())
		}
	})
}

// TestNewSkillRollsBackAtEveryDurableBoundary is round 8's recoverability
// finding. The pipeline saves fu.yaml before publishing and committing, so
// a failure after that point left the store durably changed in a way the
// command reported as a failure: a registered-but-absent skill (publish
// failed), or content and config with no commit (commit failed). Retrying
// hit "already exists" instead of completing or undoing the interrupted
// operation, so `fu new` was neither recoverable nor safely repeatable.
//
// Round 7's injection stopped before cfg.Save and so proved nothing about
// any of these. Each boundary below is exercised separately, and each must
// leave the store exactly as it was and let the same command succeed on a
// second attempt.
func TestNewSkillRollsBackAtEveryDurableBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage failStage
	}{
		{"after the config is saved", failAfterSave},
		{"while publishing", failDuringPublish},
		{"after publishing, while committing", failDuringCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupStore(t)
			headBefore, err := s.Log(10)
			if err != nil {
				t.Fatal(err)
			}
			cfgBefore, err := os.ReadFile(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			boom := errors.New("injected failure")

			_, err = newSkillFailingAt(s, nil, "alpha", tc.stage, boom)
			if !errors.Is(err, boom) {
				t.Fatalf("want the injected failure propagated, got %v", err)
			}

			// Config restored byte for byte.
			cfgAfter, err := os.ReadFile(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if string(cfgAfter) != string(cfgBefore) {
				t.Errorf("a failed command must leave fu.yaml as it found it:\n before %s\n after  %s",
					cfgBefore, cfgAfter)
			}
			// No content left in the store.
			if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !os.IsNotExist(err) {
				t.Errorf("a failed command must leave no content in the store: %v", err)
			}
			// No commit.
			headAfter, err := s.Log(10)
			if err != nil {
				t.Fatal(err)
			}
			if len(headAfter) != len(headBefore) {
				t.Errorf("a failed command must not commit: before=%d after=%d", len(headBefore), len(headAfter))
			}
			// And the store is left clean, not dirt for the next command.
			dirty, err := s.IsDirty()
			if err != nil {
				t.Fatal(err)
			}
			if dirty {
				t.Error("a failed command must not leave the store dirty")
			}

			// The point of all of the above: the operation is repeatable.
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatalf("retrying the same command after a failure must work: %v", err)
			}
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.HasSkill("alpha") {
				t.Error("the retry must register the skill")
			}
			if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
				t.Errorf("the retry must materialize the content: %v", err)
			}
		})
	}
}

// Commit can write an object and move the branch before a later verification
// reports an error. That outcome must preserve the committed snapshot and its
// WAL for recovery; rolling the worktree back would make HEAD and disk diverge.
func TestNewSkillDoesNotRollbackACommitThatWasAlreadyWritten(t *testing.T) {
	s, _ := setupStore(t)
	verificationErr := errors.New("injected post-write verification failure")
	h := hooks{commit: func(st *store.Store, message string, prepared store.PreparedCommit) (store.CommitOutcome, error) {
		outcome, err := st.CommitPrepared(message, prepared)
		if err != nil {
			return outcome, err
		}
		return outcome, verificationErr
	}}

	if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, verificationErr) {
		t.Fatalf("want post-write verification error, got %v", err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") {
		t.Fatal("a written commit's config must not be rolled back")
	}
	if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
		t.Fatalf("a written commit's published content must not be removed: %v", err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if commit.Message != "new: alpha" {
		t.Fatalf("HEAD must retain the written operation commit, got %q", commit.Message)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Op != "new" {
		t.Fatalf("ambiguous written outcome must retain its recovery record, got %+v", pending)
	}
}

func TestCommittedNewRecoveryResumesAfterConfigWasAlreadyRestored(t *testing.T) {
	s, _ := setupStore(t)
	interrupted := errors.New("stop after operation commit")
	if _, err := newSkill(s, nil, "alpha", hooks{
		afterCommit: func() error { return interrupted },
	}); !errors.Is(err, interrupted) {
		t.Fatalf("setup must leave a committed transaction pending, got %v", err)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("setup must leave one transaction record, got %+v", pending)
	}
	if err := store.WriteFileAtomic(s.ConfigPath(), pending[0].ConfigBefore, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("recovery must accept its already-restored config and let the retry proceed: %v", err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") {
		t.Fatal("retry must register alpha after recovery converges")
	}
	if pending, err := PendingTxns(s); err != nil {
		t.Fatal(err)
	} else if len(pending) != 0 {
		t.Fatalf("successful recovery and retry must clear the WAL, got %+v", pending)
	}
}

func TestCommittedNewRecoveryRejectsUnexpectedOperationTree(t *testing.T) {
	s, _ := setupStore(t)
	interrupted := errors.New("stop after operation commit")
	if _, err := newSkill(s, nil, "alpha", hooks{
		afterCommit: func() error { return interrupted },
	}); !errors.Is(err, interrupted) {
		t.Fatalf("setup must leave a committed transaction pending, got %v", err)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].CommitTree == "" {
		t.Fatalf("setup must persist one prepared operation tree, got %+v", pending)
	}
	operationHead, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	start := plumbing.NewHash(pending[0].StartHead)
	if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(operationHead.Name(), start)); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(s.Dir(), "unexpected.txt")
	if err := os.WriteFile(foreign, []byte("not part of new alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	spoof, err := s.Commit(pending[0].Message)
	if err != nil {
		t.Fatal(err)
	}
	spoofCommit, err := s.Repo.CommitObject(spoof.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(spoofCommit.ParentHashes) != 1 || spoofCommit.ParentHashes[0] != start || spoofCommit.Message != pending[0].Message {
		t.Fatalf("test setup must preserve the reviewed parent/message checks, got %+v", spoofCommit)
	}

	if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("same-parent/same-message commit with an unexpected tree must conflict, got %v", err)
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "not part of new alpha" {
		t.Fatalf("unexpected committed content must remain untouched: got %q err=%v", got, err)
	}
	if pending, err := PendingTxns(s); err != nil {
		t.Fatal(err)
	} else if len(pending) != 1 {
		t.Fatalf("tree mismatch must retain the operation WAL, got %+v", pending)
	}
	log, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range log {
		if strings.HasPrefix(entry.Message, "recover: roll back interrupted") {
			t.Fatalf("tree mismatch must be rejected before compensation, got %+v", log)
		}
	}
}

// Returned errors exercise rollback, not process interruption. These child
// processes exit at durable boundaries so no defer or rollback can run; the
// next invocation must recover the WAL and make retrying the same command
// succeed without sweeping the interrupted state as an external edit.
func TestNewSkillRecoversAfterProcessInterruption(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_NEW_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_NEW_HOME")
		stage := os.Getenv("FU_TEST_CRASH_NEW_STAGE")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		var h hooks
		switch stage {
		case "after-start":
			h.afterTxnStart = crash
		case "after-staging-ownership":
			h.afterStagingOwnership = crash
		case "after-save":
			h.afterSave = crash
		case "after-publish":
			h.afterPublish = crash
		case "after-commit":
			h.afterCommit = crash
		default:
			panic("unknown crash stage " + stage)
		}
		_, _ = newSkill(s, nil, "alpha", h)
		panic("crash hook did not run")
	}

	for _, stage := range []string{"after-start", "after-staging-ownership", "after-save", "after-publish", "after-commit"} {
		t.Run(stage, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			if _, err := store.Init(home); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestNewSkillRecoversAfterProcessInterruption$")
			cmd.Env = append(os.Environ(),
				"FU_TEST_CRASH_NEW_HELPER=1",
				"FU_TEST_CRASH_NEW_HOME="+home,
				"FU_TEST_CRASH_NEW_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child must terminate at %s with code 86, err=%v output=%s", stage, err, output)
			}

			s, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatalf("retry after %s must recover and succeed: %v", stage, err)
			}
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.HasSkill("alpha") {
				t.Fatal("successful retry must register alpha")
			}
			if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
				t.Fatalf("successful retry must publish alpha: %v", err)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("successful recovery must clear its WAL, got %+v", pending)
			}
			entries, err := s.Log(10)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Message == "external: manual modifications" {
					t.Fatalf("interrupted NewSkill state must not be swept as external work: %+v", entries)
				}
			}
		})
	}
}

func TestUncommittedInstallRecoveryRevalidatesTerminalArchiveAfterProcessInterruption(t *testing.T) {
	if mode := os.Getenv("FU_TEST_CRASH_UNCOMMITTED_ARCHIVE_HELPER"); mode != "" {
		home := os.Getenv("FU_TEST_CRASH_UNCOMMITTED_ARCHIVE_HOME")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		switch mode {
		case "publish":
			_, _ = newSkill(s, nil, "alpha", hooks{
				afterPublish: func() error {
					os.Exit(86)
					return nil
				},
			})
		case "archive":
			session, err := s.BeginWrite()
			if err != nil {
				panic(err)
			}
			records, err := PendingTxns(session.Store)
			if err != nil {
				panic(err)
			}
			if len(records) != 1 || records[0].Payload == nil {
				panic(fmt.Sprintf("expected one manifested pending transaction, got %+v", records))
			}
			record := records[0]
			payload := installUncommittedPayloadName(record)
			if err := session.Store.QuarantineSkillOwned(record.Name, payload, *record.Payload); err != nil {
				panic(err)
			}
			if err := session.Store.ArchiveRecoveryPayloadOwned(payload, *record.Payload); err != nil {
				panic(err)
			}
			os.Exit(86)
		default:
			panic("unknown helper mode " + mode)
		}
		panic("process interruption hook did not run")
	}

	home := filepath.Join(t.TempDir(), "home")
	if _, err := store.Init(home); err != nil {
		t.Fatal(err)
	}
	runInterrupted := func(mode string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUncommittedInstallRecoveryRevalidatesTerminalArchiveAfterProcessInterruption$")
		cmd.Env = append(os.Environ(),
			"FU_TEST_CRASH_UNCOMMITTED_ARCHIVE_HELPER="+mode,
			"FU_TEST_CRASH_UNCOMMITTED_ARCHIVE_HOME="+home,
		)
		output, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
			t.Fatalf("child must terminate after %s with code 86, err=%v output=%s", mode, err, output)
		}
	}
	runInterrupted("publish")
	runInterrupted("archive")

	recoveryEntries, err := os.ReadDir(filepath.Join(home, "recovery"))
	if err != nil {
		t.Fatal(err)
	}
	archive := ""
	for _, entry := range recoveryEntries {
		if !strings.HasPrefix(entry.Name(), ".fu-archive-") {
			continue
		}
		if archive != "" {
			t.Fatalf("expected one terminal archive, got %+v", recoveryEntries)
		}
		archive = entry.Name()
	}
	if archive == "" {
		t.Fatalf("expected a terminal archive after interrupted recovery, got %+v", recoveryEntries)
	}
	archiveFile := filepath.Join(home, "recovery", archive, "SKILL.md")
	changed := []byte("changed after terminal archive")
	if err := os.WriteFile(archiveFile, changed, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("changed terminal archive must stop recovery with ErrTxnConflict, got %v", err)
	}
	got, err := os.ReadFile(archiveFile)
	if err != nil {
		t.Fatalf("changed terminal archive must be preserved: %v", err)
	}
	if !bytes.Equal(got, changed) {
		t.Fatalf("changed terminal archive = %q, want %q", got, changed)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Name != "alpha" {
		t.Fatalf("archive conflict must retain the original WAL, got %+v", pending)
	}
}

func TestStartedNewRecoveryPreservesManifestlessStagingReplacement(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_NEW_STAGING_CREATE_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_NEW_STAGING_CREATE_HOME")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		_, _ = newSkill(s, nil, "alpha", hooks{
			afterTxnStart: func() error {
				os.Exit(86)
				return nil
			},
		})
		panic("staging-create crash hook did not run")
	}

	home := filepath.Join(t.TempDir(), "home")
	if _, err := store.Init(home); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartedNewRecoveryPreservesManifestlessStagingReplacement$")
	cmd.Env = append(os.Environ(),
		"FU_TEST_CRASH_NEW_STAGING_CREATE_HELPER=1",
		"FU_TEST_CRASH_NEW_STAGING_CREATE_HOME="+home,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must stop after staging creation with code 86, err=%v output=%s", err, output)
	}

	staged := filepath.Join(home, "staging", "alpha")
	if err := os.Mkdir(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Lstat(staged)
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("manifest-less staging replacement must stop recovery with ErrTxnConflict, got %v", err)
	}
	current, err := os.Lstat(staged)
	if err != nil || !os.SameFile(current, replacement) {
		t.Fatalf("manifest-less empty replacement must survive: %v, %v", current, err)
	}
	if pending, err := PendingTxns(s); err != nil {
		t.Fatal(err)
	} else if len(pending) != 1 || pending[0].Stage != "started" || pending[0].Payload != nil {
		t.Fatalf("safe conflict must retain the manifest-less started WAL, got %+v", pending)
	}
}

func TestCommittedNewRecoverySurvivesProcessInterruption(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_NEW_RECOVERY_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_NEW_RECOVERY_HOME")
		stage := os.Getenv("FU_TEST_CRASH_NEW_RECOVERY_STAGE")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func(*store.Store, TxnRecord) error {
			os.Exit(86)
			return nil
		}
		var h installRecoveryHooks
		switch stage {
		case "after-compensation-started":
			h.afterCompensationStarted = crash
		case "after-config-restore":
			h.afterConfigRestore = crash
		case "after-quarantine":
			h.afterQuarantine = crash
		case "before-quarantine-cleanup":
			// Cleanup is a single rename, so the only state an interruption
			// can leave here is the intact quarantined payload. A payload that
			// lost a recorded entry is not an interrupted cleanup and must not
			// converge as one; that case is
			// TestCommittedNewRecoveryPreservesChangedQuarantineContent.
			h.beforeQuarantineCleanup = crash
		case "after-compensation-commit":
			h.afterCompensationCommit = crash
		case "before-wal-clear":
			h.beforeWALClear = crash
		default:
			panic("unknown recovery crash stage " + stage)
		}
		RegisterRecoverHandler("new", func(st *store.Store, record TxnRecord) error {
			return recoverInstallSkillWithHooks(st, record, h)
		})
		_, _ = NewSkill(s, nil, "beta")
		panic("recovery crash hook did not run")
	}

	stages := []string{
		"after-compensation-started",
		"after-config-restore",
		"after-quarantine",
		"before-quarantine-cleanup",
		"after-compensation-commit",
		"before-wal-clear",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			leaveCommittedNewPending(t, home, "alpha")

			cmd := exec.Command(os.Args[0], "-test.run=^TestCommittedNewRecoverySurvivesProcessInterruption$")
			cmd.Env = append(os.Environ(),
				"FU_TEST_CRASH_NEW_RECOVERY_HELPER=1",
				"FU_TEST_CRASH_NEW_RECOVERY_HOME="+home,
				"FU_TEST_CRASH_NEW_RECOVERY_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child must terminate during recovery at %s with code 86, err=%v output=%s", stage, err, output)
			}

			s, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatalf("recovery interrupted at %s must converge and let the original command retry: %v", stage, err)
			}
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.HasSkill("alpha") || cfg.HasSkill("beta") {
				t.Fatalf("only the retried operation may remain in config, got skills %v", cfg.SkillNames())
			}
			if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
				t.Fatalf("retried operation must publish alpha: %v", err)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("converged recovery must clear its WAL, got %+v", pending)
			}
			residue, err := os.ReadDir(s.RecoveryDir())
			if err != nil {
				t.Fatal(err)
			}
			var archives []os.DirEntry
			for _, entry := range residue {
				if strings.HasPrefix(entry.Name(), ".fu-archive-") {
					archives = append(archives, entry)
				}
			}
			if len(archives) != 1 || !archives[0].IsDir() {
				t.Fatalf("converged recovery must retain exactly one terminal payload archive, got %+v", residue)
			}
			entries, err := s.Log(10)
			if err != nil {
				t.Fatal(err)
			}
			compensations := 0
			for _, entry := range entries {
				switch entry.Message {
				case "recover: roll back interrupted new: alpha":
					compensations++
				case "external: manual modifications":
					t.Fatalf("recovery-owned state must not be swept as external work: %+v", entries)
				}
			}
			if compensations != 1 {
				t.Fatalf("re-entry must create exactly one compensation commit, got %d in %+v", compensations, entries)
			}
		})
	}
}

func TestCommittedNewRecoveryPreservesChangedQuarantineContent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string) string
	}{
		{
			name: "foreign addition",
			mutate: func(t *testing.T, payload string) string {
				t.Helper()
				path := filepath.Join(payload, "foreign.txt")
				if err := os.WriteFile(path, []byte("foreign"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "changed transaction file",
			mutate: func(t *testing.T, payload string) string {
				t.Helper()
				path := filepath.Join(payload, "SKILL.md")
				if err := os.WriteFile(path, []byte("changed externally"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			// The terminal archive is meant to prove the exact payload was
			// retained, so a recorded descendant that is simply gone has to
			// conflict like any other change. Accepting it as an interrupted
			// older cleanup made a lost payload indistinguishable from a
			// converged one, and discarded the WAL evidence either way.
			name: "removed transaction file",
			mutate: func(t *testing.T, payload string) string {
				t.Helper()
				if err := os.Remove(filepath.Join(payload, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
				return payload
			},
		},
		{
			name: "replacement directory",
			mutate: func(t *testing.T, payload string) string {
				t.Helper()
				if err := os.Rename(payload, payload+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(payload, 0o755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(payload, "sentinel")
				if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			record := stopCommittedNewRecoveryBeforeCleanup(t, home, "alpha")
			payload := filepath.Join(home, "recovery", installCompensationPayloadName(record))
			sentinel := tt.mutate(t, payload)

			s, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) {
				t.Fatalf("externally changed quarantine must stop recovery with ErrTxnConflict, got %v", err)
			}
			if _, err := os.Lstat(sentinel); err != nil {
				t.Fatalf("externally changed archive content must survive: %v", err)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].Op != "new" {
				t.Fatalf("safe conflict must retain the recovery record, got %+v", pending)
			}
		})
	}
}

func TestCommittedNewRecoveryPreservesModeChangedPublishedContent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	leaveCommittedNewPending(t, home, "alpha")
	skillFile := filepath.Join(home, "store", "skills", "alpha", "SKILL.md")
	if err := os.Chmod(skillFile, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	headBefore, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("mode-changed published content must stop recovery with ErrTxnConflict, got %v", err)
	}
	info, err := os.Lstat(skillFile)
	if err != nil {
		t.Fatalf("mode-changed published content must remain live: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode-changed published content mode changed again: got %#o want 0600", got)
	}
	headAfter, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if headAfter.Hash() != headBefore.Hash() {
		t.Fatalf("ownership conflict must not write a compensation commit: before=%s after=%s", headBefore.Hash(), headAfter.Hash())
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") || cfg.HasSkill("beta") {
		t.Fatalf("ownership conflict must leave the committed config unchanged, got skills %v", cfg.SkillNames())
	}
	if pending, err := PendingTxns(s); err != nil {
		t.Fatal(err)
	} else if len(pending) != 1 {
		t.Fatalf("ownership conflict must retain the WAL, got %+v", pending)
	}
}

func TestCommittedNewRecoveryPreservesWrongTypePublishedContent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	leaveCommittedNewPending(t, home, "alpha")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	headBefore, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(s.SkillsDir(), "alpha")
	if err := os.Rename(skillPath, skillPath+"-owned"); err != nil {
		t.Fatal(err)
	}
	wantBytes := []byte("foreign non-directory replacement")
	if err := os.WriteFile(skillPath, wantBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("wrong-type published content must stop recovery with ErrTxnConflict, got %v", err)
	}
	gotBytes, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("wrong-type published replacement must remain in place: %v", err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("wrong-type published replacement changed: got %q want %q", gotBytes, wantBytes)
	}
	headAfter, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if headAfter.Hash() != headBefore.Hash() {
		t.Fatalf("wrong-type ownership conflict must not write a compensation commit: before=%s after=%s", headBefore.Hash(), headAfter.Hash())
	}
	if pending, err := PendingTxns(s); err != nil {
		t.Fatal(err)
	} else if len(pending) != 1 {
		t.Fatalf("wrong-type ownership conflict must retain the WAL, got %+v", pending)
	}
}

func TestCommittedNewRecoveryRefusesIgnoredExternalFiles(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "inside another skill", path: "skills/other/ignored.txt"},
		{name: "elsewhere in store", path: "ignored-root.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			s, err := store.Init(home)
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(s.SkillsDir(), "other")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			ignore := "/ignored-root.txt\n/skills/other/ignored.txt\n"
			if err := os.WriteFile(filepath.Join(s.Dir(), ".gitignore"), []byte(ignore), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "SKILL.md"), []byte("unrelated skill"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Commit("test: add recovery ignore fixture"); err != nil {
				t.Fatal(err)
			}
			interrupted := errors.New("stop after operation commit")
			if _, err := newSkill(s, nil, "alpha", hooks{
				afterCommit: func() error { return interrupted },
			}); !errors.Is(err, interrupted) {
				t.Fatalf("setup must leave a committed transaction pending, got %v", err)
			}
			headBefore, err := s.Repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			full := filepath.Join(s.Dir(), filepath.FromSlash(tt.path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			wantBytes := []byte("ignored external bytes")
			if err := os.WriteFile(full, wantBytes, 0o644); err != nil {
				t.Fatal(err)
			}
			worktree, err := s.Repo.Worktree()
			if err != nil {
				t.Fatal(err)
			}
			status, err := worktree.Status()
			if err != nil {
				t.Fatal(err)
			}
			if _, visible := status[tt.path]; visible {
				t.Fatalf("fixture path %q must be hidden from ordinary status", tt.path)
			}

			if _, err := NewSkill(s, nil, "beta"); !errors.Is(err, ErrTxnConflict) {
				t.Errorf("ignored external content must stop recovery with ErrTxnConflict, got %v", err)
			}
			headAfter, err := s.Repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			if headAfter.Hash() != headBefore.Hash() {
				t.Errorf("ignored-content conflict must not write a compensation commit: before=%s after=%s", headBefore.Hash(), headAfter.Hash())
			}
			gotBytes, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("ignored external file must survive: %v", err)
			}
			if !bytes.Equal(gotBytes, wantBytes) {
				t.Errorf("ignored external bytes changed: got %q want %q", gotBytes, wantBytes)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].Name != "alpha" {
				t.Errorf("ignored-content conflict must retain alpha's WAL, got %+v", pending)
			}
		})
	}
}

func stopCommittedNewRecoveryBeforeCleanup(t *testing.T, home, name string) TxnRecord {
	t.Helper()
	leaveCommittedNewPending(t, home, name)
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	checked := session.Store
	pending, err := PendingTxns(checked)
	if err != nil {
		_ = session.Close()
		t.Fatal(err)
	}
	if len(pending) != 1 {
		_ = session.Close()
		t.Fatalf("setup must leave one pending transaction, got %+v", pending)
	}
	stop := errors.New("stop before quarantine cleanup")
	err = recoverInstallSkillWithHooks(checked, pending[0], installRecoveryHooks{
		beforeQuarantineCleanup: func(*store.Store, TxnRecord) error { return stop },
	})
	closeErr := session.Close()
	if !errors.Is(err, stop) {
		t.Fatalf("setup recovery must stop before cleanup, got %v", err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	pending, err = PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Stage != installTxnCompensationCommitted {
		t.Fatalf("setup must persist committed compensation state, got %+v", pending)
	}
	return pending[0]
}

func leaveCommittedNewPending(t *testing.T, home, name string) {
	t.Helper()
	s, err := store.Init(home)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("stop after operation commit")
	if _, err := newSkill(s, nil, name, hooks{
		afterCommit: func() error { return interrupted },
	}); !errors.Is(err, interrupted) {
		t.Fatalf("setup must leave a committed transaction pending, got %v", err)
	}
}

// failStage names a durable boundary in the write pipeline.
type failStage int

const (
	failAfterSave failStage = iota
	failDuringPublish
	failDuringCommit
)

// newSkillFailingAt runs NewSkill with an injected failure at one durable
// boundary, so each of them can be asserted separately.
func newSkillFailingAt(s *store.Store, agents []agent.Agent, name string, stage failStage, boom error) (Result, error) {
	var h hooks
	switch stage {
	case failAfterSave:
		h.afterSave = func() error { return boom }
	case failDuringPublish:
		h.beforePublish = func() error { return boom }
	case failDuringCommit:
		h.afterPublish = func() error { return boom }
	}
	return newSkill(s, agents, name, h)
}

// Each create->record pair has a window where the live tree is a strict
// superset of the manifest, and recovery demands bidirectional exact equality.
// A clean crash in either window -- no external writer involved at all -- was
// therefore indistinguishable from foreign interference, and because
// RecoverPending is the first thing every write command runs, the block was not
// scoped to the failed command: the retry, an unrelated `fu new`, and `fu
// enable` all failed identically, with no shipped command able to clear it.
func TestNewSkillConvergesAfterACrashInEitherOwnershipWindow(t *testing.T) {
	stages := []string{"after-staging-create", "after-staging-scaffold"}
	if os.Getenv("FU_TEST_CRASH_OWNERSHIP_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_OWNERSHIP_HOME")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		var h hooks
		switch os.Getenv("FU_TEST_CRASH_OWNERSHIP_STAGE") {
		case "after-staging-create":
			h.afterStagingCreate = crash
		case "after-staging-scaffold":
			h.afterStagingScaffold = crash
		default:
			panic("unknown ownership crash stage")
		}
		_, _ = newSkill(s, nil, "alpha", h)
		panic("ownership crash hook did not run")
	}

	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			if _, err := store.Init(home); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestNewSkillConvergesAfterACrashInEitherOwnershipWindow$")
			cmd.Env = append(os.Environ(),
				"FU_TEST_CRASH_OWNERSHIP_HELPER=1",
				"FU_TEST_CRASH_OWNERSHIP_HOME="+home,
				"FU_TEST_CRASH_OWNERSHIP_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child must die in %s with code 86, err=%v output=%s", stage, err, output)
			}

			s, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			// The retry of the very command that died must work.
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatalf("retrying after a crash at %s must converge: %v", stage, err)
			}
			// And so must an unrelated write command.
			if _, err := NewSkill(s, nil, "beta"); err != nil {
				t.Fatalf("an unrelated write after a crash at %s must converge: %v", stage, err)
			}
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.HasSkill("alpha") || !cfg.HasSkill("beta") {
				t.Fatalf("both skills must be registered, got %v", cfg.SkillNames())
			}
			if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
				t.Fatalf("the retried skill must have its content: %v", err)
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("converged recovery must leave no pending transaction, got %+v", pending)
			}
		})
	}
}

// TestReconcileCompleteDistinguishesAgentFailureFromHardError pins round 18
// finding I3. ReconcileComplete was set before the reconcile error was
// examined, so the phase vector DESIGN §6 documents as faithful reported a
// phase it had not necessarily reached. The two reconcile outcomes must be
// told apart: ErrOperationFailed means the pass ran to completion and found
// per-agent failures, so the phase is complete; a hard error means it never
// ran, so the phase is not.
//
// Only the ErrOperationFailed half is reachable through run() today --
// reconcile's single hard-error path is st.SkillsRoot(), and the checked
// session pins that root at BeginWrite, so nothing during the operation can
// make it fail. The flag is still computed from the error rather than set
// unconditionally, so a future hard-error path cannot silently inherit the lie.
func TestReconcileCompleteDistinguishesAgentFailureFromHardError(t *testing.T) {
	s, _ := setupStore(t)
	claudeDir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}}
	NewSkill(s, agents, "alpha")

	var outcome OperationOutcome
	// The store link's target becomes unreadable, so the agent-side apply
	// fails while the reconcile pass itself completes.
	h := hooks{afterCommit: func() error {
		return os.Chmod(s.SkillsDir(), 0o000)
	}}
	t.Cleanup(func() { _ = os.Chmod(s.SkillsDir(), 0o755) })
	_, err := setGlobalTracked(s, agents, "alpha", false, h, &outcome)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("err = %v; want ErrOperationFailed", err)
	}
	if !outcome.ReconcileComplete {
		t.Fatalf("a reconcile that ran and reported per-agent failures did complete: %+v", outcome)
	}
	if len(outcome.Reconcile.Failed) == 0 {
		t.Fatalf("the per-agent failure must be reported: %+v", outcome.Reconcile)
	}
}

// TestPendingTxnsToleratesDamagedCompletedRevision pins round 18 finding I5.
// PendingTxns re-read and SHA-256'd every revision of every transaction on
// every write command, including long-completed ones that nothing prunes. Two
// consequences: command latency grew with the store's whole lifetime history,
// and a single damaged historical byte -- on a transaction that already
// finished years earlier -- permanently bricked every write command.
//
// A completed transaction is now recognised from its terminal marker plus the
// revision filenames, which already commit to their own digests, so no
// historical revision is read at all. store/config_exchange.go made exactly
// this trade for the config-exchange journal and documents the reasoning.
func TestPendingTxnsToleratesDamagedCompletedRevision(t *testing.T) {
	s, _ := setupStore(t)
	agents := []agent.Agent{fakeAgent{"claude", t.TempDir()}}
	NewSkill(s, agents, "alpha")

	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	damaged := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "txn-new-") && strings.HasSuffix(e.Name(), ".json") {
			if damaged == "" || e.Name() < damaged {
				damaged = e.Name()
			}
		}
	}
	if damaged == "" {
		t.Fatal("the completed transaction left no revision to damage")
	}
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), damaged), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatalf("a damaged revision of an already-completed transaction must not brick every write command: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("the completed transaction must stay completed: %+v", pending)
	}
	// The store must still be writable end to end.
	if _, err := SetGlobal(s, agents, "alpha", false); err != nil {
		t.Fatalf("write commands must still run: %v", err)
	}
}

func TestInstallCompensationPayloadNameUsesOperation(t *testing.T) {
	for _, op := range []string{"new", "add", "adopt"} {
		record := TxnRecord{Op: op, Name: "alpha", StartHead: "0123456789abcdef"}
		name := installCompensationPayloadName(record)
		if !strings.HasPrefix(name, "rollback-"+op+"-alpha-") {
			t.Fatalf("operation %q produced compensation name %q", op, name)
		}
	}
}

func TestInstallCompensationPayloadNameUsesFullStartHead(t *testing.T) {
	startHead := strings.Repeat("a", 40)
	record := TxnRecord{Op: "add", Name: "alpha", StartHead: startHead}

	name := installCompensationPayloadName(record)
	if !strings.HasSuffix(name, "-"+startHead) {
		t.Fatalf("compensation payload name %q does not contain full start HEAD %q", name, startHead)
	}
}
