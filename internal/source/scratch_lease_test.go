package source

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
)

func leaseNamesUnder(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
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

// A live scratch carries its evidence: the lease exists, it is held, and it
// names the directory. Without this, a crash mid-download leaves a directory
// no later run can prove was fu's.
func TestScratchHoldsALeaseWhileItLives(t *testing.T) {
	staging := t.TempDir()
	scratch, err := newOwnedScratch(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer scratch.Close()

	leases := leaseNamesUnder(t, staging)
	if len(leases) != 1 {
		t.Fatalf("leases = %v, want exactly one covering the scratch", leases)
	}
	findings, err := store.ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].State != store.LeaseInUse {
		t.Fatalf("findings = %+v, want the live scratch reported as in use", findings)
	}
}

// A closed scratch leaves nothing at all -- neither the directory nor the
// evidence for it.
func TestScratchReleasesItsLeaseOnClose(t *testing.T) {
	staging := t.TempDir()
	scratch, err := newOwnedScratch(staging)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch.Path(), "content"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scratch.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a closed scratch must leave nothing, found %d entries", len(entries))
	}
}

// The rename Close performs keeps the lease's token, which is what lets the
// record stay true without being rewritten. If the quarantine name were
// generated afresh, a crash between the rename and the removal would strand
// the directory beyond any evidence.
func TestScratchQuarantineNameKeepsTheLeaseToken(t *testing.T) {
	staging := t.TempDir()
	scratch, err := newOwnedScratch(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer scratch.Close()

	token := store.LeaseTokenOf(filepath.Base(scratch.Path()))
	if token == "" {
		t.Fatalf("scratch name %q yields no token", scratch.Path())
	}
	leases := leaseNamesUnder(t, staging)
	if len(leases) != 1 || leases[0] != store.LeasePrefix+token {
		t.Fatalf("lease %v must share the scratch's token %q", leases, token)
	}
}

// The whole point, end to end: a process dies holding a scratch, and a later
// run can prove the leftover was fu's and remove it. Before the lease there
// was no evidence to prove it with, so this residue accumulated forever.
//
// The death is real -- a child process SIGKILLed mid-scratch -- because that
// is the only way to produce the state. A defer cannot be tested by running
// the defer.
func TestAbandonedScratchIsReclaimableAfterItsProcessDies(t *testing.T) {
	if os.Getenv("FU_TEST_SCRATCH_ABANDON_HELPER") == "1" {
		staging := os.Getenv("FU_TEST_SCRATCH_ABANDON_STAGING")
		scratch, err := newOwnedScratch(staging)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(scratch.Path(), "half-a-clone"), []byte("x"), 0o600); err != nil {
			panic(err)
		}
		// Die with the scratch open and the lease held, which is what a
		// SIGKILL mid-download leaves behind.
		os.Exit(91)
	}

	staging := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbandonedScratchIsReclaimableAfterItsProcessDies$")
	cmd.Env = append(os.Environ(),
		"FU_TEST_SCRATCH_ABANDON_HELPER=1",
		"FU_TEST_SCRATCH_ABANDON_STAGING="+staging,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
		t.Fatalf("the helper must die holding the scratch: err=%v output=%s", err, output)
	}

	// The state the old code could never account for: a populated scratch and
	// nothing to say whose it was.
	before, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("want the abandoned scratch and its lease, found %d entries", len(before))
	}

	outcome, err := store.ReclaimLeases(staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Objects != 1 || outcome.Leases != 1 {
		t.Fatalf("outcome = %+v, want the abandoned scratch and its lease reclaimed", outcome)
	}
	after, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("staging must be empty, still holds %v", after)
	}
}

// The orphan spelling must keep the object matched to its lease.
//
// A leased object wears up to three names over its life, and a collector
// matches on the token rather than the spelling precisely so that a rename
// needs no record rewrite and opens no window. The constructor's failure path
// is the only producer of `.fu-src-orphan-`, and it was also the only spelling
// no test exercised -- so nothing proved an object stayed findable while it
// wore the one name a crash is most likely to leave it under.
//
// The check runs inside the removal, because that is the only instant the name
// exists: cleanup retires and deletes in the same call, and it is a death
// between those two steps that strands the object.
func TestOrphanedScratchNameStaysMatchedToItsLease(t *testing.T) {
	staging := t.TempDir()
	scratch, err := newOwnedScratch(staging)
	if err != nil {
		t.Fatal(err)
	}
	token := store.LeaseTokenOf(scratch.name)
	if token == "" || token == scratch.name {
		t.Fatalf("scratch name %q yields no token", scratch.name)
	}

	var checked bool
	err = cleanupCreatedScratch(scratch.parent, staging, scratch.name, scratch.identity, func(retired string) error {
		checked = true
		if want := store.SourceScratchOrphanPrefix + token; retired != want {
			return fmt.Errorf("retired name %q does not carry the lease's token (want %q)", retired, want)
		}
		findings, scanErr := store.ScanLeases(staging)
		if scanErr != nil {
			return scanErr
		}
		if len(findings) != 1 {
			return fmt.Errorf("findings = %+v, want the one lease", findings)
		}
		if findings[0].Name != retired {
			return fmt.Errorf("the collector resolved the lease to %q while its object stood at %q; "+
				"an object it cannot find under the name it wears is invisible residue", findings[0].Name, retired)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("the removal never reached the retired name; this test asserted nothing")
	}
}

// A failed cleanup leaves the directory, so it must leave the lease.
//
// The constructor's unwinding releases the lease only if nothing carries its
// token any more -- because dropping the record beside a surviving object
// manufactures exactly the unaccounted residue the lease exists to prevent.
// Every unwinding path in the branch asks that question and only one of them
// was ever tested; changing this one back to an unconditional Release left the
// package green.
func TestAFailedCleanupKeepsTheScratchAccountedFor(t *testing.T) {
	staging := t.TempDir()
	// The hook fills the directory and then fails. Cleanup retires it, finds
	// the directory-only unlink refuses a non-empty directory, restores it and
	// reports the failure -- which is the state this is about.
	_, err := newOwnedScratchWithHooks(staging, scratchCreateHooks{
		afterMkdir: func(parentFD int, name string) error {
			if err := os.WriteFile(filepath.Join(staging, name, "half-a-clone"), []byte("x"), 0o600); err != nil {
				return err
			}
			return errors.New("injected failure after the directory exists")
		},
	})
	if err == nil {
		t.Fatal("the constructor must fail")
	}

	leases := leaseNamesUnder(t, staging)
	if len(leases) != 1 {
		t.Fatalf("leases = %v, want the record still beside the object it explains", leases)
	}
	// And the pair is still a pair: the collector resolves one to the other,
	// which is the whole of what keeping the record buys.
	findings, err := store.ScanLeases(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Name == "" {
		t.Fatalf("findings = %+v, want the surviving object named by its own lease", findings)
	}
	if !exists(t, filepath.Join(staging, findings[0].Name, "half-a-clone")) {
		t.Fatalf("the object %q the lease accounts for is not there", findings[0].Name)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}
