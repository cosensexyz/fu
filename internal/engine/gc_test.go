// internal/engine/gc_test.go
package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// crashRemoveAfterTxnClearedEnv, set to "1" in the environment, switches
// crashRemoveAfterTxnClearedChild from a no-op into the child-process half of
// a re-exec crash-injection test.
const crashRemoveAfterTxnClearedEnv = "FU_TEST_CRASH_GC_RM_HELPER"

// crashRemoveAfterTxnClearedChild runs an rm transaction to completion and
// kills the process at the entry of afterTxnCleared: operation committed,
// WAL cleared, journal family complete but unpruned, payload orphaned at its
// quarantine name. It is the child-process half of a re-exec crash-injection
// test (the pattern TestRemoveSkillRecoversAfterProcessInterruption uses in
// rm_test.go); callers spawn os.Args[0] with -test.run=^<TestName>$ and
// crashRemoveAfterTxnClearedEnv=1 in the environment. It is a no-op unless
// that env var is set, so every test using runCrashedRemove calls it
// unconditionally at its own entry without affecting a normal (parent-
// process) run.
func crashRemoveAfterTxnClearedChild() {
	if os.Getenv(crashRemoveAfterTxnClearedEnv) != "1" {
		return
	}
	home := os.Getenv("FU_TEST_CRASH_GC_RM_HOME")
	s, err := store.Open(home)
	if err != nil {
		panic(err)
	}
	crash := func() error { os.Exit(86); return nil }
	_, _ = removeSkill(s, nil, "alpha", hooks{beforeReclaim: crash})
	panic("crash hook did not run")
}

// runCrashedRemove spawns the re-exec child for testName, which must call
// crashRemoveAfterTxnClearedChild() at its own entry, and returns the home
// directory left in the post-crash state once the child terminates via the
// crash hook's os.Exit(86). The "new" transaction that scaffolds "alpha" is
// pruned before the crash so the only completed-but-unpruned family left
// behind is the crashed rm, keeping the caller's outcome assertions about
// that one family exact.
func runCrashedRemove(t *testing.T, testName string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	s, err := store.Init(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneCompletedTransactions(s); err != nil {
		t.Fatal(err)
	}
	spawnCrashedRemove(t, testName, home, crashRemoveAfterTxnClearedEnv)
	return home
}

// spawnCrashedRemove re-execs testName in a child process with crashEnv set,
// pointed at an existing home, and requires the child to die through its
// injected crash hook (os.Exit(86)) rather than finish or fail some other way.
func spawnCrashedRemove(t *testing.T, testName, home, crashEnv string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(),
		crashEnv+"=1",
		"FU_TEST_CRASH_GC_RM_HOME="+home,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate at the injected crash point with code 86, err=%v output=%s", err, output)
	}
}

// crashRemoveAtQuarantineEnv, set to "1" in the environment, switches
// crashRemoveAtQuarantineChild from a no-op into the child-process half of a
// re-exec crash-injection test.
const crashRemoveAtQuarantineEnv = "FU_TEST_CRASH_GC_RM_QUARANTINE"

// crashRemoveAtQuarantineChild runs an rm against an existing home and kills
// the process once the content is quarantined and that stage is durable. What
// it leaves behind is a *pending* rm: no terminal marker, the skill's content
// parked at removed-<name>-<StartHead>, and the only manifest that may ever
// restore or delete that content sitting in the pending family's revisions.
// Like crashRemoveAfterTxnClearedChild it is a no-op unless its env var is
// set, so callers invoke it unconditionally at their own entry.
func crashRemoveAtQuarantineChild() {
	if os.Getenv(crashRemoveAtQuarantineEnv) != "1" {
		return
	}
	home := os.Getenv("FU_TEST_CRASH_GC_RM_HOME")
	s, err := store.Open(home)
	if err != nil {
		panic(err)
	}
	crash := func() error { os.Exit(86); return nil }
	_, _ = removeSkill(s, nil, "alpha", hooks{afterQuarantine: crash})
	panic("crash hook did not run")
}

// orphanedRemovePayload finds the single removed-* recovery payload left by
// runCrashedRemove and fails the test if none is present.
func orphanedRemovePayload(t *testing.T, s *store.Store) string {
	t.Helper()
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "removed-") {
			if found != "" {
				t.Fatalf("more than one orphaned payload: %s and %s", found, entry.Name())
			}
			found = entry.Name()
		}
	}
	if found == "" {
		t.Fatal("crash must leave the quarantined payload orphaned")
	}
	return filepath.Join(s.RecoveryDir(), found)
}

// TestPruneReclaimsOrphanRemovePayloadBeforePruningItsJournal pins the crash
// window Task 3 closes: if the process dies exactly where rm's own
// afterTxnCleared would reclaim its quarantined payload -- after the
// transaction's terminal marker, before the inline reclaim -- the payload is
// orphaned and its manifest is only readable from the still-unpruned journal
// family. `fu gc` must reclaim that payload by the recorded manifest before
// it prunes the family carrying it: prune the journal first and the manifest
// needed to verify the payload's identity is gone for good.
func TestPruneReclaimsOrphanRemovePayloadBeforePruningItsJournal(t *testing.T) {
	crashRemoveAfterTxnClearedChild()

	home := runCrashedRemove(t, "TestPruneReclaimsOrphanRemovePayloadBeforePruningItsJournal")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// The transaction is complete, not pending: nothing left for ordinary
	// recovery to do with it.
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a crash after ClearTxn must leave no pending transaction, got %+v", pending)
	}
	payload := orphanedRemovePayload(t, s)

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("prune must reclaim the orphan and succeed: %v", err)
	}
	if outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, want the one completed rm family pruned", outcome)
	}
	if _, err := os.Lstat(payload); !os.IsNotExist(err) {
		t.Fatalf("gc must reclaim the orphan payload, err=%v", err)
	}
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "txn-rm-") {
			t.Fatalf("prune must remove the completed rm journal family once its payload is reclaimed, found %q", entry.Name())
		}
	}
}

// TestPruneDoesNotPruneFamilyWhenPayloadReclaimFails pins the other half of
// the contract: reclaim failure must not be logged and carried on from. If
// gc cannot verify and remove the orphaned payload, it must leave the
// family's journal alone -- pruning it anyway would destroy the only copy of
// the manifest the payload can ever again be verified and deleted by,
// stranding the content permanently. The failure is produced honestly: the
// payload is mutated on disk after the crash, exactly the real-world case
// (partial disk corruption, a stray write) the skip-and-retry exists to
// protect against.
func TestPruneDoesNotPruneFamilyWhenPayloadReclaimFails(t *testing.T) {
	crashRemoveAfterTxnClearedChild()

	home := runCrashedRemove(t, "TestPruneDoesNotPruneFamilyWhenPayloadReclaimFails")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	payload := orphanedRemovePayload(t, s)

	// Mutate the payload so it no longer matches its recorded manifest.
	skillFile := filepath.Join(payload, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("tampered after crash"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A second, unrelated completed family sits alongside the broken one, so
	// the assertions below can tell "this one family is skipped" apart from
	// "the whole gc run gave up".
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err == nil {
		t.Fatal("prune must report the reclamation failure rather than silently pruning the family")
	}
	if !errors.Is(err, store.ErrOwnedTreeChanged) {
		t.Fatalf("prune error = %v, want it to wrap store.ErrOwnedTreeChanged", err)
	}
	// The remedy must name the one path the user has to act on -- the payload
	// -- and must not send them at the journal family. That family is not what
	// broke, and moving it aside, which is what the journal-family remedy
	// prescribes, deletes the only manifest this payload can ever be verified
	// and removed by: the permanent stranding this whole branch exists to
	// prevent.
	if !strings.Contains(err.Error(), payload) {
		t.Fatalf("reclaim-failure remedy %q does not name the payload path %q", err, payload)
	}
	for _, forbidden := range []string{
		"move the complete transaction family",
		"damaged completed transaction",
		"prune records matching",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("reclaim-failure remedy %q reuses the journal-family remedy phrase %q", err, forbidden)
		}
	}
	if outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, want only the unrelated healthy family pruned", outcome)
	}
	// The mutated payload is preserved untouched, not partially deleted.
	if got, err := os.ReadFile(skillFile); err != nil || string(got) != "tampered after crash" {
		t.Fatalf("mismatched payload must be preserved as-is, got=%q err=%v", got, err)
	}
	// The rm family's revisions survive too, so the manifest is still there
	// for the next gc attempt to retry against, while the healthy "new"
	// family for beta is gone -- one family's failure must not stop gc from
	// finishing the others.
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var sawRevision, sawCompletion bool
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.Name(), "txn-rm-") && strings.HasSuffix(entry.Name(), ".json"):
			sawRevision = true
		case strings.HasPrefix(entry.Name(), "txn-rm-") && strings.HasSuffix(entry.Name(), ".done"):
			sawCompletion = true
		case strings.HasPrefix(entry.Name(), "txn-new-"):
			t.Fatalf("the unrelated healthy family must still be pruned despite the rm family's failure, found %q", entry.Name())
		}
	}
	if !sawRevision || !sawCompletion {
		t.Fatalf("prune must leave the family's revisions and completion marker in place, sawRevision=%v sawCompletion=%v", sawRevision, sawCompletion)
	}
}

// TestPruneKeepsPayloadClaimedByPendingTransaction pins the identity limit of
// the reclaim gc runs before pruning. `removed-<name>-<StartHead>` names what
// an object is, not which transaction owns it: two rm transactions of the same
// skill at the same HEAD derive the same payload name, and every hop
// (quarantine, rollback restore, re-quarantine) is a rename, so device, inode
// and content survive all of them. A completed family's manifest therefore
// matches a *pending* family's live payload byte for byte, and matching it is
// not ownership.
//
// The sequence below builds exactly that: a rolled-back rm leaves a completed
// family whose manifest still describes content that its own rollback put back
// under skills/, and a second rm then crashes with that same content
// quarantined under the shared name. gc sees only the completed family --
// PruneRecovery deliberately never runs recovery, and the prune loop only
// visits completed families -- so nothing in its own view distinguishes the
// two. Reclaiming there deletes the pending transaction's only content and
// wedges the store: every later write fails at the recovery boundary with
// "uncommitted rm transaction lost its content".
func TestPruneKeepsPayloadClaimedByPendingTransaction(t *testing.T) {
	crashRemoveAtQuarantineChild()

	home := filepath.Join(t.TempDir(), "home")
	s, err := store.Init(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	// Prune the scaffolding "new" family so the completed families gc sees are
	// exactly the rm ones this test is about.
	if _, err := PruneCompletedTransactions(s); err != nil {
		t.Fatal(err)
	}

	// Family A: an rm that fails at quarantine. The pipeline rolls it back
	// inline -- the payload returns to skills/alpha and the WAL is cleared --
	// so A ends up completed, unpruned, and still carrying a payload manifest.
	// No commit was written, so HEAD has not moved.
	quarantineFailed := errors.New("injected quarantine failure")
	if _, err := removeSkill(s, nil, "alpha", hooks{
		afterQuarantine: func() error { return quarantineFailed },
	}); !errors.Is(err, quarantineFailed) {
		t.Fatalf("setup rm must fail at quarantine and roll back, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatalf("the rolled-back rm must put alpha's content back: %v", err)
	}

	// Family B: the same rm again, crashing after the quarantine is durable.
	spawnCrashedRemove(t, "TestPruneKeepsPayloadClaimedByPendingTransaction", home, crashRemoveAtQuarantineEnv)

	s, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Op != "rm" {
		t.Fatalf("the crash must leave exactly one pending rm transaction, got %+v", pending)
	}
	payload := orphanedRemovePayload(t, s)
	if got, want := filepath.Base(payload), rmPayloadName(pending[0]); got != want {
		t.Fatalf("quarantined payload %q is not the pending transaction's payload %q", got, want)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("gc must not fail over a payload that is not its to collect: %v", err)
	}
	if _, statErr := os.Lstat(payload); statErr != nil {
		t.Fatalf("gc deleted the pending rm transaction's payload %s: %v (outcome=%+v)", payload, statErr, outcome)
	}
	// Skipping the reclaim does not block the prune. The completed family has
	// no remaining claim of its own -- its rollback already put its content
	// back under skills/ -- and the object at the shared name stays provable
	// from the pending family's manifest, which gc never prunes.
	if outcome.Transactions != 1 {
		t.Fatalf("gc outcome = %+v, want the one completed rm family pruned", outcome)
	}
	pendingAfter, err := PendingTxns(s)
	if err != nil {
		t.Fatalf("gc must leave the pending transaction readable: %v", err)
	}
	if len(pendingAfter) != 1 {
		t.Fatalf("gc changed the pending transaction set: %+v", pendingAfter)
	}
	// The store still takes writes: the next command recovers the pending rm,
	// which rolls back by restoring the payload gc left alone.
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatalf("gc wedged the store at the recovery boundary: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatalf("the recovered rollback must restore alpha's content: %v", err)
	}
}

// TestPruneSweepsStrandedConfigExchangeBookkeeping pins the last collector in
// recovery/. Config exchange bookkeeping is not described by any transaction
// journal, so the per-family prune loop can never reach it: what a crash
// strands between an exchange's durable terminal marker and its own inline
// reclamation is collectable only by prefix, and only gc looks. Every write
// command performs an exchange, so leaving it uncollected is what made
// recovery/ outgrow the store it protects.
func TestPruneSweepsStrandedConfigExchangeBookkeeping(t *testing.T) {
	s, _ := setupStore(t)
	stranded := filepath.Join(s.RecoveryDir(), ".fu-config-exchange-"+strings.Repeat("ab", 8)+".done")
	if err := os.WriteFile(stranded, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stranded); !os.IsNotExist(err) {
		t.Fatalf("gc must collect the stranded config exchange marker, err=%v", err)
	}
	if outcome.Files != 1 {
		t.Fatalf("prune outcome = %+v, want the one collected marker counted", outcome)
	}
}

// TestPruneReportsTheJournalScanRemedyOnce pins one gc run to one copy of the
// journal-scan remedy. A run reads the journal itself and then reads the
// pending set to learn which payload names are claimed, and that second read
// rescans the same directory -- so one malformed txn-* filename fails both.
// Wrapping each failure in the remedy printed the identical multi-line
// instruction twice for a single broken name.
func TestPruneReportsTheJournalScanRemedyOnce(t *testing.T) {
	s, _ := setupStore(t)
	malformed := filepath.Join(s.RecoveryDir(), "txn-rm-not-a-record.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := PruneCompletedTransactions(s)
	if err == nil {
		t.Fatal("a malformed journal filename must be reported")
	}
	if !strings.Contains(err.Error(), filepath.Base(malformed)) {
		t.Fatalf("prune error %q does not name the malformed file", err)
	}
	const remedy = "preserve the affected journal files under"
	if got := strings.Count(err.Error(), remedy); got != 1 {
		t.Fatalf("journal scan remedy appears %d times in %q, want exactly once", got, err)
	}
}

// TestPruneKeepsProblemsAccumulatedBeforeAHardFailure pins what a run owes the
// caller when a write stops it midway. Problems collected before that point --
// the config exchange sweep's failure, and every damaged family named so far --
// are unrelated to whatever stopped the run, and are reported by no one else.
// Returning only the stopping error drops them silently.
func TestPruneKeepsProblemsAccumulatedBeforeAHardFailure(t *testing.T) {
	s, _ := setupStore(t)
	// One healthy completed family for the run to reach the hook on, written
	// before the journal is damaged: the write path scans the journal too.
	record := &TxnRecord{Op: "prune-remedy", Name: "alpha", Stage: "committed"}
	if err := WriteTxn(s, record); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(s, *record); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(s.RecoveryDir(), "txn-rm-not-a-record.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	stop := errors.New("stop after prune marker")
	_, err := pruneCompletedTransactions(s, pruneHooks{afterMarker: func() error { return stop }})
	if !errors.Is(err, stop) {
		t.Fatalf("prune error = %v, want the injected failure", err)
	}
	if !strings.Contains(err.Error(), filepath.Base(malformed)) {
		t.Fatalf("prune error %q dropped the problem accumulated before the failure", err)
	}
}

// TestPruneSettlesAFamilyWhoseVanishedPayloadNeedsNoPendingSet pins the one
// ownership question that can be answered without the pending set. When the
// claims read fails, no name under the recovery directory can be shown to be
// unclaimed, so a family whose payload is still present has to wait for a run
// that can read it. A family whose payload is already gone waits for nothing:
// there is no object left for any transaction to claim, so ownership cannot be
// in question. Skipping those too let one malformed journal filename pin every
// rm family ever settled -- which is nearly all of them, because the inline
// reclaim collects the payload the moment its transaction completes.
func TestPruneSettlesAFamilyWhoseVanishedPayloadNeedsNoPendingSet(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	// Deliberately not an rm name: the assertion below globs the rm family's
	// own journal files, and this fixture must not be mistaken for one.
	malformed := filepath.Join(s.RecoveryDir(), "txn-adopt-not-a-record.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := PruneCompletedTransactions(s)
	if err == nil {
		t.Fatal("a malformed journal filename must still be reported")
	}
	remaining, globErr := filepath.Glob(filepath.Join(s.RecoveryDir(), "txn-rm-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(remaining) != 0 {
		t.Fatalf("an already-settled rm family must prune even when the pending set is unreadable, left %v (err=%v)", remaining, err)
	}
}
