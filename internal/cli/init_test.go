package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
)

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// runCmdSplit is runCmd with stdout and stderr kept apart. runCmd merges
// them, which is convenient for asserting that some text appeared at all --
// but it cannot tell whether a diagnostic reached the stream it was
// supposed to. Round 5 finding, established by mutation: moving
// printVersionWarning's or printInvalidNames' output from stderr to stdout
// left the whole suite green, because every assertion about either went
// through runCmd's single buffer. See exitcode_test.go for the same
// two-buffer treatment of printResult.
func runCmdSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestInitCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FU_HOME", home)
	if _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "store", "fu.yaml")); err != nil {
		t.Fatal("store not created")
	}
	_, err := runCmd(t, "init")
	if !errors.Is(err, store.ErrStoreExists) {
		t.Fatalf("second init must fail with ErrStoreExists, got %v", err)
	}
}
