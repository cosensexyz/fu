// internal/cli/agent_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/skill"
)

// The ordinary case: one agent installed with a skill delivered, one agent fu
// supports that this machine does not have.
func TestAgentCommandListsDetectedAndUndetectedAgents(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	out, err := runCmd(t, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "AGENT") || !strings.Contains(out, "DELIVERED") {
		t.Fatalf("header missing:\n%s", out)
	}
	claude := rowFor(t, out, "claude")
	if !strings.Contains(claude, "ok") || !strings.Contains(claude, filepath.Join(home, ".claude", "skills")) {
		t.Fatalf("claude row = %q, want a healthy row naming its skills dir", claude)
	}
	codex := rowFor(t, out, "codex")
	if !strings.Contains(codex, "not detected") {
		t.Fatalf("codex row = %q, want it listed as not detected", codex)
	}
}

// SPEC rule 4: an agent detected after its skills were registered has nothing
// projected yet, and a read-only command says so rather than creating the
// directory.
func TestAgentCommandReportsPendingWithoutCreatingTheDirectory(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	// codex appears only now, so its skills directory has never been created.
	mustMkdirAll(t, filepath.Join(home, ".codex"))

	out, err := runCmd(t, "agent")
	if err != nil {
		t.Fatal(err)
	}
	codex := rowFor(t, out, "codex")
	if !strings.Contains(codex, "dir missing") {
		t.Fatalf("codex row = %q, want the missing directory reported", codex)
	}
	if !strings.Contains(out, "nothing projected yet") {
		t.Fatalf("want the rule-4 note explaining what creates it:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".codex", "skills")); !os.IsNotExist(err) {
		t.Fatalf("fu agent must not create the directory: %v", err)
	}
}

// A skills directory that is itself a symlink is refused by reconcile
// (SPEC rule 10); the row says so and points at the command that converts it.
func TestAgentCommandReportsASymlinkedSkillsDir(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	mustMkdirAll(t, filepath.Join(claude, "elsewhere"))
	if err := os.Symlink(filepath.Join(claude, "elsewhere"), filepath.Join(claude, "skills")); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")

	out, err := runCmd(t, "agent")
	if err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, out, "claude")
	if !strings.Contains(row, "dir is a symlink") {
		t.Fatalf("claude row = %q, want the symlink state", row)
	}
	if !strings.Contains(out, "fu adopt") {
		t.Fatalf("want the note naming the command that converts it:\n%s", out)
	}
}

// Each state a count alone cannot explain gets its own line under the table.
// Four of them are reachable in one fixture: a broken link, an entry fu
// cannot inspect, a desired link blocked by content fu did not create, and an
// enabled skill the store no longer holds.
func TestAgentCommandNotesTheStatesACountCannotExplain(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	skills := filepath.Join(home, ".claude", "skills")
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		runCmd(t, "new", name)
	}
	// Every write command reconciles, so the last of them runs before any of
	// the damage below: `fu new` in the middle would re-project what the
	// previous lines had just broken.
	runCmd(t, "disable", "delta")
	storeSkills := filepath.Join(fuHome, "store", "skills")
	// alpha: linked, store content deleted -- a broken link.
	if err := os.RemoveAll(filepath.Join(storeSkills, "alpha")); err != nil {
		t.Fatal(err)
	}
	// beta: link replaced by unmanaged content, so fu refuses the path.
	if err := os.Remove(filepath.Join(skills, "beta")); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(skills, "beta"))
	// gamma: never mind the link, the store no longer holds the content.
	if err := os.Remove(filepath.Join(skills, "gamma")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(storeSkills, "gamma")); err != nil {
		t.Fatal(err)
	}
	// delta: turned off above, and now something fu did not create holds its
	// name -- so it may still be loaded every session despite the switch.
	mustMkdirAll(t, filepath.Join(skills, "delta"))
	// loop: an entry the scan cannot classify at all.
	if err := os.Symlink("loop", filepath.Join(storeSkills, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(storeSkills, "loop"), filepath.Join(skills, "loop")); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"claude: 1 broken link",
		"claude: 1 entry cannot be inspected",
		"claude: 2 names blocked by unmanaged content",
		"claude: 1 enabled skill the store no longer holds",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in:\n%s", want, out)
		}
	}
}

// fu agent writes nothing at all -- not the store, not an agent directory.
func TestAgentCommandIsReadOnly(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	take := func() [3]string {
		t.Helper()
		var digests [3]string
		for i, dir := range []string{filepath.Join(fuHome, "store"), filepath.Join(home, ".claude"), filepath.Join(home, ".codex")} {
			digest, err := skill.Digest(dir)
			if err != nil {
				t.Fatal(err)
			}
			digests[i] = digest
		}
		return digests
	}

	before := take()
	if _, err := runCmd(t, "agent"); err != nil {
		t.Fatal(err)
	}
	if after := take(); after != before {
		t.Fatalf("fu agent changed the filesystem:\nbefore: %v\nafter:  %v", before, after)
	}
}

// An uninitialized store is an error for fu agent, as it is for fu status:
// there is no fu.yaml to compare a projection against.
func TestAgentCommandFailsWithoutAStore(t *testing.T) {
	t.Setenv("FU_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	if _, err := runCmd(t, "agent"); err == nil {
		t.Fatal("want an error when the store is not initialized")
	}
}

// rowFor returns the table row for one agent -- the line whose first column
// is that name. Matching on the first column rather than anywhere in the line
// keeps a note under the table, which starts with the same name, from being
// mistaken for the row it describes.
func rowFor(t *testing.T, out, agentName string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) != 0 && fields[0] == agentName {
			return line
		}
	}
	t.Fatalf("no row for %q in:\n%s", agentName, out)
	return ""
}

// A name that is both reserved by an agent and invalid as a skill name must
// reach the user on some stream. readDiagnostics suppresses its config-level
// `invalid:` line whenever an inspected agent would report the name as
// reserved instead -- and `fu agent` reports no per-agent findings at all, so
// passing it the inspected agents left the name mentioned nowhere.
//
// codex reserves ".system", which is also not a valid skill name: the one
// spelling that reaches both filters at once.
func TestAgentCommandReportsANameThatIsBothReservedAndInvalid(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")

	// Written whole: a fresh store records `skills: {}`, which nothing can be
	// appended under.
	config := filepath.Join(fuHome, "store", "fu.yaml")
	if err := os.WriteFile(config, []byte("version: 1\nskills:\n  .system:\n    enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ".system") {
		t.Fatalf("a name fu refuses to manage must be named somewhere:\n%s", out)
	}
}

// An agent whose skills directory cannot be scanned at all keeps its failure
// on its own row, says why under the table, and costs no other agent its row.
func TestAgentCommandReportsAnUnscannableAgent(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".codex"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	// A plain file where codex's skills directory belongs: not a directory,
	// so the scan cannot proceed for this agent and only this agent.
	if err := os.RemoveAll(filepath.Join(home, ".codex", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "skills"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if row := rowFor(t, out, "codex"); !strings.Contains(row, "cannot inspect") {
		t.Fatalf("codex row = %q, want the scan failure in its state", row)
	}
	if !strings.Contains(out, "codex: could not be inspected:") {
		t.Fatalf("want the reason under the table:\n%s", out)
	}
	if row := rowFor(t, out, "claude"); !strings.Contains(row, "ok") {
		t.Fatalf("claude row = %q, want an unaffected row", row)
	}
}
