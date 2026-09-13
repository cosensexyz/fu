package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	number, err := inodeNumber(path)
	if err != nil {
		t.Fatal(err)
	}
	return number
}

// ext4 with a journal hands out the lowest free inode number in the group,
// so a same-name replacement lands on the original's number only while no
// lower number is free. Any lower hole -- fu's own temporary files, another
// test's cleanup in a parallel package -- sends every retry to that hole
// instead. This is the exact situation the whole-tree CI run produces, and
// the helper must reuse the original number regardless.
func TestReplaceOnSameInodeReusesTheNumberDespiteALowerFreeOne(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("inode reuse is a Linux filesystem behaviour")
	}
	for _, kind := range []struct {
		name string
		dir  bool
	}{{"directory", true}, {"file", false}} {
		t.Run(kind.name, func(t *testing.T) {
			dir := t.TempDir()
			// A file created before the entry and removed after it leaves a
			// free number below the entry's.
			hole := filepath.Join(dir, "hole")
			entry := filepath.Join(dir, "entry")
			if err := os.WriteFile(hole, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			// adopt's shape: a symlink replaced under its own name.
			if err := os.Symlink("/nowhere", entry); err != nil {
				t.Fatal(err)
			}
			original := inodeOf(t, entry)
			if err := os.Remove(hole); err != nil {
				t.Fatal(err)
			}

			reuse, err := ReplaceOnSameInode(dir, "entry", kind.dir)
			if err != nil {
				t.Fatal(err)
			}

			info, err := os.Lstat(entry)
			if err != nil {
				t.Fatal(err)
			}
			if info.IsDir() != kind.dir || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("replacement mode = %v, want a plain %s", info.Mode(), kind.name)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "entry" {
				t.Fatalf("the helper must leave nothing but the replacement behind, got %d entries", len(entries))
			}
			if !reuse.Reused {
				if FileHandlesRequired() {
					t.Fatalf("replacement landed on inode %d, not the original %d, on a filesystem required to hand freed numbers back: %s", inodeOf(t, entry), original, reuse.Miss)
				}
				t.Skipf("filesystem did not hand inode %d back: %s", original, reuse.Miss)
			}
			if got := inodeOf(t, entry); got != original {
				t.Fatalf("reuse reported but the replacement sits on inode %d, original %d", got, original)
			}
		})
	}
}

// Wherever the filesystem never hands a number back (APFS), the helper must
// still install the replacement and report the miss truthfully.
func TestReplaceOnSameInodeInstallsTheReplacementEvenWithoutReuse(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "entry")
	if err := os.WriteFile(entry, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := inodeOf(t, entry)

	reuse, err := ReplaceOnSameInode(dir, "entry", false)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the replacement must be a fresh empty file, got %q", got)
	}
	if reuse.Reused != (inodeOf(t, entry) == original) {
		t.Fatalf("reuse = %+v, but inode %d vs original %d", reuse, inodeOf(t, entry), original)
	}
	if reuse.Reused == (reuse.Miss != "") {
		t.Fatalf("a miss must carry its reason and a reuse must not: %+v", reuse)
	}
}

// A directory original releases its contents' numbers together with its
// own, so a child sitting below the directory's number would become a hole
// only after the placeholders were placed. The helper must empty the
// directory before it fills, so that a single attempt lands even outside
// the required coverage's retries.
//
// The helper's own retry is switched off here so that it cannot mask the
// regression, which leaves two windows a concurrent allocation or release
// in a parallel package can lose for this test: the child's number between
// the hole's removal and the child's creation, and the directory's number
// inside the single attempt. The regression misses every time, a lost race
// only once, so the fixture is rebuilt a few times before a miss counts.
func TestReplaceOnSameInodeEmptiesADirectoryOriginalBeforeFilling(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("inode reuse is a Linux filesystem behaviour")
	}
	required := FileHandlesRequired()
	// One attempt only: no retry may rescue a miss here.
	t.Setenv("FU_REQUIRE_FILE_HANDLES", "")
	const attempts = 3
	var lastMiss string
	for range attempts {
		dir := t.TempDir()
		low := filepath.Join(dir, "low")
		entry := filepath.Join(dir, "entry")
		if err := os.WriteFile(low, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(entry, 0o755); err != nil {
			t.Fatal(err)
		}
		original := inodeOf(t, entry)
		if err := os.Remove(low); err != nil {
			t.Fatal(err)
		}
		// The child takes the number low held, below the directory's own.
		child := filepath.Join(entry, "child")
		if err := os.WriteFile(child, []byte("child"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := inodeOf(t, child); got >= original {
			lastMiss = fmt.Sprintf("child landed on inode %d, not below the directory's %d", got, original)
			continue
		}

		reuse, err := ReplaceOnSameInode(dir, "entry", true)
		if err != nil {
			t.Fatal(err)
		}

		inside, err := os.ReadDir(entry)
		if err != nil {
			t.Fatal(err)
		}
		if len(inside) != 0 {
			t.Fatalf("the replacement directory must be empty, got %d entries", len(inside))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "entry" {
			t.Fatalf("the helper must leave nothing but the replacement behind, got %d entries", len(entries))
		}
		if !reuse.Reused {
			lastMiss = reuse.Miss
			continue
		}
		if got := inodeOf(t, entry); got != original {
			t.Fatalf("reuse reported but the replacement sits on inode %d, original %d", got, original)
		}
		return
	}
	if required {
		t.Fatalf("a single attempt must land on the directory's number once its contents are out of the way; %d fixtures in a row missed, last: %s", attempts, lastMiss)
	}
	t.Skipf("filesystem did not hand the directory's number back in %d fixtures, last: %s", attempts, lastMiss)
}

// The required coverage's retry loop has a deadline; when it passes, the
// replacement stays installed, nothing else is left behind, and the miss
// names both numbers so a CI failure can be read without reproducing it.
func TestReplaceOnSameInodeReportsANumberThatNeverComesBack(t *testing.T) {
	t.Setenv("FU_REQUIRE_FILE_HANDLES", "1")
	saved := replaceOnSameInodeDeadline
	replaceOnSameInodeDeadline = 50 * time.Millisecond
	t.Cleanup(func() { replaceOnSameInodeDeadline = saved })
	dir := t.TempDir()
	entry := filepath.Join(dir, "entry")
	if err := os.WriteFile(entry, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := inodeOf(t, entry)
	// An open descriptor keeps the inode allocated after its name is gone, so
	// the number cannot come back while it is held; APFS would not hand it
	// back anyway.
	held, err := os.Open(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	reuse, err := ReplaceOnSameInode(dir, "entry", false)
	if err != nil {
		t.Fatal(err)
	}

	if reuse.Reused {
		t.Fatalf("a number held by an open descriptor cannot be reused: %+v", reuse)
	}
	replacement := inodeOf(t, entry)
	for _, want := range []string{fmt.Sprint(original), fmt.Sprint(replacement)} {
		if !strings.Contains(reuse.Miss, want) {
			t.Fatalf("miss %q must name inode %s", reuse.Miss, want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "entry" {
		t.Fatalf("the helper must leave nothing but the replacement behind, got %d entries", len(entries))
	}
	got, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the replacement must be a fresh empty file, got %q", got)
	}
}

// A symlink original replaced by a symlink: the kind fu's link retirement
// deals in. The replacement carries the requested target and, on ext4, the
// original's number.
func TestReplaceOnSameInodeWithASymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("inode reuse is a Linux filesystem behaviour")
	}
	dir := t.TempDir()
	hole := filepath.Join(dir, "hole")
	entry := filepath.Join(dir, "entry")
	if err := os.WriteFile(hole, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/original/target", entry); err != nil {
		t.Fatal(err)
	}
	original := inodeOf(t, entry)
	if err := os.Remove(hole); err != nil {
		t.Fatal(err)
	}

	reuse, err := ReplaceOnSameInodeWith(dir, "entry", Replacement{SymlinkTarget: "/replacement/target"})
	if err != nil {
		t.Fatal(err)
	}

	if target, err := os.Readlink(entry); err != nil || target != "/replacement/target" {
		t.Fatalf("replacement = %q, %v; want a symlink to the requested target", target, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "entry" {
		t.Fatalf("the helper must leave nothing but the replacement behind, got %d entries", len(entries))
	}
	if !reuse.Reused {
		if FileHandlesRequired() {
			t.Fatalf("symlink replacement landed on inode %d, not the original %d: %s", inodeOf(t, entry), original, reuse.Miss)
		}
		t.Skipf("filesystem did not hand inode %d back: %s", original, reuse.Miss)
	}
	if got := inodeOf(t, entry); got != original {
		t.Fatalf("reuse reported but the replacement sits on inode %d, original %d", got, original)
	}
}
