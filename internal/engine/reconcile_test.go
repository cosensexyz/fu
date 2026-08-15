// internal/engine/reconcile_test.go
package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

func setupStore(t *testing.T, skills ...string) (*store.Store, *store.Config) {
	t.Helper()
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.LoadConfig(s.ConfigPath())
	for _, name := range skills {
		os.MkdirAll(filepath.Join(s.SkillsDir(), name), 0o755)
		if err := cfg.AddSkill(name, "sha256:x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

func checkedRecoveryStore(t *testing.T, s *store.Store) *store.Store {
	t.Helper()
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session.Store
}

// loadConfigFromYAML writes raw into a real store's fu.yaml and loads it
// back through store.LoadConfig -- the one constructor production code ever
// uses. Round 5 finding: the precedence test used to build its Config in
// memory through setupStore/AddSkill, which never populates Config.invalid,
// so it pinned a branch production can no longer reach at all while the
// branch production does reach behaved the opposite way.
func loadConfigFromYAML(t *testing.T, raw string) (*store.Store, *store.Config) {
	t.Helper()
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

// fakeAgentNoReserved is fakeAgent's counterpart with an empty Reserved()
// list -- Claude's real adapter reserves nothing. Precedence between a
// reserved collision and an invalid name is only observable when two agents
// in the same pass disagree about which names they reserve.
type fakeAgentNoReserved struct{ name, dir string }

func (f fakeAgentNoReserved) Name() string       { return f.name }
func (f fakeAgentNoReserved) Detect() bool       { return true }
func (f fakeAgentNoReserved) SkillsDir() string  { return f.dir }
func (f fakeAgentNoReserved) Reserved() []string { return nil }

func TestReconcileCreatesAndRemoves(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alpha")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(s.SkillsDir(), "alpha") {
		t.Fatalf("link not materialized: %v %q", err, target)
	}
	// idempotence: second run performs no changes, reports nothing
	res, err = Reconcile(s, agents)
	if err != nil || len(res.Conflicts) != 0 || len(res.Foreign) != 0 {
		t.Fatalf("second run must be clean: %+v %v", res, err)
	}
	// disable removes the link
	cfg.SetEnabled("alpha", false)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	Reconcile(s, agents)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("disabled skill link must be removed")
	}
}

func TestReconcileDoesNotApplyStaleCallerConfig(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	stale, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Enabled("alpha") {
		t.Fatal("stale snapshot must predate the durable disable")
	}
	if _, err := SetGlobal(s, agents, "alpha", false); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alpha")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("durable disable did not remove the link: %v", err)
	}

	if _, err := Reconcile(s, agents); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("public reconciliation recreated a link from stale caller-supplied config")
	}
}

func TestReconcileNeverTouchesForeign(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir := t.TempDir()
	// foreign real dir occupies the desired path
	os.MkdirAll(filepath.Join(dir, "alpha"), 0o755)
	res, err := Reconcile(s, []agent.Agent{fakeAgent{"claude", dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %+v", res)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "alpha")); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("foreign entry must not be replaced")
	}
}

// Round 2 finding 2: a skill disabled while real (genuinely unmanaged)
// content occupies its path end to end through Reconcile, not just Diff in
// isolation. Reproduced against the compiled binary pre-fix: with a real
// directory already sitting at the skill's path, `fu new` correctly
// reported "conflict: ... occupied by unmanaged content", but `fu disable`
// over that same directory produced no output and an entirely empty
// Result -- fu had no record at all that anything was there, even though
// SPEC rule 2 calls for unmanaged entries to be listed for the user's
// information. This is the general form of the matrix cell finding 1's own
// (misclassified-fu-link) scenario also fell into, which is why fixing
// only finding 1 would still have left a genuinely foreign path silently
// unreported here.
//
// Renamed from TestReconcileReportsForeignForDisabledSkillBehindForeignContent
// (the same reasoning a previous round used for an analogous rename:
// "succeeds" was the wrong word for what that fix did, so keeping the name
// would have been actively misleading). This scenario no longer lands in
// Result.Foreign at all: a later pass gave the fu.yaml-known,
// disabled-but-blocked case its own Result.DisabledForeign, specifically
// because Result.Foreign is never printed by any write command (reserved
// for a future `fu status`) -- landing here left this actionable report
// exactly as silent as the merely informational one, reproducing this same
// finding's symptom (a confirmation and nothing else) one layer down.
func TestReconcileReportsDisabledForeignBehindForeignContent(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	// Real (non-fu) content already occupies the path before alpha is ever
	// disabled -- fu was never able to link here in the first place.
	os.MkdirAll(filepath.Join(dir, "alpha"), 0o755)
	os.WriteFile(filepath.Join(dir, "alpha", "notes.txt"), []byte("mine"), 0o644)

	cfg.SetEnabled("alpha", false)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	res, err := Reconcile(s, []agent.Agent{fakeAgent{"claude", dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DisabledForeign) != 1 || res.DisabledForeign[0].Skill != "alpha" || res.DisabledForeign[0].AgentName != "claude" {
		t.Fatalf("a disabled skill behind real foreign content must be reported as disabled-foreign, got %+v", res)
	}
	// Must land on the new, actionable channel only -- not also (or instead)
	// on the informational-only one, which is what this exact case used to
	// share with entirely-unknown names before the split.
	if len(res.Foreign) != 0 {
		t.Fatalf("a fu.yaml-known disabled skill must not also be reported as plain Foreign: %+v", res.Foreign)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("disabled+foreign is a report, not a conflict (nothing was ever going to be linked): %+v", res.Conflicts)
	}
	got, err := os.ReadFile(filepath.Join(dir, "alpha", "notes.txt"))
	if err != nil || string(got) != "mine" {
		t.Fatalf("foreign content must never be touched, got %q err=%v", got, err)
	}
}

func TestReconcileSkipsSymlinkedParent(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	base := t.TempDir()
	target := filepath.Join(base, "real")
	os.MkdirAll(target, 0o755)
	link := filepath.Join(base, "linkdir")
	os.Symlink(target, link)
	res, err := Reconcile(s, []agent.Agent{fakeAgent{"claude", link}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "claude" {
		t.Fatalf("symlinked parent must be skipped: %+v", res)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Fatal("must never write through a symlinked skills dir")
	}
}

// staleSpellingLink plants a fu-owned link for skill whose recorded target
// text is a *different spelling* of the same store path -- reached through
// an alias of $FU_HOME, as a dotfiles manager leaves behind when it moves
// ~/.fu aside and links it back. It returns the link's own path.
//
// This is what "stale target" means after round 6 tightened ownership to
// exact projection identity: the leaf still names the skill (so the link is
// still fu's), but the directory half no longer matches what
// Store.SkillsDir() spells today, so Diff rebuilds it. The construction
// these tests used before -- pointing "alpha" at store/skills/old-alpha --
// is no longer a stale fu link at all but foreign content: fu has no
// operation that creates a link named one thing pointing at a skill named
// another, so such an entry is by definition not one fu wrote. See
// TestReconcileCrossNameLinkIsForeignNotRebuilt for that direction.
func staleSpellingLink(t *testing.T, s *store.Store, agentDir, skill string) string {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "myfu")
	if err := os.Symlink(s.Home, alias); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(agentDir, skill)
	if err := os.Symlink(filepath.Join(alias, "store", "skills", skill), link); err != nil {
		t.Fatal(err)
	}
	return link
}

// A fu-owned link left pointing at a stale spelling of the store path must
// actually be rebuilt on disk by Reconcile, not merely flagged by Diff: the
// final symlink must carry the skill's current address, not the older
// spelling it was found with.
func TestReconcileRebuildsStaleLinkTarget(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir := t.TempDir()
	link := staleSpellingLink(t, s, dir, "alpha")
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 || len(res.Foreign) != 0 {
		t.Fatalf("rebuild must not be reported as conflict/foreign: %+v", res)
	}
	want := filepath.Join(s.SkillsDir(), "alpha")
	target, err := os.Readlink(link)
	if err != nil || target != want {
		t.Fatalf("stale link not rebuilt: %v %q, want %q", err, target, want)
	}
}

// TestReconcileCrossNameLinkIsForeignNotRebuilt is round 6's Critical seen
// from the rebuild side. A link named "alpha" pointing at
// store/skills/other is not a fu link with a stale target -- fu has no
// operation that produces one -- so it must be left alone and reported,
// never removed and replaced. This is the behaviour DESIGN §2's state
// matrix row for "fu 链接，指向过期路径" used to cover and no longer does.
func TestReconcileCrossNameLinkIsForeignNotRebuilt(t *testing.T) {
	s, _ := setupStore(t, "alpha", "other")
	dir := t.TempDir()
	link := filepath.Join(dir, "alpha")
	target := filepath.Join(s.SkillsDir(), "other")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	res, err := Reconcile(s, []agent.Agent{fakeAgent{"claude", dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Skill != "alpha" {
		t.Fatalf("a link fu never created, occupying a desired path, is a conflict: %+v", res)
	}
	got, err := os.Readlink(link)
	if err != nil || got != target {
		t.Fatalf("the user's own link must survive byte for byte: %v %q, want %q", err, got, target)
	}
}

// A fu-owned link whose target has vanished (dangling) must still be
// recognized as fu's own when Reconcile re-verifies before removing it:
// os.Lstat succeeds for a dangling symlink (it does not follow the
// link), so a Stat-based re-check would wrongly treat this as foreign
// and refuse to touch it. So the stale link is still removed here (and
// must not be reported as a conflict) -- but see
// TestReconcileReportsMissingInsteadOfRebuildingDanglingLink below for
// what must happen next, which is not a rebuild.
//
// This test used to assert that Reconcile recreated the link pointing at
// the same (still-absent) target -- i.e. it built another dangling
// symlink. That was wrong: `fu rm` refuses a name LoadConfig isolated, so hand
// deletion under $FU_HOME is the *only* way a skill's store-side content
// disappears, making this the ordinary path, not an edge case. The old
// behavior meant `fu list` kept reporting the skill as on forever, with
// no error and no signal anywhere (finding 1). Kept in place and
// corrected rather than deleted, per the review's own instruction.
func TestReconcileReportsMissingInsteadOfRebuildingDanglingLink(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir := t.TempDir()
	want := filepath.Join(s.SkillsDir(), "alpha")
	link := filepath.Join(dir, "alpha")
	if err := os.Symlink(want, link); err != nil {
		t.Fatal(err)
	}
	// The store-side entity disappears out from under an intact link
	// (SPEC rule 6, "断链").
	if err := os.RemoveAll(want); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("broken-but-owned link must not be reported as a conflict: %+v", res)
	}
	if len(res.Missing) != 1 || res.Missing[0].Skill != "alpha" || res.Missing[0].AgentName != "claude" {
		t.Fatalf("want exactly one Missing entry for alpha/claude, got %+v", res.Missing)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("dangling link must be removed, not recreated pointing at a target that still does not exist")
	}
}

// Finding 1's exact reproduction: `fu new alpha` then hand-deleting
// $FU_HOME/store/skills/alpha, before any link toward it was ever
// created for this agent. This exercises the plain "desired && absent"
// Diff arm (CreateLink from scratch), distinct from
// TestReconcileReportsMissingInsteadOfRebuildingDanglingLink above,
// which exercises the RemoveLink+CreateLink rebuild-pair arm starting
// from a pre-existing link. Both arms funnel through the same
// Reconcile CreateLink case, so both need their own coverage.
func TestReconcileMissingStoreDirYieldsNoDanglingLink(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 || res.Missing[0].Skill != "alpha" || res.Missing[0].AgentName != "claude" {
		t.Fatalf("want exactly one Missing entry for alpha/claude, got %+v", res.Missing)
	}
	if _, err := os.Lstat(filepath.Join(dir, "alpha")); !os.IsNotExist(err) {
		t.Fatal("no dangling link must ever be created for a skill whose store content is gone")
	}
	if len(res.Conflicts) != 0 || len(res.Foreign) != 0 {
		t.Fatalf("no conflicts/foreign expected: %+v", res)
	}
}

func TestReconcileWrongTypeStoreTargetYieldsNoLink(t *testing.T) {
	tests := []struct {
		name  string
		plant func(*testing.T, string)
	}{
		{
			name: "regular file",
			plant: func(t *testing.T, target string) {
				t.Helper()
				if err := os.WriteFile(target, []byte("not a skill directory"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			plant: func(t *testing.T, target string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo",
			plant: func(t *testing.T, target string) {
				t.Helper()
				if err := unix.Mkfifo(target, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := setupStore(t, "alpha")
			target := filepath.Join(s.SkillsDir(), "alpha")
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			tt.plant(t, target)
			dir := t.TempDir()

			res, err := Reconcile(s, []agent.Agent{fakeAgent{"claude", dir}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Missing) != 1 || res.Missing[0].Skill != "alpha" || res.Missing[0].AgentName != "claude" {
				t.Errorf("wrong-type target must be reported as missing skill content, got %+v", res.Missing)
			}
			if _, err := os.Lstat(filepath.Join(dir, "alpha")); !os.IsNotExist(err) {
				t.Errorf("wrong-type store target must never receive an agent link, got %v", err)
			}
		})
	}
}

// A skill whose name collides with an agent's reserved entry (SPEC rule
// 11) must never be linked into that agent, no matter its on/off value,
// and the collision must be reported rather than silently dropped
// (finding 2). Before the fix, ScanAgent filtered ".system" out of
// *actual* state but Desired/Diff never consulted Reserved() at all, so
// the filter applied to only one side of the comparison: Diff believed
// the skill permanently absent from a directory it could not see into,
// re-attempting CreateLink every run (swallowed as EEXIST), and unable to
// ever emit the RemoveLink needed to reclaim it. fakeAgent reserves
// ".system" regardless of its name (see scan_test.go).
func TestReconcileNeverLinksReservedName(t *testing.T) {
	s, _ := setupStore(t, ".system")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".system")); !os.IsNotExist(err) {
		t.Fatal("a name reserved by the agent must never be linked into its skills dir")
	}
	if len(res.Reserved) != 1 || res.Reserved[0].Skill != ".system" || res.Reserved[0].AgentName != "claude" {
		t.Fatalf("collision must be reported, got %+v", res.Reserved)
	}
}

// Desired has no direct test elsewhere in this package -- every other test
// exercises it only indirectly, through Reconcile. It is nonetheless the
// origin of two of Result's eight report categories (Reserved, Invalid) and
// of the precedence between them: a reserved-name collision is checked, and
// reported, strictly before validity is even considered. That ordering is
// not incidental -- Codex's real Reserved() entry, ".system", is itself not
// a validly-formed skill name (a leading dot fails skill.ValidateName), so
// checking validity first would relabel every such collision "invalid"
// instead of "reserved" (the ordering conflict round 3 found, and fixed,
// while implementing an unrelated change -- see Desired's own doc comment).
func TestDesiredReservedAndInvalidWithPrecedence(t *testing.T) {
	reserving := fakeAgent{"codex", t.TempDir()} // Reserved() == [".system"]

	t.Run("valid names land in desired at their effective value; an on reserved collision is reported and excluded", func(t *testing.T) {
		_, cfg := loadConfigFromYAML(t, `version: 1
skills:
  alpha: {enabled: true}
  beta: {enabled: true, overrides: {codex: false}}
  .system: {enabled: true}
`)

		desired, reserved, invalid := Desired(cfg, reserving)

		if on, ok := desired["alpha"]; !ok || !on {
			t.Fatalf("alpha must be desired on (global default, no override): %+v", desired)
		}
		if on, ok := desired["beta"]; !ok || on {
			t.Fatalf("beta must be desired off (agent override wins over global): %+v", desired)
		}
		if _, ok := desired[".system"]; ok {
			t.Fatalf("a name colliding with a reserved entry must never reach desired: %+v", desired)
		}
		if len(reserved) != 1 || reserved[0].Skill != ".system" || reserved[0].AgentName != "codex" || reserved[0].Type != ReportReserved {
			t.Fatalf("want exactly one ReportReserved for .system/codex, got %+v", reserved)
		}
		// Precedence: ".system" also fails skill.ValidateName (leading
		// dot), so LoadConfig isolated it and dropped it from SkillNames()
		// -- which is exactly how round 4's fix broke this ordering, since
		// the reserved branch below only ever looked at SkillNames(). The
		// reserved diagnosis is the more specific one and must win.
		if len(invalid) != 0 {
			t.Fatalf("a reserved collision must never also be reported invalid: %+v", invalid)
		}
	})

	t.Run("an off reserved collision is inert: excluded from desired, but not reported at all", func(t *testing.T) {
		_, cfg := loadConfigFromYAML(t, `version: 1
skills:
  .system: {enabled: false}
`)

		desired, reserved, invalid := Desired(cfg, reserving)

		if _, ok := desired[".system"]; ok {
			t.Fatalf("a name colliding with a reserved entry must never reach desired, on or off: %+v", desired)
		}
		if len(reserved) != 0 {
			t.Fatalf("an off reserved collision has no consequence for the user to be told about, and must not be reported: %+v", reserved)
		}
		if len(invalid) != 0 {
			t.Fatalf("an off reserved collision must not be reported invalid either: %+v", invalid)
		}
	})

	t.Run("an invalid, non-reserved name is not reported per agent: it is a property of the config", func(t *testing.T) {
		_, cfg := loadConfigFromYAML(t, `version: 1
skills:
  alpha: {enabled: true}
  ../evil: {enabled: true}
`)

		desired, reserved, invalid := Desired(cfg, reserving)

		if _, ok := desired["../evil"]; ok {
			t.Fatalf("an invalid name must never reach desired: %+v", desired)
		}
		if len(reserved) != 0 {
			t.Fatalf("an invalid, non-reserved name must not be reported reserved: %+v", reserved)
		}
		// A name LoadConfig isolated is one fact about one file, not one
		// fact per agent -- Reconcile folds it in once for the whole pass
		// (see TestReconcileReportsInvalidNameOncePerConfig).
		if len(invalid) != 0 {
			t.Fatalf("LoadConfig-isolated names are reported once per config by Reconcile, not per agent here: %+v", invalid)
		}
		if on, ok := desired["alpha"]; !ok || !on {
			t.Fatalf("a valid name alongside an invalid one must still be desired: %+v", desired)
		}
	})

	t.Run("defence in depth: an invalid name reaching SkillNames without LoadConfig is still reported per agent", func(t *testing.T) {
		// The only path that gets here is a Config built in memory --
		// AddSkill deliberately does not validate (see store/config.go).
		// Production always routes through LoadConfig, so this branch is
		// unreachable there; it is kept, and pinned, as defence in depth.
		_, cfg := setupStore(t, "alpha", "../evil")

		desired, reserved, invalid := Desired(cfg, reserving)

		if _, ok := desired["../evil"]; ok {
			t.Fatalf("an invalid name must never reach desired: %+v", desired)
		}
		if len(reserved) != 0 {
			t.Fatalf("an invalid, non-reserved name must not be reported reserved: %+v", reserved)
		}
		if len(invalid) != 1 || invalid[0].Skill != "../evil" || invalid[0].AgentName != "codex" || invalid[0].Type != ReportInvalid {
			t.Fatalf("want exactly one ReportInvalid for ../evil/codex, got %+v", invalid)
		}
		if invalid[0].LinkPath != "" || invalid[0].Target != "" {
			t.Fatalf("an invalid name must never be joined into a path, even in its own report: %+v", invalid[0])
		}
	})
}

// TestReconcileReportsInvalidNameOncePerConfig pins the other half of round
// 5's finding: a name LoadConfig isolated is a property of fu.yaml, not of
// any agent, so folding it inside the per-agent loop both duplicated it
// (one identical line per detected agent) and lost it entirely when no
// agent was detected at all -- a write command then said nothing about an
// entry it was silently ignoring.
func TestReconcileReportsInvalidNameOncePerConfig(t *testing.T) {
	yaml := `version: 1
skills:
  alpha: {enabled: true}
  ../evil: {enabled: true}
`
	t.Run("two agents, one line", func(t *testing.T) {
		s, _ := loadConfigFromYAML(t, yaml)
		if err := os.MkdirAll(filepath.Join(s.SkillsDir(), "alpha"), 0o755); err != nil {
			t.Fatal(err)
		}
		agents := []agent.Agent{
			fakeAgent{"codex", t.TempDir()},
			fakeAgentNoReserved{"claude", t.TempDir()},
		}

		res, err := Reconcile(s, agents)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Invalid) != 1 || res.Invalid[0].Skill != "../evil" {
			t.Fatalf("one bad key in fu.yaml is one diagnostic, not one per agent: %+v", res.Invalid)
		}
		if res.Invalid[0].AgentName != "" {
			t.Fatalf("a config-level report must not be attributed to any single agent: %+v", res.Invalid[0])
		}
	})

	t.Run("no agents detected, still reported", func(t *testing.T) {
		s, _ := loadConfigFromYAML(t, yaml)

		res, err := Reconcile(s, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Invalid) != 1 || res.Invalid[0].Skill != "../evil" {
			t.Fatalf("an invalid fu.yaml entry must be reported even with no agent detected: %+v", res.Invalid)
		}
	})

	t.Run("a reserved collision is diagnosed as reserved only, for the agent that reserves it", func(t *testing.T) {
		s, _ := loadConfigFromYAML(t, `version: 1
skills:
  .system: {enabled: true}
`)
		agents := []agent.Agent{
			fakeAgent{"codex", t.TempDir()},            // reserves .system
			fakeAgentNoReserved{"claude", t.TempDir()}, // does not
		}

		res, err := Reconcile(s, agents)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Reserved) != 1 || res.Reserved[0].Skill != ".system" || res.Reserved[0].AgentName != "codex" {
			t.Fatalf("want exactly one reserved report, for codex: %+v", res.Reserved)
		}
		// "reserved" is the specific, correct diagnosis; repeating it as
		// "invalid skill name" for the agent that merely does not reserve
		// it tells the user nothing true about what to do.
		if len(res.Invalid) != 0 {
			t.Fatalf("a name diagnosed reserved must not also be reported invalid: %+v", res.Invalid)
		}
	})
}

// A RemoveLink whose entry has simply *vanished* between Diff and apply is
// not a conflict: there is nothing left to remove, and nothing the user
// needs to be told. verifyFuLink answers false for a missing entry exactly
// as it does for one swapped out from under fu, and printResult renders
// that single answer as "occupied by unmanaged content" -- the opposite of
// what actually happened (round 5 finding). On a rebuild, where Diff emits
// RemoveLink and CreateLink for the same skill, that produced two reports
// for one skill in one pass: a "conflict" for the removal half, and a
// successfully created link from the other half.
func TestReconcileVanishedEntryIsNotReportedAsOccupied(t *testing.T) {
	removeOnRemoveLink := func(t *testing.T) beforeApply {
		return func(a agent.Agent, acts []Action) {
			for _, act := range acts {
				if act.Type == RemoveLink && act.Skill == "alpha" {
					if err := os.Remove(act.LinkPath); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}

	t.Run("plain removal: nothing left to remove, nothing to report", func(t *testing.T) {
		s, cfg := setupStore(t, "alpha")
		dir := t.TempDir()
		agents := []agent.Agent{fakeAgent{"claude", dir}}
		if _, err := reconcile(s, cfg, agents, nil); err != nil {
			t.Fatal(err)
		}
		cfg.SetEnabled("alpha", false)

		res, err := reconcile(s, cfg, agents, removeOnRemoveLink(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Conflicts) != 0 {
			t.Fatalf("an entry that vanished is not unmanaged content occupying a path: %+v", res.Conflicts)
		}
		if len(res.Failed) != 0 {
			t.Fatalf("an already-absent entry is not a failure either: %+v", res.Failed)
		}
	})

	t.Run("rebuild: the link is recreated and the skill is not also reported", func(t *testing.T) {
		s, cfg := setupStore(t, "alpha")
		dir := t.TempDir()
		agents := []agent.Agent{fakeAgent{"claude", dir}}
		// A fu-owned link recorded with a stale spelling of the store path
		// makes Diff emit RemoveLink + CreateLink for the same skill.
		link := staleSpellingLink(t, s, dir, "alpha")

		res, err := reconcile(s, cfg, agents, removeOnRemoveLink(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Conflicts) != 0 {
			t.Fatalf("a skill whose link was rebuilt in this same pass must not also be reported "+
				"as occupied by unmanaged content: %+v", res.Conflicts)
		}
		target, err := os.Readlink(link)
		if err != nil || target != filepath.Join(s.SkillsDir(), "alpha") {
			t.Fatalf("the rebuild's create half must still land: %v %q", err, target)
		}
	})
}

func verifyFuLink(root agentDirReader, name, storeSkillsDir string) (bool, error) {
	_, _, owned, err := inspectFuLink(root, name, storeSkillsDir)
	return owned, err
}

// verifyFuLink is a test-only adapter over the production inspection helper.
// RemoveLink, closing the TOCTOU window between the scan that produced
// Diff's decision and the moment of removal (DESIGN §2). A live race
// cannot be reproduced deterministically inside one synchronous
// Reconcile call — there is no externally observable gap in which to
// land a concurrent mutation — so the guard is exercised directly here
// against every way the entry could have changed underneath it since
// Diff last looked.
func TestVerifyFuLinkRejectsWhateverItIsNotStillOwned(t *testing.T) {
	root := t.TempDir()
	storeSkills := filepath.Join(root, "store", "skills")
	if err := os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// verifyFuLink works relative to an open descriptor for the agent
	// directory (round 7), so entries are addressed by name within it rather
	// than by path.
	agentRoot, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer agentRoot.Close()

	// Named "alpha", pointing at the skill "alpha" -- the only shape fu ever
	// creates, and since round 6 the only shape that verifies (see ownsLink).
	owned := filepath.Join(dir, "alpha")
	if err := os.Symlink(filepath.Join(storeSkills, "alpha"), owned); err != nil {
		t.Fatal(err)
	}
	if owned, err := verifyFuLink(agentRoot, "alpha", storeSkills); err != nil || !owned {
		t.Fatalf("intact fu-owned link must verify: owned=%v err=%v", owned, err)
	}

	// The same target under a name of the user's own choosing: not fu's, so
	// the removal re-check must refuse it just as the scan does.
	if err := os.Symlink(filepath.Join(storeSkills, "alpha"), filepath.Join(dir, "notes")); err != nil {
		t.Fatal(err)
	}
	if owned, err := verifyFuLink(agentRoot, "notes", storeSkills); err != nil || owned {
		t.Fatalf("a link whose name disagrees with its target's leaf is not fu's and must never "+
			"verify -- and saying so is not an error: owned=%v err=%v", owned, err)
	}

	// Swapped for a real foreign directory since Diff last looked.
	if err := os.MkdirAll(filepath.Join(dir, "swapped-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if owned, err := verifyFuLink(agentRoot, "swapped-dir", storeSkills); err != nil || owned {
		t.Fatalf("a real directory must never verify as a fu link: owned=%v err=%v", owned, err)
	}

	// Swapped for a symlink pointing outside the store.
	if err := os.Symlink("/elsewhere", filepath.Join(dir, "swapped-link")); err != nil {
		t.Fatal(err)
	}
	if owned, err := verifyFuLink(agentRoot, "swapped-link", storeSkills); err != nil || owned {
		t.Fatalf("a foreign symlink must never verify as a fu link: owned=%v err=%v", owned, err)
	}

	// Removed entirely since Diff last looked. This one reports an error --
	// and specifically a NotExist error, which reconcile treats as "nothing
	// to do" rather than as a conflict or a failure (round 8 finding).
	goneOwned, goneErr := verifyFuLink(agentRoot, "gone", storeSkills)
	if goneOwned {
		t.Fatal("a path that no longer exists must never verify")
	}
	if !os.IsNotExist(goneErr) {
		t.Fatalf("an absent entry must be distinguishable from a genuine inspection failure, got %v", goneErr)
	}
}

// Test gap T1: deleting verifyFuLink's re-check from Reconcile's
// RemoveLink case failed no test, because nothing exercised the guard
// through Reconcile's actual call site -- only the standalone function
// above (TestVerifyFuLinkRejectsWhateverItIsNotStillOwned) did. The
// unexported reconcile/beforeApply seam lets a test land a mutation in
// the exact gap DESIGN §2 names: after Diff has decided to remove a
// link, but before Reconcile acts on that decision, the entry is swapped
// for real foreign content, standing in for a concurrent actor racing
// the window between scan and removal. The re-check must refuse to
// remove what is no longer fu's.
func TestReconcileRemoveLinkReVerifiesBeforeRemoving(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	// First pass materializes the link normally.
	if _, err := reconcile(s, cfg, agents, nil); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alpha")
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("setup: link must exist before the race is simulated")
	}

	// Disabling the skill makes the next Diff decide RemoveLink for
	// "alpha". The hook then races that decision, swapping in a plain
	// foreign file: os.Remove can delete a file just as easily as a
	// symlink, so only the ownership re-check (not some incidental
	// "directory not empty" error) stands between this content and
	// deletion.
	cfg.SetEnabled("alpha", false)
	raced := false
	const swappedContent = "mine"
	hook := beforeApply(func(a agent.Agent, acts []Action) {
		for _, act := range acts {
			if act.Type != RemoveLink || act.Skill != "alpha" {
				continue
			}
			if err := os.Remove(act.LinkPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(act.LinkPath, []byte(swappedContent), 0o644); err != nil {
				t.Fatal(err)
			}
			raced = true
		}
	})

	res, err := reconcile(s, cfg, agents, hook)
	if err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("test setup error: the hook never found the expected RemoveLink action")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Skill != "alpha" {
		t.Fatalf("content swapped in during the race must be reported as a conflict, not silently removed: %+v", res)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("content swapped in during the race must survive: %v", err)
	}
	if string(got) != swappedContent {
		t.Fatalf("content swapped in during the race must survive untouched, got %q", got)
	}
}

func TestReconcileRetiresApprovedLinkBeforeRemoval(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	if _, err := reconcile(s, cfg, agents, nil); err != nil {
		t.Fatal(err)
	}
	cfg.SetEnabled("alpha", false)

	const foreign = "foreign content"
	replaced := false
	h := reconcileHooks{
		beforeLinkRetire: func(_ agent.Agent, act Action) {
			if act.Skill != "alpha" {
				return
			}
			if err := os.Remove(act.LinkPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(act.LinkPath, []byte(foreign), 0o644); err != nil {
				t.Fatal(err)
			}
			replaced = true
		},
	}
	res, err := reconcileWithHooks(s, cfg, agents, h)
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("test setup error: retirement boundary hook did not run")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Skill != "alpha" {
		t.Fatalf("replacement at the retirement boundary must be a conflict: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(dir, "alpha"))
	if err != nil {
		t.Fatalf("foreign replacement must remain at its original name: %v", err)
	}
	if string(got) != foreign {
		t.Fatalf("foreign replacement changed: got %q", got)
	}
}

// A precondition violation on one agent (symlinked skills dir) must not
// prevent other agents in the same Reconcile pass from being processed.
func TestReconcileSkippedAgentDoesNotBlockOthers(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	skipDir := filepath.Join(base, "linkdir")
	if err := os.Symlink(target, skipDir); err != nil {
		t.Fatal(err)
	}
	okDir := t.TempDir()

	agents := []agent.Agent{
		fakeAgent{"broken-agent", skipDir},
		fakeAgent{"claude", okDir},
	}
	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "broken-agent" {
		t.Fatalf("exactly the symlinked agent must be skipped: %+v", res)
	}
	link := filepath.Join(okDir, "alpha")
	want := filepath.Join(s.SkillsDir(), "alpha")
	if target, err := os.Readlink(link); err != nil || target != want {
		t.Fatalf("later agent must still be reconciled after an earlier one is skipped: %v %q", err, target)
	}
}

// Critical finding 1 (round 3)'s own reproduction, end to end through
// Reconcile rather than ScanAgent's classification alone (see
// TestScanAgentUserSymlinkChainIntoStoreIsForeign in scan_test.go for that
// half): a user-built symlink chain landing inside the store must survive
// an unrelated write command byte for byte, and be reported
// (ReportForeign), never silently removed while every field of Result
// stays empty. Reproduced against the compiled binary pre-fix exactly as
// the reviewer described: with `~/mylink -> $FU_HOME/store/skills/alpha`
// and `~/.claude/skills/notes -> ~/mylink` both in place (fu.yaml has no
// "notes" entry at all), `fu new beta` printed only "created beta", exited
// 0, and "notes" was gone from `~/.claude/skills` afterward.
func TestReconcileNeverTouchesUserSymlinkChainIntoStore(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	// The user's own hop, outside any agent directory -- fu never reads
	// this path itself, only the second hop planted inside dir below.
	home := t.TempDir()
	hop := filepath.Join(home, "mylink")
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), hop); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "notes")
	if err := os.Symlink(hop, link); err != nil {
		t.Fatal(err)
	}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	target, rerr := os.Readlink(link)
	if rerr != nil || target != hop {
		t.Fatalf("the user's own symlink chain must survive reconcile byte for byte: err=%v got %q want %q", rerr, target, hop)
	}
	if len(res.Foreign) != 1 || res.Foreign[0].Skill != "notes" || res.Foreign[0].AgentName != "claude" {
		t.Fatalf("the chain must be reported foreign, not silently dropped from an empty Result: %+v", res)
	}
	if len(res.Conflicts) != 0 || len(res.Failed) != 0 {
		t.Fatalf("no conflicts or failures expected: %+v", res)
	}
	// The legitimate skill alongside it must still be linked normally --
	// this finding is about the foreign chain, not about breaking ordinary
	// reconcile behavior for everything else in the same pass.
	want := filepath.Join(s.SkillsDir(), "alpha")
	if target, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil || target != want {
		t.Fatalf("a legitimate skill listed alongside the foreign chain must still be reconciled: %v %q", err, target)
	}
}

// Finding I3: a genuine error scanning one agent (as opposed to the
// ParentIsSymlink precondition the previous test covers) must likewise
// not prevent Reconcile from processing the remaining agents. Before the
// fix, ScanAgent's error for "broken-agent" (its skills dir is a plain
// file, so os.ReadDir fails with "not a directory") made reconcile
// return immediately with that error, so "claude" -- with nothing wrong
// with it -- never got its link either, and Reconcile's own caller
// (NewSkill etc.) would see a hard error despite the config entry and
// commit already being durable (see ops_test.go's
// TestNewSkillIsolatesBrokenAgentButReportsOperationFailure for that angle
// end to end).
//
// Round 2 finding 4 revised this test's error expectation. I3's isolation
// (a broken agent must not starve a healthy one, and must not abort the
// pass) is unchanged and still asserted below. What changed is Reconcile's
// *own* error return once every agent has been processed: it used to stay
// nil even with res.Failed populated, which made Run/the CLI command
// report success (and exit 0) despite a genuine operation failure -- a
// script piping only stdout to /dev/null saw nothing wrong. Reconcile now
// returns ErrOperationFailed whenever Failed is non-empty, so the isolation
// this test is actually named for (the healthy agent still gets linked)
// and the error signal are both correct at once.
func TestReconcileFailedAgentScanDoesNotStarveOthers(t *testing.T) {
	s, _ := setupStore(t, "alpha")
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

	res, err := Reconcile(s, agents)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("a per-agent scan failure must surface as ErrOperationFailed (finding 4), got %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.AgentName != "broken-agent" || res.Failed[0].Err == nil {
		t.Fatalf("broken agent's scan failure must be recorded in Failed, got %+v", res.Failed)
	}
	link := filepath.Join(okDir, "alpha")
	want := filepath.Join(s.SkillsDir(), "alpha")
	if target, err := os.Readlink(link); err != nil || target != want {
		t.Fatalf("a healthy agent listed after a broken one must still be reconciled despite the other agent's failure: %v %q", err, target)
	}
}

// Finding I4: os.Symlink's EEXIST must be reported as a conflict, not
// silently discarded. This is not only a live race window: on macOS's
// case-insensitive filesystem it is routine for a differently-cased
// foreign entry (e.g. a real directory "Alpha") to already occupy the
// exact path Diff decided to CreateLink "alpha" into, since Diff's
// desired-vs-actual lookup is case-sensitive. The beforeApply hook
// reproduces the same "something now occupies this path that Diff did
// not see" shape deterministically on any platform/filesystem, standing
// in for either trigger without depending on case-insensitivity.
func TestReconcileCreateLinkEEXISTReportedAsConflict(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	raced := false
	const foreignContent = "occupied first"
	hook := beforeApply(func(a agent.Agent, acts []Action) {
		for _, act := range acts {
			if act.Type != CreateLink || act.Skill != "alpha" {
				continue
			}
			if err := os.WriteFile(act.LinkPath, []byte(foreignContent), 0o644); err != nil {
				t.Fatal(err)
			}
			raced = true
		}
	})

	res, err := reconcile(s, cfg, agents, hook)
	if err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("test setup error: the hook never found the expected CreateLink action")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Skill != "alpha" {
		t.Fatalf("EEXIST from os.Symlink must be reported as a conflict, not silently dropped: %+v", res)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("EEXIST must not also be recorded as an unexpected failure: %+v", res.Failed)
	}
	got, err := os.ReadFile(filepath.Join(dir, "alpha"))
	if err != nil {
		t.Fatalf("content that raced in ahead of the link must survive: %v", err)
	}
	if string(got) != foreignContent {
		t.Fatalf("raced-in content must survive untouched, got %q", got)
	}
}

// Finding I6 composed with I3's isolation: an agent with no usable
// skills directory (empty SkillsDir, e.g. HOME unset for a real
// adapter) must be isolated into Failed like any other ScanAgent error,
// not starve a healthy agent listed alongside it. Round 2 finding 4: the
// error return is now ErrOperationFailed (see the comment on
// TestReconcileFailedAgentScanDoesNotStarveOthers above for why).
func TestReconcileEmptySkillsDirIsolatedNotFatal(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	okDir := t.TempDir()
	agents := []agent.Agent{
		fakeAgent{"broken-agent", ""},
		fakeAgent{"claude", okDir},
	}

	res, err := Reconcile(s, agents)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("an isolated per-agent failure must surface as ErrOperationFailed (finding 4), got %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.AgentName != "broken-agent" {
		t.Fatalf("empty SkillsDir must be isolated into Failed, got %+v", res.Failed)
	}
	want := filepath.Join(s.SkillsDir(), "alpha")
	if target, err := os.Readlink(filepath.Join(okDir, "alpha")); err != nil || target != want {
		t.Fatalf("a healthy agent must still be reconciled despite the other agent's failure: %v %q", err, target)
	}
}

// Round 2 finding 3: an invalid skill name reaching Reconcile end to end
// must never plant a link outside the agent's skills directory. Reproduced
// against the compiled binary: a hand-edited fu.yaml entry literally named
// "../evil" (with real content already sitting at the store-side path the
// escape resolves to) made `fu new beta` create
// ~/.claude/evil -> $FU_HOME/store/evil -- one level *above*
// ~/.claude/skills -- while printing only "created beta" and exiting 0.
// setupStore's own os.MkdirAll(filepath.Join(s.SkillsDir(), name), ...)
// reproduces the escaped store-side target exactly the way the hand-edit
// did (filepath.Join cleans "skills/../evil" down to "evil", i.e. a real
// directory one level above skills/ in the store itself) -- config.AddSkill
// performs no name validation of its own, matching a hand-edited fu.yaml.
func TestReconcileRejectsInvalidSkillNamePlantedViaFuYaml(t *testing.T) {
	s, _ := setupStore(t, "../evil", "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Invalid) != 1 || res.Invalid[0].Skill != "../evil" || res.Invalid[0].AgentName != "" {
		t.Fatalf("want exactly one config-level Invalid entry for \"../evil\", got %+v", res.Invalid)
	}
	if res.Invalid[0].LinkPath != "" || res.Invalid[0].Target != "" {
		t.Fatalf("an invalid name must never be joined into a reported path either, got %+v", res.Invalid[0])
	}
	// The one-level-up escape target must never receive a link: neither at
	// the parent of the agent's skills directory...
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dir), "evil")); !os.IsNotExist(err) {
		t.Fatal("an invalid name must never plant a link outside the agent's skills directory")
	}
	// ...nor anywhere under the skills directory itself.
	if _, err := os.Lstat(filepath.Join(dir, "evil")); !os.IsNotExist(err) {
		t.Fatal("an invalid name must not be linked under the skills directory either")
	}
	if _, err := os.Lstat(filepath.Join(dir, "../evil")); !os.IsNotExist(err) {
		t.Fatal("an invalid name must never be used to construct any link path at all")
	}
	// A legitimate, validly-named skill alongside it must be unaffected.
	want := filepath.Join(s.SkillsDir(), "alpha")
	if target, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil || target != want {
		t.Fatalf("a valid skill listed alongside an invalid name must still be reconciled: %v %q", err, target)
	}
}

// Critical finding 2 (round 3)'s reverse direction, a regression in round 2
// finding 3's own fix above: that test (TestReconcileRejectsInvalidSkillNamePlantedViaFuYaml)
// pins the creation direction only -- it never plants a disk entry for the
// invalid name, so it cannot exercise removal at all. Here a genuine fu
// link already sits on disk under an invalid name before this pass ever
// runs, standing in for one written by an older fu (or a future
// clone/pull) before this build's naming rules applied to it, and fu.yaml
// itself records that name disabled -- the reviewer's exact reproduction:
// `Beta: {enabled: false}` in fu.yaml, a genuine
// `~/.claude/skills/Beta -> $FU_HOME/store/skills/Beta` link already in
// place. Reproduced against the compiled binary pre-fix: `fu disable
// alpha` (an unrelated skill) printed `invalid: claude: skill name "Beta"
// fails validation and will never be linked` and exited 0, yet `ls
// ~/.claude/skills` still showed Beta afterward and `fu list` reported it
// "off" while the disk still had it linked -- the message contradicted
// itself the moment it was printed. The stray link must actually be
// removed by this same pass, not merely reported as if it no longer
// mattered.
func TestReconcileRemovesFuLinkRecordedUnderInvalidName(t *testing.T) {
	s, cfg := setupStore(t, "alpha", "Beta")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	// "Beta" fails skill.ValidateName (uppercase); setupStore registers it
	// enabled by construction like any other name, so disable it explicitly
	// to match the reviewer's exact fu.yaml ("Beta: {enabled: false}").
	cfg.SetEnabled("Beta", false)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// The genuine fu link, already on disk before this pass ever runs --
	// its creation goes through neither Diff nor Desired, the same way a
	// link an older fu created, or one a future clone/pull writes, would
	// already be there before this build ever looks at it.
	link := filepath.Join(dir, "Beta")
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "Beta"), link); err != nil {
		t.Fatal(err)
	}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("a genuine fu link recorded under an invalid name must actually be removed, not left permanently unremovable")
	}
	if len(res.Invalid) != 1 || res.Invalid[0].Skill != "Beta" || res.Invalid[0].AgentName != "" {
		t.Fatalf("the invalid name must still be reported once at config scope, got %+v", res.Invalid)
	}
	if res.Invalid[0].LinkPath != "" || res.Invalid[0].Target != "" {
		t.Fatalf("an invalid name must never be joined into a reported path either, got %+v", res.Invalid[0])
	}
	// Removal must go through the ordinary RemoveLink path, not surface as
	// a conflict or an unexpected failure.
	if len(res.Conflicts) != 0 || len(res.Failed) != 0 {
		t.Fatalf("no conflicts or failures expected: %+v", res)
	}
	// A legitimate, validly-named skill alongside it must be unaffected.
	want := filepath.Join(s.SkillsDir(), "alpha")
	if target, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil || target != want {
		t.Fatalf("a valid skill listed alongside the invalid name must still be reconciled: %v %q", err, target)
	}
}

// Round 2 finding 5: no existing test ever created a link recorded with a
// non-canonical spelling of the store path, so the whole class finding 1
// describes was unguarded. TestToggleVisibleAcrossHomeSpellings
// (internal/cli/toggle_test.go) looks like cross-spelling coverage, but its
// link is created by `fu new` *after* store.Home()'s symlink resolution is
// already in effect, so the link's recorded target is canonical the moment
// it is written -- the asymmetry finding 1 actually fixed (a link recorded
// while some ancestor of $FU_HOME was still an ordinary directory, compared
// later against a freshly resolved base once that ancestor becomes a
// symlink) was never exercised by it.
//
// This test constructs the link by hand so its recorded target spells the
// store path through a symlinked alias rather than through s.SkillsDir()'s
// own spelling, standing in for exactly that "written before, read after"
// gap without going through store.Home() or FU_HOME/HOME at all. It pins
// the behavior the finding names -- ownership recognition must not depend
// on which of two physically identical spellings a link's target happens to
// use -- covering both matrix arms that hinge on it: a skill still enabled
// (the "on && present && KindFuLink" arm; misclassifying this as foreign
// would raise a false ReportConflict) and finding 1's own reproduction, a
// disable (the "!on && present && KindFuLink" arm; misclassifying this as
// foreign is exactly what let disable report success while leaving the
// link in place). This pins the behavior itself rather than re-asserting
// only the one reported repro.
func TestReconcileRecognizesLinkRecordedWithNonCanonicalStoreSpelling(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	// An alias standing in for an ancestor of $FU_HOME (e.g. ~/.fu) later
	// becoming a symlink to its own physical location, the way a dotfiles
	// manager migration does: it resolves to the exact same physical store
	// s.SkillsDir() already points at, just spelled differently.
	//
	// The alias stands in for $FU_HOME itself, not $FU_HOME/store: "store"
	// and "skills" are appended as literal, unresolved components after
	// it, the same as store.Dir/SkillsDir always append them after
	// whatever $FU_HOME resolves to, whatever alias it was reached
	// through. This is deliberate (round 4): ownsLink's directory-position
	// gate (dirHasStoreSkillsSuffix in scan.go) only resolves a candidate
	// target's directory when its raw text already ends with
	// storeSkillsDir's own trailing components -- true here regardless of
	// how deep the alias hop sits, since "store" and "skills" are still
	// spelled out literally past it. An earlier version of this fixture
	// aliased s.Dir() (i.e. $FU_HOME/store) directly from a differently
	// named symlink, collapsing $FU_HOME and "store" into one hop; that
	// shape is exactly what round 4 must now treat as foreign -- it is not
	// actually distinguishable from a user's own store-root alias (e.g.
	// `~/mystore -> $FU_HOME/store`) -- so it is no longer a valid stand-in
	// for "an ancestor of $FU_HOME became a symlink" and had to be
	// corrected here rather than left pinning that no-longer-intended
	// behavior.
	alias := filepath.Join(t.TempDir(), "aliased-fu-home")
	if err := os.Symlink(s.Home, alias); err != nil {
		t.Fatal(err)
	}
	nonCanonicalTarget := filepath.Join(alias, "store", "skills", "alpha")

	link := filepath.Join(dir, "alpha")
	if err := os.Symlink(nonCanonicalTarget, link); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.SkillsDir(), "alpha")

	// A pass with the skill still enabled must recognize the link as fu's
	// own, not foreign (no conflict, no failure). Diff's separate
	// target-freshness check (e.LinkTarget != want) is its own plain string
	// comparison, unrelated to ownsLink's ownership check this finding
	// fixes, so it still treats the non-canonical spelling as "stale" and
	// rebuilds -- but the rebuild path re-verifies ownership before
	// removing (verifyFuLink) and then recreates with the canonical
	// spelling, so the net effect is a one-time, self-correcting rebuild
	// converging on the canonical form, not data loss or a misclassified
	// conflict. Asserting that convergence here (rather than "unchanged")
	// is itself part of confirming symmetric resolution is a full repair.
	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 || len(res.Foreign) != 0 || len(res.Failed) != 0 {
		t.Fatalf("a link recorded via a non-canonical store spelling must be recognized as fu's own, not foreign: %+v", res)
	}
	if target, err := os.Readlink(link); err != nil || target != want {
		t.Fatalf("the link must converge on the canonical spelling: err=%v got %q want %q", err, target, want)
	}
	// A second pass over the now-canonical link is a true no-op.
	res, err = Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 || len(res.Foreign) != 0 || len(res.Failed) != 0 {
		t.Fatalf("once converged, a further pass must reconcile cleanly: %+v", res)
	}

	// Finding 1's own reproduction, at the behavior level: re-plant the
	// link with the non-canonical spelling once more (independent of the
	// self-correction above) and disable the skill. The link must still be
	// recognized as fu's own and removed, not silently left in place while
	// the caller reports success.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nonCanonicalTarget, link); err != nil {
		t.Fatal(err)
	}
	cfg.SetEnabled("alpha", false)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	res, err = Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 || len(res.Foreign) != 0 || len(res.Failed) != 0 {
		t.Fatalf("disabling a link recorded via a non-canonical store spelling must not report it as foreign: %+v", res)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("a disabled skill's link must be removed even when its recorded target spells the store path non-canonically")
	}
}

// TestReconcileOperatesOnTheDirectoryItChecked is round 7's third Critical.
// ScanAgent established that the agent's skills directory is a real
// directory and not a symlink (SPEC rule 10), but every operation that
// followed re-opened it by pathname: MkdirAll, Symlink, the ownership
// re-check, and Remove. A namespace replacement landing between the scan
// and the apply meant those operations no longer addressed the directory
// that had passed the precondition -- creation could land in a foreign
// directory, and removal could delete an entry fu never classified.
//
// The beforeApply seam lands the replacement in exactly that window. With
// operations anchored to a descriptor opened for the checked directory,
// the swap is irrelevant to what fu touches: it keeps acting on the
// directory it validated, and the attacker's replacement is left alone.
func TestReconcileOperatesOnTheDirectoryItChecked(t *testing.T) {
	t.Run("removal does not follow a replaced parent", func(t *testing.T) {
		s, cfg := setupStore(t, "alpha")
		base := t.TempDir()
		dir := filepath.Join(base, "skills")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		agents := []agent.Agent{fakeAgent{"claude", dir}}
		if _, err := reconcile(s, cfg, agents, nil); err != nil {
			t.Fatal(err)
		}

		// The attacker's own directory, holding at the exact name fu is
		// about to remove an entry that would *pass* the ownership re-check
		// if fu looked at it: a symlink to the same store-side skill. A
		// plain file here would survive for the wrong reason -- verifyFuLink
		// would reject it on type alone -- and prove nothing about which
		// directory fu is operating in.
		foreign := filepath.Join(base, "foreign")
		if err := os.MkdirAll(foreign, 0o755); err != nil {
			t.Fatal(err)
		}
		decoy := filepath.Join(foreign, "alpha")
		if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), decoy); err != nil {
			t.Fatal(err)
		}

		cfg.SetEnabled("alpha", false) // Diff will decide RemoveLink for alpha
		swapped := false
		hook := beforeApply(func(_ agent.Agent, acts []Action) {
			for _, act := range acts {
				if act.Type != RemoveLink || act.Skill != "alpha" {
					continue
				}
				// Replace the *validated directory itself* with a symlink to
				// the attacker's, after the scan and before the apply.
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, dir); err != nil {
					t.Fatal(err)
				}
				swapped = true
			}
		})

		if _, err := reconcile(s, cfg, agents, hook); err != nil && !errors.Is(err, ErrOperationFailed) {
			t.Fatalf("unexpected error: %v", err)
		}
		if !swapped {
			t.Fatal("test setup error: the hook never found the expected RemoveLink action")
		}
		if _, err := os.Lstat(decoy); err != nil {
			t.Fatalf("an entry inside a directory swapped in after the scan must never be removed, "+
				"however fu-owned it looks: %v", err)
		}
	})

	t.Run("creation does not follow a replaced parent", func(t *testing.T) {
		s, cfg := setupStore(t, "alpha")
		base := t.TempDir()
		dir := filepath.Join(base, "skills")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		agents := []agent.Agent{fakeAgent{"claude", dir}}

		foreign := filepath.Join(base, "foreign")
		if err := os.MkdirAll(foreign, 0o755); err != nil {
			t.Fatal(err)
		}
		swapped := false
		hook := beforeApply(func(_ agent.Agent, acts []Action) {
			for _, act := range acts {
				if act.Type != CreateLink || act.Skill != "alpha" {
					continue
				}
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, dir); err != nil {
					t.Fatal(err)
				}
				swapped = true
			}
		})

		if _, err := reconcile(s, cfg, agents, hook); err != nil && !errors.Is(err, ErrOperationFailed) {
			t.Fatalf("unexpected error: %v", err)
		}
		if !swapped {
			t.Fatal("test setup error: the hook never found the expected CreateLink action")
		}
		ents, err := os.ReadDir(foreign)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("nothing may be created inside a directory swapped in after the scan: %v", ents)
		}
	})
}

// TestMkdirAllAnchored is round 8's second Critical, agent-side. When the
// skills directory does not exist, reconcile created it with a
// pathname-based os.MkdirAll -- before OpenCheckedDir had any descriptor to
// anchor to. A symlink appearing at a component that did not exist when the
// scan looked redirected the creation, and everything the pass then did
// happened inside a directory nothing had checked.
//
// The distinction that makes this fixable without breaking real setups:
// components at or above the deepest *existing* ancestor may perfectly well
// be symlinks -- `~/.claude -> ~/dotfiles/claude` is an ordinary dotfiles
// arrangement, and the path was handed to fu that way. Components *below*
// it did not exist a moment ago, so anything appearing there is new, and fu
// has no reason to follow it.
func TestMkdirAllAnchored(t *testing.T) {
	t.Run("creates the missing components", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "cfgroot", "skills")
		if err := mkdirAllAnchored(target); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(target)
		if err != nil || !fi.IsDir() {
			t.Fatalf("the directory must exist as a real directory: %v", err)
		}
	})

	t.Run("a symlinked ancestor that already exists is followed, as the user arranged", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "dotfiles-claude")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(base, "cfgroot")); err != nil {
			t.Fatal(err)
		}
		if err := mkdirAllAnchored(filepath.Join(base, "cfgroot", "skills")); err != nil {
			t.Fatalf("a symlinked agent config directory is an ordinary dotfiles setup: %v", err)
		}
		if fi, err := os.Lstat(filepath.Join(real, "skills")); err != nil || !fi.IsDir() {
			t.Fatalf("the directory must be created through the user's own link: %v", err)
		}
	})

	t.Run("a symlink appearing below the anchor is refused", func(t *testing.T) {
		base := t.TempDir()
		foreign := filepath.Join(base, "foreign")
		if err := os.MkdirAll(foreign, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(base, "cfgroot", "skills")

		// Resolve the anchor first, as reconcile does...
		anchor, rest, anchorInfo, err := deepestExistingAncestor(target)
		if err != nil {
			t.Fatal(err)
		}
		if anchor != base {
			t.Fatalf("setup: want %s as the anchor, got %s", base, anchor)
		}
		// ...then the race: a component that did not exist becomes a symlink
		// into somebody else's tree before the creation happens.
		if err := os.Symlink(foreign, filepath.Join(base, "cfgroot")); err != nil {
			t.Fatal(err)
		}

		if err := mkdirAllUnder(anchor, rest, anchorInfo); err == nil {
			t.Error("a symlink that appeared below the anchor must not be traversed")
		}
		ents, err := os.ReadDir(foreign)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("nothing may be created inside somebody else's tree: %v", ents)
		}
	})

	t.Run("a relative in-root symlink appearing below the anchor is refused", func(t *testing.T) {
		base := t.TempDir()
		foreign := filepath.Join(base, "foreign")
		if err := os.MkdirAll(foreign, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(base, "cfgroot", "skills")

		anchor, rest, anchorInfo, err := deepestExistingAncestor(target)
		if err != nil {
			t.Fatal(err)
		}
		if anchor != base {
			t.Fatalf("setup: want %s as the anchor, got %s", base, anchor)
		}
		// Unlike an absolute target, this stays beneath os.Root and is
		// followed by Root.MkdirAll unless each new component is opened with
		// no-follow semantics.
		if err := os.Symlink("foreign", filepath.Join(base, "cfgroot")); err != nil {
			t.Fatal(err)
		}

		if err := mkdirAllUnder(anchor, rest, anchorInfo); err == nil {
			t.Fatal("a relative symlink below the checked anchor must not be traversed")
		}
		if _, err := os.Lstat(filepath.Join(foreign, "skills")); !os.IsNotExist(err) {
			t.Fatalf("nothing may be created through the relative link, got %v", err)
		}
		if _, err := os.Lstat(filepath.Join(foreign, "skills", "alpha")); !os.IsNotExist(err) {
			t.Fatalf("no agent link may be delivered through the relative link, got %v", err)
		}
	})

	t.Run("replacement of the inspected anchor is refused", func(t *testing.T) {
		base := t.TempDir()
		anchorPath := filepath.Join(base, "cfgroot")
		if err := os.Mkdir(anchorPath, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(anchorPath, "skills")
		anchor, rest, anchorInfo, err := deepestExistingAncestor(target)
		if err != nil {
			t.Fatal(err)
		}
		if anchor != anchorPath || rest != "skills" {
			t.Fatalf("setup: got anchor=%q rest=%q", anchor, rest)
		}

		foreign := filepath.Join(base, "foreign")
		if err := os.Mkdir(foreign, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(anchorPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(foreign, anchorPath); err != nil {
			t.Fatal(err)
		}

		if err := mkdirAllUnder(anchor, rest, anchorInfo); err == nil {
			t.Fatal("an anchor replaced after inspection must not be trusted by pathname")
		}
		ents, err := os.ReadDir(foreign)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("nothing may be created through the replacement anchor: %v", ents)
		}
	})
}

func TestCreateAndScanAgentDirRefusesReplacementAfterCreation(t *testing.T) {
	base := t.TempDir()
	agentParent := filepath.Join(base, "cfgroot")
	agentSkills := filepath.Join(agentParent, "skills")
	a := fakeAgent{"claude", agentSkills}
	storeSkills := filepath.Join(base, "store-skills")
	if err := os.MkdirAll(storeSkills, 0o755); err != nil {
		t.Fatal(err)
	}

	foreign := filepath.Join(base, "foreign")
	original := filepath.Join(base, "created-by-fu")
	_, err := createAndScanAgentDir(a, storeSkills, func() {
		if err := os.Rename(agentParent, original); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(foreign, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The relative target stays beneath the same checked ancestor. A
		// pathname-only rescan would accept foreign/skills as a real directory.
		if err := os.Symlink("foreign", agentParent); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("a replacement between creation and rescan must be refused")
	}
	if _, err := os.Lstat(filepath.Join(foreign, "skills", "alpha")); !os.IsNotExist(err) {
		t.Fatalf("no agent link may be delivered into the replacement directory, got %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(original, "skills")); err != nil || !fi.IsDir() {
		t.Fatalf("the directory fu actually created must remain intact: %v", err)
	}
}

// TestRemovalIOErrorIsFailedNotConflict is round 8's error-classification
// finding. verifyFuLink collapsed every inspection error into "not owned",
// and reconcile turned that into a Conflict -- an expected, actionable
// state that still exits 0. A permission or I/O failure at the removal
// boundary was therefore reported as "occupied by unmanaged content" while
// the link stayed active and the command claimed success. The user is told
// the disable took effect in new sessions; it did not.
func TestRemovalIOErrorIsFailedNotConflict(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission bits this test relies on")
	}
	s, cfg := setupStore(t, "alpha")
	base := t.TempDir()
	dir := filepath.Join(base, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := reconcile(s, cfg, agents, nil); err != nil {
		t.Fatal(err)
	}

	cfg.SetEnabled("alpha", false) // Diff decides RemoveLink
	hook := beforeApply(func(_ agent.Agent, acts []Action) {
		for _, act := range acts {
			if act.Type == RemoveLink && act.Skill == "alpha" {
				// Unreadable directory: inspecting the entry now fails with
				// EACCES rather than answering "present" or "absent".
				if err := os.Chmod(dir, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(dir, 0o755) })
			}
		}
	})

	res, err := reconcile(s, cfg, agents, hook)
	if len(res.Conflicts) != 0 {
		t.Errorf("a filesystem error is not unmanaged content occupying a path: %+v", res.Conflicts)
	}
	if len(res.Failed) == 0 {
		t.Error("a filesystem error at the removal boundary must be reported as a failure")
	}
	if !errors.Is(err, ErrOperationFailed) {
		t.Errorf("and must make the command exit non-zero, got %v", err)
	}
}

// TestInvalidNameSuppressionFollowsWhoActuallyReportsIt is round 8's
// diagnostic finding. The config-level Invalid report was suppressed
// whenever *any* detected adapter reserved the name, but the per-agent
// Reserved report is only emitted for an adapter where the entry is
// effective. Override the reserving adapter off while a non-reserving one
// stays on, and neither fires: the entry is silently ignored, and the user
// has no explanation for why an enabled skill was never reconciled.
//
// Suppression has to follow whether something else actually reports it.
func TestInvalidNameSuppressionFollowsWhoActuallyReportsIt(t *testing.T) {
	// ".system" is codex's reserved marker and also fails name validation,
	// so it is both reserved and invalid -- the case the two reports have to
	// divide between them.
	agentsOf := func(t *testing.T) []agent.Agent {
		return []agent.Agent{
			fakeAgent{"codex", t.TempDir()},            // reserves .system
			fakeAgentNoReserved{"claude", t.TempDir()}, // does not
		}
	}
	for _, tc := range []struct {
		name         string
		yaml         string
		wantReserved int
		wantInvalid  int
		why          string
	}{
		{
			name:         "on everywhere: the reserving agent reports it",
			yaml:         "version: 1\nskills:\n  .system: {enabled: true}\n",
			wantReserved: 1, wantInvalid: 0,
			why: "codex reports it as reserved, which is the specific diagnosis; repeating it as invalid says nothing true",
		},
		{
			name:         "off everywhere: inert, nothing to say",
			yaml:         "version: 1\nskills:\n  .system: {enabled: false}\n",
			wantReserved: 0, wantInvalid: 0,
			why: "nothing was ever desired for it, so there is no consequence to report",
		},
		{
			name:         "round 8: reserving agent overridden off, non-reserving agent on",
			yaml:         "version: 1\nskills:\n  .system:\n    enabled: true\n    overrides:\n      codex: false\n",
			wantReserved: 0, wantInvalid: 1,
			why: "claude wants it and will not get it; with codex silent, nothing else would ever say why",
		},
		{
			name:         "reserving agent on, non-reserving agent overridden off",
			yaml:         "version: 1\nskills:\n  .system:\n    enabled: true\n    overrides:\n      claude: false\n",
			wantReserved: 1, wantInvalid: 0,
			why: "codex still reports it as reserved",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := loadConfigFromYAML(t, tc.yaml)
			res, err := Reconcile(s, agentsOf(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Reserved) != tc.wantReserved {
				t.Errorf("want %d reserved report(s), got %+v -- %s", tc.wantReserved, res.Reserved, tc.why)
			}
			if len(res.Invalid) != tc.wantInvalid {
				t.Errorf("want %d invalid report(s), got %+v -- %s", tc.wantInvalid, res.Invalid, tc.why)
			}
		})
	}
}

// TestReconcileConflictNamesRetiredPathWhenRestoreFails pins the fix for round
// 18 finding M10, which shipped with no test: nothing set or asserted
// Action.Target on a conflict, so deleting the assignment left the suite
// green. This is the one path where fu moved its own link aside and could not
// put it back because the original name was reoccupied. The object is then
// parked under a dot-name, and the retired path is the user's only pointer to
// where their content went -- without it the report reads "occupied by
// unmanaged content", naming a path they can look at while the thing fu
// actually moved sits somewhere nothing mentions.
func TestReconcileConflictNamesRetiredPathWhenRestoreFails(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := reconcile(s, cfg, agents, nil); err != nil {
		t.Fatal(err)
	}
	cfg.SetEnabled("alpha", false)

	const squatter = "squatter arrived after the retire"
	fired := false
	h := reconcileHooks{
		// Fail the post-move validation by retargeting the retired link, and
		// reoccupy the original name in the same window so the restore hits
		// EEXIST. Both halves are needed: without the first there is nothing
		// to restore, without the second the restore succeeds.
		afterLinkRetire: func(_ agent.Agent, act Action, retired string) {
			if act.Skill != "alpha" {
				return
			}
			retiredPath := filepath.Join(dir, retired)
			if err := os.Remove(retiredPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(s.SkillsDir(), "elsewhere"), retiredPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(act.LinkPath, []byte(squatter), 0o644); err != nil {
				t.Fatal(err)
			}
			fired = true
		},
	}
	res, err := reconcileWithHooks(s, cfg, agents, h)
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("test setup error: the post-retire hook did not run")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Skill != "alpha" {
		t.Fatalf("a failed restore must be reported as a conflict: %+v", res)
	}
	target := res.Conflicts[0].Target
	if target == "" {
		t.Fatal("the conflict must name where fu's own link was parked")
	}
	if !strings.Contains(filepath.Base(target), ".fu-retired-") {
		t.Fatalf("the conflict must name the retired path, got %q", target)
	}
	if _, statErr := os.Lstat(target); statErr != nil {
		t.Fatalf("the named path must be where the object actually is: %v", statErr)
	}
	// The content that arrived at the original name is untouched.
	got, readErr := os.ReadFile(filepath.Join(dir, "alpha"))
	if readErr != nil || string(got) != squatter {
		t.Fatalf("content at the reoccupied name must survive: %q, %v", got, readErr)
	}
}

// TestResultEmptyCoversEveryReportedField pins Result.Empty per field. It is
// the engine's verdict on whether a run produced anything to say, and the CLI
// used to compute an eleven-field version of it independently -- which had
// already drifted, omitting DisabledForeign, so a run could print a
// disabled-foreign diagnostic and then claim nothing happened (round 18
// finding I20). The fix shipped with no test at all: `grep '\.Empty()'` over
// the test files returned nothing, so deleting any single condition here
// restored the defect with a green suite.
func TestResultEmptyCoversEveryReportedField(t *testing.T) {
	action := []Action{{AgentName: "claude", Skill: "alpha"}}
	cases := map[string]Result{
		"conflicts":        {Conflicts: action},
		"disabled foreign": {DisabledForeign: action},
		"missing":          {Missing: action},
		"reserved":         {Reserved: action},
		"invalid":          {Invalid: action},
		"skipped":          {Skipped: []string{"claude"}},
		"failed":           {Failed: []FailedAction{{Action{AgentName: "claude"}, errors.New("boom")}}},
	}
	if !(Result{}).Empty() {
		t.Fatal("a zero Result must be empty")
	}
	for name, res := range cases {
		if res.Empty() {
			t.Fatalf("a Result carrying %s is not empty", name)
		}
	}
	// Foreign is the one field that does not count, on the same grounds as
	// UserReports: it is inventory for a future `fu status`, not a finding
	// about this run, and printResult never prints it.
	if !(Result{Foreign: action}).Empty() {
		t.Fatal("Foreign is inventory, not a finding, so it must not make a run non-empty")
	}
}

// TestAdoptResultEmptyCoversEveryReportedField is the same property one level
// up, including the Reconcile half that carried the drift.
func TestAdoptResultEmptyCoversEveryReportedField(t *testing.T) {
	summary := []AdoptSummary{{Name: "alpha"}}
	cases := map[string]AdoptResult{
		"adopted":   {Adopted: summary},
		"pending":   {Pending: summary},
		"conflicts": {Conflicts: []string{"alpha"}},
		"skipped":   {Skipped: []string{"alpha"}},
		"warnings":  {Warnings: []string{"w"}},
		"failed":    {Failed: []FailedAction{{Action{Skill: "alpha"}, errors.New("boom")}}},
		"reconcile disabled-foreign": {Reconcile: Result{
			DisabledForeign: []Action{{AgentName: "claude", Skill: "alpha"}},
		}},
	}
	if !(AdoptResult{}).Empty() {
		t.Fatal("a zero AdoptResult must be empty")
	}
	for name, res := range cases {
		if res.Empty() {
			t.Fatalf("an AdoptResult carrying %s is not empty", name)
		}
	}
}

// TestUserReportsCollapsesRepeatedFindings pins the dedupe at the visibility
// boundary. A batch command runs one transaction per item and each ends with
// its own reconcile, so mergeResult accumulates the same standing finding once
// per item: `fu add --all` over three skills with one pre-existing foreign
// directory printed the same conflict line three times, and three identical
// lines read as three separate conflicts. Output was O(candidates × findings).
func TestUserReportsCollapsesRepeatedFindings(t *testing.T) {
	conflict := Action{AgentName: "claude", Skill: "alpha"}
	var accumulated Result
	for range 3 {
		mergeResult(&accumulated, Result{Conflicts: []Action{conflict}})
	}
	reports := accumulated.UserReports()
	if len(reports) != 1 {
		t.Fatalf("one standing finding must render once, got %d: %+v", len(reports), reports)
	}
	// The accumulated Result itself is untouched: it is the structured value a
	// second front end consumes, and each operation really did produce one.
	if len(accumulated.Conflicts) != 3 {
		t.Fatalf("the collapse belongs to UserReports, not mergeResult: %+v", accumulated.Conflicts)
	}
	// Distinct findings still all reach the user.
	var mixed Result
	mergeResult(&mixed, Result{Conflicts: []Action{conflict, {AgentName: "codex", Skill: "alpha"}}})
	mergeResult(&mixed, Result{Conflicts: []Action{{AgentName: "claude", Skill: "beta"}}})
	if got := len(mixed.UserReports()); got != 3 {
		t.Fatalf("distinct findings must all render, got %d", got)
	}
	// A conflict naming the retired path is not the same finding as a bare one
	// for the same name: collapsing it would drop the user's only pointer to
	// where their content went.
	var withTarget Result
	mergeResult(&withTarget, Result{Conflicts: []Action{conflict}})
	mergeResult(&withTarget, Result{Conflicts: []Action{{
		AgentName: "claude", Skill: "alpha", Target: "/home/u/.claude/skills/.fu-retired-ab",
	}}})
	if got := len(withTarget.UserReports()); got != 2 {
		t.Fatalf("a conflict naming the retired path must survive the collapse, got %d", got)
	}
	// Failures with different causes are different findings.
	var failures Result
	mergeResult(&failures, Result{Failed: []FailedAction{{Action{AgentName: "claude"}, errors.New("one")}}})
	mergeResult(&failures, Result{Failed: []FailedAction{{Action{AgentName: "claude"}, errors.New("one")}}})
	mergeResult(&failures, Result{Failed: []FailedAction{{Action{AgentName: "claude"}, errors.New("two")}}})
	if got := len(failures.UserReports()); got != 2 {
		t.Fatalf("failures must collapse by cause, got %d", got)
	}
}
