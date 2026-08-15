// internal/cli/toggle_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Round 2 finding 5: the confirmation must not assert an outcome the same
// command's own diagnostics contradict (SPEC rule 8 makes CLI output fu's
// only means of telling the user when a change takes effect). Reproduced
// against the compiled binary pre-fix: `fu enable alpha --agent claude`
// printed "enabled alpha for claude; takes effect in new agent sessions"
// *before* "conflict: claude/alpha occupied by unmanaged content" -- the
// next line denying what the first just asserted, and the unconditional
// wording did not change even though it was contradicted.
func TestToggleConfirmationDoesNotContradictConflictDiagnostic(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	if _, err := runCmd(t, "disable", "alpha", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}
	// Foreign content now occupies the path fu would otherwise link into.
	link := filepath.Join(home, ".claude", "skills", "alpha")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "enable", "alpha", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	conflictIdx := strings.Index(out, "conflict: claude/alpha occupied by unmanaged content")
	if conflictIdx < 0 {
		t.Fatalf("the conflict must still be reported, got %q", out)
	}
	confirmIdx := strings.Index(out, "enabled alpha for claude")
	if confirmIdx < 0 {
		t.Fatalf("the confirmation must still name the skill and agent, got %q", out)
	}
	if confirmIdx < conflictIdx {
		t.Fatalf("the confirmation must be printed after the diagnostics it might contradict, got %q", out)
	}
	if strings.Contains(out, "enabled alpha for claude; takes effect in new agent sessions") {
		t.Fatalf(`the confirmation must not unconditionally assert "takes effect" when the diagnostic right next to it denies it, got %q`, out)
	}
}

// The per-skill filtering must not over-fire: a conflict recorded against
// a *different* skill in the same reconcile pass (Reconcile scans every
// skill for the affected agents, not only the one just toggled) must not
// soften this command's own, unrelated confirmation.
func TestToggleConfirmationUnaffectedByAnotherSkillsConflict(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "new", "beta")

	// Disturb alpha's link from underneath fu, standing in for a race or
	// manual interference: alpha is still desired on, but its path is now
	// occupied by real, unmanaged content.
	alphaLink := filepath.Join(home, ".claude", "skills", "alpha")
	if err := os.Remove(alphaLink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(alphaLink, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "beta", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "conflict: claude/alpha occupied by unmanaged content") {
		t.Fatalf("setup check: alpha's conflict must still surface in the same reconcile pass, got %q", out)
	}
	if !strings.Contains(out, "disabled beta for claude; takes effect in new agent sessions") {
		t.Fatalf("beta's own confirmation must keep its normal wording -- alpha's unrelated conflict must not soften it, got %q", out)
	}
}

// The confirmation must not contradict the disabled-foreign diagnostic
// either -- the same class of bug finding 5 fixed for Conflicts/Failed,
// now reachable through Result.DisabledForeign once printResult started
// surfacing it. Without toggleDeliveryBlocked also checking DisabledForeign, this
// exact command would print "disabled alpha globally; takes effect in new
// agent sessions" directly beneath a diagnostic saying the skill may still
// be loaded -- reintroducing finding 5's bug for this one report the
// moment it started being printed.
func TestToggleConfirmationDoesNotContradictDisabledForeignDiagnostic(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	if _, err := runCmd(t, "disable", "alpha"); err != nil {
		t.Fatal(err)
	}
	// Foreign content now occupies the exact path fu just cleared.
	link := filepath.Join(home, ".claude", "skills", "alpha")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	diagIdx := strings.Index(out, "disabled-foreign: claude/alpha")
	if diagIdx < 0 {
		t.Fatalf("the disabled-foreign diagnostic must still be reported, got %q", out)
	}
	confirmIdx := strings.Index(out, "disabled alpha globally")
	if confirmIdx < 0 {
		t.Fatalf("the confirmation must still name the skill, got %q", out)
	}
	if confirmIdx < diagIdx {
		t.Fatalf("the confirmation must be printed after the diagnostics it might contradict, got %q", out)
	}
	if strings.Contains(out, "disabled alpha globally; takes effect in new agent sessions") {
		t.Fatalf(`the confirmation must not unconditionally assert "takes effect" when the diagnostic right next to it says the skill may still be loaded, got %q`, out)
	}
}

// Round 3 finding 1: toggleDeliveryBlocked only compared Conflicts/DisabledForeign/
// Failed's Action.Skill against name directly, but Reconcile records an
// agent-level failure (a broken ScanAgent, discovered before Diff ever
// runs for that agent) as a placeholder Action whose Skill is empty --
// that comparison could never match it. This is the strongest form the
// fix exists to prevent: an agent that received nothing at all still got
// an unqualified "takes effect" confirmation. Reproduced against the
// compiled binary pre-fix: with ~/.codex/skills replaced by a plain file,
// `fu disable alpha` printed "disabled alpha globally; takes effect in
// new agent sessions" on stdout while stderr said "failed: codex: open
// .../.codex/skills: not a directory", exit 1.
func TestToggleGlobalConfirmationQualifiedByAgentLevelFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// codex's skills dir is a plain file, not a directory: ScanAgent fails
	// for codex specifically, before Diff ever runs for it. `fu new alpha`
	// above already materialized it as a real directory (codex is
	// detected), so it must be cleared first.
	codexSkills := filepath.Join(home, ".codex", "skills")
	if err := os.RemoveAll(codexSkills); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexSkills, []byte("notadir"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha")
	if err == nil {
		t.Fatal("a genuine per-agent failure must still exit non-zero")
	}
	if !strings.Contains(out, "failed: codex:") {
		t.Fatalf("the failure must still be reported, got %q", out)
	}
	if !strings.Contains(out, "disabled alpha globally") {
		t.Fatalf("the confirmation must still name the skill, got %q", out)
	}
	if strings.Contains(out, "disabled alpha globally; takes effect in new agent sessions") {
		t.Fatalf(`the confirmation must not unconditionally assert "takes effect" when an agent received nothing at all, got %q`, out)
	}
}

func TestToggleNoOpStillPrintsReconcileFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	codexSkills := filepath.Join(home, ".codex", "skills")
	if err := os.RemoveAll(codexSkills); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexSkills, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "enable", "alpha")
	if err == nil {
		t.Fatal("a no-op toggle with a reconcile failure must exit non-zero")
	}
	if !strings.Contains(out, "failed: codex:") {
		t.Fatalf("no-op toggle lost its reconcile diagnostic: %q", out)
	}
	if !strings.Contains(out, "enabled alpha globally") {
		t.Fatalf("no-op toggle lost its durable-state confirmation: %q", out)
	}
}

// Round 3 finding 1: Missing was not consulted by toggleDeliveryBlocked at all, so
// a skill enabled but whose store content is gone still got an
// unqualified "takes effect" confirmation, directly contradicting the
// "missing:" diagnostic printed right above it. Reproduced against the
// compiled binary pre-fix: `rm -rf $FU_HOME/store/skills/alpha && fu
// enable alpha` printed "enabled alpha globally; takes effect in new
// agent sessions" on stdout and "missing: claude/alpha is enabled but the
// store no longer holds its content" on stderr, exit 0.
func TestToggleConfirmationQualifiedByMissingStoreContent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "disable", "alpha")

	if err := os.RemoveAll(filepath.Join(fuHome, "store", "skills", "alpha")); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "enable", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "missing: claude/alpha") {
		t.Fatalf("the missing-content diagnostic must still be reported, got %q", out)
	}
	if strings.Contains(out, "enabled alpha globally; takes effect in new agent sessions") {
		t.Fatalf(`the confirmation must not unconditionally assert "takes effect" when the store no longer holds the skill's content, got %q`, out)
	}
}

// Skipped (SPEC rule 10: an agent's own skills dir is a symlink) is
// agent-level by construction -- Result.Skipped is a bare []string of
// agent names, never an Action with a Skill field -- so it must block
// every skill on that agent the same way an agent-level Failed
// placeholder does.
func TestToggleConfirmationQualifiedBySkippedAgent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// codex's own skills dir is itself a symlink (SPEC rule 10): the whole
	// agent is skipped, not merely one entry within it.
	codexSkills := filepath.Join(home, ".codex", "skills")
	if err := os.RemoveAll(codexSkills); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), codexSkills); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "skipped agent codex") {
		t.Fatalf("setup check: codex must still be reported skipped, got %q", out)
	}
	if strings.Contains(out, "disabled alpha globally; takes effect in new agent sessions") {
		t.Fatalf(`the confirmation must not unconditionally assert "takes effect" when an entire agent was skipped, got %q`, out)
	}
}

// Round 3 finding 1's own false-positive direction, the reverse of the
// three tests above: toggleDeliveryBlocked used to ignore agent scope entirely, so
// an --agent-scoped toggle picked up a diagnostic recorded against a
// completely different agent it never touched. Reproduced against the
// compiled binary pre-fix: with claude's alpha link disturbed by foreign
// content, `fu disable alpha --agent codex` printed the softened "may not
// take effect" wording even though codex itself had no problem at all.
func TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsConflict(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// Disturb alpha's claude link with foreign content; codex is untouched.
	claudeLink := filepath.Join(home, ".claude", "skills", "alpha")
	if err := os.Remove(claudeLink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(claudeLink, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha", "--agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "conflict: claude/alpha") {
		t.Fatalf("setup check: claude's conflict must still surface in the same reconcile pass, got %q", out)
	}
	if !strings.Contains(out, "disabled alpha for codex; takes effect in new agent sessions") {
		t.Fatalf("codex's own confirmation must keep its normal wording -- claude's unrelated conflict must not soften it, got %q", out)
	}
}

func TestToggleCommands(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	link := filepath.Join(home, ".claude", "skills", "alpha")
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("link must exist after new")
	}
	if _, err := runCmd(t, "disable", "alpha", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("agent-level disable must remove the link")
	}
	if _, err := runCmd(t, "enable", "alpha", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("agent-level enable must restore the link")
	}
}

// Self-review addition: the engine layer already covers the unknown-skill
// and unknown-agent guards in detail; this confirms the CLI wiring itself
// (cobra flag parsing, RunE's error return, agent.Detected() call) actually
// surfaces those failures through Execute() rather than swallowing them.
func TestToggleUnknownSkillAndAgentFail(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	if _, err := runCmd(t, "enable", "ghost"); err == nil {
		t.Fatal("unknown skill must fail")
	}
	if _, err := runCmd(t, "disable", "alpha", "--agent", "gemini"); err == nil {
		t.Fatal("unknown agent must fail")
	}
}

// Finding I5, end to end: a link created via one FU_HOME/HOME spelling
// must still be recognized (and removable) by a later write command
// reaching the exact same physical store through a different spelling.
// Reproduced against the compiled binary pre-fix: with ~/.fu a symlink
// to a real target directory, creating the link through the HOME
// fallback (via the symlink) and then disabling it via FU_HOME set
// directly to the resolved target exited 0, recorded the override in
// fu.yaml, and committed -- but the claude link created under the other
// spelling was left completely untouched on disk, because ownsLink's raw
// string comparison classified it as foreign from the disabling
// process's point of view. `fu list` still reported it "off".
func TestToggleVisibleAcrossHomeSpellings(t *testing.T) {
	base := t.TempDir()
	realTarget := filepath.Join(base, "real-fu-target")
	if err := os.MkdirAll(realTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTarget, filepath.Join(home, ".fu")); err != nil {
		t.Fatal(err)
	}

	// Create the store and link "alpha" via the HOME fallback spelling
	// (reaching the store through the ~/.fu symlink).
	t.Setenv("FU_HOME", "")
	t.Setenv("HOME", home)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	link := filepath.Join(home, ".claude", "skills", "alpha")
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("setup: link must exist before the spelling switch")
	}

	// Disable it for claude via the OTHER spelling: FU_HOME set directly
	// to the symlink's resolved target, never mentioning ~/.fu at all.
	t.Setenv("FU_HOME", realTarget)
	if _, err := runCmd(t, "disable", "alpha", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("agent-level disable via one FU_HOME spelling must remove a link created under a different (but physically identical) spelling")
	}
}

// Finding I8: `--agent ""` is an explicit (if empty) value, not "the
// flag was never given", and must be rejected as an unknown agent rather
// than silently falling through to a broader global toggle. Reproduced
// against the compiled binary pre-fix: `fu disable pdf-tools --agent ""`
// exited 0 and wrote enabled: false at the top level -- a global
// disable performed in place of the requested (if malformed) agent-level
// one, then committed. A script doing `fu disable "$name" --agent
// "$AGENT"` with $AGENT unset triggers exactly this.
func TestToggleAgentEmptyStringRejectedNotGlobal(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	if _, err := runCmd(t, "disable", "alpha", "--agent", ""); err == nil {
		t.Fatal(`--agent "" must be rejected as an unknown agent, not perform a global disable`)
	}

	out, err := runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "global:      on") {
		t.Fatalf(`--agent "" must not have disabled the skill globally:\n%s`, out)
	}
	link := filepath.Join(home, ".claude", "skills", "alpha")
	if _, err := os.Readlink(link); err != nil {
		t.Fatal(`--agent "" must not have torn down the claude link either`)
	}
}

// Finding I9: enable/disable must confirm what changed and note that it
// takes effect in new agent sessions (SPEC rule 8 makes CLI output fu's
// only means of conveying this). Reproduced against the compiled binary
// pre-fix: `fu enable writer` and `fu disable writer` both produced
// empty output on success -- no confirmation the write happened, no
// indication of when it takes effect.
func TestToggleCommandsConfirmChangeAndTiming(t *testing.T) {
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
	if !strings.Contains(out, "alpha") || !strings.Contains(strings.ToLower(out), "session") {
		t.Fatalf("global enable must confirm the change and note it takes effect next session, got %q", out)
	}

	out, err = runCmd(t, "disable", "alpha", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "claude") || !strings.Contains(strings.ToLower(out), "session") {
		t.Fatalf("agent-level disable must confirm the change (naming the agent) and note it takes effect next session, got %q", out)
	}
}

// TestToggleAgentWriteDoesNotDestroyAnotherAgentsOverride is round 4
// finding 1's end-to-end form: store.TestAgentSwitchWriteDoesNotNormalizeAnotherAgentsOverride
// pins the same bug directly against store.Config; this drives it through
// the real CLI surface, matching the reviewer's exact four-command
// reproduction against the compiled binary (codex is never touched again
// after step 1). Pre-fix, step 3 silently deleted codex's override
// (fu.yaml's whole `overrides:` section vanished, not just claude's own
// key), so step 4's global enable re-enabled codex too and materialized
// ~/.codex/skills/alpha for an agent the user had explicitly disabled.
func TestToggleAgentWriteDoesNotDestroyAnotherAgentsOverride(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// 1) codex gets an explicit override.
	if _, err := runCmd(t, "disable", "alpha", "--agent", "codex"); err != nil {
		t.Fatal(err)
	}
	// 2) global disable: codex's override (already false) must survive.
	if _, err := runCmd(t, "disable", "alpha"); err != nil {
		t.Fatal(err)
	}
	// 3) claude's own switch -- unrelated to codex, but its new value
	// (false) happens to equal the current global, exactly the coincidence
	// the shared normalize helper used to key off.
	if _, err := runCmd(t, "disable", "alpha", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}
	// 4) global enable: must not silently re-enable codex.
	if _, err := runCmd(t, "enable", "alpha"); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "off*") {
		t.Fatalf("codex must still carry its explicit override after an unrelated agent's write, got:\n%s", out)
	}
	codexLink := filepath.Join(home, ".codex", "skills", "alpha")
	if _, err := os.Lstat(codexLink); !os.IsNotExist(err) {
		t.Fatal("codex must remain unlinked -- its override must still be honored after step 4's global enable")
	}
}

// TestOrdinaryWriteReclaimsStrayLinkUnderInvalidNameLoadedFromDisk is round
// 4 finding 2's second half, and the two-rounds-ago defect it must
// genuinely fix in production this time: round 3 finding 2's 33c06bc made
// engine.Desired filter an invalid fu.yaml name out of desired specifically
// so a stray fu-owned link recorded under it could still be reclaimed --
// but that same round's sibling fix (bdf2882, closing fu show's path
// escape) made LoadConfig refuse the entire file the moment any name
// failed validation, so that reclamation path never actually ran in
// production: no command's RunE ever got as far as calling engine.Desired
// with such a config, because LoadConfig itself errored out first.
// engine.TestReconcileRemovesFuLinkRecordedUnderInvalidName exercises the
// engine layer directly (setupStore builds its *store.Config in memory via
// AddSkill, bypassing LoadConfig's file-parsing path entirely), so it
// could not catch this -- production fu.yaml always arrives through
// LoadConfig.
//
// This test goes through that real path: a hand-edited fu.yaml on disk,
// loaded by an ordinary write command, matching the reviewer's own
// reproduction (`Beta: {enabled: false}` in fu.yaml, a genuine
// ~/.claude/skills/Beta -> store link already in place, confirmed still
// sitting on disk afterward).
func TestOrdinaryWriteReclaimsStrayLinkUnderInvalidNameLoadedFromDisk(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// A genuine fu-owned link on disk under an invalid name, standing in
	// for one written by an older fu (or a future clone/pull) before this
	// build's naming rules applied to it.
	storeBeta := filepath.Join(fuHome, "store", "skills", "Beta")
	if err := os.MkdirAll(storeBeta, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeLink := filepath.Join(home, ".claude", "skills", "Beta")
	if err := os.Symlink(storeBeta, claudeLink); err != nil {
		t.Fatal(err)
	}

	// fu.yaml itself records it, the reviewer's exact reproduction.
	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "skills:\n  alpha:",
		"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: false\n  alpha:", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected seed content to edit")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	// An ordinary write command targeting a completely different skill.
	if _, err := runCmd(t, "enable", "alpha"); err != nil {
		t.Fatalf("an unrelated write command must not be blocked by another skill's invalid name, got %v", err)
	}

	if _, err := os.Lstat(claudeLink); !os.IsNotExist(err) {
		t.Fatal("a stray fu-owned link recorded under an invalid name must be reclaimed by an ordinary write command")
	}
}

// Round 4 process rule: a narrowing fix must have a test for the side it
// excludes, written before the fix. toggleDeliveryBlocked's five report kinds
// (Conflicts, DisabledForeign, Missing, Failed, Skipped) each already had a
// "must soften" test above; only Conflicts
// (TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsConflict) also
// had the reverse -- "must NOT soften" -- direction. The other four kinds'
// own agent-scoping guard (the `targeted[...]` check in each of
// toggleDeliveryBlocked's loops) was therefore unpinned: correct today, but exactly
// the kind of adjacent coordinate three straight review rounds have found
// unguarded and regressed. The four tests below close that gap, one per
// remaining report kind, each mirroring
// TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsConflict's own
// shape: build the report on one agent, scope the toggle to a different
// one, and require the scoped agent's own confirmation to keep its
// ordinary, unqualified wording.
//
// Each was verified to discriminate by temporarily relaxing its
// corresponding toggleDeliveryBlocked branch (dropping the `targeted[...]` guard so
// the report blocks regardless of agent scope) and confirming the test
// fails, then restoring the guard and confirming it passes again; see the
// the saved code-review record for the exact before/after `go test` output.

// TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsDisabledForeignDiagnostic
// is DisabledForeign's reverse direction, the counterpart to
// TestToggleConfirmationDoesNotContradictDisabledForeignDiagnostic's
// "must soften" case above. codex's alpha is off and blocked by unmanaged
// content -- a DisabledForeign report -- while claude's own alpha is
// untouched; a claude-scoped disable must not be softened by codex's
// unrelated report, even though the same reconcile pass still surfaces it.
func TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsDisabledForeignDiagnostic(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// codex's alpha is off, with foreign content occupying the exact path
	// fu just cleared for it: a DisabledForeign report for codex/alpha.
	// claude is untouched.
	if _, err := runCmd(t, "disable", "alpha", "--agent", "codex"); err != nil {
		t.Fatal(err)
	}
	codexLink := filepath.Join(home, ".codex", "skills", "alpha")
	if err := os.MkdirAll(codexLink, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled-foreign: codex/alpha") {
		t.Fatalf("setup check: codex's disabled-foreign report must still surface in the same reconcile pass, got %q", out)
	}
	if !strings.Contains(out, "disabled alpha for claude; takes effect in new agent sessions") {
		t.Fatalf("claude's own confirmation must keep its normal wording -- codex's unrelated disabled-foreign report must not soften it, got %q", out)
	}
}

// TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsMissingStoreContent
// is Missing's reverse direction, the counterpart to
// TestToggleConfirmationQualifiedByMissingStoreContent's "must soften" case
// above. claude no longer desires alpha at all (its own override is off),
// so deleting alpha's store content afterward leaves only codex -- which
// still follows the global "on" -- attempting to rebuild a link and
// finding the content gone. Re-disabling alpha for claude (already off;
// idempotent, but still runs the full pipeline and reconcile) must not be
// softened by codex's unrelated report against the very same skill name;
// the same-name-different-agent shape mirrors the Conflicts test above so
// the assertion exercises toggleDeliveryBlocked's agent match, not its skill-name
// match (a *different* skill name would already discriminate on its own).
func TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsMissingStoreContent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	if _, err := runCmd(t, "disable", "alpha", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(fuHome, "store", "skills", "alpha")); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "missing: codex/alpha") {
		t.Fatalf("setup check: codex's missing-content report must still surface in the same reconcile pass, got %q", out)
	}
	if !strings.Contains(out, "disabled alpha for claude; takes effect in new agent sessions") {
		t.Fatalf("claude's own confirmation must keep its normal wording -- codex's unrelated missing-content report must not soften it, got %q", out)
	}
}

// TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsFailure is
// Failed's reverse direction, the counterpart to
// TestToggleGlobalConfirmationQualifiedByAgentLevelFailure's "must soften"
// case above -- and the shape the task brief calls out as mattering most:
// Failed's agent-level form (a broken ScanAgent recorded as a placeholder
// Action with AgentName set and Skill == "") blocks *every* skill on that
// agent by construction, so if toggleDeliveryBlocked's `targeted[f.Action.AgentName]`
// guard were ever dropped, this is the branch that would silently start
// softening every other agent's confirmation too. codex's skills dir is a
// plain file (ScanAgent fails for codex specifically); claude has nothing
// wrong with it. The overall command still exits non-zero -- Failed is the
// one report kind that makes Reconcile return ErrOperationFailed regardless
// of which agent it is attached to -- but claude's own confirmation wording
// must stay unqualified.
func TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// codex's skills dir is a plain file, not a directory: ScanAgent fails
	// for codex specifically, before Diff ever runs for it. `fu new alpha`
	// above already materialized it as a real directory (codex is
	// detected), so it must be cleared first.
	codexSkills := filepath.Join(home, ".codex", "skills")
	if err := os.RemoveAll(codexSkills); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexSkills, []byte("notadir"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha", "--agent", "claude")
	if err == nil {
		t.Fatal("codex's own agent-level failure must still make the overall command exit non-zero")
	}
	if !strings.Contains(out, "failed: codex:") {
		t.Fatalf("setup check: codex's failure must still be reported, got %q", out)
	}
	if !strings.Contains(out, "disabled alpha for claude; takes effect in new agent sessions") {
		t.Fatalf(`claude's own confirmation must keep its normal wording -- codex's unrelated agent-level failure must not soften it, got %q`, out)
	}
}

// TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsSkippedStatus
// is Skipped's reverse direction, the counterpart to
// TestToggleConfirmationQualifiedBySkippedAgent's "must soften" case above.
// codex's own skills dir is itself a symlink (SPEC rule 10), so the whole
// agent is skipped; claude is unaffected. A claude-scoped disable must not
// be softened by codex being skipped.
func TestToggleAgentScopedConfirmationUnaffectedByAnotherAgentsSkippedStatus(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// codex's own skills dir is itself a symlink (SPEC rule 10): the whole
	// agent is skipped, not merely one entry within it.
	codexSkills := filepath.Join(home, ".codex", "skills")
	if err := os.RemoveAll(codexSkills); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), codexSkills); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "disable", "alpha", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "skipped agent codex") {
		t.Fatalf("setup check: codex must still be reported skipped, got %q", out)
	}
	if !strings.Contains(out, "disabled alpha for claude; takes effect in new agent sessions") {
		t.Fatalf(`claude's own confirmation must keep its normal wording -- codex being skipped must not soften it, got %q`, out)
	}
}

// TestWriteCommandRefusesToRebuildFromMissingOrBlankConfig is round 6's
// second Critical. LoadConfig returned a fresh, empty configuration for a
// missing or blank fu.yaml -- correct for "no store yet", catastrophic for
// "store exists, its config was destroyed". Store.Open has already proven
// the store is initialized by the time any command loads a config, so at
// that point an absent or empty fu.yaml can only mean damage.
//
// Left unguarded, the next ordinary write reconstructs the config from
// nothing: Sweep commits the deletion into history, Save writes back a file
// holding only the new mutation, and Reconcile removes every delivered link
// whose skill is missing from the reconstruction. `fu new beta` therefore
// silently discards every registration, switch, override and source record
// the user had -- and the git history that could have recovered them now
// records the loss as an ordinary operation.
func TestWriteCommandRefusesToRebuildFromMissingOrBlankConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		destroy func(t *testing.T, cfgPath string)
	}{
		{"deleted", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated to zero length", func(t *testing.T, p string) {
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"whitespace only", func(t *testing.T, p string) {
			// Spaces and newlines only -- no tab, which YAML rejects as a
			// parse error on its own and would not exercise the blank-document
			// path this case is about.
			if err := os.WriteFile(p, []byte("\n   \n \n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fuHome, home := t.TempDir(), t.TempDir()
			t.Setenv("FU_HOME", fuHome)
			t.Setenv("HOME", home)
			if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "init")
			runCmd(t, "new", "alpha")
			link := filepath.Join(home, ".claude", "skills", "alpha")
			if _, err := os.Lstat(link); err != nil {
				t.Fatalf("setup: alpha's link must exist before the config is destroyed: %v", err)
			}

			cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
			tc.destroy(t, cfgPath)

			out, err := runCmd(t, "new", "beta")
			if err == nil {
				t.Fatalf("a write command must refuse to run against a destroyed config "+
					"rather than reconstructing it from nothing, got output %q", out)
			}
			// The user cannot act on this without being told where the file is
			// and that git holds a copy.
			if !strings.Contains(err.Error(), cfgPath) {
				t.Errorf("the refusal must name the config's own path, got %v", err)
			}
			if !strings.Contains(err.Error(), "git") {
				t.Errorf("the refusal must point at the recovery route (the store is a git repo), got %v", err)
			}
			// Nothing may have been destroyed in the meantime.
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("alpha's delivered link must survive a refused command: %v", err)
			}
		})
	}
}
