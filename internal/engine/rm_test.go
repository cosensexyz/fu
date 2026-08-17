package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

func TestRmPayloadNameUsesFullStartHead(t *testing.T) {
	startHead := strings.Repeat("a", 40)
	record := TxnRecord{Name: "alpha", StartHead: startHead}

	if got := rmPayloadName(record); !strings.HasSuffix(got, "-"+startHead) {
		t.Fatalf("rm payload name %q does not contain full start HEAD %q", got, startHead)
	}
}

func TestRemoveSkillRemovesEverything(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("alpha") {
		t.Fatal("config entry must be gone")
	}
	if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !os.IsNotExist(err) {
		t.Fatalf("store entity must be gone, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("agent link must be reclaimed, err=%v", err)
	}
	// The removal is one commit; the content is recoverable from history.
	entries, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 || entries[0].Message != "rm: alpha" {
		t.Fatalf("commit history wrong: %+v", entries)
	}
}

func TestRemoveSkillUnknownName(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := RemoveSkill(s, nil, "ghost"); err == nil || !strings.Contains(err.Error(), "unknown skill") {
		t.Fatalf("unknown skill must be refused, got %v", err)
	}
	// Nothing may have been written: the store is still at its initial HEAD.
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Name().Short() != "master" && head.Name().Short() != "main" {
		t.Fatalf("unexpected HEAD %s", head.Name())
	}
}

func TestRemoveSkillContentless(t *testing.T) {
	// A registered skill whose content is gone (failed publish from an older
	// run) must still be removable.
	s, _ := setupStore(t, "alpha")
	if _, err := RemoveSkill(s, nil, "alpha"); err != nil {
		t.Fatalf("contentless removal: %v", err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("alpha") {
		t.Fatal("config entry must be gone")
	}
}

func TestRemoveSkillNonDirectoryNamesRemedy(t *testing.T) {
	s, cfg := setupStore(t)
	if err := cfg.AddSkill("alpha", "sha256:fixture"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(s.SkillsDir(), "alpha")
	if err := os.WriteFile(entry, []byte("foreign replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureRepo, err := git.PlainOpen(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := fixtureRepo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("fixture: committed non-directory skill entry", &git.CommitOptions{
		Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid"},
	}); err != nil {
		t.Fatal(err)
	}
	txnStarted := false
	_, err = removeSkill(s, nil, "alpha", hooks{afterTxnStart: func() error {
		txnStarted = true
		return nil
	}})
	if err == nil {
		t.Fatal("non-directory store content must block rm")
	}
	for _, want := range []string{entry, "not a directory", "move", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("rm diagnostic %q lacks remedy detail %q", err, want)
		}
	}
	if pending, pendingErr := PendingTxns(s); pendingErr != nil || len(pending) != 0 {
		t.Fatalf("ordinary non-directory refusal must precede WAL creation: pending=%+v err=%v", pending, pendingErr)
	}
	if txnStarted {
		t.Fatal("non-directory refusal reached the post-WAL hook; preflight did not pin the refusal")
	}
}

func TestRemoveOutcomeReportsRecoveryPendingAfterCommitBoundaryFailure(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted after commit")

	outcome, err := removeSkill(s, nil, "alpha", hooks{
		afterCommit: func() error { return interrupted },
	})
	if !errors.Is(err, interrupted) {
		t.Fatalf("remove error = %v, want %v", err, interrupted)
	}
	if !outcome.Operation.Committed || !outcome.Operation.RecoveryPending {
		t.Fatalf("outcome must report a durable removal with pending recovery: %+v", outcome)
	}
	if outcome.Operation.PostCommitComplete || outcome.Operation.WALComplete || outcome.Operation.CanonicalChecked {
		t.Fatalf("phases after the interruption must remain incomplete: %+v", outcome.Operation)
	}
}

func TestRemoveOutcomeReportsWALCompletionFailure(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	poisonCompletion := func() error {
		pending, err := PendingTxns(s)
		if err != nil {
			return err
		}
		if len(pending) != 1 {
			return fmt.Errorf("pending transactions = %d, want 1", len(pending))
		}
		return os.WriteFile(filepath.Join(s.RecoveryDir(), txnCompletionName(pending[0])), []byte("foreign completion"), 0o644)
	}

	outcome, err := removeSkill(s, nil, "alpha", hooks{afterCommit: poisonCompletion})
	if err == nil {
		t.Fatal("foreign completion marker must make WAL completion fail")
	}
	if !outcome.Operation.Committed || !outcome.Operation.PostCommitComplete || outcome.Operation.WALComplete || !outcome.Operation.RecoveryPending {
		t.Fatalf("outcome must stop at WAL completion with recovery pending: %+v", outcome.Operation)
	}
}

func TestRemoveOutcomeTreatsWrittenCommitErrorAsDurable(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	verificationFailed := errors.New("post-write commit verification failed")

	outcome, err := removeSkill(s, nil, "alpha", hooks{
		commit: func(st *store.Store, message string, prepared store.PreparedCommit) (store.CommitOutcome, error) {
			written, err := st.CommitPrepared(message, prepared)
			if err != nil {
				return written, err
			}
			return written, verificationFailed
		},
	})
	if !errors.Is(err, verificationFailed) {
		t.Fatalf("remove error = %v, want %v", err, verificationFailed)
	}
	if !outcome.Operation.Committed || !outcome.Operation.RecoveryPending {
		t.Fatalf("written commit must be reported as durable with pending recovery: %+v", outcome.Operation)
	}
	if outcome.Operation.PostCommitComplete || outcome.Operation.WALComplete || outcome.Operation.CanonicalChecked {
		t.Fatalf("no later phase may be reported complete: %+v", outcome.Operation)
	}
}

// TestRemoveSkillRecoversAfterProcessInterruption crashes an rm at each
// durable boundary and asserts the next write command converges.
func TestRemoveSkillRecoversAfterProcessInterruption(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_RM_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_RM_HOME")
		stage := os.Getenv("FU_TEST_CRASH_RM_STAGE")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		var h hooks
		switch stage {
		case "after-start":
			// Crash between the "started" WAL and the snapshot: the skill's
			// content is still untouched at skills/<name>.
			h.afterTxnStart = crash
		case "after-snapshot":
			h.afterSnapshot = crash
		case "after-quarantine":
			h.afterQuarantine = crash
		case "after-save":
			h.afterSave = crash
		case "after-commit":
			h.afterCommit = crash
		default:
			panic("unknown crash stage " + stage)
		}
		_, _ = removeSkill(s, nil, "alpha", h)
		panic("crash hook did not run")
	}

	for _, stage := range []string{"after-start", "after-snapshot", "after-quarantine", "after-save", "after-commit"} {
		t.Run(stage, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			s, err := store.Init(home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRemoveSkillRecoversAfterProcessInterruption$")
			cmd.Env = append(os.Environ(),
				"FU_TEST_CRASH_RM_HELPER=1",
				"FU_TEST_CRASH_RM_HOME="+home,
				"FU_TEST_CRASH_RM_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child must terminate at %s with code 86, err=%v output=%s", stage, err, output)
			}

			s, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "after-commit" {
				// The rm commit was already written when the process died;
				// any write command recovers and clears the WAL. Trigger
				// recovery with an unrelated write op.
				if _, err := NewSkill(s, nil, "beta"); err != nil {
					t.Fatalf("next write after %s must recover: %v", stage, err)
				}
			} else {
				if _, err := RemoveSkill(s, nil, "alpha"); err != nil {
					t.Fatalf("retry after %s must recover and succeed: %v", stage, err)
				}
			}
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HasSkill("alpha") {
				t.Fatal("retry must complete the removal")
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("successful recovery must clear its WAL, got %+v", pending)
			}
			// An interrupted rm must never leave the skill's content either
			// in the store or stray in staging.
			if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); !os.IsNotExist(err) {
				t.Fatalf("skills/alpha must be gone, err=%v", err)
			}
		})
	}
}

// TestRemoveSkillRecoverySurvivesCrashBeforeWALClear pins recovery's
// resumability across its own last durable boundary under the reclaim design,
// whose invariant is that reclamation runs strictly after the transaction's
// terminal marker and so is never part of a recovery precondition. Reclaiming
// after ClearTxn leaves no archive-then-clear window for a crash to interrupt
// -- finishCommittedRemove only validates before that point, with no durable
// side effect of its own to be interrupted mid-way. If recovery
// dies right before clearing the WAL, the payload is exactly where the
// original rm left it, untouched; the next write must still recover: clear
// the WAL and reclaim the payload in the same pass.
//
// This replaces the former TestRemoveSkillRecoverySurvivesPostArchiveCrash,
// which pinned round 8 finding C1 (recovery idempotent over the archived-but-
// not-yet-cleared state it itself produced). That state no longer exists:
// there is nothing to archive, so nothing sits at a .fu-archive-* name
// anymore.
func TestRemoveSkillRecoverySurvivesCrashBeforeWALClear(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_RM_RECOVERY_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_RM_RECOVERY_HOME")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		// Build the crashed-operation state: rm committed, WAL open. The
		// afterCommit error stops the pipeline before ClearTxn.
		if _, err := NewSkill(s, nil, "alpha"); err != nil {
			panic(err)
		}
		stop := func() error { return errors.New("stop before wal clear") }
		if _, err := removeSkill(s, nil, "alpha", hooks{afterCommit: stop}); err == nil {
			panic("pipeline must stop at afterCommit")
		}
		// Recovery needs the checked write-session roots, exactly like the
		// RecoverPending path production uses.
		session, err := s.BeginWrite()
		if err != nil {
			panic(err)
		}
		checked := session.Store
		// Now run the recovery itself and kill it right before the WAL is
		// cleared.
		pending, err := PendingTxns(checked)
		if err != nil {
			panic(err)
		}
		if len(pending) != 1 {
			panic("expected exactly one pending rm transaction")
		}
		crash := func(st *store.Store, record TxnRecord) error { os.Exit(86); return nil }
		if err := recoverRemoveSkillWithHooks(checked, pending[0], removeRecoveryHooks{beforeWALClear: crash}); err != nil {
			panic(fmt.Sprintf("recovery failed: %v", err))
		}
		panic("recovery crash hook did not run")
	}

	home := filepath.Join(t.TempDir(), "home")
	if _, err := store.Init(home); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemoveSkillRecoverySurvivesCrashBeforeWALClear$")
	cmd.Env = append([]string{}, os.Environ()...)
	cmd.Env = append(cmd.Env,
		"FU_TEST_CRASH_RM_RECOVERY_HELPER=1",
		"FU_TEST_CRASH_RM_RECOVERY_HOME="+home,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate with code 86, err=%v output=%s", err, output)
	}

	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatalf("next write after the pre-clear recovery crash must recover: %v", err)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("recovery must clear its WAL, got %+v", pending)
	}
	// The resumed recovery attempt both clears the WAL and reclaims the
	// payload in the same pass: there is no more archive step to leave it
	// under, and no crash-injection seam between ClearTxn and reclaim.
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "removed-") {
			t.Fatalf("recovered rm payload %q must be reclaimed once the resumed recovery clears the WAL", entry.Name())
		}
	}
}

// TestCommittedRemoveRecoversWithoutItsRecoveryPayload pins the flip side of
// the retired round 8 finding C1 under the reclaim design, whose invariant is
// that reclamation runs strictly after the transaction's terminal marker: the
// payload's presence is no longer a recovery precondition, so an
// already-missing payload -- reclaimed early, or removed by whatever
// means, while a committed rm's WAL was still open -- must not block
// recovery. The state the payload's absence describes is exactly the state
// this operation is trying to reach.
//
// This replaces the former TestCommittedRemoveMissingRecoveryPayloadNamesRemedy,
// which pinned the opposite behavior (a missing payload was a safe conflict
// with a manual journal-family remedy) that finishCommittedRemove's own
// "payload absent from both names is refused" check used to enforce. That
// check is gone by design; recovery no longer reads the payload at all.
func TestCommittedRemoveRecoversWithoutItsRecoveryPayload(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after committed remove")
	if _, err := removeSkill(s, nil, "alpha", hooks{afterCommit: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("remove must stop with an open committed WAL: %v", err)
	}
	pending, err := PendingTxns(s)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending remove = %+v, %v", pending, err)
	}
	record := pending[0]
	payloadPath := filepath.Join(s.RecoveryDir(), rmPayloadName(record))
	if err := os.RemoveAll(payloadPath); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatalf("committed-rm recovery must tolerate an already-missing payload: %v", err)
	}
	pending, err = PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("recovery must clear its WAL, got %+v", pending)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("alpha") {
		t.Fatal("the recovered rm must still have removed alpha")
	}
}

// TestRemoveSkillRollbackRefusesTamperedContent pins round 9 finding I-A:
// the uncommitted rollback must validate the live content against the
// recorded manifest before accepting it. A crash at after-snapshot leaves
// the snapshot recorded with the content still at skills/<name>; external
// tampering of that content must surface as a safe conflict with all
// versions preserved, not be silently absorbed.
func TestRemoveSkillRollbackRefusesTamperedContent(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_RM_TAMPER_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_RM_TAMPER_HOME")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		h := hooks{afterSnapshot: crash}
		_, _ = removeSkill(s, nil, "alpha", h)
		panic("crash hook did not run")
	}

	home := filepath.Join(t.TempDir(), "home")
	s, err := store.Init(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemoveSkillRollbackRefusesTamperedContent$")
	cmd.Env = append([]string{}, os.Environ()...)
	cmd.Env = append(cmd.Env,
		"FU_TEST_CRASH_RM_TAMPER_HELPER=1",
		"FU_TEST_CRASH_RM_TAMPER_HOME="+home,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate with code 86, err=%v output=%s", err, output)
	}

	// Tamper with the still-present content before the next write.
	tampered := "---\nname: alpha\ndescription: tampered\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingTxns(s)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending remove = %+v, %v", pending, err)
	}
	record := pending[0]
	_, err = RemoveSkill(s, nil, "alpha")
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("tampered content must surface as a safe conflict, got %v", err)
	}
	journalPattern := filepath.Join(s.RecoveryDir(), fmt.Sprintf("txn-%s-%s-*.json", record.Op, record.TxnID))
	for _, want := range []string{s.RecoveryDir(), journalPattern, "move", "restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("conflict must name its WAL and manual resolution, error %q lacks %q", err, want)
		}
	}
	if single := txnDisplayPath(s, txnRecordName(record)); strings.Contains(err.Error(), single) {
		t.Fatalf("remedy must name the immutable journal family, not prescribe deleting only %s: %v", single, err)
	}
	// The tampered content is preserved, not deleted or overwritten.
	if got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil || string(got) != tampered {
		t.Fatalf("tampered content must be preserved, got %q err %v", got, err)
	}
}

func TestRemoveSkillReclaimsQuarantinedPayload(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "removed-") {
			t.Fatalf("rm payload %q must be reclaimed once the WAL is cleared", entry.Name())
		}
	}
	// The content stays recoverable from history: reclamation disposes of the
	// second copy, not the first.
	log, err := s.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) < 2 || log[0].Message != "rm: alpha" {
		t.Fatalf("commit history wrong: %+v", log)
	}
}
