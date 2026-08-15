package source

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/store"
	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestCloneSourceHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cloneSourceWithContext(ctx, Source{Kind: KindGit, URL: "https://example.com/repo.git"}, t.TempDir(), func() error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("clone error = %v, want context.Canceled", err)
	}
}

func TestCloneSourceStopsAtAggregateByteLimit(t *testing.T) {
	url, _ := makeGitSourceRepo(t, map[string]string{
		"large": strings.Repeat("0123456789abcdef", 1<<17),
	})
	destination := t.TempDir()
	const limit = int64(256 << 10)
	_, err := cloneSourceWithContextLimit(
		context.Background(), Source{Kind: KindGit, URL: url}, destination,
		func() error { return nil }, limit,
	)
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("oversized clone error = %v, want ErrSourceTooLarge", err)
	}
	var size int64
	walkErr := filepath.WalkDir(destination, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if size > limit {
		t.Fatalf("failed clone wrote %d bytes, over limit %d", size, limit)
	}
}

func TestFileCloneBudgetFailureReturnsPromptly(t *testing.T) {
	const helper = "FU_TEST_FILE_CLONE_BUDGET_HELPER"
	if os.Getenv(helper) == "1" {
		_, err := cloneSourceWithContextLimit(
			context.Background(),
			Source{Kind: KindGit, URL: os.Getenv("FU_TEST_FILE_CLONE_URL")},
			os.Getenv("FU_TEST_FILE_CLONE_DEST"),
			func() error { return nil },
			128<<10,
		)
		if !errors.Is(err, ErrSourceTooLarge) {
			t.Fatalf("file clone error = %v, want ErrSourceTooLarge", err)
		}
		return
	}

	payload := make([]byte, 2<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	url, _ := makeGitSourceRepo(t, map[string]string{"large": string(payload)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileCloneBudgetFailureReturnsPromptly$")
	cmd.Env = append(os.Environ(),
		helper+"=1",
		"FU_TEST_FILE_CLONE_URL="+url,
		"FU_TEST_FILE_CLONE_DEST="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("budget-exhausted file clone did not return before the deadline: %v\n%s", ctx.Err(), out)
	}
	if err != nil {
		t.Fatalf("file clone helper: %v\n%s", err, out)
	}
}

func TestLimitedCloneFilesystemChrootSharesBudget(t *testing.T) {
	budget := &cloneByteBudget{remaining: 100}
	root := &limitedCloneFilesystem{Filesystem: osfs.New(t.TempDir()), budget: budget}
	childFS, err := root.Chroot("objects")
	if err != nil {
		t.Fatal(err)
	}
	child, ok := childFS.(*limitedCloneFilesystem)
	if !ok {
		t.Fatalf("Chroot filesystem = %T, want *limitedCloneFilesystem", childFS)
	}
	if child.budget != budget {
		t.Fatalf("Chroot budget = %p, want shared budget %p", child.budget, budget)
	}
}

func TestLimitedCloneFileTruncateChargesOnlySizeDelta(t *testing.T) {
	budget := &cloneByteBudget{remaining: 100}
	filesystem := &limitedCloneFilesystem{Filesystem: osfs.New(t.TempDir()), budget: budget}
	file, err := filesystem.Create("object")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(make([]byte, 40)); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(50); err != nil {
		t.Fatal(err)
	}
	if budget.remaining != 50 {
		t.Fatalf("growing 40-byte file to 50 left budget %d, want 50", budget.remaining)
	}
	if err := file.Truncate(20); err != nil {
		t.Fatal(err)
	}
	if budget.remaining != 80 {
		t.Fatalf("shrinking 50-byte file to 20 left budget %d, want 80", budget.remaining)
	}
	if err := file.Truncate(101); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("over-budget growth error = %v, want ErrSourceTooLarge", err)
	}
	if budget.remaining != 80 {
		t.Fatalf("failed truncate changed budget to %d, want 80", budget.remaining)
	}
}

func TestLimitedCloneFilesystemSymlinkChargesTargetBytes(t *testing.T) {
	budget := &cloneByteBudget{remaining: 5}
	root := t.TempDir()
	filesystem := &limitedCloneFilesystem{
		Filesystem: osfs.New(root),
		budget:     budget,
		ctx:        context.Background(),
	}
	if err := filesystem.Symlink("four", "first"); err != nil {
		t.Fatal(err)
	}
	if budget.remaining != 1 {
		t.Fatalf("four-byte target left budget %d, want 1", budget.remaining)
	}
	if err := filesystem.Symlink("xx", "second"); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("over-budget symlink error = %v, want ErrSourceTooLarge", err)
	}
	if budget.remaining != 1 {
		t.Fatalf("failed symlink changed budget to %d, want 1", budget.remaining)
	}
	if _, err := os.Lstat(filepath.Join(root, "second")); !os.IsNotExist(err) {
		t.Fatalf("failed symlink must not be created: %v", err)
	}
}

func TestCloneSourceSharesBudgetAcrossBranchAndTagAttempts(t *testing.T) {
	budget := &cloneByteBudget{remaining: 100}
	var seen []*cloneByteBudget
	clone := func(_ context.Context, _ string, _ *git.CloneOptions, got *cloneByteBudget) (*git.Repository, error) {
		seen = append(seen, got)
		if err := got.reserve(60); err != nil {
			return nil, err
		}
		return nil, errors.New("missing ref")
	}
	_, err := cloneSourceWithContextBudget(
		context.Background(), Source{Kind: KindGit, URL: "https://example.invalid/repo.git", Ref: "release"},
		t.TempDir(), func() error { return nil }, budget, clone,
	)
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("second ref attempt error = %v, want shared-budget ErrSourceTooLarge", err)
	}
	if len(seen) != 2 {
		t.Fatalf("clone attempts = %d, want branch and tag", len(seen))
	}
	if seen[0] != budget || seen[1] != budget {
		t.Fatalf("clone attempts did not share the caller's budget: %p %p, want %p", seen[0], seen[1], budget)
	}
}

// seedWorktree writes skills into a fresh worktree repo and returns the
// repository, its path, and its current branch name.
func seedWorktree(t *testing.T, skills map[string]string) (*git.Repository, string, string) {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatalf("init source repo: %v", err)
	}
	for name, body := range skills {
		dir := filepath.Join(work, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatalf("write SKILL.md: %v", err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatalf("stage source repo: %v", err)
	}
	if _, err := wt.Commit("seed skills", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t"},
	}); err != nil {
		t.Fatalf("commit source repo: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return repo, work, head.Name().Short()
}

// makeGitSourceRepo builds a bare repository holding skills and returns its
// file:// URL plus the branch name it serves.
func makeGitSourceRepo(t *testing.T, skills map[string]string) (string, string) {
	t.Helper()
	_, work, branch := seedWorktree(t, skills)
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}
	pushWorktree(t, work, bare)
	return "file://" + bare, branch
}

func startGitDaemon(t *testing.T, barePath string) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git daemon integration requires git: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(barePath)
	var output strings.Builder
	cmd := exec.Command(gitPath, "daemon", "--reuseaddr", "--export-all", "--base-path="+base,
		"--listen=127.0.0.1", "--port="+strconv.Itoa(port), base)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Skipf("git daemon is unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			// git may leave its daemon or connection workers behind after the
			// launcher exits, so own and terminate the complete process group.
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		})
	}
	t.Cleanup(stop)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		select {
		case waitErr := <-done:
			if strings.Contains(output.String(), "not a git command") {
				t.Skipf("git daemon is unavailable: %s", output.String())
			}
			t.Fatalf("git daemon exited before accepting connections: %v: %s", waitErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("git daemon did not accept connections: %s", output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "git://" + address + "/" + filepath.Base(barePath)
}

// pushWorktree pushes the current branch of work into the bare repository at
// barePath.
func pushWorktree(t *testing.T, work, barePath string) {
	t.Helper()
	repo, err := git.PlainOpen(work)
	if err != nil {
		t.Fatalf("open worktree: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{barePath}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatalf("push to bare: %v", err)
	}
}

func TestParseArgGitURL(t *testing.T) {
	cases := []struct {
		name            string
		arg             string
		wantURL         string
		wantRef         string
		wantKind        Kind
		wantError       bool
		wantUnsupported bool
	}{
		{"https no ref", "https://github.com/x/skills.git", "https://github.com/x/skills.git", "", KindGit, false, false},
		{"at-sign path preserved", "https://host/x@y.git", "https://host/x@y.git", "", KindGit, false, false},
		{"scp syntax", "git@github.com:x/skills.git", "git@github.com:x/skills.git", "", KindGit, false, false},
		{"scp arbitrary user", "alice@example.com:team/skills.git", "alice@example.com:team/skills.git", "", KindGit, false, false},
		{"scp no user", "example.com:team/skills.git", "example.com:team/skills.git", "", KindGit, false, false},
		{"scp at-sign path preserved", "git@host:x/skills@team.git", "git@host:x/skills@team.git", "", KindGit, false, false},
		{"ssh scheme", "ssh://git@host/x.git", "ssh://git@host/x.git", "", KindGit, false, false},
		{"bare commit hash rejected", "0123456789abcdef0123456789abcdef01234567", "", "", "", true, true},
		{"bare uppercase commit hash rejected", strings.Repeat("AB", 20), "", "", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseArg(tc.arg)
			if tc.wantError {
				if err == nil {
					t.Fatalf("ParseArg(%q) succeeded, want error", tc.arg)
				}
				if tc.wantUnsupported && !errors.Is(err, ErrCommitRefUnsupported) {
					t.Fatalf("ParseArg(%q) error = %v, want ErrCommitRefUnsupported", tc.arg, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArg(%q): %v", tc.arg, err)
			}
			if got.Kind != tc.wantKind || got.URL != tc.wantURL || got.Ref != tc.wantRef {
				t.Fatalf("ParseArg(%q) = %+v, want kind=%v url=%q ref=%q", tc.arg, got, tc.wantKind, tc.wantURL, tc.wantRef)
			}
		})
	}
}

func TestParseArgPrefersExistingLocalPathOverSCPSyntax(t *testing.T) {
	t.Chdir(t.TempDir())
	local := filepath.Join("example.com:team", "skills.git")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ParseArg("example.com:team/skills.git")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindLocal {
		t.Fatalf("existing ambiguous path must be local, got %+v", got)
	}
}

func TestParseArgTreatsSCPSyntaxAsGitWhenLocalPrefixIsAFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("example.com:team", []byte("shadow"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ParseArg("example.com:team/skills.git")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindGit || got.URL != "example.com:team/skills.git" {
		t.Fatalf("shadowed SCP source = %+v, want git URL", got)
	}
}

func TestParseArgWithRef(t *testing.T) {
	t.Run("branch with slash", func(t *testing.T) {
		got, err := ParseArgWithRef("https://host/x@y.git", "feature/foo")
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindGit || got.URL != "https://host/x@y.git" || got.Ref != "feature/foo" {
			t.Fatalf("parsed source = %+v", got)
		}
	})
	t.Run("commit hash rejected", func(t *testing.T) {
		_, err := ParseArgWithRef("https://host/x.git", "0123456789abcdef0123456789abcdef01234567")
		if !errors.Is(err, ErrCommitRefUnsupported) {
			t.Fatalf("error = %v, want ErrCommitRefUnsupported", err)
		}
	})
	t.Run("local ref rejected", func(t *testing.T) {
		_, err := ParseArgWithRef(t.TempDir(), "main")
		if err == nil {
			t.Fatal("local source with --ref must be rejected")
		}
	})
}

func TestParseArgLocalPath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "skills")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// t.TempDir() on macOS sits behind /private symlinks; ParseArg resolves
	// through them, so the comparison side is resolved the same way.
	wantSub, err := filepath.EvalSymlinks(sub)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		arg       string
		wantPath  string
		wantError bool
	}{
		{"absolute dir", sub, wantSub, false},
		{"missing path", filepath.Join(dir, "nope"), "", true},
		{"file not dir", file, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseArg(tc.arg)
			if tc.wantError {
				if err == nil {
					t.Fatalf("ParseArg(%q) succeeded, want error", tc.arg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArg(%q): %v", tc.arg, err)
			}
			if got.Kind != KindLocal || got.Path != tc.wantPath {
				t.Fatalf("ParseArg(%q) = %+v, want kind=local path=%q", tc.arg, got, tc.wantPath)
			}
		})
	}
}

func TestParseArgLocalPathResolvesSymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(filepath.Dir(real), "link-to-src")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	got, err := ParseArg(link)
	if err != nil {
		t.Fatalf("ParseArg(%q): %v", link, err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindLocal || got.Path != resolved {
		t.Fatalf("ParseArg(%q) = %+v, want resolved path %q", link, got, resolved)
	}
}

func TestEncodeFields(t *testing.T) {
	t.Run("git", func(t *testing.T) {
		s := Source{Kind: KindGit, URL: "https://x/y.git", Ref: "main"}
		got := s.EncodeFields("sub/dir", LockInfo{Ref: "refs/heads/main", RefKind: "branch", Commit: "abc123"})
		want := map[string]string{
			"type": "git", "url": "https://x/y.git", "ref": "refs/heads/main",
			"ref_kind": "branch", "commit": "abc123", "subdir": "sub/dir",
		}
		if !mapEqual(got, want) {
			t.Fatalf("EncodeFields = %v, want %v", got, want)
		}
	})
	t.Run("git root subdir omitted", func(t *testing.T) {
		s := Source{Kind: KindGit, URL: "https://x/y.git"}
		got := s.EncodeFields(".", LockInfo{Ref: "refs/heads/main", RefKind: "branch", Commit: "abc123"})
		if _, ok := got["subdir"]; ok {
			t.Fatalf("root subdir must be omitted: %v", got)
		}
	})
	t.Run("local", func(t *testing.T) {
		s := Source{Kind: KindLocal, Path: "/a/b"}
		got := s.EncodeFields("c", LockInfo{})
		if got["type"] != "local" || got["path"] != "/a/b" || got["subdir"] != "c" {
			t.Fatalf("EncodeFields = %v", got)
		}
	})
}

func TestPrepareGitSource(t *testing.T) {
	url, branch := makeGitSourceRepo(t, map[string]string{
		"pdf-tools": "---\nname: pdf-tools\ndescription: d\n---\n",
		"linter":    "---\nname: linter\ndescription: d\n---\n",
	})
	s, err := ParseArg(url)
	if err != nil {
		t.Fatalf("ParseArg: %v", err)
	}
	staging := t.TempDir()
	p, err := s.Prepare(staging)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer p.Close()

	if !strings.HasPrefix(p.Dir(), staging) {
		t.Fatalf("prepared dir %q not under staging %q", p.Dir(), staging)
	}
	for _, name := range []string{"pdf-tools", "linter"} {
		if _, err := os.Stat(filepath.Join(p.Dir(), name, "SKILL.md")); err != nil {
			t.Fatalf("clone missing %s: %v", name, err)
		}
	}
	lock := p.Lock()
	if lock.RefKind != "branch" || lock.Ref != "refs/heads/"+branch {
		t.Fatalf("lock = %+v, want branch refs/heads/%s", lock, branch)
	}
	if !plumbing.IsHash(lock.Commit) || len(lock.Commit) != 40 {
		t.Fatalf("lock commit %q is not a full hash", lock.Commit)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(p.Dir()); !os.IsNotExist(err) {
		t.Fatalf("prepared dir still exists after Close")
	}
}

func TestPrepareGitSourceRejectsReplacedCheckedStagingDirectory(t *testing.T) {
	url, _ := makeGitSourceRepo(t, map[string]string{
		"alpha": "---\nname: alpha\ndescription: d\n---\n",
	})
	src, err := ParseArg(url)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	var stat unix.Stat_t
	if err := unix.Stat(staging, &stat); err != nil {
		t.Fatal(err)
	}
	expected := store.FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	moved := staging + "-original"
	if err := os.Rename(staging, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatal(err)
	}

	prepared, err := src.PrepareChecked(staging, expected)
	if prepared != nil || err == nil || !strings.Contains(err.Error(), "validated staging directory") {
		t.Fatalf("checked prepare = prepared %v, err %v; want staging identity refusal", prepared, err)
	}
	entries, readErr := os.ReadDir(staging)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("replacement staging received scratch entries: %v", entries)
	}
}

func TestPrepareGitSourceCheckedRequiresStagingIdentity(t *testing.T) {
	url, _ := makeGitSourceRepo(t, map[string]string{
		"alpha": "---\nname: alpha\ndescription: d\n---\n",
	})
	src, err := ParseArg(url)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := src.PrepareChecked(t.TempDir(), store.FileIdentity{})
	if prepared != nil {
		t.Cleanup(func() { _ = prepared.Close() })
	}
	if prepared != nil || err == nil || !strings.Contains(err.Error(), "validated staging directory identity is missing") {
		t.Fatalf("checked prepare with empty identity = prepared %v, err %v", prepared, err)
	}
}

func TestPrepareGitSourceViaDaemonExercisesProductionClone(t *testing.T) {
	fileURL, branch := makeGitSourceRepo(t, map[string]string{
		"pdf-tools": "---\nname: pdf-tools\ndescription: d\n---\n",
	})
	bare := strings.TrimPrefix(fileURL, "file://")
	src, err := ParseArg(startGitDaemon(t, bare))
	if err != nil {
		t.Fatal(err)
	}
	p, err := src.Prepare(t.TempDir())
	if err != nil {
		t.Fatalf("prepare through git daemon: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := os.Stat(filepath.Join(p.Dir(), "pdf-tools", "SKILL.md")); err != nil {
		t.Fatalf("production clone did not materialize the selected tree: %v", err)
	}
	lock := p.Lock()
	if lock.Ref != "refs/heads/"+branch || lock.RefKind != "branch" || !plumbing.IsHash(lock.Commit) {
		t.Fatalf("production clone lock = %+v", lock)
	}
}

func TestFileCloneBudgetIncludesSymlinkTargets(t *testing.T) {
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(work, "alpha")
	if err := os.Mkdir(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: alpha\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		target := strings.Repeat(string(rune('a'+i%26)), 200)
		if err := os.Symlink(target, filepath.Join(skillDir, fmt.Sprintf("link-%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("symlinks", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}}); err != nil {
		t.Fatal(err)
	}
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	pushWorktree(t, work, bare)
	_, err = cloneSourceWithContextLimit(context.Background(), Source{Kind: KindGit, URL: "file://" + bare},
		t.TempDir(), func() error { return nil }, 4096)
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("symlink-heavy file clone error = %v, want ErrSourceTooLarge", err)
	}
}

func TestCheckoutLocalRepositoryRejectsUnsupportedCloneShape(t *testing.T) {
	fileURL, _ := makeGitSourceRepo(t, map[string]string{
		"alpha": "---\nname: alpha\ndescription: d\n---\n",
	})
	worktree := &limitedCloneFilesystem{
		Filesystem: osfs.New(t.TempDir()),
		budget:     &cloneByteBudget{remaining: 1 << 20},
		ctx:        context.Background(),
	}
	_, err := checkoutLocalRepository(context.Background(), strings.TrimPrefix(fileURL, "file://"), worktree, &git.CloneOptions{
		SingleBranch: false,
		Depth:        0,
	})
	if err == nil || !strings.Contains(err.Error(), "Depth: 1") || !strings.Contains(err.Error(), "SingleBranch") {
		t.Fatalf("unsupported local checkout options error = %v", err)
	}
}

func TestPreparedClosePreservesPathReplacement(t *testing.T) {
	url, _ := makeGitSourceRepo(t, map[string]string{
		"pdf-tools": "---\nname: pdf-tools\ndescription: d\n---\n",
	})
	src, err := ParseArg(url)
	if err != nil {
		t.Fatal(err)
	}
	p, err := src.Prepare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := p.Dir()
	moved := original + ".owned"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(original, "foreign.txt")
	if err := os.WriteFile(marker, []byte("foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.Close(); err == nil {
		t.Fatal("Close must reject a replaced scratch pathname")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "foreign\n" {
		t.Fatalf("replacement scratch directory = %q, %v; want preserved", got, err)
	}
	if _, err := os.Stat(filepath.Join(moved, "pdf-tools", "SKILL.md")); err != nil {
		t.Fatalf("original prepared clone must be preserved after conflict: %v", err)
	}
}

func TestPreparedCloseRetriesCleanupAfterFailure(t *testing.T) {
	want := errors.New("injected cleanup failure")
	calls := 0
	p := &Prepared{root: new(os.Root), cleanup: func() error {
		calls++
		if calls == 1 {
			return want
		}
		return nil
	}}

	if err := p.Close(); !errors.Is(err, want) {
		t.Fatalf("first Close error = %v, want %v", err, want)
	}
	if p.cleanup == nil || p.root == nil {
		t.Fatal("failed Close must retain cleanup state for a retry")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
	if calls != 2 || p.cleanup != nil || p.root != nil {
		t.Fatalf("retry state: calls=%d cleanup=%v root=%v", calls, p.cleanup != nil, p.root != nil)
	}
}

func TestCreatedScratchErrorCleanupRetiresBeforeRemoval(t *testing.T) {
	parentPath := t.TempDir()
	parent, err := os.Open(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	const name = ".fu-src-created"
	if err := os.Mkdir(filepath.Join(parentPath, name), 0o700); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	expected := sourceScratchIdentity(&stat)
	marker := filepath.Join(parentPath, name, "foreign")

	err = cleanupCreatedScratch(parent, parentPath, name, expected, func(retired string) error {
		if _, statErr := os.Lstat(filepath.Join(parentPath, name)); !os.IsNotExist(statErr) {
			return fmt.Errorf("original scratch name must be free during cleanup: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(parentPath, retired)); statErr != nil {
			return fmt.Errorf("retired scratch must exist during cleanup: %w", statErr)
		}
		if mkdirErr := os.Mkdir(filepath.Join(parentPath, name), 0o700); mkdirErr != nil {
			return mkdirErr
		}
		return os.WriteFile(marker, []byte("foreign"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "foreign" {
		t.Fatalf("a replacement at the original name must survive cleanup: %q, %v", got, err)
	}
}

func TestOwnedScratchConstructorCleansDirectoryAfterOpenFailure(t *testing.T) {
	parent := t.TempDir()
	want := errors.New("injected failure after mkdir")
	_, err := newOwnedScratchWithHooks(parent, scratchCreateHooks{
		afterMkdir: func(_ int, _ string) error { return want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("constructor error = %v, want %v", err, want)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed constructor leaked scratch entries: %v", entries)
	}
}

func TestOwnedScratchConstructorCleansDirectoryAfterCreatedStatFailure(t *testing.T) {
	parent := t.TempDir()
	want := errors.New("injected created-directory stat failure")
	_, err := newOwnedScratchWithHooks(parent, scratchCreateHooks{
		inspectCreated: func(_ int, _ string, _ *unix.Stat_t) error { return want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("constructor error = %v, want %v", err, want)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("created-directory stat failure leaked scratch entries: %v", entries)
	}
}

func TestCloneRefErrorNamesURLAndRefWithoutAtSyntax(t *testing.T) {
	const url = "https://example.com/team/repo.git"
	const ref = "feature/x"
	clone := func(context.Context, string, *git.CloneOptions, *cloneByteBudget) (*git.Repository, error) {
		return nil, errors.New("missing ref")
	}
	_, err := cloneSourceWithContextBudget(
		context.Background(), Source{Kind: KindGit, URL: url, Ref: ref}, t.TempDir(),
		func() error { return nil }, &cloneByteBudget{remaining: 1}, clone,
	)
	if err == nil {
		t.Fatal("missing branch and tag must fail")
	}
	if strings.Contains(err.Error(), url+"@"+ref) {
		t.Fatalf("clone error reintroduced ambiguous @ syntax: %v", err)
	}
	for _, want := range []string{url, ref, "branch", "tag"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("clone error %q does not contain %q", err, want)
		}
	}
}

func TestOwnedScratchResetPreservesPathReplacement(t *testing.T) {
	scratch, err := newOwnedScratch(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := scratch.Path()
	moved := original + ".owned"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(original, "foreign.txt")
	if err := os.WriteFile(marker, []byte("foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := scratch.Reset(); err == nil {
		t.Fatal("Reset must reject a replaced scratch pathname")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "foreign\n" {
		t.Fatalf("replacement scratch directory = %q, %v; want preserved", got, err)
	}
	if err := scratch.Close(); err == nil {
		t.Fatal("Close must continue to reject the replacement")
	}
}

func TestOwnedScratchResetPreservesChildReplacementAtCleanup(t *testing.T) {
	scratch, err := newOwnedScratch(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scratch.Close()
	child := filepath.Join(scratch.Path(), "clone-output")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "owned.txt"), []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(child, "foreign.txt")
	err = scratch.resetWithHooks(scratchCleanupHooks{beforeContentsCleanup: func() error {
		if err := os.Rename(child, child+".owned"); err != nil {
			return err
		}
		if err := os.Mkdir(child, 0o755); err != nil {
			return err
		}
		return os.WriteFile(marker, []byte("foreign"), 0o644)
	}})
	if err == nil {
		t.Fatal("reset must reject a child replacement at cleanup")
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "foreign" {
		t.Fatalf("foreign scratch child must survive: %q, %v", got, readErr)
	}
}

func TestOwnedScratchClosePreservesRootReplacementAtRetirement(t *testing.T) {
	scratch, err := newOwnedScratch(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := scratch.Path()
	marker := filepath.Join(original, "foreign.txt")
	err = scratch.closeWithHooks(scratchCleanupHooks{beforeRootRetire: func() error {
		if err := os.Rename(original, original+".owned"); err != nil {
			return err
		}
		if err := os.Mkdir(original, 0o755); err != nil {
			return err
		}
		return os.WriteFile(marker, []byte("foreign"), 0o644)
	}})
	if err == nil {
		t.Fatal("close must reject a root replacement at retirement")
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "foreign" {
		t.Fatalf("foreign scratch root must survive: %q, %v", got, readErr)
	}
}

func TestOwnedScratchCloseRetriesQuarantinedCleanup(t *testing.T) {
	parent := t.TempDir()
	scratch, err := newOwnedScratch(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch.Path(), "clone-output"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected cleanup interruption")
	err = scratch.closeWithHooks(scratchCleanupHooks{beforeContentsCleanup: func() error { return want }})
	if !errors.Is(err, want) {
		t.Fatalf("first close error = %v, want injected interruption", err)
	}
	if err := scratch.Close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".fu-src-") {
			t.Fatalf("retry must remove quarantined scratch, found %v", entries)
		}
	}
}

func TestPrepareGitSourceTag(t *testing.T) {
	repo, work, _ := seedWorktree(t, map[string]string{
		"pdf-tools": "---\nname: pdf-tools\ndescription: d\n---\n",
	})
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v1.2.3", head.Hash(), &git.CreateTagOptions{
		Tagger:  &object.Signature{Name: "tagger", Email: "tagger@example.com", When: time.Now()},
		Message: "annotated release",
	}); err != nil {
		t.Fatal(err)
	}
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	pushWorktree(t, work, bare)
	// Tags do not ride along the default push; push the tag from the source
	// worktree to the bare repository explicitly, reusing its origin remote.
	srcRepo, err := git.PlainOpen(work)
	if err != nil {
		t.Fatal(err)
	}
	tagRemote, err := srcRepo.Remote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if err := tagRemote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/tags/v1.2.3:refs/tags/v1.2.3")},
	}); err != nil {
		t.Fatalf("push tag: %v", err)
	}

	for _, rawURL := range []string{"file://" + bare, startGitDaemon(t, bare)} {
		s, err := ParseArgWithRef(rawURL, "v1.2.3")
		if err != nil {
			t.Fatalf("ParseArg: %v", err)
		}
		p, err := s.Prepare(t.TempDir())
		if err != nil {
			t.Fatalf("Prepare %s: %v", rawURL, err)
		}
		lock := p.Lock()
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if lock.RefKind != "tag" || lock.Ref != "refs/tags/v1.2.3" {
			t.Fatalf("lock = %+v, want tag refs/tags/v1.2.3", lock)
		}
		if lock.Commit != head.Hash().String() {
			t.Fatalf("lock commit %q, want peeled commit %q for %s", lock.Commit, head.Hash().String(), rawURL)
		}
	}
}

func TestPrepareGitSourceBranchWithSlash(t *testing.T) {
	repo, _, _ := seedWorktree(t, map[string]string{
		"pdf-tools": "---\nname: pdf-tools\ndescription: d\n---\n",
	})
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	branch := plumbing.NewBranchReferenceName("feature/foo")
	if err := repo.Storer.SetReference(plumbing.NewHashReference(branch, head.Hash())); err != nil {
		t.Fatal(err)
	}
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(branch.String() + ":" + branch.String())},
	}); err != nil {
		t.Fatalf("push slash branch: %v", err)
	}

	src, err := ParseArgWithRef("file://"+bare, "feature/foo")
	if err != nil {
		t.Fatal(err)
	}
	p, err := src.Prepare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := p.Lock(); got.Ref != branch.String() || got.RefKind != "branch" {
		t.Fatalf("lock = %+v, want branch %s", got, branch)
	}
}

func TestPrepareGitSourceURLPathContainingAt(t *testing.T) {
	_, work, _ := seedWorktree(t, map[string]string{
		"pdf-tools": "---\nname: pdf-tools\ndescription: d\n---\n",
	})
	bare := filepath.Join(t.TempDir(), "skills@team.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	pushWorktree(t, work, bare)
	url := "file://" + bare
	src, err := ParseArg(url)
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != url || src.Ref != "" {
		t.Fatalf("parsed source = %+v, want URL preserved", src)
	}
	p, err := src.Prepare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := os.Stat(filepath.Join(p.Dir(), "pdf-tools", "SKILL.md")); err != nil {
		t.Fatalf("clone from @ URL path: %v", err)
	}
}

func TestPrepareLocalSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdf-tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdf-tools", "SKILL.md"),
		[]byte("---\nname: pdf-tools\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ParseArg(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Prepare(t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer p.Close()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir() != resolvedDir {
		t.Fatalf("prepared dir %q, want %q", p.Dir(), resolvedDir)
	}
	if p.Lock().RefKind != "" {
		t.Fatalf("local lock must not carry git fields: %+v", p.Lock())
	}
}

// mapEqual compares two string maps without requiring reflect.
func mapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestParseArgWithRefRejectsFullyQualifiedRef pins round 18 finding M7.
// plumbing.NewBranchReferenceName("refs/heads/main").Validate() returns nil, so
// a fully-qualified ref passed validation and cloneSource then probed
// refs/heads/refs/heads/main and refs/tags/refs/heads/main, failing with a
// two-part transport error that named neither the mistake nor the fix.
// The suggestion has to be a ref the user could actually pass: the double
// TrimPrefix left any other prefix untouched, so `--ref refs/remotes/origin/main`
// was refused with a message suggesting the very string just refused.
func TestParseArgWithRefRejectsFullyQualifiedRef(t *testing.T) {
	for ref, want := range map[string]string{
		"refs/heads/main":           "main",
		"refs/tags/v1.0.0":          "v1.0.0",
		"refs/heads/feature/nested": "feature/nested",
		"refs/remotes/origin/main":  "main",
		"refs/pull/12/head":         "head",
	} {
		_, err := ParseArgWithRef("https://example.com/team/repo.git", ref)
		if err == nil {
			t.Fatalf("ref %q must be refused", ref)
		}
		if !strings.Contains(err.Error(), "fully qualified") {
			t.Fatalf("ref %q: the refusal must name the fix, got %v", ref, err)
		}
		if !strings.Contains(err.Error(), strconv.Quote(want)) {
			t.Fatalf("ref %q: the suggestion must be %q, got %v", ref, want, err)
		}
		if strings.Contains(err.Error(), strconv.Quote(ref)+")") {
			t.Fatalf("ref %q: the suggestion must not echo the refused input: %v", ref, err)
		}
	}
}

// TestParseArgWithRefAcceptsSlashedBranch keeps the property the check must not
// break: a branch name containing '/' is ordinary and stays valid.
func TestParseArgWithRefAcceptsSlashedBranch(t *testing.T) {
	src, err := ParseArgWithRef("https://example.com/team/repo.git", "feature/nested/thing")
	if err != nil {
		t.Fatalf("a slashed branch name must stay valid: %v", err)
	}
	if src.Ref != "feature/nested/thing" {
		t.Fatalf("ref = %q", src.Ref)
	}
}
