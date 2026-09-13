package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	indexformat "github.com/go-git/go-git/v5/plumbing/format/index"
)

// intentToAdd stands in for `git add -N <rel>` on a file with the given
// content on disk: the index carries an entry with the intent-to-add flag and
// the empty blob, and the empty blob is in the object database, exactly as
// git writes it (git stores that blob when it registers the intent; verified
// against git 2.x and go-git's decoder).
func intentToAdd(t *testing.T, s *Store, rel, content string) {
	t.Helper()
	writeStoreFile(t, s, rel, content)
	registerIntentToAdd(t, s, rel)
}

// registerIntentToAdd writes the index entry and the empty blob for a path
// that is already on disk, whether or not the path is tracked (git allows
// `git rm --cached p && git add -N p`).
func registerIntentToAdd(t *testing.T, s *Store, rel string) {
	t.Helper()
	empty := s.Repo.Storer.NewEncodedObject()
	empty.SetType(plumbing.BlobObject)
	if _, err := s.Repo.Storer.SetEncodedObject(empty); err != nil {
		t.Fatal(err)
	}
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = idx.Remove(rel)
	idx.Entries = append(idx.Entries, &indexformat.Entry{Name: rel, Hash: blobHash(""), Mode: filemode.Regular, IntentToAdd: true})
	if err := s.Repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
}

func indexEntry(t *testing.T, s *Store, rel string) *indexformat.Entry {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := idx.Entry(rel)
	if err != nil {
		t.Fatalf("index entry %s: %v", rel, err)
	}
	return entry
}

// gitStatusPorcelain asks the git binary how the store looks, or reports
// that no binary is available.
func gitStatusPorcelain(t *testing.T, s *Store) (string, bool) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(gitPath, "-C", s.Dir(), "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	return string(out), true
}

// An intent-to-add entry is not staged content: git shows it as unstaged,
// and the external snapshot must not turn its empty placeholder blob into a
// recorded empty file. A genuinely staged empty file still counts.
func TestPrepareStagedSnapshotIgnoresIntentToAddEntries(t *testing.T) {
	s := scopedFixture(t)
	intentToAdd(t, s, "skills/alpha/notes.md", "real content")
	writeStoreFile(t, s, "skills/alpha/empty.md", "")
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("skills/alpha/empty.md"); err != nil {
		t.Fatal(err)
	}

	prepared, err := s.PrepareStagedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.ChangedPaths(); !slices.Equal(got, []string{"skills/alpha/empty.md"}) {
		t.Fatalf("changed = %v, want the staged empty file alone", got)
	}
	outcome, err := s.CommitStagedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written {
		t.Fatalf("the staged empty file must be recorded: %+v", outcome)
	}
	head := headCommit(t, s)
	if got := commitFileContents(t, head, "skills/alpha/empty.md"); got != "" {
		t.Fatalf("empty.md = %q, want empty", got)
	}
	if _, err := head.File("skills/alpha/notes.md"); err == nil {
		t.Fatal("an intent-to-add placeholder must not enter the external snapshot")
	}
}

// A sweep over an intent-to-add file records its real content once, in the
// worktree layer, and the index it installs no longer carries the flag, so
// git agrees the store is clean.
func TestSweepRecordsIntentToAddContentAndClearsTheFlag(t *testing.T) {
	s := scopedFixture(t)
	before := headCommit(t, s).Hash
	intentToAdd(t, s, "skills/alpha/notes.md", "real content")

	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}

	head := headCommit(t, s)
	if len(head.ParentHashes) != 1 || head.ParentHashes[0] != before {
		t.Fatalf("exactly one commit must be recorded, got parents %v over %s", head.ParentHashes, before)
	}
	if got := commitFileContents(t, head, "skills/alpha/notes.md"); got != "real content" {
		t.Fatalf("notes.md = %q, want the real content", got)
	}
	entry := indexEntry(t, s, "skills/alpha/notes.md")
	if entry.IntentToAdd || entry.Hash != blobHash("real content") {
		t.Fatalf("the installed index must hold the recorded blob without the flag: %+v", entry)
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("the store must be clean after the sweep")
	}
	if out, ok := gitStatusPorcelain(t, s); ok && strings.TrimSpace(out) != "" {
		t.Fatalf("git must agree the store is clean, got:\n%s", out)
	}
}

// A scoped commit materialises intent-to-add entries inside its prefix and
// leaves those outside exactly as git wrote them.
func TestPrepareCommitUnderClearsIntentToAddInsideThePrefixAndKeepsItOutside(t *testing.T) {
	s := scopedFixture(t)
	intentToAdd(t, s, "skills/alpha/notes.md", "alpha notes")
	intentToAdd(t, s, "skills/beta/notes.md", "beta notes")

	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.ChangedPaths(); !slices.Equal(got, []string{"skills/alpha/notes.md"}) {
		t.Fatalf("changed = %v, want alpha's note alone", got)
	}
	outcome, err := s.CommitPrepared("commit: alpha", prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written || outcome.IndexSkipped {
		t.Fatalf("outcome = %+v, want a written commit with the index refreshed", outcome)
	}
	head := headCommit(t, s)
	if got := commitFileContents(t, head, "skills/alpha/notes.md"); got != "alpha notes" {
		t.Fatalf("alpha notes = %q", got)
	}
	if _, err := head.File("skills/beta/notes.md"); err == nil {
		t.Fatal("beta's intent-to-add file must not enter alpha's commit")
	}
	alpha := indexEntry(t, s, "skills/alpha/notes.md")
	if alpha.IntentToAdd || alpha.Hash != blobHash("alpha notes") {
		t.Fatalf("alpha's entry must be materialised: %+v", alpha)
	}
	beta := indexEntry(t, s, "skills/beta/notes.md")
	if !beta.IntentToAdd || beta.Hash != blobHash("") {
		t.Fatalf("beta's intent-to-add entry must be kept verbatim: %+v", beta)
	}
}

// The canonical `touch f && git add -N f`: the file is empty, so go-git's add
// has nothing to restage and the placeholder blob is the real content. The
// sweep records the empty file and clears the flag.
func TestSweepRecordsAnEmptyIntentToAddFile(t *testing.T) {
	s := scopedFixture(t)
	intentToAdd(t, s, "skills/alpha/empty.md", "")

	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}

	if got := commitFileContents(t, headCommit(t, s), "skills/alpha/empty.md"); got != "" {
		t.Fatalf("empty.md = %q, want empty", got)
	}
	entry := indexEntry(t, s, "skills/alpha/empty.md")
	if entry.IntentToAdd {
		t.Fatalf("the flag must be cleared once the empty file is recorded: %+v", entry)
	}
	if out, ok := gitStatusPorcelain(t, s); ok && strings.TrimSpace(out) != "" {
		t.Fatalf("git must agree the store is clean, got:\n%s", out)
	}
}

// A scoped no-op still materialises: `git rm --cached p && git add -N p`
// with p unchanged leaves the tree where it was, and the no-change return
// installs the entry with its real blob and no flag.
func TestPrepareCommitUnderMaterialisesIntentToAddAtTheNoOpReturn(t *testing.T) {
	s := scopedFixture(t)
	registerIntentToAdd(t, s, "skills/alpha/SKILL.md")
	before := mustHeadTree(t, s)

	prepared, err := s.PrepareCommitUnder([]string{"skills/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.ChangedPaths(); len(got) != 0 {
		t.Fatalf("changed = %v, want nothing: the content equals HEAD", got)
	}
	outcome, err := s.CommitPrepared("commit: alpha", prepared)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Written || outcome.IndexSkipped {
		t.Fatalf("outcome = %+v, want no commit and the index installed", outcome)
	}
	if after := mustHeadTree(t, s); after != before {
		t.Fatalf("HEAD tree moved: %s -> %s", before, after)
	}
	entry := indexEntry(t, s, "skills/alpha/SKILL.md")
	committed := commitFileContents(t, headCommit(t, s), "skills/alpha/SKILL.md")
	if entry.IntentToAdd || entry.Hash != blobHash(committed) {
		t.Fatalf("the entry must be materialised at the no-op return: %+v", entry)
	}
	if out, ok := gitStatusPorcelain(t, s); ok && strings.TrimSpace(out) != "" {
		t.Fatalf("git must agree the store is clean, got:\n%s", out)
	}
}

// An intent-to-add file that vanished from disk before the sweep is dropped
// from the index, as `git add -A` drops it, without any commit.
func TestSweepDropsAVanishedIntentToAddEntry(t *testing.T) {
	s := scopedFixture(t)
	before := headCommit(t, s).Hash
	intentToAdd(t, s, "skills/alpha/notes.md", "real content")
	if err := os.Remove(filepath.Join(s.Dir(), "skills", "alpha", "notes.md")); err != nil {
		t.Fatal(err)
	}

	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}

	if got := headCommit(t, s).Hash; got != before {
		t.Fatalf("no commit may be recorded for a placeholder that never had content: %s -> %s", before, got)
	}
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Entry("skills/alpha/notes.md"); err == nil {
		t.Fatal("the vanished intent-to-add entry must be dropped from the index")
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("the store must be clean after the sweep")
	}
	if out, ok := gitStatusPorcelain(t, s); ok && strings.TrimSpace(out) != "" {
		t.Fatalf("git must agree the store is clean, got:\n%s", out)
	}
}
