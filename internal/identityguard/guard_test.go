package identityguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func TestFirstFindingRecognizesCompilingIdentityConstruction(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   Kind
	}{
		{"inode selector", `package p; func f(s struct{ Ino uint64 }) { _ = s.Ino }`, InodeRead},
		{"device selector", `package p; func f(s struct{ Dev uint64 }) { _ = s.Dev }`, InodeRead},
		{"handle assignment", `package p; type FileIdentity struct{ Handle string }; func f(id FileIdentity) { id.Handle = "" }`, HandleAssignment},
		{"device assignment", `package p; type FileIdentity struct{ Device uint64 }; func f() { var id FileIdentity; id.Device = 1 }`, IdentityFieldAssignment},
		{"device increment", `package p; type FileIdentity struct{ Device uint64 }; func f() { var id FileIdentity; id.Device++ }`, IdentityFieldAssignment},
		{"device address", `package p; type FileIdentity struct{ Device uint64 }; func take(*uint64) {}; func f() { var id FileIdentity; take(&id.Device) }`, IdentityFieldAssignment},
		{"new identity assignment", `package p; type FileIdentity struct{ Inode uint64 }; func f() { id := new(FileIdentity); id.Inode = 1 }`, IdentityFieldAssignment},
		{"identity literal", `package p; type FileIdentity struct{ Device uint64 }; var id = FileIdentity{Device: 1}`, IdentityLiteral},
		{"slice elision", `package p; type FileIdentity struct{ Device uint64 }; var ids = []FileIdentity{{Device: 1}}`, IdentityLiteral},
		{"pointer slice elision", `package p; type FileIdentity struct{ Device, Inode uint64 }; var ids = []*FileIdentity{{Device: 1, Inode: 2}}`, IdentityLiteral},
		{"pointer map elision", `package p; type FileIdentity struct{ Device, Inode uint64 }; var ids = map[string]*FileIdentity{"a": {Device: 1, Inode: 2}}`, IdentityLiteral},
		{"nested slice elision", `package p; type FileIdentity struct{ Device uint64 }; var ids = [][]FileIdentity{{{Device: 1}}}`, IdentityLiteral},
		{"nested map slice elision", `package p; type FileIdentity struct{ Device uint64 }; var ids = map[string][]FileIdentity{"a": {{Device: 1}}}`, IdentityLiteral},
		{"named slice elision", `package p; type FileIdentity struct{ Device uint64 }; type List []FileIdentity; var ids = List{{Device: 1}}`, IdentityLiteral},
		{"map key elision", `package p; type FileIdentity struct{ Device uint64 }; var ids = map[FileIdentity]bool{{Device: 1}: true}`, IdentityLiteral},
		{"generic named slice elision", `package p; type FileIdentity struct{ Device uint64 }; type Box[T any] []T; var ids = Box[FileIdentity]{{Device: 1}}`, IdentityLiteral},
		{"identity alias literal", `package p; type FileIdentity struct{ Device uint64 }; type Alias = FileIdentity; var id = Alias{Device: 1}`, IdentityLiteral},
		{"identity alias slice elision", `package p; type FileIdentity struct{ Device uint64 }; type Alias = FileIdentity; var ids = []Alias{{Device: 1}}`, IdentityLiteral},
		{"identity struct conversion", `package p; type FileIdentity struct{ Device, Inode uint64; Handle string }; type weak struct{ Device, Inode uint64; Handle string }; var id = FileIdentity(weak{Device: 1, Inode: 2})`, IdentityLiteral},
		{"identity pointer conversion", `package p; type FileIdentity struct{ Device, Inode uint64; Handle string }; type weak struct{ Device, Inode uint64; Handle string }; var source *weak; var id = (*FileIdentity)(source)`, IdentityLiteral},
		{"empty identity literal", `package p; type FileIdentity struct{ Device uint64 }; var id = FileIdentity{}`, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("fixture", fset, []*ast.File{file}, nil); err != nil {
				t.Fatalf("fixture must compile: %v", err)
			}
			if got := FirstFinding(file); got != test.want {
				t.Fatalf("finding = %q, want %q", got, test.want)
			}
		})
	}
}

func TestIdentityWritesInRangeClauses(t *testing.T) {
	for _, test := range []struct {
		source string
		want   Kind
	}{
		{"for id.Inode = range 7 {}", IdentityFieldAssignment},
		{"for _, id.Device = range inodes {}", IdentityFieldAssignment},
		{"for _, id.Handle = range handles {}", HandleAssignment},
		{"for _, h := range handles { _ = h }", ""},
	} {
		t.Run(test.source, func(t *testing.T) {
			file := compilingCrossFileFixture(t, "func f(id *FileIdentity, inodes []uint64, handles []string) {"+test.source+"}")
			if got := FirstFinding(file); got != test.want {
				t.Fatalf("finding = %q, want %q", got, test.want)
			}
		})
	}
}

func TestIdentityTypeParameterConstruction(t *testing.T) {
	for _, test := range []struct {
		source string
		want   Kind
	}{
		{"func build[T FileIdentity](d uint64) T { return T{Device:d} }", IdentityLiteral},
		{"func build[T FileIdentity](d uint64) []T { return []T{{Device:d}} }", IdentityLiteral},
		{"func build[T FileIdentity](d uint64) map[string]T { return map[string]T{\"id\":{Device:d}} }", IdentityLiteral},
		{"func build[T FileIdentity]() T { return T{} }", ""},
		{"func allow[T FileIdentity]() {} ; func build[T Other](d uint64) T { return T{Device:d} }", ""},
	} {
		t.Run(test.source, func(t *testing.T) {
			file := compilingCrossFileFixture(t, test.source)
			if got := FirstFinding(file); got != test.want {
				t.Fatalf("finding = %q, want %q", got, test.want)
			}
		})
	}
}

func compilingCrossFileFixture(t *testing.T, source string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", "package p; "+source, 0)
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := parser.ParseFile(fset, "identity.go", "package p; type FileIdentity struct { Device, Inode uint64; Handle string }; type Other struct { Device uint64 }", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&types.Config{}).Check("fixture", fset, []*ast.File{file, declarations}, nil); err != nil {
		t.Fatalf("fixture must compile: %v", err)
	}
	return file
}

func TestAllowedFunctionDoesNotAllowAMethodWithTheSameName(t *testing.T) {
	file := compilingCrossFileFixture(t, "type receiver struct{}; func allowed() { var id FileIdentity; id.Handle = \"\" }; func (receiver) allowed() { var id FileIdentity; id.Handle = \"\" }")
	findings := Findings(file)
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want two writes", findings)
	}
	allow := map[string]bool{"allowed": true}
	if !InsideAllowedFunction(file, findings[0].Pos, allow) {
		t.Fatal("standalone function must be allowed")
	}
	if InsideAllowedFunction(file, findings[1].Pos, allow) {
		t.Fatal("method must not inherit the standalone function allowance")
	}
}

func TestGenericIdentityAliasesAndExplicitIdentityConstraints(t *testing.T) {
	for _, test := range []struct {
		name, source string
		want         Kind
	}{
		{"alias", `type Alias[T any] = T; func build(d uint64) Alias[FileIdentity] {return Alias[FileIdentity]{Device:d}}`, IdentityLiteral},
		{"multiple arguments", `type Alias[Unused, T any] = T; func build(d uint64) Alias[int,FileIdentity] {return Alias[int,FileIdentity]{Device:d}}`, IdentityLiteral},
		{"elided alias", `type Alias[T any] = T; func build(d uint64) []Alias[FileIdentity] {return []Alias[FileIdentity]{{Device:d}}}`, IdentityLiteral},
		{"alias conversion", `type Alias[T any] = T; func build(id FileIdentity) Alias[FileIdentity] {return Alias[FileIdentity](id)}`, IdentityLiteral},
		{"interface", `func build[T interface{FileIdentity}](d uint64) T {return T{Device:d}}`, IdentityLiteral},
		{"intersection", `func build[T interface{comparable;FileIdentity}](d uint64) T {return T{Device:d}}`, IdentityLiteral},
		{"reversed intersection", `func build[T interface{FileIdentity;comparable}](d uint64) T {return T{Device:d}}`, IdentityLiteral},
		{"structural constraint outside scope", `func build[T interface{comparable;~struct{Device uint64}}](d uint64) T {return T{Device:d}}`, ""},
		{"elided interface", `func build[T interface{FileIdentity}](d uint64) []T {return []T{{Device:d}}}`, IdentityLiteral},
		{"recursive generic pointer", `type Link[T any] *Link[T]; func build() Link[int] {return Link[int](nil)}`, ""},
		{"mutually recursive generic pointers", `type Left[T any] *Right[T]; type Right[T any] *Left[T]; func build() Left[int] {return Left[int](nil)}`, ""},
		{"nested identity alias", `type Alias[T any] = T; func build(id FileIdentity) Alias[Alias[FileIdentity]] {return Alias[Alias[FileIdentity]](id)}`, IdentityLiteral},
		{"other alias", `type Alias[T any] = T; func build(d uint64) Alias[Other] {return Alias[Other]{Device:d}}`, ""},
		{"empty alias", `type Alias[T any] = T; func build() Alias[FileIdentity] {return Alias[FileIdentity]{}}`, ""},
		{"other constraint scope", `func allow[T interface{FileIdentity}]() {}; func build[T interface{Other}](d uint64) T {return T{Device:d}}`, ""},
	} {
		for _, sameFile := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same-file=%v", test.name, sameFile), func(t *testing.T) {
				var file *ast.File
				if sameFile {
					fset := token.NewFileSet()
					var err error
					file, err = parser.ParseFile(fset, "fixture.go", "package p; type FileIdentity struct{Device,Inode uint64; Handle string}; type Other struct{Device uint64}; "+test.source, 0)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := (&types.Config{}).Check("fixture", fset, []*ast.File{file}, nil); err != nil {
						t.Fatalf("fixture must compile: %v", err)
					}
				} else {
					file = compilingCrossFileFixture(t, test.source)
				}
				if got := FirstFinding(file); got != test.want {
					t.Fatalf("finding=%q, want %q", got, test.want)
				}
			})
		}
	}
}

func TestParenthesizedIdentityFieldWrites(t *testing.T) {
	for _, test := range []struct {
		body string
		want Kind
	}{
		{`(id.Handle)=""`, HandleAssignment},
		{`((id.Inode))=i`, IdentityFieldAssignment},
		{`(id.Device)++`, IdentityFieldAssignment},
		{`take(&(id.Device))`, IdentityFieldAssignment},
		{`for _,(id.Handle)=range hs {}`, HandleAssignment},
	} {
		t.Run(test.body, func(t *testing.T) {
			file := compilingCrossFileFixture(t, `func take(*uint64){}; func f(id FileIdentity,i uint64,hs []string){`+test.body+`}`)
			if got := FirstFinding(file); got != test.want {
				t.Fatalf("finding=%q, want %q", got, test.want)
			}
		})
	}
}

func TestLocalIdentityTypesRespectDeclarationScopes(t *testing.T) {
	for _, test := range []struct {
		name, source string
		want         int
	}{
		{"alias", `func f(d uint64) FileIdentity {type A=FileIdentity;return /* identity */A{Device:d}}`, 1},
		{"slice", `func f(d uint64) []FileIdentity {type L []FileIdentity;return L{/* identity */{Device:d}}}`, 1},
		{"map", `func f(d uint64){type M map[string]FileIdentity;_=M{"a":/* identity */{Device:d}}}`, 1},
		{"nested shadow exits", `type A=Other; func f(d uint64){{type A=FileIdentity;_=/* identity */A{Device:d}};_=A{Device:d}}`, 1},
		{"declaration starts scope", `type A=FileIdentity;func f(d uint64){_=/* identity */A{Device:d};type A=Other;_=A{Device:d}}`, 1},
		{"switch arms", `func f(b bool,d uint64){switch b {case true:type A=FileIdentity;_=/* identity */A{Device:d};case false:type A=Other;_=A{Device:d}}}`, 1},
		{"select arms", `func f(ch chan bool,d uint64){select {case <-ch:type A=FileIdentity;_=/* identity */A{Device:d};default:type A=Other;_=A{Device:d}}}`, 1},
		{"different functions", `func f(d uint64){type A=FileIdentity;_=/* identity */A{Device:d}};func g(d uint64){type A=Other;_=A{Device:d}}`, 1},
		{"alias retains declaration binding", `type A=FileIdentity;func f(d uint64){type B=A;type A=Other;_=/* identity */B{Device:d};_=A{Device:d}}`, 1},
		{"package alias retains binding", `type A=FileIdentity;type B=A;func f(d uint64){type A=Other;_=/* identity */B{Device:d};_=A{Device:d}}`, 1},
		{"slice alias keeps distinct bindings", `type A []FileIdentity;type B=A;func f(){type A=B;_=A{/* identity */{Device:1}}}`, 1},
		{"map alias keeps distinct bindings", `type A map[string]FileIdentity;type B=A;func f(){type A=B;_=A{"a":/* identity */{Device:1}}}`, 1},
		{"same name with distinct declaration bindings", `type A=FileIdentity;type B=A;func f(){type A=B;_=/* identity */A{Device:1}}`, 1},
		{"generic arguments retain bindings", `type Matching struct{Device,Inode uint64;Handle string};type X[T any]=T;type A=FileIdentity;type B=*X[A];func f(){type A=B;_=/* identity */X[A](&Matching{Device:1})}`, 1},
		{"recursive local pointer", `func f(){type A *A;_=A(nil)}`, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := compilingCrossFileFixture(t, test.source)
			got := Findings(file)
			if len(got) != test.want {
				t.Fatalf("findings=%v, want %d", got, test.want)
			}
			if test.want == 1 {
				const marker = "/* identity */"
				at := strings.Index(test.source, marker)
				if at < 0 {
					t.Fatal("fixture must mark the exact forbidden construction")
				}
				expected := file.Pos() + token.Pos(len("package p; ")+at+len(marker))
				if got[0].Kind != IdentityLiteral || got[0].Pos != expected {
					t.Fatalf("finding=%+v, want identity literal at %d", got[0], expected)
				}
			}
		})
	}
}
