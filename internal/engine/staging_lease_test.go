package engine

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
)

func leasesUnder(t *testing.T, staging string) []string {
	t.Helper()
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), store.LeasePrefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// The WAL cannot cover its own beginning: a transaction record cannot name a
// staged root before the root exists, so the stretch between creating one and
// journalling it is the one place a crash leaves a private root no record
// mentions. A lease accounts for exactly that stretch -- and hands over the
// moment the journal can speak for the name, because two owners would be one
// too many.
func TestStagedRootLeaseCoversOnlyTheWindowTheJournalCannot(t *testing.T) {
	s, _ := setupStore(t)
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store
	staging := checked.StagingDir()

	reservation, lease, err := checked.ReserveStagedRootOwned(0o755)
	if err != nil {
		t.Fatal(err)
	}
	// Before the journal write: the lease is the only account of this root.
	if got := leasesUnder(t, staging); len(got) != 1 {
		t.Fatalf("leases = %v, want the reservation accounted for", got)
	}
	findings, err := store.ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].State != store.LeaseInUse {
		t.Fatalf("findings = %+v, want the live reservation reported as in use", findings)
	}
	if _, err := os.Lstat(filepath.Join(staging, reservation.Name)); err != nil {
		t.Fatalf("precondition: the private root must exist: %v", err)
	}

	// After the handover the journal owns the name, so the lease must be gone
	// rather than a second claim on it.
	if err := lease.Release(staging); err != nil {
		t.Fatal(err)
	}
	if got := leasesUnder(t, staging); len(got) != 0 {
		t.Fatalf("leases = %v, want the lease released once the journal can name the root", got)
	}
}

// The whole pipeline leaves no lease behind: every one that is taken is handed
// over or released, so a completed command's staging area is as clean as it
// was before.
func TestWriteCommandLeavesNoLeaseBehind(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}

	if got := leasesUnder(t, s.StagingDir()); len(got) != 0 {
		t.Fatalf("leases = %v, want none after a completed write command", got)
	}
}

// status must promise exactly what gc performs. The two reach their verdict
// through one classifier, and this is the end-to-end check that they agree on
// a directory holding all three kinds at once.
func TestStatusPromisesWhatGcThenReclaims(t *testing.T) {
	s, cfg := setupStore(t)
	staging := s.StagingDir()

	// Abandoned: reclaimable.
	dead := leaseFixture(t, staging, ".fu-src-", true)
	dead.Abandon()
	// Held by a live holder: left alone.
	live := leaseFixture(t, staging, ".fu-src-", true)
	defer live.Release(staging)
	// Substituted: fu cannot account for it, so it stays.
	swapped := leaseFixture(t, staging, ".fu-src-", true)
	swappedName := swapped.Name()
	if err := os.Remove(filepath.Join(staging, swappedName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, swappedName), []byte("not fu's"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapped.Abandon()

	before, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The inventory counts entries under staging, and an object and its lease
	// are two of them -- so three leased objects make six entries, two in each
	// bucket. Counting the record beside the thing it explains is the point:
	// a lease is not residue, and burying it in the bucket for things nothing
	// collects would describe the evidence as the problem.
	if len(before) != 6 {
		t.Fatalf("fixture: want three objects and three leases, found %v", before)
	}
	// In use is its own bucket, not Blocked: those two have opposite remedies.
	// Blocked waits on a recovery pass the user can run; in use waits on
	// another process, and telling that user to run `fu restore` sends them to
	// do nothing.
	if report.Staging.Collectable != 2 || report.Staging.InUse != 2 || report.Staging.Uncollectable != 2 {
		t.Fatalf("staging = %+v, want two entries in each of collectable, in-use and uncollectable", report.Staging)
	}
	if report.Staging.Blocked != 0 {
		t.Fatalf("staging = %+v: nothing here waits on a recovery pass", report.Staging)
	}

	outcome, err := reclaimStagingLeases(s.Home, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The promise and the performance, stated in the one unit both speak:
	// entries removed, and entries left.
	removed := outcome.Payloads + outcome.Leases
	if removed != report.Staging.Collectable {
		t.Fatalf("gc removed %d entries, status promised %d collectable", removed, report.Staging.Collectable)
	}
	after, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	left := report.Staging.Blocked + report.Staging.InUse + report.Staging.Uncollectable + report.Staging.Unmatched
	if len(after) != left {
		t.Fatalf("gc left %d entries, status promised %d would remain", len(after), left)
	}
}

// leaseFixture builds a leased object the way a producer does.
func leaseFixture(t *testing.T, staging, prefix string, withIdentity bool) *store.Lease {
	t.Helper()
	name := prefix + randomHexForTest(t)
	lease, err := store.AcquireLease(staging, store.LeaseSourceScratch, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(staging, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if withIdentity {
		identity, _, err := store.EntryIdentityAtPath(filepath.Join(staging, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.AttachIdentity(staging, identity); err != nil {
			t.Fatal(err)
		}
	}
	return lease
}

func randomHexForTest(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw[:])
}

// The handover from lease to journal is two steps, and this is the window
// between them: the journal already names the staged root, and the lease has
// not yet been dropped. A sweep that acted on the lease alone would delete a
// root the journal still names -- which no recovery pass repairs, because
// rollback finds the reservation at neither its private nor its final name and
// refuses, wedging every later command.
//
// Every other sweep in `fu gc` excludes names a pending transaction claims.
// This one was the exception until it was not.
func TestReclaimLeavesAStagedRootThePendingJournalStillNames(t *testing.T) {
	s, _ := setupStore(t)
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checked := session.Store

	reservation, lease, err := checked.ReserveStagedRootOwned(0o755)
	if err != nil {
		t.Fatal(err)
	}
	// Journalled, exactly as createTxnStagedRoot does...
	txn := &TxnRecord{Op: "new", Name: "alpha", StagingReservation: &reservation}
	if err := WriteTxn(checked, txn); err != nil {
		t.Fatal(err)
	}
	// ...and then the holder dies before releasing the lease.
	lease.Abandon()

	pending, err := PendingTxns(checked)
	if err != nil {
		t.Fatal(err)
	}
	claimed := pendingStagingClaims(pending)
	if !claimed[reservation.Name] {
		t.Fatalf("precondition: the journal must claim %s, claims=%v", reservation.Name, claimed)
	}

	outcome, err := store.ReclaimLeases(checked.StagingDir(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 0 || outcome.Claimed != 1 {
		t.Fatalf("outcome = %+v, want the journalled root left to recovery", outcome)
	}
	if _, err := os.Lstat(filepath.Join(checked.StagingDir(), reservation.Name)); err != nil {
		t.Fatalf("gc deleted a staged root the journal still names: %v", err)
	}
}

// status must not promise what gc refuses. A staged root the journal still
// names is left alone by the sweep, so reporting it collectable would send the
// user to run `fu gc` and watch the count stay where it was -- the precise
// failure the bucketing exists to prevent, and a breach of this batch's own
// invariant that a count promised here is one gc will honour.
func TestStatusDoesNotPromiseAStagedRootTheJournalClaims(t *testing.T) {
	s, cfg := setupStore(t)
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	checked := session.Store
	reservation, lease, err := checked.ReserveStagedRootOwned(0o755)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{Op: "new", Name: "alpha", StagingReservation: &reservation}
	if err := WriteTxn(checked, txn); err != nil {
		t.Fatal(err)
	}
	// The holder dies between the journal write and the release.
	lease.Abandon()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Staging.Collectable != 0 {
		t.Fatalf("staging = %+v: gc refuses a claimed name, so nothing here is collectable", report.Staging)
	}
	// The lease file shares its object's fate. Suppressing only its verdict
	// left it matching nothing, so it fell to the bucket for names fu has no
	// pending record for -- about fu's own evidence, for a record that exists
	// and is the very reason the verdict was suppressed.
	if report.Staging.Unmatched != 0 {
		t.Fatalf("staging = %+v: fu has a pending record for every name here, including its own lease", report.Staging)
	}
	if report.Staging.Blocked != 2 {
		t.Fatalf("staging = %+v: the claimed root and its lease both await the same recovery pass", report.Staging)
	}

	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := store.ReclaimLeases(s.StagingDir(), pendingStagingClaims(pending))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 0 || outcome.Leases != 0 {
		t.Fatalf("outcome = %+v: the sweep must refuse a claimed name", outcome)
	}
}
