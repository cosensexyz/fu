package store

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

func TestOwnedDirectoryRetirementForwardsTheStrongestObservation(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the store package directory")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(currentFile), "retire.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		capture string
		field   string
	}{
		"RemoveOwnedTreeAt":            {capture: "snapshotOwnedOpenDirectory", field: "RootIdentity"},
		"removeOwnedDirectoryContents": {capture: "openIdentity"},
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		expectedOrigin, wanted := want[function.Name.Name]
		if !wanted {
			continue
		}
		captured := identityCaptureResults(function.Body, expectedOrigin.capture)
		found := false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			identifier, identifierOK := call.Fun.(*ast.Ident)
			if !identifierOK || identifier.Name != "retireOwnedDirectoryAtPath" || len(call.Args) != 6 {
				return true
			}
			found = true
			if !identityArgumentComesFromCapture(call.Args[4], captured, expectedOrigin.field) {
				t.Errorf("%s:%d forwards a recorded identity into directory retirement instead of its stronger live observation",
					function.Name.Name, fset.Position(call.Args[4].Pos()).Line)
			}
			return true
		})
		if !found {
			t.Errorf("%s no longer calls retireOwnedDirectoryAtPath", function.Name.Name)
		}
		delete(want, function.Name.Name)
	}
	for name := range want {
		t.Errorf("function %s was not found", name)
	}
}

func TestConfigExchangeRecoveryForwardsLiveArchiveIdentities(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the store package directory")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(currentFile), "config_exchange.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var function *ast.FuncDecl
	for _, declaration := range file.Decls {
		candidate, ok := declaration.(*ast.FuncDecl)
		if ok && candidate.Name.Name == "recoverConfigExchange" {
			function = candidate
			break
		}
	}
	if function == nil {
		t.Fatal("recoverConfigExchange was not found")
	}
	var got []string
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || storeGuardCallName(call) != "archiveNamedConfigEntry" || len(call.Args) != 4 {
			return true
		}
		got = append(got, identityExpression(call.Args[3]))
		return true
	})
	want := []string{"candidate.identity", "active.identity", "active.identity", "targetBefore"}
	if len(got) != len(want) {
		t.Fatalf("archive identity arguments = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("archive identity argument %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func identityExpression(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.SelectorExpr:
		return identityExpression(expression.X) + "." + expression.Sel.Name
	default:
		return ""
	}
}

func identityCaptureResults(body *ast.BlockStmt, captureName string) map[string]bool {
	results := make(map[string]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) == 0 || len(assignment.Rhs) != 1 {
			return true
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || storeGuardCallName(call) != captureName {
			return true
		}
		if identifier, ok := assignment.Lhs[0].(*ast.Ident); ok {
			results[identifier.Name] = true
		}
		return true
	})
	return results
}

func identityArgumentComesFromCapture(expression ast.Expr, captured map[string]bool, field string) bool {
	if field == "" {
		identifier, ok := expression.(*ast.Ident)
		return ok && captured[identifier.Name]
	}
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != field {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && captured[identifier.Name]
}

func storeGuardCallName(call *ast.CallExpr) string {
	switch function := call.Fun.(type) {
	case *ast.Ident:
		return function.Name
	case *ast.SelectorExpr:
		return function.Sel.Name
	default:
		return ""
	}
}

// TestStatInodeIsReadOnlyByTheIdentityPrimitive rejects direct Stat_t.Dev/Ino
// selectors outside identity.go. FileIdentity's fields use the longer Device
// and Inode names, so this catches shortcuts that produce device+inode identities
// without a handle. It also rejects field-by-field identity construction and
// FileIdentity literals, including pointer and recursively type-elided
// container elements, outside the capture primitive and narrowly allowlisted
// decoding paths. It does not claim to detect os.SameFile calls, cross-file
// named declarations, serialization, whole-identity assignment, or whole-stat
// comparisons.
func TestStatInodeIsReadOnlyByTheIdentityPrimitive(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the store package directory")
	}
	assertInodeSelectorsOnlyIn(t, filepath.Dir(currentFile), map[string]bool{"identity.go": true},
		"capture identities through entryIdentityAt or openIdentity in identity.go",
		map[string]map[string]bool{"config_exchange.go": {"parseConfigArchiveName": true}})
}

// assertInodeSelectorsOnlyIn reports every selector named Dev or Ino in the
// package's non-test files outside the allowed set.
func assertInodeSelectorsOnlyIn(
	t *testing.T,
	dir string,
	allowed map[string]bool,
	remedy string,
	downgradeAllowlist map[string]map[string]bool,
) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}
		checked++
		for _, finding := range identityguard.Findings(file) {
			downgradeAllowed := identityguard.InsideAllowedFunction(file, finding.Pos, downgradeAllowlist[name])
			switch finding.Kind {
			case identityguard.InodeRead:
				t.Errorf("%s:%d reads a Stat_t inode directly; %s", name, fset.Position(finding.Pos).Line, remedy)
			case identityguard.HandleAssignment:
				if !downgradeAllowed {
					t.Errorf("%s:%d strips or replaces a captured file handle", name, fset.Position(finding.Pos).Line)
				}
			case identityguard.IdentityFieldAssignment:
				if !downgradeAllowed {
					t.Errorf("%s:%d constructs a FileIdentity field by field outside the capture primitive", name, fset.Position(finding.Pos).Line)
				}
			case identityguard.IdentityLiteral:
				if !downgradeAllowed {
					t.Errorf("%s:%d constructs a FileIdentity outside the capture primitive", name, fset.Position(finding.Pos).Line)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test files were checked; the guard would pass vacuously")
	}
}
