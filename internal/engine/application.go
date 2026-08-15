package engine

import (
	"fmt"
	"path/filepath"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
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

func readDiagnostics(st *store.Store, cfg *store.Config) ReadDiagnostics {
	diagnostics := ReadDiagnostics{
		ConfigPath:    st.ConfigPath(),
		VersionTooNew: cfg.VersionTooNew(),
	}
	for _, invalid := range cfg.InvalidNames() {
		diagnostics.InvalidNames = append(diagnostics.InvalidNames, InvalidConfigName{
			Name: invalid.Name, Reason: invalid.Reason,
		})
	}
	return diagnostics
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

func (a *Application) ListSkills() (ListOutcome, error) {
	st, cfg, err := a.readStore()
	if err != nil {
		return ListOutcome{}, err
	}
	agents := a.detectedAgents()
	outcome := ListOutcome{Diagnostics: readDiagnostics(st, cfg)}
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
	outcome := ShowOutcome{Name: name, Diagnostics: readDiagnostics(st, cfg)}
	if !cfg.HasSkill(name) {
		for _, invalid := range outcome.Diagnostics.InvalidNames {
			if invalid.Name == name {
				return outcome, fmt.Errorf("skill name %q fails validation (%s) and is ignored; edit %s to fix or remove it", invalid.Name, invalid.Reason, outcome.Diagnostics.ConfigPath)
			}
		}
		return outcome, fmt.Errorf("unknown skill %q", name)
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
