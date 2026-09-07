package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestRemoteIsUnsetUntilSetAndReportsThePreviousURLOnOverwrite(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	url, configured, err := s.Remote()
	if err != nil || configured || url != "" {
		t.Fatalf("a fresh store must have no remote, got url=%q configured=%v err=%v", url, configured, err)
	}
	previous, err := s.SetRemote("/srv/one.git")
	if err != nil || previous != "" {
		t.Fatalf("the first SetRemote must report no previous url, got %q err=%v", previous, err)
	}
	url, configured, err = s.Remote()
	if err != nil || !configured || url != "/srv/one.git" {
		t.Fatalf("Remote after SetRemote = %q %v %v", url, configured, err)
	}
	previous, err = s.SetRemote("ssh://git@example.com/two.git")
	if err != nil || previous != "/srv/one.git" {
		t.Fatalf("an overwrite must report the replaced url, got %q err=%v", previous, err)
	}
	reopened, err := Open(s.Home)
	if err != nil {
		t.Fatal(err)
	}
	url, _, err = reopened.Remote()
	if err != nil || url != "ssh://git@example.com/two.git" {
		t.Fatalf("the remote must persist in .git/config, got %q err=%v", url, err)
	}
}

func TestSetRemoteRejectsAnEmptyOrMalformedURL(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"", "   ", "http://[::1"} {
		if _, err := s.SetRemote(url); err == nil {
			t.Fatalf("SetRemote(%q) must fail", url)
		}
	}
	if _, configured, _ := s.Remote(); configured {
		t.Fatal("a rejected url must leave the remote unconfigured")
	}
}

func TestSetRemoteAnchorsRelativePathsAndPreservesURLs(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, want string }{
		{"./R.git", filepath.Join(cwd, "R.git")},
		{"../R.git/", filepath.Join(filepath.Dir(cwd), "R.git")},
		{"./remote with spaces.git", filepath.Join(cwd, "remote with spaces.git")},
		{"/srv/R.git/", "/srv/R.git/"},
		{"file:///srv/R.git", "file:///srv/R.git"},
		{"ssh://git@example.com/R.git", "ssh://git@example.com/R.git"},
		{"https://example.com/R.git", "https://example.com/R.git"},
		{"git://example.com/R.git", "git://example.com/R.git"},
		{"git@example.com:R.git", "git@example.com:R.git"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if _, err := s.SetRemote(tc.input); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(s.Home)
			if err != nil {
				t.Fatal(err)
			}
			url, configured, err := reopened.Remote()
			if err != nil || !configured || url != tc.want {
				t.Fatalf("saved remote = %q configured=%v err=%v, want %q", url, configured, err, tc.want)
			}
		})
	}
}

func TestCurrentBranchIsTheBranchHeadPointsAt(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.currentBranch()
	if err != nil || branch != head.Name() {
		t.Fatalf("currentBranch = %q err=%v, want %q", branch, err, head.Name())
	}
	if branch != plumbing.NewBranchReferenceName(branch.Short()) {
		t.Fatalf("currentBranch must be a branch reference, got %q", branch)
	}
}

// newBareRemote is the remote every test here pushes to: a bare repository
// on a local path, reached through go-git's file transport (which runs
// git-upload-pack / git-receive-pack as subprocesses, so git must be on PATH,
// exactly as add_test.go already assumes).
func newBareRemote(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	return bare
}

// commitFile writes content to rel inside the store and commits it as msg.
func commitFile(t *testing.T, s *Store, rel, content, msg string) plumbing.Hash {
	t.Helper()
	path := filepath.Join(s.Dir(), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.Commit(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written {
		t.Fatalf("commit %q wrote nothing", msg)
	}
	return outcome.Hash
}

func remoteBranchHash(t *testing.T, bare, branch string) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("read %s on the remote: %v", branch, err)
	}
	return ref.Hash()
}

func storeWithRemote(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := newBareRemote(t)
	if _, err := s.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	return s, bare
}

const alphaSkill = "---\nname: alpha\ndescription: the alpha skill\n---\n\nbody\n"

func TestPushCreatesTheBranchOnAnEmptyRemoteAndIsThenUpToDate(t *testing.T) {
	s, bare := storeWithRemote(t)
	out, err := s.Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if out.UpToDate || out.Branch != head.Name().Short() || out.Head != head.Hash() {
		t.Fatalf("first push outcome = %+v, want branch %s at %s", out, head.Name().Short(), head.Hash())
	}
	if got := remoteBranchHash(t, bare, out.Branch); got != head.Hash() {
		t.Fatalf("remote %s = %s, want %s", out.Branch, got, head.Hash())
	}
	out, err = s.Push(context.Background())
	if err != nil || !out.UpToDate {
		t.Fatalf("a second push must be up to date, got %+v err=%v", out, err)
	}
}

func TestPushRefusesWhenTheRemoteIsAhead(t *testing.T) {
	a, bare := storeWithRemote(t)
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Divergence has to be structural rather than a matter of timing: two Init
	// stores write the same bootstrap config and sign with the same fixed
	// identity, so roots created in the same second collide. Giving each side a
	// commit the other lacks makes the push a non-fast-forward either way.
	commitFile(t, a, "notes-a.md", "a has its own history", "new: a")
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	commitFile(t, b, "notes-b.md", "b has its own history", "new: b")
	if _, err := b.Push(context.Background()); !errors.Is(err, ErrRemoteDiverged) {
		t.Fatalf("pushing over a remote that is ahead must report ErrRemoteDiverged, got %v", err)
	}
	aHead, err := a.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteBranchHash(t, bare, aHead.Name().Short()); got != aHead.Hash() {
		t.Fatalf("a refused push must leave the remote untouched, got %s want %s", got, aHead.Hash())
	}
}

func TestFetchWritesOnlyRemoteTrackingReferences(t *testing.T) {
	a, bare := storeWithRemote(t)
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := commitFile(t, a, "skills/alpha/SKILL.md", alphaSkill, "new: alpha")
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	before, err := b.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := b.Fetch(context.Background())
	if err != nil || !updated {
		t.Fatalf("the first fetch must bring something, got updated=%v err=%v", updated, err)
	}
	tracking, err := b.Repo.Reference(plumbing.NewRemoteReferenceName(remoteName, before.Name().Short()), true)
	if err != nil || tracking.Hash() != second {
		t.Fatalf("fetch must record the remote head under refs/remotes/%s, got %v err=%v", remoteName, tracking, err)
	}
	after, err := b.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if after.Hash() != before.Hash() {
		t.Fatal("fetch must not move the local branch")
	}
	if _, err := os.Stat(filepath.Join(b.SkillsDir(), "alpha")); !os.IsNotExist(err) {
		t.Fatal("fetch must not touch the worktree")
	}
	updated, err = b.Fetch(context.Background())
	if err != nil || updated {
		t.Fatalf("a second fetch must report nothing new, got updated=%v err=%v", updated, err)
	}
}

func TestFetchReportsAnEmptyRemote(t *testing.T) {
	s, _ := storeWithRemote(t)
	if _, err := s.Fetch(context.Background()); !errors.Is(err, ErrRemoteEmpty) {
		t.Fatalf("fetching an empty bare repository must report ErrRemoteEmpty, got %v", err)
	}
}

func assertNoCloneScratch(t *testing.T, home string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "staging"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), cloneScratchPrefix) {
			t.Fatalf("clone scratch %s must not survive", entry.Name())
		}
	}
}

func assertNoStore(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(home, "store")); !os.IsNotExist(err) {
		t.Fatalf("store/ must not exist under %s, Lstat err=%v", home, err)
	}
}

// pushedStore is a store with one skill pushed to a fresh bare remote.
func pushedStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, bare := storeWithRemote(t)
	commitFile(t, s, "skills/alpha/SKILL.md", alphaSkill, "new: alpha")
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, bare
}

func TestCloneRefusesAnExistingStoreDirectory(t *testing.T) {
	_, bare := pushedStore(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Clone(context.Background(), home, bare); !errors.Is(err, ErrStoreExists) {
		t.Fatalf("clone over an existing store/ must report ErrStoreExists, got %v", err)
	}
	assertNoCloneScratch(t, home)
}

func TestCloneRejectsARepositoryThatIsNotAFuStore(t *testing.T) {
	bare := newBareRemote(t)
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("not a store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("not a store", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(head.Name().String() + ":" + head.Name().String())},
	}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	_, err = Clone(context.Background(), home, bare)
	if err == nil || !strings.Contains(err.Error(), "not a fu store") {
		t.Fatalf("a repository without fu.yaml must be refused as not a fu store, got %v", err)
	}
	assertNoStore(t, home)
	assertNoCloneScratch(t, home)
}

func TestCloneRejectsAnEmptyRemote(t *testing.T) {
	bare := newBareRemote(t)
	home := t.TempDir()
	if _, err := Clone(context.Background(), home, bare); !errors.Is(err, ErrRemoteEmpty) {
		t.Fatalf("cloning an empty remote must report ErrRemoteEmpty, got %v", err)
	}
	assertNoStore(t, home)
	assertNoCloneScratch(t, home)
}

func TestCloneRecreatesTheSkillsDirectoryAndOpens(t *testing.T) {
	// An Init-only store: skills/ is empty, so git tracked nothing under it
	// and the clone arrives without the directory.
	src, bare := storeWithRemote(t)
	if _, err := src.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cloned, err := Clone(context.Background(), home, bare)
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Dir() != filepath.Join(home, "store") {
		t.Fatalf("cloned store dir = %s", cloned.Dir())
	}
	info, err := os.Stat(cloned.SkillsDir())
	if err != nil || !info.IsDir() {
		t.Fatalf("clone must recreate skills/, got err=%v", err)
	}
	url, configured, err := cloned.Remote()
	if err != nil || !configured || url != bare {
		t.Fatalf("clone must record its origin, got %q %v %v", url, configured, err)
	}
	entries, err := cloned.Log(1)
	if err != nil || len(entries) != 1 || entries[0].Message != "init: store" {
		t.Fatalf("clone must carry the history, got %+v err=%v", entries, err)
	}
	srcHead, err := src.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	clonedHead, err := cloned.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if clonedHead.Hash() != srcHead.Hash() || clonedHead.Name() != srcHead.Name() {
		t.Fatalf("clone head = %v, want %v", clonedHead, srcHead)
	}
	assertNoCloneScratch(t, home)
	if _, err := Init(home); !errors.Is(err, ErrStoreExists) {
		t.Fatalf("Init after Clone must see an existing store, got %v", err)
	}
}

// pushTreeToBareRemote builds a fresh work repo whose files are laid out by
// build, commits it, and pushes it to a fresh bare remote. It is the fixture
// the layout and absolute-symlink clone tests share: a HEAD tree shaped by
// hand rather than by fu, which is exactly what a hand-edited or malicious
// remote can advertise. Unlike TestCloneRejectsARepositoryThatIsNotAFuStore,
// build may plant a symlink; a real filesystem is what makes that possible
// (go-git's worktree here runs on osfs, so a symlink build creates on disk is
// staged as a symlink blob, the same as real git would).
func pushTreeToBareRemote(t *testing.T, build func(root string)) string {
	t.Helper()
	bare := newBareRemote(t)
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	build(work)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("not fu's own layout", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(head.Name().String() + ":" + head.Name().String())},
	}); err != nil {
		t.Fatal(err)
	}
	return bare
}

// Defect 1 (Critical): Open enforces the store layout contract --
// storeDirEntries pins "skills" to a directory -- but the verification Clone
// ran before its rename checked only that HEAD resolves and tracks fu.yaml.
// A remote whose HEAD tree holds "skills" as a symlink therefore cloned and
// renamed into place, and only the subsequent Open call refused it, leaving a
// store/ that every later fu command (including a second clone or init)
// tripped over with no remedy. checkStoreLayout must run before the rename,
// exactly as Open runs it after.
func TestCloneRejectsARemoteWhoseSkillsIsASymlinkAndLeavesNoStore(t *testing.T) {
	bare := pushTreeToBareRemote(t, func(root string) {
		if err := os.WriteFile(filepath.Join(root, "fu.yaml"), []byte("version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A real, tracked directory the symlink below can resolve through:
		// git tracks no empty directory, so it needs a file inside to survive
		// the clone's checkout, and Clone's own MkdirAll(skills) must not
		// silently succeed by following the link to it.
		if err := os.MkdirAll(filepath.Join(root, "elsewhere"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "elsewhere", ".keep"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		// A symlink at "skills" rather than a directory: exactly the shape
		// storeDirEntries (store.go) refuses, and Open would refuse it too --
		// but only after Clone had already renamed it into $FU_HOME/store.
		if err := os.Symlink("elsewhere", filepath.Join(root, "skills")); err != nil {
			t.Fatal(err)
		}
	})
	home := t.TempDir()
	_, err := Clone(context.Background(), home, bare)
	if err == nil || !strings.Contains(err.Error(), "not a fu store") {
		t.Fatalf("a remote whose skills is a symlink must be refused as not a fu store, got %v", err)
	}
	assertNoStore(t, home)
	assertNoCloneScratch(t, home)
}

// Defect 2 (Important): every write path refuses a store holding a symlink
// with an absolute target via ErrAbsoluteSymlink, because go-git's chroot
// filesystem rewrites such a target on checkout, so history cannot record it
// faithfully (worktree_apply.go:250-256). Clone had no such check, so a
// remote holding one cloned successfully and then wedged every later write
// command against a link the user could only remove by hand.
func TestCloneRefusesAnAbsoluteSymlinkAndLeavesNoStore(t *testing.T) {
	bare := pushTreeToBareRemote(t, func(root string) {
		if err := os.WriteFile(filepath.Join(root, "fu.yaml"), []byte("version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/passwd", filepath.Join(root, "skills", "evil")); err != nil {
			t.Fatal(err)
		}
	})
	home := t.TempDir()
	_, err := Clone(context.Background(), home, bare)
	if !errors.Is(err, ErrAbsoluteSymlink) {
		t.Fatalf("a remote holding an absolute symlink must be refused via ErrAbsoluteSymlink, got %v", err)
	}
	if !strings.Contains(err.Error(), "evil") {
		t.Fatalf("the refusal must name the offending path, got %v", err)
	}
	assertNoStore(t, home)
	assertNoCloneScratch(t, home)
}

// Defect 3 (Important): PlainCloneContext follows the remote's HEAD with no
// ReferenceName override. A bare remote created with `git init --bare`
// defaults its HEAD to the branch init.defaultBranch names (typically
// "main"), which stays unborn until the first push. fu's own stores push
// "master" (go-git's PlainInit default), so a remote fu itself populated can
// still advertise a HEAD naming a branch that was never created. Real git
// clone tolerates this with a warning; go-git's clone hard-fails with a bare
// "reference not found". This can only be reproduced with a bare repository
// the git binary created: go-git's own PlainInit sets HEAD to "master" too,
// so every fixture built with it agrees with the implementation by
// construction and can never observe the bug.
func TestCloneRetriesWhenTheRemoteHeadNamesABranchItDoesNotHave(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("this test requires the git binary: %v", err)
	}
	bare := t.TempDir()
	cmd := exec.Command(gitPath, "-c", "init.defaultBranch=main", "init", "--bare", bare)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, output)
	}
	src, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	// fu's own store pushes "master" (its current branch); the bare remote's
	// HEAD still names the unborn "main" that `git init --bare` gave it.
	if _, err := src.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cloned, err := Clone(context.Background(), home, bare)
	if err != nil {
		t.Fatalf("clone must recover when the remote advertises exactly one branch, got %v", err)
	}
	srcHead, err := src.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	clonedHead, err := cloned.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if clonedHead.Name() != srcHead.Name() || clonedHead.Hash() != srcHead.Hash() {
		t.Fatalf("clone head = %v, want %v", clonedHead, srcHead)
	}
	assertNoCloneScratch(t, home)
}

// The multi-branch half of defect 3: when the remote's HEAD names a branch
// that does not exist and more than one real branch could be meant instead,
// fu clone cannot guess, and must fail with a message naming what it found.
func TestCloneFailsClearlyWhenTheRemoteHeadIsAmbiguous(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("this test requires the git binary: %v", err)
	}
	bare := t.TempDir()
	if output, err := exec.Command(gitPath, "-c", "init.defaultBranch=main", "init", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, output)
	}
	src, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A second branch on the remote, pointing at the same commit: HEAD still
	// names the unborn "main", and now two real branches could be meant.
	if output, err := exec.Command(gitPath, "--git-dir="+bare, "update-ref", "refs/heads/other", "refs/heads/master").CombinedOutput(); err != nil {
		t.Fatalf("git update-ref: %v: %s", err, output)
	}
	home := t.TempDir()
	_, err = Clone(context.Background(), home, bare)
	if err == nil {
		t.Fatal("clone must fail when the remote's branches are ambiguous")
	}
	if !strings.Contains(err.Error(), "master") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("the refusal must name the branches found, got %v", err)
	}
	assertNoStore(t, home)
	assertNoCloneScratch(t, home)
}

func TestCloneRemovesItsScratchWhenTheRenameIsRefused(t *testing.T) {
	_, bare := pushedStore(t)
	home := t.TempDir()
	boom := errors.New("boom")
	_, err := cloneWithHooks(context.Background(), home, bare, cloneHooks{beforeRename: func() error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatalf("the hook's error must surface, got %v", err)
	}
	assertNoStore(t, home)
	assertNoCloneScratch(t, home)
}

const (
	cloneCrashHelperEnv = "FU_TEST_CRASH_CLONE_HELPER"
	cloneCrashHomeEnv   = "FU_TEST_CRASH_CLONE_HOME"
	cloneCrashURLEnv    = "FU_TEST_CRASH_CLONE_URL"
	cloneCrashExitCode  = 87
)

// TestCloneCrashHelper is the child half of the crash test below: it runs a
// clone whose beforeRename hook exits the process, so no defer and no
// cleanup runs -- what a killed process leaves behind.
func TestCloneCrashHelper(t *testing.T) {
	if os.Getenv(cloneCrashHelperEnv) != "1" {
		return
	}
	_, _ = cloneWithHooks(context.Background(), os.Getenv(cloneCrashHomeEnv), os.Getenv(cloneCrashURLEnv),
		cloneHooks{beforeRename: func() error { os.Exit(cloneCrashExitCode); return nil }})
	t.Fatal("the crash hook did not run")
}

func TestCloneKilledBeforeTheRenameLeavesNoStoreAndOneScratch(t *testing.T) {
	_, bare := pushedStore(t)
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCloneCrashHelper$")
	cmd.Env = append(os.Environ(),
		cloneCrashHelperEnv+"=1",
		cloneCrashHomeEnv+"="+home,
		cloneCrashURLEnv+"="+bare,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != cloneCrashExitCode {
		t.Fatalf("child must terminate with code %d, err=%v output=%s", cloneCrashExitCode, err, output)
	}
	assertNoStore(t, home)
	entries, err := os.ReadDir(filepath.Join(home, "staging"))
	if err != nil {
		t.Fatal(err)
	}
	scratches := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), cloneScratchPrefix) {
			scratches++
		}
	}
	if scratches != 1 {
		t.Fatalf("a killed clone must leave exactly one scratch under staging/, found %d", scratches)
	}
	if _, err := Open(home); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("Open must still say the store is missing, got %v", err)
	}
	if _, err := Clone(context.Background(), home, bare); err != nil {
		t.Fatalf("a second clone must succeed beside the residue: %v", err)
	}
}

// fastForwardIn runs FastForward inside a write session, as engine does.
func fastForwardIn(t *testing.T, s *Store) (FastForwardOutcome, error) {
	t.Helper()
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	}()
	return session.Store.FastForward()
}

const betaSkill = "---\nname: beta\ndescription: the beta skill\n---\n\nbody\n"

func TestFastForwardAdvancesTheBranchAndWorktreeWithoutANewCommit(t *testing.T) {
	a, bare := storeWithRemote(t)
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := Clone(context.Background(), t.TempDir(), bare)
	if err != nil {
		t.Fatal(err)
	}
	second := commitFile(t, a, "skills/alpha/SKILL.md", alphaSkill, "new: alpha")
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := fastForwardIn(t, b)
	if err != nil {
		t.Fatal(err)
	}
	if out.UpToDate || out.NoRemoteBranch || out.To != second || len(out.Changed) == 0 {
		t.Fatalf("fast-forward outcome = %+v, want To=%s with changed paths", out, second)
	}
	head, err := b.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash() != second {
		t.Fatalf("branch must now be at %s, got %s", second, head.Hash())
	}
	content, err := os.ReadFile(filepath.Join(b.SkillsDir(), "alpha", "SKILL.md"))
	if err != nil || string(content) != alphaSkill {
		t.Fatalf("worktree must hold the remote tree, got %q err=%v", content, err)
	}
	aLog, err := a.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	bLog, err := b.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(aLog) != len(bLog) {
		t.Fatalf("fast-forward must add no commit of its own: %d local vs %d remote entries", len(bLog), len(aLog))
	}
	for i := range aLog {
		if aLog[i].Hash != bLog[i].Hash {
			t.Fatalf("history must be identical after fast-forward, entry %d: %s vs %s", i, bLog[i].Hash, aLog[i].Hash)
		}
	}
	dirty, err := b.IsDirty()
	if err != nil || dirty {
		t.Fatalf("index and worktree must match the new head, dirty=%v err=%v", dirty, err)
	}
}

func TestFastForwardIsUpToDateWhenLocalContainsTheRemote(t *testing.T) {
	a, bare := storeWithRemote(t)
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := Clone(context.Background(), t.TempDir(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := fastForwardIn(t, b)
	if err != nil || !out.UpToDate || out.From != out.To {
		t.Fatalf("an equal remote must be up to date, got %+v err=%v", out, err)
	}
	// Local ahead of the remote is up to date too: there is nothing to follow.
	third := commitFile(t, b, "skills/beta/SKILL.md", betaSkill, "new: beta")
	out, err = fastForwardIn(t, b)
	if err != nil || !out.UpToDate {
		t.Fatalf("a remote that is an ancestor must be up to date, got %+v err=%v", out, err)
	}
	head, err := b.Repo.Head()
	if err != nil || head.Hash() != third {
		t.Fatalf("the local commit must stay, got %v err=%v", head, err)
	}
}

func TestFastForwardRefusesDivergence(t *testing.T) {
	a, bare := storeWithRemote(t)
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := Clone(context.Background(), t.TempDir(), bare)
	if err != nil {
		t.Fatal(err)
	}
	local := commitFile(t, b, "skills/beta/SKILL.md", betaSkill, "new: beta")
	commitFile(t, a, "skills/alpha/SKILL.md", alphaSkill, "new: alpha")
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := fastForwardIn(t, b)
	if !errors.Is(err, ErrRemoteDiverged) {
		t.Fatalf("diverged histories must report ErrRemoteDiverged, got %v", err)
	}
	if len(out.Changed) != 0 {
		t.Fatalf("a refused fast-forward must change nothing, got %v", out.Changed)
	}
	head, err := b.Repo.Head()
	if err != nil || head.Hash() != local {
		t.Fatalf("the branch must stay at %s, got %v err=%v", local, head, err)
	}
	if _, err := os.Stat(filepath.Join(b.SkillsDir(), "alpha")); !os.IsNotExist(err) {
		t.Fatal("the worktree must not receive the remote's tree")
	}
}

func TestFastForwardReportsAMissingRemoteBranch(t *testing.T) {
	s, _ := storeWithRemote(t) // never fetched: no refs/remotes/origin/<branch>
	out, err := fastForwardIn(t, s)
	if err != nil || !out.NoRemoteBranch {
		t.Fatalf("a missing remote-tracking ref must be reported, got %+v err=%v", out, err)
	}
}

func TestFastForwardRequiresAWriteSession(t *testing.T) {
	s, _ := storeWithRemote(t)
	if _, err := s.FastForward(); !errors.Is(err, errUnpinnedWorktree) {
		t.Fatalf("FastForward outside a write session must refuse, got %v", err)
	}
}

// TestCurrentBranchRefusesADetachedHead pins the negative half of
// currentBranch: Push and FastForward both depend on it refusing a HEAD
// that names a commit rather than a branch, which only direct git produces.
func TestCurrentBranchRefusesADetachedHead(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, head.Hash())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.currentBranch(); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("a detached HEAD must be refused by name, got %v", err)
	}
}

// TestResetScratchRestoresTheModeRegardlessOfUmask pins that the retry path
// of Clone produces a store directory with the same mode as the first
// attempt's: MkdirAll is umask-masked, so without an explicit chmod a clone
// that retried under umask 077 landed a 0700 store while a clone that did
// not retry landed 0755.
func TestResetScratchRestoresTheModeRegardlessOfUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	scratch := filepath.Join(t.TempDir(), ".fu-clone-mode")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := resetScratch(scratch); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("resetScratch must leave the scratch at 0755 whatever the umask, got %o", got)
	}
}

// TestFetchFollowsARewrittenRemoteBranch pins that the tracking ref follows a
// remote branch whose history was rewritten, which is what direct git on the
// other machine can do. The refspec Fetch uses carries no "+": go-git's
// prune reverses the spec textually, and a leading "+" lands mid-string
// where it defeats the prune's lookup, so FetchOptions.Force is what makes
// this a forced update instead. Dropping Force would make this fetch fail.
func TestFetchFollowsARewrittenRemoteBranch(t *testing.T) {
	a, bare := storeWithRemote(t)
	commitFile(t, a, "skills/alpha/SKILL.md", alphaSkill, "new: alpha")
	if _, err := a.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	// An unrelated history force-pushed over the branch: nothing b tracks is
	// an ancestor of what the remote now holds.
	c, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rewritten := commitFile(t, c, "notes.md", "c rewrote the branch", "new: c")
	cRemote, err := c.Repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	head, err := c.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := cRemote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("+" + head.Name().String() + ":" + head.Name().String())},
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := b.Fetch(context.Background())
	if err != nil || !updated {
		t.Fatalf("fetch must follow the rewritten branch, got updated=%v err=%v", updated, err)
	}
	tracking, err := b.Repo.Reference(plumbing.NewRemoteReferenceName(remoteName, head.Name().Short()), true)
	if err != nil || tracking.Hash() != rewritten {
		t.Fatalf("tracking ref must move to the rewritten head %s, got %v err=%v", rewritten, tracking, err)
	}
}
