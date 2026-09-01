package engine

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

func newValidOwnedTree() *store.OwnedTree {
	return &store.OwnedTree{
		RootIdentity: store.FileIdentity{Device: 1, Inode: 2},
		RootMode:     uint32(os.ModeDir | 0o700),
	}
}

// The replaced tree must survive a JSON round trip: recovery reads it back from
// the journal to decide which side of the exchange a crash landed on.
func TestTxnRecordRoundTripsPreviousPayload(t *testing.T) {
	tree := store.OwnedTree{RootMode: 0o755}
	rec := TxnRecord{Op: "update", Name: "kit", PreviousPayload: &tree}

	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "previous_payload") {
		t.Fatalf("previous_payload must be serialized: %s", raw)
	}

	var back TxnRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.PreviousPayload == nil || back.PreviousPayload.RootMode != 0o755 {
		t.Fatalf("PreviousPayload did not round trip: %+v", back.PreviousPayload)
	}
}

// An absent field must stay absent, so records written by the other four
// operations are byte-identical to what they were before this field existed.
func TestTxnRecordOmitsPreviousPayloadWhenUnset(t *testing.T) {
	raw, err := json.Marshal(TxnRecord{Op: "rm", Name: "kit"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "previous_payload") {
		t.Fatalf("an unset previous_payload must not be serialized: %s", raw)
	}
}

func TestValidateUpdateRecordRequiresBothPayloadsPastTheStagedStage(t *testing.T) {
	tree := newValidOwnedTree()
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "kit", Stage: updateTxnStaged, PreviousPayload: tree,
	}); err == nil {
		t.Fatal("a staged update with no Payload must be rejected")
	}
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "kit", Stage: updateTxnStaged, Payload: tree,
	}); err == nil {
		t.Fatal("a staged update with no PreviousPayload must be rejected")
	}
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "kit", Stage: updateTxnStaged, Payload: tree, PreviousPayload: tree,
	}); err != nil {
		t.Fatalf("a well-formed staged update must be accepted: %v", err)
	}
}

// A record at "published" stage without both manifests must be rejected.
// Recovery will dereference both unconditionally.
func TestValidateUpdateRecordRejectsPublishedWithMissingManifest(t *testing.T) {
	tree := newValidOwnedTree()
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "kit", Stage: "published", PreviousPayload: tree,
	}); err == nil {
		t.Fatal("a published update with no Payload must be rejected")
	}
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "kit", Stage: "published", Payload: tree,
	}); err == nil {
		t.Fatal("a published update with no PreviousPayload must be rejected")
	}
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "kit", Stage: "published", Payload: tree, PreviousPayload: tree,
	}); err != nil {
		t.Fatalf("a well-formed published update must be accepted: %v", err)
	}
}

// A record carrying an unknown stage must be rejected.
func TestValidateUpdateRecordRejectsUnknownStage(t *testing.T) {
	tree := newValidOwnedTree()
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "kit", Stage: "garbage", Payload: tree, PreviousPayload: tree,
	}); err == nil {
		t.Fatal("an update with unknown stage must be rejected")
	}
}

// The Op and Name guards reject invalid records.
func TestValidateUpdateRecordGuardsOpAndName(t *testing.T) {
	tree := newValidOwnedTree()
	if err := validateUpdateRecord(TxnRecord{
		Op: "rm", Name: "kit", Stage: updateTxnStaged, Payload: tree, PreviousPayload: tree,
	}); err == nil {
		t.Fatal("a non-update operation must be rejected")
	}
	if err := validateUpdateRecord(TxnRecord{
		Op: "update", Name: "", Stage: updateTxnStaged, Payload: tree, PreviousPayload: tree,
	}); err == nil {
		t.Fatal("an update with no skill name must be rejected")
	}
}

// The tests below cover the four rows of recoverUpdate's decision table (see
// its doc comment). Each runs one real update in a child process, terminates
// that child at a named durable boundary, and asserts what RecoverPending
// makes of the state the crash left -- the shape rm's own crash-injection
// tests use (rm_test.go).
const (
	updateCrashHelperEnv = "FU_TEST_CRASH_UPDATE_HELPER"
	updateCrashHomeEnv   = "FU_TEST_CRASH_UPDATE_HOME"
	updateCrashSourceEnv = "FU_TEST_CRASH_UPDATE_SOURCE"
	updateCrashStageEnv  = "FU_TEST_CRASH_UPDATE_STAGE"
	updateCrashExitCode  = 86

	updateOldBody = "the body kit was installed with"
	updateNewBody = "the body upstream moved on to"
)

// preparedUpdate reopens the local source the parent seeded and returns
// everything update's content shape needs to pull it in.
func preparedUpdate(t *testing.T, srcDir string) (*source.Prepared, Candidate, map[string]string) {
	t.Helper()
	p := prepareLocal(t, srcDir)
	proj, err := skill.ProjectDir(mustRootFS(t, p), ".")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := skill.DigestManifest(proj)
	if err != nil {
		t.Fatal(err)
	}
	return p, Candidate{Name: "kit", Subdir: ".", Digest: digest},
		map[string]string{"type": "local", "path": srcDir}
}

// TestUpdateCrashHelper is not a test of its own: it is the child process
// crashUpdateAt re-execs, running one real update against the store the parent
// prepared and dying at the boundary the parent named. An ordinary test run
// has none of this in its environment and returns immediately.
func TestUpdateCrashHelper(t *testing.T) {
	if os.Getenv(updateCrashHelperEnv) != "1" {
		return
	}
	s, err := store.Open(os.Getenv(updateCrashHomeEnv))
	if err != nil {
		t.Fatal(err)
	}
	crash := func() error { os.Exit(updateCrashExitCode); return nil }
	var h hooks
	switch stage := os.Getenv(updateCrashStageEnv); stage {
	case "after-staging-create":
		// The staging root exists and is journalled, but nothing has been
		// copied into it and the config has not been touched.
		h.afterStagingCreate = crash
	case "before-exchange":
		// fu.yaml already names the new baseline and staging/kit holds the
		// replacement, but the published tree has not moved.
		h.beforePublish = crash
	case "after-exchange":
		// The exchange has run and been journalled; the commit has not.
		h.afterPublish = crash
	case "after-commit":
		h.afterCommit = crash
	default:
		t.Fatalf("unknown crash stage %q", stage)
	}
	p, cand, fields := preparedUpdate(t, os.Getenv(updateCrashSourceEnv))
	_, _ = updateSkill(s, nil, p, "kit", cand, localSourceRecord(os.Getenv(updateCrashSourceEnv)), fields, false, h)
	t.Fatal("the crash hook did not run")
}

// crashUpdateAt installs "kit" from a local source, points that source at
// replacement content, and runs one update in a child process that terminates
// at stage. It returns the store reopened over the state the crash left and
// the baseline digest fu.yaml recorded when kit was installed.
func crashUpdateAt(t *testing.T, stage string) (*store.Store, string) {
	t.Helper()
	s, cfg := setupStore(t)
	srcDir, baseline := installedFromLocal(t, s, cfg, "kit", updateOldBody)
	writeSkillBody(t, srcDir, "kit", updateNewBody)

	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateCrashHelper$")
	cmd.Env = append(os.Environ(),
		updateCrashHelperEnv+"=1",
		updateCrashHomeEnv+"="+s.Home,
		updateCrashSourceEnv+"="+srcDir,
		updateCrashStageEnv+"="+stage,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != updateCrashExitCode {
		t.Fatalf("child must terminate at %s with code %d, err=%v output=%s",
			stage, updateCrashExitCode, err, output)
	}
	crashed, err := store.Open(s.Home)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing below means anything unless the crash really did leave the one
	// update transaction these tests recover.
	pending, err := PendingTxns(crashed)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Op != "update" {
		t.Fatalf("the crash must leave exactly one pending update transaction, got %+v", pending)
	}
	return crashed, baseline
}

// assertSkillBody fails unless the SKILL.md under dir carries want.
func assertSkillBody(t *testing.T, dir, want string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), want) {
		t.Fatalf("%s holds:\n%s\nwant a body containing %q", dir, raw, want)
	}
}

func assertStagingEmpty(t *testing.T, s *store.Store) {
	t.Helper()
	entries, err := os.ReadDir(s.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("staging must be empty once recovery has settled, holds %v", names)
	}
}

// assertStoreMatchesHEAD fails unless the worktree is exactly what the store's
// history says it is. A recovery that settles the WAL but leaves the store
// disagreeing with HEAD is not visibly wrong: the next write command's Sweep
// absorbs the difference under "external: manual modifications", and whatever
// recovery got wrong becomes a commit nobody made on purpose.
func assertStoreMatchesHEAD(t *testing.T, s *store.Store) {
	t.Helper()
	changed, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("recovery must leave the store agreeing with HEAD, found %v uncommitted", changed)
	}
}

func assertNoPendingTxn(t *testing.T, s *store.Store) {
	t.Helper()
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a settled recovery must clear its WAL, got %+v", pending)
	}
}

func recordedDigest(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Digest(name)
}

// assertUpdateRolledBack pins both rollback rows: the tree the update meant to
// replace is published again, the replacement is gone, fu.yaml still records
// the old baseline, and the WAL is closed.
func assertUpdateRolledBack(t *testing.T, s *store.Store, baseline string) {
	t.Helper()
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), updateOldBody)
	assertStagingEmpty(t, s)
	if got := recordedDigest(t, s, "kit"); got != baseline {
		t.Fatalf("fu.yaml digest = %q, want the baseline recorded before the update, %q", got, baseline)
	}
	assertStoreMatchesHEAD(t, s)
	assertNoPendingTxn(t, s)
}

// Before updateTxnStaged no exchange can have happened, so recovery has only
// staging to clean up. This is also the stage that makes a non-nil Payload
// worthless as evidence: createTxnStagedRoot journals the staging root it
// published while the record still reads "snapshotted" (staging.go), so
// Payload here is a root-only manifest of a directory the copy never filled
// in -- not the staged replacement a later stage's Payload describes.
func TestRecoverUpdateDiscardsStagingLeftBeforeTheStagedRevision(t *testing.T) {
	s, baseline := crashUpdateAt(t, "after-staging-create")
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	record := pending[0]
	if record.Stage != updateTxnSnapshotted {
		t.Fatalf("stage = %q, want %q", record.Stage, updateTxnSnapshotted)
	}
	if record.Payload == nil || len(record.Payload.Entries) != 0 {
		t.Fatalf("this stage must carry the root-only staging manifest, got %+v", record.Payload)
	}
	// The entries the copy had not created yet are still declarations, which is
	// what recovery has to settle the residue against before it can remove it.
	if len(record.Declared) == 0 {
		t.Fatal("this stage must still declare the entries the copy never created")
	}
	if _, err := os.Stat(filepath.Join(s.StagingDir(), "kit")); err != nil {
		t.Fatalf("the crash must leave the staging root behind, or the discard below proves nothing: %v", err)
	}

	if err := RecoverPending(checkedRecoveryStore(t, s)); err != nil {
		t.Fatalf("recovery must discard the staging residue of an update that never staged: %v", err)
	}
	assertUpdateRolledBack(t, s, baseline)
}

// Decision table row 1: not committed, skills/<name> still holds the old tree.
// Recovery discards the staged replacement and restores fu.yaml.
func TestRecoverUpdateRollsBackBeforeTheExchange(t *testing.T) {
	s, baseline := crashUpdateAt(t, "before-exchange")
	// The state recovery is handed: the replacement is staged and fu.yaml
	// already names it, while the published tree has not moved.
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), updateOldBody)
	assertSkillBody(t, filepath.Join(s.StagingDir(), "kit"), updateNewBody)
	if got := recordedDigest(t, s, "kit"); got == baseline {
		t.Fatal("the crash must come after the config was saved, or the restore below proves nothing")
	}

	if err := RecoverPending(checkedRecoveryStore(t, s)); err != nil {
		t.Fatalf("recovery must roll back an update interrupted before its exchange: %v", err)
	}
	assertUpdateRolledBack(t, s, baseline)
}

// Row 2: not committed, but the exchange already happened. Recovery must
// exchange back before discarding.
func TestRecoverUpdateExchangesBackWhenTheCommitDidNotLand(t *testing.T) {
	s, baseline := crashUpdateAt(t, "after-exchange")
	// The mirror of row 1: the two trees have swapped places, and only the
	// commit is missing.
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), updateNewBody)
	assertSkillBody(t, filepath.Join(s.StagingDir(), "kit"), updateOldBody)

	if err := RecoverPending(checkedRecoveryStore(t, s)); err != nil {
		t.Fatalf("recovery must exchange an uncommitted update back: %v", err)
	}
	assertUpdateRolledBack(t, s, baseline)
}

// Row 2 again, from the window no hook sits inside: update performs the
// exchange and journals it afterwards, so a crash in between leaves a record
// whose stage says the swap has not happened about a swap that has. Recovery
// must still undo it, because what it decides on is the manifest skills/kit
// matches and not the stage the record stopped at. The test reproduces that
// window by performing the exchange exactly as Publish does, after a crash at
// the boundary immediately before it.
func TestRecoverUpdateExchangesBackWhenTheStageNeverRecordedTheSwap(t *testing.T) {
	s, baseline := crashUpdateAt(t, "before-exchange")
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	record := pending[0]
	if record.Stage != "config-saved" {
		t.Fatalf("stage = %q, want the revision that precedes the journalled exchange", record.Stage)
	}
	checked := checkedRecoveryStore(t, s)
	if err := checked.ExchangeStagedWithSkillOwned("kit", *record.Payload, *record.PreviousPayload); err != nil {
		t.Fatal(err)
	}
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), updateNewBody)

	if err := RecoverPending(checked); err != nil {
		t.Fatalf("recovery must undo an exchange its record never got to name: %v", err)
	}
	assertUpdateRolledBack(t, s, baseline)
}

// Row 3: committed. The exchange is implied, so recovery only disposes of the
// replaced tree left at the staging name and clears the WAL.
func TestRecoverUpdateFinishesAfterTheCommitLanded(t *testing.T) {
	s, baseline := crashUpdateAt(t, "after-commit")
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), updateNewBody)
	assertSkillBody(t, filepath.Join(s.StagingDir(), "kit"), updateOldBody)
	updated := recordedDigest(t, s, "kit")
	if updated == baseline {
		t.Fatal("a committed update must already have advanced the recorded baseline")
	}

	if err := RecoverPending(checkedRecoveryStore(t, s)); err != nil {
		t.Fatalf("recovery must carry a committed update to its end state: %v", err)
	}
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), updateNewBody)
	assertStagingEmpty(t, s)
	if got := recordedDigest(t, s, "kit"); got != updated {
		t.Fatalf("a committed update's baseline must stand, got %q want %q", got, updated)
	}
	assertStoreMatchesHEAD(t, s)
	assertNoPendingTxn(t, s)
}

// Row 4, the committed half. The refusal row spans both sides of the commit,
// and this is the side where getting it wrong costs the most: with the commit
// landed, the only remaining work is disposal, so an implementation that
// carried on past the mismatch would delete the replaced tree at staging/kit
// while skills/kit holds content fu never wrote -- the one state where both of
// the user's copies are gone at once.
func TestRecoverUpdateRefusesWhenACommittedUpdateLostItsPublishedTree(t *testing.T) {
	s, _ := crashUpdateAt(t, "after-commit")
	const foreign = "written by something that is not fu"
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", foreign)

	err := RecoverPending(checkedRecoveryStore(t, s))
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("recovery error = %v, want a conflict refusing to finish over an unrecognised published tree", err)
	}
	for _, want := range []string{filepath.Join(s.SkillsDir(), "kit"), "does not hold the replacement"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name %q; got: %v", want, err)
		}
	}
	pending, pendingErr := PendingTxns(s)
	if pendingErr != nil {
		t.Fatal(pendingErr)
	}
	if len(pending) != 1 {
		t.Fatalf("a refused recovery must keep its transaction pending, got %+v", pending)
	}
	// The assertion the whole test exists for: the tree the update replaced is
	// untouched, because the refusal came before the disposal and not after it.
	assertSkillBody(t, filepath.Join(s.StagingDir(), "kit"), updateOldBody)
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), foreign)
}

// Row 4: skills/<name> matches neither manifest -- something outside fu wrote
// there. Recovery refuses and keeps the WAL rather than guessing.
func TestRecoverUpdateRefusesWhenTheSkillMatchesNeitherManifest(t *testing.T) {
	s, _ := crashUpdateAt(t, "after-exchange")
	const foreign = "written by something that is not fu"
	// Between the crash and the next fu command, something outside fu rewrites
	// the published skill. Neither recorded manifest describes it now, so
	// nothing can tell recovery which side of the exchange it is looking at.
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", foreign)

	err := RecoverPending(checkedRecoveryStore(t, s))
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("recovery error = %v, want a conflict refusing the unrecognised published tree", err)
	}
	// The refusal must be fu's own decision and must say what it saw, not a
	// destructive step that happened to be stopped by a primitive's own
	// validation. Asserted on the message because that difference is invisible
	// in the resulting state: both leave the store untouched.
	for _, want := range []string{filepath.Join(s.SkillsDir(), "kit"), "matches neither"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name %q; got: %v", want, err)
		}
	}
	// The refusal is worth little unless the record survives it: the WAL holds
	// the only description of the two trees a later repair has to work from.
	pending, pendingErr := PendingTxns(s)
	if pendingErr != nil {
		t.Fatal(pendingErr)
	}
	if len(pending) != 1 {
		t.Fatalf("a refused recovery must keep its transaction pending, got %+v", pending)
	}
	// Refusing moved and deleted nothing: both the foreign content and the
	// tree the update replaced are still where they were.
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), foreign)
	assertSkillBody(t, filepath.Join(s.StagingDir(), "kit"), updateOldBody)
}

// TestRecoverUpdateRefusesWhenTheConfigMovedUnderIt covers both rollback config
// guards round 3's Important #7 found unpinned -- one for each side of the
// staging boundary. Dropping either survived the suite, and both make recovery
// *silently succeed*: restoreTxnConfig would install ConfigBefore over an
// external fu.yaml edit that arrived during the crash window, because the
// conditional install is conditioned on the bytes recovery just observed
// rather than on the bytes it expected.
//
// Losing a hand edit to fu.yaml is exactly what SPEC §5.3's sweep discipline
// exists to prevent, and recovery is the one path that runs before any sweep.
func TestRecoverUpdateRefusesWhenTheConfigMovedUnderIt(t *testing.T) {
	for _, stage := range []string{"after-staging-create", "before-exchange"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := crashUpdateAt(t, stage)

			// An external edit lands in the crash window. Appending keeps the
			// file valid YAML, so recovery fails on the guard rather than on a
			// parse error.
			raw, err := os.ReadFile(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			edited := string(raw) + "\n# a hand edit made while fu was not running\n"
			if err := os.WriteFile(s.ConfigPath(), []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}

			err = RecoverPending(checkedRecoveryStore(t, s))
			if !errors.Is(err, ErrTxnConflict) {
				t.Fatalf("recovery over an edited fu.yaml must report a safe conflict, got %v", err)
			}
			after, err := os.ReadFile(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != edited {
				t.Fatalf("the hand edit must survive the refusal:\ngot:\n%s\nwant:\n%s", after, edited)
			}
		})
	}
}

// TestRecoverUpdateSettlesAPartiallyCopiedStagingTree covers round 3's
// Important #6. Dropping SettleDeclaredStagedEntries survived, because the only
// test reaching this path crashes where the staged root is still *empty* -- so
// settling is a no-op and the root-only manifest already describes reality.
//
// A crash inside CopyStagedTreeOwned leaves a strict subset of the declarations
// on disk. Without settling, RemoveOwnedTreeAt's all-or-nothing preflight
// refuses with "gained unknown entry", recovery never reaches a terminal state,
// and every write command is blocked at its recovery prologue from then on.
func TestRecoverUpdateSettlesAPartiallyCopiedStagingTree(t *testing.T) {
	s, _ := crashUpdateAt(t, "after-staging-create")

	// One real declared file inside the staged root, standing for the copy
	// having reached that entry before the process died. It must carry the
	// content the transaction declared, because that is what settling verifies
	// -- arbitrary bytes on a declared name are a different state (an external
	// write), and recovery correctly refuses that one.
	staged := filepath.Join(s.StagingDir(), "kit")
	writeSkillBody(t, staged, "kit", updateNewBody)

	if err := RecoverPending(checkedRecoveryStore(t, s)); err != nil {
		t.Fatalf("recovery must settle a partially copied staged tree, not refuse it: %v", err)
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("the rolled-back staged tree must be gone, stat err=%v", err)
	}
	// The published skill is untouched: this crash is before the exchange.
	assertSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), updateOldBody)
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("recovery must reach a terminal state, still pending: %+v", pending)
	}
}
