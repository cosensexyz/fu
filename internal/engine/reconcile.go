// internal/engine/reconcile.go
package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
// different again: it is not printed at all (reserved for a future `fu
// status`), so it cannot contradict anything a write command tells the
// user, and does not belong in the "reported prominently" list above.
var ErrOperationFailed = errors.New("reconcile: one or more agent operations failed")

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
		if !reservedNames[inv.Name] || !cfg.Effective(inv.Name, a.Name()) {
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
func alreadyReportedAsReserved(cfg *store.Config, agents []agent.Agent, name string) bool {
	reservedBySomeone, effectiveAnywhere := false, false
	for _, a := range agents {
		reserves := false
		for _, r := range a.Reserved() {
			if r == name {
				reserves = true
				break
			}
		}
		reservedBySomeone = reservedBySomeone || reserves
		if !cfg.Effective(name, a.Name()) {
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
		if alreadyReportedAsReserved(cfg, agents, inv.Name) {
			continue
		}
		out = append(out, Action{Type: ReportInvalid, Skill: inv.Name})
	}
	return out
}

// Result carries the non-mutating findings of one reconcile pass.
type Result struct {
	Conflicts       []Action
	Foreign         []Action       // name fu.yaml has no opinion on at all; informational inventory reserved for a future `fu status`, never printed by printResult
	DisabledForeign []Action       // fu.yaml-known skill, disabled, blocked by unmanaged content at its own path; actionable, printed by printResult
	Missing         []Action       // desired CreateLink whose store-side target no longer exists
	Reserved        []Action       // desired skill name collides with an agent's reserved entry
	Invalid         []Action       // fu.yaml skill name fails validation (round 2 finding 3); computed by Desired so a genuine fu link recorded under the name is still reachable by Diff's removal loop (round 3 finding 2)
	Skipped         []string       // agents skipped: skills dir is a symlink (SPEC rule 10)
	Failed          []FailedAction // per-entry or per-agent failures isolated from the rest (finding I3)
}

// FailedAction pairs an Action -- or, for a failure discovered before
// Diff ever ran for that agent (e.g. ScanAgent itself erroring), a
// placeholder Action carrying only AgentName -- with the unexpected
// error hit while computing or applying it.
type FailedAction struct {
	Action Action
	Err    error
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
		if err := RecoverPending(checked); err != nil {
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
		res, err = reconcileChecked(checked, cfg, agents, nil)
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

// reconcile is the package-local harness for tests that need an in-memory
// Config or a boundary hook. Exported callers use Reconcile, which owns the
// lock, pending recovery, and durable config reload; the write pipeline calls
// reconcileChecked only while it already holds that same boundary.
func reconcile(st *store.Store, cfg *store.Config, agents []agent.Agent, hook beforeApply) (res Result, retErr error) {
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
	return reconcileChecked(session.Store, cfg, agents, hook)
}

func reconcileChecked(st *store.Store, cfg *store.Config, agents []agent.Agent, hook beforeApply) (Result, error) {
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
		applyToAgent(st, skillsRoot, cfg, a, state, hook, &res)
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
func applyToAgent(st *store.Store, skillsRoot *os.Root, cfg *store.Config, a agent.Agent, state AgentState, hook beforeApply, res *Result) {
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
	if hook != nil {
		hook(a, acts)
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
			// Checked and removed through the same descriptor, so the entry
			// whose ownership is verified is necessarily the entry that
			// gets deleted (round 7 finding).
			owned, err := verifyFuLink(root, act.Skill, st.SkillsDir())
			if err != nil {
				if os.IsNotExist(err) {
					// Vanished since Diff looked: nothing to remove, and
					// nothing to report.
					continue
				}
				// A permission or I/O failure is not a statement about
				// ownership and must not be reported as one (round 8
				// finding). Collapsing it into "not owned" produced a
				// Conflict -- an expected, actionable state that still
				// exits 0 -- while the link stayed active and the command
				// told the user the change had taken effect.
				res.Failed = append(res.Failed, FailedAction{act, err})
				continue
			}
			if !owned {
				res.Conflicts = append(res.Conflicts, act)
				continue
			}
			if err := root.Remove(act.Skill); err != nil && !os.IsNotExist(err) {
				res.Failed = append(res.Failed, FailedAction{act, err})
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

// verifyFuLink re-checks at execution time that name, inside the agent
// directory root was opened for, is still a symlink owned by fu; anything
// else was swapped in since Diff ran and must be left alone.
//
// Both the check and the removal that follows it go through root (round 7
// finding), so the entry whose ownership is approved here is necessarily
// the entry that gets deleted. Addressed by pathname, they could be
// different entries in different directories: replacing the agent's skills
// directory with a symlink between Diff and apply used to make this
// function approve a link the *attacker* had placed at the same name --
// pointing at the real store, so it passed -- and os.Remove then deleted
// it. The window between this check and the removal is narrower still than
// that, but it is the one DESIGN §2 documents as irreducible: nothing
// stands between an fstatat and an unlinkat on the same descriptor except
// time.
// It returns three distinguishable outcomes, because they call for
// different handling and collapsing them lost a real failure (round 8
// finding): (true, nil) the entry is fu's and may be removed; (false, nil)
// it is genuinely something else, which is a conflict; and (_, err) fu
// could not tell -- the entry is gone (os.IsNotExist, a no-op) or
// inspection failed outright (a permission or I/O error, a failed action).
// Returning a bare bool turned every one of those errors into "not owned",
// so a directory fu could not read was reported as unmanaged content
// occupying the path, the command exited 0, and the link stayed where it
// was.
type agentDirReader interface {
	Lstat(string) (os.FileInfo, error)
	Readlink(string) (string, error)
}

func verifyFuLink(root agentDirReader, name, storeSkillsDir string) (bool, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return false, err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		// A real file or directory: not fu's, and nothing went wrong in
		// establishing that.
		return false, nil
	}
	target, err := root.Readlink(name)
	if err != nil {
		return false, err
	}
	// name is the entry's own name -- the same name ScanAgent read from the
	// directory listing when it classified this entry, so this re-check asks
	// ownsLink exactly the question the scan asked.
	return ownsLink(storeSkillsDir, name, target), nil
}
