package store

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	indexformat "github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func blobHash(content string) plumbing.Hash {
	return plumbing.ComputeHash(plumbing.BlobObject, []byte(content))
}

// moveBranchToOrphan stands in for a direct-git commit landing while fu
// works: a new commit object over HEAD's own tree becomes the branch target.
func moveBranchToOrphan(t *testing.T, s *Store) plumbing.Hash {
	t.Helper()
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	orphan := &object.Commit{
		Author:       *fuSignature(),
		Committer:    *fuSignature(),
		Message:      "external: direct git commit",
		TreeHash:     mustHeadTree(t, s),
		ParentHashes: []plumbing.Hash{before.Hash()},
	}
	obj := s.Repo.Storer.NewEncodedObject()
	if err := orphan.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := s.Repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(before.Name(), h)); err != nil {
		t.Fatal(err)
	}
	return h
}

func publicIndexBytes(t *testing.T, s *Store) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir(), ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A scoped commit takes the rest of the tree from HEAD, not from the public
// index, so whatever the user staged elsewhere with direct git is neither
// recorded by it nor disturbed in the index (SPEC §5.1; batch 2 design §4.1).
func TestPrepareCommitUnderLeavesOutOfPrefixStagingAloneAndUncommitted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, s *Store)
		check func(t *testing.T, s *Store, head *object.Commit)
	}{
		{
			name: "staged modification",
			stage: func(t *testing.T, s *Store) {
				raw, err := os.ReadFile(s.ConfigPath())
				if err != nil {
					t.Fatal(err)
				}
				writeStoreFile(t, s, "fu.yaml", string(raw)+"# staged note\n")
				wt, err := s.Repo.Worktree()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := wt.Add("fu.yaml"); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, s *Store, head *object.Commit) {
				if got := commitFileContents(t, head, "fu.yaml"); len(got) == 0 || got[len(got)-len("# staged note\n"):] == "# staged note\n" {
					t.Fatalf("the staged fu.yaml must not enter a commit named for alpha, got %q", got)
				}
			},
		},
		{
			name: "staged deletion",
			stage: func(t *testing.T, s *Store) {
				unstagePublicIndex(t, s, "skills/beta/SKILL.md")
			},
			check: func(t *testing.T, s *Store, head *object.Commit) {
				if _, err := head.File("skills/beta/SKILL.md"); err != nil {
					t.Fatalf("a deletion staged outside alpha must not enter alpha's commit: %v", err)
				}
			},
		},
		{
			name: "staged addition",
			stage: func(t *testing.T, s *Store) {
				writeStoreFile(t, s, "skills/beta/notes.md", "staged addition")
				wt, err := s.Repo.Worktree()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := wt.Add("skills/beta/notes.md"); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, s *Store, head *object.Commit) {
				if _, err := head.File("skills/beta/notes.md"); err == nil {
					t.Fatal("an addition staged outside alpha must not enter alpha's commit")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scopedFixture(t)
			tc.stage(t, s)
			outside := func(name string) bool { return !underAnyPrefix(name, []string{"skills/alpha"}) }
			indexBefore := publicIndexHashes(t, s, outside)
			entriesBefore := publicIndexEntries(t, s, outside)
			writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")

			prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
			if err != nil {
				t.Fatalf("staging outside the prefix must not refuse the scoped commit: %v", err)
			}
			if got := prepared.ChangedPaths(); !slices.Equal(got, []string{"skills/alpha/SKILL.md"}) {
				t.Fatalf("changed = %v, want alpha alone", got)
			}
			outcome, err := s.CommitPrepared("commit: alpha", prepared)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.Written || outcome.IndexSkipped {
				t.Fatalf("outcome = %+v, want a written commit with the index refreshed", outcome)
			}
			head := headCommit(t, s)
			if got := commitFileContents(t, head, "skills/alpha/SKILL.md"); got != "alpha edited" {
				t.Fatalf("alpha = %q, want the worktree version", got)
			}
			tc.check(t, s, head)
			if indexAfter := publicIndexHashes(t, s, outside); !maps.Equal(indexBefore, indexAfter) {
				t.Fatalf("index entries outside the prefix must survive the commit:\nbefore %v\nafter  %v", indexBefore, indexAfter)
			}
			if entriesAfter := publicIndexEntries(t, s, outside); !reflect.DeepEqual(entriesBefore, entriesAfter) {
				t.Fatalf("index entries outside the prefix must survive verbatim, stat and flags included:\nbefore %+v\nafter  %+v", entriesBefore, entriesAfter)
			}
			if got := publicIndexHashes(t, s, func(name string) bool { return name == "skills/alpha/SKILL.md" }); got["skills/alpha/SKILL.md"] != blobHash("alpha edited").String() {
				t.Fatalf("alpha's index entry must be refreshed to the committed blob, got %v", got)
			}
		})
	}
}

// The public index is re-read under its lock at install time: an entry the
// user staged outside the prefix while fu was committing is kept, and the
// prefix's own entries are still refreshed.
func TestPrepareCommitUnderInstallsOverALiveIndexOutsideThePrefix(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.commitPreparedWithHook("commit: alpha", prepared, func() {
		writeStoreFile(t, s, "skills/beta/SKILL.md", "beta staged mid-commit")
		if _, err := wt.Add("skills/beta/SKILL.md"); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || outcome.IndexSkipped {
		t.Fatalf("outcome = %+v, want a written commit with the index refreshed", outcome)
	}
	got := publicIndexHashes(t, s, func(string) bool { return true })
	if got["skills/alpha/SKILL.md"] != blobHash("alpha edited").String() {
		t.Fatalf("alpha's entry must be refreshed, got %v", got)
	}
	if got["skills/beta/SKILL.md"] != blobHash("beta staged mid-commit").String() {
		t.Fatalf("beta's concurrently staged entry must be kept, got %v", got)
	}
}

// When the prefix's own entries moved in the public index while fu was
// committing, the install is skipped rather than overwriting them, and the
// outcome says so.
func TestPrepareCommitUnderSkipsTheInstallWhenInPrefixEntriesMoved(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.commitPreparedWithHook("commit: alpha", prepared, func() {
		writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha staged mid-commit")
		if _, err := wt.Add("skills/alpha/SKILL.md"); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || !outcome.IndexSkipped {
		t.Fatalf("outcome = %+v, want a written commit whose index install was skipped", outcome)
	}
	if got := commitFileContents(t, headCommit(t, s), "skills/alpha/SKILL.md"); got != "alpha edited" {
		t.Fatalf("the commit must hold the frozen candidate, got %q", got)
	}
	got := publicIndexHashes(t, s, func(name string) bool { return name == "skills/alpha/SKILL.md" })
	if got["skills/alpha/SKILL.md"] != blobHash("alpha staged mid-commit").String() {
		t.Fatalf("the concurrently staged entry must not be overwritten, got %v", got)
	}
}

// A candidate is published only onto the HEAD it was prepared against.
// Otherwise a direct-git commit landing in between would become the parent
// of a tree frozen without its changes, and the scoped shape would even
// revert them out of its HEAD-seeded entries.
func TestCommitPreparedRefusesAStaleParent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(s *Store) (PreparedCommit, error)
	}{
		{"scoped", func(s *Store) (PreparedCommit, error) { return s.PrepareCommitUnder([]string{"skills/alpha"}) }},
		{"store-wide", func(s *Store) (PreparedCommit, error) { return s.PrepareCommit() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scopedFixture(t)
			writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
			prepared, err := tc.prepare(s)
			if err != nil {
				t.Fatal(err)
			}
			moved := moveBranchToOrphan(t, s)
			indexBefore := publicIndexBytes(t, s)

			outcome, err := s.CommitPrepared("commit: alpha", prepared)
			if !errors.Is(err, ErrStaleCandidate) {
				t.Fatalf("err = %v, want ErrStaleCandidate", err)
			}
			if outcome.Written {
				t.Fatalf("nothing may be published onto a moved branch, got %+v", outcome)
			}
			head, err := s.Repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			if head.Hash() != moved {
				t.Fatalf("the branch must stay where the other writer left it: %s, want %s", head.Hash(), moved)
			}
			if got := publicIndexBytes(t, s); string(got) != string(indexBefore) {
				t.Fatal("a refused commit must leave the public index untouched")
			}
		})
	}
}

// The frozen candidate's provenance is checked both ways: its in-prefix
// entries against the private index, and its out-of-prefix entries against
// the HEAD tree it was seeded from.
func TestValidatePreparedCommitRejectsATamperedScopedCandidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(entries []preparedEntry)
	}{
		{"in-prefix entry", func(entries []preparedEntry) {
			for i := range entries {
				if entries[i].Path == "skills/alpha/SKILL.md" {
					entries[i].Hash = blobHash("something else")
				}
			}
		}},
		{"out-of-prefix entry", func(entries []preparedEntry) {
			for i := range entries {
				if entries[i].Path == "skills/beta/SKILL.md" {
					entries[i].Hash = blobHash("something else")
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scopedFixture(t)
			writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
			prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
			if err != nil {
				t.Fatal(err)
			}
			tampered := prepared
			tampered.entries = slices.Clone(prepared.entries)
			tc.tamper(tampered.entries)
			tampered.fingerprint = fingerprintPreparedEntries(tampered.entries)
			before := mustHeadTree(t, s)

			outcome, err := s.CommitPrepared("commit: alpha", tampered)
			if err == nil || outcome.Written {
				t.Fatalf("a tampered candidate must be refused, got err=%v outcome=%+v", err, outcome)
			}
			if after := mustHeadTree(t, s); after != before {
				t.Fatalf("HEAD tree moved: %s -> %s", before, after)
			}
		})
	}
}

// A stat refresh is not a restage. `git status` rewrites stat fields on
// index entries all the time; only content-bearing fields (name, blob,
// mode, stage, flags) decide whether an in-prefix entry moved, so a refresh
// alone must not turn the install into a skip.
func TestPrepareCommitUnderInstallsOverAStatRefreshedInPrefixEntry(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.commitPreparedWithHook("commit: alpha", prepared, func() {
		idx, err := s.Repo.Storer.Index()
		if err != nil {
			t.Fatal(err)
		}
		entry, err := idx.Entry("skills/alpha/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		entry.ModifiedAt = entry.ModifiedAt.Add(time.Hour)
		entry.Size++
		if err := s.Repo.Storer.SetIndex(idx); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || outcome.IndexSkipped {
		t.Fatalf("outcome = %+v, want the index refreshed despite a stat-only rewrite", outcome)
	}
	if got := publicIndexHashes(t, s, func(name string) bool { return name == "skills/alpha/SKILL.md" }); got["skills/alpha/SKILL.md"] != blobHash("alpha edited").String() {
		t.Fatalf("alpha's entry must be refreshed, got %v", got)
	}
}

// publicIndexEntries decodes the on-disk index and returns clones of every
// entry the predicate admits, stat fields and flags included.
func publicIndexEntries(t *testing.T, s *Store, admit func(string) bool) []indexformat.Entry {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	var out []indexformat.Entry
	for _, e := range idx.Entries {
		if admit(e.Name) {
			out = append(out, *e)
		}
	}
	return out
}

// The store-wide install follows the same rule as the scoped one: a stat
// refresh of the live index is not a restage and must not skip the install.
func TestPrepareCommitInstallsOverAStatRefreshedIndex(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	prepared, err := s.PrepareCommit()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.commitPreparedWithHook("commit: alpha", prepared, func() {
		idx, err := s.Repo.Storer.Index()
		if err != nil {
			t.Fatal(err)
		}
		entry, err := idx.Entry("skills/beta/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		entry.ModifiedAt = entry.ModifiedAt.Add(time.Hour)
		if err := s.Repo.Storer.SetIndex(idx); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || outcome.IndexSkipped {
		t.Fatalf("outcome = %+v, want the index installed despite a stat-only rewrite", outcome)
	}
	if got := publicIndexHashes(t, s, func(name string) bool { return name == "skills/alpha/SKILL.md" }); got["skills/alpha/SKILL.md"] != blobHash("alpha edited").String() {
		t.Fatalf("alpha's entry must be refreshed, got %v", got)
	}
}

// fu never writes an index holding unmerged entries: go-git's encoder
// re-sorts entries by name alone, so conflict stages could come out in an
// order git rejects. A live index that gained such entries -- a user mid-merge
// in the store -- leaves the install skipped, with the reason named.
func TestPrepareCommitUnderSkipsTheInstallOverUnmergedEntries(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	var afterHook []byte
	outcome, err := s.commitPreparedWithHook("commit: alpha", prepared, func() {
		conflictStaged(t, s, "skills/beta/SKILL.md")
		afterHook = publicIndexBytes(t, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || !outcome.IndexSkipped || outcome.IndexSkipReason != IndexSkipReasonUnmerged {
		t.Fatalf("outcome = %+v, want a written commit with the install skipped for unmerged entries", outcome)
	}
	if got := publicIndexBytes(t, s); string(got) != string(afterHook) {
		t.Fatal("an index holding unmerged entries must be left exactly as it was")
	}
}

// conflictStaged replaces one path's entry in the public index with the
// three conflict stages a merge leaves behind.
func conflictStaged(t *testing.T, s *Store, name string) {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	base, err := idx.Entry(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Remove(name); err != nil {
		t.Fatal(err)
	}
	for stage, content := range map[indexformat.Stage]string{indexformat.AncestorMode: "base", indexformat.OurMode: "ours", indexformat.TheirMode: "theirs"} {
		conflict := *base
		conflict.Stage = stage
		conflict.Hash = blobHash(content)
		idx.Entries = append(idx.Entries, &conflict)
	}
	if err := s.Repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
}

// A store mid-merge is refused at preparation by both shapes, as git refuses
// a commit until the merge is concluded; a scoped commit that published
// anyway would be undone by the eventual merge commit until the next sweep.
func TestPrepareCommitRefusesAnUnmergedBaseline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(s *Store) (PreparedCommit, error)
	}{
		{"scoped", func(s *Store) (PreparedCommit, error) { return s.PrepareCommitUnder([]string{"skills/alpha"}) }},
		{"store-wide", func(s *Store) (PreparedCommit, error) { return s.PrepareCommit() }},
		{"staged snapshot", func(s *Store) (PreparedCommit, error) { return s.PrepareStagedSnapshot() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scopedFixture(t)
			conflictStaged(t, s, "skills/beta/SKILL.md")
			writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
			before := mustHeadTree(t, s)

			_, err := tc.prepare(s)
			if !errors.Is(err, ErrUnmergedIndex) {
				t.Fatalf("err = %v, want ErrUnmergedIndex", err)
			}
			if after := mustHeadTree(t, s); after != before {
				t.Fatalf("HEAD tree moved: %s -> %s", before, after)
			}
		})
	}
}

// The store-wide install is skipped with the unmerged reason, not the
// restaged one, when conflict entries arrive after preparation.
func TestPrepareCommitSkipsTheInstallOverUnmergedEntries(t *testing.T) {
	s := scopedFixture(t)
	writeStoreFile(t, s, "skills/alpha/SKILL.md", "alpha edited")
	prepared, err := s.PrepareCommit()
	if err != nil {
		t.Fatal(err)
	}
	var afterHook []byte
	outcome, err := s.commitPreparedWithHook("commit: alpha", prepared, func() {
		conflictStaged(t, s, "skills/beta/SKILL.md")
		afterHook = publicIndexBytes(t, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || !outcome.IndexSkipped || outcome.IndexSkipReason != IndexSkipReasonUnmerged {
		t.Fatalf("outcome = %+v, want a written commit with the install skipped for unmerged entries", outcome)
	}
	if got := publicIndexBytes(t, s); string(got) != string(afterHook) {
		t.Fatal("an index holding unmerged entries must be left exactly as it was")
	}
}
