//go:build darwin

package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	darwinFoldOracleOnce sync.Once
	darwinFoldOracleData []string
)

// TestFolderNameCoversDarwinFilesystemEquivalence asks the filesystem for the
// equivalence classes instead of duplicating its Unicode rules in the test.
// O_CREATE|O_EXCL returns EEXIST when a later spelling names an earlier file;
// that file contains the fold key assigned to the earlier spelling. Therefore
// every EEXIST must recover the same key for the later spelling.
//
// The candidate corpus is generated from Unicode data rather than a list of
// known regressions. Canonically decomposable scalars exercise singleton and
// multi-rune normalization. Every cased scalar is also paired with every rune
// that participates in a canonical decomposition, which covers case folding
// followed by composition, including supplementary-plane starters.
//
// This intentionally checks only the security-relevant lower bound:
//
//	filesystem-equivalent => folder.name-equivalent
//
// Extra merging by folder.name safely refuses content that could otherwise be
// installed, while missing a filesystem equivalence can permit link escape.
func TestFolderNameCoversDarwinFilesystemEquivalence(t *testing.T) {
	if testing.Short() {
		t.Skip("filesystem Unicode oracle is exhaustive")
	}

	candidates := darwinFoldOracleCandidates()
	dir := t.TempDir()
	fold := newFolder()
	checkedAliases := 0
	for _, name := range candidates {
		key := fold.name(name)
		filename := filepath.Join(dir, name)
		file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, err := file.WriteString(key); err != nil {
				_ = file.Close()
				t.Fatalf("write oracle marker for %U: %v", []rune(name), err)
			}
			if err := file.Close(); err != nil {
				t.Fatalf("close oracle marker for %U: %v", []rune(name), err)
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			// APFS refuses a few Unicode noncharacters as path components. They
			// cannot participate in a name-resolution escape on this filesystem.
			continue
		}
		checkedAliases++
		priorKey, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read oracle marker for alias %U: %v", []rune(name), err)
		}
		if string(priorKey) != key {
			t.Fatalf("filesystem-equivalent name %U has fold key %U, want existing class key %U",
				[]rune(name), []rune(key), []rune(string(priorKey)))
		}
	}
	if checkedAliases < 100000 {
		t.Fatalf("filesystem oracle checked only %d equivalent spellings, want at least 100000", checkedAliases)
	}
}

func TestValidateLinksAcceptedTreesCannotEscapeDarwinRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("filesystem Unicode validator oracle is exhaustive")
	}
	const iterations = 2000
	pairs := darwinFilesystemAliasPairs(t, iterations)
	base := t.TempDir()
	for i, pair := range pairs {
		root := filepath.Join(base, fmt.Sprintf("root-%04d", i))
		outsideName := fmt.Sprintf("outside-%04d", i)
		outside := filepath.Join(base, outsideName)
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(".", filepath.Join(root, pair[0])); err != nil {
			t.Fatal(err)
		}
		target := pair[1] + "/../" + outsideName
		if err := os.Symlink(target, filepath.Join(root, "probe")); err != nil {
			t.Fatal(err)
		}
		entries := []ManifestEntry{
			{Path: pair[0], Mode: fs.ModeSymlink, Target: "."},
			{Path: "probe", Mode: fs.ModeSymlink, Target: target},
		}
		if err := ValidateLinks(entries); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(filepath.Join(root, "probe"))
			if resolveErr != nil {
				t.Fatalf("validator accepted alias pair %U/%U but kernel resolution failed: %v", []rune(pair[0]), []rune(pair[1]), resolveErr)
			}
			rel, relErr := filepath.Rel(root, resolved)
			if relErr != nil {
				t.Fatal(relErr)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				t.Fatalf("validator accepted tree whose probe escapes through Darwin alias %U/%U to %s", []rune(pair[0]), []rune(pair[1]), resolved)
			}
		}
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(outside); err != nil {
			t.Fatal(err)
		}
	}
}

func darwinFilesystemAliasPairs(t *testing.T, limit int) [][2]string {
	t.Helper()
	candidates := darwinFoldOracleCandidates()
	rand.New(rand.NewSource(1)).Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	dir := t.TempDir()
	pairs := make([][2]string, 0, limit)
	for _, name := range candidates {
		filename := filepath.Join(dir, name)
		file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, err := file.WriteString(name); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			continue
		}
		prior, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if string(prior) == name {
			continue
		}
		pairs = append(pairs, [2]string{string(prior), name})
		if len(pairs) == limit {
			return pairs
		}
	}
	t.Fatalf("filesystem corpus yielded %d alias pairs, want %d", len(pairs), limit)
	return nil
}

func darwinFoldOracleCandidates() []string {
	darwinFoldOracleOnce.Do(func() {
		darwinFoldOracleData = buildDarwinFoldOracleCandidates()
	})
	return append([]string(nil), darwinFoldOracleData...)
}

func buildDarwinFoldOracleCandidates() []string {
	seen := make(map[string]struct{})
	candidates := make([]string, 0, 200000)
	add := func(name string) {
		if name == "" || len(name) > 255 || !utf8.ValidString(name) {
			return
		}
		for _, r := range name {
			if r == 0 || r == '/' {
				return
			}
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		candidates = append(candidates, name)
	}

	compositionRunes := make(map[rune]struct{})
	cased := make([]rune, 0, 5000)
	caseFold := cases.Fold()
	for r := rune(1); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		raw := string(r)
		decomposed := norm.NFD.String(raw)
		if decomposed != raw {
			add("n" + raw + "n")
			add("n" + decomposed + "n")
			parts := []rune(decomposed)
			for _, part := range parts[1:] {
				if unicode.Is(unicode.Mn, part) {
					compositionRunes[part] = struct{}{}
				}
			}
		}
		if unicode.SimpleFold(r) != r || caseFold.String(raw) != raw {
			cased = append(cased, r)
		}
	}

	marks := make([]rune, 0, len(compositionRunes))
	for r := range compositionRunes {
		marks = append(marks, r)
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i] < marks[j] })
	for _, base := range cased {
		for _, mark := range marks {
			add("c" + string(base) + string(mark) + "c")
		}
	}
	return candidates
}
