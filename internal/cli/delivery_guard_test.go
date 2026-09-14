// internal/cli/delivery_guard_test.go
package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryCommandThatReportsAReconcileAlsoReportsItsEffect pins SPEC rule 8's
// scope mechanically instead of by anyone's memory of which commands
// reconcile.
//
// The rule is one sentence: a command that changes what agents will load must
// say when the change takes effect. Which commands those are is not obvious
// from the command's own purpose -- `fu commit` is about history and `fu push`
// is about a remote, and both reconcile, so both can project a link an agent
// was owed and say nothing about it. Both actually did: commit removed a link
// while recording a hand edit to fu.yaml, and push projected into an agent
// detected since the last write command, each on a run whose entire output was
// about something else.
//
// printResult(cmd, X) is the mechanical marker, because a file calls it
// exactly when it has a reconcile Result to report to the user. Any such file
// must also reach for the hint -- printDeliveryHint for a run-level line, or
// hintSuffix for a confirmation that names one skill. The check is on the file
// rather than the expression, since add alone prints four different prologue
// results on its early exits and one final one.
func TestEveryCommandThatReportsAReconcileAlsoReportsItsEffect(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		calls := callNamesIn(t, name)
		if !calls["printResult"] {
			continue
		}
		checked++
		if !calls["printDeliveryHint"] && !calls["hintSuffix"] {
			t.Errorf("%s reports a reconcile result but never reports when it takes effect; "+
				"add printDeliveryHint (run-level line) or hintSuffix (confirmation naming one skill)", name)
		}
	}
	// A guard that silently matches nothing is worse than none: the green tick
	// would then be evidence for a property no longer being tested.
	if checked == 0 {
		t.Fatal("no command file calls printResult; the check would pass vacuously")
	}
}

func callNamesIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Clean(path), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok {
			names[ident.Name] = true
		}
		return true
	})
	return names
}
