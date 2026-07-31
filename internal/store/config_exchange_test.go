package store

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// recoverConfigExchange has six arms and only "installed" had a real crash
// test. Each arm below is reached by killing a process at the durable boundary
// that produces its state, then reopening and requiring convergence.
//
// The "withdrawn-after-precondition-mismatch" arm matters most: it is the one
// place recovery performs a renameExchange of its own, so it is the one a
// refactor would break silently.
func TestConfigExchangeRecoveryConvergesFromEveryCrashPoint(t *testing.T) {
	stages := []string{"after-record", "before-exchange", "after-exchange", "after-exchange-mismatch"}
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
			if stage == "after-exchange-mismatch" && !bytes.Equal(current, original) {
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
