// internal/cli/gc_lease_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
)

// abandonedPayload leaves the state a killed process leaves: a temporary
// object and the lease that accounts for it, with no holder.
func abandonedPayload(t *testing.T, staging string) string {
	t.Helper()
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	name := ".fu-src-" + strings.Repeat("a", 32)
	lease, err := store.AcquireLease(staging, store.LeaseSourceScratch, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(staging, name), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, _, err := store.EntryIdentityAtPath(filepath.Join(staging, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.AttachIdentity(staging, identity); err != nil {
		t.Fatal(err)
	}
	// Drop the descriptor without removing the lease: this is what a death
	// looks like from the outside.
	lease.Abandon()
	return name
}

// gc reclaims an abandoned payload and says so.
func TestGCReclaimsAnAbandonedPayload(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")
	name := abandonedPayload(t, filepath.Join(fuHome, "staging"))

	out, err := runCmd(t, "gc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "reclaimed 1 abandoned temporary payload") {
		t.Fatalf("want the reclamation reported:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(fuHome, "staging", name)); !os.IsNotExist(err) {
		t.Fatalf("the abandoned payload must be gone: %v", err)
	}
}

// The case that had no remedy at all: an interrupted `fu clone` leaves its
// residue in a home where no store exists, and every command but this one
// refuses such a home. gc's whole job is reclamation, so refusing to reclaim
// because the home is half-built would leave the one leftover nothing else
// can reach.
func TestGCSweepsStagingWithNoStore(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	name := abandonedPayload(t, filepath.Join(fuHome, "staging"))

	out, err := runCmd(t, "gc")
	if err != nil {
		t.Fatalf("gc must not refuse a home with no store: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no store here yet") {
		t.Fatalf("want the reduced scope stated:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(fuHome, "staging", name)); !os.IsNotExist(err) {
		t.Fatalf("the clone-shaped residue must be reclaimed: %v", err)
	}
}

// Every other command still refuses a home with no store; gc is the exception,
// not a new general rule.
func TestOtherCommandsStillRefuseAHomeWithNoStore(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)

	for _, args := range [][]string{{"status"}, {"list"}, {"new", "alpha"}} {
		if _, err := runCmd(t, args...); err == nil {
			t.Fatalf("fu %v must still refuse a home with no store", args)
		}
	}
}

// A lease whose object is already gone is the convergent crash boundary the
// design names: the holder removed its object and died before dropping the
// lease, and the next run settles the lease alone. `fu status` counts it as
// collectable and tells the user to run `fu gc` -- so `fu gc` must say it did
// something. It printed nothing at all, exit 0, and the user was left to
// wonder whether the command had run.
func TestGCReportsSettlingALeaseWhoseObjectIsAlreadyGone(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")

	staging := filepath.Join(fuHome, "staging")
	name := abandonedPayload(t, staging)
	// The object goes; its lease stays. Exactly what a death between the two
	// steps of a release leaves behind.
	if err := os.Remove(filepath.Join(staging, name)); err != nil {
		t.Fatal(err)
	}

	before, err := runCmd(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "1 collectable") {
		t.Fatalf("status must count the orphaned record as collectable:\n%s", before)
	}

	out, err := runCmd(t, "gc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) == "" || strings.Contains(out, "nothing to prune") {
		t.Fatalf("gc reclaimed a record and reported nothing:\n%q", out)
	}
	if !strings.Contains(out, "record") {
		t.Fatalf("want the settled record named:\n%s", out)
	}
	after, err := runCmd(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after, "collectable") {
		t.Fatalf("nothing should be left to collect:\n%s", after)
	}
}

// The one bucket with no remedy is the one where the explanation is the whole
// deliverable. `fu status` printed a bare count while `fu gc` pointed here for
// the names, and the reason the classifier had already computed -- "another
// object now stands at the leased name" -- went nowhere. Both commands carry it
// now; this pins the `fu status` side.
func TestStatusNamesWhatGCCannotAccountFor(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")

	staging := filepath.Join(fuHome, "staging")
	name := abandonedPayload(t, staging)
	// Replace the object with a different one, so its identity no longer
	// matches what the lease recorded. fu must refuse it, and say why.
	if err := os.Remove(filepath.Join(staging, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(staging, name), 0o700); err != nil {
		t.Fatal(err)
	}

	gcOut, err := runCmd(t, "gc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gcOut, "fu cannot account for") {
		t.Fatalf("want the refusal reported:\n%s", gcOut)
	}

	statusOut, err := runCmd(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusOut, name) {
		t.Fatalf("the entry fu refused must be named here:\n%s", statusOut)
	}
	if !strings.Contains(statusOut, "another object now stands at the leased name") {
		t.Fatalf("the reason fu computed must reach the reader:\n%s", statusOut)
	}

	// And the two commands must give the same number for the same directory.
	// They share a classifier so they cannot disagree about a verdict; they can
	// still disagree about arithmetic, and did -- gc counted refused leases
	// while status counted the entries left on disk, so a refusal whose object
	// resolved read as 1 here and 2 there.
	if !strings.Contains(statusOut, "2 that no command collects yet") {
		t.Fatalf("status counts the record and its object:\n%s", statusOut)
	}
	if !strings.Contains(gcOut, "left 2 staging entries fu cannot account for") {
		t.Fatalf("gc must count what status counts:\n%s", gcOut)
	}
}

// `fu gc` must explain its refusals where it makes them.
//
// It used to point at `fu status`, which is unreachable in the one home this
// command was given a storeless mode for: an interrupted `fu clone` leaves
// residue before any store exists, and there `fu status` exits 1. A user
// following gc's own advice got `store not initialized` instead of the reason
// gc had already computed.
func TestGCExplainsWhatItCannotAccountForWithNoStore(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)

	// No `fu init`: this is the home the storeless sweep exists for.
	staging := filepath.Join(fuHome, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	// A death between the lease's exclusive create and its first write, which
	// is what leaves a record no later run can parse.
	token := strings.Repeat("b", 32)
	leaseName := store.LeasePrefix + token
	if err := os.WriteFile(filepath.Join(staging, leaseName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	scratchName := ".fu-clone-" + token
	if err := os.Mkdir(filepath.Join(staging, scratchName), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "gc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no store here yet") {
		t.Fatalf("want the storeless sweep announced:\n%s", out)
	}
	if !strings.Contains(out, "fu cannot account for") {
		t.Fatalf("want the refusal reported:\n%s", out)
	}
	if strings.Contains(out, "`fu status` names them") {
		t.Fatalf("gc must not send a storeless home to a command that exits 1 there:\n%s", out)
	}
	// Named against the object, not the record: the clone directory is what the
	// user can see in staging, and the lease file is fu's bookkeeping about it.
	// The lease's own file name is what makes that attribution safe -- there is
	// exactly one per token -- rather than anything the unreadable record says.
	if !strings.Contains(out, scratchName) {
		t.Fatalf("gc must name the object the user can see:\n%s", out)
	}
	if !strings.Contains(out, "lease record is empty") {
		t.Fatalf("gc computed the reason, so gc must say it:\n%s", out)
	}
	// And it says which of the two states this is, because they are different
	// news: a holder starting up, or one that died before its first write.
	if !strings.Contains(out, "died before its first write") {
		t.Fatalf("an empty record is not the same news as an unparsable one:\n%s", out)
	}

	// And the advice it replaced really was unreachable here.
	if _, err := runCmd(t, "status"); err == nil {
		t.Fatal("`fu status` is expected to refuse a home with no store; if that changed, the pointer could come back")
	}
}
