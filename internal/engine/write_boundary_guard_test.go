package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// engineSources parses every non-test file in this package once.
func engineSources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = parsed
	}
	if len(files) == 0 {
		t.Fatal("no sources parsed; this guard is looking at the wrong place")
	}
	return fset, files
}

// A session that is opened must be closed, and its close error must reach the
// caller.
//
// Every write command has always done this, and the reason is in restore.go's
// own comment: a close error means the pinned descriptors did not release
// cleanly, which is not something a successful command should hide. Extracting
// the opening into beginWrite makes forgetting the other half easy -- the
// `defer` no longer sits two lines under the `BeginWrite` that demanded it --
// so the pairing is checked rather than remembered.
//
// The check is written around the discard forms someone would actually reach
// for, which is the lesson of its own first draft: that version only noticed a
// bare `ws.close()` expression statement, so `defer ws.close()` -- the obvious
// way to write the bug -- sailed past it with the whole package green. A guard
// that misses the natural spelling of the mistake is worse than none, because
// it reads as proof.
//
// Both openings count: beginWrite, and the six places that still call
// st.BeginWrite directly. The session's own identifier is tracked so that an
// unrelated `defer f.Close()` in the same function is not mistaken for it.
func TestEveryWriteSessionIsClosedAndItsErrorKept(t *testing.T) {
	fset, files := engineSources(t)
	var checked int
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			held := sessionIdent(fn)
			if held == "" {
				continue
			}
			checked++
			var closed, discarded bool
			var where token.Pos
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch stmt := node.(type) {
				case *ast.DeferStmt:
					// `defer ws.close()` -- the error goes nowhere.
					if closesSession(stmt.Call, held) {
						discarded, where = true, stmt.Pos()
					}
				case *ast.ExprStmt:
					if call, ok := stmt.X.(*ast.CallExpr); ok && closesSession(call, held) {
						discarded, where = true, stmt.Pos()
					}
				case *ast.AssignStmt:
					// `_ = ws.close()`
					if len(stmt.Rhs) == 1 && allBlank(stmt.Lhs) {
						if call, ok := stmt.Rhs[0].(*ast.CallExpr); ok && closesSession(call, held) {
							discarded, where = true, stmt.Pos()
						}
					}
				}
				if call, ok := node.(*ast.CallExpr); ok && closesSession(call, held) {
					closed = true
				}
				return true
			})
			if !closed {
				t.Errorf("%s: %s opens a write session and never closes it", name, fn.Name.Name)
			}
			if discarded {
				t.Errorf("%s: %s throws away its session close error at %s",
					name, fn.Name.Name, fset.Position(where))
			}
		}
	}
	// Seven through beginWrite, six that still open one directly.
	if checked < 13 {
		t.Fatalf("only %d write entry points found; this guard is looking at the wrong thing", checked)
	}
}

// sessionIdent returns the name a function binds its write session to, or "".
func sessionIdent(fn *ast.FuncDecl) string {
	var held string
	ast.Inspect(fn, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		opens := false
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			opens = fun.Name == "beginWrite"
		case *ast.SelectorExpr:
			opens = fun.Sel.Name == "BeginWrite"
		}
		if !opens {
			return true
		}
		if ident, ok := assign.Lhs[0].(*ast.Ident); ok && ident.Name != "_" {
			held = ident.Name
		}
		return true
	})
	return held
}

// closesSession reports whether call is held.close() or held.Close().
func closesSession(call *ast.CallExpr, held string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "close" && selector.Sel.Name != "Close") {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == held
}

func allBlank(exprs []ast.Expr) bool {
	for _, expr := range exprs {
		ident, ok := expr.(*ast.Ident)
		if !ok || ident.Name != "_" {
			return false
		}
	}
	return len(exprs) > 0
}

// Which commands sweep is a product decision, not an implementation detail, and
// a refactor is exactly where an extra one gets added by accident -- a shared
// prologue that sweeps would give restore and a named commit a store-wide sweep
// neither is allowed to have.
//
// SPEC §5.3 rests restore on not sweeping; batch 2 requires a named commit to
// take only its own prefix. Both are invisible in a diff that merely moves the
// call into a shell, which is why the set is named here rather than trusted.
func TestOnlyTheCommandsThatMaySweepDoSweep(t *testing.T) {
	_, files := engineSources(t)
	allowed := map[string]bool{
		"writeCommandPrologue": true,
		"run":                  true,
		"RevertOperations":     true,
		"PullOperations":       true,
	}
	found := map[string]bool{}
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var sweeps bool
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok && isSelectorCall(call, "Sweep") {
					sweeps = true
				}
				return true
			})
			if !sweeps {
				continue
			}
			found[fn.Name.Name] = true
			if !allowed[fn.Name.Name] {
				t.Errorf("%s: %s sweeps the store, and only %v may", name, fn.Name.Name, keysOf(allowed))
			}
		}
	}
	for want := range allowed {
		if !found[want] {
			t.Errorf("%s no longer sweeps; if that is deliberate it is a semantic change, not a refactor", want)
		}
	}
}

func callsFunc(fn *ast.FuncDecl, name string) bool {
	var found bool
	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
			found = true
		}
		return true
	})
	return found
}

func isSelectorCall(call *ast.CallExpr, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == name
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
