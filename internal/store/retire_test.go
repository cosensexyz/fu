package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRetirementPrimitivesDirectly(t *testing.T) {
	t.Run("retire and restore name", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("owned"), 0o644); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		retired, err := RetireNameAt(parent, "entry", ".retired-")
		if err != nil {
			t.Fatal(err)
		}
		if err := RestoreRetiredAt(parent, retired, "entry"); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "entry")); err != nil || string(got) != "owned" {
			t.Fatalf("restored entry = %q, %v", got, err)
		}
	})

	t.Run("remove owned contents", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("owned"), 0o644); err != nil {
			t.Fatal(err)
		}
		opened, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		manifest, err := snapshotOwnedOpenDirectory(opened)
		if err != nil {
			t.Fatal(err)
		}
		if err := RemoveOwnedContents(opened, manifest); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("directory after cleanup = %v, %v", entries, err)
		}
	})

	t.Run("remove owned symlink", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		var stat unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), "link", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatal(err)
		}
		mode, _, err := modeAndKind(&stat)
		if err != nil {
			t.Fatal(err)
		}
		if err := RemoveOwnedSymlinkAt(parent, "link", identityFromStat(&stat), uint32(mode), "target"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "link")); !os.IsNotExist(err) {
			t.Fatalf("owned symlink remains: %v", err)
		}
	})
}

func TestRemoveOwnedTreeAtValidatesWholeTreeBeforeDeleting(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(filepath.Join(entry, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"SKILL.md": "owned", "sub/owned.txt": "nested"} {
		if err := os.WriteFile(filepath.Join(entry, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, ".DS_Store"), []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = RemoveOwnedTreeAt(parent, "alpha", manifest)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("stray entry must be rejected as an ownership change, got %v", err)
	}
	for name, want := range map[string]string{"SKILL.md": "owned", "sub/owned.txt": "nested", ".DS_Store": "foreign"} {
		got, readErr := os.ReadFile(filepath.Join(entry, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("preflight conflict changed %s: got %q err=%v", name, got, readErr)
		}
	}
}

func TestRetirementRenameConflictIsOwnedTreeChange(t *testing.T) {
	err := retirementRenameError("alpha", ".fu-retired-dir-deadbeef", unix.EEXIST)
	if !errors.Is(err, ErrOwnedTreeChanged) || !errors.Is(err, unix.EEXIST) {
		t.Fatalf("retirement EEXIST classification = %v", err)
	}
	for _, path := range []string{"alpha", ".fu-retired-dir-deadbeef"} {
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("retirement conflict %q lacks path %q", err, path)
		}
	}
}

func TestWriteFileAtomicNoReplaceRejectsTemporaryReplacementBeforeRename(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	const foreign = "foreign temporary replacement"
	var tempName string
	err = writeFileAtomicNoReplaceRoot(root, "record", []byte("owned record"), 0o644, atomicWriteHooks{
		beforeRename: func(name string) error {
			tempName = name
			if err := root.Remove(name); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, name), []byte(foreign), 0o600)
		},
	})
	if err == nil {
		t.Fatal("temporary replacement must be rejected")
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "record")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a replaced temporary must not remain installed as the record: %v", statErr)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, tempName))
	if readErr != nil {
		t.Fatalf("foreign replacement must be restored and preserved: %v", readErr)
	}
	if string(got) != foreign {
		t.Fatalf("foreign replacement changed: %q", got)
	}
}

func TestWriteFileAtomicNoReplaceDoesNotCleanReoccupiedTemporaryAfterRename(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	const foreign = "new occupant"
	var tempName string
	err = writeFileAtomicNoReplaceRoot(root, "record", []byte("owned record"), 0o644, atomicWriteHooks{
		afterRename: func(name string) error {
			tempName = name
			return os.WriteFile(filepath.Join(dir, name), []byte(foreign), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, "record"))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "owned record" {
		t.Fatalf("installed record = %q", written)
	}
	replacement, err := os.ReadFile(filepath.Join(dir, tempName))
	if err != nil {
		t.Fatalf("reoccupied temporary name must survive successful publication: %v", err)
	}
	if string(replacement) != foreign {
		t.Fatalf("reoccupied temporary changed: %q", replacement)
	}
}

func TestRetireOwnedDirectoryRestoresOriginalWhenRemovalFails(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "foreign"), []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	root := &checkedRoot{dir: parent, display: dir}
	manifest, err := snapshotOwnedTree(root, "alpha")
	if err != nil {
		t.Fatal(err)
	}

	err = retireOwnedDirectoryAt(parent, "alpha", ".fu-retired-dir-", manifest.RootIdentity, manifest.RootMode)
	if err == nil {
		t.Fatal("non-empty retired directory must fail removal")
	}
	if got, readErr := os.ReadFile(filepath.Join(entry, "foreign")); readErr != nil || string(got) != "preserve" {
		t.Fatalf("failed removal must restore the original name and content: %q, %v", got, readErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, ".fu-retired-dir-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("failed removal stranded retired names: %v", matches)
	}
}

func TestRemoveOwnedTreeAtDoesNotRestoreResumedRetiredDirectoryWhenRemovalFails(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	retiredName := ownedCleanupRetiredName(".fu-retired-dir-", "alpha", manifest.RootIdentity)
	retired := filepath.Join(dir, retiredName)
	if err := os.Rename(entry, retired); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "foreign"), []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = RemoveOwnedTreeAt(parent, "alpha", manifest)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("resumed non-empty retirement must be an ownership conflict, got %v", err)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Fatalf("resume must not republish the retired directory at the live name: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(retired, "foreign")); err != nil || string(got) != "preserve" {
		t.Fatalf("retired foreign content changed: %q, %v", got, err)
	}
}

func TestRemoveOwnedTreeAtDualNameConflictNamesBothEntries(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	retired := ownedCleanupRetiredName(".fu-retired-dir-", "alpha", manifest.RootIdentity)
	if err := os.Mkdir(filepath.Join(dir, retired), 0o755); err != nil {
		t.Fatal(err)
	}

	err = RemoveOwnedTreeAt(parent, "alpha", manifest)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("dual names must conflict, got %v", err)
	}
	for _, want := range []string{filepath.Join(dir, "alpha"), filepath.Join(dir, retired)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("dual-name conflict %q lacks %q", err, want)
		}
	}
}

func TestOwnedCleanupDualNameErrorsNameBothAbsolutePaths(t *testing.T) {
	t.Run("nested directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "alpha")
		if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(filepath.Dir(root))
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: filepath.Dir(root)}, "alpha")
		_ = parent.Close()
		if err != nil {
			t.Fatal(err)
		}
		var sub OwnedTreeEntry
		for _, entry := range manifest.Entries {
			if entry.Path == "sub" {
				sub = entry
			}
		}
		retired := ownedCleanupRetiredName(".fu-retired-dir-", sub.Path, sub.Identity)
		if err := os.Mkdir(filepath.Join(root, retired), 0o755); err != nil {
			t.Fatal(err)
		}
		opened, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		err = removeOwnedDirectoryContents(opened, "", manifest)
		if !errors.Is(err, ErrOwnedTreeChanged) {
			t.Fatalf("nested dual names must conflict: %v", err)
		}
		for _, want := range []string{filepath.Join(root, "sub"), filepath.Join(root, retired)} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("nested dual-name conflict %q lacks %q", err, want)
			}
		}
	})

	t.Run("leaf", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "leaf"), []byte("owned"), 0o644); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		expected, err := snapshotOwnedLeafAt(int(parent.Fd()), "leaf", "leaf")
		if err != nil {
			t.Fatal(err)
		}
		retired := ownedCleanupRetiredName(".fu-retired-entry-", expected.Path, expected.Identity)
		if err := os.WriteFile(filepath.Join(dir, retired), []byte("foreign"), 0o644); err != nil {
			t.Fatal(err)
		}
		err = retireOwnedEntryAt(parent, "leaf", ".fu-retired-entry-", expected)
		if !errors.Is(err, ErrOwnedTreeChanged) {
			t.Fatalf("leaf dual names must conflict: %v", err)
		}
		for _, want := range []string{filepath.Join(dir, "leaf"), filepath.Join(dir, retired)} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("leaf dual-name conflict %q lacks %q", err, want)
			}
		}
	})

	t.Run("directory retirement", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "child"), 0o755); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "child")
		if err != nil {
			t.Fatal(err)
		}
		retired := ownedCleanupRetiredName(".fu-retired-dir-", "child", manifest.RootIdentity)
		if err := os.Mkdir(filepath.Join(dir, retired), 0o755); err != nil {
			t.Fatal(err)
		}
		err = retireOwnedDirectoryAtPath(parent, "child", "child", ".fu-retired-dir-", manifest.RootIdentity, manifest.RootMode)
		if !errors.Is(err, ErrOwnedTreeChanged) {
			t.Fatalf("directory dual names must conflict: %v", err)
		}
		for _, want := range []string{filepath.Join(dir, "child"), filepath.Join(dir, retired)} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("directory dual-name conflict %q lacks %q", err, want)
			}
		}
	})
}

func TestRemoveOwnedTreeAtNestedDualNameConflictNamesBothPaths(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var sub OwnedTreeEntry
	for _, entry := range manifest.Entries {
		if entry.Path == "sub" {
			sub = entry
		}
	}
	retired := ownedCleanupRetiredName(".fu-retired-dir-", sub.Path, sub.Identity)
	if err := os.Mkdir(filepath.Join(root, retired), 0o755); err != nil {
		t.Fatal(err)
	}
	err = RemoveOwnedTreeAt(parent, "alpha", manifest)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("nested dual names must conflict: %v", err)
	}
	for _, want := range []string{filepath.Join(root, "sub"), filepath.Join(root, retired)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("preflight dual-name conflict %q lacks %q", err, want)
		}
	}
}

func TestRemoveOwnedTreeAtResumesRetiredLeaf(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "SKILL.md"), []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	leaf := manifest.Entries[0]
	retired := ownedCleanupRetiredName(".fu-retired-entry-", leaf.Path, leaf.Identity)
	if err := os.Rename(filepath.Join(entry, "SKILL.md"), filepath.Join(entry, retired)); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwnedTreeAt(parent, "alpha", manifest); err != nil {
		t.Fatalf("resume owned-tree removal: %v", err)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Fatalf("owned tree still exists: %v", err)
	}
}

func TestRemoveOwnedTreeAtResumesRetiredNestedDirectory(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	deep := filepath.Join(entry, "sub", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "owned.txt"), []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var deepEntry OwnedTreeEntry
	for _, candidate := range manifest.Entries {
		if candidate.Path == "sub/deep" {
			deepEntry = candidate
			break
		}
	}
	if deepEntry.Path == "" {
		t.Fatal("nested directory missing from ownership manifest")
	}
	if err := os.Remove(filepath.Join(deep, "owned.txt")); err != nil {
		t.Fatal(err)
	}
	retired := ownedCleanupRetiredName(".fu-retired-dir-", deepEntry.Path, deepEntry.Identity)
	if err := os.Rename(deep, filepath.Join(filepath.Dir(deep), retired)); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwnedTreeAt(parent, "alpha", manifest); err != nil {
		t.Fatalf("resume nested owned-directory removal: %v", err)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Fatalf("owned tree still exists: %v", err)
	}
}

func TestRemoveOwnedTreeAtDoesNotAcceptLegacyBasenameRetirement(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	deep := filepath.Join(entry, "sub", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "owned.txt"), []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var deepEntry OwnedTreeEntry
	for _, candidate := range manifest.Entries {
		if candidate.Path == "sub/deep" {
			deepEntry = candidate
			break
		}
	}
	if err := os.Remove(filepath.Join(deep, "owned.txt")); err != nil {
		t.Fatal(err)
	}
	legacyName := ownedCleanupRetiredName(".fu-retired-dir-", filepath.Base(deepEntry.Path), deepEntry.Identity)
	legacy := filepath.Join(filepath.Dir(deep), legacyName)
	if err := os.Rename(deep, legacy); err != nil {
		t.Fatal(err)
	}

	if err := RemoveOwnedTreeAt(parent, "alpha", manifest); err == nil {
		t.Fatal("an unreachable legacy basename retirement must not be adopted as owned state")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy retirement must remain untouched: %v", err)
	}
}

func TestRemoveOwnedTreeAtDoesNotRestoreForeignRetiredLeaf(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(entry, "SKILL.md")
	if err := os.WriteFile(live, []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	leaf := manifest.Entries[0]
	retired := filepath.Join(entry, ownedCleanupRetiredName(".fu-retired-entry-", leaf.Path, leaf.Identity))
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retired, []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwnedTreeAt(parent, "alpha", manifest); !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("foreign retired leaf must be rejected, got %v", err)
	}
	if _, err := os.Lstat(live); !os.IsNotExist(err) {
		t.Fatalf("foreign retired leaf must not be published at the live name: %v", err)
	}
	if got, err := os.ReadFile(retired); err != nil || string(got) != "foreign" {
		t.Fatalf("foreign retired leaf changed: %q, %v", got, err)
	}
}

func TestRemoveOwnedTreeAtDoesNotRestoreForeignRetiredDirectory(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "alpha")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "owned.txt"), []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	manifest, err := snapshotOwnedTree(&checkedRoot{dir: parent, display: dir}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(dir, ownedCleanupRetiredName(".fu-retired-dir-", "alpha", manifest.RootIdentity))
	if err := os.RemoveAll(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(retired, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "foreign.txt"), []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwnedTreeAt(parent, "alpha", manifest); !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("foreign retired directory must be rejected, got %v", err)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Fatalf("foreign retired directory must not be published at the live name: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(retired, "foreign.txt")); err != nil || string(got) != "foreign" {
		t.Fatalf("foreign retired directory changed: %q, %v", got, err)
	}
}

func TestWriteFileAtomicNoReplaceCloseFailurePrecedesPublication(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	closeErr := errors.New("simulated close failure")

	err = writeFileAtomicNoReplaceRoot(root, "record", []byte("owned record"), 0o644, atomicWriteHooks{
		closeTemp: func(file *os.File) error {
			_ = file.Close()
			return closeErr
		},
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("write error = %v, want close failure", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "record")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a close failure must happen before publication, record err=%v", statErr)
	}
}
