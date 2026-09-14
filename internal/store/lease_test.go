package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestLeaseLockIsReleasedWhenTheHolderDies pins the one assumption the whole
// reclamation design rests on: a lease's flock is released by the kernel when
// its holder's last descriptor closes, and process death closes every
// descriptor however the process died.
//
// It is the premise that makes liveness answerable at all. The alternatives
// the design rejects cannot answer it: a PID can be reused by an unrelated
// process, and the existence of a record says the same thing whether its
// writer is alive or dead -- which is the question, not the answer.
//
// So the premise is tested rather than trusted, against SIGKILL, which is the
// harshest exit short of losing the machine (and losing the machine takes the
// lock with it). It spawns a real process because that is the only way to
// observe a real death; nothing here is skippable on either platform, since
// both are supported and both must behave this way.
func TestLeaseLockIsReleasedWhenTheHolderDies(t *testing.T) {
	if os.Getenv("FU_TEST_LEASE_HOLD_HELPER") == "1" {
		path := os.Getenv("FU_TEST_LEASE_HOLD_PATH")
		lease, err := acquireLeaseFile(path)
		if err != nil {
			panic(err)
		}
		// Announce the lock is held, then block forever: the parent kills us.
		if err := os.WriteFile(path+".held", []byte("x"), 0o600); err != nil {
			panic(err)
		}
		// Parked in a package-level variable rather than a local: after
		// keepLeaseAlive returns, a local is unreachable, and a finalised
		// *os.File closes its descriptor -- which drops the flock and tells
		// every other process this holder died while it is still working.
		// Observed, not theorised: with a local here, the parent below found
		// the lease free while this process was very much alive.
		heldLeaseForTest = lease
		// Not select{}: the runtime's deadlock detector treats a process with
		// every goroutine parked as a bug and kills it, and being killed
		// releases the very lock this helper exists to hold -- which made the
		// parent below read "holder died" while the holder had merely been
		// told off by its own runtime. Sleeping is not a deadlock.
		for {
			time.Sleep(time.Hour)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".fu-lease-probe")
	// AcquireLease is the only creator, so the file exists before anyone
	// locks it; here the parent stands in for that.
	seed, err := AcquireLease(dir, LeaseSourceScratch, SourceScratchPrefix+"0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	path = seed.Path()
	releaseLeaseFile(seed.file)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseLockIsReleasedWhenTheHolderDies$")
	cmd.Env = append(os.Environ(),
		"FU_TEST_LEASE_HOLD_HELPER=1",
		"FU_TEST_LEASE_HOLD_PATH="+path,
	)
	var childOutput bytes.Buffer
	cmd.Stderr = &childOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		// A helper that died on its own explains a lock that looks released;
		// without this the two are indistinguishable.
		if t.Failed() && childOutput.Len() != 0 {
			t.Logf("helper output:\n%s", childOutput.String())
		}
	}()
	waitForFile(t, path+".held")

	// While the holder lives, the lease must refuse a second acquisition --
	// this is what stops gc from deleting a scratch somebody is downloading
	// into.
	if _, err := tryAcquireLeaseFile(path); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("a live holder must keep the lease: %v", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
	}

	// The kernel releases it on death; no cooperation from the dead process is
	// possible, which is the point.
	deadline := time.Now().Add(10 * time.Second)
	for {
		taken, err := tryAcquireLeaseFile(path)
		if err == nil {
			releaseLeaseFile(taken)
			return
		}
		if !errors.Is(err, errLeaseHeld) {
			t.Fatalf("unexpected error after the holder died: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease was never released after its holder was killed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A pid is not evidence, and the completion this design was written against
// says so outright: liveness may not be inferred from a pid, nor from a record
// merely existing. The lease record carries a pid anyway, for a person reading
// the file -- which is exactly how it could come to be read by a decision one
// day, since it is right there.
//
// So the prohibition is checked at the source level rather than left to
// discipline: nothing in the file that decides a lease's state may mention the
// field at all. Reading the source is the only way to state this; no runtime
// test can observe an absence of consultation.
func TestLeaseReclamationNeverConsultsThePid(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "lease_reclaim.go", nil, 0)
	if err != nil {
		t.Fatalf("parse the reclamation decision: %v", err)
	}
	mentions := 0
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "Pid" {
			mentions++
		}
		if ident, ok := node.(*ast.Ident); ok && ident.Name == "Pid" {
			mentions++
		}
		return true
	})
	if mentions != 0 {
		t.Fatalf("the reclamation decision mentions Pid %d time(s); a pid can be reused by an unrelated process, so it cannot answer whether a holder lives -- the flock is the only signal that can", mentions)
	}
	// Vacuity guard: if the decision ever moves out of this file, the check
	// above starts proving nothing. The state it decides must be in here.
	if !strings.Contains(sourceOf(t, "lease_reclaim.go"), "LeaseInUse") {
		t.Fatal("lease_reclaim.go no longer decides lease state; point this check at the file that does")
	}
}

func sourceOf(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// heldLeaseForTest keeps a helper's lease reachable for the process's life.
var heldLeaseForTest *os.File

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Lstat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper never signalled through %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A second holder in this same process must be refused too: flock is per open
// file description, so two opens of one path contend even within a process.
// Without this, a test could hold a lease through a descriptor it already owns
// and see no contention at all.
func TestLeaseRefusesASecondHolderInTheSameProcess(t *testing.T) {
	dir := t.TempDir()
	seed, err := AcquireLease(dir, LeaseSourceScratch, SourceScratchPrefix+"0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	path := seed.Path()
	releaseLeaseFile(seed.file)
	first, err := acquireLeaseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLeaseFile(first)

	if _, err := tryAcquireLeaseFile(path); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("a held lease must refuse a second acquisition: %v", err)
	}
	releaseLeaseFile(first)
	second, err := tryAcquireLeaseFile(path)
	if err != nil {
		t.Fatalf("a released lease must be acquirable: %v", err)
	}
	releaseLeaseFile(second)
	_ = unix.Unlink(path)
}

// A record only ever grows -- the identity is added to it and nothing is taken
// away -- so a rewrite must never shorten the file first. A truncation is
// visible the instant it returns, and a death before the write that follows
// leaves a zero-length lease no later run can parse, which therefore never
// collects the object it covered.
//
// What this checks is the premise -- that the record really does only grow, so
// that an in-place write always covers the old one completely. It does not
// check that the truncation is absent, and cannot: restoring the Truncate
// leaves every assertion below true, because the empty state exists only
// between two syscalls and nothing here stands between them.
// TestLeaseWriteDoesNotTruncate reads the source for that half.
func TestLeaseRewriteNeverShortensTheRecordFirst(t *testing.T) {
	staging := t.TempDir()
	lease, err := AcquireLease(staging, LeaseSourceScratch, ".fu-src-"+"abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(staging)

	first, err := os.ReadFile(lease.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("the first record must be written whole")
	}
	if err := lease.AttachIdentity(staging, FileIdentity{Device: 1, Inode: 2}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(lease.Path())
	if err != nil {
		t.Fatal(err)
	}
	// Strictly longer is what makes writing in place safe without truncating:
	// the new record always covers the old one completely.
	if len(second) <= len(first) {
		t.Fatalf("the record with an identity (%d bytes) must be longer than the one without (%d); "+
			"if it can ever be shorter, writing in place leaves the tail of the old record behind",
			len(second), len(first))
	}
	var record leaseRecord
	if err := json.Unmarshal(second, &record); err != nil {
		t.Fatalf("the rewritten record must parse: %v", err)
	}
	if record.Identity == nil {
		t.Fatal("the rewritten record must carry the identity")
	}
}

// A lease is minted from the object's name, and a name whose prefix is not
// registered yields the whole name as its token -- which every later
// classification then refuses, forever. That is the failure this mechanism
// exists to end, arrived at from the other side: a payload with a record
// nobody can act on.
//
// Failing at creation puts the mistake in front of whoever made it, in the
// build they made it in, instead of surfacing months later as a count in the
// one bucket that has no remedy.
func TestAcquireLeaseRefusesANameWithNoRegisteredPrefix(t *testing.T) {
	staging := t.TempDir()
	if _, err := AcquireLease(staging, LeaseSourceScratch, ".fu-unregistered-abcdef0123456789abcdef01"); err == nil {
		t.Fatal("a lease whose token is not a token must not be written")
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging = %v, want nothing left behind by the refusal", entries)
	}

	// And the registered spellings still work, including every one a producer
	// can emit.
	for _, prefix := range LeasedObjectPrefixes {
		lease, err := AcquireLease(staging, LeaseSourceScratch, prefix+"abcdef0123456789abcdef01")
		if err != nil {
			t.Fatalf("%s: %v", prefix, err)
		}
		if err := lease.Release(staging); err != nil {
			t.Fatal(err)
		}
	}
}

// A producer must wait for a reader rather than failing.
//
// Between creating its lease and locking it, the file is listable and unlocked,
// so any `fu status` scanning staging at that instant takes its shared probe
// first. A non-blocking acquisition here made `fu add <url>` refuse to start
// because somebody else had run a read-only command -- and nothing in the suite
// noticed when that was fixed, which is the shape this series keeps finding.
func TestAcquireLeaseWaitsForAReaderRatherThanFailing(t *testing.T) {
	staging := t.TempDir()
	afterLeaseCreateHook = func(path string) {
		reader, err := openLeaseFile(path, unix.LOCK_SH|unix.LOCK_NB)
		if err != nil {
			t.Errorf("a lease must be lockable by a reader in this window: %v", err)
			return
		}
		// Released shortly, the way a scan releases as soon as it has its
		// verdict. The producer must still be here when it does.
		go func() {
			time.Sleep(50 * time.Millisecond)
			releaseLeaseFile(reader)
		}()
	}
	defer func() { afterLeaseCreateHook = nil }()

	lease, err := AcquireLease(staging, LeaseSourceScratch, SourceScratchPrefix+"0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("a producer must wait for a reader, not fail: %v", err)
	}
	defer lease.Release(staging)
	if _, err := os.Lstat(lease.Path()); err != nil {
		t.Fatalf("the lease must exist after a successful acquisition: %v", err)
	}
}
