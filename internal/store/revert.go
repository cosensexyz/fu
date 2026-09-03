// internal/store/revert.go
package store

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
)

// Revert rolls back the last n operations as a forward snapshot: the worktree
// is put back to the tree from n operations ago (resolveOperationsBack below,
// which counts operations rather than raw commits) and that state is then
// committed, so the commits rolled past stay reachable from the branch. It is
// git revert's shape, not git reset's -- nothing leaves the branch, and SPEC's
// "store content is backed by git history" therefore still holds afterwards.
//
// The order is the point. An earlier version built a commit object,
// compare-and-swapped the branch reference, and then called
// Worktree.Reset(HardReset), which writes that reference again
// unconditionally: a concurrent write landing in between was overwritten back
// to fu's commit and lost without a trace. Updating the worktree first and
// letting the ordinary Commit path publish the result leaves exactly one
// writer of the reference, so the compare-and-swap means what it says.
//
// A crash between the worktree update and the commit leaves the target content
// in place with HEAD unmoved; the next write command's sweep records it as an
// external modification. The revert's effect survives, its commit message does
// not. The opposite order would let that same sweep commit the pre-revert
// content back, silently undoing the revert -- which is why this order is not
// negotiable even though neither is transactional.
//
// The returned slice is the paths the worktree update actually converged, in
// sorted order. It is the same report `fu restore --hard` gives for its own
// reset, and it is what lets a caller say which files a revert moved rather
// than only how many operations it claims to have undone.
//
// n is both the count resolved and the count reported. An earlier signature
// took the two separately, so a caller adjusting the resolved count could keep
// fu's own bookkeeping out of what `fu log` prints and out of a refusal the
// user did not cause (review round finding 2). No caller adjusts any more --
// counting by operations rather than raw commits removed the reason -- so the
// second parameter was always equal to the first, with nothing enforcing it: a
// future mismatched call would silently make the refusal and the commit
// message name a different count from the one actually resolved. One parameter
// cannot drift from itself.
func (s *Store) Revert(n int) ([]string, error) {
	if n < 1 {
		return nil, fmt.Errorf("revert count must be >= 1, got %d", n)
	}
	target, err := s.resolveOperationsBack(n)
	if err != nil {
		return nil, fmt.Errorf("no commit %d operation(s) back: %w", n, err)
	}
	targetCommit, err := s.Repo.CommitObject(target)
	if err != nil {
		return nil, fmt.Errorf("load target commit %s: %w", target.String()[:7], err)
	}
	tree, err := targetCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("load target tree for %s: %w", target.String()[:7], err)
	}
	paths, err := targetTreePaths(tree)
	if err != nil {
		return nil, err
	}
	changed, err := s.applyTreeToWorktree(paths)
	if err != nil {
		return changed, fmt.Errorf("reset worktree to %s: %w", target.String()[:7], err)
	}

	// The declared path set DESIGN §6's "命令提交候选冻结" asks for, connected
	// rather than discarded. RevertOperations does not go through run
	// (pipeline.go), so validatePreparedOperation -- with its AllowedChanges
	// declaration and its unstaged-paths guard -- never runs for this command,
	// and nothing else checked that the tree about to be published is the one
	// this operation is defined to produce.
	//
	// It is checked here in the strongest available terms: not that the change
	// set looks plausible, but that the frozen commit candidate's full-tree
	// identity is the target commit's own. Anything that reached the store
	// between the caller's sweep and this staging -- a hand edit, a direct-git
	// write, an untracked file stageAll force-adds -- changes that identity,
	// and every one of those cases used to be folded silently into a commit
	// still claiming to be "back n operations".
	prepared, err := s.PrepareCommit()
	if err != nil {
		return changed, err
	}
	want, err := commitTreeFingerprint(targetCommit)
	if err != nil {
		return changed, errors.Join(err, s.withdrawPreparedIndex(prepared))
	}
	if prepared.TreeFingerprint() != want {
		return changed, errors.Join(
			fmt.Errorf("refusing to record this revert: the store worktree no longer matches %s, "+
				"so the commit would not be the tree %d operation(s) back. The worktree and index have already been "+
				"moved to that tree; nothing was committed, and the next write command will record the result as an "+
				"external modification. Record the extra content with a write command, or remove it, then re-run",
				target.String()[:7], n),
			s.withdrawPreparedIndex(prepared))
	}

	outcome, err := s.CommitPrepared(fmt.Sprintf("revert: back %d operation(s) to %s", n, target.String()[:7]), prepared)
	if err == nil && !outcome.Written {
		// The target tree equals HEAD's, so CommitPrepared's no-change branch
		// published nothing. The worktree is right and nothing is broken, but
		// saying "reverted n operation(s)" over a history that did not move is
		// a false statement, and SPEC §5.1's "revert is itself a revertible
		// operation" does not hold for a call that left no commit. Reachable
		// with `new alpha; disable alpha; enable alpha; fu revert 2`.
		return changed, fmt.Errorf("nothing to revert: the tree %d operation(s) back is the tree already at HEAD, "+
			"so no commit was recorded", n)
	}
	if err != nil && !outcome.Written {
		return changed, errors.Join(err, s.withdrawPreparedIndex(prepared))
	}
	if err != nil {
		// The worktree already holds the target content at this point: the
		// updater ran and converged before any of this. Saying so is what
		// keeps the next command's sweep from looking like an unexplained
		// "external: manual modifications" commit over content fu itself
		// wrote seconds earlier.
		return changed, fmt.Errorf("the store worktree already holds the tree from %d operation(s) back, "+
			"and the next write command will record it as an external modification: %w", n, err)
	}
	return changed, nil
}

// resolveOperationsBack finds the commit whose tree is the store's state
// before the last n operations, walking first-parent history and counting only
// the commits that are operations.
//
// What counts is decided by SPEC §5.3's own list, through IsOperationMessage
// (git.go). The verbs are not restated here: this comment named eight of them
// and went stale the moment the list gained `commit`, which is exactly the
// drift §5.3 warns about (review 2026-09-03, Minor). operationVerbs is the
// one place to read them. It is a whitelist rather than a list of exclusions,
// and that is the point: an earlier version excluded only the sweep's
// ExternalCommitMessage, so every other message fu writes on its own account
// silently became a user operation. The recovery pass's compensation commit
// did exactly that, and
// because RevertOperations recovers before it sweeps, `fu revert 1` after a
// crash wrote that compensation itself and then reverted it -- restoring the
// content fu had just rolled back and leaving the user's real last operation
// in place, at exit 0. `init: store` was miscounted the same way, inflating
// the out-of-range refusal by one.
//
// A rolled-back operation and its compensation are discounted as a pair, not
// just the compensation. The compensation names the operation it cancels
// (RecoveryCompensationPrefix, git.go), so WalkOperations can recognise the
// other half when it reaches it; the two together net to zero, and an
// operation that was interrupted and undone is not one the user ever
// completed. Why that pairing is matched by message rather than by adjacency
// is stated once, at WalkOperations (operations.go) -- the single place that
// decides what counts as an operation. This comment used to restate it and
// had it backwards, claiming the matching is what makes a sweep between the
// pair safe when validateRecoveryCommit already forbids anything landing
// there (review 2026-09-03 round 4, Minor).
//
// HEAD~n could not tell any of this apart. Counting operations here also
// subsumes the caller's old skip adjustment, which compensated only for the
// sweep of the call performing the revert -- so a sweep left by any earlier
// command still consumed one of the n the user asked for.
//
// The target is the n-th operation's own first parent, not the (n+1)-th
// operation. The difference is what happens to a hand edit that was swept
// before the operations being undone: taking the parent keeps that edit in the
// resulting tree, which is what the skip adjustment already did and what a
// user undoing operations expects of content no operation touched.
//
// The counting itself is WalkOperations's job now; this function only reads
// the ordinal it hands back and stops the walk once it reaches n.
func (s *Store) resolveOperationsBack(n int) (plumbing.Hash, error) {
	target := plumbing.ZeroHash
	operations, walked := 0, 0
	err := s.WalkOperations(func(e OperationEntry) (bool, error) {
		walked++
		if e.Ordinal != 0 {
			operations = e.Ordinal
		}
		if e.Ordinal != 0 && e.Ordinal == n {
			// The target is this operation's own first parent, not the
			// (n+1)-th operation; a root commit has none, so the walk ends
			// here and the shortfall is reported below.
			target = e.FirstParent
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if target.IsZero() {
		return plumbing.ZeroHash, fmt.Errorf(
			"the store holds %d operation(s) in %d commit(s) of history", operations, walked)
	}
	return target, nil
}
