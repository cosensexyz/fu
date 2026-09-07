package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func rewriteTransitionFixture(t *testing.T, forward bool) *Store {
	t.Helper()
	s := applyFixture(t)
	shape := filepath.Join(s.SkillsDir(), "shape")
	if err := os.Mkdir(shape, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shape/child.txt", "removed.txt"} {
		if err := os.WriteFile(filepath.Join(s.SkillsDir(), name), []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Commit("new: before transition"); err != nil {
		t.Fatal(err)
	}
	first, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	// Stage the tracked child deletion before replacing its parent directory.
	wt, err := s.Repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Remove("skills/shape/child.txt"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shape", "removed.txt"} {
		if err := os.Remove(filepath.Join(s.SkillsDir(), name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"shape", "added.txt"} {
		if err := os.WriteFile(filepath.Join(s.SkillsDir(), name), []byte("after"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Commit("update: transition"); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildIndexFromTarget(headTargets(t, s)); err != nil {
		t.Fatal(err)
	}
	if forward {
		second, err := s.Repo.Head()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", first.Name().Short()), second.Hash())); err != nil {
			t.Fatal(err)
		}
		if err := s.Repo.Storer.SetReference(first); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResetWorktreeToHead(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRewriteProtectsDeletionAndCreation(t *testing.T) {
	for _, forward := range []bool{false, true} {
		for _, deletion := range []bool{false, true} {
			label := map[bool]string{false: "revert", true: "pull"}[forward] + "/" + map[bool]string{false: "create", true: "delete"}[deletion]
			t.Run(label, func(t *testing.T) {
				s := rewriteTransitionFixture(t, forward)
				name := "skills/removed.txt"
				if forward {
					name = "skills/added.txt"
				}
				if deletion {
					name = "skills/shape"
					if forward {
						name = "skills/shape/child.txt"
					}
				}
				fired := false
				hooks := worktreeRewriteHooks{beforePath: func(path string) {
					if path == name {
						fired = true
						if err := os.WriteFile(filepath.Join(s.Dir(), name), []byte("editor"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}}
				var err error
				if forward {
					_, err = s.fastForwardWithHooks(hooks)
				} else {
					_, err = s.revertWithHooks(1, hooks)
				}
				if !fired || !errors.Is(err, ErrConcurrentWorktreeChange) {
					t.Fatalf("expected a conflict at %s: fired=%v err=%v", name, fired, err)
				}
				if got, err := os.ReadFile(filepath.Join(s.Dir(), name)); err != nil || string(got) != "editor" {
					t.Fatalf("editor bytes lost at %s: %q, %v", name, got, err)
				}
			})
		}
	}
}

func TestRewriteAllowsTrackedTypeTransitions(t *testing.T) {
	for _, forward := range []bool{false, true} {
		t.Run(map[bool]string{false: "file-to-directory", true: "directory-to-file"}[forward], func(t *testing.T) {
			s := rewriteTransitionFixture(t, forward)
			var err error
			if forward {
				_, err = s.FastForward()
			} else {
				_, err = s.Revert(1)
			}
			if err != nil {
				t.Fatal(err)
			}
			shape := filepath.Join(s.SkillsDir(), "shape")
			info, err := os.Stat(shape)
			if err != nil || info.IsDir() == forward {
				t.Fatalf("wrong final type: %v, %v", info, err)
			}
			if dirty, err := s.ChangedPathsIncludingIgnored(); err != nil || len(dirty) != 0 {
				t.Fatalf("transition did not converge: %v, %v", dirty, err)
			}
		})
	}
}
