package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHashFileAtRefusesFIFOReplacementWithoutBlocking(t *testing.T) {
	dirPath := t.TempDir()
	name := "entry"
	entryPath := filepath.Join(dirPath, name)
	if err := os.WriteFile(entryPath, []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	observed, err := statAt(int(dir.Fd()), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(entryPath); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(entryPath, 0o600); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, _, err := hashFileAt(int(dir.Fd()), name, identityFromStat(&observed))
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ErrOwnedTreeChanged) {
			t.Fatalf("FIFO replacement must be rejected as an ownership change, got %v", err)
		}
	case <-time.After(time.Second):
		fd, err := unix.Open(entryPath, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-result:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("owned-tree hashing blocked after a regular file was replaced by a FIFO")
	}
}

func TestArchiveRecoveryPayloadOwnedRejectsCompletelyMissingPayload(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	if err := os.RemoveAll(filepath.Join(checked.RecoveryDir(), payload)); err != nil {
		t.Fatal(err)
	}
	err := checked.ArchiveRecoveryPayloadOwned(payload, manifest)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("missing original, holding, and archive names must be an ownership conflict, got %v", err)
	}
}

func TestArchiveRecoveryPayloadOwnedRejectsUnrecognizedRename(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	original := filepath.Join(checked.RecoveryDir(), payload)
	renamed := original + "-moved-elsewhere"
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	err := checked.ArchiveRecoveryPayloadOwned(payload, manifest)
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("payload renamed outside every recognized archive location must conflict, got %v", err)
	}
	if _, err := os.Stat(renamed); err != nil {
		t.Fatalf("unrecognized renamed payload must remain untouched: %v", err)
	}
}

func TestOwnedTreeCleanupPreservesEntryReplacedAtRemovalBoundary(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	foreignBytes := []byte("foreign replacement")
	payloadDir := filepath.Join(checked.RecoveryDir(), payload)
	replaced := false

	err := archiveRecoveryPayloadOwned(checked, payload, manifest, ownedCleanupHooks{
		beforeEntryRemoval: func(entry OwnedTreeEntry) {
			if replaced || entry.Path != "SKILL.md" {
				return
			}
			replaced = true
			path := filepath.Join(payloadDir, entry.Path)
			if err := os.Rename(path, path+"-owned"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, foreignBytes, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("entry replacement at the removal boundary must conflict, got %v", err)
	}
	got, err := os.ReadFile(filepath.Join(payloadDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("foreign entry replacement must survive: %v", err)
	}
	if !bytes.Equal(got, foreignBytes) {
		t.Fatalf("foreign entry replacement changed: got %q want %q", got, foreignBytes)
	}
}

func TestOwnedTreeCleanupPreservesRootReplacedAtRemovalBoundary(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, false)
	payloadDir := filepath.Join(checked.RecoveryDir(), payload)
	replaced := false

	err := archiveRecoveryPayloadOwned(checked, payload, manifest, ownedCleanupHooks{
		beforeRootRemoval: func() {
			if replaced {
				return
			}
			replaced = true
			if err := os.Rename(payloadDir, payloadDir+"-owned"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(payloadDir, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("root replacement at the removal boundary must conflict, got %v", err)
	}
	info, err := os.Lstat(payloadDir)
	if err != nil {
		t.Fatalf("foreign root replacement must survive: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("foreign root replacement changed type: %v", info.Mode())
	}
}

func TestOwnedTreeCleanupPreservesEntryReplacedAtFinalBoundary(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	payloadDir := filepath.Join(checked.RecoveryDir(), payload)
	foreignBytes := []byte("foreign final-boundary replacement")
	replaced := false
	entryPath := filepath.Join(payloadDir, "SKILL.md")

	err := archiveRecoveryPayloadOwned(checked, payload, manifest, ownedCleanupHooks{
		beforeEntryFinalization: func(entry OwnedTreeEntry) {
			if replaced || entry.Path != "SKILL.md" {
				return
			}
			replaced = true
			if err := os.Rename(entryPath, entryPath+"-owned"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entryPath, foreignBytes, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	})
	if !replaced {
		t.Fatal("test setup error: final entry hook did not run")
	}
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("entry replacement at the final boundary must conflict, got %v", err)
	}
	got, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("foreign final-boundary entry must survive: %v", err)
	}
	if !bytes.Equal(got, foreignBytes) {
		t.Fatalf("foreign final-boundary entry changed: got %q want %q", got, foreignBytes)
	}
}

func TestOwnedTreeCleanupPreservesRootReplacedAtFinalBoundary(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, false)
	payloadDir := filepath.Join(checked.RecoveryDir(), payload)
	replaced := false

	err := archiveRecoveryPayloadOwned(checked, payload, manifest, ownedCleanupHooks{
		beforeRootFinalization: func() {
			if replaced {
				return
			}
			replaced = true
			if err := os.Rename(payloadDir, payloadDir+"-owned"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(payloadDir, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	})
	if !replaced {
		t.Fatal("test setup error: final root hook did not run")
	}
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("root replacement at the final boundary must conflict, got %v", err)
	}
	info, err := os.Lstat(payloadDir)
	if err != nil {
		t.Fatalf("foreign final-boundary root must survive: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("foreign final-boundary root changed type: %v", info.Mode())
	}
}

func TestOwnedTreeCleanupRestoresUnsupportedEntryReplacement(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	payloadDir := filepath.Join(checked.RecoveryDir(), payload)
	replaced := false

	err := archiveRecoveryPayloadOwned(checked, payload, manifest, ownedCleanupHooks{
		beforeEntryRemoval: func(entry OwnedTreeEntry) {
			if replaced || entry.Path != "SKILL.md" {
				return
			}
			replaced = true
			path := filepath.Join(payloadDir, entry.Path)
			if err := os.Rename(path, path+"-owned"); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("unsupported entry replacement at the removal boundary must conflict, got %v", err)
	}
	info, err := os.Lstat(filepath.Join(payloadDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("unsupported entry replacement must be restored: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("restored replacement type = %v, want named pipe", info.Mode())
	}
}

func TestOwnedTreeCleanupRestoresWrongTypeRootReplacement(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, false)
	payloadDir := filepath.Join(checked.RecoveryDir(), payload)
	target := t.TempDir()
	replaced := false

	err := archiveRecoveryPayloadOwned(checked, payload, manifest, ownedCleanupHooks{
		beforeRootRemoval: func() {
			if replaced {
				return
			}
			replaced = true
			if err := os.Rename(payloadDir, payloadDir+"-owned"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, payloadDir); err != nil {
				t.Fatal(err)
			}
		},
	})
	if !errors.Is(err, ErrOwnedTreeChanged) {
		t.Fatalf("wrong-type root replacement at the removal boundary must conflict, got %v", err)
	}
	got, err := os.Readlink(payloadDir)
	if err != nil {
		t.Fatalf("wrong-type root replacement must be restored: %v", err)
	}
	if got != target {
		t.Fatalf("restored root link target = %q, want %q", got, target)
	}
}

func ownedRecoveryFixture(t *testing.T, withFile bool) (*Store, OwnedTree, string) {
	t.Helper()
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	checked := session.Store
	stagingRoot, err := checked.StagingRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := stagingRoot.Mkdir("alpha", 0o755); err != nil {
		t.Fatal(err)
	}
	if withFile {
		if err := stagingRoot.WriteFile(filepath.Join("alpha", "SKILL.md"), []byte("owned"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	const payload = "owned-payload"
	if err := checked.QuarantineStagedOwned("alpha", payload, manifest); err != nil {
		t.Fatal(err)
	}
	return checked, manifest, payload
}
