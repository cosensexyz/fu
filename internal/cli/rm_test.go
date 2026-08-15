package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRmCommand removes a skill installed via add and reclaims the agent
// link.
func TestRmCommand(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "pdf-tools")
	if _, err := runCmd(t, "add", srcDir); err != nil {
		t.Fatal(err)
	}
	// Split streams, not runCmd's merged buffer: `removed <name>` is the line
	// this branch added, and it is a result, so it belongs on stdout. Both of
	// rm's tests used runCmd, so moving that line to stderr left the suite
	// green -- the exact regression class runCmdSplit was introduced for.
	stdout, stderr, err := runCmdSplit(t, "rm", "pdf-tools")
	if err != nil {
		t.Fatalf("rm: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "removed pdf-tools") {
		t.Fatalf("the confirmation must reach stdout: %q (stderr %q)", stdout, stderr)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "pdf-tools")); !os.IsNotExist(err) {
		t.Fatalf("agent link must be reclaimed, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(fuHome, "store", "skills", "pdf-tools")); !os.IsNotExist(err) {
		t.Fatalf("store entity must be gone, err=%v", err)
	}
}

// TestRmUnknownName exits non-zero with a clear message.
func TestRmUnknownName(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	_, err := runCmd(t, "rm", "ghost")
	if err == nil || !strings.Contains(err.Error(), "unknown skill") {
		t.Fatalf("unknown skill must be refused, got %v", err)
	}
}

func TestRmExplainsAnInvalidConfigName(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "skills: {}",
		"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: true\n", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected seed content")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCmdSplit(t, "rm", "Beta")
	if err == nil {
		t.Fatal("an invalid config name cannot be removed as a skill")
	}
	combined := err.Error() + stderr
	for _, want := range []string{"Beta", "validation", cfgPath, "edit"} {
		if !strings.Contains(combined, want) {
			t.Errorf("diagnostic must contain %q, got err=%v stderr=%q", want, err, stderr)
		}
	}
	if strings.Contains(combined, "unknown skill") {
		t.Fatalf("a name visibly present in fu.yaml must not be called unknown: err=%v stderr=%q", err, stderr)
	}
}

// TestRmPartialFailureSplitsStreams covers the half no test reached end to
// end: the removal committed, but reconcile hit a genuine per-agent failure,
// so the command must confirm what durably happened on stdout, report the
// failure on stderr, and still exit non-zero. A script capturing only stdout
// sees the confirmation; one capturing only the exit code sees the failure.
func TestRmPartialFailureSplitsStreams(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	for _, dir := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runCmd(t, "init")
	srcDir := t.TempDir()
	writeSkill(t, srcDir, "pdf-tools")
	if _, err := runCmd(t, "add", srcDir); err != nil {
		t.Fatal(err)
	}
	// codex's skills directory becomes a plain file: ScanAgent fails for that
	// agent, reconcile isolates it into Failed, and the pass returns
	// ErrOperationFailed -- after fu.yaml and the commit are already durable.
	codexSkills := filepath.Join(home, ".codex", "skills")
	if err := os.RemoveAll(codexSkills); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexSkills, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCmdSplit(t, "rm", "pdf-tools")
	if err == nil {
		t.Fatalf("a per-agent failure must still exit non-zero: %q / %q", stdout, stderr)
	}
	if !strings.Contains(stdout, "removed pdf-tools") {
		t.Fatalf("the durable half must still be confirmed on stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "failed:") {
		t.Fatalf("the per-agent failure must be reported on stderr: %q", stderr)
	}
	if strings.Contains(stdout, "failed:") {
		t.Fatalf("diagnostics must not leak onto stdout: %q", stdout)
	}
	if _, statErr := os.Lstat(filepath.Join(fuHome, "store", "skills", "pdf-tools")); !os.IsNotExist(statErr) {
		t.Fatalf("the removal itself must be durable, err=%v", statErr)
	}
}
