package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgent lets tests point an adapter at any directory.
type fakeAgent struct{ name, dir string }

func (f fakeAgent) Name() string       { return f.name }
func (f fakeAgent) Detect() bool       { return true }
func (f fakeAgent) SkillsDir() string  { return f.dir }
func (f fakeAgent) Reserved() []string { return []string{".system"} }

// TestOwnsLinkIsExactProjectionIdentity pins the ownership predicate
// directly, at the level of raw path text. Round 6 replaced containment
// ("does the target land somewhere inside store/skills?") with exact
// identity ("is the target precisely the skill this entry is named after?"),
// because containment is satisfied by paths fu would never write.
func TestOwnsLinkIsExactProjectionIdentity(t *testing.T) {
	const store = "/fu/store/skills"
	cases := []struct {
		name, entry, target string
		want                bool
		why                 string
	}{
		{"exactly what fu writes", "pdf", "/fu/store/skills/pdf", true,
			"the one shape fu ever creates"},
		{"round 6 Critical: entry name disagrees with the target's leaf", "notes", "/fu/store/skills/pdf", false,
			"fu never names a link one thing and points it at a skill named another"},
		{"target below a skill's own root", "pdf", "/fu/store/skills/pdf/sub", false,
			"fu links at a skill's root, never inside it"},
		{"target above the skills dir", "pdf", "/fu/store/pdf", false,
			"not the store's skills directory at all"},
		// Round 1's Critical, now falling out of the equality rather than
		// needing a rel != "." special case: the whole-set alias
		// `ln -s ~/.fu/store/skills ~/.codex/skills` SPEC §2.3 cites as
		// community practice. Reproduced against the compiled binary back
		// then: `fu new beta` printed only "created beta" and silently
		// deleted the link.
		{"the skills root itself", "all", "/fu/store/skills", false,
			"a link at the skills root is not a link at any one skill"},
		// The string-prefix trap that made component-wise comparison
		// mandatory under containment. Equality cannot fall for it either.
		{"sibling directory sharing a name prefix", "pdf", "/fu/store/skills-foreign/pdf", false,
			"skills-foreign is not skills"},
		{"unrelated path", "pdf", "/elsewhere", false,
			"nothing to do with the store"},
		{"relative target", "pdf", "skills/pdf", false,
			"fu only ever writes absolute targets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownsLink(store, tc.entry, tc.target); got != tc.want {
				t.Fatalf("ownsLink(%q, %q, %q) = %v, want %v -- %s",
					store, tc.entry, tc.target, got, tc.want, tc.why)
			}
		})
	}
}

// Critical finding 2, at the level it was actually observed: a symlink
// named "all" inside an agent's skills directory, pointing not at one
// skill but at the store's skills root itself. Reproduced against the
// compiled binary: with such a link in place, `fu new beta` printed only
// "created beta" and silently deleted the link -- no Result field
// recorded it, nothing was printed. ScanAgent must classify this as
// foreign (fu never created a link shaped like this), not as a fu link
// eligible for RemoveLink.
func TestScanAgentLinkToStoreSkillsRootIsForeign(t *testing.T) {
	storeSkills := filepath.Join(t.TempDir(), "store", "skills")
	os.MkdirAll(storeSkills, 0o755)
	dir := t.TempDir()
	if err := os.Symlink(storeSkills, filepath.Join(dir, "all")); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Kind != KindForeign {
		t.Fatalf("a link to the store's skills root itself must be foreign, not fu-owned: %+v", st.Entries)
	}
}

func TestScanAgentClassification(t *testing.T) {
	storeSkills := filepath.Join(t.TempDir(), "store", "skills")
	os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755)
	dir := t.TempDir()
	a := fakeAgent{"claude", dir}

	os.Symlink(filepath.Join(storeSkills, "alpha"), filepath.Join(dir, "alpha")) // fu link
	os.Symlink(filepath.Join(storeSkills, "gone"), filepath.Join(dir, "gone"))   // broken fu link
	os.Symlink("/elsewhere/thing", filepath.Join(dir, "ext"))                    // foreign link
	os.MkdirAll(filepath.Join(dir, "manual"), 0o755)                             // foreign real dir
	os.MkdirAll(filepath.Join(dir, ".system"), 0o755)                            // reserved

	st, err := ScanAgent(a, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range st.Entries {
		got[e.Name] = e
	}
	if _, ok := got[".system"]; ok {
		t.Fatal("reserved entries must be excluded")
	}
	if got["alpha"].Kind != KindFuLink || got["alpha"].Broken {
		t.Fatalf("alpha misclassified: %+v", got["alpha"])
	}
	if got["gone"].Kind != KindFuLink || !got["gone"].Broken {
		t.Fatalf("gone must be a broken fu link: %+v", got["gone"])
	}
	if got["ext"].Kind != KindForeign || got["manual"].Kind != KindForeign {
		t.Fatal("foreign entries misclassified")
	}
}

func TestScanAgentParentStates(t *testing.T) {
	storeSkills := t.TempDir()
	missing := fakeAgent{"claude", filepath.Join(t.TempDir(), "absent")}
	st, _ := ScanAgent(missing, storeSkills)
	if !st.ParentMissing {
		t.Fatal("absent skills dir must set ParentMissing")
	}
	base := t.TempDir()
	target := filepath.Join(base, "real")
	os.MkdirAll(target, 0o755)
	link := filepath.Join(base, "linkdir")
	os.Symlink(target, link)
	st, _ = ScanAgent(fakeAgent{"claude", link}, storeSkills)
	if !st.ParentIsSymlink {
		t.Fatal("symlinked skills dir must set ParentIsSymlink (SPEC rule 10)")
	}
}

// Finding I6: an agent whose SkillsDir is empty (e.g. the real Claude/
// Codex adapters when HOME is unset) must be refused outright, not
// scanned as if "" were a valid relative path. Without this guard,
// os.Lstat("") happens to fail with a NotExist-shaped error, which
// ScanAgent would otherwise read as an ordinary "not created yet"
// ParentMissing -- and the caller (reconcile) would then try to
// os.MkdirAll and os.Symlink into a directory derived from "", resolved
// relative to the process's current working directory. Reproduced
// against the compiled binary pre-fix (before Claude/Codex themselves
// were fixed to return ""): `env -u HOME FU_HOME=... fu new alpha`, run
// from a directory containing its own ./.claude, created a link at
// <cwd>/.claude/skills/alpha.
func TestScanAgentRefusesEmptySkillsDir(t *testing.T) {
	storeSkills := t.TempDir()
	if _, err := ScanAgent(fakeAgent{"claude", ""}, storeSkills); err == nil {
		t.Fatal("an agent with an empty SkillsDir must be refused, not scanned as if valid")
	}
}

// A plain file is neither a directory nor a symlink fu could own; it
// must always be classified foreign.
func TestScanAgentPlainFileIsForeign(t *testing.T) {
	storeSkills := filepath.Join(t.TempDir(), "store", "skills")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Kind != KindForeign {
		t.Fatalf("plain file must be foreign: %+v", st.Entries)
	}
}

// A reserved name is excluded by name alone, regardless of what occupies
// it — even a symlink into the store, which would otherwise qualify as
// a fu link, must never be surfaced (SPEC rule 11).
func TestScanAgentReservedNameExcludedRegardlessOfType(t *testing.T) {
	storeSkills := filepath.Join(t.TempDir(), "store", "skills")
	os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755)
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(storeSkills, "alpha"), filepath.Join(dir, ".system")); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 0 {
		t.Fatalf("reserved name must be excluded even as a fu-shaped symlink: %+v", st.Entries)
	}
}

// Critical finding 1 (round 3), a regression in round 2 finding 1's own
// fix (see ownsLink's doc comment in scan.go): ownsLink used to resolve a
// symlink's raw readlink value all the way through its own leaf component,
// not just the directory the leaf sits in. That let a two-hop chain built
// entirely by the user -- one hop outside any agent directory, pointing
// straight at a real skill inside the store (standing in for `ln -s
// $FU_HOME/store/skills/alpha ~/mylink`), and a second hop from inside the
// agent's skills directory to that first hop (`ln -s ~/mylink
// ~/.claude/skills/notes`) -- resolve down to a path physically inside the
// store even though fu.yaml has no entry for either hop's name at all.
// Reproduced against the compiled binary pre-fix: with exactly this chain
// in place, `fu new beta` printed only "created beta", exited 0, and
// deleted "notes" -- every field of Result stayed empty, so nothing
// anywhere recorded that it had ever existed. The second hop must be
// classified foreign, the same as any other symlink fu did not create.
func TestScanAgentUserSymlinkChainIntoStoreIsForeign(t *testing.T) {
	root := t.TempDir()
	storeSkills := filepath.Join(root, "store", "skills")
	if err := os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user's own hop, created entirely outside any agent directory --
	// standing in for ~/mylink in the reviewer's reproduction. fu never
	// sees this path at all; it only ever reads the second hop below.
	home := t.TempDir()
	hop := filepath.Join(home, "mylink")
	if err := os.Symlink(filepath.Join(storeSkills, "alpha"), hop); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(hop, filepath.Join(dir, "notes")); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Kind != KindForeign {
		t.Fatalf("a symlink chain the user built entirely on their own, even one that happens to land inside the store, must be foreign, not fu-owned: %+v", st.Entries)
	}
}

// Critical finding 1 (round 4), a regression in round 3 finding 1's own fix
// (see ownsLink's and resolveTargetDir's doc comments in scan.go): round 3
// stopped ownsLink from resolving a candidate target's leaf, but left the
// *directory* portion fully resolved via resolveLongestExisting -- through
// any symlink sitting there, not just through an ancestor of $FU_HOME. That
// let a hop shaped like `ln -s "$FU_HOME/store/skills" ~/hopdir` (the
// reviewer's own reproduction), followed by `ln -s ~/hopdir/alpha
// ~/.claude/skills/notes` (fu.yaml has no "notes" entry at all), resolve the
// directory half of notes's raw target straight through ~/hopdir and into
// the store: the leaf itself, "alpha", is a plain, unresolved path
// component, so round 3's leaf-only fix never sees a symlink to refuse.
// Reproduced against the compiled binary pre-fix: with exactly this hop in
// place, `fu new beta` printed only "created beta", exited 0, and deleted
// "notes" -- every field of Result stayed empty. The hop must be classified
// foreign, the same as the leaf-position hop round 3 already covers.
func TestScanAgentUserSymlinkHopInDirectoryPositionIsForeign(t *testing.T) {
	root := t.TempDir()
	storeSkills := filepath.Join(root, "store", "skills")
	if err := os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user's own alias of the store's skills directory itself --
	// standing in for `ln -s "$FU_HOME/store/skills" ~/hopdir`. Unlike round
	// 3's fixture, the hop sits in the *directory* position: it is not the
	// agent-directory entry itself, and it is not the final component of
	// the entry's raw target either.
	home := t.TempDir()
	hop := filepath.Join(home, "hopdir")
	if err := os.Symlink(storeSkills, hop); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// notes's raw target is hop/alpha: a plain, unresolved leaf component
	// ("alpha") appended after the hop -- exactly the shape round 3's fix
	// does not touch.
	if err := os.Symlink(filepath.Join(hop, "alpha"), filepath.Join(dir, "notes")); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Kind != KindForeign {
		t.Fatalf("a user's own symlink hop in the directory position must be foreign, not fu-owned: %+v", st.Entries)
	}
}

// Round 5's Critical: an alias landing *above* store/skills reproduces the
// store's own two trailing components in the entry's raw target text, so
// round 4's suffix gate accepted it and resolved straight into the store.
// Each case below is a directory of the user's own -- fu never created any
// of it, and fu.yaml never mentions "notes".
//
// The last two cases are the same physical alias differing only in case.
// Under round 4's gate they reached *opposite* verdicts, which is what
// showed the criterion was filtering on the user's own spelling rather than
// deciding ownership; they must now agree.
func TestScanAgentUserAliasAboveStoreSkillsIsForeign(t *testing.T) {
	cases := []struct {
		name string
		// plant builds the user's own directory under aliasRoot and returns
		// the raw target text the agent-directory entry will carry.
		plant func(t *testing.T, aliasRoot, storeRoot, storeSkills string) string
	}{
		{
			// `mkdir ~/backup && ln -s "$FU_HOME/store" ~/backup/` -- ln
			// keeps the basename when handed a directory, so this is the
			// most natural way to write it.
			name: "alias of the store root, one level up",
			plant: func(t *testing.T, aliasRoot, storeRoot, _ string) string {
				if err := os.Symlink(storeRoot, filepath.Join(aliasRoot, "store")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(aliasRoot, "store", "skills", "alpha")
			},
		},
		{
			name: "alias of the skills dir, nested under a directory named like the store",
			plant: func(t *testing.T, aliasRoot, _, storeSkills string) string {
				if err := os.MkdirAll(filepath.Join(aliasRoot, "store"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(storeSkills, filepath.Join(aliasRoot, "store", "skills")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(aliasRoot, "store", "skills", "alpha")
			},
		},
		{
			// No symlink at all: a real directory tree of the user's own
			// that merely happens to be spelled like the store's.
			name: "real directory tree spelled like the store's",
			plant: func(t *testing.T, aliasRoot, _, _ string) string {
				p := filepath.Join(aliasRoot, "store", "skills", "alpha")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "alias whose own name matches the store's exactly",
			plant: func(t *testing.T, aliasRoot, storeRoot, _ string) string {
				if err := os.Symlink(storeRoot, filepath.Join(aliasRoot, "store")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(aliasRoot, "store", "skills", "alpha")
			},
		},
		{
			name: "the same alias, named in a different case",
			plant: func(t *testing.T, aliasRoot, storeRoot, _ string) string {
				if err := os.Symlink(storeRoot, filepath.Join(aliasRoot, "STORE")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(aliasRoot, "STORE", "skills", "alpha")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			storeRoot := filepath.Join(root, "store")
			storeSkills := filepath.Join(storeRoot, "skills")
			if err := os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.plant(t, t.TempDir(), storeRoot, storeSkills)

			dir := t.TempDir()
			if err := os.Symlink(target, filepath.Join(dir, "notes")); err != nil {
				t.Fatal(err)
			}
			st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Entries) != 1 || st.Entries[0].Kind != KindForeign {
				t.Fatalf("a path the user built themselves must be foreign, not fu-owned "+
					"(target %q): %+v", target, st.Entries)
			}
		})
	}
}

// The one residual prefixResolvesToStoreHome accepts on purpose, pinned
// here so that closing it later is a deliberate decision rather than an
// accident, and so that nobody re-derives it as a fresh finding.
//
// An alias of $FU_HOME *itself* resolves, so a target reached through it is
// classified fu-owned. This is not a hole the predicate could close while
// still doing its job: the recorded target text is byte-for-byte what fu
// would have written had $FU_HOME been spelled that way, and recognizing
// its own links across exactly such respellings is why the prefix is
// resolved at all (round 2 finding 1 -- a dotfiles manager moving ~/.fu
// aside and linking it back). Path text cannot separate the two cases;
// only a record of what fu created could, and DESIGN §2 rules that out.
//
// The consequence is worth stating plainly: a link the *user* created,
// through their own alias of $FU_HOME, under a name fu.yaml does not
// mention, is reclaimed by the next reconcile.
func TestKnownResidualSameNameLinkIsTreatedAsFuOwned(t *testing.T) {
	cases := []struct {
		name string
		// target builds the entry's raw target text, given the store root
		// and its skills directory.
		target func(t *testing.T, root, storeSkills string) string
	}{
		{
			// Literally what fu writes. Nothing in the path text, at any
			// level of resolution, differs from a link fu created itself.
			name: "canonical store path",
			target: func(_ *testing.T, _, storeSkills string) string {
				return filepath.Join(storeSkills, "alpha")
			},
		},
		{
			// Reached through the user's own alias of $FU_HOME. Accepted for
			// the same reason the prefix is resolved at all: this is exactly
			// what fu would have written had $FU_HOME been spelled that way,
			// and recognizing its own links across such respellings is round
			// 2 finding 1's whole point (a dotfiles manager moving ~/.fu
			// aside and linking it back).
			name: "through an alias of $FU_HOME",
			target: func(t *testing.T, root, _ string) string {
				alias := filepath.Join(t.TempDir(), "myfu")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(alias, "store", "skills", "alpha")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			storeSkills := filepath.Join(root, "store", "skills")
			if err := os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			// The entry carries the *same* name as the skill it points at --
			// the one case name agreement cannot rule out.
			if err := os.Symlink(tc.target(t, root, storeSkills), filepath.Join(dir, "alpha")); err != nil {
				t.Fatal(err)
			}

			st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Entries) != 1 || st.Entries[0].Kind != KindFuLink {
				t.Fatalf("known, accepted residual: a link at the same name fu itself would use, "+
					"pointing where fu would point, is byte-for-byte what fu writes and is therefore "+
					"classified fu-owned: %+v", st.Entries)
			}
		})
	}
}

// A relative symlink target must never be claimed as a fu link, even
// when its text shares trailing components with the store path: fu
// itself only ever creates absolute-target links (see reconcile), so a
// relative target is never fu's, and ownsLink's base/path mismatch
// (absolute base, relative candidate) correctly falls to "not within"
// rather than silently slipping into the "ours" bucket.
func TestScanAgentRelativeSymlinkTargetIsForeign(t *testing.T) {
	root := t.TempDir()
	storeSkills := filepath.Join(root, "store", "skills")
	os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755)
	dir := t.TempDir()
	// Textually looks like a suffix of storeSkills, but is relative.
	if err := os.Symlink("store/skills/alpha", filepath.Join(dir, "rel")); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Kind != KindForeign {
		t.Fatalf("relative symlink target must not be claimed as a fu link: %+v", st.Entries)
	}
}

// Classification looks at the entry's own type (via Lstat/ReadDir),
// never at what a symlink points to: a symlink to an existing directory
// is still foreign, and its contents must not be pulled into the scan.
func TestScanAgentSymlinkToDirIsNotFollowedForClassification(t *testing.T) {
	storeSkills := filepath.Join(t.TempDir(), "store", "skills")
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(outside, "real", "nested"), 0o755)
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 {
		t.Fatalf("must not recurse into the symlink target's contents: %+v", st.Entries)
	}
	if st.Entries[0].Kind != KindForeign || st.Entries[0].LinkTarget == "" {
		t.Fatalf("symlink-to-dir must be classified by its own type: %+v", st.Entries[0])
	}
}

// Broken tracking is a fu-link concept only (it drives Diff's rebuild
// decision); a dangling foreign symlink stays foreign and must not be
// reported as Broken, since fu never rebuilds what it doesn't own.
func TestScanAgentForeignDanglingLinkNotMarkedBroken(t *testing.T) {
	storeSkills := filepath.Join(t.TempDir(), "store", "skills")
	dir := t.TempDir()
	if err := os.Symlink("/definitely/does/not/exist", filepath.Join(dir, "ext")); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Kind != KindForeign || st.Entries[0].Broken {
		t.Fatalf("foreign dangling link must stay foreign and unbroken: %+v", st.Entries[0])
	}
}

// Broken must distinguish between "target does not exist" vs "target is
// unreadable for other reasons": the former sets Broken=true with no error,
// the latter returns an error from ScanAgent.
//
// Case 2's target lives directly under storeSkills (depth 1), the same
// shape every genuine fu link actually has -- want := filepath.Join(storeSkillsDir,
// skillName) in diff.go never nests any deeper. This is deliberate, not
// incidental (round 4): dirHasStoreSkillsSuffix in scan.go only resolves a
// candidate's directory portion when its raw text ends *exactly* at
// storeSkillsDir's own trailing components, nothing further appended --
// the earlier version of this fixture nested the target one level deeper
// (storeSkills/readonly/target) and, post round 4, was reclassified
// foreign before ever reaching the permission check this test exists to
// exercise. Nesting one level deeper was never a real requirement here;
// what the test actually needs is some way to make os.Stat on a
// depth-1-shaped target fail with EACCES rather than ENOENT, which instead
// comes from restricting storeSkills' own permissions (see below).
func TestScanAgentBrokenVsUnreadableTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}

	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	os.MkdirAll(storeDir, 0o755)
	storeSkills := filepath.Join(storeDir, "skills")
	os.MkdirAll(storeSkills, 0o755)

	// Create a real target
	realTarget := filepath.Join(storeSkills, "real")
	os.MkdirAll(realTarget, 0o755)

	// Create agent dir
	agentDir := filepath.Join(root, "agent")
	os.MkdirAll(agentDir, 0o755)

	// Case 1: fu-link to a target that does not exist. Entry and target leaf
	// must agree for the link to be fu's at all (round 6, ownsLink), so both
	// are named "gone" rather than the entry carrying a description of its
	// own state.
	gonePath := filepath.Join(storeSkills, "gone")
	os.MkdirAll(gonePath, 0o755)
	linkToDone := filepath.Join(agentDir, "gone")
	os.Symlink(gonePath, linkToDone)
	os.RemoveAll(gonePath) // Target now does not exist

	// Case 2: fu-link to a target that becomes unreadable. The target
	// itself, "unreadable", sits directly under storeSkills -- see this
	// test's own doc comment for why depth matters here post round 4.
	unreadableTarget := filepath.Join(storeSkills, "unreadable")
	os.MkdirAll(unreadableTarget, 0o755)
	linkToUnreadable := filepath.Join(agentDir, "unreadable")
	os.Symlink(unreadableTarget, linkToUnreadable)

	// First scan: both should work before we restrict permissions
	st, err := ScanAgent(fakeAgent{"claude", agentDir}, storeSkills)
	if err != nil {
		t.Fatalf("scan before permission restriction failed: %v", err)
	}

	// Verify case 1 is broken but no error
	var gotMissing *Entry
	for i := range st.Entries {
		if st.Entries[i].Name == "gone" {
			gotMissing = &st.Entries[i]
			break
		}
	}
	if gotMissing == nil {
		t.Fatal("broken_missing entry not found")
	}
	if gotMissing.Kind != KindFuLink || !gotMissing.Broken {
		t.Fatalf("broken_missing must be a broken fu-link: %+v", gotMissing)
	}

	// broken_missing's own target also sits directly under storeSkills, so
	// restricting storeSkills' permissions below (to reproduce case 2)
	// would equally block resolving "gone" and turn its classification
	// into the same unreadable-parent error this test wants isolated to
	// case 2 alone. Its assertion above already ran; remove it so the
	// second scan exercises case 2 by itself.
	if err := os.Remove(linkToDone); err != nil {
		t.Fatal(err)
	}

	// Now restrict permissions on storeSkills itself: stat() needs search
	// permission on every directory a path traverses *through*, but not on
	// the final looked-up component's own bits, so this blocks resolving
	// anything *inside* storeSkills (case 2's target) without blocking a
	// bare stat of storeSkills itself (looked up via its own parent,
	// storeDir, left untouched) -- which dirHasStoreSkillsSuffix's
	// resolution path (resolveLongestExisting) still needs to succeed.
	t.Cleanup(func() {
		// Restore permissions so temp dir cleanup succeeds
		os.Chmod(storeSkills, 0o755)
	})
	if err := os.Chmod(storeSkills, 0o000); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}

	// Case 2: scan should fail with an error, not silently mark it as Broken
	st, err = ScanAgent(fakeAgent{"claude", agentDir}, storeSkills)
	if err == nil {
		t.Fatal("scan must return an error when target is unreadable")
	}
	// Verify the error message references the unreadable symlink
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("error must reference the unreadable symlink: %v", err)
	}
}

// TestOpenCheckedDirRefusesADirectorySwappedSinceTheScan pins the boundary
// before the directory descriptor exists. The final component must be opened
// without following it, and the resulting descriptor must still have the
// identity ScanAgent recorded. Everything *after* that open is immune by
// construction (see TestReconcileOperatesOnTheDirectoryItChecked).
func TestOpenCheckedDirRefusesADirectorySwappedSinceTheScan(t *testing.T) {
	base := t.TempDir()
	storeSkills := filepath.Join(base, "store", "skills")
	if err := os.MkdirAll(filepath.Join(storeSkills, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := ScanAgent(fakeAgent{"claude", dir}, storeSkills)
	if err != nil {
		t.Fatal(err)
	}
	// The scan approved this directory; opening it now must yield it.
	root, err := st.OpenCheckedDir()
	if err != nil {
		t.Fatalf("the directory the scan checked must open: %v", err)
	}
	root.Close()

	// A final symlink is forbidden even when it resolves back to the exact
	// inode the scan inspected. Identity alone cannot preserve SPEC rule 10's
	// no-symlink precondition.
	original := filepath.Join(base, "skills-original")
	if err := os.Rename(dir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, dir); err != nil {
		t.Fatal(err)
	}
	if root, err := st.OpenCheckedDir(); err == nil {
		root.Close()
		t.Fatal("a final symlink back to the scanned directory must be refused even though its target has the same inode")
	}

	// Now the path comes to name somebody else's directory instead.
	foreign := filepath.Join(base, "foreign")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, dir); err != nil {
		t.Fatal(err)
	}

	if root, err := st.OpenCheckedDir(); err == nil {
		root.Close()
		t.Fatal("a path that no longer names the scanned directory must be refused: opening it " +
			"anyway hands every later operation a directory nothing has checked")
	}

	// A plain replacement (a different real directory, not a symlink) must
	// be refused for the same reason -- the type is not what makes it unsafe.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if root, err := st.OpenCheckedDir(); err == nil {
		root.Close()
		t.Fatal("a freshly created directory at the same path is still not the one that was scanned")
	}
}
