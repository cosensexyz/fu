// internal/cli/list_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fu/internal/skill"
	"fu/internal/store"
)

func TestListMatrixAndShow(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "disable", "alpha", "--agent", "codex")

	out, err := runCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "claude") {
		t.Fatalf("matrix incomplete:\n%s", out)
	}
	if !strings.Contains(out, "off*") {
		t.Fatalf("codex override must be marked with *:\n%s", out)
	}

	out, err = runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha", "sha256:", "codex: off (override)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show missing %q:\n%s", want, out)
		}
	}
}

// Self-review addition: list and show are the project's first read-only
// commands (SPEC §9); this proves it rather than trusting the absence of
// a Save()/Commit() call by inspection. It snapshots the store's commit
// history, its working tree content, and both detected agents' skills
// directories before and after running each command, and requires every
// snapshot to compare equal.
func TestListAndShowAreReadOnly(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	codexDir := filepath.Join(home, ".codex")
	os.MkdirAll(claudeDir, 0o755)
	os.MkdirAll(codexDir, 0o755)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	runCmd(t, "disable", "alpha", "--agent", "codex")

	st, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}

	type snapshot struct {
		log          []store.LogEntry
		dirty        bool
		storeDigest  string
		claudeDigest string
		codexDigest  string
	}
	take := func() snapshot {
		t.Helper()
		log, err := st.Log(50)
		if err != nil {
			t.Fatal(err)
		}
		dirty, err := st.IsDirty()
		if err != nil {
			t.Fatal(err)
		}
		storeDigest, err := skill.Digest(st.Dir())
		if err != nil {
			t.Fatal(err)
		}
		claudeDigest, err := skill.Digest(claudeDir)
		if err != nil {
			t.Fatal(err)
		}
		codexDigest, err := skill.Digest(codexDir)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot{log, dirty, storeDigest, claudeDigest, codexDigest}
	}

	before := take()
	if before.dirty {
		t.Fatal("store must be clean before the read-only commands run")
	}

	if _, err := runCmd(t, "list"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "show", "alpha"); err != nil {
		t.Fatal(err)
	}

	after := take()
	if len(before.log) != len(after.log) {
		t.Fatalf("commit count changed: %d -> %d", len(before.log), len(after.log))
	}
	for i := range before.log {
		if before.log[i].Hash != after.log[i].Hash {
			t.Fatalf("history rewritten at entry %d: %s -> %s", i, before.log[i].Hash, after.log[i].Hash)
		}
	}
	if after.dirty {
		t.Fatal("store worktree left dirty by a read-only command")
	}
	if before.storeDigest != after.storeDigest {
		t.Fatal("store content changed")
	}
	if before.claudeDigest != after.claudeDigest {
		t.Fatal("claude skills directory changed")
	}
	if before.codexDigest != after.codexDigest {
		t.Fatal("codex skills directory changed")
	}
}

// Self-review addition: an empty store (init only, no skills yet) must
// still print a usable header instead of failing or printing nothing.
func TestListEmptyStoreShowsHeader(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")

	out, err := runCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SKILL") || !strings.Contains(out, "GLOBAL") {
		t.Fatalf("empty store must still print a header:\n%q", out)
	}
}

// Self-review addition: with no agent detected, the matrix must degrade to
// a SKILL/GLOBAL-only table and the detail view to global-only lines,
// rather than erroring or printing empty agent columns.
func TestListAndShowWithNoAgentsDetected(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home) // no .claude or .codex: no agents detected
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "GLOBAL") {
		t.Fatalf("matrix must still list the skill and global column:\n%q", out)
	}
	if strings.Contains(out, "claude") || strings.Contains(out, "codex") {
		t.Fatalf("no agent detected must mean no agent columns:\n%q", out)
	}

	out, err = runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name:") || !strings.Contains(out, "global:") {
		t.Fatalf("show must still report global-level details:\n%q", out)
	}
	if strings.Contains(out, "claude:") || strings.Contains(out, "codex:") {
		t.Fatalf("no agent detected must mean no per-agent lines:\n%q", out)
	}
}

// Self-review addition: an unknown skill name must fail cleanly rather
// than panicking or falling through to a blank report.
func TestShowUnknownSkillFails(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")

	_, err := runCmd(t, "show", "ghost")
	if err == nil {
		t.Fatal("unknown skill must fail")
	}
	if !strings.Contains(err.Error(), "unknown skill") {
		t.Fatalf("error should name the problem, got: %v", err)
	}
}

// Self-review addition: a skill registered in fu.yaml whose SKILL.md was
// deleted out of band (e.g. manual edit outside fu) must still report
// what fu.yaml knows, rather than failing outright.
func TestShowSkillWithMissingSkillMD(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	if err := os.Remove(filepath.Join(fuHome, "store", "skills", "alpha", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "SKILL.md unreadable") {
		t.Fatalf("show must degrade gracefully, not fail outright:\n%q", out)
	}
	if !strings.Contains(out, "digest:") || !strings.Contains(out, "global:") {
		t.Fatalf("show must still report what fu.yaml knows:\n%q", out)
	}
}

// TestListAndShowSurviveOneInvalidNameInConfig guards round 4 finding 2's
// adjacent coordinate. TestShowRejectsPathTraversalNamePlantedViaFuYaml
// (round 3 finding 2) only ever looks up the invalid name itself; it never
// asks what happens to a *different*, validly-named skill recorded
// alongside one that fails validation. Reproduced against the compiled
// binary pre-fix -- fu.yaml hand-edited to add invalid entry "Beta"
// alongside the real, valid skill "alpha":
//
//	fu list          exit=1  error: fu.yaml: skills.Beta invalid skill name: ...
//	fu show alpha    exit=1  error: fu.yaml: skills.Beta invalid skill name: ...
//
// Neither error names fu.yaml's path, and hand-editing it by feel was the
// only way out -- for an unrelated skill, not merely the bad entry itself.
// Both commands must now survive one invalid name elsewhere in the config
// and say something actionable about it.
func TestListAndShowSurviveOneInvalidNameInConfig(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "skills:\n  alpha:",
		"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: true\n  alpha:", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected seed content to edit")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "list")
	if err != nil {
		t.Fatalf("fu list must survive one invalid name elsewhere in the config, got %v", err)
	}
	if !strings.Contains(out, "alpha") {
		t.Fatalf("the valid skill must still be listed, got %q", out)
	}
	if !strings.Contains(out, "Beta") {
		t.Fatalf("list must say something actionable about the invalid name, got %q", out)
	}

	out, err = runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatalf("fu show alpha must survive an unrelated invalid name in the config, got %v", err)
	}
	if !strings.Contains(out, "name:        alpha") {
		t.Fatalf("show must still print the valid skill's own content, got %q", out)
	}
	if !strings.Contains(out, "Beta") {
		t.Fatalf("show must say something actionable about the invalid name too, got %q", out)
	}
}

// TestListAndShowWarnOnVersionTooNew closes round 4 doc item 3's
// documentation-vs-code discrepancy: DESIGN §3 has always said a version
// higher than this build supports makes read-only commands proceed
// best-effort *with a warning*, and the write side implements exactly that
// (store.Config.CheckWritable, enforced by engine.Run before Sweep). The
// read side did not -- list and show printed a complete, normal-looking
// report with nothing on stderr and exit 0, silently contradicting the
// design's own documented behavior. Reproduced against the compiled binary
// pre-fix: hand-editing fu.yaml's `version: 1` to `version: 99` and running
// `fu list` or `fu show alpha` produced no diagnostic of any kind.
func TestListAndShowWarnOnVersionTooNew(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "version: 1", "version: 99", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected version line to edit")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "list")
	if err != nil {
		t.Fatalf("a too-new version must not fail a read-only command, got %v", err)
	}
	if !strings.Contains(out, cfgPath) {
		t.Fatalf("the warning must name fu.yaml's own path, got %q", out)
	}
	if !strings.Contains(out, "alpha") {
		t.Fatalf("the normal report must still appear, got %q", out)
	}

	out, err = runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatalf("a too-new version must not fail a read-only command, got %v", err)
	}
	if !strings.Contains(out, cfgPath) {
		t.Fatalf("the warning must name fu.yaml's own path, got %q", out)
	}
	if !strings.Contains(out, "name:        alpha") {
		t.Fatalf("the normal report must still appear, got %q", out)
	}
}

// Self-review addition: the brief's own test only proves the override
// marker appears when an override exists (codex "off*"). This proves the
// converse for both commands -- an agent that merely follows the global
// default must never carry "*" in the matrix, and must be spelled out as
// "(follows global)" in the detail view -- the two display details the
// task calls out as meaningful.
func TestListAndShowMarkOverridesOnlyWhenPresent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	runCmd(t, "init")
	runCmd(t, "new", "alpha") // no per-agent overrides at all

	out, err := runCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "*") {
		t.Fatalf("no overrides exist; matrix must carry no marker:\n%q", out)
	}

	out, err = runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claude: on (follows global)", "codex: on (follows global)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show missing %q:\n%s", want, out)
		}
	}
}

// TestReadOnlyDiagnosticsGoToStderrOnly pins both the stream and the
// absence of list/show's two diagnostics. Round 5 finding: three separate
// mutations to internal/cli/root.go left the entire suite green --
// printVersionWarning writing to stdout, printInvalidNames writing to
// stdout, and printVersionWarning losing its `if !cfg.VersionTooNew()`
// guard altogether (warning on every run, at every version). The first two
// survived because every existing assertion reads runCmd's merged buffer,
// which cannot distinguish the streams; the third because no test ever
// asserted that a *supported* version produces no warning at all. A
// diagnostic on the wrong stream is the same defect printResult was fixed
// for (finding 4): `fu list >/dev/null` would swallow it, or a script
// parsing stdout would choke on it.
func TestReadOnlyDiagnosticsGoToStderrOnly(t *testing.T) {
	// seedStore returns the config path of a fresh store holding one real
	// skill, with fu.yaml rewritten by edit. anchor is the text edit expects
	// to find and is checked separately, since one case below deliberately
	// rewrites "version: 1" to itself.
	seedStore := func(t *testing.T, anchor string, edit func(raw string) string) string {
		t.Helper()
		fuHome, home := t.TempDir(), t.TempDir()
		t.Setenv("FU_HOME", fuHome)
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		runCmd(t, "init")
		runCmd(t, "new", "alpha")

		cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), anchor) {
			t.Fatalf("setup check: fu.yaml no longer contains the anchor %q this test edits", anchor)
		}
		if err := os.WriteFile(cfgPath, []byte(edit(string(raw))), 0o644); err != nil {
			t.Fatal(err)
		}
		return cfgPath
	}

	t.Run("version warning lands on stderr and never on stdout", func(t *testing.T) {
		cfgPath := seedStore(t, "version: 1", func(raw string) string {
			return strings.Replace(raw, "version: 1", "version: 99", 1)
		})
		for _, args := range [][]string{{"list"}, {"show", "alpha"}} {
			stdout, stderr, err := runCmdSplit(t, args...)
			if err != nil {
				t.Fatalf("%v: a too-new version must not fail a read-only command: %v", args, err)
			}
			if !strings.Contains(stderr, cfgPath) || !strings.Contains(stderr, "warning:") {
				t.Errorf("%v: the warning must reach stderr, got stderr=%q", args, stderr)
			}
			if strings.Contains(stdout, "warning:") || strings.Contains(stdout, cfgPath) {
				t.Errorf("%v: a diagnostic must never be mixed into the command's own output, got stdout=%q", args, stdout)
			}
			if !strings.Contains(stdout, "alpha") {
				t.Errorf("%v: the normal report must still go to stdout, got stdout=%q", args, stdout)
			}
		}
	})

	t.Run("a supported version warns about nothing at all", func(t *testing.T) {
		// Round 8 narrowed this list, reversing what it used to assert. It
		// ran over {"version: 1", "version: 0", "# no version key"} on the
		// reading that anything not *above* SupportedVersion was supported.
		// That was the defect stated as a requirement: version 0 is not a
		// schema fu ever defined, and a file declaring no version at all was
		// being read and rewritten under this build's assumptions. Both are
		// now refused at load (see TestPersistedConfigMustDeclareASupportedVersion),
		// so they no longer belong to "supported" -- and this case is about
		// not crying wolf over versions that are.
		//
		// Only one schema exists today, so the list is a single entry. It
		// stays a loop because MinSupportedVersion and SupportedVersion are
		// separate constants precisely so the range can widen.
		for _, version := range []string{"version: 1"} {
			t.Run(version, func(t *testing.T) {
				seedStore(t, "version: 1", func(raw string) string {
					return strings.Replace(raw, "version: 1", version, 1)
				})
				for _, args := range [][]string{{"list"}, {"show", "alpha"}} {
					_, stderr, err := runCmdSplit(t, args...)
					if err != nil {
						t.Fatalf("%v: %v", args, err)
					}
					if strings.Contains(stderr, "warning:") {
						t.Errorf("%v: %q is supported; warning about it teaches the user to ignore warnings, got stderr=%q",
							args, version, stderr)
					}
				}
			})
		}
	})

	t.Run("invalid-name diagnostic lands on stderr and never on stdout", func(t *testing.T) {
		seedStore(t, "skills:\n  alpha:", func(raw string) string {
			return strings.Replace(raw, "skills:\n  alpha:",
				"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: true\n  alpha:", 1)
		})
		for _, args := range [][]string{{"list"}, {"show", "alpha"}} {
			stdout, stderr, err := runCmdSplit(t, args...)
			if err != nil {
				t.Fatalf("%v: one invalid name must not fail a read-only command: %v", args, err)
			}
			if !strings.Contains(stderr, "Beta") || !strings.Contains(stderr, "invalid:") {
				t.Errorf("%v: the invalid-name diagnostic must reach stderr, got stderr=%q", args, stderr)
			}
			if strings.Contains(stdout, "Beta") || strings.Contains(stdout, "invalid:") {
				t.Errorf("%v: a diagnostic must never be mixed into the command's own output, got stdout=%q", args, stdout)
			}
		}
	})
}
