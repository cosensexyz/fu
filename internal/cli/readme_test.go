package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// readmeVerbatimCliLines are the sentence openings README quotes from this
// package's output word for word.
//
// Every entry is a line some command prints from a constant string, so a
// transcript in README can be checked against the source literal exactly
// rather than approximately. Add an entry when README starts quoting another
// such line; a line that is assembled with %s or a path does not belong here,
// because there is no single literal to compare it to.
//
// The literals these are matched against come from package cli only. A
// message printed by store or engine would never be found and would fail as
// a false red, so widening this list to one of those means widening the
// search below to match.
var readmeVerbatimCliLines = []string{
	"restored agent links",
	"record them with `fu commit`",
	"the store worktree was left alone; these changes are not committed:",
	"these are untracked or ignored, so no restore touches them:",
}

// TestReadmeTranscriptsQuoteRealCliOutput pins README's `$ fu ...` transcripts
// to the strings this package actually emits.
//
// Four documents quote CLI output verbatim and, until this test, nothing
// checked any of them. That is not hypothetical drift: two review rounds
// running found a message that had been corrected in code while its quoted
// transcript kept the old wording, and the second time README's stale copy
// asserted something README itself denied 239 lines later (review 2026-09-03
// round 4, Important). Catching it needs a machine, not a reviewer.
//
// The check is deliberately narrow. It looks only at lines inside a fenced
// block that contains a `$ fu ` prompt, and only at those beginning with one
// of the openings listed above -- each of which a command prints from a
// constant. Such a line must appear verbatim as a string literal somewhere in
// this package. Lines assembled from paths or counts are skipped, since there
// is no literal to hold them to and a fuzzy match would either miss real
// drift or fail on correct text.
//
// It also counts how many lines it actually checked and fails at zero. A
// narrow guard that silently matches nothing is worse than none, because the
// green tick is then evidence for a property no longer being tested: changing
// README's prompt from `$ fu restore` to `% fu restore` retired the whole
// check while leaving a stale line in place, and this test passed (review
// 2026-09-03 round 5, Minor).
func TestReadmeTranscriptsQuoteRealCliOutput(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	literals := stringLiteralsInPackage(t, ".")
	if len(literals) == 0 {
		t.Fatal("no string literals parsed from the package; the check would pass vacuously")
	}

	checked := 0
	inFence, isShell, block := false, false, []string(nil)
	checkBlock := func() {
		if !isShell {
			return
		}
		for _, line := range block {
			line = strings.TrimSpace(line)
			for _, prefix := range readmeVerbatimCliLines {
				if !strings.HasPrefix(line, prefix) {
					continue
				}
				checked++
				if !literals[line] {
					t.Errorf("README quotes a line no string literal in this package emits:\n  README: %q\nEvery `$ fu ...` transcript must match the code that prints it; if the message changed, copy the new one out of the source.", line)
				}
			}
		}
	}
	for _, line := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(line, "```") {
			if inFence {
				checkBlock()
				inFence, isShell, block = false, false, nil
			} else {
				inFence = true
			}
			continue
		}
		if !inFence {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "$ fu ") {
			isShell = true
			continue
		}
		block = append(block, line)
	}

	if checked == 0 {
		t.Fatalf("this guard examined no README line at all, so its passing means nothing.\nEither README stopped quoting the messages in readmeVerbatimCliLines, or its `$ fu ` prompts changed shape; fix whichever it is rather than deleting this check.")
	}
}

// stringLiteralsInPackage returns every string literal in the package's
// non-test sources, so a README line can be compared against what the code
// actually contains rather than against a second hand-maintained copy.
//
// Files are listed and parsed one at a time rather than through
// parser.ParseDir, which has been deprecated since Go 1.22 (review 2026-09-03
// round 5, Minor).
func stringLiteralsInPackage(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	literals := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(lit.Value); err == nil {
				literals[value] = true
			}
			return true
		})
	}
	return literals
}
