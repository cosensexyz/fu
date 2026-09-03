// internal/store/commit_scope.go
package store

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
)

// SkillsPrefix is the store-relative git path prefix every skill's content
// lives under. Concatenate it with a skill name to get that skill's prefix:
// the constant carries a trailing slash and so is not itself a valid argument
// to PrepareCommitUnder, which rejects one. It lives here rather than in the
// engine because the tree layout is the store's own knowledge -- a caller
// building "skills/" + name by hand restates a fact only this package owns.
const SkillsPrefix = "skills/"

// PrepareCommitUnder is PrepareCommit restricted to the given store-relative
// path prefixes: the candidate takes the worktree's state for every path
// under a prefix -- modified, added, deleted, and .gitignored alike, the same
// indiscriminate projection a sweep records -- and the public index's state
// for everything else. It is what lets `fu commit <name>` record one skill
// while leaving the rest of the worktree pending.
//
// A prefix is a clean slash-separated relative path with no trailing slash,
// no leading slash and no ".." component, and admits exactly itself and its
// descendants. Nothing requires it to name a directory: a file path is a
// legitimate prefix that admits exactly that one file.
func (s *Store) PrepareCommitUnder(prefixes []string) (PreparedCommit, error) {
	if len(prefixes) == 0 {
		return PreparedCommit{}, errors.New("prepare commit under: no path prefix given")
	}
	for _, prefix := range prefixes {
		if err := validateCommitPrefix(prefix); err != nil {
			return PreparedCommit{}, err
		}
	}
	baseline, err := s.capturePublicIndex()
	if err != nil {
		return PreparedCommit{}, err
	}
	private, wt, err := s.privateWorktree(baseline)
	if err != nil {
		return PreparedCommit{}, err
	}
	// The same refusal stageAll makes, for the same reason: never record a
	// symlink go-git would rewrite (checkNoAbsoluteSymlinks' own doc).
	if err := s.checkNoAbsoluteSymlinks(); err != nil {
		return PreparedCommit{}, err
	}
	storeFS, _ := s.storeFiles()
	// Modifications and additions go-git's status can see. AddWithOptions on
	// a directory walks status entries under it, so deletions of individual
	// files are covered too -- but not when the directory itself is gone: a
	// removed skill's prefix is not a file, so go-git's doAdd falls into its
	// single-file path and calls index.Remove on a name that was never
	// itself an entry, surfacing index.ErrEntryNotFound ("entry not found").
	// Skip the add for that prefix; the baseline scan below still records
	// every file that vanished with the directory.
	for _, prefix := range prefixes {
		// Only ErrNotExist means "gone"; any other lstat error stops the
		// write (final review, finding 7). Treating an unreadable path as
		// present is this file's one departure from the policy stageAll
		// states -- one file fu cannot read stops every write command --
		// and explainStagingFailure is the same renderer the store-wide
		// path uses, so both report an identical on-disk obstruction
		// identically.
		switch _, err := fs.Lstat(storeFS, prefix); {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return PreparedCommit{}, s.explainStagingFailure(err)
		}
		if err := wt.AddWithOptions(&git.AddOptions{Path: prefix}); err != nil {
			return PreparedCommit{}, s.explainStagingFailure(err)
		}
	}
	// Ignored files under a prefix, through the same force-add stageAll uses
	// for the whole store (stageUntracked, git.go) -- one definition, since
	// the SkipStatus detail that makes it work is easy to get subtly wrong.
	if err := s.stageUntracked(private, wt, func(rel string) bool {
		return underAnyPrefix(rel, prefixes)
	}); err != nil {
		return PreparedCommit{}, err
	}
	// Baseline entries under a prefix that are gone from disk: removed from
	// the candidate whether or not go-git's directory add noticed them.
	idx, err := private.Storer.Index()
	if err != nil {
		return PreparedCommit{}, err
	}
	removed := false
	for _, e := range baseline.Entries {
		if !underAnyPrefix(e.Name, prefixes) {
			continue
		}
		// Same reading as the prefix scan above (final review, finding 7):
		// anything but ErrNotExist is an obstruction, not a presence. Read
		// as presence, a baseline entry whose parent directory had been
		// replaced by a file kept its stale blob in the candidate, whose
		// changed set then came out empty -- `fu commit <name>` reporting
		// "nothing to commit" over a skill that had just been destroyed.
		switch _, err := fs.Lstat(storeFS, e.Name); {
		case errors.Is(err, fs.ErrNotExist):
			if _, err := idx.Remove(e.Name); err == nil {
				removed = true
			}
		case err != nil:
			return PreparedCommit{}, s.explainStagingFailure(err)
		}
	}
	if removed {
		if err := private.Storer.SetIndex(idx); err != nil {
			return PreparedCommit{}, err
		}
	}
	prepared, err := s.freezePrepared(private, wt, baseline)
	if err != nil {
		return PreparedCommit{}, err
	}
	// The role Op.AllowedChanges plays for pipeline operations: nothing
	// outside the prefixes may have reached the candidate. Not defensive,
	// though it reads that way -- the branch is user-reachable, for the
	// reason CommitScopeViolationError's own doc gives, and today it also
	// guards the unconditional sync below (DESIGN's known-gap list). Every
	// offending path is collected, not just the first: the one caller that
	// renders this for a user (explainCommitScopeViolation, engine/commit.go)
	// can then name them all at once instead of refusing once per path
	// (final review, finding 2).
	var outside []string
	for _, changed := range prepared.changed {
		if !underAnyPrefix(changed, prefixes) {
			outside = append(outside, changed)
		}
	}
	if len(outside) != 0 {
		return PreparedCommit{}, &CommitScopeViolationError{Prefixes: slices.Clone(prefixes), Paths: outside}
	}
	// Refresh the public index even though the baseline did not equal HEAD.
	//
	// freezePrepared's rule -- sync only when the public index equalled HEAD
	// at capture -- keeps a full-store candidate from installing itself over
	// work a direct-git user had staged, and it is right for that shape. It
	// is too coarse for this one. A scoped candidate *is* the baseline with
	// only in-prefix entries replaced: the adds, the untracked pass and the
	// removal loop above all refuse to touch anything outside the prefixes.
	// That is what makes it safe, and it is the construction that establishes
	// it -- the containment check just above is a guard on the changed set,
	// not a proof about every entry (review 2026-09-03, Minor). So writing it
	// back updates exactly the entries this commit superseded, and leaves
	// every other entry at bytes syncPreparedPublicIndex has already
	// confirmed are unchanged.
	//
	// Without this, staging a file inside the named skill and then editing it
	// further left the public index pinned to the superseded version: the
	// commit recorded the worktree, `fu status` went on reporting that skill
	// as uncommitted, and no fu command could clear it -- the next ordinary
	// write then swept the stale index entry back in, rolling the skill
	// backwards and forwards across two commits (review 2026-09-02,
	// Critical). git's own path-limited commit discards a superseded staged
	// version the same way; the cost, accepted deliberately, is that the
	// scoped form does not preserve a staged-only intermediate in history the
	// way the store-wide form's external layer does.
	prepared.syncPublic = true
	return prepared, nil
}

// CommitScopeViolationError reports that the candidate PrepareCommitUnder
// froze would change Paths, none of which lie under Prefixes. Paths is
// non-empty and sorted, in the changed set's own order.
//
// It is a named type rather than a fmt.Errorf string because the branch is
// user-reachable, not merely defensive -- the scoped candidate takes the
// public index's state outside the prefixes, so anything the user staged
// there with direct git lands in the changed set -- and the engine has to
// recover the paths to say what to do about them. Recovering them by
// scanning the rendered message for a marker substring is what this replaces
// (final review, finding 2): that failed *wrong* rather than safe if the
// message ever grew a suffix, could only ever surface one path, and
// mis-parsed a path that happened to contain the marker text.
type CommitScopeViolationError struct {
	Prefixes []string
	Paths    []string
}

func (e *CommitScopeViolationError) Error() string {
	return fmt.Sprintf("prepare commit under %v: candidate unexpectedly changes %s", e.Prefixes, strings.Join(e.Paths, ", "))
}

func validateCommitPrefix(prefix string) error {
	switch {
	case prefix == "":
		return errors.New("prepare commit under: empty path prefix")
	case strings.HasPrefix(prefix, "/"), strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("prepare commit under: prefix %q must be relative and carry no trailing slash", prefix)
	case path.Clean(prefix) != prefix:
		return fmt.Errorf("prepare commit under: prefix %q is not a clean path", prefix)
	}
	for _, component := range strings.Split(prefix, "/") {
		if component == ".." || component == "." {
			return fmt.Errorf("prepare commit under: prefix %q must not contain %q", prefix, component)
		}
	}
	return nil
}

// underAnyPrefix reports whether rel is one of the prefixes or a descendant
// of one, by path component -- "skills/alpha" admits "skills/alpha/SKILL.md"
// and not "skills/alphabet/SKILL.md".
func underAnyPrefix(rel string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}
