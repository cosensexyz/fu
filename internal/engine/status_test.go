// internal/engine/status_test.go
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// TestStatusDoesNotCreateAMissingAgentDirectory pins the read-only contract of
// SPEC §9. The write path's createAndScanAgentDir creates a missing skills
// directory before scanning it; a status that reused that helper would make a
// read-only command change the filesystem it is reporting on.
func TestStatusDoesNotCreateAMissingAgentDirectory(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "never-created")
	agents := []agent.Agent{fakeAgent{"claude", absent}}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Fatalf("status must not create %s, lstat err=%v", absent, err)
	}
	if len(report.Agents) != 1 || !report.Agents[0].DirMissing {
		t.Fatalf("status must report the absent directory as pending projection: %+v", report.Agents)
	}
}

// TestStatusReportsSymlinkedAgentDirWithoutTouchingIt pins the other
// precondition ScanAgent already computes and Status must merely relay
// (SPEC rule 10): an agent whose skills directory is itself a symlink is
// never written through by reconcile, and status must say so rather than
// silently treating it like any other agent.
func TestStatusReportsSymlinkedAgentDirWithoutTouchingIt(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linkdir")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{fakeAgent{"claude", link}}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Agents) != 1 || !report.Agents[0].DirIsSymlink {
		t.Fatalf("status must report a symlinked skills dir: %+v", report.Agents)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Fatal("status must never write through a symlinked skills dir")
	}
}

// TestStatusIsolatesAScanFailureToOneAgent mirrors Reconcile's own per-agent
// isolation (finding I3): a broken scan for one agent -- here, a skills
// "directory" that is actually a plain file, so os.ReadDir fails -- must be
// recorded on that agent alone, in ScanErr, without preventing a healthy
// agent listed alongside it from being reported normally.
func TestStatusIsolatesAScanFailureToOneAgent(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	brokenPath := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(brokenPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	okDir := t.TempDir()
	agents := []agent.Agent{
		fakeAgent{"broken-agent", brokenPath},
		fakeAgent{"claude", okDir},
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Agents) != 2 {
		t.Fatalf("want one report entry per given agent, got %+v", report.Agents)
	}
	if report.Agents[0].Name != "broken-agent" || report.Agents[0].ScanErr == "" {
		t.Fatalf("broken agent's scan failure must be recorded in ScanErr, got %+v", report.Agents[0])
	}
	if report.Agents[1].Name != "claude" || report.Agents[1].ScanErr != "" || report.Agents[1].DirMissing {
		t.Fatalf("a healthy agent listed after a broken one must still be reported cleanly, got %+v", report.Agents[1])
	}
}

// TestStatusReportsHealthyAgentCleanly pins the zero-drift baseline: an
// agent whose skills directory already exists and whose one enabled skill is
// already linked exactly as fu.yaml asks -- reality matching desired state,
// not merely a directory that happens to exist -- reports no missing
// directory, no symlink precondition violation, no scan error, and no
// drift. NewSkill is used for the fixture (rather than setupStore's
// content-only registration) precisely because it also reconciles: it is
// the only way to get a real fu link on disk without reaching into
// Reconcile by hand.
func TestStatusReportsHealthyAgentCleanly(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Agents) != 1 {
		t.Fatalf("want exactly one report entry, got %+v", report.Agents)
	}
	got := report.Agents[0]
	if got.Name != "claude" || got.DirMissing || got.DirIsSymlink || got.ScanErr != "" {
		t.Fatalf("a plain existing skills dir must report cleanly, got %+v", got)
	}
	if len(got.Drift) != 0 {
		t.Fatalf("reality matching fu.yaml must report zero drift, got %+v", got.Drift)
	}
}

// TestStatusReportsAMissingLinkAsDrift pins that status classifies without
// acting: an enabled skill whose link was deleted by hand must surface as a
// CreateLink action that status reports and does not perform.
func TestStatusReportsAMissingLinkAsDrift(t *testing.T) {
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
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, action := range report.Agents[0].Drift {
		if action.Type == CreateLink && action.Skill == "alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a deleted link must be reported as drift: %+v", report.Agents[0].Drift)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("status must report the missing link, not recreate it")
	}
}

// TestStatusReportsABrokenLinkAsOneMissingFinding pins SPEC rule 6's own
// headline example -- the store entity deleted by hand -- as one finding
// rather than two contradictory ones. Diff answers this state with the pair
// that repairs it, RemoveLink then CreateLink for the same skill; that is a
// work list for the write path, and read as a description it says the link is
// both stale and missing at once. The single true statement is the one
// reconcile itself reports when its CreateLink half finds no store content:
// ReportMissing, which Diff never emits, leaving status's only accurate
// wording for this state unreachable.
func TestStatusReportsABrokenLinkAsOneMissingFinding(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	drift := report.Agents[0].Drift
	if len(drift) != 1 {
		t.Fatalf("a broken link is one fact about one entry, got %+v", drift)
	}
	if drift[0].Type != ReportMissing || drift[0].Skill != "alpha" {
		t.Fatalf("drift = %+v, want ReportMissing for alpha", drift[0])
	}
	// The write path keeps Diff's pair: it needs both halves to do the repair,
	// and reconcile's own CreateLink arm is what turns the missing target into
	// a ReportMissing there.
	if acts := Diff(map[string]bool{"alpha": true}, mustScan(t, agents[0], s.SkillsDir()), s.SkillsDir()); len(acts) != 2 {
		t.Fatalf("Diff's repair pair must be left alone, got %+v", acts)
	}
}

func mustScan(t *testing.T, a agent.Agent, storeSkillsDir string) AgentState {
	t.Helper()
	state, err := ScanAgent(a, storeSkillsDir)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestStatusReportsAMisspelledLinkTargetAsOneStaleLink covers the other half
// of the same Diff arm: a fu link whose target still resolves into the store
// but is not spelled the way fu writes it. Reconcile rewrites it; status has
// one thing to say about it, not two.
func TestStatusReportsAMisspelledLinkTargetAsOneStaleLink(t *testing.T) {
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
	// An alias of $FU_HOME, the one residual ownsLink accepts deliberately
	// (see prefixResolvesToStoreHome): the entry is still fu's own link, and
	// its target still resolves to the store content, but the raw target text
	// is not the one Diff wants written there.
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(filepath.Dir(filepath.Dir(s.SkillsDir())), alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(alias, "store", "skills", "alpha"), link); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	drift := report.Agents[0].Drift
	if len(drift) != 1 || drift[0].Type != RemoveLink || drift[0].Skill != "alpha" {
		t.Fatalf("drift = %+v, want one RemoveLink for alpha", drift)
	}
}

// TestStatusReportsAnUncommittedStoreEdit pins that hand-editing the store is
// visible before any write command sweeps it into history.
func TestStatusReportsAnUncommittedStoreEdit(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")
	if err := os.WriteFile(edited, []byte("---\nname: alpha\ndescription: edited by hand\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Store.DirtyPaths) == 0 {
		t.Fatal("a hand-edited skill must show as an uncommitted store change")
	}
}

// TestStatusInventoriesRecoveryByWhatTheUserCanDo pins the four buckets. The
// split is by remedy, not by age: run fu gc, settle an unfinished write first,
// do nothing, or judge by hand. .fu-retired-dir- belongs with the collectable
// ones because its name is derived from the manifest and RemoveOwnedTreeAt
// resumes from it; reporting it as uncollectable would push a user toward
// deleting a resumable intermediate.
//
// The config exchange record here carries no .done marker, so it is one
// RecoverConfigExchanges must still finish or withdraw and `fu gc` will not
// touch, and the .fu-config-archive- entry beside it is frozen behind that
// same pending record (ReclaimCompletedConfigExchanges refuses the archive
// sweep wholesale while any exchange is pending). Both count Blocked rather
// than Collectable; TestStatusCountsRecoveryWorkWaitingOnARecoveryPassApart
// FromCollectable drives the marker in and out to pin both directions.
//
// rollback- is excluded only when some pending record still derives it, not
// absolutely: a new, add, or adopt transaction's own compensation payload is
// present on disk while its owning transaction's WAL entry is still open, and
// StatusReport.Store.Pending already reports that transaction by op and skill
// name, so counting the payload again under "judge by hand" would report the
// one fact twice. But addRecoveryConflictRemedy's abandon-recovery advice
// (txn.go) tells a user to move a conflicted transaction's journal files --
// revisions, completion marker, prune records -- out of recovery/ without
// mentioning this payload, so a rollback- name can also outlive its own
// journal; nothing then remains to ever collect it, so it must count
// Uncollectable rather than be silently dropped. The fixture pins both
// directions: pendingAdopt's own uncommitted quarantine name (computed via
// installUncommittedPayloadName, the same derivation the classifier under
// test uses) is excluded, and "rollback-new-alpha-deadbeef", which matches
// neither pending record below, counts as one of the three Uncollectable
// entries.
//
// The "txn-" entries are canonical, single-revision pending transaction
// records rather than arbitrary placeholders, so the same fixture also pins
// that both pending transactions are reported through
// StatusReport.Store.Pending and are not additionally counted into any
// recovery bucket.
func TestStatusInventoriesRecoveryByWhatTheUserCanDo(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	// pendingAdopt is built before the fixture map below so its own
	// uncommitted compensation-payload name -- computed through
	// installUncommittedPayloadName rather than hand-typed -- cannot
	// silently drift from what pendingPayloadClaims would derive from the
	// same record.
	pendingAdopt := TxnRecord{
		Op:        "adopt",
		TxnID:     "ffeeddccbbaa99887766554433221100",
		Sequence:  1,
		Name:      "beta",
		StartHead: "cafef00d",
	}
	claimedRollback := installUncommittedPayloadName(pendingAdopt)

	for name, dir := range map[string]bool{
		// pendingTxn below is rm/alpha at start HEAD "cafef00d", so it derives
		// "removed-alpha-cafef00d" and not this name: the unclaimed direction.
		// Unclaimed is not the same as collectable, though, and this fixture
		// holds no completed family describing this payload either -- so
		// nothing in `fu gc` will ever name it and it counts Uncollectable
		// alongside the unclaimed rollback- below.
		// TestStatusExcludesARemovedPayloadAPendingTransactionStillClaims pins
		// the claimed direction, and TestStatusCountsARemovedPayloadA
		// CompletedFamilyDescribesAsCollectable pins the genuinely collectable
		// one.
		"removed-alpha-deadbeef":                               true,
		".fu-config-exchange-0011223344556677.json":            false,
		".fu-config-archive-0011223344556677-8899aabbccddeeff": false,
		// Same reasoning: a retired root is resumable only while the manifest
		// its name was derived from is still on disk, and this fixture has no
		// such family.
		".fu-retired-dir-0011223344556677aabbccdd": true,
		"adopt-archive-claude-alpha-00112233":      true,
		"adopt-link-00112233.json":                 false,
		// No pending record derives this name -- it matches neither
		// pendingTxn ("rm"/"alpha") nor pendingAdopt ("adopt"/"beta")
		// below -- so it pins the "unclaimed" direction: it must count
		// Uncollectable rather than be excluded like a claimed name.
		"rollback-new-alpha-deadbeef":                        true,
		".fu-archive-0011223344556677aabbccdd":               true,
		".fu-retired-entry-00112233445566778899aabbccddeeff": false,
		// claimedRollback pins the "claimed" direction: pendingAdopt's own
		// uncommitted quarantine name must be excluded rather than counted.
		claimedRollback: true,
	} {
		path := filepath.Join(s.RecoveryDir(), name)
		var mkErr error
		if dir {
			mkErr = os.MkdirAll(path, 0o755)
		} else {
			mkErr = os.WriteFile(path, []byte("{}"), 0o600)
		}
		if mkErr != nil {
			t.Fatal(mkErr)
		}
	}

	// A real "txn-...json" name is validated by parseTxnRecordName (called
	// from PendingTxns, which Status already runs earlier in the same call),
	// so an arbitrary placeholder would fail the whole Status call rather
	// than exercise the inventory's own "txn-" exclusion. Both fixtures
	// below are therefore canonical single-revision pending transaction
	// records, named through the package's own txnRecordName so each
	// filename's digest suffix matches the bytes written to disk -- the same
	// binding decodeTxnFile checks on every real revision.
	pendingTxn := TxnRecord{
		Op:        "rm",
		TxnID:     "00112233445566778899aabbccddeeff",
		Sequence:  1,
		Name:      "alpha",
		StartHead: "cafef00d",
	}
	for _, record := range []TxnRecord{pendingTxn, pendingAdopt} {
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		txnPath := filepath.Join(s.RecoveryDir(), txnRecordName(record))
		if err := os.WriteFile(txnPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := report.Recovery
	// Nothing here is Collectable, and that is the finding rather than a gap
	// in the fixture: every name in it is either claimed by a pending
	// transaction, frozen behind a pending exchange, kept on purpose, or --
	// for the removed-, .fu-retired-dir- and rollback- entries -- described by
	// no journal family at all, which is the state `fu gc` walks straight
	// past. An earlier expectation of Collectable: 2 counted the first two of
	// those as gc's work, so `fu status` told the user to run a command and
	// watch the count stay where it was.
	if want := (RecoveryInventory{Collectable: 0, Blocked: 2, Retained: 2, Uncollectable: 5}); got != want {
		t.Fatalf("recovery inventory = %+v, want %+v", got, want)
	}
	// PendingTxns orders by (op, id); "adopt" sorts before "rm".
	wantPending := []PendingOperation{{Op: "adopt", Name: "beta"}, {Op: "rm", Name: "alpha"}}
	if len(report.Store.Pending) != len(wantPending) {
		t.Fatalf("both pending transactions must be reported in Store.Pending: got %+v, want %+v", report.Store.Pending, wantPending)
	}
	for i, want := range wantPending {
		if report.Store.Pending[i] != want {
			t.Fatalf("Store.Pending[%d] = %+v, want %+v (full: %+v)", i, report.Store.Pending[i], want, report.Store.Pending)
		}
	}
}

// TestStatusInventoriesStagingByWhatTheUserCanDo pins the staging half of the
// same split-by-remedy. It has three buckets where recovery has four, and the
// missing one is missing for a reason: staging holds no authority SPEC §9
// promises, so nothing there is kept on purpose. Nothing is collectable in
// recovery's sense either -- `fu gc` never looks at staging at all -- so that
// bucket carries a different distinction here: an entry a recovery pass
// settles, one nothing settles ever, and one whose remedy belongs to whoever
// put it there.
//
// Every cleanup path in staging is an in-process defer (source/scratch.go's
// constructor cleanup and Close, ownedtree.go's reservation cleanup), which is
// precisely why a process exit strands these names: no later run enumerates the
// directory to finish the job.
func TestStatusInventoriesStagingByWhatTheUserCanDo(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	// The reservation name is read back off the pending record rather than
	// hand-matched below, so the fixture cannot drift from the name recovery
	// actually looks for.
	reservation := store.StagedRootReservation{
		Name: ".fu-new-00112233445566778899aabbccddeeff",
		Manifest: store.OwnedTree{
			RootIdentity: store.FileIdentity{Device: 1, Inode: 2},
			RootMode:     uint32(os.ModeDir | 0o700),
		},
	}
	pendingInstall := TxnRecord{
		Op:                 "new",
		TxnID:              "00112233445566778899aabbccddeeff",
		Sequence:           1,
		Name:               "alpha",
		StartHead:          "cafef00d",
		StagingReservation: &reservation,
	}
	raw, err := json.Marshal(pendingInstall)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), txnRecordName(pendingInstall)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// A config exchange record with no .done marker beside it, so its candidate
	// and the active swap name are both what RecoverConfigExchanges settles.
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), ".fu-config-exchange-0011223344556677.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, dir := range map[string]bool{
		// Settled by a recovery pass.
		"alpha":                                 true,
		reservation.Name:                        true,
		".fu-config-candidate-0011223344556677": false,
		".fu-config-swap":                       false,
		// Settled by nothing.
		".fu-src-00112233445566778899aabbccddeeff":             true,
		".fu-src-orphan-00112233445566778899aabbccddeeff":      true,
		".fu-src-clean-00112233445566778899aabbccddeeff":       true,
		".fu-retired-staging-00112233445566778899aabbccddeeff": true,
		// A private reservation name no pending record claims: the crash that
		// left it landed before the reservation reached the journal, so nothing
		// can attribute it back to fu.
		".fu-new-ffeeddccbbaa99887766554433221100": true,
	} {
		path := filepath.Join(s.StagingDir(), name)
		var mkErr error
		if dir {
			mkErr = os.MkdirAll(path, 0o755)
		} else {
			mkErr = os.WriteFile(path, []byte("{}"), 0o600)
		}
		if mkErr != nil {
			t.Fatal(mkErr)
		}
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (StagingInventory{Blocked: 4, Uncollectable: 5}); report.Staging != want {
		t.Fatalf("staging inventory = %+v, want %+v", report.Staging, want)
	}
}

// TestApplicationStatusKeepsThePartialReportBesideTheError pins the whole
// point of reading the store-side facts after the agents: a damaged journal
// family costs the user that one section, not the report. `fu status` is the
// command you run *because* the store looks damaged, and `fu gc` already
// isolates the same damage per family and still reports what it did. Returning
// a zero StatusOutcome here threw away everything Status had already
// assembled, plus the diagnostics every read command owes its caller.
func TestApplicationStatusKeepsThePartialReportBesideTheError(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := NewApplication()
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.NewSkill("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, ".claude", "skills", "alpha")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	// The established damaged-family fixture (gc_test.go): a txn- name no
	// parse can make sense of, which fails the journal scan PendingTxns runs.
	if err := os.WriteFile(filepath.Join(st.RecoveryDir(), "txn-rm-not-a-record.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := app.Status()
	if err == nil {
		t.Fatal("a damaged journal family must still be reported as an error")
	}
	var drifted bool
	for _, agentStatus := range outcome.Report.Agents {
		for _, action := range agentStatus.Drift {
			if action.Skill == "alpha" {
				drifted = true
			}
		}
	}
	if !drifted {
		t.Fatalf("the agent half of the report must survive the store half's failure: %+v", outcome.Report)
	}
	if outcome.Diagnostics.ConfigPath == "" {
		t.Fatalf("the diagnostics every read command carries must survive too: %+v", outcome.Diagnostics)
	}
}

// TestStatusExcludesARemovedPayloadAPendingTransactionStillClaims is the
// removed- half of the exclusion TestStatusInventoriesRecoveryByWhatTheUserCanDo
// pins for rollback-. `fu gc` leaves a removed- payload alone while a pending
// transaction still derives its name (txn_prune.go), so counting it collectable
// tells a user to run a command that will collect nothing -- and says so
// alongside the "unfinished rm alpha" line, which reports the same one fact in
// incompatible terms. The payload name here is derived through rmPayloadName,
// the same derivation the classifier under test uses, so the fixture cannot
// drift from it.
func TestStatusExcludesARemovedPayloadAPendingTransactionStillClaims(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	pendingRm := TxnRecord{
		Op:        "rm",
		TxnID:     "00112233445566778899aabbccddeeff",
		Sequence:  1,
		Name:      "alpha",
		StartHead: "deadbeef",
	}
	raw, err := json.Marshal(pendingRm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), txnRecordName(pendingRm)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.RecoveryDir(), rmPayloadName(pendingRm)), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Recovery != (RecoveryInventory{}) {
		t.Fatalf("a claimed removed- payload is the pending transaction Store.Pending already reports, not inventory: %+v", report.Recovery)
	}
	if len(report.Store.Pending) != 1 || report.Store.Pending[0] != (PendingOperation{Op: "rm", Name: "alpha"}) {
		t.Fatalf("the claiming transaction must still be reported: %+v", report.Store.Pending)
	}
}

// TestStatusCountsRecoveryWorkWaitingOnARecoveryPassApartFromCollectable pins
// the second state `fu gc` is responsible for but cannot act on: a config
// exchange record whose completion marker never landed. Only
// RecoverConfigExchanges finishes or withdraws it, and that runs from
// RecoverPending -- a write command's first step, and `fu restore`'s -- never
// from `fu gc`. While one is pending, ReclaimCompletedConfigExchanges also
// declines to sweep any .fu-config-archive- name at all, so the archives are
// frozen behind it. Counting either as collectable sends the user to a command
// that collects nothing.
func TestStatusCountsRecoveryWorkWaitingOnARecoveryPassApartFromCollectable(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	record := ".fu-config-exchange-0011223344556677.json"
	marker := ".fu-config-exchange-0011223344556677.done"
	archive := ".fu-config-archive-0011223344556677-8899aabbccddeeff"
	for _, name := range []string{record, archive} {
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecoveryInventory{Blocked: 2}); report.Recovery != want {
		t.Fatalf("a pending exchange freezes itself and the archives: got %+v, want %+v", report.Recovery, want)
	}

	// The marker is the whole difference: with it the record is finished, so
	// gc collects the pair and the archive sweep runs again.
	//
	// The archive still does not become collectable, and that is correct
	// rather than a leftover freeze. reclaimConfigExchangeStatedArchive
	// unlinks a name only while it resolves to the exact device/inode the name
	// states, and this fixture's name is hand-written, so it states an
	// identity the file it was written to does not have -- the same shape as a
	// real archive whose inode has since drifted, which
	// TestReclaimCompletedConfigExchangesPreservesAnArchiveNameItDoesNotDescribe
	// pins gc as preserving forever. Counting it Collectable claimed a
	// collection that provably never happens.
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), marker), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecoveryInventory{Collectable: 2, Uncollectable: 1}); report.Recovery != want {
		t.Fatalf("a finished exchange is gc's; an archive it does not describe is not: got %+v, want %+v", report.Recovery, want)
	}
}

// TestStatusReportsAMissingLinkWhoseStoreContentIsAlsoGoneTheWayRestoreDoes
// pins the other half of the defect commit 943b147 fixed once. That commit
// made status report a *broken* fu link as the single thing that is wrong; the
// state where the link is absent and the store content is absent too was never
// covered, so Diff produced a bare CreateLink and driftLabel rendered it
// "missing link" -- which tells the reader `fu restore` will create it.
//
// It will not. Run against the same state, restore says "alpha is enabled but
// the store no longer holds its content", which is the accurate wording and
// already exists as ReportMissing. Two commands describing one state in
// incompatible terms is what this pins shut, with the accurate one winning.
func TestStatusReportsAMissingLinkWhoseStoreContentIsAlsoGoneTheWayRestoreDoes(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	// Both halves gone: the link, and the store content it would point at.
	if err := os.Remove(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	drift := report.Agents[0].Drift
	if len(drift) != 1 || drift[0].Skill != "alpha" {
		t.Fatalf("drift = %+v, want a single finding for alpha", drift)
	}
	if drift[0].Type != ReportMissing {
		t.Fatalf("drift[0].Type = %v, want ReportMissing -- restore cannot create a link to content that is gone", drift[0].Type)
	}
}

// TestStatusReportsAMissingLinkWhoseStorePathIsNotADirectory is the same
// finding through the other door reconcile already treats as Missing
// (reconcile.go): the store-side path exists but is a plain file, so there is
// no skill directory to link to.
func TestStatusReportsAMissingLinkWhoseStorePathIsNotADirectory(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	drift := report.Agents[0].Drift
	if len(drift) != 1 || drift[0].Type != ReportMissing {
		t.Fatalf("drift = %+v, want a single ReportMissing for alpha", drift)
	}
}

// TestStatusCountsRecoveryObjectsNoCompletedFamilyDescribesAsUncollectable
// pins the inventory against what `fu gc` actually collects.
//
// gc reclaims a removed- payload in exactly one place: while pruning the
// completed, unpruned family whose manifest describes it (txn_prune.go). A
// .fu-retired-dir- name is resumed from the same manifest. With no family on
// disk, gc walks past both and the count never moves -- which is the precise
// outcome DESIGN §108 and status.go's own bucket comment say the buckets exist
// to prevent: "run this command and watch a number not change".
//
// The state is reachable by following fu's own advice.
// addRecoveryConflictRemedy (txn.go) tells a user to move a damaged
// transaction's whole journal family out of recovery/, and doing so strands
// exactly these payloads. The author had already worked this through for
// rollback- and counted it Uncollectable, naming that remedy in the comment;
// this is the same reasoning on the two prefixes it was not carried to.
func TestStatusCountsRecoveryObjectsNoCompletedFamilyDescribesAsUncollectable(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// Shaped like a real payload name, but with no journal -- pending or
	// completed -- anywhere in recovery/ that derives it.
	orphan := TxnRecord{
		Op:        "rm",
		TxnID:     "ffeeddccbbaa99887766554433221100",
		Sequence:  1,
		Name:      "alpha",
		StartHead: "cafebabe",
	}
	if err := os.MkdirAll(filepath.Join(s.RecoveryDir(), rmPayloadName(orphan)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.RecoveryDir(), ".fu-retired-dir-001122334455"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecoveryInventory{Uncollectable: 2}); report.Recovery != want {
		t.Fatalf("nothing collects these, so `fu gc` must not be offered: got %+v, want %+v", report.Recovery, want)
	}
}

// TestStatusCountsARemovedPayloadACompletedFamilyDescribesAsCollectable is the
// positive half: with the completed family present, gc really will reclaim the
// payload on its next run, so Collectable is the honest bucket. Without this,
// the fix above could be "call everything uncollectable", which would be just
// as wrong in the other direction.
func TestStatusCountsARemovedPayloadACompletedFamilyDescribesAsCollectable(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	// A completed rm leaves its journal family behind for `fu gc`; the inline
	// reclaim already took the payload, so put one back under the name that
	// family's manifest describes.
	journal, err := scanTxnJournalReport(s)
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	for key, revisions := range journal.revisions {
		latest, err := validateTxnChain(s, key, revisions)
		if err != nil {
			t.Fatal(err)
		}
		if latest.Op == "rm" {
			payload = rmPayloadName(latest)
		}
	}
	if payload == "" {
		t.Fatal("precondition: the completed rm family must be on disk")
	}
	delta, err := recoveryInventoryDelta(t, s, cfg, func() {
		if err := os.MkdirAll(filepath.Join(s.RecoveryDir(), payload), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecoveryInventory{Collectable: 1}); delta != want {
		t.Fatalf("a payload the completed family describes is gc's to collect: got %+v, want %+v", delta, want)
	}
}

// TestStatusIsolatesAFailedSectionFromTheRestOfTheReport carries commit
// 868e83d's standard -- "let a damaged store cost `fu status` one section, not
// the report" -- to the three store-side sections, where it had only ever been
// met at the agents/store boundary.
//
// Each of the store, recovery and staging sections returned on its first
// error, so one damaged journal family took all three down together and the
// output was a single agents section plus an error, with no line anywhere
// saying that the recovery and staging inventories had simply not been taken.
// The two local inventories have nothing to do with git, and the git-side read
// has nothing to do with either of them.
func TestStatusIsolatesAFailedSectionFromTheRestOfTheReport(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// A txn- name PendingTxns cannot parse: the pending-transaction section
	// fails, and nothing else in the report depends on it.
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), "txn-not-a-valid-name.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Something for the recovery inventory to find, so an empty count cannot
	// be mistaken for a section that ran.
	if err := os.MkdirAll(filepath.Join(s.RecoveryDir(), "adopt-archive-claude-alpha-00112233"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, nil)
	if err == nil {
		t.Fatal("a damaged journal must still be reported as an error")
	}
	if report.Recovery.Retained != 1 {
		t.Fatalf("the recovery inventory must still be taken: %+v", report.Recovery)
	}
}

// TestStatusCommitsNothingEvenWithADirtyStore is one of design §7's two
// read-only guards, and the one that had no test. cli/status_test.go's
// before/after log comparison runs on a clean store, where a sweep would have
// nothing to commit either way, so it cannot fail; engine/status_test.go's
// TestStatusReportsAnUncommittedStoreEdit does dirty the store but never looks
// at the commit count.
func TestStatusCommitsNothingEvenWithADirtyStore(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	beforeLog, err := s.Log(50)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Store.DirtyPaths) == 0 {
		t.Fatal("precondition: the hand edit must be visible as dirty")
	}

	after, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatalf("status must not move HEAD: %s -> %s", before.Hash(), after.Hash())
	}
	afterLog, err := s.Log(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeLog) != len(afterLog) {
		t.Fatalf("status must add no commit: %d -> %d", len(beforeLog), len(afterLog))
	}
	// Still dirty afterwards: the edit was reported, not swept.
	dirty, err := s.ChangedPathsIncludingIgnored()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) == 0 {
		t.Fatal("status must leave the edit uncommitted, not fold it into history")
	}
}

// TestStatusTakesNoLock pins design §3's deliberate decision. A read-only
// command must not block behind a long write, and the cost -- a report that a
// concurrent write may already have moved past -- was accepted explicitly.
// Nothing enforced it: wrapping Status in withLock would reintroduce exactly
// the blocking the design refused, and no test would have failed.
//
// The lock is held for the duration of the call, so a Status that tried to
// take it would block rather than return, which is why this runs with a
// timeout instead of asserting on an error.
func TestStatusTakesNoLock(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	homeRoot, err := s.Root()
	if err != nil {
		// Root needs a write session; take one just for the lock below.
		session, beginErr := s.BeginWrite()
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() {
			if err := session.Close(); err != nil {
				t.Error(err)
			}
		}()
		homeRoot, err = session.Store.Root()
		if err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	if err := withLock(homeRoot, "fu.lock", s.LockPath(), func() error {
		go func() {
			_, statusErr := Status(s, cfg, nil)
			done <- statusErr
		}()
		select {
		case statusErr := <-done:
			return statusErr
		case <-time.After(10 * time.Second):
			return errors.New("status blocked on fu.lock; a read-only command must not take it (design §3)")
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStatusReportsARealDirectoryOccupyingASkillName fills the one cell design
// §7's drift matrix lists and no test covered: the desired link's name is
// taken by an ordinary directory rather than by a symlink.
//
// It is the case a user creates by hand -- copying a skill into the agent
// directory instead of letting fu link it -- and the one where reconcile's
// "never touch unmanaged content" rule does the most work, since the wrong
// answer here would delete real content. The output was confirmed by hand to
// be "occupied by unmanaged content"; nothing pinned it.
func TestStatusReportsARealDirectoryOccupyingASkillName(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	// Replace fu's link with a real directory holding real content.
	if err := os.Remove(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "SKILL.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, agents)
	if err != nil {
		t.Fatal(err)
	}
	drift := report.Agents[0].Drift
	if len(drift) != 1 || drift[0].Type != ReportConflict || drift[0].Skill != "alpha" {
		t.Fatalf("a real directory on the name is unmanaged content, not a link to repair: %+v", drift)
	}
	// Read-only means read-only: the occupying content must be exactly as the
	// user left it.
	if got, err := os.ReadFile(filepath.Join(occupied, "SKILL.md")); err != nil || string(got) != "mine" {
		t.Fatalf("status must not touch unmanaged content: %q %v", got, err)
	}
}

// TestStatusCountsUnmatchedStagingNames pins the third staging bucket, which
// was delivered with no failing test behind it: deleting the bucket and its
// output line left the whole cli package and every engine Status test green,
// because TestStatusInventoriesStagingByWhatTheUserCanDo's nine fixtures all
// land in the other two.
//
// The bucket exists for the user who has just been refused by `fu new` or
// `fu add` and opens `fu status` to see what is occupying the name -- and who
// previously saw nothing at all, because an unclaimed public name was
// deliberately not counted.
func TestStatusCountsUnmatchedStagingNames(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// A public name no pending record claims and no residue prefix matches.
	if err := os.MkdirAll(filepath.Join(s.StagingDir(), "leftover-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Beside it, one entry from each of the other two buckets, so this asserts
	// a real three-way split rather than "everything lands in Unmatched".
	if err := os.MkdirAll(filepath.Join(s.StagingDir(), ".fu-src-00112233"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (StagingInventory{Uncollectable: 1, Unmatched: 1}); report.Staging != want {
		t.Fatalf("staging inventory = %+v, want %+v", report.Staging, want)
	}
}

// TestStatusDoesNotCallMalformedExchangeNamesCollectable pins the inventory to
// what `fu gc` will really unlink on the record/marker side.
//
// "Not pending" and "collectable" are not complements. The pending scan uses a
// loose grammar (prefix + ".json", hex unchecked); the collector uses a strict
// one (exactly 16 hex digits, round-tripped). Everything carrying the prefix
// but failing the strict form was counted Collectable and then walked past by
// every gc run -- reproduced end to end: three such names, `fu gc` reports
// "nothing to prune", the count never moves.
//
// Reachable by hand (a user moves a record aside; an editor leaves a .bak),
// which is the same argument that hardened the neighbouring archive case in
// the previous round. The .tmp- entry is here too: it is fu's own residue that
// nothing collects, and it used to fall through the switch entirely, letting
// `fu status` claim "nothing to report" over a directory the design itself
// records as dirty.
func TestStatusDoesNotCallMalformedExchangeNamesCollectable(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		".fu-config-exchange-0011223344556677.json.bak",
		".fu-config-exchange-nothex.json",
		".fu-config-exchange-nothex.done",
		".tmp-00112233445566778899aabbccddeeff",
	} {
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Recovery.Collectable != 0 {
		t.Fatalf("`fu gc` collects none of these, so none may be offered to it: %+v", report.Recovery)
	}
	if report.Recovery.Uncollectable != 4 {
		t.Fatalf("every name must still be accounted for rather than vanishing: %+v", report.Recovery)
	}
}

// TestStatusCountsAWellFormedCompletedExchangeAsCollectable is the positive
// half, so the fix above cannot degenerate into "call everything
// uncollectable": a record beside its own well-formed marker really is gc's to
// collect on its next run.
func TestStatusCountsAWellFormedCompletedExchangeAsCollectable(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		".fu-config-exchange-0011223344556677.json",
		".fu-config-exchange-0011223344556677.done",
	} {
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecoveryInventory{Collectable: 2}); report.Recovery != want {
		t.Fatalf("recovery inventory = %+v, want %+v", report.Recovery, want)
	}
}

// TestStatusCountsARetiredRootItsManifestDescribesAsCollectable closes the
// positive direction of one of two derivations that only had their negative
// half tested: deleting the RetiredRecoveryRootName entry from
// collectableRecoveryNames left every status test green.
//
// That mutation moves a name gc really does collect into "no command collects
// this yet" -- the quieter half of the same false-report family the
// Collectable bucket exists to prevent, and the one that would push a reader
// toward deleting a resumable intermediate by hand. The archive derivation's
// positive half is pinned in the store package, beside the naming function it
// depends on (TestCollectableConfigArchiveNamesAdmitsANameItDescribes).
func TestStatusCountsARetiredRootItsManifestDescribesAsCollectable(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	journal, err := scanTxnJournalReport(s)
	if err != nil {
		t.Fatal(err)
	}
	var retired string
	for _, revisions := range journal.revisions {
		latest, err := newestTxnRevision(s, revisions)
		if err != nil || latest.Op != "rm" || latest.Payload == nil {
			continue
		}
		retired = store.RetiredRecoveryRootName(rmPayloadName(latest), *latest.Payload)
	}
	if retired == "" {
		t.Fatal("precondition: the completed rm family must be on disk")
	}
	delta, err := recoveryInventoryDelta(t, s, cfg, func() {
		if err := os.MkdirAll(filepath.Join(s.RecoveryDir(), retired), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecoveryInventory{Collectable: 1}); delta != want {
		t.Fatalf("a retired root the manifest still describes is resumable, so Collectable: got %+v, want %+v", delta, want)
	}
}

// TestStatusDescribesNoPerEntryDriftForASymlinkedAgentDir pins the guard that
// makes the CLI's suppression argument true.
//
// Reconcile refuses a symlinked skills directory wholesale (SPEC rule 10), so
// there is no per-entry work to describe and describing it anyway would list
// work fu will never do. The guard is not cosmetic: ScanAgent returns no
// Entries for such a directory, Diff therefore answers every enabled skill
// with CreateLink, and storeSideMissing upgrades a CreateLink whose store-side
// target is absent to ReportMissing -- which printAgentSection deliberately
// does *not* suppress, on the stated premise that ReportMissing is a store-side
// fact unrelated to the agent's directory. Without this guard that premise
// fails exactly here, and `fu status` would report a skill as gone from the
// store because an unrelated agent's directory is a symlink, while `fu restore`
// over the same state stays silent.
func TestStatusDescribesNoPerEntryDriftForASymlinkedAgentDir(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	// fu.yaml names alpha, but the store holds no content for it: the state in
	// which the CreateLink Diff would emit becomes ReportMissing.
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linkdir")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, []agent.Agent{fakeAgent{"claude", link}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Agents) != 1 || !report.Agents[0].DirIsSymlink {
		t.Fatalf("the fixture must produce a symlinked skills dir: %+v", report.Agents)
	}
	for _, action := range report.Agents[0].Drift {
		if action.Type == CreateLink || action.Type == RemoveLink || action.Type == ReportMissing {
			t.Fatalf("a refused agent must carry no per-entry projection drift, got %+v", report.Agents[0].Drift)
		}
	}
}

// TestStatusStopsOfferingGCForAFamilyItWillNotPrune pins both disjuncts of
// collectableRecoveryNames' skip condition, which
// TestStatusCountsARemovedPayloadACompletedFamilyDescribesAsCollectable above
// reaches only in its true-for-both form.
//
// Each disjunct is a state `fu gc` really refuses. A family carrying a prune
// record has already had its reclaim step run to a conclusion, so the loop
// resumes the deletion and reclaims nothing (txn_prune.go); an invalid family
// is skipped outright with a repair remedy. Counting either payload Collectable
// is the false promise this whole derivation exists to prevent: a remedy the
// user runs and watches a count not move.
func TestStatusStopsOfferingGCForAFamilyItWillNotPrune(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, s *store.Store, key txnKey)
	}{
		{
			// gc resumes such a family and deliberately reclaims nothing: the
			// prune record is written strictly after the payload was settled.
			name: "already pruned",
			stage: func(t *testing.T, s *store.Store, key txnKey) {
				t.Helper()
				name := txnPruneName(txnPrune{Op: key.op, TxnID: key.id}, []byte("resumed"))
				if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("resumed"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Two prune records for one transaction: the family lands in
			// journal.invalid, and gc skips it with a repair remedy.
			name: "invalid family",
			stage: func(t *testing.T, s *store.Store, key txnKey) {
				t.Helper()
				for _, raw := range [][]byte{[]byte("one"), []byte("two")} {
					name := txnPruneName(txnPrune{Op: key.op, TxnID: key.id}, raw)
					if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), raw, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupStore(t)
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatal(err)
			}
			if _, err := RemoveSkill(s, nil, "alpha"); err != nil {
				t.Fatal(err)
			}
			journal, err := scanTxnJournalReport(s)
			if err != nil {
				t.Fatal(err)
			}
			var rmKey txnKey
			var payload string
			for key, revisions := range journal.revisions {
				latest, err := validateTxnChain(s, key, revisions)
				if err != nil {
					t.Fatal(err)
				}
				if latest.Op == "rm" {
					rmKey, payload = key, rmPayloadName(latest)
				}
			}
			if payload == "" {
				t.Fatal("precondition: the completed rm family must be on disk")
			}
			// The family is put into the state under test first, so the
			// delta below isolates the payload alone.
			tc.stage(t, s, rmKey)
			delta, _ := recoveryInventoryDelta(t, s, cfg, func() {
				// The payload the manifest describes, back under its own name
				// -- the same fixture the positive case uses, so the only
				// difference between them is the family's state.
				if err := os.MkdirAll(filepath.Join(s.RecoveryDir(), payload), 0o755); err != nil {
					t.Fatal(err)
				}
			})
			if want := (RecoveryInventory{Uncollectable: 1}); delta != want {
				t.Fatalf("gc will not prune this family, so its payload may not be offered to `fu gc`: got %+v, want %+v", delta, want)
			}
		})
	}
}

// TestStatusCountsTheJournalBacklogTheWayGCWill closes the report's largest
// blind spot: the completed, unpruned transaction families that are by far the
// most common thing accumulating under recovery/.
//
// They used to be excluded on the grounds that the unfinished-transaction
// section and gc's own pruning each had them covered. Neither did -- that
// section shows *pending* transactions only -- so a single `fu new` left nine
// files that `fu status` said nothing about, under an unqualified "nothing to
// report: what fu.yaml asks for is what is on disk", immediately before `fu gc`
// removed all nine.
//
// The assertion is agreement with gc rather than a fixed count, because the
// count is a property of the fixture and the agreement is the property of the
// code: whatever `fu status` offers to `fu gc` must be what `fu gc` then takes,
// and running out of collectables must be visible as the section going away.
func TestStatusCountsTheJournalBacklogTheWayGCWill(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}

	before, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Recovery.Collectable == 0 {
		t.Fatalf("a completed, unpruned family is exactly what `fu gc` prunes next: %+v", before.Recovery)
	}

	outcome, err := PruneCompletedTransactions(s)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Files != before.Recovery.Collectable {
		t.Fatalf("status offered %d collectable and gc removed %d; the two must agree",
			before.Recovery.Collectable, outcome.Files)
	}

	after, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.Recovery != (RecoveryInventory{}) {
		t.Fatalf("with the backlog gone the section must go with it: %+v", after.Recovery)
	}
}

// TestStatusLeavesAPendingFamilysJournalToTheUnfinishedSection pins the other
// side of the rule above. A pending family's files are not collectable -- gc's
// prune loop never visits it -- and report.Store.Pending already names that
// transaction by op and skill, so counting its journal here would state one
// fact twice in incompatible terms: "unfinished, the next write command settles
// it" beside "collectable, run `fu gc`", where gc collects nothing.
func TestStatusLeavesAPendingFamilysJournalToTheUnfinishedSection(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	stop := errors.New("stop at archive boundary")
	_, _ = adopt(s, []agent.Agent{agent.Claude{}}, "", hooks{beforeAdoptRetire: func() error { return stop }})

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Store.Pending) != 1 {
		t.Fatalf("the fixture must leave exactly one pending transaction: %+v", report.Store.Pending)
	}
	if report.Recovery.Collectable != 0 {
		t.Fatalf("a pending family is the unfinished section's to report, not `fu gc`'s: %+v", report.Recovery)
	}
}

// TestStatusAccountsForATxnNameNoFamilyClaims covers the residue inside the
// txn- prefix: a name the journal scan resolves to no family at all. fu writes
// only three shapes there (.json revisions, .done markers, .pruned records),
// so anything else is a hand-dropped file -- and before the switch consulted
// the scan, a bare prefix test swallowed it into no bucket, leaving `fu status`
// to call a directory holding it clean.
func TestStatusAccountsForATxnNameNoFamilyClaims(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	delta, err := recoveryInventoryDelta(t, s, cfg, func() {
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), "txn-leftover.txt"), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecoveryInventory{Uncollectable: 1}); delta != want {
		t.Fatalf("a txn- name no family claims must still be accounted for: got %+v, want %+v", delta, want)
	}
}

// TestStatusAccountsForADamagedFamilysJournalFiles covers the other residue
// inside the txn- prefix (review round 27 finding 1): a name the scan does
// attribute to a family, but to a family it found *invalid*.
//
// collectableRecoveryNames drops such a family from every collectable set --
// `fu gc`'s prune loop skips it with a repair remedy rather than deleting
// anything -- and its own doc comment names the intended answer: "Being unable
// to prove a collector exists is the Uncollectable answer." The attributed set
// reached the name first, though, and that arm counts nothing on the grounds
// that a *pending* family is already reported by op and skill. A damaged
// family is not, so its files fell out of the inventory entirely.
//
// Two prune records sharing one transaction's op and ID is the shape the scan
// calls invalid. Only the first is kept in journal.pruned, so only the first is
// attributed to the family; the second resolves to no family and already
// counted Uncollectable, which is exactly why this asserts on the count moving
// from one to two rather than on it being non-zero.
func TestStatusAccountsForADamagedFamilysJournalFiles(t *testing.T) {
	s, _ := setupStore(t)
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// txn-<op>-<id>-<digest>.pruned, per parseTxnPruneName: a 32-hex ID and a
	// 64-hex digest. The two names differ only in the digest, so both parse to
	// the same key and the second is what makes the family invalid.
	id := strings.Repeat("a", txnIDHexLength)
	first := fmt.Sprintf("txn-rm-%s-%s.pruned", id, strings.Repeat("b", txnDigestHexWidth))
	second := fmt.Sprintf("txn-rm-%s-%s.pruned", id, strings.Repeat("c", txnDigestHexWidth))

	delta, _ := recoveryInventoryDelta(t, s, cfg, func() {
		for _, name := range []string{first, second} {
			if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	// The joined Status error is ignored deliberately: this fixture damages a
	// family on purpose, so PendingTxns reports it, and recoveryInventoryDelta's
	// own comment says a caller doing that ignores the error. The bucketing is
	// what is under test.
	if want := (RecoveryInventory{Uncollectable: 2}); delta != want {
		t.Fatalf("a damaged family's journal files must be accounted for, not dropped: got %+v, want %+v", delta, want)
	}
}

// TestStatusReportsAnUnreadableJournalOnce pins the deduplication (review
// round 27 finding 2). PendingTxns and the collectable derivation both need the
// transaction journal, and each used to scan recovery/ for itself -- so one
// unreadable directory produced two problem lines with the same root cause,
// worst in exactly the case a user runs `fu status` to diagnose.
//
// errors.Is could not have removed it and the fix does not try: two independent
// reads produce two distinct *os.PathError values, which compare unequal. One
// scan feeding both derivations is what makes a single failure a single line.
//
// A plain file where the directory belongs, rather than chmod 000: it fails the
// read the same way and does not depend on the test process being unprivileged.
func TestStatusReportsAnUnreadableJournalOnce(t *testing.T) {
	s, cfg := setupStore(t)
	if err := os.RemoveAll(s.RecoveryDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.RecoveryDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Status(s, cfg, nil)
	if err == nil {
		t.Fatal("an unreadable recovery directory must be reported, not swallowed")
	}
	// Counted by the directory the lines name rather than by their wording: the
	// user-facing property is "one failure, one line", and the three reads that
	// used to produce three lines each worded it differently. Before the fix
	// this store reported the identical "not a directory" three times -- the
	// journal scan under two wrappers, then the inventory listing under a third.
	var lines int
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.Contains(line, s.RecoveryDir()) {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("one unreadable recovery directory must produce one problem line, got %d in:\n%s", lines, err)
	}
}

// recoveryInventoryDelta reports how one added recovery-directory entry changed
// the inventory, which is the only way a payload-bucketing test can say
// anything precise now that a settled journal family is itself Collectable.
//
// A fixture built from `fu new` + `fu rm` leaves two settled families behind,
// so the absolute Collectable count is dominated by their journal files and an
// assertion of "== 1" pins the fixture's file count rather than the rule under
// test. The delta isolates the one name the test placed.
// The joined error of both reads comes back rather than being swallowed: a
// caller whose fixture is meant to be intact asserts it is nil, and a caller
// that deliberately damaged a family ignores it, since that damage is reported
// by its own section and this helper is about bucketing alone.
func recoveryInventoryDelta(t *testing.T, s *store.Store, cfg *store.Config, place func()) (RecoveryInventory, error) {
	t.Helper()
	before, beforeErr := Status(s, cfg, nil)
	place()
	after, afterErr := Status(s, cfg, nil)
	return RecoveryInventory{
		Collectable:   after.Recovery.Collectable - before.Recovery.Collectable,
		Blocked:       after.Recovery.Blocked - before.Recovery.Blocked,
		Retained:      after.Recovery.Retained - before.Recovery.Retained,
		Uncollectable: after.Recovery.Uncollectable - before.Recovery.Uncollectable,
	}, errors.Join(beforeErr, afterErr)
}
