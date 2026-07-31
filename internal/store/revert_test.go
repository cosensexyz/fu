// internal/store/revert_test.go
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/storage"
)

// A-B-C → revert 1 → D(parent=C, tree=B) → revert 1 → E(parent=D, tree=C)
func TestRevertSnapshotForward(t *testing.T) {
	s, err := Init(t.TempDir()) // commit A (init)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(s.Dir(), "state.txt")
	os.WriteFile(f, []byte("B"), 0o644)
	s.Commit("op: B")
	os.WriteFile(f, []byte("C"), 0o644)
	s.Commit("op: C")

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

	if err := s.Revert(1); err != nil { // → D
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

	if err := s.Revert(1); err != nil { // → E, undoing the revert itself
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

// A non-positive count is rejected before touching the repository at all.
//
// Asserting merely err != nil would not isolate the n < 1 guard: for
// negative n, the revision string "HEAD~-1" the code would go on to build
// is itself invalid syntax and ResolveRevision rejects it independently
// (confirmed via go-git's revision parser -- "-1" after "~" scans as a
// second Ref, and "reference must be defined once at the beginning" fires
// there), so a test that only checks for *an* error would stay green even
// if the guard were deleted. Checking the exact message ties the failure
// to the guard itself, not to whatever downstream parsing happens to reject.
func TestRevertRejectsNonPositiveCount(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := mustHead(t, s)

	for _, n := range []int{0, -1, -5} {
		err := s.Revert(n)
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

	if err := s.Revert(1); err == nil {
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
	keep := filepath.Join(s.Dir(), "keep.txt")
	toDelete := filepath.Join(s.Dir(), "to-delete.txt")
	added := filepath.Join(s.Dir(), "added.txt")

	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toDelete, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("op: B"); err != nil { // B has keep.txt + to-delete.txt
		t.Fatal(err)
	}

	if err := os.Remove(toDelete); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(added, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("op: C"); err != nil { // C drops to-delete.txt, adds added.txt
		t.Fatal(err)
	}

	if err := s.Revert(1); err != nil { // back to B's tree
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
	f := filepath.Join(s.Dir(), "state.txt")
	os.WriteFile(f, []byte("B"), 0o644)
	s.Commit("op: B")
	os.WriteFile(f, []byte("C"), 0o644)
	s.Commit("op: C")

	headBefore, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	cHash := headBefore.Hash()

	if err := s.Revert(1); err != nil {
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

// Worktree.Reset re-writes the branch ref unconditionally (no old-value
// check) while it refreshes the index and worktree -- see Revert's doc
// comment. A write that lands in that specific window cannot be prevented
// by the CAS that came before it, so Revert must at least detect it
// afterward instead of reporting success.
//
// indexSpyStorer.Index() is the first call Reset makes after its own ref
// write and is never revisited once Reset moves on, so firing a foreign
// SetReference from inside it deterministically reproduces a write
// landing in that exact window, without any test-only seam in revert.go
// itself: Repository.Storer is already a plain exported interface field.
func TestRevertDetectsConcurrentWriteDuringReset(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(s.Dir(), "state.txt")
	os.WriteFile(f, []byte("B"), 0o644)
	s.Commit("op: B")
	os.WriteFile(f, []byte("C"), 0o644)
	s.Commit("op: C")

	headBefore, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	branch := headBefore.Name()
	// Stand-in for "whatever a concurrent writer left the branch pointing
	// at": any resolvable commit other than the one Revert is about to
	// create works, so headBefore's own hash (the pre-revert C) does fine.
	foreign := headBefore.Hash()

	real := s.Repo.Storer
	s.Repo.Storer = &indexSpyStorer{
		Storer: real,
		onIndex: func() {
			interloper := plumbing.NewHashReference(branch, foreign)
			if err := real.SetReference(interloper); err != nil {
				t.Fatalf("simulated concurrent write failed: %v", err)
			}
		},
	}

	err = s.Revert(1)
	if err == nil {
		t.Fatal("Revert must report an error when the branch ref is clobbered mid-reset, got nil")
	}
	if !strings.Contains(err.Error(), "concurrent write") {
		t.Fatalf("error must describe a detected concurrent write, got %q", err.Error())
	}

	current, err := s.Repo.Reference(branch, true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Hash() != foreign {
		t.Fatalf("branch must be left exactly as the interloper set it (%s), not silently corrected, got %s", foreign, current.Hash())
	}
}

// indexSpyStorer wraps a real storage.Storer and runs onIndex the first
// time Index is called, then delegates. All other methods are promoted
// straight through to the embedded Storer.
type indexSpyStorer struct {
	storage.Storer
	onIndex func()
	fired   bool
}

func (s *indexSpyStorer) Index() (*index.Index, error) {
	if !s.fired {
		s.fired = true
		s.onIndex()
	}
	return s.Storer.Index()
}
