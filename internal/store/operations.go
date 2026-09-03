// internal/store/operations.go
package store

import (
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// OperationEntry is one commit as the operation walk sees it. Ordinal is the
// commit's position among the operations SPEC §5.3 enumerates, counted from
// HEAD (1 is the newest), and is therefore exactly the n that `fu revert n`
// would resolve to this commit. A commit fu wrote on its own account -- a
// sweep, a recovery compensation and the interrupted operation it cancels,
// `init: store` -- carries Ordinal 0.
type OperationEntry struct {
	Hash        plumbing.Hash
	FirstParent plumbing.Hash // ZeroHash at the root commit
	Message     string
	When        time.Time
	Ordinal     int
}

// WalkOperations walks first-parent history from HEAD, newest first, and
// hands every commit to visit with its ordinal. The walk stops when visit
// returns false or history runs out; an error from visit is returned as is.
//
// This is the single place that decides what counts as an operation, for both
// consumers: resolveOperationsBack (revert.go) resolves `fu revert n`'s target
// from it, and Log (git.go) numbers `fu log`'s rows with it, so the number a
// user reads off `fu log` is the number `fu revert` acts on. The rules are the
// ones resolveOperationsBack used to apply on its own: the whitelist in
// IsOperationMessage; and a recovery compensation cancelling the interrupted
// operation it names (RecoveryCompensationPrefix), matched by message rather
// than adjacency. Matching by message is not what makes a sweep between the
// pair safe -- validateRecoveryCommit already requires a compensation's first
// parent to be the operation it cancels, so nothing can land between them. It
// is what keeps the pairing readable here without re-deriving that invariant,
// and its one failure mode is a compensation whose named operation is not its
// parent, which would shift every ordinal below it. Not reachable through fu
// today (review 2026-09-02, Minor).
func (s *Store) WalkOperations(visit func(OperationEntry) (bool, error)) error {
	head, err := s.Repo.Head()
	if err != nil {
		return err
	}
	commit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		return err
	}
	// Messages of operations that a compensation already seen in this walk
	// cancels. A multiset: the same operation message can be interrupted and
	// compensated more than once over a store's life.
	cancelled := map[string]int{}
	ordinal := 0
	for {
		entry := OperationEntry{Hash: commit.Hash, Message: commit.Message, When: commit.Author.When}
		switch {
		case strings.HasPrefix(commit.Message, RecoveryCompensationPrefix):
			cancelled[strings.TrimPrefix(commit.Message, RecoveryCompensationPrefix)]++
		case cancelled[commit.Message] > 0:
			cancelled[commit.Message]--
		case IsOperationMessage(commit.Message):
			ordinal++
			entry.Ordinal = ordinal
		}
		if len(commit.ParentHashes) != 0 {
			entry.FirstParent = commit.ParentHashes[0]
		}
		more, err := visit(entry)
		if err != nil {
			return err
		}
		if !more || entry.FirstParent.IsZero() {
			return nil
		}
		if commit, err = s.Repo.CommitObject(entry.FirstParent); err != nil {
			return err
		}
	}
}
