// internal/store/git_test.go
package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	indexformat "github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage"

	"github.com/cosensexyz/fu/internal/skill"
)

// Critical finding 3: AddWithOptions{All: true} goes through go-git's
// Worktree.Status(), which itself applies every .gitignore found inside
// the worktree. A file that has never been tracked and matches such a
// rule never appears in Status at all, so it is never staged and never
// committed -- not now, not on any later commit either, since nothing
// ever makes it a tracked (and therefore no-longer-ignorable) path.
// Reproduced against the compiled binary: after `skills/ign/.gitignore`
// (containing "secret.md") and `skills/ign/secret.md` existed on disk and
// a write command ran, `git -C $FU_HOME/store ls-files skills/ign` listed
// only .gitignore and SKILL.md, and `git status --short` was empty
// despite secret.md holding real content. This breaks SPEC §5.3 (every
// content change must enter history) and poisons skill.Digest's baseline
// comparison (DESIGN §3), since Digest hashes exactly the files a
// .gitignore would keep out of the store's own history.
func TestCommitIncludesGitignoredContent(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "ign")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.md"), []byte("TOP SECRET CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Commit("test: add ign"); err != nil {
		t.Fatal(err)
	}

	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("worktree must be clean once the gitignored file is actually committed")
	}
	assertTracked(t, s, "skills/ign/secret.md", "TOP SECRET CONTENT")
}

func TestChangedPathsIncludingIgnoredMatchesCommitProjection(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(s.SkillsDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	ignore := "/ignored-root.txt\n/skills/other/ignored.txt\n"
	if err := os.WriteFile(filepath.Join(s.Dir(), ".gitignore"), []byte(ignore), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "SKILL.md"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: add ignore projection fixture"); err != nil {
		t.Fatal(err)
	}

	ignored := map[string]string{
		"ignored-root.txt":         "root bytes",
		"skills/other/ignored.txt": "skill bytes",
	}
	for rel, content := range ignored {
		full := filepath.Join(s.Dir(), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nestedGit := filepath.Join(other, ".git", "private")
	if err := os.MkdirAll(filepath.Dir(nestedGit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedGit, []byte("metadata"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(paths, "\n"), "ignored-root.txt\nskills/other/ignored.txt"; got != want {
		t.Fatalf("complete change projection = %q, want %q", got, want)
	}
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range idx.Entries {
		if _, ignoredPath := ignored[entry.Name]; ignoredPath {
			t.Fatalf("side-effect-free projection must not stage %q", entry.Name)
		}
	}
}

func TestPreparedCommitFreezesIndexAndLeavesLaterFilesystemChangeUncommitted(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), ".gitignore"), []byte("late-*.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "late-before.txt"), []byte("prepared ignored content"), 0o644); err != nil {
		t.Fatal(err)
	}

	prepared, err := s.PrepareCommit()
	if err != nil {
		t.Fatal(err)
	}
	if prepared.TreeFingerprint() == "" {
		t.Fatal("prepared commit must carry a full-tree fingerprint")
	}
	if got := strings.Join(prepared.ChangedPaths(), ","); got != ".gitignore,late-before.txt" {
		t.Fatalf("prepared changed paths = %q, want sorted complete projection", got)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "late-after.txt"), []byte("arrived after preparation"), 0o644); err != nil {
		t.Fatal(err)
	}
	unstaged, err := s.UnstagedPathsIncludingIgnored(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(unstaged, ","); got != "late-after.txt" {
		t.Fatalf("unstaged projection = %q, want late-after.txt", got)
	}

	outcome, err := s.CommitPrepared("test: frozen candidate", prepared)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := s.Repo.CommitObject(outcome.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commit.File("late-after.txt"); err == nil {
		t.Fatal("filesystem content introduced after preparation must not enter the prepared commit")
	}
	assertTracked(t, s, "late-before.txt", "prepared ignored content")
}

// PrepareCommit may create blobs and freeze an immutable tree, but it must not
// publish that temporary candidate through the repository's public index. A
// direct Git user is allowed to inspect or commit that index throughout Fu's
// later validation and WAL writes and must never see Fu's uncommitted tree.
func TestPrepareCommitKeepsCandidateOutOfPublicIndex(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "intended.txt"), []byte("intended"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := indexFingerprint(t, s)

	prepared, err := s.PrepareCommit()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(prepared.ChangedPaths(), ","); got != "intended.txt" {
		t.Fatalf("private candidate changed paths = %q, want intended.txt", got)
	}
	if after := indexFingerprint(t, s); after != before {
		t.Fatalf("preparation exposed its candidate in the public index: got %s want %s", after, before)
	}
	status, err := mustWorktree(t, s).Status()
	if err != nil {
		t.Fatal(err)
	}
	if state := status["intended.txt"]; state.Staging != git.Untracked || state.Worktree != git.Untracked {
		t.Fatalf("direct Git sees prepared content as staged: %+v", state)
	}
}

func TestCommitPreparedPreservesChangedPublicIndex(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "intended.txt"), []byte("intended"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareCommit()
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "index-race.txt"), []byte("foreign index entry"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("index-race.txt"); err != nil {
		t.Fatal(err)
	}
	foreignFingerprint := indexFingerprint(t, s)
	outcome, err := s.CommitPrepared("test: changed index", prepared)
	if err != nil {
		t.Fatalf("a public-index change must not invalidate a private prepared tree: %v", err)
	}
	if !outcome.Written {
		t.Fatalf("private prepared tree was not committed: %+v", outcome)
	}
	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if after.Hash() == before.Hash() {
		t.Fatalf("private prepared tree did not move HEAD: before=%s after=%s", before.Hash(), after.Hash())
	}
	if got := indexFingerprint(t, s); got != foreignFingerprint {
		t.Fatalf("commit overwrote the direct Git writer's index: got %s want %s", got, foreignFingerprint)
	}
	commit, err := s.Repo.CommitObject(outcome.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commit.File("index-race.txt"); err == nil {
		t.Fatal("public-index content added after preparation entered the private candidate")
	}
}

func mustWorktree(t *testing.T, s *Store) *git.Worktree {
	t.Helper()
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	return wt
}

type replaceIndexAfterReadStorer struct {
	storage.Storer
	replaceAfter int
	reads        int
	replacement  *indexformat.Index
}

func (s *replaceIndexAfterReadStorer) Index() (*indexformat.Index, error) {
	idx, err := s.Storer.Index()
	if err != nil {
		return nil, err
	}
	s.reads++
	if s.reads == s.replaceAfter {
		if err := s.Storer.SetIndex(s.replacement); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

func TestCommitPreparedDoesNotRereadIndexAfterFinalValidation(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "intended.txt"), []byte("intended"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareCommit()
	if err != nil {
		t.Fatal(err)
	}
	preparedIndex, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "foreign.txt"), []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("foreign.txt"); err != nil {
		t.Fatal(err)
	}
	foreignIndex, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetIndex(preparedIndex); err != nil {
		t.Fatal(err)
	}

	// Each validation receives the exact prepared index. Immediately after
	// the final validation has received its snapshot, replace the durable
	// index so any later reread observes a foreign candidate.
	s.Repo.Storer = &replaceIndexAfterReadStorer{
		Storer:       s.Repo.Storer,
		replaceAfter: 2,
		replacement:  foreignIndex,
	}
	outcome, err := s.CommitPrepared("test: immutable prepared candidate", prepared)
	if err != nil {
		t.Fatalf("a late index replacement must not affect the immutable commit candidate: %v", err)
	}
	if !outcome.Written {
		t.Fatalf("prepared change was not committed: %+v", outcome)
	}
	commit, err := s.Repo.CommitObject(outcome.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commit.File("intended.txt"); err != nil {
		t.Fatalf("prepared file is absent from commit: %v", err)
	}
	if _, err := commit.File("foreign.txt"); err == nil {
		t.Fatal("index content introduced after final validation entered the commit")
	}
}

func TestPreparedCommitValidatesConfigAndOwnedTree(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store
	skillsRoot, err := checked.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Mkdir("alpha", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.WriteFile(filepath.Join("alpha", "SKILL.md"), []byte("owned bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshotOwnedTree(checked.writeRoots.skills, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := checked.PrepareCommit()
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(checked.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := checked.ValidatePreparedFile(prepared, "fu.yaml", configBytes); err != nil {
		t.Fatalf("exact prepared config must validate: %v", err)
	}
	if err := checked.ValidatePreparedOwnedTree(prepared, "skills/alpha", manifest); err != nil {
		t.Fatalf("exact prepared owned tree must validate: %v", err)
	}
	if err := checked.ValidatePreparedFile(prepared, "fu.yaml", append(configBytes, 'x')); err == nil {
		t.Fatal("wrong expected config bytes must be rejected")
	}
	changed := manifest
	changed.Entries = append([]OwnedTreeEntry(nil), manifest.Entries...)
	changed.Entries[0].Digest = strings.Repeat("0", len(changed.Entries[0].Digest))
	if err := checked.ValidatePreparedOwnedTree(prepared, "skills/alpha", changed); err == nil {
		t.Fatal("wrong expected payload digest must be rejected")
	}
}

// Once a previously-ignored file is tracked, ordinary Commit calls must
// pick up further edits to it exactly like any other tracked file -- no
// special force-add machinery should be needed a second time.
func TestCommitPicksUpEditToPreviouslyIgnoredFile(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "ign")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(secret, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: add ign"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(secret, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("editing a tracked (even if gitignored) file must be visible to IsDirty")
	}
	if _, err := s.Commit("test: edit ign"); err != nil {
		t.Fatal(err)
	}
	assertTracked(t, s, "skills/ign/secret.md", "v2")
}

// Deleting a previously-force-tracked ignored file must still be recorded
// as a removal, the same way Commit already handles deleting an ordinary
// (non-ignored) tracked file -- the new force-add path introduced for
// Critical finding 3 must not regress that existing behavior.
func TestCommitStillRecordsDeletionOfPreviouslyIgnoredFile(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "ign")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(secret, []byte("gone soon"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: add ign"); err != nil {
		t.Fatal(err)
	}
	assertTracked(t, s, "skills/ign/secret.md", "gone soon")

	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: remove secret"); err != nil {
		t.Fatal(err)
	}

	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("worktree must be clean after the deletion is committed")
	}
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
	if _, err := tree.File("skills/ign/secret.md"); err == nil {
		t.Fatal("deleted file must not still be present in the committed tree")
	}
}

// assertTracked confirms path is present in HEAD's tree with the exact
// given content, i.e. genuinely committed rather than merely absent from
// IsDirty's report.
func assertTracked(t *testing.T, s *Store, path, wantContent string) {
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
	f, err := tree.File(path)
	if err != nil {
		t.Fatalf("%s must be committed: %v", path, err)
	}
	got, err := f.Contents()
	if err != nil {
		t.Fatal(err)
	}
	if got != wantContent {
		t.Fatalf("%s committed content = %q, want %q", path, got, wantContent)
	}
}

func TestSweepAndLog(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirty, _ := s.IsDirty()
	if dirty {
		t.Fatal("fresh store must be clean")
	}
	if err := s.Sweep(); err != nil { // clean sweep is a no-op
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.Dir(), "hand-edit.md"), []byte("x"), 0o644)
	dirty, _ = s.IsDirty()
	if !dirty {
		t.Fatal("manual edit must make store dirty")
	}
	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Log(2)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Message != "external: manual modifications" {
		t.Fatalf("sweep commit missing, head is %q", entries[0].Message)
	}
	if entries[1].Message != "init: store" {
		t.Fatalf("unexpected log order: %+v", entries)
	}
}

// The public index is user state, including content no longer present in the
// worktree. With HEAD/worktree at A and the index at B, staging the worktree as
// part of dirty detection erased B and then reported the repository clean. A
// sweep must preserve both observable snapshots in history, in their order,
// and leave the public index synchronized with the final worktree state.
func TestSweepPreservesContentThatExistsOnlyInThePublicIndex(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(), "staged-only.txt")
	if err := os.WriteFile(path, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: baseline A"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := mustWorktree(t, s)
	if _, err := wt.Add("staged-only.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	finalCommit, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if got := commitFileContents(t, finalCommit, "staged-only.txt"); got != "A" {
		t.Fatalf("final swept worktree snapshot = %q, want A", got)
	}
	if finalCommit.Message != "external: manual modifications" || len(finalCommit.ParentHashes) != 1 {
		t.Fatalf("final external commit is malformed: message=%q parents=%v", finalCommit.Message, finalCommit.ParentHashes)
	}
	stagedCommit, err := s.Repo.CommitObject(finalCommit.ParentHashes[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := commitFileContents(t, stagedCommit, "staged-only.txt"); got != "B" {
		t.Fatalf("staged-only snapshot was lost from history: got %q want B", got)
	}
	if stagedCommit.Message != "external: manual modifications" {
		t.Fatalf("staged-only snapshot message = %q, want external attribution", stagedCommit.Message)
	}
	status, err := wt.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsClean() {
		t.Fatalf("sweep did not converge the public index and worktree: %s", status)
	}
}

func commitFileContents(t *testing.T, commit *object.Commit, name string) string {
	t.Helper()
	file, err := commit.File(name)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

// Round 3 finding 3: IsDirty was a bare wt.Status().IsClean(), which still
// reports clean for content matching a tracked .gitignore rule that has
// never itself been tracked -- gitignore only ever hides an *untracked*
// path from Status (see stageAll's own doc comment), so this content was
// invisible to the very check Sweep gates on, even though Commit's own
// stageAll (Critical finding 3) already force-adds it. Sweep therefore
// silently skipped the separate "external:" commit SPEC §5.3 requires, and
// the *next* write command's own Commit force-added the content under its
// own, unrelated message -- the user's manual edit misattributed as part
// of fu's own operation. Reproduced against the compiled binary: with
// skills/web/.gitignore (listing "node_modules/") already tracked, then
// hand-writing skills/web/node_modules/dep/index.js, `git status --short`
// in the store reported nothing, and a subsequent `fu enable other`
// recorded node_modules inside its own "enable: other" commit rather than
// a preceding "external: manual modifications" one.
//
// The reviewer's own note on why the previous round's test missed this:
// it pinned Commit, the entry point named in that report, while this
// requirement actually lives in Sweep/IsDirty -- so this test drives the
// scenario through Sweep followed by a separate Commit call (mirroring
// exactly what engine.Run's pipeline does: sweep external edits, then the
// operation's own commit), not through Commit alone.
func TestSweepCommitsGitignoredContentSeparatelyFromTheNextOperation(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The .gitignore itself must already be tracked before the scenario
	// begins, mirroring the reviewer's own setup ("skills/web/.gitignore
	// ... is tracked").
	if _, err := s.Commit("test: add web with gitignore"); err != nil {
		t.Fatal(err)
	}

	// The user hand-writes gitignored content directly -- never through fu.
	nm := filepath.Join(dir, "node_modules", "dep", "index.js")
	if err := os.MkdirAll(filepath.Dir(nm), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nm, []byte("module.exports = {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("gitignored content that was never tracked must still be reported dirty, not silently invisible to IsDirty")
	}

	// Mirrors engine.Run's own pipeline order: Sweep external edits, then
	// the operation's own mutation and Commit (here standing in for e.g.
	// "disable: other" editing fu.yaml). Without some real change of its
	// own, Commit would have nothing left to record (Sweep above already
	// claimed the gitignored content) and would correctly no-op via
	// git.ErrEmptyCommit -- exactly like a real op.Mutate, this write must
	// be genuine for the "next operation's commit" half of the scenario to
	// mean anything.
	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), []byte("version: 1\nskills: {}\n# other: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("disable: other"); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 || entries[0].Message != "disable: other" {
		t.Fatalf("unexpected log head: %+v", entries)
	}
	if entries[1].Message != "external: manual modifications" {
		t.Fatalf(`the manual edit must land in its own "external: manual modifications" commit, immediately before the next operation's, not be folded into it: %+v`, entries)
	}
	assertTracked(t, s, "skills/web/node_modules/dep/index.js", "module.exports = {}")
}

// Reverse direction: a store with genuinely nothing pending -- not even
// gitignored content -- must still produce no commit when swept, whether
// or not a tracked .gitignore is present. IsDirty sharing stageAll with
// Commit (the fix above) must not turn a real no-op into a spurious
// commit.
func TestSweepGenuinelyCleanStoreProducesNoCommitEvenWithGitignore(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: add web with gitignore"); err != nil {
		t.Fatal(err)
	}

	before, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("a store with nothing pending, gitignored or otherwise, must not be reported dirty")
	}
	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	after, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a genuinely clean store must produce no commit when swept: before=%d after=%d", len(before), len(after))
	}
}

func TestLogMoreEntriesThanExist(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Repository starts with 1 commit (init: store)
	entries, err := s.Log(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Message != "init: store" {
		t.Fatalf("unexpected message: %q", entries[0].Message)
	}
}

func TestExplainStagingFailureDoesNotMislabelGenericInvalidError(t *testing.T) {
	s := &Store{Home: t.TempDir()}
	err := s.explainStagingFailure(fs.ErrInvalid)
	if strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("generic invalid staging error was mislabelled: %v", err)
	}
	for _, want := range []string{"stage", s.Dir(), "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("staging error %q does not contain %q", err, want)
		}
	}
}

func TestLogSingleCommit(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.Log(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Message != "init: store" {
		t.Fatalf("unexpected message: %q", entries[0].Message)
	}
}

func TestLogNonPositiveCount(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Test with count = 0
	entries, err := s.Log(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for count=0, got %d", len(entries))
	}
	// Test with negative count
	entries, err = s.Log(-5)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for negative count, got %d", len(entries))
	}
}

// TestAbsoluteSymlinkInStoreIsNotSilentlyCorrupted is round 5's finding.
// go-git's worktree runs on a go-billy chroot filesystem whose Readlink
// rewrites an absolute target to "/" + filepath.Rel(base, target). The
// blob committed for a symlink therefore holds a path that is not what the
// link says on disk, and both directions are broken: a clone rebuilds a
// dangling link, and IsDirty keeps reporting the store dirty no matter how
// many times Sweep commits, because the same rewrite happens on every
// status computation. The corruption is silent -- nothing fails, nothing
// is printed.
//
// That matters here specifically because the store's git history is SPEC
// §9's stated safety net for store content, and DESIGN §4's premise that
// "sweep keeps the worktree normally clean" is what the baseline three-way
// comparison rests on. It is reachable inside this plan: scenario 7 is `fu
// new` followed by editing the skill directly, and hand-adding a symlink
// there is exactly the kind of manual edit Sweep exists to capture.
//
// Detecting and refusing is the fix this round ships; repairing the blob
// would mean bypassing go-git's staging *and* its status computation, which
// is a larger change. See DESIGN §6's known-gap entry.
func TestAbsoluteSymlinkInStoreIsNotSilentlyCorrupted(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "abs")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	_, err = s.Commit("test: absolute symlink")
	if err == nil {
		t.Fatalf("committing an absolute symlink under the store must be refused explicitly, "+
			"not silently recorded with a rewritten target (%s -> %s)", link, outside)
	}
	// The refusal has to be actionable on its own: the user has no other
	// way to find out which entry is at fault.
	if !strings.Contains(err.Error(), link) {
		t.Fatalf("the refusal must name the offending path, got %v", err)
	}

	// Sweep shares stageAll with Commit, so it must refuse for the same
	// reason rather than looping on a store it can never make clean.
	if err := s.Sweep(); err == nil {
		t.Fatal("Sweep must surface the same refusal instead of never converging")
	}

	// A relative symlink is unaffected: go-billy's chroot passes those
	// through untouched, so nothing is corrupted and nothing is refused.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "rel")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: relative symlink"); err != nil {
		t.Fatalf("a relative symlink under the store must still commit normally: %v", err)
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("after committing, a store holding only a relative symlink must be clean")
	}
}

// TestDigestSurvivesCommitAndClone is round 6's projection finding. DESIGN
// §3 claims one normalized projection is shared by copying, digesting and
// history -- so a skill's digest must be the same in the store's worktree
// and in a fresh clone of the store. It was not, in two ways:
//
//   - the projection recorded a "D" record per directory, but Git cannot
//     store an empty directory at all, so a skill holding one digested
//     differently after a clone, forever;
//
// The separate ProjectDir/DigestFS consistency tests cover .git entries,
// including the required refusal of a .git symlink.
//
// Either one makes digest(store) differ from the recorded baseline on any
// machine that obtained the store by cloning, which is the comparison
// DESIGN §3's three-state baseline rule rests on.
func TestDigestSurvivesCommitAndClone(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An empty directory: real content a user can create, that Git drops.
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory holding only another empty directory -- Git drops the
	// whole branch, so a projection counting directories disagrees twice.
	if err := os.MkdirAll(filepath.Join(dir, "outer", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A relative symlink Git *does* persist, so this one must stay in the
	// digest -- the fix must not exclude symlinks wholesale.
	if err := os.Symlink("SKILL.md", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}

	before, err := skill.Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("test: alpha"); err != nil {
		t.Fatal(err)
	}

	clonePath := filepath.Join(t.TempDir(), "clone")
	if _, err := git.PlainClone(clonePath, false, &git.CloneOptions{URL: s.Dir()}); err != nil {
		t.Fatal(err)
	}
	after, err := skill.Digest(filepath.Join(clonePath, "skills", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("the projection must be defined in terms git can persist, so a skill digests "+
			"identically in the store and in a fresh clone of it (DESIGN §3):\n  store %s\n  clone %s",
			before, after)
	}
}

// TestCommitDetectsConcurrentBranchUpdate is round 6's history-integrity
// finding. The fu lock serializes fu processes but says nothing about direct
// Git, which DESIGN §4 presents as a supported recovery and power-user path.
// The prepared commit must publish through a compare-and-swap so it detects
// the move without replacing the direct actor's commit.
func TestCommitDetectsConcurrentBranchUpdate(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.SkillsDir(), "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stand in for a concurrent direct-git commit: move the branch to a
	// commit fu has never seen, between fu capturing HEAD and writing.
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	var externalHash plumbing.Hash
	outcome, err := s.commitWithHook("test: alpha", func() {
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
		externalHash = h
		if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(before.Name(), h)); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("a branch that moved under fu mid-commit must be reported, not silently overwritten")
	}
	if !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("the report must name what happened, got %v", err)
	}
	if outcome.Written || !outcome.Hash.IsZero() {
		t.Fatalf("failed compare-and-swap must not report a published commit, got %+v", outcome)
	}
	current, err := s.Repo.Reference(before.Name(), true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Hash() != externalHash {
		t.Fatalf("concurrent branch target was overwritten: got %s want %s", current.Hash(), externalHash)
	}
}

type moveAfterReferenceCASStorer struct {
	storage.Storer
	branch      plumbing.ReferenceName
	replacement plumbing.Hash
	armed       bool
}

func (s *moveAfterReferenceCASStorer) CheckAndSetReference(ref, old *plumbing.Reference) error {
	if err := s.Storer.CheckAndSetReference(ref, old); err != nil {
		return err
	}
	if s.armed && ref.Name() == s.branch {
		s.armed = false
		return s.Storer.SetReference(plumbing.NewHashReference(s.branch, s.replacement))
	}
	return nil
}

// A compare-and-swap only controls the instant it writes. This wrapper moves
// the branch immediately after that succeeds, so Commit must also verify the
// branch's final target.
func TestCommitDetectsBranchMoveAfterItsOwnRefWrite(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "change.txt"), []byte("fu"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	external := &object.Commit{
		Author:       *fuSignature(),
		Committer:    *fuSignature(),
		Message:      "external: after fu ref write",
		TreeHash:     mustHeadTree(t, s),
		ParentHashes: []plumbing.Hash{before.Hash()},
	}
	obj := s.Repo.Storer.NewEncodedObject()
	if err := external.Encode(obj); err != nil {
		t.Fatal(err)
	}
	externalHash, err := s.Repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	s.Repo.Storer = &moveAfterReferenceCASStorer{
		Storer:      s.Repo.Storer,
		branch:      before.Name(),
		replacement: externalHash,
		armed:       true,
	}

	outcome, err := s.Commit("test: fu commit")
	if err == nil {
		t.Fatal("Commit must report a branch move that lands after its own ref write")
	}
	if !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("error must identify the concurrent branch move, got %v", err)
	}
	if !outcome.Written || outcome.Hash.IsZero() {
		t.Fatalf("post-write ref verification error must report the written commit, got %+v", outcome)
	}
	current, err := s.Repo.Reference(before.Name(), true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Hash() != externalHash {
		t.Fatalf("verification must not overwrite the later branch target: got %s want %s", current.Hash(), externalHash)
	}
}

func mustHeadTree(t *testing.T, s *Store) plumbing.Hash {
	t.Helper()
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c.TreeHash
}

func indexFingerprint(t *testing.T, s *Store) string {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := preparedEntriesFromIndex(idx.Entries)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprintPreparedEntries(entries)
}

// A failed commit must leave the public index byte-for-byte where it began.
// Private staging makes this invariant hold without a compensating write.
func TestFailedCommitNeverPublishesPrivateStagingToPublicIndex(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store
	skillsRoot, err := checked.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Mkdir("alpha", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.WriteFile(filepath.Join("alpha", "SKILL.md"), []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := indexFingerprint(t, checked)

	// Moving the branch under the captured reference makes the publishing CAS
	// fail after staging has already happened.
	_, err = checked.commitWithHook("new: alpha", func() {
		head, headErr := checked.Repo.Head()
		if headErr != nil {
			t.Fatal(headErr)
		}
		moved := plumbing.NewHashReference(head.Name(), plumbing.NewHash(strings.Repeat("0", 39)+"1"))
		if setErr := checked.Repo.Storer.SetReference(moved); setErr != nil {
			t.Fatal(setErr)
		}
	})
	if err == nil {
		t.Fatal("a commit whose branch moved under it must fail")
	}
	if after := indexFingerprint(t, checked); after != before {
		t.Fatalf("a failed commit exposed private staging: index %s, want %s", after, before)
	}
}

// Capturing the public-index baseline under .git/index.lock prevents a torn
// read while a supported direct Git process replaces it. Private staging runs
// only after that short capture releases the lock.
func TestPrepareCommitRespectsTheGitIndexLock(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store

	lock := filepath.Join(s.Dir(), ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := indexFingerprint(t, checked)

	if _, err := checked.PrepareCommit(); err == nil {
		t.Fatal("preparation must not proceed while another Git process holds the index lock")
	} else if !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("the error must name the lock so it can be cleared, got %v", err)
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Fatalf("a lock fu did not take must not be released by fu: %v", err)
	}
	if after := indexFingerprint(t, checked); after != before {
		t.Fatalf("a refused preparation must not stage anything: index %s, want %s", after, before)
	}
}

// A symlink inside the store must round-trip through the session filesystem
// view, which is the one every write command uses.
//
// rootStandardFS implemented ReadLink but not Lstat, and fs.ReadLinkFS requires
// both, so the interface assertion failed and fs.ReadLink returned ErrInvalid
// for every symlink. Sweep failed with a bare "invalid argument" naming neither
// the store nor a remedy, and since every write command sweeps first, one
// hand-made link -- scenario 7, in plan today -- disabled all of them. The
// absolute-target refusal below could never fire either: it sits behind the
// same failing call.
func TestSessionCommitsRelativeSymlinksAndConverges(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store
	skillsRoot, err := checked.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Mkdir("alpha", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.WriteFile(filepath.Join("alpha", "SKILL.md"), []byte("real content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Symlink("SKILL.md", filepath.Join("alpha", "rel")); err != nil {
		t.Fatal(err)
	}

	if err := checked.Sweep(); err != nil {
		t.Fatalf("a relative symlink in the store must sweep: %v", err)
	}
	// Converged: the link is committed and nothing is left pending. Without a
	// real Size() the noder hashes a different blob every time, so Status keeps
	// reporting it modified and no write command can ever finish.
	dirty, err := checked.IsDirty()
	if err != nil {
		t.Fatalf("IsDirty after sweep: %v", err)
	}
	if dirty {
		t.Error("the store must converge after committing a symlink")
	}
	prepared, err := checked.PrepareCommit()
	if err != nil {
		t.Fatalf("preparing after a committed symlink: %v", err)
	}
	if unstaged, err := checked.UnstagedPathsIncludingIgnored(prepared); err != nil {
		t.Fatal(err)
	} else if len(unstaged) != 0 {
		t.Errorf("symlink left unprepared paths %q", unstaged)
	}
	head, err := checked.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := checked.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	file, err := commit.File("skills/alpha/rel")
	if err != nil {
		t.Fatalf("the symlink must reach history: %v", err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	if contents != "SKILL.md" {
		t.Fatalf("committed symlink target = %q, want %q", contents, "SKILL.md")
	}
}

// The documented refusal has to be the one the user actually sees.
func TestSessionRefusesAbsoluteSymlinkWithItsNamedError(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store
	skillsRoot, err := checked.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Mkdir("alpha", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Symlink(filepath.Join(home, "outside"), filepath.Join("alpha", "abs")); err != nil {
		t.Fatal(err)
	}

	err = checked.Sweep()
	if !errors.Is(err, ErrAbsoluteSymlink) {
		t.Fatalf("an absolute symlink must be refused by name, got %v", err)
	}
	if !strings.Contains(err.Error(), "abs") {
		t.Fatalf("the refusal must name the offending entry, got %v", err)
	}
}

// Dirty detection needs no read permission, and must not claim any.
//
// statEntryNoFollow classified each entry with fstatat -- which needs no read
// permission -- and then opened every regular file purely to Stat it again. An
// EACCES there aborted the whole directory walk, so even asking "is the store
// dirty" failed on a worktree plain `git status` handles. The open bought
// nothing: the special-file refusal it exists for already happened at the
// fstatat, and the guarantee is re-established at real read time by
// openReadOnlyRootFile and hashFileAt.
//
// Staging is a different matter and this test deliberately says nothing about
// it -- see TestUnreadableFileFailsStagingWithGuidance.
func TestUnreadableFileDoesNotBreakDirtyDetection(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(s.Dir(), "skills", "alpha")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(skills, "secret.txt")
	if err := os.WriteFile(secret, []byte("unreadable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o644) })

	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store

	if _, err := checked.IsDirty(); err != nil {
		t.Fatalf("an unreadable file must not break dirty detection: %v", err)
	}
	if _, err := checked.ChangedPathsIncludingIgnored(); err != nil {
		t.Fatalf("an unreadable file must not break the change projection: %v", err)
	}
}

// The limitation the fix above does not remove, pinned so it is a documented
// property rather than a surprise: staging has to read every file, so one
// unreadable file does stop a write command. `git add -A` behaves the same way
// (exit 128, "unable to index file"). What must not happen is a bare errno.
func TestUnreadableFileFailsStagingWithGuidance(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(s.Dir(), "skills", "alpha")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(skills, "secret.bin")
	if err := os.WriteFile(secret, []byte("unreadable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o644) })

	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	err = session.Store.Sweep()
	if err == nil {
		t.Fatal("staging cannot succeed without reading every file")
	}
	if !strings.Contains(err.Error(), secret) {
		t.Errorf("the error must name the offending file in full, got: %v", err)
	}
	if !strings.Contains(err.Error(), "readable") {
		t.Errorf("the error must say what to do about it, got: %v", err)
	}
}

// The store projection, exercised on the view production actually uses.
//
// storeFiles() returns rootStandardFS inside a write session and os.DirFS
// outside one, and almost every projection test used the latter. That is the
// hole Critical #1 fell through: a defect present only in the session view was
// invisible, and DESIGN was then written from the passing os.DirFS result. Each
// case below commits through BeginWrite() and requires the committed tree to
// match what is on disk.
func TestSessionProjectionCommitsEveryEntryKind(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store
	skillsRoot, err := checked.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Mkdir("alpha", 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel string, data []byte, perm os.FileMode) {
		t.Helper()
		if err := skillsRoot.MkdirAll(filepath.Dir(filepath.Join("alpha", rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := skillsRoot.WriteFile(filepath.Join("alpha", rel), data, perm); err != nil {
			t.Fatal(err)
		}
	}
	write("SKILL.md", []byte("scaffold"), 0o644)
	// Ignored content: DESIGN §6 requires it in history anyway, or a fresh
	// clone rebuilds an incomplete skill and the digest disagrees forever.
	write(".gitignore", []byte("secret.txt\n"), 0o644)
	write("secret.txt", []byte("ignored but recorded"), 0o644)
	// A nested .git is excluded at any depth, deliberately and identically by
	// walkStoreFiles and skill.Digest. What matters is that excluding it does
	// not leave the store permanently dirty -- asserted after the commit.
	write(filepath.Join("vendor", ".git", "config"), []byte("[core]\n"), 0o644)
	write(filepath.Join("vendor", "real.txt"), []byte("kept"), 0o644)
	write("run.sh", []byte("#!/bin/sh\n"), 0o755)
	write("doomed.txt", []byte("removed before the second commit"), 0o644)

	if _, err := checked.Commit("new: alpha"); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	want := map[string]string{
		"skills/alpha/SKILL.md":        "scaffold",
		"skills/alpha/.gitignore":      "secret.txt\n",
		"skills/alpha/secret.txt":      "ignored but recorded",
		"skills/alpha/vendor/real.txt": "kept",
		"skills/alpha/run.sh":          "#!/bin/sh\n",
		"skills/alpha/doomed.txt":      "removed before the second commit",
	}
	head, err := checked.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := checked.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commit.File("skills/alpha/vendor/.git/config"); err == nil {
		t.Error("a nested .git must stay out of history at any depth")
	}
	for path, contents := range want {
		file, err := commit.File(path)
		if err != nil {
			t.Errorf("%s must be committed through the session view: %v", path, err)
			continue
		}
		got, err := file.Contents()
		if err != nil {
			t.Fatal(err)
		}
		if got != contents {
			t.Errorf("%s = %q, want %q", path, got, contents)
		}
		if path == "skills/alpha/run.sh" && file.Mode.String() != "0100755" {
			t.Errorf("exec bit lost through the session view: mode %s", file.Mode)
		}
	}

	// Mode-only change and a deletion, both through the session view.
	if err := skillsRoot.Chmod(filepath.Join("alpha", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skillsRoot.Remove(filepath.Join("alpha", "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := checked.Commit("external: manual modifications"); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	head, err = checked.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err = checked.Repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if file, err := commit.File("skills/alpha/SKILL.md"); err != nil {
		t.Fatal(err)
	} else if file.Mode.String() != "0100755" {
		t.Errorf("mode-only change not projected: SKILL.md mode %s, want 0100755", file.Mode)
	}
	if _, err := commit.File("skills/alpha/doomed.txt"); err == nil {
		t.Error("a deletion was not projected through the session view")
	}
	dirty, err := checked.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Error("the session projection must converge after committing every entry kind")
	}
}
