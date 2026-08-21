package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	indexformat "github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

// worktreeTarget is one non-directory entry a target tree holds. Directories
// are implied by their children, the same way the commit candidate and the
// digest projection treat them: git stores no standalone directory, so there
// is nothing for a directory entry to restore.
type worktreeTarget struct {
	Mode filemode.FileMode
	Hash plumbing.Hash
}

// targetTreePaths flattens a tree into path -> entry. FileIter skips Dir and
// Submodule and yields everything else, so regular files, executables and
// symlinks all arrive here with the mode that distinguishes them.
func targetTreePaths(tree *object.Tree) (map[string]worktreeTarget, error) {
	out := make(map[string]worktreeTarget)
	iter := tree.Files()
	defer iter.Close()
	if err := iter.ForEach(func(file *object.File) error {
		out[file.Name] = worktreeTarget{Mode: file.Mode, Hash: file.Hash}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// blobBytes reads one blob whole. Trees name content by hash, so restoring a
// path means materialising the blob it points at.
func (s *Store) blobBytes(hash plumbing.Hash) ([]byte, error) {
	blob, err := s.Repo.BlobObject(hash)
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

// worktreeMatchesTarget reports whether the worktree already holds exactly what
// the target names. It is what lets the updater leave an unchanged path alone
// rather than rewriting every tracked file and churning its mtime.
func (s *Store) worktreeMatchesTarget(name string, want worktreeTarget) (bool, error) {
	if s.worktreeFS == nil {
		return false, errUnpinnedWorktree
	}
	info, err := s.worktreeFS.Lstat(name)
	if err != nil {
		// ENOTDIR alongside ENOENT, because both mean the same thing to this
		// comparison: a name whose parent is a plain file cannot exist, so it
		// cannot match the target either. Reporting the errno instead ended the
		// whole apply on a bare "lstat ...: not a directory", one step short of
		// writeWorktreeEntry's MkdirAll -- which recognises the identical
		// collision and refuses it by name. os.IsNotExist does not cover
		// ENOTDIR (unlike ENOENT it is false for a PathError wrapping it), so
		// the two have to be spelled out separately.
		if os.IsNotExist(err) || isNotADirectory(err) {
			return false, nil
		}
		return false, err
	}
	// Read inside each arm rather than ahead of the switch: a path whose type
	// no longer matches the target's mode is decided by the type alone, and
	// hoisting this read made every such path pay for a whole blob nothing
	// went on to compare against.
	switch want.Mode {
	case filemode.Symlink:
		if info.Mode()&os.ModeSymlink == 0 {
			return false, nil
		}
		data, err := s.blobBytes(want.Hash)
		if err != nil {
			return false, err
		}
		target, err := s.worktreeFS.Readlink(name)
		if err != nil {
			return false, err
		}
		return target == string(data), nil
	case filemode.Regular, filemode.Executable:
		if !info.Mode().IsRegular() {
			return false, nil
		}
		// Git's own executable bit is the owner's alone: go-git's
		// isSetUserExecutable (filemode.go:109) and this codebase's own
		// skill/digest.go:114 and :233 both test m&0100, never group or
		// other. A 0o111 mask would call a file executable whenever any of
		// the three bits was set, so a file with owner-exec unset but
		// group- or other-exec set would silently match an Executable
		// target that git itself considers different.
		if (want.Mode == filemode.Executable) != (info.Mode().Perm()&0o100 != 0) {
			return false, nil
		}
		data, err := s.blobBytes(want.Hash)
		if err != nil {
			return false, err
		}
		// Size settles the common mismatch without reading the worktree file
		// at all. A blob's length is exactly the content length git records,
		// so a differing size is proof of difference; an equal size proves
		// nothing and falls through to the byte comparison.
		if info.Size() != int64(len(data)) {
			return false, nil
		}
		have, err := s.readWorktreeFileWhole(name)
		if err != nil {
			return false, err
		}
		return bytes.Equal(have, data), nil
	default:
		return false, fmt.Errorf("unsupported target mode %s for %q", want.Mode, name)
	}
}

func (s *Store) readWorktreeFileWhole(name string) ([]byte, error) {
	f, err := s.worktreeFS.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

// errUnpinnedWorktree refuses every worktree mutation outside a write session.
// Precondition 4 of DESIGN §6: these writes must run on the session's pinned
// descriptors, never on a PlainOpen worktree re-resolved by pathname.
var errUnpinnedWorktree = errors.New("store is not attached to a checked write session")

// checkTargetNoAbsoluteSymlinks refuses a target tree carrying a symlink whose
// recorded content is an absolute path, before the updater writes anything.
//
// It is checkNoAbsoluteSymlinks's counterpart for the destination rather than
// the source, and it exists because the two ends are reached differently: the
// worktree can only acquire such a link from outside fu, while a target tree
// can hold one that fu itself is about to materialise. Writing it and letting
// the following commit refuse it is the worst of both -- the store ends up
// holding exactly the thing every write command rejects, so nothing fu offers
// can undo it.
//
// Only Symlink entries cost a blob read; every other mode is skipped on the
// mode alone. Names are reported the way checkNoAbsoluteSymlinks reports them,
// so a user who has seen one refusal recognises the other.
func (s *Store) checkTargetNoAbsoluteSymlinks(target map[string]worktreeTarget) error {
	names := make([]string, 0, len(target))
	for name, want := range target {
		if want.Mode == filemode.Symlink {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := s.blobBytes(target[name].Hash)
		if err != nil {
			return err
		}
		if !path.IsAbs(string(data)) {
			continue
		}
		displayPath := filepath.Join(s.Dir(), filepath.FromSlash(name))
		return fmt.Errorf("the target tree records %s pointing at %s: %w; fu will not write it, "+
			"so the store is left as it was", displayPath, data, ErrAbsoluteSymlink)
	}
	return nil
}

// applyTreeToWorktree makes every path the target tree or the index names hold
// exactly what the target says, and returns the paths it actually changed.
//
// The path set is union(index, target) and nothing else, computed entirely in
// memory from two sources that are already in hand. That is what keeps
// untracked and ignored *files* out of reach: their names are in neither
// source, so no write and no unlink of a file here can ever name one, and
// DESIGN §6's "archive untracked before hard reset" requirement has no
// subject. That requirement exists because go-git's Worktree.Reset enumerates
// the worktree and deletes what it finds there; this updater decides what to
// touch without consulting the worktree at all.
//
// The worktree is enumerated exactly once, by the checkNoAbsoluteSymlinks
// precondition below (walkStoreFiles, git.go), and that pass is read-only: it
// reads link targets to decide whether to refuse, and not one of the names it
// visits flows into the change set computed afterwards.
//
// The path-set argument does not cover directories, and on its own it is
// therefore not the safety argument. pruneEmptiedParents below issues Remove
// on ancestor *directory* names, and git indexes no directory, so those names
// are in neither the index nor the target: this updater demonstrably names and
// unlinks paths outside union(index, target). What actually protects untracked
// content there is a different invariant, and it belongs beside this one --
// pruning never lists a directory and then decides; it attempts the rmdir and
// reads the result, taking ENOTEMPTY as its stop condition. Emptiness is thus
// proven atomically at the syscall, so any untracked content inside a
// directory blocks its removal with no window to race.
//
// Both have to be stated, because a change that made pruning recursive would
// satisfy every rule written down here -- the path set would still be
// union(index, target) -- and destroy untracked content anyway.
//
// The set is complete for both callers without a third source: restore's
// target is HEAD, so HEAD is a subset of it, and revert sweeps first, so the
// index equals HEAD.
func (s *Store) applyTreeToWorktree(target map[string]worktreeTarget) (changed []string, err error) {
	if s.worktreeFS == nil {
		return nil, errUnpinnedWorktree
	}
	// Precondition 5 of DESIGN §6, and it must stay a precondition: an
	// escaping link is refused before a single path is touched, so a store
	// carrying one is left exactly as it was found.
	if err := s.checkNoAbsoluteSymlinks(); err != nil {
		return nil, err
	}
	// The other half of that precondition, and the one it was missing: the
	// check above validates where the worktree is now, this one validates
	// where it is being taken. A commit holding an absolute symlink is
	// reachable through the direct-Git path DESIGN treats as supported, and
	// without this the updater wrote such a link itself and then had the very
	// next commit refuse it -- wedging every write command behind a link fu
	// had just created, with only a manual rm to break out. Refusing here
	// costs one blob read per symlink entry and turns that wedge into a clean
	// rejection that leaves the store untouched.
	if err := s.checkTargetNoAbsoluteSymlinks(target); err != nil {
		return nil, err
	}
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(target)+len(idx.Entries))
	for name := range target {
		names[name] = struct{}{}
	}
	for _, entry := range idx.Entries {
		names[entry.Name] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	// Real git rejects a tree or index entry under .git outright, and so must
	// this: worktreeFS has no .git mount, so such a name would be written into
	// -- or deleted from -- the store's own git directory. Nothing fu writes
	// produces one today, but this is a new writer on a repository the design
	// explicitly says a user may drive with git directly, and the check is one
	// comparison per path.
	for _, name := range ordered {
		if first, _, _ := strings.Cut(name, "/"); first == ".git" {
			return nil, fmt.Errorf("refusing to apply %q: a tracked path may not live under .git", name)
		}
	}

	// Sorted on the way out, including the partial slice an error returns:
	// Store.Revert documents the paths it hands back as sorted, and the two
	// passes below walk the path set in opposite directions, so neither one
	// produces that order on its own. A deferred sort on the named result is
	// what keeps that true at every return below without repeating it at each.
	defer func() { sort.Strings(changed) }()

	// Deletions first, deepest name first -- and both halves of that are load
	// bearing, because a path's *type* can differ between the worktree and the
	// target and neither pass can fix that alone.
	//
	// Interleaving them in sorted order could not converge a tracked directory
	// back into the target's file: "skills/alpha" sorts before its own child
	// "skills/alpha/SKILL.md", so the write was attempted while the tracked
	// children that made the directory non-empty were still in it, and the
	// removal underneath writeWorktreeEntry refused with ENOTEMPTY. Real `git
	// reset --hard` converges on that state, and it is reachable through the
	// direct-Git path DESIGN treats as supported, so refusing it left `fu
	// restore --hard` and `fu revert` unable to repair a store git itself can
	// -- with a message ("an unexpected directory") describing the untracked
	// case rather than this one.
	//
	// Reverse order within the deletion pass is the other half. go-git's index
	// really can hold both "skills/dir" and "skills/dir/f.txt" at once (real
	// git rejects the pair, go-git does not), and forward order reaches the
	// directory's own name while its tracked child is still inside it. Deepest
	// first unlinks the child, lets pruneEmptiedParents take the directory, and
	// finds nothing left at the name by the time it arrives.
	//
	// None of this widens what may be deleted. Every name either pass touches
	// still comes from union(index, target), and pruning still proves emptiness
	// at the rmdir rather than by listing, so untracked content inside a
	// directory blocks its removal exactly as before -- which is why the
	// untracked-blocker case still refuses instead of converging.
	targetDirs := targetDirectories(target)
	for i := len(ordered) - 1; i >= 0; i-- {
		name := ordered[i]
		if _, wanted := target[name]; wanted {
			continue
		}
		// A name the target only names as a *directory* is not a deletion at
		// all, however the index describes it. The index can carry a stale file
		// blob at such a name -- the path held a plain file when it was staged,
		// and a directory replaced it afterwards -- and taking that entry at
		// face value made the pass try to unlink a directory the target itself
		// wants, refusing with ENOTEMPTY over the untracked content inside it.
		// Real `git reset --hard` converges here: it drops the stale entry,
		// writes the target's files into the existing directory and keeps the
		// untracked content, warning "unable to unlink" as it goes.
		//
		// Nothing needs unlinking to make that happen. The stale entry lives
		// only in the index, and rebuildIndexFromTarget below rewrites the index
		// from the target outright, so declining here is what drops it.
		//
		// The type test is not optional. When a plain *file* occupies the name
		// instead, it has to go, or the write pass has no directory to create
		// beneath it -- so only an actual directory is left standing, which is
		// also the only shape that could have raised the refusal. Lstat rather
		// than Stat: a symlink is not the directory the target asked for, and
		// removing it is what lets the write pass put a real one there.
		//
		// This only ever declines to delete. The untracked-blocker refusal is
		// untouched for the case it was written about, where the target wants a
		// file at the occupied name and the directory really is in the way.
		if _, wantsDirHere := targetDirs[name]; wantsDirHere {
			info, statErr := s.worktreeFS.Lstat(name)
			if statErr == nil && info.IsDir() {
				continue
			}
		}
		removed, err := s.removeWorktreeEntry(name)
		if err != nil {
			return changed, err
		}
		if removed {
			changed = append(changed, name)
			if err := s.pruneEmptiedParents(name); err != nil {
				return changed, err
			}
		}
	}
	for _, name := range ordered {
		want, wanted := target[name]
		if !wanted {
			continue
		}
		matched, err := s.worktreeMatchesTarget(name, want)
		if err != nil {
			return changed, err
		}
		if matched {
			continue
		}
		if err := s.writeWorktreeEntry(name, want); err != nil {
			return changed, err
		}
		changed = append(changed, name)
	}
	// Skipped when nothing moved and the index already describes the target.
	// Rebuilding unconditionally made a --hard over a worktree dirty only with
	// untracked content take .git/index.lock and rewrite the index for no
	// reason -- and fail outright when another git process held the lock,
	// despite having no work to do.
	if len(changed) != 0 || !s.indexMatchesTarget(idx, target) {
		if err := s.rebuildIndexFromTarget(target); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// targetDirectories names every directory a target tree implies, derived from
// the target's own keys: "a/b/c.txt" implies "a" and "a/b".
//
// Git stores no standalone directory entry, so this is the only way to ask
// whether a target wants a directory at some name -- targetTreePaths flattens
// Dir entries away, exactly as the commit candidate and the digest projection
// do. The walk stops climbing the first time it meets a prefix already in the
// set, so the whole map costs one pass over the target rather than one per
// ancestor.
func targetDirectories(target map[string]worktreeTarget) map[string]struct{} {
	dirs := make(map[string]struct{})
	for name := range target {
		for dir := path.Dir(name); dir != "." && dir != ""; dir = path.Dir(dir) {
			if _, seen := dirs[dir]; seen {
				break
			}
			dirs[dir] = struct{}{}
		}
	}
	return dirs
}

// indexMatchesTarget reports whether the public index already names exactly the
// target's paths, modes and blob hashes. Stat fields are not compared: they are
// a cache go-git falls back from, not part of what the index is required to
// say.
func (s *Store) indexMatchesTarget(idx *indexformat.Index, target map[string]worktreeTarget) bool {
	if len(idx.Entries) != len(target) {
		return false
	}
	for _, entry := range idx.Entries {
		want, ok := target[entry.Name]
		if !ok || entry.Hash != want.Hash || entry.Mode != want.Mode {
			return false
		}
	}
	return true
}

// ResetWorktreeToHead makes every tracked path match HEAD. It is `git reset
// --hard` with no commit argument: the index and worktree move, the branch
// reference does not. Untracked and ignored content is left alone, for the
// same reason applyTreeToWorktree above never learns their names -- the path
// set it walks is union(index, target), and HEAD's own tree is what supplies
// target here.
//
// This is the entry point `fu restore --hard` calls; the confirmation and the
// `--hard` gate that gets a caller here belong to that command layer, not to
// this method -- once called, it simply performs the reset.
//
// There is no WAL guarding this operation and none is needed. Every other
// mutator in this store (recovery, staged writes) needs one because it moves
// through states that are not individually safe to repeat -- a create
// followed by a publish is not idempotent if replayed from the wrong point.
// A worktree reset has no such intermediate state: applyTreeToWorktree
// compares each path against the target and only touches the paths that
// still differ, so calling this again after a crash, a partial run, or
// simply because the caller is unsure whether the first call finished is
// always safe and converges to the same result. The second call in
// TestResetWorktreeToHeadIsIdempotent finds nothing left to do and reports an
// empty slice, not an error.
func (s *Store) ResetWorktreeToHead() ([]string, error) {
	target, err := s.headTargets()
	if err != nil {
		return nil, err
	}
	return s.applyTreeToWorktree(target)
}

// SplitResettablePaths divides reported store paths into the ones
// ResetWorktreeToHead would actually put back and the ones it never touches.
//
// The dividing line is the updater's own path set, union(index, HEAD): a path
// in neither is untracked or ignored, and by construction no write or unlink
// in applyTreeToWorktree can name it. Callers need the distinction because the
// two halves have different remedies and only one of them is `--hard`. Telling
// a user to discard an untracked file with `--hard` produces a loop that
// cannot terminate -- the flag resets nothing, the file is still there, and
// the next run prints the same advice.
//
// Both slices come back in the order they arrived, which is the sorted order
// ChangedPathsIncludingIgnored produces.
func (s *Store) SplitResettablePaths(paths []string) (resettable, left []string, err error) {
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		return nil, nil, err
	}
	known := make(map[string]struct{}, len(idx.Entries))
	for _, entry := range idx.Entries {
		known[entry.Name] = struct{}{}
	}
	// HEAD as well as the index: `--hard` converges on HEAD's tree, so a path
	// HEAD names is one the reset restores even when the index has dropped it.
	head, err := s.headTargets()
	if err != nil {
		// A store with no commit yet has no HEAD tree, and every path in it is
		// then untracked as far as a reset is concerned. That is not a failure
		// to report, it is the answer.
		if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, nil, err
		}
	}
	for name := range head {
		known[name] = struct{}{}
	}
	for _, p := range paths {
		if _, ok := known[p]; ok {
			resettable = append(resettable, p)
			continue
		}
		left = append(left, p)
	}
	return resettable, left, nil
}

// headTargets flattens HEAD's tree the same way headTargets in
// worktree_apply_test.go does for tests: through the current branch tip's
// commit, not the index or the worktree, since HEAD is the destination
// ResetWorktreeToHead is defined to converge on.
func (s *Store) headTargets() (map[string]worktreeTarget, error) {
	head, err := s.Repo.Head()
	if err != nil {
		return nil, err
	}
	commit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	return targetTreePaths(tree)
}

// rebuildIndexFromTarget replaces the public index with exactly the target
// tree. It runs under .git/index.lock (precondition 3 of DESIGN §6) because
// the index is public and the supported direct-Git path may write it, and
// holding the lock is what serialises this rewrite against that writer.
//
// It is a whole-index overwrite, not a compare-and-swap: nothing here reads
// the previous index or compares against it. That is correct for what this
// implements -- `git reset --hard` discards the index outright -- but the
// distinction is worth stating, because a caller who read this as a CAS would
// expect a concurrent index write to be detected, and it is not; it is
// overwritten. The lock bounds the window, it does not report on it.
//
// Version 2 is written unconditionally for the same reason: the target fully
// determines the result, so nothing from a v3 or v4 index survives. A store
// whose index some other git wrote at a later version is quietly written back
// at v2, losing that version's extensions -- harmless, since git rewrites the
// index freely, but not a preservation this function performs.
//
// Stat fields are filled from the worktree the updater has just written, and
// that is a performance choice rather than a correctness one -- said plainly,
// because the claim here used to be the opposite and named a test as its
// evidence. go-git's metadataMatches compares Size, ModifiedAt and Mode, so an
// index carrying zeroes does make every tracked path look modified; it then
// falls back to hashing the content, finds the paths unchanged, and reports the
// store clean anyway. Leaving both fields at zero keeps the whole
// internal/store suite green, TestApplyTreeToWorktreeLeavesTheStoreClean
// included. What filling them buys is that the fallback is not taken, which is
// also what real git writes into its index.
func (s *Store) rebuildIndexFromTarget(target map[string]worktreeTarget) error {
	names := make([]string, 0, len(target))
	for name := range target {
		names = append(names, name)
	}
	sort.Strings(names)
	return s.withIndexLock(func() error {
		idx := &indexformat.Index{Version: 2}
		for _, name := range names {
			want := target[name]
			entry := &indexformat.Entry{Name: name, Hash: want.Hash, Mode: want.Mode}
			info, err := s.worktreeFS.Lstat(name)
			switch {
			case err == nil:
				entry.Size = uint32(info.Size())
				entry.ModifiedAt = info.ModTime()
			case os.IsNotExist(err):
				// The updater just wrote this path, so its absence means
				// something removed it in between. Degrading to a zero stat is
				// safe -- the metadata mismatch only makes go-git rehash -- and
				// is what keeps a racing external delete from failing a reset
				// that has otherwise fully converged.
			default:
				// Anything else (a permission change, an I/O error) is not a
				// race this function should absorb into a silently degraded
				// index entry.
				return fmt.Errorf("stat %q while rebuilding the index: %w", name, err)
			}
			idx.Entries = append(idx.Entries, entry)
		}
		// Through this package's own atomic writer, never Storer.SetIndex.
		// That one is fs.Create(".git/index") -- an in-place O_TRUNC -- and
		// git's readers take no lock, so it lets a concurrent `git status`
		// observe a truncated index and lets a crash leave one behind
		// permanently, together with a stale lock that makes every later fu
		// command fail in Status() with a decode error naming the lock rather
		// than the index. writePublicIndexAtomically (git.go) exists for
		// exactly this and documents it at length; calling SetIndex here
		// reintroduced the defect it was written to close.
		//
		// It matters more here than anywhere else: this updater deliberately
		// has no WAL, and every recovery claim it makes rests on re-running
		// converging. That holds for a returned error and not for a torn
		// write -- and `--hard` is the command a worried user interrupts.
		//
		// Safe to call while .git/index.lock is held: the writer stages under
		// its own .fu-index- name precisely so it does not consume the lock by
		// renaming it.
		return s.writePublicIndexAtomically(idx)
	})
}

// writeWorktreeEntry materialises one target entry, replacing whatever is at
// the name. The existing object is removed first because a path may currently
// hold the wrong type entirely -- a directory where the target has a file, or
// a symlink where it has a regular file -- and an open-for-write would either
// fail or follow the link out of the store.
func (s *Store) writeWorktreeEntry(name string, want worktreeTarget) error {
	data, err := s.blobBytes(want.Hash)
	if err != nil {
		return err
	}
	if dir := path.Dir(name); dir != "." && dir != "" {
		if err := s.worktreeFS.MkdirAll(dir, 0o755); err != nil {
			// A plain file occupying dir (or one of its own ancestors) is the
			// mirror image of removeWorktreeEntry's directory-not-empty case
			// below: a prior, unrelated write left this path as a file where
			// the target now needs a directory. MkdirAll's own failure is a
			// bare ENOTDIR PathError naming whichever path component it tried
			// to open as a directory, not necessarily name itself -- not
			// obviously wrong, but not obviously this either. Refusing is
			// correct either way -- this updater never deletes what it
			// cannot prove it owns, and the occupying file is exactly that --
			// but the caller deserves the same named, explanatory refusal
			// removeWorktreeEntry already gives the opposite shape of this
			// collision.
			if isNotADirectory(err) {
				return fmt.Errorf("refusing to write %q: %s is occupied by a file, not a directory: %w", name, dir, err)
			}
			return err
		}
	}
	if _, err := s.removeWorktreeEntry(name); err != nil {
		return err
	}
	if want.Mode == filemode.Symlink {
		// Defence in depth behind checkTargetNoAbsoluteSymlinks, which has
		// already refused this whole apply before any path was touched. This
		// guard is what makes the property local: it holds for any future
		// caller that reaches writeWorktreeEntry by another route, without
		// that caller having to know the pre-flight exists.
		if path.IsAbs(string(data)) {
			return fmt.Errorf("refusing to write %q as a symlink to %s: %w", name, data, ErrAbsoluteSymlink)
		}
		return s.worktreeFS.Symlink(string(data), name)
	}
	perm := os.FileMode(0o644)
	if want.Mode == filemode.Executable {
		perm = 0o755
	}
	f, err := s.worktreeFS.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// removeWorktreeEntry drops one path and reports whether anything was there.
func (s *Store) removeWorktreeEntry(name string) (bool, error) {
	if _, err := s.worktreeFS.Lstat(name); err != nil {
		// ENOTDIR counts as absent for the same reason it does in
		// worktreeMatchesTarget: a plain file occupying one of name's parents
		// makes name unable to exist, so there is nothing here to remove and
		// nothing to report. Answering the errno instead failed the whole apply
		// on a state real `git reset --hard` converges on -- it simply drops
		// such an entry from the index -- and did it with a bare PathError.
		//
		// Nothing is deleted on this branch, so the rule that this updater never
		// removes what it cannot prove it owns is untouched: the occupying file
		// is left exactly as it was found, and if it is itself a tracked path
		// the target no longer wants, the deletion pass reaches it under its own
		// name.
		if os.IsNotExist(err) || isNotADirectory(err) {
			return false, nil
		}
		return false, err
	}
	if err := s.worktreeFS.Remove(name); err != nil {
		// A directory occupying name is the one case worth a better message
		// than the raw errno: it happens whenever a target file's path was
		// replaced by a directory out from under fu (writeWorktreeEntry hits
		// this before it can write the target's regular file, and the
		// deletion branch of applyTreeToWorktree hits it when a tracked path
		// was replaced the same way). Refusing is correct either way -- this
		// updater never deletes what it cannot prove it owns, and a
		// directory's contents are exactly that -- but "unlinkat foo:
		// directory not empty" does not tell a caller that, only that some
		// syscall somewhere failed. Every other Remove failure (permissions,
		// I/O) passes through unchanged.
		if isDirectoryNotEmpty(err) {
			return false, fmt.Errorf("refusing to remove %q: an unexpected directory occupies this path and is not empty: %w", name, err)
		}
		// The same collision one level up: a plain file occupying an ancestor
		// of name makes the removal fail with ENOTDIR from whichever component
		// was opened with O_DIRECTORY. writeWorktreeEntry already turns this
		// into a named refusal (isNotADirectory, below) and the deletion path
		// was simply never given the matching arm, so the identical situation
		// surfaced as a bare PathError depending on which side hit it first.
		if isNotADirectory(err) {
			return false, fmt.Errorf("refusing to remove %q: a file occupies one of its parent directories: %w", name, err)
		}
		return false, err
	}
	return true, nil
}

// isDirectoryNotEmpty reports whether err is a Remove failure caused by a
// directory that still has entries in it. It is Darwin's and Linux's shared
// signal for that (both surface ENOTEMPTY once rootFilesystem.Remove has
// already retried the initial EISDIR/EPERM as AT_REMOVEDIR; see the comment
// there), and it means two different things depending on who is asking:
// removeWorktreeEntry above turns it into a named, explanatory refusal, and
// pruneEmptiedParents below reads the identical failure as proof a directory
// still holds real content and stops quietly instead of erroring.
func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, unix.ENOTEMPTY)
}

// isNotADirectory reports whether err was caused by a plain file occupying a
// path component where a directory was required -- the ENOTDIR that openat
// surfaces (reopenDirNoFollow, rootfs.go) when it opens an existing,
// wrong-type entry with O_DIRECTORY.
//
// Like isDirectoryNotEmpty above, the same errno means two different things
// depending on who is asking, and both readings are deliberate. To
// writeWorktreeEntry's MkdirAll and pruneEmptiedParents it is a collision to
// refuse by name, the counterpart to a leftover directory's ENOTEMPTY. To the
// two Lstats -- worktreeMatchesTarget's and removeWorktreeEntry's -- it means
// the path simply does not exist, because a name under a plain file cannot;
// they read it as absence and converge rather than reporting an errno.
func isNotADirectory(err error) bool {
	return errors.Is(err, unix.ENOTDIR)
}

// pruneEmptiedParents removes the ancestor directories that the deletion of
// name just left empty, walking upward one level at a time and stopping the
// first time a level still has something in it.
//
// This is not tidiness for its own sake: measured against real git, `git
// reset --hard` takes an emptied directory with it (a repo whose HEAD lacks
// nested/deep/f.txt, with that file created and staged, ends up with neither
// nested/ nor nested/deep/ after a hard reset), and applyTreeToWorktree's own
// baseline is exactly that measured behaviour. Without this, a deletion here
// would leave the directory behind, and because git tracks no empty
// directory, that leftover is invisible to ChangedPathsIncludingIgnored and
// every later status the store reports -- nothing would ever name it, let
// alone remove it. It would simply accumulate as silent litter in the store
// worktree for as long as the store exists.
//
// The walk stops at a pinned logical root: the store root itself, or a name
// rootFilesystem.Mount installed -- "skills" being the only one the worktree
// has (store.go's openRepositoryForRoots). Such a name is not an ordinary
// directory that happens to be tracked empty. The store's own bootstrap
// creates it and always expects it to exist, and resolving a bare mount name
// back through rootFilesystem.resolve does not behave like an ordinary path:
// it targets "." inside the mounted root itself, so the Remove would not even
// mean what it reads as. Stopping there keeps this a content cleanup, never a
// structural one.
//
// The stop asks isLogicalRoot rather than testing for a store-root child
// (review round 26 finding 3). Those are not the same set, and the difference
// is a real leak: `path.Dir(dir) != "."` spared *every* top-level directory,
// so a user-tracked "misc/notes.txt" -- reachable through the direct-Git path
// the design supports -- left an empty "misc/" behind that git takes with a
// hard reset. Exactly the silent litter the paragraph above describes, one
// level shallower than it imagined. staging/ and recovery/ raise no such
// question here: both are siblings of the repo directory, outside the worktree
// entirely (store.go's StagingDir, RecoveryDir), so no tracked path can lead
// the walk to them.
//
// worktreeFS exposes no "list then decide" primitive that would not itself
// race a concurrent writer between the check and the remove, so emptiness is
// proven exactly the way rootFilesystem.Remove already proves it when a path
// turns out to hold a directory instead of a file: attempt the removal and
// read the result. isDirectoryNotEmpty means real content is still there and
// is the stop condition, not an error -- including the ordinary case where a
// sibling path the target still wants sorts after this one in
// applyTreeToWorktree's alphabetical walk and has not been written yet; when
// that later write runs, writeWorktreeEntry's MkdirAll recreates whatever
// directory pruning removed out from under it, so an early prune here costs
// at most a redundant mkdir, never a wrong result.
func (s *Store) pruneEmptiedParents(name string) error {
	for dir := path.Dir(name); dir != "." && !s.worktreeFS.isLogicalRoot(dir); dir = path.Dir(dir) {
		if err := s.worktreeFS.Remove(dir); err != nil {
			switch {
			case isDirectoryNotEmpty(err):
				return nil
			case os.IsNotExist(err):
				// Already gone -- an external delete raced this walk. That is
				// the outcome pruning wanted, so it is not a failure, and
				// rebuildIndexFromTarget deliberately absorbs the identical
				// race rather than failing a reset that has otherwise fully
				// converged. Keep walking upward: the parent may still be
				// prunable.
				continue
			case isNotADirectory(err):
				return fmt.Errorf("refusing to prune %q: a file occupies one of its parent directories: %w", dir, err)
			}
			return err
		}
	}
	return nil
}
