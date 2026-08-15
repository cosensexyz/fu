package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func configExchangeCompletionOutcome(t *testing.T, checked *Store, record configExchangeRecord) string {
	t.Helper()
	done, err := configExchangeDoneName(record.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	doneRaw, err := os.ReadFile(filepath.Join(checked.RecoveryDir(), done))
	if err != nil {
		t.Fatal(err)
	}
	var completion configExchangeCompletion
	if err := json.Unmarshal(doneRaw, &completion); err != nil {
		t.Fatal(err)
	}
	return completion.Outcome
}

func TestConfigExchangeActiveStagedConvergesAfterMismatchedPreviousIsRestored(t *testing.T) {
	checked, record, raw := pendingActiveConfigExchange(t)
	if err := os.WriteFile(checked.ConfigPath(), []byte("third-party config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverConfigExchange(checked.writeRoots.store, checked.writeRoots.staging, checked.writeRoots.recovery, record, raw); err != nil {
		t.Fatalf("restored previous inode with changed bytes must converge: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(checked.StagingDir(), configSwapName)); !os.IsNotExist(err) {
		t.Fatalf("converged recovery must archive the staged active object: %v", err)
	}
	if got := configExchangeCompletionOutcome(t, checked, record); got != "withdrawn-after-precondition-mismatch" {
		t.Fatalf("completion outcome = %q, want mismatch withdrawal", got)
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

func TestConfigExchangeActiveStagedRecordsTerminalState(t *testing.T) {
	checked, record, raw := pendingActiveConfigExchange(t)
	if err := recoverConfigExchange(checked.writeRoots.store, checked.writeRoots.staging, checked.writeRoots.recovery, record, raw); err != nil {
		t.Fatal(err)
	}
	if got := configExchangeCompletionOutcome(t, checked, record); got != "withdrawn-with-previous-current" {
		t.Fatalf("completion outcome = %q, want a terminal-state label", got)
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

			// Recovery must converge, and must be idempotent: running it twice
			// exercises the already-completed arms as well.
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
