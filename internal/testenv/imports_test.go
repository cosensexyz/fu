package testenv

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The package doc promises that production code never imports testenv, and
// internal/cli's architecture guard cannot enforce that: it exempts the core
// packages from its dependency rule, so internal/store or internal/engine
// importing this package would pass it unnoticed. This guard walks every
// production Go file in the module instead. It matters more now that the
// package creates and removes entries in a caller-chosen directory.
func TestProductionCodeDoesNotImportTestenv(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate repository root")
	}
	root := filepath.Join(filepath.Dir(currentFile), "..", "..")
	const self = "github.com/cosensexyz/fu/internal/testenv"
	inspected := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			// A documentation snippet with a .go suffix; syntax is the
			// build's concern, as in internal/cli's guard.
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		inspected[rel] = true
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if imported == self {
				t.Errorf("%s imports %s; testenv is test support only", rel, self)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A guard that inspects nothing proves nothing.
	for _, want := range []string{"internal/store/store.go", "internal/engine/application.go", "cmd/fu/main.go"} {
		if !inspected[want] {
			t.Fatalf("the guard did not inspect %s; inspected %d files", want, len(inspected))
		}
	}
}
