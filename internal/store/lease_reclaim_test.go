package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// leasedScratch builds a lease and the directory it covers, the way a producer
// does, and hands back the token so a test can name the object at any of the
// spellings it may wear.
func leasedScratch(t *testing.T, staging string, withIdentity bool) (*Lease, string) {
	t.Helper()
	name, err := randomLeaseName(".fu-src-")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(staging, LeaseSourceScratch, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(staging, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if withIdentity {
		identity, _, err := EntryIdentityAt(atFDCWD, filepath.Join(staging, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.AttachIdentity(staging, identity); err != nil {
			t.Fatal(err)
		}
	}
	return lease, name
}

func stagingDirForLeases(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

// A lease still held is the one answer that needs no inspection: somebody is
// using what it covers. This is the completion condition in its plainest form
// -- gc running while a download is in progress must not take the download's
// directory away.
func TestReclaimLeavesAHeldLeaseAlone(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	defer lease.Release(staging)
	if err := os.WriteFile(filepath.Join(staging, name, "downloading"), []byte("half a repo"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.InUse != 1 || outcome.Objects != 0 || outcome.Leases != 0 {
		t.Fatalf("outcome = %+v, want the live holder's object left alone", outcome)
	}
	if !exists(t, filepath.Join(staging, name, "downloading")) {
		t.Fatal("gc removed content a live process was still writing")
	}
}

// Every crash boundary in the lifecycle, and what a later run makes of it.
// The table is the design's, and each row is produced by doing exactly as much
// of the lifecycle as the boundary allows and then abandoning it.
func TestReclaimConvergesFromEveryCrashBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange leaves the staging directory in the state that boundary
		// produces, with no lease held -- which is what a dead holder looks
		// like.
		arrange func(t *testing.T, staging string) (object string)
	}{
		{
			// Lease written, object never created.
			"after the lease, before the object",
			func(t *testing.T, staging string) string {
				name, err := randomLeaseName(".fu-src-")
				if err != nil {
					t.Fatal(err)
				}
				lease, err := AcquireLease(staging, LeaseSourceScratch, name)
				if err != nil {
					t.Fatal(err)
				}
				releaseLeaseFile(lease.file)
				return name
			},
		},
		{
			// Object created, identity not yet captured. It is empty, which is
			// what makes the directory-only unlink sufficient authority.
			"after the object, before its identity",
			func(t *testing.T, staging string) string {
				lease, name := leasedScratch(t, staging, false)
				releaseLeaseFile(lease.file)
				return name
			},
		},
		{
			// The ordinary abandoned scratch: identity recorded, content
			// written, holder gone.
			"while in use",
			func(t *testing.T, staging string) string {
				lease, name := leasedScratch(t, staging, true)
				if err := os.WriteFile(filepath.Join(staging, name, "partial"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				releaseLeaseFile(lease.file)
				return name
			},
		},
		{
			// Renamed to its quarantine spelling, then abandoned. The lease
			// was never rewritten, and does not need to have been: the token
			// is what it matches on.
			"after the close rename, before the removal",
			func(t *testing.T, staging string) string {
				lease, name := leasedScratch(t, staging, true)
				quarantine := ".fu-src-clean-" + lease.Token()
				if err := os.Rename(filepath.Join(staging, name), filepath.Join(staging, quarantine)); err != nil {
					t.Fatal(err)
				}
				releaseLeaseFile(lease.file)
				return quarantine
			},
		},
		{
			// Interrupted mid-removal: the object was retired under the name
			// reclamation moves it to, and the run died before deleting it.
			// The retired name carries the token, so the next run resolves the
			// lease to it and finishes -- which is the whole reason that name
			// is derived rather than random.
			"after the retirement, before the deletion",
			func(t *testing.T, staging string) string {
				lease, name := leasedScratch(t, staging, true)
				if err := os.WriteFile(filepath.Join(staging, name, "partial"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				retired := RetiredLeasedPayloadPrefix + lease.Token()
				if err := os.Rename(filepath.Join(staging, name), filepath.Join(staging, retired)); err != nil {
					t.Fatal(err)
				}
				releaseLeaseFile(lease.file)
				return retired
			},
		},
		{
			// Object removed, lease not yet dropped.
			"after the removal, before the lease",
			func(t *testing.T, staging string) string {
				lease, name := leasedScratch(t, staging, true)
				if err := os.Remove(filepath.Join(staging, name)); err != nil {
					t.Fatal(err)
				}
				releaseLeaseFile(lease.file)
				return name
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := stagingDirForLeases(t)
			object := tc.arrange(t, staging)

			outcome, err := ReclaimLeases(staging, nil)
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if outcome.Leases != 1 {
				t.Fatalf("outcome = %+v, want the lease settled", outcome)
			}
			if exists(t, filepath.Join(staging, object)) {
				t.Fatalf("%s survived reclamation", object)
			}
			left, err := os.ReadDir(staging)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Fatalf("staging must be empty after reclamation, still holds %d entries", len(left))
			}

			// Re-running is the other half of convergence: a second pass has
			// nothing to do and must not fail for having nothing to do.
			again, err := ReclaimLeases(staging, nil)
			if err != nil {
				t.Fatalf("second reclaim: %v", err)
			}
			if !reclaimDidNothing(again) {
				t.Fatalf("second pass = %+v, want nothing left", again)
			}
		})
	}
}

// Substitution is preserved, whichever shape it takes and whichever window it
// lands in. The authority differs between the two -- an identity check when
// the lease has one, a directory-only unlink when it does not -- and both must
// refuse.
func TestReclaimPreservesWhatItCannotAccountFor(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withIdentity bool
		substitute   func(t *testing.T, path string)
	}{
		{"a plain file where the scratch was", true, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not fu's"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlink where the scratch was", true, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/etc", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"a different directory at the same name", true, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{
			// No identity recorded, so the only authority is the
			// directory-only unlink -- which content defeats, as it must:
			// an empty directory is all fu could have left in that window.
			"content in the window before the identity was captured", false,
			func(t *testing.T, path string) {
				if err := os.WriteFile(filepath.Join(path, "someone-elses"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := stagingDirForLeases(t)
			lease, name := leasedScratch(t, staging, tc.withIdentity)
			path := filepath.Join(staging, name)
			tc.substitute(t, path)
			releaseLeaseFile(lease.file)

			outcome, err := ReclaimLeases(staging, nil)
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if !exists(t, path) {
				t.Fatalf("outcome = %+v: reclamation removed an object it could not account for", outcome)
			}
			if outcome.Objects != 0 {
				t.Fatalf("outcome = %+v, want nothing removed", outcome)
			}
		})
	}
}

// Residue with no lease behind it is not this batch's to judge. It predates
// the mechanism, nothing recorded who made it, and inventing ownership for it
// is the guess the design refuses.
func TestReclaimIgnoresResidueWithNoLease(t *testing.T) {
	staging := stagingDirForLeases(t)
	orphan := filepath.Join(staging, ".fu-src-0123456789abcdef")
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimDidNothing(outcome) {
		t.Fatalf("outcome = %+v, want a run that found nothing it could account for", outcome)
	}
	if !exists(t, orphan) {
		t.Fatal("residue with no evidence behind it must be preserved")
	}
}

// status and gc must agree, so they share the classification. This pins that
// the scan reports exactly what a reclamation then does.
func TestScanAgreesWithWhatReclaimDoes(t *testing.T) {
	staging := stagingDirForLeases(t)
	// One collectable, one held, one unaccountable.
	dead, _ := leasedScratch(t, staging, true)
	releaseLeaseFile(dead.file)
	live, _ := leasedScratch(t, staging, true)
	defer live.Release(staging)
	swapped, swappedName := leasedScratch(t, staging, true)
	if err := os.Remove(filepath.Join(staging, swappedName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, swappedName), []byte("not fu's"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLeaseFile(swapped.file)

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[LeaseState]int{}
	for _, finding := range findings {
		counts[finding.State]++
	}
	if counts[LeaseCollectable] != 1 || counts[LeaseInUse] != 1 || counts[LeaseUnaccountable] != 1 {
		t.Fatalf("scan = %+v, want one of each state", counts)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Leases != counts[LeaseCollectable] {
		t.Fatalf("gc settled %d leases, status promised %d", outcome.Leases, counts[LeaseCollectable])
	}
	if outcome.InUse != counts[LeaseInUse] || outcome.Unaccountable != counts[LeaseUnaccountable] {
		t.Fatalf("outcome = %+v disagrees with the scan %+v", outcome, counts)
	}
}

// A leased object under a prefix the resolver does not know is invisible to
// the collector -- the exact failure this mechanism exists to end, which a
// producer reintroduces by forgetting to register its own prefix.
//
// The first version of this read the AcquireLease call sites with go/ast and
// asserted their string literals were registered. It could not fail: both call
// sites pass a variable, so it collected nothing and the assertion loop never
// ran. What it checked and what it claimed had nothing to do with each other.
//
// This runs each producer instead and asks the only question that matters --
// does the collector find what the producer made -- which no amount of reading
// the source can answer.
func TestEveryProducerLeavesAnObjectTheCollectorCanFind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		produce func(t *testing.T, staging string) (token, object string)
	}{
		{"clone scratch name", func(t *testing.T, staging string) (string, string) {
			name, err := newCloneScratchName()
			if err != nil {
				t.Fatal(err)
			}
			return leaseTokenOf(name), name
		}},
		{"private staged root name", func(t *testing.T, staging string) (string, string) {
			name, err := privateStagedRootName()
			if err != nil {
				t.Fatal(err)
			}
			return leaseTokenOf(name), name
		}},
		{"retired payload name", func(t *testing.T, staging string) (string, string) {
			name, err := randomLeaseName(RetiredLeasedPayloadPrefix)
			if err != nil {
				t.Fatal(err)
			}
			return leaseTokenOf(name), name
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := stagingDirForLeases(t)
			token, object := tc.produce(t, staging)
			// A token equal to the whole name means no registered prefix
			// matched, so the lease would be named for the object rather than
			// derived from it.
			if token == object {
				t.Fatalf("%q matches no registered prefix; LeasedObjectPrefixes would never resolve it", object)
			}
			if err := os.Mkdir(filepath.Join(staging, object), 0o700); err != nil {
				t.Fatal(err)
			}
			found, err := resolveLeasedObject(staging, token)
			if err != nil {
				t.Fatal(err)
			}
			if found != object {
				t.Fatalf("the collector resolved %q to %q; a producer whose spelling it cannot find leaves invisible residue", token, found)
			}
		})
	}
}

// The reason reclamation retires before it deletes.
//
// Verifying a name and then deleting that name leaves a window: whatever
// occupies the name at the moment of deletion is what gets deleted, which need
// not be what was verified. Retiring first closes it -- the object is moved,
// under a no-replace rename, to a name nobody else is using, and only that is
// removed.
//
// This is the only way to observe the difference. Convergence looks identical
// either way, which is why the crash-boundary table cannot show it: something
// has to occupy the live name in the instant between the check and the delete.
func TestReclaimDeletesWhatItRetiredRatherThanWhatHoldsTheName(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	if err := os.WriteFile(filepath.Join(staging, name, "ours"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLeaseFile(lease.file)

	// A different process claims the freed name the instant fu's object moves
	// off it.
	afterLeasedPayloadRetireHook = func(live, _ string) {
		if err := os.Mkdir(filepath.Join(staging, live), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staging, live, "theirs"), []byte("y"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterLeasedPayloadRetireHook = nil })

	if _, err := ReclaimLeases(staging, nil); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(staging, name, "theirs")) {
		t.Fatal("reclamation deleted whatever held the name rather than the object it had verified")
	}
	if exists(t, filepath.Join(staging, RetiredLeasedPayloadPrefix+lease.Token())) {
		t.Fatal("the retired object fu did own must still be removed")
	}
}

// A lease record is written by fu and read back from disk, where anything may
// have edited it -- so every string in it that becomes a path is input, and is
// checked as input. Without the check a record was a delete-anything
// instruction: `fu gc` removed a tree outside $FU_HOME and called it a
// reclaimed payload.
func TestReclaimRefusesALeaseThatNamesAnythingButAStagingEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
	}{
		{"a traversing name", `{"version":1,"kind":"source-scratch","token":"%TOKEN%","name":"../../victim"}`},
		{"a traversing token", `{"version":1,"kind":"source-scratch","token":"../../victim","name":".fu-src-x"}`},
		{"an absolute name", `{"version":1,"kind":"source-scratch","token":"%TOKEN%","name":"/etc/passwd"}`},
		{"a name with a separator", `{"version":1,"kind":"source-scratch","token":"%TOKEN%","name":"sub/dir"}`},
		{"dot-dot alone", `{"version":1,"kind":"source-scratch","token":"%TOKEN%","name":".."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			staging := filepath.Join(root, "staging")
			if err := os.MkdirAll(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			// A victim two levels up, exactly where a traversal would land.
			victim := filepath.Join(root, "victim")
			if err := os.MkdirAll(victim, 0o700); err != nil {
				t.Fatal(err)
			}

			token := "deadbeefdeadbeefdeadbeefdeadbeef"
			body := strings.ReplaceAll(tc.record, "%TOKEN%", token)
			leasePath := filepath.Join(staging, LeasePrefix+token)
			if err := os.WriteFile(leasePath, []byte(body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			outcome, err := ReclaimLeases(staging, nil)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Unaccountable != 1 || outcome.Objects != 0 || outcome.Leases != 0 {
				t.Fatalf("outcome = %+v, want the record refused outright", outcome)
			}
			if !exists(t, victim) {
				t.Fatal("reclamation followed a path out of the staging area")
			}
		})
	}
}

// Liveness is bound to a lease file's inode, but the object is found through
// the token inside it. A second file carrying the same token would answer for
// an object whose real lease is held elsewhere -- a plain copy was enough to
// have gc delete a live holder's scratch while reporting it as in use.
func TestReclaimRefusesALeaseWhoseNameIsNotItsOwnToken(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	defer lease.Release(staging)
	if err := os.WriteFile(filepath.Join(staging, name, "in-flight"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(lease.Path())
	if err != nil {
		t.Fatal(err)
	}
	// The same record under a name its token does not derive, and unheld.
	if err := os.WriteFile(lease.Path()+"dup", body, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 0 {
		t.Fatalf("outcome = %+v: a duplicated record must not speak for a held object", outcome)
	}
	if !exists(t, filepath.Join(staging, name, "in-flight")) {
		t.Fatal("gc removed a live holder's work on the word of a copied lease")
	}
}

// A populated object under a lease that records no identity is refused at
// classification, and its lease stays.
//
// This used to reach the sweep's own refusal guard; since classification gained
// an emptiness probe it stops one step earlier, so the guard itself is pinned
// separately by TestReclaimKeepsTheLeaseWhenTheUnlinkDeclines. Worth keeping as
// its own case: the property the user sees is the same either way, and the two
// paths that produce it should both be held.
func TestReclaimKeepsTheLeaseWhenItRefusesTheObject(t *testing.T) {
	staging := stagingDirForLeases(t)
	// No identity yet, and content in the directory: the directory-only unlink
	// declines, as it must.
	lease, name := leasedScratch(t, staging, false)
	if err := os.WriteFile(filepath.Join(staging, name, "someone-elses"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	leasePath := lease.Path()
	releaseLeaseFile(lease.file)

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 0 || outcome.Leases != 0 {
		t.Fatalf("outcome = %+v, want nothing claimed as done", outcome)
	}
	if !exists(t, filepath.Join(staging, name)) {
		t.Fatal("the object was removed after all")
	}
	if !exists(t, leasePath) {
		t.Fatal("the evidence was destroyed for an object that survived it")
	}
	// And it stays that way: a second run must not quietly finish the job by
	// forgetting why it declined.
	again, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Objects != 0 || again.Leases != 0 {
		t.Fatalf("second pass = %+v, want the same refusal", again)
	}
	if !exists(t, leasePath) {
		t.Fatal("the second pass destroyed the evidence")
	}
}

// A name from a record that fails validation must not be reported either.
// Nothing deletes by it -- such a lease is unaccountable and nothing is
// removed -- but the reporting layer keys its buckets by this name, so a
// planted record could otherwise attach its own verdict to an unrelated
// staging entry. The command's worth is that what it says is true.
func TestReclaimDoesNotReportAnUnvalidatedNameFromARecord(t *testing.T) {
	staging := stagingDirForLeases(t)
	// A real, healthy object that a planted record tries to speak for.
	innocent, innocentName := leasedScratch(t, staging, true)
	defer innocent.Release(staging)

	token := "0123456789abcdef0123456789abcdef"
	body := `{"version":1,"kind":"source-scratch","token":"` + token + `","name":"../` + innocentName + `"}`
	if err := os.WriteFile(filepath.Join(staging, LeasePrefix+token), []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.State != LeaseUnaccountable {
			continue
		}
		if finding.Name != "" {
			t.Fatalf("an unaccountable lease reported the name %q, which a reader would attribute a verdict to", finding.Name)
		}
	}
}

// A lease released by another process between the listing and the open must
// not be treated as a licence to delete.
//
// This is the shape a regression took: a state added for an unrelated reason
// fell through a switch that listed what must not be deleted instead of what
// may be, reaching the removal path with no lock held and a name read before
// the lock was tried.
//
// Reaching that state takes the hook. Removing the lease before the sweep
// starts -- which is how this was first written -- means os.ReadDir never lists
// it, so no finding is built and the state is never produced. The test passed
// against the defect it existed to pin, and so did the whole package: the
// window is between the record read and the lock attempt, and nothing
// single-threaded can stand in it without help.
//
// The object is left empty on purpose. That is the real state of this window --
// a producer has made its directory and not yet filled it -- and it is also the
// only shape the defect could actually destroy, since without an identity the
// removal authority is a directory-only unlink that a populated object refuses
// on its own.
func TestReclaimRemovesNothingWhenALeaseVanishesAfterItsRecordIsRead(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	leasePath := lease.Path()
	releaseLeaseFile(lease.file)

	beforeLeaseLockHook = func(path string) {
		if path != leasePath {
			return
		}
		if err := os.Remove(path); err != nil {
			t.Errorf("remove lease in hook: %v", err)
		}
	}
	defer func() { beforeLeaseLockHook = nil }()

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimDidNothing(outcome) {
		t.Fatalf("outcome = %+v, want nothing acted on", outcome)
	}
	if !exists(t, filepath.Join(staging, name)) {
		t.Fatal("a lease that vanished between the read and the lock was treated as permission to delete its object")
	}
}

// The state the sweep never sees is still a state it must answer for. A lease
// removed before the sweep begins is simply not listed, so it produces no
// finding at all -- which is correct, and worth pinning separately from the
// window above so neither is mistaken for the other.
func TestReclaimIgnoresALeaseAlreadyGoneBeforeItStarts(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	releaseLeaseFile(lease.file)
	if err := os.Remove(lease.Path()); err != nil {
		t.Fatal(err)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimDidNothing(outcome) {
		t.Fatalf("outcome = %+v, want nothing acted on", outcome)
	}
	if !exists(t, filepath.Join(staging, name)) {
		t.Fatal("an object whose lease was already gone was deleted without evidence")
	}
}

// One lease that cannot be read must not cost every other its sweep. `fu gc`
// resumes safely from any deletion prefix elsewhere, and reclaiming nothing at
// all because one file is unreadable is the opposite of that posture.
func TestReclaimSweepsPastALeaseItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	staging := stagingDirForLeases(t)
	healthy, healthyName := leasedScratch(t, staging, true)
	releaseLeaseFile(healthy.file)
	broken, _ := leasedScratch(t, staging, true)
	releaseLeaseFile(broken.file)
	if err := os.Chmod(broken.Path(), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(broken.Path(), 0o600) })

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatalf("an unreadable lease must not fail the sweep: %v", err)
	}
	if outcome.Objects != 1 || outcome.Leases != 1 {
		t.Fatalf("outcome = %+v, want the healthy pair reclaimed regardless", outcome)
	}
	if outcome.Unaccountable != 1 {
		t.Fatalf("outcome = %+v, want the unreadable one reported", outcome)
	}
	if exists(t, filepath.Join(staging, healthyName)) {
		t.Fatal("the healthy object was not reclaimed")
	}
}

// No producer may name a staging object with a literal, because a literal can
// be mistyped and nothing would notice: both earlier forms of this guard
// enumerated the prefixes they expected and so were blind to an unexpected one.
// The prefixes are constants now, and this checks that producers reference them
// rather than retyping the text -- which is the one thing a constant cannot
// enforce on its own.
func TestNoProducerNamesAStagingObjectWithALiteral(t *testing.T) {
	literal := regexp.MustCompile(`"\.fu-[a-z-]+"`)
	checked := 0
	for _, path := range []string{"../source/scratch.go"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if found := literal.FindAllString(string(body), -1); len(found) != 0 {
			t.Errorf("%s names staging objects with the literals %v; use the exported prefix constants, "+
				"so a mistyped one is a build error rather than something a guard has to notice", path, found)
		}
	}
	if checked == 0 {
		t.Fatal("no producer sources inspected; the check would pass vacuously")
	}
}

// Something that is not a regular file at a lease name must cost that name its
// verdict and nothing else.
//
// A FIFO is the sharp case: fu can never create one, anything that can write to
// staging can, and an ordinary blocking open of one parks until a writer
// arrives. That wedged `fu status` and `fu gc` outright, and since the sweep
// holds fu.lock for its whole run, every write command queued behind it.
func TestScanRefusesALeaseNameThatIsNotARegularFile(t *testing.T) {
	staging := stagingDirForLeases(t)
	healthy, healthyName := leasedScratch(t, staging, true)
	releaseLeaseFile(healthy.file)

	token := "cccccccccccccccccccccccccccccccc"
	if err := unix.Mkfifo(filepath.Join(staging, LeasePrefix+token), 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		findings []LeaseFinding
		err      error
	}
	done := make(chan result, 1)
	go func() {
		findings, err := ScanLeases(staging)
		done <- result{findings, err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("ScanLeases did not return: one entry fu cannot even create must not be able to stop the command")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}

	var refused, collectable int
	for _, finding := range got.findings {
		switch {
		case finding.State == LeaseUnaccountable && finding.Name == "":
			refused++
		case finding.State == LeaseCollectable && finding.Name == healthyName:
			collectable++
		default:
			t.Fatalf("unexpected finding %+v", finding)
		}
	}
	if refused != 1 {
		t.Fatalf("refused %d leases, want the FIFO refused exactly once", refused)
	}
	// And the entry beside it keeps its verdict: one planted name costs its own
	// answer, not the sweep.
	if collectable != 1 {
		t.Fatal("a healthy lease lost its verdict because of an unrelated planted name")
	}
}

// A symlink at a lease name must not let a file outside staging answer for a
// name inside it.
//
// The locked open has refused symlinks from the start, but the read that
// precedes it did not -- and that read is where a reported name comes from. So
// the contents of a file anywhere on the machine decided what a staging entry's
// verdict was labelled with, for a lease fu had already refused as not its own.
//
// The record planted here is internally coherent, which is what makes this
// about the symlink rather than about the record: it passes every check a
// record is subject to, and must still be disbelieved because of where it was
// read from.
func TestScanReportsNoNameForALeaseItRefusesAsASymlink(t *testing.T) {
	staging := stagingDirForLeases(t)
	token := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	outside := filepath.Join(t.TempDir(), "planted")
	body := fmt.Sprintf(`{"version":1,"kind":"source-scratch","token":%q,"name":%q}`, token, SourceScratchPrefix+token)
	if err := os.WriteFile(outside, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(staging, LeasePrefix+token)); err != nil {
		t.Fatal(err)
	}

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the refused symlink", findings)
	}
	if findings[0].State != LeaseUnaccountable {
		t.Fatalf("state = %v, want a symlink at a lease name refused", findings[0].State)
	}
	if findings[0].Name != "" {
		t.Fatalf("the refused lease reported the name %q, read from a file outside staging", findings[0].Name)
	}
}

// A lease record must not be able to speak for an object that is not its own.
//
// Liveness belongs to a lease file; the object is found through the token
// inside it. Nothing tied the two together for a held lease, so a second file
// -- held, and naming somebody else's object -- had `fu status` call that
// object in use while `fu gc` went ahead and deleted it. The two commands share
// this function precisely so they cannot disagree, and here they did.
func TestScanDoesNotLetAHeldForeignLeaseSpeakForAnotherObject(t *testing.T) {
	staging := stagingDirForLeases(t)
	victim, victimName := leasedScratch(t, staging, true)
	// Its own holder is gone, so the truthful answer is "collectable".
	releaseLeaseFile(victim.file)

	foreign := "ffffffffffffffffffffffffffffffff"
	foreignPath := filepath.Join(staging, LeasePrefix+foreign)
	body := `{"version":1,"kind":"source-scratch","token":"` + foreign + `","name":"` + victimName + `"}`
	if err := os.WriteFile(foreignPath, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := acquireLeaseFile(foreignPath)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLeaseFile(held)

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	var named []LeaseState
	for _, finding := range findings {
		if finding.Name == victimName {
			named = append(named, finding.State)
		}
	}
	if len(named) != 1 || named[0] != LeaseCollectable {
		t.Fatalf("findings naming the object = %v, want exactly [collectable]: a held lease claimed an object that is not its own", named)
	}

	// And what gc does must match what status just said.
	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 1 {
		t.Fatalf("outcome = %+v: gc must collect the object status called collectable", outcome)
	}
}

// When a publish fails, the private root stays where it is -- so its lease must
// stay too. This is the one unwinding path in the package that released the
// lease unconditionally, and the object it left behind was an unbacked
// `.fu-new-*`: exactly the residue leases exist to abolish, manufactured by the
// mechanism's own code.
func TestAFailedPublishLeavesItsPrivateRootAccountedFor(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	staging := session.Store.StagingDir()
	// Occupy the public name so the no-replace rename must fail.
	if err := os.Mkdir(filepath.Join(staging, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Store.CreateStagedRootOwned("alpha", 0o755); err == nil {
		t.Fatal("publishing onto an occupied name must fail")
	}

	// Whatever survives must be collectable, which is only true if its lease
	// survived with it.
	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 1 {
		t.Fatalf("outcome = %+v: the private root left by a failed publish must be collectable", outcome)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "alpha" {
		t.Fatalf("staging = %v, want only the name that was already there", entries)
	}
}

// status must not promise a collection gc will refuse.
//
// A lease with no identity yet covers the window between the mkdir and the
// capture, where the object can only be an empty directory. If something is
// inside it, the directory-only unlink declines -- correctly, since the lease
// cannot prove the object is fu's -- but the classification still said
// collectable, so `fu status` counted it and `fu gc` did not reclaim it. The
// user runs gc and watches the number not move.
func TestScanDoesNotCallCollectableWhatTheSweepWillRefuse(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, false)
	if err := os.WriteFile(filepath.Join(staging, name, "someone-elses"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLeaseFile(lease.file)

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the one lease", findings)
	}
	if findings[0].State != LeaseUnaccountable {
		t.Fatalf("state = %v, want unaccountable: the sweep cannot remove this and status must not say it will", findings[0].State)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 0 || outcome.Unaccountable != 1 {
		t.Fatalf("outcome = %+v, want the same answer status gave", outcome)
	}
}

// Everything the sweep removes, it removes through the descriptor it pinned at
// the start. So every read it makes must go through that descriptor too --
// otherwise a directory swapped in underneath computes the verdict while the
// deletion lands somewhere else, where no identity was ever checked.
//
// The swap happens at the pin, before anything has been read, because a hook
// any later cannot see the reads that precede it. Placed after the record read,
// this covered two of the five reads it was claimed to cover.
//
// The replacement directory is furnished to make each read distinguishable: an
// object at the same spelling (so a path-based identity check finds a stranger)
// and a second one carrying the same token under a different prefix (so a
// path-based resolution finds two and refuses).
//
// Arranging any of this takes write access to $FU_HOME, which is already enough
// to do the damage directly. The reason to close it anyway is that half a
// pinned root is worse than none: it invites the next reader to assume the
// other half.
func TestTheSweepActsOnTheDirectoryItPinnedNotOnItsName(t *testing.T) {
	swapStaging := func(t *testing.T, staging, moved, token string) {
		t.Helper()
		if err := os.Rename(staging, moved); err != nil {
			t.Errorf("move staging aside: %v", err)
			return
		}
		if err := os.Mkdir(staging, 0o700); err != nil {
			t.Errorf("plant a replacement staging: %v", err)
			return
		}
		for _, name := range []string{SourceScratchPrefix + token, SourceScratchOrphanPrefix + token} {
			if err := os.Mkdir(filepath.Join(staging, name), 0o700); err != nil {
				t.Errorf("plant %s: %v", name, err)
			}
		}
	}

	t.Run("collectable", func(t *testing.T) {
		root := t.TempDir()
		staging := filepath.Join(root, "staging")
		if err := os.Mkdir(staging, 0o700); err != nil {
			t.Fatal(err)
		}
		lease, name := leasedScratch(t, staging, true)
		releaseLeaseFile(lease.file)
		moved := filepath.Join(root, "staging-moved")

		afterStagingPinHook = func() { swapStaging(t, staging, moved, leaseTokenOf(name)) }
		defer func() { afterStagingPinHook = nil }()

		outcome, err := ReclaimLeases(staging, nil)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Objects != 1 {
			t.Fatalf("outcome = %+v: the sweep lost track of the directory it pinned", outcome)
		}
		if exists(t, filepath.Join(moved, name)) {
			t.Fatal("the object under the pinned descriptor survived; the sweep was reading somewhere else")
		}
		if !exists(t, filepath.Join(staging, name)) {
			t.Fatal("the sweep deleted an object in a directory it never inspected")
		}
	})

	// A held lease reports a name, and that name can only come from the read
	// that happens before the lock -- which is the read the collectable case
	// above cannot see, because there the name is taken again after the lock.
	t.Run("in use", func(t *testing.T) {
		root := t.TempDir()
		staging := filepath.Join(root, "staging")
		if err := os.Mkdir(staging, 0o700); err != nil {
			t.Fatal(err)
		}
		lease, name := leasedScratch(t, staging, true)
		defer lease.Release(staging)
		moved := filepath.Join(root, "staging-moved")

		afterStagingPinHook = func() { swapStaging(t, staging, moved, leaseTokenOf(name)) }
		defer func() { afterStagingPinHook = nil }()

		findings, err := ScanLeases(staging)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %+v, want the one lease under the pinned directory", findings)
		}
		if findings[0].State != LeaseInUse || findings[0].Name != name {
			t.Fatalf("finding = %+v, want %q in use: the name was read from somewhere other than the pinned directory", findings[0], name)
		}
	})
}

// A lease may only speak for the object that is its own, and the binds that
// enforce that constrain names -- while the deletion is driven by resolution.
//
// The object prefixes nest: `.fu-src-` is a proper prefix of `.fu-src-clean-`
// and `.fu-src-orphan-`. So a token of "clean-<T>" spells, through the shorter
// prefix, the very object a live holder's lease covers under <T>. Every name
// check passes -- the lease file derives from its token, and the name the record
// declares carries that token -- because none of them looks at what resolution
// actually returned.
//
// The victim is real: `.fu-src-clean-<T>` is what a source scratch wears between
// Close's quarantine rename and its removal, with its holder alive throughout.
func TestScanRefusesALeaseWhoseTokenSpellsAnotherLeasesObject(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	defer lease.Release(staging)

	token := leaseTokenOf(name)
	quarantine := SourceScratchCleanPrefix + token
	if err := os.Rename(filepath.Join(staging, name), filepath.Join(staging, quarantine)); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(staging, quarantine, "in-flight")
	if err := os.WriteFile(live, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, _, err := EntryIdentityAt(atFDCWD, filepath.Join(staging, quarantine))
	if err != nil {
		t.Fatal(err)
	}

	// One planted file. Its token is the live object's token wearing the
	// longer prefix's tail, so `.fu-src-` + token spells the live object.
	planted := "clean-" + token
	body, err := json.Marshal(leaseRecord{
		Version:  leaseVersion,
		Kind:     LeaseSourceScratch,
		Token:    planted,
		Name:     SourceScratchCleanPrefix + planted,
		Identity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, LeasePrefix+planted), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.State == LeaseCollectable && finding.Name == quarantine {
			t.Fatalf("a planted lease was allowed to speak for %q, whose own lease is held", quarantine)
		}
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 0 {
		t.Fatalf("outcome = %+v: gc removed a live holder's object", outcome)
	}
	if !exists(t, live) {
		t.Fatal("gc deleted a live holder's work on the word of a lease that resolved onto it")
	}
}

// Resolution is where an object is chosen, so it is where the token bind
// belongs. Both directions matter: a legitimate token must still find the
// object under every spelling it may wear, and a token that only spells one
// through a nesting prefix must find nothing.
func TestResolutionOnlyFindsNamesThatCarryTheToken(t *testing.T) {
	staging := stagingDirForLeases(t)
	token := "abcdef0123456789abcdef0123456789"
	for _, prefix := range LeasedObjectPrefixes {
		name := prefix + token
		if err := os.Mkdir(filepath.Join(staging, name), 0o700); err != nil {
			t.Fatal(err)
		}
		found, err := resolveLeasedObject(staging, token)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if found != name {
			t.Fatalf("resolved %q to %q, want %q: a spelling the collector cannot find is invisible residue", token, found, name)
		}
		// Nothing else may reach it. `.fu-src-`+"clean-<T>" spells
		// `.fu-src-clean-<T>`, which is somebody else's object.
		for _, nesting := range []string{"clean-" + token, "orphan-" + token} {
			found, err := resolveLeasedObject(staging, nesting)
			if err != nil {
				t.Fatalf("%s: %v", nesting, err)
			}
			if found != "" {
				t.Fatalf("token %q resolved to %q, which does not carry it", nesting, found)
			}
		}
		if err := os.Remove(filepath.Join(staging, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// The token's shape is the second half of the same guard: a token carrying
// prefix text of its own is what makes a nesting name spellable at all.
func TestOnlyAProducersTokenShapeIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  bool
	}{
		{"abcdef0123456789abcdef01", true},         // 12 random bytes
		{"abcdef0123456789abcdef0123456789", true}, // 16 random bytes
		{"clean-abcdef0123456789abcdef0123456789", false},
		{"orphan-abcdef0123456789abcdef01", false},
		{"ABCDEF0123456789ABCDEF01", false},
		{"abcdef0123456789abcdef0g", false},
		{"abcdef01", false},
		{"", false},
		{"../victim", false},
	} {
		if got := isLeaseToken(tc.token); got != tc.want {
			t.Errorf("isLeaseToken(%q) = %v, want %v", tc.token, got, tc.want)
		}
	}
}

// The identity check after the retire is the reason retire-then-delete is a
// protocol rather than a habit, and nothing could see it: deleting the check
// left every package green.
//
// The window it covers is between the pre-rename identity check and the rename
// itself, and RENAME_NOREPLACE constrains only the destination -- so whatever
// stands at the live name when the rename runs is what gets carried to the
// retired name and recursively removed. The check after the move is what
// notices.
func TestReclaimRefusesAnObjectSwappedInBeforeTheRetire(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	if err := os.WriteFile(filepath.Join(staging, name, "ours"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLeaseFile(lease.file)

	stranger := filepath.Join(staging, "stranger")
	beforeLeasedPayloadRetireHook = func(live string) {
		// The object the lease accounts for steps aside and something else
		// takes its name, after the check and before the move.
		if err := os.Rename(filepath.Join(staging, live), stranger); err != nil {
			t.Errorf("move the leased object aside: %v", err)
			return
		}
		if err := os.Mkdir(filepath.Join(staging, live), 0o700); err != nil {
			t.Errorf("plant a replacement: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(staging, live, "theirs"), []byte("x"), 0o600); err != nil {
			t.Errorf("fill the replacement: %v", err)
		}
	}
	defer func() { beforeLeasedPayloadRetireHook = nil }()

	outcome, err := ReclaimLeases(staging, nil)
	if err == nil {
		t.Fatalf("outcome = %+v: removing an object the lease cannot account for must be reported, not done quietly", outcome)
	}
	if outcome.Objects != 0 {
		t.Fatalf("outcome = %+v, want nothing counted as reclaimed", outcome)
	}
	// Whatever was swapped in must still exist, under whichever name it now
	// wears: it is not fu's, so fu may not delete it.
	if !exists(t, filepath.Join(stranger, "ours")) {
		t.Fatal("the object the lease accounted for was destroyed")
	}
	var survivors int
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if exists(t, filepath.Join(staging, entry.Name(), "theirs")) {
			survivors++
		}
	}
	if survivors != 1 {
		t.Fatal("a stranger's directory was removed on the authority of a lease that did not describe it")
	}
}

// A refusal is not a completion. When the unlink itself declines, the lease
// must stay: dropping it turns something fu merely refused to delete into
// residue with no record at all, which is the one degradation the design
// forbids in as many words -- the evidence has to stay beside the object, or
// the object can never be explained again.
//
// Reaching the guard takes the hook. Classification probes for emptiness and
// the syscall decides, and those are two observations of a directory anyone may
// write to; the guard covers the gap between them. The fixture that used to
// reach it -- a populated object under an identity-less lease -- now stops at
// classification, so the guard went unheld while its test kept passing.
func TestReclaimKeepsTheLeaseWhenTheUnlinkDeclines(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, false)
	leasePath := lease.Path()
	releaseLeaseFile(lease.file)

	planted := filepath.Join(staging, name, "arrived-late")
	beforeLeasedPayloadRemoveHook = func(object string) {
		if object != name {
			return
		}
		if err := os.WriteFile(planted, []byte("x"), 0o600); err != nil {
			t.Errorf("fill the directory between the probe and the unlink: %v", err)
		}
	}
	defer func() { beforeLeasedPayloadRemoveHook = nil }()

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 0 || outcome.Leases != 0 || outcome.Unaccountable != 1 {
		t.Fatalf("outcome = %+v, want one refusal and nothing claimed as done", outcome)
	}
	if !exists(t, planted) {
		t.Fatal("content that arrived before the unlink was removed anyway")
	}
	if !exists(t, leasePath) {
		t.Fatal("the evidence was destroyed for an object that survived it")
	}
}

// The other half of the same three-way answer: an object already gone is a
// completion, not a refusal. The lease is all that is left, and settling it is
// how the boundary converges -- reporting it as something fu cannot account for
// sent the user to `fu status`, which names nothing of the sort.
func TestReclaimSettlesALeaseWhoseObjectVanishedBeforeTheUnlink(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, false)
	leasePath := lease.Path()
	releaseLeaseFile(lease.file)

	beforeLeasedPayloadRemoveHook = func(object string) {
		if object != name {
			return
		}
		if err := os.Remove(filepath.Join(staging, object)); err != nil {
			t.Errorf("remove the object between the probe and the unlink: %v", err)
		}
	}
	defer func() { beforeLeasedPayloadRemoveHook = nil }()

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Unaccountable != 0 {
		t.Fatalf("outcome = %+v: an object already gone is nothing left to account for", outcome)
	}
	if outcome.Leases != 1 {
		t.Fatalf("outcome = %+v, want the lease settled", outcome)
	}
	if exists(t, leasePath) {
		t.Fatal("a lease with nothing left to account for was kept")
	}
}

// A reader is not a holder. Liveness is "somebody holds this exclusively", and
// the scan behind `fu status` must ask that question without becoming an
// answer to it -- otherwise two concurrent status runs report each other as
// live holders and tell the user to wait for a process that is only looking.
func TestScanDoesNotReportAnotherReaderAsALiveHolder(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	// The holder is gone, so the truthful answer is "collectable".
	releaseLeaseFile(lease.file)

	// What a concurrent `fu status` looks like from here.
	reader, err := openLeaseFile(lease.Path(), unix.LOCK_SH|unix.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLeaseFile(reader)

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one lease", findings)
	}
	if findings[0].State != LeaseCollectable || findings[0].Name != name {
		t.Fatalf("finding = %+v, want %q collectable: another reader is not a live holder", findings[0], name)
	}
}

// The same property at the reservation's own unwinding site: a rollback that
// fails leaves the private root, so it must leave the lease.
func TestAFailedReservationRollbackKeepsItsPrivateRootAccountedFor(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	staging := session.Store.StagingDir()

	// Fill the private root and then fail: the rollback's directory-only
	// removal refuses a non-empty directory, which is the state this is about.
	_, _, err = session.Store.reserveStagedRootOwnedWithHooks(0o755, stagedRootReservationHooks{
		afterMkdir: func(private string) error {
			if err := os.WriteFile(filepath.Join(staging, private, "half-written"), []byte("x"), 0o600); err != nil {
				return err
			}
			return errors.New("injected failure after the private root exists")
		},
	})
	if err == nil {
		t.Fatal("the reservation must fail")
	}

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Name == "" {
		t.Fatalf("findings = %+v, want the surviving private root named by its own lease", findings)
	}
	if !exists(t, filepath.Join(staging, findings[0].Name, "half-written")) {
		t.Fatalf("the object %q the lease accounts for is not there", findings[0].Name)
	}
}

// A collector must hold the lease exclusively, because it decides and then
// deletes under the same lock.
//
// Round 4 split the probe -- LOCK_EX for the collector, LOCK_SH for the
// read-only scan -- and nothing pinned the collector's half: collapsing both to
// LOCK_SH left every package green while the built binary went on to delete a
// payload somebody else held. That matters most where there is least to fall
// back on: the storeless `fu gc` path takes no fu.lock at all, so this flock is
// the only thing serialising two runs over a `fu clone` leftover.
//
// flock conflicts between descriptors rather than between processes, so a
// second descriptor in this process is a faithful stand-in for another fu.
func TestReclaimWillNotDeleteUnderALockItOnlyShares(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	// The producer is gone; only a reader remains.
	releaseLeaseFile(lease.file)

	reader, err := openLeaseFile(lease.Path(), unix.LOCK_SH|unix.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLeaseFile(reader)

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.InUse != 1 || outcome.Objects != 0 || outcome.Leases != 0 {
		t.Fatalf("outcome = %+v: a collector that cannot take the lease exclusively must not act", outcome)
	}
	if !exists(t, filepath.Join(staging, name)) {
		t.Fatal("the object was deleted while another descriptor held its lease")
	}
	if !exists(t, lease.Path()) {
		t.Fatal("the evidence was removed while another descriptor held it")
	}
}

// The identity check *before* the retire is what stops fu moving an object it
// has no authority over.
//
// Its sibling -- the re-verification after the move -- got a test in round 4,
// and the two look like one guard from a distance. They are not: the pre-retire
// check covers the window between classification and the rename, and deleting it
// leaves every package green while fu renames a stranger's directory to
// `.fu-retired-payload-<token>` and only then refuses. A rename is not harmless
// just because the deletion that would have followed is refused: the object is
// no longer at the name its owner left it at.
func TestReclaimRefusesAnObjectSwappedInBeforeTheIdentityCheck(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	if err := os.WriteFile(filepath.Join(staging, name, "ours"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLeaseFile(lease.file)

	ours := filepath.Join(staging, "ours-moved-aside")
	beforeLeasedPayloadRemoveHook = func(object string) {
		if object != name {
			return
		}
		// Classification is done and said collectable; the object it judged
		// steps aside and a stranger's takes the name.
		if err := os.Rename(filepath.Join(staging, object), ours); err != nil {
			t.Errorf("move the leased object aside: %v", err)
			return
		}
		if err := os.Mkdir(filepath.Join(staging, object), 0o700); err != nil {
			t.Errorf("plant a replacement: %v", err)
			return
		}
		// Populated, so the removal reaches the whole-tree path rather than
		// being refused by the directory-only unlink on its own.
		if err := os.WriteFile(filepath.Join(staging, object, "theirs"), []byte("x"), 0o600); err != nil {
			t.Errorf("fill the replacement: %v", err)
		}
	}
	defer func() { beforeLeasedPayloadRemoveHook = nil }()

	outcome, err := ReclaimLeases(staging, nil)
	if err == nil {
		t.Fatalf("outcome = %+v: acting on an object the lease cannot account for must be reported", outcome)
	}
	if outcome.Objects != 0 {
		t.Fatalf("outcome = %+v, want nothing counted as reclaimed", outcome)
	}
	if retired := filepath.Join(staging, RetiredLeasedPayloadPrefix+leaseTokenOf(name)); exists(t, retired) {
		t.Fatalf("fu renamed an object it had no authority over: %s", retired)
	}
	if !exists(t, filepath.Join(staging, name, "theirs")) {
		t.Fatal("the stranger's directory was moved out from under its name")
	}
	if !exists(t, filepath.Join(ours, "ours")) {
		t.Fatal("the object the lease accounted for was destroyed")
	}
}

// reclaimDidNothing is what `outcome != (LeaseReclaimOutcome{})` used to say,
// spelled out because Refusals made the struct incomparable.
//
// It is written field by field on purpose. The tempting version checks the
// counts and forgets the slice, and then a run that refused something -- and
// said why -- reads as a run that did nothing at all, which is the opposite of
// what these tests assert.
func reclaimDidNothing(outcome LeaseReclaimOutcome) bool {
	return outcome.Objects == 0 && outcome.Leases == 0 && outcome.InUse == 0 &&
		outcome.Unaccountable == 0 && outcome.Claimed == 0 && len(outcome.Refusals) == 0
}

// And its own per-field guard, for the reason the engine's Result.Empty has
// one: a hand-written predicate over a growing struct drifts silently, and the
// drift is invisible precisely where it matters.
func TestReclaimDidNothingCoversEveryReportedField(t *testing.T) {
	cases := map[string]LeaseReclaimOutcome{
		"objects":       {Objects: 1},
		"leases":        {Leases: 1},
		"in use":        {InUse: 1},
		"unaccountable": {Unaccountable: 1},
		"claimed":       {Claimed: 1},
		"refusals":      {Refusals: []LeaseRefusal{{Name: ".fu-src-x", Reason: "because"}}},
	}
	if !reclaimDidNothing(LeaseReclaimOutcome{}) {
		t.Fatal("a zero outcome must read as nothing done")
	}
	for name, outcome := range cases {
		if reclaimDidNothing(outcome) {
			t.Fatalf("an outcome carrying %s did something", name)
		}
	}
}

// The two reads of a lease can disagree only if the bytes changed between them,
// and the reachable way that happens is a producer killed part-way through
// attaching its identity: the unlocked read sees the complete first record, the
// locked read sees a torn one.
//
// That window is the only place the classifier's ordering is observable, and it
// pins three things at once -- nothing is taken from a record the fault check
// refused, the refusal still explains the object rather than only the record,
// and the name it explains comes from the lease's own file name rather than from
// the bytes that just failed to parse.
func TestARefusedRecordExplainsItsObjectWithoutBelievingItsBytes(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	leasePath := lease.Path()
	releaseLeaseFile(lease.file)

	beforeLeaseLockHook = func(path string) {
		if path != leasePath {
			return
		}
		// A death mid-rewrite: the record is truncated after the unlocked read
		// and before the locked one.
		if err := os.WriteFile(path, []byte(`{"version":1,"kind":"sou`), 0o600); err != nil {
			t.Errorf("tear the record: %v", err)
		}
	}
	defer func() { beforeLeaseLockHook = nil }()

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one lease", findings)
	}
	got := findings[0]
	if got.State != LeaseUnaccountable {
		t.Fatalf("state = %v, want a record fu cannot parse refused", got.State)
	}
	// The object is still on disk and still the thing the user sees, so the
	// refusal has to be about it.
	if got.Name != name {
		t.Fatalf("name = %q, want %q: a refusal with no object named leaves the "+
			"directory in the one bucket with no remedy and no explanation", got.Name, name)
	}
	// But nothing from the bytes that failed to parse.
	if got.Kind != "" {
		t.Fatalf("kind = %q, want nothing taken from a record the fault check refused", got.Kind)
	}
	if got.Reason == "" {
		t.Fatal("a refusal must say why")
	}

	// And the sweep carries that explanation out, from the syscall-level
	// refusal as well as from classification.
	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Unaccountable != 1 {
		t.Fatalf("outcome = %+v, want the one refused lease", outcome)
	}
	// Both entries it left behind, since both are still in staging for the
	// user to find: the torn record and the directory it was about.
	var namedObject bool
	for _, refusal := range outcome.Refusals {
		if refusal.Name == name {
			namedObject = true
		}
	}
	if len(outcome.Refusals) != 2 || !namedObject {
		t.Fatalf("refusals = %+v, want the object %q named beside its record", outcome.Refusals, name)
	}
}

// The other feeder of that list: a refusal the syscall made, after
// classification had already admitted the object. It has no reason from the
// classifier, so the sweep supplies one -- and deleting that line left every
// package green.
func TestASyscallRefusalIsCarriedOutWithItsReason(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, false)
	releaseLeaseFile(lease.file)

	beforeLeasedPayloadRemoveHook = func(object string) {
		if object != name {
			return
		}
		if err := os.WriteFile(filepath.Join(staging, object, "arrived-late"), []byte("x"), 0o600); err != nil {
			t.Errorf("fill the directory between the probe and the unlink: %v", err)
		}
	}
	defer func() { beforeLeasedPayloadRemoveHook = nil }()

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Unaccountable != 1 {
		t.Fatalf("outcome = %+v, want the refusal counted", outcome)
	}
	// Both entries the refusal leaves behind: the record and the object. `fu
	// status` counts them both, so a list that mentioned one would put the two
	// commands at different numbers for the same directory.
	named := map[string]string{}
	for _, refusal := range outcome.Refusals {
		if refusal.Reason == "" {
			t.Fatalf("refusal %+v counted without a reason is the bare count this list exists to replace", refusal)
		}
		named[refusal.Name] = refusal.Reason
	}
	if len(named) != 2 || named[name] == "" || named[filepath.Base(lease.Path())] == "" {
		t.Fatalf("refusals = %+v, want the object and its record both named", outcome.Refusals)
	}
}

// Every refusal carries a reason, so a count and its explanations never
// disagree. `fu gc` prints the count and then the list; a silent refusal would
// print as a number with nothing under it, which is what this whole list
// replaced.
func TestEveryRefusalIsExplained(t *testing.T) {
	staging := stagingDirForLeases(t)
	// Four refusals of different shapes at once.
	live, liveName := leasedScratch(t, staging, true)
	defer live.Release(staging)
	if err := os.WriteFile(filepath.Join(staging, liveName, "in-flight"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mismatched, mismatchedName := leasedScratch(t, staging, true)
	releaseLeaseFile(mismatched.file)
	if err := os.Remove(filepath.Join(staging, mismatchedName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(staging, mismatchedName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(staging, LeasePrefix+strings.Repeat("c", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, LeasePrefix+strings.Repeat("d", 32)), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Unaccountable != 3 {
		t.Fatalf("outcome = %+v, want the three refused leases (the live one is in use, not refused)", outcome)
	}
	for _, refusal := range outcome.Refusals {
		if refusal.Name == "" || refusal.Reason == "" {
			t.Fatalf("refusal %+v must name something and say why", refusal)
		}
	}
	// One entry per thing left on disk, which is the unit `fu status` counts:
	// the FIFO and the unparsable record are one each, and the mismatched
	// object leaves its record beside it.
	if len(outcome.Refusals) != 4 {
		t.Fatalf("refusals = %+v, want one per staging entry left behind", outcome.Refusals)
	}
	// And that really is what a directory listing shows, rather than a number
	// that happens to be four.
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, entry := range entries {
		left[entry.Name()] = true
	}
	for _, refusal := range outcome.Refusals {
		if !left[refusal.Name] {
			t.Fatalf("refusal names %q, which is not in staging", refusal.Name)
		}
	}
	if !exists(t, filepath.Join(staging, liveName, "in-flight")) {
		t.Fatal("a live holder's work was touched")
	}
}

// The same property one branch over: a record that parses cleanly and is still
// refused. Nothing may be taken from it either -- a well-formed lie is the case
// the fault check exists for, and it is the one where believing a field is
// easiest to justify to yourself.
func TestAFaultedRecordExplainsItsObjectWithoutBelievingItsFields(t *testing.T) {
	staging := stagingDirForLeases(t)
	lease, name := leasedScratch(t, staging, true)
	leasePath := lease.Path()
	releaseLeaseFile(lease.file)

	beforeLeaseLockHook = func(path string) {
		if path != leasePath {
			return
		}
		// Parses, carries a kind, and claims a token that is not the one its
		// own file name derives -- so the fault check refuses it.
		body := fmt.Sprintf(`{"version":1,"kind":%q,"token":%q,"name":%q}`,
			LeaseCloneScratch, strings.Repeat("9", 32), SourceScratchPrefix+strings.Repeat("9", 32))
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Errorf("plant the faulted record: %v", err)
		}
	}
	defer func() { beforeLeaseLockHook = nil }()

	findings, err := ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one lease", findings)
	}
	got := findings[0]
	if got.State != LeaseUnaccountable {
		t.Fatalf("state = %v, want the faulted record refused", got.State)
	}
	if got.Kind != "" {
		t.Fatalf("kind = %q, want nothing taken from a record the fault check refused", got.Kind)
	}
	// Still attributed to the object on disk, by the lease's own file name.
	if got.Name != name {
		t.Fatalf("name = %q, want %q", got.Name, name)
	}
	if got.Reason == "" {
		t.Fatal("a refusal must say why")
	}
}
