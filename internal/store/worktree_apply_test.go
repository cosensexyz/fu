package store

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// applyFixture returns a write session on a store whose HEAD holds one
// regular file, one executable and one symlink, so every mode the updater
// supports is present before a single test touches it.
//
// The commit message is a real operation verb, not a "test: ..." label, and
// has to stay one: resolveOperationsBack counts operations by SPEC §5.3's verb
// whitelist (IsOperationMessage), so a fixture commit that does not look like
// an operation is invisible to every revert test built on it.
func applyFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plain.txt", filepath.Join(s.SkillsDir(), "link")); err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := session.Store.Commit("new: fixture"); err != nil {
		t.Fatal(err)
	}
	return session.Store
}

func headTargets(t *testing.T, s *Store) map[string]worktreeTarget {
	t.Helper()
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := targetTreePaths(tree)
	if err != nil {
		t.Fatal(err)
	}
	return targets
}

// TestTargetTreePathsCarriesEveryNonDirectoryMode pins that the flattening
// keeps symlinks and the executable bit. go-git's FileIter skips only Dir and
// Submodule, so a symlink reaching the updater as a regular file would be
// written as a file containing its own target text.
func TestTargetTreePathsCarriesEveryNonDirectoryMode(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)

	for name, wantMode := range map[string]string{
		// filemode.FileMode.String() zero-pads to 7 octal digits (its own doc
		// comment gives "0100644" as the example for Regular), not the 6-digit
		// form `git ls-tree` prints, so the leading zero belongs here too.
		"skills/plain.txt": "0100644",
		"skills/run.sh":    "0100755",
		"skills/link":      "0120000",
	} {
		got, ok := targets[name]
		if !ok {
			t.Fatalf("target tree is missing %q; got %v", name, targets)
		}
		if got.Mode.String() != wantMode {
			t.Fatalf("%q mode = %s, want %s", name, got.Mode, wantMode)
		}
	}
}

// TestWorktreeMatchesTargetSeesEachKindOfDivergence pins the comparison the
// updater skips unchanged paths with. Reporting "matches" for a diverged path
// leaves it un-restored; reporting "differs" for an identical one rewrites a
// file that did not need it and churns its mtime for nothing.
func TestWorktreeMatchesTargetSeesEachKindOfDivergence(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)

	for _, name := range []string{"skills/plain.txt", "skills/run.sh", "skills/link"} {
		matched, err := s.worktreeMatchesTarget(name, targets[name])
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			t.Fatalf("%q must match the tree it was just committed from", name)
		}
	}

	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	matched, err := s.worktreeMatchesTarget("skills/plain.txt", targets["skills/plain.txt"])
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("edited content must not match")
	}

	if err := os.Remove(filepath.Join(s.SkillsDir(), "link")); err != nil {
		t.Fatal(err)
	}
	matched, err = s.worktreeMatchesTarget("skills/link", targets["skills/link"])
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("an absent path must not match")
	}
}

// TestWorktreeMatchesTargetJudgesTheExecutableBitTheWayGitDoes pins the mask.
// git, go-git and skill/digest.go all read the owner execute bit alone, so a
// file carrying only a group execute bit is a plain file to every one of them.
// Reading any of the three bits instead reports such a file as matching an
// executable target, which is a silent wrong answer from the one predicate
// this file exists to give.
func TestWorktreeMatchesTargetJudgesTheExecutableBitTheWayGitDoes(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	// run.sh is committed as an executable (0100755). Drop the owner exec
	// bit and leave only the group exec bit set: git calls that a plain
	// file, but a mask of 0o111 would still call it executable.
	if err := os.Chmod(filepath.Join(s.SkillsDir(), "run.sh"), 0o654); err != nil {
		t.Fatal(err)
	}
	matched, err := s.worktreeMatchesTarget("skills/run.sh", targets["skills/run.sh"])
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("a file with only a group execute bit is not executable to git")
	}
}

// TestApplyTreeToWorktreeFollowsGitResetHardRules pins the one rule the whole
// batch rests on, measured against real git: paths in index or HEAD are reset
// to the target, and paths in neither are never touched. Design §8 names four
// classes -- modified, untracked, ignored, and staged-but-not-in-HEAD -- and
// this test drives three of them (the fourth has its own test,
// TestApplyTreeToWorktreeDeletesPathsAbsentFromTheTarget below).
//
// Ignored is deliberately not a fourth fixture here, and its absence is the
// point rather than a gap: fu's stageAll commits gitignored content on
// purpose, so an ignored file in a converged store is *tracked*, and an
// ignored file that is not tracked is untracked -- indistinguishable, to this
// updater, from the scratch.md fixture below, because the path set it walks is
// union(index, target) and consults no ignore rules at all. Adding an ignored
// fixture would assert the same fact twice under a different name.
func TestApplyTreeToWorktreeFollowsGitResetHardRules(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	skills := s.SkillsDir()

	// modified: must come back
	if err := os.WriteFile(filepath.Join(skills, "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	// untracked: must survive untouched
	if err := os.WriteFile(filepath.Join(skills, "scratch.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	// deleted tracked file: must come back
	if err := os.Remove(filepath.Join(skills, "run.sh")); err != nil {
		t.Fatal(err)
	}

	changed, err := s.applyTreeToWorktree(targets)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(skills, "plain.txt"))
	if err != nil || string(got) != "v1" {
		t.Fatalf("modified tracked file must be reset: %q %v", got, err)
	}
	survived, err := os.ReadFile(filepath.Join(skills, "scratch.md"))
	if err != nil || string(survived) != "mine" {
		t.Fatalf("untracked file must survive untouched: %q %v", survived, err)
	}
	restored, err := os.Stat(filepath.Join(skills, "run.sh"))
	if err != nil {
		t.Fatalf("deleted tracked file must be restored: %v", err)
	}
	if restored.Mode().Perm()&0o111 == 0 {
		t.Fatalf("restored file must keep its executable bit, got %v", restored.Mode())
	}
	for _, name := range changed {
		if name == "skills/scratch.md" {
			t.Fatal("an untracked path must never appear in the changed set")
		}
	}
}

// TestApplyTreeToWorktreeDeletesPathsAbsentFromTheTarget pins the staged-new
// case: a path the index tracks but the target tree does not hold is deleted,
// exactly as `git reset --hard` deletes a staged-but-uncommitted file.
func TestApplyTreeToWorktreeDeletesPathsAbsentFromTheTarget(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	staged := filepath.Join(s.SkillsDir(), "staged-new.txt")
	if err := os.WriteFile(staged, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Put it in the index without committing, which is what makes it tracked.
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/staged-new.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.applyTreeToWorktree(targets); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("a path tracked in the index but absent from the target must be deleted, err=%v", err)
	}
}

// TestApplyTreeToWorktreeRemovesDirectoriesItEmptied pins measured git
// behaviour: `git reset --hard` takes the directories its own deletions
// emptied with it. Leaving them behind is worse than untidy -- git tracks no
// empty directory, so the leftover is invisible to every status the store can
// report and nothing later collects it.
func TestApplyTreeToWorktreeRemovesDirectoriesItEmptied(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	nested := filepath.Join(s.SkillsDir(), "nested", "deep", "f.txt")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Put it in the index without committing, which is what makes it tracked
	// -- the same setup TestApplyTreeToWorktreeDeletesPathsAbsentFromTheTarget
	// already exercises successfully on this pinned filesystem.
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/nested/deep/f.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.applyTreeToWorktree(targets); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(nested); !os.IsNotExist(err) {
		t.Fatalf("staged file must be deleted, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Dir(nested)); !os.IsNotExist(err) {
		t.Fatalf("directory %q emptied by the deletion must be removed with it, err=%v", filepath.Dir(nested), err)
	}
	if _, err := os.Lstat(filepath.Dir(filepath.Dir(nested))); !os.IsNotExist(err) {
		t.Fatalf("directory %q emptied by the deletion must be removed with it, err=%v", filepath.Dir(filepath.Dir(nested)), err)
	}
	// The pruning walk must stop at the store root: skills/ is a separately
	// mounted checked root the store's own bootstrap creates and always
	// expects to exist, not an ordinary directory that happens to be tracked
	// empty.
	if _, err := os.Lstat(s.SkillsDir()); err != nil {
		t.Fatalf("the skills root itself must survive pruning: %v", err)
	}
}

// TestApplyTreeToWorktreeRefusesWhenADirectoryBlocksAFile pins the message a
// caller gets when an unexpected directory occupies a path the target wants
// as a regular file. Deleting a user's directory tree to make way for it
// would be exactly the kind of destruction this updater exists to avoid (it
// never removes what it cannot prove it owns), so it must refuse -- but the
// refusal has to name the path and say a directory is in the way, not
// surface a bare ENOTEMPTY/EISDIR that gives no hint what happened.
func TestApplyTreeToWorktreeRefusesWhenADirectoryBlocksAFile(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)

	// skills/plain.txt is a committed regular-file target; replace it with a
	// non-empty directory so the updater must refuse rather than delete it.
	blocked := filepath.Join(s.SkillsDir(), "plain.txt")
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "stray.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := s.applyTreeToWorktree(targets)
	if err == nil {
		t.Fatal("applying over a non-empty directory must fail, not silently delete it")
	}
	// A bare os.PathError ("unlinkat skills/plain.txt: directory not empty")
	// already happens to contain the path and the word "directory", so
	// asserting on those alone would pass without the fix. "unexpected
	// directory" is the phrase the wrap adds; it is what proves the error was
	// actually rewritten to say what is wrong, not just coincidentally
	// contains the right substrings.
	if !strings.Contains(err.Error(), "skills/plain.txt") || !strings.Contains(err.Error(), "unexpected directory") {
		t.Fatalf("error must name the path and say an unexpected directory is in the way, got: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(blocked, "stray.txt")); statErr != nil {
		t.Fatalf("the blocking directory's content must survive a refusal: %v", statErr)
	}
}

// TestWriteWorktreeEntryRefusesWhenAFileBlocksADirectory pins the mirror
// image of TestApplyTreeToWorktreeRefusesWhenADirectoryBlocksAFile above
// (review round finding 4): there, an unexpected directory occupies a path
// the target wants as a file; here, an unexpected file occupies a path
// segment the target needs as a directory, so writeWorktreeEntry's own
// MkdirAll fails with a bare ENOTDIR PathError unless it is wrapped the same
// way.
//
// This calls writeWorktreeEntry directly, which is the unit of the refusal;
// TestApplyTreeToWorktreeRefusesWhenAFileBlocksADirectoryEndToEnd below pins
// that a full apply actually routes here rather than failing earlier. It used
// to fail earlier: applyTreeToWorktree's loop calls worktreeMatchesTarget
// first, whose Lstat crosses the identical blocking file on its way to the
// leaf and returned the raw ENOTDIR (unlike ENOENT, os.IsNotExist is false for
// a PathError wrapping ENOTDIR, so it was not read as "not found"), so
// writeWorktreeEntry was never reached. Both tests are kept: this one exercises
// the refusal for any future caller reaching writeWorktreeEntry by another
// route, without depending on how the comparison above it classifies errors.
func TestWriteWorktreeEntryRefusesWhenAFileBlocksADirectory(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)

	// plain.txt is a committed regular file; replace it with a file that
	// blocks directory creation for anything the target wants nested
	// beneath it (mirroring the sibling test, which replaces it with a
	// non-empty directory instead).
	blocked := filepath.Join(s.SkillsDir(), "plain.txt")
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := s.writeWorktreeEntry("skills/plain.txt/nested.txt", targets["skills/plain.txt"])
	if err == nil {
		t.Fatal("writing beneath a path a plain file occupies must fail, not silently delete the file")
	}
	// As with the sibling test, a bare os.PathError ("mkdirat plain.txt: not
	// a directory") already happens to contain the path and the word
	// "directory", so the assertion checks for the wrap's own added phrase.
	if !strings.Contains(err.Error(), "skills/plain.txt") || !strings.Contains(err.Error(), "occupied by a file") {
		t.Fatalf("error must name the path and say a file is occupying it, got: %v", err)
	}
	got, statErr := os.ReadFile(blocked)
	if statErr != nil || string(got) != "in the way" {
		t.Fatalf("the blocking file must survive a refusal untouched: %q %v", got, statErr)
	}
}

// nestedFixture commits skills/nested/deep.txt on top of applyFixture and then
// replaces the whole nested/ directory with a plain, untracked file at the same
// name -- the mirror of the directory-where-a-file-belongs collision, and the
// state both ENOTDIR tests below start from.
func nestedFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s := applyFixture(t)
	nested := filepath.Join(s.SkillsDir(), "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: nested"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s, nested
}

// TestApplyTreeToWorktreeRefusesWhenAFileBlocksADirectoryEndToEnd pins that a
// full apply reaches writeWorktreeEntry's named refusal (review round 25
// finding 4). worktreeMatchesTarget's Lstat crosses the blocking file first,
// and because os.IsNotExist is false for a PathError wrapping ENOTDIR it used
// to return that errno unwrapped -- so the caller got "lstat ...: not a
// directory" and never saw the explanatory refusal the sibling unit test pins.
// A path whose parent is a plain file cannot exist, so "does not match" is the
// truthful answer there and routing on to the write is what produces the
// message.
func TestApplyTreeToWorktreeRefusesWhenAFileBlocksADirectoryEndToEnd(t *testing.T) {
	s, blocking := nestedFixture(t)

	_, err := s.applyTreeToWorktree(headTargets(t, s))
	if err == nil {
		t.Fatal("writing beneath a path a plain file occupies must fail, not silently delete the file")
	}
	if !strings.Contains(err.Error(), "skills/nested") || !strings.Contains(err.Error(), "occupied by a file") {
		t.Fatalf("error must name the path and say a file is occupying it, got: %v", err)
	}
	if got, readErr := os.ReadFile(blocking); readErr != nil || string(got) != "in the way" {
		t.Fatalf("the blocking file must survive a refusal untouched: %q %v", got, readErr)
	}
}

// TestRemoveWorktreeEntryTreatsAFileOccupiedParentAsAbsent pins the deletion
// side of the same errno (review round 25 finding 4). A path whose parent is a
// plain file does not exist, so there is nothing to remove and nothing to
// report -- and answering so is what lets the updater converge on a state real
// `git reset --hard` handles, instead of failing with a bare PathError. It
// deletes nothing, so "never remove what cannot be proven owned" is untouched:
// the occupying file is untracked here and is still there afterwards.
func TestRemoveWorktreeEntryTreatsAFileOccupiedParentAsAbsent(t *testing.T) {
	s, blocking := nestedFixture(t)

	removed, err := s.removeWorktreeEntry("skills/nested/deep.txt")
	if err != nil {
		t.Fatalf("a path whose parent is a plain file simply is not there: %v", err)
	}
	if removed {
		t.Fatal("nothing was removed, so nothing may be reported as removed")
	}

	// End to end: the same path as a stale index entry the target no longer
	// wants. The apply must converge and leave the untracked blocker alone.
	target := headTargets(t, s)
	delete(target, "skills/nested/deep.txt")
	if _, err := s.applyTreeToWorktree(target); err != nil {
		t.Fatalf("deleting a tracked path that cannot exist must converge, not fail: %v", err)
	}
	if got, readErr := os.ReadFile(blocking); readErr != nil || string(got) != "in the way" {
		t.Fatalf("the untracked file occupying the parent must survive: %q %v", got, readErr)
	}
}

// TestApplyTreeToWorktreeConvergesADirectoryBackIntoAFile pins the type flip
// the sorted single-pass walk could not converge (review round 25 finding 1):
// a tracked path that is a directory on disk while the target wants it as a
// regular file.
//
// The difference from TestApplyTreeToWorktreeRefusesWhenADirectoryBlocksAFile
// above is the *ownership* of what fills the directory. There, an untracked
// file does, and refusing is the whole point. Here every child is tracked --
// each name is in union(index, target) -- so nothing in the way is content the
// updater cannot prove it owns, and `git reset --hard` converges on exactly
// this state. Refusing it left `fu restore --hard` and `fu revert` unable to
// repair a store the design explicitly allows a user to drive with git
// directly, with a message ("an unexpected directory") that described the
// untracked case rather than this one.
//
// Order is the only thing that makes it work: the tracked child has to be
// unlinked, and the directory it emptied pruned, before the walk reaches the
// directory's own name.
func TestApplyTreeToWorktreeConvergesADirectoryBackIntoAFile(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)

	// skills/plain.txt is a committed regular-file target. Turn it into a
	// directory holding a *tracked* file, the way a direct-Git write between
	// two fu operations does.
	flipped := filepath.Join(s.SkillsDir(), "plain.txt")
	if err := os.Remove(flipped); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(flipped, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flipped, "inner.txt"), []byte("inner"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/plain.txt/inner.txt"); err != nil {
		t.Fatal(err)
	}

	changed, err := s.applyTreeToWorktree(targets)
	if err != nil {
		t.Fatalf("a directory filled only with tracked content must converge back into the target's file: %v", err)
	}

	got, err := os.ReadFile(flipped)
	if err != nil || string(got) != "v1" {
		t.Fatalf("skills/plain.txt must be back as the target's regular file: %q %v", got, err)
	}
	// Sorted, because Store.Revert documents the slice it returns as sorted and
	// the two passes visit the paths in opposite directions.
	if !sort.StringsAreSorted(changed) {
		t.Fatalf("changed paths must come back sorted, got %v", changed)
	}
}

// TestApplyTreeToWorktreeDeletesADirectoryNameAndItsChildTogether pins the
// deletion half of the same ordering problem, which needs the pass to run
// deepest-name-first rather than merely before the writes.
//
// The index really can hold both `skills/dir` and `skills/dir/f.txt` at once:
// real git rejects that pair, go-git does not, and `wt.Add` produces it here
// exactly as it did in the reproduction. Sorted order reaches the directory's
// own name first and refuses on the child that is still inside it; reverse
// order unlinks the child, prunes the emptied directory, and finds nothing
// left at the name by the time it gets there.
func TestApplyTreeToWorktreeDeletesADirectoryNameAndItsChildTogether(t *testing.T) {
	s := applyFixture(t)
	dir := filepath.Join(s.SkillsDir(), "dir")
	if err := os.WriteFile(dir, []byte("was a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/dir"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/dir/f.txt"); err != nil {
		t.Fatal(err)
	}

	// The target is HEAD, which names neither, so both are deletions.
	if _, err := s.applyTreeToWorktree(headTargets(t, s)); err != nil {
		t.Fatalf("a tracked directory name and the tracked child inside it must both go: %v", err)
	}

	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("the emptied directory must be pruned with its child, stat err=%v", err)
	}
}

// TestApplyTreeToWorktreeKeepsADirectoryTheTargetWants pins the complement of
// TestApplyTreeToWorktreeConvergesADirectoryBackIntoAFile (review round 26
// finding 1): there the index and the target agree the name is a file and the
// worktree disagrees; here the *index* is the stale one, holding a file blob at
// a name the target only names as a directory.
//
// Measured against real git before it was written: `git reset --hard` converges
// on this state -- it drops the stale index entry, writes the target's file
// into the existing directory, and keeps the untracked content, warning "unable
// to unlink 'a'" as it goes. The updater refused it instead, and refused with
// the untracked-blocker message, which describes a different situation: there
// the target wants a *file* at the occupied name, so the directory really is in
// the way. Here the directory is precisely what the target wants, and the
// untracked file inside it survives either way.
//
// This is a decline to delete, never a new deletion, so nothing about what may
// be removed widens. The stale index entry needs no unlink at all -- the index
// rebuild that ends the apply drops it by rewriting the index from the target.
func TestApplyTreeToWorktreeKeepsADirectoryTheTargetWants(t *testing.T) {
	s := applyFixture(t)
	dir := filepath.Join(s.SkillsDir(), "a")

	// HEAD names skills/a only as a directory, through the file inside it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: a"); err != nil {
		t.Fatal(err)
	}
	targets := headTargets(t, s)

	// Now the stale index entry: the name held a plain file at some point and
	// was staged as one, which is all it takes for the index to carry a blob
	// there -- go-git keeps it even once a directory replaces the file.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("was a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/a"); err != nil {
		t.Fatal(err)
	}
	// Back to a directory, holding untracked content and nothing else: b.txt is
	// gone from disk, so the apply has real work to do inside a directory it
	// must not remove.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, "stray.txt")
	if err := os.WriteFile(stray, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.applyTreeToWorktree(targets); err != nil {
		t.Fatalf("a directory the target itself names must not be removed for a stale index entry: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "b.txt")); err != nil || string(got) != "b" {
		t.Fatalf("the target's file must be written into the directory: %q %v", got, err)
	}
	if got, err := os.ReadFile(stray); err != nil || string(got) != "mine" {
		t.Fatalf("untracked content must survive, exactly as it does under git reset --hard: %q %v", got, err)
	}
	// The stale entry has to be gone from the index too, or the next run finds
	// the identical work waiting and the apply has not converged.
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range idx.Entries {
		if entry.Name == "skills/a" {
			t.Fatalf("the stale file entry must be dropped by the index rebuild, index still has %v", entry.Name)
		}
	}
}

// TestApplyTreeToWorktreeLeavesUnchangedPathsAlone pins the skip in the
// comparison: rewriting an identical file churns its mtime for nothing and
// makes every later status walk re-hash it.
func TestApplyTreeToWorktreeLeavesUnchangedPathsAlone(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	before, err := os.Stat(filepath.Join(s.SkillsDir(), "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}

	changed, err := s.applyTreeToWorktree(targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("a clean worktree needs no changes, got %v", changed)
	}
	after, err := os.Stat(filepath.Join(s.SkillsDir(), "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("an unchanged path must not be rewritten")
	}
	// Identity as well as mtime. writeWorktreeEntry unlinks the old object and
	// creates a new one, so a rewrite always changes the inode -- but on a
	// filesystem with coarse timestamps, or within one tick, it need not change
	// the mtime. Comparing only mtime could therefore pass over exactly the
	// rewrite this test exists to forbid.
	beforeStat, ok := before.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("expected a unix stat")
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("expected a unix stat")
	}
	if beforeStat.Ino != afterStat.Ino {
		t.Fatalf("an unchanged path must keep its inode: %d -> %d", beforeStat.Ino, afterStat.Ino)
	}
}

// TestApplyTreeToWorktreeLeavesTheStoreClean is the observable property the
// index rebuild exists for: after a reset the store must report no changes at
// all. An index left describing the pre-reset content makes every later status
// call report paths that are in fact identical to HEAD, so `fu restore --hard`
// would refuse on the dirt it had just finished cleaning.
func TestApplyTreeToWorktreeLeavesTheStoreClean(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.applyTreeToWorktree(targets); err != nil {
		t.Fatal(err)
	}

	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("the store must be clean after a reset, still dirty: %v", dirty)
	}
}

// TestApplyTreeToWorktreeClearsDeletedPathsFromTheIndex pins the case that
// actually falsifies a missing rebuild, as opposed to the edit above it,
// which cannot: go-git falls back to re-hashing a path's real file content
// whenever the index's cached Size/ModifiedAt disagree with what is on disk,
// so a path that is edited and then restored to byte-identical bytes compares
// equal no matter how stale the index is left -- that is why
// TestApplyTreeToWorktreeLeavesTheStoreClean stays green even with the
// rebuild removed. A path the target does not contain has no such fallback:
// applyTreeToWorktree deletes it from the worktree, and a deleted path has no
// file left to re-hash against. An index that still lists it is reported as
// staged for deletion by every later status forever after, so `fu restore
// --hard` would refuse on dirt that it had itself just created.
func TestApplyTreeToWorktreeClearsDeletedPathsFromTheIndex(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	staged := filepath.Join(s.SkillsDir(), "staged-new.txt")
	if err := os.WriteFile(staged, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Put it in the index without committing, which is what makes it tracked
	// while HEAD does not hold it -- the same setup
	// TestApplyTreeToWorktreeDeletesPathsAbsentFromTheTarget uses.
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/staged-new.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.applyTreeToWorktree(targets); err != nil {
		t.Fatal(err)
	}

	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("the store must be clean after deleting a target-absent tracked path, still dirty: %v", dirty)
	}
}

// TestApplyTreeToWorktreeRefusesWhenTheGitIndexIsLocked pins precondition 3:
// the index refresh takes .git/index.lock, so it is atomic against the
// supported direct-Git writer rather than racing it.
func TestApplyTreeToWorktreeRefusesWhenTheGitIndexIsLocked(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(s.Dir(), ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })

	_, err := s.applyTreeToWorktree(targets)
	if err == nil || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("a held index lock must stop the reset and name the file, got %v", err)
	}
}

// TestApplyTreeToWorktreeRefusesOutsideAWriteSession pins precondition 4: the
// writes must land through the session's pinned descriptors. A store opened
// read-only re-resolves the worktree by pathname, which is the PlainOpen
// footing DESIGN §6 names.
func TestApplyTreeToWorktreeRefusesOutsideAWriteSession(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	unpinned, err := Open(s.Home)
	if err != nil {
		t.Fatal(err)
	}

	_, err = unpinned.applyTreeToWorktree(targets)
	if !errors.Is(err, errUnpinnedWorktree) {
		t.Fatalf("a reset outside a write session must be refused, got %v", err)
	}
}

// TestApplyTreeToWorktreeRefusesAnAbsoluteSymlinkBeforeWriting pins
// precondition 5, and pins it as a *precondition*: the refusal must land
// before any path is touched, so a store carrying an escaping link is left
// exactly as it was found.
func TestApplyTreeToWorktreeRefusesAnAbsoluteSymlinkBeforeWriting(t *testing.T) {
	s := applyFixture(t)
	targets := headTargets(t, s)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(s.SkillsDir(), "escape")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.applyTreeToWorktree(targets); err == nil {
		t.Fatal("an absolute symlink in the store must stop the reset")
	}
	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "plain.txt"))
	if err != nil || string(got) != "edited" {
		t.Fatalf("the refusal must land before any write: %q %v", got, err)
	}
}

// TestResetWorktreeToHeadIsIdempotent pins the error model. The updater has no
// WAL and needs none: it is not a multi-stage transaction but a repeatable
// convergence, so a run interrupted anywhere is finished by running it again.
func TestResetWorktreeToHeadIsIdempotent(t *testing.T) {
	s := applyFixture(t)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := s.ResetWorktreeToHead()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0] != "skills/plain.txt" {
		t.Fatalf("first run must report the one path it reset, got %v", first)
	}
	second, err := s.ResetWorktreeToHead()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("a second run must find nothing left to do, got %v", second)
	}
}

// TestApplyTreeToWorktreeRefusesAnAbsoluteSymlinkInTheTargetTree pins the
// precondition on the half checkNoAbsoluteSymlinks never covered.
//
// That check validates the *current* worktree, which is right as far as it
// goes -- it is what keeps a stageAll from recording a link git cannot carry.
// Nothing validated the *target*. A commit holding an absolute symlink is
// reachable through the supported direct-Git path (DESIGN calls the store an
// ordinary git repo), and reverting into one made fu write the link itself and
// then refuse it at the very next commit: `fu new`, `fu restore --hard` and
// `fu revert` all failed afterwards, every one of them pointing at a link fu
// had created milliseconds earlier, and only a manual rm broke the cycle.
//
// The refusal has to land before anything is written, so the store is left
// exactly as it was found -- which is what the second half of this test
// asserts, and what makes this a clean rejection rather than a wedge.
func TestApplyTreeToWorktreeRefusesAnAbsoluteSymlinkInTheTargetTree(t *testing.T) {
	s := applyFixture(t)
	target := headTargets(t, s)
	blob := s.Repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("/etc/passwd")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	hash, err := s.Repo.Storer.SetEncodedObject(blob)
	if err != nil {
		t.Fatal(err)
	}
	target["skills/escape"] = worktreeTarget{Mode: filemode.Symlink, Hash: hash}

	// The fixture has to make the pre-flight the *only* thing that can refuse,
	// and an earlier version of this test did neither. On a clean worktree
	// every other path matches its target and is skipped, so there is no write
	// for the refusal to get ahead of; and "skills/escape" sorts first, so
	// writeWorktreeEntry's own defence-in-depth guard would refuse before the
	// loop ever reached anything else. Deleting the pre-flight entirely left
	// store, engine and cli all green.
	//
	// So: dirty a path that sorts *before* "skills/escape" -- "fu.yaml" does --
	// and assert it was not rewritten. Without the pre-flight the loop converges
	// fu.yaml first and only then refuses, leaving a partially applied tree
	// indistinguishable from a hand edit.
	dirtied := filepath.Join(s.Dir(), "fu.yaml")
	if err := os.WriteFile(dirtied, []byte("edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if "fu.yaml" >= "skills/escape" {
		t.Fatal("fixture assumption: the dirtied path must sort before the escaping link")
	}
	before, err := os.ReadFile(filepath.Join(s.SkillsDir(), "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.applyTreeToWorktree(target); !errors.Is(err, ErrAbsoluteSymlink) {
		t.Fatalf("an absolute symlink in the target tree must be refused, got %v", err)
	}

	if _, err := os.Lstat(filepath.Join(s.Dir(), "skills", "escape")); !os.IsNotExist(err) {
		t.Fatalf("the refused link must never reach disk, stat error = %v", err)
	}
	// The load-bearing assertion: refusal precedes the first write, so no path
	// is left converged and the store is exactly as it was found.
	if got, err := os.ReadFile(dirtied); err != nil || string(got) != "edited by hand\n" {
		t.Fatalf("refusal must precede every write; fu.yaml was already converged: %q %v", got, err)
	}
	after, err := os.ReadFile(filepath.Join(s.SkillsDir(), "plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("refusal must precede every write, plain.txt went %q -> %q", before, after)
	}
}

// TestApplyTreeToWorktreeSparesADirectoryHoldingUntrackedContent is the
// highest-risk deletion path in this updater, and it had no test.
//
// pruneEmptiedParents walks upward removing directories a deletion just
// emptied, and proves emptiness the only race-free way this filesystem offers:
// attempt the removal and read ENOTEMPTY back. That is a subtle contract --
// an error return used as a stop condition -- and everything that keeps
// untracked content alive rests on it. Both directions are walked: content
// inside the emptied directory itself, and content one level up.
func TestApplyTreeToWorktreeSparesADirectoryHoldingUntrackedContent(t *testing.T) {
	t.Run("untracked sibling inside the emptied directory", func(t *testing.T) {
		s := applyFixture(t)
		nested := filepath.Join(s.SkillsDir(), "nested", "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "tracked.txt"), []byte("staged"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Commit("new: nested"); err != nil {
			t.Fatal(err)
		}
		// Untracked, so it is in neither the index nor the target: the updater
		// never learns its name, and only the prune walk can reach its parent.
		scratch := filepath.Join(nested, "scratch.md")
		if err := os.WriteFile(scratch, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}

		// A target that no longer holds the tracked file: its removal empties
		// nothing, because the untracked sibling is still there.
		target := headTargets(t, s)
		delete(target, "skills/nested/deep/tracked.txt")
		if _, err := s.applyTreeToWorktree(target); err != nil {
			t.Fatal(err)
		}

		if got, err := os.ReadFile(scratch); err != nil || string(got) != "mine" {
			t.Fatalf("untracked content must survive: %q %v", got, err)
		}
		if _, err := os.Stat(nested); err != nil {
			t.Fatalf("a directory still holding untracked content must survive with it: %v", err)
		}
	})

	t.Run("a store-root-level directory the deletion emptied goes too", func(t *testing.T) {
		// The prune walk used to stop one level short of the store root
		// wholesale, which protected the mounted skills root but also spared
		// every other top-level directory (review round 26 finding 3). A
		// user-tracked directory reachable through the supported direct-Git
		// path is an ordinary one: `git reset --hard` takes it when its last
		// tracked file goes, and leaving it behind is the silent litter this
		// function exists to prevent -- git tracks no empty directory, so
		// nothing would ever name it again.
		s := applyFixture(t)
		misc := filepath.Join(s.Dir(), "misc")
		if err := os.MkdirAll(misc, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(misc, "notes.txt"), []byte("notes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Commit("new: misc"); err != nil {
			t.Fatal(err)
		}

		target := headTargets(t, s)
		delete(target, "misc/notes.txt")
		if _, err := s.applyTreeToWorktree(target); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(misc); !os.IsNotExist(err) {
			t.Fatalf("an emptied store-root-level directory must be pruned, stat err=%v", err)
		}
		// The mounted skills root is what the old blanket guard was really
		// protecting, and it must still be protected -- by being a mount, not by
		// being top level. It is empty of tracked content in this target only if
		// nothing above deleted from it, so assert its survival directly.
		if _, err := os.Stat(s.SkillsDir()); err != nil {
			t.Fatalf("the mounted skills root must survive pruning: %v", err)
		}
	})

	t.Run("the mounted skills root survives even when the deletion empties it", func(t *testing.T) {
		// The precise case the old guard existed for: every tracked path under
		// skills/ goes, so the walk reaches the mount name itself with nothing
		// left inside. Removing it would be a structural change, and resolving
		// a bare mount name through rootFilesystem.resolve targets "." inside
		// the mounted root rather than the name in its parent, so the removal
		// would not even mean what it reads as.
		s := applyFixture(t)
		target := headTargets(t, s)
		for name := range target {
			if strings.HasPrefix(name, "skills/") {
				delete(target, name)
			}
		}

		if _, err := s.applyTreeToWorktree(target); err != nil {
			t.Fatal(err)
		}

		info, err := os.Stat(s.SkillsDir())
		if err != nil || !info.IsDir() {
			t.Fatalf("the mounted skills root must survive being emptied: %v", err)
		}
	})

	t.Run("untracked sibling one level up stops the walk there", func(t *testing.T) {
		s := applyFixture(t)
		nested := filepath.Join(s.SkillsDir(), "nested", "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "tracked.txt"), []byte("staged"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Commit("new: nested"); err != nil {
			t.Fatal(err)
		}
		// One level above the directory the deletion empties.
		scratch := filepath.Join(s.SkillsDir(), "nested", "scratch.md")
		if err := os.WriteFile(scratch, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}

		target := headTargets(t, s)
		delete(target, "skills/nested/deep/tracked.txt")
		if _, err := s.applyTreeToWorktree(target); err != nil {
			t.Fatal(err)
		}

		// deep/ is genuinely empty and goes, exactly as `git reset --hard`
		// takes an emptied directory with it...
		if _, err := os.Stat(nested); !os.IsNotExist(err) {
			t.Fatalf("an emptied directory must be pruned, stat err=%v", err)
		}
		// ...and the walk stops at the level that still holds something.
		if got, err := os.ReadFile(scratch); err != nil || string(got) != "mine" {
			t.Fatalf("the prune walk must stop at the first non-empty level: %q %v", got, err)
		}
	})
}

// TestResetWorktreeToHeadConvergesAfterAnInterruptedRun is the interruption
// design §8 asks for and TestResetWorktreeToHeadIsIdempotent does not provide:
// that test runs twice over an already-converged store, so both runs are
// no-ops and nothing is ever actually interrupted.
//
// Holding .git/index.lock stops the run at its last step, after the worktree
// has been rewritten but before the index is rebuilt -- the one intermediate
// state this updater can be caught in. Releasing the lock and re-running must
// converge: no error, nothing left to do, and a store that reports clean.
func TestResetWorktreeToHeadConvergesAfterAnInterruptedRun(t *testing.T) {
	s := applyFixture(t)
	edited := filepath.Join(s.SkillsDir(), "plain.txt")
	if err := os.WriteFile(edited, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(s.Dir(), ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	interrupted, err := s.ResetWorktreeToHead()
	if err == nil {
		t.Fatal("a held index lock must stop the run")
	}
	// The worktree half already landed: this is the partial state, not a
	// no-op that failed early.
	if got, readErr := os.ReadFile(edited); readErr != nil || string(got) != "v1" {
		t.Fatalf("the worktree half must already have converged: %q %v", got, readErr)
	}
	if len(interrupted) != 1 || interrupted[0] != "skills/plain.txt" {
		t.Fatalf("the interrupted run must still report what it changed, got %v", interrupted)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ResetWorktreeToHead()
	if err != nil {
		t.Fatalf("re-running after an interruption must converge: %v", err)
	}
	if len(resumed) != 0 {
		t.Fatalf("the worktree was already converged, so the resumed run has nothing to do, got %v", resumed)
	}
	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("the store must be clean once the run completes, still dirty: %v", dirty)
	}
}

// TestResetWorktreeToHeadDoesNotWriteTheBranchReference pins precondition 2 for
// the restore --hard entry point. Store.Revert has this pinned already
// (TestRevertWritesTheBranchReferenceExactlyOnce); ResetWorktreeToHead is the
// other caller of the same updater, and it must write the reference zero
// times -- it is `git reset --hard` with no commit argument, so the index and
// worktree move and the branch does not.
func TestResetWorktreeToHeadDoesNotWriteTheBranchReference(t *testing.T) {
	s := applyFixture(t)
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeRef, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	counter := &branchWriteCounter{Storer: s.Repo.Storer, branch: beforeRef.Name()}
	s.Repo.Storer = counter

	if _, err := s.ResetWorktreeToHead(); err != nil {
		t.Fatal(err)
	}

	if counter.count != 0 {
		t.Fatalf("a hard reset to HEAD must not write the branch reference, wrote it %d times", counter.count)
	}
	afterRef, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if beforeRef.Hash() != afterRef.Hash() {
		t.Fatalf("HEAD must not move: %s -> %s", beforeRef.Hash(), afterRef.Hash())
	}
}

// TestRebuildIndexFromTargetInstallsTheIndexByRename pins that the updater
// goes through this repository's own atomic index writer rather than go-git's
// Storer.SetIndex.
//
// The distinction is not stylistic. Storer.SetIndex is fs.Create(".git/index"),
// an in-place O_TRUNC; writePublicIndexAtomically encodes into a temporary and
// renames it over. git's readers take no lock -- `git status` and `git diff`
// read the index unlocked -- so an in-place rewrite lets them observe a
// truncated index, and a crash inside that window leaves a truncated index
// plus a stale lock, after which every fu command fails in Status() with a
// decode error that names the lock and says nothing about the index. That is
// the exact defect writePublicIndexAtomically's own doc comment (git.go) was
// written to explain, twenty lines from where the updater reintroduced it.
//
// It also destroys the property the whole updater is built on. There is no WAL
// here by deliberate design, and every recovery story rests on "re-running
// converges" -- true for a returned error, false for a torn write. `--hard` is
// the command a worried user is most likely to interrupt.
//
// The inode is what tells the two apart: a rename installs a new one, an
// in-place truncate keeps the old.
func TestRebuildIndexFromTargetInstallsTheIndexByRename(t *testing.T) {
	s := applyFixture(t)
	indexPath := filepath.Join(s.Dir(), ".git", "index")
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, ok := before.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("expected a unix stat")
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ResetWorktreeToHead(); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("expected a unix stat")
	}
	if beforeStat.Ino == afterStat.Ino {
		t.Fatalf("the index must be installed by rename, not truncated in place (inode stayed %d)", afterStat.Ino)
	}
	// Still a decodable index describing HEAD, so the rename installed a
	// complete file rather than merely a different one.
	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("the installed index must leave the store clean, still dirty: %v", dirty)
	}
}

// TestSplitResettablePathsDividesByTheResetsOwnPathSet pins the whole point of
// the split, which had no test at all: `--hard` walks union(index, HEAD), so a
// path in neither is one it provably will not touch however many times it runs.
//
// Degrading this to "every dirty path is resettable" left store, engine and
// cli green while reproducing the defect the split was added to fix -- fu
// naming an untracked file and suggesting `--hard`, `--hard` resetting
// nothing, and the next `fu restore` printing the same suggestion forever.
//
// The three fixtures are the three classes that decide the answer, including
// the HEAD-only one, which no test executed: a path HEAD names but the index
// has dropped is still restored by a reset and must not be reported as
// untouchable.
func TestSplitResettablePathsDividesByTheResetsOwnPathSet(t *testing.T) {
	s := applyFixture(t)

	// Tracked in both index and HEAD.
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untracked: in neither source.
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "scratch.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	// HEAD-only: drop run.sh from the index while HEAD still names it, which is
	// the branch nothing else reaches.
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	kept := idx.Entries[:0]
	for _, entry := range idx.Entries {
		if entry.Name != "skills/run.sh" {
			kept = append(kept, entry)
		}
	}
	idx.Entries = kept
	if err := s.Repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}

	resettable, left, err := s.SplitResettablePaths([]string{
		"skills/plain.txt", "skills/run.sh", "skills/scratch.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantResettable := []string{"skills/plain.txt", "skills/run.sh"}
	if len(resettable) != len(wantResettable) {
		t.Fatalf("resettable = %v, want %v", resettable, wantResettable)
	}
	for i, want := range wantResettable {
		if resettable[i] != want {
			t.Fatalf("resettable = %v, want %v", resettable, wantResettable)
		}
	}
	if len(left) != 1 || left[0] != "skills/scratch.md" {
		t.Fatalf("left = %v, want only the untracked path", left)
	}
}

// TestResetWorktreeToHeadRebuildsAStaleIndexWithNothingLeftToDo pins the
// index-comparison half of applyTreeToWorktree's rebuild condition, which
// TestResetWorktreeToHeadConvergesAfterAnInterruptedRun above cannot reach.
//
// That test interrupts a run whose work is a *write*, so the resumed run finds
// the worktree already converged and `changed` empty -- but the index it left
// behind still describes the target, so `len(changed) != 0` alone would be
// enough. Interrupting a run whose work is a *deletion* separates the two: the
// worktree half removes a staged-new path, the index rebuild is blocked, and
// the resumed run then has an index naming a path neither the worktree nor the
// target holds. With the rebuild gated on `len(changed) != 0` alone, that
// index is never rewritten, `fu restore --hard` reports the same dirty paths
// forever, and no number of runs clears them -- the exact opposite of the
// convergence claim this updater makes in place of a WAL.
func TestResetWorktreeToHeadRebuildsAStaleIndexWithNothingLeftToDo(t *testing.T) {
	s := applyFixture(t)
	staged := filepath.Join(s.SkillsDir(), "staged-new.txt")
	if err := os.WriteFile(staged, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/staged-new.txt"); err != nil {
		t.Fatal(err)
	}

	lock := filepath.Join(s.Dir(), ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.ResetWorktreeToHead()
	if err == nil {
		t.Fatal("a held index lock must stop the run before the index is rebuilt")
	}
	if len(interrupted) != 1 || interrupted[0] != "skills/staged-new.txt" {
		t.Fatalf("the worktree half must already have deleted the staged path, got %v", interrupted)
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("the staged-new path must be gone from the worktree: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}

	resumed, err := s.ResetWorktreeToHead()
	if err != nil {
		t.Fatalf("re-running after the interruption must converge: %v", err)
	}
	if len(resumed) != 0 {
		t.Fatalf("the worktree was already converged, so the resumed run touches nothing, got %v", resumed)
	}
	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("the resumed run must leave the store clean, still dirty: %v", dirty)
	}
}

// TestApplyTreeToWorktreeRefusesATrackedPathUnderDotGit pins the guard that
// keeps this updater out of the store's own git directory.
//
// Nothing fu writes produces such a path today, which is exactly why it needs
// a test rather than a reader's confidence: worktreeFS has no .git mount, so a
// tree or index entry named ".git/..." would be written into -- or deleted
// from -- the repository itself, and the only thing standing between the two is
// one string comparison. Real git rejects the same entry outright, and DESIGN
// says a user may drive this repository with git directly, so the entry is
// reachable from outside fu.
func TestApplyTreeToWorktreeRefusesATrackedPathUnderDotGit(t *testing.T) {
	s := applyFixture(t)
	target := headTargets(t, s)
	// Borrow a real blob so the refusal cannot be mistaken for a lookup
	// failure: the entry is well-formed in every respect but its name.
	var borrowed worktreeTarget
	for _, want := range target {
		borrowed = want
		break
	}
	target[".git/hooks/pre-commit"] = borrowed

	_, err := s.applyTreeToWorktree(target)
	if err == nil {
		t.Fatal("a tracked path under .git must be refused")
	}
	if !strings.Contains(err.Error(), ".git") {
		t.Fatalf("the refusal must name the offending path: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(s.Dir(), ".git", "hooks", "pre-commit")); !os.IsNotExist(statErr) {
		t.Fatalf("nothing may be written into the store's own git directory: %v", statErr)
	}
}

// TestApplyTreeToWorktreeConvergesAfterAnInterruptedEntryLoop is the step-4
// interruption case design §8 asks for and this file did not have.
//
// TestResetWorktreeToHeadConvergesAfterAnInterruptedRun covers step 5, the
// index rebuild; the entry loop above it has its own intermediate state, where
// some paths have been converged and one has not. Blocking a single path
// mid-walk produces exactly that -- the alphabetically earlier entries are
// already written, the blocked one is not, and the run returns with a partial
// changed set. Clearing the obstruction and re-running must finish the job
// without redoing the part that landed, which is the convergence claim this
// updater makes in place of a WAL.
func TestApplyTreeToWorktreeConvergesAfterAnInterruptedEntryLoop(t *testing.T) {
	s := applyFixture(t)
	target := headTargets(t, s)
	skills := s.SkillsDir()
	// Two tracked paths to restore. "run.sh" sorts after "plain.txt", so the
	// walk reaches the blocked one second and the first is already converged
	// when the run stops.
	for _, name := range []string{"plain.txt", "run.sh"} {
		if err := os.Remove(filepath.Join(skills, name)); err != nil {
			t.Fatal(err)
		}
	}
	// A non-empty directory where the target needs a regular file:
	// removeWorktreeEntry refuses it rather than deleting content it cannot
	// prove it owns, which stops the loop with real work left.
	blocked := filepath.Join(skills, "run.sh")
	if err := os.MkdirAll(filepath.Join(blocked, "mine"), 0o755); err != nil {
		t.Fatal(err)
	}

	partial, err := s.applyTreeToWorktree(target)
	if err == nil {
		t.Fatal("a directory occupying a target path must stop the run")
	}
	if got, readErr := os.ReadFile(filepath.Join(skills, "plain.txt")); readErr != nil || string(got) != "v1" {
		t.Fatalf("the entries before the obstruction must already have converged: %q %v", got, readErr)
	}
	if len(partial) != 1 || partial[0] != "skills/plain.txt" {
		t.Fatalf("the interrupted run must report exactly what it changed, got %v", partial)
	}

	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.applyTreeToWorktree(target)
	if err != nil {
		t.Fatalf("re-running after the obstruction is cleared must converge: %v", err)
	}
	// Only the path that was blocked: the converged half is compared against
	// the target and skipped, not rewritten.
	if len(resumed) != 1 || resumed[0] != "skills/run.sh" {
		t.Fatalf("the resumed run must finish only the unfinished half, got %v", resumed)
	}
	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("the resumed run must leave the store clean, still dirty: %v", dirty)
	}
}
