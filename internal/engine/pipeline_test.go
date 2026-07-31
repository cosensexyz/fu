// internal/engine/pipeline_test.go
package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/gofrs/flock"

	"fu/internal/agent"
	"fu/internal/store"
)

func TestRunPipelineOrderAndSweep(t *testing.T) {
	s, _ := setupStore(t)
	// external edit exists before the operation
	os.WriteFile(filepath.Join(s.Dir(), "hand.md"), []byte("x"), 0o644)

	_, err := Run(s, nil, Op{
		Message:        "op: test",
		AllowedChanges: []string{"op.md"},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			return os.WriteFile(filepath.Join(st.Dir(), "op.md"), []byte("y"), 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := s.Log(3)
	// newest first: op commit, then the sweep commit, then init
	if entries[0].Message != "op: test" {
		t.Fatalf("head must be the op commit, got %q", entries[0].Message)
	}
	if entries[1].Message != "external: manual modifications" {
		t.Fatalf("external edit must be swept into its own commit, got %q", entries[1].Message)
	}
}

func TestRunBlocksOnUnknownPendingTxn(t *testing.T) {
	s, _ := setupStore(t)
	WriteTxn(s, &TxnRecord{Op: "future-op", Stage: "started"})
	_, err := Run(s, nil, Op{Message: "op: x",
		Mutate: func(*store.Store, *store.Config) error { return nil }})
	if err == nil {
		t.Fatal("unknown pending txn must block write commands")
	}
}

func TestRecoverHandlerRunsAndClears(t *testing.T) {
	s, _ := setupStore(t)
	WriteTxn(s, &TxnRecord{Op: "test-op", Stage: "started"})
	ran := false
	RegisterRecoverHandler("test-op", func(st *store.Store, r TxnRecord) error {
		ran = true
		return ClearTxn(st, r)
	})
	t.Cleanup(func() { deleteRecoverHandler("test-op") })
	if err := RecoverPending(checkedRecoveryStore(t, s)); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("handler must run for its pending txn")
	}
	pend, _ := PendingTxns(s)
	if len(pend) != 0 {
		t.Fatal("handler cleared the record; none should remain")
	}
}

func TestRunReconcilesAtEnd(t *testing.T) {
	s, cfg := setupStore(t)
	_ = cfg
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	_, err := Run(s, agents, Op{
		Message:        "new: alpha",
		AllowedChanges: []string{"fu.yaml", "skills/alpha"},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			os.MkdirAll(filepath.Join(st.SkillsDir(), "alpha"), 0o755)
			return cfg.AddSkill("alpha", "sha256:x")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal("pipeline must reconcile links after the commit")
	}
}

// ---- self-review additions ----

// The lock must be released whether Mutate succeeds or fails: withLock's
// Unlock is deferred right after the lock is acquired, so a Mutate error
// must not leave a stale lock behind for the next command.
func TestWithLockReleasesOnMutateError(t *testing.T) {
	s, _ := setupStore(t)
	wantErr := errors.New("boom")
	_, err := Run(s, nil, Op{
		Message: "op: fail",
		Mutate:  func(*store.Store, *store.Config) error { return wantErr },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("want mutate error propagated, got %v", err)
	}
	fl := flock.New(s.LockPath())
	locked, err := fl.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("lock must be released after Run returns an error")
	}
	fl.Unlock()
}

// A Mutate failure must leave the store uncommitted, not half-recorded:
// no commit under the op's message, and any partial on-disk side effect
// stays as plain uncommitted dirt for the next Sweep to pick up, rather
// than silently vanishing or being folded into a commit.
func TestRunMutateFailureLeavesStoreUncommitted(t *testing.T) {
	s, _ := setupStore(t)
	before, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("boom")
	_, err = Run(s, nil, Op{
		Message: "op: should-not-appear",
		Mutate: func(st *store.Store, cfg *store.Config) error {
			// A partial side effect before the failure: e.g. a multi-step
			// Mutate that wrote a file and mutated the in-memory config,
			// then hit an error.
			if writeErr := os.WriteFile(filepath.Join(st.Dir(), "partial.md"), []byte("x"), 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
			if addErr := cfg.AddSkill("alpha", "sha256:x"); addErr != nil {
				t.Fatal(addErr)
			}
			return wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("want mutate error propagated, got %v", err)
	}
	after, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("mutate failure must not create a commit: before=%d after=%d", len(before), len(after))
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("partial mutate side effect must remain as uncommitted dirt, not vanish silently")
	}
	onDisk, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.HasSkill("alpha") {
		t.Fatal("cfg.Save must never run after a failed Mutate: the in-memory config edit must not reach disk")
	}
}

// Recovery must run before sweep, and that order must be observable in
// history: a recovery handler that leaves an uncommitted change (instead
// of committing it itself) is only captured as its own "external:" commit
// if Sweep runs after recovery. Were the order reversed, the leftover
// would still be dirty when the op's own commit runs and would be folded
// into it instead, leaving no separate external commit.
func TestRecoveryRunsBeforeSweep(t *testing.T) {
	s, _ := setupStore(t)
	WriteTxn(s, &TxnRecord{Op: "leaves-dirty-op", Stage: "started"})
	RegisterRecoverHandler("leaves-dirty-op", func(st *store.Store, r TxnRecord) error {
		if err := os.WriteFile(filepath.Join(st.Dir(), "recovered.md"), []byte("r"), 0o644); err != nil {
			return err
		}
		return ClearTxn(st, r)
	})
	t.Cleanup(func() { deleteRecoverHandler("leaves-dirty-op") })

	_, err := Run(s, nil, Op{
		Message:        "op: after-recovery",
		AllowedChanges: []string{"op-marker.md"},
		// Mutate must itself change something: store.Commit treats an
		// empty worktree as a no-op (see git.go), so a no-op Mutate here
		// would leave HEAD at the sweep commit and defeat the assertion
		// below for a reason unrelated to ordering.
		Mutate: func(st *store.Store, cfg *store.Config) error {
			return os.WriteFile(filepath.Join(st.Dir(), "op-marker.md"), []byte("m"), 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.Log(3)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Message != "op: after-recovery" {
		t.Fatalf("head must be the op commit, got %q", entries[0].Message)
	}
	if entries[1].Message != "external: manual modifications" {
		t.Fatalf("recovery's leftover change must be swept into its own commit before the op commit, got %q", entries[1].Message)
	}
}

// Unrelated files sharing the recovery directory (future residues like
// *.old, or anything else that ends up there) must not be misread as
// transaction records.
func TestPendingTxnsIgnoresUnrelatedFiles(t *testing.T) {
	s, _ := setupStore(t)
	if err := WriteTxn(s, &TxnRecord{Op: "real-op", Stage: "started"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes.txt", "backup.old", "txn-missing-suffix", "wrong-prefix.json"} {
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pend, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || pend[0].Op != "real-op" {
		t.Fatalf("unrelated files in the recovery dir must be ignored, got %+v", pend)
	}
}

// A file that matches the txn-*.json naming convention but is not valid
// JSON (e.g. corruption, or a write that bypassed WriteFileAtomic) must
// surface as an error rather than being silently skipped: an
// unrecognizable record must block, exactly like an unknown op type.
func TestPendingTxnsErrorsOnMalformedRecord(t *testing.T) {
	s, _ := setupStore(t)
	name := txnRecordName(TxnRecord{Op: "corrupt", TxnID: "00000000000000000000000000000001", Sequence: 1})
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingTxns(s); err == nil {
		t.Fatal("malformed txn record must surface as an error, not be silently ignored")
	}
	if err := RecoverPending(checkedRecoveryStore(t, s)); err == nil {
		t.Fatal("RecoverPending must propagate the malformed-record error rather than proceeding")
	}
}

func TestPendingTxnsRejectsOversizedRecord(t *testing.T) {
	s, _ := setupStore(t)
	record := TxnRecord{
		Op:       "future-op",
		TxnID:    "00000000000000000000000000000001",
		Sequence: 1,
	}
	raw := []byte(`{"op":"future-op","txn_id":"00000000000000000000000000000001","sequence":1}` + strings.Repeat(" ", 17<<20))
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), txnRecordName(record)), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingTxns(s); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized transaction record must be rejected before parsing, got %v", err)
	}
}

// A registered handler that fails must abort recovery for that record
// without clearing it: RecoverPending must not treat a failed attempt as
// resolved, and must not move on past it to a half-recovered state.
func TestRecoverPendingAbortsOnHandlerError(t *testing.T) {
	s, _ := setupStore(t)
	WriteTxn(s, &TxnRecord{Op: "failing-op", Stage: "started"})
	handlerErr := errors.New("cannot safely recover")
	RegisterRecoverHandler("failing-op", func(*store.Store, TxnRecord) error {
		return handlerErr
	})
	t.Cleanup(func() { deleteRecoverHandler("failing-op") })

	if err := RecoverPending(checkedRecoveryStore(t, s)); !errors.Is(err, handlerErr) {
		t.Fatalf("want handler error propagated, got %v", err)
	}
	pend, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || pend[0].Op != "failing-op" {
		t.Fatalf("failed recovery must leave the record in place, got %+v", pend)
	}
}

// A failing recovery handler must abort the whole pipeline: Mutate must
// never run, and neither a sweep commit nor an op commit may appear.
func TestRunAbortsWhenRecoveryHandlerFails(t *testing.T) {
	s, _ := setupStore(t)
	WriteTxn(s, &TxnRecord{Op: "failing-op2", Stage: "started"})
	RegisterRecoverHandler("failing-op2", func(*store.Store, TxnRecord) error {
		return errors.New("cannot safely recover")
	})
	t.Cleanup(func() { deleteRecoverHandler("failing-op2") })
	if err := os.WriteFile(filepath.Join(s.Dir(), "hand.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	mutateRan := false
	_, err := Run(s, nil, Op{
		Message: "op: should-not-run",
		Mutate: func(*store.Store, *store.Config) error {
			mutateRan = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("Run must abort when a recovery handler fails")
	}
	if mutateRan {
		t.Fatal("mutate must not run when recovery fails -- no half-recovered progression")
	}
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != "init: store" {
		t.Fatalf("no sweep/op commit must happen after a failed recovery, got %+v", entries)
	}
}

// An unknown pending txn must block before any other pipeline effect:
// Mutate never runs, no sweep or op commit appears, and the returned
// error wraps ErrUnknownTxn so callers can distinguish this case.
func TestRunBlocksOnUnknownPendingTxnPreventsSweepAndMutate(t *testing.T) {
	s, _ := setupStore(t)
	WriteTxn(s, &TxnRecord{Op: "future-op", Stage: "started"})
	if err := os.WriteFile(filepath.Join(s.Dir(), "hand.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mutateRan := false
	_, err := Run(s, nil, Op{Message: "op: x",
		Mutate: func(*store.Store, *store.Config) error { mutateRan = true; return nil }})
	if err == nil {
		t.Fatal("unknown pending txn must block write commands")
	}
	if !errors.Is(err, ErrUnknownTxn) {
		t.Fatalf("error must wrap ErrUnknownTxn, got %v", err)
	}
	if mutateRan {
		t.Fatal("mutate must not run when blocked by an unknown pending txn")
	}
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("no sweep/op commit must happen when blocked, got %+v", entries)
	}
}

// Parse-failure errors must name the offending file so users can act on them.
func TestPendingTxnsParseErrorNamesFile(t *testing.T) {
	s, _ := setupStore(t)
	badFilename := txnRecordName(TxnRecord{Op: "corrupt-record", TxnID: "00000000000000000000000000000001", Sequence: 1})
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), badFilename), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := PendingTxns(s)
	if err == nil {
		t.Fatal("malformed record must return an error")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, badFilename) {
		t.Fatalf("error message must name the offending file %q, got: %s", badFilename, errMsg)
	}
}

// Round 2 finding 4: the config version guard must be a precondition
// checked before EITHER Sweep or Mutate runs, not discovered only when
// cfg.Save runs after Mutate has already written into the store
// directory, and not only after Sweep has already committed the very
// content this guard exists to refuse. Reproduced against the compiled
// binary pre-fix (finding I1 alone, before CheckWritable existed): on a
// version:99 store, `fu new alpha` returned ErrVersionTooNew but left
// store/skills/alpha/SKILL.md on disk, which the next write command's
// Sweep then committed as "external: manual modifications". Reproduced
// again for round 2 finding 4 with CheckWritable already in place ahead
// of Mutate: `fu enable alpha` over a version:99 store still exited 1
// with the correct refusal message *and* left a new "external: manual
// modifications" commit in history containing that v99 config, because
// Sweep is itself a commit and still ran before CheckWritable. The commit
// message was wrong too: the actual cause was an out-of-range version,
// not a manual content edit.
//
// This replaces the old TestRunRefusesVersionTooNewBeforeMutate, whose
// final assertion ("dirty must be false") pinned the bug instead of
// catching it: it passed precisely because Sweep-before-CheckWritable had
// already committed the offending content by the time IsDirty was
// checked, leaving the worktree clean. Now that CheckWritable runs before
// Sweep too, a refused write must leave that content exactly as the user
// left it -- uncommitted -- which flips the assertion; the added
// commit-count check directly guards the "no commit at all" property
// finding 4 asks for.
func TestRunRefusesVersionTooNewBeforeSweepOrMutate(t *testing.T) {
	s, _ := setupStore(t)
	before, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	// Left dirty on purpose, standing in for a hand-edited fu.yaml sitting
	// in the store uncommitted: the whole point of this test is that
	// CheckWritable must refuse before Sweep ever gets a chance to commit
	// this content under an unrelated "external: manual modifications"
	// message.
	if err := os.WriteFile(s.ConfigPath(), []byte("version: 99\nskills: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mutateRan := false
	marker := filepath.Join(s.SkillsDir(), "alpha.marker")
	_, err = Run(s, nil, Op{
		Message: "op: should-not-run",
		Mutate: func(st *store.Store, cfg *store.Config) error {
			mutateRan = true
			return os.WriteFile(marker, []byte("x"), 0o644)
		},
	})
	if !errors.Is(err, store.ErrVersionTooNew) {
		t.Fatalf("want ErrVersionTooNew, got %v", err)
	}
	if mutateRan {
		t.Fatal("Mutate must not run when the config version guard refuses the write")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("Mutate's side effect must never reach disk when the version guard refuses the write")
	}
	after, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused write must leave no new commit at all -- not even a sweep commit -- before=%+v after=%+v", before, after)
	}
	dirty, err := s.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("a refused write must leave the pre-existing manual edit exactly as uncommitted as the user left it, not swept into history under an unrelated message")
	}
}

// An external edit that lands after the initial sweep must not be overwritten
// by the operation's own config install.
//
// The pipeline captures configBefore right after the sweep and then writes
// whatever the operation produced, so the whole window between them was
// unguarded: a valid fu.yaml written into that gap was replaced wholesale by a
// plain rename, the frozen candidate only ever saw fu's own bytes, and the
// command committed and reported success with the edit gone.
func TestWriteRefusesToOverwriteAPostSweepConfigEdit(t *testing.T) {
	s, _ := setupStore(t)
	var external []byte
	h := hooks{afterMutate: func() error {
		current, err := os.ReadFile(s.ConfigPath())
		if err != nil {
			return err
		}
		external = append(append([]byte(nil), current...), "\n# edited outside fu after the sweep\n"...)
		return os.WriteFile(s.ConfigPath(), external, 0o644)
	}}

	if _, err := newSkill(s, nil, "alpha", h); err == nil {
		t.Fatal("a post-sweep external fu.yaml edit must stop the command")
	}
	got, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, external) {
		t.Fatalf("external fu.yaml edit was not preserved:\ngot  %q\nwant %q", got, external)
	}
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Message == "new: alpha" {
			t.Fatalf("a refused operation must not commit, got %+v", entries)
		}
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("alpha") {
		t.Fatal("a refused operation must not register its skill")
	}
}

// An external edit arriving between the sweep and the baseline capture must not
// become the baseline.
//
// cfg is parsed before Sweep; configBefore was read after it. An edit landing in
// between left the two disagreeing: the mutation model still held the old YAML
// while the conditional install expected the new bytes, so the install matched,
// succeeded, and replaced the external edit with a config derived from a
// snapshot that was already stale.
func TestWriteRefusesAConfigEditArrivingBeforeTheBaseline(t *testing.T) {
	s, _ := setupStore(t)
	var external []byte
	_, err := Run(s, nil, Op{
		Message:        "new: alpha",
		AllowedChanges: []string{"fu.yaml"},
		Preflight: func(_ *store.Store, _ *store.Config) error {
			current, err := os.ReadFile(s.ConfigPath())
			if err != nil {
				return err
			}
			external = append(append([]byte(nil), current...), []byte("\nexternal: keep-me\n")...)
			return os.WriteFile(s.ConfigPath(), external, 0o644)
		},
		Mutate: func(_ *store.Store, cfg *store.Config) error {
			return cfg.AddSkill("alpha", "sha256:"+strings.Repeat("a", 64))
		},
	})
	if err == nil {
		t.Fatal("an external fu.yaml edit arriving before the baseline must stop the command")
	}
	got, readErr := os.ReadFile(s.ConfigPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, external) {
		t.Fatalf("external fu.yaml edit was not preserved:\ngot  %q\nwant %q", got, external)
	}
	cfg, loadErr := store.LoadConfig(s.ConfigPath())
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if cfg.HasSkill("alpha") {
		t.Fatal("a refused operation must not register its skill")
	}
}

// A rolled back operation must leave the public Git index where it found it.
// The operation candidate stays private from preparation through abandonment,
// so a supported direct `git commit` can never publish the rejected config or
// skill while Fu is rolling back.
func TestFailedWriteLeavesNothingStagedInTheIndex(t *testing.T) {
	s, _ := setupStore(t)
	boom := errors.New("commit refused after preparation")
	h := hooks{commit: func(*store.Store, string, store.PreparedCommit) (store.CommitOutcome, error) {
		return store.CommitOutcome{}, boom
	}}

	if _, err := newSkill(s, nil, "alpha", h); !errors.Is(err, boom) {
		t.Fatalf("the injected commit failure must surface, got %v", err)
	}

	// Opened independently, so this is the view a direct git user gets.
	repo, err := git.PlainOpen(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	status, err := wt.Status()
	if err != nil {
		t.Fatal(err)
	}
	for path, state := range status {
		if state.Staging != git.Unmodified {
			t.Errorf("%s is still staged as %q after rollback", path, string(state.Staging))
		}
		if state.Worktree != git.Unmodified {
			t.Errorf("%s is still modified in the worktree as %q after rollback", path, string(state.Worktree))
		}
	}
}
