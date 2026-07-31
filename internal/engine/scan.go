package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"fu/internal/agent"
)

// EntryKind classifies one entry in an agent skills directory.
type EntryKind int

const (
	// KindFuLink: symlink whose normalized raw target is exactly
	// <store skills dir>/<this entry's own name> -- the one shape fu ever
	// creates (see ownsLink). Ownership needs no manifest.
	KindFuLink EntryKind = iota
	// KindForeign: everything else — real dirs/files and symlinks
	// pointing elsewhere. Never touched (SPEC rule 2).
	KindForeign
)

type Entry struct {
	Name       string
	Kind       EntryKind
	LinkTarget string // raw readlink value for symlinks
	Broken     bool   // fu link whose target no longer exists
}

type AgentState struct {
	Agent           agent.Agent
	ParentMissing   bool // skills dir does not exist yet
	ParentIsSymlink bool // precondition violation: never write through
	Entries         []Entry
	// dirInfo is the identity of the skills directory this scan actually
	// inspected, recorded so the apply phase can prove it is acting on that
	// same directory and not one swapped in since (round 7 finding; see
	// OpenCheckedDir). Nil when the directory did not exist or was refused.
	dirInfo os.FileInfo
}

// OpenCheckedDir opens a descriptor for the skills directory this scan
// validated, and refuses if the path no longer names that same directory.
//
// Everything the apply phase does used to re-open the directory by pathname
// -- MkdirAll, Symlink, the ownership re-check, Remove -- so a namespace
// replacement landing between the scan and the apply meant those operations
// no longer addressed the directory that had passed SPEC rule 10's
// precondition. Creation could land inside a directory the user had
// substituted, and removal could delete an entry fu never classified: in
// the reproduction, a symlink the attacker placed at the same name, which
// the ownership re-check happily approved because it pointed at the real
// store.
//
// checkedAgentDir closes it: every method resolves one component relative to
// the open descriptor, so no later pathname replacement can redirect an
// operation. The final component is opened with O_NOFOLLOW, then the opened
// descriptor's identity is compared with the scan.
func (st AgentState) OpenCheckedDir() (*checkedAgentDir, error) {
	dir := st.Agent.SkillsDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	file := os.NewFile(uintptr(fd), dir)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open skills directory %s: invalid file descriptor", dir)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened skills directory %s: %w", dir, err)
	}
	if st.dirInfo == nil || !os.SameFile(st.dirInfo, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("%s is no longer the directory it was when scanned: it was replaced "+
			"between the scan and this write, so nothing here can be assumed about what fu owns", dir)
	}
	return &checkedAgentDir{file: file, display: dir}, nil
}

// checkedAgentDir exposes only the single-component operations reconcile
// needs. Each operation is relative to the no-follow descriptor opened above.
type checkedAgentDir struct {
	file    *os.File
	display string
}

func (d *checkedAgentDir) Close() error {
	return d.file.Close()
}

func (d *checkedAgentDir) checkedName(op, name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return &os.PathError{Op: op, Path: filepath.Join(d.display, name), Err: unix.EINVAL}
	}
	return nil
}

func (d *checkedAgentDir) Lstat(name string) (os.FileInfo, error) {
	if err := d.checkedName("lstat", name); err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(d.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &os.PathError{Op: "lstat", Path: filepath.Join(d.display, name), Err: err}
	}
	return checkedAgentFileInfo{name: name, stat: stat}, nil
}

func (d *checkedAgentDir) Readlink(name string) (string, error) {
	if err := d.checkedName("readlink", name); err != nil {
		return "", err
	}
	for size := 128; size <= 1<<20; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(int(d.file.Fd()), name, buf)
		if err != nil {
			return "", &os.PathError{Op: "readlink", Path: filepath.Join(d.display, name), Err: err}
		}
		if n < len(buf) {
			return string(buf[:n]), nil
		}
	}
	return "", &os.PathError{Op: "readlink", Path: filepath.Join(d.display, name), Err: unix.ENAMETOOLONG}
}

func (d *checkedAgentDir) Symlink(target, name string) error {
	if err := d.checkedName("symlink", name); err != nil {
		return err
	}
	if err := unix.Symlinkat(target, int(d.file.Fd()), name); err != nil {
		return &os.LinkError{Op: "symlink", Old: target, New: filepath.Join(d.display, name), Err: err}
	}
	return nil
}

func (d *checkedAgentDir) Remove(name string) error {
	if err := d.checkedName("remove", name); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(d.file.Fd()), name, 0); err != nil {
		return &os.PathError{Op: "remove", Path: filepath.Join(d.display, name), Err: err}
	}
	return nil
}

type checkedAgentFileInfo struct {
	name string
	stat unix.Stat_t
}

func (i checkedAgentFileInfo) Name() string       { return i.name }
func (i checkedAgentFileInfo) Size() int64        { return i.stat.Size }
func (i checkedAgentFileInfo) Mode() os.FileMode  { return checkedAgentFileMode(uint32(i.stat.Mode)) }
func (i checkedAgentFileInfo) ModTime() time.Time { return time.Time{} }
func (i checkedAgentFileInfo) IsDir() bool        { return i.Mode().IsDir() }
func (i checkedAgentFileInfo) Sys() any           { return &i.stat }

func checkedAgentFileMode(raw uint32) os.FileMode {
	mode := os.FileMode(raw & 0o777)
	if raw&uint32(unix.S_ISUID) != 0 {
		mode |= os.ModeSetuid
	}
	if raw&uint32(unix.S_ISGID) != 0 {
		mode |= os.ModeSetgid
	}
	if raw&uint32(unix.S_ISVTX) != 0 {
		mode |= os.ModeSticky
	}
	switch raw & uint32(unix.S_IFMT) {
	case uint32(unix.S_IFDIR):
		mode |= os.ModeDir
	case uint32(unix.S_IFLNK):
		mode |= os.ModeSymlink
	case uint32(unix.S_IFIFO):
		mode |= os.ModeNamedPipe
	case uint32(unix.S_IFSOCK):
		mode |= os.ModeSocket
	case uint32(unix.S_IFBLK):
		mode |= os.ModeDevice
	case uint32(unix.S_IFCHR):
		mode |= os.ModeDevice | os.ModeCharDevice
	case uint32(unix.S_IFREG):
	default:
		mode |= os.ModeIrregular
	}
	return mode
}

// ownsLink reports whether the entry named entryName, whose raw readlink
// value is target, is a link fu itself created -- the single question SPEC
// rule 2 turns on.
//
// The test is exact projection identity, not containment: fu writes every
// link as storeSkillsDir joined with the skill's own name, at an entry of
// that same name, so a link fu created always satisfies
//
//	resolved(target) == resolved(storeSkillsDir) + "/" + entryName
//
// and nothing else does. Anything else on disk is the user's (DESIGN §2).
//
// Rounds 1 through 5 all asked the weaker question -- "does target land
// somewhere inside store/skills?" -- and spent four rounds narrowing *how*
// a target was allowed to resolve, while leaving that question's own shape
// alone. Round 6 found what the shape was costing: a user's own
// `ln -s ~/.fu/store/skills/alpha ~/.claude/skills/notes`, with no alias
// and no hop, satisfies containment outright, so fu deleted it on the next
// write. Requiring the entry name to agree with the target's leaf settles
// that case with an invariant instead of a heuristic -- fu has no reason to
// ever create a link named one thing pointing at a skill named another,
// so their disagreeing is proof the link is not fu's. Requiring the parent
// to be storeSkillsDir exactly (rather than any ancestor of it) settles the
// same way for a target reaching *below* a skill's own root.
//
// Two shapes earlier rounds each needed their own special case for now fall
// out of the equality with no code of their own: a link pointing at the
// skills root itself (the whole-set alias `ln -s ~/.fu/store/skills
// ~/.codex/skills` SPEC §2.3 cites as community practice -- round 1's
// Critical, previously an explicit rel != "." check) and the string-prefix
// trap where "/store/skills-foreign/pdf" must not count as inside
// "/store/skills" (previously the reason component-wise comparison was
// mandatory). Neither can equal storeSkillsDir + "/" + entryName.
//
// What remains indistinguishable, and is accepted deliberately: a link the
// user created at the *same* name fu would use, pointing where fu would
// point (`ln -s ~/.fu/store/skills/alpha ~/.claude/skills/alpha`). That is
// byte-for-byte what fu writes; separating the two needs a record of what
// fu created, which DESIGN §2 rules out as a third state that drifts. See
// TestKnownResidualSameNameLinkIsTreatedAsFuOwned.
//
// The rest of this comment is the history of how the *resolution* rules
// below were arrived at, which the equality above still depends on.
//
// storeSkillsDir is resolved through resolveLongestExisting before comparing (round
// 2 finding 1). store.Home() resolves FU_HOME/HOME through any symlinks,
// but storeSkillsDir (derived from Home()) and target (a symlink's raw,
// unresolved readlink value) used to be compared as plain strings with only
// one side ever normalized: a link written while $FU_HOME was still an ordinary
// directory keeps that literal spelling in its recorded target forever, so
// the moment any ancestor of $FU_HOME is replaced by a symlink (e.g. a
// dotfiles manager moving ~/.fu aside and linking it back), every link fu
// created before that point compared unequal against the freshly resolved
// base and was reclassified KindForeign -- silently, since nothing
// re-writes an existing symlink's target text.
//
// target, by contrast, is resolved through resolveTargetDir, which stops one
// component short of what storeSkillsDir gets (round 3 finding 1, a regression in
// round 2 finding 1's own fix above). target is a symlink's raw readlink
// value: the user fully controls it, and unlike base's ancestors, its
// final component is not something fu ever needs resolved. Round 2's fix
// ran path through the same full resolution as base -- symlinks included
// in the leaf -- so a chain of two hops the user created entirely on their
// own (`ln -s $FU_HOME/store/skills/alpha ~/mylink`, then
// `ln -s ~/mylink ~/.claude/skills/notes`, with fu.yaml never mentioning
// "notes" at all) resolved down to a path physically inside the store: fu
// classified it KindFuLink and the next reconcile deleted it via
// RemoveLink, silently, since verifyFuLink's TOCTOU re-check calls this
// same predicate and agreed. A legitimate fu link's target never has this
// problem -- its final component is always the skill's own directory
// inside the store, a real directory fu itself created, never a symlink --
// so leaving the leaf unresolved costs nothing for links fu actually owns
// while making it impossible for a symlink the user put in the leaf
// position to resolve its way into the store and steal ownership.
//
// Round 3's fix only ever stopped the *leaf* from being resolved; the rest
// of the directory portion was still resolved in full, including through
// any symlink the user placed there themselves -- not just through an
// ancestor of $FU_HOME (round 4 finding). `ln -s "$FU_HOME/store/skills"
// ~/hopdir`, followed by `ln -s ~/hopdir/alpha ~/.claude/skills/notes`
// (again with no "notes" entry in fu.yaml), walks straight through
// ~/hopdir on the way to resolving the directory half of notes's target:
// "alpha" is a plain, unresolved leaf component, so round 3's fix never
// sees a symlink there to refuse.
//
// Round 4 gated that resolution on the directory half's raw text already
// ending with the store's own two trailing components, and round 5 found
// that gate open in turn: those two components say nothing about the text
// in front of them, which is the user's to write. Any alias landing
// *above* store/skills reproduces them verbatim -- `mkdir ~/backup &&
// ln -s "$FU_HOME/store" ~/backup/`, then a target of
// ~/backup/store/skills/alpha -- and passed. So the same physical alias
// reached opposite verdicts depending on nothing but what the user had
// named it, which is a filter on a coincidence of spelling rather than a
// judgement about ownership.
//
// What replaces it (round 5): the suffix is a *precondition*, and the
// prefix is the invariant. Once the two trailing components are confirmed
// present, they are stripped and what remains must resolve to the same
// directory $FU_HOME resolves to (prefixResolvesToStoreHome). Every target
// fu has ever written satisfies this by construction -- it is
// storeSkillsDir joined with a skill name, and storeSkillsDir is $FU_HOME
// plus those two components -- and it holds however $FU_HOME was spelled at
// write time and however many symlinks its ancestors have since acquired,
// because both sides are resolved before comparison. A hop the user built
// somewhere of their own fails it, at any depth, location, or spelling.
// One residual is accepted deliberately and documented at
// prefixResolvesToStoreHome: an alias of $FU_HOME itself. The alternative
// considered was abandoning resolution of arbitrary targets entirely and
// instead permitting traversal only through symlinks lying on the store's
// own ancestor chain; that was not chosen because "the store's own
// ancestor chain" is open-ended and history-dependent (any ancestor of
// $FU_HOME, replaced at any point in the past, possibly more than once),
// so recognizing it would mean reconstructing that history rather than
// checking an invariant that holds for every fu-written link unconditionally.
func ownsLink(storeSkillsDir, entryName, target string) bool {
	// entryName always comes from os.ReadDir (ScanAgent) or filepath.Base
	// (verifyFuLink), so it is a single path component by construction and
	// joining it here cannot escape.
	want := filepath.Join(resolveLongestExisting(storeSkillsDir), entryName)
	return resolveTargetDir(storeSkillsDir, target) == want
}

// resolveTargetDir resolves symlinks in the directory a path's leaf
// component sits in, then rejoins that leaf verbatim -- it never follows
// the leaf itself, even when the leaf happens to be a symlink of its own
// (round 3 finding 1; see ownsLink's doc comment for the full reasoning).
// A relative path is cleaned and returned untouched, never probed against
// the filesystem at all: resolveLongestExisting calls filepath.EvalSymlinks,
// which resolves a relative path against the process's current working
// directory -- an even less appropriate answer to "does this belong to the
// store" than following the leaf would be, since the store's ownership
// question has nothing to do with wherever fu's own process happens to
// have been launched from.
//
// Before the directory is resolved at all, it must pass two checks in
// order (rounds 4 and 5; see ownsLink's doc comment for how each came
// about). First, its raw text -- still unresolved at this point -- must
// end with storeSkillsDir's own trailing path components
// (dirHasStoreSkillsSuffix), which fixes where the $FU_HOME portion ends.
// Second, everything in front of those components must resolve to the same
// directory $FU_HOME does (prefixResolvesToStoreHome). Without any gate at
// all, resolveLongestExisting will happily walk through *any* symlink
// sitting in the directory portion, not just an ancestor of $FU_HOME; with
// only the first, it walks through any symlink the user happened to name
// after the store's own directories. A target fu actually wrote passes
// both by construction: its directory half is always storeSkillsDir joined
// with nothing else, so it ends with those components and its prefix is
// some spelling of $FU_HOME -- regardless of how $FU_HOME was spelled at
// creation time or has been spelled (or moved, or aliased) since.
func resolveTargetDir(storeSkillsDir, p string) string {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return p
	}
	dir := filepath.Dir(p)
	if !dirHasStoreSkillsSuffix(storeSkillsDir, dir) {
		return p
	}
	if !prefixResolvesToStoreHome(storeSkillsDir, dir) {
		return p
	}
	return filepath.Join(resolveLongestExisting(dir), filepath.Base(p))
}

// storeHomeOf strips the two trailing components store.go appends beneath
// $FU_HOME (Dir = Home/"store", SkillsDir = Dir/"skills"), yielding the
// $FU_HOME that a given skills-directory spelling implies. Only meaningful
// for a path dirHasStoreSkillsSuffix has already accepted.
func storeHomeOf(skillsDir string) string {
	return filepath.Dir(filepath.Dir(filepath.Clean(skillsDir)))
}

// prefixResolvesToStoreHome reports whether everything dir carries in front
// of those two trailing components resolves to the same directory $FU_HOME
// itself resolves to (round 5 finding). This is the half of the gate that
// is actually an invariant: fu writes every link's target as
// storeSkillsDir joined with the skill's own name, and storeSkillsDir is
// always $FU_HOME plus those two components, so the raw text in front of
// them is *always* some spelling of $FU_HOME -- whatever spelling was in
// effect when the link was written, and through however many symlinks any
// ancestor has since acquired, since both sides are resolved before being
// compared.
//
// dirHasStoreSkillsSuffix on its own is not an invariant, which is what
// round 5 found: it constrains only the last two components and says
// nothing about the text before them, and that text is entirely the user's
// to choose. `mkdir ~/backup && ln -s "$FU_HOME/store" ~/backup/` -- the
// spelling `ln` produces when handed a directory, i.e. the most natural way
// to write it -- puts the alias at ~/backup/store, so a target of
// ~/backup/store/skills/alpha carries those two components verbatim and
// passed the suffix gate unchanged, resolved into the store, and was
// deleted as fu's own. Requiring the prefix to resolve to $FU_HOME rejects
// it: ~/backup is a directory of the user's own and resolves to itself.
//
// One residual is accepted deliberately, and is stated here rather than
// hedged: an alias of $FU_HOME *itself* (`ln -s "$FU_HOME" ~/myfu`, then a
// target of ~/myfu/store/skills/alpha) passes, because its prefix does
// resolve to $FU_HOME. That is not a gap this predicate could close while
// still doing its job -- it is byte-for-byte the same situation as a link
// fu wrote when $FU_HOME was spelled ~/myfu, and the whole reason the
// prefix is resolved at all is that fu must keep recognizing its own links
// across exactly such respellings (see resolveLongestExisting). Path text
// cannot separate the two; only a record of what fu created could, and
// DESIGN §2 rules that out as a third state that drifts. See
// TestKnownResidualFuHomeAliasIsTreatedAsFuOwned, which pins this so
// closing it later is a deliberate decision rather than an accident.
func prefixResolvesToStoreHome(storeSkillsDir, dir string) bool {
	return resolveLongestExisting(storeHomeOf(dir)) ==
		resolveLongestExisting(storeHomeOf(storeSkillsDir))
}

// dirHasStoreSkillsSuffix reports whether dir's own raw text -- never
// resolved through symlinks, compared purely as path components -- ends
// with the same two trailing components as storeSkillsDir itself ("store"
// then "skills" today, per store.go's Dir/SkillsDir layout). The two
// components are read off storeSkillsDir itself rather than hardcoded as
// literals here, so this keeps matching store.go's actual layout even if
// its directory names ever change, without duplicating them.
//
// Two components -- not more, not fewer -- is the exact width of what
// store.go appends beneath $FU_HOME (Dir = Home/"store",
// SkillsDir = Dir/"skills"). This check exists to establish that width, so
// that stripping it leaves exactly a candidate $FU_HOME for
// prefixResolvesToStoreHome to test; it is a precondition of that test, not
// a decision of its own.
//
// It is emphatically NOT sufficient by itself (round 5 finding, reversing
// how round 4 introduced it): it constrains only the last two components
// and asks nothing of the text in front of them, and that text is entirely
// user-chosen. Round 4's comment here claimed that "checking only this
// fixed-width suffix, and nothing about what precedes it" was what made a
// genuine fu link survive $FU_HOME being respelled while still rejecting
// every user-built hop -- the first half was right and the second half was
// wrong. Any hop whose alias lands *above* store/skills carries those two
// components in its own raw text and passed unchanged; see
// prefixResolvesToStoreHome for the reproduction and for what actually
// carries the ownership decision now.
func dirHasStoreSkillsSuffix(storeSkillsDir, dir string) bool {
	wantLeaf := filepath.Base(filepath.Clean(storeSkillsDir))
	wantParent := filepath.Base(filepath.Dir(filepath.Clean(storeSkillsDir)))
	clean := filepath.Clean(dir)
	return filepath.Base(clean) == wantLeaf &&
		filepath.Base(filepath.Dir(clean)) == wantParent
}

// mkdirAllAnchored creates dir and any missing parents, without ever
// traversing a component that was not already there (round 8 finding).
//
// Reconcile used a plain os.MkdirAll for an agent's missing skills
// directory, which happens before OpenCheckedDir has any descriptor to
// anchor to -- so a symlink appearing at one of those missing components
// redirected the creation, and the whole pass then operated inside a
// directory nothing had checked, with an entirely empty Result.
//
// What separates the hazard from ordinary practice is which components
// already exist. Symlinks at or above the deepest existing ancestor are the
// user's own arrangement -- `~/.claude -> ~/dotfiles/claude` is a perfectly
// normal dotfiles setup, and that path is what fu was asked to use, so it
// is followed. Components below it did not exist a moment ago; anything
// that appears there is new, and fu has no reason to follow it. Creating
// them relative to a descriptor opened for the anchor gives exactly that
// split, because os.Root refuses to traverse a symlink inside its root.
func mkdirAllAnchored(dir string) error {
	_, err := mkdirAllAnchoredIdentity(dir)
	return err
}

// mkdirAllAnchoredIdentity is mkdirAllAnchored plus the identity of the final
// directory opened by the no-follow walk. Reconcile compares that identity
// with its rescan so a replacement after creation cannot be adopted.
func mkdirAllAnchoredIdentity(dir string) (os.FileInfo, error) {
	anchor, rest, anchorInfo, err := deepestExistingAncestor(dir)
	if err != nil {
		return nil, err
	}
	return mkdirAllUnderIdentity(anchor, rest, anchorInfo)
}

// deepestExistingAncestor splits dir into the deepest ancestor that exists
// on disk and the slash-separated remainder that does not. It also captures
// the identity of the followed anchor so opening it later cannot silently
// adopt a replacement. rest is empty when dir itself already exists.
func deepestExistingAncestor(dir string) (anchor, rest string, anchorInfo os.FileInfo, err error) {
	anchor = filepath.Clean(dir)
	for {
		if _, err := os.Lstat(anchor); err == nil {
			anchorInfo, err := os.Stat(anchor)
			if err != nil {
				return "", "", nil, fmt.Errorf("inspect existing ancestor %s: %w", anchor, err)
			}
			return anchor, rest, anchorInfo, nil
		} else if !os.IsNotExist(err) {
			return "", "", nil, fmt.Errorf("inspect %s: %w", anchor, err)
		}
		parent := filepath.Dir(anchor)
		if parent == anchor {
			// Reached the root without finding anything that exists, which
			// on any real filesystem means the path was never usable.
			return "", "", nil, fmt.Errorf("no existing ancestor of %s", dir)
		}
		if rest == "" {
			rest = filepath.Base(anchor)
		} else {
			rest = filepath.Join(filepath.Base(anchor), rest)
		}
		anchor = parent
	}
}

// mkdirAllUnder creates rest beneath the same anchor that discovery inspected,
// through directory descriptors, so neither replacement of the anchor nor a
// symlink in any new component can redirect creation. os.Root alone is not
// sufficient here: it follows relative symlinks that stay beneath its root.
func mkdirAllUnder(anchor, rest string, anchorInfo os.FileInfo) error {
	_, err := mkdirAllUnderIdentity(anchor, rest, anchorInfo)
	return err
}

func mkdirAllUnderIdentity(anchor, rest string, anchorInfo os.FileInfo) (os.FileInfo, error) {
	current, err := os.Open(anchor)
	if err != nil {
		return nil, err
	}
	defer func() { _ = current.Close() }()
	opened, err := current.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened ancestor %s: %w", anchor, err)
	}
	if anchorInfo == nil || !os.SameFile(anchorInfo, opened) {
		return nil, fmt.Errorf("%s is no longer the directory inspected before creation: refusing to create %s through a replacement", anchor, rest)
	}

	if rest == "" {
		return opened, nil
	}
	for _, component := range strings.Split(filepath.Clean(rest), string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("invalid missing path component %q beneath %s", component, anchor)
		}
		parentFD := int(current.Fd())
		if err := unix.Mkdirat(parentFD, component, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create %s beneath checked ancestor %s: %w", component, anchor, err)
		}
		fd, err := unix.Openat(parentFD, component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open created component %s beneath checked ancestor %s without following links: %w", component, anchor, err)
		}
		next := os.NewFile(uintptr(fd), component)
		if next == nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("open created component %s beneath checked ancestor %s: invalid file descriptor", component, anchor)
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, fmt.Errorf("close parent while creating %s beneath %s: %w", rest, anchor, err)
		}
		current = next
	}
	created, err := current.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat created directory %s: %w", filepath.Join(anchor, rest), err)
	}
	return created, nil
}

// resolveLongestExisting resolves symlinks in the longest prefix of path
// that actually exists on disk, leaving any trailing components that do
// not exist untouched. Plain filepath.EvalSymlinks refuses to resolve
// anything unless the entire path exists, which would be too strong here:
// a fu-owned link is allowed to be broken (its store-side target
// hand-deleted, see Entry.Broken) and must still be recognized as fu's
// own by ownsLink even then -- e.g. after the store's own ancestor
// directory was also replaced by a symlink, EvalSymlinks on the whole raw
// target would fail on the missing leaf and this would fall back to
// comparing the stale, unresolved spelling again, reintroducing round 2
// finding 1's asymmetry for exactly the links most in need of the fix (see
// TestReconcileRecognizesLinkRecordedWithNonCanonicalStoreSpelling for the
// combined scenario). A path with nothing resolvable at all (e.g. $FU_HOME
// before the store has ever been created) is returned unchanged, the same
// fallback store.resolveExisting already uses for the whole-path case.
//
// ownsLink calls this directly on base (full resolution, leaf included --
// base is store.SkillsDir()'s output, not attacker-controlled) and
// indirectly, through resolveTargetDir, on just the directory half of path
// (round 3 finding 1: path's own leaf must never be resolved) -- and, since
// round 4, only once resolveTargetDir has confirmed that directory half's
// raw text already ends with storeSkillsDir's own trailing components (see
// dirHasStoreSkillsSuffix); a directory that fails that check is never
// passed here at all. The broken case above still applies at that narrower
// call site exactly as described: the directory resolveTargetDir asks this
// function to resolve is the store-side skill's parent, which can itself
// sit behind a symlinked ancestor whether or not the skill directory named
// by the leaf still exists.
func resolveLongestExisting(path string) string {
	clean := filepath.Clean(path)
	suffix := ""
	cur := clean
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if suffix == "" {
				return resolved
			}
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the root (or, for a relative path, "."; see
			// filepath.Dir) without finding anything that exists: nothing
			// left to resolve.
			return clean
		}
		if suffix == "" {
			suffix = filepath.Base(cur)
		} else {
			suffix = filepath.Join(filepath.Base(cur), suffix)
		}
		cur = parent
	}
}

// ScanAgent inventories one agent skills directory with lstat semantics
// throughout; the parent symlink is never followed.
func ScanAgent(a agent.Agent, storeSkillsDir string) (AgentState, error) {
	st := AgentState{Agent: a}
	dir := a.SkillsDir()
	if dir == "" {
		// An empty SkillsDir means the adapter has no resolvable home
		// directory (e.g. HOME unset, finding I6) rather than a genuine
		// relative path. os.Lstat("") and os.MkdirAll("", ...) below would
		// both error anyway, but relying on that would be an accident of
		// their exact error kind rather than a deliberate refusal, and
		// os.Symlink into a directory *derived* from "" (a relative
		// ".claude/skills" resolved against the process's current working
		// directory) is exactly the silent-write-into-cwd failure this
		// finding is about -- refuse explicitly instead of ever reaching
		// that path. Reconcile isolates this per agent the same way any
		// other ScanAgent error is isolated (finding I3), so one agent with
		// no usable directory does not stop the rest.
		return st, fmt.Errorf("agent %q has no skills directory (its home directory is not resolvable)", a.Name())
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			st.ParentMissing = true
			return st, nil
		}
		return st, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		st.ParentIsSymlink = true
		return st, nil
	}
	// Recorded so the apply phase can prove it operates on this very
	// directory rather than one substituted afterwards (OpenCheckedDir).
	st.dirInfo = fi
	reserved := map[string]bool{}
	for _, r := range a.Reserved() {
		reserved[r] = true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return st, err
	}
	for _, e := range ents {
		if reserved[e.Name()] {
			continue
		}
		p := filepath.Join(dir, e.Name())
		entry := Entry{Name: e.Name(), Kind: KindForeign}
		if e.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return st, err
			}
			entry.LinkTarget = target
			if ownsLink(storeSkillsDir, e.Name(), target) {
				entry.Kind = KindFuLink
				if _, err := os.Stat(p); err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						entry.Broken = true
					} else {
						return st, fmt.Errorf("stat fu-owned symlink %s: %w", p, err)
					}
				}
			}
		}
		st.Entries = append(st.Entries, entry)
	}
	return st, nil
}
