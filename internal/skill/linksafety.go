package skill

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// maxLinkResolution bounds how many in-tree symlinks one link may traverse
// before the resolution is declared a loop. It is the only cycle guard: a real
// loop exceeds it. A visited set keyed on the link path used to sit alongside
// it and false-rejected legitimate layouts that revisit a link (round 18
// finding M1).
//
// It does not by itself bound the work: a chain of 40 links whose targets are
// each padded to the kernel's path limit is acyclic, is accepted, and still
// expands to tens of thousands of components. maxLinkComponents below bounds
// the component count, and the trie below makes each component cost O(1) --
// both are needed, because a per-component cost that grows with the resolved
// depth turns a bounded component count back into unbounded work.
const maxLinkResolution = 40

// maxLinkComponents caps the total components one resolution may process,
// splices included. maxLinkResolution × a kernel-limit target is roughly
// 41 × 2048 ≈ 84k components, so this leaves an order of magnitude of headroom
// for any real layout while still refusing to run unbounded on input `fu add
// <git-url>` accepted from a third party. With the trie every component is
// O(1), so this is now a genuine bound on work rather than on count alone.
const maxLinkComponents = 1 << 20

// linkNode is one directory position in the in-tree symlink index.
//
// The index is a trie over path components rather than a map keyed on whole
// paths. A whole-path map has to rebuild and hash the resolved position once
// per component, which is O(depth) -- and the depth is attacker-controlled,
// because the attacker writes the link paths. Suppressing that lookup at
// depths no link path has (an earlier attempt) does not help: putting the
// padded chain inside a deep directory registers exactly the depth being
// walked. Measured on 182 entries: 9 ms with every link at depth 1 against
// 3.9 s with the same links at depth 380, and a 2.9 MB git source cost
// 2 m 49 s end to end, ~40 s of it inside fu.lock.
//
// With the trie, a push is one lookup on a single component name and a pop
// restores the parent pointer, so the walk is linear in components and
// independent of depth.
type linkNode struct {
	name     string
	children map[string]*linkNode
	// folded maps each child's stable case-folded, canonically normalized name
	// to that child.
	// It exists to *refuse* an inexact match, never to follow one -- see
	// resolveLinkPath's default arm.
	folded map[string]*linkNode
	target string
	isLink bool
}

func (n *linkNode) child(name string) (*linkNode, bool) {
	if n == nil || n.children == nil {
		return nil, false
	}
	c, ok := n.children[name]
	return c, ok
}

// folder returns a conservative equivalence key for names on macOS's default
// APFS volume. It must merge at least every pair the filesystem merges;
// merging extra pairs is a safe refusal, while failing to merge one can let a
// symlink escape the validated tree.
//
// A cases.Caser is stateful and must not be shared between goroutines, so one
// is built per ValidateLinks call and threaded through rather than kept in a
// package-level variable.
type folder struct {
	fold cases.Caser
}

func newFolder() *folder { return &folder{fold: cases.Fold()} }

func (f *folder) name(name string) string {
	s := norm.NFD.String(f.fold.String(norm.NFD.String(name)))
	var b strings.Builder
	for _, r := range s {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		b.WriteRune(minimum)
	}
	// SimpleFold can turn a base rune into a combining rune and thereby alter
	// canonical ordering. Normalize once more so the representative is stable
	// under a second call as well as shared by canonically equivalent names.
	return norm.NFD.String(b.String())
}

// buildLinkTrie indexes every symlink in the projection by its path
// components, and refuses up front any two sibling names that differ only
// under macOS's folding.
//
// Refusing such a tree costs nothing: it cannot be checked out on a
// case-insensitive volume at all, so it is not a skill anyone can install
// there, and SPEC rule 7 requires non-compliant content to be refused with a
// reason rather than handled on a best-effort basis.
func buildLinkTrie(entries []ManifestEntry, fold *folder) (*linkNode, error) {
	root := &linkNode{}
	insert := func(p, target string) error {
		node := root
		for _, comp := range splitResolvedDir(p) {
			if comp == "" {
				continue
			}
			if node.children == nil {
				node.children = map[string]*linkNode{}
				node.folded = map[string]*linkNode{}
			}
			next, ok := node.children[comp]
			if !ok {
				folded := fold.name(comp)
				if other, clash := node.folded[folded]; clash {
					return fmt.Errorf("entries %q and %q differ only by case or Unicode normalization; they name the same file on macOS", other.name, comp)
				}
				next = &linkNode{name: comp}
				node.children[comp] = next
				node.folded[folded] = next
			}
			node = next
		}
		node.target, node.isLink = target, true
		return nil
	}
	for _, e := range entries {
		if e.Mode&fs.ModeSymlink == 0 {
			continue
		}
		if err := insert(e.Path, e.Target); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// ValidateLinks enforces SPEC rule 7's escape check on a projected skill
// tree: every symlink's target, resolved through directory components and
// any in-tree symlink chain, must remain inside the skill root. It runs on
// the staged copy before a skill can be registered or published, so an
// imported skill can never carry a link that reaches outside the store
// (e.g. into the user's home directory) after publication.
//
// The check is a pure function over the projection, so it is table-testable
// without a filesystem. Rules:
//
//   - an absolute target is refused: it cannot be part of a portable skill,
//     and its meaning is machine-specific;
//   - a relative target is resolved component-wise with kernel semantics:
//     ".." pops the *resolved* position (refusing to pop above the root),
//     a component that lands on an in-tree symlink splices that link's own
//     target components ahead of the remaining ones, and resolution
//     continues until the components are exhausted;
//   - a component that names an in-tree symlink only under macOS's folding --
//     a different case, or a different Unicode normalization form -- is
//     refused, because fu cannot know which of the two the kernel will follow;
//   - an in-tree chain of symlinks is followed within the same resolution;
//     a chain longer than maxLinkResolution -- which a cycle necessarily is --
//     is refused;
//   - files and directories are never resolved -- they carry no target.
//
// Textual path.Clean collapsing is deliberately not used: it resolves ".."
// before symlinks are followed, so a composition like `b -> .` plus
// `a -> b/../x` would look in-root while the kernel pops above the root
// after following b (round 4 critical finding). Component-wise resolution
// with ".." applied to the resolved position closes exactly that gap.
//
// A broken link that points inside the tree (target not present) is
// allowed: git persists it, the copy preserves it, and it leaks nothing.
func ValidateLinks(entries []ManifestEntry) error {
	return validateLinksWithBudget(entries, maxLinkComponents)
}

func validateLinksWithBudget(entries []ManifestEntry, componentBudget int) error {
	return validateLinksWithFolder(entries, componentBudget, newFolder())
}

func validateLinksWithFolder(entries []ManifestEntry, componentBudget int, fold *folder) error {
	foldCache := make(map[string]string)
	root, err := buildLinkTrie(entries, fold)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Mode&fs.ModeSymlink == 0 {
			continue
		}
		if err := resolveLinkPath(e.Path, e.Target, root, fold, foldCache, &componentBudget); err != nil {
			return fmt.Errorf("symlink %q: %w", e.Path, err)
		}
	}
	return nil
}

const maxFoldCacheEntries = 4096

// resolveLinkPath follows one symlink at entryPath with raw target through
// the in-tree link trie, verifying every intermediate position stays inside
// the root. Resolution is component-wise over the target, with kernel
// semantics: each component is applied to the current resolved position, a
// ".." pops that position (refusing to leave the root), and a component
// that lands on an in-tree symlink splices the link's own target components
// -- relative to the link's parent directory -- ahead of the remaining
// ones.
//
// The resolved position is a component stack with a parallel trie-node stack,
// not a path string. Rebuilding the position as a string once per component
// made the walk quadratic in the resolved depth, which the attacker chooses;
// see linkNode for the measurements.
func resolveLinkPath(entryPath, target string, root *linkNode, fold *folder, foldCache map[string]string, budget *int) error {
	if path.IsAbs(target) {
		return fmt.Errorf("target %q is absolute; a skill must be self-contained", target)
	}
	// nodes[i] is the trie position after cur[:i] has been pushed, so nodes[0]
	// is the root and len(nodes) == len(cur)+1. A nil entry means the position
	// is below the indexed tree: no descendant of it can be a link, because
	// inserting a link path creates every one of its ancestors.
	cur := splitResolvedDir(path.Dir(entryPath))
	nodes := make([]*linkNode, 1, len(cur)+8)
	nodes[0] = root
	for _, comp := range cur {
		next, _ := nodes[len(nodes)-1].child(comp)
		nodes = append(nodes, next)
	}
	// No visited set. Keying one on the link's path alone rejected legitimate
	// acyclic layouts that traverse the same link twice -- e.g. m -> n,
	// n/x -> ../m/y, n/y, a -> m/x, which the kernel resolves in-root to n/y
	// (round 18 finding M1). maxLinkResolution below terminates genuine cycles
	// on its own, so the set bought nothing but false rejections.
	type componentFrame struct {
		components []string
		next       int
	}
	steps := 0
	newFrame := func(target string) componentFrame {
		return componentFrame{components: strings.Split(target, "/")}
	}
	frames := []componentFrame{newFrame(target)}
	for len(frames) > 0 {
		frame := &frames[len(frames)-1]
		if frame.next == len(frame.components) {
			frames = frames[:len(frames)-1]
			continue
		}
		*budget--
		if *budget < 0 {
			return fmt.Errorf("the skill as a whole exceeds the total path components budget while resolving target %q", target)
		}
		comp := frame.components[frame.next]
		frame.next++
		switch {
		case comp == "" || comp == ".":
			continue
		case comp == "..":
			if len(cur) == 0 {
				return fmt.Errorf("target %q escapes the skill directory", target)
			}
			cur = cur[:len(cur)-1]
			nodes = nodes[:len(nodes)-1]
		default:
			here := nodes[len(nodes)-1]
			next, ok := here.child(comp)
			if !ok {
				// An inexact match is refused, never followed. Folding the
				// lookup instead would be unsafe in the other direction: on a
				// case-sensitive filesystem the kernel would *not* follow this
				// link, so following it here can push fu's resolved position
				// deeper than the kernel's whenever the link's own target has
				// more than one component -- reopening the same slack that
				// makes the escape work. Refusing is correct on both platforms.
				if here != nil && here.folded != nil {
					folded, cached := foldCache[comp]
					if !cached {
						folded = fold.name(comp)
						if foldCache != nil && len(foldCache) < maxFoldCacheEntries {
							foldCache[comp] = folded
						}
					}
					if clash, hit := here.folded[folded]; hit {
						return fmt.Errorf("target %q names %q, which is not an entry, but matches entry %q under macOS's case and Unicode folding; a skill must name its own files exactly", target, comp, clash.name)
					}
				}
				cur = append(cur, comp)
				nodes = append(nodes, nil)
				continue
			}
			if !next.isLink {
				cur = append(cur, comp)
				nodes = append(nodes, next)
				continue
			}
			steps++
			if steps > maxLinkResolution {
				return fmt.Errorf("target %q resolves through more than %d symlinks", target, maxLinkResolution)
			}
			// Refused here, not only at the entry check above: that check
			// runs in a different loop and covers only a link's own raw
			// target. A spliced "/etc" splits to ["", "etc"], and the
			// empty-component arm skips the leading "" silently, so an
			// absolute path would resolve as if it were relative (round 18
			// finding M2).
			if path.IsAbs(next.target) {
				return fmt.Errorf("target %q reaches absolute target %q through symlink %q; a skill must be self-contained",
					target, next.target, path.Join(append(append([]string{}, cur...), comp)...))
			}
			// Follow the link from its own parent directory: the position is
			// already the link's parent, so only the target is spliced ahead
			// of the remaining components.
			frames = append(frames, newFrame(next.target))
		}
	}
	return nil
}

// splitResolvedDir turns a link's parent directory into the component stack
// resolution starts from. The root is the empty stack, which is what the ".."
// arm tests against.
func splitResolvedDir(dir string) []string {
	dir = path.Clean(dir)
	if dir == "." || dir == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(dir, "/"), "/")
}
