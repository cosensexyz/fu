// internal/store/revert_test.go
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage"
)

func TestGoGitV519HardResetDeletesUntrackedAndIgnoredFiles(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: ignore rule"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"untracked.txt", "ignored.txt"} {
		if err := os.WriteFile(filepath.Join(s.Dir(), name), []byte("residue"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: head.Hash()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"untracked.txt", "ignored.txt"} {
		if _, err := os.Stat(filepath.Join(s.Dir(), name)); !os.IsNotExist(err) {
			t.Fatalf("go-git v5.19.2 hard reset must delete %s; stat error = %v", name, err)
		}
	}
}

// TestRevertWritesTheBranchReferenceExactlyOnce pins precondition 2. The old
// implementation built a commit, compare-and-swapped the reference, and then
// let Worktree.Reset write it again unconditionally -- which silently undid
// any concurrent write that had landed in between. Ordering the worktree
// update before the commit removes the second writer entirely.
//
// applyFixture's fixture holds no untracked file for Worktree.Reset to
// mishandle and no concurrent writer for a lost second write to erase, so the
// end-state assertions below (worktree content, parent count, clean store)
// pass identically whether Revert writes the branch reference once or twice --
// they were confirmed, by actually running this test against the pre-Task-7
// two-writer Revert, to be green on both implementations. branchWriteCounter
// below is what tells the two apart: it counts writes to the branch reference
// itself, which the old implementation performs twice (its own
// CheckAndSetReference, then Worktree.Reset's unconditional SetReference) and
// the new one performs once (only Commit's CheckAndSetReference).
func TestRevertWritesTheBranchReferenceExactlyOnce(t *testing.T) {
	s := applyFixture(t)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: second"); err != nil {
		t.Fatal(err)
	}

	beforeRef, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	counter := &branchWriteCounter{Storer: s.Repo.Storer, branch: beforeRef.Name()}
	s.Repo.Storer = counter

	if _, err := s.Revert(1); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "plain.txt"))
	if err != nil || string(got) != "v1" {
		t.Fatalf("revert must put the worktree back: %q %v", got, err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if len(commit.ParentHashes) != 1 {
		t.Fatalf("revert must be a forward snapshot with one parent, got %d", len(commit.ParentHashes))
	}
	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("revert must leave the store clean, still dirty: %v", dirty)
	}
	if counter.count != 1 {
		t.Fatalf("revert must write the branch reference exactly once, wrote it %d times", counter.count)
	}
}

// branchWriteCounter wraps a real storage.Storer and counts writes -- via
// either SetReference (unconditional) or CheckAndSetReference (a
// compare-and-swap) -- that target one specific reference name. See
// TestRevertWritesTheBranchReferenceExactlyOnce for why counting is the part
// that actually distinguishes the old Revert from the new one.
type branchWriteCounter struct {
	storage.Storer
	branch plumbing.ReferenceName
	count  int
}

func (c *branchWriteCounter) SetReference(ref *plumbing.Reference) error {
	if ref.Name() == c.branch {
		c.count++
	}
	return c.Storer.SetReference(ref)
}

func (c *branchWriteCounter) CheckAndSetReference(ref, old *plumbing.Reference) error {
	if ref.Name() == c.branch {
		c.count++
	}
	return c.Storer.CheckAndSetReference(ref, old)
}

// TestRevertKeepsRevertedCommitsReachable pins that fu's revert is a revert
// and not a reset: the commits it rolls past stay on the branch, which is what
// SPEC's "store content is backed by git history" rests on.
func TestRevertKeepsRevertedCommitsReachable(t *testing.T) {
	s := applyFixture(t)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: second"); err != nil {
		t.Fatal(err)
	}
	rolled, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	rolledHash := rolled.Hash()

	if _, err := s.Revert(1); err != nil {
		t.Fatal(err)
	}

	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	iter, err := s.Repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()
	found := false
	if err := iter.ForEach(func(c *object.Commit) error {
		if c.Hash == rolledHash {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the rolled-past commit must remain reachable from the branch")
	}
}

// A-B-C → revert 1 → D(parent=C, tree=B) → revert 1 → E(parent=D, tree=C)
func TestRevertSnapshotForward(t *testing.T) {
	s, err := Init(t.TempDir()) // commit A (init)
	if err != nil {
		t.Fatal(err)
	}
	s = pinnedForWrite(t, s)
	f := filepath.Join(s.Dir(), "state.txt")
	os.WriteFile(f, []byte("B"), 0o644)
	s.Commit("new: b")
	os.WriteFile(f, []byte("C"), 0o644)
	s.Commit("new: c")

	treeOf := func() string {
		head, _ := s.Repo.Head()
		c, _ := s.Repo.CommitObject(head.Hash())
		return c.TreeHash.String()
	}
	headHash := func() string {
		head, _ := s.Repo.Head()
		return head.Hash().String()
	}

	cHash, cTree := headHash(), treeOf()

	if _, err := s.Revert(1); err != nil { // → D
		t.Fatal(err)
	}
	dHash := headHash()
	dCommit, _ := s.Repo.CommitObject(mustHead(t, s))
	if dCommit.ParentHashes[0].String() != cHash {
		t.Fatal("D's parent must be C (history never rewritten)")
	}
	if got, _ := os.ReadFile(f); string(got) != "B" {
		t.Fatalf("worktree not restored to B, got %q", got)
	}

	if _, err := s.Revert(1); err != nil { // → E, undoing the revert itself
		t.Fatal(err)
	}
	eCommit, _ := s.Repo.CommitObject(mustHead(t, s))
	if eCommit.ParentHashes[0].String() != dHash {
		t.Fatal("E's parent must be D")
	}
	if eCommit.TreeHash.String() != cTree {
		t.Fatal("E's tree must equal C's (revert of revert)")
	}
	if got, _ := os.ReadFile(f); string(got) != "C" {
		t.Fatalf("worktree not restored to C, got %q", got)
	}
}

// helper in the same test file
func mustHead(t *testing.T, s *Store) plumbing.Hash {
	t.Helper()
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash()
}

// pinnedForWrite opens a write session on s and returns its pinned Store,
// registering the session's Close with t.Cleanup. Revert now reaches
// applyTreeToWorktree (worktree_apply.go), which refuses to touch a worktree
// outside a checked write session (errUnpinnedWorktree); a plain
// Init/Open-returned Store has no pinned worktree, so every test below that
// calls Revert needs this first.
func pinnedForWrite(t *testing.T, s *Store) *Store {
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

// A non-positive count is rejected before touching the repository at all.
//
// Asserting merely err != nil would not isolate the n < 1 guard, so the exact
// message is checked instead. The reason has changed since this was written
// and the assertion has not, which is why it is restated rather than left as
// it was: the original argument was that a negative n produced the revision
// string "HEAD~-1", which go-git's own parser rejects independently, so any
// test looking only for *an* error stayed green with the guard deleted.
// resolveOperationsBack replaced that string-building entirely -- it walks
// first-parent history counting operations -- but the hazard it created is the
// same shape. A walk asked for a non-positive count runs off the end of
// history and fails there, on its own terms, so the message is still what ties
// this failure to the guard rather than to whatever downstream step happens to
// reject first.
func TestRevertRejectsNonPositiveCount(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := mustHead(t, s)

	for _, n := range []int{0, -1, -5} {
		_, err := s.Revert(n)
		if err == nil {
			t.Fatalf("Revert(%d) must fail, got nil error", n)
		}
		want := fmt.Sprintf("revert count must be >= 1, got %d", n)
		if err.Error() != want {
			t.Fatalf("Revert(%d) error = %q, want the guard's own message %q (a different message means some other check rejected it, not the n < 1 guard)", n, err.Error(), want)
		}
	}

	if mustHead(t, s) != before {
		t.Fatal("HEAD must not move when Revert rejects the count")
	}
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("rejected Revert must not create commits, got %d entries", len(entries))
	}
}

// Reverting further than history goes (only the init commit exists, which
// has no parent) must fail cleanly, leaving HEAD and history untouched.
func TestRevertPastBeginningOfHistory(t *testing.T) {
	s, err := Init(t.TempDir()) // only "init: store" exists, no parent
	if err != nil {
		t.Fatal(err)
	}
	before := mustHead(t, s)

	if _, err := s.Revert(1); err == nil {
		t.Fatal("Revert(1) past the root commit must fail, got nil error")
	}

	if mustHead(t, s) != before {
		t.Fatal("HEAD must not move when the target revision does not exist")
	}
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("failed Revert must not create commits, got %d entries", len(entries))
	}
}

// The worktree after Revert must match the restored snapshot exactly:
// files the reverted-away commit added must disappear, files it deleted
// must come back, and untouched files must be unaffected.
func TestRevertWorktreeMatchesSnapshot(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s = pinnedForWrite(t, s)
	keep := filepath.Join(s.Dir(), "keep.txt")
	toDelete := filepath.Join(s.Dir(), "to-delete.txt")
	added := filepath.Join(s.Dir(), "added.txt")

	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toDelete, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: b"); err != nil { // B has keep.txt + to-delete.txt
		t.Fatal(err)
	}

	if err := os.Remove(toDelete); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(added, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: c"); err != nil { // C drops to-delete.txt, adds added.txt
		t.Fatal(err)
	}

	if _, err := s.Revert(1); err != nil { // back to B's tree
		t.Fatal(err)
	}

	if _, err := os.Stat(added); !os.IsNotExist(err) {
		t.Fatalf("added.txt (added by the reverted-away commit) must disappear, stat err = %v", err)
	}
	if got, err := os.ReadFile(toDelete); err != nil || string(got) != "bye" {
		t.Fatalf("to-delete.txt (deleted by the reverted-away commit) must come back, got %q err %v", got, err)
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "keep" {
		t.Fatalf("keep.txt must be unaffected, got %q err %v", got, err)
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("worktree must exactly match the new HEAD after Revert, IsDirty must be false")
	}
}

// The branch reference must end up pointing at the new commit (not
// detached), under its original name, while the reverted-away history
// stays reachable and resolvable -- nothing is rewritten or discarded.
func TestRevertPreservesOldHistoryAndBranchRef(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s = pinnedForWrite(t, s)
	f := filepath.Join(s.Dir(), "state.txt")
	os.WriteFile(f, []byte("B"), 0o644)
	s.Commit("new: b")
	os.WriteFile(f, []byte("C"), 0o644)
	s.Commit("new: c")

	headBefore, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	cHash := headBefore.Hash()

	if _, err := s.Revert(1); err != nil {
		t.Fatal(err)
	}

	headAfter, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if headAfter.Name() != headBefore.Name() {
		t.Fatalf("branch reference name must be unchanged, got %s want %s", headAfter.Name(), headBefore.Name())
	}
	if headAfter.Hash() == cHash {
		t.Fatal("HEAD must move to a new commit, not stay at C")
	}
	if _, err := s.Repo.CommitObject(cHash); err != nil {
		t.Fatalf("C must remain reachable and resolvable after revert: %v", err)
	}
	// A-B-C-D(revert): 4 commits must be walkable from the new HEAD.
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected all 4 commits (A,B,C,D) reachable from HEAD, got %d", len(entries))
	}
}

// TestRevertDetectsConcurrentWriteDuringReset used to pin a manual
// post-Worktree.Reset verification: Reset rewrote the branch ref
// unconditionally after Revert's own compare-and-swap, so a write landing in
// that specific window could only be caught after the fact, not prevented.
// Reordering the worktree update before the commit (this task) deletes that
// window outright -- Worktree.Reset is no longer called at all, so there is
// no second ref write for a concurrent one to race against or hide behind.
//
// A rewritten version of this test that fires a foreign SetReference during
// applyTreeToWorktree's first Storer.Index() call (the replacement for the
// old hook point) does not reproduce a race: that call now runs before
// Commit's own capture-and-CAS even starts, so Commit simply reads the
// (unconditionally rewritten, but unchanged-in-value) branch as its "before"
// and publishes normally -- confirmed by actually running that rewrite, which
// got "Revert must report an error ... got nil". The concurrent-write
// detection this test exercised now lives entirely in Commit's own
// compare-and-swap, which already has dedicated, more general coverage in
// git_test.go: TestCommitDetectsConcurrentBranchUpdate (a move landing before
// Commit captures its "before" reference) and
// TestCommitDetectsBranchMoveAfterItsOwnRefWrite (a move landing immediately
// after Commit's own compare-and-swap succeeds). Revert has no ref-writing
// logic of its own left to test past what those two already cover.

// TestRevertRefusesWhenTheCommittedTreeWouldNotBeTheTargetTree pins the
// primitive's defining invariant: after Revert, the branch tip's tree must be
// the target's tree, exactly.
//
// Nothing enforced it. RevertOperations does not go through run (pipeline.go),
// so the AllowedChanges declaration and the UnstagedPathsIncludingIgnored
// guard every other write command gets from validatePreparedOperation never
// ran here, and applyTreeToWorktree's returned change set -- which the design
// names as revert's declared path set -- was discarded at the call site. Any
// content landing in the store between the sweep and Commit's staging was then
// folded into the revert commit by stageAll, which force-adds untracked files:
// the result was a commit claiming to be "back n operations" whose tree was
// the target plus whatever else happened to be lying around.
//
// An untracked file reproduces it with no hook at all, because it is exactly
// the thing the updater is defined not to touch (it is in neither the index
// nor the target, so its name is not in union(index, target)) and exactly the
// thing stageAll is defined to add.
func TestRevertRefusesWhenTheCommittedTreeWouldNotBeTheTargetTree(t *testing.T) {
	s := applyFixture(t)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: second"); err != nil {
		t.Fatal(err)
	}
	// Neither tracked nor in the target: the updater leaves it alone by
	// construction, and stageAll would sweep it into the revert commit.
	stray := filepath.Join(s.SkillsDir(), "stray.txt")
	if err := os.WriteFile(stray, []byte("landed between the sweep and the commit"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Revert(1); err == nil {
		t.Fatal("revert must refuse to publish a tree that is not the target's tree")
	}

	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if commit.Message != "new: second" {
		t.Fatalf("no commit may be published on the refusal path, HEAD is now %q", commit.Message)
	}
}

// TestRevertReportsThePathsItChanged pins the report half. `fu restore --hard`
// lists every path it reset; revert had the same list in hand at the
// applyTreeToWorktree call and threw it away, so the command could only say
// "reverted n operation(s)" -- which is also why a link layer reconciled
// against a stale config produced no visible sign of anything wrong.
func TestRevertReportsThePathsItChanged(t *testing.T) {
	s := applyFixture(t)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: second"); err != nil {
		t.Fatal(err)
	}

	changed, err := s.Revert(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "skills/plain.txt" {
		t.Fatalf("revert must report the paths it converged, got %v", changed)
	}
}
