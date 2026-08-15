// internal/store/config_test.go
package store

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLoadConfigRefusesFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fu.yaml")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := LoadConfig(path)
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("FIFO config must fail with an unsupported-type error, got %v", err)
		}
	case <-time.After(time.Second):
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-result:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("LoadConfig blocked while opening a FIFO")
	}
}

func TestLoadConfigRefusesFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nskills: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fu.yaml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig followed a final symlink")
	}
}

func TestLoadConfigRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fu.yaml")
	raw := append([]byte("version: 1\nskills: {}\n"), bytes.Repeat([]byte("# padding\n"), (9<<20)/10)...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized config must be refused explicitly, got %v", err)
	}
}

func TestLoadConfigRejectsSameInodeMutationAtFinalBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fu.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nskills: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadConfigWithHooks(path, regularFileReadHooks{beforePostStat: func() error {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		if _, err := file.Write([]byte("version: 1\nskills:\n  changed: {enabled: true}\n")); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		changed := before.ModTime().Add(2 * time.Second)
		return os.Chtimes(path, changed, changed)
	}})
	if !errors.Is(err, errRegularFileChanged) {
		t.Fatalf("same-inode config mutation must be rejected as an unstable read, got %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("test mutation unexpectedly replaced the config inode")
	}
}

func TestConfigRoundTripPreservesUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := `version: 1
future_top_level: keep-me
skills:
  alpha:
    digest: sha256:abc
    enabled: true
    future_field: keep-me-too
`
	os.WriteFile(p, []byte(raw), 0o644)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	c.SetEnabled("alpha", false)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	for _, want := range []string{"future_top_level: keep-me", "future_field: keep-me-too", "enabled: false"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSameValueNormalization(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	c := NewConfig(p)
	c.AddSkill("alpha", "sha256:x")
	// override differing from global is recorded
	c.SetAgent("alpha", "codex", false)
	if v, ok := c.Override("alpha", "codex"); !ok || v {
		t.Fatal("override codex=false should be recorded")
	}
	// setting it back equal to global removes the override
	c.SetAgent("alpha", "codex", true)
	if _, ok := c.Override("alpha", "codex"); ok {
		t.Fatal("override equal to global must be normalized away")
	}

	// Round 3 finding 4: a global flip must NOT drop an override that
	// becomes equal to it, reversing this test's own prior assertion here
	// (it used to require the opposite: "global flip must normalize
	// now-equal overrides"). SPEC §4.1 gives the agent-level setter the
	// normalization rule and separately says a global toggle preserves
	// overrides; this build follows that scope distinction -- see SetEnabled's
	// doc comment for the full reasoning. The normalizing helper now runs
	// only from SetAgent.
	c.SetAgent("alpha", "codex", false)
	c.SetEnabled("alpha", false)
	if v, ok := c.Override("alpha", "codex"); !ok || v {
		t.Fatal("a global flip must preserve an override that becomes equal to the new global value")
	}

	// The agent-level setter must still normalize even after a global flip
	// happened to put the override and the global at the same value:
	// setting codex explicitly (still to false, still equal to the current
	// global) through SetAgent -- not SetEnabled -- must clear it, since
	// agent-level writes remain the only way an override is ever cleared.
	c.SetAgent("alpha", "codex", false)
	if _, ok := c.Override("alpha", "codex"); ok {
		t.Fatal("an explicit agent-level write equal to global must still normalize the override away")
	}
}

// TestAgentSwitchWriteDoesNotNormalizeAnotherAgentsOverride guards round 4
// finding 1: SetAgent used to normalize via a shared normalize(entry,
// global) helper that walked *every* override recorded on the skill and
// deleted each one equal to the given global value -- not just the single
// (name, agent) key actually being written. TestSameValueNormalization
// above only ever calls SetAgent for codex and only ever checks what
// SetEnabled (the global setter) does to it; it never writes a *different*
// agent's switch and asks what that does to codex's own override, so it
// never exercised the shared helper's blast radius onto an agent the write
// never named.
//
// Reproduced against the compiled binary pre-fix, the reviewer's exact
// four ordinary commands (codex is never touched again after step 1):
//
//  1. fu disable alpha --agent codex  -> overrides: {codex: false}
//  2. fu disable alpha                -> enabled: false, overrides: {codex: false}   (correctly preserved)
//  3. fu disable alpha --agent claude -> enabled: false, overrides section gone      (codex silently destroyed)
//  4. fu enable alpha                 -> fu list: alpha  on  on  on, and
//     ~/.codex/skills/alpha materialized -- an agent the user explicitly
//     disabled now has the skill.
func TestAgentSwitchWriteDoesNotNormalizeAnotherAgentsOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	c := NewConfig(p)
	c.AddSkill("alpha", "sha256:x")

	// 1) codex gets an explicit override, differing from global (true).
	c.SetAgent("alpha", "codex", false)
	if v, ok := c.Override("alpha", "codex"); !ok || v {
		t.Fatal("setup: codex override should be recorded as false")
	}

	// 2) global flip to false: SetEnabled never normalizes (round 3 finding
	// 4), so codex's override -- now coincidentally equal to the new
	// global -- must survive untouched. This step alone is already covered
	// by TestSameValueNormalization; repeated here only to reach step 3's
	// exact starting state.
	c.SetEnabled("alpha", false)
	if v, ok := c.Override("alpha", "codex"); !ok || v {
		t.Fatal("setup: a global flip must preserve codex's override even once it equals the new global")
	}

	// 3) writing claude's own switch -- a completely different agent, whose
	// new value also happens to equal the current global -- must never
	// touch codex's unrelated override.
	c.SetAgent("alpha", "claude", false)
	if v, ok := c.Override("alpha", "codex"); !ok || v {
		t.Fatal("a write to claude's switch must not delete codex's unrelated override")
	}
}

// TestAgentSwitchNormalizationClearsOwnOverrideEvenAlongsideOthers is the
// reverse direction of the test above, and round 5's finding: with only
// that test in place, narrowing SetAgent's same-value branch to "normalize
// only when this is the *sole* override" left the suite green. Every
// existing case reaches the branch with at most one override recorded, so
// nothing distinguished "clear the key I was asked to write" from "clear
// the key I was asked to write, but only if it is alone".
//
// Both halves of the rule have to be pinned together: the call must clear
// its own override (that is what same-value normalization *is*, and what
// keeps a redundant entry from accumulating on every write), and it must
// leave every other agent's alone (round 4 finding 1) -- including one that
// is equally redundant, which is the case the two halves could be confused
// by.
func TestAgentSwitchNormalizationClearsOwnOverrideEvenAlongsideOthers(t *testing.T) {
	c := NewConfig(filepath.Join(t.TempDir(), "fu.yaml"))
	c.AddSkill("alpha", "sha256:x") // global: true

	// Two agents explicitly overridden to false while global is still true.
	c.SetAgent("alpha", "codex", false)
	c.SetAgent("alpha", "claude", false)

	// The global flips to false. SetEnabled never normalizes (round 3
	// finding 4), so *both* overrides are now redundant and both survive --
	// which is exactly the state the previous test never builds.
	c.SetEnabled("alpha", false)
	if _, ok := c.Override("alpha", "codex"); !ok {
		t.Fatal("setup: a global flip must preserve codex's override")
	}
	if _, ok := c.Override("alpha", "claude"); !ok {
		t.Fatal("setup: a global flip must preserve claude's override")
	}

	c.SetAgent("alpha", "claude", false) // same value as the global

	if _, ok := c.Override("alpha", "claude"); ok {
		t.Fatal("same-value normalization must clear the override this very call wrote, " +
			"whether or not other agents also have one")
	}
	if v, ok := c.Override("alpha", "codex"); !ok || v {
		t.Fatal("a write to claude's switch must not delete codex's unrelated override, " +
			"redundant or not")
	}
}

// TestSaveRoundTripsInvalidEntryUnchanged pins LoadConfig's own documented
// promise -- an isolated entry "round-trips unchanged rather than silently
// erasing it from fu.yaml" -- which round 5 found unguarded: dropping such
// an entry from skills.Content inside validateConfigTree left the whole
// suite green. That mutation is unrecoverable data loss today, since this
// `fu rm` refuses a name LoadConfig isolated, so hand-editing fu.yaml is the only way back; the
// entry would vanish as a side effect of any unrelated write.
func TestSaveRoundTripsInvalidEntryUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := `version: 1
skills:
  ../evil:
    digest: sha256:bad
    enabled: true
    overrides:
      codex: false
  alpha:
    digest: sha256:x
    enabled: true
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("one invalid name must not fail the load: %v", err)
	}
	if len(c.InvalidNames()) != 1 || c.InvalidNames()[0].Name != "../evil" {
		t.Fatalf("setup: want ../evil isolated, got %+v", c.InvalidNames())
	}

	// Any unrelated write, of the kind an ordinary command performs.
	c.SetEnabled("alpha", false)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// The whole entry, not merely its key: its digest and its overrides are
	// just as unrecoverable if a write silently drops them.
	for _, want := range []string{"../evil", "sha256:bad", "codex"} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("an unrelated write erased %q from fu.yaml -- unrecoverable, this plan "+
				"cannot be recovered with `fu rm`:\n%s", want, after)
		}
	}

	// Still isolated after the round trip: preserved in the file, but never
	// resurrected into the skill set (which is what makes fu show's own
	// path-escape guard hold, see HasSkill).
	reloaded, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.HasSkill("../evil") {
		t.Fatal("a preserved invalid entry must stay excluded from the skill set")
	}
	if len(reloaded.InvalidNames()) != 1 {
		t.Fatalf("want ../evil still reported as invalid after the round trip, got %+v", reloaded.InvalidNames())
	}
}

func TestEffective(t *testing.T) {
	c := NewConfig(filepath.Join(t.TempDir(), "fu.yaml"))
	c.AddSkill("alpha", "sha256:x")
	if !c.Effective("alpha", "claude") {
		t.Fatal("default must follow global=true")
	}
	c.SetAgent("alpha", "claude", false)
	if c.Effective("alpha", "claude") {
		t.Fatal("override must win over global")
	}
	if !c.Effective("alpha", "codex") {
		t.Fatal("codex has no override; follows global")
	}
}

func TestVersionGuard(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	os.WriteFile(p, []byte("version: 99\nskills: {}\n"), 0o644)
	c, err := LoadConfig(p) // read is best-effort
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != ErrVersionTooNew {
		t.Fatalf("want ErrVersionTooNew, got %v", err)
	}
}

// TestCheckWritable exercises the precondition Save's own guard is built
// on (finding I1): callers that need to know a config cannot be written
// *before* running a mutation -- not merely learn it after Save fails --
// must be able to ask directly.
func TestCheckWritable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	if err := os.WriteFile(p, []byte("version: 99\nskills: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckWritable(); err != ErrVersionTooNew {
		t.Fatalf("want ErrVersionTooNew, got %v", err)
	}

	c2, err := NewConfig(filepath.Join(t.TempDir(), "fu.yaml")), error(nil) // missing file -> SupportedVersion
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.CheckWritable(); err != nil {
		t.Fatalf("a supported version must be writable, got %v", err)
	}
}

// TestVersionTooNew exercises the read-side counterpart to CheckWritable
// (round 4 doc item 3): a read-only command needs to know the same
// condition -- the loaded version exceeds what this build supports -- but,
// unlike a write command, must not refuse to proceed over it, so it asks
// through a plain boolean rather than CheckWritable's error return.
func TestVersionTooNew(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	if err := os.WriteFile(p, []byte("version: 99\nskills: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.VersionTooNew() {
		t.Fatal("version 99 must be reported too new")
	}

	c2, err := NewConfig(filepath.Join(t.TempDir(), "fu.yaml")), error(nil) // missing file -> SupportedVersion
	if err != nil {
		t.Fatal(err)
	}
	if c2.VersionTooNew() {
		t.Fatal("a supported version must not be reported too new")
	}
}

// TestSaveWritesBlockStyleWithTwoSpaceIndent guards finding I2: both
// Init and defaultDoc seed fu.yaml with "skills: {}", and yaml.v3 marks
// a mapping node parsed from that flow-style "{}" syntax permanently --
// the mark is not just kept on that node but forces every descendant
// added later (each skill entry, each entry's overrides) to also render
// in flow style, since block style cannot nest inside an already-open
// flow collection. Reproduced against the compiled binary pre-fix: after
// `fu init`, `fu new writer`, `fu new pdf-tools`, and one agent-level
// override, fu.yaml collapsed to a single line,
// `skills: {writer: {digest: ..., enabled: true}, pdf-tools: {...}}`,
// contradicting the block-style examples in both SPEC §4.2 and DESIGN §3.
// This test seeds fu.yaml with the exact bytes Init writes, so it
// exercises the real on-disk path rather than only the in-memory
// default.
func TestSaveWritesBlockStyleWithTwoSpaceIndent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	if err := os.WriteFile(p, []byte("version: 1\nskills: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddSkill("writer", "sha256:aaa"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddSkill("pdf-tools", "sha256:bbb"); err != nil {
		t.Fatal(err)
	}
	c.SetAgent("pdf-tools", "codex", false)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `version: 1
skills:
  writer:
    digest: sha256:aaa
    enabled: true
  pdf-tools:
    digest: sha256:bbb
    enabled: true
    overrides:
      codex: false
`
	if string(out) != want {
		t.Fatalf("fu.yaml must be block style with a 2-space indent:\ngot:\n%s\nwant:\n%s", out, want)
	}
}

// TestSaveStaysBlockStyleAcrossReload is the permanence angle of finding
// I2: a config that is loaded, saved, reloaded, and saved again (the
// shape of real usage -- every write command re-opens fu.yaml from
// scratch) must never regress into flow style on a later write, not just
// on the very first one.
func TestSaveStaysBlockStyleAcrossReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	c := NewConfig(p)
	if err := c.AddSkill("alpha", "sha256:aaa"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	c2, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.AddSkill("beta", "sha256:bbb"); err != nil {
		t.Fatal(err)
	}
	if err := c2.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(out), "{}") {
		t.Fatalf("fu.yaml must stay block style across reloads, got:\n%s", out)
	}
	want := "version: 1\nskills:\n  alpha:\n    digest: sha256:aaa\n    enabled: true\n  beta:\n    digest: sha256:bbb\n    enabled: true\n"
	if string(out) != want {
		t.Fatalf("unexpected fu.yaml shape:\ngot:\n%s\nwant:\n%s", out, want)
	}
}

// Round 2 finding 3: clearStyle used to reset Style to 0 on every node,
// scalars included, which silently strips quoting from unknown scalar
// fields -- rewriting `truthy: "yes"` as bare `truthy: yes`. yaml.v3
// itself round-trips fine either way (it keeps the resolved !!str/!!bool
// tag independently of Style), but any YAML 1.1 reader -- PyYAML, Ruby
// Psych, yaml.v2, most non-Go libraries -- parses the unquoted form as a
// boolean, silently changing what the field *means* to every tool but fu
// itself, which defeats the entire point of preserving unknown fields
// untouched (DESIGN §3). The same indiscriminate reset also turned a
// folded scalar (">") into a literal ("|"), since Style carries block
// style too, not just quoting.
//
// This pins the *behavior* -- scalar quoting and style survive a round
// trip unchanged -- rather than only the three strings from the finding's
// own reproduction ("yes"/"no"/"on"), per the finding's explicit
// instruction: the previous round's tests each pinned one reported input,
// so a quoted non-boolean-looking string or an anchored value fell
// outside coverage and this regression shipped with the whole suite
// green.
func TestSaveRoundTripsUnknownScalarStyleByteForByte(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := `version: 1
truthy: "yes"
negative: "no"
onny: "on"
offy: "off"
unicode_field: "héllo wörld 你好"
anchor_src: &shared_name "quoted-anchor"
anchor_use: *shared_name
folded: >
  line one
  line two
skills:
  alpha:
    digest: sha256:abc
    enabled: true
`
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	// A real mutation, so Save (and therefore clearStyle) runs over the
	// whole tree exactly like every real write command does -- this must
	// not be a vacuous pass that never actually calls Save.
	c.SetEnabled("alpha", false)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	// Byte-fidelity: compare whole lines, not substrings, so a match can
	// only come from the exact rendering, not from a lucky substring of
	// something else.
	lines := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		lines[l] = true
	}

	wantLines := []string{
		`truthy: "yes"`,
		`negative: "no"`,
		`onny: "on"`,
		`offy: "off"`,
		`unicode_field: "héllo wörld 你好"`,
		`anchor_src: &shared_name "quoted-anchor"`,
		`anchor_use: *shared_name`,
		`folded: >`,
	}
	for _, want := range wantLines {
		if !lines[want] {
			t.Fatalf("quoted/anchored/folded scalar style must survive a round trip unchanged; missing line %q in:\n%s", want, got)
		}
	}

	// The YAML-1.1-coercible forms must never appear unquoted: that is
	// exactly the type-changing rewrite this finding is about (PyYAML,
	// yaml.v2, and most non-Go readers parse a bare yes/no/on/off as a
	// boolean). A folded scalar demoted to literal style is the other
	// named symptom of the same root cause.
	unwantLines := []string{
		`truthy: yes`,
		`negative: no`,
		`onny: on`,
		`offy: off`,
		`folded: |`,
	}
	for _, unwant := range unwantLines {
		if lines[unwant] {
			t.Fatalf("scalar must not be silently rewritten to a form YAML 1.1 (or block style) reads differently; found line %q in:\n%s", unwant, got)
		}
	}

	// The actual mutation must still have happened -- this guards against
	// a vacuous pass where Save silently no-ops.
	if !lines["    enabled: false"] {
		t.Fatalf("the actual mutation must still be saved, got:\n%s", got)
	}
}

func TestConfigSaveWritesThroughPinnedParentAfterPathReplacement(t *testing.T) {
	parent := t.TempDir()
	configDir := filepath.Join(parent, "config")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "fu.yaml")
	initial := []byte("version: 1\nskills:\n  alpha:\n    enabled: true\n")
	if err := os.WriteFile(configPath, initial, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetEnabled("alpha", false)
	movedDir := filepath.Join(parent, "config-moved")
	replacement := []byte("replacement parent\n")
	err = cfg.saveWithHooks(configSaveHooks{afterOpenRoot: func() error {
		if err := os.Rename(configDir, movedDir); err != nil {
			return err
		}
		if err := os.Mkdir(configDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(configPath, replacement, 0o644)
	}})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := os.ReadFile(filepath.Join(movedDir, "fu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(moved), "enabled: false") {
		t.Fatalf("pinned original config was not updated: %q", moved)
	}
	gotReplacement, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotReplacement, replacement) {
		t.Fatalf("replacement parent was modified: %q", gotReplacement)
	}
}

// TestLoadConfigValidatesStructure verifies LoadConfig rejects a
// parsed-but-malformed fu.yaml -- one that does not have the shape
// every mutator assumes -- instead of silently accepting it and later
// losing writes (mapSet appends to whatever node it is handed; a write
// aimed at a scalar node is dropped by yaml.Marshal with no error).
// fu.yaml lives inside a repository the user may hand-edit, so this is
// a realistic input, not a theoretical one.
func TestLoadConfigValidatesStructure(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
		// wantInvalidName (round 4 finding 2): when set, the load must
		// succeed (wantErr must be false) with exactly this name excluded
		// from the skill set and reported by InvalidNames, rather than
		// failing the load outright.
		wantInvalidName string
		// wantIsErr (round 6): the sentinel the refusal must wrap. Defaults
		// to ErrMalformedConfig, the structural violations this table was
		// originally about; a missing or blank file wraps ErrConfigMissing
		// instead, and the two must not be conflated -- one means "this file
		// is not shaped like a config", the other "this file is not there".
		wantIsErr error
	}{
		{
			name:    "root is not a mapping",
			yaml:    "oops\n",
			wantErr: true,
		},
		{
			name:    "skills is not a mapping",
			yaml:    "version: 1\nskills: oops\n",
			wantErr: true,
		},
		{
			name:    "skill entry is not a mapping",
			yaml:    "version: 1\nskills:\n  alpha: oops\n",
			wantErr: true,
		},
		{
			name:    "enabled is not a boolean",
			yaml:    "version: 1\nskills:\n  alpha:\n    enabled: maybe\n",
			wantErr: true,
		},
		{
			name:    "overrides is not a mapping",
			yaml:    "version: 1\nskills:\n  alpha:\n    overrides: oops\n",
			wantErr: true,
		},
		{
			name:    "override value is not a boolean",
			yaml:    "version: 1\nskills:\n  alpha:\n    overrides:\n      codex: maybe\n",
			wantErr: true,
		},
		{
			// fmt.Sscanf(v.Value, "%d", &c.version) used to discard its
			// error, leaving c.version at its zero-cost default
			// (SupportedVersion) for a non-integer version like "v2". Save
			// would then proceed to overwrite a config an older fu cannot
			// necessarily read, defeating the whole point of the guard.
			name:    "version is not an integer",
			yaml:    "version: v2\nskills: {}\n",
			wantErr: true,
		},
		{
			// Round 6 Critical, reversing this case's own prior assertion
			// (it required wantErr: false -- "a blank file loads like a
			// missing one"). That was the defect stated as a requirement:
			// nothing calls LoadConfig until Store.Open has proven the store
			// is initialized, so a blank fu.yaml there is destroyed state,
			// and inventing an empty config for it let the next write commit
			// the reconstruction over the user's registrations. Stated
			// explicitly rather than quietly edited, since the old assertion
			// is what let the behaviour ship. See missingConfigErr.
			name:      "blank file is refused, not treated as a fresh config",
			yaml:      "",
			wantErr:   true,
			wantIsErr: ErrConfigMissing,
		},
		{
			name:      "whitespace-only file is refused the same way",
			yaml:      "\n   \n \n",
			wantErr:   true,
			wantIsErr: ErrConfigMissing,
		},
		{
			// Round 3 finding 2 (bdf2882) originally pinned this case as
			// wantErr: true: a skill name becomes a path component
			// wherever a skill is looked up on disk (fu show's SKILL.md
			// read, the engine's own link materialization), so an invalid
			// one -- here, a path-traversal key a hand-edited fu.yaml
			// could carry, or a future clone/pull could bring in from the
			// network -- was rejected by failing the *whole load*.
			//
			// Round 4 finding 2 deliberately reverses that expectation:
			// rejecting the entire file over one bad name also took down
			// every other, validly-named skill recorded alongside it --
			// `fu list`, `fu show <anything>`, every write command, all
			// failed, not just a lookup of the offending name itself. The
			// escape this case exists to guard must still be closed (`fu
			// show '../../evilskill'` must still refuse -- see
			// TestLoadConfigIsolatesInvalidNameFromRestOfConfig and
			// cli/show_test.go's TestShowRejectsPathTraversalNamePlantedViaFuYaml,
			// now via HasSkill simply reporting the name absent), but the
			// load itself must now succeed, with the name isolated into
			// InvalidNames instead.
			name:            "skill name is a path-traversal escape",
			yaml:            "version: 1\nskills:\n  ../../evilskill:\n    digest: sha256:x\n    enabled: true\n",
			wantInvalidName: "../../evilskill",
		},
		{
			// The reverse direction, and a more mundane trigger than a
			// deliberate escape: a name that merely fails the Agent
			// Skills naming rules (here, uppercase), the exact shape an
			// older fu build or a hand-edit could leave behind. Same
			// round 4 finding 2 reversal as the case above: isolated, not
			// whole-file fatal.
			name:            "skill name fails naming-rule validation",
			yaml:            "version: 1\nskills:\n  Beta:\n    digest: sha256:x\n    enabled: true\n",
			wantInvalidName: "Beta",
		},
		{
			name: "well-formed file with unknown fields loads cleanly",
			yaml: `version: 1
future_top_level: keep-me
skills:
  alpha:
    digest: sha256:abc
    enabled: true
    future_field: keep-me-too
`,
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "fu.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := LoadConfig(p)
			if tc.wantErr {
				want := tc.wantIsErr
				if want == nil {
					want = ErrMalformedConfig
				}
				if !errors.Is(err, want) {
					t.Fatalf("want %v, got %v", want, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantInvalidName != "" {
				inv := c.InvalidNames()
				if len(inv) != 1 || inv[0].Name != tc.wantInvalidName || inv[0].Reason == "" {
					t.Fatalf("want %q recorded as the sole invalid name (with a reason), got %+v", tc.wantInvalidName, inv)
				}
				if c.HasSkill(tc.wantInvalidName) {
					t.Fatalf("an invalid name must never be reachable via HasSkill, got true for %q", tc.wantInvalidName)
				}
			}
		})
	}
}

// TestLoadConfigIsolatesInvalidNameFromRestOfConfig guards round 4 finding
// 2's adjacent coordinate. Every "invalid name" case in
// TestLoadConfigValidatesStructure above plants the bad name *alone*, so
// wantErr: true there is satisfied identically whether LoadConfig rejects
// only that one entry or the entire file -- it never asks what happens to
// a *different*, validly-named skill recorded alongside one that fails
// validation. bdf2882 (round 3 finding 2) made an invalid name abort the
// whole load, which -- verified against the compiled binary -- also took
// down `fu list`, `fu show <anything>`, `fu enable <anything>`, and `fu
// new <anything>`, not merely a lookup of the bad name itself: an
// unrelated skill like "alpha" became completely unreachable through any
// command until the user hand-edited fu.yaml, guided by nothing in `fu
// list`/`fu show`'s own output (neither names the file).
//
// round 4 finding 2 downgrades whole-file rejection to per-entry
// isolation: the offending entry is excluded from the config's skill set
// and reported through InvalidNames, but the rest of fu.yaml loads and
// stays fully usable.
func TestLoadConfigIsolatesInvalidNameFromRestOfConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := `version: 1
skills:
  Beta:
    digest: sha256:bad
    enabled: true
  alpha:
    digest: sha256:x
    enabled: true
`
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("one invalid name must not fail the whole load, got %v", err)
	}
	if c.HasSkill("Beta") {
		t.Fatal("an invalid name must not be reachable through HasSkill")
	}
	if !c.HasSkill("alpha") {
		t.Fatal("a valid skill recorded alongside an invalid name must still be registered")
	}
	if got := c.SkillNames(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("SkillNames must exclude the invalid name and keep the valid one, got %v", got)
	}
	if got := c.Digest("alpha"); got != "sha256:x" {
		t.Fatalf("the valid skill's own fields must be unaffected, got %q", got)
	}
	inv := c.InvalidNames()
	if len(inv) != 1 || inv[0].Name != "Beta" || inv[0].Reason == "" {
		t.Fatalf("InvalidNames must report the dropped entry and why, got %+v", inv)
	}

	// A config with no invalid names at all must behave exactly as before:
	// no false positives from this same mechanism.
	if len(c.InvalidNames()) != 1 {
		t.Fatalf("setup check failed: %+v", inv)
	}
	p2 := filepath.Join(t.TempDir(), "fu.yaml")
	os.WriteFile(p2, []byte("version: 1\nskills:\n  alpha:\n    digest: sha256:x\n    enabled: true\n"), 0o644)
	c2, err := LoadConfig(p2)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.InvalidNames(); len(got) != 0 {
		t.Fatalf("a config with no invalid names must report none, got %+v", got)
	}
}

func TestInvalidNameIsolationAppliesToEverySkillAccessor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := `version: 1
skills:
  ../evil:
    digest: sha256:bad
    enabled: true
    source:
      type: local
      path: /tmp/evil
    overrides:
      codex: false
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest("../evil") != "" || c.Enabled("../evil") || c.Effective("../evil", "codex") {
		t.Fatal("read accessors must not expose an isolated invalid entry")
	}
	if fields := c.SourceFields("../evil"); len(fields) != 0 {
		t.Fatalf("SourceFields exposed isolated data: %v", fields)
	}
	if _, ok := c.Override("../evil", "codex"); ok {
		t.Fatal("Override exposed isolated data")
	}
	c.SetDigest("../evil", "sha256:changed")
	c.SetEnabled("../evil", false)
	c.SetSourceFields("../evil", map[string]string{"type": "git"})
	c.SetAgent("../evil", "codex", true)
	c.RemoveSkill("../evil")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"../evil", "sha256:bad", "/tmp/evil", "codex: false"} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("invalid entry mutation erased or changed %q:\n%s", want, after)
		}
	}
}

// TestBooleanCaseVariantsAgreeAcrossAccessors guards against Enabled and
// Override diverging on how they interpret a boolean scalar. Before the
// fix, Enabled treated anything but the literal "false" as true while
// Override treated anything but the literal "true" as false, so a
// hand-written case variant like "False"/"True" (both valid YAML
// booleans, just not the canonical lowercase fu itself always writes)
// was read inconsistently depending on which accessor was asked.
func TestBooleanCaseVariantsAgreeAcrossAccessors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := `version: 1
skills:
  alpha:
    digest: sha256:abc
    enabled: False
    overrides:
      codex: True
`
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Enabled("alpha") {
		t.Fatal(`enabled: False must be read as false regardless of case`)
	}
	v, ok := c.Override("alpha", "codex")
	if !ok || !v {
		t.Fatalf(`overrides.codex: True must be read as true regardless of case; got (%v, %v)`, v, ok)
	}
}

// TestSkillAccessors covers the plain read/write accessors that had no
// direct test: SkillNames, HasSkill, RemoveSkill, Digest, SetDigest, and
// AddSkill's duplicate-name error branch.
func TestSkillAccessors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	c := NewConfig(p)

	if got := c.SkillNames(); len(got) != 0 {
		t.Fatalf("want no skills initially, got %v", got)
	}
	if c.HasSkill("alpha") {
		t.Fatal("alpha should not exist yet")
	}
	if got := c.Digest("alpha"); got != "" {
		t.Fatalf("Digest on an unregistered skill must return \"\", got %q", got)
	}

	if err := c.AddSkill("alpha", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddSkill("beta", "sha256:y"); err != nil {
		t.Fatal(err)
	}
	if !c.HasSkill("alpha") {
		t.Fatal("alpha should exist after AddSkill")
	}
	if got := c.SkillNames(); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("want [alpha beta] in file order, got %v", got)
	}
	if got := c.Digest("alpha"); got != "sha256:x" {
		t.Fatalf("want sha256:x, got %q", got)
	}

	// A duplicate name must fail and must not touch the existing entry.
	err := c.AddSkill("alpha", "sha256:z")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want an \"already exists\" error, got %v", err)
	}
	if got := c.Digest("alpha"); got != "sha256:x" {
		t.Fatalf("failed AddSkill must not overwrite: want sha256:x, got %q", got)
	}

	c.SetDigest("alpha", "sha256:new")
	if got := c.Digest("alpha"); got != "sha256:new" {
		t.Fatalf("want sha256:new after SetDigest, got %q", got)
	}
	// SetDigest is documented as a no-op if the skill is not registered.
	c.SetDigest("ghost", "sha256:ghost")
	if c.HasSkill("ghost") {
		t.Fatal("SetDigest must not register an absent skill as a side effect")
	}

	c.RemoveSkill("alpha")
	if c.HasSkill("alpha") {
		t.Fatal("alpha should be gone after RemoveSkill")
	}
	if got := c.SkillNames(); !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("want [beta] remaining, got %v", got)
	}

	// RemoveSkill on a name that was never registered is a harmless no-op.
	c.RemoveSkill("does-not-exist")
	if got := c.SkillNames(); !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("RemoveSkill on an absent name must be a no-op, got %v", got)
	}
}

// TestVersionMustBeCanonicalInteger is round 6's forward-compatibility
// finding. parseVersion decoded through yaml.v3's int conversion, which
// silently truncates a float: `version: 1.5` became 1, so CheckWritable
// saw a supported version and let this build write a config whose schema it
// has no reason to understand. The old comment acknowledged the truncation
// and argued it "errs toward refusing to write" -- exactly backwards, since
// truncation lowers the version and so *permits* the write.
//
// The guard exists so an older fu never overwrites a newer schema; anything
// it cannot interpret exactly must be refused rather than approximated.
func TestVersionMustBeCanonicalInteger(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		wantErr       bool
	}{
		{"plain integer", "1", false},
		// Round 8, reversing this case's own prior assertion (it required
		// wantErr: false). This test was about *canonical integer syntax*
		// and said nothing about which integers name a schema, so it ended
		// up asserting that 0 -- a version fu has never defined -- loads
		// fine. Schema semantics live in
		// TestPersistedConfigMustDeclareASupportedVersion; this case just
		// stops contradicting them.
		{"zero is syntactically fine but is not a schema this build defines", "0", true},
		{"a future integer", "99", false},
		{"float truncating into range", "1.5", true},
		{"float truncating from far above", "99.9", true},
		{"quoted integer is a string, not an integer", `"1"`, true},
		{"negative", "-1", true},
		{"leading-plus form", "+1", true},
		{"non-numeric", "v2", true},
		{"overflowing an int64", "99999999999999999999999", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "fu.yaml")
			if err := os.WriteFile(p, []byte("version: "+tc.version+"\nskills: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(p)
			if tc.wantErr {
				if !errors.Is(err, ErrMalformedConfig) {
					t.Fatalf("version %s must be refused as malformed, got %v", tc.version, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("version %s must load, got %v", tc.version, err)
			}
		})
	}

	// The point of all of the above: a float must not truncate its way past
	// the write guard.
	t.Run("a float above the supported version cannot be written", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fu.yaml")
		if err := os.WriteFile(p, []byte("version: 1.5\nskills: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Fatal("a version this build cannot interpret exactly must never reach CheckWritable at all")
		}
	})
}

// TestDuplicateKeysAreRejected is round 7's forward-compatibility finding.
// mapGet returns the *first* matching key, so a document carrying
// `version: 1` followed by `version: 99` was read as version 1: the write
// guard saw a supported schema, the file was mutated and saved with both
// entries intact, and what the file means became a question of which reader
// you ask. YAML itself calls duplicate mapping keys an error; yaml.v3's
// node API simply does not enforce it, so the check has to live here.
//
// The same ambiguity applies below the top level -- two `skills` mappings,
// one skill name twice, one agent twice under `overrides` -- and for the
// same reason: every accessor in this file resolves a key by scanning for
// the first match.
func TestDuplicateKeysAreRejected(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"version twice", "version: 1\nversion: 99\nskills: {}\n"},
		{"version twice, newer first", "version: 99\nversion: 1\nskills: {}\n"},
		{"skills twice", "version: 1\nskills:\n  alpha: {enabled: true}\nskills:\n  beta: {enabled: true}\n"},
		{"one skill name twice", "version: 1\nskills:\n  alpha: {enabled: true}\n  alpha: {enabled: false}\n"},
		{"one override agent twice", "version: 1\nskills:\n  alpha:\n    enabled: true\n    overrides:\n      codex: true\n      codex: false\n"},
		{"an unknown top-level key twice", "version: 1\nskills: {}\nfuture: a\nfuture: b\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "fu.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(p); !errors.Is(err, ErrMalformedConfig) {
				t.Fatalf("a duplicate key makes the file's meaning reader-dependent and must be "+
					"refused, got %v", err)
			}
		})
	}

	// A non-string key is the same class of problem: it cannot be addressed
	// by any accessor here, and its meaning depends on the reader.
	t.Run("a non-string key", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fu.yaml")
		if err := os.WriteFile(p, []byte("version: 1\nskills: {}\n1: oops\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); !errors.Is(err, ErrMalformedConfig) {
			t.Fatalf("a non-string mapping key must be refused, got %v", err)
		}
	})

	// Distinct keys at every level must of course keep loading.
	t.Run("distinct keys still load", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fu.yaml")
		raw := "version: 1\nskills:\n  alpha:\n    enabled: true\n    overrides:\n      codex: false\n      claude: true\n  beta: {enabled: true}\n"
		if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err != nil {
			t.Fatalf("distinct keys are ordinary: %v", err)
		}
	})
}

// TestPersistedConfigMustDeclareASupportedVersion is round 8's schema
// finding. LoadConfig seeded c.version with SupportedVersion and only
// overwrote it when a `version` key was present, so a persisted config
// without one was silently read, mutated and written back under version-1
// assumptions. parseVersion also accepted 0, and CheckWritable rejected
// only versions *above* the supported one, so version 0 -- a schema this
// build has never defined -- was writable too.
//
// The guard exists so fu never writes a file whose schema it cannot claim
// to understand. That has to cut both ways: a version it is too old for,
// and a version that was never a version.
func TestPersistedConfigMustDeclareASupportedVersion(t *testing.T) {
	t.Run("a persisted config without a version is refused", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fu.yaml")
		if err := os.WriteFile(p, []byte("skills:\n  alpha: {enabled: true}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); !errors.Is(err, ErrMalformedConfig) {
			t.Fatalf("a config that declares no schema must not be assumed to be version %d, got %v",
				SupportedVersion, err)
		}
	})

	t.Run("a version below the minimum is refused", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fu.yaml")
		if err := os.WriteFile(p, []byte("version: 0\nskills: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); !errors.Is(err, ErrMalformedConfig) {
			t.Fatalf("version 0 is not a schema this build ever defined, got %v", err)
		}
	})

	t.Run("a fresh config declares the supported version", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fu.yaml")
		c := NewConfig(p)
		if err := c.CheckWritable(); err != nil {
			t.Fatalf("a config fu just created must be writable: %v", err)
		}
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "version: 1") {
			t.Fatalf("a config fu writes must declare its schema:\n%s", raw)
		}
		// And it must load back, which is the round trip that matters.
		if _, err := LoadConfig(p); err != nil {
			t.Fatalf("what fu writes must be what fu accepts: %v", err)
		}
	})

	t.Run("the supported version still loads and is writable", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fu.yaml")
		if err := os.WriteFile(p, []byte("version: 1\nskills: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.CheckWritable(); err != nil {
			t.Fatalf("version %d is this build's own schema: %v", SupportedVersion, err)
		}
	})
}

func checkedWriteSession(t *testing.T) *Store {
	t.Helper()
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session.Store
}

func configSwapArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if entry.Name() == configSwapName {
			found = append(found, entry.Name())
		}
	}
	return found
}

func configArchiveArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), configArchivePrefix) {
			found = append(found, entry.Name())
		}
	}
	return found
}

// A crash between the exchange and the cleanup leaves the displaced config
// parked under a private name. Beside fu.yaml that file is ordinary untracked
// store content, so the next command's sweep commits it into history; nothing
// can identify it afterwards, and it is attributed to whatever operation ran
// next.
func TestConfigExchangeKeepsItsArtifactOutOfTheVersionedStore(t *testing.T) {
	checked := checkedWriteSession(t)
	before, err := ReadConfigFileRoot(mustStoreRoot(t, checked), "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var inStore, inStaging []string
	err = checked.installConfigExpecting(before, append(before, []byte("\n# installed\n")...), configExchangeHooks{
		afterExchange: func() {
			inStore = configSwapArtifacts(t, checked.Dir())
			inStaging = configSwapArtifacts(t, checked.StagingDir())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inStore) != 0 {
		t.Errorf("swap artifacts must never exist inside the versioned store, found %v", inStore)
	}
	if len(inStaging) != 1 {
		t.Errorf("the displaced config must be parked in staging, found %v", inStaging)
	}
}

// A real process exit after the atomic exchange leaves no defer or caller
// rollback to retire the displaced config. The next checked write must use the
// durable exchange proof to finish that retirement before starting a new
// install; requiring a human to identify and remove the scratch file would
// violate the command-level recovery guarantee in DESIGN section 2.
func TestConfigExchangeRecoversAfterProcessExit(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_CONFIG_EXCHANGE_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_CONFIG_EXCHANGE_HOME")
		s, err := Open(home)
		if err != nil {
			panic(err)
		}
		session, err := s.BeginWrite()
		if err != nil {
			panic(err)
		}
		defer session.Close()
		before, err := os.ReadFile(session.Store.ConfigPath())
		if err != nil {
			panic(err)
		}
		installed := append(append([]byte(nil), before...), []byte("\n# installed before crash\n")...)
		_ = session.Store.installConfigExpecting(before, installed, configExchangeHooks{
			afterExchange: func() { os.Exit(86) },
		})
		panic("config exchange crash hook did not run")
	}

	home := filepath.Join(t.TempDir(), "home")
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	installed := append(append([]byte(nil), before...), []byte("\n# installed before crash\n")...)
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigExchangeRecoversAfterProcessExit$")
	cmd.Env = append(os.Environ(),
		"FU_TEST_CRASH_CONFIG_EXCHANGE_HELPER=1",
		"FU_TEST_CRASH_CONFIG_EXCHANGE_HOME="+home,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must exit immediately after config exchange: err=%v output=%s", err, output)
	}

	s, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Store.RecoverConfigExchanges(); err != nil {
		t.Fatalf("the unified write recovery step must finish the interrupted config exchange: %v", err)
	}
	if artifacts := configSwapArtifacts(t, session.Store.StagingDir()); len(artifacts) != 0 {
		t.Fatalf("unified recovery left an active config exchange artifact: %v", artifacts)
	}
	current, err := os.ReadFile(session.Store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, installed) {
		t.Fatalf("the atomic exchange did not leave the installed config canonical: got %q want %q", current, installed)
	}
	next := append(append([]byte(nil), installed...), []byte("# next install\n")...)
	if err := session.Store.InstallConfigExpecting(installed, next); err != nil {
		t.Fatalf("the next install must recover the interrupted exchange without manual cleanup: %v", err)
	}
	if got, err := os.ReadFile(session.Store.ConfigPath()); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, next) {
		t.Fatalf("recovered install wrote %q, want %q", got, next)
	}
	if artifacts := configSwapArtifacts(t, session.Store.StagingDir()); len(artifacts) != 0 {
		t.Fatalf("recovery left an active config exchange artifact: %v", artifacts)
	}
}

// The conflict path must not delete an unverified replacement. After the
// exchange detects displaced bytes it did not expect, a third writer installing
// its own config at fu.yaml used to be carried to the private name by the
// restoring exchange and unlinked there, unexamined.
func TestConfigExchangePreservesAThirdVersionInstalledDuringTheSwap(t *testing.T) {
	checked := checkedWriteSession(t)
	storeRoot := mustStoreRoot(t, checked)
	before, err := ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The install is told to expect bytes the file never held, so it takes the
	// conflict path; the hook then plays the third writer.
	third := append(append([]byte(nil), before...), []byte("\n# third writer\n")...)
	err = checked.installConfigExpecting([]byte("bytes fu.yaml never held\n"), append(before, []byte("\n# fu\n")...),
		configExchangeHooks{
			afterExchange: func() {
				if err := os.WriteFile(filepath.Join(checked.Dir(), "fu.yaml"), third, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		})
	if !errors.Is(err, ErrConfigChangedExternally) {
		t.Fatalf("a displaced config that is not the expected one must conflict, got %v", err)
	}
	got, err := os.ReadFile(filepath.Join(checked.Dir(), "fu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, third) {
		t.Fatalf("a third writer's config was destroyed:\ngot  %q\nwant %q", got, third)
	}
	if artifacts := configSwapArtifacts(t, checked.StagingDir()); len(artifacts) == 0 {
		t.Error("the displaced version must be preserved rather than unlinked")
	}
}

func mustStoreRoot(t *testing.T, s *Store) *os.Root {
	t.Helper()
	root, err := s.StoreRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func soleConfigSwapArtifact(t *testing.T, dir string) string {
	t.Helper()
	found := configSwapArtifacts(t, dir)
	if len(found) != 1 {
		t.Fatalf("expected exactly one swap artifact in %s, found %v", dir, found)
	}
	return filepath.Join(dir, found[0])
}

// The parked object is the proof that fu.yaml held the expected bytes at the
// instant of the swap. Comparing only its bytes cannot establish that: a
// separately created file carrying the same bytes satisfies the comparison
// while proving nothing, and it was then unlinked by pathname as if it were
// fu's own displaced config.
//
// The identity mismatch now means the precondition is unproven, so fu withdraws
// instead of completing: the parked object goes back to fu.yaml and fu's own
// bytes are retired. Either way it survives -- what it must never be is deleted.
func TestConfigExchangePreservesAReplacedScratchEntry(t *testing.T) {
	checked := checkedWriteSession(t)
	storeRoot := mustStoreRoot(t, checked)
	before, err := ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	installed := append(append([]byte(nil), before...), []byte("\n# fu\n")...)

	err = checked.installConfigExpecting(before, installed, configExchangeHooks{
		afterExchange: func() {
			parked := filepath.Join(checked.StagingDir(), configSwapName)
			// Same bytes, different object.
			replacement := parked + "-replacement"
			if err := os.WriteFile(replacement, before, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, parked); err != nil {
				t.Fatal(err)
			}
		},
	})
	if !errors.Is(err, ErrConfigChangedExternally) {
		t.Fatalf("a scratch entry that is not the displaced object must conflict, got %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(checked.Dir(), "fu.yaml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, before) {
		t.Fatalf("a withdrawn install must leave the pre-swap bytes canonical:\ngot  %q\nwant %q", got, before)
	}
}

// The scratch entry is also unproven on the way in. Between writing fu's bytes
// into it and exchanging it into place, a replacement at that name would be
// installed as fu.yaml -- fu publishing content it never generated.
func TestConfigExchangeRefusesAScratchReplacedBeforeTheSwap(t *testing.T) {
	checked := checkedWriteSession(t)
	storeRoot := mustStoreRoot(t, checked)
	before, err := ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("version: 1\nskills: {}\n# content fu never generated\n")

	var planted string
	err = checked.installConfigExpecting(before, append(before, []byte("\n# fu\n")...), configExchangeHooks{
		beforeExchange: func() {
			planted = soleConfigSwapArtifact(t, checked.StagingDir())
			replacement := planted + "-replacement"
			if err := os.WriteFile(replacement, foreign, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, planted); err != nil {
				t.Fatal(err)
			}
		},
	})
	if err == nil {
		t.Fatal("a scratch entry replaced before the swap must not be installed")
	}
	got, readErr := os.ReadFile(filepath.Join(checked.Dir(), "fu.yaml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, before) {
		t.Fatalf("fu.yaml was replaced with an object fu did not write:\ngot  %q\nwant %q", got, before)
	}
	if planted != "" {
		if kept, err := os.ReadFile(planted); err != nil {
			t.Fatalf("the planted scratch entry must survive: %v", err)
		} else if !bytes.Equal(kept, foreign) {
			t.Fatalf("planted scratch entry changed: got %q want %q", kept, foreign)
		}
	}
}

// The exchange's whole purpose is to report what fu.yaml held at the instant of
// the swap. When that turns out not to be the expected bytes, the pre-swap
// state has to come back: an external writer who replaced fu.yaml between the
// identity sample and the swap owns the canonical file, and DESIGN §6 promises
// their version stays there for the next external commit. Leaving fu's own
// bytes canonical and parking theirs in staging inverts that.
func TestConfigExchangeRestoresAnExternalConfigInstalledBeforeTheSwap(t *testing.T) {
	checked := checkedWriteSession(t)
	storeRoot := mustStoreRoot(t, checked)
	before, err := ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	external := append(append([]byte(nil), before...), []byte("\n# external, newest\n")...)

	err = checked.installConfigExpecting(before, append(before, []byte("\n# fu\n")...), configExchangeHooks{
		beforeExchange: func() {
			tmp := filepath.Join(checked.Dir(), "external.tmp")
			if werr := os.WriteFile(tmp, external, 0o644); werr != nil {
				t.Fatal(werr)
			}
			if rerr := os.Rename(tmp, filepath.Join(checked.Dir(), "fu.yaml")); rerr != nil {
				t.Fatal(rerr)
			}
		},
	})
	if !errors.Is(err, ErrConfigChangedExternally) {
		t.Fatalf("a target replaced before the swap must conflict, got %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(checked.Dir(), "fu.yaml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, external) {
		t.Fatalf("the external config must stay canonical:\ngot  %q\nwant %q", got, external)
	}
}

// A completed exchange moves the displaced inode to a no-replace terminal
// archive. It must not clear that inode: external hard links and open file
// descriptors can still refer to the same object after its pathname moves.
func TestConfigExchangeArchivesItsDisplacedConfigIntact(t *testing.T) {
	checked := checkedWriteSession(t)
	storeRoot := mustStoreRoot(t, checked)
	before, err := ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := checked.InstallConfigExpecting(before, append(before, []byte("\n# fu\n")...)); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(checked.StagingDir(), configSwapName)
	if _, err := os.Lstat(scratch); !os.IsNotExist(err) {
		t.Fatalf("a completed exchange must move the active scratch name to recovery: %v", err)
	}
	archives := configArchiveArtifacts(t, checked.RecoveryDir())
	if len(archives) != 1 {
		t.Fatalf("a completed exchange must retain one config archive, got %v", archives)
	}
	archived, err := os.ReadFile(filepath.Join(checked.RecoveryDir(), archives[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archived, before) {
		t.Fatalf("archived displaced config changed: got %q want %q", archived, before)
	}
}

// A non-empty scratch entry is the durable trace of an exchange that did not
// finish. It has to be surfaced rather than silently reused or removed: its
// bytes are a config version nothing else records.
func TestConfigExchangeReportsAnInterruptedSnapshot(t *testing.T) {
	checked := checkedWriteSession(t)
	storeRoot := mustStoreRoot(t, checked)
	before, err := ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(checked.StagingDir(), configSwapName)
	stranded := []byte("version: 1\nskills: {}\n# stranded by an interrupted exchange\n")
	if err := os.WriteFile(scratch, stranded, 0o644); err != nil {
		t.Fatal(err)
	}

	err = checked.InstallConfigExpecting(before, append(before, []byte("\n# fu\n")...))
	if err == nil {
		t.Fatal("an unfinished exchange snapshot must stop the next install")
	}
	if !strings.Contains(err.Error(), configSwapName) {
		t.Fatalf("the error must name the snapshot so it can be found, got %v", err)
	}
	got, readErr := os.ReadFile(scratch)
	if readErr != nil {
		t.Fatalf("the stranded snapshot must survive: %v", readErr)
	}
	if !bytes.Equal(got, stranded) {
		t.Fatalf("stranded snapshot changed: got %q want %q", got, stranded)
	}
	current, readErr := os.ReadFile(filepath.Join(checked.Dir(), "fu.yaml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(current, before) {
		t.Fatalf("a refused install must not touch fu.yaml: got %q want %q", current, before)
	}
}

// Retiring the displaced config must not mutate its inode. A path outside the
// store may be a hard link to the old fu.yaml; it is not owned by fu and must
// retain the exact bytes it had before a successful install.
func TestConfigExchangePreservesHardLinkToPreviousConfig(t *testing.T) {
	checked := checkedWriteSession(t)
	before, err := os.ReadFile(checked.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "fu-yaml-backup")
	if err := os.Link(checked.ConfigPath(), backup); err != nil {
		t.Fatal(err)
	}

	installed := append(append([]byte(nil), before...), []byte("\n# installed\n")...)
	if err := checked.InstallConfigExpecting(before, installed); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, before) {
		t.Fatalf("retiring the displaced config changed an external hard link: got %q want %q", got, before)
	}
}

// An existing scratch pathname is not evidence that its inode belongs to fu,
// even when it is an empty regular file. Opening and filling it would mutate
// every external hard link to that inode before the exchange even begins.
func TestConfigExchangeRefusesPreexistingScratchHardLink(t *testing.T) {
	checked := checkedWriteSession(t)
	before, err := os.ReadFile(checked.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-empty-file")
	if err := os.WriteFile(external, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(checked.StagingDir(), configSwapName)
	if err := os.Link(external, scratch); err != nil {
		t.Fatal(err)
	}

	installed := append(append([]byte(nil), before...), []byte("\n# installed\n")...)
	if err := checked.InstallConfigExpecting(before, installed); err == nil {
		t.Fatal("an existing scratch inode must not be adopted and modified")
	}
	got, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("refused scratch hard link changed external bytes to %q", got)
	}
	current, err := os.ReadFile(checked.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, before) {
		t.Fatalf("refused scratch hard link changed fu.yaml: got %q want %q", current, before)
	}
}

// The scratch name is visible after fu creates and fills it, so another process
// can hard-link that inode before the exchange. If the install is later
// withdrawn, fu still must not truncate the shared inode while retiring its own
// generated config.
func TestConfigExchangePreservesHardLinkToWithdrawnConfig(t *testing.T) {
	checked := checkedWriteSession(t)
	before, err := os.ReadFile(checked.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	installed := append(append([]byte(nil), before...), []byte("\n# installed\n")...)
	linked := filepath.Join(t.TempDir(), "linked-staged-config")

	err = checked.installConfigExpecting([]byte("different expected bytes\n"), installed, configExchangeHooks{
		beforeExchange: func() {
			if linkErr := os.Link(filepath.Join(checked.StagingDir(), configSwapName), linked); linkErr != nil {
				t.Fatal(linkErr)
			}
		},
	})
	if !errors.Is(err, ErrConfigChangedExternally) {
		t.Fatalf("a mismatching displaced config must withdraw the install, got %v", err)
	}
	got, err := os.ReadFile(linked)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installed) {
		t.Fatalf("retiring the withdrawn config changed an external hard link: got %q want %q", got, installed)
	}
	current, err := os.ReadFile(checked.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, before) {
		t.Fatalf("withdrawn install left the wrong canonical config: got %q want %q", current, before)
	}
}
