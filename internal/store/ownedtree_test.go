package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestReadSourceFileRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "entry"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	result := make(chan error, 1)
	go func() {
		_, err := readSourceFile(root, "entry")
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO source must be refused by type, got %v", err)
		}
	case <-time.After(time.Second):
		fd, openErr := unix.Open(filepath.Join(dir, "entry"), unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if openErr == nil {
			_ = unix.Close(fd)
		}
		t.Fatal("readSourceFile blocked while opening a FIFO")
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

func TestReclaimRecoveryPayloadOwnedRejectsUnattachedSession(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	// Reclamation deletes content, so it must never run through anything but
	// the session's pinned recovery descriptor: an unattached store would have
	// to re-resolve $FU_HOME/recovery by pathname.
	unattached, err := Open(checked.Home)
	if err != nil {
		t.Fatal(err)
	}
	err = unattached.ReclaimRecoveryPayloadOwned(payload, manifest)
	if err == nil || !strings.Contains(err.Error(), "not attached to a checked recovery-root session") {
		t.Fatalf("reclaim outside a checked write session must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(checked.RecoveryDir(), payload)); err != nil {
		t.Fatalf("refused reclaim must leave the payload untouched: %v", err)
	}
}

func TestReclaimRecoveryPayloadOwnedRejectsReservedName(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	// The .fu- namespace holds fu's own retirement names, including the
	// archive name this payload would be retired under. Reclamation takes
	// public payload names only, so no caller can aim it at that machinery.
	reserved := ownedArchiveName(payload, manifest)
	err := checked.ReclaimRecoveryPayloadOwned(reserved, manifest)
	if err == nil || !strings.Contains(err.Error(), "public single-component name") {
		t.Fatalf("reserved payload name %q must be refused, got %v", reserved, err)
	}
	if _, err := os.Stat(filepath.Join(checked.RecoveryDir(), payload)); err != nil {
		t.Fatalf("refused reclaim must leave the payload untouched: %v", err)
	}
}

// TestRecoveryPayloadSettledRejectsReservedName is the mirror of the guard
// above, on the disposal counterpart's read-only twin.
//
// RecoveryPayloadSettled and ReclaimRecoveryPayloadOwned are reached with the
// same kind of name -- one taken straight off a completed family's Name field,
// which nothing on the prune path validates: decodeTxnFile checks op, id,
// sequence and digest, while validateRemoveTxn's skill.ValidateName runs only
// in the recovery handlers. A hand-edited journal carrying a traversal in Name
// would otherwise put this fstatat outside recovery/. Its twin has had a test
// since it was written; this one is a stat rather than a deletion, but the
// whole file rests on the discipline and the pair must not be asymmetric about
// it -- least of all in the tests, where the asymmetry is what lets one copy
// be deleted while the suite stays green.
func TestRecoveryPayloadSettledRejectsReservedName(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	reserved := ownedArchiveName(payload, manifest)
	settled, err := checked.RecoveryPayloadSettled(reserved, manifest)
	if err == nil || !strings.Contains(err.Error(), "public single-component name") {
		t.Fatalf("reserved payload name %q must be refused, got settled=%v err=%v", reserved, settled, err)
	}
	if settled {
		t.Fatal("a refused name must never be reported settled")
	}
}

// interruptReclaimAtRetiredRoot reproduces the exact durable state
// RemoveOwnedTreeAt leaves behind when the process dies between the root's
// retirement rename and the rmdir that immediately follows it: the contents are
// already gone, the live payload name is free, and the emptied root sits at the
// deterministic sibling derived from the manifest. It drives the same two steps
// RemoveOwnedTreeAt runs in that order (retire.go) rather than hand-building a
// directory, so the fixture cannot drift from the protocol it stands for.
func interruptReclaimAtRetiredRoot(t *testing.T, s *Store, payload string, manifest OwnedTree) string {
	t.Helper()
	payloadDir := filepath.Join(s.RecoveryDir(), payload)
	root, err := os.Open(payloadDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedDirectoryContents(root, "", manifest); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	retired := ownedCleanupRetiredName(".fu-retired-dir-", payload, manifest.RootIdentity)
	if err := os.Rename(payloadDir, filepath.Join(s.RecoveryDir(), retired)); err != nil {
		t.Fatal(err)
	}
	return retired
}

func TestRecoveryPayloadSettledReportsLivePayloadUnsettled(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)

	settled, err := checked.RecoveryPayloadSettled(payload, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if settled {
		t.Fatal("a payload still standing at its live name has not been disposed of")
	}
}

// TestRecoveryPayloadSettledReportsRetiredRootUnsettled pins the state an
// active-name-only check cannot see. RemoveOwnedTreeAt's retirement protocol
// spans two names, and the manifest is what lets it resume from the second one;
// answering "settled" here is what lets `fu gc` prune the journal carrying that
// manifest, after which nothing can ever collect the retired root.
func TestRecoveryPayloadSettledReportsRetiredRootUnsettled(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	retired := interruptReclaimAtRetiredRoot(t, checked, payload, manifest)

	settled, err := checked.RecoveryPayloadSettled(payload, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if settled {
		t.Fatalf("an interrupted reclaim parked at %q still needs its manifest to resume", retired)
	}
}

func TestRecoveryPayloadSettledReportsFullyReclaimedPayloadSettled(t *testing.T) {
	checked, manifest, payload := ownedRecoveryFixture(t, true)
	if err := checked.ReclaimRecoveryPayloadOwned(payload, manifest); err != nil {
		t.Fatal(err)
	}

	settled, err := checked.RecoveryPayloadSettled(payload, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !settled {
		t.Fatal("a fully reclaimed payload holds nothing under either name")
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
