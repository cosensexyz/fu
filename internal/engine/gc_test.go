// internal/engine/gc_test.go
package engine

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
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

// crashUpdateAfterTxnClearedEnv, set to "1" in the environment, switches
// crashUpdateAfterTxnClearedChild from a no-op into the child-process half of
// a re-exec crash-injection test.
const crashUpdateAfterTxnClearedEnv = "FU_TEST_CRASH_GC_UPDATE"

// crashUpdateAfterTxnClearedChild runs a content-shape update to completion
// and kills the process at the entry of afterTxnCleared: the exchange has
// happened, the operation is committed, the WAL is cleared, the journal
// family is complete but unpruned, and the tree the update replaced is
// orphaned at staging/<name>. It is update's counterpart of
// crashRemoveAfterTxnClearedChild above and works the same way -- a no-op
// unless its env var is set, so callers invoke it unconditionally at their
// own entry.
//
// It drives updateSkill directly rather than Application.UpdateSkills: the
// crash point is inside the operation, and a local source keeps the child
// free of any network dependency.
func crashUpdateAfterTxnClearedChild() {
	if os.Getenv(crashUpdateAfterTxnClearedEnv) != "1" {
		return
	}
	home := os.Getenv("FU_TEST_CRASH_GC_UPDATE_HOME")
	srcDir := os.Getenv("FU_TEST_CRASH_GC_UPDATE_SRC")
	s, err := store.Open(home)
	if err != nil {
		panic(err)
	}
	src, err := source.ParseArg(srcDir)
	if err != nil {
		panic(err)
	}
	scratch, err := os.MkdirTemp("", "fu-crash-update")
	if err != nil {
		panic(err)
	}
	p, err := src.Prepare(scratch)
	if err != nil {
		panic(err)
	}
	root, err := p.Root()
	if err != nil {
		panic(err)
	}
	proj, err := skill.ProjectDir(root.FS(), ".")
	if err != nil {
		panic(err)
	}
	digest, err := skill.DigestManifest(proj)
	if err != nil {
		panic(err)
	}
	crash := func() error { os.Exit(86); return nil }
	_, _ = updateSkill(s, nil, p, "alpha",
		Candidate{Name: "alpha", Subdir: ".", Digest: digest},
		localSourceRecord(srcDir), localSourceRecord(srcDir), false,
		hooks{beforeUpdateReclaim: crash})
	panic("crash hook did not run")
}

// runCrashedUpdate seeds a store holding "alpha" installed from a local
// source, moves that source ahead so the update takes the content shape
// (the exchange), and spawns the re-exec child for testName, which must call
// crashUpdateAfterTxnClearedChild() at its own entry. It returns the home
// directory left in the post-crash state.
func runCrashedUpdate(t *testing.T, testName string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	s, err := store.Init(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	srcDir, _ := installedFromLocal(t, s, cfg, "alpha", "version one")
	writeSkillBody(t, srcDir, "alpha", "version two")

	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(),
		crashUpdateAfterTxnClearedEnv+"=1",
		"FU_TEST_CRASH_GC_UPDATE_HOME="+home,
		"FU_TEST_CRASH_GC_UPDATE_SRC="+srcDir,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate at the injected crash point with code 86, err=%v output=%s", err, output)
	}
	return home
}

// countTxnFamilyFiles counts the journal files a transaction family left in
// the recovery directory, so an assertion about what gc removed does not have
// to hard-code how many revisions the operation happens to write.
func countTxnFamilyFiles(t *testing.T, s *store.Store, prefix string) int {
	t.Helper()
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			count++
		}
	}
	return count
}

// TestPruneReclaimsOrphanUpdateStagingPayloadBeforePruningItsJournal is the
// update-side mirror of TestPruneReclaimsOrphanRemovePayloadBeforePruningItsJournal
// above, and design §7's acceptance for this window stated as a measurement:
// "崩溃在 afterTxnCleared 后，fu gc 能把 staging 孤立载荷收干净". A crash inside
// afterTxnCleared leaves the tree an update replaced orphaned at
// staging/<name>, with its journal family complete but the inline reclaim
// (update.go's reclaimExchangedUpdatePayload) never having run. `fu gc` must
// reclaim that tree by the recorded PreviousPayload manifest before it prunes
// the family carrying it: prune the journal first and the manifest needed to
// verify the tree's identity is gone for good.
//
// Fix round 2, Important #9: this used to build the post-crash state by hand
// -- a TxnRecord carrying only Op, Name, Stage and PreviousPayload -- because
// update had no crash seam at that point. That proved gc's arm handles the
// shape, but not that a real crash there produces a record the arm accepts,
// nor that afterTxnCleared is wired to the reclaim at all. It now runs the
// real operation and kills the real process, through the beforeUpdateReclaim
// hook added for exactly this (pipeline.go), the way rm's own fixtures have
// always done it.
func TestPruneReclaimsOrphanUpdateStagingPayloadBeforePruningItsJournal(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestPruneReclaimsOrphanUpdateStagingPayloadBeforePruningItsJournal")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// The transaction is complete, not pending: nothing left for ordinary
	// recovery to do with it, so the orphan is gc's alone to collect.
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a crash after ClearTxn must leave no pending transaction, got %+v", pending)
	}
	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	replaced, err := os.ReadFile(filepath.Join(stagingPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("the crash must leave the replaced tree at staging/alpha: %v", err)
	}
	if !strings.Contains(string(replaced), "version one") {
		t.Fatalf("staging must hold the tree the update replaced:\n%s", replaced)
	}
	familyFiles := countTxnFamilyFiles(t, s, "txn-update-")
	if familyFiles == 0 {
		t.Fatal("the crashed update must leave its completed journal family behind")
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("prune must reclaim the orphan and succeed: %v", err)
	}
	if outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, want the one completed update family pruned", outcome)
	}
	// Exactly the family's own journal files, and nothing for the reclaimed
	// tree: Files counts files under the recovery directory, and a tree
	// removed whole against its manifest is neither -- the rm side's own
	// payload is not tallied either (PruneOutcome, txn_prune.go).
	if outcome.Files != familyFiles {
		t.Fatalf("prune outcome = %+v, want %d journal files and no file credit for a reclaimed tree", outcome, familyFiles)
	}
	if _, err := os.Lstat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("gc must reclaim the orphaned staging tree, err=%v", err)
	}
	if left := countTxnFamilyFiles(t, s, "txn-update-"); left != 0 {
		t.Fatalf("prune must remove the completed update journal family once its staging residue is reclaimed, %d files left", left)
	}
}

// TestPruneSucceedsWhenTheUpdateStagingTreeIsAlreadyReclaimed pins the
// ordinary, uncrashed path: update's own inline reclaim
// (reclaimExchangedUpdatePayload, update.go) already removed staging/<name>
// synchronously, in the same process, long before gc ever runs. gc's own
// reclaim attempt is then a no-op -- store.RemoveOwnedTreeAt returns nil, not
// an error, when nothing is at either the live name or the deterministic
// retired sibling its own protocol can leave it parked at (retire.go; the rule
// is stated for the recovery side by ReclaimRecoveryPayloadOwned's "An already
// absent payload is not an error", ownedtree.go) -- and it must neither fail nor
// stop the family being pruned. This is not a rare edge case: PreviousPayload
// is set unconditionally and early by every completed content-shape update
// (update.go) and nothing ever clears it, so this is the ordinary shape of
// nearly every completed update family gc ever prunes.
func TestPruneSucceedsWhenTheUpdateStagingTreeIsAlreadyReclaimed(t *testing.T) {
	s, _ := setupStore(t)
	checked := checkedRecoveryStore(t, s)

	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "replaced content")
	previousPayload, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary path: the tree update's own reclaim left is already gone
	// by the time gc gets here.
	if err := os.RemoveAll(stagingPath); err != nil {
		t.Fatal(err)
	}

	record := &TxnRecord{
		Op:              "update",
		Name:            "alpha",
		Stage:           "published",
		PreviousPayload: &previousPayload,
	}
	if err := WriteTxn(checked, record); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *record); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("prune must succeed over an already-reclaimed staging tree: %v", err)
	}
	if outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, want the one completed update family pruned", outcome)
	}
	// The revision and completion marker, the same two a family with a tree
	// still to reclaim reports: neither state credits the tree itself.
	if outcome.Files != 2 {
		t.Fatalf("prune outcome = %+v, want 1 revision + 1 completion marker", outcome)
	}
}

// TestPruneKeepsAStagingEntryThatDoesNotMatchTheRecordedManifest is the
// update-side mirror of TestPruneDoesNotPruneFamilyWhenPayloadReclaimFails
// above. A staging entry that does not match the family's recorded
// PreviousPayload is not proven to be the tree that family replaced, so gc
// must leave it alone -- and must leave the family's journal alone too,
// since pruning it would destroy the only manifest that could ever again
// verify or reclaim the entry.
func TestPruneKeepsAStagingEntryThatDoesNotMatchTheRecordedManifest(t *testing.T) {
	s, _ := setupStore(t)
	checked := checkedRecoveryStore(t, s)

	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "replaced content")
	previousPayload, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}

	record := &TxnRecord{
		Op:              "update",
		Name:            "alpha",
		Stage:           "published",
		PreviousPayload: &previousPayload,
	}
	if err := WriteTxn(checked, record); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *record); err != nil {
		t.Fatal(err)
	}

	// Mutate the staging entry so it no longer matches the recorded
	// PreviousPayload manifest -- the honest way to produce this failure (a
	// stray write, partial disk corruption), the same technique
	// TestPruneDoesNotPruneFamilyWhenPayloadReclaimFails uses for the rm side.
	skillFile := filepath.Join(stagingPath, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("tampered after the fact"), 0o644); err != nil {
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
		t.Fatal("prune must report the mismatch rather than silently pruning the family")
	}
	if !errors.Is(err, store.ErrOwnedTreeChanged) {
		t.Fatalf("prune error = %v, want it to wrap store.ErrOwnedTreeChanged", err)
	}
	if !strings.Contains(err.Error(), stagingPath) {
		t.Fatalf("mismatch remedy %q does not name the staging path %q", err, stagingPath)
	}
	for _, forbidden := range []string{
		"move the complete transaction family",
		"damaged completed transaction",
		"prune records matching",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("mismatch remedy %q reuses the journal-family remedy phrase %q", err, forbidden)
		}
	}
	if outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, want only the unrelated healthy family pruned", outcome)
	}
	// The mismatched entry is preserved untouched, not partially deleted.
	if got, err := os.ReadFile(skillFile); err != nil || string(got) != "tampered after the fact" {
		t.Fatalf("mismatched staging entry must be preserved as-is, got=%q err=%v", got, err)
	}
	// The update family's revisions survive too, so the manifest is still
	// there for the next gc attempt to retry against, while the healthy "new"
	// family for beta is gone -- one family's failure must not stop gc from
	// finishing the others.
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var sawRevision, sawCompletion bool
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.Name(), "txn-update-") && strings.HasSuffix(entry.Name(), ".json"):
			sawRevision = true
		case strings.HasPrefix(entry.Name(), "txn-update-") && strings.HasSuffix(entry.Name(), ".done"):
			sawCompletion = true
		case strings.HasPrefix(entry.Name(), "txn-new-"):
			t.Fatalf("the unrelated healthy family must still be pruned despite the update family's failure, found %q", entry.Name())
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
// orphanedRemoveManifest returns the ownership manifest the crashed rm
// family's still-unpruned journal carries: the same record `fu gc` reclaims
// the orphaned payload by, and the one thing that makes an interrupted
// disposal resumable.
func orphanedRemoveManifest(t *testing.T, s *store.Store) store.OwnedTree {
	t.Helper()
	journal, err := scanTxnJournalReport(s)
	if err != nil {
		t.Fatal(err)
	}
	for key := range journal.completed {
		if key.op != "rm" {
			continue
		}
		latest, err := validateTxnChain(s, key, journal.revisions[key])
		if err != nil {
			t.Fatal(err)
		}
		if latest.Payload == nil {
			t.Fatal("the completed rm family must carry its payload manifest")
		}
		return *latest.Payload
	}
	t.Fatal("no completed rm family left by the crash")
	return store.OwnedTree{}
}

// TestPruneKeepsFamilyWhosePayloadIsParkedAtItsRetiredRootName pins the other
// half of the "vanished payload needs no pending set" rule below. Disposal is
// not one syscall: RemoveOwnedTreeAt empties the tree, renames the root to a
// sibling derived from the manifest, and only then unlinks it, so a crash
// between the last two steps frees the live payload name while the emptied root
// remains. Judging that state by the live name alone answers "settled" and
// prunes the family -- destroying the one manifest RemoveOwnedTreeAt could have
// resumed from, after which nothing collects the retired root while `fu status`
// goes on counting it collectable (status.go).
func TestPruneKeepsFamilyWhosePayloadIsParkedAtItsRetiredRootName(t *testing.T) {
	crashRemoveAfterTxnClearedChild()

	home := runCrashedRemove(t, "TestPruneKeepsFamilyWhosePayloadIsParkedAtItsRetiredRootName")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	payload := orphanedRemovePayload(t, s)
	manifest := orphanedRemoveManifest(t, s)

	// Reproduce the interrupted disposal through the same two steps
	// RemoveOwnedTreeAt performs in that order, so the fixture cannot drift
	// from the protocol it stands for.
	payloadDir, err := os.Open(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveOwnedContents(payloadDir, manifest); err != nil {
		_ = payloadDir.Close()
		t.Fatal(err)
	}
	if err := payloadDir.Close(); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(s.RecoveryDir(),
		store.RetiredRecoveryRootName(filepath.Base(payload), manifest))
	if err := os.Rename(payload, retired); err != nil {
		t.Fatal(err)
	}

	// A malformed pending journal name makes the claims read fail, which is the
	// branch that consults the payload directly instead of the pending set.
	malformed := filepath.Join(s.RecoveryDir(), "txn-adopt-not-a-record.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneCompletedTransactions(s); err == nil {
		t.Fatal("a malformed journal filename must still be reported")
	}
	remaining, globErr := filepath.Glob(filepath.Join(s.RecoveryDir(), "txn-rm-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(remaining) == 0 {
		t.Fatal("pruning the family strands the retired root: its manifest was the only way to resume the disposal")
	}
	if _, err := os.Lstat(retired); err != nil {
		t.Fatalf("the retired root must be left in place for a later resume: %v", err)
	}
}

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

// TestPruneFinishesADisposalParkedAtItsRetiredRootName is the positive half of
// TestPruneKeepsFamilyWhosePayloadIsParkedAtItsRetiredRootName above. That one
// asserts gc leaves the retired root and its family alone; then it ends. Why
// leaving them is the right answer -- that a later gc, with the manifest still
// in place, actually finishes the disposal -- was never asserted anywhere.
//
// The gap was specific: retire_test.go covers resuming from an inner retired
// leaf and an inner retired directory, and the failure branch for a retired
// root, but not RemoveOwnedTreeAt's `!livePresent && retiredPresent` success
// branch -- which is the entire reason RecoveryPayloadSettled checks two names
// instead of one. Without it, a regression in finishRetiredOwnedDirectory
// would strand the retired root forever while `fu status` kept counting it
// collectable: exactly the failure that check exists to prevent.
//
// Same fixture as its negative twin, minus the malformed journal name, so this
// is the ordinary run that follows a repaired one.
func TestPruneFinishesADisposalParkedAtItsRetiredRootName(t *testing.T) {
	crashRemoveAfterTxnClearedChild()

	home := runCrashedRemove(t, "TestPruneFinishesADisposalParkedAtItsRetiredRootName")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	payload := orphanedRemovePayload(t, s)
	manifest := orphanedRemoveManifest(t, s)

	// The interrupted disposal, reproduced through the same two steps
	// RemoveOwnedTreeAt performs in that order.
	payloadDir, err := os.Open(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveOwnedContents(payloadDir, manifest); err != nil {
		_ = payloadDir.Close()
		t.Fatal(err)
	}
	if err := payloadDir.Close(); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(s.RecoveryDir(),
		store.RetiredRecoveryRootName(filepath.Base(payload), manifest))
	if err := os.Rename(payload, retired); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("gc must resume the interrupted disposal, not report a problem: %v", err)
	}
	if outcome.Transactions == 0 {
		t.Fatalf("the family must be pruned once its payload is disposed of: %+v", outcome)
	}
	if _, err := os.Lstat(retired); !os.IsNotExist(err) {
		t.Fatalf("the retired root must be collected, not left forever, stat err=%v", err)
	}
	if _, err := os.Lstat(payload); !os.IsNotExist(err) {
		t.Fatalf("the live payload name must stay gone, stat err=%v", err)
	}
	remaining, globErr := filepath.Glob(filepath.Join(s.RecoveryDir(), "txn-rm-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(remaining) != 0 {
		t.Fatalf("the family's journal must be pruned once the disposal completed: %v", remaining)
	}
}

// TestPruneKeepsAStagingNameClaimedByPendingTransaction is the update-side
// counterpart of TestPruneKeepsPayloadClaimedByPendingTransaction above, and it
// pins the same identity limit on the other name gc reclaims by.
//
// staging/<name> is the bare skill name. It says what a tree is called, never
// which transaction owns it, so a completed update family's PreviousPayload
// naming that name is not a claim on whatever happens to be sitting there now.
// A pending transaction's own staged content lands on exactly the same name --
// legitimately, because that transaction's preflight found the name free, which
// is only possible once the completed family's own residue was already gone.
//
// The sequence below builds that: an earlier update of alpha completed and its
// inline reclaim succeeded, leaving a completed, unpruned family that still
// carries PreviousPayload; a second update of the same skill then staged its
// replacement at staging/alpha and died before the exchange. gc sees only the
// completed family -- PruneRecovery deliberately never runs recovery -- so
// nothing in its own view distinguishes the two names.
//
// Touching that tree is what must not happen. It is the only on-disk copy the
// pending update's rollback can act on, and gc's own remedy for a tree it
// cannot verify tells the user to move it out of staging -- which would leave
// recovery failing forever and every later write command blocked at the
// recovery gate.
func TestPruneKeepsAStagingNameClaimedByPendingTransaction(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	// The completed family: the tree that earlier update replaced. Its own
	// inline reclaim (reclaimExchangedUpdatePayload, update.go) succeeded, so
	// this family owns nothing under staging any more.
	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "the tree an earlier update replaced")
	settled, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stagingPath); err != nil {
		t.Fatal(err)
	}
	completed := &TxnRecord{Op: "update", Name: "alpha", Stage: "published", PreviousPayload: &settled}
	if err := WriteTxn(checked, completed); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *completed); err != nil {
		t.Fatal(err)
	}

	// The pending family: a second update of the same skill, stopped after it
	// staged its replacement and before the exchange.
	writeSkillBody(t, stagingPath, "alpha", "the replacement a pending update staged")
	staged, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	published, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	pending := &TxnRecord{
		Op: "update", Name: "alpha", Stage: updateTxnStaged,
		Payload: &staged, PreviousPayload: &published,
	}
	if err := WriteTxn(checked, pending); err != nil {
		t.Fatal(err)
	}

	stagedFile := filepath.Join(stagingPath, "SKILL.md")
	before, err := os.ReadFile(stagedFile)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("gc must not fail over a staging name that is not its to collect: %v", err)
	}
	if got, err := os.ReadFile(stagedFile); err != nil || string(got) != string(before) {
		t.Fatalf("gc touched the pending transaction's staged tree: got=%q err=%v (outcome=%+v)", got, err, outcome)
	}
	// Skipping the reclaim does not block the prune: the completed family has
	// nothing left of its own at this name -- the pending transaction's own
	// preflight could not have started otherwise -- so pruning it, judged on
	// its own merits, is still right.
	if outcome.Transactions != 1 {
		t.Fatalf("gc outcome = %+v, want the one completed update family pruned", outcome)
	}
	pendingAfter, err := PendingTxns(s)
	if err != nil {
		t.Fatalf("gc must leave the pending transaction readable: %v", err)
	}
	if len(pendingAfter) != 1 || pendingAfter[0].Name != "alpha" {
		t.Fatalf("gc changed the pending transaction set: %+v", pendingAfter)
	}
}

// TestPruneRefusesAForeignStagingEntryWithoutAdvisingItsRemoval covers the
// other half the claims check cannot answer: a staging name no pending record
// claims, holding something that is not the tree the completed family
// recorded. A user's own directory is the ordinary case -- staging/<name> is
// the bare skill name, so it is a name a person can land on by hand -- and
// `fu new`, `fu add` and `fu update` all refuse to run against it and name the
// path when they do.
//
// gc still refuses rather than pruning: the mismatch may equally be fu's own
// orphan with a byte damaged, and the family's revisions carry the only
// manifest that could ever verify or reclaim it. What must not survive is the
// old remedy's advice to "move that directory out of staging to abandon the
// copy", offered over content fu has just proved is not its own.
func TestPruneRefusesAForeignStagingEntryWithoutAdvisingItsRemoval(t *testing.T) {
	s, _ := setupStore(t)
	checked := checkedRecoveryStore(t, s)

	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "the tree the update replaced")
	previousPayload, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stagingPath); err != nil {
		t.Fatal(err)
	}
	record := &TxnRecord{Op: "update", Name: "alpha", Stage: "published", PreviousPayload: &previousPayload}
	if err := WriteTxn(checked, record); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *record); err != nil {
		t.Fatal(err)
	}
	// Somebody else's directory on the same name, with its own identity.
	if err := os.MkdirAll(stagingPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingPath, "notes.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = PruneCompletedTransactions(s)
	if err == nil {
		t.Fatal("prune must report content it cannot verify rather than pruning the manifest that describes it")
	}
	if !strings.Contains(err.Error(), stagingPath) {
		t.Fatalf("remedy %q does not name the staging path %q", err, stagingPath)
	}
	if !strings.Contains(err.Error(), "is not the tree this transaction recorded") {
		t.Fatalf("remedy %q does not say the entry is not fu's own tree", err)
	}
	if strings.Contains(err.Error(), "abandon the copy") {
		t.Fatalf("remedy %q still advises abandoning content fu has proved is not its own", err)
	}
	if got, err := os.ReadFile(filepath.Join(stagingPath, "notes.md")); err != nil || string(got) != "mine" {
		t.Fatalf("the foreign entry must be preserved exactly: got=%q err=%v", got, err)
	}
}

// TestPruneHonoursAStagingClaimFromAnOpThatCannotStage pins the cost of
// deciding a claimed staging name by the claim alone, so that the cost is a
// recorded limitation rather than something a later reader discovers.
//
// `fu rm` stages nothing -- its preflight reads fu.yaml and skills/<name> and
// never looks at staging (checkRemoveAvailable, checkRemoveStoreEntry, rm.go)
// -- yet pendingStagingClaims claims record.Name from every pending record
// whatever its op, deliberately, on the asymmetry that claiming a name too many
// only skips a deletion. So an update whose inline reclaim did not complete,
// followed by an rm of the same skill that died mid-transaction, leaves this
// family's own orphan under a name the rm claims: gc leaves the tree and prunes
// the manifest, after which nothing can verify or collect it and `fu status`
// files it under Unmatched.
//
// The alternative -- asking whether the object still carries the identity this
// family recorded, and reclaiming when it does -- is what
// TestPruneKeepsATreeAPendingUpdateNeedsWhoseIdentityMatchesAnOlderFamily shows
// to be destructive: the exchange preserves inodes, so an older family's
// manifest matches a pending update's live tree exactly. This limitation is a
// double fault whose content survives in the store's git history (SPEC rule 3);
// that one wedges the store. Changing this behaviour means answering that test
// first.
func TestPruneHonoursAStagingClaimFromAnOpThatCannotStage(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	// The completed update family, with its inline reclaim never finished: the
	// tree it replaced is still at the live staging name.
	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "the tree the update replaced")
	previousPayload, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	completed := &TxnRecord{Op: "update", Name: "alpha", Stage: "published", PreviousPayload: &previousPayload}
	if err := WriteTxn(checked, completed); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *completed); err != nil {
		t.Fatal(err)
	}

	// The claimant: an rm of the same skill, stopped mid-transaction. It has
	// nothing under staging and never could have.
	pending := &TxnRecord{Op: "rm", Name: "alpha", Stage: "started"}
	if err := WriteTxn(checked, pending); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("prune must not fail over a claimed name: %v", err)
	}
	// The claim is honoured: the tree is left exactly where it is.
	if _, err := os.Lstat(stagingPath); err != nil {
		t.Fatalf("gc must not touch a claimed staging name: %v", err)
	}
	if outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, want the one completed update family pruned", outcome)
	}
	pendingAfter, err := PendingTxns(s)
	if err != nil {
		t.Fatalf("gc must leave the pending transaction readable: %v", err)
	}
	if len(pendingAfter) != 1 || pendingAfter[0].Op != "rm" {
		t.Fatalf("gc changed the pending transaction set: %+v", pendingAfter)
	}
}

// TestPruneKeepsATreeAPendingUpdateNeedsWhoseIdentityMatchesAnOlderFamily is
// the staging-side proof of the rule DESIGN §2 already states for recovery
// payloads: a name plus a manifest that matches it is not ownership. It is the
// regression guard against deciding a claimed staging name by identity.
//
// The exchange is a rename, so inodes survive it, and that is what makes an
// older family's manifest match a newer transaction's live tree exactly --
// identity, mode and content alike:
//
//  1. An update of alpha rolls back. restoreExchangedUpdate exchanges the
//     replaced tree back to skills/alpha and ClearTxn completes the family, so
//     a completed, unpruned family is left whose PreviousPayload names the
//     inode of the tree now published at skills/alpha.
//  2. The next update of alpha exchanges that very inode out to staging/alpha,
//     and is interrupted between the exchange and its commit.
//  3. staging/alpha now holds the one on-disk copy that update's own
//     restoreExchangedUpdate has to swap back -- and it matches the older
//     family's PreviousPayload down to the byte, because it is the same object.
//
// Reclaiming it there leaves recovery unable to finish: ValidateSkillOwned
// fails for PreviousPayload and succeeds for Payload, so recovery exchanges
// back, and ExchangeStagedWithSkillOwned validates against a staging name gc
// has deleted. The transaction can never reach a terminal state and every
// write command stays blocked at its recovery prologue.
//
// One ordinary rolled-back update -- a designed-for outcome -- plus one
// interrupted update, plus a `fu gc` in between, which is the very command
// checkUpdateAvailable tells the user to run when staging is occupied.
// TestPruneStaysQuietWhenALaterFamilyCollectsTheStagingTree pins round 4's
// finding that two completed update families on one name could make `fu gc`
// report a failure it then resolved in the same run.
//
// Families are visited in (op, id) order and id is random, so the family whose
// manifest no longer describes the tree may be visited first: its
// RemoveOwnedTreeAt fails the manifest preflight, and gc used to exit 1 naming
// staging/alpha and telling the user to "restore it to its recorded content"
// -- about an entry the very next family in the same run removes. The two
// transaction IDs are pinned here so the order under test is the failing one
// every run, not half of them.
//
// The traced sequence, which is entirely made of designed-for outcomes:
//
//  1. `fu update alpha` rolls back, leaving a completed family whose
//     PreviousPayload names skills/alpha's inode holding content one.
//  2. The user hand-edits skills/alpha. The directory inode is unchanged, so
//     the older manifest still matches by identity and no longer by content.
//  3. `fu update alpha --force` completes but its inline reclaim is
//     interrupted, so the exchange has moved that same inode out to
//     staging/alpha and a second completed family names it there.
func TestPruneStaysQuietWhenALaterFamilyCollectsTheStagingTree(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	skillPath := filepath.Join(s.SkillsDir(), "alpha")
	writeSkillBody(t, skillPath, "alpha", "content one")
	older, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	olderFamily := &TxnRecord{
		Op: "update", TxnID: strings.Repeat("0", 32), Name: "alpha",
		Stage: "published", PreviousPayload: &older,
	}
	if err := WriteTxn(checked, olderFamily); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *olderFamily); err != nil {
		t.Fatal(err)
	}

	// The hand edit: same directory, new content, so the older manifest now
	// matches by identity and fails on content.
	writeSkillBody(t, skillPath, "alpha", "content two")

	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "the replacement")
	staged, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	published, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if published.RootIdentity != older.RootIdentity {
		t.Fatal("fixture is not exercising the case: the hand edit changed the directory identity")
	}
	if err := checked.ExchangeStagedWithSkillOwned("alpha", staged, published); err != nil {
		t.Fatal(err)
	}
	newerFamily := &TxnRecord{
		Op: "update", TxnID: strings.Repeat("f", 32), Name: "alpha",
		Stage: updateTxnExchanged, Payload: &staged, PreviousPayload: &published,
	}
	if err := WriteTxn(checked, newerFamily); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *newerFamily); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("gc reported a failure it resolved in the same run: %v (outcome=%+v)", err, outcome)
	}
	if _, statErr := os.Lstat(stagingPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the newer family must still collect its own tree: %v", statErr)
	}
}

// A reclaim that never reached the filesystem is not the failure the deferral
// above exists to suppress, and releasing on "both names are clear" alone drops
// it. reclaimUpdateStagingPayload refuses a Name that is not a public skill
// name before it opens staging at all (update.go), and RemoveOwnedTreeAt
// refuses an invalid manifest the same way (retire.go); neither means
// "something is sitting on this name", so both leave the two candidate names
// clear and updateStagingPayloadSettled duly answers "settled". Dropped, the
// family is skipped on every future run while `fu gc` prints "nothing to prune"
// and exits 0 and `fu status` counts the same files collectable forever --
// exactly the "run a command and watch a count not move" incoherence this whole
// change exists to end, and the state PruneOutcome's doc says cannot happen.
//
// Only ErrOwnedTreeChanged -- what compareOwnedTreeCleanupState raises when the
// tree is real but no longer the recorded one -- can be resolved by a later
// family collecting it, which is why that class alone may be released quietly.
func TestPruneReportsAReclaimRefusedBeforeItReachedTheTree(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	writeSkillBody(t, filepath.Join(s.SkillsDir(), "alpha"), "alpha", "content one")
	tree, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing on the prune path validates Name: decodeTxnFile runs no
	// validateUpdateRecord, so gc reaches the reclaim with this field exactly
	// as written. That is the reachability the reclaim's own guard was added
	// for in round 2.
	family := &TxnRecord{
		Op: "update", TxnID: strings.Repeat("0", 32), Name: ".fu-not-a-skill-name",
		Stage: "published", PreviousPayload: &tree,
	}
	if err := WriteTxn(checked, family); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *family); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err == nil {
		t.Fatalf("a reclaim refused before it reached the tree must be reported, not dropped (outcome=%+v)", outcome)
	}
	if !strings.Contains(err.Error(), "not a public skill name") {
		t.Fatalf("the reported failure must name the refusal that happened: %v", err)
	}
	if outcome.Transactions != 0 {
		t.Fatalf("the family must still be skipped, not pruned: %+v", outcome)
	}
}

// The same guard on the other arm, which is the only way to reach the settled
// probe at all: gc asks updateStagingPayloadSettled only when the pending set
// could not be read, and the Name it asks about comes off a completed family's
// record that nothing on the prune path validates. Unguarded, the probe's two
// Lstats land wherever that field points, find nothing, and answer "settled" --
// and the prune then deletes the manifest on the strength of that answer. The
// reclaim beside it has had this guard since round 2; the probe was left
// without one.
func TestPruneRefusesToProbeStagingUnderANameThatIsNotASkillName(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	writeSkillBody(t, filepath.Join(s.SkillsDir(), "alpha"), "alpha", "content one")
	tree, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	family := &TxnRecord{
		Op: "update", TxnID: strings.Repeat("1", 32), Name: ".fu-not-a-skill-name",
		Stage: "published", PreviousPayload: &tree,
	}
	if err := WriteTxn(checked, family); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *family); err != nil {
		t.Fatal(err)
	}
	// One hand-dropped file the pending read cannot parse, which is what routes
	// the family to the claimErr arm rather than to the reclaim above.
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), "txn-broken.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err == nil {
		t.Fatalf("a probe refused before it reached the tree must be reported (outcome=%+v)", outcome)
	}
	if !strings.Contains(err.Error(), "not a public skill name") {
		t.Fatalf("the reported failure must name the refusal that happened: %v", err)
	}
	if outcome.Transactions != 0 {
		t.Fatalf("a family whose staging name could not even be probed must not be pruned: %+v", outcome)
	}
}

// A held staging failure is released by abort() as well as by the normal exit,
// and only the normal exit was tested: deleting the release from abort left the
// suite green. The state needs two families, because the release is deferred
// precisely so a later family can be shown to have collected the tree -- so the
// failure has to be held while a *second* family is still being processed, and
// that second one has to be what stops the run.
//
// What the release buys is the run's whole point here: the entry on
// staging/alpha is not fu's, fu cannot remove it, and this remedy is the only
// way the user learns that. Dropped on the abort path, `fu gc` reports the
// injected failure alone and the family is skipped silently on every run after.
func TestPruneReleasesAHeldStagingFailureWhenTheRunAborts(t *testing.T) {
	s, _ := setupStore(t)
	checked := checkedRecoveryStore(t, s)

	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "replaced content")
	manifest, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}

	// Held: the entry no longer matches this manifest and is still sitting
	// there, so no later family can account for it and the failure must survive.
	held := &TxnRecord{
		Op: "update", TxnID: strings.Repeat("3", 32), Name: "alpha",
		Stage: "published", PreviousPayload: &manifest,
	}
	// Aborting: nothing is at staging/beta, so its reclaim is a no-op success
	// and the run reaches the prune record, where the hook stops it. Ordered
	// second by TxnID, which is what puts it after the held family (families are
	// ordered by op then id, and both of these are updates).
	aborting := &TxnRecord{
		Op: "update", TxnID: strings.Repeat("4", 32), Name: "beta",
		Stage: "published", PreviousPayload: &manifest,
	}
	for _, record := range []*TxnRecord{held, aborting} {
		if err := WriteTxn(checked, record); err != nil {
			t.Fatal(err)
		}
		if err := ClearTxn(checked, *record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stagingPath, "SKILL.md"), []byte("tampered after the fact"), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := errors.New("stop after prune marker")
	outcome, err := pruneCompletedTransactions(s, pruneHooks{afterMarker: func() error { return stop }})
	if !errors.Is(err, stop) {
		t.Fatalf("prune error = %v, want the injected failure", err)
	}
	if !strings.Contains(err.Error(), stagingPath) {
		t.Fatalf("prune error %q dropped the staging failure held before the abort", err)
	}
	if outcome.Transactions != 0 {
		t.Fatalf("the run stopped at the prune record, so nothing is pruned: %+v", outcome)
	}
}

// The second class releaseStagingPayloadProblems' comment names, and the one
// its test coverage was missing: a manifest RemoveOwnedTreeAt refuses outright.
// Both existing tests use an unusable Name, which now fails at the settled
// probe instead (settledErr != nil), so the clause that decides *this* class --
// `!errors.Is(held.err, ErrOwnedTreeChanged)` -- survived deletion. Without it
// the refusal is dropped as if a later family had collected the tree, and `fu
// gc` exits 0 having pruned nothing and said nothing, while the family is
// skipped on every future run.
//
// The name is a real public skill name here, so nothing upstream of the reclaim
// refuses; the manifest is what is wrong, and staging is clear, so the probe
// answers "settled" and only that clause keeps the failure.
func TestPruneReportsAReclaimRefusedByAnUnusableManifest(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	writeSkillBody(t, filepath.Join(s.SkillsDir(), "alpha"), "alpha", "content one")
	tree, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// A root recorded as something other than a directory: OwnedTree.Validate
	// refuses it (ownedtree.go), which is the first thing RemoveOwnedTreeAt
	// does. RootIdentity stays valid, so the settled probe still works and
	// still finds both candidate names clear.
	tree.RootMode = 0o644
	family := &TxnRecord{
		Op: "update", TxnID: strings.Repeat("2", 32), Name: "alpha",
		Stage: "published", PreviousPayload: &tree,
	}
	if err := WriteTxn(checked, family); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *family); err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err == nil {
		t.Fatalf("a reclaim refused by the manifest itself must be reported, not dropped (outcome=%+v)", outcome)
	}
	if !strings.Contains(err.Error(), "invalid manifest") {
		t.Fatalf("the reported failure must name the refusal that happened: %v", err)
	}
	if outcome.Transactions != 0 {
		t.Fatalf("the family must still be skipped, not pruned: %+v", outcome)
	}
}

// Two completed update families can name one staging name (the name is the bare
// skill name, and families are independent), and when the entry there matches
// neither manifest both fail and both release. The remedy is a long, multi-line
// instruction about a single directory, so emitting it once per family told the
// user to move the same entry aside twice. Deduplicated by name, with the
// distinct causes joined rather than dropped -- which family failed is still
// information, it just does not need its own copy of the instructions.
func TestPruneReportsOneRemedyPerStagingNameNotPerFamily(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	writeSkillBody(t, filepath.Join(s.SkillsDir(), "alpha"), "alpha", "content one")
	manifest, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// The user's own directory, sitting on the staging name: it matches neither
	// family's manifest, so both reclaims fail and neither is settled.
	writeSkillBody(t, filepath.Join(s.StagingDir(), "alpha"), "alpha", "mine")

	for _, id := range []string{strings.Repeat("0", 32), strings.Repeat("f", 32)} {
		family := &TxnRecord{
			Op: "update", TxnID: id, Name: "alpha",
			Stage: "published", PreviousPayload: &manifest,
		}
		if err := WriteTxn(checked, family); err != nil {
			t.Fatal(err)
		}
		if err := ClearTxn(checked, *family); err != nil {
			t.Fatal(err)
		}
	}

	_, err = PruneCompletedTransactions(s)
	if err == nil {
		t.Fatal("an entry matching no manifest must be reported")
	}
	if got := strings.Count(err.Error(), "kept the transaction journal intact"); got != 1 {
		t.Fatalf("the remedy for one staging name must be printed once, got %d copies:\n%v", got, err)
	}
}

func TestPruneKeepsATreeAPendingUpdateNeedsWhoseIdentityMatchesAnOlderFamily(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	checked := checkedRecoveryStore(t, s)

	// The tree a rolled-back update put back at skills/alpha, and the completed
	// family that rollback left behind naming it.
	skillPath := filepath.Join(s.SkillsDir(), "alpha")
	writeSkillBody(t, skillPath, "alpha", "the published tree")
	rolledBack, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	completed := &TxnRecord{Op: "update", Name: "alpha", Stage: "published", PreviousPayload: &rolledBack}
	if err := WriteTxn(checked, completed); err != nil {
		t.Fatal(err)
	}
	if err := ClearTxn(checked, *completed); err != nil {
		t.Fatal(err)
	}

	// The next update stages its replacement and performs the exchange, then
	// stops before its commit. The exchange is the real one, so the inode the
	// completed family recorded is now the inode under staging/alpha.
	stagingPath := filepath.Join(s.StagingDir(), "alpha")
	writeSkillBody(t, stagingPath, "alpha", "the replacement being staged")
	staged, err := checked.SnapshotStagedPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	published, err := checked.SnapshotSkillPayload("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if published.RootIdentity != rolledBack.RootIdentity {
		t.Fatalf("fixture is not exercising the case: the published tree changed identity")
	}
	if err := checked.ExchangeStagedWithSkillOwned("alpha", staged, published); err != nil {
		t.Fatal(err)
	}
	pending := &TxnRecord{
		Op: "update", Name: "alpha", Stage: updateTxnExchanged,
		Payload: &staged, PreviousPayload: &published,
	}
	if err := WriteTxn(checked, pending); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(filepath.Join(stagingPath, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatalf("prune must not fail here: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(stagingPath, "SKILL.md")); err != nil || string(got) != string(before) {
		t.Fatalf("gc destroyed the only copy the pending update's rollback can exchange back: got=%q err=%v (outcome=%+v)",
			got, err, outcome)
	}
	pendingAfter, err := PendingTxns(s)
	if err != nil {
		t.Fatalf("gc must leave the pending transaction readable: %v", err)
	}
	if len(pendingAfter) != 1 {
		t.Fatalf("gc changed the pending transaction set: %+v", pendingAfter)
	}
}

// orphanedUpdatePreviousPayload reads the replaced-tree manifest out of the
// completed update family a crash left behind. It is orphanedRemoveManifest's
// counterpart, reading PreviousPayload rather than Payload -- the update side's
// manifest for the tree sitting in staging.
func orphanedUpdatePreviousPayload(t *testing.T, s *store.Store) store.OwnedTree {
	t.Helper()
	journal, err := scanTxnJournalReport(s)
	if err != nil {
		t.Fatal(err)
	}
	for key := range journal.completed {
		if key.op != "update" {
			continue
		}
		latest, err := validateTxnChain(s, key, journal.revisions[key])
		if err != nil {
			t.Fatal(err)
		}
		if latest.PreviousPayload == nil {
			t.Fatal("the completed update family must carry the manifest of the tree it replaced")
		}
		return *latest.PreviousPayload
	}
	t.Fatal("no completed update family left by the crash")
	return store.OwnedTree{}
}

// The three tests below cover gc's degraded arm for the staging residue class
// -- the `claimErr != nil` branch at txn_prune.go, reached when the pending set
// cannot be read, which asks updateStagingPayloadSettled instead of the claims
// set. Round 2, Important #2: that arm and the ~40 lines behind it had no test
// at all, proven by two mutations that left the whole Prune|Status|Update suite
// green (making updateStagingPayloadSettled always return settled, and making
// updateStagingTreePresent ignore the retired sibling). They mirror the rm
// side's own pair, which has always had them.
//
// What is at stake is the same thing in both: answering "settled" while the
// tree is still there prunes the family carrying the only manifest that tree
// can ever be verified or removed by, so it becomes permanently uncollectable
// and its name permanently unusable.

func TestPruneKeepsAnUpdateFamilyWhoseStagingOrphanIsStillThere(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestPruneKeepsAnUpdateFamilyWhoseStagingOrphanIsStillThere")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// A malformed pending journal name makes the claims read fail, which is the
	// branch that consults the staging payload directly instead of the pending
	// set. Deliberately not an update name, so the glob below cannot match it.
	malformed := filepath.Join(s.RecoveryDir(), "txn-adopt-not-a-record.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneCompletedTransactions(s); err == nil {
		t.Fatal("a malformed journal filename must still be reported")
	}
	if left := countTxnFamilyFiles(t, s, "txn-update-"); left == 0 {
		t.Fatal("the family carrying the orphan's only manifest must survive a run that cannot read the pending set")
	}
	if _, err := os.Lstat(filepath.Join(s.StagingDir(), "alpha")); err != nil {
		t.Fatalf("the orphan itself must be left alone too: %v", err)
	}
}

func TestPruneKeepsAnUpdateFamilyWhoseStagingOrphanIsParkedAtItsRetiredRootName(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestPruneKeepsAnUpdateFamilyWhoseStagingOrphanIsParkedAtItsRetiredRootName")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	manifest := orphanedUpdatePreviousPayload(t, s)

	// Reproduce the interrupted disposal through the same two steps
	// RemoveOwnedTreeAt performs in that order -- empty the tree, then retire
	// the root -- so the fixture cannot drift from the protocol it stands for.
	live := filepath.Join(s.StagingDir(), "alpha")
	liveDir, err := os.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveOwnedContents(liveDir, manifest); err != nil {
		_ = liveDir.Close()
		t.Fatal(err)
	}
	if err := liveDir.Close(); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(s.StagingDir(), store.RetiredRecoveryRootName("alpha", manifest))
	if err := os.Rename(live, retired); err != nil {
		t.Fatal(err)
	}

	malformed := filepath.Join(s.RecoveryDir(), "txn-adopt-not-a-record.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneCompletedTransactions(s); err == nil {
		t.Fatal("a malformed journal filename must still be reported")
	}
	if left := countTxnFamilyFiles(t, s, "txn-update-"); left == 0 {
		t.Fatal("a free live name with the root still parked at its retired sibling is not settled; the family must survive")
	}
	if _, err := os.Lstat(retired); err != nil {
		t.Fatalf("the retired root must be left in place for the next run to finish: %v", err)
	}
}

func TestPruneSettlesAnUpdateFamilyWhoseStagingOrphanIsGone(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestPruneSettlesAnUpdateFamilyWhoseStagingOrphanIsGone")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary shape of every settled update: the inline reclaim already
	// cleared staging/<name>. Here it is produced by hand because this run's
	// whole point is that the pending set cannot be read, so nothing else can
	// show the name unclaimed.
	if err := os.RemoveAll(filepath.Join(s.StagingDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(s.RecoveryDir(), "txn-adopt-not-a-record.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneCompletedTransactions(s); err == nil {
		t.Fatal("a malformed journal filename must still be reported")
	}
	if left := countTxnFamilyFiles(t, s, "txn-update-"); left != 0 {
		t.Fatalf("a family with nothing left at either candidate name waits for nothing and must prune, %d files left", left)
	}
}

// TestStatusDoesNotPromiseCollectionBlockedByAForeignStagingEntry pins round
// 2's Important #3, the one report incoherence reachable by ordinary user
// action. `staging/<name>` is the bare skill name, so a user creating a
// directory there on a name some completed update family also holds makes gc
// refuse that family's prune -- its reclaim fails first, and a failed reclaim
// skips the whole family (txn_prune.go's default arm). status counted the
// family's journal files Collectable anyway, so `fu status` said "N
// collectable (run `fu gc`)" while `fu gc` exited 1 and the count never
// moved, run after run: exactly the "run a command and watch a count not
// move" failure this file's accounting exists to prevent.
//
// The staging entry itself was already reported honestly (Unmatched, not
// Collectable); it is the recovery half that lied.
func TestStatusDoesNotPromiseCollectionBlockedByAForeignStagingEntry(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestStatusDoesNotPromiseCollectionBlockedByAForeignStagingEntry")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	// Baseline: the crash left fu's own orphan on the name, so the family is
	// genuinely collectable and status must say so. Without this the assertion
	// below could pass against a report that simply never counts anything.
	before, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Recovery.Collectable == 0 {
		t.Fatalf("fixture error: fu's own staging orphan must make the family collectable, got %+v", before.Recovery)
	}

	// The user's own directory replaces it on the same name.
	staging := filepath.Join(s.StagingDir(), "alpha")
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	writeSkillBody(t, staging, "alpha", "mine, not fu's")

	after, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.Recovery.Collectable != 0 {
		t.Fatalf("no journal file is collectable while gc's reclaim will fail: %+v", after.Recovery)
	}
	if after.Recovery.Uncollectable != before.Recovery.Collectable {
		t.Fatalf("the family's files must move to the inactionable bucket, not vanish: before=%+v after=%+v",
			before.Recovery, after.Recovery)
	}
	if after.Staging.Unmatched != 1 {
		t.Fatalf("the blocking entry itself is still the user's to move: %+v", after.Staging)
	}

	// And the promise the report now withholds is one gc really cannot keep.
	if _, err := PruneCompletedTransactions(s); err == nil {
		t.Fatal("gc must refuse to prune the family whose staging residue it cannot verify")
	}
	if left := countTxnFamilyFiles(t, s, "txn-update-"); left == 0 {
		t.Fatal("fixture error: the refused family must still be on disk")
	}
}

// The staging listing is the only evidence that a completed update family's
// orphan is blocked, and it was consulted without asking whether it could be
// read. When StagingNames fails for anything other than IsNotExist,
// stagingPresent is silently empty, so a blocked family skips the
// present-but-mismatching arm and lands in default: -- its journal files
// counted Collectable while gc refuses them on every run. That is exactly the
// over-promise stagingBlocked exists to prevent, arrived at from the other
// direction, and it is the same shape as claimsKnown three lines above: an
// empty set from a failed read is indistinguishable from an empty set from a
// clean one, and the difference decides the bucket.
//
// Narrow -- it needs a staging/ the process cannot list, and status already
// exits 1 naming that -- but the accounting must not quietly depend on the
// listing having succeeded.
func TestStatusWithholdsCollectionWhenTheStagingListingFailed(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the listing cannot be made to fail")
	}
	home := runCrashedUpdate(t, "TestStatusWithholdsCollectionWhenTheStagingListingFailed")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	// Baseline: fu's own orphan is on the name, so the family really is
	// collectable and the assertion below is not passing against a report that
	// never counts anything.
	before, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Recovery.Collectable == 0 {
		t.Fatalf("fixture error: the staging orphan must make the family collectable, got %+v", before.Recovery)
	}

	// Unreadable, rather than absent: IsNotExist is the answer "there is
	// nothing there", which is knowledge. This is the absence of knowledge.
	if err := os.Chmod(s.StagingDir(), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.StagingDir(), 0o755) })

	after, statusErr := Status(s, cfg, nil)
	if statusErr == nil {
		t.Fatal("an unreadable staging directory must be reported, not passed over")
	}
	if after.Recovery.Collectable != 0 {
		t.Fatalf("nothing may be promised collectable while the staging listing is unknown: %+v", after.Recovery)
	}
}

// TestStatusPromisesNoCollectionWhenTheClaimsReadFailed pins round 3's
// Important #3. scanTxnJournalReport succeeds while recording a malformed
// journal filename; pendingTxnsFromJournal is what fails on it. Guarding the
// collectable derivation on the scan alone ran it with a silently empty claims
// set, so a *pending* transaction's own staged tree -- the one on-disk copy
// restoreExchangedUpdate must exchange back -- was reported collectable, and
// `fu gc`, taking its claimErr arm on the same store, collected nothing. A
// reader who acted on "collectable" would wedge recovery permanently.
//
// It pins the other half of that rule too, which is what the second family
// below is for: withholding is owed to the *claim-dependent* sets only. A
// pending claim never blocks a journal prune -- both of gc's claimed arms fall
// through and prune the family anyway (txn_prune.go) -- so a settled family
// with nothing left to reclaim at either of its own payload names must stay
// collectable here, and the count must keep matching what gc then does.
func TestStatusPromisesNoCollectionWhenTheClaimsReadFailed(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestStatusPromisesNoCollectionWhenTheClaimsReadFailed")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// The second family: settled, and with no payload of its own at all, so gc
	// prunes it on its claimErr arm without ever asking what is claimed. Made
	// before the journal is broken below, because every write command recovers
	// pending transactions first and would fail on that file.
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatal(err)
	}
	settled := countTxnFamilyFiles(t, s, "txn-new-")
	if settled == 0 {
		t.Fatal("fixture error: the completed new family must have journal files on disk")
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	// Healthy journal: fu's own orphan is genuinely collectable, so the
	// assertion below cannot pass against a report that counts nothing.
	before, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Recovery.Collectable == 0 || before.Staging.Collectable == 0 {
		t.Fatalf("fixture error: the settled family and its orphan must both be collectable, got recovery=%+v staging=%+v",
			before.Recovery, before.Staging)
	}

	// One hand-dropped file the pending read cannot parse.
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), "txn-broken.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	after, statusErr := Status(s, cfg, nil)
	if statusErr == nil {
		t.Fatal("a journal the pending read cannot parse must be reported as a problem")
	}
	if after.Staging.Collectable != 0 {
		t.Fatalf("no staging name may be promised while the claims set is unknown: %+v", after.Staging)
	}
	// Exactly the settled family's files, so both halves are pinned at once:
	// the update family's are withheld -- its own orphan is still on the
	// staging name, which is what gc's claimErr arm stops at -- and the new
	// family's are not, because nothing about it depends on claims.
	if after.Recovery.Collectable != settled {
		t.Fatalf("a settled family with nothing to reclaim stays collectable while the claims set is unknown: want %d, got recovery=%+v",
			settled, after.Recovery)
	}
	// And both the promise made and the promise withheld are gc's own.
	outcome, _ := PruneCompletedTransactions(s)
	if outcome.Transactions != 1 || outcome.Files != settled {
		t.Fatalf("gc collects exactly what status promised: want 1 transaction and %d files, got %+v", settled, outcome)
	}
}

// TestStatusPromisesNoRemovePayloadCollectionWhenTheClaimsReadFailed is the rm
// half of what the test above pins, and it exists because that test could not
// reach it: the retraction has two clauses, one per residue-bearing op, and
// runCrashedUpdate exercises only the update one. Deleting the rm clause left
// the whole TestPrune/TestStatus suite green while `fu status` promised a
// crashed rm's journal files that `fu gc` then removes none of -- the same
// count that never moves the update clause was written to prevent.
//
// The two fixtures differ only in where the residue sits: there at
// staging/<name>, here at the quarantine name under recovery/. gc asks the same
// question about each on the same arm (RecoveryPayloadSettled here,
// updateStagingPayloadSettled there), so status has to withhold for both.
func TestStatusPromisesNoRemovePayloadCollectionWhenTheClaimsReadFailed(t *testing.T) {
	crashRemoveAfterTxnClearedChild()

	home := runCrashedRemove(t, "TestStatusPromisesNoRemovePayloadCollectionWhenTheClaimsReadFailed")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// The settled family with nothing to reclaim anywhere, exactly as in the
	// update twin: it is what shows the retraction is aimed at residue rather
	// than at every family in the journal.
	if _, err := NewSkill(s, nil, "beta"); err != nil {
		t.Fatal(err)
	}
	settled := countTxnFamilyFiles(t, s, "txn-new-")
	if settled == 0 {
		t.Fatal("fixture error: the completed new family must have journal files on disk")
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// The crash left the quarantined payload orphaned, which is the whole
	// reason the rm family's promise is conditional.
	orphanedRemovePayload(t, s)

	before, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Recovery.Collectable <= settled {
		t.Fatalf("fixture error: the rm family and its payload must be collectable too, got %+v", before.Recovery)
	}

	// One hand-dropped file the pending read cannot parse.
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), "txn-broken.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	after, statusErr := Status(s, cfg, nil)
	if statusErr == nil {
		t.Fatal("a journal the pending read cannot parse must be reported as a problem")
	}
	if after.Recovery.Collectable != settled {
		t.Fatalf("a family whose newest revision declares a payload manifest may not be promised: want %d, got recovery=%+v",
			settled, after.Recovery)
	}
	outcome, _ := PruneCompletedTransactions(s)
	if outcome.Transactions != 1 || outcome.Files != settled {
		t.Fatalf("gc collects exactly what status promised: want 1 transaction and %d files, got %+v", settled, outcome)
	}
}

// TestStatusDoesNotPromiseARetiredSiblingUnderAClaimedName pins round 3's
// Important #4. gc's claimed arm returns without touching either candidate
// name -- live or retired -- and prunes the family anyway, but status admitted
// the retired sibling to stagingPayloads unconditionally. So status reported it
// collectable, gc exited 0 having collected everything except that, and the
// count never moved.
func TestStatusDoesNotPromiseARetiredSiblingUnderAClaimedName(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestStatusDoesNotPromiseARetiredSiblingUnderAClaimedName")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	manifest := orphanedUpdatePreviousPayload(t, s)

	// An interrupted disposal: the tree is emptied and its root retired, but
	// never unlinked.
	live := filepath.Join(s.StagingDir(), "alpha")
	liveDir, err := os.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveOwnedContents(liveDir, manifest); err != nil {
		_ = liveDir.Close()
		t.Fatal(err)
	}
	if err := liveDir.Close(); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(s.StagingDir(), store.RetiredRecoveryRootName("alpha", manifest))
	if err := os.Rename(live, retired); err != nil {
		t.Fatal(err)
	}

	// Unclaimed, the retired sibling is exactly what gc resumes from, so it is
	// honestly collectable -- the baseline this test needs.
	unclaimed, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unclaimed.Staging.Collectable != 1 {
		t.Fatalf("fixture error: an unclaimed retired root is collectable, got %+v", unclaimed.Staging)
	}

	// Now a pending rm claims the bare skill name.
	claim := &TxnRecord{Op: "rm", Name: "alpha", Stage: "snapshotted"}
	if err := WriteTxn(checkedRecoveryStore(t, s), claim); err != nil {
		t.Fatal(err)
	}

	claimed, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Staging.Collectable != 0 {
		t.Fatalf("gc touches neither name under a claim, so neither may be promised: %+v", claimed.Staging)
	}
}

// TestPruneReclaimsTheStagingOrphanBeforeItsPruneRecordIsDurable pins the
// ordering DESIGN §2 and §5 both call a 硬性要求, and which round 3 found
// unenforced: the existing test asserts only the end state of one uninterrupted
// run, and because PreviousPayload is already in memory by then, moving the
// reclaim to *after* the prune record produces an identical end state. That
// mutation survived the whole Prune/Status suite.
//
// The violation only shows on a crash between the marker and the reclaim: the
// next gc run takes the "resumed prune reclaims nothing, and needs to reclaim
// nothing" branch, deletes the revisions, and the orphan is unverifiable
// forever. This asserts the ordering directly instead, at the one instant that
// distinguishes the two: when the prune record is durable, the tree must
// already be gone.
func TestPruneReclaimsTheStagingOrphanBeforeItsPruneRecordIsDurable(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestPruneReclaimsTheStagingOrphanBeforeItsPruneRecordIsDurable")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(s.StagingDir(), "alpha")
	if _, err := os.Lstat(staging); err != nil {
		t.Fatalf("fixture error: the crash must leave the orphan in place: %v", err)
	}

	var markerFired bool
	var orphanStillThere bool
	outcome, err := pruneCompletedTransactions(s, pruneHooks{afterMarker: func() error {
		markerFired = true
		_, statErr := os.Lstat(staging)
		orphanStillThere = statErr == nil
		return nil
	}})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !markerFired {
		t.Fatal("the prune record was never written; this test proves nothing")
	}
	if orphanStillThere {
		t.Fatal("the orphan must be reclaimed before its family's prune record is durable: " +
			"a crash in this window leaves it unverifiable, because the manifest goes with the revisions")
	}
	if outcome.Transactions != 1 {
		t.Fatalf("prune outcome = %+v, want the one completed update family pruned", outcome)
	}
}

// TestStatusCallsAMatchingClaimedStagingNameBlocked pins round 3's Important
// #11: the staging walk must test the claim *before* the identity match, so a
// name a pending record claims is Blocked even when its content is exactly what
// a completed family's manifest describes -- the inode-preserving case that
// makes the two indistinguishable by content.
//
// The existing claimed-name test builds a name whose identity deliberately does
// not match, so it cannot see the ordering at all.
func TestStatusCallsAMatchingClaimedStagingNameBlocked(t *testing.T) {
	crashUpdateAfterTxnClearedChild()

	home := runCrashedUpdate(t, "TestStatusCallsAMatchingClaimedStagingNameBlocked")
	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// Unclaimed, this very tree is Collectable: it is the completed family's
	// own orphan, identity and all. That is what makes the claim the only thing
	// that can move it.
	before, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Staging.Collectable != 1 || before.Staging.Blocked != 0 {
		t.Fatalf("fixture error: the orphan must start out collectable, got %+v", before.Staging)
	}

	claim := &TxnRecord{Op: "update", Name: "alpha", Stage: "snapshotted"}
	if err := WriteTxn(checkedRecoveryStore(t, s), claim); err != nil {
		t.Fatal(err)
	}

	after, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.Staging.Collectable != 0 {
		t.Fatalf("a claimed name is gc's to leave alone, whatever its identity says: %+v", after.Staging)
	}
	if after.Staging.Blocked != 1 {
		t.Fatalf("a claimed name waits on that transaction, so it belongs in Blocked: %+v", after.Staging)
	}
}
