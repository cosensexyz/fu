// internal/engine/restore_test.go
package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// TestRestoreRebuildsADeletedLinkWithoutWritingHistory pins both halves of the
// link layer: a hand-deleted link comes back, and restore adds no commit of
// its own doing it. Nothing is pending here, which is what makes HEAD the
// right thing to assert on -- see
// TestRestoreCompletesAPendingInstallAndItsCompensationCommit for the case
// where HEAD moves and must. The deleted link lives outside the store's git
// worktree, so this test alone stays green even if Reconcile started sweeping;
// TestRestoreDoesNotSweepHandEditedStoreContent below is the test that
// actually fails under that mutation.
func TestRestoreRebuildsADeletedLinkWithoutWritingHistory(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alpha")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(s, agents, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("restore must rebuild the deleted link: %v", err)
	}
	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatalf("restore must not produce history: HEAD moved %s -> %s", before.Hash(), after.Hash())
	}
}

// TestRestoreDoesNotSweepHandEditedStoreContent is the guard the brief's own
// test does not actually provide (see the self-review note in the task
// report): TestRestoreRebuildsADeletedLinkWithoutWritingHistory only deletes
// an agent-side symlink, which lives entirely outside the store's git
// worktree, so it stays green even if Reconcile started sweeping -- there is
// nothing dirty in the store for a sweep to find. This test hand-edits
// content inside the store's own worktree, exactly the "未经 fu 的手工改动"
// SPEC §5.3 describes and the kind every other write command's Sweep step
// folds into a standalone "external modifications" commit -- so it is the
// one that actually fails if a sweep is added to Reconcile.
//
// This is the whole of what SPEC §5.3 claims for restore, and all it can
// claim: no sweep, and no commit restore writes on its own account. HEAD is
// asserted on here because nothing is pending in this fixture, not because
// restore keeps HEAD still in general.
func TestRestoreDoesNotSweepHandEditedStoreContent(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	skillFile := filepath.Join(s.SkillsDir(), "alpha", "NOTES.md")
	if err := os.WriteFile(skillFile, []byte("hand-edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty, err := s.IsDirty(); err != nil {
		t.Fatal(err)
	} else if !dirty {
		t.Fatal("setup check: the store must actually be dirty for this test to mean anything")
	}

	if _, err := Restore(s, agents, false); err != nil {
		t.Fatal(err)
	}

	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatalf("restore must not sweep hand-edited store content into history: HEAD moved %s -> %s", before.Hash(), after.Hash())
	}
	if dirty, err := s.IsDirty(); err != nil {
		t.Fatal(err)
	} else if !dirty {
		t.Fatal("restore must leave the hand edit exactly as the user left it, not commit it")
	}
}

// TestRestoreCompletesAPendingInstallAndItsCompensationCommit pins the case
// SPEC §5.3's old wording -- "restore 使现实回归期望、不产生新历史" -- read as
// impossible, and which is in fact restore's job. Reconcile begins with
// RecoverPendingReporting, and rolling back an interrupted, already-committed
// install writes that transaction's own compensation commit (new_txn.go), so
// HEAD moves. It is the exact state `fu status` names as unfinished and tells
// the user a write command will settle; refusing to settle it to keep HEAD
// still would be the bug. The commit belongs to the interrupted transaction,
// not to restore, which is what the corrected claim says.
func TestRestoreCompletesAPendingInstallAndItsCompensationCommit(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	interrupted := errors.New("interrupted after commit")
	outcome := OperationOutcome{}
	if _, err := newSkillTracked(s, agents, "alpha", hooks{
		afterCommit: func() error { return interrupted },
	}, &outcome); !errors.Is(err, interrupted) {
		t.Fatalf("new error = %v, want %v", err, interrupted)
	}
	if !outcome.Committed || !outcome.RecoveryPending {
		t.Fatalf("setup check: the fixture must leave a committed, unrecovered transaction: %+v", outcome)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("setup check: pending transactions = %d, want 1", len(pending))
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(s, agents, false); err != nil {
		t.Fatal(err)
	}

	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() == after.Hash() {
		t.Fatal("restore must settle the pending transaction, and settling this one writes its compensation commit")
	}
	settled, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 0 {
		t.Fatalf("the transaction must be settled, not left pending: %+v", settled)
	}
}

// TestRestoreReportsNoRefusalWhenOnlyTheLinkLayerNeedsRepair guards the
// boundary between Restore's two layers: a hand-deleted agent-side link lives
// entirely outside the store's git worktree, so ChangedPathsIncludingIgnored
// reports nothing to refuse, and Refused must stay at its zero value while
// the link layer is still repaired. hard is passed false because the
// property holds regardless of it -- Restore returns before ever consulting
// hard once dirty is empty -- and false is what this test was named for after
// an earlier round removed --hard entirely (renamed from
// TestRestoreHardIsANoOpWhenOnlyTheLinkLayerNeedsRepair). This round
// reintroduces --hard properly (internal/store/worktree_apply.go); the name
// is kept rather than reverted, since the property pinned here was never
// really about --hard's presence in the first place.
func TestRestoreReportsNoRefusalWhenOnlyTheLinkLayerNeedsRepair(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alpha")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(s, agents, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Refused) != 0 {
		t.Fatalf("nothing in the store worktree was dirty, so restore must not report a refusal, got %v", outcome.Refused)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the link layer must still be repaired: %v", err)
	}
}

// Self-review addition: RestoreOutcome.Result is Restore's only reporting
// channel back to the user (printResult renders it, same as every other
// write command) -- this pins that Reconcile's own findings actually reach
// it unfiltered, rather than merely trusting the three-line passthrough by
// reading it. Foreign content already occupies alpha's path in the agent
// directory before Restore ever runs, so this is a standing conflict
// Reconcile reports on every pass, not a one-shot side effect of setup.
func TestRestoreOutcomeCarriesReconcileConflicts(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	// Occupy alpha's path with real, unmanaged content before the skill is
	// even registered, so NewSkill's own reconcile pass already reports (and
	// leaves standing) the conflict Restore is expected to surface again.
	if err := os.MkdirAll(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(s, agents, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Result.Conflicts) != 1 || outcome.Result.Conflicts[0].Skill != "alpha" || outcome.Result.Conflicts[0].AgentName != "claude" {
		t.Fatalf("Restore must surface Reconcile's Conflicts through RestoreOutcome.Result, got %+v", outcome.Result)
	}
}

// TestRestoreRefusesToDiscardUncommittedWork pins the borrowed git semantics
// of the default, hard=false path: the user runs plain restore to repair
// links, and discarding uncommitted store content is a side effect they did
// not ask for -- the class git refuses on (checkout, merge), not the class it
// discards on (reset --hard). Refusing is what hard=false does in place of the
// discard hard=true opts into (TestRestoreHardResetsTheStoreWorktree pins that
// side). The link layer must still be repaired regardless -- that is what was
// asked for -- and the refusal itself must be a pure report: the blocking
// path is named, the edit survives byte-for-byte, and HEAD does not move.
func TestRestoreRefusesToDiscardUncommittedWork(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")
	want := []byte("---\nname: alpha\ndescription: edited by hand\n---\n")
	if err := os.WriteFile(edited, want, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alpha")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(s, agents, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Refused) != 1 || outcome.Refused[0] != "skills/alpha/SKILL.md" {
		t.Fatalf("the uncommitted edit must be named as the sole blocking path, got %v", outcome.Refused)
	}
	got, err := os.ReadFile(edited)
	if err != nil || string(got) != string(want) {
		t.Fatalf("the edit must survive untouched, restore must not write to the store worktree: %q err=%v", got, err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the link layer must still be repaired: %v", err)
	}
	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatalf("restore must not move HEAD: %s -> %s", before.Hash(), after.Hash())
	}
}

// TestRestoreHardResetsTheStoreWorktree pins the second layer. Without --hard
// the command still refuses and reports; with it, tracked paths come back and
// untracked content is still left alone.
//
// Self-review fix: the brief's own fixture called setupStore(t, "alpha"),
// which only os.MkdirAll's an empty skills/alpha directory and writes fu.yaml
// straight to disk (Config.Save is a plain atomic file write, never a commit
// -- see DESIGN §6) -- it commits nothing, so skills/alpha/SKILL.md never
// existed and the very first os.ReadFile below failed with ENOENT, not the
// assertion this test means to make. Every sibling test in this file
// (TestRestoreDoesNotSweepHandEditedStoreContent,
// TestRestoreRefusesToDiscardUncommittedWork, ...) uses the pattern restored
// here instead -- an empty setupStore(t) plus a real NewSkill -- specifically
// because NewSkill runs the full commit pipeline and actually leaves a
// tracked SKILL.md at HEAD for a later hand edit to diverge from. agents is
// nil here and at both Restore calls below: this test's assertions never
// inspect link state, only store.Store.SkillsDir() content, so there is
// nothing for a real agent to do.
func TestRestoreHardResetsTheStoreWorktree(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")
	original, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(s.SkillsDir(), "alpha", "scratch.md")
	if err := os.WriteFile(scratch, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	refused, err := Restore(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(refused.Refused) == 0 {
		t.Fatal("without --hard the store worktree must still be refused, not reset")
	}

	outcome, err := Restore(s, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Reset) == 0 {
		t.Fatal("--hard must report the paths it reset")
	}
	got, err := os.ReadFile(skillFile)
	if err != nil || string(got) != string(original) {
		t.Fatalf("--hard must reset the tracked edit: %q %v", got, err)
	}
	survived, err := os.ReadFile(scratch)
	if err != nil || string(survived) != "mine" {
		t.Fatalf("--hard must leave untracked content alone: %q %v", survived, err)
	}
}

// TestRestoreHardReconcilesAgainstTheWorktreeItLeavesBehind pins the same
// invariant TestRevertRebuildsLinksFromThePostRevertConfig pins for revert,
// in the other command it fails in: the link layer must reflect the config
// this command actually leaves on disk.
//
// Restore runs the link layer first on purpose -- repairing links is what the
// user asked for, and reporting on the store worktree must not cost them that
// (restore.go's own comment). The ordering is right; what was missing was the
// second pass. With --hard the store worktree is finalised *after* the link
// layer has already run, so the reconcile ran against a config the very next
// step discarded.
//
// Both halves below are the same defect seen from two directions, and both
// end with the command having produced new drift while claiming to have
// removed it.
func TestRestoreHardReconcilesAgainstTheWorktreeItLeavesBehind(t *testing.T) {
	t.Run("a discarded config edit must not leave the link layer following it", func(t *testing.T) {
		s, _ := setupStore(t)
		dir := t.TempDir()
		agents := []agent.Agent{fakeAgent{"claude", dir}}
		if _, err := NewSkill(s, agents, "alpha"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil {
			t.Fatalf("precondition: new must have linked alpha: %v", err)
		}
		// A hand edit to fu.yaml, uncommitted -- exactly what --hard exists to
		// throw away.
		cfg, err := store.LoadConfig(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		cfg.SetEnabled("alpha", false)
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}

		outcome, err := Restore(s, agents, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(outcome.Reset) == 0 {
			t.Fatalf("precondition: --hard must have discarded the fu.yaml edit, reset %v", outcome.Reset)
		}
		reloaded, err := store.LoadConfig(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		if !reloaded.Enabled("alpha") {
			t.Fatal("precondition: the discarded edit must be gone from fu.yaml")
		}
		// The command whose whole purpose is removing drift must not finish by
		// having created some.
		if _, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil {
			t.Fatalf("the link must follow the config restore left behind, not the edit it discarded: %v", err)
		}
	})

	t.Run("one run must repair both layers when store content was deleted", func(t *testing.T) {
		s, _ := setupStore(t)
		dir := t.TempDir()
		agents := []agent.Agent{fakeAgent{"claude", dir}}
		if _, err := NewSkill(s, agents, "alpha"); err != nil {
			t.Fatal(err)
		}
		// SPEC §3 scenario 5 and rule 6 both describe this as one repair, and
		// SPEC §10's acceptance criterion is that `fu restore` repairs it
		// completely. With the link layer running before the content came
		// back, the first run deleted the (then genuinely broken) link and the
		// user had to run the command a second time.
		if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
			t.Fatal(err)
		}

		if _, err := Restore(s, agents, true); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
			t.Fatalf("precondition: --hard must have restored the deleted store content: %v", err)
		}
		if _, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil {
			t.Fatalf("a single restore must repair both layers, link still missing: %v", err)
		}
	})
}

// TestRestoreHardDoesNotMoveHEAD pins for the hard path what
// TestRestoreRefusesToDiscardUncommittedWork already pins for the default one.
// It is the same property and the more important half: --hard is the
// destructive branch, and if it ever started folding the content it discards
// into a commit of its own it would violate SPEC §5.3's "restore produces no
// commit of its own" while looking, from the worktree, like it had worked.
func TestRestoreHardDoesNotMoveHEAD(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(s, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Reset) == 0 {
		t.Fatal("precondition: --hard must have discarded the edit")
	}

	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatalf("--hard must discard, never commit: HEAD moved %s -> %s", before.Hash(), after.Hash())
	}
}

// TestRestoreReportsUntrackedContentApartFromWhatHardCanDiscard is the engine
// half of the same guard. The CLI tests only prove the printer displays two
// hand-built slices differently; nothing asserted that the engine ever puts an
// untracked path in Left rather than Refused -- so degrading the split left
// every test green while restoring the loop it was added to break.
func TestRestoreReportsUntrackedContentApartFromWhatHardCanDiscard(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "scratch.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Refused) != 1 || outcome.Refused[0] != "skills/alpha/SKILL.md" {
		t.Fatalf("only the tracked edit is within --hard's reach, got %v", outcome.Refused)
	}
	if len(outcome.Left) != 1 || outcome.Left[0] != "skills/alpha/scratch.md" {
		t.Fatalf("the untracked path must be reported apart from it, got %v", outcome.Left)
	}
}

// TestRevertHoldsTheLockForItsDestructiveHalf pins the lock acquisition added
// to RevertOperations in the previous review round. Replacing it with a bare
// call left every test in engine, cli and store green, so the defect it closed
// -- revert mutating the store while another fu process writes it -- could
// return silently.
//
// The shape is TestStatusTakesNoLock's, inverted: hold fu.lock in the
// foreground and require the command to *block* rather than proceed. A command
// that took no lock would return promptly and fail here. Restore is not in
// this table because contention cannot isolate its second acquisition -- it
// would block on Reconcile's first one either way; see
// TestRestoreHardTakesTheLockASecondTimeForTheReset.
//
// A goroutine plus a short deadline is the whole mechanism: reaching the
// deadline means the command is still waiting for the lock, which is the
// property being asserted. The lock is released afterwards so the call can
// finish and its session close cleanly.
func TestRevertHoldsTheLockForItsDestructiveHalf(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*store.Store) error
	}{
		{"revert", func(s *store.Store) error {
			_, err := RevertOperations(s, nil, 1)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupStore(t)
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatal(err)
			}
			if _, err := NewSkill(s, nil, "beta"); err != nil {
				t.Fatal(err)
			}
			// Dirty, so restore --hard has real work and cannot return early
			// on a clean worktree before ever reaching the lock.
			if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("hand edited"), 0o644); err != nil {
				t.Fatal(err)
			}

			fl := flock.New(s.LockPath())
			locked, err := fl.TryLock()
			if err != nil {
				t.Fatal(err)
			}
			if !locked {
				t.Fatal("precondition: the test must hold fu.lock")
			}

			done := make(chan error, 1)
			go func() { done <- tc.run(s) }()
			select {
			case err := <-done:
				fl.Unlock()
				t.Fatalf("the destructive half must wait for fu.lock, but the call finished while it was held: %v", err)
			case <-time.After(2 * time.Second):
			}

			if err := fl.Unlock(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the call must complete once the lock is free: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the call did not complete after the lock was released")
			}
		})
	}
}

// TestRestoreHardTakesTheLockASecondTimeForTheReset is the Restore half of the
// guard above, which contention cannot express: Reconcile takes fu.lock for
// the link layer and releases it, so a foreground holder blocks that first
// acquisition whether or not the destructive half has one of its own.
//
// Counting acquisitions states the property directly. --hard must take the
// lock twice: once inside Reconcile, once around the dirty read and the reset.
// Deleting the second acquisition -- the mutation this closes -- takes the
// count to one.
func TestRestoreHardTakesTheLockASecondTimeForTheReset(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	acquired := 0
	lockAcquiredHook = func(string) { acquired++ }
	t.Cleanup(func() { lockAcquiredHook = nil })

	outcome, err := Restore(s, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Reset) == 0 {
		t.Fatal("precondition: --hard must have had work to do")
	}
	if acquired != 2 {
		t.Fatalf("--hard must hold fu.lock for the reset as well as the link layer, acquisitions = %d, want 2", acquired)
	}
}

// TestRestoreWithoutHardDoesNotTakeTheLockForItsReport is the complement: the
// default path only names paths and never acts on them, so it deliberately
// reads the store worktree unlocked rather than making a report wait behind a
// long write. Pinning it keeps the exemption from quietly widening to the
// destructive path.
func TestRestoreWithoutHardDoesNotTakeTheLockForItsReport(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	acquired := 0
	lockAcquiredHook = func(string) { acquired++ }
	t.Cleanup(func() { lockAcquiredHook = nil })

	outcome, err := Restore(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Refused) == 0 {
		t.Fatal("precondition: the report must have found the hand edit")
	}
	if acquired != 1 {
		t.Fatalf("the report path takes only Reconcile's lock, acquisitions = %d, want 1", acquired)
	}
}

// TestRestoreHardDoesNotReportWhatItJustFixed pins the result contract of the
// two reconcile passes. The first pass runs before the store worktree is
// final, so on the SPEC §10 flagship case -- store content deleted by hand,
// link broken, `--hard` -- its findings describe a state the second pass then
// resolves. Appending them left the command reporting a problem three lines
// above the output saying it had fixed it:
//
//	missing: claude/alpha is enabled but the store no longer holds its content
//	reset 1 path(s) in the store worktree to the last commit:
//	  skills/alpha/SKILL.md
//	restored agent links
//
// Result.UserReports's de-duplication cannot help: these are not duplicates,
// they are findings the second pass has already resolved.
func TestRestoreHardDoesNotReportWhatItJustFixed(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(s, agents, true)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Readlink(filepath.Join(dir, "alpha")); err != nil {
		t.Fatalf("precondition: the run must have rebuilt the link: %v", err)
	}
	if len(outcome.Result.Missing) != 0 {
		t.Fatalf("the second pass resolved this; reporting it describes a state that no longer exists: %+v", outcome.Result.Missing)
	}
	if !outcome.Result.Empty() {
		t.Fatalf("a run that fixed everything must report nothing: %+v", outcome.Result)
	}
}

// TestRestoreStillRunsTheStoreLayerWhenOneAgentFails pins the converse of the
// ordering argument restore.go already makes. That argument says a report on
// the store's own worktree must not cost the user their link repair; what
// shipped was its inverse -- any agent scan failure made Reconcile return
// ErrOperationFailed, and Restore returned immediately, so a single broken
// agent directory silently cancelled the entire store worktree layer,
// including a `--hard` the user had explicitly asked for.
//
// Reconcile already isolates per-agent failures rather than aborting, so the
// error belongs at the end, not in place of the second layer.
func TestRestoreStillRunsTheStoreLayerWhenOneAgentFails(t *testing.T) {
	s, _ := setupStore(t)
	// A skills "directory" that is really a file: ScanAgent fails on it, which
	// Reconcile records in Failed and reports as ErrOperationFailed.
	broken := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(broken, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", broken}}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")
	if err := os.WriteFile(edited, []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, restoreErr := Restore(s, agents, true)
	if restoreErr == nil {
		t.Fatal("the agent failure must still be reported")
	}
	if len(outcome.Result.Failed) == 0 {
		t.Fatalf("the failing agent must be named: %+v", outcome.Result)
	}
	// The half the user asked for must have run regardless.
	if len(outcome.Reset) != 1 || outcome.Reset[0] != "skills/alpha/SKILL.md" {
		t.Fatalf("--hard must still discard the tracked edit, reset = %v", outcome.Reset)
	}
	got, err := os.ReadFile(edited)
	if err != nil || string(got) == "hand edited" {
		t.Fatalf("the tracked edit must have been discarded: %q %v", got, err)
	}
	// Both passes fail here -- the reset moved a path, so the second reconcile
	// runs and hits the same broken agent -- and joining two copies of one
	// sentinel made exitcode.go print the identical sentence twice under a
	// single "error:" prefix. Result is deduplicated by being replaced; the
	// error has to be deduplicated too.
	if n := strings.Count(restoreErr.Error(), ErrOperationFailed.Error()); n != 1 {
		t.Fatalf("the same failure must be reported once, not %d times: %v", n, restoreErr)
	}
}

// TestCarryWarningsForwardKeepsTheFirstPassFindings pins the merge `fu restore
// --hard` performs between its two reconcile passes (review round 27 finding
// 3). The second pass's Result replaces the first's outright -- deliberately,
// since the first ran before the store worktree was final -- and this merge is
// the single exception carried across it.
//
// Tested at this level because production cannot reach the call site: the only
// Warnings any recovery handler produces sit behind preconditions that force
// the reset to move nothing, and a run that moves nothing returns before the
// merge. See carryWarningsForward's own comment. What can be pinned is the
// contract, so that a future handler emitting a Warning on a path the reset
// reaches does not have it silently dropped.
func TestCarryWarningsForwardKeepsTheFirstPassFindings(t *testing.T) {
	got := carryWarningsForward([]string{"first-a", "first-b"}, []string{"second"})
	want := []string{"first-a", "first-b", "second"}
	if len(got) != len(want) {
		t.Fatalf("both passes' warnings must survive: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the first pass's warnings must come first, in order: got %v, want %v", got, want)
		}
	}

	// The merge must build a new slice rather than append onto the first pass's
	// own array. At the call site that array belongs to outcome.Result.Warnings,
	// which is being read at the same moment -- and a plain
	// append(first, second...) writes into it whenever it has spare capacity,
	// which a slice built by earlier appends usually does.
	spare := make([]string, 2, 8)
	spare[0], spare[1] = "first-a", "first-b"
	merged := carryWarningsForward(spare, []string{"second"})
	merged[0] = "overwritten"
	if spare[0] != "first-a" || spare[1] != "first-b" || len(spare) != 2 {
		t.Fatalf("the merge must not write through to the first pass's slice, got %v", spare)
	}
}
