package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"sort"

	indexformat "github.com/go-git/go-git/v5/plumbing/format/index"
)

// ErrConcurrentWorktreeChange means a rewrite's clean starting state no
// longer matches its captured references, index, or worktree content.
var ErrConcurrentWorktreeChange = errors.New("concurrent store change during worktree rewrite")

type worktreeGuard struct {
	store    *Store
	ref      preparedCommitReference
	index    *indexformat.Index
	expected map[string]worktreeTarget
	hooks    worktreeRewriteHooks
}

// worktreeRewriteHooks inject real external edits at deterministic boundaries.
// Production entry points always pass the zero value.
type worktreeRewriteHooks struct {
	beforeApply   func()
	beforePath    func(string)
	beforeIndex   func()
	beforePublish func()
}

func (s *Store) newWorktreeGuard() (*worktreeGuard, error) {
	if s.worktreeFS == nil {
		return nil, errUnpinnedWorktree
	}
	ref, err := s.capturePreparedCommitReference()
	if err != nil {
		return nil, err
	}
	if ref.before == nil {
		return nil, fmt.Errorf("%w: rewrite requires an existing HEAD", ErrConcurrentWorktreeChange)
	}
	commit, err := s.Repo.CommitObject(ref.before.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	expected, err := targetTreePaths(tree)
	if err != nil {
		return nil, err
	}
	idx, err := s.capturePublicIndex()
	if err != nil {
		return nil, err
	}
	if _, err := preparedEntriesFromIndex(idx.Entries); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConcurrentWorktreeChange, err)
	}
	if !s.indexMatchesTarget(idx, expected) {
		return nil, fmt.Errorf("%w: index no longer matches the swept HEAD", ErrConcurrentWorktreeChange)
	}
	g := &worktreeGuard{store: s, ref: ref, index: idx, expected: expected}
	return g, g.checkAll()
}

func (g *worktreeGuard) checkMetadata() error {
	current, err := g.store.capturePreparedCommitReference()
	if err != nil {
		return fmt.Errorf("%w: read HEAD: %v", ErrConcurrentWorktreeChange, err)
	}
	if current.before == nil || current.head.String() != g.ref.head.String() || current.before.String() != g.ref.before.String() {
		return fmt.Errorf("%w: HEAD changed", ErrConcurrentWorktreeChange)
	}
	// Index readers observe the atomic rename. This also runs while the
	// rewrite already holds index.lock, so it must not acquire it again.
	idx, err := g.store.Repo.Storer.Index()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(idx, g.index) {
		return fmt.Errorf("%w: index changed", ErrConcurrentWorktreeChange)
	}
	return nil
}

func (g *worktreeGuard) checkPath(name string) error {
	if err := g.checkMetadata(); err != nil {
		return err
	}
	if want, exists := g.expected[name]; exists {
		matched, err := g.store.worktreeMatchesTarget(name, want)
		if err != nil {
			return fmt.Errorf("%w: inspect %q: %v", ErrConcurrentWorktreeChange, name, err)
		}
		if !matched {
			return fmt.Errorf("%w: %q changed before it could be rewritten", ErrConcurrentWorktreeChange, name)
		}
		return nil
	}
	info, err := g.store.worktreeFS.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: inspect new path %q: %v", ErrConcurrentWorktreeChange, name, err)
	}
	// Directories can remain after tracked children have been removed.
	// The updater only rmdirs empty directories; new content blocks it.
	if info.IsDir() {
		return nil
	}
	return fmt.Errorf("%w: new path %q is occupied", ErrConcurrentWorktreeChange, name)
}

// checkAll compares bytes and Git modes, never go-git's stat cache. The walk
// also includes ignored files so a newly created path cannot hide from the
// final-tree validation. Empty directories are outside Git's projection.
func (g *worktreeGuard) checkAll() error {
	if err := g.checkMetadata(); err != nil {
		return err
	}
	names := make([]string, 0, len(g.expected))
	for name := range g.expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		matched, err := g.store.worktreeMatchesTarget(name, g.expected[name])
		if err != nil {
			return fmt.Errorf("%w: inspect %q: %v", ErrConcurrentWorktreeChange, name, err)
		}
		if !matched {
			return fmt.Errorf("%w: %q no longer contains the expected bytes and mode", ErrConcurrentWorktreeChange, name)
		}
	}
	if err := g.store.walkStoreFiles(func(_ fs.FS, _, name string, _ fs.DirEntry) error {
		if _, known := g.expected[name]; !known {
			return fmt.Errorf("%w: unexpected path %q", ErrConcurrentWorktreeChange, name)
		}
		return nil
	}); err != nil {
		return err
	}
	return g.checkMetadata()
}
