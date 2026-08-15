// internal/engine/adopt.go
package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

// AdoptSummary reports one successfully adopted skill.
type AdoptSummary struct {
	Name      string
	Agents    []string // agents whose entries were switched to store links
	Warnings  []string // per-skill observations that did not abort the adopt
	Operation OperationOutcome
}

// AdoptResult aggregates one `fu adopt` run.
type AdoptResult struct {
	Adopted   []AdoptSummary
	Pending   []AdoptSummary // committed to the store, but agent switching is incomplete
	Conflicts []string       // names skipped because their content differs across agents
	Skipped   []string       // names already managed by fu
	Warnings  []string
	Failed    []FailedAction
	// PreflightConflicts are capture-time races or filesystem conflicts, not
	// malformed candidate content.
	PreflightConflicts []FailedAction
	// Reconcile accumulates the trailing reconcile findings of every
	// constituent operation, so a switch failure that isolated an agent is
	// never silent (round 6 finding I1). The CLI prints it via printResult.
	Reconcile Result
}

// Empty reports that the run produced nothing to say: nothing adopted, nothing
// pending, nothing conflicted, skipped, warned, failed, and no reconcile
// finding of any kind. It is the engine's verdict, not the CLI's.
//
// The CLI used to compute this as an eleven-field emptiness expression over
// AdoptResult and AdoptResult.Reconcile. Any second front end had to reproduce
// it exactly or disagree with the CLI about whether a run was a no-op, which is
// the duplication SPEC §5.2 and DESIGN §1 exist to prevent -- and the copy in
// the CLI had already drifted, omitting DisabledForeign, so a run could print a
// disabled-foreign diagnostic and then claim nothing happened (round 18
// finding I20).
func (r AdoptResult) Empty() bool {
	return len(r.Adopted) == 0 && len(r.Pending) == 0 && len(r.Conflicts) == 0 &&
		len(r.Skipped) == 0 && len(r.Warnings) == 0 && len(r.Failed) == 0 && len(r.PreflightConflicts) == 0 &&
		r.Reconcile.Empty()
}

// AdoptScope distinguishes an omitted agent selector from an explicitly
// empty one. All and Agent are mutually exclusive: All selects every
// detected agent, while Agent limits inventory and switching to one name.
type AdoptScope struct {
	All   bool
	Agent string
}

// errInstalledNotSwitched marks an adopt failure where the skill was
// committed and registered but the agent-side switching failed (round 13
// finding I4). It is a distinct class from the install-invalid failures:
// the transaction stays open, so the next write command's recovery finishes
// the switching.
var errInstalledNotSwitched = errors.New("installed but not switched")

// ErrWholeDirRootSkillUnsupported reports a whole-directory target that is
// itself a skill. The replacement-directory model cannot give that root skill
// a managed child name without changing the agent's view.
var ErrWholeDirRootSkillUnsupported = errors.New("whole-directory root skill cannot be adopted safely")

// ErrEmptyAgentScope reports `--agent ""`: the flag was passed with an empty
// value, which is a shell mistake rather than a request. It is exported
// because the same refusal is reachable from three places -- AdoptScope, the
// adopt command, and the toggle commands -- and three copies of the same
// literal is three chances for them to drift apart.
var ErrEmptyAgentScope = errors.New("agent scope cannot be empty")

// ErrUnknownAgent reports an --agent value naming no adapter fu knows. It is a
// malformed flag value typed on the command line, so the CLI classifies it as
// a usage error (DESIGN §7 exit code 2) -- the same class `--agent ""` already
// exits with. An agent that is known but simply not installed on this machine
// is a different thing entirely and stays an ordinary operation failure.
var ErrUnknownAgent = errors.New("unknown agent")

// errAdoptPreflight marks a failure raised while capturing adopt targets --
// inside Preflight, which DESIGN §6 places before the transaction record is
// written. Nothing is durable at that point and no WAL exists, so there is
// nothing for a later write command to finish and nothing to preserve for
// inspection; aborting the run dropped every remaining candidate over a
// condition affecting one. captureAdoptTarget reaches pairBoundAdoptRoot,
// which wraps ErrTxnConflict, so without this marker the generic conflict
// abort swallowed the whole run.
var errAdoptPreflight = errors.New("adopt preflight failed before any transaction record")

// AdoptSkill is one candidate adopted from a source directory: installed
// like add, then its originals in every agent that held it are archived and
// replaced by store links (SPEC §5.1 adopt; scenario 6).
type AdoptCandidate struct {
	Name      string
	Dir       string // source directory (agent entry or symlink target), read-only
	SourceDir string // unique symlink target path, if any ("" = none)
	Agents    []string
}

// adoptEntry describes one foreign entry an adopt scan found.
type adoptEntry struct {
	agentName  string
	name       string
	dir        string // real dir path or symlink target
	symlinkSrc string // symlink target path ("" for a real dir)
	// sourceAmbiguous records a merged name whose holders do not agree on a
	// single external source (including a mix of external links and real
	// per-entry directories). It makes source reporting independent of scan
	// order without conflating that presentation fact with content conflict.
	sourceAmbiguous bool
}

// adoptScan is the inventory phase's output. It grew from a bare entry list
// because two of its findings have to reach the user: a rejected candidate
// needs its reason (SPEC rule 7 requires a reason for non-compliance), and a refused agent
// needs to be isolated rather than aborting the run (round 18 findings I11
// and I12).
//
//   - entries: every adoptable entry found.
//   - warnings: per-agent scan failures (finding M4); never fatal.
//   - failed: per-candidate and per-agent refusals with their reasons; the
//     CLI renders these as "invalid:".
//   - failedScans: agents whose scan failed, so the later classification does
//     not re-warn for them (round 9 finding M2).
//   - states: successful agent inventories reused for switch classification;
//     the per-skill loop must not rescan every agent.
type adoptScan struct {
	entries     []adoptEntry
	warnings    []string
	failed      []FailedAction
	failedScans map[string]bool
	states      map[string]AgentState
}

// scanAdoptEntries inventories every adoptable entry of the scanned agents.
// An agent whose skills directory is itself a symlink (SPEC rule 10's
// whole-directory form) is scanned read-only through its target: only
// entries that are valid skills are adoption candidates -- everything else
// in the target is preserved by the whole-directory switch as a passthrough
// link, so it must never be adopted as a candidate. The same filter applies
// to the per-entry form (finding I3): a directory that is not a valid skill
// is skipped, never fatal to the run.
func scanAdoptEntries(st *store.Store, agents []agent.Agent) (adoptScan, error) {
	return scanAdoptEntriesWithHooks(st, agents, hooks{})
}

func scanAdoptEntriesWithHooks(st *store.Store, agents []agent.Agent, h hooks) (adoptScan, error) {
	var entries []adoptEntry
	var warnings []string
	var failed []FailedAction
	failedScans := map[string]bool{}
	states := map[string]AgentState{}
	// invalidOnce keeps one reason per name: the same broken skill is often
	// present in several agents, and repeating it per agent buries the fix.
	invalidOnce := map[string]bool{}
	reject := func(name string, err error) {
		if invalidOnce[name] {
			return
		}
		invalidOnce[name] = true
		failed = append(failed, FailedAction{Action{Skill: name}, err})
	}
	scanned := adoptScan{failedScans: failedScans, states: states}
	for _, a := range agents {
		state, err := h.scanAdoptInventory(a, st.SkillsDir())
		if err != nil {
			failedScans[a.Name()] = true
			warnings = append(warnings, fmt.Sprintf("agent %s: skills scan failed: %v", a.Name(), err))
			continue // per-agent isolation happens in phase 2; keep scanning
		}
		states[a.Name()] = state
		if state.ParentIsSymlink {
			target, err := wholeDirTarget(a)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("agent %s: resolve whole-directory target: %v", a.Name(), err))
				continue
			}
			// Isolated to this agent, not fatal to the run. DESIGN §6 requires
			// refusing the entire agent while preserving its link and target,
			// and requires one skill's failure not to affect other items; returning here stopped the
			// command before any agent was processed, so a second agent full
			// of adoptable skills got nothing (round 18 finding I12).
			rootSkillPath := filepath.Join(target, "SKILL.md")
			if _, err := os.Lstat(rootSkillPath); err == nil {
				failedScans[a.Name()] = true
				failed = append(failed, FailedAction{Action{AgentName: a.Name()}, fmt.Errorf(
					"%w: agent %s target %s contains a root SKILL.md; move that skill into a named child directory or install it with `fu add`",
					ErrWholeDirRootSkillUnsupported, a.Name(), target)})
				continue
			} else if !errors.Is(err, fs.ErrNotExist) {
				failedScans[a.Name()] = true
				failed = append(failed, FailedAction{Action{AgentName: a.Name()},
					fmt.Errorf("inspect whole-directory root skill %s: %w", rootSkillPath, err)})
				continue
			}
			infos, err := os.ReadDir(target)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("agent %s: read whole-directory target: %v", a.Name(), err))
				continue
			}
			for _, e := range infos {
				name := e.Name()
				if name == "" || !isSinglePathComponent(name) {
					continue
				}
				if err := skill.ValidateName(name); err != nil {
					continue
				}
				if isReserved(a, name) {
					continue
				}
				entry := adoptEntry{agentName: a.Name(), name: name}
				if e.Type()&os.ModeSymlink != 0 {
					// A symlink inside the target: follow it read-only --
					// unless it is fu's own link into the store, which is a
					// managed entry, never an adoption candidate (round 9
					// finding M1; the per-entry form skips the same shape as
					// KindFuLink).
					raw, err := os.Readlink(filepath.Join(target, name))
					if err != nil {
						continue
					}
					if ownsLink(st.SkillsDir(), name, raw) {
						continue
					}
					resolved, err := filepath.EvalSymlinks(filepath.Join(target, name))
					if err != nil {
						continue
					}
					entry.dir = resolved
					entry.symlinkSrc = resolved
				} else if e.IsDir() {
					entry.dir = filepath.Join(target, name)
					entry.symlinkSrc = entry.dir
				} else {
					continue // plain file: never adoptable
				}
				// Only valid skills are adoption candidates: non-skill
				// entries must survive as passthrough links. A missing
				// SKILL.md means "not a skill" and is silent; anything else
				// is a rule-7 rejection and owes the user a reason
				// (SPEC rule 7, round 18 finding I11).
				meta, err := skill.ParseMeta(entry.dir)
				if err != nil {
					if !errors.Is(err, skill.ErrNoSkillFile) {
						reject(name, err)
					}
					continue
				}
				if verr := skill.Validate(meta, name); verr != nil {
					reject(name, verr)
					continue
				}
				entries = append(entries, entry)
			}
			continue
		}
		for _, e := range state.Entries {
			if e.Kind == KindFuLink {
				continue
			}
			if e.Name == "" || !isSinglePathComponent(e.Name) {
				continue
			}
			if err := skill.ValidateName(e.Name); err != nil {
				continue
			}
			// Rule 11, mirrored from the whole-directory branch: ScanAgent
			// already excludes reserved names from state.Entries, but the
			// per-entry pass must not depend on the inventory's goodwill
			// (round 8 finding I2).
			if isReserved(a, e.Name) {
				continue
			}
			entry := adoptEntry{agentName: a.Name(), name: e.Name}
			if e.LinkTarget != "" {
				target, err := filepath.EvalSymlinks(filepath.Join(a.SkillsDir(), e.Name))
				if err != nil {
					entry.dir = filepath.Join(a.SkillsDir(), e.Name)
				} else {
					entry.dir = target
				}
				entry.symlinkSrc = entry.dir
			} else {
				entry.dir = filepath.Join(a.SkillsDir(), e.Name)
			}
			// Same filter as the whole-directory form (finding I3): a
			// directory that is not a valid skill is left alone instead of
			// aborting the whole run at install time. A missing SKILL.md is
			// silent; a rule-7 violation is reported with its reason
			// (SPEC rule 7, round 18 finding I11).
			meta, err := skill.ParseMeta(entry.dir)
			if err != nil {
				if !errors.Is(err, skill.ErrNoSkillFile) {
					reject(e.Name, err)
				}
				continue
			}
			if verr := skill.Validate(meta, e.Name); verr != nil {
				reject(e.Name, verr)
				continue
			}
			entries = append(entries, entry)
		}
	}
	scanned.entries = entries
	scanned.warnings = warnings
	scanned.failed = failed
	return scanned, nil
}

// wholeDirTarget resolves an agent's symlinked skills directory to its
// target's canonical absolute path.
func wholeDirTarget(a agent.Agent) (string, error) {
	raw, err := os.Readlink(a.SkillsDir())
	if err != nil {
		return "", err
	}
	abs := raw
	if !filepath.IsAbs(raw) {
		abs = filepath.Join(filepath.Dir(a.SkillsDir()), raw)
	}
	return filepath.EvalSymlinks(abs)
}

// isReserved reports whether an agent reserves a name (SPEC rule 11).
func isReserved(a agent.Agent, name string) bool {
	for _, r := range a.Reserved() {
		if r == name {
			return true
		}
	}
	return false
}

// Adopt runs the three-phase adoption (DESIGN §6 AdoptPlan, per-entry form):
// read-only inventory, per-skill install, per-agent switching. scope limits
// the inventory to one agent ("") while the switch-encoding still covers
// every detected agent.
func Adopt(st *store.Store, allAgents []agent.Agent, scope string) (AdoptResult, error) {
	return adopt(st, allAgents, scope, hooks{})
}

// AdoptScoped is the typed application boundary for adoption. The legacy
// Adopt wrapper remains for lower-level callers that already distinguish an
// omitted string before calling the engine.
func AdoptScoped(st *store.Store, allAgents []agent.Agent, scope AdoptScope) (AdoptResult, error) {
	selected, err := scope.agentName()
	if err != nil {
		return AdoptResult{}, err
	}
	return adopt(st, allAgents, selected, hooks{})
}

func (s AdoptScope) agentName() (string, error) {
	switch {
	case s.All && s.Agent == "":
		return "", nil
	case !s.All && s.Agent != "":
		return s.Agent, nil
	case !s.All:
		return "", ErrEmptyAgentScope
	default:
		return "", errors.New("agent scope cannot select all agents and one named agent")
	}
}

func adopt(st *store.Store, allAgents []agent.Agent, scope string, h hooks) (AdoptResult, error) {
	agents := allAgents
	if scope != "" {
		found := false
		for _, a := range allAgents {
			if a.Name() == scope {
				agents = []agent.Agent{a}
				found = true
				break
			}
		}
		if !found {
			if _, known := agent.ByName(scope); known {
				return AdoptResult{}, fmt.Errorf("agent %q is not detected on this machine", scope)
			}
			return AdoptResult{}, fmt.Errorf("%w %q", ErrUnknownAgent, scope)
		}
	}
	prologueResult, err := writeCommandPrologue(st, allAgents)
	if err != nil {
		return AdoptResult{Reconcile: prologueResult}, err
	}
	// The prologue reconcile's findings ride along on every exit, including
	// these two: they come from the mandatory recovery boundary, which is
	// exactly where losing them matters most (round 18 finding M18).
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		return AdoptResult{Reconcile: prologueResult}, err
	}
	scan, err := scanAdoptEntriesWithHooks(st, agents, h)
	if err != nil {
		return AdoptResult{Reconcile: prologueResult}, err
	}
	rawEntries, failedScans := scan.entries, scan.failedScans
	res := AdoptResult{}
	res.Warnings = append(res.Warnings, scan.warnings...)
	// Dedupe across agents by name, merging agents with identical content
	// and refusing names whose content differs.
	type merged struct {
		adoptEntry
		agents      []string
		digest      string
		sourceDirs  map[string]bool
		directEntry bool
	}
	byName := map[string]*merged{}
	conflicted := map[string]bool{}
	skippedSeen := map[string]bool{}
	type digestResult struct {
		digest string
		err    error
	}
	digestsByDir := map[string]digestResult{}
	for _, e := range rawEntries {
		if cfg.HasSkill(e.name) {
			if !skippedSeen[e.name] {
				res.Skipped = append(res.Skipped, e.name)
				skippedSeen[e.name] = true
				// A managed name that some agent still holds as an unmanaged
				// copy can never be adopted again ("already managed" skips
				// it forever, round 8 finding I1): say so and name the way
				// out, instead of leaving the copy orphaned.
				res.Warnings = append(res.Warnings, fmt.Sprintf("skill %s: %s still holds an unadopted copy; run `fu rm %s` then `fu adopt` to take it in", e.name, e.agentName, e.name))
			}
			continue
		}
		if conflicted[e.name] {
			// The name already conflicted in this run: DESIGN §6 requires
			// the complete item to be skipped, so a
			// later agent matching the first digest must not re-create the
			// entry and make "left untouched" a lie.
			continue
		}
		digestKey := filepath.Clean(e.dir)
		cached, ok := digestsByDir[digestKey]
		if !ok {
			cached.digest, cached.err = digestDir(e.dir)
			digestsByDir[digestKey] = cached
		}
		d, err := cached.digest, cached.err
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Action{Skill: e.name}, err})
			continue
		}
		if m, ok := byName[e.name]; ok {
			if m.digest != d {
				if !conflicted[e.name] {
					res.Conflicts = append(res.Conflicts, e.name)
					conflicted[e.name] = true
				}
				delete(byName, e.name)
				continue
			}
			m.agents = append(m.agents, e.agentName)
			if e.symlinkSrc == "" {
				m.directEntry = true
			} else {
				m.sourceDirs[filepath.Clean(e.symlinkSrc)] = true
			}
			continue
		}
		m := &merged{
			adoptEntry: e,
			agents:     []string{e.agentName},
			digest:     d,
			sourceDirs: make(map[string]bool),
		}
		if e.symlinkSrc == "" {
			m.directEntry = true
		} else {
			m.sourceDirs[filepath.Clean(e.symlinkSrc)] = true
		}
		byName[e.name] = m
	}
	// Scan-time refusals are surfaced now that the candidate set is known: a
	// name another agent supplied a valid copy of is being adopted, so
	// reporting it as invalid alongside "adopted" would contradict itself.
	// Agent-level refusals carry no skill name and always surface.
	for _, f := range scan.failed {
		if f.Action.Skill != "" && byName[f.Action.Skill] != nil {
			continue
		}
		res.Failed = append(res.Failed, f)
	}
	// Install each unique candidate in its own transaction. The switching
	// agents are the scanned agents that held the skill; every other
	// detected agent gets an explicit false override so the trailing
	// reconcile never delivers the skill to them. Even when --agent limits
	// the adoption source, all other detected agents receive explicit
	// overrides from the read-only inventory (DESIGN §6).
	//
	// Sorted by name so the "adopted ..." output order is deterministic
	// across runs (round 9 finding M4).
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	warnedLeftUntouched := map[string]bool{}
	completedWholeDirSwitch := map[string]bool{}
	var postCommitErr error
	perSkillReconciled := false
	for _, name := range names {
		m := byName[name]
		m.symlinkSrc = ""
		m.sourceAmbiguous = len(m.sourceDirs) > 1 || (m.directEntry && len(m.sourceDirs) != 0)
		if !m.directEntry && len(m.sourceDirs) == 1 {
			for sourceDir := range m.sourceDirs {
				m.symlinkSrc = sourceDir
			}
		}
		switched := map[string]bool{}
		var switchAgents, switchWhole []agent.Agent
		for _, a := range agents {
			// An agent the scan already refused is out of this run entirely.
			// failedScans used to gate only the warning below, so a refused
			// agent was re-admitted here by hasEntry -- which follows the
			// parent symlink, so an entry inside a rooted target matches --
			// and the refusal fired again at capture time, where it aborted
			// the whole command instead of isolating one agent. DESIGN §6
			// requires preserving the refused agent's link and target while
			// allowing independent skills to continue.
			if failedScans[a.Name()] {
				continue
			}
			if !hasEntry(a, m.name) {
				continue
			}
			state, ok := scan.states[a.Name()]
			if !ok {
				if !warnedLeftUntouched[a.Name()] {
					warnedLeftUntouched[a.Name()] = true
					res.Warnings = append(res.Warnings, fmt.Sprintf("agent %s: initial skills inventory is unavailable; %s left untouched", a.Name(), m.name))
				}
				continue
			}
			switchAgents = append(switchAgents, a)
			switched[a.Name()] = true
			if state.ParentIsSymlink && !completedWholeDirSwitch[a.Name()] {
				switchWhole = append(switchWhole, a)
			}
		}
		summary, reconcileRes, err := adoptOne(st, allAgents, m.adoptEntry, m.digest, switchAgents, switchWhole, switched, h)
		mergeResult(&res.Reconcile, reconcileRes)
		if summary != nil {
			if summary.Operation.PostCommitComplete {
				switchedOK := make(map[string]bool, len(summary.Agents))
				for _, agentName := range summary.Agents {
					switchedOK[agentName] = true
				}
				for _, a := range switchWhole {
					if switchedOK[a.Name()] {
						// The cached inventory describes the command's initial
						// state. Advance only changes Fu itself completed; later
						// capture still verifies the live directory identities.
						completedWholeDirSwitch[a.Name()] = true
					}
				}
			}
			perSkillReconciled = perSkillReconciled || summary.Operation.ReconcileComplete
			// Agent-side switching is adopt (SPEC §5.1 requires replacing the
			// original entry with a store link). A summary naming no agent means every holding
			// agent was isolated, so the destructive half never happened --
			// reporting it as adopted told the user otherwise, and rendered
			// as `adopted <name> (from )` with an empty parenthetical at exit
			// 0 (round 18 finding C2). Pending already means exactly this
			// state: committed to the store, agent switching incomplete.
			switched := summary.Operation.PostCommitComplete && len(summary.Agents) > 0
			if summary.Operation.PostCommitComplete && len(summary.Agents) == 0 {
				// "any copy still in place", not "each remaining copy is
				// reported as a conflict": on the route where the agent's entry
				// vanished between scanAdoptEntries and hasEntry, switchAgents
				// is nil, nothing is left at the name, and no conflict is
				// produced -- so the stronger promise was simply false there.
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"skill %s: installed into the store, but no agent entry could be switched; any copy still in place is reported below", m.name))
			}
			if switched {
				res.Adopted = append(res.Adopted, *summary)
			} else {
				res.Pending = append(res.Pending, *summary)
			}
			res.Warnings = append(res.Warnings, summary.Warnings...)
		}
		if err != nil {
			// ErrWholeDirRootSkillUnsupported is deliberately not special-cased
			// here. The scan-time detection above already excludes such an
			// agent, so reaching this point means the root SKILL.md appeared
			// between the scan and the capture -- a race, at a point where
			// nothing has been committed. Aborting the run for it contradicted
			// the same two DESIGN §6 rules the scan-time path was fixed to
			// honour; it falls through to the per-candidate isolation below.
			// A conflict raised before the transaction record exists isolates
			// the candidate instead of aborting: see errAdoptPreflight.
			if errors.Is(err, ErrTxnConflict) && !errors.Is(err, errAdoptPreflight) {
				if !perSkillReconciled {
					mergeResult(&res.Reconcile, prologueResult)
				}
				return res, err
			}
			if errors.Is(err, errAdoptPreflight) {
				failed := FailedAction{Action{Skill: m.name}, err}
				if errors.Is(err, ErrTxnConflict) {
					res.PreflightConflicts = append(res.PreflightConflicts, failed)
				} else {
					res.Failed = append(res.Failed, failed)
				}
				continue
			}
			if errors.Is(err, ErrOperationFailed) {
				// Agent-side reconcile failures keep their exit-1 semantics
				// and are surfaced through Reconcile/printResult; they are
				// not per-skill install failures. They also do not prevent an
				// independent candidate from being installed.
				postCommitErr = errors.Join(postCommitErr, err)
				continue
			}
			if errors.Is(err, errInstalledNotSwitched) {
				// The skill was committed and registered; only the
				// agent-side switching failed (round 13 finding I4). The
				// transaction stays open, so the next write command's
				// recovery continues the switching. Report the state as a
				// warning with exit-1 semantics -- never as the
				// install-class "invalid", which reads like the skill was
				// not installed.
				res.Warnings = append(res.Warnings, fmt.Sprintf("skill %s was installed but its agents could not be switched; a later write command will finish it (%v)", m.name, err))
				postCommitErr = errors.Join(postCommitErr, err)
				continue
			}
			if summary != nil && summary.Operation.Committed {
				if summary.Operation.RecoveryPending {
					res.Warnings = append(res.Warnings, fmt.Sprintf("skill %s was committed but post-commit recovery is pending; a later write command will finish it (%v)", m.name, err))
				} else {
					res.Warnings = append(res.Warnings, fmt.Sprintf("skill %s was committed but a later verification phase failed (%v)", m.name, err))
				}
				postCommitErr = errors.Join(postCommitErr, err)
				continue
			}
			// Per-skill isolation covers the install stage too (round 11
			// finding I1, DESIGN §6 phase 3 step 7): an install failure of
			// one candidate -- an escape symlink refused by ValidateLinks,
			// content changed between inventory and install -- must not
			// abort the remaining candidates. The failed candidate's own
			// transaction was rolled back; report it and continue. The CLI
			// prints these as "invalid:".
			res.Failed = append(res.Failed, FailedAction{Action{Skill: m.name}, err})
			continue
		}
	}
	// The prologue describes the state before adoption. Once a constituent
	// operation has completed reconciliation, that later pass is authoritative;
	// carrying the prologue as well would report conditions the adoption fixed.
	// When discovery yields no operation (or every operation stops before its
	// trailing reconcile), the prologue remains the only reconciliation result.
	if !perSkillReconciled {
		mergeResult(&res.Reconcile, prologueResult)
	}
	if postCommitErr != nil {
		return res, errors.Join(ErrOperationFailed, postCommitErr)
	}
	if len(res.Reconcile.Failed) != 0 {
		return res, ErrOperationFailed
	}
	return res, nil
}

// adoptOne installs one candidate and switches every scanned agent that held
// it. switchedWhole names the subset whose skills directory is itself a
// symlink (whole-directory form, DESIGN §6 phase 2).
func adoptOne(st *store.Store, allAgents []agent.Agent, e adoptEntry, digest string, switched, switchedWhole []agent.Agent, held map[string]bool, h hooks) (*AdoptSummary, Result, error) {
	summary := &AdoptSummary{Name: e.name}
	for _, a := range switched {
		summary.Agents = append(summary.Agents, a.Name())
	}
	// Source record: a symlink with a single consistent target is a local
	// source; anything else has no upstream.
	var fields map[string]string
	if e.sourceAmbiguous {
		summary.Warnings = append(summary.Warnings,
			fmt.Sprintf("skill %s: symlink targets differ across agents; local source not recorded", e.name))
	} else if e.symlinkSrc != "" {
		consistent := true
		for _, a := range switched {
			target, err := filepath.EvalSymlinks(filepath.Join(a.SkillsDir(), e.name))
			if err != nil || filepath.Clean(target) != filepath.Clean(e.symlinkSrc) {
				consistent = false
				break
			}
		}
		if consistent {
			fields = map[string]string{"type": "local", "path": e.symlinkSrc}
		} else {
			summary.Warnings = append(summary.Warnings,
				fmt.Sprintf("skill %s: symlink targets differ across agents; local source not recorded", e.name))
		}
	}
	// Overrides: an agent that did not have this skill before the adopt stays
	// off, so the trailing reconcile never delivers it somewhere it was never
	// in effect.
	//
	// "Did not have it" is a question about the filesystem, not about scope.
	// held covers only the scanned agents, so with `--agent X` every other
	// detected agent used to be written off unconditionally -- including one
	// that holds its own copy and loads the skill every session, which made
	// `fu list` report it off while it was demonstrably on (round 18 finding
	// I10). SPEC §5.1 requires preserving the pre-adopt switch state, and
	// DESIGN §6 spells out that a scoped run still read-only inventories the
	// other detected agents. hasEntry is that inventory: read-only, and the
	// only thing an out-of-scope agent's state is allowed to decide.
	//
	// Leaving such an agent unoverridden does not risk an unwanted delivery:
	// reconcile finds unmanaged content at the name and reports a conflict.
	overrides := map[string]bool{}
	for _, a := range allAgents {
		if held[a.Name()] || hasEntry(a, e.name) {
			continue
		}
		overrides[a.Name()] = false
	}
	txn := &TxnRecord{
		Op:             "adopt",
		Name:           e.name,
		SourceFields:   fields,
		Agents:         summary.Agents,
		WholeDirAgents: wholeDirNames(switchedWhole),
		Overrides:      overrides,
		Targets: []string{
			filepath.Join("staging", e.name),
			filepath.Join("store", "skills", e.name),
		},
	}
	srcRoot, err := os.OpenRoot(e.dir)
	if err != nil {
		return nil, Result{}, fmt.Errorf("open adopted source %s: %w", e.dir, err)
	}
	defer srcRoot.Close()
	operationOutcome := OperationOutcome{Name: e.name}
	var isolatedEntries []adoptIsolation
	opRes, err := run(st, allAgents, Op{
		Message:        "adopt: " + e.name,
		Txn:            txn,
		outcome:        &operationOutcome,
		AllowedChanges: []string{"fu.yaml", filepath.ToSlash(filepath.Join("skills", e.name))},
		ValidatePrepared: func(st *store.Store, prepared store.PreparedCommit) error {
			if txn.Payload == nil {
				return errors.New("adopt transaction has no payload manifest at commit preparation")
			}
			if err := st.ValidateSkillOwned(e.name, *txn.Payload); err != nil {
				return fmt.Errorf("validate published skill before commit: %w", err)
			}
			return st.ValidatePreparedOwnedTree(prepared, filepath.ToSlash(filepath.Join("skills", e.name)), *txn.Payload)
		},
		Preflight: func(st *store.Store, cfg *store.Config) error {
			if err := checkAddAvailable(st, cfg, e.name); err != nil {
				return err
			}
			if err := h.fire(h.beforeAdoptTargetCapture); err != nil {
				return err
			}
			targets, err := captureAdoptTargetsWithHooks(switched, switchedWhole, e.name, digest, h.beforeAdoptSourcePair, h.afterAdoptLinkRead)
			if err != nil {
				return fmt.Errorf("%w: %w", errAdoptPreflight, err)
			}
			txn.AdoptTargets = targets
			return nil
		},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			if err := checkAddAvailable(st, cfg, e.name); err != nil {
				return err
			}
			proj, err := skill.ProjectDir(srcRoot.FS(), ".")
			if err != nil {
				return fmt.Errorf("project adopted source %s: %w", e.dir, err)
			}
			declared := declaredFromProjection(proj)
			txn.Declared = declared
			rootPayload, err := createTxnStagedRoot(st, txn, e.name, 0o755, h)
			if err != nil {
				return err
			}
			if err := h.fire(h.afterDeclaredTxn); err != nil {
				return err
			}
			payload, err := st.CopyStagedTreeOwned(e.name, rootPayload, srcRoot, ".", declared)
			if err != nil {
				return fmt.Errorf("copy adopted source into staging: %w", err)
			}
			if err := skill.ValidateLinks(manifestEntries(payload)); err != nil {
				return fmt.Errorf("path-safety check: %w", err)
			}
			if err := h.fire(h.afterCopy); err != nil {
				return err
			}
			txn.Payload = &payload
			txn.Declared = nil
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record adopted skill: %w", err)
			}
			if err := st.ValidateStagedOwned(e.name, payload); err != nil {
				return fmt.Errorf("validate exact staged skill: %w", err)
			}
			stagedRoot, err := st.StagingRoot()
			if err != nil {
				return err
			}
			if err := skill.ValidateSkillDir(stagedRoot.FS(), e.name); err != nil {
				return fmt.Errorf("validate staged skill: %w", err)
			}
			d, err := digestOwnedPayload(payload)
			if err != nil {
				return fmt.Errorf("digest staged skill ownership: %w", err)
			}
			if d != digest {
				return fmt.Errorf("adopted content changed between inventory and install")
			}
			txn.Digest = d
			txn.Stage = "prepared"
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record prepared adopt: %w", err)
			}
			if err := cfg.AddSkill(e.name, d); err != nil {
				return err
			}
			if len(fields) > 0 {
				cfg.SetSourceFields(e.name, fields)
			}
			for _, agentName := range sortedNames(overrides) {
				cfg.SetAgent(e.name, agentName, overrides[agentName])
			}
			return nil
		},
		Publish: func(st *store.Store) error {
			if txn.Payload == nil {
				return errors.New("adopt transaction has no staged ownership manifest")
			}
			return st.PublishStagedOwned(e.name, *txn.Payload)
		},
		PostCommit: func(st *store.Store) error {
			// Whole-directory agents are switched by the directory swap, not
			// per entry: exclude them from the per-entry pass so their
			// original entries are never individually archived.
			var perEntry []agent.Agent
			for _, a := range switched {
				whole := false
				for _, w := range switchedWhole {
					if w.Name() == a.Name() {
						whole = true
						break
					}
				}
				if !whole {
					perEntry = append(perEntry, a)
				}
			}
			isolated, err := switchAdoptedEntriesReporting(st, perEntry, e.name, txn, h)
			isolatedEntries = append(isolatedEntries, isolated...)
			if err != nil {
				// The skill was committed and registered; only the agent-side
				// switching failed (round 13 finding I4, refined in round 14
				// finding M1): the transaction stays open, so the next write
				// command's recovery finishes the switching. The wrap lives
				// in the PostCommit closure -- not at the adoptOne level --
				// so later pipeline failures (ClearTxn, CheckCanonicalPath),
				// where the switching already completed, are not
				// misclassified.
				return fmt.Errorf("%w: %w", errInstalledNotSwitched, err)
			}
			isolated, err = switchWholeDirAgentsReporting(st, switchedWhole, e.name, txn, h)
			isolatedEntries = append(isolatedEntries, isolated...)
			if err != nil {
				return fmt.Errorf("%w: %w", errInstalledNotSwitched, err)
			}
			return nil
		},
	}, h)
	if err != nil && !operationOutcome.Committed {
		return nil, opRes, err
	}
	for _, isolated := range isolatedEntries {
		summary.Warnings = append(summary.Warnings, adoptIsolationWarning(e.name, isolated))
	}
	// Correct the summary from the isolation-filtered transaction record:
	// switchAdoptedEntriesWithHooks durably removes failed per-entry agents
	// from txn.Agents, and switchWholeDirAgentsWithHook removes failed
	// whole-directory agents from txn.WholeDirAgents. The summary must
	// never name an agent whose switch failed (round 6 finding M4).
	wholeDir := map[string]bool{}
	for _, a := range switchedWhole {
		wholeDir[a.Name()] = true
	}
	switchedWholeOK := map[string]bool{}
	for _, n := range txn.WholeDirAgents {
		switchedWholeOK[n] = true
	}
	summary.Agents = filteredAdoptSummaryAgents(txn.Agents, wholeDir, switchedWholeOK)
	summary.Operation = operationOutcome
	return summary, opRes, err
}

func adoptIsolationWarning(name string, isolated adoptIsolation) string {
	return fmt.Sprintf(
		"agent %s could not switch skill %s: %v; repair or reduce that agent entry, then run `fu rm %s` followed by `fu adopt`",
		isolated.agent, name, isolated.err, name)
}

func filteredAdoptSummaryAgents(txnAgents []string, wholeDir, switchedWholeOK map[string]bool) []string {
	agents := make([]string, 0, len(txnAgents))
	for _, name := range txnAgents {
		if wholeDir[name] && !switchedWholeOK[name] {
			continue
		}
		agents = append(agents, name)
	}
	return agents
}

// adoptTargets records the absolute agent locations selected during the
// inventory. Recovery validates these paths before looking up an adapter by
// name, preventing a later HOME from redirecting a committed switch.

// hasEntry reports whether an agent's skills directory currently holds a
// foreign or fu-owned entry at name.
func hasEntry(a agent.Agent, name string) bool {
	_, err := os.Lstat(filepath.Join(a.SkillsDir(), name))
	return err == nil
}

// digestDir computes the canonical content digest of a directory.
func digestDir(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	entries, err := skill.ProjectDir(root.FS(), ".")
	if err != nil {
		return "", err
	}
	return skill.DigestManifest(entries)
}

// wholeDirNames extracts the agent names of a whole-directory switch list.
func wholeDirNames(agents []agent.Agent) []string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name())
	}
	return names
}

// sortedNames returns the keys of an override map in sorted order. Both the
// operation and its recovery reconstruction write per-agent overrides by
// iterating this map, and Go randomizes map iteration order per map
// instance -- the transaction record JSON-round-trips the map, so the two
// sides would otherwise emit unrelated orders and the committed-state byte
// comparison could never match (finding C1).
func sortedNames(m map[string]bool) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
