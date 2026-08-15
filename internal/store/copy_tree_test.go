package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/skill"
)

// zeros is a 64-hex zero string usable as a syntactically valid digest.
const zeros = "0000000000000000000000000000000000000000000000000000000000000000"

func TestReserveStagedRootOwnedCleansPrivateRootAfterSnapshotFailure(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	want := errors.New("injected after mkdir")
	_, err = session.Store.reserveStagedRootOwnedWithHooks(0o755, stagedRootReservationHooks{
		afterMkdir: func(string) error { return want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("reserve error = %v, want injected failure", err)
	}
	entries, err := os.ReadDir(s.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stagedRootPrefix) {
			t.Fatalf("failed reservation leaked private staging root %q", entry.Name())
		}
	}
}

func TestCopyStagedTreeOwnedRefusesGitSymlink(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	base, err := session.Store.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	if err := os.Symlink("elsewhere", filepath.Join(sourceDir, ".git")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := session.Store.CopyStagedTreeOwned("alpha", base, root, ".", nil); err == nil || !strings.Contains(err.Error(), ".git symlink") {
		t.Fatalf(".git symlink must be refused, got %v", err)
	}
}

func TestDeclaredFromOwnedTreeStripsSpecialModeBits(t *testing.T) {
	tree := OwnedTree{Entries: []OwnedTreeEntry{
		{Path: "dir", Kind: ownedDirectory, Mode: uint32(os.ModeSetgid | os.ModeSticky | 0o751)},
		{Path: "file", Kind: ownedFile, Mode: uint32(os.ModeSetuid | 0o640), Digest: "sha256:" + zeros},
	}}

	declared := declaredFromOwnedTree(tree)
	if len(declared) != 2 {
		t.Fatalf("declarations = %+v, want two entries", declared)
	}
	if got := os.FileMode(declared[0].Mode); got != os.ModeDir|0o751 {
		t.Fatalf("directory declaration mode = %v, want directory type plus permission bits", got)
	}
	if got := os.FileMode(declared[1].Mode); got != 0o640 {
		t.Fatalf("file declaration mode = %v, want permission bits only", got)
	}
	for _, entry := range declared {
		if err := entry.Validate(); err != nil {
			t.Fatalf("declaration %+v is invalid: %v", entry, err)
		}
	}
}

func TestPublishStagedReservationRejectsPrivateRootReplacement(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	reservation, err := session.Store.ReserveStagedRootOwned(0o755)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(s.StagingDir(), reservation.Name)
	if err := os.Rename(private, private+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Lstat(private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Store.PublishStagedRootOwned(reservation, "alpha"); !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("private-root replacement must conflict, got %v", err)
	}
	current, err := os.Lstat(private)
	if err != nil || !os.SameFile(current, replacement) {
		t.Fatalf("replacement private root must survive: %v, %v", current, err)
	}
}

func TestCreateStagedRootOwnedReservesFuNamespace(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	if _, err := session.Store.CreateStagedRootOwned(".fu-new-attacker", 0o755); err == nil {
		t.Fatal("a public staged root must not enter fu's private namespace")
	}
	entries, err := os.ReadDir(session.Store.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refusing a reserved public name must leave no private residue: %v", entries)
	}
}

func TestMkdirDeclaredRejectsExistingEntryWithoutChangingIt(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		external := t.TempDir()
		if err := os.Chmod(external, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(root, "sub")); err != nil {
			t.Fatal(err)
		}
		rootDir, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rootDir.Close()

		err = mkdirDeclared(rootDir, "sub", NewDeclaredDir("sub", 0o755))
		if err == nil {
			t.Fatal("existing symlink must be rejected")
		}
		info, err := os.Stat(external)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("external directory mode = %o, want 700", got)
		}
	})

	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		existing := filepath.Join(root, "sub")
		if err := os.Mkdir(existing, 0o700); err != nil {
			t.Fatal(err)
		}
		rootDir, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rootDir.Close()

		err = mkdirDeclared(rootDir, "sub", NewDeclaredDir("sub", 0o755))
		if err == nil {
			t.Fatal("existing directory must be rejected")
		}
		info, err := os.Stat(existing)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("existing directory mode = %o, want 700", got)
		}
	})
}

// TestDeclaredEntryNestedAndSymlink pins the widened declaration grammar:
// nested relative paths and symlink declarations are valid, old shapes
// still validate, and malformed ones are refused.
func TestDeclaredEntryNestedAndSymlink(t *testing.T) {
	good := []DeclaredEntry{
		NewDeclaredFile("SKILL.md", 0o644, []byte("x")),
		NewDeclaredFile("lib/util.go", 0o644, []byte("y")),
		NewDeclaredSymlink("lib/link", "util.go"),
		NewDeclaredSymlink("up", "../x"),
	}
	for _, e := range good {
		if err := e.Validate(); err != nil {
			t.Errorf("valid entry %+v rejected: %v", e, err)
		}
	}
	bad := []DeclaredEntry{
		{Path: "../escape", Kind: declaredFile, Mode: 0o644, Digest: "sha256:" + zeros},
		{Path: "/abs", Kind: declaredFile, Mode: 0o644, Digest: "sha256:" + zeros},
		{Path: "a/../../escape", Kind: declaredFile, Mode: 0o644, Digest: "sha256:" + zeros},
		{Path: "a//b", Kind: declaredFile, Mode: 0o644, Digest: "sha256:" + zeros},
		{Path: "x", Kind: declaredFile, Mode: 0o644},                                             // no digest
		{Path: "x", Kind: declaredFile, Mode: uint32(os.ModeSymlink), Digest: "sha256:" + zeros}, // wrong mode for a file
		{Path: "x", Kind: declaredSymlink},                                                       // no target
		{Path: "x", Kind: declaredSymlink, Target: "/abs"},                                       // absolute symlink target: content-safety checked later, shape only here
	}
	for _, e := range bad {
		if err := e.Validate(); err == nil {
			t.Errorf("invalid entry %+v accepted", e)
		}
	}
}

// TestSettleDeclaredMissingParentsDropsEntries pins the round-4 finding
// that a transaction dying right after the declaration revision (nothing
// created) must settle cleanly even for nested entries: a declared entry
// whose *parent* was never created is absent for the same reason as the
// leaf itself, and refusing with ErrOwnedTreeChanged would block every
// later write command.
func TestSettleDeclaredMissingParentsDropsEntries(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	declared := []DeclaredEntry{
		NewDeclaredDir("sub", 0o755),
		NewDeclaredFile("sub/file.md", 0o644, []byte("x")),
		NewDeclaredFile("other.txt", 0o644, []byte("y")),
	}
	settled, err := checked.SettleDeclaredStagedEntries("alpha", base, declared)
	if err != nil {
		t.Fatalf("settle with missing parents must drop, not refuse: %v", err)
	}
	if len(settled.Entries) != 0 {
		t.Fatalf("nothing was created, so nothing may be settled: %+v", settled.Entries)
	}
}

// TestSettleDeclaredNestedEntries pins that SettleDeclaredStagedEntries
// resolves nested file and symlink declarations against the staged tree,
// drops absent ones, and refuses mismatched content or kinds.
func TestSettleDeclaredNestedEntries(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	stagingRoot, err := checked.StagingRoot()
	if err != nil {
		t.Fatal(err)
	}
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if err := stagingRoot.Mkdir(filepath.Join("alpha", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stagingRoot.WriteFile(filepath.Join("alpha", "lib", "util.go"), []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("util.go", filepath.Join(checked.StagingDir(), "alpha", "lib", "link")); err != nil {
		t.Fatal(err)
	}
	declared := []DeclaredEntry{
		NewDeclaredDir("lib", 0o755),
		NewDeclaredFile("lib/util.go", 0o644, []byte("package lib\n")),
		NewDeclaredSymlink("lib/link", "util.go"),
		NewDeclaredFile("absent.txt", 0o644, []byte("never created")),
		NewDeclaredFile("mismatch.txt", 0o644, []byte("expected")),
	}
	settled, err := checked.SettleDeclaredStagedEntries("alpha", base, declared)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	byPath := map[string]OwnedTreeEntry{}
	for _, e := range settled.Entries {
		byPath[e.Path] = e
	}
	if _, ok := byPath["lib/util.go"]; !ok {
		t.Fatal("settled manifest missing lib/util.go")
	}
	if e := byPath["lib/link"]; e.Kind != ownedSymlink || e.Target != "util.go" {
		t.Fatalf("lib/link settled as %+v", e)
	}
	if _, ok := byPath["absent.txt"]; ok {
		t.Fatal("absent declared entry must be dropped")
	}
	if _, ok := byPath["mismatch.txt"]; ok {
		t.Fatal("mismatch.txt was never created; it must not appear")
	}
}

// TestCopyStagedTreeOwned pins the full copy primitive: a nested source tree
// lands in staging byte-identical (files, modes, symlinks), .git is
// excluded, and the returned manifest validates against the live staged
// tree.
func TestCopyStagedTreeOwned(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"),
		[]byte("---\nname: pdf-tools\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("scripts", filepath.Join(src, "scripts-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	manifest, err := checked.CopyStagedTreeOwned("alpha", base, srcRoot, ".", copyDeclarations(t, src))
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := checked.ValidateStagedOwned("alpha", manifest); err != nil {
		t.Fatalf("copied tree does not match its own manifest: %v", err)
	}
	stagedDir := filepath.Join(checked.StagingDir(), "alpha")
	if _, err := os.Stat(filepath.Join(stagedDir, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git must not be copied")
	}
	if got, err := os.ReadFile(filepath.Join(stagedDir, "scripts", "run.sh")); err != nil || string(got) != "#!/bin/sh\n" {
		t.Fatalf("copied file content = %q, err %v", got, err)
	}
	info, err := os.Lstat(filepath.Join(stagedDir, "scripts", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("copied executable bit lost: %v", info.Mode())
	}
	linkTarget, err := os.Readlink(filepath.Join(stagedDir, "scripts-link"))
	if err != nil || linkTarget != "scripts" {
		t.Fatalf("symlink copied as %q, err %v", linkTarget, err)
	}
}

// TestCopyStagedTreeOwnedRejectsSourceMismatch pins that a source entry that
// does not match its declaration (or an undeclared entry) fails the copy.
func TestCopyStagedTreeOwnedRejectsSourceMismatch(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	// SKILL.md declared with the wrong digest: the copy must refuse.
	wrong := []DeclaredEntry{NewDeclaredFile("SKILL.md", 0o644, []byte("y"))}
	if _, err := checked.CopyStagedTreeOwned("alpha", base, srcRoot, ".", wrong); err == nil {
		t.Fatal("a source file that does not match its declaration must be refused")
	}
	// Empty declaration while the source holds content: refused too.
	if _, err := checked.CopyStagedTreeOwned("alpha", base, srcRoot, ".", nil); !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("an undeclared source entry must be refused as an ownership change, got %v", err)
	}
}

func TestCopyStagedTreeOwnedRejectsContentSubstitutedBeforeSnapshot(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a-victim.txt"), []byte("declared"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	stagedVictim := filepath.Join(checked.StagingDir(), "alpha", "a-victim.txt")
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	if err := validateOwnedTreeAt(checked.writeRoots.staging, "alpha", base); err != nil {
		t.Fatal(err)
	}
	_, copyErr := copyTreeOwnedWithHooks(
		checked.writeRoots.staging, "alpha", base, srcRoot, ".", copyDeclarations(t, src), false,
		copyTreeHooks{beforeSnapshot: func() error {
			return os.WriteFile(stagedVictim, []byte("foreign"), 0o644)
		}},
	)
	if copyErr == nil || !errors.Is(copyErr, ErrOwnedTreeChanged) {
		t.Fatalf("content substituted before snapshot must be refused as an ownership change, got %v", copyErr)
	}
}

func TestCopyTreeOwnedRejectsDestinationRootReplacement(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	staged := filepath.Join(s.StagingDir(), "alpha")
	parked := staged + ".parked"

	_, err = copyTreeOwnedWithHooks(
		checked.writeRoots.staging, "alpha", base, srcRoot, ".", copyDeclarations(t, src), false,
		copyTreeHooks{beforeSnapshot: func() error {
			if err := os.Rename(staged, parked); err != nil {
				return err
			}
			if err := os.Mkdir(staged, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(staged, "SKILL.md"), []byte("owned"), 0o644)
		}},
	)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("destination-root replacement must be rejected, got %v", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(staged, "SKILL.md")); readErr != nil || string(got) != "owned" {
		t.Fatalf("replacement destination changed: %q, %v", got, readErr)
	}
}

func TestCopyTreeOwnedBindsDestinationBeforeCopy(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	staged := filepath.Join(s.StagingDir(), "alpha")
	parked := staged + ".parked"
	_, err = copyTreeOwnedWithHooks(
		checked.writeRoots.staging, "alpha", base, srcRoot, ".", copyDeclarations(t, src), false,
		copyTreeHooks{beforeOpen: func() error {
			if err := os.Rename(staged, parked); err != nil {
				return err
			}
			if err := os.Mkdir(staged, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(staged, "foreign.txt"), []byte("foreign"), 0o644)
		}},
	)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("pre-copy destination replacement must be rejected, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(staged, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("copy wrote into the substituted destination: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(staged, "foreign.txt")); err != nil || string(got) != "foreign" {
		t.Fatalf("substituted destination changed: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(parked); err != nil || len(entries) != 0 {
		t.Fatalf("parked owned destination changed before binding: %v, %v", entries, err)
	}
}

func TestCreateRecoveryRootOwnedRejectsReplacementBeforeChmod(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	payload := "adopt-archive-alpha"
	recoveryPath := filepath.Join(s.RecoveryDir(), payload)
	parked := recoveryPath + ".parked"
	_, err = session.Store.createRecoveryRootOwnedWithHooks(payload, 0o711, createRecoveryRootHooks{
		afterOpen: func() error {
			if err := os.Rename(recoveryPath, parked); err != nil {
				return err
			}
			return os.Mkdir(recoveryPath, 0o700)
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("replacement recovery root must be rejected, got %v", err)
	}
	if !strings.Contains(err.Error(), "was replaced while applying its mode") {
		t.Fatalf("replacement must be caught by the post-chmod binding guard, got %v", err)
	}
	info, statErr := os.Stat(recoveryPath)
	if statErr != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("replacement recovery root was modified: mode=%v err=%v", info, statErr)
	}
}

func TestOwnedPublishSurfacesReserveFuNamespace(t *testing.T) {
	t.Run("publish staged", func(t *testing.T) {
		s, err := Init(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		name := ".fu-attacker"
		if err := os.Mkdir(filepath.Join(s.StagingDir(), name), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest, err := session.Store.SnapshotStagedPayload(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Store.PublishStagedOwned(name, manifest); err == nil {
			t.Fatal("publish must reject a reserved public name")
		}
	})

	t.Run("quarantine staged source", func(t *testing.T) {
		s, err := Init(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		name := ".fu-attacker"
		if err := os.Mkdir(filepath.Join(s.StagingDir(), name), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest, err := session.Store.SnapshotStagedPayload(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Store.QuarantineStagedOwned(name, "rollback-alpha", manifest); err == nil {
			t.Fatal("staged quarantine must reject a reserved source name")
		}
		if _, err := os.Stat(filepath.Join(s.StagingDir(), name)); err != nil {
			t.Fatalf("reserved staged source moved despite rejection: %v", err)
		}
	})

	t.Run("quarantine staged payload", func(t *testing.T) {
		s, err := Init(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if err := os.Mkdir(filepath.Join(s.StagingDir(), "alpha"), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest, err := session.Store.SnapshotStagedPayload("alpha")
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Store.QuarantineStagedOwned("alpha", ".fu-attacker", manifest); err == nil {
			t.Fatal("staged quarantine must reject a reserved payload name")
		}
		if _, err := os.Stat(filepath.Join(s.StagingDir(), "alpha")); err != nil {
			t.Fatalf("staged source moved despite rejection: %v", err)
		}
	})

	t.Run("quarantine skill", func(t *testing.T) {
		s, err := Init(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		name := ".fu-attacker"
		if err := os.Mkdir(filepath.Join(s.SkillsDir(), name), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest, err := session.Store.SnapshotSkillPayload(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Store.QuarantineSkillOwned(name, "removed-attacker", manifest); err == nil {
			t.Fatal("quarantine must reject a reserved public name")
		}
	})

	t.Run("quarantine skill payload", func(t *testing.T) {
		s, err := Init(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if err := os.Mkdir(filepath.Join(s.SkillsDir(), "alpha"), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest, err := session.Store.SnapshotSkillPayload("alpha")
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Store.QuarantineSkillOwned("alpha", ".fu-attacker", manifest); err == nil {
			t.Fatal("skill quarantine must reject a reserved payload name")
		}
		if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
			t.Fatalf("skill moved despite rejection: %v", err)
		}
	})

	t.Run("restore destination", func(t *testing.T) {
		checked, manifest, payload := ownedRecoveryFixture(t, true)
		if err := checked.RestoreRecoveryPayloadToSkills(payload, ".fu-attacker", manifest); err == nil {
			t.Fatal("restore must reject a reserved live destination")
		}
		if err := checked.ValidateRecoveryPayloadOwned(payload, manifest); err != nil {
			t.Fatalf("restore rejection moved the recovery payload: %v", err)
		}
	})

	t.Run("create recovery", func(t *testing.T) {
		s, err := Init(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if _, err := session.Store.CreateRecoveryRootOwned(".fu-attacker", 0o755); err == nil {
			t.Fatal("recovery root must reject a reserved public name")
		}
	})
}

// TestCopyStagedTreeOwnedRejectsStrayAndVanishedEntries pins the
// bidirectional snapshot-vs-declared verification (finding I4): an entry
// that appears in the staged root while the copy runs (a same-user racer,
// otherwise captured by the snapshot and published as fu's content) and a
// declared source entry that vanishes before the copy (otherwise silently
// missing from the skill) must both be refused.
func TestCopyStagedTreeOwnedRejectsStrayAndVanishedEntries(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "helper.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	declared := copyDeclarations(t, src)
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	checked := session.Store
	stagingRoot, err := checked.StagingRoot()
	if err != nil {
		t.Fatal(err)
	}

	// Direction one: a stray lands in the staged root after its exclusive
	// creation but before the copy. The snapshot would otherwise record it
	// and the whole pipeline would publish it as fu's content.
	base, err := checked.CreateStagedRootOwned("alpha", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if err := stagingRoot.WriteFile(filepath.Join("alpha", "stray.txt"), []byte("racer"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checked.CopyStagedTreeOwned("alpha", base, srcRoot, ".", declared); !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("a stray entry in the staged root must be refused as an ownership change, got %v", err)
	}
	srcRoot.Close()

	// Direction two: a declared source entry disappears after the projection.
	// The copy would otherwise finish with the skill silently missing it.
	if err := os.Remove(filepath.Join(src, "helper.txt")); err != nil {
		t.Fatal(err)
	}
	base, err = checked.CreateStagedRootOwned("beta", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot, err = os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	if _, err := checked.CopyStagedTreeOwned("beta", base, srcRoot, ".", declared); !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("a declared source entry that vanished mid-copy must be refused as an ownership change, got %v", err)
	}
}

// copyDeclarations projects a source tree into declarations the way the
// engine will (skill.ProjectDir -> DeclaredEntry), minus .git which the
// copy excludes on its own.
func copyDeclarations(t *testing.T, src string) []DeclaredEntry {
	t.Helper()
	entries, err := skill.ProjectDir(os.DirFS(src), ".")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]DeclaredEntry, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Mode&os.ModeDir != 0:
			out = append(out, NewDeclaredDir(e.Path, e.Mode.Perm()))
		case e.Mode&os.ModeSymlink != 0:
			out = append(out, NewDeclaredSymlink(e.Path, e.Target))
		default:
			out = append(out, DeclaredEntry{Path: e.Path, Kind: declaredFile, Mode: uint32(e.Mode.Perm()), Digest: e.Digest})
		}
	}
	return out
}

// TestSnapshotSkillPayloadAndRestore pins the two rm-side primitives: a live
// skill can be snapshotted, quarantined, and moved back out of recovery.
func TestSnapshotSkillPayloadAndRestore(t *testing.T) {
	checked, manifest, _ := ownedRecoveryFixture(t, true)
	// ownedRecoveryFixture already quarantined staging/"alpha" into
	// recovery/"owned-payload"; restore it back into the skills root.
	const payload = "owned-payload"
	if err := checked.RestoreRecoveryPayloadToSkills(payload, "alpha", manifest); err != nil {
		t.Fatalf("restore: %v", err)
	}
	skillsRoot, err := checked.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	info, err := skillsRoot.Lstat("alpha")
	if err != nil || !info.IsDir() {
		t.Fatalf("skill not restored to skills root: %v %v", info, err)
	}
	snapshot, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := compareOwnedTreeExact(snapshot, manifest); err != nil {
		t.Fatalf("restored skill differs from its manifest: %v", err)
	}
}
