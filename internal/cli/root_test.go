package cli

import (
	"bytes"
	"errors"
	"github.com/cosensexyz/fu/internal/engine"
	"github.com/spf13/cobra"
	"strings"
	"testing"
)

func TestRootHelp(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "skill manager") {
		t.Fatalf("help output missing description: %q", out.String())
	}
}

// A failure isolated to one entry names both the agent and the entry.
func TestPrintResultNamesAnEntryLevelFailure(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetErr(&out)
	printResult(cmd, engine.Result{Failed: []engine.FailedAction{{
		Action: engine.Action{Type: engine.ReportFailed, AgentName: "claude", Skill: "loop"},
		Err:    errors.New("too many levels of symbolic links"),
	}}})
	if !strings.Contains(out.String(), "failed: claude/loop: too many levels of symbolic links") {
		t.Fatalf("output = %q", out.String())
	}
}
