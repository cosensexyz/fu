package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// rewriteFixture keeps both target trees in a real repository. FastForward
// starts at the first tree and follows a tracking ref to the second; Revert
// starts at the second and returns to the first through an operation commit.
func rewriteFixture(t *testing.T, forward bool) *Store {
	t.Helper()
	s := applyFixture(t)
	first, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "plain.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "run.sh"), []byte("#!/bin/sh\necho second\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit("new: second"); err != nil {
		t.Fatal(err)
	}
	second, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if forward {
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

func TestRewriteRefusesChangesAtMutationBoundaries(t *testing.T) {
	for _, forward := range []bool{false, true} {
		command := "revert"
		if forward {
			command = "pull"
		}
		for _, boundary := range []string{"initial", "next-file", "index", "final-content", "head", "symbolic-head"} {
			t.Run(command+"/"+boundary, func(t *testing.T) {
				s := rewriteFixture(t, forward)
				head, err := s.Repo.Head()
				if err != nil {
					t.Fatal(err)
				}
				indexPath := filepath.Join(s.Dir(), ".git", "index")
				beforeIndex, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				var editorIndex []byte
				var editorHead *plumbing.Reference
				var editedPath string
				fired := false
				hooks := worktreeRewriteHooks{}
				editFile := func(name string) {
					fired = true
					editedPath = filepath.Join(s.Dir(), filepath.FromSlash(name))
					if err := os.WriteFile(editedPath, []byte("editor content"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				switch boundary {
				case "initial":
					hooks.beforeApply = func() { editFile("skills/plain.txt") }
				case "next-file":
					hooks.beforePath = func(name string) {
						if name == "skills/run.sh" {
							editFile(name)
						}
					}
				case "index":
					hooks.beforeIndex = func() {
						fired = true
						idx, err := s.Repo.Storer.Index()
						if err != nil {
							t.Fatal(err)
						}
						if _, err := idx.Remove("skills/run.sh"); err != nil {
							t.Fatal(err)
						}
						if err := s.Repo.Storer.SetIndex(idx); err != nil {
							t.Fatal(err)
						}
						editorIndex, err = os.ReadFile(indexPath)
						if err != nil {
							t.Fatal(err)
						}
					}
				case "final-content":
					hooks.beforePublish = func() { editFile("skills/plain.txt") }
				case "head", "symbolic-head":
					hooks.beforePublish = func() {
						fired = true
						if boundary == "symbolic-head" {
							editorHead = plumbing.NewHashReference(plumbing.HEAD, head.Hash())
						} else {
							// A different valid commit is enough to distinguish a
							// captured-parent CAS from adopting the latest parent.
							commit, err := s.Repo.CommitObject(head.Hash())
							if err != nil {
								t.Fatal(err)
							}
							editorHead = plumbing.NewHashReference(head.Name(), commit.ParentHashes[0])
						}
						if err := s.Repo.Storer.SetReference(editorHead); err != nil {
							t.Fatal(err)
						}
					}
				}
				var changed []string
				if forward {
					out, applyErr := s.fastForwardWithHooks(hooks)
					changed, err = out.Changed, applyErr
				} else {
					changed, err = s.revertWithHooks(1, hooks)
				}
				if !fired {
					t.Fatal("mutation boundary was not reached")
				}
				if !errors.Is(err, ErrConcurrentWorktreeChange) {
					t.Errorf("must report a concurrent change, got %v", err)
				}
				if boundary == "initial" && len(changed) != 0 {
					t.Errorf("initial refusal modified %v", changed)
				}
				if boundary != "initial" && !slices.Contains(changed, "skills/plain.txt") {
					t.Errorf("partial outcome lost the first applied path: %v", changed)
				}
				if editedPath != "" {
					if got, err := os.ReadFile(editedPath); err != nil || string(got) != "editor content" {
						t.Errorf("editor content lost: %q, %v", got, err)
					}
				}
				if editorHead != nil {
					got, err := s.Repo.Storer.Reference(editorHead.Name())
					if err != nil || got.String() != editorHead.String() {
						t.Errorf("editor reference lost: %v, %v", got, err)
					}
				} else {
					got, err := s.Repo.Head()
					if err != nil || got.String() != head.String() {
						t.Errorf("refusal advanced HEAD: %v, %v", got, err)
					}
				}
				if boundary == "initial" || boundary == "next-file" || boundary == "index" {
					want := beforeIndex
					if editorIndex != nil {
						want = editorIndex
					}
					if got, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(got, want) {
						t.Errorf("refusal overwrote the public index: %v", err)
					}
				}
			})
		}
	}
}

func TestRewriteRefusesPostSweepChanges(t *testing.T) {
	for _, forward := range []bool{false, true} {
		command := "revert"
		if forward {
			command = "pull"
		}
		for _, edit := range []string{"bytes", "deleted", "directory", "symlink", "config", "index", "unmerged", "mode", "ignored"} {
			t.Run(command+"/"+edit, func(t *testing.T) {
				s := rewriteFixture(t, forward)
				name := filepath.Join(s.SkillsDir(), "plain.txt")
				head, err := s.Repo.Head()
				if err != nil {
					t.Fatal(err)
				}
				switch edit {
				case "bytes":
					info, err := os.Stat(name)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(name, []byte("xx"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "deleted", "directory", "symlink":
					if err := os.Remove(name); err != nil {
						t.Fatal(err)
					}
					if edit == "directory" {
						if err := os.Mkdir(name, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					if edit == "symlink" {
						if err := os.Symlink("run.sh", name); err != nil {
							t.Fatal(err)
						}
					}
				case "config":
					name = s.ConfigPath()
					if err := os.WriteFile(name, []byte("version: 1\nskills: {}\n# editor\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				case "mode":
					if err := os.Chmod(name, 0o755); err != nil {
						t.Fatal(err)
					}
				case "ignored":
					if err := os.MkdirAll(filepath.Join(s.Dir(), ".git", "info"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(s.Dir(), ".git", "info", "exclude"), []byte("*.tmp\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					name = filepath.Join(s.SkillsDir(), "editor.tmp")
					if err := os.WriteFile(name, []byte("xx"), 0o644); err != nil {
						t.Fatal(err)
					}
				case "index", "unmerged":
					idx, err := s.Repo.Storer.Index()
					if err != nil {
						t.Fatal(err)
					}
					if edit == "unmerged" {
						idx.Entries[0].Stage = 1
					} else {
						if _, err := idx.Remove("skills/plain.txt"); err != nil {
							t.Fatal(err)
						}
					}
					if err := s.Repo.Storer.SetIndex(idx); err != nil {
						t.Fatal(err)
					}
				}
				beforeIndex, err := os.ReadFile(filepath.Join(s.Dir(), ".git", "index"))
				if err != nil {
					t.Fatal(err)
				}
				var changed []string
				if forward {
					out, applyErr := s.FastForward()
					changed, err = out.Changed, applyErr
				} else {
					changed, err = s.Revert(1)
				}
				if err == nil {
					t.Errorf("%s must refuse a post-sweep %s edit", command, edit)
				}
				if len(changed) != 0 {
					t.Errorf("initial refusal must precede every write, changed=%v", changed)
				}
				after, headErr := s.Repo.Head()
				if headErr != nil || after.String() != head.String() {
					t.Errorf("HEAD changed after refusal: %v, %v", after, headErr)
				}
				afterIndex, readErr := os.ReadFile(filepath.Join(s.Dir(), ".git", "index"))
				if readErr != nil || !bytes.Equal(beforeIndex, afterIndex) {
					t.Errorf("public index changed after refusal: %v", readErr)
				}
				switch edit {
				case "bytes", "config", "ignored":
					want := "xx"
					if edit == "config" {
						want = "version: 1\nskills: {}\n# editor\n"
					}
					got, err := os.ReadFile(name)
					if err != nil || string(got) != want {
						t.Errorf("editor bytes were overwritten: %q, %v", got, err)
					}
				case "deleted":
					if _, err := os.Lstat(name); !os.IsNotExist(err) {
						t.Errorf("editor deletion was overwritten: %v", err)
					}
				case "directory":
					if info, err := os.Lstat(name); err != nil || !info.IsDir() {
						t.Errorf("editor directory was overwritten: %v", err)
					}
				case "symlink":
					if got, err := os.Readlink(name); err != nil || got != "run.sh" {
						t.Errorf("editor symlink was overwritten: %q, %v", got, err)
					}
				case "mode":
					if info, err := os.Stat(name); err != nil || info.Mode().Perm() != 0o755 {
						t.Errorf("editor mode was overwritten: %v", err)
					}
				}
			})
		}
	}
}

func TestRewritePartialFailureLeavesEditsForTheNextSweep(t *testing.T) {
	for _, forward := range []bool{false, true} {
		t.Run(map[bool]string{false: "revert", true: "pull"}[forward], func(t *testing.T) {
			s := rewriteFixture(t, forward)
			fired := false
			hooks := worktreeRewriteHooks{beforePath: func(name string) {
				if name == "skills/run.sh" {
					fired = true
					if err := os.WriteFile(filepath.Join(s.Dir(), name), []byte("later editor bytes"), 0o755); err != nil {
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
				t.Fatalf("expected partial conflict: fired=%v, err=%v", fired, err)
			}
			if err := s.Sweep(); err != nil {
				t.Fatal(err)
			}
			head, err := s.Repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			commit, err := s.Repo.CommitObject(head.Hash())
			if err != nil {
				t.Fatal(err)
			}
			file, err := commit.File("skills/run.sh")
			if err != nil {
				t.Fatal(err)
			}
			content, err := file.Contents()
			if err != nil || content != "later editor bytes" || commit.Message != ExternalCommitMessage {
				t.Fatalf("next sweep did not preserve the editor's bytes: %q, %s, %v", content, commit.Message, err)
			}
		})
	}
}

func TestRewriteCommitKeepsTheCapturedParent(t *testing.T) {
	for _, detach := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch", true: "HEAD"}[detach], func(t *testing.T) {
			s := rewriteFixture(t, false)
			guard, err := s.newWorktreeGuard()
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := s.PrepareCommit()
			if err != nil {
				t.Fatal(err)
			}
			commit, err := s.Repo.CommitObject(guard.ref.before.Hash())
			if err != nil {
				t.Fatal(err)
			}
			name := guard.ref.target
			if detach {
				name = plumbing.HEAD
			}
			editor := plumbing.NewHashReference(name, commit.ParentHashes[0])
			if err := s.Repo.Storer.SetReference(editor); err != nil {
				t.Fatal(err)
			}
			out, err := s.commitPreparedWithReference("revert: captured parent", prepared, nil, &guard.ref)
			if !errors.Is(err, ErrConcurrentWorktreeChange) || out.Written {
				t.Errorf("must refuse a changed parent before commit: %+v, %v", out, err)
			}
			got, err := s.Repo.Storer.Reference(name)
			if err != nil || got.String() != editor.String() {
				t.Errorf("editor reference was overwritten: %v, %v", got, err)
			}
		})
	}
}

func TestRewritePruningPreservesReplacedParent(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "symlink"}[symlink], func(t *testing.T) {
			s := applyFixture(t)
			name := filepath.Join(s.SkillsDir(), "replaced")
			if symlink {
				if err := os.Symlink("plain.txt", name); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(name, []byte("editor replacement"), 0o644); err != nil {
				t.Fatal(err)
			}
			// This is the state after the last tracked child was removed and
			// an editor replaced its now-empty parent before ancestor pruning.
			if err := s.pruneEmptiedParents("skills/replaced/child.txt"); err == nil {
				t.Error("pruning must refuse a parent that is no longer a directory")
			}
			if symlink {
				if got, err := os.Readlink(name); err != nil || got != "plain.txt" {
					t.Errorf("pruning deleted the editor's symlink: %q, %v", got, err)
				}
			} else if got, err := os.ReadFile(name); err != nil || string(got) != "editor replacement" {
				t.Errorf("pruning deleted the editor's file: %q, %v", got, err)
			}
		})
	}
}
