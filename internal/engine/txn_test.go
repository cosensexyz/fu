// internal/engine/txn_test.go
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

func TestRecoverPendingAddsRemedyToBareConflict(t *testing.T) {
	s, _ := setupStore(t)
	record := TxnRecord{Op: "remedy-probe", Name: "alpha"}
	if err := WriteTxn(s, &record); err != nil {
		t.Fatal(err)
	}
	RegisterRecoverHandler(record.Op, func(*store.Store, TxnRecord) error {
		return fmt.Errorf("changed recovery state: %w", ErrTxnConflict)
	})
	t.Cleanup(func() { deleteRecoverHandler(record.Op) })
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	err = RecoverPending(session.Store)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("recovery error = %v, want ErrTxnConflict", err)
	}
	journalPattern := filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s-*.json", record.Op, record.TxnID))
	completionPath := filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s.done", record.Op, record.TxnID))
	prunePattern := filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s-*.pruned", record.Op, record.TxnID))
	for _, want := range []string{
		s.RecoveryDir(), journalPattern, completionPath, prunePattern,
		filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(s.StagingDir(), "alpha"),
		"preserve the complete transaction family", "move every transaction family file", "retry",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("recovery conflict %q lacks actionable detail %q", err, want)
		}
	}
}

func TestPruneCompletedTransactionsRemovesValidatedJournalFamily(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var transactionFiles int
	for _, entry := range before {
		if strings.HasPrefix(entry.Name(), "txn-") {
			transactionFiles++
		}
	}
	if transactionFiles < 2 {
		t.Fatalf("completed transaction family = %d files, want revisions plus marker", transactionFiles)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Transactions != 1 || outcome.Files != transactionFiles {
		t.Fatalf("prune outcome = %+v, want one transaction and %d files", outcome, transactionFiles)
	}
	after, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range after {
		if strings.HasPrefix(entry.Name(), "txn-") {
			t.Fatalf("successful prune left transaction artifact %s", entry.Name())
		}
	}
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatalf("pruned journal must not block later writes: %v", err)
	}
}

func TestPruneCompletedTransactionsResumesAfterInterruption(t *testing.T) {
	for _, stage := range []string{"after marker", "after first removal"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := setupStore(t)
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("stop pruning")
			h := pruneHooks{}
			switch stage {
			case "after marker":
				h.afterMarker = func() error { return stop }
			case "after first removal":
				fired := false
				h.afterRemove = func(string) error {
					if fired {
						return nil
					}
					fired = true
					return stop
				}
			}
			if _, err := pruneCompletedTransactions(s, h); !errors.Is(err, stop) {
				t.Fatalf("interrupted prune error = %v, want %v", err, stop)
			}
			if stage == "after marker" {
				entries, err := os.ReadDir(s.RecoveryDir())
				if err != nil {
					t.Fatal(err)
				}
				var markers []string
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".pruned") {
						markers = append(markers, entry.Name())
					}
				}
				if len(markers) != 1 {
					t.Fatalf("interruption left prune markers %v, want exactly one", markers)
				}
				key, _, err := parseTxnPruneName(markers[0])
				if err != nil {
					t.Fatal(err)
				}
				record, err := decodeTxnPrune(s, key, markers[0])
				if err != nil {
					t.Fatal(err)
				}
				if len(record.Revisions) == 0 || record.CompletionName == "" || uint64(len(record.Revisions)) != record.Completion.Sequence {
					t.Fatalf("prune marker does not bind the complete family: %+v", record)
				}
			}
			if pending, err := PendingTxns(s); err != nil || len(pending) != 0 {
				t.Fatalf("durable prune record must make partial cleanup safe: pending=%+v err=%v", pending, err)
			}
			if _, err := PruneCompletedTransactions(s); err != nil {
				t.Fatalf("retry must finish prune cleanup: %v", err)
			}
			entries, err := os.ReadDir(s.RecoveryDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "txn-") {
					t.Fatalf("retry left transaction artifact %s", entry.Name())
				}
			}
		})
	}
}

func TestPruneCompletedTransactionsIsolatesDamagedFamilies(t *testing.T) {
	s, _ := setupStore(t)
	for _, record := range []*TxnRecord{
		{Op: "aaa", Name: "damaged", Stage: "committed"},
		{Op: "zzz", Name: "healthy", Stage: "committed"},
	} {
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		if err := ClearTxn(s, *record); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var damagedPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "txn-aaa-") && strings.HasSuffix(entry.Name(), ".json") {
			damagedPath = filepath.Join(s.RecoveryDir(), entry.Name())
			break
		}
	}
	if damagedPath == "" {
		t.Fatal("damaged transaction revision not found")
	}
	revisionName, err := parseTxnRecordName(filepath.Base(damagedPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damagedPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	damagedBefore := map[string][]byte{}
	entries, err = os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "txn-aaa-") || strings.HasPrefix(entry.Name(), "txn-aaa.") {
			raw, readErr := os.ReadFile(filepath.Join(s.RecoveryDir(), entry.Name()))
			if readErr != nil {
				t.Fatal(readErr)
			}
			damagedBefore[entry.Name()] = raw
		}
	}

	outcome, err := PruneCompletedTransactions(s)
	if err == nil || outcome.Transactions != 1 {
		t.Fatalf("partial prune = %+v, %v; want one healthy family plus an error", outcome, err)
	}
	if !strings.Contains(err.Error(), damagedPath) {
		t.Fatalf("damaged-family error %q lacks exact path %q", err, damagedPath)
	}
	for _, want := range []string{
		filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s-*.json", revisionName.key.op, revisionName.key.id)),
		filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s.done", revisionName.key.op, revisionName.key.id)),
		filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s-*.pruned", revisionName.key.op, revisionName.key.id)),
		"move the complete transaction family", "retry",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("damaged-family error %q lacks remedy detail %q", err, want)
		}
	}
	entries, err = os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "txn-zzz-") {
			t.Fatalf("healthy family was not pruned: %s", entry.Name())
		}
	}
	for name, want := range damagedBefore {
		got, readErr := os.ReadFile(filepath.Join(s.RecoveryDir(), name))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("damaged family artifact %s changed: got=%q want=%q err=%v", name, got, want, readErr)
		}
	}
	moved := t.TempDir()
	for name := range damagedBefore {
		if err := os.Rename(filepath.Join(s.RecoveryDir(), name), filepath.Join(moved, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewSkill(s, nil, "epsilon"); err != nil {
		t.Fatalf("following the whole-family remedy must not manufacture a pending transaction: %v", err)
	}
}

func TestPrunedFamilyWithoutCompletionUsesWholeFamilyRemedy(t *testing.T) {
	s, _ := setupStore(t)
	record := &TxnRecord{Op: "prune-remedy", Name: "alpha", Stage: "committed"}
	if err := WriteTxn(s, record); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(s, *record); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after prune marker")
	if _, err := pruneCompletedTransactions(s, pruneHooks{afterMarker: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("setup prune interruption = %v, want %v", err, stop)
	}
	completion := filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s.done", record.Op, record.TxnID))
	if err := os.Remove(completion); err != nil {
		t.Fatal(err)
	}
	_, err := PruneCompletedTransactions(s)
	if err == nil {
		t.Fatal("prune record without completion and with revisions must conflict")
	}
	for _, want := range []string{"move the complete transaction family", "*.json", ".done", "*.pruned", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("pruned-family remedy %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "move the prune record aside") {
		t.Fatalf("pruned-family remedy still prescribes the wedging single-file action: %v", err)
	}
	family, err := filepath.Glob(filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s*", record.Op, record.TxnID)))
	if err != nil {
		t.Fatal(err)
	}
	moved := t.TempDir()
	for _, artifact := range family {
		if err := os.Rename(artifact, filepath.Join(moved, filepath.Base(artifact))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatalf("whole-family abandonment must unblock the following write: %v", err)
	}
}

func TestPruneCompletedTransactionsPreservesNonJournalAndPendingRecovery(t *testing.T) {
	s, _ := setupStore(t)
	completed := &TxnRecord{Op: "complete", Name: "done", Stage: "committed"}
	if err := WriteTxn(s, completed); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(s, *completed); err != nil {
		t.Fatal(err)
	}
	pending := &TxnRecord{Op: "pending", Name: "wait", Stage: "started"}
	if err := WriteTxn(s, pending); err != nil {
		t.Fatal(err)
	}
	// SPEC §5.1 names four things gc must never delete, and three of them had
	// no retention assertion anywhere: .fu-archive-*, rollback-* and
	// adopt-link-*.json. All four are pinned here now.
	preserved := []string{
		"adopt-archive-claude-alpha-deadbeef",
		"removed-alpha-deadbeef",
		".fu-config-exchange-deadbeefdeadbeef.json",
		".fu-archive-0011223344556677aabbccdd",
		"rollback-new-alpha-deadbeef",
		"adopt-link-00112233.json",
	}
	for _, name := range preserved {
		path := filepath.Join(s.RecoveryDir(), name)
		if strings.Contains(name, "archive") || strings.HasPrefix(name, "removed-") || strings.HasPrefix(name, "rollback-") {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("preserve"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if outcome, err := PruneCompletedTransactions(s); err != nil || outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, %v", outcome, err)
	}
	for _, name := range preserved {
		if _, err := os.Lstat(filepath.Join(s.RecoveryDir(), name)); err != nil {
			t.Fatalf("gc removed excluded recovery object %s: %v", name, err)
		}
	}
	pendingTxns, err := PendingTxns(s)
	if err != nil || len(pendingTxns) != 1 || pendingTxns[0].Op != "pending" {
		t.Fatalf("gc changed pending transaction: %+v, %v", pendingTxns, err)
	}
}

func TestJournalScanFailureIncludesActionableRemedy(t *testing.T) {
	s, _ := setupStore(t)
	bad := filepath.Join(s.RecoveryDir(), "txn-malformed.json")
	if err := os.WriteFile(bad, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewSkill(s, nil, "alpha")
	if err == nil {
		t.Fatal("malformed journal must block ordinary writes")
	}
	for _, want := range []string{bad, "preserve", "move", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("journal scan error %q lacks actionable detail %q", err, want)
		}
	}
}

func TestPruneRecordCannotHideTransactionWithoutCompletionMarker(t *testing.T) {
	s, _ := setupStore(t)
	record := &TxnRecord{Op: "new", Name: "alpha", Stage: "committed"}
	if err := WriteTxn(s, record); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(s, *record); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after prune marker")
	if _, err := pruneCompletedTransactions(s, pruneHooks{afterMarker: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("interrupted prune error = %v, want %v", err, stop)
	}
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var completion, prune string
	for _, entry := range entries {
		switch {
		case strings.HasSuffix(entry.Name(), ".done"):
			completion = filepath.Join(s.RecoveryDir(), entry.Name())
		case strings.HasSuffix(entry.Name(), ".pruned"):
			prune = filepath.Join(s.RecoveryDir(), entry.Name())
		}
	}
	if completion == "" || prune == "" {
		t.Fatalf("interrupted family lacks completion or prune marker: completion=%q prune=%q", completion, prune)
	}
	if err := os.Remove(completion); err != nil {
		t.Fatal(err)
	}

	_, err = PendingTxns(s)
	if err == nil {
		t.Fatal("a prune record must not hide remaining revisions without their completion marker")
	}
	if !strings.Contains(err.Error(), prune) || !strings.Contains(err.Error(), "move") {
		t.Fatalf("prune conflict %q must name the record and an actionable remedy", err)
	}
}

func txnRevisionPaths(t *testing.T, s *store.Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "txn-") && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, filepath.Join(s.RecoveryDir(), entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths
}

func rewriteTxnMessage(t *testing.T, path, message string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record TxnRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record.Message = message
	raw, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func txnRecordWithSerializedSize(t *testing.T, size int) *TxnRecord {
	t.Helper()
	const id = "00000000000000000000000000000001"
	probe := TxnRecord{Op: "sized", TxnID: id, Sequence: 1, Message: "x"}
	raw, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	messageBytes := size - (len(raw) - 1)
	if messageBytes < 1 {
		t.Fatalf("requested serialized size %d is too small", size)
	}
	record := &TxnRecord{Op: "sized", TxnID: id, Message: strings.Repeat("x", messageBytes)}
	persisted := *record
	persisted.Sequence = 1
	raw, err = json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != size {
		t.Fatalf("constructed record is %d bytes, want %d", len(raw), size)
	}
	return record
}

func TestTxnWriterMatchesReaderSizeLimit(t *testing.T) {
	t.Run("exact limit remains readable and completable", func(t *testing.T) {
		s, _ := setupStore(t)
		record := txnRecordWithSerializedSize(t, int(maxTxnRecordBytes))
		if err := WriteTxn(s, record); err != nil {
			t.Fatalf("exact-limit record must be accepted: %v", err)
		}
		paths := txnRevisionPaths(t, s)
		if len(paths) != 1 {
			t.Fatalf("got %d revisions, want 1", len(paths))
		}
		info, err := os.Stat(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != maxTxnRecordBytes {
			t.Fatalf("persisted size = %d, want %d", info.Size(), maxTxnRecordBytes)
		}
		pending, err := PendingTxns(s)
		if err != nil || len(pending) != 1 {
			t.Fatalf("exact-limit revision must be readable: pending=%+v err=%v", pending, err)
		}
		if err := ClearTxn(s, *record); err != nil {
			t.Fatalf("complete exact-limit transaction: %v", err)
		}
		if err := ClearTxn(s, *record); err != nil {
			t.Fatalf("retry completed transaction: %v", err)
		}
		pending, err = PendingTxns(s)
		if err != nil || len(pending) != 0 {
			t.Fatalf("completed exact-limit transaction must stay completed: pending=%+v err=%v", pending, err)
		}
	})

	t.Run("one byte over is rejected before create", func(t *testing.T) {
		s, _ := setupStore(t)
		record := txnRecordWithSerializedSize(t, int(maxTxnRecordBytes)+1)
		if err := WriteTxn(s, record); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("one-byte-over record must be rejected with a size error, got %v", err)
		}
		if paths := txnRevisionPaths(t, s); len(paths) != 0 {
			t.Fatalf("oversized record created journal files: %v", paths)
		}
	})
}

// The name this test used to carry (TestOversizedInitialTxnStopsBeforeMutation)
// claimed the WAL's own 16 MiB record limit. It never reached it: the fixture
// writes a 13 MiB fu.yaml, MaxConfigBytes is 8 MiB, and LoadConfigRootBytes
// terminates the command before CheckWritable, before Sweep and long before a
// WAL byte is written. The scenario is unreachable for `new` at all, since
// ConfigBefore is bounded by the config limit and 8 MiB < 16 MiB.
//
// What it does verify is still worth having, so it keeps the test and loses the
// misleading name: an oversized config stops the pipeline before Mutate runs
// and leaves no journal file behind. The WAL writer/reader symmetry it appeared
// to cover is covered by TestTxnWriterMatchesReaderSizeLimit.
func TestOversizedConfigStopsBeforeMutation(t *testing.T) {
	s, _ := setupStore(t)
	raw, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n', '#')
	raw = append(raw, strings.Repeat("x", 13<<20)...)
	if err := store.WriteFileAtomic(s.ConfigPath(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	mutated := false
	_, err = Run(s, nil, Op{
		Message: "oversized: must stop",
		Txn:     &TxnRecord{Op: "oversized"},
		Mutate: func(*store.Store, *store.Config) error {
			mutated = true
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized initial transaction must return a size error, got %v", err)
	}
	if mutated {
		t.Fatal("transaction mutation ran after its initial journal record exceeded the readable limit")
	}
	if paths := txnRevisionPaths(t, s); len(paths) != 0 {
		t.Fatalf("oversized initial transaction created journal files: %v", paths)
	}
}

func TestTxnRevisionChainRejectsSemanticReplacement(t *testing.T) {
	t.Run("an older revision cannot change beneath a newer revision", func(t *testing.T) {
		s, _ := setupStore(t)
		record := &TxnRecord{Op: "chain", Stage: "started"}
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		record.Stage = "prepared"
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		paths := txnRevisionPaths(t, s)
		if len(paths) != 2 {
			t.Fatalf("got %d revisions, want 2", len(paths))
		}
		rewriteTxnMessage(t, paths[0], "foreign older revision")

		if _, err := PendingTxns(s); err == nil {
			t.Fatal("a syntactically valid replacement of an older revision must be rejected")
		}
	})

	// A completion marker binds to its newest revision by sequence and digest,
	// both of which live in the immutable, no-replace-created filename. Round
	// 18 finding I5 moved that check off the file contents: a completed
	// transaction's revisions are never read again, so an appended, deleted or
	// renamed revision is still caught, while in-place content tampering on a
	// record nothing will ever act on is not. ClearTxn still validates the
	// whole chain from disk before writing the marker, so the binding is
	// established against real bytes at the one moment it decides anything.
	t.Run("completion remains bound to the newest revision filename", func(t *testing.T) {
		s, _ := setupStore(t)
		record := &TxnRecord{Op: "complete", Stage: "committed"}
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		if err := ClearTxn(s, *record); err != nil {
			t.Fatal(err)
		}
		paths := txnRevisionPaths(t, s)
		if len(paths) != 1 {
			t.Fatalf("got %d revisions, want 1", len(paths))
		}
		if _, err := PendingTxns(s); err != nil {
			t.Fatalf("the completed transaction must validate: %v", err)
		}

		// A revision appended past the one the marker names is a semantic
		// replacement of the transaction's tail and must be rejected. WriteTxn
		// itself refuses to extend a completed transaction, so the file is
		// planted directly -- the shape an attacker or a partial restore would
		// leave behind.
		raw, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		planted := strings.Replace(filepath.Base(paths[0]),
			fmt.Sprintf("-%0*d-", txnSequenceWidth, 1), fmt.Sprintf("-%0*d-", txnSequenceWidth, 2), 1)
		if planted == filepath.Base(paths[0]) {
			t.Fatalf("could not derive a higher-sequence name from %q", paths[0])
		}
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), planted), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := PendingTxns(s); err == nil {
			t.Fatal("a completion marker must not hide a revision appended after it")
		}
	})

	t.Run("a deleted revision breaks a completed chain", func(t *testing.T) {
		s, _ := setupStore(t)
		record := &TxnRecord{Op: "gapped", Stage: "started"}
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		record.Stage = "committed"
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		if err := ClearTxn(s, *record); err != nil {
			t.Fatal(err)
		}
		paths := txnRevisionPaths(t, s)
		if len(paths) != 2 {
			t.Fatalf("got %d revisions, want 2", len(paths))
		}
		if err := os.Remove(paths[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := PendingTxns(s); err == nil {
			t.Fatal("a completed chain missing a revision must be rejected")
		}
	})
}

func TestTxnUpdatesValidateThePersistedLatestRevision(t *testing.T) {
	t.Run("append", func(t *testing.T) {
		s, _ := setupStore(t)
		record := &TxnRecord{Op: "append", Stage: "started"}
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		rewriteTxnMessage(t, txnRevisionPaths(t, s)[0], "foreign before append")
		record.Stage = "prepared"
		if err := WriteTxn(s, record); err == nil {
			t.Fatal("appending must reject a replaced latest revision")
		}
	})

	t.Run("completion", func(t *testing.T) {
		s, _ := setupStore(t)
		record := &TxnRecord{Op: "finish", Stage: "committed"}
		if err := WriteTxn(s, record); err != nil {
			t.Fatal(err)
		}
		rewriteTxnMessage(t, txnRevisionPaths(t, s)[0], "foreign before completion")
		if err := ClearTxn(s, *record); err == nil {
			t.Fatal("completion must reject a replaced latest revision")
		}
	})
}

// TestTxnOpNamesAreValidatedAndDispatchFollowsTheFilename is round 6's
// recovery-record finding. The operation name was concatenated straight
// into "txn-"+op+".json", so a name carrying a path separator or dot-dot
// wrote its record outside the recovery directory entirely -- or, at the
// less dramatic end, produced a filename PendingTxns would never list
// again, leaving a transaction recorded but permanently unrecoverable.
//
// Recovery also trusted the record's own JSON "op" field to choose a
// handler, without checking it against the filename it came from. Since
// the filename is what WriteTxn validated, and the content is what recovery
// dispatched on, a record whose two halves disagreed ran -- and cleared --
// the wrong handler.
func TestTxnOpNamesAreValidatedAndDispatchFollowsTheFilename(t *testing.T) {
	t.Run("a malformed op name is refused at write time", func(t *testing.T) {
		s, _ := setupStore(t)
		for _, op := range []string{
			"", "..", "../escape", "a/b", "a\\b", "UPPER", "-leading", "trailing-",
			"with space", "with.dot", "with_underscore",
		} {
			if err := WriteTxn(s, &TxnRecord{Op: op}); err == nil {
				t.Errorf("op %q must be refused before it becomes part of a path", op)
			}
		}
		for _, op := range []string{"adopt", "update", "a", "multi-word-op", "op2"} {
			if err := WriteTxn(s, &TxnRecord{Op: op}); err != nil {
				t.Errorf("op %q is well formed and must be accepted: %v", op, err)
			}
		}
	})

	t.Run("nothing is written outside the recovery directory", func(t *testing.T) {
		s, _ := setupStore(t)
		escaped := filepath.Join(s.RecoveryDir(), "..", "txn-escape.json")
		if err := WriteTxn(s, &TxnRecord{Op: "../escape"}); err == nil {
			t.Fatal("a path-escaping op must be refused")
		}
		if _, err := os.Lstat(escaped); err == nil {
			t.Fatalf("a refused record must leave nothing at %s", escaped)
		}
	})

	t.Run("recovery dispatches on the filename, not the record's own claim", func(t *testing.T) {
		s, _ := setupStore(t)
		// A record filed as "adopt" whose content claims to be "update".
		const id = "00000000000000000000000000000001"
		filed := TxnRecord{Op: "adopt", TxnID: id, Sequence: 1}
		raw := []byte(`{"op":"update","txn_id":"` + id + `","sequence":1,"start_head":"","stage":"","targets":null}`)
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), txnRecordName(filed)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		ran := ""
		RegisterRecoverHandler("update", func(*store.Store, TxnRecord) error { ran = "update"; return nil })
		RegisterRecoverHandler("adopt", func(*store.Store, TxnRecord) error { ran = "adopt"; return nil })
		defer deleteRecoverHandler("update")
		defer deleteRecoverHandler("adopt")

		err := RecoverPending(checkedRecoveryStore(t, s))
		if err == nil && ran == "update" {
			t.Fatal("a record whose filename and content disagree must not be dispatched on its own claim")
		}
		if err == nil && ran == "adopt" {
			t.Fatal("a record whose filename and content disagree must be refused, not resolved in the filename's favour")
		}
		if err == nil {
			t.Fatal("a record whose filename and content disagree must be reported")
		}
	})
}

func TestTxnLifecycleRejectsReplacementAtUpdateAndCompletionBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		hooks func(func() error) hooks
	}{
		{
			name: "stage update",
			hooks: func(replace func() error) hooks {
				return hooks{afterTxnStart: replace}
			},
		},
		{
			name: "completion",
			hooks: func(replace func() error) hooks {
				return hooks{afterCommit: replace}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := setupStore(t)
			foreign := []byte("foreign WAL replacement")
			replacedPath := ""
			replace := func() error {
				entries, err := os.ReadDir(s.RecoveryDir())
				if err != nil {
					return err
				}
				for _, entry := range entries {
					if len(entry.Name()) >= len("txn-.json") &&
						entry.Name()[:4] == "txn-" && filepath.Ext(entry.Name()) == ".json" {
						replacedPath = filepath.Join(s.RecoveryDir(), entry.Name())
					}
				}
				if replacedPath == "" {
					return os.ErrNotExist
				}
				if err := os.Remove(replacedPath); err != nil {
					return err
				}
				return os.WriteFile(replacedPath, foreign, 0o644)
			}

			if _, err := newSkill(s, nil, "alpha", tt.hooks(replace)); err == nil {
				t.Fatal("operation must stop when its latest journal revision was replaced")
			}
			got, err := os.ReadFile(replacedPath)
			if err != nil {
				t.Fatalf("foreign replacement at %s must survive: %v", replacedPath, err)
			}
			if !bytes.Equal(got, foreign) {
				t.Fatalf("foreign replacement changed: got %q want %q", got, foreign)
			}
			if _, err := PendingTxns(s); err == nil {
				t.Fatal("the replaced journal must remain a visible safe conflict")
			}
		})
	}
}

// TestRecoverPendingNamesTheWayOutOfADamagedConfigExchangeRecord pins the one
// recovery-directory failure in this package that used to arrive without a
// remedy.
//
// It is also the worst place to omit one. RecoverConfigExchanges is the first
// step of every write command and of `fu restore`, so a name fu cannot parse
// stops all of them; and the pending scan reads a looser grammar than the
// collector does, so it takes authority over names `fu gc` walks straight past
// and no command will ever clear. What the user saw was a bare filename with
// no directory and no instruction.
func TestRecoverPendingNamesTheWayOutOfADamagedConfigExchangeRecord(t *testing.T) {
	s, _ := setupStore(t)
	// A well-formed-enough name for the loose pending grammar, holding bytes
	// no version of the record schema accepts.
	name := ".fu-config-exchange-nothex.json"
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Through a real write command, which is where a user meets this: recovery
	// is its mandatory first step, so the record wedges the command before it
	// does anything of its own.
	_, err := NewSkill(s, nil, "alpha")
	if err == nil {
		t.Fatal("a record fu cannot read must stop the write command")
	}
	for _, want := range []string{name, s.RecoveryDir(), "move the exact file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must say where the file is and what to do with it (missing %q): %v", want, err)
		}
	}
}

// TestEveryOperationCommitThisPackageWritesIsCountable is the coupling test
// store.IsOperationMessage's whitelist actually needs.
//
// The guard that existed for it lived in internal/store and asserted against a
// hand-maintained list of message strings, so both sides of the coupling were
// hand-maintained lists of the same thing: a ninth verb-producing command could
// be added, operationVerbs left alone, and the test would stay green because
// nobody had told it about the new verb either. The failure that hides behind
// that is quiet and wrong in the worst direction -- an uncounted operation is
// one `fu revert n` steps straight over, reaching one operation too far back.
//
// This runs the real commands and reads back what they actually committed, so
// the message set is the engine's rather than a copy of it. Every commit fu
// writes must be classifiable: an operation SPEC §5.3 lists, or one of the
// three forms that are deliberately not operations.
func TestEveryOperationCommitThisPackageWritesIsCountable(t *testing.T) {
	s, _ := setupStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{agent.Claude{}}

	// One call per Op.Message producer in this package: ops.go's three forms,
	// rm.go, adopt.go, and the store's own revert message.
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := SetGlobal(s, agents, "alpha", false); err != nil {
		t.Fatal(err)
	}
	if _, err := SetAgentSwitch(s, agents, "alpha", "claude", true); err != nil {
		t.Fatal(err)
	}
	// A hand edit, so a sweep's own "external" commit is in history too and the
	// classification below has to admit it without counting it.
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "NOTES.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, agentDir, "beta", "---\nname: beta\ndescription: d\n---\n")
	if _, err := adopt(s, agents, "", hooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := RevertOperations(s, agents, 1); err != nil {
		t.Fatal(err)
	}

	log, err := s.Log(200)
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string]bool{}
	for _, commit := range log {
		message := commit.Message
		switch {
		case message == store.ExternalCommitMessage:
		case strings.HasPrefix(message, store.RecoveryCompensationPrefix):
		case message == "init: store":
		case store.IsOperationMessage(message):
			verb, _, _ := strings.Cut(message, ":")
			verbs[verb] = true
		default:
			t.Errorf("commit %q is neither one of SPEC §5.3's operations nor one of fu's three bookkeeping forms; if it is a new operation, add its verb to operationVerbs (internal/store/git.go) and to SPEC §5.3", message)
		}
	}
	// And the fixture really did drive the producers, so a future edit that
	// quietly stops exercising one is visible rather than silently narrowing
	// what the loop above can catch.
	for _, want := range []string{"new", "enable", "disable", "adopt", "rm", "revert"} {
		if !verbs[want] {
			t.Errorf("the fixture must exercise the %q producer; observed %v", want, verbs)
		}
	}
}
