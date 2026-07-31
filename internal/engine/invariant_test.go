// internal/engine/invariant_test.go
package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fu/internal/agent"
	"fu/internal/store"
)

// TestWriteOperationsNeverSilentlyTouchForeignContent is the mechanical,
// spec-level version of SPEC rule 2 ("fu never touches content it did not
// create"), rather than one more test pinned to a specific reproduction.
// Four review rounds have now found the same failure shape, each time in
// a direction the previous round's own fix left unguarded: round 1's
// symlink-to-the-store's-own-skills-root hole, round 3 Critical finding
// 1's user-built symlink chain with the hop at the *leaf* position, and
// round 4's own variant of that same chain with the hop in the
// *directory* position instead, all let something fu does not own vanish
// while every field of Result stayed empty -- "the command succeeded,
// printed nothing wrong, and something disappeared from disk."
// Hand-writing one more assertion for the next specific shape only ever
// catches shapes someone has already thought of -- which is exactly how
// round 4's own defect survived a fully green suite for an entire review
// round: this file's own hop fixture, before this round, hand-enumerated
// only the leaf-position shape round 3 had already found.
//
// This instead walks a spread of starting states -- foreign directories,
// foreign files, foreign symlinks (absolute, relative, chained through the
// store at every hop position and depth round 3 and round 4 each found a
// gap in, pointing at the store's own skills root, pointing at a real
// unrelated directory), and a reserved name -- through every write
// operation this plan ships (NewSkill, SetGlobal, SetAgentSwitch) and
// checks two things every single one of them must satisfy no matter which
// state it starts from:
//
//  1. every entry the test itself planted and knows to be foreign --
//     ground truth the test controls directly, independent of what fu's
//     own classifier says about it, since trusting the code under test to
//     also grade its own homework is exactly what let each of these bugs
//     hide from every narrower test -- survives byte for byte;
//  2. if it did not (i.e. disk state actually changed), the pass must not
//     have reported a completely empty Result while that happened. An
//     empty Result is supposed to mean "nothing needed doing"; a write
//     command is never allowed to make that claim while quietly deleting
//     or altering something it was never told to touch.
//
// Round 4 also closed two structural gaps in this test itself, beyond the
// missing hop shape: no fixture's name ever collided with a name fu.yaml
// actually tracks, so Diff's ReportConflict and ReportDisabledForeign
// branches were never exercised here at all; and every write op left
// occupied positions alone (disabling, or creating an unrelated name) --
// none ever attempted to *enable* a skill at a path foreign content
// already occupies, the most destructive direction, since that is exactly
// the case where a misclassification does not just fail to report
// something but actively deletes the foreign entry and replaces it with a
// genuine fu link. "occupied" (registered, disabled, no disk entry) below
// gives fixtures a colliding name to plant at, and the new "enable
// occupied skill" write op is what actually reaches for it.
func TestWriteOperationsNeverSilentlyTouchForeignContent(t *testing.T) {
	for _, fx := range foreignFixtures() {
		for _, op := range writeOps() {
			t.Run(op.label+"/"+fx.label, func(t *testing.T) {
				s, _ := setupStore(t)
				dir := t.TempDir()
				agents := []agent.Agent{fakeAgent{"claude", dir}}

				// Materialize "seed" the ordinary way, through the real
				// pipeline (NewSkill), before the foreign fixture is ever
				// planted: this both gives SetGlobal/SetAgentSwitch a
				// legitimate, already-registered skill to act on and
				// matches the realistic starting point every reproduction
				// in this round started from -- an agent directory that
				// already has fu's own content in it, not an empty one.
				if _, err := NewSkill(s, agents, "seed"); err != nil {
					t.Fatalf("setup: seed creation failed: %v", err)
				}
				// "occupied" is registered too, then immediately disabled
				// (removing its own link but leaving its store-side content
				// in place): a second managed skill whose name a fixture can
				// deliberately collide with, and whose own real store
				// directory a directory-position hop can alias through --
				// see hopFixtures' occupied-name fixtures below.
				if _, err := NewSkill(s, agents, "occupied"); err != nil {
					t.Fatalf("setup: occupied creation failed: %v", err)
				}
				if _, err := SetGlobal(s, agents, "occupied", false); err != nil {
					t.Fatalf("setup: disabling occupied failed: %v", err)
				}

				fx.plant(t, dir, s.SkillsDir(), filepath.Join(s.SkillsDir(), "seed"))
				entryPath := filepath.Join(dir, fx.name)
				before := snapshotPath(t, entryPath)
				if !before.present {
					t.Fatalf("setup error: fixture %q did not plant anything at %q", fx.label, entryPath)
				}

				res, err := op.run(s, agents)
				if err != nil && !errors.Is(err, ErrOperationFailed) {
					t.Fatalf("write operation failed unexpectedly: %v", err)
				}

				after := snapshotPath(t, entryPath)
				changed := before != after
				if changed {
					t.Errorf("%s/%s: foreign entry %q did not survive byte for byte: before=%+v after=%+v",
						op.label, fx.label, entryPath, before, after)
				}
				if changed && resultEmpty(res) {
					t.Errorf("%s/%s: disk changed at %q but Result was entirely empty -- "+
						"this is the exact signature all three review rounds share: a write "+
						"command succeeds, reports nothing, and something fu does not own is "+
						"gone or altered: %+v", op.label, fx.label, entryPath, res)
				}
			})
		}
	}
}

// resultEmpty reports whether every field of res is empty. A write
// command's Result is supposed to mean "here is everything unusual this
// pass found"; if disk state changed in a way the test did not expect and
// Result is still entirely empty, nothing anywhere would ever have told
// the user.
func resultEmpty(res Result) bool {
	return len(res.Conflicts) == 0 &&
		len(res.Foreign) == 0 &&
		len(res.DisabledForeign) == 0 &&
		len(res.Missing) == 0 &&
		len(res.Reserved) == 0 &&
		len(res.Invalid) == 0 &&
		len(res.Skipped) == 0 &&
		len(res.Failed) == 0
}

// foreignSnapshot captures one directory entry's disk state precisely
// enough to detect any change to it at all: its presence, its kind, a
// symlink's raw target text, or a regular file/directory's content
// (recursively, for a directory -- see snapshotPath). Deliberately
// independent of Entry/EntryKind, which are the classifier under test
// here: reusing fu's own classification to decide what counts as
// "changed" would let exactly the same misclassification bug hide from
// this test that it hides from production.
type foreignSnapshot struct {
	present     bool
	isDir       bool
	target      string // symlink's raw readlink value
	fingerprint string // regular file: its content; directory: a manifest of every nested path and content
}

// snapshotPath reads path's current disk state into a foreignSnapshot.
// Lstat semantics throughout (a symlink is recorded by its own raw target
// text, never followed), matching how ScanAgent itself observes an agent
// skills directory.
func snapshotPath(t *testing.T, path string) foreignSnapshot {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return foreignSnapshot{}
		}
		t.Fatalf("lstat %s: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatalf("readlink %s: %v", path, err)
		}
		return foreignSnapshot{present: true, target: target}
	}
	if fi.IsDir() {
		var manifest strings.Builder
		err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(path, p)
			if relErr != nil {
				return relErr
			}
			if d.IsDir() {
				fmt.Fprintf(&manifest, "dir:%s\n", rel)
				return nil
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&manifest, "file:%s:%x\n", rel, content)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", path, err)
		}
		return foreignSnapshot{present: true, isDir: true, fingerprint: manifest.String()}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return foreignSnapshot{present: true, fingerprint: string(content)}
}

// foreignFixture plants one piece of content fu must never touch, at a
// known name inside an agent's skills directory. plant receives the
// agent's skills directory, the store's skills directory, and the
// already-materialized "seed" skill's own store-side directory (a real,
// legitimate skill fixtures can point at, for shapes that need one).
type foreignFixture struct {
	label string
	name  string
	plant func(t *testing.T, agentDir, storeSkillsDir, seedSkillDir string)
}

// foreignFixtures spans a spread of starting states broader than any single
// round's own reported reproduction on its own: real directories and files
// fu never created, every shape of symlink hop the review has collectively
// found reason to worry about (see hopShapes), pointing at the store's own
// skills root, a relative target that only looks like a store path, and a
// plain symlink to a real but unrelated directory -- plus a reserved name
// and a managed-but-disabled name ("occupied"), both occupied by real
// content, so a defect in either of the two name-collision branches
// (ReportReserved's disk-side counterpart, and ReportConflict/
// ReportDisabledForeign) has a fixture to be caught by. Each fixture is
// exercised in isolation (see the test above), so a defect in classifying
// one shape cannot hide behind another, correctly-classified entry sharing
// the same pass.
func foreignFixtures() []foreignFixture {
	fixtures := []foreignFixture{
		{
			label: "plain directory with nested content",
			name:  "manual",
			plant: func(t *testing.T, agentDir, _, _ string) {
				d := filepath.Join(agentDir, "manual")
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "notes.md"), []byte("hand-authored"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			label: "plain file",
			name:  "notes.txt",
			plant: func(t *testing.T, agentDir, _, _ string) {
				if err := os.WriteFile(filepath.Join(agentDir, "notes.txt"), []byte("hello"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			label: "symlink to an unrelated, nonexistent location",
			name:  "ext",
			plant: func(t *testing.T, agentDir, _, _ string) {
				if err := os.Symlink("/definitely/does/not/exist/elsewhere", filepath.Join(agentDir, "ext")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The whole-set alias SPEC §2.3 itself cites as community
			// practice (`ln -s ~/.fu/store/skills ~/.codex/skills`) --
			// fu never created this, and must not silently reclaim it.
			label: "symlink to the store's skills root itself",
			name:  "all",
			plant: func(t *testing.T, agentDir, storeSkillsDir, _ string) {
				if err := os.Symlink(storeSkillsDir, filepath.Join(agentDir, "all")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Round 6 Critical: no hop, no alias, no trickery -- the user
			// simply made their own link at a name of their choosing,
			// pointing straight at a skill the store really does hold.
			// Ownership used to be "the target lands somewhere inside
			// store/skills", which this satisfies, so fu reclaimed a link it
			// never created. fu's own links always name the skill they point
			// at, so the entry name disagreeing with the target's leaf is
			// proof this is not one of fu's.
			label: "direct link into the store under a name of the user's own",
			name:  "notes-direct",
			plant: func(t *testing.T, agentDir, _, seedSkillDir string) {
				if err := os.Symlink(seedSkillDir, filepath.Join(agentDir, "notes-direct")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The same shape one level deeper: a link at a name of the
			// user's own, pointing *inside* a store-side skill directory
			// rather than at it. fu never creates a link below a skill's own
			// root either.
			label: "direct link below a store-side skill directory",
			name:  "notes-deep",
			plant: func(t *testing.T, agentDir, _, seedSkillDir string) {
				if err := os.Symlink(filepath.Join(seedSkillDir, "SKILL.md"), filepath.Join(agentDir, "notes-deep")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			label: "relative symlink target that only looks like a store path",
			name:  "rel",
			plant: func(t *testing.T, agentDir, _, _ string) {
				if err := os.Symlink("store/skills/alpha", filepath.Join(agentDir, "rel")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			label: "symlink to a real, unrelated directory (not dangling)",
			name:  "linked-elsewhere",
			plant: func(t *testing.T, agentDir, _, _ string) {
				outside := t.TempDir()
				real := filepath.Join(outside, "real")
				if err := os.MkdirAll(real, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, filepath.Join(agentDir, "linked-elsewhere")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// fakeAgent reserves ".system" (see scan_test.go); ScanAgent
			// excludes it by name alone, regardless of what occupies it, so
			// it must never even be inspected, let alone touched.
			label: "reserved name occupied by real content",
			name:  ".system",
			plant: func(t *testing.T, agentDir, _, _ string) {
				if err := os.MkdirAll(filepath.Join(agentDir, ".system"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Collides with "occupied", a name fu.yaml tracks and has
			// disabled (see the test body's setup above) -- round 4
			// test-weakness finding: no fixture previously used a name
			// fu.yaml had any opinion on at all, so Diff's
			// ReportDisabledForeign arm (crossed with any op that leaves
			// "occupied" off) and its ReportConflict arm (crossed with the
			// "enable occupied skill" op below) were both dead code as far
			// as this invariant could tell.
			label: "plain directory occupying a name fu.yaml tracks and has disabled",
			name:  "occupied",
			plant: func(t *testing.T, agentDir, _, _ string) {
				d := filepath.Join(agentDir, "occupied")
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "notes.md"), []byte("hand-authored"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	// Every hop shape hopShapes enumerates, planted at "notes" -- a name
	// fu.yaml has no opinion on at all, matching every hop reproduction
	// reported so far. See hopShapes' own doc comment for what each shape
	// stands in for and which review round found it unguarded.
	for _, sh := range hopShapes() {
		fixtures = append(fixtures, hopFixture(sh, "notes", "seed"))
	}
	// The directory-position shapes only, replanted at "occupied" instead
	// of "notes", and aliased through "occupied" itself rather than
	// "seed": this is the most destructive direction (round 4
	// test-weakness finding) -- crossed with the "enable occupied skill"
	// write op below, a misclassification here does not just fail to
	// report something, it deletes the user's own hop and replaces it
	// with a genuine fu link (RemoveLink + CreateLink; occupied's own
	// store-side directory still exists, so the create half would
	// actually succeed instead of being caught by the Missing bookkeeping
	// in reconcile.go). The leaf-position control is deliberately not
	// repeated here for hops outside the store: round 3 already covers its
	// own collision behavior at "notes" above.
	//
	// A leaf hop planted *inside* store/skills is the exception, and round 13
	// found that exclusion is what kept round 3's own Critical fix
	// untested. ownsLink compares the resolved target against
	// storeSkillsDir/<entryName>, so a hand-edited store/skills/<hop> ->
	// <skill> alias only becomes a misclassification when the alias names the
	// same skill the entry is named after -- which is this pairing and not the
	// "notes"/"seed" one. Resolving the final component then lands exactly on
	// want, and the user's own link is deleted and replaced.
	for _, sh := range hopShapes() {
		if !sh.directoryPosition && !sh.location.insideStore {
			continue
		}
		fixtures = append(fixtures, hopFixture(sh, "occupied", "occupied"))
	}
	return fixtures
}

// hopShape parameterizes one way a user-built symlink hop can end up
// resolving into the store, as an explicit cross product of every axis the
// ownership decision actually reads -- not a hand-written list of shapes
// someone has already thought of. Rounds 3, 4 and 5 each found the same
// failure in the direction the previous round's own fix left unguarded, and
// each time this file's fixtures had held the deciding input constant.
//
// The axes:
//
//   - position: where the hop sits relative to the entry's own final path
//     component (leaf vs. directory), rounds 3 and 4 respectively;
//   - spelling: the hop symlink's own raw path text (see hopSpellings) --
//     round 5's axis, and the one this fixture used to hardcode;
//   - depth: what a directory-position hop aliases (the skills directory
//     itself, or the store root reached through a literal, unresolved
//     "skills" component the alias does not cover);
//   - location: whether the hop lives in a directory of the user's own or
//     inside the agent's own skills directory.
//
// Round 5's test-weakness finding is what forced the spelling axis: the
// gate round 4 added reads exactly one input -- the raw, unresolved text of
// the target's directory half -- and hopFixture hardcoded the hop's own
// path as "hopdir", a spelling that gate rejects unconditionally. So the
// gate was never once handed a path it would accept, and round 5's
// Critical (a hop whose own path ends with the store's two trailing
// components) had no fixture at all, on a fully green suite.
type hopShape struct {
	label string
	// spelling is the hop symlink's own path, relative to whichever
	// directory it lives in. It may be several components deep, in which
	// case the intermediate components are real directories the user made
	// (hopFixture creates them). See hopSpellings.
	spelling string
	// directoryPosition is false for the leaf-position control (the hop
	// symlink IS what the agent-directory entry points at, round 3's own
	// shape, which must keep surviving) and true for every
	// directory-position shape (the hop is an intermediate directory; a
	// plain, unresolved leaf component is appended after it, round 4's
	// shape).
	directoryPosition bool
	// location decides which directory the hop symlink itself lives in.
	// This is the axis round 13 found held constant: every location used to
	// sit outside $FU_HOME, so prefixResolvesToStoreHome rejected all of
	// them on its own and dirHasStoreSkillsSuffix was never the deciding
	// input -- leaving DESIGN §2's "两道缺一不可" gate one deletable with a
	// green suite.
	location hopLocation
	// viaStoreRoot makes a directory-position hop alias the store *root*
	// rather than its skills directory, so the final target reaches the
	// skill through a literal, unresolved "skills" component the alias
	// itself does not cover. Meaningless for the leaf position (a
	// leaf-position hop aliases one skill directory outright), so it is
	// crossed only with directoryPosition.
	viaStoreRoot bool
}

// hopSpellings enumerates the axis round 4's gate actually reads and this
// fixture used to hold constant: the raw text of the hop symlink's own
// path. Every entry is a name the *user* chose for their own symlink, and
// none of them says anything about who created the agent-directory entry
// pointing through it -- which is the only question ownership turns on. A
// criterion that answers differently for two of these is filtering on a
// coincidence of the user's own naming, not deciding ownership.
// Labels are deliberately terse tokens rather than prose: the full
// subtest name becomes a t.TempDir() directory name, and descriptive
// labels here overran the filesystem's name limit, turning whole shapes
// into setup errors instead of assertions.
func hopSpellings() []struct{ label, path string } {
	return []struct{ label, path string }{
		{"hopdir", "hopdir"},
		// Round 5's Critical: the hop's own raw path already ends with the
		// store's two trailing components, so round 4's suffix gate accepts
		// it and resolves straight through the hop into the store.
		{"store+skills", filepath.Join("store", "skills")},
		// The most natural command spelling of all: `mkdir ~/backup &&
		// ln -s "$FU_HOME/store" ~/backup/` keeps the basename, so the hop
		// lands at ~/backup/store and a target of ~/backup/store/skills/alpha
		// ends with those same two components.
		{"store", "store"},
		// The same physical alias as the entry above, differing only in
		// case -- it must reach the same verdict, or the criterion is
		// filtering on the user's own spelling rather than deciding
		// ownership.
		{"STORE", "STORE"},
		{"skills", "skills"},
		{"store+skills+hopdir", filepath.Join("store", "skills", "hopdir")},
	}
}

// hopLocation is where the hop symlink itself is planted. The two gates in
// resolveTargetDir are only both consulted for certain locations, so this axis
// decides which gate a shape actually exercises.
type hopLocation struct {
	label string
	// root is the directory the hop symlink is created in.
	root func(t *testing.T, agentDir, storeSkillsDir string) string
	// relativeAlias points the hop at its target by a relative path. Required
	// inside the store, where an absolute symlink is refused outright by
	// checkNoAbsoluteSymlinks -- every write command would fail on the fixture
	// itself rather than on the property under test.
	relativeAlias bool
	// leafOnly restricts the location to the leaf position.
	leafOnly bool
	// insideStore marks a location within the store's own skills directory.
	// Such a hop is only a misclassification when its alias names the same
	// skill as the agent-directory entry does, so it has to be crossed with
	// the entry-name-matching pairing below as well as the "notes" one.
	insideStore bool
	// allows filters spellings that cannot be planted at this root, either
	// because the name is already taken by the real store or because planting
	// it there would put extra content inside the store.
	allows func(spelling string) bool
}

func hopLocations() []hopLocation {
	return []hopLocation{
		{
			label: "usrdir",
			root:  func(t *testing.T, _, _ string) string { return t.TempDir() },
		},
		{
			label: "agentdir",
			root:  func(_ *testing.T, agentDir, _ string) string { return agentDir },
		},
		{
			// $FU_HOME itself, which SPEC §4 sanctions as 本机自用: a user may
			// keep their own directories and links here. This is the location
			// that makes prefixResolvesToStoreHome pass -- the target's
			// directory becomes $FU_HOME plus two components -- so
			// dirHasStoreSkillsSuffix becomes the deciding gate for the first
			// time.
			label: "fuhome",
			root: func(_ *testing.T, _, storeSkillsDir string) string {
				return filepath.Dir(filepath.Dir(storeSkillsDir))
			},
			// "store"/"STORE"/"store/skills" name the real store directory,
			// which already exists, and "store/skills/hopdir" would plant the
			// hop inside it.
			allows: func(spelling string) bool { return !strings.HasPrefix(strings.ToLower(spelling), "store") },
		},
		{
			// Inside the real skills directory: a hand-edited
			// store/skills/<name> -> <other skill> alias, which is the shape
			// round 3's Critical fix exists for. It is only decisive at the
			// leaf position, where the question is whether the final component
			// is resolved.
			label:         "storeskills",
			root:          func(_ *testing.T, _, storeSkillsDir string) string { return storeSkillsDir },
			relativeAlias: true,
			leafOnly:      true,
			insideStore:   true,
			allows:        func(spelling string) bool { return !strings.ContainsRune(spelling, filepath.Separator) },
		},
	}
}

func hopShapes() []hopShape {
	locations := hopLocations()
	depths := []struct {
		label        string
		viaStoreRoot bool
	}{
		{"skillsdir", false},
		{"storeroot", true},
	}
	var out []hopShape
	for _, sp := range hopSpellings() {
		for _, loc := range locations {
			if loc.allows != nil && !loc.allows(sp.path) {
				continue
			}
			out = append(out, hopShape{
				label:    fmt.Sprintf("leaf/%s/%s", sp.label, loc.label),
				spelling: sp.path,
				location: loc,
			})
			if loc.leafOnly {
				continue
			}
			for _, d := range depths {
				out = append(out, hopShape{
					label:             fmt.Sprintf("dir/%s/%s/%s", sp.label, d.label, loc.label),
					spelling:          sp.path,
					directoryPosition: true,
					location:          loc,
					viaStoreRoot:      d.viaStoreRoot,
				})
			}
		}
	}
	return out
}

// hopAlias builds what the hop symlink itself points at.
func (sh hopShape) hopAlias(storeSkillsDir, throughSkill string) string {
	switch {
	case !sh.directoryPosition && sh.location.relativeAlias:
		// A sibling inside the skills directory itself.
		return throughSkill
	case !sh.directoryPosition:
		return filepath.Join(storeSkillsDir, throughSkill)
	case sh.viaStoreRoot:
		return filepath.Dir(storeSkillsDir)
	default:
		return storeSkillsDir
	}
}

// finalTarget builds the raw target text of the agent-directory entry
// itself, given the hop's own path and the name of the skill being aliased
// through.
func (sh hopShape) finalTarget(hop, throughSkill string) string {
	switch {
	case !sh.directoryPosition:
		return hop
	case sh.viaStoreRoot:
		return filepath.Join(hop, "skills", throughSkill)
	default:
		return filepath.Join(hop, throughSkill)
	}
}

// hopFixture builds one foreignFixture for shape sh: a symlink entry named
// entryName whose raw target is a user-built hop resolving toward
// storeSkillsDir/throughSkill -- landing exactly on a real skill directory
// the store actually holds, the same as every reviewer reproduction (an
// alias that resolves nowhere real would at least be caught by
// reconcile.go's own Missing bookkeeping; one that resolves to real
// content is not).
func hopFixture(sh hopShape, entryName, throughSkill string) foreignFixture {
	return foreignFixture{
		label: fmt.Sprintf("hop@%s %s", entryName, sh.label),
		name:  entryName,
		plant: func(t *testing.T, agentDir, storeSkillsDir, _ string) {
			hopRoot := sh.location.root(t, agentDir, storeSkillsDir)
			// The hop's spelling may be several components deep; the
			// components above it are ordinary directories the user made.
			hop := filepath.Join(hopRoot, sh.spelling)
			if err := os.MkdirAll(filepath.Dir(hop), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sh.hopAlias(storeSkillsDir, throughSkill), hop); err != nil {
				t.Fatal(err)
			}
			target := sh.finalTarget(hop, throughSkill)
			if err := os.Symlink(target, filepath.Join(agentDir, entryName)); err != nil {
				t.Fatal(err)
			}
		},
	}
}

// writeOp is one write command this plan ships, reduced to what this test
// needs to invoke it: a fixed, already-registered "seed" skill (created by
// the test body before any fixture is planted) is the target for the two
// toggle operations; NewSkill instead registers a brand new, unrelated
// name, so all three operations run a real reconcile pass over whatever
// foreign fixture is sitting alongside "seed" in the agent directory.
//
// "enable occupied skill" (round 4 test-weakness finding) is the odd one
// out: the other three either disable something or create an unrelated
// name, so none of them ever asks Diff to *create* a link at a position
// foreign content already occupies -- the most destructive direction,
// since a misclassified entry there is not just left unreported but
// actively removed and replaced. "occupied" is registered (disabled) by
// the test body before every fixture is planted, so this op has something
// to flip on regardless of which fixture is under test in a given subtest.
type writeOp struct {
	label string
	run   func(s *store.Store, agents []agent.Agent) (Result, error)
}

func writeOps() []writeOp {
	return []writeOp{
		{"new unrelated skill", func(s *store.Store, agents []agent.Agent) (Result, error) {
			return NewSkill(s, agents, "freshly-added")
		}},
		{"disable seed globally", func(s *store.Store, agents []agent.Agent) (Result, error) {
			return SetGlobal(s, agents, "seed", false)
		}},
		{"disable seed for claude", func(s *store.Store, agents []agent.Agent) (Result, error) {
			return SetAgentSwitch(s, agents, "seed", "claude", false)
		}},
		{"enable occupied skill", func(s *store.Store, agents []agent.Agent) (Result, error) {
			return SetGlobal(s, agents, "occupied", true)
		}},
	}
}
