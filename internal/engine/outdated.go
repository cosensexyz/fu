// internal/engine/outdated.go
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

// UpdateStatus is one skill's upstream-vs-local judgement. It is produced by
// Outdated and consumed by both `fu outdated` and `fu update`'s batch
// selection, so the two commands share one definition of "updatable" and can
// never disagree about it.
//
// SPEC rule 9 keeps two questions strictly apart, and this struct carries
// both answers side by side rather than folding them into one bit:
//
//   - Updatable asks whether the skill's recorded source has moved past what
//     fu.yaml locked at install time -- "可更新", is there something new
//     upstream to pull in.
//   - LocallyModified asks whether the store's own copy has drifted from the
//     digest fu.yaml recorded as its install baseline -- "本地修改", did this
//     copy get hand-edited after the fact.
//
// The two are independent and can both be true at once: pulling an available
// update onto a hand-edited copy is a real decision a caller has to make, not
// something this type is allowed to obscure by picking only one bit to report.
type UpdateStatus struct {
	Name       string
	Comparable bool
	Updatable  bool
	// Reason explains why Comparable is false. It is otherwise empty, except
	// that a LocallyModified snapshot failure (see judgeLocalModification) is
	// folded in alongside whatever judgeUpdate already put there, since that
	// failure is its own, independent thing to report and must not be lost
	// just because the source-side judgement already succeeded.
	Reason string
	// NoUpstream distinguishes the two ways Comparable can be false. It is
	// set when the record itself settles the question -- there is no source
	// record at all, or the recorded ref is a fixed lock -- so there is no
	// upstream that can ever move and re-running changes nothing. It is left
	// false when fu simply could not reach the answer this time: an
	// unreachable remote, a vanished ref, a local path that is not there, a
	// record too malformed to judge.
	//
	// Both kinds still print under `fu outdated`'s "not comparable" heading,
	// which is that command's job. What reads the bit is `fu update`'s batch
	// selection (selectUpdateTargets, application.go): "I could not tell" is
	// worth a line on stderr every run, while "this skill has no upstream,
	// ever" said once per `fu new` skill per run is noise on the same channel
	// that carries skipped:, failed: and not attempted:.
	NoUpstream bool
	// Current and Latest hold whatever unit the source type compares: a git
	// commit for a git source (the locked commit and the one its ref
	// currently resolves to), or a content digest for a local source (the
	// recorded baseline and the source directory's current digest).
	Current, Latest string
	LocallyModified bool
	// Baseline and StoreDigest are the two values LocallyModified is the
	// comparison of: the digest fu.yaml recorded when the skill was installed,
	// and the digest the store's own copy hashes to now. They are carried
	// rather than recomputed because SPEC rule 3 promises 拒绝覆盖并提示差异 --
	// the refusal has to show the difference, not merely assert one
	// (selectUpdateTargets, application.go).
	//
	// A digest pair is the whole of what can be shown here. fu.yaml records a
	// digest, not a manifest (DESIGN §3's 内容基线), so no file-level diff is
	// derivable from the baseline at all; what names the changed files is the
	// store's own git history, which the refusal points at instead.
	//
	// StoreDigest is empty when the snapshot or the hashing failed -- the same
	// failure that folds a Reason onto the row and leaves LocallyModified
	// false, meaning "unknown", not "no".
	Baseline, StoreDigest string
	// Kind, Ref and Path are the source record's own raw fields (fu.yaml's
	// "type", "ref" and "path"), carried through unconditionally alongside
	// whatever judgeUpdate separately decides -- pure passthrough data for a
	// presentation layer to render per source kind, never a judgement of any
	// kind itself. A git and a local row need different presentation (a ref
	// versus a path), and neither Current nor Latest says which one a row is:
	// a git row's are commit hashes meaningless without the ref they resolve
	// against, and a local row's are raw content digests
	// (skill.DigestManifest) meaningless to a reader at all.
	//
	// Kind is "git", "local", or "" when there is no source record at all;
	// Ref is set only for a git source and Path only for a local one.
	//
	// Subdir is the skill's own directory inside that source, "" or "." when
	// the source root is itself the skill. It is carried alongside Path
	// because one local directory commonly serves several skills -- the
	// subdir field exists for precisely that -- and without it every one of
	// them renders the same path, which is the one thing the row exists to
	// make actionable.
	Kind   string
	Ref    string
	Path   string
	Subdir string
}

// Outdated judges every registered skill's update status against the store's
// own fu.yaml, in one pass. It is the shared judgement layer behind `fu
// outdated` and `fu update`'s batch selection.
//
// fu outdated is read-only (SPEC §9): this never writes to the store, never
// takes the write lock, and never clones. For a git source that means
// upstream *content* is out of reach -- only the commit its recorded ref
// currently resolves to may be compared (Source.ResolveRemoteRef lists refs
// without fetching any object). A local source has no such restriction; its
// "upstream" is a plain directory this process reads directly.
//
// A single skill's source being unreachable (an offline remote, a vanished
// local path, a ref the remote no longer advertises) degrades only that
// skill's row -- Comparable is left false with Reason explaining why -- and
// never fails the whole call.
//
// Two kinds of error remain, and they differ in whether rows come back with
// them. One leaves the store unreadable altogether, before any row can be
// judged, and returns nothing. The other is the write session's close, joined
// on in outdatedFor's defer: it happens once every row has been judged, so a
// non-nil error arrives alongside a complete slice. Callers must return both --
// Application.Outdated's own doc argues the case, and a front end that
// discarded the report over a failure to release descriptors would throw away a
// finished answer.
func Outdated(st *store.Store, cfg *store.Config) ([]UpdateStatus, error) {
	return outdatedFor(st, cfg, "")
}

// outdatedFor is Outdated narrowed to one skill when only is non-empty, and
// is what `fu update <name>` judges through (UpdateSkills, application.go).
//
// The narrowing is a filter over the names judged, not a change to how any
// one of them is judged: both callers run the same judgeUpdate and
// judgeLocalModification over the same fields, so the design spec's "两条命令
// 共用一套判定" invariant holds by construction rather than by convention.
//
// Fix round 2, Important #1: `fu update <name>` used to judge the whole
// config and discard every row but the named one afterwards. Every distinct
// (url, ref) in the store was resolved first, serially, each under its own
// fresh 2-minute timeout (judgeGitUpdate) -- so naming one skill blocked on
// every unrelated remote, and naming a skill with no remote at all did too.
// The per-source dedup and the per-row degradation below were both designed
// for `fu outdated`'s whole-store sweep; neither makes a named update owe
// anything to a source it will never touch.
//
// A name the config does not hold yields no rows at all rather than a row
// saying so: selectUpdateTargets already answers that case with "unknown
// skill", and a "no source record" row would have replaced that answer with a
// worse one.
func outdatedFor(st *store.Store, cfg *store.Config, only string) (results []UpdateStatus, err error) {
	// SnapshotSkillPayload (used below by judgeLocalModification) only works
	// through pinned, checked descriptors -- the same TOCTOU protection every
	// other reader of live store content in this codebase uses -- which only
	// a write session provides. Opening one here does not conflict with the
	// read-only requirement above: BeginWrite itself only opens read-only
	// directory descriptors and validates identity, never writing anything,
	// and the actual write lock is a separate step (withLock, in lock.go)
	// that every write command layers on top of it and that Outdated never
	// calls.
	session, beginErr := st.BeginWrite()
	if beginErr != nil {
		return nil, fmt.Errorf("open store for a read-only content comparison: %w", beginErr)
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	pinned := session.Store

	// Two caches keyed by (url, ref), so a repository serving several skills
	// on the same ref resolves its remote once, not once per skill: one
	// records a resolved commit, the other a resolution failure already
	// diagnosed for that same pair.
	resolvedCommits := map[[2]string]string{}
	resolveErrs := map[[2]string]error{}

	names := cfg.SkillNames()
	if only != "" {
		names = onlyRegisteredName(names, only)
	}
	results = make([]UpdateStatus, 0, len(names))
	for _, name := range names {
		status := UpdateStatus{Name: name}
		fields := cfg.SourceFields(name)
		// Raw passthrough, populated unconditionally before judgeUpdate runs:
		// see UpdateStatus's own doc comment for why the presentation layer
		// needs these and why this is data, not a judgement.
		status.Kind = fields["type"]
		status.Ref = fields["ref"]
		status.Path = fields["path"]
		status.Subdir = fields["subdir"]
		judgeUpdate(&status, fields, cfg.Digest(name), resolvedCommits, resolveErrs)
		// Computed for every row regardless of the source-side judgement
		// above: whether upstream moved and whether the store copy was
		// hand-edited are independent questions (SPEC rule 9), so one is
		// never skipped because the other could not be decided.
		judgeLocalModification(&status, pinned, cfg, name)
		results = append(results, status)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results, nil
}

// onlyRegisteredName keeps want if the config actually holds it, and returns
// nothing otherwise. Filtering the config's own name list rather than
// returning want directly is what keeps an unregistered name out of the
// results: SourceFields answers with an empty map for a name that is not
// there, which would judge as a perfectly ordinary "no source record" row.
func onlyRegisteredName(names []string, want string) []string {
	for _, name := range names {
		if name == want {
			return []string{want}
		}
	}
	return nil
}

// judgeUpdate decides Comparable, Updatable, Current, Latest and (on
// failure) Reason from a skill's recorded source alone -- "did upstream move
// away from what is recorded". It never looks at the store's own copy; that
// is judgeLocalModification's job, kept as a separate pass so the two SPEC
// rule 9 questions can never be conflated into one verdict.
func judgeUpdate(status *UpdateStatus, fields map[string]string, baseline string, resolvedCommits map[[2]string]string, resolveErrs map[[2]string]error) {
	switch fields["type"] {
	case "":
		status.Reason = "no source record"
		status.NoUpstream = true
	case string(source.KindGit):
		judgeGitUpdate(status, fields, resolvedCommits, resolveErrs)
	case string(source.KindLocal):
		judgeLocalUpdate(status, fields, baseline)
	default:
		// Defensive: SourceFields only ever holds what EncodeFields writes
		// (source.go), i.e. "git" or "local", but fu.yaml is a hand-editable
		// file, and a row must always say why it is not comparable rather
		// than silently reporting Comparable=false with no Reason.
		status.Reason = fmt.Sprintf("unrecognized source type %q", fields["type"])
	}
}

// resolveRemoteRefHook observes each real ResolveRemoteRef call the dedup
// cache below actually makes, keyed by (url, ref). It is nil in production
// and set only by tests, mirroring lockAcquiredHook's own pattern (lock.go):
// the call it observes always happens; this only lets a test count how many
// times, without adding any test-only code path production does not
// exercise.
//
// Package-level and written by tests, like lockAcquiredHook and
// updateSourcePreparedHook, which rests on a constraint worth stating: no test
// in this package calls t.Parallel(), so only one test writes any of these at a
// time. The constraint is checked rather than remembered --
// TestNoTestInThisPackageRunsInParallel parses this package's test files
// (test_hygiene_test.go) -- because the first parallel test added here has to
// give the hooks a home that is not a global first, and unchecked, -race would
// fail somewhere that does not look like the cause. It is this package's
// constraint alone: all four hooks are unexported, and each package's tests are
// a separate process, so a parallel test elsewhere cannot reach them.
var resolveRemoteRefHook func(url, ref string)

// judgeGitUpdate compares a git source's locked commit against whatever its
// recorded ref currently resolves to on the remote.
func judgeGitUpdate(status *UpdateStatus, fields map[string]string, resolvedCommits map[[2]string]string, resolveErrs map[[2]string]error) {
	// A record with no ref kind is told apart from a real fixed lock, because
	// they are different facts and the message is repeated verbatim by
	// `fu update <name>`'s own refusal (round 2, Minor #6). EncodeFields writes
	// ref_kind only when non-empty (source.go), so an incomplete or hand-edited
	// record arrives here with the field absent -- and calling that "a fixed
	// lock" attributes a deliberate decision to a record that simply does not
	// say. The missing-commit case further down this function already gets its
	// own accurate wording.
	if fields["ref_kind"] == "" {
		status.Reason = "git source record has no ref kind"
		return
	}
	switch kind := fields["ref_kind"]; kind {
	case "branch":
	case "tag", "commit":
		// A tag or a bare commit pin names one exact point in history that will
		// never move out from under it, so there is no "upstream" to compare
		// against. The message names the ref generically rather than "tag"
		// specifically -- SPEC's own vocabulary is just "固定锁" (fixed lock),
		// and this same arm also covers a commit pin, so naming "tag"
		// unconditionally would tell a commit-pinned skill's reader about a tag
		// that was never there. Only these two kinds land here: an unrecognised
		// one is a hand edit, not a decision, and gets the default: arm below.
		status.Reason = "source ref is a fixed lock"
		// A determinate answer, unlike the malformed-record arms around it: a
		// tag or commit pin is a decision the record states, not a question
		// left open. Every other arm in this function leaves NoUpstream false
		// -- the missing and unrecognised ref kinds, the unusable url and the
		// missing commit alike -- because a hand-edited record is something the
		// user can fix and should keep hearing about.
		status.NoUpstream = true
		return
	default:
		// Named rather than swept into the fixed-lock arm, which is what the
		// bare `!= "branch"` test did. An unrecognised kind is not a decision
		// the record states -- it is the same hand edit the empty case above is
		// unmarked for, so it stays unmarked too, and selectUpdateTargets goes
		// on reporting the row instead of silently dropping it from the batch
		// forever. The wording matches judgeUpdate's own default: arm, which
		// makes the identical call one field over for `type`.
		status.Reason = fmt.Sprintf("unrecognized ref kind %q", kind)
		return
	}
	// Ahead of the commit guard below, not merely ahead of the cache lookup. A
	// record damaged in both fields reported "branch source has no recorded
	// commit" first, so the user fixed the commit and only then learned the url
	// was unusable -- two round trips for one hand edit, and the second finding
	// is the sharper of the two. Still before the cache lookup, which is the
	// other thing this placement has to keep: one unusable record must never
	// seed the deduplicated (url, ref) map every other row reads from.
	//
	// go-git treats a url it cannot parse as a transport as a *path*:
	// transport.NewEndpoint("") falls into parseFile, which does
	// filepath.Abs("") -- the process's working directory -- and returns a file
	// endpoint. Without this, a record with no url resolved against whatever
	// repository fu happened to be run from, so `fu outdated`'s verdict depended
	// on $PWD and `fu update` would publish that repository's content into the
	// store and link it into every agent. Every relative url is the same defect.
	//
	// IsGitURL is the predicate that draws the line exactly where the writers
	// do: every url fu records satisfies it (file:///abs passes through its own
	// branch), while "" and a bare relative path do not. Unreachable from fu's
	// own writes -- EncodeFields always writes url, and ParseArg cannot produce
	// an empty-URL git source -- but reachable by the hand edit or partially
	// damaged YAML that the ref_kind and commit guards above already accept as
	// worth defending against. A non-comparable row is refused by name for
	// `fu update <name>` and skipped by the batch, which is the same handling
	// those two get.
	if !source.IsGitURL(fields["url"]) {
		status.Reason = fmt.Sprintf("git source record has no usable url %q", fields["url"])
		return
	}
	// fu.yaml is a hand-editable file (the same reasoning the default: arm in
	// judgeUpdate relies on), so a branch lock can still arrive with no
	// recorded commit even though EncodeFields never produces that shape.
	// Comparing "" against a real resolved commit would silently report
	// every such row Updatable=true with nothing in its Current column.
	if fields["commit"] == "" {
		status.Reason = "branch source has no recorded commit"
		return
	}
	key := [2]string{fields["url"], fields["ref"]}
	commit, ok := resolvedCommits[key]
	if !ok {
		if cached, failedBefore := resolveErrs[key]; failedBefore {
			status.Reason = describeRemoteErr(cached)
			return
		}
		src := source.Source{Kind: source.KindGit, URL: fields["url"]}
		// ResolveRemoteRef forwards whatever context it is given and applies
		// no deadline of its own (internal/source/lsremote.go); without one
		// here, an unresponsive remote would hang `fu outdated` indefinitely.
		// Applied once per deduplicated (url, ref) pair, matching the clone
		// path's own convention (cloneSource, internal/source/git.go).
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		resolved, resolveErr := src.ResolveRemoteRef(ctx, fields["ref"])
		cancel()
		if resolveRemoteRefHook != nil {
			resolveRemoteRefHook(fields["url"], fields["ref"])
		}
		if resolveErr != nil {
			resolveErrs[key] = resolveErr
			status.Reason = describeRemoteErr(resolveErr)
			return
		}
		resolvedCommits[key] = resolved
		commit = resolved
	}
	status.Comparable = true
	status.Current = fields["commit"]
	status.Latest = commit
	status.Updatable = status.Current != commit
}

// describeRemoteErr distinguishes a vanished ref from a merely unreachable
// remote: they mean different things to a user reading `fu outdated`'s
// output (internal/source/lsremote.go's own doc comment on
// ErrRemoteRefNotFound), so the reason must name which one happened rather
// than collapsing both into one generic message.
//
// A record that cannot name a commit at all is the third case, and it is not a
// transport failure either (round 4): ResolveRemoteRef refuses an empty ref, an
// empty url and a ref the remote advertises symbolically (lsremote.go), all of
// which are facts about a hand-edited fu.yaml. Calling those "unreachable" told
// an online user their network was down -- the same misdirection the
// vanished-ref split exists to prevent, applied inconsistently. Checked before
// the fallback, so the fallback keeps its one honest meaning: fu could not talk
// to the remote.
func describeRemoteErr(err error) string {
	switch {
	case errors.Is(err, source.ErrRemoteRefNotFound):
		return fmt.Sprintf("recorded ref no longer exists: %v", err)
	case errors.Is(err, source.ErrRemoteRefUnusable):
		return fmt.Sprintf("recorded ref is unusable: %v", err)
	}
	return fmt.Sprintf("unreachable: %v", err)
}

// judgeLocalUpdate compares a local source's current content digest against
// baseline, the digest fu.yaml recorded when the skill was installed. There
// is no remote to poll for a local source, so its directory is projected and
// digested directly, the same read-only projection add and adopt already
// share (skill.ProjectDir / skill.DigestManifest) -- no clone is involved on
// either side of this comparison.
func judgeLocalUpdate(status *UpdateStatus, fields map[string]string, baseline string) {
	path := fields["path"]
	// The local arm's counterpart to judgeGitUpdate's url guard, and the wider
	// of the two: os.Stat and os.OpenRoot below both resolve a relative path
	// against the process's working directory, so without this the verdict
	// depended on where fu was run and `fu update` went on to publish content
	// from a cwd-relative directory into the store and link it into every agent.
	// (Every `file:` url is $PWD-independent by contrast -- go-git parses
	// file://../repo as host ".." with path "/repo" -- so the git guard closed
	// the narrower surface of the two.)
	//
	// This is the invariant DESIGN already states for local records; nothing
	// enforced it on the read side. Unreachable from fu's own writes, since
	// parseExistingLocal absolutizes and resolves symlinks before recording
	// (source.go), but reachable by the same hand edit the adjacent git guards
	// are written for.
	//
	// Refused before the stat, so a malformed record is not reported as a
	// reachability failure -- "unreachable: stat : no such file or directory"
	// told the user their disk was the problem, the same misdirection
	// ErrRemoteRefUnusable exists to prevent one field over. NoUpstream stays
	// false: this is a record the user can repair and should keep hearing about.
	if !filepath.IsAbs(path) {
		status.Reason = fmt.Sprintf("local source record has no usable path %q", path)
		return
	}
	// Lstat rather than Stat, and the arm below is why: openPreparedRoot opens
	// the recorded path with O_NOFOLLOW (internal/source/git.go), so a symlink
	// on the source root is a path `fu update` will not open, while os.Stat and
	// os.OpenRoot here both follow one at the leaf. Judging through the link
	// reported the skill updatable on every run while every update failed at
	// the open (with whichever errno the kernel gives O_NOFOLLOW on a symlink
	// -- ELOOP on Linux, ENOTDIR on macOS, and neither is a fact worth
	// quoting) -- permanent noise of exactly the kind the design requires
	// designed out, produced by a user doing the ordinary thing of moving a
	// source directory and symlinking it back. It is the same invariant the
	// IsAbs guard above restores, held by only one of the two sides until now.
	//
	// The leaf and only the leaf, which is O_NOFOLLOW's own scope: a symlink
	// anywhere in the parent chain is followed by both sides alike, so both
	// still agree.
	info, statErr := os.Lstat(path)
	if statErr != nil {
		status.Reason = fmt.Sprintf("local source unreachable: %v", statErr)
		return
	}
	// NoUpstream stays false with the rest of this arm's siblings: the record
	// is repairable, and the message says how, because the path is a directory
	// as far as the user can see and the refusal is about the link, not it.
	if info.Mode()&os.ModeSymlink != 0 {
		status.Reason = fmt.Sprintf(
			"local source %s is a symlink, which `fu update` refuses to open; record the directory it points at", path)
		return
	}
	subdir := fields["subdir"]
	if subdir == "" {
		subdir = "."
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		status.Reason = fmt.Sprintf("open local source %s: %v", path, err)
		return
	}
	defer root.Close()
	entries, err := skill.ProjectDir(root.FS(), subdir)
	if err != nil {
		status.Reason = fmt.Sprintf("project local source %s: %v", path, err)
		return
	}
	latest, err := skill.DigestManifest(entries)
	if err != nil {
		status.Reason = fmt.Sprintf("digest local source %s: %v", path, err)
		return
	}
	status.Comparable = true
	status.Current = baseline
	status.Latest = latest
	status.Updatable = baseline != latest
}

// judgeLocalModification computes SPEC rule 9's other, independent question:
// whether the store's own copy has drifted from the digest fu.yaml recorded
// as its install baseline. It never looks at the skill's source record --
// judgeUpdate already did, and separately -- so a hand-edit of the store
// copy and an upstream move are always reported as what they each are,
// never folded into a single verdict.
//
// A snapshot failure (e.g. the store no longer holds the skill's content)
// degrades only this half of the row: LocallyModified is left false and the
// failure is folded into whatever Reason judgeUpdate already produced,
// without discarding that verdict or failing the whole command.
func judgeLocalModification(status *UpdateStatus, pinned *store.Store, cfg *store.Config, name string) {
	// Recorded even when the comparison below cannot be completed: the
	// baseline is read straight out of fu.yaml and is what a refusal names as
	// the content the install actually recorded.
	status.Baseline = cfg.Digest(name)
	payload, err := pinned.SnapshotSkillPayload(name)
	if err != nil {
		status.Reason = foldReason(status.Reason, fmt.Sprintf("snapshot store content: %v", err))
		return
	}
	digest, err := digestOwnedPayload(payload)
	if err != nil {
		status.Reason = foldReason(status.Reason, fmt.Sprintf("digest store content: %v", err))
		return
	}
	status.StoreDigest = digest
	status.LocallyModified = digest != status.Baseline
}

// UpdateRefusesWithoutForce reports whether `fu update` will refuse this
// skill unless --force is passed. It lives here, on the judgement both front
// ends read, because it is a judgement: SPEC §5.2 puts the business logic in
// the core library so a second front end inherits the answer instead of
// re-deriving it.
//
// Round 2, Important #1: `fu outdated` had re-derived it as
// `Updatable && LocallyModified`, which is not the rule. selectUpdateTargets
// (application.go) refuses on store-side drift alone -- there is no Updatable
// condition -- so a hand-edited skill with nothing new upstream was omitted
// from `fu outdated` entirely and then refused by `fu update`. That is the
// broken chain design §3.4 exists to prevent: 若 update 注定拒绝而 outdated
// 不说，这条链就断了.
//
// Comparability is deliberately not folded in. A row that could not be judged
// is refused too, but for its own reason and with its own message, and
// `Comparable` already says so on the row.
func (s UpdateStatus) UpdateRefusesWithoutForce() bool { return s.LocallyModified }

// foldReason appends addition to an existing Reason, so a second,
// independent failure on the same row is never lost just because the field
// already held one.
func foldReason(reason, addition string) string {
	if reason == "" {
		return addition
	}
	return reason + "; " + addition
}
