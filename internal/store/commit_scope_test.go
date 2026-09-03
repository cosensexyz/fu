package store

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// scopedFixture seeds two committed skills and returns the store. Every
// test below then dirties both skills and fu.yaml, and asks for alpha alone.
func scopedFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(s.SkillsDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: seed\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Commit("test: seed alpha and beta"); err != nil {
		t.Fatal(err)
	}
	return s
}

func writeStoreFile(t *testing.T, s *Store, rel, content string) {
	t.Helper()
	path := filepath.Join(s.Dir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func headCommit(t *testing.T, s *Store) *object.Commit {
	t.Helper()
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPrepareCommitUnderFreezesOnlyThePrefix(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	writeStoreFile(t, s, "skills/alpha/extra.md", "new in alpha")
	writeStoreFile(t, s, "skills/beta/SKILL.md", "beta edited")
	// Untracked as well as modified dirt outside the prefix: the first cut of
	// this test carried only modified tracked files, which is why the
	// untracked leak below went unnoticed (final review, finding 1).
	writeStoreFile(t, s, "skills/beta/notes.md", "new in beta")
	writeStoreFile(t, s, "fu.yaml", "version: 1\nskills: {}\n# hand edit\n")

	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"skills/alpha/SKILL.md", "skills/alpha/extra.md"}
	if got := prepared.ChangedPaths(); !slices.Equal(got, want) {
		t.Fatalf("changed = %v, want %v", got, want)
	}
	outcome, err := s.CommitPrepared("commit: alpha", prepared)
	if err != nil || !outcome.Written {
		t.Fatalf("commit: written=%v err=%v", outcome.Written, err)
	}

	// The commit holds alpha's new content and beta's old content.
	c := headCommit(t, s)
	if got := commitFileContents(t, c, "skills/alpha/extra.md"); got != "new in alpha" {
		t.Fatalf("alpha/extra.md in commit = %q", got)
	}
	if got := commitFileContents(t, c, "skills/beta/SKILL.md"); got == "beta edited" {
		t.Fatal("beta's edit must not be in a commit scoped to alpha")
	}
	// The rest of the worktree is still pending, untouched.
	remaining, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if wantRemaining := []string{"fu.yaml", "skills/beta/SKILL.md", "skills/beta/notes.md"}; !slices.Equal(remaining, wantRemaining) {
		t.Fatalf("remaining = %v, want %v", remaining, wantRemaining)
	}
	if got, _ := os.ReadFile(filepath.Join(s.SkillsDir(), "beta", "SKILL.md")); string(got) != "beta edited" {
		t.Fatalf("beta's worktree edit was disturbed: %q", got)
	}
}

// TestPrepareCommitUnderIgnoresUntrackedPathsOutsideThePrefix pins the final
// review's finding 1. go-git reports Staging == Untracked ('?', not ' ') for
// every path present in the worktree and absent from the index, so a
// changed-path projection that only tested `!= Unmodified` counted each of
// them as a HEAD-to-index change. Inside prepareCommit that was invisible --
// stageAll stages the whole store first, so nothing is untracked by the time
// the projection runs -- but PrepareCommitUnder deliberately leaves the index
// partially staged, so every untracked path anywhere else in the store leaked
// into the candidate's changed set and tripped its containment check.
// The user-visible effect was that `fu commit alpha` failed outright, blaming
// a file "staged in git's index" that git had never heard of, whenever a
// brand-new file or a stray .DS_Store sat somewhere else in the store.
func TestPrepareCommitUnderIgnoresUntrackedPathsOutsideThePrefix(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	writeStoreFile(t, s, "skills/beta/notes.md", "brand new, never staged")
	writeStoreFile(t, s, ".DS_Store", "junk at the store root")

	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatalf("untracked paths outside the prefix must not block a scoped commit: %v", err)
	}
	if got := prepared.ChangedPaths(); !slices.Equal(got, []string{"skills/alpha/SKILL.md"}) {
		t.Fatalf("changed = %v, want alpha's edit alone", got)
	}
	if _, err := s.CommitPrepared("commit: alpha", prepared); err != nil {
		t.Fatal(err)
	}
	c := headCommit(t, s)
	for _, outside := range []string{"skills/beta/notes.md", ".DS_Store"} {
		if _, err := c.File(outside); !errors.Is(err, object.ErrFileNotFound) {
			t.Fatalf("%s must stay out of a commit scoped to alpha, got err=%v", outside, err)
		}
	}
	// Still on disk and still pending: the scoped commit left them alone.
	remaining, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".DS_Store", "skills/beta/notes.md"}; !slices.Equal(remaining, want) {
		t.Fatalf("remaining = %v, want %v", remaining, want)
	}
}

func TestPrepareCommitUnderRecordsDeletions(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/extra.md", "to be removed")
	if _, err := s.Commit("test: add alpha/extra.md"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.SkillsDir(), "alpha", "extra.md")); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.ChangedPaths(); !slices.Equal(got, []string{"skills/alpha/extra.md"}) {
		t.Fatalf("changed = %v, want the deletion", got)
	}
	if _, err := s.CommitPrepared("commit: alpha", prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := headCommit(t, s).File("skills/alpha/extra.md"); !errors.Is(err, object.ErrFileNotFound) {
		t.Fatalf("deleted file must be gone from the commit, got err=%v", err)
	}
}

func TestPrepareCommitUnderRecordsAWholeSkillDirectoryRemoved(t *testing.T) {
	s := scopedFixture(t)
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.ChangedPaths(); !slices.Equal(got, []string{"skills/alpha/SKILL.md"}) {
		t.Fatalf("changed = %v, want alpha's one file deleted", got)
	}
}

// TestPrepareCommitUnderRefusesABaselinePathBlockedByAFile pins the final
// review's finding 7: an fs.Lstat error that is not fs.ErrNotExist was read
// as "the path is still there". Here alpha's directory has been replaced by a
// regular file, so lstat on the baseline entry skills/alpha/SKILL.md returns
// ENOTDIR rather than ENOENT -- the file is gone, but the scan concluded it
// was present and left its stale blob in the candidate. The candidate's
// changed set then came out *empty*, so `fu commit alpha` reported "nothing
// to commit" and exited 0 over a skill that had just been obliterated (and
// the candidate itself was unbuildable: an index cannot hold both
// skills/alpha as a blob and skills/alpha/SKILL.md beneath it).
//
// Refusing is this file's own policy -- one file it cannot read stops the
// write -- and it is what the store-wide path already does for the identical
// on-disk state, through stageAll and the same explainStagingFailure.
func TestPrepareCommitUnderRefusesABaselinePathBlockedByAFile(t *testing.T) {
	s := scopedFixture(t)
	alpha := filepath.Join(s.SkillsDir(), "alpha")
	if err := os.RemoveAll(alpha); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alpha, []byte("alpha is a file now"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err == nil {
		t.Fatalf("want a refusal, got a candidate changing %v", prepared.ChangedPaths())
	}
	if !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), alpha) {
		t.Fatalf("the refusal must name the blocking store entry: %v", err)
	}
}

func TestPrepareCommitUnderIncludesIgnoredFiles(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/.gitignore", "node_modules/\n")
	writeStoreFile(t, s, "skills/alpha/node_modules/dep.js", "module.exports = {}")
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(prepared.ChangedPaths(), "skills/alpha/node_modules/dep.js") {
		t.Fatalf("ignored content under the prefix must be recorded, like a sweep does: %v", prepared.ChangedPaths())
	}
}

func TestPrepareCommitUnderRejectsBadPrefixes(t *testing.T) {
	s := scopedFixture(t)
	// "../fu.yaml" and "." reach the component guard rather than the
	// path.Clean check (final review, finding 5): path.Clean leaves both
	// unchanged, so nothing else in validateCommitPrefix rejects them, and
	// "skills/../fu.yaml" -- the row that was already here -- is caught by
	// the Clean check instead, which is why the guard had no coverage.
	// Deleting the guard makes "." pass through and this test fail;
	// "../fu.yaml" is then still refused, but only by a downstream
	// `lstat: invalid path "../fu.yaml"` from go-git, which is the guard's
	// named refusal replaced by an internal one.
	for _, prefixes := range [][]string{nil, {""}, {"/skills/alpha"}, {"skills/../fu.yaml"}, {"skills/alpha/"}, {"../fu.yaml"}, {"."}} {
		if _, err := s.PrepareCommitUnder(prefixes); err == nil {
			t.Errorf("prefixes %q must be refused", prefixes)
		}
	}
}

func TestPrepareCommitUnderWithNothingChangedIsEmpty(t *testing.T) {
	s := scopedFixture(t)
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.ChangedPaths()) != 0 {
		t.Fatalf("clean prefix must yield no changes, got %v", prepared.ChangedPaths())
	}
	outcome, err := s.CommitPrepared("commit: alpha", prepared)
	if err != nil || outcome.Written {
		t.Fatalf("an unchanged candidate must not write: written=%v err=%v", outcome.Written, err)
	}
}

// unstagePublicIndex is `git rm --cached <path>`: the entry leaves the public
// index, the file stays on disk and stays in HEAD.
func unstagePublicIndex(t *testing.T, s *Store, names ...string) {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := idx.Remove(name); err != nil {
			t.Fatalf("remove %s from the index: %v", name, err)
		}
	}
	if err := s.Repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareCommitUnderRefusesAStagedDeletionOutsideThePrefix pins the
// regression that skipping every Untracked path introduced. go-git's status
// reports Staging == Untracked for two genuinely different states: a file
// that is in neither HEAD nor the index -- new, and rightly outside a
// candidate that never staged it -- and a file removed from the index with
// `git rm --cached` while still on disk, which is a real HEAD-to-index
// deletion (the index-to-worktree loop overwrites the Deleted the HEAD-to-index
// loop set). Skipping both dropped the deletion from the changed set, so the
// containment check never saw it and CommitPrepared wrote a tree with the
// path gone.
//
// With fu.yaml that destroys the store: store identity is "fu.yaml tracked at
// HEAD", so `fu log`, `fu list` and `fu status` all failed with "store not
// initialized" and `fu init` with "store already initialized". With another
// skill it is silent data loss -- exit 0, only the named skill reported, and
// `fu list` still showing the skill from the untouched on-disk fu.yaml, while
// the committed tree no longer holds its content.
func TestPrepareCommitUnderRefusesAStagedDeletionOutsideThePrefix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unstage []string
	}{
		{name: "config", unstage: []string{"fu.yaml"}},
		{name: "another skill", unstage: []string{"skills/beta/SKILL.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scopedFixture(t)
			unstagePublicIndex(t, s, tc.unstage...)
			writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")

			prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
			var violation *CommitScopeViolationError
			if !errors.As(err, &violation) {
				t.Fatalf("a staged deletion of %v outside the prefix must be refused; got err=%v over a candidate changing %v",
					tc.unstage, err, prepared.ChangedPaths())
			}
			if !slices.Equal(violation.Paths, tc.unstage) {
				t.Fatalf("refusal names %v, want %v", violation.Paths, tc.unstage)
			}
			// Refusing leaves the store usable: nothing was committed, so the
			// path is still in HEAD and fu still recognises its own store.
			for _, name := range tc.unstage {
				if _, err := headCommit(t, s).File(name); err != nil {
					t.Fatalf("%s must still be in HEAD after the refusal: %v", name, err)
				}
			}
		})
	}
}

// publicIndexHashes reads the on-disk index and returns path to blob hash for
// every entry the predicate admits. Used to state what a scoped commit may and
// may not touch there.
func publicIndexHashes(t *testing.T, s *Store, admit func(string) bool) map[string]string {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range idx.Entries {
		if admit(e.Name) {
			out[e.Name] = e.Hash.String()
		}
	}
	return out
}

// TestPrepareCommitUnderConvergesWhenAnInPrefixPathWasStagedThenEdited pins the
// property a user actually relies on: after a scoped commit succeeds, nothing
// under that skill is still reported as pending.
//
// The state is an ordinary one -- `git add` a file inside the skill, keep
// editing it, then commit. It used to end with the tree holding the new bytes
// while the public index kept the staged ones, so the skill was reported
// uncommitted forever and no fu command could clear it; the next ordinary
// write then swept that stale entry back in, rolling the skill backwards and
// then forwards across two commits (review 2026-09-02, Critical).
func TestPrepareCommitUnderConvergesWhenAnInPrefixPathWasStagedThenEdited(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "staged version")
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/alpha/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "later version")

	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitPrepared("commit: alpha", prepared); err != nil {
		t.Fatal(err)
	}
	if got := commitFileContents(t, headCommit(t, s), "skills/alpha/SKILL.md"); got != "later version" {
		t.Fatalf("the commit must hold the worktree version, got %q", got)
	}
	pending, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a successful scoped commit must leave nothing pending, got %v", pending)
	}
}

// TestPrepareCommitUnderLeavesThePublicIndexAloneOutsideThePrefix states the
// other half of that rule: refreshing in-prefix entries may not disturb an
// entry the user staged elsewhere.
//
// It is a statement of the invariant, not a regression test for it. Staging a
// path whose content equals HEAD leaves the index fingerprint equal to HEAD,
// so syncPublic would have been set anyway and removing the scoped-sync line
// leaves this green (review 2026-09-02 round 2, Minor). The safety proof is
// the containment check rather than this test: whenever a scoped commit
// succeeds with a baseline that differs from HEAD, every out-of-prefix entry
// necessarily equals HEAD, or the refusal would have fired first.
//
// beta is staged with content equal to HEAD, which is the out-of-prefix staged
// state that does not trip the containment refusal -- a staged path that
// differs is refused before any commit, so this is the only shape in which an
// out-of-prefix index entry can survive into the sync.
func TestPrepareCommitUnderLeavesThePublicIndexAloneOutsideThePrefix(t *testing.T) {
	s := scopedFixture(t)
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/beta/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	outside := func(name string) bool { return !underAnyPrefix(name, []string{"skills/alpha"}) }
	before := publicIndexHashes(t, s, outside)
	if len(before) == 0 {
		t.Fatal("fixture must leave entries outside the prefix for this to mean anything")
	}
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")

	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitPrepared("commit: alpha", prepared); err != nil {
		t.Fatal(err)
	}
	after := publicIndexHashes(t, s, outside)
	if !maps.Equal(before, after) {
		t.Fatalf("entries outside the prefix must be untouched:\nbefore %v\nafter  %v", before, after)
	}
}

// TestPrepareCommitUnderConvergesWhenTheTreeDoesNotMove is the other half of
// the convergence invariant, and the half round 1's fix missed: a candidate
// can supersede index entries while leaving the tree exactly where it was, and
// the publish path returns before syncing in precisely that case.
//
// Both shapes leave HEAD's tree unchanged and the public index disagreeing with
// it, so before the fix each reported the skill pending forever while
// `fu commit <name>` answered "nothing to commit" (review 2026-09-02 round 2).
func TestPrepareCommitUnderConvergesWhenTheTreeDoesNotMove(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty func(t *testing.T, s *Store, wt *git.Worktree)
	}{
		{
			// Stage a version, then put the worktree back to the committed
			// bytes: the candidate equals HEAD, the index does not.
			name: "staged then reverted",
			dirty: func(t *testing.T, s *Store, wt *git.Worktree) {
				committed := commitFileContents(t, headCommit(t, s), "skills/alpha/SKILL.md")
				writeStoreFile(t, s, "skills/alpha/SKILL.md", "staged version")
				if _, err := wt.Add("skills/alpha/SKILL.md"); err != nil {
					t.Fatal(err)
				}
				writeStoreFile(t, s, "skills/alpha/SKILL.md", committed)
			},
		},
		{
			// Drop an in-prefix path from the index with the file still on
			// disk: staging the prefix puts it straight back, so again the
			// tree does not move.
			name: "unstaged in prefix",
			dirty: func(t *testing.T, s *Store, wt *git.Worktree) {
				idx, err := s.Repo.Storer.Index()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := idx.Remove("skills/alpha/SKILL.md"); err != nil {
					t.Fatal(err)
				}
				if err := s.Repo.Storer.SetIndex(idx); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scopedFixture(t)
			wt, err := s.Repo.Worktree()
			if err != nil {
				t.Fatal(err)
			}
			tc.dirty(t, s, wt)
			before := mustHeadTree(t, s)

			prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := s.CommitPrepared("commit: alpha", prepared)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Written {
				t.Fatalf("setup check: this case must leave the tree unmoved, got a published commit")
			}
			if after := mustHeadTree(t, s); after != before {
				t.Fatalf("HEAD tree moved: %s -> %s", before, after)
			}
			pending, err := s.ChangedPathsIncludingIgnored()
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("a scoped commit must converge even when it writes no commit, got %v", pending)
			}
		})
	}
}
