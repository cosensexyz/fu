package cli

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestArchitectureImportsIgnoreNonGoSnippets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.go")
	if err := os.WriteFile(path, []byte("this is documentation, not a Go package\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	imports, parsed, err := architectureImports(path)
	if err != nil || parsed || len(imports) != 0 {
		t.Fatalf("non-Go snippet = imports %v parsed %v err %v; want ignored", imports, parsed, err)
	}
}

// corePackages are engine and the packages engine is built on. The dependency
// rule constrains what sits *above* that boundary, so these are exempt from it.
var corePackages = map[string]bool{
	"internal/engine": true,
	"internal/agent":  true,
	"internal/skill":  true,
	"internal/source": true,
	"internal/store":  true,
}

func shouldSkipArchitectureDir(name string) bool {
	switch name {
	case ".git", ".worktrees", "testdata", "vendor":
		return true
	default:
		return false
	}
}

// architectureImports distinguishes a compilable Go source file from a
// documentation snippet that merely has a .go suffix. Syntax is verified by
// the normal build; this guard owns only valid production import boundaries.
func architectureImports(path string) ([]string, bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, false, nil
	}
	imports := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, true, err
		}
		imports = append(imports, imported)
	}
	return imports, true, nil
}

func TestArchitectureGuardDoesNotSkipDocsDirectories(t *testing.T) {
	if shouldSkipArchitectureDir("docs") {
		t.Fatal("a docs directory can contain Go production code and must be inspected")
	}
	for _, name := range []string{".git", ".worktrees", "testdata", "vendor"} {
		if !shouldSkipArchitectureDir(name) {
			t.Fatalf("architecture walk must skip conventional non-production directory %q", name)
		}
	}
}

func TestArchitectureGuardInspectsNewProductionDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "future", "pkg", "new.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package future\n\nimport _ \"github.com/cosensexyz/fu/internal/cli\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := architectureProductionFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.ToSlash(files[0].path) != "future/pkg/new.go" {
		t.Fatalf("checked production files = %+v, want future/pkg/new.go", files)
	}
}

type architectureProductionFile struct {
	path    string
	imports []string
}

func architectureProductionFiles(root string) ([]architectureProductionFile, error) {
	var files []architectureProductionFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if shouldSkipArchitectureDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if corePackages[filepath.ToSlash(filepath.Dir(rel))] {
			return nil
		}
		imports, parsed, err := architectureImports(path)
		if err != nil {
			return err
		}
		if parsed {
			files = append(files, architectureProductionFile{path: rel, imports: imports})
		}
		return nil
	})
	return files, err
}

// TestProductionCLIUsesOnlyApplicationCoreBoundary enforces DESIGN §1's
// dependency rule by AST over every production file outside the core, so
// aliased and dot imports are covered by construction.
//
// It walks the whole module rather than internal/cli's own directory (round 18
// finding M24). The narrower version left cmd/fu unchecked -- it happens to
// import only internal/cli, but nothing made that so -- and its entry.IsDir()
// guard silently skipped any future subpackage, so the guard's scope was
// narrower than the claim it enforces.
func TestProductionCLIUsesOnlyApplicationCoreBoundary(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := architectureProductionFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := make(map[string]bool, len(files))
	for _, file := range files {
		checked[file.path] = true
		for _, imported := range file.imports {
			if !strings.HasPrefix(imported, "github.com/cosensexyz/fu/internal/") {
				continue
			}
			short := strings.TrimPrefix(imported, "github.com/cosensexyz/fu/")
			if short == "internal/cli" || short == "internal/engine" {
				continue
			}
			t.Errorf("%s imports %s; presentation code must use engine.Application as its only core boundary", file.path, imported)
		}
	}
	// A guard that silently inspects nothing is worse than no guard at all --
	// which is exactly how the entry.IsDir() skip went unnoticed.
	if len(files) == 0 {
		t.Fatal("the architecture guard inspected no production files")
	}
	if !checked["cmd/fu/main.go"] {
		t.Fatalf("the architecture guard did not inspect cmd/fu/main.go; checked %v", checked)
	}
}
