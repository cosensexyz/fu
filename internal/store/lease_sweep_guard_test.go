package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The sweep's switch is the last thing standing between a classification and a
// deletion, and the only guard it had checked that every LeaseState has a name
// in String(). That is not the same property at all: reintroducing the original
// defect -- dropping the LeaseVanished arm, the default, and the held == nil
// check, so the switch once again listed what must not be deleted -- left the
// whole package green, including the test that claimed to cover exactly this.
//
// So the guard reads the switch itself. A test that can only see String() can
// only tell you the enum is tidy.
func TestSweepDeletesOnlyForTheOneStateItNames(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lease_reclaim.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	declared := declaredLeaseStates(t, file)
	sweep := sweepStateSwitch(t, file)

	// Every case body must end in a return, or the state falls out of the
	// switch and into the removal below it -- which is the defect, exactly.
	// The one exception is the empty body that deliberately falls through.
	var fallsThrough []string
	seen := map[string]bool{}
	for _, clause := range sweep.Body.List {
		caseClause := clause.(*ast.CaseClause)
		names := caseStateNames(caseClause)
		for _, name := range names {
			if seen[name] {
				t.Fatalf("state %s appears in more than one case of the sweep's switch", name)
			}
			seen[name] = true
		}
		if len(caseClause.Body) == 0 {
			fallsThrough = append(fallsThrough, names...)
			continue
		}
		last := caseClause.Body[len(caseClause.Body)-1]
		if _, ok := last.(*ast.ReturnStmt); !ok {
			label := "default"
			if len(names) > 0 {
				label = names[0]
			}
			t.Fatalf("the %s case of the sweep's switch does not end in a return, so it reaches the removal below it", label)
		}
	}

	if len(fallsThrough) != 1 || fallsThrough[0] != "LeaseCollectable" {
		t.Fatalf("states reaching the removal = %v, want exactly [LeaseCollectable]", fallsThrough)
	}

	// A default that does nothing is what makes forgetting a new state cost a
	// missed collection rather than a deletion nobody asked for.
	if !hasDefaultReturningNil(sweep) {
		t.Fatal("the sweep's switch has no default that returns without deleting; a state added later would fall through into the removal")
	}

	// And every state that exists today is answered explicitly, so the default
	// stays a backstop rather than the place most states are handled.
	for _, name := range declared {
		if name == "LeaseStateUnset" {
			// Not a verdict. It reaches the default, which is where a value
			// that means "nothing was decided" belongs.
			continue
		}
		if !seen[name] {
			t.Fatalf("state %s is declared but not named in the sweep's switch", name)
		}
	}
}

// Lease.write must not shorten the record before rewriting it. The Truncate
// that was there made the file empty to every reader the instant it returned,
// and a death in that window left a zero-length lease no later run can parse --
// so the object it covered would never be collected.
//
// This is a source guard because the behaviour cannot be observed from outside:
// restoring the Truncate leaves every assertion about the resulting record
// true, and the whole package green. The only way to see it is to look.
func TestLeaseWriteDoesNotTruncate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lease.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	write := methodDecl(file, "Lease", "write")
	if write == nil {
		t.Fatal("Lease.write not found; this guard is looking at the wrong thing")
	}
	ast.Inspect(write, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "Truncate" {
			t.Fatalf("Lease.write calls Truncate at %s: a record only ever grows, and the truncation is visible to readers before the rewrite lands", fset.Position(selector.Pos()))
		}
		return true
	})
}

func declaredLeaseStates(t *testing.T, file *ast.File) []string {
	t.Helper()
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		typed := false
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			if ident, ok := value.Type.(*ast.Ident); ok && ident.Name == "LeaseState" {
				typed = true
			}
			if !typed {
				continue
			}
			for _, name := range value.Names {
				if name.Name != "_" {
					names = append(names, name.Name)
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no LeaseState constants found; this guard is looking at the wrong thing")
	}
	return names
}

// sweepStateSwitch finds the switch on finding.State inside ReclaimLeases.
func sweepStateSwitch(t *testing.T, file *ast.File) *ast.SwitchStmt {
	t.Helper()
	var found *ast.SwitchStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ReclaimLeases" || fn.Recv != nil {
			continue
		}
		ast.Inspect(fn, func(node ast.Node) bool {
			stmt, ok := node.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			selector, ok := stmt.Tag.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "State" {
				return true
			}
			if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != "finding" {
				return true
			}
			found = stmt
			return false
		})
	}
	if found == nil {
		t.Fatal("no switch on finding.State inside ReclaimLeases; this guard is looking at the wrong thing")
	}
	return found
}

func caseStateNames(clause *ast.CaseClause) []string {
	var names []string
	for _, expr := range clause.List {
		if ident, ok := expr.(*ast.Ident); ok {
			names = append(names, ident.Name)
		}
	}
	return names
}

func hasDefaultReturningNil(sweep *ast.SwitchStmt) bool {
	for _, clause := range sweep.Body.List {
		caseClause := clause.(*ast.CaseClause)
		if caseClause.List != nil {
			continue
		}
		if len(caseClause.Body) != 1 {
			return false
		}
		ret, ok := caseClause.Body[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return false
		}
		ident, ok := ret.Results[0].(*ast.Ident)
		return ok && ident.Name == "nil"
	}
	return false
}

func methodDecl(file *ast.File, receiver, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		typeExpr := fn.Recv.List[0].Type
		if star, ok := typeExpr.(*ast.StarExpr); ok {
			typeExpr = star.X
		}
		if ident, ok := typeExpr.(*ast.Ident); ok && ident.Name == receiver {
			return fn
		}
	}
	return nil
}
