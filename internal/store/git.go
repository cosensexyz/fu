// internal/store/git.go
package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	indexformat "github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitstorage "github.com/go-git/go-git/v5/storage"
	"golang.org/x/sys/unix"
)

// ErrAbsoluteSymlink is returned by every staging path (Commit, IsDirty,
// and therefore Sweep) when the store holds a symlink whose target is an
// absolute path. See checkNoAbsoluteSymlinks for why such a link cannot be
// recorded faithfully, and DESIGN §6's known-gap entry for the boundary of
// what this refusal does and does not solve.
var ErrAbsoluteSymlink = errors.New("store holds a symlink with an absolute target, which git history cannot record faithfully")

func fuSignature() *object.Signature {
	return &object.Signature{Name: "fu", Email: "fu@local", When: time.Now()}
}

// CommitOutcome distinguishes errors raised before branch publication from
// verification failures raised after the commit object and ref update both
// succeeded. An orphan object created before a failed ref CAS is not Written.
type CommitOutcome struct {
	Hash    plumbing.Hash
	Written bool
}

type preparedEntry struct {
	Path string
	Mode filemode.FileMode
	Hash plumbing.Hash
}

// PreparedCommit freezes the exact Git index candidate a later commit may
// write. Its fields are private so callers cannot manufacture authority for a
// tree that PrepareCommit did not stage and inspect.
type PreparedCommit struct {
	entries     []preparedEntry
	changed     []string
	fingerprint string
	// candidateIndex belongs only to the in-memory staging repository. The
	// public baseline is retained solely for a conditional post-commit sync;
	// preparation and withdrawal never write either snapshot to the public
	// index.
	candidateIndex *indexformat.Index
	publicBaseline *indexformat.Index
	syncPublic     bool
}

// privateIndexStorer delegates objects, references, and configuration to the
// real repository while keeping all index reads and writes in memory. Blobs
// produced during staging therefore remain available to the eventual commit,
// but a direct Git process can never observe Fu's temporary candidate.
type privateIndexStorer struct {
	gitstorage.Storer
	index *indexformat.Index
}

func (s *privateIndexStorer) Index() (*indexformat.Index, error) {
	return cloneIndex(s.index), nil
}

func (s *privateIndexStorer) SetIndex(index *indexformat.Index) error {
	s.index = cloneIndex(index)
	return nil
}

// ChangedPaths returns the sorted HEAD-to-index path projection.
func (p PreparedCommit) ChangedPaths() []string {
	return append([]string(nil), p.changed...)
}

// TreeFingerprint identifies the complete prepared tree by stable Git fields:
// path, file mode, and blob hash.
func (p PreparedCommit) TreeFingerprint() string { return p.fingerprint }

// Commit stages everything and records one commit. An empty worktree
// (nothing changed) is not an error.
//
// The branch HEAD pointed at before staging is captured and updated with a
// compare-and-swap, so a concurrent direct-git commit cannot be overwritten.
// The commit tree is built from PreparedCommit's immutable entries rather
// than rereading the mutable index after validation. Fu's lock serializes fu
// processes, but these two checks also protect the supported direct-Git path.
func (s *Store) Commit(msg string) (CommitOutcome, error) {
	return s.commitWithHook(msg, nil)
}

// commitWithHook is Commit with a test-only seam: when non-nil, beforeWrite
// runs after staging and branch capture but before final validation and object
// construction. Production code always calls Commit, which passes nil.
func (s *Store) commitWithHook(msg string, beforeWrite func()) (CommitOutcome, error) {
	prepared, err := s.PrepareCommit()
	if err != nil {
		return CommitOutcome{}, err
	}
	outcome, err := s.commitPreparedWithHook(msg, prepared, beforeWrite)
	// A private candidate has nothing to withdraw from the public index. Keep
	// the call so every caller retains one abandonment path and older prepared
	// values remain harmless.
	if err != nil && !outcome.Written {
		return outcome, errors.Join(err, s.withdrawPreparedIndex(prepared))
	}
	return outcome, err
}

// withdrawPreparedIndex is RestorePreparedIndex for callers already reporting
// a failure. Private candidates make this a no-op, but keeping the boundary
// avoids teaching every transaction caller about the storage strategy.
func (s *Store) withdrawPreparedIndex(prepared PreparedCommit) error {
	if err := s.RestorePreparedIndex(prepared); err != nil {
		return fmt.Errorf("withdraw the prepared Git index: %w", err)
	}
	return nil
}

// PrepareCommit stages the complete worktree projection once in a private
// in-memory index and freezes the result. Changes arriving afterward stay
// outside this candidate, and direct Git never sees temporary Fu staging.
func (s *Store) PrepareCommit() (PreparedCommit, error) {
	baseline, err := s.capturePublicIndex()
	if err != nil {
		return PreparedCommit{}, err
	}
	return s.prepareCommit(baseline)
}

func (s *Store) capturePublicIndex() (*indexformat.Index, error) {
	var baseline *indexformat.Index
	err := s.withIndexLock(func() error {
		index, err := s.Repo.Storer.Index()
		if err != nil {
			return err
		}
		baseline = cloneIndex(index)
		return nil
	})
	return baseline, err
}

func (s *Store) prepareCommit(baseline *indexformat.Index) (PreparedCommit, error) {
	private, wt, err := s.privateWorktree(baseline)
	if err != nil {
		return PreparedCommit{}, err
	}
	if err := s.stageAll(private, wt); err != nil {
		return PreparedCommit{}, err
	}
	idx, err := private.Storer.Index()
	if err != nil {
		return PreparedCommit{}, err
	}
	entries, err := preparedEntriesFromIndex(idx.Entries)
	if err != nil {
		return PreparedCommit{}, err
	}
	status, err := wt.Status()
	if err != nil {
		return PreparedCommit{}, err
	}
	changed := make([]string, 0, len(status))
	for name, state := range status {
		if state.Staging != git.Unmodified {
			changed = append(changed, filepath.ToSlash(name))
		}
	}
	sort.Strings(changed)
	return PreparedCommit{
		entries:        entries,
		changed:        changed,
		fingerprint:    fingerprintPreparedEntries(entries),
		candidateIndex: cloneIndex(idx),
		publicBaseline: cloneIndex(baseline),
		syncPublic:     s.indexMatchesHEAD(baseline),
	}, nil
}

// preparePublicIndexSnapshot freezes exactly what a direct Git user staged,
// without consulting or rewriting the worktree side of the public index. Sweep
// commits this snapshot before separately recording later worktree bytes, so a
// staged-only version remains recoverable in history.
func (s *Store) preparePublicIndexSnapshot() (PreparedCommit, error) {
	baseline, err := s.capturePublicIndex()
	if err != nil {
		return PreparedCommit{}, err
	}
	entries, err := preparedEntriesFromIndex(baseline.Entries)
	if err != nil {
		return PreparedCommit{}, err
	}
	_, wt, err := s.privateWorktree(baseline)
	if err != nil {
		return PreparedCommit{}, err
	}
	status, err := wt.Status()
	if err != nil {
		return PreparedCommit{}, err
	}
	changed := make([]string, 0, len(status))
	for name, state := range status {
		if state.Staging != git.Unmodified {
			changed = append(changed, filepath.ToSlash(name))
		}
	}
	sort.Strings(changed)
	return PreparedCommit{
		entries:        entries,
		changed:        changed,
		fingerprint:    fingerprintPreparedEntries(entries),
		candidateIndex: cloneIndex(baseline),
		publicBaseline: cloneIndex(baseline),
		// The public index already is this candidate. Leaving it alone both
		// preserves a concurrent later writer and gives normal Git commit
		// semantics when it remains unchanged.
		syncPublic: false,
	}, nil
}

func (s *Store) privateWorktree(index *indexformat.Index) (*git.Repository, *git.Worktree, error) {
	publicWorktree, err := s.Repo.Worktree()
	if err != nil {
		return nil, nil, err
	}
	storer := &privateIndexStorer{Storer: s.Repo.Storer, index: cloneIndex(index)}
	private, err := git.Open(storer, publicWorktree.Filesystem)
	if err != nil {
		return nil, nil, err
	}
	worktree, err := private.Worktree()
	if err != nil {
		return nil, nil, err
	}
	// Carried across explicitly rather than left to chance: the private
	// worktree must apply the same ignore patterns as the public one, and
	// go-git populates Excludes lazily, so this is empty today and is the
	// line that keeps it correct if that ever changes.
	worktree.Excludes = append([]gitignore.Pattern(nil), publicWorktree.Excludes...)
	return private, worktree, nil
}

func (s *Store) indexMatchesHEAD(index *indexformat.Index) bool {
	entries, err := preparedEntriesFromIndex(index.Entries)
	if err != nil {
		return false
	}
	want := fingerprintPreparedEntries(entries)
	head, err := s.Repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return want == fingerprintPreparedEntries(nil)
	}
	if err != nil {
		return false
	}
	commit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		return false
	}
	got, err := commitTreeFingerprint(commit)
	return err == nil && got == want
}

// withIndexLock holds .git/index.lock while Fu captures a public baseline or
// conditionally synchronizes that still-unchanged baseline after publication.
//
// A check followed by SetIndex is not a compare-and-swap, and the index is
// public: the supported direct-Git path can write it between the two. index.lock
// is the protocol every Git implementation already honours for exactly this, so
// taking it makes each capture or conditional sync atomic against that writer.
// Private candidate staging never needs the lock. A lock left by an interrupted
// process stops Fu the same way it stops Git, and is cleared the same way.
func (s *Store) withIndexLock(fn func() error) (err error) {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.git == nil || s.writeRoots.git.dir == nil {
		// Outside a checked write session (Init's bootstrap commit) there is no
		// pinned Git root to anchor the lock to, and no concurrent writer to
		// exclude: the repository is not published yet.
		return fn()
	}
	gitFD := int(s.writeRoots.git.dir.Fd())
	display := filepath.Join(s.writeRoots.git.display, "index.lock")
	fd, err := unix.Openat(gitFD, "index.lock",
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("another Git process holds %s; if none is running, remove the file and retry", display)
		}
		return fmt.Errorf("take the Git index lock %s: %w", display, err)
	}
	// Deferred so a panic inside fn cannot leave the lock behind, which would
	// stop both fu and git until someone removed it by hand.
	defer func() {
		closeErr := unix.Close(fd)
		unlinkErr := unix.Unlinkat(gitFD, "index.lock", 0)
		if unlinkErr != nil {
			unlinkErr = fmt.Errorf("release the Git index lock %s: %w", display, unlinkErr)
		}
		err = errors.Join(err, closeErr, unlinkErr)
	}()
	return fn()
}

func cloneIndex(idx *indexformat.Index) *indexformat.Index {
	if idx == nil {
		return nil
	}
	clone := *idx
	if idx.Entries != nil {
		clone.Entries = make([]*indexformat.Entry, len(idx.Entries))
		for i, entry := range idx.Entries {
			copied := *entry
			clone.Entries[i] = &copied
		}
	}
	if idx.Cache != nil {
		cache := *idx.Cache
		if idx.Cache.Entries != nil {
			cache.Entries = append([]indexformat.TreeEntry(nil), idx.Cache.Entries...)
		}
		clone.Cache = &cache
	}
	if idx.ResolveUndo != nil {
		undo := *idx.ResolveUndo
		if idx.ResolveUndo.Entries != nil {
			undo.Entries = make([]indexformat.ResolveUndoEntry, len(idx.ResolveUndo.Entries))
			for i, entry := range idx.ResolveUndo.Entries {
				copied := entry
				if entry.Stages != nil {
					copied.Stages = make(map[indexformat.Stage]plumbing.Hash, len(entry.Stages))
					for stage, hash := range entry.Stages {
						copied.Stages[stage] = hash
					}
				}
				undo.Entries[i] = copied
			}
		}
		clone.ResolveUndo = &undo
	}
	if idx.EndOfIndexEntry != nil {
		end := *idx.EndOfIndexEntry
		clone.EndOfIndexEntry = &end
	}
	return &clone
}

// RestorePreparedIndex abandons a prepared candidate. Preparation is private,
// so there is deliberately no public state to restore or validate.
func (s *Store) RestorePreparedIndex(prepared PreparedCommit) error {
	return nil
}

// CommitPrepared writes only the immutable candidate returned by
// PrepareCommit. It never stages the filesystem or uses the public index as
// commit input after preparation.
func (s *Store) CommitPrepared(msg string, prepared PreparedCommit) (CommitOutcome, error) {
	return s.commitPreparedWithHook(msg, prepared, nil)
}

func (s *Store) commitPreparedWithHook(msg string, prepared PreparedCommit, beforeWrite func()) (CommitOutcome, error) {
	if prepared.fingerprint == "" {
		return CommitOutcome{}, errors.New("prepared commit has no tree fingerprint")
	}
	if err := validatePreparedCommit(prepared); err != nil {
		return CommitOutcome{}, err
	}
	refState, err := s.capturePreparedCommitReference()
	if err != nil {
		return CommitOutcome{}, err
	}
	if beforeWrite != nil {
		beforeWrite()
	}
	if err := validatePreparedCommit(prepared); err != nil {
		return CommitOutcome{}, err
	}
	treeHash, err := s.storePreparedTree(prepared.entries)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("build prepared Git tree: %w", err)
	}
	parents := []plumbing.Hash(nil)
	if refState.before == nil {
		if len(prepared.entries) == 0 {
			return CommitOutcome{}, nil
		}
	} else {
		parent, err := s.Repo.CommitObject(refState.before.Hash())
		if err != nil {
			return CommitOutcome{}, fmt.Errorf("read commit parent %s: %w", refState.before.Hash(), err)
		}
		if parent.TreeHash == treeHash {
			return CommitOutcome{}, nil
		}
		parents = []plumbing.Hash{refState.before.Hash()}
	}

	signature := fuSignature()
	commit := &object.Commit{
		Author:       *signature,
		Committer:    *signature,
		Message:      msg,
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	encoded := s.Repo.Storer.NewEncodedObject()
	if err := commit.Encode(encoded); err != nil {
		return CommitOutcome{}, fmt.Errorf("encode prepared commit: %w", err)
	}
	hash, err := s.Repo.Storer.SetEncodedObject(encoded)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("store prepared commit: %w", err)
	}

	if err := s.verifyCapturedHEAD(refState); err != nil {
		return CommitOutcome{}, err
	}
	updated := plumbing.NewHashReference(refState.target, hash)
	if err := s.Repo.Storer.CheckAndSetReference(updated, refState.before); err != nil {
		if errors.Is(err, gitstorage.ErrReferenceHasChanged) {
			return CommitOutcome{}, fmt.Errorf("concurrent write to %s detected before fu could publish commit %s; the branch was not overwritten", refState.target, hash.String()[:7])
		}
		return CommitOutcome{}, fmt.Errorf("compare-and-swap branch %s to commit %s: %w", refState.target, hash.String()[:7], err)
	}
	outcome := CommitOutcome{Hash: hash, Written: true}
	current, err := s.Repo.Storer.Reference(refState.target)
	if err != nil {
		return outcome, fmt.Errorf("verify branch after writing commit %s: %w", hash.String()[:7], err)
	}
	if current.Type() != plumbing.HashReference || current.Hash() != hash {
		return outcome, fmt.Errorf("concurrent write to %s detected after fu wrote commit %s: branch now points at %s; fu's commit remains in the object database and the later branch target was not overwritten",
			refState.target, hash.String()[:7], current.Hash().String()[:7])
	}
	if err := s.syncPreparedPublicIndex(prepared); err != nil {
		return outcome, fmt.Errorf("commit %s was published but its public index could not be synchronized: %w", hash.String()[:7], err)
	}
	return outcome, nil
}

func validatePreparedCommit(prepared PreparedCommit) error {
	if prepared.candidateIndex == nil || prepared.publicBaseline == nil {
		return errors.New("prepared commit has no private-index provenance")
	}
	entries, err := preparedEntriesFromIndex(prepared.candidateIndex.Entries)
	if err != nil {
		return err
	}
	if got := fingerprintPreparedEntries(entries); got != prepared.fingerprint || !reflect.DeepEqual(entries, prepared.entries) {
		return errors.New("prepared commit's private index no longer matches its frozen tree")
	}
	return nil
}

func (s *Store) syncPreparedPublicIndex(prepared PreparedCommit) error {
	if !prepared.syncPublic {
		return nil
	}
	return s.withIndexLock(func() error {
		current, err := s.Repo.Storer.Index()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, prepared.publicBaseline) {
			return nil
		}
		return s.writePublicIndexAtomically(prepared.candidateIndex)
	})
}

// writePublicIndexAtomically installs a new .git/index by rename rather than by
// truncating the live file.
//
// Storer.SetIndex is fs.Create(".git/index"), i.e. O_TRUNC in place. Real git
// writes into index.lock and renames it over index, which is exactly why a
// reader never needs the lock: `git status` and `git diff` take none, so an
// in-place rewrite lets them observe a truncated index, and a crash inside that
// window leaves a truncated index plus a stale lock, after which every fu
// command fails in Status() with a decode error that points at the lock and
// says nothing about the index. The temp name is fu's own rather than
// index.lock itself, because the lock is already held here for mutual exclusion
// and reusing it as the staging file would release it by rename.
func (s *Store) writePublicIndexAtomically(idx *indexformat.Index) error {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.git == nil || s.writeRoots.git.dir == nil {
		// No pinned Git root (Init's bootstrap commit): nothing else can be
		// reading this repository yet.
		return s.Repo.Storer.SetIndex(cloneIndex(idx))
	}
	gitFD := int(s.writeRoots.git.dir.Fd())
	file, name, err := createTempAt(s.writeRoots.git.dir, ".fu-index-")
	if err != nil {
		return err
	}
	discard := func() { _ = unix.Unlinkat(gitFD, name, 0) }
	writer := bufio.NewWriter(file)
	if err := indexformat.NewEncoder(writer).Encode(cloneIndex(idx)); err != nil {
		_ = file.Close()
		discard()
		return fmt.Errorf("encode Git index: %w", err)
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		discard()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		discard()
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		discard()
		return err
	}
	if err := file.Close(); err != nil {
		discard()
		return err
	}
	if err := unix.Renameat(gitFD, name, gitFD, "index"); err != nil {
		discard()
		return fmt.Errorf("install Git index %s/index: %w", s.writeRoots.git.display, err)
	}
	return nil
}

type preparedCommitReference struct {
	head   *plumbing.Reference
	target plumbing.ReferenceName
	before *plumbing.Reference
}

func (s *Store) capturePreparedCommitReference() (preparedCommitReference, error) {
	head, err := s.Repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return preparedCommitReference{}, fmt.Errorf("read HEAD before commit: %w", err)
	}
	state := preparedCommitReference{head: head, target: plumbing.HEAD}
	switch head.Type() {
	case plumbing.SymbolicReference:
		state.target = head.Target()
		state.before, err = s.Repo.Storer.Reference(state.target)
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			state.before = nil
			return state, nil
		}
	case plumbing.HashReference:
		state.before = head
	default:
		return preparedCommitReference{}, fmt.Errorf("HEAD has unsupported reference type %s", head.Type())
	}
	if err != nil {
		return preparedCommitReference{}, fmt.Errorf("read commit branch %s: %w", state.target, err)
	}
	if state.before.Type() != plumbing.HashReference {
		return preparedCommitReference{}, fmt.Errorf("commit branch %s is not a direct hash reference", state.target)
	}
	return state, nil
}

func (s *Store) verifyCapturedHEAD(state preparedCommitReference) error {
	if state.head.Type() != plumbing.SymbolicReference {
		return nil
	}
	current, err := s.Repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return fmt.Errorf("verify HEAD before publishing prepared commit: %w", err)
	}
	if current.Type() != plumbing.SymbolicReference || current.Target() != state.head.Target() {
		return fmt.Errorf("concurrent HEAD change detected before fu could publish its prepared commit; no branch was overwritten")
	}
	if state.before == nil {
		if current, err := s.Repo.Storer.Reference(state.target); err == nil {
			return fmt.Errorf("concurrent first commit to %s detected before fu could publish its prepared commit: branch now points at %s", state.target, current.Hash().String()[:7])
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("verify unborn branch %s before first commit: %w", state.target, err)
		}
	}
	return nil
}

type preparedTreeNode struct {
	entry    *preparedEntry
	children map[string]*preparedTreeNode
}

func (s *Store) storePreparedTree(entries []preparedEntry) (plumbing.Hash, error) {
	root := &preparedTreeNode{children: make(map[string]*preparedTreeNode)}
	for _, prepared := range entries {
		if prepared.Path == "" || path.Clean(prepared.Path) != prepared.Path || strings.HasPrefix(prepared.Path, "/") {
			return plumbing.ZeroHash, fmt.Errorf("prepared index path %q is not canonical", prepared.Path)
		}
		if prepared.Hash.IsZero() || prepared.Mode.IsMalformed() || prepared.Mode == filemode.Dir {
			return plumbing.ZeroHash, fmt.Errorf("prepared index entry %q has invalid mode %s or object %s", prepared.Path, prepared.Mode, prepared.Hash)
		}
		parts := strings.Split(prepared.Path, "/")
		node := root
		for i, part := range parts {
			if part == "" || part == "." || part == ".." {
				return plumbing.ZeroHash, fmt.Errorf("prepared index path %q has invalid component %q", prepared.Path, part)
			}
			child := node.children[part]
			last := i == len(parts)-1
			if last {
				if child != nil {
					return plumbing.ZeroHash, fmt.Errorf("prepared index contains duplicate or file/directory-conflicting path %q", prepared.Path)
				}
				entry := prepared
				node.children[part] = &preparedTreeNode{entry: &entry}
				continue
			}
			if child == nil {
				child = &preparedTreeNode{children: make(map[string]*preparedTreeNode)}
				node.children[part] = child
			} else if child.entry != nil {
				return plumbing.ZeroHash, fmt.Errorf("prepared index path %q descends through file %q", prepared.Path, strings.Join(parts[:i+1], "/"))
			}
			node = child
		}
	}
	return s.storePreparedTreeNode(root)
}

func (s *Store) storePreparedTreeNode(node *preparedTreeNode) (plumbing.Hash, error) {
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := names[i], names[j]
		if node.children[left].entry == nil {
			left += "/"
		}
		if node.children[right].entry == nil {
			right += "/"
		}
		return left < right
	})
	tree := &object.Tree{Entries: make([]object.TreeEntry, 0, len(names))}
	for _, name := range names {
		child := node.children[name]
		if child.entry != nil {
			if err := s.Repo.Storer.HasEncodedObject(child.entry.Hash); err != nil {
				return plumbing.ZeroHash, fmt.Errorf("prepared object %s for %q is unavailable: %w", child.entry.Hash, child.entry.Path, err)
			}
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: child.entry.Mode, Hash: child.entry.Hash})
			continue
		}
		hash, err := s.storePreparedTreeNode(child)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
	}
	encoded := s.Repo.Storer.NewEncodedObject()
	if err := tree.Encode(encoded); err != nil {
		return plumbing.ZeroHash, err
	}
	hash := encoded.Hash()
	if err := s.Repo.Storer.HasEncodedObject(hash); err == nil {
		return hash, nil
	}
	return s.Repo.Storer.SetEncodedObject(encoded)
}

func preparedEntriesFromIndex(indexEntries []*indexformat.Entry) ([]preparedEntry, error) {
	entries := make([]preparedEntry, 0, len(indexEntries))
	for _, entry := range indexEntries {
		if entry.Stage != 0 {
			return nil, fmt.Errorf("cannot prepare unmerged index entry %q at stage %d", entry.Name, entry.Stage)
		}
		entries = append(entries, preparedEntry{Path: entry.Name, Mode: entry.Mode, Hash: entry.Hash})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func fingerprintPreparedEntries(entries []preparedEntry) string {
	h := sha256.New()
	for _, entry := range entries {
		_, _ = fmt.Fprintf(h, "%d:%s\x00%d\x00%s\n", len(entry.Path), entry.Path, entry.Mode, entry.Hash)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func commitTreeFingerprint(commit *object.Commit) (string, error) {
	tree, err := commit.Tree()
	if err != nil {
		return "", err
	}
	iter := tree.Files()
	defer iter.Close()
	var entries []preparedEntry
	if err := iter.ForEach(func(file *object.File) error {
		entries = append(entries, preparedEntry{Path: file.Name, Mode: file.Mode, Hash: file.Hash})
		return nil
	}); err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return fingerprintPreparedEntries(entries), nil
}

// CommitTreeFingerprint returns the same stable full-tree identity used by a
// PreparedCommit, but for an already-written commit.
func (s *Store) CommitTreeFingerprint(hash plumbing.Hash) (string, error) {
	commit, err := s.Repo.CommitObject(hash)
	if err != nil {
		return "", err
	}
	return commitTreeFingerprint(commit)
}

func preparedEntryByPath(prepared PreparedCommit, name string) (preparedEntry, bool) {
	name = filepath.ToSlash(name)
	i := sort.Search(len(prepared.entries), func(i int) bool { return prepared.entries[i].Path >= name })
	if i == len(prepared.entries) || prepared.entries[i].Path != name {
		return preparedEntry{}, false
	}
	return prepared.entries[i], true
}

func (s *Store) preparedEntryBytes(entry preparedEntry) ([]byte, error) {
	blob, err := s.Repo.BlobObject(entry.Hash)
	if err != nil {
		return nil, err
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

// ValidatePreparedFile requires one prepared regular file to contain exactly
// the supplied bytes.
func (s *Store) ValidatePreparedFile(prepared PreparedCommit, name string, want []byte) error {
	entry, ok := preparedEntryByPath(prepared, name)
	if !ok {
		return fmt.Errorf("prepared tree has no file %q", filepath.ToSlash(name))
	}
	if entry.Mode != filemode.Regular {
		return fmt.Errorf("prepared file %q has Git mode %s, want regular", filepath.ToSlash(name), entry.Mode)
	}
	got, err := s.preparedEntryBytes(entry)
	if err != nil {
		return fmt.Errorf("read prepared file %q: %w", filepath.ToSlash(name), err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("prepared file %q does not contain the expected bytes", filepath.ToSlash(name))
	}
	return nil
}

// ValidatePreparedPathAbsent requires the frozen Git tree to contain neither
// the named path nor any descendant below it.
func (s *Store) ValidatePreparedPathAbsent(prepared PreparedCommit, name string) error {
	name = path.Clean(filepath.ToSlash(name))
	for _, entry := range prepared.entries {
		if entry.Path == name || strings.HasPrefix(entry.Path, name+"/") {
			return fmt.Errorf("prepared tree unexpectedly contains %q", entry.Path)
		}
	}
	return nil
}

// ValidatePreparedOwnedTree checks the Git-representable descendants of an
// ownership manifest against the frozen candidate. Directories and their
// permission bits are validated through the live manifest boundary; Git
// records only regular/executable files and symlinks.
func (s *Store) ValidatePreparedOwnedTree(prepared PreparedCommit, root string, expected OwnedTree) error {
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("invalid transaction-owned tree manifest: %w", err)
	}
	root = path.Clean(filepath.ToSlash(root))
	if root == "." || path.IsAbs(root) || root == ".." || strings.HasPrefix(root, "../") {
		return fmt.Errorf("prepared owned-tree root must be a safe relative path: %q", root)
	}
	prefix := root + "/"
	actual := make(map[string]preparedEntry)
	for _, entry := range prepared.entries {
		if strings.HasPrefix(entry.Path, prefix) {
			actual[strings.TrimPrefix(entry.Path, prefix)] = entry
		}
	}
	for _, owned := range expected.Entries {
		if owned.Kind == ownedDirectory {
			continue
		}
		entry, ok := actual[owned.Path]
		if !ok {
			return fmt.Errorf("prepared tree is missing transaction-owned entry %q", path.Join(root, owned.Path))
		}
		delete(actual, owned.Path)
		wantMode := filemode.Regular
		switch owned.Kind {
		case ownedFile:
			if os.FileMode(owned.Mode).Perm()&0o111 != 0 {
				wantMode = filemode.Executable
			}
		case ownedSymlink:
			wantMode = filemode.Symlink
		default:
			return fmt.Errorf("unsupported prepared transaction entry kind %q", owned.Kind)
		}
		if entry.Mode != wantMode {
			return fmt.Errorf("prepared transaction entry %q has Git mode %s, want %s", path.Join(root, owned.Path), entry.Mode, wantMode)
		}
		data, err := s.preparedEntryBytes(entry)
		if err != nil {
			return fmt.Errorf("read prepared transaction entry %q: %w", path.Join(root, owned.Path), err)
		}
		if owned.Kind == ownedSymlink {
			if string(data) != owned.Target {
				return fmt.Errorf("prepared transaction symlink %q has an unexpected target", path.Join(root, owned.Path))
			}
			continue
		}
		digest := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(digest[:]) != owned.Digest {
			return fmt.Errorf("prepared transaction file %q has an unexpected digest", path.Join(root, owned.Path))
		}
	}
	if len(actual) != 0 {
		unknown := make([]string, 0, len(actual))
		for name := range actual {
			unknown = append(unknown, path.Join(root, name))
		}
		sort.Strings(unknown)
		return fmt.Errorf("prepared transaction tree contains unknown entries %q", unknown)
	}
	return nil
}

// stageAll stages every worktree change, including content a .gitignore
// anywhere in the store would otherwise keep out of history forever
// (Critical finding 3). AddWithOptions{All: true} stages exactly what
// Worktree.Status() reports, and Status applies every .gitignore in the
// worktree the same way `git status` does: a path that has never been
// tracked and matches such a rule never appears in Status at all, so the
// ordinary All-based add never even considers it, on this call or any
// later one. Skills are expected to ship arbitrary content, including
// their own .gitignore (e.g. copied wholesale from an existing project),
// and the store must preserve all of it regardless: skill.Digest already
// hashes such files (DESIGN §3's normalized projection excludes only
// .git), so if they never enter history a fresh clone or machine
// migration silently rebuilds an incomplete skill, and digest(store) then
// permanently disagrees with the recorded baseline.
//
// The All-based add runs first, unchanged: it is the well-tested path for
// ordinary adds, modifications, and deletions. Deletions in particular
// keep working through it even for a path a newer .gitignore rule now
// matches, because gitignore only ever hides *untracked* paths from
// Status -- a path already tracked in the index still shows as deleted
// when missing from disk (see go-git's excludeIgnoredChanges, which only
// ever drops changes with no index-side entry). The worktree is then
// walked directly; any file still missing from the index afterward can
// only be one Status silently dropped, and is force-added with
// SkipStatus, which go-git documents as bypassing ignore rules entirely
// for an explicit file Path (unlike a directory Path, where Status is
// still consulted internally). Once a path is tracked this way, ordinary
// edits to it are picked up by the All-based add on every later Commit
// call like any other tracked file -- no special handling is needed a
// second time.
func (s *Store) stageAll(repo *git.Repository, wt *git.Worktree) error {
	// Before anything is staged: refuse outright if the store holds a
	// symlink git cannot record faithfully, rather than committing a
	// rewritten target. Checked ahead of the All-based add specifically so
	// the on-disk index is never left carrying the corrupt entry.
	if err := s.checkNoAbsoluteSymlinks(); err != nil {
		return err
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return s.explainStagingFailure(err)
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	// A set, not repeated idx.Entry lookups: Index.Entry is a linear scan,
	// and the walk below may visit every file in the store.
	tracked := make(map[string]bool, len(idx.Entries))
	for _, e := range idx.Entries {
		tracked[e.Name] = true
	}
	if err := s.walkStoreFiles(func(_ fs.FS, _, rel string, _ fs.DirEntry) error {
		if tracked[rel] {
			return nil // already tracked by the All-based add above
		}
		return wt.AddWithOptions(&git.AddOptions{Path: rel, SkipStatus: true})
	}); err != nil {
		return s.explainStagingFailure(err)
	}
	return nil
}

// explainStagingFailure turns an unreadable entry into advice.
//
// Staging must read every file in the store, so one file fu cannot open stops
// every write command -- a stricter requirement than `git status`, which
// tolerates an unreadable untracked file, and the same one `git add -A` has
// (it exits 128 with "unable to index file"). The behaviour is not reducible,
// but a bare errno naming a store-relative path is: it says nothing about
// which store, or what to do.
func (s *Store) explainStagingFailure(err error) error {
	var pathErr *fs.PathError
	target := ""
	if errors.As(err, &pathErr) {
		target = s.absoluteStagingFailurePath(pathErr.Path)
	}
	if errors.Is(err, unix.ENOTDIR) {
		if target == "" {
			return fmt.Errorf("%w; a tracked store path is not a directory; move or remove the conflicting entry, then retry the command", err)
		}
		return fmt.Errorf("%w; store entry %s is not a directory; move or remove it, then retry the command", err, target)
	}
	if errors.Is(err, fs.ErrInvalid) {
		if target == "" {
			return fmt.Errorf("%w; unable to stage the store under %s; inspect its entries and retry the command", err, s.Dir())
		}
		return fmt.Errorf("%w; unable to stage store entry %s; inspect or move it, then retry the command", err, target)
	}
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if target == "" {
		return fmt.Errorf("%w; every write command records the whole store, so it must be able to read every file under %s",
			err, s.Dir())
	}
	return fmt.Errorf("%w; fu records the whole store on every write command, so make %s readable or move it out of the store",
		err, target)
}

// absoluteStagingFailurePath restores the logical store prefix that a mounted
// checked root may omit from an fs.PathError. Prefer the candidate that exists
// on disk so diagnostics name the entry the user can actually repair.
func (s *Store) absoluteStagingFailurePath(name string) string {
	if name == "" {
		return ""
	}
	name = filepath.FromSlash(name)
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	candidates := []string{
		filepath.Join(s.Dir(), name),
		filepath.Join(s.SkillsDir(), name),
	}
	for _, candidate := range candidates {
		if _, err := os.Lstat(candidate); err == nil {
			return candidate
		}
	}
	return candidates[0]
}

// ChangedPathsIncludingIgnored reports the side-effect-free worktree
// projection that stageAll would stage. Worktree.Status supplies tracked and
// ordinary untracked changes; the shared filesystem walk adds ignored paths
// that are present on disk but absent from the index.
func (s *Store) ChangedPathsIncludingIgnored() ([]string, error) {
	wt, err := s.Repo.Worktree()
	if err != nil {
		return nil, err
	}
	status, err := wt.Status()
	if err != nil {
		return nil, err
	}
	changed := make(map[string]struct{}, len(status))
	for name := range status {
		changed[filepath.ToSlash(name)] = struct{}{}
	}
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	tracked := make(map[string]struct{}, len(idx.Entries))
	for _, entry := range idx.Entries {
		tracked[entry.Name] = struct{}{}
	}
	if err := s.walkStoreFiles(func(_ fs.FS, _, rel string, _ fs.DirEntry) error {
		if _, ok := tracked[rel]; !ok {
			changed[rel] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(changed))
	for name := range changed {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths, nil
}

// UnstagedPathsIncludingIgnored reports filesystem changes that are not in a
// private prepared candidate. It is used after PrepareCommit to detect writers
// that landed before the operation commit without consulting or mutating the
// unrelated public index.
func (s *Store) UnstagedPathsIncludingIgnored(prepared PreparedCommit) ([]string, error) {
	if err := validatePreparedCommit(prepared); err != nil {
		return nil, err
	}
	_, wt, err := s.privateWorktree(prepared.candidateIndex)
	if err != nil {
		return nil, err
	}
	status, err := wt.Status()
	if err != nil {
		return nil, err
	}
	changed := make(map[string]struct{}, len(status))
	for name, state := range status {
		if state.Worktree != git.Unmodified {
			changed[filepath.ToSlash(name)] = struct{}{}
		}
	}
	tracked := make(map[string]struct{}, len(prepared.candidateIndex.Entries))
	for _, entry := range prepared.candidateIndex.Entries {
		tracked[entry.Name] = struct{}{}
	}
	if err := s.walkStoreFiles(func(_ fs.FS, _, rel string, _ fs.DirEntry) error {
		if _, ok := tracked[rel]; !ok {
			changed[rel] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(changed))
	for name := range changed {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *Store) storeFiles() (fs.FS, string) {
	if s.worktreeFS != nil {
		return rootStandardFS{billy: s.worktreeFS}, "."
	}
	return os.DirFS(s.Dir()), "."
}

func (s *Store) walkStoreFiles(visit func(fs.FS, string, string, fs.DirEntry) error) error {
	storeFS, walkRoot := s.storeFiles()
	return fs.WalkDir(storeFS, walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			// Excluded at any depth, mirroring skill.Digest and go-git's
			// worktree projection.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(walkRoot, p)
		if err != nil {
			return err
		}
		return visit(storeFS, p, filepath.ToSlash(rel), d)
	})
}

// checkNoAbsoluteSymlinks refuses to stage anything while the store holds
// a symlink whose target is an absolute path (round 5 finding).
//
// go-git's worktree runs on a go-billy chroot filesystem, whose Readlink
// rewrites an absolute target to "/" + filepath.Rel(base, target). Every
// path that reads a symlink through the worktree therefore sees a
// different string than the filesystem does, and the damage runs both
// ways. Staging records that rewritten string as the blob, so the store's
// git history -- SPEC §9's stated safety net for store content -- holds a
// link that points somewhere the user never wrote, and a fresh clone
// rebuilds it that way (core scenario 4, "clone is restore"). Status reads
// it the same way, so the worktree compares unequal against its own
// committed blob forever: IsDirty stays true no matter how many times
// Sweep commits, permanently falsifying DESIGN §4's premise that sweep
// keeps the worktree normally clean.
//
// Reproduced against the compiled binary, with a hand-added
// `ln -s /tmp/x store/skills/alpha/abs`:
//
//	on disk           /tmp/fusym.9XNyuY/outside.txt
//	git cat-file      /../../../../../../tmp/fusym.9XNyuY/outside.txt
//	git status        " M skills/alpha/abs", still dirty after three more writes
//	fresh clone       rebuilt with the rewritten target
//
// The cause is go-git core, not this file's own SkipStatus force-add path:
// a bare AddWithOptions{All: true} produces the same wrong blob, and even
// a correct commit made with system git still leaves go-git's IsDirty
// reporting true.
//
// Refusing is not the same as fixing. Recording such a link correctly
// means bypassing go-git for both staging (writing the blob and index
// entry directly from os.Readlink) and status computation, which is a
// larger change than this round takes on; it is written down in DESIGN
// §6's known gaps rather than left implicit. What the refusal does buy is
// that the failure is no longer silent: fu never records content it cannot
// reproduce, and says which entry is at fault. fu itself never creates a
// symlink inside the store -- links point *into* the store from agent
// directories -- so this can only come from a hand edit (scenario 7) or,
// later, from adopt.
func (s *Store) checkNoAbsoluteSymlinks() error {
	return s.walkStoreFiles(func(storeFS fs.FS, p, rel string, d fs.DirEntry) error {
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := fs.ReadLink(storeFS, p)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(target) {
			// A relative target passes through go-billy's chroot untouched,
			// so it round-trips correctly and needs no refusal.
			return nil
		}
		displayPath := filepath.Join(s.Dir(), filepath.FromSlash(rel))
		return fmt.Errorf("%s points at %s: %w; replace it with a relative symlink or real content, "+
			"then re-run", displayPath, target, ErrAbsoluteSymlink)
	})
}

// LogEntry is one operation in `fu log` (newest first).
type LogEntry struct {
	Hash    string
	Message string
	When    time.Time
}

// IsDirty reports whether HEAD, the public index, and the worktree differ,
// including present ignored files absent from the index. It is strictly
// side-effect-free: staging for this query used to replace staged-only user
// bytes with worktree bytes before Sweep had a chance to preserve them.
func (s *Store) IsDirty() (bool, error) {
	if err := s.checkNoAbsoluteSymlinks(); err != nil {
		return false, err
	}
	changed, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		return false, err
	}
	return len(changed) != 0, nil
}

// Sweep commits pending manual edits as one "external" operation so any
// content change enters history before fu's own operation (SPEC §5.3). A
// public staged snapshot is committed first; if the worktree contains a later
// version, it receives a second external commit. This preserves both states
// instead of silently collapsing the index into the worktree.
func (s *Store) Sweep() error {
	dirty, err := s.IsDirty()
	if err != nil {
		return err
	}
	if !dirty {
		return nil
	}
	staged, err := s.preparePublicIndexSnapshot()
	if err != nil {
		return err
	}
	if len(staged.changed) != 0 {
		if _, err := s.CommitPrepared("external: manual modifications", staged); err != nil {
			return err
		}
	}
	dirty, err = s.IsDirty()
	if err != nil || !dirty {
		return err
	}
	_, err = s.Commit("external: manual modifications")
	return err
}

// Log returns up to n commits, newest first. If n is non-positive, returns
// an empty slice with no error. Returns all available commits if history
// contains fewer than n entries. Returns an error if iteration fails.
func (s *Store) Log(n int) ([]LogEntry, error) {
	iter, err := s.Repo.Log(&git.LogOptions{})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var out []LogEntry
	for len(out) < n {
		c, err := iter.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break // history exhausted
			}
			return nil, err // real error (corrupt object, I/O failure, etc.)
		}
		out = append(out, LogEntry{
			Hash:    c.Hash.String()[:7],
			Message: c.Message,
			When:    c.Author.When,
		})
	}
	return out, nil
}
