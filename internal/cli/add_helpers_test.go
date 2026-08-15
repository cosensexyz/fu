package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeSkill plants a valid single-skill tree at root/<name>.
func writeSkill(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: d\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runCmdWithInput is runCmd with stdin supplied.
func runCmdWithInput(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var out, in bytes.Buffer
	in.WriteString(input)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(&in)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// captureOutput executes one command with stdin input and returns stdout.
func captureOutput(t *testing.T, cmd *cobra.Command, input string, args ...string) string {
	t.Helper()
	var out, in bytes.Buffer
	in.WriteString(input)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(&in)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute %v: %v", args, err)
	}
	return out.String()
}

// makeGitSourceCLI builds a bare repository holding one skill and returns
// its file:// URL.
func makeGitSourceCLI(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	writeSkill(t, work, "pdf-tools")
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("seed", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}}); err != nil {
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
	headRef, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + headRef.Name().Short() + ":refs/heads/" + headRef.Name().Short())},
	}); err != nil {
		t.Fatal(err)
	}
	return "file://" + bare
}
