package cli

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

func TestCLITestsDoNotIgnoreFilesystemSetupErrors(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate CLI test directory")
	}
	dir := filepath.Dir(currentFile)
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	returnsError := map[string]bool{
		"Chmod": true, "MkdirAll": true, "Remove": true,
		"Rename": true, "Symlink": true, "WriteFile": true,
	}
	fset := token.NewFileSet()
	for _, entry := range files {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		filename := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", entry.Name(), err)
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			expr, ok := node.(*ast.ExprStmt)
			if !ok {
				return true
			}
			call, ok := expr.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !returnsError[selector.Sel.Name] {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			position := fset.Position(call.Pos())
			t.Errorf("%s:%d ignores os.%s error", entry.Name(), position.Line, selector.Sel.Name)
			return true
		})
	}
}
