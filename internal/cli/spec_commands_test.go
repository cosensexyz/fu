// internal/cli/spec_commands_test.go
package cli

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// specCommandName finds every command named in a SPEC table row. Rows are not
// one command each: `| `fu enable <name>` / `fu disable <name>` | ... |` names
// two, and `| `fu push` / `fu pull` |` likewise. A pattern that read only the
// first reported the others as undocumented -- a false alarm from the guard
// rather than drift in the document, which is the failure mode a narrow matcher
// produces when it is narrow in the wrong direction.
var specCommandName = regexp.MustCompile("`fu ([a-z-]+)")

// TestSpecNamesEveryCommandAndOnlyThose reconciles SPEC §5.1's command table
// with what the binary actually registers.
//
// Every batch of this project has ended with someone counting commands by hand
// and comparing the number against a document; that is exactly the check a
// machine should be doing, and it is the one the closeout batch is asked to
// make stick. Both directions matter. A command in SPEC that nothing registers
// is a promise the binary does not keep. A command the binary registers that
// SPEC does not list is a surface nobody agreed to support -- and it is the
// easier of the two to introduce, because adding a cobra command is one line
// and updating a table in another language is a separate thought.
//
// Aliases and hidden commands count as registered: a user can type them. cobra
// adds `help` and `completion` itself, so they are excluded by name rather than
// by any property this test could infer -- if cobra ever adds a third, this
// fails and someone decides deliberately.
//
// Like the README transcript guard beside it, this counts what it checked and
// fails at zero rather than passing vacuously when the table's shape changes.
func TestSpecNamesEveryCommandAndOnlyThose(t *testing.T) {
	// Both documents that carry a command table, because the criterion names
	// both and README's is the one a contributor is likelier to forget.
	tables := map[string]map[string]bool{
		"SPEC.md":   commandTableOf(t, "../../SPEC.md"),
		"README.md": commandTableOf(t, "../../README.md"),
	}

	cobraOwn := map[string]bool{"help": true, "completion": true}
	registered := map[string]bool{}
	for _, cmd := range NewRootCmd().Commands() {
		// Aliases count. A user can type one, so it is a surface, and an
		// undocumented alias is precisely the thing this guard exists to
		// refuse. Reading only Name() made the aliases sentence above a claim
		// about a mechanism that was not there -- the defect class this whole
		// batch was convened to clear, reproduced inside its own guard.
		for _, name := range append([]string{cmd.Name()}, cmd.Aliases...) {
			if cobraOwn[name] {
				continue
			}
			registered[name] = true
		}
	}
	if len(registered) == 0 {
		t.Fatal("no commands registered; the check would pass vacuously")
	}

	for doc, documented := range tables {
		for name := range documented {
			if !registered[name] {
				t.Errorf("%s lists `fu %s`, which the binary does not register", doc, name)
			}
		}
		for name := range registered {
			if !documented[name] {
				t.Errorf("the binary registers `fu %s`, which %s does not list", name, doc)
			}
		}
		if t.Failed() {
			t.Logf("%s documents: %v", doc, sortedKeys(documented))
		}
	}
	if t.Failed() {
		t.Logf("registered: %v", sortedKeys(registered))
	}
}

// commandTableOf reads the `fu <command>` names out of a document's command
// table. Fails at zero rather than reporting every registered command as
// undocumented, which is what a formatting change to the table would otherwise
// produce.
func commandTableOf(t *testing.T, path string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "| `fu ") {
			continue
		}
		for _, match := range specCommandName.FindAllStringSubmatch(line, -1) {
			names[match[1]] = true
		}
	}
	if len(names) == 0 {
		t.Fatalf("no command rows parsed from %s; the check would pass vacuously", path)
	}
	return names
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
