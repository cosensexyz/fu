package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// With no remote configured there is nothing to compare, and nothing to say.
func TestStatusOmitsTheRemoteSectionWithoutARemote(t *testing.T) {
	s, cfg := setupStore(t, "alpha")

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Store.Remote != nil {
		t.Fatalf("remote = %+v, want nothing to report", report.Store.Remote)
	}
}

// The ordinary case: a remote is set, the comparison runs, and status carries
// the relation with both commits named.
func TestStatusReportsTheRemoteRelation(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	bare := filepath.Join(t.TempDir(), "store.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRemote(bare); err != nil {
		t.Fatal(err)
	}

	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Store.Remote == nil {
		t.Fatal("a configured remote must be reported")
	}
	if report.Store.Remote.Relation != RemoteEmpty {
		t.Fatalf("relation = %v, want RemoteEmpty for a fresh bare remote (%+v)", report.Store.Remote.Relation, report.Store.Remote)
	}
	if report.Store.Remote.Err != "" {
		t.Fatalf("an empty remote is a state, not a failure: %q", report.Store.Remote.Err)
	}
}

// A remote that cannot be reached costs the user that one line and nothing
// else: the agent section, the worktree section and the two inventories are
// all local facts, and the command still exits 0.
func TestStatusKeepsEveryLocalSectionWhenTheRemoteFails(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	if _, err := s.SetRemote(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	report, err := Status(s, cfg, []agent.Agent{fakeAgent{"claude", dir}})
	if err != nil {
		t.Fatalf("an unreachable remote must not fail the report: %v", err)
	}
	if report.Store.Remote == nil || report.Store.Remote.Err == "" {
		t.Fatalf("want the remote failure recorded: %+v", report.Store.Remote)
	}
	if len(report.Agents) != 1 || report.Agents[0].Name != "claude" {
		t.Fatalf("the agent section must survive: %+v", report.Agents)
	}
	if len(report.Agents[0].Drift) == 0 {
		t.Fatalf("alpha is enabled and unlinked; that drift must still be reported: %+v", report.Agents[0])
	}
	// The remote is named by its own field rather than by the error text --
	// go-git's "repository not found" carries no path -- so the report can
	// say which remote failed without every transport error having to.
	if !strings.Contains(report.Store.Remote.URL, "does-not-exist") {
		t.Fatalf("the failing remote must still be named: %+v", report.Store.Remote)
	}
}

// A hand-kept copy of another package's enum has exactly one failure mode: a
// value added there and missed here, which relationFromStore answers with
// RemoteUnknown -- a plausible-looking lie rather than a build error, since Go
// has no exhaustive switch.
//
// This is checked at the source level, not by probing values at runtime. A
// runtime probe cannot be complete: Go constants leave nothing in the binary
// for reflection to enumerate, so a walk over integers only ever catches a new
// value that volunteers its own identity -- through a String() case, or a
// mapping that does not fall through. A value with neither is indistinguishable
// from an undefined number, and that is exactly the shape a hurried addition
// takes. Parsing the const block has no such blind spot, and is the technique
// this repo already uses for cross-file invariants (identityguard, the README
// transcript check, the delivery-hint guard).
func TestEveryStoreRemoteRelationHasAnEngineCounterpart(t *testing.T) {
	declared := storeRelationNames(t)
	if len(declared) == 0 {
		t.Fatal("no RemoteRelation constants parsed from internal/store; the check would pass vacuously")
	}
	mapped := relationFromStoreCases(t)
	for _, name := range declared {
		if !mapped[name] {
			t.Errorf("store.%s has no case in relationFromStore; a value it does not map is answered with RemoteUnknown, which reads as a finding rather than an omission", name)
		}
	}
	for name := range mapped {
		if !slices.Contains(declared, name) {
			t.Errorf("relationFromStore maps store.%s, which internal/store no longer declares", name)
		}
	}

	// The mapping is also checked by value, so a case that compiles but sends
	// two states to one engine value cannot let the report describe one as the
	// other.
	seen := map[RemoteRelation]store.RemoteRelation{}
	for from, to := range map[store.RemoteRelation]RemoteRelation{
		store.RemoteSynced:   RemoteSynced,
		store.RemoteAhead:    RemoteAhead,
		store.RemoteBehind:   RemoteBehind,
		store.RemoteDiverged: RemoteDiverged,
		store.RemoteUnknown:  RemoteUnknown,
		store.RemoteEmpty:    RemoteEmpty,
		store.RemoteNoBranch: RemoteNoBranch,
	} {
		if got := relationFromStore(from); got != to {
			t.Fatalf("relationFromStore(%v) = %v, want %v", from, got, to)
		}
		if other, clash := seen[to]; clash {
			t.Fatalf("%v and %v both map to %v", from, other, to)
		}
		seen[to] = from
	}
}

// storeRelationNames reads the names of every constant declared with type
// RemoteRelation anywhere in the store package.
//
// The whole package, not one file: a constant of this type declared in a
// second file would otherwise leave the guard green while relationFromStore
// answered it with RemoteUnknown -- the exact failure this exists to prevent,
// and a one-file read is how a guard comes to describe the package while
// checking a file.
func storeRelationNames(t *testing.T) []string {
	t.Helper()
	packages, err := parser.ParseDir(token.NewFileSet(), "../store", func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the store package: %v", err)
	}
	pkg, ok := packages["store"]
	if !ok {
		t.Fatalf("no package store under ../store, found %v", slices.Collect(maps.Keys(packages)))
	}
	var names []string
	var decls []ast.Decl
	for _, file := range pkg.Files {
		decls = append(decls, file.Decls...)
	}
	inRelationBlock := false
	for _, decl := range decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Only the first spec of an iota block carries the type; the rest
			// inherit it, which is what inRelationBlock tracks.
			if ident, ok := value.Type.(*ast.Ident); ok {
				inRelationBlock = ident.Name == "RemoteRelation"
			}
			if !inRelationBlock {
				continue
			}
			for _, name := range value.Names {
				if name.Name != "_" {
					names = append(names, name.Name)
				}
			}
		}
		inRelationBlock = false
	}
	return names
}

// relationFromStoreCases reads the store constant each case of
// relationFromStore's switch names.
func relationFromStoreCases(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "status.go", nil, 0)
	if err != nil {
		t.Fatalf("parse the engine's mapping: %v", err)
	}
	cases := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "relationFromStore" {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			clause, ok := inner.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				selector, ok := expr.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "store" {
					cases[selector.Sel.Name] = true
				}
			}
			return true
		})
		return false
	})
	return cases
}
