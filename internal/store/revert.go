// internal/store/revert.go
package store

import (
	"fmt"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Revert rolls back the last n operations as a forward snapshot commit:
// new commit's tree is HEAD~n's tree, sole parent is current HEAD.
// Deliberately avoids Checkout (detached HEAD) and branch rewrites: the
// commit object is built directly (DESIGN §4).
//
// The branch ref update is a genuine compare-and-swap: a revert computed
// against a stale HEAD is rejected. Worktree.Reset then re-writes that
// ref unconditionally to refresh the index and worktree, which leaves two
// gaps this function cannot close:
//
//   - A concurrent write landing between the CAS and Reset's own ref write
//     is overwritten back to our commit by Reset itself. The re-check below
//     then sees the expected hash, so that write is lost without a trace.
//   - A concurrent write landing after Reset's ref write is caught by the
//     re-check and reported as an error -- caught, not prevented.
//
// Serializing fu processes so neither can happen is the store lock's job,
// not this function's.
func (s *Store) Revert(n int) error {
	if n < 1 {
		return fmt.Errorf("revert count must be >= 1, got %d", n)
	}
	headRef, err := s.Repo.Head()
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	target, err := s.Repo.ResolveRevision(plumbing.Revision(fmt.Sprintf("HEAD~%d", n)))
	if err != nil {
		return fmt.Errorf("no commit %d operation(s) back: %w", n, err)
	}
	targetCommit, err := s.Repo.CommitObject(*target)
	if err != nil {
		return fmt.Errorf("load target commit %s: %w", target.String()[:7], err)
	}
	newCommit := &object.Commit{
		Author:       *fuSignature(),
		Committer:    *fuSignature(),
		Message:      fmt.Sprintf("revert: back %d operation(s) to %s", n, target.String()[:7]),
		TreeHash:     targetCommit.TreeHash,
		ParentHashes: []plumbing.Hash{headRef.Hash()},
	}
	obj := s.Repo.Storer.NewEncodedObject()
	if err := newCommit.Encode(obj); err != nil {
		return fmt.Errorf("encode revert commit: %w", err)
	}
	newHash, err := s.Repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return fmt.Errorf("store revert commit: %w", err)
	}
	newRef := plumbing.NewHashReference(headRef.Name(), newHash)
	if err := s.Repo.Storer.CheckAndSetReference(newRef, headRef); err != nil {
		return fmt.Errorf("update branch reference %s: %w", headRef.Name(), err)
	}
	// Branch already points at the new commit; hard reset to that same
	// commit only refreshes index and worktree to match it.
	wt, err := s.Repo.Worktree()
	if err != nil {
		return fmt.Errorf("open worktree: %w", err)
	}
	if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: newHash}); err != nil {
		return fmt.Errorf("reset worktree to revert commit: %w", err)
	}
	// Reset just re-wrote the branch ref unconditionally (see doc comment);
	// confirm it still landed on our commit before reporting success.
	current, err := s.Repo.Reference(headRef.Name(), true)
	if err != nil {
		return fmt.Errorf("verify branch reference after reset: %w", err)
	}
	if current.Hash() != newHash {
		return fmt.Errorf("concurrent write to %s detected during revert: branch now points at %s, expected revert commit %s; the revert may not have taken effect", headRef.Name(), current.Hash(), newHash)
	}
	return nil
}
