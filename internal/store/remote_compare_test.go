package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func compare(t *testing.T, s *Store) RemoteComparison {
	t.Helper()
	got, err := s.CompareRemote(context.Background())
	if err != nil {
		t.Fatalf("CompareRemote: %v", err)
	}
	return got
}

// Both sides on the same commit is the one relation that needs no local
// object beyond HEAD: the hashes answer it outright.
func TestCompareRemoteReportsSynced(t *testing.T) {
	s, bare := storeWithRemote(t)
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := compare(t, s)
	if got.Relation != RemoteSynced {
		t.Fatalf("relation = %v, want RemoteSynced (%+v)", got.Relation, got)
	}
	if got.Local != got.Remote || got.Local == "" {
		t.Fatalf("both sides must name the same non-empty commit: %+v", got)
	}
	if got.URL != bare {
		t.Fatalf("url = %q, want %q", got.URL, bare)
	}
}

// After a push, the remote's commit is in the local object database, so a
// local commit on top of it is provably ahead.
func TestCompareRemoteReportsAhead(t *testing.T) {
	s, _ := storeWithRemote(t)
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	local := commitFile(t, s, "skills/alpha/SKILL.md", "---\nname: alpha\ndescription: d\n---\n", "add alpha")

	got := compare(t, s)
	if got.Relation != RemoteAhead {
		t.Fatalf("relation = %v, want RemoteAhead (%+v)", got.Relation, got)
	}
	if got.Local != local.String() {
		t.Fatalf("local = %q, want the new commit %q", got.Local, local)
	}
}

// The remote moved ahead and the local store already holds that commit --
// here because it made the commit and pushed it, then rewound. Behind is
// only claimable with the object in hand.
func TestCompareRemoteReportsBehind(t *testing.T) {
	s, _ := storeWithRemote(t)
	base, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	basePoint := base.Hash()
	commitFile(t, s, "skills/alpha/SKILL.md", "---\nname: alpha\ndescription: d\n---\n", "add alpha")
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Rewind the branch; the pushed commit stays in the object database.
	branch, err := s.currentBranch()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(branch, basePoint)); err != nil {
		t.Fatal(err)
	}

	got := compare(t, s)
	if got.Relation != RemoteBehind {
		t.Fatalf("relation = %v, want RemoteBehind (%+v)", got.Relation, got)
	}
}

// Each side holds a commit the other lacks, and the local store can prove it
// because both commits are in its object database.
func TestCompareRemoteReportsDiverged(t *testing.T) {
	s, _ := storeWithRemote(t)
	base, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	basePoint := base.Hash()
	theirs := commitFile(t, s, "skills/theirs/SKILL.md", "---\nname: theirs\ndescription: d\n---\n", "add theirs")
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	branch, err := s.currentBranch()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(branch, basePoint)); err != nil {
		t.Fatal(err)
	}
	ours := commitFile(t, s, "skills/ours/SKILL.md", "---\nname: ours\ndescription: d\n---\n", "add ours")
	if ours == theirs {
		t.Fatal("precondition: the two sides must hold different commits")
	}

	got := compare(t, s)
	if got.Relation != RemoteDiverged {
		t.Fatalf("relation = %v, want RemoteDiverged (%+v)", got.Relation, got)
	}
}

// The heart of the contract: ls-remote hands back a commit this store has
// never seen, and no object may be fetched to judge it. Unknown is the honest
// answer -- not behind, though behind is the likeliest truth, and not a
// remote failure, since the remote answered perfectly.
func TestCompareRemoteReportsUnknownWithoutTheRemoteObject(t *testing.T) {
	s, bare := storeWithRemote(t)
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A commit made directly on the remote, which this store never fetched.
	remoteOnly := commitDirectlyOnBare(t, bare)

	got := compare(t, s)
	if got.Relation != RemoteUnknown {
		t.Fatalf("relation = %v, want RemoteUnknown (%+v)", got.Relation, got)
	}
	if got.Remote != remoteOnly.String() {
		t.Fatalf("remote = %q, want the commit only the remote holds %q", got.Remote, remoteOnly)
	}
	if got.Local == "" {
		t.Fatalf("the local side is known and must still be named: %+v", got)
	}
}

// A remote right after `git init --bare` advertises nothing. That is not a
// failure and not a relation between two commits; it is its own state.
func TestCompareRemoteReportsAnEmptyRemote(t *testing.T) {
	s, _ := storeWithRemote(t)

	got := compare(t, s)
	if got.Relation != RemoteEmpty {
		t.Fatalf("relation = %v, want RemoteEmpty (%+v)", got.Relation, got)
	}
	if got.Remote != "" {
		t.Fatalf("an empty remote names no commit: %+v", got)
	}
}

// The remote has commits, but none on the branch this store syncs.
func TestCompareRemoteReportsNoMatchingBranch(t *testing.T) {
	s, bare := storeWithRemote(t)
	commitDirectlyOnBare(t, bare)
	// Rename the remote's branch out from under the store.
	renameBareBranch(t, bare, "elsewhere")

	got := compare(t, s)
	if got.Relation != RemoteNoBranch {
		t.Fatalf("relation = %v, want RemoteNoBranch (%+v)", got.Relation, got)
	}
	if got.Branch == "" {
		t.Fatalf("the branch fu looked for must be named: %+v", got)
	}
}

// No remote configured is the caller's cue to print nothing at all, so it is
// an error rather than a seventh relation -- there is no comparison to
// describe.
func TestCompareRemoteRefusesWithoutARemote(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CompareRemote(context.Background()); !errors.Is(err, ErrNoRemoteConfigured) {
		t.Fatalf("want ErrNoRemoteConfigured, got %v", err)
	}
}

// A remote that cannot be reached is a failure, distinct from every relation
// above: the report says the remote could not be asked, never that the
// branches are in any particular state.
func TestCompareRemoteReportsAnUnreachableRemote(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRemote(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CompareRemote(context.Background()); err == nil {
		t.Fatal("want an error for a remote that cannot be reached")
	}
}

// SPEC §9: the comparison writes nothing. Not a tracking ref, not an object,
// not FETCH_HEAD -- the whole point of restricting it to ls-remote.
func TestCompareRemoteWritesNothing(t *testing.T) {
	s, bare := storeWithRemote(t)
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	commitDirectlyOnBare(t, bare)

	gitDir := filepath.Join(s.Dir(), ".git")
	if _, err := os.Lstat(filepath.Join(gitDir, "refs", "remotes")); err != nil {
		t.Fatalf("precondition: Push must have left a tracking ref for the snapshot to be able to catch it moving: %v", err)
	}
	before := treeSnapshot(t, gitDir)
	if _, err := s.CompareRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := treeSnapshot(t, gitDir)
	if before != after {
		t.Fatalf("CompareRemote changed .git:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// FETCH_HEAD is the one path a fetch would create that nothing else in
	// this fixture does, so it is worth naming outright. refs/remotes is not:
	// Push already made it, so asserting it does not exist would test the
	// fixture rather than the comparison -- the snapshot above is what covers
	// a tracking ref being moved, which is why it hashes content.
	if _, err := os.Lstat(filepath.Join(gitDir, "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("FETCH_HEAD must not be created by a read-only comparison: %v", err)
	}
}

// commitDirectlyOnBare adds a commit to the bare remote without the store
// ever seeing it -- the state ls-remote reports and no local object explains.
func commitDirectlyOnBare(t *testing.T, bare string) plumbing.Hash {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainClone(work, false, &git.CloneOptions{URL: bare})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		repo, err = git.PlainInit(work, false)
	}
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "remote-only.txt"), []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Add("remote-only.txt"); err != nil {
		t.Fatal(err)
	}
	hash, err := tree.Commit("remote-only", &git.CommitOptions{
		Author: &object.Signature{Name: "fu test", Email: "test@example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Remote("origin"); err != nil {
		if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}
	return hash
}

// renameBareBranch moves the remote's only branch to a name the store does
// not sync.
func renameBareBranch(t *testing.T, bare, to string) {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(to), head.Hash())); err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.RemoveReference(head.Name()); err != nil {
		t.Fatal(err)
	}
}

// treeSnapshot renders a directory tree as sorted "mode digest path" lines,
// so a read-only claim can be checked against the whole of .git rather than
// against the handful of paths a test remembered to name.
//
// Content, not size. Almost everything git writes under refs/ is exactly 41
// bytes -- a hash and a newline -- so a size-based snapshot is blind to the
// one mutation that matters most here: a tracking ref moved to a different
// commit. The tree is a test store's .git, so hashing all of it is cheap.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		digest := "dir"
		if !d.IsDir() {
			// A symlink is recorded by its target rather than followed: what
			// it points at is another entry's line, and following it would
			// double-count or escape the tree.
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				digest = "link:" + target
			} else {
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				digest = fmt.Sprintf("%x", sha256.Sum256(body))
			}
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", info.Mode(), digest, rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// fu syncs the branch HEAD points at, so a detached HEAD -- which only direct
// git could produce -- has no branch to compare and is refused rather than
// guessed around. It is an error, not a relation: there is no pair of commits
// whose relationship the report could describe.
func TestCompareRemoteRefusesADetachedHead(t *testing.T) {
	s, _ := storeWithRemote(t)
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, head.Hash())); err != nil {
		t.Fatal(err)
	}

	_, err = s.CompareRemote(context.Background())
	if err == nil {
		t.Fatal("want an error for a detached HEAD")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Fatalf("the reason must name the state: %v", err)
	}
}

// HEAD naming a branch nothing points at yet: fu's own Init commits, so this
// takes a hand-built store -- but a comparison against nothing is not a
// relation either, and saying so beats reporting one side as the zero hash.
func TestCompareRemoteRefusesAnUnbornBranch(t *testing.T) {
	s, _ := storeWithRemote(t)
	unborn := plumbing.NewBranchReferenceName("never-committed")
	if err := s.Repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, unborn)); err != nil {
		t.Fatal(err)
	}

	_, err := s.CompareRemote(context.Background())
	if err == nil {
		t.Fatal("want an error for a branch with no commit")
	}
	if !strings.Contains(err.Error(), "never-committed") {
		t.Fatalf("the reason must name the branch: %v", err)
	}
}

// A remote holding tags but no branches answers perfectly well, so go-git
// does not call it empty -- and neither should fu. It has commits; what it
// lacks is the branch this store syncs, which is a different sentence and a
// different remedy.
func TestCompareRemoteCallsATagsOnlyRemoteBranchless(t *testing.T) {
	s, bare := storeWithRemote(t)
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1"), head.Hash())); err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.RemoveReference(head.Name()); err != nil {
		t.Fatal(err)
	}

	got := compare(t, s)
	if got.Relation != RemoteNoBranch {
		t.Fatalf("relation = %v, want RemoteNoBranch: the remote has commits, just no branch (%+v)", got.Relation, got)
	}
}

// Only a failure to reach the remote is marked as one. A detached HEAD stops
// the comparison before fu asks anything of the remote, so reporting it as
// unreachable would assert something this run never established.
func TestCompareRemoteMarksOnlyTransportFailuresUnreachable(t *testing.T) {
	unreachable, _ := storeWithRemote(t)
	if _, err := unreachable.SetRemote(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}
	if _, err := unreachable.CompareRemote(context.Background()); !errors.Is(err, ErrRemoteUnreachable) {
		t.Fatalf("a remote that cannot be reached must say so: %v", err)
	}

	local, _ := storeWithRemote(t)
	head, err := local.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, head.Hash())); err != nil {
		t.Fatal(err)
	}
	comparison, err := local.CompareRemote(context.Background())
	if err == nil {
		t.Fatal("precondition: a detached HEAD must stop the comparison")
	}
	if errors.Is(err, ErrRemoteUnreachable) {
		t.Fatalf("a local condition must not be reported as a transport failure: %v", err)
	}
	// The remote is still named, so the report can say which store's remote
	// the comparison was about even when it never got that far.
	if comparison.URL == "" {
		t.Fatalf("the configured remote must be carried out with every failure: %+v", comparison)
	}
}

// Classification must not cost the message its wording: the report writes
// "could not be reached" in its own words, so the error carries only the
// reason.
func TestUnreachableErrorCarriesOnlyTheReason(t *testing.T) {
	err := error(unreachable{errors.New("repository not found")})
	if !errors.Is(err, ErrRemoteUnreachable) {
		t.Fatal("the marker must still be matchable")
	}
	if err.Error() != "repository not found" {
		t.Fatalf("message = %q, want the reason alone", err.Error())
	}
}
