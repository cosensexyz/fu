package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShowDisplaysSource pins that `fu show` renders the source record and
// the locked commit (SPEC §5.1 show: 来源、锁定版本) once a skill carries
// one. The source block is line-oriented like every other field, so a
// multi-line value cannot forge a later field; values are rendered as
// `key: value` lines.
func TestShowDisplaysSource(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	if _, err := runCmd(t, "new", "alpha"); err != nil {
		t.Fatal(err)
	}

	// Plant a source record the way an add command would write one.
	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw),
		"    enabled: true",
		`    enabled: true
    source:
      type: git
      url: https://example.com/skills.git
      ref: refs/heads/main
      ref_kind: branch
      commit: a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c
      subdir: alpha`, 1)
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, out)
	}
	for _, want := range []string{
		"source:",
		"  type:     git",
		"  url:      https://example.com/skills.git",
		"  ref:      refs/heads/main",
		"  commit:   a1b2c3d4e5f6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestShowOmitsSourceWhenAbsent pins that a skill without a source record
// (e.g. one created by `fu new`) shows no source block at all.
func TestShowOmitsSourceWhenAbsent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	if _, err := runCmd(t, "new", "alpha"); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, out)
	}
	if strings.Contains(out, "source:") {
		t.Errorf("no source record must mean no source block:\n%s", out)
	}
}
