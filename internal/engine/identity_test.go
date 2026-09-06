package engine

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/identityguard"
	"github.com/cosensexyz/fu/internal/store"
	"github.com/cosensexyz/fu/internal/testenv"
	"golang.org/x/sys/unix"
)

// TestStatInodeIsReadOnlyByTheStoreIdentityPrimitive is the engine half of the
// store guard of the same name. It rejects direct Stat_t.Dev/Ino selectors,
// field-by-field identity construction, and FileIdentity literals, including
// pointer and recursively type-elided container elements. It does not detect
// os.SameFile calls, cross-file named declarations, serialization,
// whole-identity assignment, or whole-stat comparisons. reconcile.go is
// allowed currently only for sameCheckedEntry, the retired-link recheck that
// still compares device and inode from two os.FileInfo snapshots. Shrink the
// allowlist when that site moves onto the store identity primitive.
func TestStatInodeIsReadOnlyByTheStoreIdentityPrimitive(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the engine package directory")
	}
	dir := filepath.Dir(currentFile)
	inodeAllowlist := map[string]map[string]bool{
		"reconcile.go": {"sameCheckedEntry": true},
	}
	downgradeAllowlist := map[string]map[string]bool{
		"adopt_link_archive.go": {"marshalAdoptLinkArchive": true},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}
		checked++
		for _, finding := range identityguard.Findings(file) {
			inodeAllowed := identityguard.InsideAllowedFunction(file, finding.Pos, inodeAllowlist[name])
			downgradeAllowed := identityguard.InsideAllowedFunction(file, finding.Pos, downgradeAllowlist[name])
			switch finding.Kind {
			case identityguard.InodeRead:
				if !inodeAllowed {
					t.Errorf("%s:%d reads a Stat_t inode directly; capture identities through store.EntryIdentityAt or store.OpenIdentity",
						name, fset.Position(finding.Pos).Line)
				}
			case identityguard.HandleAssignment:
				if !downgradeAllowed {
					t.Errorf("%s:%d strips or replaces a captured file handle", name, fset.Position(finding.Pos).Line)
				}
			case identityguard.IdentityFieldAssignment:
				if !downgradeAllowed {
					t.Errorf("%s:%d constructs a FileIdentity field by field outside the store capture primitive", name, fset.Position(finding.Pos).Line)
				}
			case identityguard.IdentityLiteral:
				if !downgradeAllowed {
					t.Errorf("%s:%d constructs a FileIdentity outside the store capture primitive", name, fset.Position(finding.Pos).Line)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test files were checked; the guard would pass vacuously")
	}
}

func TestFreshArchivePlanDoesNotRestatTheValidatedEntry(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the engine package directory")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(currentFile), "adopt_switch.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "freshArchivePlan" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok && adoptEntryRecaptureCall(call) {
				t.Errorf("freshArchivePlan re-stats the name at line %d instead of using the sealed validation stat", fset.Position(call.Pos()).Line)
			}
			return true
		})
		return
	}
	t.Fatal("freshArchivePlan was not found")
}

func TestPairBoundAdoptRootRequiresTheFullDescriptorIdentityTriangle(t *testing.T) {
	path := t.TempDir()
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	tests := []struct {
		name     string
		expected store.FileIdentity
		captures []store.FileIdentity
	}{
		{
			name:     "directory descriptor versus expectation",
			expected: store.FileIdentity{Device: 1, Inode: 2, Handle: "1:aa"},
			captures: []store.FileIdentity{{Device: 1, Inode: 2, Handle: "1:bb"}, {Device: 1, Inode: 2}},
		},
		{
			name:     "root descriptor versus expectation",
			expected: store.FileIdentity{Device: 1, Inode: 2, Handle: "1:aa"},
			captures: []store.FileIdentity{{Device: 1, Inode: 2}, {Device: 1, Inode: 2, Handle: "1:bb"}},
		},
		{
			name:     "descriptor pair",
			expected: store.FileIdentity{Device: 1, Inode: 2},
			captures: []store.FileIdentity{{Device: 1, Inode: 2, Handle: "1:aa"}, {Device: 1, Inode: 2, Handle: "1:bb"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			root, err := pairBoundAdoptRootWithIdentityCapture(path, dir, test.expected, func(int) (store.FileIdentity, unix.Stat_t, error) {
				identity := test.captures[calls]
				calls++
				return identity, unix.Stat_t{}, nil
			})
			if root != nil {
				_ = root.Close()
			}
			if !errors.Is(err, ErrTxnConflict) {
				t.Fatalf("identity triangle mismatch must conflict, got %v", err)
			}
			if calls != 2 {
				t.Fatalf("descriptor identity captures = %d, want 2", calls)
			}
		})
	}
}

func TestPostNamespaceActionValidationDoesNotReuseRecordedIdentity(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the engine package directory")
	}
	dir := filepath.Dir(currentFile)
	fset := token.NewFileSet()
	for _, name := range engineProductionGoFiles(t, dir) {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range postNamespaceActionRecordIdentities(file) {
			t.Errorf("%s:%d namespace action is not followed by revalidation proven from its live pre-action observation",
				name, fset.Position(finding.Pos()).Line)
		}
	}
}

func TestRecordBoundPostActionWrappersAreNotAvailable(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the engine package directory")
	}
	dir := filepath.Dir(currentFile)
	forbidden := map[string]bool{
		"validateDirSwitchBackup":        true,
		"validateDirSwitchSibling":       true,
		"validateDirSwitchLink":          true,
		"validateRetiredAdoptOriginal":   true,
		"openDirSwitchDirectoryObserved": true,
	}
	for _, name := range engineProductionGoFiles(t, dir) {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && forbidden[function.Name.Name] {
				t.Errorf("%s still exposes record-bound wrapper %s", name, function.Name.Name)
			}
		}
	}
}

func engineProductionGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("no engine production files found")
	}
	return names
}

func TestPostNamespaceActionRecordIdentityDetector(t *testing.T) {
	const source = `package p
type Identity struct{}
type State struct { BackupIdentity Identity }
type Info struct{}
func renameDirSwitchEntry(any, string, string, string) error { return nil }
func validateDirSwitchBackup(...any) error { return nil }
func validateDirSwitchSibling(...any) error { return nil }
func validateDirSwitchLink(...any) error { return nil }
func validateRetiredAdoptOriginal(...any) error { return nil }
func observeDirSwitchBackup(any, any, *State, Identity) (Identity, error) { return Identity{}, nil }
func inspectFuLink(any, string, string) (Info, string, bool, error) { return Info{}, "", true, nil }
func sameCheckedEntry(Info, Info) bool { return true }
var store = struct { RetireNameAt func(any, string, string) (string, error) }{}
func wrapperBad(parent any, sw *State) error {
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	return validateDirSwitchBackup(parent, sw)
}
func siblingWrapperBad(parent any, sw *State) error {
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	return validateDirSwitchSibling(parent, sw)
}
func linkWrapperBad(parent any, sw *State) error {
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	return validateDirSwitchLink(parent, sw)
}
func retiredWrapperBad(parent any, sw *State) error {
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	return validateRetiredAdoptOriginal(parent, sw)
}
func renamedBad(parent any, sw *State) error {
	state := sw
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	_, err := observeDirSwitchBackup(parent, nil, state, state.BackupIdentity)
	return err
}
func aliasBad(parent any, sw *State) error {
	recorded := sw.BackupIdentity
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	_, err := observeDirSwitchBackup(parent, nil, sw, recorded)
	return err
}
func nestedBad(parent any, sw *State, live bool) error {
	if live {
		if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	}
	_, err := observeDirSwitchBackup(parent, nil, sw, sw.BackupIdentity)
	return err
}
func overwrittenBad(parent any, sw *State) error {
	observed, err := observeDirSwitchBackup(parent, nil, sw, sw.BackupIdentity)
	if err != nil { return err }
	observed = sw.BackupIdentity
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	_, err = observeDirSwitchBackup(parent, nil, sw, observed)
	return err
}
func overwrittenAfterActionBad(parent any, sw *State) error {
	observed, err := observeDirSwitchBackup(parent, nil, sw, sw.BackupIdentity)
	if err != nil { return err }
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	observed = sw.BackupIdentity
	_, err = observeDirSwitchBackup(parent, nil, sw, observed)
	return err
}
func retireRecordedBad(parent any, recorded Info) error {
	approved, _, _, err := inspectFuLink(parent, "live", "store")
	if err != nil { return err }
	if _, err := store.RetireNameAt(parent, "live", ".retired-"); err != nil { return err }
	if !sameCheckedEntry(recorded, approved) { return nil }
	return nil
}
func retireLiveGood(parent any) error {
	approved, _, _, err := inspectFuLink(parent, "live", "store")
	if err != nil { return err }
	if _, err := store.RetireNameAt(parent, "live", ".retired-"); err != nil { return err }
	if !sameCheckedEntry(approved, approved) { return nil }
	return nil
}
func good(parent any, sw *State) error {
	observed, err := observeDirSwitchBackup(parent, nil, sw, sw.BackupIdentity)
	if err != nil { return err }
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	_, err = observeDirSwitchBackup(parent, nil, sw, observed)
	return err
}`
	file := parseAndTypeCheckIdentityFixture(t, source)
	findings := postNamespaceActionRecordIdentities(file)
	want := []string{"wrapperBad", "siblingWrapperBad", "linkWrapperBad", "retiredWrapperBad", "renamedBad", "aliasBad", "nestedBad", "overwrittenBad", "overwrittenAfterActionBad", "retireRecordedBad"}
	got := findingFunctionNames(file, findings)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("finding functions = %v, want %v", got, want)
	}

}

func TestPostNamespaceActionDetectorReportsTheActionForAnUnprovenRevalidation(t *testing.T) {
	const source = `package p
type Identity struct{}
type State struct { BackupIdentity Identity }
func renameDirSwitchEntry(any, string, string, string) error { return nil }
func renamedWrapper(any, *State) error { return nil }
func observeDirSwitchBackup(any, any, *State, Identity) (Identity, error) { return Identity{}, nil }
func bad(parent any, sw *State) error {
	if err := renameDirSwitchEntry(parent, "live", "retired", "retire"); err != nil { return err }
	if err := renamedWrapper(parent, sw); err != nil { return err }
	_, err := observeDirSwitchBackup(parent, nil, sw, sw.BackupIdentity)
	return err
}`
	file := parseAndTypeCheckIdentityFixture(t, source)
	var action *ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && namespaceActionCall(call) {
			action = call
		}
		return true
	})
	findings := postNamespaceActionRecordIdentities(file)
	if action == nil || len(findings) != 1 || findings[0].Pos() != action.Pos() {
		t.Fatalf("finding = %v, want the namespace action at %v", findings, action)
	}
}

func TestPostActionIdentityArgumentTracksEveryHelperSignature(t *testing.T) {
	const source = `package p
type Identity struct{}
func (Identity) Same(Identity) bool { return true }
func observeRetiredAdoptOriginalWithCapture(...any) (Identity,error) { return Identity{},nil }
func validateCurrentAdoptEntryStatWithCapture(...any) (Identity, int, error) { return Identity{}, 0, nil }
func observeDirSwitchBackup(...any) (Identity, error) { return Identity{}, nil }
func observeDirSwitchBackupWithCapture(...any) (Identity, error) { return Identity{}, nil }
func observeDirSwitchSibling(...any) (Identity, error) { return Identity{}, nil }
func observeDirSwitchSiblingWithCapture(...any) (Identity, error) { return Identity{}, nil }
func observeDirSwitchLink(...any) (Identity, error) { return Identity{}, nil }
func observeDirSwitchLinkWithCapture(...any) (Identity, error) { return Identity{}, nil }
func f(expected Identity) {
	_, _ = observeRetiredAdoptOriginalWithCapture(nil, nil, nil, expected, nil)
	_, _, _ = validateCurrentAdoptEntryStatWithCapture(nil, nil, "", expected, nil)
	_, _ = observeDirSwitchBackup(nil, nil, nil, expected)
	_, _ = observeDirSwitchBackupWithCapture(nil, nil, nil, expected, nil)
	_, _ = observeDirSwitchSibling(nil, "", "", nil, expected)
	_, _ = observeDirSwitchSiblingWithCapture(nil, "", "", nil, expected, nil)
	_, _ = observeDirSwitchLink(nil, "", nil, expected)
	_, _ = observeDirSwitchLinkWithCapture(nil, "", nil, expected, nil)
	_ = expected.Same(expected)
}`
	file := parseAndTypeCheckIdentityFixture(t, source)
	want := map[string]int{
		"observeRetiredAdoptOriginalWithCapture":   1,
		"validateCurrentAdoptEntryStatWithCapture": 1,
		"observeDirSwitchBackup":                   1,
		"observeDirSwitchBackupWithCapture":        1,
		"observeDirSwitchSibling":                  1,
		"observeDirSwitchSiblingWithCapture":       1,
		"observeDirSwitchLink":                     1,
		"observeDirSwitchLinkWithCapture":          1,
		"Same":                                     1,
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || want[calledFunctionName(call)] == 0 {
			return true
		}
		expected, ok := postActionIdentityArgument(call)
		identifier, identifierOK := expected.(*ast.Ident)
		if !ok || !identifierOK || identifier.Name != "expected" {
			t.Errorf("%s expected-identity argument = %T %v", calledFunctionName(call), expected, expected)
		}
		delete(want, calledFunctionName(call))
		return true
	})
	for name := range want {
		t.Errorf("signature fixture for %s was not inspected", name)
	}
}

func TestPostActionIdentityIndicesMatchProductionHelperSignatures(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the engine package directory")
	}
	dir := filepath.Dir(currentFile)
	want := map[string]string{
		"observeRetiredAdoptOriginalWithCapture":    "expectedIdentity",
		"validateCurrentAdoptEntryStatWithCapture":  "expectedIdentity",
		"openDirSwitchDirectory":                    "expected",
		"openDirSwitchDirectoryObservedWithCapture": "expected",
		"observeDirSwitchBackup":                    "expectedIdentity",
		"observeDirSwitchBackupWithCapture":         "expectedIdentity",
		"observeDirSwitchSibling":                   "expectedIdentity",
		"observeDirSwitchSiblingWithCapture":        "expectedIdentity",
		"observeDirSwitchLink":                      "expectedIdentity",
		"observeDirSwitchLinkWithCapture":           "expectedIdentity",
		"sameCheckedEntry":                          "left",
	}
	for _, filename := range engineProductionGoFiles(t, dir) {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, filename), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			parameter, tracked := want[function.Name.Name]
			if !tracked {
				continue
			}
			index, found := functionParameterIndex(function, parameter)
			if !found {
				t.Errorf("%s no longer has parameter %s", function.Name.Name, parameter)
			} else if got := postActionExpectedIdentityIndex(function.Name.Name); got != index {
				t.Errorf("%s expected-identity index = %d, production signature index = %d", function.Name.Name, got, index)
			}
			delete(want, function.Name.Name)
		}
	}
	for name := range want {
		t.Errorf("production helper %s was not found", name)
	}
}

func functionParameterIndex(function *ast.FuncDecl, parameter string) (int, bool) {
	index := 0
	for _, field := range function.Type.Params.List {
		if len(field.Names) == 0 {
			index++
			continue
		}
		for _, name := range field.Names {
			if name.Name == parameter {
				return index, true
			}
			index++
		}
	}
	return -1, false
}

func TestCompleteDirSwitchThreadsThePostLandObservationIntoItsSharedTail(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the engine package directory")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(currentFile), "adopt_switchdir.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var function *ast.FuncDecl
	for _, declaration := range file.Decls {
		candidate, ok := declaration.(*ast.FuncDecl)
		if ok && candidate.Name.Name == "completeDirSwitch" {
			function = candidate
			break
		}
	}
	if function == nil {
		t.Fatal("completeDirSwitch was not found")
	}
	if !completeDirSwitchTailUsesLiveIdentity(function) {
		t.Fatal("completeDirSwitch must prove its landing from a live identity before removing its backup")
	}
}

func completeDirSwitchTailUsesLiveIdentity(function *ast.FuncDecl) bool {
	var landAction token.Pos
	var finalObservation *ast.CallExpr
	var backupRemoval token.Pos
	var capturedAt token.Pos
	var capturedName string
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, callOK := node.(*ast.CallExpr)
		if callOK && calledFunctionName(call) == "removeDirSwitchBackup" {
			backupRemoval = call.Pos()
		}
		if callOK && calledFunctionName(call) == "renameDirSwitchEntry" {
			landAction = call.Pos()
		}
		assignment, assignmentOK := node.(*ast.AssignStmt)
		if assignmentOK && landAction != token.NoPos && assignment.Pos() > landAction && len(assignment.Rhs) == 1 {
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if ok && calledFunctionName(call) == "observeDirSwitchSibling" && len(assignment.Lhs) > 0 {
				if identifier, ok := assignment.Lhs[0].(*ast.Ident); ok && identifier.Name != "_" {
					capturedAt = assignment.End()
					capturedName = identifier.Name
				}
			}
		}
		if callOK && calledFunctionName(call) == "observeDirSwitchSibling" {
			finalObservation = call
		}
		return true
	})
	live := map[string]bool{capturedName: capturedName != ""}
	if finalObservation != nil {
		updateIdentityLiveness(function.Body, capturedAt, finalObservation.Pos(), live, false)
	}
	if landAction == token.NoPos || capturedAt == token.NoPos || finalObservation == nil || backupRemoval == token.NoPos || finalObservation.Pos() >= backupRemoval || len(finalObservation.Args) < 5 ||
		!liveIdentityExpression(finalObservation.Args[4], live) {
		return false
	}
	return true
}

func postNamespaceActionRecordIdentities(file *ast.File) []ast.Node {
	var findings []ast.Node
	seen := make(map[token.Pos]bool)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		var actions []*ast.CallExpr
		var candidates []postActionIdentityCandidate
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if namespaceActionCall(call) {
				actions = append(actions, call)
			}
			if candidate, ok := postActionCandidate(call); ok {
				candidates = append(candidates, candidate)
			}
			return true
		})
		for _, action := range actions {
			candidate, ok := firstCandidateAfter(candidates, action.End())
			if !ok {
				// This low-level wrapper delegates object validation to its callers.
				// Its calls are themselves recognized actions, so deleting a caller's
				// proof cannot disappear into this allowance.
				if function.Recv == nil && function.Name.Name == "renameDirSwitchEntry" {
					continue
				}
				findings = append(findings, action)
				continue
			}
			liveIdentities := liveIdentityNamesBefore(function.Body, action.Pos())
			updateIdentityLiveness(function.Body, action.End(), candidate.node.Pos(), liveIdentities, false)
			if candidate.alwaysUnsafe || !liveIdentityExpression(candidate.expected, liveIdentities) {
				if !seen[action.Pos()] {
					seen[action.Pos()] = true
					findings = append(findings, action)
				}
			}
		}
	}
	return findings
}

type postActionIdentityCandidate struct {
	node         ast.Node
	expected     ast.Expr
	alwaysUnsafe bool
}

func postActionCandidate(call *ast.CallExpr) (postActionIdentityCandidate, bool) {
	if postActionRecordWrapper(call) {
		return postActionIdentityCandidate{node: call, alwaysUnsafe: true}, true
	}
	if expected, ok := postActionIdentityArgument(call); ok {
		return postActionIdentityCandidate{node: expected, expected: expected}, true
	}
	for _, argument := range call.Args {
		var recorded ast.Expr
		ast.Inspect(argument, func(node ast.Node) bool {
			if recorded != nil {
				return false
			}
			if selector, ok := node.(*ast.SelectorExpr); ok && recordedIdentitySelector(selector) {
				recorded = selector
			}
			return true
		})
		if recorded != nil {
			return postActionIdentityCandidate{node: recorded, expected: recorded, alwaysUnsafe: true}, true
		}
	}
	return postActionIdentityCandidate{}, false
}

// firstCandidateAfter deliberately does not scan every later observation:
// a full scan of the engine produced nine false positives from mutually
// exclusive switch arms, observations of a different object, and later
// captures belonging to a different action. Cross-action shared tails need
// their own ordering proof; positional proximity is not control-flow analysis.
// Known misses include a proof in a mutually exclusive switch arm, a proof
// inside a deferred closure, and a namespace action in a function literal
// invoked later. Helper-contained actions are invisible at the call site;
// adopt's resume paths instead have behavioral tests at durable boundaries.
// Object correspondence is also unchecked: a first candidate observing a
// different object can hide a missing proof, not just create false positives.
func firstCandidateAfter(candidates []postActionIdentityCandidate, boundary token.Pos) (postActionIdentityCandidate, bool) {
	var first postActionIdentityCandidate
	found := false
	for _, candidate := range candidates {
		if candidate.node.Pos() <= boundary || found && candidate.node.Pos() >= first.node.Pos() {
			continue
		}
		first = candidate
		found = true
	}
	return first, found
}

func liveIdentityNamesBefore(body *ast.BlockStmt, boundary token.Pos) map[string]bool {
	live := make(map[string]bool)
	updateIdentityLiveness(body, token.NoPos, boundary, live, true)
	return live
}

// updateIdentityLiveness follows textual assignments, not loop iterations.
// Loop-carried values require a separate data-flow proof.
func updateIdentityLiveness(body *ast.BlockStmt, after, boundary token.Pos, live map[string]bool, allowCaptures bool) {
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || assignment.Pos() <= after || assignment.Pos() >= boundary {
			return true
		}
		captureIndex := -1
		if allowCaptures && len(assignment.Rhs) == 1 {
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if ok {
				captureIndex = liveIdentityCaptureResultIndex(call)
			}
		}
		for index, left := range assignment.Lhs {
			isLive := captureIndex >= 0 && index == captureIndex
			if !isLive && index < len(assignment.Rhs) {
				isLive = liveIdentityExpression(assignment.Rhs[index], live)
			}
			setIdentityLiveness(left, live, isLive)
		}
		return true
	})
}

func setIdentityLiveness(expression ast.Expr, live map[string]bool, isLive bool) {
	var name string
	switch left := expression.(type) {
	case *ast.Ident:
		name = left.Name
	case *ast.SelectorExpr:
		if left.Sel.Name == "Identity" {
			if identifier, ok := left.X.(*ast.Ident); ok {
				name = identifier.Name
			}
		}
	}
	if name == "" {
		return
	}
	if isLive {
		live[name] = true
		return
	}
	delete(live, name)
}

func liveIdentityCaptureResultIndex(call *ast.CallExpr) int {
	switch calledFunctionName(call) {
	case "EntryIdentityAt", "OpenIdentity", "validateCurrentAdoptEntryStat", "validateCurrentAdoptEntryStatWithCapture",
		"observeDirSwitchBackup", "observeDirSwitchBackupWithCapture", "observeDirSwitchSibling", "observeDirSwitchSiblingWithCapture",
		"observeDirSwitchLink", "observeDirSwitchLinkWithCapture", "observeRetiredAdoptOriginalWithCapture", "ensureAdoptOriginalRetired", "observeDirSwitchMovedEntry", "inspectFuLink":
		return 0
	case "openDirSwitchDirectoryObservedWithCapture":
		return 2
	default:
		return -1
	}
}

func liveIdentityExpression(expression ast.Expr, live map[string]bool) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && live[identifier.Name]
}

func postActionRecordWrapper(call *ast.CallExpr) bool {
	switch calledFunctionName(call) {
	case "validateDirSwitchBackup", "validateDirSwitchSibling", "validateDirSwitchLink", "validateRetiredAdoptOriginal":
		return true
	default:
		return false
	}
}

func postActionIdentityArgument(call *ast.CallExpr) (ast.Expr, bool) {
	index := postActionExpectedIdentityIndex(calledFunctionName(call))
	if index < 0 || index >= len(call.Args) {
		return nil, false
	}
	return call.Args[index], true
}

func postActionExpectedIdentityIndex(name string) int {
	index := -1
	switch name {
	case "observeRetiredAdoptOriginalWithCapture", "validateCurrentAdoptEntryStatWithCapture", "observeDirSwitchBackup", "observeDirSwitchBackupWithCapture":
		index = 3
	case "observeDirSwitchSibling", "observeDirSwitchSiblingWithCapture":
		index = 4
	case "observeDirSwitchLink", "observeDirSwitchLinkWithCapture":
		index = 3
	case "openDirSwitchDirectory", "openDirSwitchDirectoryObservedWithCapture":
		index = 3
	case "Same", "sameCheckedEntry":
		index = 0
	}
	return index
}

func calledFunctionName(call *ast.CallExpr) string {
	switch function := call.Fun.(type) {
	case *ast.Ident:
		return function.Name
	case *ast.SelectorExpr:
		return function.Sel.Name
	default:
		return ""
	}
}

func namespaceActionCall(call *ast.CallExpr) bool {
	if identifier, ok := call.Fun.(*ast.Ident); ok {
		return identifier.Name == "renameDirSwitchEntry"
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "RenameNoReplaceAt" && selector.Sel.Name != "RetireNameAt" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	return ok && qualifier.Name == "store"
}

func recordedIdentitySelector(selector *ast.SelectorExpr) bool {
	return strings.HasSuffix(selector.Sel.Name, "Identity")
}

func TestAdoptEntryRecaptureDetectorCoversQualifiedPrimitives(t *testing.T) {
	const source = `package p
var unix struct { Fstatat func(int, string) error }
var store struct {
	EntryIdentityAt func(int, string) (int, int, error)
	SnapshotRootOwnedForCopy func(any, string) (int, error)
}
func statAdoptEntry(int, string) (int, error) { return 0, nil }
func f() {
	statAdoptEntry(1, "entry")
	unix.Fstatat(1, "entry")
	store.EntryIdentityAt(1, "entry")
	store.SnapshotRootOwnedForCopy(nil, ".")
}`
	file := parseAndTypeCheckIdentityFixture(t, source)
	var got []bool
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			got = append(got, adoptEntryRecaptureCall(call))
		}
		return true
	})
	want := []bool{true, true, true, false}
	if len(got) != len(want) {
		t.Fatalf("detector results = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("detector result %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func parseAndTypeCheckIdentityFixture(t *testing.T, source string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&types.Config{}).Check("fixture", fset, []*ast.File{file}, nil); err != nil {
		t.Fatalf("fixture must compile: %v", err)
	}
	return file
}

func adoptEntryRecaptureCall(call *ast.CallExpr) bool {
	if call == nil {
		return false
	}
	if identifier, ok := call.Fun.(*ast.Ident); ok {
		return identifier.Name == "statAdoptEntry"
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	return (qualifier.Name == "unix" && selector.Sel.Name == "Fstatat") ||
		(qualifier.Name == "store" && selector.Sel.Name == "EntryIdentityAt")
}

// skipUnlessReplacementDetectable skips a test whose assertion depends on a
// same-name replacement being distinguishable from the original. See the
// store helper of the same name.
func skipUnlessReplacementDetectable(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	supported, err := store.IdentityHandleSupported(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		if testenv.FileHandlesRequired() {
			t.Fatalf("%s does not export file handles; Linux CI must exercise replacement detection", dir)
		}
		t.Skipf("%s does not export file handles; a same-name replacement is not detectable here", dir)
	}
}

func findingFunctionNames(file *ast.File, findings []ast.Node) []string {
	var names []string
	for _, finding := range findings {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Pos() <= finding.Pos() && finding.Pos() <= function.End() {
				names = append(names, function.Name.Name)
			}
		}
	}
	slices.Sort(names)
	return names
}

func TestPostActionDetectorRequiresAnObservationAndTracksThirdResults(t *testing.T) {
	file := parseAndTypeCheckIdentityFixture(t, `package p
 type Identity struct{}
 func renameDirSwitchEntry(...any) error { return nil }
 func openDirSwitchDirectoryObservedWithCapture(any,string,string,Identity,any)(any,any,Identity,error) {return nil,nil,Identity{},nil}
 func observeDirSwitchSibling(any,string,string,any,Identity)(Identity,error) {return Identity{},nil}
 func ensureAdoptOriginalRetired(...any)(Identity,error) {return Identity{},nil}
 func helperResult(parent any,recorded Identity) {
  observed,_:=ensureAdoptOriginalRetired(parent)
  _=renameDirSwitchEntry(parent,"old","new","move")
  _,_=observeDirSwitchSibling(parent,"","new",nil,observed)
 }
 func missing(parent any) { _ = renameDirSwitchEntry(parent,"old","new","move") }
 func thirdResult(parent any, recorded Identity) {
  _,_,observed,_:=openDirSwitchDirectoryObservedWithCapture(parent,"","old",recorded,nil)
  _ = renameDirSwitchEntry(parent,"old","new","move")
  _,_=observeDirSwitchSibling(parent,"","new",nil,observed)
 }
 `)
	got := findingFunctionNames(file, postNamespaceActionRecordIdentities(file))
	if !slices.Equal(got, []string{"missing"}) {
		t.Fatalf("finding functions = %v, want only missing", got)
	}
}

func TestSharedTailDetectorRequiresProofBeforeBackupRemoval(t *testing.T) {
	const prefix = `package p
 type Identity struct{}
 type State struct { SiblingIdentity Identity }
 func renameDirSwitchEntry(...any) error {return nil}
 func observeDirSwitchSibling(any,string,string,*State,Identity)(Identity,error) {return Identity{},nil}
 func removeDirSwitchBackup() error {return nil}
 func completeDirSwitch(parent any,sw *State) error {
  observed,_:=observeDirSwitchSibling(parent,"","old",sw,sw.SiblingIdentity)
  if err:=renameDirSwitchEntry(parent,"old","new","move");err!=nil{return err}
  landed,_:=observeDirSwitchSibling(parent,"","new",sw,observed)
 `
	const proof = `if _,err:=observeDirSwitchSibling(parent,"","new",sw,landed);err!=nil{return err};`
	const weak = `if _,err:=observeDirSwitchSibling(parent,"","new",sw,sw.SiblingIdentity);err!=nil{return err};`
	const remove = `if err:=removeDirSwitchBackup();err!=nil{return err};`
	for _, test := range []struct {
		name, body string
		want       bool
	}{
		{"proof before deletion", proof + remove, true},
		{"proof after deletion", remove + proof, false},
		{"record before deletion and live proof after", weak + remove + proof, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := parseAndTypeCheckIdentityFixture(t, prefix+test.body+"return nil}")
			function := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
			if got := completeDirSwitchTailUsesLiveIdentity(function); got != test.want {
				t.Fatalf("tail proof = %v, want %v", got, test.want)
			}
		})
	}
}

func TestReservationRecoveryPromotesTheAlreadyPublishedObservation(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("current file")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(current), "new_txn.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		condition, ok := branch.Cond.(*ast.Ident)
		if !ok || condition.Name != "privatePresent" || branch.Else == nil {
			return true
		}
		for _, arm := range []ast.Node{branch.Body, branch.Else} {
			captured := false
			ast.Inspect(arm, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok || len(assignment.Rhs) != 1 || len(assignment.Lhs) < 1 {
					return true
				}
				call, ok := assignment.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calledFunctionName(call)
				left, ok := assignment.Lhs[0].(*ast.Ident)
				if ok && left.Name == "manifest" && (name == "PublishStagedRootOwned" || name == "ObserveStagedOwned") {
					captured = true
				}
				return true
			})
			if !captured {
				t.Error("both reservation recovery arms must promote their live manifest before journaling the payload")
			}
		}
		found = true
		return false
	})
	if !found {
		t.Fatal("reservation recovery branch was not inspected")
	}
}

func TestBackupRemovalDetectorRequiresProofBeforeUnlink(t *testing.T) {
	const prefix = `package p
 type Identity struct{}
 func observeDirSwitchBackup(any,any,any,Identity)(Identity,error) {return Identity{},nil}
 var store struct{RenameNoReplaceAt func(...any)error}
 var unix struct{Unlinkat func(...any)error}
 func removeDirSwitchBackup(parent any,recorded Identity) error {
 observed,_:=observeDirSwitchBackup(parent,nil,nil,recorded)
 if err:=store.RenameNoReplaceAt(parent,"old",parent,"retired");err!=nil{return err}
 _=observed
 `
	const proof = `if _,err:=observeDirSwitchBackup(parent,nil,nil,observed);err!=nil{return err};`
	const weak = `if _,err:=observeDirSwitchBackup(parent,nil,nil,recorded);err!=nil{return err};`
	const remove = `if err:=unix.Unlinkat(parent,"retired",0);err!=nil{return err};`
	for _, test := range []struct {
		name, body string
		want       bool
	}{
		{"proof before unlink", proof + remove, true},
		{"proof after unlink", remove + proof, false},
		{"weak before and live after", weak + remove + proof, false},
		{"missing proof", remove, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := parseAndTypeCheckIdentityFixture(t, prefix+test.body+`return nil}`)
			if got := dirSwitchBackupProofPrecedesRemoval(file.Decls[len(file.Decls)-1].(*ast.FuncDecl)); got != test.want {
				t.Fatalf("proof before unlink=%v, want %v", got, test.want)
			}
		})
	}
}

// dirSwitchBackupProofPrecedesRemoval pins the straight-line proof between
// this helper's retirement and unlink. It does not infer branch feasibility.
func dirSwitchBackupProofPrecedesRemoval(function *ast.FuncDecl) bool {
	var action, removal token.Pos
	var proof *ast.CallExpr
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case namespaceActionCall(call):
			action = call.End()
		case calledFunctionName(call) == "observeDirSwitchBackup":
			proof = call
		case calledFunctionName(call) == "Unlinkat":
			if removal == token.NoPos {
				removal = call.Pos()
			}
		}
		return true
	})
	if action == token.NoPos || removal == token.NoPos || proof == nil || proof.Pos() <= action || proof.End() >= removal || len(proof.Args) != 4 {
		return false
	}
	return liveIdentityExpression(proof.Args[3], liveIdentityNamesBefore(function.Body, proof.Pos()))
}

func TestRemoveDirSwitchBackupProvesRetirementBeforeUnlink(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate engine source")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(current), "adopt_switchdir.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "removeDirSwitchBackup" {
			if !dirSwitchBackupProofPrecedesRemoval(fn) {
				t.Fatal("backup identity must be proved after retirement and before unlink")
			}
			return
		}
	}
	t.Fatal("removeDirSwitchBackup was not found")
}

func TestAdoptArchiveAdmissionDoesNotFallThroughIntoReadmission(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate engine source")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(current), "adopt_switch.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"resumeAdoptArchive": true, "resumeRetiredAdoptArchive": true, "resumeCopiedAdoptArchive": true}
	admissions := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !wanted[fn.Name.Name] {
			continue
		}
		delete(wanted, fn.Name.Name)
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if branch, ok := node.(*ast.BranchStmt); ok && branch.Tok == token.FALLTHROUGH {
				t.Error("archive admission and tails must not fall through into another admission")
			}
			if call, ok := node.(*ast.CallExpr); ok && fn.Name.Name != "resumeAdoptArchive" && calledFunctionName(call) == "ensureAdoptOriginalRetired" {
				t.Errorf("%s must retain its supplied identity instead of re-admitting the WAL", fn.Name.Name)
			}
			clause, ok := node.(*ast.CaseClause)
			if !ok || fn.Name.Name != "resumeAdoptArchive" || len(clause.List) != 1 {
				return true
			}
			label, ok := clause.List[0].(*ast.BasicLit)
			if !ok || label.Value != `"planned"` && label.Value != `"retired"` {
				return true
			}
			admissions++
			var capture, hook token.Pos
			var tail *ast.CallExpr
			captures, tails := 0, 0
			body := &ast.BlockStmt{List: clause.Body}
			ast.Inspect(body, func(child ast.Node) bool {
				if selector, ok := child.(*ast.SelectorExpr); ok && selector.Sel.Name == "afterAdoptRetiredJournal" {
					hook = selector.Pos()
				}
				if call, ok := child.(*ast.CallExpr); ok {
					switch calledFunctionName(call) {
					case "ensureAdoptOriginalRetired":
						capture = call.End()
						captures++
					case "resumeRetiredAdoptArchive":
						tail = call
						tails++
					}
				}
				return true
			})
			if captures != 1 || tails != 1 || tail == nil || len(tail.Args) != 8 || !liveIdentityExpression(tail.Args[6], liveIdentityNamesBefore(body, tail.Pos())) {
				t.Errorf("%s admission must pass its single live proof to the retired tail", label.Value)
			}
			if label.Value == `"planned"` && (tail == nil || hook <= capture || hook >= tail.Pos()) {
				t.Error("the retired journal hook must stay after admission and before entering the retired tail")
			}
			return true
		})
	}
	if len(wanted) != 0 || admissions != 2 {
		t.Fatalf("missing functions=%v, admission arms=%d", wanted, admissions)
	}
}
