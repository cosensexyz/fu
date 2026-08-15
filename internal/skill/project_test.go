package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestProjectDir pins the read-only projection: files with digest and mode,
// symlinks with raw targets, directories with mode, .git excluded at any
// depth, and special files refused.
func TestProjectDir(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "SKILL.md", "---\nname: pdf-tools\ndescription: d\n---\n")
	mustWrite(t, root, "scripts/run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(root, "scripts", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, root, "tools/.keep", "")
	mustWrite(t, root, ".git/hooks/sample", "x") // excluded by name
	mustWrite(t, root, "sub/.git", "x")          // a .git file also excluded
	if err := os.Symlink("scripts", filepath.Join(root, "scripts-link")); err != nil {
		t.Fatal(err)
	}
	// A directory fu's own walk must not descend into: a symlinked dir.
	if err := os.Symlink("tools", filepath.Join(root, "tools-link")); err != nil {
		t.Fatal(err)
	}

	entries, err := ProjectDir(os.DirFS(root), ".")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	byPath := map[string]ManifestEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	for _, want := range []string{"SKILL.md", "scripts", "scripts/run.sh", "tools", "tools/.keep", "scripts-link"} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("projection missing %q", want)
		}
	}
	for _, forbidden := range []string{".git", ".git/hooks/sample", "sub/.git"} {
		if _, ok := byPath[forbidden]; ok {
			t.Errorf("projection must exclude %q", forbidden)
		}
	}
	if e := byPath["scripts/run.sh"]; e.Mode.Perm() != 0o755 || e.Digest == "" {
		t.Errorf("run.sh projected mode=%v digest=%q", e.Mode, e.Digest)
	}
	if e := byPath["scripts-link"]; e.Target != "scripts" {
		t.Errorf("scripts-link target = %q, want scripts", e.Target)
	}
	if e := byPath["tools-link"]; e.Target != "tools" {
		t.Errorf("tools-link target = %q, want tools", e.Target)
	}
}

// TestProjectDirMatchesDigestFS pins the one agreement this projection
// exists to keep (DESIGN §3): ProjectDir and DigestFS see the same set, so
// the source projection and the store digest cannot drift.
func TestProjectDirMatchesDigestFS(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "SKILL.md", "---\nname: a\ndescription: d\n---\n")
	mustWrite(t, root, "lib/x.txt", "hello\n")
	if err := os.Chmod(filepath.Join(root, "lib", "x.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lib", filepath.Join(root, "lib-link")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, root, ".git/config", "x")

	entries, err := ProjectDir(os.DirFS(root), ".")
	if err != nil {
		t.Fatal(err)
	}
	fromEntries, err := DigestManifest(entries)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	if fromEntries != direct {
		t.Fatalf("ProjectDir+DigestManifest = %s, Digest = %s", fromEntries, direct)
	}
}

func TestProjectDirRefusesFileLargerThanCopyLimit(t *testing.T) {
	root := t.TempDir()
	oversized := filepath.Join(root, "oversized.bin")
	if err := os.WriteFile(oversized, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversized, (64<<20)+1); err != nil {
		t.Fatal(err)
	}

	if _, err := ProjectDir(os.DirFS(root), "."); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("ProjectDir oversized error = %v, want shared 64 MiB limit", err)
	}
}

// TestProjectDirRefusesSpecialFile pins that a FIFO is refused before any
// open could block (the walk classifies before opening).
func TestProjectDirRefusesSpecialFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "SKILL.md", "---\nname: a\ndescription: d\n---\n")
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	if _, err := ProjectDir(os.DirFS(root), "."); err == nil {
		t.Fatal("a FIFO inside a skill must be refused")
	}
}

func TestProjectDirRefusesGitSymlink(t *testing.T) {
	for _, rel := range []string{".git", "nested/.git"} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(".", filepath.Join(root, rel)); err != nil {
				t.Fatal(err)
			}
			if _, err := ProjectDir(os.DirFS(root), "."); err == nil {
				t.Fatal("a .git symlink must be refused rather than hidden from the projection")
			}
		})
	}
}

func TestDigestFSRefusesGitSymlinkLikeProjectDir(t *testing.T) {
	for _, rel := range []string{".git", "nested/.git"} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(".", filepath.Join(root, rel)); err != nil {
				t.Fatal(err)
			}
			if _, err := DigestFS(os.DirFS(root), "."); err == nil {
				t.Fatal("DigestFS must refuse the same .git symlink that ProjectDir refuses")
			}
		})
	}
}

func TestValidateLinks(t *testing.T) {
	link := func(p, target string) ManifestEntry {
		return ManifestEntry{Path: p, Mode: os.ModeSymlink, Target: target}
	}
	file := func(p string) ManifestEntry {
		return ManifestEntry{Path: p, Mode: 0}
	}
	cases := []struct {
		name      string
		entries   []ManifestEntry
		wantError bool
	}{
		{
			name: "no links",
			entries: []ManifestEntry{
				file("a.txt"),
				{Path: "sub", Mode: os.ModeDir},
			},
		},
		{
			name: "relative link inside root",
			entries: []ManifestEntry{
				file("a.txt"),
				link("link.txt", "a.txt"),
			},
		},
		{
			name: "relative link to subdir",
			entries: []ManifestEntry{
				{Path: "sub", Mode: os.ModeDir},
				link("go", "sub"),
			},
		},
		{
			name: "dotdot inside root is fine",
			entries: []ManifestEntry{
				{Path: "a", Mode: os.ModeDir},
				file("a/b.txt"),
				link("c.txt", "a/../a/b.txt"),
			},
		},
		{
			name: "absolute target refused",
			entries: []ManifestEntry{
				file("a.txt"),
				link("evil", "/etc/passwd"),
			},
			wantError: true,
		},
		{
			name: "dotdot escape refused",
			entries: []ManifestEntry{
				file("a.txt"),
				link("evil", "../outside"),
			},
			wantError: true,
		},
		{
			name: "nested dotdot escape refused",
			entries: []ManifestEntry{
				{Path: "a", Mode: os.ModeDir},
				file("a/b.txt"),
				link("evil", "a/../../outside"),
			},
			wantError: true,
		},
		{
			name: "chain into outside refused",
			entries: []ManifestEntry{
				{Path: "a", Mode: os.ModeDir},
				link("a/hop", "../../out"),
				link("evil", "a/hop"),
			},
			wantError: true,
		},
		{
			name: "legal chain stays inside",
			entries: []ManifestEntry{
				{Path: "a", Mode: os.ModeDir},
				file("a/target.txt"),
				link("a/hop", "target.txt"),
				link("evil", "a/hop"),
			},
		},
		{
			name: "cycle refused",
			entries: []ManifestEntry{
				link("a", "b"),
				link("b", "a"),
			},
			wantError: true,
		},
		{
			name: "self cycle refused",
			entries: []ManifestEntry{
				link("a", "a"),
			},
			wantError: true,
		},
		{
			name: "escape through link resolving to root",
			entries: []ManifestEntry{
				file("x"),
				link("b", "."),
				link("a", "b/../x"),
			},
			wantError: true,
		},
		{
			name: "escape through link resolving to root via sub",
			entries: []ManifestEntry{
				link("b", "sub/.."),
				link("a", "b/.."),
			},
			wantError: true,
		},
		{
			name: "broken link inside root is fine",
			entries: []ManifestEntry{
				link("gone", "not-there.txt"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLinks(tc.entries)
			if tc.wantError && err == nil {
				t.Fatal("want an escape error, got none")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestParseMetaRefusesFifoSkillFile pins the classify-before-open discipline on
// the one reader that lacked it. fsys.Open on a FIFO with no writer never
// returns, and readSkillFile is reached for every directory `fu add
// <local-dir>` scans and for every `fu adopt` candidate, so a mkfifo'd SKILL.md
// hung the command outright. DigestFS's own comment states the rule and
// ProjectDir implements it; TestProjectDirRefusesSpecialFile pins that half.
func TestParseMetaRefusesFifoSkillFile(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "SKILL.md"), 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ParseMeta(root)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO SKILL.md must be refused")
		}
		if !strings.Contains(err.Error(), "named pipe") {
			t.Fatalf("the refusal must name what it found: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ParseMeta blocked on a FIFO instead of classifying it first")
	}
}
