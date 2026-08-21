package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func pendingActiveConfigExchange(t *testing.T) (*Store, configExchangeRecord, []byte) {
	t.Helper()
	checked := checkedWriteSession(t)
	previous, err := inspectConfigObject(checked.writeRoots.store, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	stagedBytes := []byte("version: 1\nskills:\n  alpha:\n    enabled: true\n")
	if err := WriteFileAtomicNoReplaceRoot(checked.writeRoots.staging.root, configSwapName, stagedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := inspectConfigObject(checked.writeRoots.staging, configSwapName)
	if err != nil {
		t.Fatal(err)
	}
	record := configExchangeRecord{
		Version:      configExchangeRecordVersion,
		Candidate:    ".fu-config-candidate-0123456789abcdef",
		Previous:     previous.identity,
		Staged:       staged.identity,
		ExpectDigest: previous.digest,
		DataDigest:   staged.digest,
	}
	raw, err := writeConfigExchangeRecord(checked.writeRoots.recovery, record)
	if err != nil {
		t.Fatal(err)
	}
	return checked, record, raw
}

// configExchangeResidue lists every recovery/ entry that belongs to a config
// exchange's disposable bookkeeping -- a record, a terminal marker, or an
// archived inode -- using the same prefixes reclaimConfigExchangeResidue
// recognizes. Once an exchange completes durably, whether on the plain
// install path or through recovery, none of these may remain.
func configExchangeResidue(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, configExchangeRecordPrefix) || strings.HasPrefix(name, configArchivePrefix) {
			found = append(found, name)
		}
	}
	return found
}

func TestConfigExchangeActiveStagedConvergesAfterMismatchedPreviousIsRestored(t *testing.T) {
	checked, record, raw := pendingActiveConfigExchange(t)
	// Truncated in place, so fu.yaml keeps the recorded previous identity and
	// only its bytes stop matching the recorded expectation.
	if err := os.WriteFile(checked.ConfigPath(), []byte("third-party config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := inspectConfigObject(checked.writeRoots.store, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The outcome label this arm records is chosen before anything is written,
	// and the terminal marker carrying it is reclaimed as soon as it is durable
	// (reclaimConfigExchangeResidue), so it is no longer readable back off disk.
	// Asserting the decision itself keeps this arm's two readings distinguished:
	// changed bytes under the recorded previous inode is a precondition
	// mismatch, not a withdrawal of an exchange that never took effect.
	if got := configExchangeWithdrawalOutcome(current, record); got != "withdrawn-after-precondition-mismatch" {
		t.Fatalf("withdrawal outcome = %q, want mismatch withdrawal", got)
	}

	if err := recoverConfigExchange(checked.writeRoots.store, checked.writeRoots.staging, checked.writeRoots.recovery, record, raw); err != nil {
		t.Fatalf("restored previous inode with changed bytes must converge: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(checked.StagingDir(), configSwapName)); !os.IsNotExist(err) {
		t.Fatalf("converged recovery must archive the staged active object: %v", err)
	}
	if residue := configExchangeResidue(t, checked.RecoveryDir()); len(residue) != 0 {
		t.Fatalf("a recovered exchange must leave no residue once its terminal marker is durable, found %v", residue)
	}
}

func TestConfigExchangeActiveStagedRejectsDifferentCurrentIdentity(t *testing.T) {
	checked, record, raw := pendingActiveConfigExchange(t)
	foreign := checked.ConfigPath() + ".foreign"
	if err := os.WriteFile(foreign, []byte("third-party replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(foreign, checked.ConfigPath()); err != nil {
		t.Fatal(err)
	}

	err := recoverConfigExchange(checked.writeRoots.store, checked.writeRoots.staging, checked.writeRoots.recovery, record, raw)
	if err == nil || !strings.Contains(err.Error(), "cannot be recovered safely") {
		t.Fatalf("different current identity must leave the exchange in conflict, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(checked.StagingDir(), configSwapName)); err != nil {
		t.Fatalf("conflict must preserve the staged active object: %v", err)
	}
	done, err := configExchangeDoneName(record.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(checked.RecoveryDir(), done)); !os.IsNotExist(err) {
		t.Fatalf("conflict must not write a completion marker: %v", err)
	}
}

// The sibling above reaches this same arm with fu.yaml's bytes changed under
// the recorded previous inode. Here they are untouched, which is the arm's
// other reading: the exchange never took effect at all, so the withdrawal is
// not repairing a precondition mismatch and is labelled differently.
func TestConfigExchangeActiveStagedWithdrawsWhenPreviousIsStillCurrent(t *testing.T) {
	checked, record, raw := pendingActiveConfigExchange(t)
	current, err := inspectConfigObject(checked.writeRoots.store, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := configExchangeWithdrawalOutcome(current, record); got != "withdrawn-with-previous-current" {
		t.Fatalf("withdrawal outcome = %q, want an untouched-previous label", got)
	}

	if err := recoverConfigExchange(checked.writeRoots.store, checked.writeRoots.staging, checked.writeRoots.recovery, record, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(checked.StagingDir(), configSwapName)); !os.IsNotExist(err) {
		t.Fatalf("converged recovery must archive the staged active object: %v", err)
	}
	if residue := configExchangeResidue(t, checked.RecoveryDir()); len(residue) != 0 {
		t.Fatalf("a recovered exchange must leave no residue once its terminal marker is durable, found %v", residue)
	}
}

// An archive name is derived from nothing but a device/inode pair, so it names
// a slot the recorded object may or may not occupy: an exchange creates only
// one of the two names its record spans, and an inode number the filesystem has
// since reused regenerates the same name for an unrelated object. Reclaiming
// those names is therefore only safe against the identity the record already
// binds -- the one archiveNamedConfigEntry proved arrived there. Reading the
// name's current identity instead would prove nothing and delete whatever it
// found.
func TestConfigExchangeReclaimPreservesAnUnrecordedArchiveName(t *testing.T) {
	checked, record, raw := pendingActiveConfigExchange(t)
	// This arm archives the staged object, never the previous one, so the
	// previous object's archive name is free for an unrelated occupant.
	foreign := filepath.Join(checked.RecoveryDir(), configArchiveName(record.Previous))
	foreignBytes := []byte("an object the record does not describe\n")
	if err := os.WriteFile(foreign, foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverConfigExchange(checked.writeRoots.store, checked.writeRoots.staging, checked.writeRoots.recovery, record, raw); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatalf("reclaim must preserve an archive name holding an object the record does not bind: %v", err)
	}
	if !bytes.Equal(got, foreignBytes) {
		t.Fatalf("preserved archive name changed: got %q want %q", got, foreignBytes)
	}
	// The one archive this exchange did record is still reclaimed.
	staged := filepath.Join(checked.RecoveryDir(), configArchiveName(record.Staged))
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("the recorded archive must still be reclaimed: %v", err)
	}
}

// A record whose terminal marker is present is finished, and the enumeration
// must skip it on that one fstatat alone -- without reading the marker, parsing
// it, or comparing digests. Every completed exchange's safety rests on this
// branch, and reclamation now removes the record itself, so no other test can
// still reach it: the exchange that would have exercised it deletes its own
// evidence first. The pairing is hand-placed here instead, exactly as a crash
// between the durable marker write and its reclamation leaves it.
//
// The marker's bytes are deliberately not a valid completion. Should the scan
// ever regress to verifying every historical record it enumerates, it would
// report an error here rather than silently skipping, which is the cost that
// branch exists to avoid.
func TestPendingConfigExchangeRecordsSkipARecordWithATerminalMarker(t *testing.T) {
	checked, record, _ := pendingActiveConfigExchange(t)
	pending, err := readPendingConfigExchangeRecords(checked.writeRoots.recovery)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("a record with no terminal marker is pending work, got %d", len(pending))
	}

	doneName, err := configExchangeDoneName(record.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checked.RecoveryDir(), doneName), []byte("not a completion"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err = readPendingConfigExchangeRecords(checked.writeRoots.recovery)
	if err != nil {
		t.Fatalf("a marked record must be skipped without its marker being read: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a record beside its terminal marker is finished, got %d pending", len(pending))
	}
}

func TestInspectConfigObjectReportsOversizeBeforeStabilityMismatch(t *testing.T) {
	checked := checkedWriteSession(t)
	if err := os.WriteFile(checked.ConfigPath(), bytes.Repeat([]byte("x"), int(MaxConfigBytes)+2), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := inspectConfigObject(checked.writeRoots.store, "fu.yaml")
	if err == nil || !strings.Contains(err.Error(), "exceeds config limit") {
		t.Fatalf("oversized config must report its limit directly, got %v", err)
	}
}

func TestConfigExchangeConflictNamesEveryPreservedLocationAndRemedy(t *testing.T) {
	record := configExchangeRecord{
		Candidate: ".fu-config-candidate-0123456789abcdef",
		Previous:  FileIdentity{Device: 1, Inode: 2},
		Staged:    FileIdentity{Device: 3, Inode: 4},
	}
	target := &checkedRoot{display: "/store"}
	scratch := &checkedRoot{display: "/staging"}
	archive := &checkedRoot{display: "/recovery"}
	err := configExchangeConflictError(target, scratch, archive, record)
	recordName, nameErr := configExchangeRecordName(record.Candidate)
	if nameErr != nil {
		t.Fatal(nameErr)
	}
	wants := []string{
		filepath.Join(target.display, "fu.yaml"),
		filepath.Join(scratch.display, record.Candidate),
		filepath.Join(scratch.display, configSwapName),
		filepath.Join(archive.display, recordName),
		filepath.Join(archive.display, configArchiveName(record.Previous)),
		filepath.Join(archive.display, configArchiveName(record.Staged)),
		"preserve", "move", "retry",
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("config exchange conflict %q does not contain %q", err, want)
		}
	}
}

// recoverConfigExchange has six arms and only "installed" had a real crash
// test. Each arm below is reached by killing a process at the durable boundary
// that produces its state, then reopening and requiring convergence.
//
// The "withdrawn-after-precondition-mismatch" arm matters most: it is the one
// place recovery performs a renameExchange of its own, so it is the one a
// refactor would break silently.
func TestConfigExchangeRecoveryConvergesFromEveryCrashPoint(t *testing.T) {
	stages := []string{"after-record", "before-exchange", "after-exchange", "after-exchange-mismatch", "after-withdrawal-restore"}
	if os.Getenv("FU_TEST_EXCHANGE_CRASH_HELPER") == "1" {
		home := os.Getenv("FU_TEST_EXCHANGE_CRASH_HOME")
		s, err := Open(home)
		if err != nil {
			panic(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			panic(err)
		}
		checked := session.Store
		root, err := checked.StoreRoot()
		if err != nil {
			panic(err)
		}
		before, err := ReadConfigFileRoot(root, "fu.yaml")
		if err != nil {
			panic(err)
		}
		crash := func() { os.Exit(86) }
		var hooks configExchangeHooks
		expect := before
		switch os.Getenv("FU_TEST_EXCHANGE_CRASH_STAGE") {
		case "after-record":
			hooks.afterRecord = crash
		case "before-exchange":
			hooks.beforeExchange = crash
		case "after-exchange":
			hooks.afterExchange = crash
		case "after-exchange-mismatch":
			// The recorded expectation never matched what fu.yaml held, so
			// recovery must roll the exchange back rather than complete it.
			expect = []byte("bytes fu.yaml never held\n")
			hooks.afterExchange = crash
		case "after-withdrawal-restore":
			// Crash after the mismatching previous inode has been restored but
			// before Fu's staged inode is archived.
			expect = []byte("bytes fu.yaml never held\n")
			hooks.afterRestore = crash
		default:
			panic("unknown config exchange crash stage")
		}
		_ = checked.installConfigExpecting(expect, append(before, []byte("\n# fu\n")...), hooks)
		panic("config exchange crash hook did not run")
	}

	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			home := t.TempDir()
			if _, err := Init(home); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(home, "store", "fu.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestConfigExchangeRecoveryConvergesFromEveryCrashPoint$")
			cmd.Env = append(os.Environ(),
				"FU_TEST_EXCHANGE_CRASH_HELPER=1",
				"FU_TEST_EXCHANGE_CRASH_HOME="+home,
				"FU_TEST_EXCHANGE_CRASH_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child must die at %s with code 86, err=%v output=%s", stage, err, output)
			}

			s, err := Open(home)
			if err != nil {
				t.Fatalf("the store must reopen after a crash at %s: %v", stage, err)
			}
			session, err := s.BeginWrite()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			checked := session.Store

			// Recovery must converge, and repeating it must stay convergent.
			// The second call no longer re-enters an already-completed arm the
			// way it did before reclamation: the first call retires each record
			// it finishes, so the second enumerates nothing at all. What it
			// still proves is that a reclaimed exchange leaves behind no state a
			// later recovery can trip over. The skip-a-marked-record branch it
			// used to exercise on the way through is now pinned directly by
			// TestPendingConfigExchangeRecordsSkipARecordWithATerminalMarker,
			// which is the only remaining coverage of it.
			for range 2 {
				if err := checked.RecoverConfigExchanges(); err != nil {
					t.Fatalf("recovery after a crash at %s must converge: %v", stage, err)
				}
			}
			current, err := os.ReadFile(filepath.Join(home, "store", "fu.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if (stage == "after-exchange-mismatch" || stage == "after-withdrawal-restore") && !bytes.Equal(current, original) {
				t.Fatalf("a failed precondition must leave the original config canonical:\ngot  %q\nwant %q", current, original)
			}
			// And an ordinary install must work afterwards, from whatever
			// state recovery settled on.
			next := append(append([]byte(nil), current...), []byte("\n# after recovery\n")...)
			if err := checked.InstallConfigExpecting(current, next); err != nil {
				t.Fatalf("an install after recovery at %s must succeed: %v", stage, err)
			}
			if _, err := os.Lstat(filepath.Join(home, "staging", configSwapName)); !os.IsNotExist(err) {
				t.Errorf("an active exchange entry survived recovery at %s: %v", stage, err)
			}
		})
	}
}

// TestConfigExchangeLeavesNoResidueAfterRepeatedSaves guards the reclamation
// this change adds: every completed exchange must leave neither its record,
// its terminal marker, nor its archived inode behind in recovery/. Before
// this change, each of the 8 saves below left 3 files (a record, a ".done"
// marker, and one archived config), so recovery/ grew without bound across
// ordinary config writes -- SPEC's own measurement found recovery/ larger
// than store/ itself after 17 write commands.
//
// SaveConfigExpecting (not Config.Save, which writes fu.yaml directly via
// os.OpenRoot and never touches the exchange journal at all) is what drives
// a real exchange here, mirroring how every write command's pipeline installs
// its own config mutation (pipeline.go's checked.SaveConfigExpecting call).
func TestConfigExchangeLeavesNoResidueAfterRepeatedSaves(t *testing.T) {
	s := checkedWriteSession(t)
	for i := range 8 {
		before, err := os.ReadFile(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.AddSkill(fmt.Sprintf("skill-%d", i), fmt.Sprintf("sha256:%064d", i)); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveConfigExpecting(cfg, before); err != nil {
			t.Fatal(err)
		}
	}
	if residue := configExchangeResidue(t, s.RecoveryDir()); len(residue) != 0 {
		t.Fatalf("completed config exchanges must leave no residue, found %d: %v", len(residue), residue)
	}
	// configExchangeResidue only knows the two config-exchange prefixes, so it
	// cannot see a name reclamation retired but never unlinked. Nothing else in
	// this test writes to recovery/, so requiring the whole directory to hold no
	// fu-private entry at all closes that gap for free.
	entries, err := os.ReadDir(s.RecoveryDir())
	if err != nil {
		t.Fatal(err)
	}
	var stranded []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".fu-") {
			stranded = append(stranded, entry.Name())
		}
	}
	if len(stranded) != 0 {
		t.Fatalf("reclamation must strand no fu-private entry in recovery/, found %v", stranded)
	}
}

// Inline reclamation disposes of a completed exchange's bookkeeping, but it is
// a sequence of namespace operations a crash can cut anywhere, and it retires
// the record first -- so every state an interruption leaves behind holds a bare
// marker or a bare archive with no record left to derive either from. gc has to
// collect those by prefix, which makes the two rules below the whole of its
// safety over the record family: a record with no marker is recovery's
// authority over an unfinished exchange, and a marker whose record is gone is
// finished by construction.
func TestReclaimCompletedConfigExchangesLeavesPendingRecordsAlone(t *testing.T) {
	s := checkedWriteSession(t)
	// A record without its marker is pending recovery authority and must survive.
	if _, err := writeConfigExchangeRecord(s.writeRoots.recovery, configExchangeRecord{
		Version:      configExchangeRecordVersion,
		Candidate:    configCandidatePrefix + strings.Repeat("ab", 8),
		Previous:     FileIdentity{Device: 1, Inode: 2},
		Staged:       FileIdentity{Device: 1, Inode: 3},
		ExpectDigest: "sha256:" + strings.Repeat("ab", 32),
		DataDigest:   "sha256:" + strings.Repeat("cd", 32),
	}); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(s.RecoveryDir(), configExchangeRecordPrefix+strings.Repeat("ab", 8)+".json")
	// A marker whose record a crash already removed is collectable.
	stranded := filepath.Join(s.RecoveryDir(), configExchangeRecordPrefix+strings.Repeat("cd", 8)+".done")
	if err := os.WriteFile(stranded, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	collected, err := s.ReclaimCompletedConfigExchanges()
	if err != nil {
		t.Fatal(err)
	}
	if collected != 1 {
		t.Fatalf("collected %d entries, want only the stranded marker", collected)
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Fatalf("a record without its marker must be preserved, err=%v", err)
	}
	if _, err := os.Lstat(stranded); !os.IsNotExist(err) {
		t.Fatalf("a stranded marker must be collected, err=%v", err)
	}
}

// An archive's name states the device and inode of the object it must hold, so
// the sweep can prove that much before unlinking it -- but proving what an
// object is does not prove whose it is. Recovery archives the object and only
// then writes the terminal marker, so a crash in between leaves an archive
// whose record is still pending, and that archive is the only remaining copy
// of the object recovery is about to converge on. Collecting it because the
// identity matched would wedge the exchange in a conflict no later run can
// resolve.
func TestReclaimCompletedConfigExchangesKeepsAnArchiveWhileARecordIsPending(t *testing.T) {
	checked, record, _ := pendingActiveConfigExchange(t)
	if err := archiveNamedConfigEntry(checked.writeRoots.staging, configSwapName, checked.writeRoots.recovery, record.Staged); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(checked.RecoveryDir(), configArchiveName(record.Staged))

	collected, err := checked.ReclaimCompletedConfigExchanges()
	if err != nil {
		t.Fatal(err)
	}
	if collected != 0 {
		t.Fatalf("collected %d entries; an unfinished exchange's archive is not the sweep's to collect", collected)
	}
	if _, err := os.Lstat(archived); err != nil {
		t.Fatalf("an archive an unfinished exchange may still need must survive: %v", err)
	}
	// Converging on that archive is what the claim protects.
	if err := checked.RecoverConfigExchanges(); err != nil {
		t.Fatalf("recovery after the sweep must still converge: %v", err)
	}
	if residue := configExchangeResidue(t, checked.RecoveryDir()); len(residue) != 0 {
		t.Fatalf("the recovered exchange must leave no residue, found %v", residue)
	}
}

// The same archive once nothing is pending. Inline reclamation retires the
// record first, so this is precisely what a crash one step later leaves: an
// archive with nothing on disk to derive it from but its own name.
//
// This is also the sweep's one risky pass -- the only loop that walks names by
// prefix with no record behind them -- so the rule that it judges nothing
// outside the two prefixes it owns has to be asserted here, where that loop
// actually runs. A retirement rename parks whatever it found under
// .fu-retired-entry-<random>, so an object fu does not own can sit there with
// nothing on disk tying the name back to fu, and it must survive the pass that
// is walking right past it.
func TestReclaimCompletedConfigExchangesCollectsAnArchiveNothingCanClaim(t *testing.T) {
	checked, record, _ := pendingActiveConfigExchange(t)
	if err := archiveNamedConfigEntry(checked.writeRoots.staging, configSwapName, checked.writeRoots.recovery, record.Staged); err != nil {
		t.Fatal(err)
	}
	recordName, err := configExchangeRecordName(record.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(checked.RecoveryDir(), recordName)); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(checked.RecoveryDir(), configArchiveName(record.Staged))
	// An object parked under a retirement name by an interrupted removal. fu may
	// not own it, and no record anywhere names it.
	foreign := filepath.Join(checked.RecoveryDir(), ".fu-retired-entry-"+strings.Repeat("ef", 16))
	foreignBytes := []byte("an object fu does not own\n")
	if err := os.WriteFile(foreign, foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	collected, err := checked.ReclaimCompletedConfigExchanges()
	if err != nil {
		t.Fatal(err)
	}
	if collected != 1 {
		t.Fatalf("collected %d entries, want the one unclaimable archive", collected)
	}
	if _, err := os.Lstat(archived); !os.IsNotExist(err) {
		t.Fatalf("an archive no unfinished exchange can claim must be collected, err=%v", err)
	}
	got, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatalf("a retired entry the sweep cannot attribute to fu must be preserved, err=%v", err)
	}
	if !bytes.Equal(got, foreignBytes) {
		t.Fatalf("preserved retired entry changed: got %q want %q", got, foreignBytes)
	}
}

// Collecting archives by prefix means meeting names with no record behind them,
// and a name is not evidence: an inode number the filesystem has reused
// regenerates a name an unrelated object now occupies. The mismatch between what
// the name states and what it resolves to is the sweep's only reading of "not
// mine", and it has to be enough: the object survives byte for byte, as the same
// inode, and is not counted as collected.
func TestReclaimCompletedConfigExchangesPreservesAnArchiveNameItDoesNotDescribe(t *testing.T) {
	s := checkedWriteSession(t)
	foreign := filepath.Join(s.RecoveryDir(), configArchiveName(FileIdentity{Device: 1, Inode: 2}))
	foreignBytes := []byte("an object the name does not describe\n")
	if err := os.WriteFile(foreign, foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(foreign)
	if err != nil {
		t.Fatal(err)
	}

	collected, err := s.ReclaimCompletedConfigExchanges()
	if err != nil {
		t.Fatal(err)
	}
	if collected != 0 {
		t.Fatalf("collected %d entries; a name stating an identity it does not hold is not the sweep's", collected)
	}
	got, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatalf("an archive name holding an object it does not describe must be preserved: %v", err)
	}
	if !bytes.Equal(got, foreignBytes) {
		t.Fatalf("preserved archive name changed: got %q want %q", got, foreignBytes)
	}
	after, err := os.Lstat(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the preserved object must be the same inode, not a replacement")
	}
}

// TestReclaimConfigExchangeRejectsAnArchiveNameBeforeRetiringIt pins *where*
// the preservation above happens, which every assertion in it is blind to.
//
// The identity check is a pre-flight: it refuses before the object is renamed
// aside. Delete it and the object is retired to an unpredictable sibling and
// then restored by the post-move revalidation, so this file's end-state
// assertions -- contents equal, os.SameFile, collected == 0 -- all still hold
// and the entire package stays green. What is lost is only visible at a crash
// inside that window, or when RestoreRetiredAt loses an EEXIST race: the
// user's file is then stranded under a random name that, by this repository's
// own account, nothing can ever attribute back to fu.
//
// Its own comment calls the check load-bearing, and the same two conditions in
// CollectableConfigArchiveNames are pinned; this stops the two copies drifting
// apart in the direction that loses data.
func TestReclaimConfigExchangeRejectsAnArchiveNameBeforeRetiringIt(t *testing.T) {
	s := checkedWriteSession(t)
	name := configArchiveName(FileIdentity{Device: 1, Inode: 2})
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("not what the name says\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	reclaimConfigExchangeStatedArchiveHook = func(n string, admitted bool) { seen[n] = admitted }
	t.Cleanup(func() { reclaimConfigExchangeStatedArchiveHook = nil })

	if _, err := s.ReclaimCompletedConfigExchanges(); err != nil {
		t.Fatal(err)
	}

	admitted, examined := seen[name]
	if !examined {
		t.Fatalf("the sweep must consider %s at all; hook saw %v", name, seen)
	}
	if admitted {
		t.Fatalf("%s states an identity it does not hold and must be rejected before any rename", name)
	}
}

// The state the sweep exists for. Inline reclamation begins the instant the
// terminal marker is durable, so the first thing a crash can cut is the step
// that retires the record -- leaving a record beside its own marker.
// readPendingConfigExchangeRecords skips that pair forever, on the marker's
// existence alone, so no later write command will ever revisit it and nothing
// but this sweep collects it.
//
// Both names go, and in that order: the record first, then the marker once the
// record is gone. TestReclaimCompletedConfigExchangesKeepsAMarkerWhoseRecordSurvives
// pins the other direction, where a record that cannot be collected keeps its
// marker pinned beside it.
func TestReclaimCompletedConfigExchangesCollectsARecordBesideItsMarker(t *testing.T) {
	s := checkedWriteSession(t)
	record := configExchangeRecord{
		Version:      configExchangeRecordVersion,
		Candidate:    configCandidatePrefix + strings.Repeat("ab", 8),
		Previous:     FileIdentity{Device: 1, Inode: 2},
		Staged:       FileIdentity{Device: 1, Inode: 3},
		ExpectDigest: "sha256:" + strings.Repeat("ab", 32),
		DataDigest:   "sha256:" + strings.Repeat("cd", 32),
	}
	if _, err := writeConfigExchangeRecord(s.writeRoots.recovery, record); err != nil {
		t.Fatal(err)
	}
	recordName, err := configExchangeRecordName(record.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	doneName, err := configExchangeDoneName(record.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	// The marker's bytes are deliberately not a valid completion: the sweep
	// finishes this exchange on the same evidence the pending scan finishes it
	// on, which is the marker's existence and nothing it contains.
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), doneName), []byte("not a completion"), 0o600); err != nil {
		t.Fatal(err)
	}

	collected, err := s.ReclaimCompletedConfigExchanges()
	if err != nil {
		t.Fatal(err)
	}
	if collected != 2 {
		t.Fatalf("collected %d entries, want the record and its marker both counted", collected)
	}
	if _, err := os.Lstat(filepath.Join(s.RecoveryDir(), recordName)); !os.IsNotExist(err) {
		t.Fatalf("a record whose marker is present is finished and must be collected, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.RecoveryDir(), doneName)); !os.IsNotExist(err) {
		t.Fatalf("a marker must be collected once its record is gone, err=%v", err)
	}
}

// The record goes first and the marker only once the record is gone, the same
// order inline reclamation is built on: removing a marker whose record survives
// turns a finished exchange back into pending work, and the next write command
// would replay it against a store that has already moved on. The record here
// cannot be collected at all -- it is a directory, which fu never writes and
// the identity-bound removal refuses to touch -- so the marker beside it has to
// stay too.
func TestReclaimCompletedConfigExchangesKeepsAMarkerWhoseRecordSurvives(t *testing.T) {
	s := checkedWriteSession(t)
	base := filepath.Join(s.RecoveryDir(), configExchangeRecordPrefix+strings.Repeat("ab", 8))
	if err := os.Mkdir(base+".json", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".done", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	collected, err := s.ReclaimCompletedConfigExchanges()
	if err != nil {
		t.Fatal(err)
	}
	if collected != 0 {
		t.Fatalf("collected %d entries; neither name is the sweep's to collect", collected)
	}
	if _, err := os.Lstat(base + ".json"); err != nil {
		t.Fatalf("a record the sweep cannot prove is fu's must be preserved, err=%v", err)
	}
	if _, err := os.Lstat(base + ".done"); err != nil {
		t.Fatalf("a marker whose record survives must survive with it, err=%v", err)
	}
}

// TestReclaimConfigExchangeResidueRemovesTheRecordBeforeItsMarker pins the
// order reclaimConfigExchangeResidue's own comment calls load-bearing, for the
// *inline* reclaim rather than gc's.
//
// The order is not decoration. readPendingConfigExchangeRecords treats a
// record with no marker beside it as pending work, so removing the marker
// first makes a finished exchange look interrupted and sends the next write
// command through a completed exchange again, against a store that has already
// moved on. That is the one silent, destructive failure mode in this file.
//
// Nothing pinned it. Swapping the loop to []string{doneName, recordName} left
// the whole internal/store suite green: the loop has no early exit, so both
// names are gone by the end of any completed run and only a crash between the
// two removals can tell the orders apart. gc's twin invariant is pinned by
// TestReclaimCompletedConfigExchangesKeepsAMarkerWhoseRecordSurvives above --
// that sweep genuinely conditions one removal on the other, so a frozen
// intermediate state is observable there and not here.
//
// So the sequence is observed directly, through a hook that is nil in
// production. That is a smaller change than injecting a crash into
// completeConfigExchange, and it fails on the swap, which is what was needed.
func TestReclaimConfigExchangeResidueRemovesTheRecordBeforeItsMarker(t *testing.T) {
	s := checkedWriteSession(t)
	suffix := strings.Repeat("cd", 8)
	recordName := configExchangeRecordPrefix + suffix + ".json"
	doneName := configExchangeRecordPrefix + suffix + ".done"
	for _, name := range []string{recordName, doneName} {
		if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var order []string
	reclaimConfigExchangeOrderHook = func(name string) { order = append(order, name) }
	t.Cleanup(func() { reclaimConfigExchangeOrderHook = nil })

	reclaimConfigExchangeResidue(s.writeRoots.recovery, configExchangeRecord{
		Candidate: configCandidatePrefix + suffix,
	}, recordName, doneName)

	if len(order) != 2 || order[0] != recordName || order[1] != doneName {
		t.Fatalf("the record must go first: a marker removed ahead of it leaves a bare record that reads as pending work; got %v", order)
	}
}

// TestCollectableConfigArchiveNamesAdmitsANameItDescribes is the positive half
// of the archive derivation. Only its negative direction was tested: replacing
// the result with an empty map left every status test green, which would move
// an archive `fu gc` really does unlink into "no command collects this yet".
//
// It lives here rather than in engine because it needs configArchiveName, and
// exporting a naming function for a test would be a worse trade than putting
// the test beside it.
func TestCollectableConfigArchiveNamesAdmitsANameItDescribes(t *testing.T) {
	s := checkedWriteSession(t)
	scratch := filepath.Join(s.RecoveryDir(), "scratch")
	if err := os.WriteFile(scratch, []byte("archived config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(scratch)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("expected a unix stat")
	}
	described := configArchiveName(FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)})
	if err := os.Rename(scratch, filepath.Join(s.RecoveryDir(), described)); err != nil {
		t.Fatal(err)
	}
	// A second name that states an identity it does not hold, so this asserts
	// a real division rather than "everything is admitted".
	undescribed := configArchiveName(FileIdentity{Device: 1, Inode: 2})
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), undescribed), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := s.CollectableConfigArchiveNames([]string{described, undescribed})
	if !got[described] {
		t.Fatalf("%s holds exactly the object it names and must be collectable", described)
	}
	if got[undescribed] {
		t.Fatalf("%s states an identity it does not hold and must not be", undescribed)
	}
}

// TestReclaimConfigExchangeRejectsANonRegularOwnNameBeforeRetiringIt is the
// mirror of TestReclaimConfigExchangeRejectsAnArchiveNameBeforeRetiringIt, on
// the half of the sweep that had no such test.
//
// reclaimConfigExchangeOwnName's doc comment claims the same load-bearing
// property its archive twin claims -- "a name holding anything but a regular
// file is left where it is rather than taken through the retirement rename" --
// and until this test the claim was unenforced: deleting requireRegularStat
// left the whole internal/store suite green, including
// TestReclaimCompletedConfigExchangesKeepsAMarkerWhoseRecordSurvives, whose
// comment nominates itself as the coverage. It is not: retireOwnedLeafAt's own
// S_IFREG check catches the directory *after* the retirement rename and hands
// it back, so every end-state assertion still holds. Only the position of the
// check distinguishes the two, and only this hook can see it.
func TestReclaimConfigExchangeRejectsANonRegularOwnNameBeforeRetiringIt(t *testing.T) {
	s := checkedWriteSession(t)
	// A well-formed record name -- so the sweep's grammar admits it and the
	// only thing standing between a directory and the rename is the pre-flight.
	name := configExchangeRecordPrefix + strings.Repeat("ab", 8) + ".json"
	if err := os.Mkdir(filepath.Join(s.RecoveryDir(), name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), name, "user-content.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A marker beside it, which is what makes the record collectable at all;
	// without one the pending scan claims the pair and the sweep never looks.
	doneName := configExchangeRecordPrefix + strings.Repeat("ab", 8) + ".done"
	if err := os.WriteFile(filepath.Join(s.RecoveryDir(), doneName), []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	reclaimConfigExchangeOwnNameHook = func(n string, admitted bool) { seen[n] = admitted }
	t.Cleanup(func() { reclaimConfigExchangeOwnNameHook = nil })

	if _, err := s.ReclaimCompletedConfigExchanges(); err != nil {
		t.Fatal(err)
	}

	admitted, examined := seen[name]
	if !examined {
		t.Fatalf("the sweep must consider %s at all; hook saw %v", name, seen)
	}
	if admitted {
		t.Fatalf("%s holds a directory, not a regular file, and must be rejected before any rename", name)
	}
	// And the end state fu never disturbed.
	if got, err := os.ReadFile(filepath.Join(s.RecoveryDir(), name, "user-content.txt")); err != nil || string(got) != "mine\n" {
		t.Fatalf("the directory and its content must be left exactly where they were: %q %v", got, err)
	}
}
