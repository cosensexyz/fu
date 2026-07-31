// internal/store/rootfs_test.go
package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A writable open must never land on a symlink's target, even when that target
// stays inside the pinned root. os.Root guarantees containment, not no-follow:
// it discovers a final symlink with O_NOFOLLOW and then deliberately resolves
// it unless the call is O_CREATE|O_EXCL. go-git reaches this path for every
// control file it rewrites -- Create("index") and the loose-reference update --
// so a direct Git writer racing fu could redirect either write onto a different
// file inside .git that fu had already read and validated.
func TestRootFilesystemRefusesToWriteThroughAFinalSymlink(t *testing.T) {
	tests := []struct {
		name string
		flag int
		perm os.FileMode
	}{
		{name: "index rewrite", flag: os.O_RDWR | os.O_CREATE | os.O_TRUNC, perm: 0o666},
		{name: "loose reference update", flag: os.O_RDWR | os.O_CREATE, perm: 0o666},
		{name: "append", flag: os.O_WRONLY | os.O_APPEND, perm: 0},
		{name: "truncate in place", flag: os.O_WRONLY | os.O_TRUNC, perm: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(dir, "sub", "victim")
			want := []byte("content fu already read and validated")
			if err := os.WriteFile(victim, want, 0o644); err != nil {
				t.Fatal(err)
			}
			// A relative target that stays inside the pinned root, which is
			// exactly the case os.Root permits.
			if err := os.Symlink(filepath.Join("sub", "victim"), filepath.Join(dir, "control")); err != nil {
				t.Fatal(err)
			}
			root, err := openPinnedTop(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.close()
			fsys, err := newRootFilesystem(root, ".")
			if err != nil {
				t.Fatal(err)
			}

			file, err := fsys.OpenFile("control", tt.flag, tt.perm)
			if err == nil {
				_, _ = file.Write([]byte("overwritten by fu"))
				_ = file.Close()
				t.Errorf("writable open of a symlinked control file must fail, got a usable file")
			}
			got, readErr := os.ReadFile(victim)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("symlink target changed: got %q want %q", got, want)
			}
			link, err := os.Lstat(filepath.Join(dir, "control"))
			if err != nil {
				t.Fatal(err)
			}
			if link.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("refused open must leave the symlink itself alone, got mode %v", link.Mode())
			}
		})
	}
}

// A link at any component redirects the write just as surely as a link at the
// name itself. Refusing only the basename left `refs/heads -> tags` open: the
// write to refs/heads/main landed in refs/tags/main, a different reference
// inside the same pinned root.
func TestRootFilesystemRefusesToWriteThroughASymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "refs", "tags"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "refs", "tags", "main")
	want := []byte("the tag fu already read")
	if err := os.WriteFile(victim, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tags", filepath.Join(dir, "refs", "heads")); err != nil {
		t.Fatal(err)
	}
	root, err := openPinnedTop(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	fsys, err := newRootFilesystem(root, ".")
	if err != nil {
		t.Fatal(err)
	}

	file, err := fsys.OpenFile(filepath.Join("refs", "heads", "main"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
	if err == nil {
		_, _ = file.Write([]byte("the branch fu meant to write"))
		_ = file.Close()
		t.Error("a writable open through a symlinked parent must fail, got a usable file")
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("write reached %s through its symlinked parent: got %q want %q", victim, got, want)
	}
}

// The same refusal for a special file at the target, and without blocking on
// it. A FIFO accepted here is worse than a wrong write: go-git reading the ref
// or index afterwards blocks indefinitely, and it does so while the command
// still holds fu.lock, so every later fu process waits on it too.
func TestRootFilesystemRefusesToWriteSpecialFiles(t *testing.T) {
	tests := []struct {
		name string
		flag int
	}{
		{name: "read-write create", flag: os.O_RDWR | os.O_CREATE},
		{name: "index rewrite", flag: os.O_RDWR | os.O_CREATE | os.O_TRUNC},
		// Without O_NONBLOCK this open blocks until a reader appears. The
		// timeout below turns a regression into a failure rather than a hang.
		{name: "write only", flag: os.O_WRONLY | os.O_CREATE},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			control := filepath.Join(dir, "index")
			if err := unix.Mkfifo(control, 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := openPinnedTop(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.close()
			fsys, err := newRootFilesystem(root, ".")
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				file, err := fsys.OpenFile("index", tt.flag, 0o666)
				if err == nil {
					_ = file.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Error("a writable open of a FIFO must fail, got a usable file")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("a writable open of a FIFO blocked while the write lock was held")
			}
			info, err := os.Lstat(control)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatalf("refused open must leave the FIFO alone, got mode %v", info.Mode())
			}
		})
	}
}

// The same boundary through the real commit path.
//
// A link whose target already exists is caught earlier, when go-git reads
// .git/index before rewriting it and the read path's identity check rejects
// the name. A *dangling* link is the case with no read to fall back on: the
// read reports "no index yet", go-git proceeds with an empty one, and the write
// is then the first and only operation to touch the name. Before the write path
// stopped following links, that write created the link's target and left the
// staged index in a file no Git implementation would ever look at.
func TestPrepareCommitNeverWritesThroughADanglingSymlinkedGitIndex(t *testing.T) {
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

	gitDir := filepath.Join(s.Dir(), ".git")
	index := filepath.Join(gitDir, "index")
	victim := filepath.Join(gitDir, "fu-victim")
	if err := os.Remove(index); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink("fu-victim", index); err != nil {
		t.Fatal(err)
	}

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

	if _, err := checked.PrepareCommit(); err == nil {
		t.Error("staging through a symlinked .git/index must fail")
	}
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Fatalf("staging must not create the link target %s, got err %v", victim, err)
	}
	link, err := os.Lstat(index)
	if err != nil {
		t.Fatal(err)
	}
	if link.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".git/index must be left as the link it was, got mode %v", link.Mode())
	}
}

// The parent-link case through the real reference CAS. The decoy tag holds the
// same hash as the branch, so go-git's compare-and-swap reads what it expects
// and proceeds to the write -- which is the step that must not land on it.
func TestCommitNeverWritesAReferenceThroughASymlinkedParent(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(s.Dir(), ".git")
	heads := filepath.Join(gitDir, "refs", "heads")
	branch := filepath.Base(head.Name().String())
	victim := filepath.Join(gitDir, "refs", "tags", branch)
	want := []byte(head.Hash().String() + "\n")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "tags"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(heads); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tags", heads); err != nil {
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

	if _, err := checked.Commit("new: alpha"); err == nil {
		t.Error("publishing a branch through a symlinked refs/heads must fail")
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("branch update reached the tag through refs/heads: got %q want %q", got, want)
	}
}

// The refusal must not cost the ordinary writable opens go-git depends on.
func TestRootFilesystemStillWritesOrdinaryFiles(t *testing.T) {
	dir := t.TempDir()
	root, err := openPinnedTop(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	fsys, err := newRootFilesystem(root, ".")
	if err != nil {
		t.Fatal(err)
	}

	want := []byte("ordinary content")
	file, err := fsys.Create(filepath.Join("refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("created file = %q, want %q", got, want)
	}

	// The loose-reference shape: O_RDWR|O_CREATE without O_TRUNC overwrites in
	// place and leaves whatever the new bytes do not cover, which is what
	// go-git expects when it rewrites a reference over its previous value.
	rewritten := []byte("REWRITTEN BYTES!")
	reopened, err := fsys.OpenFile(filepath.Join("refs", "heads", "main"), os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Write(rewritten); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, rewritten) {
		t.Fatalf("rewritten file = %q, want %q", got, rewritten)
	}
}

// Every adapter operation, not just the writable ones, must refuse to resolve
// a symlinked path component. os.Root keeps resolution inside the pinned root
// but follows links contained in it, so `refs/heads -> tags` silently retargets
// the whole surface: reads return a different ref's bytes, renames replace a
// different object, and the identity comparison on the read path compares the
// resolved target rather than the logical name the caller asked for.
func TestRootFilesystemRefusesEveryOperationThroughASymlinkedParent(t *testing.T) {
	const victimBytes = "the tag fu never named"
	setup := func(t *testing.T) (*checkedRoot, *rootFilesystem, string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "refs", "tags"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "refs", "tags", "main"), []byte(victimBytes), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "source"), []byte("the object fu meant to move"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("tags", filepath.Join(dir, "refs", "heads")); err != nil {
			t.Fatal(err)
		}
		root, err := openPinnedTop(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.close() })
		fsys, err := newRootFilesystem(root, ".")
		if err != nil {
			t.Fatal(err)
		}
		return root, fsys, dir
	}

	through := filepath.Join("refs", "heads", "main")
	tests := []struct {
		name string
		call func(*rootFilesystem) error
	}{
		{"open", func(f *rootFilesystem) error {
			file, err := f.Open(through)
			if err == nil {
				_ = file.Close()
			}
			return err
		}},
		{"stat", func(f *rootFilesystem) error { _, err := f.Stat(through); return err }},
		{"lstat", func(f *rootFilesystem) error { _, err := f.Lstat(through); return err }},
		{"readdir", func(f *rootFilesystem) error { _, err := f.ReadDir(filepath.Join("refs", "heads")); return err }},
		{"rename onto", func(f *rootFilesystem) error { return f.Rename("source", through) }},
		{"rename from", func(f *rootFilesystem) error { return f.Rename(through, "moved") }},
		{"remove", func(f *rootFilesystem) error { return f.Remove(through) }},
		{"mkdirall", func(f *rootFilesystem) error { return f.MkdirAll(filepath.Join("refs", "heads", "sub"), 0o755) }},
		{"symlink", func(f *rootFilesystem) error {
			return f.Symlink("elsewhere", filepath.Join("refs", "heads", "link"))
		}},
		{"readlink", func(f *rootFilesystem) error { _, err := f.Readlink(through); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fsys, dir := setup(t)
			if err := tt.call(fsys); err == nil {
				t.Errorf("%s must refuse a symlinked path component, got no error", tt.name)
			}
			victim := filepath.Join(dir, "refs", "tags", "main")
			got, err := os.ReadFile(victim)
			if err != nil {
				t.Fatalf("the object behind the link must survive: %v", err)
			}
			if string(got) != victimBytes {
				t.Fatalf("%s reached %s through its symlinked parent: got %q want %q", tt.name, victim, got, victimBytes)
			}
			// Nothing may be created beside it either.
			for _, unexpected := range []string{"sub", "link"} {
				if _, err := os.Lstat(filepath.Join(dir, "refs", "tags", unexpected)); !os.IsNotExist(err) {
					t.Fatalf("%s created refs/tags/%s through its symlinked parent", tt.name, unexpected)
				}
			}
		})
	}
}

// Ordinary nested paths must keep working through the same walk.
func TestRootFilesystemStillServesOrdinaryNestedPaths(t *testing.T) {
	dir := t.TempDir()
	root, err := openPinnedTop(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	fsys, err := newRootFilesystem(root, ".")
	if err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join("refs", "heads", "main")
	want := []byte("branch tip")
	file, err := fsys.Create(nested)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := fsys.Open(nested)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := opened.Read(got); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read back %q, want %q", got, want)
	}
	if info, err := fsys.Stat(nested); err != nil {
		t.Fatal(err)
	} else if info.Size() != int64(len(want)) {
		t.Fatalf("Stat size = %d, want %d", info.Size(), len(want))
	}
	if _, err := fsys.Lstat(nested); err != nil {
		t.Fatal(err)
	}
	if infos, err := fsys.ReadDir(filepath.Join("refs", "heads")); err != nil {
		t.Fatal(err)
	} else if len(infos) != 1 || infos[0].Name() != "main" {
		t.Fatalf("ReadDir = %+v, want one entry named main", infos)
	}
	if err := fsys.MkdirAll(filepath.Join("refs", "remotes", "origin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Symlink("main", filepath.Join("refs", "heads", "alias")); err != nil {
		t.Fatal(err)
	}
	if target, err := fsys.Readlink(filepath.Join("refs", "heads", "alias")); err != nil {
		t.Fatal(err)
	} else if target != "main" {
		t.Fatalf("Readlink = %q, want %q", target, "main")
	}
	// Stat follows a final link; Lstat reports the link itself.
	if info, err := fsys.Stat(filepath.Join("refs", "heads", "alias")); err != nil {
		t.Fatal(err)
	} else if info.Size() != int64(len(want)) {
		t.Fatalf("Stat through a final link size = %d, want %d", info.Size(), len(want))
	}
	if info, err := fsys.Lstat(filepath.Join("refs", "heads", "alias")); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat of a link reported mode %v", info.Mode())
	}
	if err := fsys.Rename(filepath.Join("refs", "heads", "main"), filepath.Join("refs", "remotes", "origin", "main")); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove(filepath.Join("refs", "heads", "alias")); err != nil {
		t.Fatal(err)
	}
}

// The object side of the same guarantee: go-git creates loose objects through
// TempFile under .git/objects, so a link at that component would divert every
// object fu writes into a directory no Git implementation reads.
//
// This one pins the end-to-end property rather than isolating the walk -- a
// link at .git/objects is over-determined, and several checks refuse it before
// TempFile is reached. The isolating evidence for each method is
// TestRootFilesystemRefusesEveryOperationThroughASymlinkedParent.
func TestCommitNeverWritesObjectsThroughASymlinkedParent(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(s.Dir(), ".git")
	decoy := filepath.Join(gitDir, "decoy-objects")
	if err := os.Rename(filepath.Join(gitDir, "objects"), decoy); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("decoy-objects", filepath.Join(gitDir, "objects")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(decoy)
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

	if _, err := checked.Commit("new: alpha"); err == nil {
		t.Error("writing objects through a symlinked .git/objects must fail")
	}
	after, err := os.ReadDir(decoy)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("objects reached %s through its symlinked parent: %d entries before, %d after", decoy, len(before), len(after))
	}
}
