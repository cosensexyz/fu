// internal/engine/reconcile.go
package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

// ErrOperationFailed is returned by Reconcile (and, wrapped, by Run and
// every write command built on it) whenever the pass leaves Result.Failed
// non-empty, so the process exits 1 (DESIGN §7) for a genuine operation
// failure -- e.g. a broken agent scan, or a filesystem error applying an
// action -- rather than reporting success.
//
// Decision (round 2 finding 4, left open by the review for this pass to
// make): Failed is the only Result field that reaches this sentinel.
// Conflicts, Missing, Reserved, Invalid, DisabledForeign, and Skipped do
// NOT -- each of those is an expected, actionable state that fu is
// correctly refusing to resolve on its own (unmanaged content in the way,
// the store missing a skill's content, a reserved-name collision, an
// invalid fu.yaml entry, a disabled skill still blocked by unmanaged
// content at its path, an agent directory that is itself a symlink), not a
// mechanical failure like a permission error or a non-directory skills
// path. They are reported prominently (printResult, on stderr) so the user
// can act on them, but the command that surfaced them still exited 0: fu
// did exactly what it could safely do and said so. Only Failed --
// something Diff never anticipated and Reconcile could not safely
// categorize -- is treated as this build's own failure. Foreign is
// different again: it is not printed at all -- not by printResult, and not
// by `fu status`, which runs its own Diff rather than consuming this field --
// so it cannot contradict anything a write command tells the user, and does
// not belong in the "reported prominently" list above.
var ErrOperationFailed = errors.New("one or more agent operations failed")

// Desired computes the desired link set for one agent from config
// (override wins, else global — SPEC §4.1).
//
// A skill name that collides with one of the agent's Reserved() entries
// (SPEC rule 11) is excluded from desired regardless of its on/off
// value, and never handed to Diff at all: ScanAgent already filters
// reserved names out of the *actual* state it reports, and applying that
// same filter to only one side of the desired-vs-actual comparison was
// finding 2 -- Diff would believe such a skill permanently absent from a
// directory it cannot see into, recreate it every run (silently
// swallowed as EEXIST), and never be able to remove it again, because
// Diff cannot emit a removal for an entry it cannot see. Every collision
// that was actually on for this agent is reported back in reserved so
// Reconcile can surface it instead of dropping it silently; one that was
// already off is inert -- nothing was ever desired for it, so there is
// no consequence for the user to be told about, unlike a non-reserved
// skill that is off and blocked by foreign content at its own path (see
// ReportDisabledForeign in diff.go), where the user's own disable is
// exactly what makes the block worth reporting.
//
// A skill name that fails validation is excluded from desired the same
// way (checked after the reserved-collision case above -- see the inline
// comment at that check for why the order matters), and unconditionally
// (round 3 finding 2, a regression in round 2
// finding 3's own fix): Diff's trailing "leftover" loop skips every name
// present in desired regardless of value, so a name Desired handed to Diff
// anyway -- leaving Diff's own per-skillName loop to report it and
// `continue`, never falling through to a removal decision -- was invisible
// to both of Diff's loops at once. A genuine fu link recorded under such a
// name (written by an older fu, or arriving via a future clone/pull,
// before this build's naming rules applied to it) was then neither removed
// nor even reported as foreign: `fu disable` printed "invalid ... will
// never be linked" while that exact link stayed on disk forever,
// contradicting its own message. Excluding the name here lets the trailing
// loop treat it like any other name desired has no opinion on, so a stray
// fu link is reclaimed (RemoveLink, its path built from the name ReadDir
// itself reported -- never from this unsafe one) while the report below
// still reaches the user. Diff keeps its own copy of this same check
// (diff.go) as a backstop for any caller that does not route through
// Desired first; production code always does, so that copy is normally
// unreachable, the same way isSinglePathComponent is already documented as
// redundant against skill.ValidateName there.
//
// cfg.SkillNames() itself now already excludes a name store.LoadConfig
// found invalid (round 4 finding 2): LoadConfig no longer aborts the whole
// load over one bad name, so this loop's own ValidateName check above --
// the one described in the previous paragraph -- never actually observes
// such a name for a *Config LoadConfig produced; it fires only for a
// Config built in memory without going through LoadConfig (AddSkill does
// not itself validate; see store's own doc comments). Config still tracks
// every name it excluded this way (store.Config.InvalidNames), so none of
// them stops being reported: a reserved-name collision among them is
// diagnosed per agent in the second loop below, and the rest are folded in
// once per pass by configInvalidNames -- both reusing this same
// Reserved/Invalid channel rather than adding a third one. Either way the
// reclamation this whole function exists for stays reachable: a name Config
// already excluded from SkillNames() never enters desired, so Diff's
// trailing "leftover" loop still reclaims a genuine fu link recorded under
// it.
func Desired(cfg *store.Config, a agent.Agent) (desired map[string]bool, reserved []Action, invalid []Action) {
	reservedNames := map[string]bool{}
	for _, r := range a.Reserved() {
		reservedNames[r] = true
	}
	desired = map[string]bool{}
	for _, s := range cfg.SkillNames() {
		on := cfg.Effective(s, a.Name())
		// Reserved is checked before validity, preserving the precedence
		// the two checks already had before this fix: a reserved name used
		// to be excluded from desired (and therefore from ever reaching
		// Diff's validation check) before Diff ran at all, since Desired's
		// reserved-filter was the only filter here. Codex's own real
		// Reserved() list (".system") is not a validly-formed skill name
		// either -- reserved markers are not skills and were never meant to
		// satisfy the Agent Skills naming rules -- so checking validity
		// first would relabel every such collision "invalid" instead of
		// "reserved", a strictly less specific diagnosis for exactly the
		// name that names it.
		if reservedNames[s] {
			if on {
				reserved = append(reserved, Action{
					Type:      ReportReserved,
					AgentName: a.Name(),
					Skill:     s,
					LinkPath:  filepath.Join(a.SkillsDir(), s),
				})
			}
			continue
		}
		if err := skill.ValidateName(s); err != nil || !isSinglePathComponent(s) {
			invalid = append(invalid, Action{
				Type:      ReportInvalid,
				AgentName: a.Name(),
				Skill:     s,
			})
			continue
		}
		desired[s] = on
	}
	// Names store.LoadConfig itself already excluded from cfg.SkillNames()
	// (round 4 finding 2) -- the loop above never sees these, so a second
	// pass over them is what keeps them reported at all.
	//
	// Only the reserved half of that reporting is decided per agent, and
	// only it belongs here (round 5 finding, correcting how round 4's fold
	// was written). Round 4 folded every isolated name straight into
	// invalid, unconditionally, which inverted the reserved-before-valid
	// precedence the loop above is careful to preserve: codex's own
	// ".system" marker is *both* reserved and not a validly-formed skill
	// name, so once LoadConfig started isolating it, it stopped reaching the
	// reserved branch and came out labelled "invalid" instead -- for every
	// agent, including agents that do not reserve it, and even when it was
	// off, which the reserved branch treats as inert precisely because
	// nothing was ever desired for it. The invalid half is a property of
	// fu.yaml rather than of any agent, and is folded in once per pass by
	// configInvalidNames below.
	for _, inv := range cfg.InvalidNames() {
		if !reservedNames[inv.Name] || !inv.Effective(a.Name()) {
			continue
		}
		reserved = append(reserved, Action{
			Type:      ReportReserved,
			AgentName: a.Name(),
			Skill:     inv.Name,
			// LinkPath is deliberately left empty here, unlike the
			// validly-named reserved collision above: this name failed
			// skill.ValidateName, and joining such a name onto a directory
			// is the exact operation round 3 finding 2 was about. Nothing
			// reads LinkPath for a ReportReserved (printResult renders
			// agent and skill only), so there is nothing to lose by it.
		})
	}
	return desired, reserved, invalid
}

// configInvalidNames folds the skill names store.LoadConfig isolated into
// ReportInvalid actions -- once for the whole pass, carrying no AgentName,
// because one bad key in fu.yaml is one fact about one file (round 5
// finding). Folded inside the per-agent loop, as round 4 wrote it, the same
// diagnostic printed once per detected agent, and vanished entirely when no
// agent was detected at all: a write command then said nothing whatsoever
// about an entry it was silently ignoring.
//
// A name is skipped here only when something else already accounts for it,
// which is a narrower test than "some agent reserves it" (round 8 finding).
// "Reserved" is the specific, actionable diagnosis where it applies --
// codex's ".system" marker is not a skill anyone should rename, and
// repeating it as "invalid skill name" for the agents that merely do not
// reserve it tells the user nothing true. But Desired only emits
// ReportReserved for an agent where the entry is *effective*, so suppressing
// on reservation alone left a hole: override the reserving agent off while a
// non-reserving one stays on, and neither report fires. The entry is
// silently ignored, and the user has no explanation for why an enabled skill
// was never reconciled.
//
// See alreadyReportedAsReserved for the exact condition and the four cases
// it has to divide correctly.
// alreadyReportedAsReserved reports whether the per-agent reserved
// diagnostics cover name, so the config-level Invalid report can stand down.
//
// Two situations qualify, and they are the two in which the user is not left
// without an explanation:
//
//   - some agent that reserves the name has it effective, so Desired emits
//     ReportReserved for that agent and names the real reason;
//   - some agent reserves it and no agent has it effective at all, so
//     nothing was ever desired and there is no consequence to report -- the
//     same "inert" reading Desired already applies to an off reserved
//     collision.
//
// What does *not* qualify is the case round 8 found: a reserving agent with
// the name overridden off, while a non-reserving agent still wants it. No
// ReportReserved fires, the skill is silently never delivered, and only the
// config-level Invalid report is left to say so.
func alreadyReportedAsReserved(agents []agent.Agent, invalid store.InvalidName) bool {
	reservedBySomeone, effectiveAnywhere := false, false
	for _, a := range agents {
		reserves := false
		for _, r := range a.Reserved() {
			if r == invalid.Name {
				reserves = true
				break
			}
		}
		reservedBySomeone = reservedBySomeone || reserves
		if !invalid.Effective(a.Name()) {
			continue
		}
		effectiveAnywhere = true
		if reserves {
			return true // this agent's ReportReserved covers it
		}
	}
	return reservedBySomeone && !effectiveAnywhere
}

func configInvalidNames(cfg *store.Config, agents []agent.Agent) []Action {
	var out []Action
	for _, inv := range cfg.InvalidNames() {
		if alreadyReportedAsReserved(agents, inv) {
			continue
		}
		out = append(out, Action{Type: ReportInvalid, Skill: inv.Name})
	}
	return out
}

// Result carries the non-mutating findings of one reconcile pass.
type Result struct {
	Warnings        []string // durable recovery/isolation notices that do not fit reconcile action categories
	Conflicts       []Action
	Foreign         []Action       // name fu.yaml has no opinion on at all; informational inventory, never printed by printResult and not consumed by `fu status` either (that command recomputes Diff)
	DisabledForeign []Action       // fu.yaml-known skill, disabled, blocked by unmanaged content at its own path; actionable, printed by printResult
	Missing         []Action       // desired CreateLink whose store-side target no longer exists
	Reserved        []Action       // desired skill name collides with an agent's reserved entry
	Invalid         []Action       // fu.yaml skill name fails validation (round 2 finding 3); computed by Desired so a genuine fu link recorded under the name is still reachable by Diff's removal loop (round 3 finding 2)
	Skipped         []string       // agents skipped: skills dir is a symlink (SPEC rule 10)
	Failed          []FailedAction // per-entry or per-agent failures isolated from the rest (finding I3)
}

// Empty reports that a reconcile pass produced no finding a caller could act
// on or display. Foreign is excluded on the same grounds as UserReports: it is
// inventory, not a finding about this run. `fu status` reports the same state
// from its own Diff pass, so nothing consumes this field.
func (r Result) Empty() bool {
	return len(r.Warnings) == 0 && len(r.Conflicts) == 0 && len(r.DisabledForeign) == 0 && len(r.Missing) == 0 &&
		len(r.Reserved) == 0 && len(r.Invalid) == 0 && len(r.Skipped) == 0 && len(r.Failed) == 0
}

// UserReport is one finding ordinary write commands should present. Foreign
// inventory is intentionally absent; it belongs to the future status command,
// while every report returned here is actionable in the current command.
type UserReport struct {
	Kind   ActionType
	Action Action
	Err    error
}

// UserReports applies the engine's visibility and ordering policy to a
// reconcile result. UI layers only format these structured reports.
//
// Identical findings are collapsed here, and only here. A batch command runs
// one transaction per constituent operation and each ends with its own
// reconcile, so mergeResult accumulates the same standing finding once per
// item: `fu add --all` over three skills with one pre-existing foreign
// directory printed the same `conflict: claude/alpha occupied by unmanaged
// content` three times, and three identical lines read as three separate
// conflicts. Output was O(candidates × findings) -- a 30-skill repo with five
// conflicts printed 150 lines.
//
// This is the visibility and ordering boundary, which is why the collapse
// belongs here rather than in mergeResult: the accumulated Result is also the
// structured value a second front end would consume, and one operation really
// did produce each of those findings.
func (r Result) UserReports() []UserReport {
	reports := make([]UserReport, 0, len(r.Conflicts)+len(r.DisabledForeign)+len(r.Missing)+len(r.Reserved)+len(r.Invalid)+len(r.Skipped)+len(r.Failed))
	// Target is part of the key, not just agent/skill: a conflict carries the
	// retired path only when fu moved its own link aside and could not put it
	// back, and collapsing that into an earlier bare conflict for the same
	// name would drop the user's only pointer to where their content went.
	type reportKey struct {
		kind                      ActionType
		agent, skill, target, err string
	}
	seen := make(map[reportKey]bool, cap(reports))
	add := func(report UserReport) {
		key := reportKey{
			kind:   report.Kind,
			agent:  report.Action.AgentName,
			skill:  report.Action.Skill,
			target: report.Action.Target,
		}
		if report.Err != nil {
			key.err = report.Err.Error()
		}
		if seen[key] {
			return
		}
		seen[key] = true
		reports = append(reports, report)
	}
	appendActions := func(kind ActionType, actions []Action) {
		for _, action := range actions {
			add(UserReport{Kind: kind, Action: action})
		}
	}
	appendActions(ReportConflict, r.Conflicts)
	appendActions(ReportDisabledForeign, r.DisabledForeign)
	appendActions(ReportMissing, r.Missing)
	appendActions(ReportReserved, r.Reserved)
	appendActions(ReportInvalid, r.Invalid)
	for _, agentName := range r.Skipped {
		add(UserReport{Kind: ReportSkipped, Action: Action{AgentName: agentName}})
	}
	for _, failed := range r.Failed {
		add(UserReport{Kind: ReportFailed, Action: failed.Action, Err: failed.Err})
	}
	return reports
}

// FailedAction pairs an Action -- or, for a failure discovered before
// Diff ever ran for that agent (e.g. ScanAgent itself erroring), a
// placeholder Action carrying only AgentName -- with the unexpected
// error hit while computing or applying it.
type FailedAction struct {
	Action Action
	Err    error
}

// mergeResult accumulates the reconcile findings of one operation into dst,
// so a multi-operation command (adopt's per-skill transactions, add's
// batch) can surface every constituent pass, not just the last one (round 6
// finding I1).
func mergeResult(dst *Result, src Result) {
	dst.Warnings = append(dst.Warnings, src.Warnings...)
	dst.Conflicts = append(dst.Conflicts, src.Conflicts...)
	dst.Foreign = append(dst.Foreign, src.Foreign...)
	dst.DisabledForeign = append(dst.DisabledForeign, src.DisabledForeign...)
	dst.Missing = append(dst.Missing, src.Missing...)
	dst.Reserved = append(dst.Reserved, src.Reserved...)
	dst.Invalid = append(dst.Invalid, src.Invalid...)
	dst.Skipped = append(dst.Skipped, src.Skipped...)
	dst.Failed = append(dst.Failed, src.Failed...)
}

// Reconcile loads the durable config and applies Diff for every given agent.
// It owns the same checked-root, lock, and recovery boundary as every other
// exported mutating engine entry point. Idempotent; RemoveLink re-verifies
// ownership immediately before unlinking to close the TOCTOU window
// (DESIGN §2).
//
// Every per-entry and per-agent failure is isolated into Failed rather
// than aborting the pass mid-loop (finding I3): a self-referential symlink
// under the store making one agent's ScanAgent fail with ELOOP, or one
// agent's skills directory being unreadable or a plain file, used to make
// *every* action for *every remaining agent* silently not happen --
// including agents with nothing wrong with them -- because the loop
// below returned on the first error it saw. That is the same isolation
// discipline ParentIsSymlink already got via Skipped; it just was not
// carried through to genuine errors. This also fixes an ordering
// surprise: Reconcile runs after Store.Commit in the write pipeline, so
// an aborting error here used to make a command like NewSkill report
// failure to the user when the config entry and the commit were already
// durable.
//
// Isolation is not the same as success, though (round 2 finding 4): once
// every agent has actually been processed this way, a non-empty Failed
// still makes Reconcile return ErrOperationFailed (see its doc comment for
// the full Failed-vs-everything-else decision). Isolating a failure into a
// Result field and reporting the pass as having failed are independent
// properties -- the former is about *what else still gets done* in the
// same pass, the latter about *what the caller, and ultimately the process
// exit code, is told happened*. Before this, both were bundled together as
// "isolated therefore nil error," which made a genuine per-agent failure
// (a permission error, a non-directory skills path) indistinguishable from
// an expected, actionable report like Conflicts or Missing: every write
// command exited 0 either way, and printResult's diagnostics went to
// stdout, so a script redirecting only stdout saw a clean run while an
// agent silently got nothing.
func Reconcile(st *store.Store, agents []agent.Agent) (res Result, retErr error) {
	session, err := st.BeginWrite()
	if err != nil {
		return res, fmt.Errorf("open checked reconcile session: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, session.Close())
	}()
	checked := session.Store
	homeRoot, err := checked.Root()
	if err != nil {
		return res, fmt.Errorf("use checked reconcile root: %w", err)
	}
	storeRoot, err := checked.StoreRoot()
	if err != nil {
		return res, fmt.Errorf("use checked store root for reconcile: %w", err)
	}
	retErr = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		recoveryResult, err := RecoverPendingReporting(checked)
		mergeResult(&res, recoveryResult)
		if err != nil {
			return fmt.Errorf("recover pending transactions before reconcile: %w", err)
		}
		cfg, err := store.LoadConfigRoot(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("load config %s for reconcile: %w", st.ConfigPath(), err)
		}
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check config writable before reconcile: %w", err)
		}
		if err := session.CheckCanonicalPath(); err != nil {
			return err
		}
		reconcileResult, err := reconcileChecked(checked, cfg, agents, nil)
		mergeResult(&res, reconcileResult)
		return err
	})
	return res, retErr
}

// beforeApply is a test-only seam: when non-nil, it runs for one agent
// after Diff has computed that agent's actions but before Reconcile
// applies any of them, so a test can perturb disk state into the exact
// gap verifyFuLink's re-check exists to close (DESIGN §2). Production
// code always calls Reconcile, which passes a nil hook; the seam stays
// unexported so it never widens Reconcile's public signature.
type beforeApply func(a agent.Agent, acts []Action)

type reconcileHooks struct {
	beforeApply      beforeApply
	beforeLinkRetire func(agent.Agent, Action)
	// afterLinkRetire runs in the window between the approved link's rename to
	// its retired name and the post-move validation, so a test can reoccupy the
	// original name and reach the one path where fu moved its own link aside
	// and could not put it back. That path is the only producer of a conflict
	// carrying Action.Target, and no test could reach it otherwise: after the
	// rename the original name is free, and nothing single-threaded reoccupies
	// it. Production always passes nil.
	afterLinkRetire func(agent.Agent, Action, string)
}

// reconcile is the package-local harness for tests that need an in-memory
// Config or a boundary hook. Exported callers use Reconcile, which owns the
// lock, pending recovery, and durable config reload; the write pipeline calls
// reconcileChecked only while it already holds that same boundary.
func reconcile(st *store.Store, cfg *store.Config, agents []agent.Agent, hook beforeApply) (res Result, retErr error) {
	return reconcileWithHooks(st, cfg, agents, reconcileHooks{beforeApply: hook})
}

func reconcileWithHooks(st *store.Store, cfg *store.Config, agents []agent.Agent, hooks reconcileHooks) (res Result, retErr error) {
	session, err := st.BeginWrite()
	if err != nil {
		return res, fmt.Errorf("open checked reconcile session: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, session.Close())
	}()
	if err := session.CheckCanonicalPath(); err != nil {
		return res, err
	}
	return reconcileCheckedWithHooks(session.Store, cfg, agents, hooks)
}

func reconcileChecked(st *store.Store, cfg *store.Config, agents []agent.Agent, hook beforeApply) (Result, error) {
	return reconcileCheckedWithHooks(st, cfg, agents, reconcileHooks{beforeApply: hook})
}

func reconcileCheckedWithHooks(st *store.Store, cfg *store.Config, agents []agent.Agent, hooks reconcileHooks) (Result, error) {
	var res Result
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return res, fmt.Errorf("use checked skills root for reconcile: %w", err)
	}
	// Config-level findings first: they hold for the pass as a whole, and
	// must reach the user even when the loop below runs zero times because
	// no agent was detected.
	res.Invalid = append(res.Invalid, configInvalidNames(cfg, agents)...)
	for _, a := range agents {
		state, err := ScanAgent(a, st.SkillsDir())
		if err != nil {
			// Isolated per agent (finding I3): a broken scan for this agent
			// (e.g. ELOOP from a self-referential symlink under the store, or
			// this agent's skills dir being unreadable or a plain file) must
			// not starve every other, perfectly healthy agent in the same
			// pass. There is no Action yet -- Diff never ran for this agent --
			// so the placeholder carries only the agent's name.
			res.Failed = append(res.Failed, FailedAction{Action{AgentName: a.Name()}, err})
			continue
		}
		if state.ParentIsSymlink {
			res.Skipped = append(res.Skipped, a.Name())
			continue
		}
		if state.ParentMissing {
			state, err = createAndScanAgentDir(a, st.SkillsDir(), nil)
			if err != nil {
				res.Failed = append(res.Failed, FailedAction{Action{AgentName: a.Name()}, err})
				continue
			}
		}
		// Every mutation below goes through a descriptor rather than the
		// pathname, so replacing the directory after it was checked cannot
		// redirect a create or a remove (round 7 finding; see
		// AgentState.OpenCheckedDir). applyToAgent owns that descriptor's
		// lifetime -- it is a real file handle, and a long-running caller
		// reconciling repeatedly would otherwise accumulate one per pass
		// (round 8 finding).
		applyToAgent(st, skillsRoot, cfg, a, state, hooks, &res)
	}
	if len(res.Failed) > 0 {
		return res, ErrOperationFailed
	}
	return res, nil
}

// createAndScanAgentDir creates a missing agent skills directory and scans it
// immediately so the apply phase can pin the directory identity. afterCreate
// is a test-only seam for the namespace-replacement window between those two
// operations; production always passes nil.
func createAndScanAgentDir(a agent.Agent, storeSkillsDir string, afterCreate func()) (AgentState, error) {
	// Anchored at the deepest component that already exists, so a symlink
	// appearing at one of the missing ones cannot redirect creation.
	created, err := mkdirAllAnchoredIdentity(a.SkillsDir())
	if err != nil {
		return AgentState{}, err
	}
	if afterCreate != nil {
		afterCreate()
	}
	// Re-scan so the state carries the identity of the directory that now
	// exists; OpenCheckedDir has nothing to compare against otherwise.
	state, err := ScanAgent(a, storeSkillsDir)
	if err != nil {
		return AgentState{}, err
	}
	if state.dirInfo == nil || !os.SameFile(created, state.dirInfo) {
		return AgentState{}, fmt.Errorf("%s is not the directory fu just created: it was replaced before the verification scan, so refusing to reconcile it", a.SkillsDir())
	}
	return state, nil
}

// applyToAgent runs one agent's diff and applies it, holding the checked
// directory's descriptor for exactly as long as that takes. Findings are
// appended to res; failures are isolated here rather than propagated, so one
// agent cannot starve the rest of the pass (finding I3).
func applyToAgent(st *store.Store, skillsRoot *os.Root, cfg *store.Config, a agent.Agent, state AgentState, hooks reconcileHooks, res *Result) {
	root, err := state.OpenCheckedDir()
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Action{AgentName: a.Name()}, err})
		return
	}
	defer root.Close()

	desired, reserved, invalid := Desired(cfg, a)
	res.Reserved = append(res.Reserved, reserved...)
	res.Invalid = append(res.Invalid, invalid...)
	acts := Diff(desired, state, st.SkillsDir())
	if hooks.beforeApply != nil {
		hooks.beforeApply(a, acts)
	}
	for _, act := range acts {
		switch act.Type {
		case CreateLink:
			// Validate the store-side target through the pinned skills
			// descriptor and without following its final component. Missing
			// content and wrong-type replacements are both unavailable skill
			// directories; neither may receive a live agent link.
			targetInfo, err := skillsRoot.Lstat(act.Skill)
			if err != nil {
				if os.IsNotExist(err) {
					res.Missing = append(res.Missing, act)
					continue
				}
				res.Failed = append(res.Failed, FailedAction{act, err})
				continue
			}
			if !targetInfo.IsDir() {
				res.Missing = append(res.Missing, act)
				continue
			}
			// Created relative to the checked directory's descriptor, not
			// its pathname. act.Skill is the entry's own name (Diff builds
			// LinkPath as agentDir/skill), and the target stays absolute;
			// symlinkat creates that target text without traversing it.
			if err := root.Symlink(act.Target, act.Skill); err != nil {
				if os.IsExist(err) {
					// Diff's desired-vs-actual lookup is case-sensitive; on
					// macOS's case-insensitive filesystem a differently-cased
					// foreign entry (e.g. "Alpha") can already occupy this
					// exact path for skill "alpha". Discarding EEXIST here
					// used to mean fu.yaml and `fu list` both claimed the
					// link existed while nothing was actually delivered
					// (finding I4) -- report it like any other occupied
					// path instead.
					res.Conflicts = append(res.Conflicts, act)
				} else {
					res.Failed = append(res.Failed, FailedAction{act, err})
				}
				continue
			}
		case RemoveLink:
			// An entry that has vanished since Diff looked is not a
			// conflict (round 5 finding): there is nothing left to
			// remove, and nothing the user needs to act on. verifyFuLink
			// answers false here for the same reason it does for an
			// entry swapped out from under fu, and printResult renders
			// that one answer as "occupied by unmanaged content" -- the
			// opposite of what happened. On a rebuild (RemoveLink +
			// CreateLink for one skill) that reported the skill as a
			// conflict while its link was, in the same pass,
			// successfully created.
			outcome, retiredName, err := retireFuLink(root, act.Skill, st.SkillsDir(),
				func() {
					if hooks.beforeLinkRetire != nil {
						hooks.beforeLinkRetire(a, act)
					}
				},
				func(retired string) {
					if hooks.afterLinkRetire != nil {
						hooks.afterLinkRetire(a, act, retired)
					}
				})
			if err != nil {
				// A permission or I/O failure is not a statement about
				// ownership and must not be reported as one (round 8
				// finding). Collapsing it into "not owned" produced a
				// Conflict -- an expected, actionable state that still
				// exits 0 -- while the link stayed active and the command
				// told the user the change had taken effect.
				res.Failed = append(res.Failed, FailedAction{act, err})
				continue
			}
			if outcome == linkRetireConflict {
				conflict := act
				if retiredName != "" {
					// fu moved the link aside and could not put it back
					// because the original name was reoccupied. Naming the
					// retired path is the difference between a user who can
					// find their content and one who cannot (round 18 M10).
					conflict.Target = filepath.Join(a.SkillsDir(), retiredName)
				}
				res.Conflicts = append(res.Conflicts, conflict)
				continue
			}
		case ReportConflict:
			res.Conflicts = append(res.Conflicts, act)
		case ReportForeign:
			res.Foreign = append(res.Foreign, act)
		case ReportDisabledForeign:
			res.DisabledForeign = append(res.DisabledForeign, act)
		case ReportInvalid:
			res.Invalid = append(res.Invalid, act)
		default:
			// Every action Diff can emit is handled above. A new one arriving
			// here is a bug, and silently dropping it would make the report
			// claim work was done that never was.
			res.Failed = append(res.Failed, FailedAction{act, fmt.Errorf("unhandled reconcile action %v", act.Type)})
		}
	}
}

type agentDirReader interface {
	Lstat(string) (os.FileInfo, error)
	Readlink(string) (string, error)
}

func inspectFuLink(root agentDirReader, name, storeSkillsDir string) (os.FileInfo, string, bool, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, "", false, err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		// A real file or directory: not fu's, and nothing went wrong in
		// establishing that.
		return fi, "", false, nil
	}
	target, err := root.Readlink(name)
	if err != nil {
		return nil, "", false, err
	}
	// name is the entry's own name -- the same name ScanAgent read from the
	// directory listing when it classified this entry, so this re-check asks
	// ownsLink exactly the question the scan asked.
	return fi, target, ownsLink(storeSkillsDir, name, target), nil
}

type linkRetireOutcome uint8

const (
	linkRetireRemoved linkRetireOutcome = iota
	linkRetireAbsent
	linkRetireConflict
)

// retireFuLink moves the approved link away from its live name before it
// removes anything. The unpredictable retired name turns replacement of the
// original name into a harmless new occupant, while the post-move identity
// and target checks ensure the object removed is the one that was approved.
// The retired name is returned alongside the outcome so a conflict can name
// it. Without that, an object parked at .fu-retired-<hex> because the restore
// found the original name reoccupied was reported only as "occupied by
// unmanaged content" -- naming a path the user could look at, while the thing
// fu had actually moved sat under a dot-name nothing mentioned (round 18
// finding M10).
func retireFuLink(root *checkedAgentDir, name, storeSkillsDir string, beforeRetire func(), afterRetire func(string)) (linkRetireOutcome, string, error) {
	approved, approvedTarget, owned, err := inspectFuLink(root, name, storeSkillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return linkRetireAbsent, "", nil
		}
		return linkRetireConflict, "", err
	}
	if !owned {
		return linkRetireConflict, "", nil
	}
	if beforeRetire != nil {
		beforeRetire()
	}

	retired, err := store.RetireNameAt(root.file, name, ".fu-retired-")
	if err != nil {
		if os.IsNotExist(err) {
			return linkRetireAbsent, "", nil
		}
		return linkRetireConflict, "", err
	}
	if afterRetire != nil {
		afterRetire(retired)
	}

	moved, inspectErr := root.Lstat(retired)
	movedTarget := ""
	if inspectErr == nil && moved.Mode()&os.ModeSymlink != 0 {
		movedTarget, inspectErr = root.Readlink(retired)
	}
	if inspectErr != nil || moved.Mode()&os.ModeSymlink == 0 || !sameCheckedEntry(approved, moved) || movedTarget != approvedTarget {
		restoreErr := store.RestoreRetiredAt(root.file, retired, name)
		if restoreErr != nil && !os.IsExist(restoreErr) {
			if inspectErr != nil {
				return linkRetireConflict, retired, errors.Join(inspectErr, fmt.Errorf("restore mismatched retired link: %w", restoreErr))
			}
			return linkRetireConflict, retired, fmt.Errorf("restore mismatched retired link: %w", restoreErr)
		}
		// A restore that hit EEXIST leaves the object parked under the retired
		// name: the caller must be able to say where it went.
		if restoreErr != nil {
			return linkRetireConflict, retired, nil
		}
		return linkRetireConflict, "", nil
	}
	if err := root.Remove(retired); err != nil && !os.IsNotExist(err) {
		return linkRetireConflict, retired, err
	}
	return linkRetireRemoved, "", nil
}

func sameCheckedEntry(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*unix.Stat_t)
	rightStat, rightOK := right.Sys().(*unix.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino
}
