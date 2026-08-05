// internal/engine/txn_test.go
package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
)

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

	t.Run("completion remains bound to the exact latest revision", func(t *testing.T) {
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
		rewriteTxnMessage(t, paths[0], "foreign completed revision")

		if _, err := PendingTxns(s); err == nil {
			t.Fatal("a completion marker must not hide a replaced latest revision")
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
