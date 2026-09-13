package engine

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

func TestCommitSubjectDerivesScopeFromChangedPaths(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		want    string
	}{
		{"alpha", []string{"skills/alpha/SKILL.md"}, "commit: alpha"},
		{"alpha", []string{"skills/zzz/SKILL.md", "fu.yaml"}, "commit: alpha"},
		{"", []string{"skills/beta/SKILL.md", "skills/alpha/x.md", "skills/alpha/SKILL.md"}, "commit: alpha, beta"},
		{"", []string{"fu.yaml"}, "commit: fu.yaml"},
		{"", []string{"skills/alpha/SKILL.md", "fu.yaml"}, "commit: alpha, fu.yaml"},
		{"", []string{"README.md", "skills/alpha/SKILL.md"}, "commit: alpha, store"},
		{"", []string{"skills/stray.md"}, "commit: store"},
		{"", []string{"skills/a/x", "skills/b/x", "skills/c/x", "skills/d/x", "skills/e/x"}, "commit: a, b, c, d, e"},
		{"", []string{"skills/a/x", "skills/b/x", "skills/c/x", "skills/d/x", "skills/e/x", "skills/f/x", "fu.yaml"}, "commit: 6 skills, fu.yaml"},
		{"", []string{}, "commit: store"},
	}
	for _, c := range cases {
		if got := commitSubject(c.name, c.changed); got != c.want {
			t.Errorf("commitSubject(%q, %v) = %q, want %q", c.name, c.changed, got, c.want)
		}
	}
}

func editSkillFile(t *testing.T, s *store.Store, skill, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), skill, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func logMessages(t *testing.T, s *store.Store, n int) []string {
	t.Helper()
	entries, err := s.Log(n)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out
}

func TestCommitNamedRecordsOnlyThatSkill(t *testing.T) {
	s, _ := setupStore(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := NewSkill(s, nil, name); err != nil {
			t.Fatal(err)
		}
	}
	editSkillFile(t, s, "alpha", "SKILL.md", "alpha edited")
	editSkillFile(t, s, "beta", "SKILL.md", "beta edited")

	outcome, err := CommitOperations(s, nil, CommitScope{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || outcome.Subject != "commit: alpha" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !slices.Equal(outcome.Changed, []string{"skills/alpha/SKILL.md"}) {
		t.Fatalf("changed = %v", outcome.Changed)
	}
	if got := logMessages(t, s, 1); got[0] != "commit: alpha" {
		t.Fatalf("head = %q", got[0])
	}
	remaining, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(remaining, []string{"skills/beta/SKILL.md"}) {
		t.Fatalf("beta's edit must stay pending, remaining = %v", remaining)
	}
}

func TestCommitAllRecordsEverythingUnderADerivedSubject(t *testing.T) {
	s, _ := setupStore(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := NewSkill(s, nil, name); err != nil {
			t.Fatal(err)
		}
	}
	editSkillFile(t, s, "alpha", "SKILL.md", "alpha edited")
	editSkillFile(t, s, "beta", "SKILL.md", "beta edited")
	cfgBytes, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), append(cfgBytes, []byte("# hand note\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := CommitOperations(s, nil, CommitScope{Message: "sync after editing"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Subject != "commit: alpha, beta, fu.yaml" {
		t.Fatalf("subject = %q", outcome.Subject)
	}
	if got := logMessages(t, s, 1); got[0] != "commit: alpha, beta, fu.yaml\n\nsync after editing" {
		t.Fatalf("message = %q", got[0])
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("a store-wide commit must leave nothing pending")
	}
}

func TestCommitAllRecordsAStagedSnapshotAsExternalFirst(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	editSkillFile(t, s, "alpha", "SKILL.md", "staged version")
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/alpha/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	editSkillFile(t, s, "alpha", "SKILL.md", "later version")

	if _, err := CommitOperations(s, nil, CommitScope{}); err != nil {
		t.Fatal(err)
	}
	got := logMessages(t, s, 2)
	if got[0] != "commit: alpha" || got[1] != store.ExternalCommitMessage {
		t.Fatalf("log = %q, want the staged snapshot recorded as external before the commit", got)
	}
	// The messages alone do not pin what the split is for (final review,
	// finding 8). The whole reason a store-wide commit records two layers
	// instead of collapsing the index into the worktree is that a version the
	// user only ever staged stays recoverable from history; so the external
	// commit must hold the *staged* bytes, and the commit on top of it the
	// later worktree bytes. Asserting only the two subjects would still pass
	// if the external layer committed the worktree twice.
	if got := commitFileAt(t, s, 1, "skills/alpha/SKILL.md"); got != "staged version" {
		t.Fatalf("external commit holds %q, want the staged version -- a staged-only version must stay recoverable", got)
	}
	if got := commitFileAt(t, s, 0, "skills/alpha/SKILL.md"); got != "later version" {
		t.Fatalf("the commit on top holds %q, want the later worktree version", got)
	}
}

// commitFileAt reads one file out of the store commit back steps behind HEAD
// along first-parent history; back == 0 is HEAD itself.
func commitFileAt(t *testing.T, s *store.Store, back int, name string) string {
	t.Helper()
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	for range back {
		if c, err = c.Parent(0); err != nil {
			t.Fatal(err)
		}
	}
	file, err := c.File(name)
	if err != nil {
		t.Fatalf("%s at HEAD~%d (%q): %v", name, back, c.Message, err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

// TestCommitAllReportsAStagedOnlySnapshotAsExternalWritten pins review round
// 1 finding 2: `git add -A` followed by `fu commit -m reason` with nothing
// pending beyond what was staged writes a durable "external: manual
// modifications" commit (PrepareStagedSnapshot's candidate is non-empty),
// but the store-wide candidate PrepareCommit takes right afterward now
// equals that same new HEAD -- there is nothing left on top of it -- so
// Changed and Subject stay empty and Written stays false. Unlike
// TestCommitAllRecordsAStagedSnapshotAsExternalFirst, which adds a *later*
// worktree edit so both layers fire, this is the case where only the
// external layer has anything to record; without ExternalWritten the
// outcome would look identical to "nothing to commit" even though a commit
// was in fact written and the store's history moved.
func TestCommitAllReportsAStagedOnlySnapshotAsExternalWritten(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	editSkillFile(t, s, "alpha", "SKILL.md", "staged version")
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/alpha/SKILL.md"); err != nil {
		t.Fatal(err)
	}

	outcome, err := CommitOperations(s, nil, CommitScope{Message: "reason"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.ExternalWritten {
		t.Fatalf("outcome = %+v, want ExternalWritten true: a staged snapshot was recorded", outcome)
	}
	if outcome.Written || outcome.Subject != "" || len(outcome.Changed) != 0 {
		t.Fatalf("outcome = %+v, want nothing left for the store-wide candidate after the external commit", outcome)
	}
	got := logMessages(t, s, 1)
	if got[0] != store.ExternalCommitMessage {
		t.Fatalf("head = %q, want the staged snapshot recorded as external", got[0])
	}
}

func TestCommitWithNothingPendingWritesNothing(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	before := logMessages(t, s, 10)
	outcome, err := CommitOperations(s, nil, CommitScope{Name: "alpha", Message: "nothing"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Written || len(outcome.Changed) != 0 {
		t.Fatalf("outcome = %+v, want nothing written", outcome)
	}
	if after := logMessages(t, s, 10); len(after) != len(before) {
		t.Fatalf("history moved from %d to %d commits", len(before), len(after))
	}
}

// TestCommitWithNothingPendingStillReconciles pins review round 1 finding 1:
// the empty-candidate path used to return before reconcileChecked ran, which
// made `fu commit` the one write command that did not settle link drift on
// a no-op. Here a hand-deleted agent-side symlink is the drift, and there is
// nothing pending in the store's own worktree -- exactly the case the
// finding named, contrasted with the pending-edit case
// TestCommitIsOneRevertibleOperation already covers elsewhere in this file.
func TestCommitWithNothingPendingStillReconciles(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alpha")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(s.SkillsDir(), "alpha") {
		t.Fatalf("fixture must start with the link materialized: %v %q", err, target)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	outcome, err := CommitOperations(s, agents, CommitScope{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Written || len(outcome.Changed) != 0 {
		t.Fatalf("outcome = %+v, want nothing written: nothing was pending", outcome)
	}
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(s.SkillsDir(), "alpha") {
		t.Fatalf("commit with nothing pending must still repair the link layer: %v %q", err, target)
	}
}

func TestCommitRefusesAnUnknownSkill(t *testing.T) {
	s, _ := setupStore(t)
	_, err := CommitOperations(s, nil, CommitScope{Name: "ghost"})
	if err == nil || !strings.Contains(err.Error(), `unknown skill "ghost"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestCommitIsOneRevertibleOperation(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	editSkillFile(t, s, "alpha", "SKILL.md", "edited")
	if _, err := CommitOperations(s, nil, CommitScope{Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RevertOperations(s, nil, 1); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("revert 1 must undo the commit: got %q, want %q", got, original)
	}
}

func TestApplicationCommitUsesTheStoreUnderFuHome(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	app := NewApplication()
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.NewSkill("alpha"); err != nil {
		t.Fatal(err)
	}
	st, err := app.openStore()
	if err != nil {
		t.Fatal(err)
	}
	editSkillFile(t, st, "alpha", "SKILL.md", "edited")
	outcome, err := app.Commit("alpha", "why")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || outcome.Subject != "commit: alpha" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// interruptedTransaction leaves a committed but unrecovered transaction in s,
// the state the next write command's recovery pass has to settle before doing
// anything of its own.
func interruptedTransaction(t *testing.T, s *store.Store, name string) {
	t.Helper()
	interrupted := errors.New("interrupted after commit")
	outcome := OperationOutcome{}
	if _, err := newSkillTracked(s, nil, name, hooks{
		afterCommit: func() error { return interrupted },
	}, &outcome); !errors.Is(err, interrupted) {
		t.Fatalf("new error = %v, want %v", err, interrupted)
	}
	if !outcome.Committed || !outcome.RecoveryPending {
		t.Fatalf("setup check: need a committed, unrecovered transaction: %+v", outcome)
	}
}

// TestCommitRecoversPendingTransactionsBeforeRecordingItsOwn pins the ordering
// half of the write-command contract: an interrupted transaction is settled
// before this command records anything of its own.
//
// The assertion has to be made on the *conflict* path, and an earlier version
// of this test claimed the opposite -- that a hand edit made first turns
// recovery into a reported conflict and so ordering could only be stated on a
// clean worktree. That premise is right about the mechanism and wrong about
// the conclusion: the conflict is exactly where the two orderings diverge, and
// on a clean worktree neither ordering can be distinguished, because the
// command writes nothing of its own either way. Mutation testing showed the
// old test stayed green with recovery moved after the commit (review
// 2026-09-02 round 2, Important).
//
// With recovery first, it refuses before anything is published. With recovery
// after the commit, the command publishes `commit: alpha` on top of an
// unrecovered transaction and only then fails -- so Written and an unmoved
// head tell the orderings apart.
func TestCommitRecoversPendingTransactionsBeforeRecordingItsOwn(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	interruptedTransaction(t, s, "gamma")

	editSkillFile(t, s, "alpha", "SKILL.md", "edited")
	before := logMessages(t, s, 1)[0]
	refused, err := CommitOperations(s, nil, CommitScope{Name: "alpha"})
	if err == nil {
		t.Fatal("a hand edit alongside an unrecovered transaction must be refused by the recovery pass")
	}
	if refused.Written || refused.Subject != "" {
		t.Fatalf("nothing may be published before recovery has settled: %+v", refused)
	}
	if after := logMessages(t, s, 1)[0]; after != before {
		t.Fatalf("history moved before recovery settled: %q -> %q", before, after)
	}
}

// TestCommitSettlesAnInterruptedTransactionThenRecordsNormally is the other
// side of that contract: once the recovery pass can reach a terminal state, it
// does, and the command then behaves exactly as it would have without the
// interruption.
func TestCommitSettlesAnInterruptedTransactionThenRecordsNormally(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	interruptedTransaction(t, s, "gamma")

	// Nothing pending of the user's own, so the compensation is the only
	// commit this call can write -- and it must write it.
	recovered, err := CommitOperations(s, nil, CommitScope{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Written {
		t.Fatalf("nothing was pending, so this command had nothing of its own to record: %+v", recovered)
	}
	if head := logMessages(t, s, 1)[0]; !strings.HasPrefix(head, store.RecoveryCompensationPrefix) {
		t.Fatalf("the recovery compensation must be the newest commit, got %q", head)
	}
	if _, err := os.Stat(filepath.Join(s.SkillsDir(), "gamma")); !os.IsNotExist(err) {
		t.Fatalf("the interrupted skill must be rolled back, err=%v", err)
	}

	editSkillFile(t, s, "alpha", "SKILL.md", "edited")
	committed, err := CommitOperations(s, nil, CommitScope{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !committed.Written || committed.Subject != "commit: alpha" {
		t.Fatalf("outcome = %+v, want the edit recorded", committed)
	}
}

// stageInStore and unstageInStore drive the store's *public* git index the way
// a user running git in the store does, which is the only way to build the
// index states a scoped commit has to converge.
func stageInStore(t *testing.T, s *store.Store, path string) {
	t.Helper()
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(path); err != nil {
		t.Fatal(err)
	}
}

func unstageInStore(t *testing.T, s *store.Store, path string) {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
}

// TestCommitConvergesWhenTheTreeDoesNotMove is the engine-level counterpart of
// the store test of nearly the same name, and it is the one that was actually
// missing.
//
// Round 2's finding was that the index sync never ran when the candidate left
// the tree unmoved. Fixing the store's publish path made the store test pass
// while the real binary stayed dirty, because this caller short-circuits past
// CommitPrepared entirely when the candidate has no changes -- a gap no
// store-layer test can see. Both shapes are driven through the production
// entry point here.
func TestCommitConvergesWhenTheTreeDoesNotMove(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty func(t *testing.T, s *store.Store)
	}{
		{
			// Stage a version, then put the worktree back to the committed
			// bytes: the candidate equals HEAD, the index does not.
			name: "staged then reverted",
			dirty: func(t *testing.T, s *store.Store) {
				path := filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")
				committed, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				editSkillFile(t, s, "alpha", "SKILL.md", "staged version")
				stageInStore(t, s, "skills/alpha/SKILL.md")
				if err := os.WriteFile(path, committed, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Drop an in-prefix path from the index with the file still on
			// disk: staging the prefix puts it straight back.
			name: "unstaged in prefix",
			dirty: func(t *testing.T, s *store.Store) {
				unstageInStore(t, s, "skills/alpha/SKILL.md")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupStore(t)
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatal(err)
			}
			tc.dirty(t, s)

			outcome, err := CommitOperations(s, nil, CommitScope{Name: "alpha"})
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Written {
				t.Fatalf("setup check: this shape must leave the tree unmoved, got %+v", outcome)
			}
			pending, err := s.ChangedPathsIncludingIgnored()
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("a scoped commit must converge even when it writes nothing, got %v", pending)
			}
		})
	}
}
