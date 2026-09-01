package engine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

// installedFromGit installs one skill from a real bare repository through the
// production add path (PrepareAdd -> AddPlan.Install), so the source record
// under test is the one `fu add` actually writes -- a fully qualified ref, a
// locked commit, a subdir -- rather than a hand-assembled approximation of it.
// That matters here specifically: what update does with a git record is
// reconstruct a Source from those fields (sourceFromFields, application.go),
// and a fixture that wrote the fields itself would be asserting against its
// own guess at their shape.
func installedFromGit(t *testing.T, s *store.Store, url string) {
	t.Helper()
	preparation, err := PrepareAdd(s, url, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := preparation.Session.(*AddPlan)
	installErr := func() error {
		_, err := plan.Install(plan.Candidates())
		return err
	}()
	// Closed here rather than in a t.Cleanup, because the clone scratch it
	// releases lives under staging/: deferring it to the end of the test would
	// leave a staging entry behind for the whole run, and what these tests
	// assert about staging is that update left nothing there.
	closeErr := plan.Close()
	if installErr != nil {
		t.Fatal(installErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

// advanceGitSource commits mutate's changes on top of the fixture
// repository's branch and returns the commit the branch now points at. It
// clones the bare repository makeGitSourceNamed built, applies the change and
// pushes it back -- the same way the upstream this fixture stands in for
// would move.
func advanceGitSource(t *testing.T, url string, mutate func(work string)) string {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainClone(work, false, &git.CloneOptions{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	mutate(work)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	advanced, err := wt.Commit("advance", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	branch := head.Name().Short()
	if err := repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)},
	}); err != nil {
		t.Fatal(err)
	}
	return advanced.String()
}

// recordedSourceFields reads a skill's source record back from fu.yaml on
// disk, so an assertion about the lock sees what was actually saved rather
// than an in-memory Config the command never wrote.
func recordedSourceFields(t *testing.T, s *store.Store, name string) map[string]string {
	t.Helper()
	reloaded, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	return reloaded.SourceFields(name)
}

// TestUpdateSkillsUpdatesAGitSourcedSkillsContent closes fix round 2's
// Important #6: every update test used a local source, leaving SPEC scenario
// 3's primary path -- update a skill installed from git -- unguarded. What
// runs only here is sourceFromFields's git arm, which reconstructs the short
// branch name from the fully qualified ref fu.yaml records
// (strings.TrimPrefix of "refs/heads/") and hands it to cloneSource's
// branch-then-tag probe, plus the re-clone and the advanced-commit write. A
// regression in any of them would report success while leaving the skill
// permanently outdated, and no local-source test can reach them.
func TestUpdateSkillsUpdatesAGitSourcedSkillsContent(t *testing.T) {
	app, st, _ := applicationEnv(t)
	url := makeGitSourceNamed(t, false, "pdf-tools")
	installedFromGit(t, st, url)

	advanced := advanceGitSource(t, url, func(work string) {
		writeSkillBody(t, filepath.Join(work, "pdf-tools"), "pdf-tools", "version two")
	})

	outcome, err := app.UpdateSkills("pdf-tools", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "pdf-tools" || len(outcome.LockOnly) != 0 {
		t.Fatalf("outcome = %+v, want pdf-tools updated by the content shape", outcome)
	}
	got, err := os.ReadFile(filepath.Join(st.SkillsDir(), "pdf-tools", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "version two") {
		t.Fatalf("the new upstream content did not land:\n%s", got)
	}
	fields := recordedSourceFields(t, st, "pdf-tools")
	if fields["commit"] != advanced {
		t.Fatalf("recorded commit = %q, want the advanced commit %q", fields["commit"], advanced)
	}
	if !strings.HasPrefix(fields["ref"], "refs/heads/") {
		t.Fatalf("the recorded ref must stay fully qualified, got %q", fields["ref"])
	}
	entries, err := os.ReadDir(st.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a settled update must leave staging empty, holds %d entries", len(entries))
	}
}

// TestUpdateSkillsAdvancesAGitSourcedLockWhenTheSubdirIsUnchanged is design
// §2.5's actual motivating case, which no test reached: an upstream commit
// advances while the skill's own subdir is untouched. `fu outdated` sees only
// the commit, so it reports the skill updatable; update re-clones, finds the
// content identical to the baseline, and takes the lock-only shape. The final
// assertion is the one that matters -- after the update, outdated must be
// quiet. Without the lock advancing, the skill is reported updatable forever
// and every update does nothing.
//
// The local-source test of the same shape
// (TestUpdateSkillAdvancesTheLockWhenContentIsUnchanged) cannot stand in for
// this: a local source's record does not change at all when the content does
// not, so the case is degenerate there.
func TestUpdateSkillsAdvancesAGitSourcedLockWhenTheSubdirIsUnchanged(t *testing.T) {
	app, st, _ := applicationEnv(t)
	url := makeGitSourceNamed(t, false, "pdf-tools")
	installedFromGit(t, st, url)
	locked := recordedSourceFields(t, st, "pdf-tools")["commit"]

	// Outside the skill's own subdir: the commit moves, the skill does not.
	advanced := advanceGitSource(t, url, func(work string) {
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("unrelated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if advanced == locked {
		t.Fatal("fixture error: the upstream commit did not move")
	}

	outcome, err := app.UpdateSkills("pdf-tools", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(outcome.LockOnly) != 1 || outcome.LockOnly[0] != "pdf-tools" || len(outcome.Updated) != 0 {
		t.Fatalf("outcome = %+v, want pdf-tools resolved by the lock-only shape", outcome)
	}
	if got := recordedSourceFields(t, st, "pdf-tools")["commit"]; got != advanced {
		t.Fatalf("recorded commit = %q, want the advanced commit %q", got, advanced)
	}
	entries, err := os.ReadDir(st.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a lock-only update must touch nothing under staging, holds %d entries", len(entries))
	}

	reloaded, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Outdated(st, reloaded)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if len(rows) != 1 || rows[0].Updatable {
		t.Fatalf("the advanced lock must settle the report, got %+v", rows)
	}
}

// mustRootFS borrows the prepared source's pinned descriptor.
func mustRootFS(t *testing.T, p *source.Prepared) fs.FS {
	t.Helper()
	root, err := p.Root()
	if err != nil {
		t.Fatal(err)
	}
	return root.FS()
}

// installedFromLocal seeds a store with one skill whose content matches a local
// source, returning the source dir and the recorded baseline.
func installedFromLocal(t *testing.T, s *store.Store, cfg *store.Config, name, body string) (srcDir, baseline string) {
	t.Helper()
	srcDir = filepath.Join(t.TempDir(), "src")
	writeSkillBody(t, srcDir, name, body)
	writeSkillBody(t, filepath.Join(s.SkillsDir(), name), name, body)
	// SnapshotSkillPayload only works through pinned, checked descriptors (a
	// write session; see ownedtree.go), which a plain store.Init/store.Open
	// does not hold. checkedRecoveryStore (reconcile_test.go) opens exactly
	// such a session and registers its own cleanup, the same pattern the
	// outdated tests already use for the same reason.
	payload, err := checkedRecoveryStore(t, s).SnapshotSkillPayload(name)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err = digestOwnedPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddSkill(name, baseline); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields(name, localSourceRecord(srcDir))
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return srcDir, baseline
}

// localSourceRecord is the source record installedFromLocal writes, and so
// the record every local-source update in these tests was prepared from.
// updateSkill takes it back as its `recorded` argument, where
// checkRecordedSourceUnchanged re-checks it against the config the operation
// actually mutates (update.go); passing the fixture's own writer keeps the two
// from ever standing for different records. It takes no *testing.T so the
// re-exec crash children can build it too (gc_test.go, update_txn_test.go).
func localSourceRecord(srcDir string) map[string]string {
	return map[string]string{"type": "local", "path": srcDir}
}

// When upstream content is identical, the lock still advances -- otherwise the
// skill is reported outdated forever, since outdated only ever sees the commit.
func TestUpdateSkillAdvancesTheLockWhenContentIsUnchanged(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, baseline := installedFromLocal(t, s, cfg, "kit", "same body")
	p := prepareLocal(t, srcDir)

	newFields := map[string]string{"type": "local", "path": srcDir, "commit": "newcommit"}
	cand := Candidate{Name: "kit", Subdir: ".", Digest: baseline}

	outcome, err := updateSkill(s, nil, p, "kit", cand, localSourceRecord(srcDir), newFields, false, hooks{})
	if err != nil {
		t.Fatalf("updateSkill: %v", err)
	}
	if !outcome.LockOnly {
		t.Fatal("identical content must take the lock-only shape")
	}

	reloaded, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.SourceFields("kit")["commit"]; got != "newcommit" {
		t.Fatalf("recorded commit = %q, want newcommit", got)
	}
	if got := reloaded.Digest("kit"); got != baseline {
		t.Fatalf("baseline must not move when content did not: %q", got)
	}
}

// A lock-only update touches fu.yaml and nothing else: no staging entry is
// created and the published tree is byte-identical afterwards.
func TestUpdateSkillLockOnlyLeavesStagingEmpty(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, baseline := installedFromLocal(t, s, cfg, "kit", "same body")
	p := prepareLocal(t, srcDir)

	before, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := updateSkill(s, []agent.Agent(nil), p, "kit",
		Candidate{Name: "kit", Subdir: ".", Digest: baseline},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir, "commit": "c2"}, false, hooks{}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(s.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging must stay empty for a lock-only update, holds %d entries", len(entries))
	}
	after, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a lock-only update must not rewrite the published tree")
	}
}

func TestUpdateSkillReplacesPublishedContentAtomically(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "kit", "version two")

	p := prepareLocal(t, srcDir)
	proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := skill.DigestManifest(proj)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := updateSkill(s, nil, p, "kit",
		Candidate{Name: "kit", Subdir: ".", Digest: newDigest},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir}, false, hooks{})
	if err != nil {
		t.Fatalf("updateSkill: %v", err)
	}
	if outcome.LockOnly {
		t.Fatal("changed content must take the full transaction shape")
	}

	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "version two") {
		t.Fatalf("published content was not replaced:\n%s", got)
	}

	reloaded, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Digest("kit") != newDigest {
		t.Fatal("the baseline must move to the new content")
	}
}

// TestUpdateSkillPreservesTheGlobalSwitchAndAgentOverrides pins the property
// design §4.2 rests the whole architecture on. The alternative it rejected --
// an engine-level `rm` followed by an `add` -- was disqualified by exactly one
// defect: 「`rm` 会连同 agent 级覆盖一并删除 fu.yaml 条目，更新完开关状态就丢了」.
// The chosen design avoids it by construction, because SetDigest touches only
// the `digest` key and SetSourceFields only the `source` mapping (config.go),
// but nothing pinned it: a regression would silently re-enable a skill the
// user had deliberately switched off for one agent, and it would go unnoticed
// on the recovery side too, since expectedUpdatedConfig (update_txn_test.go)
// performs the identical pair of mutations and so would agree with the damage.
//
// The reload from disk is the point of the test. Asserting against the
// in-memory cfg the fixture already holds would only prove that this process
// did not forget; what update owes the user is that the saved file still
// carries their decision.
func TestUpdateSkillPreservesTheGlobalSwitchAndAgentOverrides(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		lockOnly bool
	}{
		{name: "content shape", body: "version two"},
		{name: "lock-only shape", body: "version one", lockOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg := setupStore(t)
			srcDir, baseline := installedFromLocal(t, s, cfg, "kit", "version one")
			cfg.SetEnabled("kit", false)
			cfg.SetAgent("kit", "claude", true)
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			writeSkillBody(t, srcDir, "kit", tc.body)

			p := prepareLocal(t, srcDir)
			proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
			if err != nil {
				t.Fatal(err)
			}
			digest, err := skill.DigestManifest(proj)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := updateSkill(s, nil, p, "kit",
				Candidate{Name: "kit", Subdir: ".", Digest: digest},
				localSourceRecord(srcDir),
				map[string]string{"type": "local", "path": srcDir, "commit": "advanced"}, false, hooks{})
			if err != nil {
				t.Fatalf("updateSkill: %v", err)
			}
			if outcome.LockOnly != tc.lockOnly {
				t.Fatalf("outcome.LockOnly = %v, want %v -- the fixture no longer exercises the shape it names", outcome.LockOnly, tc.lockOnly)
			}
			if tc.lockOnly && digest != baseline {
				t.Fatal("the lock-only fixture must leave the content identical to the baseline")
			}

			reloaded, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Enabled("kit") {
				t.Fatal("update must not re-enable a skill the user switched off")
			}
			value, present := reloaded.Override("kit", "claude")
			if !present || !value {
				t.Fatalf("agent override after update = (%v, %v), want (true, true): update must not drop a per-agent decision", value, present)
			}
		})
	}
}

// TestUpdateSkillRefusesWhenTheRecordedSourceChangedSinceItWasInspected pins
// the pre-lock/post-lock symmetry the lock-only shape already had for its own
// digest input (round 4). UpdateSkills reads fu.yaml, clones from what it
// found -- up to two minutes -- and only then takes the write lock, so a hand
// edit landing in that window is swept into a commit by the prologue and then
// overwritten by this transaction's own SetSourceFields. Without the check,
// the user's edit is silently reverted under the message `update: kit`, and
// the content shape publishes content from the repository they just stopped
// tracking.
//
// Both shapes are exercised: the lock-only Mutate writes the same source
// record the content shape does, so a guard on one of them alone leaves the
// other reverting the edit exactly as before.
func TestUpdateSkillRefusesWhenTheRecordedSourceChangedSinceItWasInspected(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "content shape", body: "version two"},
		{name: "lock-only shape", body: "version one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg := setupStore(t)
			srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
			recorded := localSourceRecord(srcDir)
			writeSkillBody(t, srcDir, "kit", tc.body)
			p := prepareLocal(t, srcDir)

			// The hand edit lands while the clone above is still running:
			// fu.yaml now tracks a different directory than the one this
			// update was prepared from.
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			writeSkillBody(t, elsewhere, "kit", "version one")
			cfg.SetSourceFields("kit", localSourceRecord(elsewhere))
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}

			proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
			if err != nil {
				t.Fatal(err)
			}
			digest, err := skill.DigestManifest(proj)
			if err != nil {
				t.Fatal(err)
			}
			_, err = updateSkill(s, nil, p, "kit",
				Candidate{Name: "kit", Subdir: ".", Digest: digest},
				recorded, map[string]string{"type": "local", "path": srcDir}, false, hooks{})
			if err == nil {
				t.Fatal("an update prepared from a record fu.yaml no longer holds must be refused")
			}
			if !strings.Contains(err.Error(), "changed since it was inspected") {
				t.Fatalf("error = %v, want the same wording the digest guard already uses", err)
			}

			reloaded, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if got := reloaded.SourceFields("kit")["path"]; got != elsewhere {
				t.Fatalf("recorded path = %q, want the hand edit %q left intact", got, elsewhere)
			}
		})
	}
}

// TestRecordedSourceGuardRefusesARefKindHandEdit adds ref_kind to the fields
// the guard above compares. Its doc excluded ref_kind alongside commit as "an
// output this update is about to rewrite", which is true of commit and false of
// ref_kind: judgeGitUpdate refuses any row whose ref_kind is not "branch"
// (outdated.go), and cloneSource with RefKind "branch" always returns "branch"
// (git.go), so on every reachable path in equals out. Left out, a user pinning
// the skill by hand-editing ref_kind from branch to tag inside the clone window
// passed the guard, and cfg.SetSourceFields -- a true replacement, not a merge
// (config.go) -- wrote branch straight back: the pin silently reverted and the
// skill still tracking a moving branch. That is the exact failure
// Source.RefKind's own doc says the field was added to prevent, and the race
// class this guard exists to close.
//
// The second case is the reason the field was excluded in the first place, kept
// as a test so the fix cannot reintroduce a false refusal: an ordinary advance
// moves commit and nothing else, and must still pass.
func TestRecordedSourceGuardRefusesARefKindHandEdit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		refuse  bool
	}{
		{name: "hand edit pins a tag", current: "tag", refuse: true},
		{name: "ordinary advance keeps the branch", current: "branch", refuse: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg := setupStore(t, "kit")
			recorded := map[string]string{
				"type": "git", "url": "https://example.invalid/kit.git",
				"ref": "release", "ref_kind": "branch", "commit": strings.Repeat("a", 40),
			}
			current := map[string]string{}
			for field, value := range recorded {
				current[field] = value
			}
			current["ref_kind"] = tc.current
			// commit differs on every case, pinning that it stays outside the
			// compared set. Note what this does *not* model: a single update
			// never moves the config's commit between the two reads this guard
			// compares (the new value lives in a separate fields map), so
			// comparing commit would not refuse an ordinary advance either. It
			// is excluded because it is not part of the identity the source was
			// prepared from -- see checkRecordedSourceUnchanged's own doc.
			current["commit"] = strings.Repeat("b", 40)
			cfg.SetSourceFields("kit", current)

			err := checkRecordedSourceUnchanged(cfg, "kit", recorded)
			if tc.refuse && err == nil {
				t.Fatal("a ref_kind hand edit must be refused; SetSourceFields would revert the pin")
			}
			if !tc.refuse && err != nil {
				t.Fatalf("an ordinary commit advance must not be refused: %v", err)
			}
		})
	}
}

// TestUpdateSkillLockOnlyRefusesWhenThePublishedContentIsGone pins the one
// precondition the lock-only shape could not otherwise reach. observedDigest
// is the digest of the *upstream* tree, not of the store copy, so the shape's
// own guard passes cleanly when skills/<name> is absent; judgeLocalModification
// degrades a missing store copy to LocallyModified=false (outdated.go), so
// nothing upstream refuses it either. The command used to exit 0 saying
// "recorded the source lock, content unchanged" -- recording a lock for a
// skill whose content is not there. The content shape has always refused this,
// inside checkUpdateAvailable; this is the two shapes agreeing again.
func TestUpdateSkillLockOnlyRefusesWhenThePublishedContentIsGone(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, baseline := installedFromLocal(t, s, cfg, "kit", "same body")
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "kit")); err != nil {
		t.Fatal(err)
	}
	p := prepareLocal(t, srcDir)

	_, err := updateSkill(s, nil, p, "kit",
		Candidate{Name: "kit", Subdir: ".", Digest: baseline},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir, "commit": "advanced"}, false, hooks{})
	if err == nil {
		t.Fatal("a lock must not advance over content that is not there")
	}

	reloaded, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.SourceFields("kit")["commit"]; got != "" {
		t.Fatalf("recorded commit = %q, want the refusal to have written nothing", got)
	}
}

// SPEC rule 3: a hand-edited store copy is not silently overwritten.
func TestUpdateSkillRefusesALocallyModifiedSkill(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, baseline := installedFromLocal(t, s, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "kit", "version two")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "hand edited")

	p := prepareLocal(t, srcDir)

	_, refusal := updateSkill(s, nil, p, "kit",
		Candidate{Name: "kit", Subdir: ".", Digest: "sha256:whatever"},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir}, false, hooks{})
	if !errors.Is(refusal, ErrLocallyModified) {
		t.Fatalf("error = %v, want ErrLocallyModified", refusal)
	}
	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "hand edited") {
		t.Fatal("a refusal must leave the hand edit in place")
	}
	assertRefusalShowsTheDifference(t, refusal, s, "kit", baseline)
}

// assertRefusalShowsTheDifference checks the half of SPEC rule 3 the refusal
// text owes the user: 拒绝覆盖并提示差异 (SPEC §5.1 and rule 3, repeated in
// design §2.1). Fix round 2, Important #5: both rule-3 refusals used to state
// only the fact of divergence, and nothing else in the product could show it
// afterwards -- `fu status` compares the store worktree against git, not the
// digest against the baseline, and once any write command has swept the hand
// edit into history the worktree is clean.
//
// fu.yaml records a digest, not a manifest, so a file-level diff is not
// derivable at this layer at all. What the refusal can name is the digest pair
// and the git command that shows the content, and this asserts it does.
func assertRefusalShowsTheDifference(t *testing.T, err error, s *store.Store, name, baseline string) {
	t.Helper()
	payload, snapErr := checkedRecoveryStore(t, s).SnapshotSkillPayload(name)
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	current, digestErr := digestOwnedPayload(payload)
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	if current == baseline {
		t.Fatal("fixture error: the store copy must differ from the baseline for this assertion to mean anything")
	}
	for _, want := range []string{
		shortDigest(baseline),
		shortDigest(current),
		"git -C " + s.Dir(),
		"skills/" + name,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must show the difference, missing %q in: %v", want, err)
		}
	}
	// Round 8: `git diff` was offered first and prints nothing on every
	// practically reachable path, because both call sites run after a sweep has
	// already committed the edit -- selectUpdateTargets after the prologue's,
	// checkUpdateAvailable inside run's. A first suggestion that is a guaranteed
	// dead end is not 提示差异 (SPEC rule 3); the command that does show the edit
	// has to lead.
	logAt := strings.Index(err.Error(), "log -p")
	diffAt := strings.Index(err.Error(), "diff --")
	if logAt < 0 || diffAt < 0 {
		t.Fatalf("the refusal must name both git commands: %v", err)
	}
	if logAt > diffAt {
		t.Fatalf("`git log -p` shows the swept edit and must be offered first: %v", err)
	}
}

// updateSkill's own name guard, pinned for the reason its sibling in
// reclaimUpdateStagingPayload was pinned in round 3: an unexercised guard is
// indistinguishable from a removable one, and the two are a deliberate pair.
// Production callers cannot reach it -- UpdateSkills takes names out of a
// config LoadConfig has already filtered -- so this calls the unexported entry
// point directly, which is exactly what a second front end reaching past
// Application would be doing. Without the guard the name flows on into
// staging paths and an ownership manifest under a .fu- name fu reserves for
// itself.
func TestUpdateSkillRefusesANameThatIsNotAPublicSkillName(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, baseline := installedFromLocal(t, s, cfg, "kit", "version one")
	p := prepareLocal(t, srcDir)

	_, err := updateSkill(s, nil, p, ".fu-not-a-skill-name",
		Candidate{Name: ".fu-not-a-skill-name", Subdir: ".", Digest: baseline},
		localSourceRecord(srcDir), localSourceRecord(srcDir), false, hooks{})

	if err == nil {
		t.Fatal("a name outside the public skill namespace must be refused")
	}
	if !strings.Contains(err.Error(), "not a public skill name") {
		t.Fatalf("the refusal must name what was wrong with it: %v", err)
	}
}

// The digest pair degrades when there is no pair. A fu.yaml whose `digest:`
// key was removed by hand still reaches this refusal -- judgeLocalModification
// sets Baseline unconditionally and calls anything but the empty string a
// modification (outdated.go) -- and the empty baseline rendered as
// "(recorded , now sha256:...)": a blank exactly where the user looks for the
// value, reading as a rendering fault rather than as the missing record it is.
func TestLocalModificationRefusalNamesAnAbsentBaseline(t *testing.T) {
	s, _ := setupStore(t)
	current := "sha256:" + strings.Repeat("a", 64)

	message := describeLocalModification(s, "kit", "", current)

	if strings.Contains(message, "recorded ,") {
		t.Fatalf("an absent baseline must not be rendered as a blank: %q", message)
	}
	if !strings.Contains(message, "nothing recorded") {
		t.Fatalf("message = %q, want it to say no baseline was recorded at all", message)
	}
	// The half that does exist is still shown: the pair is what rule 3 owes,
	// and only the missing side may go missing.
	if !strings.Contains(message, shortDigest(current)) {
		t.Fatalf("message = %q, want it to still show the current digest", message)
	}
}

func TestUpdateSkillOverwritesALocallyModifiedSkillWithForce(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "kit", "version two")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "hand edited")

	p := prepareLocal(t, srcDir)
	proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := skill.DigestManifest(proj)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := updateSkill(s, nil, p, "kit",
		Candidate{Name: "kit", Subdir: ".", Digest: newDigest},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir}, true, hooks{}); err != nil {
		t.Fatalf("--force must overwrite: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "version two") {
		t.Fatalf("forced update did not land:\n%s", got)
	}
	// SPEC rule 3's other half: 被覆盖内容留存于 git 历史. That promise is the
	// sole reason update discards the tree it replaced instead of quarantining
	// it (DESIGN §2's staging residue entry), and this is the case where it
	// carries the whole weight -- the hand edit existed nowhere but the store
	// worktree, so only the prologue sweep committing it first keeps it
	// recoverable. Round 2, Minor #10: nothing tied the two together for
	// update, and a regression would be silent and unrecoverable.
	assertHistoryHolds(t, s, "skills/kit/SKILL.md", "hand edited")
}

// assertHistoryHolds walks the store's history and fails unless some commit's
// copy of path contains want.
func assertHistoryHolds(t *testing.T, s *store.Store, path, want string) {
	t.Helper()
	iter, err := s.Repo.Log(&git.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()
	for {
		commit, err := iter.Next()
		if err != nil {
			break
		}
		file, err := commit.File(path)
		if err != nil {
			continue
		}
		contents, err := file.Contents()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(contents, want) {
			return
		}
	}
	t.Fatalf("no commit in the store's history holds %q at %s, so the overwritten content is not recoverable", want, path)
}

// SPEC rule 1 forbids renaming, so upstream renaming the skill is a refusal
// rather than a silent re-registration.
func TestUpdateSkillRefusesWhenUpstreamRenamedTheSkill(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "renamed-kit", "version two")

	p := prepareLocal(t, srcDir)

	_, err := updateSkill(s, nil, p, "kit",
		Candidate{Name: "renamed-kit", Subdir: ".", Digest: "sha256:whatever"},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir}, false, hooks{})
	if err == nil {
		t.Fatal("expected a refusal when upstream renamed the skill")
	}
	// The message is asserted, not merely non-nil-ness: every unimplemented or
	// unrelated failure in this path also returns an error, so a bare non-nil
	// check would pass without the rename ever being the reason. It must name
	// the new name and the way out, the standing rule for any refusal.
	for _, want := range []string{"renamed-kit", "fu rm kit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name %q; got: %v", want, err)
		}
	}
}

// The happy path leaves nothing behind: the replaced tree is disposed of after
// the terminal marker, and update never archives into recovery/.
func TestUpdateSkillLeavesNoResidue(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "kit", "version two")

	p := prepareLocal(t, srcDir)
	proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := skill.DigestManifest(proj)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := updateSkill(s, nil, p, "kit",
		Candidate{Name: "kit", Subdir: ".", Digest: newDigest},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir}, false, hooks{}); err != nil {
		t.Fatal(err)
	}

	// The exchange leaves the replaced tree at staging/kit; this is the
	// assertion that the reclaim ran, and it fails if afterTxnCleared is
	// removed.
	staging, err := os.ReadDir(s.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Fatalf("staging/ must be empty after a settled update, holds %d entries", len(staging))
	}
	// recovery/ still holds this transaction's own journal family, as every
	// write command's does until `fu gc` prunes it
	// (pruneCompletedTransactionsLocked). What must not be there is content:
	// unlike rm, update quarantines nothing, because SPEC rule 3 leaves the
	// overwritten tree recoverable from the store's git history instead.
	recovery, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range recovery {
		if !strings.HasPrefix(entry.Name(), "txn-") {
			t.Fatalf("update must archive nothing into recovery/, found %q", entry.Name())
		}
	}
}

// txnRecordNames lists the transaction journal files under recovery/.
func txnRecordNames(t *testing.T, s *store.Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "txn-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// A crash inside the reclaim leaves the replaced tree orphaned at
// staging/<name>, and nothing clears it: reclamation runs after the terminal
// marker, so no recovery pass treats it as work. The next update of that skill
// must refuse before the transaction record is written -- discovering the same
// condition inside Mutate would manufacture a pending transaction out of a
// state that was knowable while the store was still untouched.
func TestUpdateSkillRefusesAnOccupiedStagingNameBeforeOpeningTheWAL(t *testing.T) {
	// Run under both switches: --force waives the local-modification refusal
	// and nothing else, so it must not carry a caller past this one.
	for _, force := range []bool{false, true} {
		label := "plain"
		if force {
			label = "force"
		}
		t.Run(label, func(t *testing.T) {
			s, cfg := setupStore(t)
			srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
			writeSkillBody(t, srcDir, "kit", "version two")
			writeSkillBody(t, filepath.Join(s.StagingDir(), "kit"), "kit", "orphaned by a crash")

			p := prepareLocal(t, srcDir)
			proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
			if err != nil {
				t.Fatal(err)
			}
			newDigest, err := skill.DigestManifest(proj)
			if err != nil {
				t.Fatal(err)
			}
			before := txnRecordNames(t, s)

			_, err = updateSkill(s, nil, p, "kit",
				Candidate{Name: "kit", Subdir: ".", Digest: newDigest},
				localSourceRecord(srcDir),
				map[string]string{"type": "local", "path": srcDir}, force, hooks{})
			if err == nil {
				t.Fatal("expected a refusal while staging/kit is occupied")
			}
			// "fu gc" is the first remedy and the right one for the orphan
			// this test builds. The fallback is required too (fix round 1,
			// Important #2): checkUpdateAvailable cannot tell that orphan
			// apart from a directory the user put there themselves, and `fu
			// gc` collects only the former -- so a refusal naming gc alone
			// leaves the latter with a remedy that exits 0 having done
			// nothing, and the skill un-updatable forever.
			//
			// The fallback is asserted by its instruction, not by the claim it
			// used to carry: round 2's Minor #2 dropped "that entry is not a
			// tree fu left behind", because the documented double fault leaves
			// gc silent over a tree that *is* fu's own, and the message cannot
			// tell the two apart.
			for _, want := range []string{filepath.Join(s.StagingDir(), "kit"), "fu gc", "move that entry out of"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal must name %q; got: %v", want, err)
				}
			}
			// The whole point of refusing in Preflight: no transaction record
			// exists, so recovery has nothing to resolve.
			if after := txnRecordNames(t, s); len(after) != len(before) {
				t.Fatalf("a pre-WAL refusal must write no transaction record; recovery/ went from %v to %v", before, after)
			}
			// The orphan is preserved, never cleared as a side effect: it may
			// be the only copy of that content the user still has.
			if _, err := os.Stat(filepath.Join(s.StagingDir(), "kit", "SKILL.md")); err != nil {
				t.Fatalf("the refusal must leave the staged orphan in place: %v", err)
			}
		})
	}
}

// applicationEnv initializes a store the same way `fu init` does (FU_HOME +
// HOME), returning the Application plus the underlying store and config so a
// test can seed skills directly with this file's existing fixtures
// (installedFromLocal, writeSkillBody). Application.UpdateSkills goes through
// a.readStore(), which resolves the store from FU_HOME on every call -- the
// plain setupStore(t) fixture (reconcile_test.go) is unrelated to it.
func applicationEnv(t *testing.T) (*Application, *store.Store, *store.Config) {
	t.Helper()
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := NewApplication()
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}
	st, err := app.openStore()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	return app, st, cfg
}

// TestUpdateSkillsNamedRefusesANotComparableSkillWithOutdatedsReason pins
// that a named update on a skill Outdated could not judge (here: no source
// record at all) fails with Outdated's own Reason, not a generic error --
// the other examples the task names (a fixed tag/commit lock, a missing
// local path) all reach the same branch in selectUpdateTargets.
func TestUpdateSkillsNamedRefusesANotComparableSkillWithOutdatedsReason(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "homegrown"), "homegrown", "one")
	if err := cfg.AddSkill("homegrown", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	_, err := app.UpdateSkills("homegrown", false)
	if err == nil {
		t.Fatal("expected a refusal naming Outdated's own reason")
	}
	if !strings.Contains(err.Error(), "no source record") {
		t.Fatalf("error must carry Outdated's own reason, got: %v", err)
	}
	if !strings.Contains(err.Error(), "homegrown") {
		t.Fatalf("error must name the skill, got: %v", err)
	}
}

// TestUpdateSkillsNamedLocallyModifiedRefusesWithoutForceAndSucceedsWithIt
// covers both halves of SPEC rule 3 through the Application boundary:
// without --force the hand edit is left untouched and the error wraps
// ErrLocallyModified; with it the update proceeds.
func TestUpdateSkillsNamedLocallyModifiedRefusesWithoutForceAndSucceedsWithIt(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	srcDir, baseline := installedFromLocal(t, st, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "kit", "version two")
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "kit"), "kit", "hand edited")

	before, err := os.ReadFile(filepath.Join(st.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}

	refusal := func() error {
		_, err := app.UpdateSkills("kit", false)
		return err
	}()
	if !errors.Is(refusal, ErrLocallyModified) {
		t.Fatalf("error = %v, want ErrLocallyModified", refusal)
	}
	// selectUpdateTargets raises this one, before any clone; the sibling in
	// checkUpdateAvailable is covered by TestUpdateSkillRefusesALocallyModifiedSkill.
	// Both owe the user the difference (SPEC rule 3's 提示差异).
	assertRefusalShowsTheDifference(t, refusal, st, "kit", baseline)
	after, err := os.ReadFile(filepath.Join(st.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refusal must leave the hand edit in place")
	}

	outcome, err := app.UpdateSkills("kit", true)
	if err != nil {
		t.Fatalf("--force must overwrite: %v", err)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "kit" {
		t.Fatalf("outcome = %+v, want kit in Updated", outcome)
	}
	got, err := os.ReadFile(filepath.Join(st.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "version two") {
		t.Fatalf("forced update did not land:\n%s", got)
	}
}

// TestUpdateSkillsBatchSkipsLocallyModifiedAndUpdatesTheRest pins the batch
// half of SPEC rule 3: a locally modified skill is skipped, not failed, and
// does not block the rest of the batch.
func TestUpdateSkillsBatchSkipsLocallyModifiedAndUpdatesTheRest(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	modSrc, _ := installedFromLocal(t, st, cfg, "api-docs", "version one")
	writeSkillBody(t, modSrc, "api-docs", "version two")
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "api-docs"), "api-docs", "hand edited")

	cleanSrc, _ := installedFromLocal(t, st, cfg, "pdf-tools", "version one")
	writeSkillBody(t, cleanSrc, "pdf-tools", "version two")

	outcome, err := app.UpdateSkills("", false)
	if err != nil {
		t.Fatalf("a skip must not fail the batch: %v", err)
	}
	if len(outcome.Skipped) != 1 || outcome.Skipped[0].Name != "api-docs" || outcome.Skipped[0].Reason != "locally modified" {
		t.Fatalf("outcome.Skipped = %+v, want exactly api-docs/locally modified", outcome.Skipped)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "pdf-tools" {
		t.Fatalf("outcome.Updated = %+v, want pdf-tools", outcome.Updated)
	}
	got, err := os.ReadFile(filepath.Join(st.SkillsDir(), "api-docs", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "hand edited") {
		t.Fatal("a skipped skill's hand edit must be left in place")
	}
}

// TestUpdateSkillsBatchIsolatesARenamedUpstreamFailureAndContinues pins that
// a per-skill failure (here: SPEC rule 1's rename refusal, checkUpdateAvailable
// in update.go) is isolated into outcome.Reconcile.Failed and the batch
// continues -- addSkillsDetailed's own semantics (add.go) -- while still
// surfacing ErrOperationFailed overall, since something in the run did fail.
func TestUpdateSkillsBatchIsolatesARenamedUpstreamFailureAndContinues(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	renamedSrc, _ := installedFromLocal(t, st, cfg, "old-name", "version one")
	writeSkillBody(t, renamedSrc, "new-name", "version two") // upstream renamed the skill

	goodSrc, _ := installedFromLocal(t, st, cfg, "pdf-tools", "version one")
	writeSkillBody(t, goodSrc, "pdf-tools", "version two")

	outcome, err := app.UpdateSkills("", false)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("an isolated failure must still surface as ErrOperationFailed, got %v", err)
	}
	if len(outcome.Reconcile.Failed) != 1 || outcome.Reconcile.Failed[0].Action.Skill != "old-name" {
		t.Fatalf("outcome.Reconcile.Failed = %+v, want exactly one entry naming old-name", outcome.Reconcile.Failed)
	}
	if !strings.Contains(outcome.Reconcile.Failed[0].Err.Error(), "new-name") {
		t.Fatalf("the failure must name the rename: %v", outcome.Reconcile.Failed[0].Err)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "pdf-tools" {
		t.Fatalf("the isolated failure must not block the rest of the batch: %+v", outcome)
	}
}

// Design §7 names three rule-landing tests as required: the upstream-rename
// refusal (rule 1, pinned by the test above), the `subdir` disappearance
// refusal, and the non-compliant refusal (rule 7). The last two are the two arms
// of candidateAt (application.go) and neither had a test. Both already behave
// correctly -- they were simply unpinned -- and the arms are worth holding down
// for a reason beyond coverage: they are told apart only by ps.invalid's key
// convention, filepath.FromSlash(c.Dir) as ScanSource spells it. If the two
// sides ever stopped agreeing on how a subdir is written, the invalid-skill arm
// would go quiet and every such skill would be reported as one that vanished,
// sending the user to look for a directory that is sitting right there.
//
// Driven through the batch, so each case also shows the refusal is isolated: the
// sibling skill in the same repository still updates.
// Both arms are driven over a real ScanSource result rather than a hand-built
// map, because what distinguishes them is which of ScanSource's two outputs the
// recorded subdir turns up in: `invalid` is keyed by filepath.FromSlash of the
// scanned directory (add.go), and candidateAt looks it up under the spelling
// fu.yaml recorded. If those two ever stopped agreeing, the invalid arm would go
// quiet and every skill whose SKILL.md had merely become non-compliant would be
// reported as one that vanished upstream -- sending the user to look for a
// directory sitting right where they left it.
func TestCandidateAtTellsAVanishedSubdirFromAnInvalidOne(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	writeSkillBody(t, filepath.Join(repo, "good"), "good", "one")
	// Present, readable, and refused by skill validation: a blank description
	// is rule 7's own boundary, so this lands in ScanSource's invalid map.
	if err := os.MkdirAll(filepath.Join(repo, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: broken\ndescription: \"\"\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(repo, "broken", "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	candidates, invalid, err := ScanSource(prepareLocal(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	ps := &preparedUpdateSource{candidates: candidates, invalid: invalid}

	if _, err := ps.candidateAt("good"); err != nil {
		t.Fatalf("a subdir that still holds a valid skill must resolve: %v", err)
	}

	_, err = ps.candidateAt("broken")
	if err == nil || !strings.Contains(err.Error(), `recorded subdir "broken" is no longer a valid skill`) {
		t.Fatalf("a present-but-invalid subdir must say so: %v", err)
	}

	_, err = ps.candidateAt("gone")
	if err == nil || !strings.Contains(err.Error(), `recorded subdir "gone" no longer holds a valid skill upstream`) {
		t.Fatalf("a subdir that is not there at all must say so: %v", err)
	}
}

// The rule-7 landing end to end: a skill whose upstream SKILL.md stopped
// validating is refused by name, the refusal is isolated, its sibling in the
// same repository still updates, and staging is left clean.
//
// Only this arm is reachable through the batch for a local source: deleting the
// subdir outright is caught earlier and more precisely by judgeLocalUpdate,
// which stats the recorded path and reports it Unjudged (SPEC rule 9), so the
// vanished arm is exercised directly by the test above instead.
func TestUpdateSkillsRefusesASubdirThatNoLongerHoldsAValidSkill(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	repo := filepath.Join(t.TempDir(), "repo")
	for _, name := range []string{"broken", "pdf-tools"} {
		writeSkillBody(t, filepath.Join(repo, name), name, "one")
		writeSkillBody(t, filepath.Join(st.SkillsDir(), name), name, "one")
		payload, err := checkedRecoveryStore(t, st).SnapshotSkillPayload(name)
		if err != nil {
			t.Fatal(err)
		}
		baseline, err := digestOwnedPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.AddSkill(name, baseline); err != nil {
			t.Fatal(err)
		}
		cfg.SetSourceFields(name, map[string]string{"type": "local", "path": repo, "subdir": name})
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// Both move upstream, so both are genuinely updatable and the refusal is
	// about the recorded subdir and nothing else.
	writeSkillBody(t, filepath.Join(repo, "broken"), "broken", "two")
	writeSkillBody(t, filepath.Join(repo, "pdf-tools"), "pdf-tools", "two")
	doc := "---\nname: broken\ndescription: \"\"\n---\n\ntwo\n"
	if err := os.WriteFile(filepath.Join(repo, "broken", "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := app.UpdateSkills("", false)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("a refused subdir must surface as ErrOperationFailed, got %v (outcome=%+v)", err, outcome)
	}
	if len(outcome.Reconcile.Failed) != 1 || outcome.Reconcile.Failed[0].Action.Skill != "broken" {
		t.Fatalf("outcome.Reconcile.Failed = %+v, want exactly one entry naming broken", outcome.Reconcile.Failed)
	}
	if got := outcome.Reconcile.Failed[0].Err.Error(); !strings.Contains(got, `recorded subdir "broken" is no longer a valid skill`) {
		t.Fatalf("failure = %q, want candidateAt's invalid-subdir wording", got)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "pdf-tools" {
		t.Fatalf("the sibling in the same repository must still update: %+v", outcome)
	}
	assertStagingEmpty(t, st)
}

// TestUpdateSkillsDedupsSourcePreparationForASharedLocalPath pins that two
// skills sharing one repository (the subdir source field exists precisely
// for this, DESIGN §3) prepare that source exactly once, mirroring Outdated's
// own ls-remote dedup (TestOutdatedResolvesAGitSourceOnceForManySkillsAndSortsByName).
func TestUpdateSkillsDedupsSourcePreparationForASharedLocalPath(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	parent := filepath.Join(t.TempDir(), "repo")
	writeSkillBody(t, filepath.Join(parent, "foo"), "foo", "one")
	writeSkillBody(t, filepath.Join(parent, "bar"), "bar", "one")
	for _, name := range []string{"foo", "bar"} {
		writeSkillBody(t, filepath.Join(st.SkillsDir(), name), name, "one")
	}
	for _, name := range []string{"foo", "bar"} {
		payload, err := checkedRecoveryStore(t, st).SnapshotSkillPayload(name)
		if err != nil {
			t.Fatal(err)
		}
		baseline, err := digestOwnedPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.AddSkill(name, baseline); err != nil {
			t.Fatal(err)
		}
		cfg.SetSourceFields(name, map[string]string{"type": "local", "path": parent, "subdir": name})
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	writeSkillBody(t, filepath.Join(parent, "foo"), "foo", "two")
	writeSkillBody(t, filepath.Join(parent, "bar"), "bar", "two")

	var prepareCount int
	updateSourcePreparedHook = func(updateSourceKey) { prepareCount++ }
	t.Cleanup(func() { updateSourcePreparedHook = nil })

	outcome, err := app.UpdateSkills("", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if prepareCount != 1 {
		t.Fatalf("two skills sharing one repository must prepare its source once, got %d", prepareCount)
	}
	if len(outcome.Updated) != 2 {
		t.Fatalf("both skills sharing the source must still update: %+v", outcome)
	}
}

// TestUpdateSkillsPreparesAllSourcesOutsideTheLock pins the design spec's
// lock-boundary rule (task 9's own binding correction): network I/O must never
// happen while the write lock is held, and every distinct source must be
// prepared in one run rather than interleaved one skill's prepare-then-update
// at a time.
//
// Both halves are measured, because neither implies the other. It previously
// asserted the single stricter proxy "every prepare precedes the first lock
// acquisition", which is not the rule and stopped being equivalent to it the
// moment UpdateSkills gained its writeCommandPrologue: that prologue takes and
// releases the lock before any clone, exactly as `fu add` does, so it is the
// first acquisition and satisfies §4.4 perfectly while failing the proxy. The
// two properties are now stated directly:
//
//   - No prepare falls between a matched acquire/release pair (lockAcquiredHook
//     and lockReleasedHook, lock.go). This is §4.4 itself.
//   - No acquisition falls between the first prepare and the last. This is what
//     the old check was really protecting: a naive per-skill loop (prepare
//     alpha; update alpha [lock]; prepare beta; update beta [lock]) puts a lock
//     between two prepares and still fails, even though every one of its
//     prepares happens outside the lock.
//
// Two distinct sources are required for the second half: with only one,
// "prepared before its own use" is guaranteed by updateSkill's signature and
// would prove nothing.
func TestUpdateSkillsPreparesAllSourcesOutsideTheLock(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	srcA, _ := installedFromLocal(t, st, cfg, "alpha", "one")
	srcB, _ := installedFromLocal(t, st, cfg, "beta", "one")
	writeSkillBody(t, srcA, "alpha", "two")
	writeSkillBody(t, srcB, "beta", "two")

	var events []string
	updateSourcePreparedHook = func(updateSourceKey) { events = append(events, "prepare") }
	lockAcquiredHook = func(string) { events = append(events, "acquire") }
	lockReleasedHook = func(string) { events = append(events, "release") }
	t.Cleanup(func() {
		updateSourcePreparedHook = nil
		lockAcquiredHook = nil
		lockReleasedHook = nil
	})

	outcome, err := app.UpdateSkills("", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(outcome.Updated) != 2 {
		t.Fatalf("both skills must update: %+v", outcome)
	}

	held, prepares, firstPrepare, lastPrepare := 0, 0, -1, -1
	for i, e := range events {
		switch e {
		case "acquire":
			held++
		case "release":
			held--
		case "prepare":
			if held != 0 {
				t.Fatalf("a source was prepared while the write lock was held; events = %v", events)
			}
			prepares++
			if firstPrepare == -1 {
				firstPrepare = i
			}
			lastPrepare = i
		}
	}
	if prepares != 2 {
		t.Fatalf("both distinct sources must be prepared; events = %v", events)
	}
	if held != 0 {
		t.Fatalf("every acquisition must be released; events = %v", events)
	}
	for _, e := range events[firstPrepare:lastPrepare] {
		if e == "acquire" {
			t.Fatalf("the batch must prepare every distinct source in one run, not interleave prepares with per-transaction locks; events = %v", events)
		}
	}
	// The lock is genuinely exercised, so the interval checks above are not
	// vacuously true over an empty set of acquisitions.
	if !slices.Contains(events, "acquire") {
		t.Fatalf("the lock hooks never fired; this test proves nothing: %v", events)
	}
}

// TestUpdateSkillsRecoversAnInterruptedUpdateBeforeJudging puts `fu update` on
// the same mandatory recovery boundary as every other write command. Recovery
// used to happen only inside run (pipeline.go), which a batch with zero targets
// or a refused named skill never reaches -- so UpdateSkills was the one
// discovery-first write command with no writeCommandPrologue call, while both
// its siblings (add_command.go, adopt.go) make one for exactly this reason and
// the helper's own doc says it is "intentionally complete even when discovery
// later yields zero operations".
//
// The consequence is worse than a skipped chore, because the interruption also
// corrupts the judgement: an update killed before its exchange has already
// saved the new digest, so the baseline now equals the source digest and
// Outdated reports Updatable=false, LocallyModified=true. `fu update` then
// exits 0 saying "nothing to update" over an unhealed WAL, and `fu update kit`
// fails with ErrLocallyModified and offers the destructive --force. DESIGN §2's
// promise that any interruption is settled by the next ordinary write command
// did not hold here.
//
// The prologue must run before outdatedFor, not merely before the first
// transaction: the wrong verdict above is computed from the unrecovered state,
// so judging first would still yield "nothing to update" with a healed WAL.
func TestUpdateSkillsRecoversAnInterruptedUpdateBeforeJudging(t *testing.T) {
	s, _ := crashUpdateAt(t, "before-exchange")
	t.Setenv("FU_HOME", s.Home)
	t.Setenv("HOME", t.TempDir())

	outcome, err := NewApplication().UpdateSkills("", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}

	reopened, err := store.Open(s.Home)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingTxns(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("update must reach the recovery boundary and settle the interruption, still pending: %+v", pending)
	}
	// The payoff of recovering *before* judging: with the rollback applied the
	// baseline is the old digest again, so the skill is genuinely updatable and
	// the same run performs the update the user asked for.
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "kit" {
		t.Fatalf("the recovered store must judge kit updatable and update it: %+v", outcome)
	}
}

// TestUpdateSkillsRefusesForceWithoutAName pins fix round 1's minor finding:
// Application is the shared boundary a future front end calls directly
// (PrepareAdd/ErrInvalidAddRef is the precedent), so the force-without-name
// refusal must hold here on its own, not only in internal/cli/update.go's
// own guard.
func TestUpdateSkillsRefusesForceWithoutAName(t *testing.T) {
	app, _, _ := applicationEnv(t)
	if _, err := app.UpdateSkills("", true); !errors.Is(err, ErrUpdateForceNeedsName) {
		t.Fatalf("error = %v, want ErrUpdateForceNeedsName", err)
	}
}

// TestUpdateSkillsBatchAbortRecordsTheTriggerAndTheUnattemptedRemainder pins
// fix round 1 Important finding 2: applyUpdateTargets's batchFatal branch
// used to be a bare `return err`, dropping which target triggered the abort
// and which later targets were never attempted -- addSkillsDetailed's own
// equivalent (add.go:362-370) always records both before returning, and
// AddOutcome.Unattempted's own comment says why ("round 18 finding I19").
//
// afterCommit is injected via newApplication's test-only hooks seam
// (pipeline.go's hooks struct) and fails only on its second call, so
// a-first's own transaction commits cleanly (proving the batch actually
// started) while b-second's is the one that trips
// Committed-but-not-CanonicalChecked -- exactly the batchFatal condition
// this finding is about -- and c-third, alphabetically last, is never
// reached.
func TestUpdateSkillsBatchAbortRecordsTheTriggerAndTheUnattemptedRemainder(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls int
	boom := errors.New("boom: simulated commit-phase interruption")
	app := newApplication(hooks{afterCommit: func() error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	}})
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}
	st, err := app.openStore()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a-first", "b-second", "c-third"} {
		src, _ := installedFromLocal(t, st, cfg, name, "version one")
		writeSkillBody(t, src, name, "version two")
	}

	outcome, err := app.UpdateSkills("", false)
	if err == nil {
		t.Fatal("expected the injected commit-phase failure to abort the batch")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the triggering failure", err)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "a-first" {
		t.Fatalf("outcome.Updated = %v, want exactly a-first (attempted before the trigger)", outcome.Updated)
	}
	if len(outcome.Reconcile.Failed) != 1 || outcome.Reconcile.Failed[0].Action.Skill != "b-second" {
		t.Fatalf("outcome.Reconcile.Failed = %+v, want exactly one entry naming the trigger b-second", outcome.Reconcile.Failed)
	}
	if len(outcome.Unattempted) != 1 || outcome.Unattempted[0] != "c-third" {
		t.Fatalf("outcome.Unattempted = %v, want exactly c-third (never reached)", outcome.Unattempted)
	}
}

// TestUpdateSkillsNamedLocallyModifiedRefusalNamesWhatForceWillDo pins the
// wording of selectUpdateTargets's refusal against what --force actually does.
//
// Its history is the reason it is worth reading. Round 1 found the message
// promising "re-run with --force to overwrite the local changes" in a state
// where --force provably overwrote nothing: upstream had not moved, so
// updateSkill took the lock-only shape, which writes only fu.yaml. The fix
// then hedged the message ("replacing those changes if the upstream content
// differs") and this test pinned the hedge -- and, with it, the behaviour that
// --force was a no-op here.
//
// Round 3 called that behaviour what it was: --force's own help promises to
// overwrite local modifications, it did not, and the skill was left
// advertising a refusal no command could clear. The fix routes --force through
// the content shape whenever the store copy has drifted, so the unconditional
// promise is true again and the hedge would now understate a destructive flag.
// What this test pins today is that agreement: the refusal says plainly what
// --force will do, and
// TestUpdateSkillsForceOverwritesLocalModificationsWhenUpstreamIsUnchanged
// pins that it does it.
func TestUpdateSkillsNamedLocallyModifiedRefusalNamesWhatForceWillDo(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	installedFromLocal(t, st, cfg, "kit", "version one")
	// Only the store copy moves: the source still holds the installed content,
	// so without --force this is the state that must refuse.
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "kit"), "kit", "hand edited")

	_, err := app.UpdateSkills("kit", false)
	if !errors.Is(err, ErrLocallyModified) {
		t.Fatalf("error = %v, want ErrLocallyModified", err)
	}
	if !strings.Contains(err.Error(), "replace those changes with the upstream content") {
		t.Fatalf("the refusal must name what --force will do: %v", err)
	}
	// The old hedge, kept as a negative assertion so the conditional wording
	// cannot come back now that the condition is gone.
	if strings.Contains(err.Error(), "if the upstream content differs") {
		t.Fatalf("--force replaces the local changes whether or not upstream moved: %v", err)
	}
	// The refusal itself changes nothing.
	got, err := os.ReadFile(filepath.Join(st.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "hand edited") {
		t.Fatalf("a refusal must leave the hand edit in place:\n%s", got)
	}
}

// TestUpdateSkillsNamedJudgesOnlyTheNamedSkillsSource pins fix round 2's
// Important #1: UpdateSkills used to call Outdated over the whole config and
// only afterwards discard every row but the named one, so `fu update kit`
// resolved every other skill's remote first -- each under its own fresh
// 2-minute timeout (judgeGitUpdate, outdated.go) -- and blocked on remotes
// belonging to a skill the user never named. A skill with no remote at all
// dragged the same fan-out along.
//
// The unrelated source here is a real, reachable file:// repository rather
// than an unreachable one: an unreachable remote would prove the same point
// by hanging, which is not a shape a test can assert on quickly. What is
// asserted instead is exact and stronger -- the resolve production would
// perform must not happen at all, counted through the same
// resolveRemoteRefHook seam outdated_test.go's dedup test uses.
func TestUpdateSkillsNamedJudgesOnlyTheNamedSkillsSource(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	srcDir, _ := installedFromLocal(t, st, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "kit", "version two")

	url, ref, commit := realGitBranchFixture(t, "pdf-tools")
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "pdf-tools"), "pdf-tools", "one")
	if err := cfg.AddSkill("pdf-tools", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("pdf-tools", map[string]string{
		"type": "git", "url": url, "ref": ref, "ref_kind": "branch", "commit": commit,
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var calls []string
	resolveRemoteRefHook = func(gotURL, gotRef string) { calls = append(calls, gotURL+"@"+gotRef) }
	t.Cleanup(func() { resolveRemoteRefHook = nil })

	outcome, err := app.UpdateSkills("kit", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "kit" {
		t.Fatalf("outcome = %+v, want kit updated", outcome)
	}
	if len(calls) != 0 {
		t.Fatalf("a named update must not contact any other skill's remote, resolved: %v", calls)
	}
}

// TestUpdateSkillsBatchStillJudgesEveryRegisteredSkill is the other half of
// the finding above, and the reason the name filter is applied to the name
// rather than pushed down into judgeUpdate: the batch has to keep polling
// every source, since it has no name to narrow by and Outdated's verdict is
// what selects its targets (selectUpdateTargets). Without this, the fix for
// the named case could quietly narrow the batch too and nothing would notice.
func TestUpdateSkillsBatchStillJudgesEveryRegisteredSkill(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	installedFromLocal(t, st, cfg, "kit", "version one")

	url, ref, commit := realGitBranchFixture(t, "pdf-tools")
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "pdf-tools"), "pdf-tools", "one")
	if err := cfg.AddSkill("pdf-tools", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("pdf-tools", map[string]string{
		"type": "git", "url": url, "ref": ref, "ref_kind": "branch", "commit": commit,
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var calls []string
	resolveRemoteRefHook = func(gotURL, gotRef string) { calls = append(calls, gotURL+"@"+gotRef) }
	t.Cleanup(func() { resolveRemoteRefHook = nil })

	if _, err := app.UpdateSkills("", false); err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("a batch update must judge every registered source, resolved: %v", calls)
	}
}

// TestUpdateSkillsForceOverwritesLocalModificationsWhenUpstreamIsUnchanged
// pins round 3's only behavioural finding. `--force` promises to overwrite a
// skill's local modifications, and when upstream had not moved it did not: the
// shape decision compares the *source* digest against the baseline, neither
// side being the store copy, so an unmoved upstream took the lock-only branch
// -- which never consults force, never touches skills/<name> and never moves
// the baseline. The command exited 0 reporting no content change, the hand
// edit survived, and `fu outdated` advertised a refusal that no command could
// clear.
//
// TestUpdateSkillsNamedLocallyModifiedDoesNotPromiseAnOverwriteItCannotMake is
// this test's sibling and pins the other half: *without* --force the same
// state must still refuse and must not claim an overwrite is coming.
func TestUpdateSkillsForceOverwritesLocalModificationsWhenUpstreamIsUnchanged(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	installedFromLocal(t, st, cfg, "kit", "version one")
	// Only the store copy moves: upstream still holds the installed content,
	// so the source-vs-baseline comparison finds nothing to pull.
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "kit"), "kit", "hand edited")

	outcome, err := app.UpdateSkills("kit", true)
	if err != nil {
		t.Fatalf("--force must proceed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(st.SkillsDir(), "kit", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "hand edited") {
		t.Fatalf("--force must overwrite the local modification it names:\n%s", got)
	}
	if !strings.Contains(string(got), "version one") {
		t.Fatalf("the store copy must be restored to the upstream content:\n%s", got)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "kit" {
		t.Fatalf("outcome = %+v, want kit reported as updated content, not as a lock-only run", outcome)
	}
	// And the skill stops advertising a refusal, which is what made the old
	// behaviour a dead end rather than merely a no-op.
	reloaded, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Outdated(st, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].UpdateRefusesWithoutForce() {
		t.Fatalf("after --force the skill must no longer be locally modified: %+v", rows)
	}
	// SPEC rule 3's backstop: what was overwritten is still in git history.
	assertHistoryHolds(t, st, "skills/kit/SKILL.md", "hand edited")
}

// The force arm must not fire when there is genuinely nothing to do: an
// unmodified, already-current skill still takes the lock-only shape, so
// --force does not manufacture an exchange (and a commit) out of a no-op.
func TestUpdateSkillsForceStaysLockOnlyWhenTheStoreCopyIsClean(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	installedFromLocal(t, st, cfg, "kit", "version one")

	outcome, err := app.UpdateSkills("kit", true)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(outcome.LockOnly) != 1 || len(outcome.Updated) != 0 {
		t.Fatalf("outcome = %+v, want the lock-only shape for a clean, current skill", outcome)
	}
}

// TestUpdateSkillsBatchReportsRowsItCouldNotJudge pins round 3's Important #8.
// A batch dropped non-comparable rows silently -- no Skipped entry, no
// Unattempted entry, nothing -- so `fu update` offline printed "nothing to
// update" and exited 0 over skills whose state it had failed to determine.
// "Could not tell" and "up to date" are the two answers this command acts on
// differently, and it was conflating them.
//
// The fixture is a local source whose directory has been removed, which is
// the offline case without a network: transient in exactly the way an
// unreachable remote is, and so exactly the class that owes the user a line.
// Round 4 narrowed the rule to that class -- see
// TestUpdateSkillsBatchStaysSilentAboutRowsWithNoUpstream for the other half,
// which this test used to stand for and no longer does.
func TestUpdateSkillsBatchReportsRowsItCouldNotJudge(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	srcDir, _ := installedFromLocal(t, st, cfg, "kit", "version one")
	if err := os.RemoveAll(srcDir); err != nil {
		t.Fatal(err)
	}

	outcome, err := app.UpdateSkills("", false)
	if err != nil {
		t.Fatalf("an unjudgeable row degrades, it does not fail the batch: %v", err)
	}
	if len(outcome.Unjudged) != 1 || outcome.Unjudged[0].Name != "kit" {
		t.Fatalf("outcome.Unjudged = %+v, want exactly kit", outcome.Unjudged)
	}
	if outcome.Unjudged[0].Reason == "" {
		t.Fatal("an unjudged row must carry Outdated's own reason")
	}
	if len(outcome.Skipped) != 0 {
		t.Fatalf("an unjudged row is not a skip -- --force does not help it: %+v", outcome.Skipped)
	}
}

// TestUpdateSkillsBatchStaysSilentAboutRowsWithNoUpstream pins round 4's other
// half. A skill created by `fu new` and a skill pinned to a tag are both
// judged exactly: there is no upstream, so there is nothing to pull, ever.
// Round 3 filed them under "could not judge" alongside the genuinely
// indeterminate rows, which meant every store holding one printed a line per
// such skill on every single run -- and suppressed "nothing to update" while
// doing it, because the CLI counts Unjudged. That trains a reader to ignore
// the channel skipped:, failed: and not attempted: also arrive on.
//
// Neither row reaches the network: judgeGitUpdate returns on ref_kind before
// resolving anything, which is why a tag-pinned record can name an
// unreachable URL here without making this test depend on one.
func TestUpdateSkillsBatchStaysSilentAboutRowsWithNoUpstream(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "homegrown"), "homegrown", "one")
	if err := cfg.AddSkill("homegrown", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "pinned"), "pinned", "one")
	if err := cfg.AddSkill("pinned", "sha256:y"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("pinned", map[string]string{
		"type": "git", "url": "https://example.invalid/repo.git",
		"ref": "refs/tags/v1", "ref_kind": "tag", "commit": strings.Repeat("a", 40),
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	outcome, err := app.UpdateSkills("", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(outcome.Unjudged) != 0 {
		t.Fatalf("outcome.Unjudged = %+v, want nothing: neither row is a failure to determine anything", outcome.Unjudged)
	}
	// Every slice empty is what makes the CLI print "nothing to update"
	// (internal/cli/update.go), the positive confirmation these rows used to
	// suppress.
	if len(outcome.Updated) != 0 || len(outcome.LockOnly) != 0 ||
		len(outcome.Skipped) != 0 || len(outcome.Unattempted) != 0 {
		t.Fatalf("outcome = %+v, want an entirely empty batch", outcome)
	}
	// The rows are still judged, and `fu outdated` still reports them: what
	// changed is only which command speaks up about them.
	rows, err := Outdated(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Comparable || !row.NoUpstream || row.Reason == "" {
			t.Fatalf("row %+v must still be a reported, non-comparable row with a reason", row)
		}
	}
}

// TestSourceFromFieldsCarriesTheRecordedRefKind pins both halves of round 4's
// folded `sourceFromFields` finding. The reconstruction used to discard
// ref_kind and strip both prefixes, which is one defect seen from two angles:
// it threw away information the record already carried.
//
//   - Dropping the kind let cloneSource fall back to its branch-then-tag probe
//     (internal/source/git.go). A branch deleted upstream in the window
//     between `fu outdated`'s ls-remote and update's clone would then resolve
//     to a same-named tag: the clone succeeds, EncodeFields writes
//     `ref_kind: tag`, and the skill silently becomes a fixed lock `outdated`
//     never examines again.
//   - Stripping both prefixes rewrote a branch legitimately named
//     "refs/tags/release" -- recorded as `refs/heads/refs/tags/release` -- into
//     "release", so update cloned a different ref than the one it went on
//     recording. Unreachable through `--ref`, which refuses a fully qualified
//     value (source.ParseArgWithRef), but reachable through a remote default
//     branch or a hand edit.
func TestSourceFromFieldsCarriesTheRecordedRefKind(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ref, kind   string
		wantRef     string
		wantRefKind string
	}{
		{name: "branch", ref: "refs/heads/main", kind: "branch", wantRef: "main", wantRefKind: "branch"},
		{name: "tag", ref: "refs/tags/v1", kind: "tag", wantRef: "v1", wantRefKind: "tag"},
		{
			name: "branch named like a tag", ref: "refs/heads/refs/tags/release", kind: "branch",
			wantRef: "refs/tags/release", wantRefKind: "branch",
		},
		{name: "unknown kind strips nothing", ref: "refs/heads/main", kind: "", wantRef: "refs/heads/main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, err := sourceFromFields(map[string]string{
				"type": "git", "url": "https://example.invalid/repo.git",
				"ref": tc.ref, "ref_kind": tc.kind,
			})
			if err != nil {
				t.Fatal(err)
			}
			if src.Ref != tc.wantRef {
				t.Fatalf("Ref = %q, want %q", src.Ref, tc.wantRef)
			}
			if src.RefKind != tc.wantRefKind {
				t.Fatalf("RefKind = %q, want %q -- the clone must probe only the recorded form", src.RefKind, tc.wantRefKind)
			}
		})
	}
}

// A record's ref_kind is part of which clone it names, so two skills agreeing
// on url and ref but not on kind must not share one prepared source.
func TestUpdateSourceKeyDistinguishesABranchFromASameNamedTag(t *testing.T) {
	common := map[string]string{"type": "git", "url": "https://example.invalid/repo.git", "ref": "release"}
	branch, tag := map[string]string{}, map[string]string{}
	for key, value := range common {
		branch[key], tag[key] = value, value
	}
	branch["ref_kind"], tag["ref_kind"] = "branch", "tag"
	if sourceKeyFromFields(branch) == sourceKeyFromFields(tag) {
		t.Fatal("a branch and a same-named tag must not deduplicate onto one clone")
	}
}

// makeTwoBranchGitSource builds one bare repository holding a different skill
// on each of two branches, and returns its URL plus the two fully qualified
// ref names. Both skills therefore share a URL and differ only by ref, which
// is the shape the dedup keys have to tell apart.
func makeTwoBranchGitSource(t *testing.T, firstSkill, secondSkill string) (url, firstRef, secondRef string) {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(message string) {
		t.Helper()
		if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Commit(message, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}}); err != nil {
			t.Fatal(err)
		}
	}
	writeSkillBody(t, filepath.Join(work, firstSkill), firstSkill, "FIRST v1")
	commit("first")
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	firstRef = head.Name().String()

	second := plumbing.NewBranchReferenceName(secondSkill + "-branch")
	if err := wt.Checkout(&git.CheckoutOptions{Branch: second, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(work, firstSkill)); err != nil {
		t.Fatal(err)
	}
	writeSkillBody(t, filepath.Join(work, secondSkill), secondSkill, "SECOND v1")
	commit("second")
	secondRef = second.String()

	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(&git.PushOptions{RefSpecs: []config.RefSpec{
		config.RefSpec(firstRef + ":" + firstRef),
		config.RefSpec(secondRef + ":" + secondRef),
	}}); err != nil {
		t.Fatal(err)
	}
	return "file://" + bare, firstRef, secondRef
}

// advanceGitBranch is advanceGitSource for a named branch of a multi-branch
// fixture: it checks that branch out, applies mutate, and pushes it back.
func advanceGitBranch(t *testing.T, url, ref string, mutate func(work string)) string {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainClone(work, false, &git.CloneOptions{
		URL: url, ReferenceName: plumbing.ReferenceName(ref), SingleBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mutate(work)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	advanced, err := wt.Commit("advance "+ref, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(ref + ":" + ref)},
	}); err != nil {
		t.Fatal(err)
	}
	return advanced.String()
}

// TestTwoSkillsFromOneRepositoryOnDifferentRefsStayApart pins round 3's
// Important #2, the finding its own review called the worst outcome anywhere
// in this change. Two skills installed from one repository on two branches is
// an ordinary install, and the two dedup keys -- updateSourceKey for source
// preparation and outdated.go's (url, ref) resolve cache -- are the one place
// a shared cache can hand skill B skill A's answer. Neither had a guard.
//
// Both surviving mutations the review reproduced are killed here: dropping
// `ref` from updateSourceKey published the wrong branch's content *and*
// silently rewrote the tracked ref; dropping it from the resolve key reported
// one branch's head under the other's ref.
func TestTwoSkillsFromOneRepositoryOnDifferentRefsStayApart(t *testing.T) {
	app, st, _ := applicationEnv(t)
	url, firstRef, secondRef := makeTwoBranchGitSource(t, "gamma", "delta")

	for _, ref := range []string{firstRef, secondRef} {
		preparation, err := PrepareAdd(st, url, strings.TrimPrefix(ref, "refs/heads/"))
		if err != nil {
			t.Fatal(err)
		}
		plan := preparation.Session.(*AddPlan)
		_, installErr := plan.Install(plan.Candidates())
		closeErr := plan.Close()
		if installErr != nil {
			t.Fatal(installErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}

	gammaHead := advanceGitBranch(t, url, firstRef, func(work string) {
		writeSkillBody(t, filepath.Join(work, "gamma"), "gamma", "FIRST v2")
	})
	deltaHead := advanceGitBranch(t, url, secondRef, func(work string) {
		writeSkillBody(t, filepath.Join(work, "delta"), "delta", "SECOND v2")
	})
	if gammaHead == deltaHead {
		t.Fatal("fixture error: the two branches must advance to different commits")
	}

	// The resolve cache: each row must carry its own branch's head.
	reloaded, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Outdated(st, reloaded)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	latest := map[string]string{}
	for _, row := range rows {
		if !row.Updatable {
			t.Fatalf("both branches advanced, so both rows must be updatable: %+v", row)
		}
		latest[row.Name] = row.Latest
	}
	if latest["gamma"] != gammaHead || latest["delta"] != deltaHead {
		t.Fatalf("each skill must be judged against its own ref: gamma=%q (want %q), delta=%q (want %q)",
			latest["gamma"], gammaHead, latest["delta"], deltaHead)
	}

	// The prepare-side key: two distinct sources, and each skill gets its own
	// branch's content.
	var prepared int
	updateSourcePreparedHook = func(updateSourceKey) { prepared++ }
	t.Cleanup(func() { updateSourcePreparedHook = nil })

	if _, err := app.UpdateSkills("", false); err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if prepared != 2 {
		t.Fatalf("two refs of one repository are two sources, prepared %d", prepared)
	}
	for name, want := range map[string]string{"gamma": "FIRST v2", "delta": "SECOND v2"} {
		got, err := os.ReadFile(filepath.Join(st.SkillsDir(), name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), want) {
			t.Fatalf("%s must receive its own branch's content, got:\n%s", name, got)
		}
	}
	// A silently rewritten tracked ref is the other half of that mutation.
	for name, want := range map[string]string{"gamma": firstRef, "delta": secondRef} {
		if got := recordedSourceFields(t, st, name)["ref"]; got != want {
			t.Fatalf("%s's tracked ref = %q, want %q", name, got, want)
		}
	}
}

// TestUpdateSkillsNamedRefusesAnUnknownSkill pins round 3's Important #9's
// first row: nothing at the engine or CLI layer tested the unknown-name error,
// so mutating it to `return nil, nil` left the suite green -- and `fu update
// typo` would have exited 0 saying "nothing to update", which is the worst
// possible answer to a typo.
func TestUpdateSkillsNamedRefusesAnUnknownSkill(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	installedFromLocal(t, st, cfg, "kit", "version one")

	outcome, err := app.UpdateSkills("typo", false)
	if err == nil {
		t.Fatalf("an unknown name must fail, got outcome %+v", outcome)
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Fatalf("the error must name what was not found: %v", err)
	}
	if len(outcome.Updated) != 0 || len(outcome.LockOnly) != 0 {
		t.Fatalf("an unknown name must touch nothing: %+v", outcome)
	}
}

// TestUpdateSkillsBatchLeavesCurrentSkillsAlone pins the second row: the batch
// filter drops rows that are not Updatable, and mutating it to test
// comparability alone left the suite green -- the batch would then clone every
// source and lock-only every current skill. The existing skip fixture could not
// see it because its skipped skill was both updatable and modified.
func TestUpdateSkillsBatchLeavesCurrentSkillsAlone(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	// Current: source and store both hold what was installed.
	installedFromLocal(t, st, cfg, "current", "version one")
	// Updatable: only the source moved.
	movedSrc, _ := installedFromLocal(t, st, cfg, "moved", "version one")
	writeSkillBody(t, movedSrc, "moved", "version two")

	var prepared int
	updateSourcePreparedHook = func(updateSourceKey) { prepared++ }
	t.Cleanup(func() { updateSourcePreparedHook = nil })

	outcome, err := app.UpdateSkills("", false)
	if err != nil {
		t.Fatalf("UpdateSkills: %v", err)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "moved" {
		t.Fatalf("outcome.Updated = %v, want only the skill whose source moved", outcome.Updated)
	}
	if len(outcome.LockOnly) != 0 {
		t.Fatalf("a current skill is not a target, so no lock may be rewritten for it: %+v", outcome.LockOnly)
	}
	if prepared != 1 {
		t.Fatalf("only the updatable skill's source may be prepared, prepared %d", prepared)
	}
}

// TestReclaimUpdateStagingPayloadRefusesAReservedName pins round 3's Minor #4:
// the public-name guard added in round 2 had no test, and its removal survived
// the whole engine package. gc reaches this with a Name taken straight off a
// completed family's record, which nothing on the prune path validates.
func TestReclaimUpdateStagingPayloadRefusesAReservedName(t *testing.T) {
	s, _ := setupStore(t)
	checked := checkedRecoveryStore(t, s)
	const reserved = ".fu-attacker"
	writeSkillBody(t, filepath.Join(s.StagingDir(), reserved), "attacker", "not fu's to remove")
	payload, err := checked.SnapshotStagedPayload(reserved)
	if err != nil {
		t.Fatal(err)
	}

	err = reclaimUpdateStagingPayload(checked, reserved, payload)
	if err == nil {
		t.Fatal("a reserved name must be refused, not reclaimed")
	}
	if !strings.Contains(err.Error(), reserved) {
		t.Fatalf("the refusal must name what it refused: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(s.StagingDir(), reserved)); statErr != nil {
		t.Fatalf("the refusal must leave the content alone: %v", statErr)
	}
}

// TestUpdateSkillsNamedFlattensAReasonInItsError pins round 3's Minor #3: the
// named refusal embeds Outdated's Reason, which quotes recorded paths back, and
// `fu add` on a directory whose name holds a newline records it verbatim. The
// outdated side already flattens the same string; the error path did not, so
// one skill could print further lines of forged output.
func TestUpdateSkillsNamedFlattensAReasonInItsError(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "kit"), "kit", "one")
	if err := cfg.AddSkill("kit", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	// A recorded path carrying a newline: the shape `fu add` on such a
	// directory produces, here written directly so the test needs no fixture
	// with an unusual name on disk.
	cfg.SetSourceFields("kit", map[string]string{
		"type": "local", "path": "/nowhere\nupdatable\n  ghost  pwned",
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	_, err := app.UpdateSkills("kit", false)
	if err == nil {
		t.Fatal("a missing local source must refuse")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("an error built from a recorded path must stay on one line:\n%q", err.Error())
	}
}

// TestUpdateSkillsBatchIsolatesASourceThatFailsToPrepare pins round 3's
// Important #9's third row: `if ps.err != nil { …Failed…; continue }` mutated
// to `return ps.err` survived, and would turn one unclonable source into an
// aborted batch -- the opposite of the per-skill isolation add.go establishes
// and design §3.3 requires.
//
// The failure is produced honestly rather than injected: the second repository
// is removed after its ref has been resolved (through the same
// resolveRemoteRefHook the dedup tests count with), so judgement succeeds and
// the clone that follows cannot.
func TestUpdateSkillsBatchIsolatesASourceThatFailsToPrepare(t *testing.T) {
	app, st, cfg := applicationEnv(t)
	goodSrc, _ := installedFromLocal(t, st, cfg, "kit", "version one")
	writeSkillBody(t, goodSrc, "kit", "version two")

	url, ref, _ := realGitBranchFixture(t, "doomed")
	writeSkillBody(t, filepath.Join(st.SkillsDir(), "doomed"), "doomed", "one")
	// A baseline that matches the store copy, so the row reaches source
	// preparation instead of being skipped as locally modified.
	payload, err := checkedRecoveryStore(t, st).SnapshotSkillPayload("doomed")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := digestOwnedPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddSkill("doomed", baseline); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("doomed", map[string]string{
		"type": "git", "url": url, "ref": ref, "ref_kind": "branch",
		"commit": strings.Repeat("0", 40),
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	bare := strings.TrimPrefix(url, "file://")
	resolveRemoteRefHook = func(gotURL, _ string) {
		if gotURL == url {
			// Resolved already; the clone that follows will not find it.
			_ = os.RemoveAll(bare)
		}
	}
	t.Cleanup(func() { resolveRemoteRefHook = nil })

	outcome, updateErr := app.UpdateSkills("", false)
	if !errors.Is(updateErr, ErrOperationFailed) {
		t.Fatalf("an isolated failure must still surface as ErrOperationFailed, got %v", updateErr)
	}
	if len(outcome.Updated) != 1 || outcome.Updated[0] != "kit" {
		t.Fatalf("the healthy skill must still update: %+v", outcome)
	}
	if len(outcome.Reconcile.Failed) != 1 || outcome.Reconcile.Failed[0].Action.Skill != "doomed" {
		t.Fatalf("the unclonable source must fail only its own skill: %+v", outcome.Reconcile.Failed)
	}
}

// TestUpdateSkillRefusesAStagedTreeThatEscapesItsOwnDirectory covers SPEC rule
// 7's path-safety half inside update's Mutate (round 3, Minor #5): design §7
// lists 不合规拒绝（规则 7）as a required test, and the ValidateLinks mutation
// survived. It is defence in depth -- ScanSource drops escaping candidates
// before updateSkill is reached on the production path -- so the test calls
// updateSkill directly with a candidate no scan would have produced, which is
// exactly the caller this guard exists to be safe against.
func TestUpdateSkillRefusesAStagedTreeThatEscapesItsOwnDirectory(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
	writeSkillBody(t, srcDir, "kit", "version two")
	// A symlink out of the skill directory: rule 7's 越界引用.
	if err := os.Symlink("../../../etc/passwd", filepath.Join(srcDir, "escape")); err != nil {
		t.Fatal(err)
	}

	p := prepareLocal(t, srcDir)
	proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := skill.DigestManifest(proj)
	if err != nil {
		t.Fatal(err)
	}

	_, err = updateSkill(s, nil, p, "kit",
		Candidate{Name: "kit", Subdir: ".", Digest: digest},
		localSourceRecord(srcDir),
		map[string]string{"type": "local", "path": srcDir}, false, hooks{})
	if err == nil {
		t.Fatal("a staged tree with an escaping symlink must be refused before it can be exchanged in")
	}
	// The published skill is untouched, and no staged tree is left behind.
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "version one")
	if _, statErr := os.Lstat(filepath.Join(s.StagingDir(), "kit")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused update must leave no staged tree, stat err=%v", statErr)
	}
}

// TestUpdateSkillRefusesACandidateWhoseDigestDoesNotMatchTheSource covers the
// first of the three in-Mutate revalidation guards round 3's Minor #6 found
// unpinned: `cand.Digest == "" || observedDigest != cand.Digest`, the "did the
// source change since inspection" check add.go also applies. cand.Digest has no
// other consumer, so nothing else could notice its removal.
func TestUpdateSkillRefusesACandidateWhoseDigestDoesNotMatchTheSource(t *testing.T) {
	for _, tc := range []struct{ label, digest string }{
		{"empty", ""},
		{"stale", "sha256:" + strings.Repeat("ab", 32)},
	} {
		t.Run(tc.label, func(t *testing.T) {
			s, cfg := setupStore(t)
			srcDir, _ := installedFromLocal(t, s, cfg, "kit", "version one")
			writeSkillBody(t, srcDir, "kit", "version two")

			_, err := updateSkill(s, nil, prepareLocal(t, srcDir), "kit",
				Candidate{Name: "kit", Subdir: ".", Digest: tc.digest},
				localSourceRecord(srcDir),
				map[string]string{"type": "local", "path": srcDir}, false, hooks{})
			if err == nil {
				t.Fatal("a candidate whose digest does not describe the source must be refused")
			}
			assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "version one")
		})
	}
}
