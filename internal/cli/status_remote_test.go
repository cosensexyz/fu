// internal/cli/status_remote_test.go
package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cosensexyz/fu/internal/engine"
)

// Each relation gets a line a reader can act on, and the one that cannot be
// resolved without fetching says so plainly rather than guessing.
func TestRemoteLineSaysWhatToDoAboutEachRelation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   engine.RemoteStatus
		contains []string
		absent   []string
	}{
		{
			"synced",
			engine.RemoteStatus{URL: "git@example.invalid:me/store.git", Branch: "main", Relation: engine.RemoteSynced},
			[]string{"up to date with", "git@example.invalid:me/store.git"},
			nil,
		},
		{
			"ahead",
			engine.RemoteStatus{URL: "u", Branch: "main", Relation: engine.RemoteAhead},
			[]string{"ahead of", "`fu push`"},
			nil,
		},
		{
			"behind",
			engine.RemoteStatus{URL: "u", Branch: "main", Relation: engine.RemoteBehind},
			[]string{"behind", "`fu pull`"},
			nil,
		},
		{
			"diverged",
			engine.RemoteStatus{URL: "u", Branch: "main", Relation: engine.RemoteDiverged},
			[]string{"diverged"},
			nil,
		},
		{
			// The contract's whole point: not behind, not a failure.
			"unknown",
			engine.RemoteStatus{URL: "u", Branch: "main", Remote: "abc1234", Relation: engine.RemoteUnknown},
			[]string{"abc1234", "cannot tell", "`fu pull`"},
			[]string{"behind", "failed"},
		},
		{
			"empty remote",
			engine.RemoteStatus{URL: "u", Branch: "main", Relation: engine.RemoteEmpty},
			[]string{"no commits yet"},
			nil,
		},
		{
			"no matching branch",
			engine.RemoteStatus{URL: "u", Branch: "main", Relation: engine.RemoteNoBranch},
			[]string{"no branch", "main"},
			nil,
		},
		{
			"remote could not be reached",
			engine.RemoteStatus{URL: "u", Branch: "main", Err: "repository not found", Unreachable: true},
			[]string{"could not be reached", "repository not found", "u"},
			[]string{"up to date", "behind", "ahead"},
		},
		{
			// A local condition, not a remote one: the remote may be in
			// perfect health, and fu never asked it. Saying it could not be
			// reached asserts something this run did not establish -- the one
			// thing this whole comparison exists to avoid.
			"local condition stops the comparison",
			engine.RemoteStatus{URL: "u", Branch: "main", Err: "HEAD is detached; fu syncs the branch HEAD points at"},
			[]string{"HEAD is detached", "u"},
			[]string{"could not be reached", "up to date", "behind", "ahead"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := remoteLine(tc.status)
			for _, want := range tc.contains {
				if !strings.Contains(line, want) {
					t.Fatalf("line = %q, want it to contain %q", line, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(line, unwanted) {
					t.Fatalf("line = %q, must not contain %q", line, unwanted)
				}
			}
		})
	}
}

// End to end: a configured remote puts a line in the store section, and
// finding the branches out of step is not a failure.
func TestStatusCommandReportsTheRemoteAndStillExitsZero(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	bare := filepath.Join(t.TempDir(), "store.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "remote", bare)

	out, err := runCmd(t, "status")
	if err != nil {
		t.Fatalf("a remote that is merely out of step is not a failure: %v", err)
	}
	if !strings.Contains(out, "store") || !strings.Contains(out, bare) {
		t.Fatalf("want the remote reported under the store section:\n%s", out)
	}
	if !strings.Contains(out, "no commits yet") {
		t.Fatalf("a fresh bare remote has no commits; say so:\n%s", out)
	}
}

// A store with no remote says nothing about one, and a clean store with no
// remote still reports as clean rather than growing an empty heading.
func TestStatusCommandSaysNothingAboutAnAbsentRemote(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"remote", "up to date", "cannot tell"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("no remote is configured; %q must not appear:\n%s", unwanted, out)
		}
	}
}

// The read-only proof for the remote path covers the whole of $FU_HOME, not
// just store/.git: store.CompareRemote's own test snapshots .git alone, so a
// write into the worktree, staging/ or recovery/ on this path would pass it.
// It is also the only read-only proof that runs with a remote configured --
// the two older ones use stores that have none, so they never enter this code
// at all.
func TestStatusCommandWithARemoteWritesNothingAnywhere(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	bare := filepath.Join(t.TempDir(), "store.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "remote", bare)
	runCmd(t, "push")
	// A commit only the remote has, so the comparison takes its longest path:
	// ls-remote, then a lookup that misses in the local object database.
	pushDirectlyToBare(t, bare)

	before := homeTreeDigest(t, fuHome)
	out, err := runCmd(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cannot tell") {
		t.Fatalf("precondition: the comparison must have reached the unknown case:\n%s", out)
	}
	if after := homeTreeDigest(t, fuHome); after != before {
		t.Fatalf("fu status wrote into $FU_HOME:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// homeTreeDigest renders every path under root as "mode digest path". Content
// rather than size, for the reason store's own snapshot does it: a git ref is
// 41 bytes whichever commit it names.
func homeTreeDigest(t *testing.T, root string) string {
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
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			digest = "link:" + target
		case !d.IsDir():
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			digest = fmt.Sprintf("%x", sha256.Sum256(body))
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

// pushDirectlyToBare puts a commit on the remote that the store under test has
// never seen.
func pushDirectlyToBare(t *testing.T, bare string) {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainClone(work, false, &git.CloneOptions{URL: bare})
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
	if _, err := tree.Commit("remote-only", &git.CommitOptions{
		Author: &object.Signature{Name: "fu test", Email: "test@example.invalid"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
}

// The two ends of the classification were pinned and the join between them was
// not: hard-wiring status.Unreachable to false left every package green while
// reintroducing the defect it was added to fix -- a remote line blaming a
// healthy remote for a purely local condition. These two run the real command
// against each state, so the wire is covered in both directions.
func TestStatusCommandBlamesTheRemoteOnlyWhenItAskedOne(t *testing.T) {
	t.Run("unreachable remote", func(t *testing.T) {
		home := statusRemoteFixture(t)
		out, err := runCmd(t, "status")
		if err != nil {
			t.Fatalf("an unreachable remote is not a command failure: %v", err)
		}
		if !strings.Contains(out, "could not be reached") {
			t.Fatalf("fu did ask the remote and got no answer; say so:\n%s", out)
		}
		_ = home
	})

	t.Run("local condition", func(t *testing.T) {
		home := statusRemoteFixture(t)
		// Detach HEAD, which stops the comparison before fu asks anything of
		// the remote. The remote here is unreachable too, which is the point:
		// even so, this run never contacted it and must not say it did.
		detachHead(t, filepath.Join(os.Getenv("FU_HOME"), "store"))
		out, err := runCmd(t, "status")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "not compared") || !strings.Contains(out, "HEAD is detached") {
			t.Fatalf("want the local condition named as one:\n%s", out)
		}
		if strings.Contains(out, "could not be reached") {
			t.Fatalf("fu never asked the remote; claiming it is unreachable asserts what this run did not establish:\n%s", out)
		}
		_ = home
	})
}

// A failed comparison reaches no relation, so Relation must stay unset rather
// than reading as the enum's first value. Nothing renders it today -- the line
// checks Err first -- which is exactly why restoring the old numbering would
// otherwise pass.
func TestStatusReportsNoRelationWhenTheComparisonFailed(t *testing.T) {
	statusRemoteFixture(t)

	outcome, err := (&engine.Application{}).Status()
	if err != nil {
		t.Fatal(err)
	}
	remote := outcome.Report.Store.Remote
	if remote == nil || remote.Err == "" {
		t.Fatalf("precondition: the comparison must have failed: %+v", remote)
	}
	if remote.Relation != engine.RemoteRelationUnset {
		t.Fatalf("relation = %v, want it unset: a failed comparison established none", remote.Relation)
	}
}

// statusRemoteFixture builds a store whose remote is configured but absent.
func statusRemoteFixture(t *testing.T) string {
	t.Helper()
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "remote", filepath.Join(t.TempDir(), "does-not-exist"))
	return home
}

func detachHead(t *testing.T, storeDir string) {
	t.Helper()
	repo, err := git.PlainOpen(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, head.Hash())); err != nil {
		t.Fatal(err)
	}
}
