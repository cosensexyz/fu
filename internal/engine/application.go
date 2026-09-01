package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

// Application is the reusable product boundary shared by command-line and
// future graphical interfaces. It owns store discovery, agent detection,
// business operations, and read-model construction.
type Application struct {
	hooks hooks
}

// NewApplication returns the production application service.
func NewApplication() *Application { return &Application{} }

// newApplication enables deterministic durable-boundary failures in engine
// tests while production always uses a zero hook set.
func newApplication(h hooks) *Application { return &Application{hooks: h} }

type InitOutcome struct {
	Home string
}

type InvalidConfigName struct {
	Name   string
	Reason string
}

type ReadDiagnostics struct {
	ConfigPath    string
	VersionTooNew bool
	InvalidNames  []InvalidConfigName
}

type AgentSwitch struct {
	Name     string
	Enabled  bool
	Override bool
}

type ListedSkill struct {
	Name   string
	Global bool
	Agents []AgentSwitch
}

type ListOutcome struct {
	Agents      []string
	Skills      []ListedSkill
	Diagnostics ReadDiagnostics
}

type ShowOutcome struct {
	Name            string
	Description     string
	MetadataError   error
	MetadataWarning error
	Digest          string
	Source          map[string]string
	Global          bool
	Agents          []AgentSwitch
	Diagnostics     ReadDiagnostics
}

type ToggleOutcome struct {
	Operation       OperationOutcome
	TargetAgents    []string
	DeliveryBlocked bool
}

func (a *Application) home() (string, error) {
	return store.Home()
}

func (a *Application) openStore() (*store.Store, error) {
	home, err := a.home()
	if err != nil {
		return nil, err
	}
	return store.Open(home)
}

func (a *Application) detectedAgents() []agent.Agent {
	return agent.Detected()
}

func (a *Application) Initialize() (InitOutcome, error) {
	home, err := a.home()
	if err != nil {
		return InitOutcome{}, err
	}
	if _, err := store.Init(home); err != nil {
		return InitOutcome{Home: home}, err
	}
	return InitOutcome{Home: home}, nil
}

func (a *Application) PruneRecovery() (PruneOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return PruneOutcome{}, err
	}
	return PruneCompletedTransactions(st)
}

// readDiagnostics collects the config-level findings every read command
// carries. agents is what a caller passes when its command also reports
// per-agent findings, and nil when it does not.
//
// The suppression matters only for the first kind. A name an agent reserves is
// reported by that agent's own ReportReserved finding in the terms the user
// needs -- "reserved name, never linked codex/.system" -- and repeating it here
// as "invalid: skill name \".system\" fails validation" describes one fact
// twice in two vocabularies, on two different streams. Write commands already
// suppressed it through alreadyReportedAsReserved (reconcile.go); this path did
// not, because before `fu status` no read command produced both channels at
// once.
func readDiagnostics(st *store.Store, cfg *store.Config, agents []agent.Agent) ReadDiagnostics {
	diagnostics := ReadDiagnostics{
		ConfigPath:    st.ConfigPath(),
		VersionTooNew: cfg.VersionTooNew(),
	}
	for _, invalid := range cfg.InvalidNames() {
		if alreadyReportedAsReserved(agents, invalid) {
			continue
		}
		diagnostics.InvalidNames = append(diagnostics.InvalidNames, InvalidConfigName{
			Name: invalid.Name, Reason: invalid.Reason,
		})
	}
	return diagnostics
}

// unknownSkillError answers a name the config does not hold, distinguishing a
// name that is simply absent from one LoadConfig excluded for failing
// validation. The second is not "unknown": it is present in fu.yaml, spelled
// in a way fu refuses to manage, and unreachable until the file is edited --
// so the answer has to name the file and say what is wrong with it, or the
// user is left looking for a skill the config plainly shows.
//
// Shared rather than restated (round 4). `fu show` and `fu rm` each carried
// their own copy of this sentence and `fu update` carried neither, answering
// `unknown skill %q` for a name it could see -- the only command in the family
// that did.
func unknownSkillError(st *store.Store, cfg *store.Config, name string) error {
	for _, invalid := range cfg.InvalidNames() {
		if invalid.Name == name {
			return fmt.Errorf("skill name %q fails validation (%s) and is ignored; edit %s to fix or remove it",
				invalid.Name, invalid.Reason, st.ConfigPath())
		}
	}
	return fmt.Errorf("unknown skill %q", name)
}

func (a *Application) readStore() (*store.Store, *store.Config, error) {
	st, err := a.openStore()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		return nil, nil, err
	}
	return st, cfg, nil
}

// StatusOutcome pairs the report with the diagnostics every read command
// carries, so the CLI prints both from one call.
type StatusOutcome struct {
	Report      StatusReport
	Diagnostics ReadDiagnostics
}

// OutdatedOutcome carries the per-skill update judgements of one read-only run,
// plus the config-level diagnostics every read command owes its caller.
//
// Round 2, Important #6: this type had only Rows, so `fu outdated` was the one
// read command that printed neither channel. A skill whose name fails
// validation is excluded from the config's skill set altogether
// (LoadConfig/InvalidNames), so it was silently absent from the report with no
// line saying why -- while `fu list` and `fu status` both named it -- and a
// fu.yaml written by a newer fu produced a full, confident report with no
// warning that the build may not understand it.
type OutdatedOutcome struct {
	Rows        []UpdateStatus
	Diagnostics ReadDiagnostics
}

// Status assembles the read-only consistency report. Like ListSkills it takes
// no lock and writes nothing.
//
// Whatever Status assembled before it failed is returned with the error, never
// instead of it: Status reads the store-side facts after the agents precisely
// so one damaged journal family costs the user that section rather than the
// whole report, and dropping the partial report here would undo that one step
// later. `fu gc` isolates the same damage per family and still reports what it
// did. The only failure that yields nothing is one that leaves no report to
// return -- a store that cannot be opened or a config that cannot be read.
func (a *Application) Status() (StatusOutcome, error) {
	st, cfg, err := a.readStore()
	if err != nil {
		return StatusOutcome{}, err
	}
	agents := a.detectedAgents()
	report, statusErr := Status(st, cfg, agents)
	return StatusOutcome{Report: report, Diagnostics: readDiagnostics(st, cfg, inspectedAgents(agents, report))}, statusErr
}

// Outdated judges every registered skill against its recorded source. It is a
// read-only command: it takes no lock and writes nothing (SPEC §9).
func (a *Application) Outdated() (OutdatedOutcome, error) {
	st, cfg, err := a.readStore()
	if err != nil {
		return OutdatedOutcome{}, err
	}
	// nil agents, like ListSkills and ShowSkill: the agent-level suppression in
	// readDiagnostics exists for commands that also report per-agent findings,
	// and this one reports none.
	diagnostics := readDiagnostics(st, cfg, nil)
	// Whatever was judged is returned with the error, never instead of it --
	// Status's own rule thirty lines above, and for the same reason. Outdated
	// degrades a single unreachable source into that row's Reason and returns
	// no error at all, so the one error that reaches here after rows exist is
	// the session close, which happens once every row has already been
	// judged: dropping the report for it would throw away a complete answer
	// over a failure to release descriptors.
	//
	// The analogy stops at this boundary, though: `fu status`'s command prints
	// every section before returning the error, while newOutdatedCmd returns on
	// it before printing a row (cli/outdated.go), so today the complete answer
	// reaches a caller that asks for it rather than a terminal. The rule is
	// still the engine's to keep -- a second front end (SPEC §5.2) gets the
	// choice this layer would otherwise have made for it. The diagnostics come back either
	// way, since a config fu cannot fully understand is often the reason the
	// judgement failed at all.
	rows, err := Outdated(st, cfg)
	return OutdatedOutcome{Rows: rows, Diagnostics: diagnostics}, err
}

// inspectedAgents drops the agents Status could not scan, so the suppression
// in readDiagnostics is asked about agents that actually produced findings.
//
// alreadyReportedAsReserved interrogates the agent *list*: some agent reserves
// the name, therefore some agent's ReportReserved explains it. That inference
// is exactly one step too long here. Status returns before Desired runs for an
// agent whose ScanAgent failed (status.go), so no ReportReserved exists for it,
// and standing the config-level `invalid:` line down on its behalf left a
// reserved-and-invalid name reported on neither stream -- `fu status` exiting 0
// having said nothing, while `fu list` still printed the line and `fu enable`
// still failed on it.
//
// Filtering the input rather than teaching the predicate about scan failures
// keeps the write path's own caller (configInvalidNames, reconcile.go)
// untouched: there a scan failure lands in Result.Failed and the command exits
// 1, so nothing is silently withheld.
func inspectedAgents(agents []agent.Agent, report StatusReport) []agent.Agent {
	failed := make(map[string]bool, len(report.Agents))
	for _, status := range report.Agents {
		if status.ScanErr != "" {
			failed[status.Name] = true
		}
	}
	if len(failed) == 0 {
		return agents
	}
	kept := make([]agent.Agent, 0, len(agents))
	for _, detected := range agents {
		if !failed[detected.Name()] {
			kept = append(kept, detected)
		}
	}
	return kept
}

func (a *Application) ListSkills() (ListOutcome, error) {
	st, cfg, err := a.readStore()
	if err != nil {
		return ListOutcome{}, err
	}
	agents := a.detectedAgents()
	// nil agents: `fu list` prints no per-agent reserved finding, so this
	// diagnostic is the only channel a reserved-and-invalid name has here.
	outcome := ListOutcome{Diagnostics: readDiagnostics(st, cfg, nil)}
	for _, detected := range agents {
		outcome.Agents = append(outcome.Agents, detected.Name())
	}
	for _, name := range cfg.SkillNames() {
		listed := ListedSkill{Name: name, Global: cfg.Enabled(name)}
		for _, detected := range agents {
			_, override := cfg.Override(name, detected.Name())
			listed.Agents = append(listed.Agents, AgentSwitch{
				Name: detected.Name(), Enabled: cfg.Effective(name, detected.Name()), Override: override,
			})
		}
		outcome.Skills = append(outcome.Skills, listed)
	}
	return outcome, nil
}

func (a *Application) ShowSkill(name string) (ShowOutcome, error) {
	st, cfg, err := a.readStore()
	if err != nil {
		return ShowOutcome{}, err
	}
	// nil agents for the same reason as ListSkills, and one more: the
	// unknown-skill error below reads the name back out of these diagnostics.
	outcome := ShowOutcome{Name: name, Diagnostics: readDiagnostics(st, cfg, nil)}
	if !cfg.HasSkill(name) {
		return outcome, unknownSkillError(st, cfg, name)
	}
	meta, metaErr := skill.ParseMeta(filepath.Join(st.SkillsDir(), name))
	if metaErr != nil {
		outcome.MetadataError = metaErr
	} else {
		outcome.Description = meta.Description
		outcome.MetadataWarning = skill.Validate(meta, name)
	}
	outcome.Digest = cfg.Digest(name)
	outcome.Source = cfg.SourceFields(name)
	outcome.Global = cfg.Enabled(name)
	for _, detected := range a.detectedAgents() {
		_, override := cfg.Override(name, detected.Name())
		outcome.Agents = append(outcome.Agents, AgentSwitch{
			Name: detected.Name(), Enabled: cfg.Effective(name, detected.Name()), Override: override,
		})
	}
	return outcome, nil
}

func (a *Application) NewSkill(name string) (OperationOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return OperationOutcome{Name: name}, err
	}
	outcome := OperationOutcome{Name: name}
	_, err = newSkillTracked(st, a.detectedAgents(), name, a.hooks, &outcome)
	return outcome, err
}

func (a *Application) SetGlobal(name string, on bool) (ToggleOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return ToggleOutcome{Operation: OperationOutcome{Name: name}}, err
	}
	agents := a.detectedAgents()
	outcome := ToggleOutcome{Operation: OperationOutcome{Name: name}}
	for _, detected := range agents {
		outcome.TargetAgents = append(outcome.TargetAgents, detected.Name())
	}
	_, err = setGlobalTracked(st, agents, name, on, a.hooks, &outcome.Operation)
	outcome.DeliveryBlocked = toggleDeliveryBlocked(outcome.Operation.Reconcile, name, outcome.TargetAgents)
	return outcome, err
}

func (a *Application) SetAgent(name, agentName string, on bool) (ToggleOutcome, error) {
	if _, ok := agent.ByName(agentName); !ok {
		return ToggleOutcome{Operation: OperationOutcome{Name: name}, TargetAgents: []string{agentName}}, fmt.Errorf("%w %q", ErrUnknownAgent, agentName)
	}
	st, err := a.openStore()
	if err != nil {
		return ToggleOutcome{Operation: OperationOutcome{Name: name}, TargetAgents: []string{agentName}}, err
	}
	outcome := ToggleOutcome{
		Operation: OperationOutcome{Name: name}, TargetAgents: []string{agentName},
	}
	_, err = setAgentSwitchTracked(st, a.detectedAgents(), name, agentName, on, a.hooks, &outcome.Operation)
	outcome.DeliveryBlocked = toggleDeliveryBlocked(outcome.Operation.Reconcile, name, outcome.TargetAgents)
	return outcome, err
}

func toggleDeliveryBlocked(result Result, name string, targetAgents []string) bool {
	targeted := make(map[string]bool, len(targetAgents))
	for _, agentName := range targetAgents {
		targeted[agentName] = true
	}
	for _, conflict := range result.Conflicts {
		if conflict.Skill == name && targeted[conflict.AgentName] {
			return true
		}
	}
	for _, foreign := range result.DisabledForeign {
		if foreign.Skill == name && targeted[foreign.AgentName] {
			return true
		}
	}
	for _, missing := range result.Missing {
		if missing.Skill == name && targeted[missing.AgentName] {
			return true
		}
	}
	for _, failed := range result.Failed {
		if targeted[failed.Action.AgentName] && (failed.Action.Skill == "" || failed.Action.Skill == name) {
			return true
		}
	}
	for _, skipped := range result.Skipped {
		if targeted[skipped] {
			return true
		}
	}
	return false
}

func (a *Application) PrepareAdd(arg, ref string) (AddPreparation, error) {
	src, err := parseAddSource(arg, ref)
	if err != nil {
		return AddPreparation{}, err
	}
	st, err := a.openStore()
	if err != nil {
		return AddPreparation{}, err
	}
	// Assigned and checked, never returned straight through. Returning
	// prepareAddSource's nil *AddPlan as AddSession would create a *non-nil*
	// AddSession interface holding a nil pointer, so the idiomatic defensive
	// call -- `if plan != nil { defer plan.Close() }` -- takes the branch and
	// panic. Failures inside prepareAddSource include the write prologue,
	// source preparation, and ScanSource. internal/cli/add.go survives only
	// because it checks err before the defer, and go vet does not catch this.
	// This is the one interface-returning method on the boundary this branch
	// exists to create for a second front end.
	plan, prologue, err := prepareAddSource(st, arg, src, a.detectedAgents(), a.hooks)
	preparation := AddPreparation{Prologue: prologue}
	if err != nil {
		return preparation, err
	}
	preparation.Session = plan
	return preparation, nil
}

func (a *Application) Adopt(scope AdoptScope) (AdoptResult, error) {
	selected, err := scope.agentName()
	if err != nil {
		return AdoptResult{}, err
	}
	if selected != "" {
		if _, known := agent.ByName(selected); !known {
			return AdoptResult{}, fmt.Errorf("%w %q", ErrUnknownAgent, selected)
		}
	}
	st, err := a.openStore()
	if err != nil {
		return AdoptResult{}, err
	}
	return adopt(st, a.detectedAgents(), selected, a.hooks)
}

func (a *Application) RemoveSkill(name string) (RemoveOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return RemoveOutcome{Name: name, Operation: OperationOutcome{Name: name}}, err
	}
	return removeSkill(st, a.detectedAgents(), name, a.hooks)
}

// Restore repairs the link layer and, when hard is set, discards uncommitted
// content in the store's own worktree instead of merely reporting it; see
// engine.Restore's doc comment for exactly what hard does and does not touch.
func (a *Application) Restore(hard bool) (RestoreOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return RestoreOutcome{}, err
	}
	return Restore(st, a.detectedAgents(), hard)
}

// Revert rolls the store back n operations; see RevertOperations' doc comment
// for why it sweeps pending hand edits into history first rather than
// refusing the way `git revert` does.
func (a *Application) Revert(n int) (RevertOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return RevertOutcome{}, err
	}
	return RevertOperations(st, a.detectedAgents(), n)
}

// UpdateSkip names one skill a batch update declined to touch, and why. The
// only Reason a batch itself produces today is "locally modified" (SPEC rule
// 3); it is a struct rather than a bare name so the CLI never has to
// re-derive that explanation.
type UpdateSkip struct {
	Name   string
	Reason string
}

// UpdateBatchOutcome reports one `fu update` run: which skills' published
// content moved (Updated), which only had their source lock advance because
// the content already matched (LockOnly; see UpdateOutcome.LockOnly), which
// were declined and why (Skipped), which a batch abort never reached
// (Unattempted), the per-skill operation phases (Operations, for
// printDurableOutcome), and the accumulated reconcile findings across every
// attempted target (Reconcile, for printResult) -- including any per-skill
// failure, which lands in Reconcile.Failed exactly as addSkillsDetailed's
// own res.Failed does (add.go), rather than in a separate field. Shaped
// after AddOutcome (add_command.go), the sibling batch outcome every other
// write command already reports through the same printResult /
// printDurableOutcome channel (add.go, adopt.go, new.go, rm.go, restore.go,
// revert.go, toggle.go) -- update was the only one that did not.
type UpdateBatchOutcome struct {
	Updated  []string
	LockOnly []string
	Skipped  []UpdateSkip
	// Unjudged holds the rows the batch could not judge at all -- no source
	// record, a fixed lock, an unreachable remote, a vanished local path -- each
	// with Outdated's own Reason.
	//
	// Round 3: these rows used to be dropped silently, so a user offline ran `fu
	// update`, was told "nothing to update", and reasonably concluded the store
	// was current. Design §3.3's per-item degradation is written for `outdated`,
	// which prints the reason; the batch degraded the same rows into silence, on
	// the one command that acts on the difference. Kept apart from Skipped
	// because the remedies differ: a skipped skill is offered --force, and
	// --force does nothing for a row nothing could judge.
	Unjudged    []UpdateSkip
	Unattempted []string
	Operations  []OperationOutcome
	Reconcile   Result
	// Diagnostics is the config-level channel every other command already
	// carries (ReadDiagnostics). Round 4: update was the one command that
	// printed neither half of it. When at least one transaction runs, the
	// `invalid:` line arrives through Reconcile and a too-new version is
	// refused inside run -- but a store with nothing to update returns before
	// any run(), so `fu update` printed "nothing to update" and exited 0 over
	// a fu.yaml this build may not understand and over names it had silently
	// excluded, while `fu outdated` on the same store warned about both.
	//
	// Populated as soon as the config is readable, so every later return --
	// including a failure -- carries it.
	Diagnostics ReadDiagnostics
}

// ErrUpdateForceNeedsName marks --force used without naming a skill: a batch
// force would overwrite several skills' local modifications with nothing on
// the command line saying which. internal/cli/update.go already refuses
// this the same way before ever calling UpdateSkills, but Application is
// the shared boundary a future front end (a GUI, say) calls directly --
// PrepareAdd's own ErrInvalidAddRef is the precedent for a refusal that has
// to hold at this layer too, not only in one caller's own guard.
var ErrUpdateForceNeedsName = errors.New(
	"--force needs a skill name: a batch update would overwrite several skills' " +
		"local modifications without naming any of them")

// UpdateSkills runs `fu update` (SPEC scenario 3): named, refreshing exactly
// one skill, or batch, refreshing every skill Outdated reports comparable and
// updatable.
//
// The judgement -- which performs every network read (ls-remote) -- and
// source preparation -- which clones -- both run before any skill's own
// transaction, so the write lock (taken per transaction inside run,
// pipeline.go) is never held during network I/O, exactly as `fu add`
// arranges it (prepareAddSource, add_command.go). A named update judges that
// skill alone (outdatedFor), so the network reads it performs are its own.
// Distinct sources are prepared once each and shared by every skill they
// serve (prepareUpdateSources), since several skills commonly come from one
// repository.
//
// A named skill is attempted even when Outdated found nothing new upstream:
// updateSkill itself then settles on whichever shape applies (typically
// lock-only, a harmless no-op), so only two conditions refuse the request
// outright, both with Outdated's own verdict rather than a generic failure --
// "not comparable" (no source record, a fixed tag/commit lock, a missing
// local path) and "locally modified" without --force. The batch instead
// follows Outdated's own Updatable verdict and sorts a locally modified
// skill into Skipped rather than failing the run, naming the per-skill
// --force escape (SPEC rule 3).
func (a *Application) UpdateSkills(name string, force bool) (outcome UpdateBatchOutcome, err error) {
	if name == "" && force {
		return outcome, ErrUpdateForceNeedsName
	}
	st, cfg, err := a.readStore()
	if err != nil {
		return outcome, err
	}
	// nil agents, like Outdated's own call: this command reports no per-agent
	// findings of its own, so the agent-level suppression in readDiagnostics
	// has nothing to suppress on its behalf.
	outcome.Diagnostics = readDiagnostics(st, cfg, nil)
	// The mandatory recovery boundary, and it has to be here -- before the
	// judgement, not merely before the first transaction. run takes it per
	// transaction (pipeline.go), which a batch that finds zero targets or a
	// named skill this command refuses never reaches, so update was the one
	// discovery-first write command that could complete without recovering,
	// sweeping or taking the lock at all. Its two siblings call this for the
	// same reason (prepareAddSource in add_command.go, adopt.go), and the
	// helper's own doc says it is intentionally complete even when discovery
	// later yields zero operations.
	//
	// Before the judgement because an unhealed interruption also corrupts the
	// verdict: an update killed after its config save but before its exchange
	// has already moved the baseline to the new digest, so Outdated compares the
	// source against itself and reports the skill unchanged but locally
	// modified. Judging first would leave `fu update` saying "nothing to update"
	// -- or offering --force over a hand edit that never happened -- while the
	// interruption sat unhealed, breaking DESIGN §2's promise that the next
	// ordinary write command settles it.
	//
	// The lock this takes is released before prepareUpdateSources clones
	// anything, exactly as `fu add` arranges it, so design §4.4's rule that no
	// network I/O happens while the lock is held still holds.
	prologue, err := writeCommandPrologue(st, a.detectedAgents())
	mergeResult(&outcome.Reconcile, prologue)
	if err != nil {
		return outcome, err
	}
	// Recovery, the sweep and reconcile can all have rewritten fu.yaml, so the
	// judgement below has to read the settled state rather than the one this
	// command opened with -- which is the whole point of recovering first. The
	// diagnostics are re-derived from it for the same reason; readDiagnostics is
	// a pure read of an already-loaded config, so this costs nothing.
	cfg, err = store.LoadConfig(st.ConfigPath())
	if err != nil {
		return outcome, err
	}
	outcome.Diagnostics = readDiagnostics(st, cfg, nil)
	// Narrowed to the named skill, so naming one skill never polls another
	// skill's remote (outdatedFor's own doc carries the finding). An empty
	// name is the batch, which has to judge everything: its targets are
	// exactly the rows Outdated reports updatable.
	rows, err := outdatedFor(st, cfg, name)
	if err != nil {
		return outcome, err
	}
	targets, err := selectUpdateTargets(st, cfg, rows, name, force, &outcome)
	if err != nil {
		return outcome, err
	}
	if len(targets) == 0 {
		// The prologue's failures are this command's to report, including on the
		// path where it does no other work. writeCommandPrologue deliberately
		// does not raise ErrOperationFailed for a per-agent reconcile failure --
		// it carries the finding in the Result and leaves the exit status to
		// "the final/abort boundary" (pipeline.go) -- and on this path
		// applyUpdateTargets, which holds that boundary, is never entered.
		// Without this, `fu update` against a current store printed the
		// contradictory pair `nothing to update` and `failed: claude: ...` and
		// exited 0, so a script reading $? saw success while an agent received
		// nothing. Same check adopt ends with (adopt.go), for the same reason.
		if len(outcome.Reconcile.Failed) != 0 {
			return outcome, ErrOperationFailed
		}
		return outcome, nil
	}
	// Source.Prepare needs the staging directory identity validated at
	// Store.Open; the actual prepare calls that spend it happen in
	// prepareUpdateSources below, still before any target's own transaction.
	stagingIdentity, err := st.StagingIdentity()
	if err != nil {
		return outcome, err
	}
	sources := prepareUpdateSources(st, cfg, targets, stagingIdentity)
	defer func() {
		err = errors.Join(err, closeUpdateSources(sources))
	}()
	err = a.applyUpdateTargets(st, a.detectedAgents(), cfg, targets, sources, force, &outcome)
	return outcome, err
}

// selectUpdateTargets decides which rows to update, entirely from Outdated's
// own verdict and before any source is prepared or any lock taken (the
// design spec's lock-boundary rule).
func selectUpdateTargets(st *store.Store, cfg *store.Config, rows []UpdateStatus, name string, force bool, outcome *UpdateBatchOutcome) ([]UpdateStatus, error) {
	if name != "" {
		for _, row := range rows {
			if row.Name != name {
				continue
			}
			if !row.Comparable {
				// Outdated's own Reason names the actual cause (a fixed lock,
				// a missing local path, ...); a generic failure here would
				// throw that explanation away.
				//
				// Flattened first: Reason quotes back recorded paths, and `fu
				// add` on a directory whose name contains a newline records it
				// verbatim, so an error line built from it could fabricate
				// further lines of output (round 3, Minor #3 -- the outdated
				// side already sanitizes the same string).
				return nil, fmt.Errorf("update %q: %s", name, singleLineReason(row.Reason))
			}
			if row.UpdateRefusesWithoutForce() && !force {
				// The promise is unconditional again, and now true. Round 1
				// hedged it ("replacing those changes if the upstream content
				// differs") because --force over an unmoved upstream took the
				// lock-only shape, which overwrites nothing -- an accurate
				// description of a defect. Round 3 fixed the defect instead:
				// --force now routes to the content shape whenever the store
				// copy has drifted, so it replaces the local changes with the
				// upstream content in every case this refusal can precede
				// (updateSkill's force arm). A hedge here would now understate
				// what the flag does, and understating a destructive flag is
				// its own kind of wrong.
				return nil, fmt.Errorf(
					"%w: %s; re-run with --force to replace those changes with the upstream content",
					ErrLocallyModified, describeLocalModification(st, name, row.Baseline, row.StoreDigest))
			}
			return []UpdateStatus{row}, nil
		}
		// A name Outdated produced no row for is one the config does not hold
		// -- outdatedFor filters against cfg.SkillNames (onlyRegisteredName),
		// which LoadConfig has already stripped every invalid name from. So
		// the repair path is exactly `fu show`'s and `fu rm`'s, and it is
		// reached through the same lookup rather than a fourth copy of it.
		return nil, unknownSkillError(st, cfg, name)
	}
	var targets []UpdateStatus
	for _, row := range rows {
		if !row.Comparable {
			// Reported rather than dropped: "could not tell" is not "up to
			// date", and this is the command that acts on the difference.
			//
			// Only when fu could not tell, though (round 4). A row whose
			// record has no upstream at all -- a `fu new` skill, a tag-pinned
			// one -- was judged exactly, and the answer is "there is nothing
			// upstream to pull, ever". Filing that under "could not judge"
			// said the opposite, once per such skill on every single run, on
			// the stderr channel skipped:, failed: and not attempted: share --
			// so round 3's fix ended up degrading the signal it was written to
			// protect, and suppressed "nothing to update" while doing it.
			// These rows stay silent here, the way !Updatable rows already do;
			// `fu outdated` still lists them under "not comparable", which is
			// that command's job.
			if !row.NoUpstream {
				outcome.Unjudged = append(outcome.Unjudged, UpdateSkip{Name: row.Name, Reason: row.Reason})
			}
			continue
		}
		if !row.Updatable {
			continue
		}
		if row.UpdateRefusesWithoutForce() {
			outcome.Skipped = append(outcome.Skipped, UpdateSkip{Name: row.Name, Reason: "locally modified"})
			continue
		}
		targets = append(targets, row)
	}
	return targets, nil
}

// singleLineReason flattens a judgement Reason for embedding in an error, the
// engine-side counterpart of the CLI's own singleLine (internal/cli/show.go).
// It is deliberately here rather than imported: the engine cannot depend on the
// CLI, and an error value must be safe to print wherever it surfaces.
func singleLineReason(reason string) string {
	replacer := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")
	return replacer.Replace(reason)
}

// updateSourceKey identifies one distinct source: (kind, url, ref) for a git
// source or (kind, path) for a local one. Several skills commonly share one
// repository -- the subdir source field exists precisely for that -- so
// preparing per skill would clone the same repository once per skill it
// serves; this is the dedup key prepareUpdateSources groups by.
// refKind is part of the identity and not merely of the ref: it decides which
// form the clone probes (Source.RefKind), so two records agreeing on url and
// ref but not on kind describe two different clones and must not share one.
type updateSourceKey struct {
	kind, url, ref, refKind, path string
}

func sourceKeyFromFields(fields map[string]string) updateSourceKey {
	return updateSourceKey{
		kind: fields["type"], url: fields["url"],
		ref: fields["ref"], refKind: fields["ref_kind"], path: fields["path"],
	}
}

// sourceFromFields reconstructs the source.Source a skill's recorded fields
// describe, so update can re-prepare it (clone again for git, reopen for
// local) the same way `fu add` originally prepared it. Only ever called for
// a row Outdated already proved Comparable, so the type is known to be one
// EncodeFields actually produces.
func sourceFromFields(fields map[string]string) (source.Source, error) {
	switch fields["type"] {
	case string(source.KindGit):
		return source.Source{
			Kind: source.KindGit,
			URL:  fields["url"],
			// fu.yaml records the fully-qualified ref (source.EncodeFields),
			// but Source.Ref -- and the clone it drives -- wants the short
			// branch-or-tag name the user originally typed. ref_kind (already
			// checked by Outdated's own judgeGitUpdate before this row could
			// be Comparable) says unambiguously which prefix to strip, so
			// exactly one is stripped: trimming both would rewrite a branch
			// legitimately named "refs/tags/release" into "release" and clone
			// a different ref than the one it goes on recording.
			Ref: shortRecordedRef(fields["ref"], fields["ref_kind"]),
			// Carried through, so the clone probes only the form the record
			// names. Dropping it let a branch deleted upstream fall through to
			// a same-named tag, turning the skill into a fixed lock `outdated`
			// never examines again (source.Source.RefKind).
			RefKind: fields["ref_kind"],
		}, nil
	case string(source.KindLocal):
		return source.Source{Kind: source.KindLocal, Path: fields["path"]}, nil
	default:
		// Defensive, mirroring judgeUpdate's own default arm (outdated.go):
		// fu.yaml is hand-editable, but a row this function is ever called for
		// already passed through Outdated as Comparable, so this path is not
		// expected to be reachable in practice.
		return source.Source{}, fmt.Errorf("unrecognized source type %q", fields["type"])
	}
}

// shortRecordedRef turns the fully-qualified ref fu.yaml records back into the
// short branch-or-tag name a clone takes, stripping the one prefix refKind
// names and no other. An unrecognised kind strips nothing: the record has not
// said which prefix is its own, and guessing is how the wrong ref gets cloned.
func shortRecordedRef(ref, refKind string) string {
	switch refKind {
	case "branch":
		return strings.TrimPrefix(ref, "refs/heads/")
	case "tag":
		return strings.TrimPrefix(ref, "refs/tags/")
	}
	return ref
}

// preparedUpdateSource is one distinct source Application.UpdateSkills
// prepared, plus the candidates ScanSource found inside it -- computed once
// and shared by every skill this source serves. err marks a source that
// failed to prepare or scan; every skill depending on it fails individually
// (isolated, like any other per-skill failure) rather than aborting the
// batch.
type preparedUpdateSource struct {
	src        source.Source
	prepared   *source.Prepared
	candidates []Candidate
	invalid    map[string]error
	err        error
}

// candidateAt finds the candidate ScanSource discovered at a skill's
// recorded subdir, so update can refuse by name when upstream no longer
// holds a valid skill there (moved, renamed away, or now invalid) instead of
// silently reporting nothing to do.
func (ps *preparedUpdateSource) candidateAt(subdir string) (Candidate, error) {
	for _, cand := range ps.candidates {
		if cand.Subdir == subdir {
			return cand, nil
		}
	}
	if reason, ok := ps.invalid[subdir]; ok {
		return Candidate{}, fmt.Errorf("recorded subdir %q is no longer a valid skill: %w", subdir, reason)
	}
	return Candidate{}, fmt.Errorf("recorded subdir %q no longer holds a valid skill upstream", subdir)
}

// updateSourcePreparedHook observes each distinct source
// prepareUpdateSources actually finishes preparing (successfully or not),
// keyed by its dedup identity. Nil in production and set only by a test
// asserting the lock-boundary ordering (every source prepared before any
// skill's own transaction takes the write lock, lockAcquiredHook in
// lock.go), mirroring resolveRemoteRefHook's own pattern (outdated.go): it
// fires on the exact call production already makes, never a second,
// test-only code path.
var updateSourcePreparedHook func(key updateSourceKey)

// prepareUpdateSources prepares every distinct source rows depends on,
// exactly once each (deduplicated by updateSourceKey). This is the whole of
// update's network I/O, and it always completes -- one way or another, an
// entry with err set marks a source that failed -- before returning, so
// every target's own transaction (which takes the write lock, run,
// pipeline.go) starts only once every prepare has already finished.
//
// The recorded cost of that ordering: a batch spanning N repositories holds N
// shallow clones under staging/ at once, each with its own 512 MiB budget
// (maxGitCloneBytes, internal/source/git.go), for as long as the batch runs.
// Nothing is corrupted by it -- a clone's scratch name is .fu-src-<hex>, which
// cannot collide with staging/<skill> -- and at personal scale the footprint
// is not the constraint the lock boundary is. This is a decision rather than
// an oversight: a per-source prepare -> transaction -> close pipeline would
// keep the lock boundary and bound the footprint at one clone, at the cost of
// interleaving network I/O with write transactions, and
// TestUpdateSkillsPreparesAllSourcesOutsideTheLock pins the shape that was
// chosen so a future change to it has to be deliberate.
func prepareUpdateSources(st *store.Store, cfg *store.Config, rows []UpdateStatus, stagingIdentity store.FileIdentity) map[updateSourceKey]*preparedUpdateSource {
	out := make(map[updateSourceKey]*preparedUpdateSource, len(rows))
	for _, row := range rows {
		fields := cfg.SourceFields(row.Name)
		key := sourceKeyFromFields(fields)
		if _, ok := out[key]; ok {
			continue
		}
		out[key] = prepareOneUpdateSource(st, fields, stagingIdentity)
		if updateSourcePreparedHook != nil {
			updateSourcePreparedHook(key)
		}
	}
	return out
}

func prepareOneUpdateSource(st *store.Store, fields map[string]string, stagingIdentity store.FileIdentity) *preparedUpdateSource {
	src, err := sourceFromFields(fields)
	if err != nil {
		return &preparedUpdateSource{err: err}
	}
	prepared, err := src.PrepareChecked(st.StagingDir(), stagingIdentity)
	if err != nil {
		return &preparedUpdateSource{src: src, err: fmt.Errorf("prepare source: %w", err)}
	}
	candidates, invalid, err := ScanSource(prepared)
	if err != nil {
		return &preparedUpdateSource{src: src, prepared: prepared, err: fmt.Errorf("inspect prepared source: %w", err)}
	}
	return &preparedUpdateSource{src: src, prepared: prepared, candidates: candidates, invalid: invalid}
}

// closeUpdateSources releases every prepared source's staging clone (a
// local source has nothing to release; Prepared.Close is a no-op for it).
func closeUpdateSources(sources map[updateSourceKey]*preparedUpdateSource) error {
	var errs []error
	for _, ps := range sources {
		if ps.prepared == nil {
			continue
		}
		if err := ps.prepared.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// applyUpdateTargets runs each target's own transaction against its
// already-prepared source, in the order Outdated produced them (sorted by
// Name).
//
// Every attempted target's own OperationOutcome and trailing reconcile
// Result are accumulated onto outcome (Operations, Reconcile via
// mergeResult, reconcile.go:356) regardless of how that target turned out,
// exactly as addSkillsDetailed accumulates them into its own res (add.go) --
// this is what lets the CLI report a reconcile finding (a conflict, a
// missing link, ...) through the same printResult/printDurableOutcome
// channel every sibling write command already uses, even for a target that
// otherwise updated cleanly, since reconcileChecked reconciles the whole
// config and can trip on a completely unrelated skill's already-drifted
// agent link (pipeline.go:464 passes it the whole cfg, not just this
// target's own entry).
//
// A per-skill failure is isolated into Reconcile.Failed and the batch
// continues -- addSkillsDetailed's own batch semantics (add.go) -- except
// for a setup- or store-level error, which aborts the remaining targets
// exactly as addSkillsDetailed's own batchFatal class does; that abort still
// records which target triggered it (into Reconcile.Failed) and which later
// targets were never reached (into Unattempted), the same bookkeeping
// addSkillsDetailed's own fatal path performs (add.go:362-370) before
// returning. A trailing reconcile-only failure (ErrOperationFailed) still
// counts its skill as updated, since the content or lock genuinely did
// move, but leaves the batch reporting failure overall.
func (a *Application) applyUpdateTargets(st *store.Store, agents []agent.Agent, cfg *store.Config, targets []UpdateStatus, sources map[updateSourceKey]*preparedUpdateSource, force bool, outcome *UpdateBatchOutcome) error {
	for index, row := range targets {
		fields := cfg.SourceFields(row.Name)
		ps := sources[sourceKeyFromFields(fields)]
		if ps.err != nil {
			outcome.Reconcile.Failed = append(outcome.Reconcile.Failed, FailedAction{Action: Action{Skill: row.Name}, Err: ps.err})
			continue
		}
		subdir := fields["subdir"]
		if subdir == "" {
			subdir = "."
		}
		cand, err := ps.candidateAt(subdir)
		if err != nil {
			outcome.Reconcile.Failed = append(outcome.Reconcile.Failed, FailedAction{Action: Action{Skill: row.Name}, Err: err})
			continue
		}
		newFields := ps.src.EncodeFields(cand.Subdir, ps.prepared.Lock())
		upOutcome, err := updateSkill(st, agents, ps.prepared, row.Name, cand, fields, newFields, force, a.hooks)
		// Only a real operation is recorded. updateSkill's pre-run failure paths
		// return a zero OperationOutcome, and appending that put a slot with no
		// Name in the list. Harmless today, since printDurableOutcome no-ops on
		// !DurablyStarted(), but a nameless slot is one refactor away from
		// printing "warning: update  committed...", and the failure itself is
		// already recorded by name in Reconcile.Failed either way.
		if upOutcome.Operation.Name != "" {
			outcome.Operations = append(outcome.Operations, upOutcome.Operation)
		}
		mergeResult(&outcome.Reconcile, upOutcome.Operation.Reconcile)
		if err != nil {
			batchFatal := isOperationSetupError(err) || isOperationStoreError(err) ||
				upOutcome.Operation.RecoveryPending ||
				(upOutcome.Operation.Committed && !upOutcome.Operation.CanonicalChecked) ||
				errors.Is(err, ErrTxnConflict) || errors.Is(err, ErrConcurrentStoreChange)
			if batchFatal {
				// Whatever else failed before this one, checked before this
				// target's own trigger is appended just below -- mirroring
				// addSkillsDetailed's own reconcileFailed||candidateFailed
				// check at its equivalent return (add.go:367-369).
				alreadyFailed := len(outcome.Reconcile.Failed) != 0
				outcome.Reconcile.Failed = append(outcome.Reconcile.Failed, FailedAction{Action: Action{Skill: row.Name}, Err: err})
				for _, remaining := range targets[index+1:] {
					outcome.Unattempted = append(outcome.Unattempted, remaining.Name)
				}
				if alreadyFailed {
					err = errors.Join(err, ErrOperationFailed)
				}
				return err
			}
			if !errors.Is(err, ErrOperationFailed) {
				outcome.Reconcile.Failed = append(outcome.Reconcile.Failed, FailedAction{Action: Action{Skill: row.Name}, Err: err})
				continue
			}
			// ErrOperationFailed: the trailing reconcile's own Failed
			// entries were already merged in above; the update itself still
			// committed, so this target still counts as updated below.
		}
		if upOutcome.LockOnly {
			outcome.LockOnly = append(outcome.LockOnly, row.Name)
		} else {
			outcome.Updated = append(outcome.Updated, row.Name)
		}
	}
	// An isolated per-skill failure -- or a trailing reconcile-only one,
	// merged in above -- does not abort the batch, but it must still
	// surface: len(outcome.Reconcile.Failed) != 0 is exactly
	// addSkillsDetailed's own reconcileFailed||candidateFailed check
	// (add.go), read directly off the merged Result instead of a separate
	// flag since every path that sets either now also appends here. Without
	// it, a batch that isolated a failure and kept going returned nil
	// overall, which the CLI reads as full success (exit 0).
	if len(outcome.Reconcile.Failed) != 0 {
		return ErrOperationFailed
	}
	return nil
}
