package store

import (
	"os"
	"path/filepath"
	"testing"
)

// commitState writes content to one file in the store and commits it under
// msg. Each call must change the content, or Commit records nothing.
func commitState(t *testing.T, s *Store, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir(), "state.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.Commit(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written {
		t.Fatalf("commit %q wrote nothing", msg)
	}
}

// Newest first: the interrupted "add: beta" and its compensation net to zero,
// the sweep and the init commit are never operations, so only "disable: alpha"
// (1) and "new: alpha" (2) carry ordinals -- exactly what `fu revert n` counts.
func TestWalkOperationsNumbersOnlyCountedOperations(t *testing.T) {
	s, err := Init(t.TempDir()) // "init: store"
	if err != nil {
		t.Fatal(err)
	}
	commitState(t, s, "a", "new: alpha")
	commitState(t, s, "b", ExternalCommitMessage)
	commitState(t, s, "c", "add: beta")
	commitState(t, s, "d", RecoveryCompensationPrefix+"add: beta")
	commitState(t, s, "e", "disable: alpha")

	var ordinals []int
	var messages []string
	err = s.WalkOperations(func(e OperationEntry) (bool, error) {
		ordinals = append(ordinals, e.Ordinal)
		messages = append(messages, e.Message)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantOrdinals := []int{1, 0, 0, 0, 2, 0}
	wantMessages := []string{"disable: alpha", RecoveryCompensationPrefix + "add: beta", "add: beta", ExternalCommitMessage, "new: alpha", "init: store"}
	if len(ordinals) != len(wantOrdinals) {
		t.Fatalf("walked %d commits, want %d: %v", len(ordinals), len(wantOrdinals), messages)
	}
	for i := range wantOrdinals {
		if ordinals[i] != wantOrdinals[i] || messages[i] != wantMessages[i] {
			t.Fatalf("entry %d = (%d, %q), want (%d, %q)", i, ordinals[i], messages[i], wantOrdinals[i], wantMessages[i])
		}
	}
}

func TestWalkOperationsStopsWhenVisitReturnsFalse(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commitState(t, s, "a", "new: alpha")
	commitState(t, s, "b", "new: beta")
	visited := 0
	err = s.WalkOperations(func(e OperationEntry) (bool, error) {
		visited++
		return visited < 2, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if visited != 2 {
		t.Fatalf("visited %d commits, want 2", visited)
	}
}

func TestWalkOperationsReportsTheFirstParent(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commitState(t, s, "a", "new: alpha")
	var entries []OperationEntry
	if err := s.WalkOperations(func(e OperationEntry) (bool, error) {
		entries = append(entries, e)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if entries[0].FirstParent != entries[1].Hash {
		t.Fatalf("first parent of HEAD = %s, want %s", entries[0].FirstParent, entries[1].Hash)
	}
	if !entries[1].FirstParent.IsZero() {
		t.Fatalf("root commit must report a zero first parent, got %s", entries[1].FirstParent)
	}
}

// Log is WalkOperations with a count, and now carries the same ordinals.
func TestLogCarriesOperationOrdinals(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commitState(t, s, "a", "new: alpha")
	commitState(t, s, "b", ExternalCommitMessage)
	entries, err := s.Log(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].Ordinal != 0 || entries[1].Ordinal != 1 || entries[2].Ordinal != 0 {
		t.Fatalf("ordinals = %d %d %d, want 0 1 0", entries[0].Ordinal, entries[1].Ordinal, entries[2].Ordinal)
	}
	if len(entries[0].Hash) != 7 {
		t.Fatalf("Log hash must stay the 7-char short form, got %q", entries[0].Hash)
	}
}
