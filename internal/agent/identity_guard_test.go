package agent

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/identityguard"
)

// TestIdentityConstructionStaysBehindStorePrimitives extends the shared
// identity guard to this package (batch 4): no raw device or inode read, no
// FileIdentity construction and no os.SameFile call may appear here. The
// package has none today; the scan keeps it that way.
func TestIdentityConstructionStaysBehindStorePrimitives(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the agent package directory")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	inspected := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		inspected++
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range identityguard.Findings(file) {
			t.Errorf("%s:%d contains direct %s identity construction; capture identities through store.EntryIdentityAt or store.OpenIdentity", name, fset.Position(finding.Pos).Line, finding.Kind)
		}
	}
	if inspected == 0 {
		t.Fatal("no agent production files were inspected")
	}
}
