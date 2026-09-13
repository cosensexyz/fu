// internal/store/commit_scope.go
package store

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	indexformat "github.com/go-git/go-git/v5/plumbing/format/index"
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
// indiscriminate projection a sweep records -- and HEAD's tree for everything
// else. It is what lets `fu commit <name>` record one skill while leaving the
// rest of the worktree pending, including whatever a direct-git user staged
// elsewhere: that is neither recorded by this candidate nor disturbed by its
// install, which replaces only the in-prefix entries of the public index
// (syncPreparedPublicIndex).
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
	// The branch state the candidate is frozen against: its tree seeds every
	// out-of-prefix entry, and CommitPrepared publishes onto it alone.
	parent, err := s.capturePreparedCommitReference()
	if err != nil {
		return PreparedCommit{}, err
	}
	seedTree, seed, err := s.seedFromReference(parent)
	if err != nil {
		return PreparedCommit{}, err
	}
	baseline, err := s.capturePublicIndex()
	if err != nil {
		return PreparedCommit{}, err
	}
	// A store mid-merge is refused here for the whole index, not just the
	// prefix: preparedEntriesFromIndex refuses the store-wide shape the same
	// way, and a scoped commit published over an unmerged index would be
	// undone by the eventual merge commit until a later sweep re-recorded it.
	if err := checkNoUnmergedEntries(baseline.Entries); err != nil {
		return PreparedCommit{}, err
	}
	// The private index starts as the public one so go-git's stat cache
	// applies to unchanged in-prefix files; only its in-prefix entries are
	// read back below.
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
			_, _ = idx.Remove(e.Name)
		case err != nil:
			return PreparedCommit{}, s.explainStagingFailure(err)
		}
	}
	inScope, err := preparedEntriesFromIndex(entriesUnder(idx.Entries, prefixes))
	if err != nil {
		return PreparedCommit{}, err
	}
	entries := append(outsideEntries(seed, prefixes), inScope...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	changed := scopedChangedPaths(seed, inScope, prefixes)
	// By construction nothing outside the prefixes can differ from HEAD; the
	// assertion stays as the defensive check Op.AllowedChanges is for
	// pipeline operations, and is no longer a user-reachable refusal.
	var outside []string
	for _, c := range changed {
		if !underAnyPrefix(c, prefixes) {
			outside = append(outside, c)
		}
	}
	if len(outside) != 0 {
		return PreparedCommit{}, &CommitScopeViolationError{Prefixes: slices.Clone(prefixes), Paths: outside}
	}
	return PreparedCommit{
		entries:        entries,
		changed:        changed,
		fingerprint:    fingerprintPreparedEntries(entries),
		candidateIndex: cloneIndex(idx),
		publicBaseline: cloneIndex(baseline),
		// Always installed, over the live index: the in-prefix entries are
		// replaced so that a version staged inside the prefix and then
		// edited further does not keep the skill reported pending forever
		// (review 2026-09-02, Critical). git's own path-limited commit
		// discards such a superseded staged version the same way; the cost,
		// accepted deliberately, is that the scoped form does not preserve a
		// staged-only intermediate in history the way the store-wide form's
		// external layer does (SPEC §9).
		syncPublic: true,
		parent:     parent,
		scope:      slices.Clone(prefixes),
		seedTree:   seedTree,
	}, nil
}

// seedFromReference flattens the tree of the commit a captured branch state
// points at. An unborn branch seeds nothing.
func (s *Store) seedFromReference(ref preparedCommitReference) (plumbing.Hash, map[string]worktreeTarget, error) {
	if ref.before == nil {
		return plumbing.ZeroHash, map[string]worktreeTarget{}, nil
	}
	commit, err := s.Repo.CommitObject(ref.before.Hash())
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("read commit %s to seed the candidate: %w", ref.before.Hash(), err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("read tree of %s to seed the candidate: %w", ref.before.Hash(), err)
	}
	paths, err := targetTreePaths(tree)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	return commit.TreeHash, paths, nil
}

// seedEntriesOutside returns the sorted entries of the given tree that lie
// outside every prefix, as the frozen candidate must carry them.
func (s *Store) seedEntriesOutside(seedTree plumbing.Hash, prefixes []string) ([]preparedEntry, error) {
	if seedTree.IsZero() {
		return nil, nil
	}
	tree, err := s.Repo.TreeObject(seedTree)
	if err != nil {
		return nil, fmt.Errorf("read seed tree %s: %w", seedTree, err)
	}
	paths, err := targetTreePaths(tree)
	if err != nil {
		return nil, err
	}
	return outsideEntries(paths, prefixes), nil
}

// outsideEntries is the sorted subset of a flattened tree that lies outside
// every prefix; nil when there is none.
func outsideEntries(paths map[string]worktreeTarget, prefixes []string) []preparedEntry {
	var entries []preparedEntry
	for path, target := range paths {
		if !underAnyPrefix(path, prefixes) {
			entries = append(entries, preparedEntry{Path: path, Mode: target.Mode, Hash: target.Hash})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

// scopedChangedPaths is the in-prefix difference between the seed tree and
// the candidate: added, removed, and entries whose mode or blob changed.
func scopedChangedPaths(seed map[string]worktreeTarget, inScope []preparedEntry, prefixes []string) []string {
	var changed []string
	seen := make(map[string]bool, len(inScope))
	for _, e := range inScope {
		seen[e.Path] = true
		if want, ok := seed[e.Path]; !ok || want.Mode != e.Mode || want.Hash != e.Hash {
			changed = append(changed, e.Path)
		}
	}
	for path := range seed {
		if underAnyPrefix(path, prefixes) && !seen[path] {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

// splitPreparedEntries partitions sorted entries into those under a prefix
// and the rest, preserving order.
func splitPreparedEntries(entries []preparedEntry, prefixes []string) (in, out []preparedEntry) {
	for _, e := range entries {
		if underAnyPrefix(e.Path, prefixes) {
			in = append(in, e)
		} else {
			out = append(out, e)
		}
	}
	return in, out
}

// entriesUnder returns the index entries under any prefix; never nil, so two
// empty selections compare equal.
func entriesUnder(entries []*indexformat.Entry, prefixes []string) []*indexformat.Entry {
	out := []*indexformat.Entry{}
	for _, e := range entries {
		if underAnyPrefix(e.Name, prefixes) {
			out = append(out, e)
		}
	}
	return out
}

// entriesNotUnder is the complement of entriesUnder; never nil either.
func entriesNotUnder(entries []*indexformat.Entry, prefixes []string) []*indexformat.Entry {
	out := []*indexformat.Entry{}
	for _, e := range entries {
		if !underAnyPrefix(e.Name, prefixes) {
			out = append(out, e)
		}
	}
	return out
}

// cloneIndexEntries copies entries so an install never aliases the frozen
// candidate.
func cloneIndexEntries(entries []*indexformat.Entry) []*indexformat.Entry {
	out := make([]*indexformat.Entry, 0, len(entries))
	for _, e := range entries {
		copied := *e
		out = append(out, &copied)
	}
	return out
}

// CommitScopeViolationError reports that the candidate PrepareCommitUnder
// froze would change Paths, none of which lie under Prefixes. Paths is
// non-empty and sorted, in the changed set's own order.
//
// Since the candidate is seeded from HEAD outside the prefixes and its
// changed set is computed only over in-prefix paths, this cannot happen by
// construction; the check is kept as the defensive assertion Op.AllowedChanges
// is for pipeline operations, and no caller interprets the type for a user.
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

// sameIndexContent reports whether two entry selections agree on every
// content-bearing field: name, blob, mode, stage and the flags git reads.
// Stat fields are deliberately left out -- a refresh rewrites them without
// changing what is staged.
func sameIndexContent(a, b []*indexformat.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Hash != b[i].Hash || a[i].Mode != b[i].Mode ||
			a[i].Stage != b[i].Stage || a[i].SkipWorktree != b[i].SkipWorktree || a[i].IntentToAdd != b[i].IntentToAdd {
			return false
		}
	}
	return true
}
