package engine

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/source"
	git "github.com/go-git/go-git/v5"
)

// writeSkillBody plants a valid skill directly at dir, holding canonical
// frontmatter for name and the given body.
//
// It is the counterpart of writeSkillTree (add_test.go), not a duplicate of
// it, and both are needed: writeSkillTree varies the frontmatter over a fixed
// body and creates the skill *inside* a parent directory, while these tests
// need the reverse -- a fixed, valid frontmatter with a varying body, so one
// skill name yields different digests across versions -- and must address the
// skill directory itself, because a local source root is itself a skill in
// the update fixtures.
func writeSkillBody(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: " + name + "\ndescription: test skill\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realGitBranchFixture builds a real, on-disk bare git repository (no
// network involved -- makeGitSourceNamed uses a file:// remote) and returns
// its URL together with the fully qualified ref name and the commit its
// HEAD actually resolves to. The ref name is discovered from the repository
// itself rather than assumed, so these tests do not depend on go-git's
// default branch name.
func realGitBranchFixture(t *testing.T, skillName string) (url, ref, commit string) {
	t.Helper()
	url = makeGitSourceNamed(t, false, skillName)
	bareRepo, err := git.PlainOpen(strings.TrimPrefix(url, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := bareRepo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return url, head.Name().String(), head.Hash().String()
}

// TestOutdatedTreatsAnEmptyRecordedCommitAsNotComparable pins round 1 review
// finding #2: EncodeFields never writes an empty "commit" for a branch
// source, but fu.yaml is a hand-editable file (the same reasoning
// judgeUpdate's own default: arm already relies on), so the record can
// still arrive with the field blank. Comparing "" against a real resolved
// commit made every such row silently report Updatable=true with nothing in
// its Current column.
func TestOutdatedTreatsAnEmptyRecordedCommitAsNotComparable(t *testing.T) {
	s, cfg := setupStore(t)
	url, ref, _ := realGitBranchFixture(t, "half-locked")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "half-locked"), "half-locked", "one")
	if err := cfg.AddSkill("half-locked", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("half-locked", map[string]string{
		"type": "git", "url": url, "ref": ref, "ref_kind": "branch",
		// Deliberately no "commit" field: the shape a hand-edited or
		// otherwise incomplete fu.yaml record can carry. The remote is real
		// and resolvable, so a false "Updatable" here could only come from
		// comparing "" against the real resolved commit, not from an
		// unrelated network failure.
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if got[0].Comparable {
		t.Fatalf("an absent locked commit must not be reported comparable: %+v", got[0])
	}
	if got[0].Updatable {
		t.Fatalf("a non-comparable row must never claim an update is available: %+v", got[0])
	}
	if got[0].Reason == "" {
		t.Fatal("a non-comparable row must say why")
	}
}

// TestOutdatedResolvesAGitSourceOnceForManySkillsAndSortsByName is the
// permanent covering test for the branch-resolve path, the dedup caches and
// the timeout wiring (round 1 review finding #1): none of the other tests in
// this file ever reach it -- the tag-lock test returns before any network
// code runs, and every local-source test takes the other switch arm
// entirely -- so the load-bearing claim "one repository serving N skills
// issues ONE network call" previously had no evidence beyond a probe that
// was written, run, and discarded before committing.
//
// resolveRemoteRefHook (outdated.go) is the counting seam this test uses,
// mirroring lockAcquiredHook's own pattern (lock.go): nil in production,
// firing on the exact call production already makes, never a second,
// test-only code path.
//
// Registering the three skills out of Name order also exercises Outdated's
// own sort contract (outdated.go's sort.Slice call), which later tasks
// (Task 3, Task 9) depend on: a passing assertion here cannot be
// coincidental since the insertion order is deliberately scrambled.
func TestOutdatedResolvesAGitSourceOnceForManySkillsAndSortsByName(t *testing.T) {
	s, cfg := setupStore(t)
	url, ref, wantCommit := realGitBranchFixture(t, "shared")

	for _, name := range []string{"gamma", "alpha", "beta"} {
		writeSkillBody(t, filepath.Join(s.SkillsDir(), name), name, "one")
		if err := cfg.AddSkill(name, "sha256:x"); err != nil {
			t.Fatal(err)
		}
		cfg.SetSourceFields(name, map[string]string{
			"type": "git", "url": url, "ref": ref, "ref_kind": "branch",
			// A placeholder guaranteed to differ from the real HEAD, so
			// Updatable is unambiguously true for every row below.
			"commit": strings.Repeat("0", 40),
		})
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var calls []string
	resolveRemoteRefHook = func(gotURL, gotRef string) { calls = append(calls, gotURL+"@"+gotRef) }
	t.Cleanup(func() { resolveRemoteRefHook = nil })

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("three skills sharing one (url, ref) must resolve exactly once, got %d calls: %v", len(calls), calls)
	}

	wantNames := []string{"alpha", "beta", "gamma"}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(wantNames), got)
	}
	for i, row := range got {
		if row.Name != wantNames[i] {
			t.Fatalf("rows not sorted by Name ascending at index %d: got %+v", i, got)
		}
		if !row.Comparable {
			t.Fatalf("row %+v should be comparable against a real, resolvable repo", row)
		}
		if row.Latest != wantCommit {
			t.Fatalf("row %+v: Latest = %q, want the real resolved commit %q", row, row.Latest, wantCommit)
		}
		if !row.Updatable {
			t.Fatalf("row %+v: locked commit is a placeholder and must differ from the real HEAD", row)
		}
	}
}

func TestOutdatedSkipsASkillWithNoSourceRecord(t *testing.T) {
	s, cfg := setupStore(t)
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "homegrown"), "homegrown", "one")
	if err := cfg.AddSkill("homegrown", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Comparable || got[0].Updatable {
		t.Fatalf("row = %+v, want not comparable", got[0])
	}
	if got[0].Reason == "" {
		t.Fatal("a non-comparable row must say why")
	}
}

// TestOutdatedTreatsAFixedLockAsFixed covers SPEC rule 9's "tag 来源与已有记录中的
// commit-pinned lock 视为固定，不参与 outdated" -- both kinds, which design §2.2
// lists as separate matrix rows.
//
// Rewritten for round 2's Important #5, which found the previous version
// tautological: it pointed at `https://example.invalid/r` and asserted only
// `Comparable == false`, so deleting the guard entirely (`if false {`) still
// passed -- the fixed-lock refusal was simply replaced by an unreachable-remote
// failure that is also not comparable, and the mutated build started making a
// real outbound DNS attempt from the test suite.
//
// Three things make it bite now. The remote is a real, reachable file://
// repository that genuinely advertises the ref, so removing the guard yields a
// *comparable* row rather than another failure. The Reason is asserted, so the
// two cannot be confused. And resolveRemoteRefHook must never fire, which is
// the rule itself stated directly: a fixed lock does not participate, so no
// network read may happen on its behalf at all.
func TestOutdatedTreatsAFixedLockAsFixed(t *testing.T) {
	for _, tc := range []struct {
		label   string
		refKind string
		ref     func(branchRef string) string
	}{
		{label: "tag", refKind: "tag", ref: func(string) string { return "refs/tags/v1.2.3" }},
		// A commit-pinned lock as an existing record carries it: `fu add --ref`
		// refuses a commit hash (ParseArgWithRef), so this shape only ever
		// arrives from an older record or a hand edit -- and the ref it names is
		// deliberately a live branch, so a dropped guard would resolve it.
		{label: "commit", refKind: "commit", ref: func(branchRef string) string { return branchRef }},
	} {
		t.Run(tc.label, func(t *testing.T) {
			s, cfg := setupStore(t)
			url := makeGitSourceNamed(t, true, "pinned")
			bare, err := git.PlainOpen(strings.TrimPrefix(url, "file://"))
			if err != nil {
				t.Fatal(err)
			}
			head, err := bare.Head()
			if err != nil {
				t.Fatal(err)
			}
			writeSkillBody(t, filepath.Join(s.SkillsDir(), "pinned"), "pinned", "one")
			if err := cfg.AddSkill("pinned", "sha256:x"); err != nil {
				t.Fatal(err)
			}
			cfg.SetSourceFields("pinned", map[string]string{
				"type": "git", "url": url,
				"ref": tc.ref(head.Name().String()), "ref_kind": tc.refKind,
				// Differs from anything the remote could resolve to, so a row
				// that reached the network would come back Updatable.
				"commit": strings.Repeat("0", 40),
			})
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}

			var calls []string
			resolveRemoteRefHook = func(gotURL, gotRef string) { calls = append(calls, gotURL+"@"+gotRef) }
			t.Cleanup(func() { resolveRemoteRefHook = nil })

			got, err := Outdated(s, cfg)
			if err != nil {
				t.Fatalf("Outdated: %v", err)
			}
			if got[0].Comparable || got[0].Updatable {
				t.Fatalf("a fixed lock must not be comparable or updatable: %+v", got[0])
			}
			if got[0].Reason != "source ref is a fixed lock" {
				t.Fatalf("Reason = %q, want the fixed-lock reason", got[0].Reason)
			}
			if len(calls) != 0 {
				t.Fatalf("a fixed lock must not reach the remote at all, resolved: %v", calls)
			}
		})
	}
}

// The fixed-lock arm above answered for every non-branch value, including ones
// no writer of fu.yaml ever produces. A hand-edited `ref_kind: banana` was then
// described as "a fixed lock" -- a deliberate pin it never was -- and, worse,
// marked NoUpstream, which is what selectUpdateTargets reads to drop a row from
// the batch report entirely. So `fu update` said nothing at all about that row
// while saying `could not judge badtype: unrecognized source type "svn"` about
// its exact sibling one field over. Both are hand edits the user can fix and
// should keep hearing about, which is the reason the empty-ref_kind arm above
// already gives for staying unmarked; this is the same rule applied to the
// neighbouring value, and it matches judgeUpdate's own default: arm.
func TestOutdatedNamesAnUnrecognizedRefKindRatherThanCallingItAFixedLock(t *testing.T) {
	var status UpdateStatus
	judgeGitUpdate(&status, map[string]string{
		"type": "git", "url": "file:///nowhere", "ref": "release",
		"ref_kind": "banana", "commit": strings.Repeat("0", 40),
	}, map[[2]string]string{}, map[[2]string]error{})

	if status.Comparable || status.Updatable {
		t.Fatalf("an unrecognized ref kind is not comparable: %+v", status)
	}
	if status.NoUpstream {
		t.Fatal("an unrecognized ref kind is a hand edit, not a pin: NoUpstream must stay false so the batch keeps reporting the row")
	}
	if !strings.Contains(status.Reason, `unrecognized ref kind "banana"`) {
		t.Fatalf("Reason = %q, want the unrecognized kind named", status.Reason)
	}
}

// The arm the test above holds up as its contrast, which was itself never
// asserted: judgeUpdate's default:. Two mutations of it survived the whole
// suite -- setting NoUpstream, which drops the row from `fu update`'s batch
// report forever, and clearing Reason, which is the exact outcome the arm's own
// comment says it exists to prevent ("a row must always say why it is not
// comparable"). Same shape as its sibling because it is the same rule one field
// over: a hand-edited type is a repairable record, so the row stays unmarked
// and keeps being reported.
func TestOutdatedNamesAnUnrecognizedSourceTypeRatherThanFailingSilently(t *testing.T) {
	var status UpdateStatus
	judgeUpdate(&status, map[string]string{"type": "svn", "url": "svn://nowhere"},
		"sha256:x", map[[2]string]string{}, map[[2]string]error{})

	if status.Comparable || status.Updatable {
		t.Fatalf("an unrecognized source type is not comparable: %+v", status)
	}
	if status.NoUpstream {
		t.Fatal("an unrecognized source type is a hand edit, not a decision: NoUpstream must stay false so the batch keeps reporting the row")
	}
	if !strings.Contains(status.Reason, `unrecognized source type "svn"`) {
		t.Fatalf("Reason = %q, want the unrecognized type named", status.Reason)
	}
}

// judgeGitUpdate guarded ref_kind and commit but never url, and
// ResolveRemoteRef guards an empty *ref* but not an empty *URL*. go-git's
// transport.NewEndpoint("") falls into parseFile, which does filepath.Abs("")
// -- the process's working directory -- and hands back a file endpoint. So a
// record with no url resolved against whatever repository fu happened to be run
// from: `fu outdated` printed a verdict that depended on $PWD, and `fu update`
// would then publish that repository's content into the store and link it into
// every agent. Any relative url (../other-repo) is the same defect.
//
// Not reachable from fu's own writes -- EncodeFields always writes url and
// ParseArg cannot produce an empty-URL git source -- but reachable by exactly
// the hand edit or partial YAML damage the two adjacent guards already exist
// for, and `fu outdated`'s entire value is an honest verdict.
func TestOutdatedRefusesAGitRecordWithNoUsableURL(t *testing.T) {
	for _, tc := range []struct{ label, url string }{
		{label: "missing", url: ""},
		{label: "relative", url: "../other-repo"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var calls []string
			resolveRemoteRefHook = func(gotURL, gotRef string) { calls = append(calls, gotURL+"@"+gotRef) }
			t.Cleanup(func() { resolveRemoteRefHook = nil })

			var status UpdateStatus
			judgeGitUpdate(&status, map[string]string{
				"type": "git", "url": tc.url, "ref": "refs/heads/main",
				"ref_kind": "branch", "commit": strings.Repeat("a", 40),
			}, map[[2]string]string{}, map[[2]string]error{})

			if status.Comparable || status.Updatable {
				t.Fatalf("a record with no usable url is not comparable: %+v", status)
			}
			if !strings.Contains(status.Reason, "url") {
				t.Fatalf("Reason = %q, want it to name the url as the problem", status.Reason)
			}
			// The load-bearing assertion: the verdict must not depend on what
			// the working directory happens to be, which means no resolve may
			// be attempted at all.
			if len(calls) != 0 {
				t.Fatalf("a record with no usable url must not reach any remote: %v", calls)
			}
		})
	}
}

// The local arm's counterpart to the url guard above, and the wider of the two:
// every `file:` URL form is $PWD-independent (go-git parses `file://../repo` as
// host ".." with path "/repo"), while `os.Stat(path)` and `os.OpenRoot(path)`
// here -- and openPreparedRoot on the update side -- resolve a relative path
// against the process's working directory outright. So `fu outdated` answered
// differently depending on where it was run, and `fu update` then published
// content from a cwd-relative directory into the store.
//
// DESIGN.md already states the invariant this restores (「local 来源的 `path` 为
// 绝对路径」); nothing enforced it on the read side. Reachable by hand edit or
// partial YAML damage only -- parseExistingLocal runs filepath.Abs and
// EvalSymlinks -- which is the same reachability the three adjacent git guards
// exist for.
//
// NoUpstream stays false for the same reason it does on the empty-ref_kind arm:
// this is a record the user can repair and should keep hearing about. The
// wording is checked not to be "unreachable", which is what a malformed record
// used to be labelled -- the same misdirection ErrRemoteRefUnusable was added to
// remove on the git side, applied inconsistently one field over.
func TestOutdatedRefusesALocalRecordWithNoUsablePath(t *testing.T) {
	for _, tc := range []struct{ label, path string }{
		{label: "missing", path: ""},
		{label: "relative", path: "../other-repo"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var status UpdateStatus
			judgeLocalUpdate(&status, map[string]string{"type": "local", "path": tc.path}, "sha256:x")

			if status.Comparable || status.Updatable {
				t.Fatalf("a record with no usable path is not comparable: %+v", status)
			}
			if status.NoUpstream {
				t.Fatal("a hand-edited path is repairable and must keep being reported")
			}
			if !strings.Contains(status.Reason, "path") {
				t.Fatalf("Reason = %q, want it to name the path as the problem", status.Reason)
			}
			if strings.Contains(status.Reason, "unreachable") {
				t.Fatalf("a malformed record is not a reachability failure: %q", status.Reason)
			}
		})
	}
}

// The IsAbs guard above closed one half of a rule `fu update` applies and left
// the other open. openPreparedRoot opens the recorded path with O_NOFOLLOW
// (internal/source/git.go), so update refuses a symlinked source root outright,
// while os.Stat and os.OpenRoot here both follow a link at the leaf and judged
// straight through it. A user who moves a source directory and symlinks it back
// -- an ordinary thing to do -- got `fu outdated` reporting the skill updatable
// on every run and `fu update` failing with "not a directory" on every run: the
// permanent-noise class the design says must be designed out, and unlike the
// one instance it records as unavoidable, this one is fixable.
//
// Prepare is called on the same path rather than described, so the two sides
// cannot drift apart again without this test saying so. NoUpstream stays false
// for the reason the IsAbs guard gives: the record is repairable and the user
// should keep hearing about it.
func TestOutdatedRefusesALocalSourceRootThatIsASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	writeSkillBody(t, target, "alpha", "one")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// The other side of the disagreement, asserted first so a Prepare that
	// stopped refusing would be reported as what it is.
	if _, err := (source.Source{Kind: source.KindLocal, Path: link}).Prepare(t.TempDir()); err == nil {
		t.Fatal("fixture error: update's own prepare must refuse a symlinked source root")
	}

	var status UpdateStatus
	judgeLocalUpdate(&status, map[string]string{"type": "local", "path": link}, "sha256:x")

	if status.Comparable || status.Updatable {
		t.Fatalf("a source root update refuses to open is not comparable: %+v", status)
	}
	if status.NoUpstream {
		t.Fatal("a symlinked source root is repairable and must keep being reported")
	}
	if !strings.Contains(status.Reason, "symlink") {
		t.Fatalf("Reason = %q, want it to name the symlink as the cause", status.Reason)
	}
}

// A local source whose recorded path is gone is reported honestly rather than
// guessed at (SPEC rule 9).
func TestOutdatedReportsAMissingLocalSourcePath(t *testing.T) {
	s, cfg := setupStore(t)
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "gone"), "gone", "one")
	if err := cfg.AddSkill("gone", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("gone", map[string]string{
		"type": "local", "path": filepath.Join(t.TempDir(), "not-here"),
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if got[0].Comparable {
		t.Fatalf("row = %+v, want not comparable", got[0])
	}
}

// The local case that is comparable: the source path exists and its content has
// moved away from the recorded install baseline.
func TestOutdatedReportsALocalSourceThatMovedAheadOfTheBaseline(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	writeSkillBody(t, srcDir, "local-kit", "version one")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "local-kit"), "local-kit", "version one")

	// Baseline is the store's own content, so store side == baseline.
	//
	// SnapshotSkillPayload only works through pinned, checked roots (a write
	// session; see ownedtree.go), which setupStore's plain store.Init does not
	// hold -- confirmed by running this call directly against setupStore's
	// store, which fails with "store is not attached to a checked skills
	// root". Outdated itself opens exactly such a session internally to make
	// the same call safely (see outdated.go); this scaffolding borrows the
	// same pattern through the existing checkedRecoveryStore fixture
	// (reconcile_test.go) rather than reinventing it, since a plain
	// s.SnapshotSkillPayload call here cannot succeed at all.
	payload, err := checkedRecoveryStore(t, s).SnapshotSkillPayload("local-kit")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := digestOwnedPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddSkill("local-kit", baseline); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("local-kit", map[string]string{"type": "local", "path": srcDir})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// Move the source ahead of the baseline.
	writeSkillBody(t, srcDir, "local-kit", "version two")

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if !got[0].Comparable || !got[0].Updatable {
		t.Fatalf("row = %+v, want comparable and updatable", got[0])
	}
	if got[0].LocallyModified {
		t.Fatalf("store side matches the baseline; LocallyModified must be false: %+v", got[0])
	}
}

// The two judgements must not be conflated (SPEC rule 9): editing the store
// copy is a local modification, not an available update.
func TestOutdatedSeparatesLocalModificationFromAvailableUpdate(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	writeSkillBody(t, srcDir, "kit", "same")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "same")

	// See the comment in TestOutdatedReportsALocalSourceThatMovedAheadOfTheBaseline
	// above for why this goes through checkedRecoveryStore rather than s directly.
	payload, err := checkedRecoveryStore(t, s).SnapshotSkillPayload("kit")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := digestOwnedPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddSkill("kit", baseline); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("kit", map[string]string{"type": "local", "path": srcDir})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// Hand-edit the store copy only. The source still matches the baseline.
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "hand edited")

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	// Comparable must be asserted explicitly, not inferred from Updatable
	// being false: a local source that bails out (missing path, unreadable
	// projection, ...) also reports Updatable=false, which would let this
	// test -- the one guarding against conflating the two SPEC rule 9
	// judgements -- pass vacuously on a regression that stops judging
	// anything at all.
	if !got[0].Comparable {
		t.Fatalf("a local source that still matches its baseline must be comparable, not bailed out: %+v", got[0])
	}
	if got[0].Updatable {
		t.Fatalf("the source did not move; Updatable must be false: %+v", got[0])
	}
	if !got[0].LocallyModified {
		t.Fatalf("the store copy was edited; LocallyModified must be true: %+v", got[0])
	}
}

// TestOutdatedPopulatesKindRefAndPathFromTheSourceRecord pins fix round 1's
// authorized struct extension: Kind, Ref, Path and Subdir are raw passthroughs
// of the source record, populated unconditionally in Outdated's own loop before
// judgeUpdate runs, alongside whatever it separately decides. Subdir joined the
// same struct block later and was left out of this test until round 8, where
// deleting its assignment survived the whole engine and cli suites --
// TestOutdatedCommandShowsALocalRowsSubdir builds its rows by hand, so it pins
// the renderer and not the passthrough. `fu outdated`'s
// CLI renderer needs them to show a git row's ref or a local row's path
// instead of an opaque commit/digest pair with no meaning to a reader (fix
// round 1 Important #1) -- this is the engine-side half of that fix: proof
// that the real fields actually reach UpdateStatus, not just that the CLI
// renders correctly given hand-picked values.
func TestOutdatedPopulatesKindRefAndPathFromTheSourceRecord(t *testing.T) {
	s, cfg := setupStore(t)
	url, ref, _ := realGitBranchFixture(t, "git-kit")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "git-kit"), "git-kit", "one")
	if err := cfg.AddSkill("git-kit", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("git-kit", map[string]string{
		"type": "git", "url": url, "ref": ref, "ref_kind": "branch",
		"commit": strings.Repeat("0", 40), "subdir": "packs/git-kit",
	})

	localPath := filepath.Join(t.TempDir(), "src")
	writeSkillBody(t, localPath, "local-kit", "one")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "local-kit"), "local-kit", "one")
	if err := cfg.AddSkill("local-kit", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("local-kit", map[string]string{"type": "local", "path": localPath})

	writeSkillBody(t, filepath.Join(s.SkillsDir(), "homegrown"), "homegrown", "one")
	if err := cfg.AddSkill("homegrown", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	// No SetSourceFields call: homegrown has no source record at all.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	byName := make(map[string]UpdateStatus, len(got))
	for _, row := range got {
		byName[row.Name] = row
	}

	gitRow := byName["git-kit"]
	if gitRow.Kind != "git" || gitRow.Ref != ref || gitRow.Path != "" || gitRow.Subdir != "packs/git-kit" {
		t.Fatalf("git row = %+v, want Kind=\"git\" Ref=%q Path=\"\" Subdir=\"packs/git-kit\"", gitRow, ref)
	}
	localRow := byName["local-kit"]
	// Subdir absent from the record stays empty, the same way Ref does: the
	// passthrough copies what is written, and "" is what "no subdir" is.
	if localRow.Kind != "local" || localRow.Path != localPath || localRow.Ref != "" || localRow.Subdir != "" {
		t.Fatalf("local row = %+v, want Kind=\"local\" Path=%q Ref=\"\" Subdir=\"\"", localRow, localPath)
	}
	// The no-source-record case: all three fields stay at their zero value,
	// matching UpdateStatus's own doc comment (`Kind is "git", "local", or ""
	// when there is no source record at all`).
	homegrownRow := byName["homegrown"]
	if homegrownRow.Kind != "" || homegrownRow.Ref != "" || homegrownRow.Path != "" || homegrownRow.Subdir != "" {
		t.Fatalf("homegrown row = %+v, want Kind/Ref/Path/Subdir all empty", homegrownRow)
	}
}

// TestOutdatedDegradesEverySkillOnOneDeadRemoteWithoutFailing is design §7's
// engine-side requirement -- "外加远端不可达的逐项降级" -- which fix round 2's
// Important #7 found untested. Three properties matter and none had a guard:
// the run still succeeds (a finding is not a failure, SPEC scenario 3), every
// skill on the dead remote degrades with a Reason of its own, and the failure
// is resolved once and attributed to all of them from the resolveErrs cache
// rather than re-dialled per skill.
//
// The cache is the load-bearing half. It is the one place a bug could mark
// other skills comparable off a stale entry, because it is keyed by (url,
// ref) and shared across every row.
func TestOutdatedDegradesEverySkillOnOneDeadRemoteWithoutFailing(t *testing.T) {
	s, cfg := setupStore(t)
	url, ref, commit := realGitBranchFixture(t, "shared")
	for _, name := range []string{"alpha", "beta"} {
		writeSkillBody(t, filepath.Join(s.SkillsDir(), name), name, "one")
		if err := cfg.AddSkill(name, "sha256:x"); err != nil {
			t.Fatal(err)
		}
		cfg.SetSourceFields(name, map[string]string{
			"type": "git", "url": url, "ref": ref, "ref_kind": "branch", "commit": commit,
		})
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// The remote goes away after the records were written, which is exactly
	// the shape a user offline (or with a deleted upstream) meets.
	if err := os.RemoveAll(strings.TrimPrefix(url, "file://")); err != nil {
		t.Fatal(err)
	}

	var calls []string
	resolveRemoteRefHook = func(gotURL, gotRef string) { calls = append(calls, gotURL+"@"+gotRef) }
	t.Cleanup(func() { resolveRemoteRefHook = nil })

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("an unreachable remote degrades its own rows and must not fail the run: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("one dead remote must be dialled once and cached for the rest, got %d calls: %v", len(calls), calls)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	for _, row := range got {
		if row.Comparable || row.Updatable {
			t.Fatalf("row %+v must not be comparable against a remote that could not be reached", row)
		}
		if !strings.Contains(row.Reason, "unreachable") {
			t.Fatalf("row %+v must say the remote was unreachable, not that the ref is gone", row)
		}
	}
}

// TestOutdatedWritesNothingToTheStore pins SPEC §9's read-only guarantee for
// this command, the load-bearing property of the whole thing: Outdated opens
// a write *session* (BeginWrite) to get the pinned descriptors
// SnapshotSkillPayload needs, and the argument that this is still read-only
// rests entirely on BeginWrite itself writing nothing. Fix round 2's
// Important #7: that argument lived only in a comment and a one-off manual
// check, one BeginWrite-adjacent refactor away from silently regressing.
//
// Every regular file under FU_HOME is hashed before and after, including the
// store's own .git: a stray commit, a lock file, an index rewrite or a
// journal entry all show up as a difference here.
func TestOutdatedWritesNothingToTheStore(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	writeSkillBody(t, srcDir, "kit", "version one")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "version one")
	if err := cfg.AddSkill("kit", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("kit", map[string]string{"type": "local", "path": srcDir})
	// A second skill whose source is gone, so the run also exercises the
	// degradation path while under observation.
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "gone"), "gone", "one")
	if err := cfg.AddSkill("gone", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("gone", map[string]string{"type": "local", "path": filepath.Join(t.TempDir(), "not-here")})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	writeSkillBody(t, srcDir, "kit", "version two") // something for it to report

	before := hashTree(t, s.Home)
	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("fixture error: got %d rows, want both skills judged: %+v", len(got), got)
	}
	after := hashTree(t, s.Home)
	if len(before) != len(after) {
		t.Fatalf("outdated changed what is under FU_HOME: %d entries before, %d after", len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("outdated wrote to %s: %q -> %q", path, sum, after[path])
		}
	}
}

// hashTree records every regular file under root by path and content hash,
// plus every symlink by its target, so a comparison across a command run
// catches a changed byte, a new file and a removed one alike.
func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			sums[rel] = "symlink:" + target
		case entry.Type().IsRegular():
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sums[rel] = fmt.Sprintf("%x", sha256.Sum256(content))
		default:
			sums[rel] = "dir"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

// TestOutdatedJudgesEachLocalSubdirSkillSeparately pins round 2's Important
// #4. `judgeLocalUpdate` reads the record's "subdir" and falls back to "." --
// and no engine fixture ever set that field on a local record, so replacing
// the whole lookup with `subdir := "."` left the suite green. With the subdir
// dropped, ProjectDir digests the entire source root, which can never equal
// any individual skill's baseline, so every skill installed from a
// multi-skill local directory reports permanently updatable -- exactly the
// noise design §2.5 rules out.
//
// `fu add <local dir>` over a directory holding several skills is the ordinary
// multi-skill local install, so this is the common shape, not an edge case.
func TestOutdatedJudgesEachLocalSubdirSkillSeparately(t *testing.T) {
	s, cfg := setupStore(t)
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		writeSkillBody(t, filepath.Join(root, name), name, "version one")
		writeSkillBody(t, filepath.Join(s.SkillsDir(), name), name, "version one")
		payload, err := checkedRecoveryStore(t, s).SnapshotSkillPayload(name)
		if err != nil {
			t.Fatal(err)
		}
		baseline, err := digestOwnedPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.AddSkill(name, baseline); err != nil {
			t.Fatal(err)
		}
		cfg.SetSourceFields(name, map[string]string{"type": "local", "path": root, "subdir": name})
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// Only alpha's own subdir moves ahead.
	writeSkillBody(t, filepath.Join(root, "alpha"), "alpha", "version two")

	rows, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	byName := map[string]UpdateStatus{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	if got := byName["alpha"]; !got.Comparable || !got.Updatable {
		t.Fatalf("alpha's own subdir moved and must be updatable: %+v", got)
	}
	if got := byName["beta"]; !got.Comparable || got.Updatable {
		t.Fatalf("beta's subdir did not move and must not be reported updatable: %+v", got)
	}
	if got := byName["beta"]; got.LocallyModified {
		t.Fatalf("beta's store copy matches its baseline: %+v", got)
	}
}

// TestOutdatedDoesNotCallAnIncompleteRecordAFixedLock pins round 2's Minor #6.
// EncodeFields writes ref_kind only when non-empty (source.go), so a record
// missing it is an incomplete or hand-edited one -- and the `!= "branch"` arm
// described it as a deliberate pin, wording `fu update <name>` then repeats as
// its refusal reason. The neighbouring missing-commit case already gets its own
// accurate message; this one now does too.
func TestOutdatedDoesNotCallAnIncompleteRecordAFixedLock(t *testing.T) {
	s, cfg := setupStore(t)
	url, ref, commit := realGitBranchFixture(t, "halfrecord")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "halfrecord"), "halfrecord", "one")
	if err := cfg.AddSkill("halfrecord", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("halfrecord", map[string]string{
		// No ref_kind at all: the shape an incomplete record carries.
		"type": "git", "url": url, "ref": ref, "commit": commit,
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if got[0].Comparable {
		t.Fatalf("an incomplete record must fail closed: %+v", got[0])
	}
	if strings.Contains(got[0].Reason, "fixed lock") {
		t.Fatalf("a record that says nothing must not be reported as a deliberate pin: %q", got[0].Reason)
	}
	if got[0].Reason == "" {
		t.Fatal("a non-comparable row must say why")
	}
}

// TestOutdatedFoldsAStoreSnapshotFailureOntoAJudgedRow pins round 3's
// Important #9's fourth row: mutating foldReason to `return reason` dropped a
// store-snapshot failure from the row entirely, and the only test of that path
// was a CLI test over a hand-built UpdateStatus. The row's source-side verdict
// must survive alongside the folded failure -- they are SPEC rule 9's two
// independent questions, and losing the second silently turns "unknown" into
// "no".
//
// The second skill covers foldReason's other branch, which round 8 found
// unexercised: with only the first, every fold lands on an empty Reason, so
// `return addition` -- dropping whatever the row already said -- survives the
// suite. A skill whose source is unreachable *and* whose store copy is gone is
// the ordinary shape of that: both of rule 9's questions fail, and the row must
// carry both answers rather than whichever ran last.
func TestOutdatedFoldsAStoreSnapshotFailureOntoAJudgedRow(t *testing.T) {
	s, cfg := setupStore(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	writeSkillBody(t, srcDir, "kit", "version one")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "version one")
	if err := cfg.AddSkill("kit", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("kit", map[string]string{"type": "local", "path": srcDir})
	// Neither half can answer for this one: the recorded source path does not
	// exist on this machine, and the store copy was never written.
	if err := cfg.AddSkill("both-halves", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("both-halves", map[string]string{
		"type": "local", "path": filepath.Join(t.TempDir(), "absent"),
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// The source still answers, so judgeUpdate succeeds; the store copy is gone,
	// so judgeLocalModification cannot.
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "kit")); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	byName := make(map[string]UpdateStatus, len(got))
	for _, row := range got {
		byName[row.Name] = row
	}

	kit := byName["kit"]
	if !kit.Comparable {
		t.Fatalf("the source-side verdict must survive the store-side failure: %+v", kit)
	}
	if !strings.Contains(kit.Reason, "snapshot store content") {
		t.Fatalf("the folded failure must reach the row: %+v", kit)
	}
	if kit.LocallyModified {
		t.Fatalf("an unanswerable question must not be answered no: %+v", kit)
	}

	both := byName["both-halves"]
	for _, want := range []string{"local source unreachable", "snapshot store content"} {
		if !strings.Contains(both.Reason, want) {
			t.Fatalf("a row whose two questions both failed must carry both answers, missing %q in %+v", want, both)
		}
	}
}

// TestOutdatedTellsAVanishedRefApartFromAnUnreachableRemote pins round 3's
// Important #9's fifth row: dropping describeRemoteErr's ErrRemoteRefNotFound
// arm reported a deleted upstream branch as "unreachable", telling an online
// user their network was down. Its sibling
// (TestOutdatedDegradesEverySkillOnOneDeadRemoteWithoutFailing) covers the
// other arm.
func TestOutdatedTellsAVanishedRefApartFromAnUnreachableRemote(t *testing.T) {
	s, cfg := setupStore(t)
	url, _, commit := realGitBranchFixture(t, "kit")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "one")
	if err := cfg.AddSkill("kit", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	// The repository is real and reachable; this branch is not in it.
	cfg.SetSourceFields("kit", map[string]string{
		"type": "git", "url": url, "ref": "refs/heads/gone", "ref_kind": "branch", "commit": commit,
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if got[0].Comparable {
		t.Fatalf("a ref the remote does not advertise is not comparable: %+v", got[0])
	}
	if !strings.Contains(got[0].Reason, "no longer exists") {
		t.Fatalf("a vanished ref must say so: %q", got[0].Reason)
	}
	if strings.Contains(got[0].Reason, "unreachable") {
		t.Fatalf("the remote answered; calling it unreachable misdirects the user: %q", got[0].Reason)
	}
}

// TestOutdatedTellsAMalformedRecordApartFromAnUnreachableRemote pins round 4's
// third arm of the same split. ResolveRemoteRef refuses a ref the remote
// advertises symbolically, which is a fact about a hand-edited fu.yaml and not
// about the network -- but every non-vanished failure used to fall through to
// "unreachable", so this record told an online user their network was down.
//
// `ref: HEAD` with `ref_kind: branch` is hand-edit only; EncodeFields never
// writes HEAD. The remote below is real and answers, which is the whole point.
func TestOutdatedTellsAMalformedRecordApartFromAnUnreachableRemote(t *testing.T) {
	s, cfg := setupStore(t)
	url, _, commit := realGitBranchFixture(t, "kit")
	writeSkillBody(t, filepath.Join(s.SkillsDir(), "kit"), "kit", "one")
	if err := cfg.AddSkill("kit", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	cfg.SetSourceFields("kit", map[string]string{
		"type": "git", "url": url, "ref": "HEAD", "ref_kind": "branch", "commit": commit,
	})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Outdated(s, cfg)
	if err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if got[0].Comparable {
		t.Fatalf("a ref that cannot name a commit is not comparable: %+v", got[0])
	}
	if !strings.Contains(got[0].Reason, "unusable") {
		t.Fatalf("a malformed record must say so: %q", got[0].Reason)
	}
	if strings.Contains(got[0].Reason, "unreachable") {
		t.Fatalf("the remote answered; calling it unreachable misdirects the user: %q", got[0].Reason)
	}
}
