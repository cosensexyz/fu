package skill

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// TestValidateLinksAcceptsRevisitedLink pins round 18 finding M1. The visited
// set was keyed on the link's path alone, so a resolution that legitimately
// traverses the same link twice was reported as a cycle. Traced by hand and
// confirmed against the kernel: with m -> n (a directory), n/x -> ../m/y and
// n/y a file, the link a -> m/x resolves to n/y, fully in-root -- but
// resolveLinkPath revisits m and refused. maxLinkResolution already terminates
// genuine cycles on its own, so the visited set bought nothing but false
// rejections.
func TestValidateLinksAcceptsRevisitedLink(t *testing.T) {
	entries := []ManifestEntry{
		{Path: "m", Mode: fs.ModeSymlink, Target: "n"},
		{Path: "n", Mode: fs.ModeDir | 0o755},
		{Path: "n/x", Mode: fs.ModeSymlink, Target: "../m/y"},
		{Path: "n/y", Mode: 0o644},
		{Path: "a", Mode: fs.ModeSymlink, Target: "m/x"},
	}
	if err := ValidateLinks(entries); err != nil {
		t.Fatalf("an acyclic layout that revisits a link must be accepted: %v", err)
	}
}

// TestValidateLinksStillRejectsGenuineCycle keeps the property the visited set
// was there for: a real loop must still be refused, now by maxLinkResolution.
func TestValidateLinksStillRejectsGenuineCycle(t *testing.T) {
	entries := []ManifestEntry{
		{Path: "a", Mode: fs.ModeSymlink, Target: "b"},
		{Path: "b", Mode: fs.ModeSymlink, Target: "a"},
	}
	if err := ValidateLinks(entries); err == nil {
		t.Fatal("a genuine symlink cycle must be refused")
	}
}

// TestValidateLinksRefusesSplicedAbsoluteTarget pins round 18 finding M2. An
// absolute target spliced in from an in-tree link produced components
// ["", "etc"], and the empty-component arm skipped the leading "" silently, so
// /etc resolved as if it were etc. It is unreachable today only because a
// separate check in a different loop rejects every absolute-target link
// outright; the splice must refuse it on its own rather than depend on that.
func TestValidateLinksRefusesSplicedAbsoluteTarget(t *testing.T) {
	err := resolveLinkPathForTest("a", "b/c", []ManifestEntry{{Path: "b", Mode: fs.ModeSymlink, Target: "/etc"}})
	if err == nil {
		t.Fatal("an absolute target reached through a splice must be refused")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("the refusal must name the cause: %v", err)
	}
}

// adversarialLinkTree builds the shape that made the string-based resolver
// quadratic: a chain of maxLinkResolution links whose targets are padded to
// the kernel's path limit with "z/" components, plus links pointing at the
// head of that chain so each one performs the full number of splices. The
// tree is accepted, so nothing about it is caught by a "rejects fast"
// intuition -- it is the resolved position, not the verdict, that grows.
func adversarialLinkTree(fanIn int) []ManifestEntry {
	const pad = 4096 // PATH_MAX on Linux; targets are padded to about this
	filler := strings.Repeat("z/", pad/2)
	entries := []ManifestEntry{{Path: "z", Mode: fs.ModeDir | 0o755}}
	for i := 0; i < maxLinkResolution; i++ {
		entries = append(entries, ManifestEntry{
			Path:   fmt.Sprintf("l%d", i),
			Mode:   fs.ModeSymlink,
			Target: filler + fmt.Sprintf("l%d", i+1),
		})
	}
	entries = append(entries, ManifestEntry{Path: fmt.Sprintf("l%d", maxLinkResolution), Mode: 0o644})
	for i := 0; i < fanIn; i++ {
		entries = append(entries, ManifestEntry{
			Path:   fmt.Sprintf("fan%d", i),
			Mode:   fs.ModeSymlink,
			Target: "l0",
		})
	}
	return entries
}

// adversarialLinkTreeAt is adversarialLinkTree parameterized by the depth the
// links are registered at -- the parameter the flat-only generator was
// missing, and the one the attacker controls.
//
// Everything else is held fixed: the same chain, the same fan-in, the same
// per-target component count. Only the registered depth differs, so comparing
// two of these isolates depth-dependence from every other cost.
func adversarialLinkTreeAt(fanIn, prefixDepth int) []ManifestEntry {
	const pad = 4096 // PATH_MAX on Linux
	prefix := strings.Repeat("d/", prefixDepth)
	// "z/../" pairs rather than plain "z/": the position keeps returning to the
	// depth the link paths occupy, which is what defeats a depth-indexed
	// shortcut.
	filler := strings.Repeat("z/../", pad/5)
	entries := []ManifestEntry{{Path: "d", Mode: fs.ModeDir | 0o755}}
	for i := 0; i < maxLinkResolution; i++ {
		entries = append(entries, ManifestEntry{
			Path:   prefix + fmt.Sprintf("l%d", i),
			Mode:   fs.ModeSymlink,
			Target: filler + fmt.Sprintf("l%d", i+1),
		})
	}
	entries = append(entries, ManifestEntry{Path: prefix + fmt.Sprintf("l%d", maxLinkResolution), Mode: 0o644})
	for i := 0; i < fanIn; i++ {
		entries = append(entries, ManifestEntry{
			Path:   prefix + fmt.Sprintf("fan%d", i),
			Mode:   fs.ModeSymlink,
			Target: "l0",
		})
	}
	return entries
}

// spliceHeavyLinkTree puts the next link first in every target. A resolver
// that prepends each newly expanded target to one flat pending slice therefore
// recopies the accumulated suffix at every hop; the frame-stack resolver keeps
// each target separate and visits every component once.
func spliceHeavyLinkTree(hops, fanIn int) []ManifestEntry {
	const fillerComponents = 320
	filler := strings.Repeat("z/", fillerComponents)
	entries := []ManifestEntry{{Path: "z", Mode: fs.ModeDir | 0o755}}
	for i := 0; i < hops; i++ {
		entries = append(entries, ManifestEntry{
			Path:   fmt.Sprintf("l%d", i),
			Mode:   fs.ModeSymlink,
			Target: fmt.Sprintf("l%d/", i+1) + filler,
		})
	}
	entries = append(entries, ManifestEntry{Path: fmt.Sprintf("l%d", hops), Mode: 0o644})
	for i := 0; i < fanIn; i++ {
		entries = append(entries, ManifestEntry{
			Path:   fmt.Sprintf("fan%d", i),
			Mode:   fs.ModeSymlink,
			Target: "l0",
		})
	}
	return entries
}

func TestValidateLinksSpliceAllocationsScaleLinearly(t *testing.T) {
	const fanIn = 24
	measure := func(hops int) int64 {
		t.Helper()
		entries := spliceHeavyLinkTree(hops, fanIn)
		result := testing.Benchmark(func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := validateLinksWithBudget(entries, 16*maxLinkComponents); err != nil {
					b.Fatal(err)
				}
			}
		})
		return result.AllocedBytesPerOp()
	}
	short, long := measure(10), measure(40)
	// Four times as many hops should remain near four times the allocation.
	// The old prepend-splice resolver recopied its growing pending suffix and
	// crossed this deliberately generous 8x boundary.
	if long > 8*short {
		t.Fatalf("splice allocations grew superlinearly: 10 hops=%d B/op, 40 hops=%d B/op (%.1fx)",
			short, long, float64(long)/float64(short))
	}
}

// TestValidateLinksCostIsIndependentOfDepth pins the property, not a wall-clock
// number. Resolution must cost the same whether the links sit at the root or
// 380 directories down, because the attacker writes the link paths and an
// absolute bound is not a statement about the algorithm.
//
// The earlier version of this test measured only the flat shape against a 5 s
// bound, which is why it ran in 0.01 s while the identical entry count arranged
// deep took 3.9 s and a 2.9 MB git source took 2 m 49 s end to end -- ~40 s of
// it inside fu.lock. Rebuilding the resolved position as a string once per
// component was O(depth); skipping that lookup at depths no link path has did
// not help, because the padded chain can simply be registered at the depth
// being walked.
//
// A ratio is also the only form that survives `make check`: the race detector
// multiplies both sides by roughly the same 7×, so an absolute bound would
// either flake there or be too loose to catch anything. Measured ratios are
// 1.00-1.12 with the trie; the whole-path map put them two orders of magnitude
// apart.
func TestValidateLinksCostIsIndependentOfDepth(t *testing.T) {
	const fanIn = 40
	measure := func(entries []ManifestEntry) time.Duration {
		t.Helper()
		start := time.Now()
		// This benchmark isolates the resolver's depth complexity. Production
		// uses the smaller shared aggregate budget; a deliberately larger
		// shared budget keeps this accepted input exercising the whole walk.
		if err := validateLinksWithBudget(entries, 16*maxLinkComponents); err != nil {
			t.Fatalf("the adversarial tree is in-root and must be accepted under the benchmark budget: %v", err)
		}
		return time.Since(start)
	}
	flat := measure(adversarialLinkTreeAt(fanIn, 0))
	deep := measure(adversarialLinkTreeAt(fanIn, 380))
	// Generous: the observed ratio is ~1, and anything that reintroduces a
	// per-component cost proportional to depth lands far above 5.
	if deep > 5*flat {
		t.Fatalf("resolution must not scale with the depth links are registered at: flat %s, deep %s (%.1fx)",
			flat, deep, float64(deep)/float64(flat))
	}
}

// TestValidateLinksStaysBoundedOnAdversarialInput keeps a plain absolute check
// on the flat shape alongside the ratio above. It is deliberately loose enough
// for the race detector; its job is to catch a total blow-up, while
// TestValidateLinksCostIsIndependentOfDepth catches the shape of one.
func TestValidateLinksStaysBoundedOnAdversarialInput(t *testing.T) {
	entries := adversarialLinkTree(140)
	start := time.Now()
	if err := validateLinksWithBudget(entries, 16*maxLinkComponents); err != nil {
		t.Fatalf("the adversarial tree is in-root and must be accepted under the benchmark budget: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("resolution must stay bounded on adversarial input, took %s", elapsed)
	}
}

// TestValidateLinksRefusesInexactNameMatch is the case-insensitivity escape.
// macOS's default APFS volume -- an explicitly supported v1 platform -- matches
// names case-insensitively and Unicode-normalization-insensitively, while the
// link index matched byte-exactly. A target component naming a link in a
// different case therefore missed the index, so fu counted it as an ordinary
// directory component and pushed a level, while the kernel followed the link
// and stayed put. Each such component buys the attacker one level of depth
// slack, and slack composes: `X -> .` with `x/x/x/x/../../../../.ssh/id_rsa`
// was accepted, installed, and read the user's private key through the agent's
// skills directory at exit 0.
//
// Folding the lookup instead of refusing would be unsafe in the other
// direction, so an inexact match is a refusal on both platforms.
func TestValidateLinksRefusesInexactNameMatch(t *testing.T) {
	cases := map[string][]ManifestEntry{
		"case variant reaches outside the root": {
			{Path: "SKILL.md", Mode: 0o644},
			{Path: "X", Mode: fs.ModeSymlink, Target: "."},
			{Path: "leak", Mode: fs.ModeSymlink, Target: "x/x/x/x/../../../../.ssh/id_rsa"},
		},
		"case variant with a single level of slack": {
			{Path: "Dir", Mode: fs.ModeSymlink, Target: "."},
			{Path: "leak", Mode: fs.ModeSymlink, Target: "dir/../../loot"},
		},
		// NFD in the target, NFC on disk: the same file on macOS.
		"unicode normalization variant": {
			{Path: "caf\u00e9", Mode: fs.ModeSymlink, Target: "."},
			{Path: "leak", Mode: fs.ModeSymlink, Target: "cafe\u0301/../../loot"},
		},
	}
	for name, entries := range cases {
		err := ValidateLinks(entries)
		if err == nil {
			t.Fatalf("%s: a name matching an entry only under macOS folding must be refused", name)
		}
		if !strings.Contains(err.Error(), "folding") {
			t.Fatalf("%s: the refusal must name the cause: %v", name, err)
		}
	}
	// An exact name is unaffected: the refusal fires only when the exact
	// lookup misses and the folded one hits.
	ok := []ManifestEntry{
		{Path: "X", Mode: fs.ModeSymlink, Target: "."},
		{Path: "fine", Mode: fs.ModeSymlink, Target: "X/inner"},
	}
	if err := ValidateLinks(ok); err != nil {
		t.Fatalf("an exactly-named component must still resolve: %v", err)
	}
}

// TestFolderNameIsIdempotent pins the equivalence-key invariant itself. A
// fold key is used as an equivalence-class representative, so applying the
// operation to its own output must never move to another key. cases.Fold
// swaps the two Cherokee blocks instead of choosing one representative;
// checking every Unicode scalar keeps the test independent of any one script.
func TestFolderNameIsIdempotent(t *testing.T) {
	fold := newFolder()
	for r := rune(0); r <= utf8.MaxRune; r++ {
		one := fold.name(string(r))
		if two := fold.name(one); two != one {
			t.Fatalf("folder.name is not idempotent for U+%04X: %U then %U", r, []rune(one), []rune(two))
		}
	}
	hostile := []string{"A", "a", "\u0345", "\u2126", "\u212A", "\u212B", "\u13A0", "\uAB70", "\U00010400", "\U00010428"}
	caseFold := cases.Fold()
	for _, left := range hostile {
		for _, right := range hostile {
			input := left + right
			transformed := caseFold.String(norm.NFD.String(input))
			if got, want := fold.name(transformed), fold.name(input); got != want {
				t.Fatalf("folder.name is not closed over fold(NFD(x)) for %U: got %U, want %U", []rune(input), []rune(got), []rune(want))
			}
		}
	}
}

// TestValidateLinksRefusesCherokeeInexactNameMatch exercises the real escape
// missed by cases.Fold: APFS treats each pair as one name, but cases.Fold
// swaps the spellings and therefore produced two different map keys.
func TestValidateLinksRefusesCherokeeInexactNameMatch(t *testing.T) {
	for name, pair := range map[string][2]string{
		"Cherokee main blocks": {"\u13a0", "\uab70"},
		"Cherokee small block": {"\u13f0", "\u13f8"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateLinks([]ManifestEntry{
				{Path: pair[0], Mode: fs.ModeSymlink, Target: "."},
				{Path: "leak", Mode: fs.ModeSymlink, Target: strings.Repeat(pair[1]+"/", 4) + "../../../../outside"},
			})
			if err == nil {
				t.Fatal("a Cherokee spelling matching an entry on macOS must be refused")
			}
			if !strings.Contains(err.Error(), "folding") {
				t.Fatalf("the refusal must name the folding mismatch: %v", err)
			}
		})
	}
}

// TestValidateLinksRefusesMultiRuneAPFSFoldMatch covers a fold equivalence
// that appears only after full folding expands a precomposed rune. Without an
// NFD pass before the SimpleFold representative is chosen, these two names
// produce different keys even though APFS resolves them to the same entry.
func TestValidateLinksRefusesMultiRuneAPFSFoldMatch(t *testing.T) {
	nameA := "z\u0399\u0308\u0301z"
	nameB := "z\u1fbe\u0308\u0301z"
	fold := newFolder()
	if gotA, gotB := fold.name(nameA), fold.name(nameB); gotA != gotB {
		t.Fatalf("APFS-equivalent names must share one fold key: %U != %U", []rune(gotA), []rune(gotB))
	}

	err := ValidateLinks([]ManifestEntry{
		{Path: nameA, Mode: fs.ModeSymlink, Target: "."},
		{Path: "leak", Mode: fs.ModeSymlink, Target: strings.Repeat(nameB+"/", 4) + "../../../../outside"},
	})
	if err == nil {
		t.Fatal("a multi-rune name matching an entry under APFS folding must be refused")
	}
	if !strings.Contains(err.Error(), "folding") {
		t.Fatalf("the refusal must name the folding mismatch: %v", err)
	}
}

// TestValidateLinksRefusesSupplementaryPlaneAPFSFoldMatch covers a canonical
// caseless match whose starter is outside the BMP. Normalizing to NFC before
// folding is unsafe with x/text versions that truncate a supplementary starter
// during recomposition; APFS compares the decomposed spellings instead.
func TestValidateLinksRefusesSupplementaryPlaneAPFSFoldMatch(t *testing.T) {
	nameA := "z\U00010427\u0308\u0301z"
	nameB := "z\U0001044f\u0308\u0301z"
	fold := newFolder()
	if gotA, gotB := fold.name(nameA), fold.name(nameB); gotA != gotB {
		t.Fatalf("APFS-equivalent supplementary-plane names must share one fold key: %U != %U", []rune(gotA), []rune(gotB))
	}

	err := ValidateLinks([]ManifestEntry{
		{Path: nameA, Mode: fs.ModeSymlink, Target: "."},
		{Path: "leak", Mode: fs.ModeSymlink, Target: strings.Repeat(nameB+"/", 4) + "../../../../outside"},
	})
	if err == nil {
		t.Fatal("a supplementary-plane spelling matching an entry under APFS folding must be refused")
	}
	if !strings.Contains(err.Error(), "folding") {
		t.Fatalf("the refusal must name the folding mismatch: %v", err)
	}
}

// TestValidateLinksRefusesSingletonCanonicalDecompositionMatch pins the case
// where the stored spelling itself has no case or normalization variants.
// U+037E canonically decomposes to ASCII ';', so an exact miss still needs a
// folded lookup even when ';' is the only indexed child.
func TestValidateLinksRefusesSingletonCanonicalDecompositionMatch(t *testing.T) {
	err := ValidateLinks([]ManifestEntry{
		{Path: "q/;", Mode: fs.ModeSymlink, Target: "."},
		{Path: "leak", Mode: fs.ModeSymlink, Target: "q/\u037e/\u037e/\u037e/\u037e/../../../../../outside"},
	})
	if err == nil {
		t.Fatal("a singleton canonical decomposition matching an entry on APFS must be refused")
	}
	if !strings.Contains(err.Error(), "folding") {
		t.Fatalf("the refusal must name the folding mismatch: %v", err)
	}
}

func TestValidateLinksBoundsFoldCache(t *testing.T) {
	fold := newFolder()
	root, err := buildLinkTrie([]ManifestEntry{{Path: "anchor", Mode: fs.ModeSymlink, Target: "."}}, fold)
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]string, 0, 10000)
	for i := 0; i < 5000; i++ {
		parts = append(parts, fmt.Sprintf("missing-%04d", i), "..")
	}
	cache := map[string]string{}
	budget := maxLinkComponents
	if err := resolveLinkPath("probe", strings.Join(parts, "/"), root, fold, cache, &budget); err != nil {
		t.Fatal(err)
	}
	if len(cache) > 4096 {
		t.Fatalf("fold cache grew to %d entries, want at most 4096", len(cache))
	}
}

func TestResolveLinkPathAllowsNilFoldCache(t *testing.T) {
	fold := newFolder()
	root, err := buildLinkTrie([]ManifestEntry{{Path: "X", Mode: fs.ModeSymlink, Target: "."}}, fold)
	if err != nil {
		t.Fatal(err)
	}
	budget := maxLinkComponents
	err = resolveLinkPath("probe", "x", root, fold, nil, &budget)
	if err == nil || !strings.Contains(err.Error(), "folding") {
		t.Fatalf("inexact target must be safely refused with a nil cache, got %v", err)
	}
}

// TestValidateLinksRefusesFoldedSiblingCollision refuses a tree that cannot be
// checked out on a case-insensitive volume at all. Two siblings differing only
// under folding would collapse into one file there, so which of the two any
// name resolves to is undefined -- and fu would be reasoning about a tree that
// cannot exist. Refusing costs nothing and matches rule 7's
// 不合规拒绝并说明原因.
func TestValidateLinksRefusesFoldedSiblingCollision(t *testing.T) {
	err := ValidateLinks([]ManifestEntry{
		{Path: "a/Link", Mode: fs.ModeSymlink, Target: "."},
		{Path: "a/link", Mode: fs.ModeSymlink, Target: "."},
	})
	if err == nil {
		t.Fatal("siblings colliding under folding must be refused")
	}
	if !strings.Contains(err.Error(), "same file on macOS") {
		t.Fatalf("the refusal must name the cause: %v", err)
	}
}

// TestValidateLinksRefusesUnboundedExpansion keeps the belt-and-braces cap
// honest: maxLinkResolution alone bounds hops, not work, so a target that
// expands past maxLinkComponents must be refused rather than walked.
func TestValidateLinksRefusesUnboundedExpansion(t *testing.T) {
	err := resolveLinkPathForTest("a", strings.Repeat("z/", maxLinkComponents+1)+"b", nil)
	if err == nil {
		t.Fatal("an expansion past the component cap must be refused")
	}
	if !strings.Contains(err.Error(), "path components") {
		t.Fatalf("the refusal must name the cause: %v", err)
	}
}

func TestValidateLinksSharesComponentBudgetAcrossEntries(t *testing.T) {
	entries := []ManifestEntry{
		{Path: "one", Mode: fs.ModeSymlink, Target: "a/b/c"},
		{Path: "two", Mode: fs.ModeSymlink, Target: "d/e/f"},
	}
	err := validateLinksWithBudget(entries, 5)
	if err == nil || !strings.Contains(err.Error(), "skill as a whole") {
		t.Fatalf("aggregate expansion must exhaust one shared budget, got %v", err)
	}
}

// resolveLinkPathForTest drives one resolution against a trie built from
// entries, so a table case can exercise resolveLinkPath directly without
// going through ValidateLinks' own entry-level checks.
func resolveLinkPathForTest(entryPath, target string, entries []ManifestEntry) error {
	fold := newFolder()
	root, err := buildLinkTrie(entries, fold)
	if err != nil {
		return err
	}
	budget := maxLinkComponents
	return resolveLinkPath(entryPath, target, root, fold, make(map[string]string), &budget)
}
