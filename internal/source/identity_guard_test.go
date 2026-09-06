package source

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/identityguard"
)

// TestSourceIdentityConstructionStaysBehindStorePrimitives keeps new source
// construction on the shared FileIdentity capture path. It rejects raw
// device/inode reads outside sourceScratchIdentity, but does not track new
// calls to that helper, constant scratchIdentity literals, or os.SameFile.
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
	allow := map[string]map[string]bool{"scratch.go": {"sourceScratchIdentity": true}}
	var findings []identityguard.Finding
	for _, finding := range identityguard.Findings(file) {
		if finding.Kind != identityguard.InodeRead || !identityguard.InsideAllowedFunction(file, finding.Pos, allow[name]) {
			findings = append(findings, finding)
		}
	}
	return findings
}

func TestSourceScratchAllowanceOnlyCoversRawStatReads(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       int
	}{
		{"stat", `_ = stat.Dev; _ = stat.Ino`, 0},
		{"literal", `_ = FileIdentity{Device:1,Inode:2}`, 1},
		{"field", `var id FileIdentity; id.Inode=1`, 1},
		{"handle", `var id FileIdentity; id.Handle=""`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "scratch.go", `package p; type FileIdentity struct{Device,Inode uint64;Handle string}; func sourceScratchIdentity(stat struct{Dev,Ino uint64}) {`+test.body+`}`, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("fixture", fset, []*ast.File{file}, nil); err != nil {
				t.Fatalf("fixture must compile: %v", err)
			}
			if got := sourceIdentityFindings(file, "scratch.go"); len(got) != test.want {
				t.Fatalf("findings=%v, want %d", got, test.want)
			}
			if test.name == "stat" && len(sourceIdentityFindings(file, "other.go")) != 2 {
				t.Fatal("scratch allowance leaked into another file")
			}
		})
	}
}
