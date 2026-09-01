package cli

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
)

// Fix round 1 (review): the tests below originally used 7-character
// hand-picked placeholders ("a1b2c3d"/"e4f5a6b") for Current/Latest. Those
// fit entirely under the 12-character display truncation (internal/cli/show.go's
// own convention, reused by shortCommit in outdated.go), so nothing in this
// suite ever exercised truncation, and a uniform "Current -> Latest" render
// for every source kind shipped undetected (Important #1) -- a real git
// commit is 40 hex characters (internal/source/lsremote.go's
// ResolveRemoteRef -> ref.Hash().String()), and a real local-source digest is
// "sha256:"+64 hex characters (internal/skill/digest.go's DigestManifest).
// These package-level values give every test below a shape indistinguishable
// from what the real engine actually produces.
var (
	testGitCurrent    = strings.Repeat("a1b2c3d4e5", 4)           // 40 hex chars
	testGitLatest     = strings.Repeat("f6e5d4c3b2", 4)           // 40 hex chars
	testLocalBaseline = "sha256:" + strings.Repeat("11223344", 8) // "sha256:" + 64 hex chars
	testLocalLatest   = "sha256:" + strings.Repeat("99887766", 8) // "sha256:" + 64 hex chars
)

type fakeOutdatedApp struct {
	rows []engine.UpdateStatus
	err  error
}

func (f fakeOutdatedApp) Outdated() (engine.OutdatedOutcome, error) {
	return engine.OutdatedOutcome{Rows: f.rows}, f.err
}

func runOutdated(t *testing.T, app outdatedApplication) (string, error) {
	t.Helper()
	cmd := newOutdatedCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	return out.String(), err
}

// Finding an available update is a report, not a failure -- the same rule
// `fu status` follows.
func TestOutdatedCommandSucceedsWhenUpdatesAreAvailable(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "git", Ref: "refs/heads/main",
			Current: testGitCurrent, Latest: testGitLatest},
	}})
	if err != nil {
		t.Fatalf("outdated must not fail on a finding: %v", err)
	}
	if !strings.Contains(out, "pdf-tools") || !strings.Contains(out, testGitLatest[:12]) {
		t.Fatalf("output must name the skill and the new commit:\n%s", out)
	}
}

func TestOutdatedCommandReportsWhenEverythingIsCurrent(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "up to date") {
		t.Fatalf("output must say everything is current:\n%s", out)
	}
}

// A row that cannot be judged must say so rather than be silently dropped or
// counted as current.
func TestOutdatedCommandListsNonComparableRowsWithTheirReason(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "homegrown", Comparable: false, Reason: "no source record"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not comparable") || !strings.Contains(out, "no source record") {
		t.Fatalf("output must group and explain the row:\n%s", out)
	}
}

// A locally modified row warns that `fu update` will refuse, so the
// outdated -> update path in SPEC scenario 3 does not dead-end.
func TestOutdatedCommandWarnsThatUpdateWillRefuseALocallyModifiedSkill(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "api-docs", Comparable: true, Updatable: true, LocallyModified: true,
			Kind: "git", Ref: "refs/heads/main", Current: testGitCurrent, Latest: testGitLatest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "locally modified") {
		t.Fatalf("output must warn about the refusal:\n%s", out)
	}
}

// Self-review addition: the test above only pins that the refusal warning
// appears when LocallyModified is set -- it would still pass if the warning
// were appended unconditionally to every updatable row. This pins the other
// half: an updatable row that was not locally modified must not carry it.
func TestOutdatedCommandOmitsTheLocallyModifiedWarningWhenNotModified(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "git", Ref: "refs/heads/main",
			Current: testGitCurrent, Latest: testGitLatest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "locally modified") {
		t.Fatalf("an unmodified updatable row must not carry the refusal warning:\n%s", out)
	}
}

// Task 2 review finding, carried forward as a binding requirement for this
// task: judgeLocalModification
// (internal/engine/outdated.go) can fold a store-snapshot failure into Reason
// on a row that is otherwise fully Comparable -- here Updatable is still
// correctly judged true from the source record alone, independent of that
// failure. Rendering Reason only on non-comparable rows would silently
// swallow it, leaving a confident-looking updatable row whose
// LocallyModified=false secretly means "unknown", not "no".
func TestOutdatedCommandShowsReasonOnAComparableUpdatableRowToo(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "git", Ref: "refs/heads/main",
			Current: testGitCurrent, Latest: testGitLatest,
			Reason: "snapshot store content: boom"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "snapshot store content: boom") {
		t.Fatalf("a Reason on a comparable, updatable row must still reach the output:\n%s", out)
	}
}

// Self-review addition: the same fold can also land on a row judgeUpdate
// found current (Updatable false) -- the source comparison succeeded and
// found nothing new, but the separate local-modification check still failed.
// That row is Comparable, so it does not belong in "not comparable", and it
// is not Updatable, so a naive updatable-group gate keyed on Updatable alone
// would drop it from both groups -- invisible, silently folded into
// "everything is up to date" alongside rows that were actually fully judged
// clean.
func TestOutdatedCommandShowsReasonOnACurrentRowInsteadOfCallingItUpToDate(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		// Kind set for realism (a real Comparable row always has one), even
		// though updatableRowBody's !Updatable branch does not look at it.
		{Name: "pdf-tools", Comparable: true, Updatable: false, Kind: "git", Reason: "snapshot store content: boom"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pdf-tools") || !strings.Contains(out, "snapshot store content: boom") {
		t.Fatalf("output must name the skill and surface the reason:\n%s", out)
	}
	// Pins which section rendered the row, not just that its name and reason
	// appear somewhere: without this, the assertions above would also pass if
	// the row were mistakenly rendered in the updatable section instead (an
	// empty "-> " arrow line, since Current/Latest are both unset here).
	if !strings.Contains(out, "up to date\n  pdf-tools  snapshot store content: boom\n") {
		t.Fatalf("a comparable, non-updatable row belongs under its own heading, carrying its reason:\n%s", out)
	}
	if strings.Contains(out, "everything is up to date") {
		t.Fatalf("a row carrying an unresolved reason must not be reported as a clean pass:\n%s", out)
	}
}

// TestOutdatedCommandDoesNotCountACurrentRowAsUpdatable pins fix round 2's
// Important #3. A row that is Comparable, not Updatable and carries a folded
// Reason (judgeLocalModification's own check failed on an otherwise current
// row) used to be filed into the updatable slice, which is both the body of
// the "updatable" section and the number in the trailing count -- so the
// heading said updatable, the count said updatable, and the row itself said
// "no update available". The grouping decision was right and stays; what was
// missing is that the section name and the count have to follow it.
//
// The reproduction the review recorded is exactly this shape: with
// skills/<name> missing, SnapshotSkillPayload fails and its message is folded
// onto a row whose source comparison succeeded and found nothing new.
func TestOutdatedCommandDoesNotCountACurrentRowAsUpdatable(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "kit", Comparable: true, Updatable: false, Kind: "local",
			Reason: "snapshot store content: no such file or directory"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "updatable\n") && !strings.Contains(out, "0 updatable") {
		t.Fatalf("a row with no update available must not appear under the updatable heading:\n%s", out)
	}
	if !strings.Contains(out, "0 updatable, 1 up to date, 0 not comparable") {
		t.Fatalf("the count must report the row under the group that actually holds it:\n%s", out)
	}
}

// TestOutdatedCommandCountsAllThreeGroupsItPrinted is the multi-row companion
// to the test above: every printed section must contribute its own number to
// the trailing count, and a fully clean row (no update, no reason) must still
// inflate none of them.
func TestOutdatedCommandCountsAllThreeGroupsItPrinted(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "git", Ref: "refs/heads/main",
			Current: testGitCurrent, Latest: testGitLatest},
		{Name: "kit", Comparable: true, Updatable: false, Kind: "local", Reason: "snapshot store content: boom"},
		{Name: "homegrown", Comparable: false, Reason: "no source record"},
		{Name: "clean", Comparable: true, Updatable: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 updatable, 1 up to date, 1 not comparable") {
		t.Fatalf("every printed group must carry its own count:\n%s", out)
	}
}

// TestOutdatedCommandReportsALocalEditOnANonComparableRow pins round 4's
// finding that a non-comparable row's local modification was reported nowhere
// in fu. judgeLocalModification runs for every row, comparable or not (SPEC
// rule 9's two questions are independent), but splitOutdatedRows files a
// non-comparable row into its own group first, and `fu status` deliberately
// never makes this comparison -- it compares the worktree against git, not
// cfg.Digest against store content. So for a tag-pinned skill whose store copy
// was hand edited, the fact was computed, discarded, and unobtainable by any
// command.
//
// The clause states the fact and stops: this row already says why `fu update`
// cannot work on it, and it is not a reason --force answers.
func TestOutdatedCommandReportsALocalEditOnANonComparableRow(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pinned-tool", Comparable: false, Reason: "source ref is a fixed lock", LocallyModified: true},
		{Name: "homegrown", Comparable: false, Reason: "no source record"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "source ref is a fixed lock; locally modified") {
		t.Fatalf("a non-comparable row must still report its local edit:\n%s", out)
	}
	if strings.Contains(out, "no source record; locally modified") {
		t.Fatalf("a row that was not locally modified must not claim to be:\n%s", out)
	}
	if strings.Contains(out, "--force") {
		t.Fatalf("--force does not answer this row's refusal and must not be offered:\n%s", out)
	}
}

// Self-review addition: pins the trailing count line the brief requires ("末尾
// 一行计数") -- otherwise nothing in this suite would notice if it were
// dropped, or its two counts swapped. The clean row is included deliberately:
// it must inflate neither count.
func TestOutdatedCommandPrintsATrailingCount(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "git", Ref: "refs/heads/main",
			Current: testGitCurrent, Latest: testGitLatest},
		{Name: "homegrown", Comparable: false, Reason: "no source record"},
		{Name: "clean", Comparable: true, Updatable: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 updatable, 1 not comparable") {
		t.Fatalf("output must end with a count of both groups:\n%s", out)
	}
}

// Fix round 1 (review), Important #1 and #2: outdated.go used to print
// Current/Latest directly for every updatable row regardless of source kind
// -- unusable for a local source (a bare digest pair, no path at all) and
// meaningless for a git source without the ref the commits resolve against.
// Nothing in the suite above ever rendered a row with this shape: every test
// used a 7-character placeholder that fits entirely under the 12-character
// display truncation, so truncation itself went unexercised too. These two
// tests pin the exact rendered line per kind -- not just substring presence
// -- using realistic-length values.

func TestOutdatedCommandRendersAGitUpdatableRowWithItsRefAndAShortenedCommit(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "git", Ref: "refs/heads/main",
			Current: testGitCurrent, Latest: testGitLatest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "  pdf-tools  " + testGitCurrent[:12] + " → " + testGitLatest[:12] + "   refs/heads/main\n"
	if !strings.Contains(out, want) {
		t.Fatalf("a git updatable row must show the shortened commits and the ref:\ngot:\n%s\nwant line:\n%q", out, want)
	}
	if strings.Contains(out, testGitCurrent) || strings.Contains(out, testGitLatest) {
		t.Fatalf("a full 40-character commit must be shortened for display, not printed in full:\n%s", out)
	}
}

// The regression guard fix round 1 explicitly required: a local row must
// never render its raw content digest again.
func TestOutdatedCommandRendersALocalUpdatableRowByPathNotByDigest(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "local", Path: "/home/u/src/pdf-tools",
			Current: testLocalBaseline, Latest: testLocalLatest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "  pdf-tools  source content differs from the install baseline   /home/u/src/pdf-tools\n"
	if !strings.Contains(out, want) {
		t.Fatalf("a local updatable row must read as a path, not a digest pair:\ngot:\n%s\nwant line:\n%q", out, want)
	}
	if strings.Contains(out, testLocalBaseline) || strings.Contains(out, testLocalLatest) || strings.Contains(out, "sha256:") {
		t.Fatalf("a local row must never render its raw content digest:\n%s", out)
	}
}

// TestOutdatedCommandKeepsAReasonOnOneLine pins fix round 2's Minor #1.
// Every other field on an outdated row is wrapped in singleLine (show.go's
// own convention, applied throughout updatableRowBody), but Reason was
// printed raw in all three places it appears. Reason quotes back recorded
// paths, and `fu add` on a directory whose name contains a newline records
// that path verbatim -- so a raw Reason could fabricate a heading, an extra
// row for a skill that does not exist, and a count disagreeing with the rows
// above it. One test covers all three sites, one row per group.
func TestOutdatedCommandKeepsAReasonOnOneLine(t *testing.T) {
	const forged = "open local source /home/u/evil\nupdatable\n  ghost  pwned"
	// Round 3, Minor #2: only Reason was covered, so removing singleLine from
	// either row body survived the whole package -- and the local path is the
	// one reachable through plain `fu add`, since a source directory named with
	// an embedded newline is recorded verbatim. The forged ref and path below
	// close that.
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "in-updatable", Comparable: true, Updatable: true, Kind: "git",
			Ref:     "refs/heads/main\nupdatable\n  ghost  pwned",
			Current: testGitCurrent, Latest: testGitLatest, Reason: forged},
		{Name: "in-up-to-date", Comparable: true, Updatable: false, Kind: "local", Reason: forged},
		{Name: "in-not-comparable", Comparable: false, Reason: forged},
		{Name: "in-local-path", Comparable: true, Updatable: true, Kind: "local",
			Path: "/home/u/evil\nupdatable\n  ghost  pwned", Subdir: "kit",
			Current: testLocalBaseline, Latest: testLocalLatest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\n  ghost  pwned") {
		t.Fatalf("a newline inside Reason must not fabricate a row:\n%s", out)
	}
	// Four rows plus three headings plus the count: any extra line means a
	// forged field broke out of its own row somewhere.
	if got := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; got != 8 {
		t.Fatalf("output must hold exactly 8 lines (3 headings, 4 rows, 1 count), got %d:\n%s", got, out)
	}
}

// Self-review addition, per the controller ruling: the tests above all drive
// newOutdatedCmd through a fake Application, the same shape as the brief's
// own tests -- none of them exercise the production wiring Step 3 adds,
// Application.Outdated -> readStore -> store.Open -> engine.Outdated's own
// internal BeginWrite session. Task 2's review penalised exactly this shape
// (a load-bearing path whose only evidence was not a permanent test), so this
// runs the real command against a real store. A skill with no source record
// is enough -- `fu new` never sets one -- proving the command opens a real
// store, runs the real judgement, and renders without error; it needs no
// network access.
func TestOutdatedCommandIntegrationRunsTheRealJudgement(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	runCmd(t, "new", "alpha")
	// An uncommitted edit in the store worktree, so the read-only assertion
	// below has something to catch. On a clean store a write prologue happens to
	// change nothing observable -- the lock file already exists, reconcile finds
	// every link correct, and Sweep has nothing to commit -- so a fixture
	// without this passes whether or not the command stays read-only. With it,
	// any sweep turns the edit into a commit and the hashes diverge.
	edited := filepath.Join(fuHome, "store", "skills", "alpha", "SKILL.md")
	body, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edited, append(body, []byte("\nhand edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newOutdatedCmd(engine.NewApplication())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	// SPEC §9's read-only guarantee, asserted at this layer and not only at
	// engine.Outdated (TestOutdatedWritesNothingToTheStore). The engine test
	// brackets the judgement itself; this one brackets the whole production
	// wiring, which is where a write would actually be introduced --
	// UpdateSkills just gained a writeCommandPrologue whose doc argues at length
	// that a command doing discovery needs one before judging, and the same
	// argument reads as applying to Application.Outdated. It does not: adding
	// one here would recover, sweep and reconcile inside a read command, which
	// is exactly what §9 forbids, and every existing test would still pass.
	before := hashOutdatedTree(t, fuHome)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("outdated: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "alpha") {
		t.Fatalf("output must name the skill: %s", out.String())
	}
	after := hashOutdatedTree(t, fuHome)
	if len(before) != len(after) {
		t.Fatalf("the outdated command changed what is under FU_HOME: %d entries before, %d after", len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("the outdated command wrote to %s under FU_HOME", path)
		}
	}
}

// hashOutdatedTree records every regular file, symlink and directory under root
// so a command can be shown to have written nothing. The store's own .git is
// included, so a stray commit, lock file or index rewrite shows up as a
// difference. Mirrors the engine suite's hashTree, which is not exported across
// package boundaries.
func hashOutdatedTree(t *testing.T, root string) map[string]string {
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

// TestOutdatedCommandAlignsTheNameColumn pins fix round 2's Minor #7(a).
// Design §3.4's sample output shows a padded name column; the implementation
// emitted a fixed two-space gap, so real output was ragged as soon as two
// names differed in length. `fu status` already sizes its own column with
// %-*s (status.go), so the pattern was in-repo and simply not applied here.
//
// The width is taken across every printed row, not per section: three
// sections each padded to their own widest name would line up internally and
// step in and out against each other down the page.
func TestOutdatedCommandAlignsTheNameColumn(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "kit", Comparable: true, Updatable: true, Kind: "git", Ref: "refs/heads/main",
			Current: testGitCurrent, Latest: testGitLatest},
		{Name: "a-much-longer-name", Comparable: false, Reason: "no source record"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var short, long string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "  kit"):
			short = line
		case strings.HasPrefix(line, "  a-much-longer-name"):
			long = line
		}
	}
	if short == "" || long == "" {
		t.Fatalf("both rows must be printed:\n%s", out)
	}
	// Each body is located by its own first characters, not by scanning for a
	// gap: the short name's padding is itself a run of spaces, so a
	// gap-scanning helper finds the padding rather than the body.
	shortBody := strings.Index(short, testGitCurrent[:12])
	longBody := strings.Index(long, "no source record")
	if shortBody != longBody {
		t.Fatalf("rows must share one name column:\n%q (body at %d)\n%q (body at %d)",
			short, shortBody, long, longBody)
	}
}

// TestOutdatedCommandShowsALocalRowsSubdir pins fix round 2's Minor #7(b). A
// local row printed the recorded source *root*, so two skills installed from
// one directory rendered an identical path -- the one thing that row exists
// to make actionable. The path shown must address the skill itself.
func TestOutdatedCommandShowsALocalRowsSubdir(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "pdf-tools", Comparable: true, Updatable: true, Kind: "local",
			Path: "/home/u/src", Subdir: "tools/pdf-tools",
			Current: testLocalBaseline, Latest: testLocalLatest},
		{Name: "api-docs", Comparable: true, Updatable: true, Kind: "local",
			Path: "/home/u/src", Subdir: "docs/api-docs",
			Current: testLocalBaseline, Latest: testLocalLatest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/home/u/src/tools/pdf-tools", "/home/u/src/docs/api-docs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a local row must address the skill's own directory, missing %q:\n%s", want, out)
		}
	}
}

// A root-level skill records "." (or nothing) as its subdir, and the path
// must stay the recorded root rather than growing a trailing "/.".
func TestOutdatedCommandLeavesARootLevelLocalPathAlone(t *testing.T) {
	for _, subdir := range []string{"", "."} {
		out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
			{Name: "kit", Comparable: true, Updatable: true, Kind: "local",
				Path: "/home/u/src/kit", Subdir: subdir,
				Current: testLocalBaseline, Latest: testLocalLatest},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "/home/u/src/kit\n") {
			t.Fatalf("subdir %q must leave the recorded path unchanged:\n%s", subdir, out)
		}
	}
}

// TestOutdatedCommandReportsACurrentRowUpdateWillRefuse pins round 2's
// Important #1. A skill whose store copy was hand edited while nothing moved
// upstream is Comparable, not Updatable, and carries no Reason -- so it used
// to land in no group at all, be counted nowhere, and then be refused by `fu
// update <name>`, which judges store-side drift alone. Design §3.4 names that
// broken chain: 若 update 注定拒绝而 outdated 不说，这条链就断了.
func TestOutdatedCommandReportsACurrentRowUpdateWillRefuse(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "beta", Comparable: true, Updatable: false, LocallyModified: true, Kind: "local",
			Path: "/home/u/src/beta"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "up to date\n  beta  locally modified — `fu update` will refuse\n") {
		t.Fatalf("a current row `fu update` will refuse must be reported and say so:\n%s", out)
	}
	if !strings.Contains(out, "0 updatable, 1 up to date, 0 not comparable") {
		t.Fatalf("the row must be counted in the group that holds it:\n%s", out)
	}
	if strings.Contains(out, "everything is up to date") {
		t.Fatalf("a row `fu update` will refuse is not a clean pass:\n%s", out)
	}
}

// Both halves can land on one row, and neither may swallow the other: the
// refusal and a folded Reason answer SPEC rule 9's two independent questions.
func TestOutdatedCommandShowsBothTheRefusalAndAFoldedReasonOnOneRow(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "beta", Comparable: true, Updatable: false, LocallyModified: true, Kind: "local",
			Reason: "digest store content: boom"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"locally modified", "digest store content: boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("both answers must survive, missing %q:\n%s", want, out)
		}
	}
}

// And a genuinely clean store still reports clean: the new gate must not turn
// every current row into a reported one.
func TestOutdatedCommandStillReportsACleanStoreAsClean(t *testing.T) {
	out, err := runOutdated(t, fakeOutdatedApp{rows: []engine.UpdateStatus{
		{Name: "beta", Comparable: true, Updatable: false, Kind: "local", Path: "/home/u/src/beta"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "everything is up to date") {
		t.Fatalf("a fully current row must still report clean:\n%s", out)
	}
}

// TestOutdatedCommandPrintsConfigDiagnostics pins round 2's Important #6.
// `fu outdated` was the one read command that printed neither diagnostic
// channel, so an invalid skill name -- which LoadConfig excludes from the
// skill set entirely -- vanished from the report with nothing saying why,
// while `fu list` and `fu status` both named it.
func TestOutdatedCommandPrintsConfigDiagnostics(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	// Written straight into fu.yaml: `fu new` would refuse the name, which is
	// the point -- this is the hand-edited shape the diagnostics exist for.
	configPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(raw), "  alpha:", "  Bad_Name:\n    digest: \"sha256:x\"\n  alpha:", 1)
	if patched == string(raw) {
		t.Fatalf("fixture error: could not plant an invalid name in:\n%s", raw)
	}
	if err := os.WriteFile(configPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newOutdatedCmd(engine.NewApplication())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("outdated: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "Bad_Name") {
		t.Fatalf("an excluded invalid name must be reported, not silently omitted:\n%s", out.String())
	}
}

func TestOutdatedCommandRejectsAPositionalArgument(t *testing.T) {
	app := fakeOutdatedApp{}
	cmd := newOutdatedCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"foo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a usage error")
	}
	var uerr *UsageError
	if !errors.As(err, &uerr) {
		t.Fatalf("error = %v, want a UsageError so the exit code is 2", err)
	}
}
