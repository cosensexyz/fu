package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDigestDeterministicAndSensitive(t *testing.T) {
	dir := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	d1, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := Digest(dir)
	if d1 != d2 {
		t.Fatalf("not deterministic: %s vs %s", d1, d2)
	}
	// content change changes digest
	os.WriteFile(filepath.Join(dir, "extra.md"), []byte("x"), 0o644)
	d3, _ := Digest(dir)
	if d3 == d1 {
		t.Fatal("digest ignored new file")
	}
	// exec bit change changes digest
	os.Chmod(filepath.Join(dir, "extra.md"), 0o755)
	d4, _ := Digest(dir)
	if d4 == d3 {
		t.Fatal("digest ignored exec bit")
	}
	// symlink target is part of the projection
	os.Symlink("/target/a", filepath.Join(dir, "ln"))
	d5, _ := Digest(dir)
	if d5 == d4 {
		t.Fatal("digest ignored symlink")
	}
}

// Test gap T3, probe 1/3: a rename must change the digest.
// TestDigestDeterministicAndSensitive above only ever *adds* entries; it
// never exercises a path changing while a file's content stays the same,
// so a projection that keyed only on content (not on relative path)
// could be rename-blind and still pass it.
func TestDigestChangesOnRename(t *testing.T) {
	dir := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	original := filepath.Join(dir, "original.md")
	if err := os.WriteFile(original, []byte("same content"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}

	renamed := filepath.Join(dir, "renamed.md")
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	after, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("renaming a file (same content, different path) must change the digest")
	}
}

// Test gap T3, probe 2/3: retargeting an existing symlink (same name,
// different target) must change the digest. Dropping the target from the
// per-entry record -- as opposed to merely dropping the entry's presence,
// which TestDigestDeterministicAndSensitive above already covers by
// *adding* a symlink -- would let a retarget collide with the
// pre-retarget digest, since the entry's relative path and type tag are
// otherwise unchanged. DESIGN §4's baseline three-state comparison would
// then read a retargeted link as "not locally modified," and the next
// plan's `fu update` would overwrite it on that false premise.
func TestDigestChangesOnSymlinkRetarget(t *testing.T) {
	dir := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	link := filepath.Join(dir, "ln")
	if err := os.Symlink("/target/a", link); err != nil {
		t.Fatal(err)
	}
	before, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/target/b", link); err != nil {
		t.Fatal(err)
	}
	after, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("retargeting a symlink (same name, different target) must change the digest")
	}
}

// TestDigestIgnoresEmptyDirectory reverses this test's own prior assertion
// (round 6 finding; it used to be TestDigestChangesOnEmptyDirectory and
// required the opposite). The old requirement was reasonable read alone --
// "a layout relying on an empty directory's mere presence must be hashed"
// -- but it put the projection permanently at odds with the one thing it
// has to agree with: git cannot store an empty directory, so a digest that
// counts one can never match the same skill's digest after a clone. DESIGN
// §3 makes copy, digest and history *one* projection; where they cannot
// all represent something, the projection is what gives way.
//
// The cost is stated rather than hidden: fu cannot detect a skill change
// that consists solely of adding or removing an empty directory. Neither
// can git, so the alternative was not "detect it" but "disagree with the
// store's own history forever."
func TestDigestIgnoresEmptyDirectory(t *testing.T) {
	dir := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	before, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "empty-subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("an empty directory is not something git can persist, so it must not affect the digest")
	}

	// A directory that does hold content still registers, through that
	// content's own record -- the projection loses only what git loses.
	if err := os.WriteFile(filepath.Join(dir, "empty-subdir", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	withFile, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if withFile == before {
		t.Fatal("a file inside a subdirectory must change the digest")
	}
}

func TestDigestExcludesGit(t *testing.T) {
	a := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	b := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	os.MkdirAll(filepath.Join(b, ".git"), 0o755)
	os.WriteFile(filepath.Join(b, ".git", "HEAD"), []byte("ref"), 0o644)
	da, _ := Digest(a)
	db, _ := Digest(b)
	if da != db {
		t.Fatal(".git must be excluded from the projection")
	}
}

func TestDigestFSRefusesFileLargerThanCopyLimit(t *testing.T) {
	root := t.TempDir()
	oversized := filepath.Join(root, "oversized.bin")
	if err := os.WriteFile(oversized, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversized, (64<<20)+1); err != nil {
		t.Fatal(err)
	}

	if _, err := DigestFS(os.DirFS(root), "."); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("DigestFS oversized error = %v, want shared 64 MiB limit", err)
	}
}

// TestDigestExcludesGitWorktreeFile covers the .git-as-file form used by
// git worktrees and submodules (a regular file containing a "gitdir:"
// pointer, instead of a real .git directory). It must be excluded from
// the projection exactly like the directory form.
func TestDigestExcludesGitWorktreeFile(t *testing.T) {
	a := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	b := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	if err := os.WriteFile(filepath.Join(b, ".git"), []byte("gitdir: ../.git/worktrees/s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	da, err := Digest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Digest(b)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatal(".git file (worktree/submodule pointer) must be excluded from the projection")
	}
}

// TestDigestSymlinkEncodingIsInjective proves the per-entry encoding no
// longer collides. With a human-readable "%s -> %s" rendering, a symlink
// literally named "link1 -> a" targeting "b" renders identically to a
// symlink named "link1" targeting "a -> b" (both become the line
// "L link1 -> a -> b"), so two structurally different directories used to
// hash the same. The NUL-delimited encoding must tell them apart.
func TestDigestSymlinkEncodingIsInjective(t *testing.T) {
	dirA := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	if err := os.Symlink("b", filepath.Join(dirA, "link1 -> a")); err != nil {
		t.Fatal(err)
	}

	dirB := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	if err := os.Symlink("a -> b", filepath.Join(dirB, "link1")); err != nil {
		t.Fatal(err)
	}

	da, err := Digest(dirA)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Digest(dirB)
	if err != nil {
		t.Fatal(err)
	}
	if da == db {
		t.Fatalf("structurally different symlink layouts hashed to the same digest: %s", da)
	}
}

// TestDigestPropagatesUnreadableFileError proves Digest returns an error
// instead of a wrong-but-plausible digest when an entry cannot be read,
// rather than silently omitting it or panicking.
func TestDigestPropagatesUnreadableFileError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	dir := writeSkill(t, filepath.Join(t.TempDir(), "s"), "name: s\ndescription: d")
	secret := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(secret, []byte("classified"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(secret, 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := Digest(dir); err == nil {
		t.Fatal("want error for unreadable file, got nil")
	}
}

// DigestFS is designated as the shared projection for add/adopt/update, so a
// blocking open here would hang while fu.lock is held, wedging every other fu
// process. Its sibling DigestManifest already refuses unsupported modes.
func TestDigestFSRefusesSpecialFilesInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := DigestFS(os.DirFS(dir), ".")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO in the tree must be refused, not digested")
		}
		if !strings.Contains(err.Error(), "pipe") {
			t.Fatalf("the refusal must name the entry, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DigestFS blocked on a FIFO while the write lock would be held")
	}
}
