package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoTestInThisPackageRunsInParallel checks the constraint the hook
// declarations state (resolveRemoteRefHook in outdated.go, lockAcquiredHook and
// lockReleasedHook in lock.go, updateSourcePreparedHook in application.go): each
// is a package-level variable a test writes and production code reads, so they
// are safe only while one test at a time is running in this binary.
//
// The constraint was documented and nothing enforced it. The first t.Parallel()
// added here would make -race fail somewhere that does not look like the cause
// -- inside whichever test happened to be running, not at the hook it collided
// with. Checked rather than remembered, and stated as what to do rather than as
// a prohibition: a test that genuinely needs t.Parallel() is fine once the hooks
// it races with have a home that is not a global.
//
// This package and no other, which is narrower than the comment the constraint
// was first written into. All four hooks are unexported and declared here, so no
// other package can assign them, and `go test` gives each package its own
// process, so a parallel test in internal/source or internal/cli cannot reach
// these variables even in principle. Scanning those two would have enforced a
// rule that has no reason behind it there.
func TestNoTestInThisPackageRunsInParallel(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the engine test directory")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", entry.Name(), err)
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Any zero-argument .Parallel() call: the receiver is a *testing.T
			// or *testing.B whatever it happens to be named, and nothing else
			// in a test file offers that method.
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Parallel" || len(call.Args) != 0 {
				return true
			}
			t.Errorf("%s:%d calls Parallel(); this package's test hooks "+
				"(resolveRemoteRefHook, lockAcquiredHook, lockReleasedHook, updateSourcePreparedHook) "+
				"are written by tests and read by production code, so only one test may run at a time -- "+
				"move those hooks off the package scope before adding a parallel test here",
				entry.Name(), fset.Position(call.Pos()).Line)
			return true
		})
	}
}
