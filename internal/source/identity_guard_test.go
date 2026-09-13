package source

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/identityguard"
)

// TestSourceIdentityConstructionStaysBehindStorePrimitives keeps source
// identity construction on the shared FileIdentity capture path. Since batch
// 4 no source file reads a device or inode directly, so every raw read is a
// finding; os.SameFile is allowed only where both descriptors stay open
// across the comparison (the os.Root pairings), and reported elsewhere.
func TestSourceIdentityConstructionStaysBehindStorePrimitives(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the source package directory")
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
		for _, finding := range sourceIdentityFindings(file, name) {
			t.Errorf("%s:%d contains direct %s identity construction", name, fset.Position(finding.Pos).Line, finding.Kind)
		}
	}
	if inspected == 0 {
		t.Fatal("no source production files were inspected")
	}
}

func sourceIdentityFindings(file *ast.File, name string) []identityguard.Finding {
	// os.SameFile is legitimate only where both descriptors stay open across
	// the observations and the comparison, so an inode cannot be recycled in
	// between: the os.Root pairings in these two constructors.
	sameFileAllow := map[string]map[string]bool{
		"scratch.go": {"newOwnedScratchWithIdentityHooks": true},
		"git.go":     {"openPreparedRoot": true},
	}
	var findings []identityguard.Finding
	for _, finding := range identityguard.Findings(file) {
		if finding.Kind == identityguard.SameFileCall && identityguard.InsideAllowedFunction(file, finding.Pos, sameFileAllow[name]) {
			continue
		}
		findings = append(findings, finding)
	}
	return findings
}

// TestSourceSameFileAllowanceCoversOnlyThePinnedPairings proves the
// os.SameFile allowance is scoped to the two constructors whose descriptors
// stay open across the comparison, by function and by file: the same call in
// another function of scratch.go, or in the allowed function's name under
// another file, is reported.
func TestSourceSameFileAllowanceCoversOnlyThePinnedPairings(t *testing.T) {
	const source = `package p
type FileInfo interface{}
var os struct{ SameFile func(FileInfo, FileInfo) bool }
func newOwnedScratchWithIdentityHooks(a, b FileInfo) bool { return os.SameFile(a, b) }
func elsewhere(a, b FileInfo) bool { return os.SameFile(a, b) }`
	for _, tc := range []struct {
		filename string
		want     int
	}{
		{"scratch.go", 1},
		{"other.go", 2},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, tc.filename, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := sourceIdentityFindings(file, tc.filename); len(got) != tc.want {
			t.Errorf("%s: findings=%v, want %d", tc.filename, got, tc.want)
		}
	}
}
