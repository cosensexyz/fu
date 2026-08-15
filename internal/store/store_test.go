// internal/store/store_test.go
package store

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestPublishStagedNeverReplacesDestinationInsertedAtRenameBoundary(t *testing.T) {
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
	stagingRoot, err := checked.StagingRoot()
	if err != nil {
		t.Fatal(err)
	}
	skillsRoot, err := checked.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := stagingRoot.Mkdir("alpha", 0o755); err != nil {
		t.Fatal(err)
	}
	stagedBytes := []byte("transaction-owned staged content")
	if err := stagingRoot.WriteFile(filepath.Join("alpha", "SKILL.md"), stagedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	foreignBytes := []byte("foreign destination content")

	err = renameCheckedExclusive(checked.writeRoots.staging, "alpha", checked.writeRoots.skills, "alpha", func() {
		if err := skillsRoot.Mkdir("alpha", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := skillsRoot.WriteFile(filepath.Join("alpha", "SKILL.md"), foreignBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("exclusive publish must report an occupied destination, got %v", err)
	}
	gotForeign, err := skillsRoot.ReadFile(filepath.Join("alpha", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotForeign, foreignBytes) {
		t.Fatalf("foreign destination changed: got %q want %q", gotForeign, foreignBytes)
	}
	gotStaged, err := stagingRoot.ReadFile(filepath.Join("alpha", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotStaged, stagedBytes) {
		t.Fatalf("failed publish changed staged source: got %q want %q", gotStaged, stagedBytes)
	}
}

func TestInitOpenCommit(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.ConfigPath()); err != nil {
		t.Fatal("fu.yaml missing after init")
	}
	if _, err := Init(home); err != ErrStoreExists {
		t.Fatalf("want ErrStoreExists, got %v", err)
	}
	// a change is committed with the given message
	os.WriteFile(filepath.Join(s.Dir(), "note.txt"), []byte("x"), 0o644)
	if _, err := s.Commit("test: note"); err != nil {
		t.Fatal(err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Repo.CommitObject(head.Hash())
	if c.Message != "test: note" {
		t.Fatalf("unexpected head message %q", c.Message)
	}
	// reopen works
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.TempDir()); err != ErrStoreNotFound {
		t.Fatalf("want ErrStoreNotFound, got %v", err)
	}
}

func TestStagingIdentityUsesTheValidatedDirectory(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := s.StagingIdentity()
	if err != nil || identity.Inode == 0 {
		t.Fatalf("staging identity = %+v, %v; want validated inode", identity, err)
	}

	original := s.StagingDir() + "-original"
	if err := os.Rename(s.StagingDir(), original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.StagingDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StagingIdentity(); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replacement staging identity error = %v", err)
	}
}

func TestBeginWriteRefusesReplacedLogicalRoots(t *testing.T) {
	tests := []struct {
		name    string
		path    func(*Store) string
		prepare func(*testing.T, *Store, string) string
	}{
		{
			name: "store",
			path: func(s *Store) string { return s.Dir() },
			prepare: func(t *testing.T, _ *Store, path string) string {
				decoy := filepath.Join(filepath.Dir(path), "decoy-store")
				if err := os.Mkdir(decoy, 0o755); err != nil {
					t.Fatal(err)
				}
				return decoy
			},
		},
		{
			name: "skills",
			path: func(s *Store) string { return s.SkillsDir() },
			prepare: func(t *testing.T, _ *Store, path string) string {
				decoy := filepath.Join(filepath.Dir(path), "decoy-skills")
				if err := os.Mkdir(decoy, 0o755); err != nil {
					t.Fatal(err)
				}
				return decoy
			},
		},
		{
			name: "git metadata",
			path: func(s *Store) string { return filepath.Join(s.Dir(), git.GitDirName) },
			prepare: func(t *testing.T, _ *Store, path string) string {
				decoyWorktree := filepath.Join(filepath.Dir(path), "decoy-repository")
				if _, err := git.PlainInit(decoyWorktree, false); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(decoyWorktree, git.GitDirName)
			},
		},
		{
			name: "staging",
			path: func(s *Store) string { return s.StagingDir() },
			prepare: func(t *testing.T, _ *Store, path string) string {
				decoy := filepath.Join(filepath.Dir(path), "decoy-staging")
				if err := os.Mkdir(decoy, 0o755); err != nil {
					t.Fatal(err)
				}
				return decoy
			},
		},
		{
			name: "recovery",
			path: func(s *Store) string { return s.RecoveryDir() },
			prepare: func(t *testing.T, _ *Store, path string) string {
				decoy := filepath.Join(filepath.Dir(path), "decoy-recovery")
				if err := os.Mkdir(decoy, 0o755); err != nil {
					t.Fatal(err)
				}
				return decoy
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if _, err := Init(home); err != nil {
				t.Fatal(err)
			}
			s, err := Open(home)
			if err != nil {
				t.Fatal(err)
			}
			path := tt.path(s)
			decoy := tt.prepare(t, s, path)
			original := path + "-original"
			if err := os.Rename(path, original); err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(filepath.Dir(path), decoy)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.IsAbs(rel) {
				t.Fatalf("setup: replacement target must be relative, got %q", rel)
			}
			if err := os.Symlink(rel, path); err != nil {
				t.Fatal(err)
			}

			session, err := s.BeginWrite()
			if session != nil {
				_ = session.Close()
			}
			if err == nil {
				t.Fatalf("BeginWrite must refuse a replaced %s root", tt.name)
			}
			if !strings.Contains(err.Error(), "validated") {
				t.Fatalf("replacement must be reported as an identity failure, got %v", err)
			}
		})
	}
}

func TestOpenNeverBlessesRootsReplacedAfterValidation(t *testing.T) {
	tests := []struct {
		name string
		path func(*Store) string
	}{
		{name: "home", path: func(s *Store) string { return s.Home }},
		{name: "store", path: func(s *Store) string { return s.Dir() }},
		{name: "skills", path: func(s *Store) string { return s.SkillsDir() }},
		{name: "git metadata", path: func(s *Store) string { return filepath.Join(s.Dir(), git.GitDirName) }},
		{name: "staging", path: func(s *Store) string { return s.StagingDir() }},
		{name: "recovery", path: func(s *Store) string { return s.RecoveryDir() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			home := filepath.Join(parent, "home")
			if _, err := Init(home); err != nil {
				t.Fatal(err)
			}
			candidate := &Store{Home: home}
			path := tt.path(candidate)
			decoy := filepath.Join(filepath.Dir(path), "decoy-"+strings.ReplaceAll(tt.name, " ", "-"))
			if path == home {
				decoy = filepath.Join(parent, "decoy-home")
			}
			if err := os.Mkdir(decoy, 0o755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(decoy, "sentinel")
			want := []byte("foreign content")
			if err := os.WriteFile(sentinel, want, 0o644); err != nil {
				t.Fatal(err)
			}

			opened, err := openStore(home, storeOpenHooks{afterValidation: func() {
				original := path + "-validated"
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				rel, err := filepath.Rel(filepath.Dir(path), decoy)
				if err != nil {
					t.Fatal(err)
				}
				if filepath.IsAbs(rel) {
					t.Fatalf("replacement target must be relative, got %q", rel)
				}
				if err := os.Symlink(rel, path); err != nil {
					t.Fatal(err)
				}
			}})
			if opened != nil {
				t.Fatal("Open must not return a store after a validated root is replaced")
			}
			if err == nil {
				t.Fatal("Open must reject a root replacement before returning")
			}
			got, readErr := os.ReadFile(sentinel)
			if readErr != nil {
				t.Fatalf("replacement content must survive: %v", readErr)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("replacement content changed: got %q want %q", got, want)
			}
		})
	}
}

func TestPinnedWorktreeKeepsUsingValidatedSkillsRoot(t *testing.T) {
	home := t.TempDir()
	if _, err := Init(home); err != nil {
		t.Fatal(err)
	}
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.SkillsDir(), "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("pinned"), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	original := filepath.Join(s.Dir(), "pinned-skills")
	if err := os.Rename(s.SkillsDir(), original); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(s.Dir(), "decoy-skills")
	if err := os.Mkdir(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("decoy-skills", s.SkillsDir()); err != nil {
		t.Fatal(err)
	}

	outcome, err := session.Store.Commit("test: pinned skills worktree")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := session.Store.Repo.CommitObject(outcome.Hash)
	if err != nil {
		t.Fatal(err)
	}
	file, err := commit.File("skills/alpha/SKILL.md")
	if err != nil {
		var paths []string
		iter, iterErr := commit.Files()
		if iterErr == nil {
			_ = iter.ForEach(func(file *object.File) error {
				paths = append(paths, file.Name)
				return nil
			})
		}
		t.Fatalf("commit must read skills through the pinned logical root: %v (files=%v, iterErr=%v)", err, paths, iterErr)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	if contents != "pinned" {
		t.Fatalf("committed pinned content = %q, want %q", contents, "pinned")
	}
}

// TestInitResumesPartialState covers the recovery philosophy that any
// interruption converges on the next run. It mimics a crash right after
// Init's first side effect (creating the store directory tree) and before
// git.PlainInit ever ran, and asserts a second Init call resumes rather
// than refusing.
func TestInitResumesPartialState(t *testing.T) {
	home := t.TempDir()
	s := &Store{Home: home}
	// Mimic an interrupted run: the store directory exists (Init's first
	// side effect), but nothing past that -- no repository, no config.
	if err := os.MkdirAll(s.SkillsDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Init(home)
	if err != nil {
		t.Fatalf("Init over a partial state should resume, got error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got.Dir(), ".git")); err != nil {
		t.Fatalf("no git repository after resumed Init: %v", err)
	}
	if _, err := os.Stat(got.ConfigPath()); err != nil {
		t.Fatalf("fu.yaml missing after resumed Init: %v", err)
	}
	if _, err := Open(home); err != nil {
		t.Fatalf("Open after resumed Init: %v", err)
	}
}

// TestInitRefusesFullyInitializedStore guards the flip side of resumability:
// once Init has actually completed, a second call must still refuse rather
// than silently re-running (or worse, resuming forever).
func TestInitRefusesFullyInitializedStore(t *testing.T) {
	home := t.TempDir()
	if _, err := Init(home); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(home); !errors.Is(err, ErrStoreExists) {
		t.Fatalf("want ErrStoreExists for a fully initialized store, got %v", err)
	}
}

// TestInitResumesExactBootstrapConfig covers the only config bytes Init can
// have written before its first commit.
func TestInitResumesExactBootstrapConfig(t *testing.T) {
	home := t.TempDir()
	s := &Store{Home: home}
	if err := os.MkdirAll(s.SkillsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), bootstrapConfig, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Init(home)
	if err != nil {
		t.Fatalf("Init over a partial state with an existing config: %v", err)
	}
	raw, err := os.ReadFile(got.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, bootstrapConfig) {
		t.Fatalf("Init changed its existing bootstrap config:\ngot:  %q\nwant: %q", raw, bootstrapConfig)
	}
}

// TestInitResumesUnbornHeadRepository covers the resume branch that matters
// most: a git repository already exists at Dir() (git.PlainInit ran) but has
// no commits yet (an unborn HEAD), mimicking a crash between repository
// creation and Init's first commit. This is the one case the two mkdir-only
// resume tests above never reach, since git.PlainOpen on a plain directory
// with no .git returns ErrRepositoryNotExists, not a resolvable repository.
// Init must detect the unborn HEAD and reuse the existing repository handle
// rather than calling git.PlainInit again (which would fail with
// ErrRepositoryAlreadyExists).
func TestInitResumesUnbornHeadRepository(t *testing.T) {
	home := t.TempDir()
	s := &Store{Home: home}
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainInit(s.Dir(), false); err != nil {
		t.Fatalf("PlainInit on the pre-created store dir: %v", err)
	}

	got, err := Init(home)
	if err != nil {
		t.Fatalf("Init over an unborn-HEAD repository should resume, got error: %v", err)
	}
	if _, err := got.Repo.Head(); err != nil {
		t.Fatalf("HEAD does not resolve after resumed Init: %v", err)
	}
	if _, err := os.Stat(got.ConfigPath()); err != nil {
		t.Fatalf("fu.yaml missing after resumed Init: %v", err)
	}
	if _, err := Open(home); err != nil {
		t.Fatalf("Open after resuming an unborn-HEAD repository: %v", err)
	}
}

// TestInitResumesUnbornHeadRepositoryWithBootstrapConfig is the config-present
// counterpart of TestInitResumesUnbornHeadRepository.
func TestInitResumesUnbornHeadRepositoryWithBootstrapConfig(t *testing.T) {
	home := t.TempDir()
	s := &Store{Home: home}
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainInit(s.Dir(), false); err != nil {
		t.Fatalf("PlainInit on the pre-created store dir: %v", err)
	}
	if err := os.WriteFile(s.ConfigPath(), bootstrapConfig, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Init(home)
	if err != nil {
		t.Fatalf("Init over an unborn-HEAD repository with an existing config: %v", err)
	}
	raw, err := os.ReadFile(got.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, bootstrapConfig) {
		t.Fatalf("Init changed its existing bootstrap config:\ngot:  %q\nwant: %q", raw, bootstrapConfig)
	}
}

// TestOpenDistinguishesErrorKinds asserts that Open only reports
// ErrStoreNotFound for a genuinely absent store, and propagates other
// failures (e.g. permission denied) as a distinct error so the user isn't
// told to run `fu init` when that would not help.
func TestOpenDistinguishesErrorKinds(t *testing.T) {
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("want ErrStoreNotFound for an absent store, got %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}

	home := t.TempDir()
	if _, err := Init(home); err != nil {
		t.Fatal(err)
	}
	// Remove permissions on home (the parent of Dir()) so the store path
	// exists but cannot be traversed/read.
	if err := os.Chmod(home, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(home, 0o755) })

	_, err := Open(home)
	if err == nil {
		t.Fatal("want an error for an unreadable store, got nil")
	}
	if errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("want a distinct error for a permission failure, got ErrStoreNotFound: %v", err)
	}
}

// TestOpenRejectsUnbornHeadRepository asserts that Open uses the same
// definition of "a real store" as Init: a repository whose HEAD does not
// resolve yet (e.g. git.PlainInit ran but the process died before the first
// commit) is an incomplete store, not a usable one. Without this, Open would
// hand back a Store with no fu.yaml and no commits, and the first caller to
// touch it would see a raw file-not-found error instead of the actionable
// ErrStoreNotFound message.
func TestOpenRejectsUnbornHeadRepository(t *testing.T) {
	home := t.TempDir()
	s := &Store{Home: home}
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainInit(s.Dir(), false); err != nil {
		t.Fatalf("PlainInit on the pre-created store dir: %v", err)
	}

	if _, err := Open(home); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("want ErrStoreNotFound for a repository with no commits, got %v", err)
	}
}

// TestLayoutIsolatesMachineLocalDirs guards a core layout invariant: the
// machine-local directories (staging/, recovery/) and the lock path are
// siblings of the repository, never descendants, so they never appear in
// the store's own git status.
func TestLayoutIsolatesMachineLocalDirs(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{s.StagingDir(), s.RecoveryDir(), s.LockPath()} {
		rel, err := filepath.Rel(s.Dir(), p)
		if err != nil {
			t.Fatal(err)
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("%s (relative to the repo: %q) is not outside the repository", p, rel)
		}
	}

	if err := os.WriteFile(filepath.Join(s.StagingDir(), "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	status, err := wt.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsClean() {
		t.Fatalf("worktree status not clean after writing under staging/: %v", status)
	}
}

// TestCommitNoOpOnCleanWorktree asserts that Commit on an unchanged
// worktree is a true no-op: it returns nil and does not add a commit.
func TestCommitNoOpOnCleanWorktree(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Commit("test: no-op"); err != nil {
		t.Fatalf("Commit on a clean worktree should be a no-op, got error: %v", err)
	}

	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatalf("Commit on a clean worktree moved HEAD: before=%s after=%s", before.Hash(), after.Hash())
	}
}

func TestHomeEnvOverride(t *testing.T) {
	t.Setenv("FU_HOME", "/custom/fu")
	h, err := Home()
	if err != nil || h != "/custom/fu" {
		t.Fatalf("want /custom/fu, got %q err=%v", h, err)
	}
	t.Setenv("FU_HOME", "")
	t.Setenv("HOME", "/users/x")
	h, _ = Home()
	if h != filepath.Join("/users/x", ".fu") {
		t.Fatalf("want HOME fallback, got %q", h)
	}
}

// TestHomeFallsBackWhenPathDoesNotExistYet guards the other half of
// finding I5's fix: a spelling that does not exist on disk cannot be
// resolved by filepath.EvalSymlinks, so Home must return it unchanged
// rather than erroring -- the common case of a store that has not been
// created yet.
func TestHomeFallsBackWhenPathDoesNotExistYet(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("FU_HOME", "")
	home := filepath.Join(parent, "does-not-exist-yet")
	t.Setenv("HOME", home)
	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".fu")
	if got != want {
		t.Fatalf("want the unresolved spelling %q, got %q", want, got)
	}
}

// TestHomeResolvesSymlinkedHome is finding I5's exact reproduction: one
// store reached by two different spellings -- FU_HOME set explicitly to
// the physical target, and HOME's default ~/.fu fallback through a
// symlink a dotfiles manager (or similar) points at that same target --
// must resolve to one identical, canonical path. Before the fix, Home
// returned each spelling verbatim, so ownsLink's raw string comparison
// (no filesystem access of its own) disagreed about which links were
// fu's own depending on which spelling the calling process happened to
// use; see TestToggleVisibleAcrossHomeSpellings in internal/cli for the
// user-visible consequence this produced end to end.
func TestHomeResolvesSymlinkedHome(t *testing.T) {
	base := t.TempDir()
	realTarget := filepath.Join(base, "real-fu-target")
	if err := os.MkdirAll(realTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(home, ".fu")
	if err := os.Symlink(realTarget, linkedHome); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FU_HOME", "")
	t.Setenv("HOME", home)
	viaHomeFallback, err := Home()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("FU_HOME", linkedHome)
	viaFuHomeSymlinkSpelling, err := Home()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("FU_HOME", realTarget)
	viaFuHomeDirectSpelling, err := Home()
	if err != nil {
		t.Fatal(err)
	}

	wantCanonical, err := filepath.EvalSymlinks(realTarget)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"HOME fallback (through the symlink)":     viaHomeFallback,
		"FU_HOME set to the symlink spelling":     viaFuHomeSymlinkSpelling,
		"FU_HOME set directly to the real target": viaFuHomeDirectSpelling,
	} {
		if got != wantCanonical {
			t.Fatalf("%s: want the canonical resolved path %q, got %q -- the three spellings must agree", name, wantCanonical, got)
		}
	}
}

// TestHomeRejectsRelativePaths is round 6's path-normalization finding, and
// the other half of the gap DESIGN §6 had recorded as FU_HOME-only. A
// relative FU_HOME -- or a relative HOME reached through the ~/.fu fallback
// -- was neither rejected nor absolutized, so every layout path below it
// stayed relative to whatever directory fu happened to be launched from.
// Two invocations from different directories then address different stores,
// and the link targets written from one are broken when read from the
// other, rebuilt endlessly.
//
// Rejecting rather than absolutizing with filepath.Abs is deliberate:
// resolving against the current working directory would make the store's
// *identity* depend on where `fu init` first ran, which is a subtler
// failure than an explicit refusal. This matches agent.homeDir, which
// refuses a relative HOME for the same reason.
func TestHomeRejectsRelativePaths(t *testing.T) {
	// A real directory tree, so nothing fails merely for want of content.
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "relhome", ".fu"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	for _, tc := range []struct{ name, fuHome, home string }{
		{"relative FU_HOME", "relhome/.fu", base},
		{"dot-relative FU_HOME", "./relhome/.fu", base},
		{"bare dot FU_HOME", ".", base},
		{"relative HOME via the ~/.fu fallback", "", "relhome"},
		{"dot-relative HOME via the fallback", "", "./relhome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FU_HOME", tc.fuHome)
			t.Setenv("HOME", tc.home)
			got, err := Home()
			if err == nil {
				t.Fatalf("a relative path must be refused, not turned into a cwd-relative store; got %q", got)
			}
		})
	}

	// The absolute forms of the same inputs must still work.
	t.Run("absolute FU_HOME still works", func(t *testing.T) {
		t.Setenv("FU_HOME", filepath.Join(base, "relhome", ".fu"))
		if _, err := Home(); err != nil {
			t.Fatalf("an absolute FU_HOME must be accepted: %v", err)
		}
	})
	t.Run("absolute HOME via the fallback still works", func(t *testing.T) {
		t.Setenv("FU_HOME", "")
		t.Setenv("HOME", filepath.Join(base, "relhome"))
		if _, err := Home(); err != nil {
			t.Fatalf("an absolute HOME must be accepted: %v", err)
		}
	})
}

// TestInitRefusesUnrecognizedDirectory and TestOpenRequiresFuConfig are
// round 6's store-identity finding. Init treated *any* pre-existing
// directory as resumable partial initialization and committed whatever it
// held; Open accepted *any* git repository with a resolvable HEAD. A
// mistyped or stale FU_HOME could therefore make fu initialize an unrelated
// directory -- committing its contents, potentially including private files
// -- or operate on someone else's repository as though it were the store.
//
// Init's resume path is genuinely needed (a crash between PlainInit and the
// first commit leaves an unborn HEAD), so the fix is not to refuse every
// non-empty directory, but to refuse every one whose contents fu does not
// recognize as its own partial work.
func TestInitRefusesUnrecognizedDirectory(t *testing.T) {
	t.Run("a directory of somebody else's files", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, "store")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "taxes.pdf"), []byte("private"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := Init(home); err == nil {
			t.Fatal("init must refuse a directory holding content fu did not put there, " +
				"rather than committing it into a new repository")
		}
		// Nothing may have been created or committed in the meantime.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			t.Error("a refused init must not leave a repository behind")
		}
		if got, err := os.ReadFile(filepath.Join(dir, "taxes.pdf")); err != nil || string(got) != "private" {
			t.Errorf("the user's own file must be untouched: %q %v", got, err)
		}
	})

	t.Run("a partial init is still resumable", func(t *testing.T) {
		// Exactly the state a crash between PlainInit and the first commit
		// leaves: a repository with an unborn HEAD and fu's own layout.
		home := t.TempDir()
		dir := filepath.Join(home, "store")
		if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := git.PlainInit(dir, false); err != nil {
			t.Fatal(err)
		}

		if _, err := Init(home); err != nil {
			t.Fatalf("init must still resume its own partial work: %v", err)
		}
	})
}

// A resumable Init state must be one Init itself could have produced. The
// recognized top-level names are not sufficient evidence: Init never writes
// skill content or a non-bootstrap config before its first commit.
func TestInitRefusesRecognizedLayoutWithUnmanagedContents(t *testing.T) {
	t.Run("non-empty skills directory", func(t *testing.T) {
		home := t.TempDir()
		s := &Store{Home: home}
		foreign := filepath.Join(s.SkillsDir(), "nested", "private.txt")
		if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(foreign, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := Init(home); err == nil {
			t.Fatal("init must refuse skill content it could not have created before its first commit")
		}
		if got, err := os.ReadFile(foreign); err != nil || string(got) != "keep" {
			t.Fatalf("refused init must preserve nested content: %q %v", got, err)
		}
		if _, err := os.Lstat(filepath.Join(s.Dir(), ".git")); !os.IsNotExist(err) {
			t.Fatalf("refused init must not create a repository, got %v", err)
		}
	})

	t.Run("non-bootstrap config", func(t *testing.T) {
		home := t.TempDir()
		s := &Store{Home: home}
		if err := os.MkdirAll(s.SkillsDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		const foreign = "version: 1\nskills:\n  demo:\n    digest: private\n"
		if err := os.WriteFile(s.ConfigPath(), []byte(foreign), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := Init(home); err == nil {
			t.Fatal("init must refuse a config it could not have written before its first commit")
		}
		if got, err := os.ReadFile(s.ConfigPath()); err != nil || string(got) != foreign {
			t.Fatalf("refused init must preserve the existing config: %q %v", got, err)
		}
		if _, err := os.Lstat(filepath.Join(s.Dir(), ".git")); !os.IsNotExist(err) {
			t.Fatalf("refused init must not create a repository, got %v", err)
		}
	})

	t.Run("unborn repository with non-bootstrap config", func(t *testing.T) {
		home := t.TempDir()
		s := &Store{Home: home}
		if err := os.MkdirAll(s.SkillsDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		repo, err := git.PlainInit(s.Dir(), false)
		if err != nil {
			t.Fatal(err)
		}
		const foreign = "version: 1\nskills:\n  demo:\n    enabled: true\n"
		if err := os.WriteFile(s.ConfigPath(), []byte(foreign), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := Init(home); err == nil {
			t.Fatal("init must not adopt an unborn repository carrying a foreign config")
		}
		if _, err := repo.Head(); !errors.Is(err, plumbing.ErrReferenceNotFound) {
			t.Fatalf("refused init must leave the repository unborn, got %v", err)
		}
		if got, err := os.ReadFile(s.ConfigPath()); err != nil || string(got) != foreign {
			t.Fatalf("refused init must preserve the existing config: %q %v", got, err)
		}
	})
}

func TestOpenRequiresFuConfig(t *testing.T) {
	// An unrelated project repository, with real history, sitting exactly
	// where a mistyped FU_HOME would point fu at it.
	home := t.TempDir()
	dir := filepath.Join(home, "store")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("main.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("unrelated work", &git.CommitOptions{Author: fuSignature()}); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(home); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("a git repository without fu.yaml is not a fu store, want ErrStoreNotFound, got %v", err)
	}
}

// TestStoreLayoutMustHaveTheRightTypes is round 7's first Critical. The
// layout check accepted any entry *named* skills, fu.yaml or .git, without
// asking what kind of entry it was. A symlink at the recognized "skills"
// position therefore passed, and every later write followed it: Init's
// MkdirAll and NewSkill's own MkdirAll/WriteFile all address SkillsDir by
// pathname, so a successful `fu new` wrote outside the repository while git
// recorded nothing but the link itself. A *relative* link evades the
// absolute-symlink guard in stageAll, so that safeguard does not cover this.
//
// Names are the attacker's to choose; filesystem types are not. The check
// runs in Init (before anything is created) and again in Open (so a store
// that turns bad afterwards is caught before the next write, not after it).
func TestStoreLayoutMustHaveTheRightTypes(t *testing.T) {
	// corrupt replaces one layout entry of a real, working store with
	// something of the wrong type.
	cases := []struct {
		name    string
		corrupt func(t *testing.T, s *Store)
	}{
		{"skills is a symlink pointing outside the store", func(t *testing.T, s *Store) {
			outside := t.TempDir()
			if err := os.RemoveAll(s.SkillsDir()); err != nil {
				t.Fatal(err)
			}
			// Relative on purpose: stageAll's absolute-symlink guard does not
			// see this one, so nothing else in the system catches it.
			rel, err := filepath.Rel(s.Dir(), outside)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rel, s.SkillsDir()); err != nil {
				t.Fatal(err)
			}
		}},
		{"skills is a regular file", func(t *testing.T, s *Store) {
			if err := os.RemoveAll(s.SkillsDir()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.SkillsDir(), []byte("not a directory"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"fu.yaml is a symlink", func(t *testing.T, s *Store) {
			outside := filepath.Join(t.TempDir(), "elsewhere.yaml")
			if err := os.WriteFile(outside, []byte("version: 1\nskills: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(s.ConfigPath()); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, s.ConfigPath()); err != nil {
				t.Fatal(err)
			}
		}},
		{"fu.yaml is a directory", func(t *testing.T, s *Store) {
			if err := os.Remove(s.ConfigPath()); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(s.ConfigPath(), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run("Open refuses: "+tc.name, func(t *testing.T) {
			home := t.TempDir()
			s, err := Init(home)
			if err != nil {
				t.Fatal(err)
			}
			tc.corrupt(t, s)
			if _, err := Open(home); err == nil {
				t.Fatal("a store whose layout has the wrong type at a recognized name must be " +
					"refused, not opened and written through")
			}
		})
		t.Run("Init refuses: "+tc.name, func(t *testing.T) {
			home := t.TempDir()
			s, err := Init(home)
			if err != nil {
				t.Fatal(err)
			}
			tc.corrupt(t, s)
			if _, err := Init(home); err == nil {
				t.Fatal("init must refuse to resume onto a layout whose types are not fu's own")
			}
		})
	}
}

// TestNewSkillCannotWriteOutsideTheStore is the end-to-end half: the write
// path itself must not follow indirection at the skills position, even if
// something slips past the layout check.
func TestNewSkillCannotWriteOutsideTheStore(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(s.SkillsDir()); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(s.Dir(), outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, s.SkillsDir()); err != nil {
		t.Fatal(err)
	}

	// Whatever happens, nothing may be created beneath the redirected path.
	_, openErr := Open(home)
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("nothing may be written outside the store: found %d entries (Open err=%v)", len(ents), openErr)
	}
}

// TestOpenRequiresTrackedConfigNotJustAFileOnDisk is round 7's second
// Critical. Open used to consult the worktree first and history only as a
// fallback, so an unrelated repository with an untracked, schema-valid
// fu.yaml lying in it was accepted as a fu store. Every command then
// operated on that repository -- and Sweep, which runs before the requested
// mutation, committed its pending working-tree content under fu's own name
// and message, whether or not the mutation itself later succeeded.
//
// Identity has to be something the repository's own history asserts, not
// something a stray file can supply.
func TestOpenRequiresTrackedConfigNotJustAFileOnDisk(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "store")
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Real, unrelated history -- nothing to do with fu.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("main.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("unrelated work", &git.CommitOptions{Author: fuSignature()}); err != nil {
		t.Fatal(err)
	}
	// A perfectly valid fu.yaml, never committed. Under the old rule this
	// alone made the repository a "store".
	if err := os.WriteFile(filepath.Join(dir, "fu.yaml"), []byte("version: 1\nskills: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(home); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("an untracked fu.yaml must not confer store identity, want ErrStoreNotFound, got %v", err)
	}

	// The genuine article still opens: Init commits fu.yaml, so it is
	// tracked at HEAD.
	realHome := t.TempDir()
	if _, err := Init(realHome); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(realHome); err != nil {
		t.Fatalf("a store whose fu.yaml is tracked must open: %v", err)
	}
}
