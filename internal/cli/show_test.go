// internal/cli/show_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

type fakeShowApplication struct {
	outcome engine.ShowOutcome
	err     error
}

func (f fakeShowApplication) ShowSkill(string) (engine.ShowOutcome, error) {
	return f.outcome, f.err
}

func TestShowSourceFieldsStayOnOneLine(t *testing.T) {
	cmd := newShowCmd(fakeShowApplication{outcome: engine.ShowOutcome{
		Description: "d",
		Source:      map[string]string{"subdir": "tools/alpha\nglobal: off"},
	}})
	stdout, err := executeCommandForOutcomeTest(cmd, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stdout, "global:") != 2 {
		t.Fatalf("a source value must not forge an additional global field:\n%s", stdout)
	}
	if strings.Contains(stdout, "\nglobal: off\n") {
		t.Fatalf("source field newline was not flattened:\n%s", stdout)
	}
}

// Round 3 finding 2: `fu show` joined an unvalidated fu.yaml key straight
// onto st.SkillsDir() (internal/cli/show.go), the other place a name
// becomes a path component that round 2 finding 3's Diff-level validation
// never covered. fu.yaml is hand-editable today, and a future clone/pull
// will populate it from a network source, so a key like "../../evilskill"
// is a genuine trust boundary, not a theoretical one.
//
// The actual fix lives in store.LoadConfig (internal/store/config.go).
// Originally (round 3 finding 2's bdf2882) it rejected the whole file
// outright whenever any skill name failed validation; round 4 finding 2
// downgraded that to per-entry isolation -- such a name is now excluded
// from the config's skill set instead, so show's own (unchanged)
// `cfg.HasSkill(name)` check simply reports it absent, the same "unknown
// skill" error path an ordinary typo would hit. Either mechanism keeps
// this test's own assertions true (err != nil, no leaked content), which
// is what this test exercises end to end (openStoreAndConfig -> LoadConfig
// -> show's RunE), matching exactly how the reviewer reproduced it against
// the compiled binary: a hand-added fu.yaml entry named "../../evilskill",
// with real, differently-named content planted at the escaped location one
// level above the store's skills directory. Pre-fix, `fu show
// '../../evilskill'` printed that content ("name: pwned", "description:
// content read from OUTSIDE the store skills dir") and exited 0.
func TestShowRejectsPathTraversalNamePlantedViaFuYaml(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")

	// Real content one level above $FU_HOME/store (i.e. outside the store's
	// skills directory entirely), reachable only by escaping through "..".
	evil := filepath.Join(fuHome, "evilskill")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: pwned\ndescription: content read from OUTSIDE the store skills dir\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(evil, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hand-edit fu.yaml the way a stray entry from an older fu, a manual
	// edit, or (in a later plan) a clone/pull might: register the
	// traversal key directly, standing in for content this build never
	// wrote itself.
	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "skills: {}",
		"skills:\n  ../../evilskill:\n    digest: sha256:x\n    enabled: true\n", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected seed content to edit")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "show", "../../evilskill")
	if err == nil {
		t.Fatalf("an invalid skill name must be refused, not shown, got output %q", out)
	}
	if strings.Contains(out, "pwned") || strings.Contains(out, "OUTSIDE the store") {
		t.Fatalf("content from outside the store must never be printed, got %q", out)
	}
}

// Reverse direction: this same invalid entry must not silently disable
// fu.yaml -- a *different*, well-formed fu.yaml (no invalid names at all)
// must keep loading and showing normally, and once the invalid entry is
// removed by hand (the only recovery path today; this plan ships no `fu
// rm`), an unrelated command must work again. This guards against an
// overly broad fix that rejects every config, not just ones actually
// carrying an invalid name.
func TestShowStillWorksWhenNoInvalidNamesPresent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "show", "alpha")
	if err != nil {
		t.Fatalf("a well-formed config must still load and show normally, got %v", err)
	}
	if !strings.Contains(out, "name:        alpha") {
		t.Fatalf("show must still print the real skill's own content, got %q", out)
	}
}

// TestShowExplainsAnInvalidNameItWasAskedFor is round 7's diagnostic
// finding. HasSkill excludes names LoadConfig isolated, so `fu show <that
// name>` returned a bare "unknown skill" and stopped -- never reaching
// printInvalidNames further down. The entry is sitting in fu.yaml, plainly
// visible to the user, and fu's answer was that it does not exist. The
// command that a confused user reaches for first was the one command that
// would not explain the situation.
func TestShowExplainsAnInvalidNameItWasAskedFor(t *testing.T) {
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
	edited := strings.Replace(string(raw), "skills:\n  alpha:",
		"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: true\n  alpha:", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected content to edit")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCmdSplit(t, "show", "Beta")
	if err == nil {
		t.Fatal("an invalid name is still not a skill that can be shown; the command must fail")
	}
	// What it must not say is "unknown", with nothing else: the name is in
	// the file, and the user needs to know why it is being ignored and
	// where to fix it.
	combined := err.Error() + stderr
	if !strings.Contains(combined, "Beta") {
		t.Errorf("the diagnostic must name the entry asked for, got err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(combined, cfgPath) {
		t.Errorf("the diagnostic must name the file to repair, got err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(combined, "validation") {
		t.Errorf("the diagnostic must say why the entry is ignored, got err=%v stderr=%q", err, stderr)
	}
}

func TestShowWarnsAboutTooNewConfigBeforeLookupErrors(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if _, _, err := runCmdSplit(t, "init"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmdSplit(t, "new", "alpha"); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "version: 1", "version: 99", 1)
	edited = strings.Replace(edited, "skills:\n  alpha:",
		"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: true\n  alpha:", 1)
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"missing", "Beta"} {
		t.Run(name, func(t *testing.T) {
			_, stderr, err := runCmdSplit(t, "show", name)
			if err == nil {
				t.Fatalf("show %q must still return its lookup error", name)
			}
			if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, cfgPath) {
				t.Fatalf("too-new warning must precede the lookup error, stderr=%q err=%v", stderr, err)
			}
		})
	}
}

// Frontmatter is user-editable content (scenario 7), so it must never be able
// to assert an identity fu does not use, nor forge a later field.
func TestShowValidatesAndContainsFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		frontmatter string
		wantWarning bool
		mustNotHave string
	}{
		{
			name:        "name mismatch is reported, not adopted",
			frontmatter: "name: totally-different\ndescription: I am not alpha",
			wantWarning: true,
			mustNotHave: "name:        totally-different",
		},
		{
			name:        "multi-line description cannot forge a field",
			frontmatter: "name: alpha\ndescription: \"line1\\nglobal:      off\"",
			mustNotHave: "\nglobal:      off\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			skillFile := filepath.Join(fuHome, "store", "skills", "alpha", "SKILL.md")
			if err := os.WriteFile(skillFile, []byte("---\n"+tt.frontmatter+"\n---\n\nbody\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			out, err := runCmd(t, "show", "alpha")
			if err != nil {
				t.Fatalf("show must still succeed: %v (%s)", err, out)
			}
			if !strings.Contains(out, "name:        alpha\n") {
				t.Errorf("the identity line must be the store's own name, got:\n%s", out)
			}
			if tt.mustNotHave != "" && strings.Contains(out, tt.mustNotHave) {
				t.Errorf("output contains %q it must not:\n%s", tt.mustNotHave, out)
			}
			if tt.wantWarning && !strings.Contains(out, "warning:") {
				t.Errorf("an invalid frontmatter must be reported, got:\n%s", out)
			}
		})
	}
}

// A read command must not resolve a store-internal symlink outward.
func TestShowRefusesASymlinkedSkillDirectory(t *testing.T) {
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
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := "---\nname: alpha\ndescription: content from OUTSIDE the store\n---\n"
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(fuHome, "store", "skills", "alpha")
	if err := os.RemoveAll(skillDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, skillDir); err != nil {
		t.Fatal(err)
	}

	out, _ := runCmd(t, "show", "alpha")
	if strings.Contains(out, "OUTSIDE the store") {
		t.Fatalf("show followed a symlink out of the store:\n%s", out)
	}
}
