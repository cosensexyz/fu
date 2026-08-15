package engine

import (
	"errors"
	"fmt"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

// AddPlan is an engine-owned source inspection whose prepared source remains
// pinned while a UI presents Candidates and chooses a subset to install.
type AddPlan struct {
	store *store.Store
	// arg is the source as the user typed it, kept for diagnostics: a
	// resolved path or clone directory is not what they would recognise.
	arg        string
	source     source.Source
	prepared   *source.Prepared
	candidates []Candidate
	invalid    map[string]error
	agents     []agent.Agent
	prologue   Result
	installed  bool
	hooks      hooks
}

// AddSession is the application-facing lifetime of one prepared source.
// It keeps source ownership pinned while a UI chooses candidates.
type AddSession interface {
	Candidates() []Candidate
	// Invalid maps each rejected candidate's path *relative to the source* to
	// why it was rejected. It used to be keyed on the absolute location the
	// candidate was read from, which for a git source is the clone directory
	// -- `invalid: /private/tmp/.../staging/.fu-src-<hex>/broken: ...`, naming
	// a path deleted the moment the command exits. Pair it with SourceArg to
	// render something the user can act on.
	Invalid() map[string]error
	// SourceArg is the source exactly as the user typed it. A resolved path or
	// clone directory is not what they would recognise.
	SourceArg() string
	// NoCandidates reports that the source held nothing installable. Whether
	// that is an error or an ordinary outcome is a product decision, so it
	// belongs here rather than in each front end's own len() check (round 18
	// finding I20).
	NoCandidates() error
	// NoSelection returns the engine verdict when a user selects no candidate.
	// A clean prologue makes that an ordinary no-op; a failed prologue keeps
	// the command in the operation-failure class even though no install ran.
	NoSelection() error
	// Prologue is the mandatory recovery prologue's reconcile result. Install
	// folds it into its own outcome, but a front end that returns before
	// installing has to be able to report it: those are the exits where losing
	// it matters most, since the findings come from the recovery boundary
	// (round 18 finding M18's class, on add's two early returns).
	Prologue() Result
	Install([]Candidate) (AddOutcome, error)
	Close() error
}

// AddPreparation carries the recovery prologue even when source preparation
// fails before an AddSession can be created. Front ends must report Prologue
// on both success and failure paths.
type AddPreparation struct {
	Session  AddSession
	Prologue Result
}

// ErrNoSkillsFound reports a prepared source with no installable skill. It is
// an error rather than an empty success: the user named a source expecting to
// install from it, and exiting 0 having done nothing would be indistinguishable
// from a successful install to a script.
var ErrNoSkillsFound = errors.New("no valid skills found")

// ErrInvalidAddRef marks every malformed or inapplicable --ref value so front
// ends have one usage-error classification boundary.
var ErrInvalidAddRef = errors.New("invalid add ref")

// ErrEmptyAddRef is the explicit-empty member of ErrInvalidAddRef's usage
// class. Only a front end can distinguish an omitted string flag from one the
// user supplied as "", so it uses this engine-owned reason directly.
var ErrEmptyAddRef = fmt.Errorf("%w: --ref cannot be empty", ErrInvalidAddRef)

// AddOutcome reports the durable batch result plus agent reconciliation.
type AddOutcome struct {
	Added   []string
	Skipped []string
	// Unattempted names the candidates the batch never reached because an
	// earlier one failed. Without it, `fu add --all` reported the failure of
	// candidate 1 and said nothing at all about 2 and 3, leaving the user to
	// re-run and re-fail their way to convergence (round 18 finding I19).
	Unattempted []string
	Operations  []OperationOutcome
	Reconcile   Result
}

func parseAddSource(arg, ref string) (source.Source, error) {
	src, err := source.ParseArgWithRef(arg, ref)
	if err != nil {
		if errors.Is(err, source.ErrRefRequiresGit) ||
			errors.Is(err, source.ErrCommitRefUnsupported) ||
			errors.Is(err, source.ErrInvalidRef) {
			return source.Source{}, fmt.Errorf("%w: %w", ErrInvalidAddRef, err)
		}
		return source.Source{}, err
	}
	return src, nil
}

func prepareAddSource(st *store.Store, arg string, src source.Source, agents []agent.Agent, h hooks) (*AddPlan, Result, error) {
	prologue, err := writeCommandPrologue(st, agents)
	if err != nil {
		return nil, prologue, err
	}
	stagingIdentity, err := st.StagingIdentity()
	if err != nil {
		return nil, prologue, err
	}
	prepared, err := src.PrepareChecked(st.StagingDir(), stagingIdentity)
	if err != nil {
		return nil, prologue, err
	}
	candidates, invalid, err := ScanSource(prepared)
	if err != nil {
		return nil, prologue, errors.Join(err, prepared.Close())
	}
	return &AddPlan{
		store: st, arg: arg, source: src, prepared: prepared,
		candidates: candidates, invalid: invalid, agents: agents, prologue: prologue, hooks: h,
	}, prologue, nil
}

func (p *AddPlan) Candidates() []Candidate {
	return append([]Candidate(nil), p.candidates...)
}

func (p *AddPlan) Invalid() map[string]error {
	out := make(map[string]error, len(p.invalid))
	for path, err := range p.invalid {
		out[path] = err
	}
	return out
}

func (p *AddPlan) SourceArg() string { return p.arg }

func (p *AddPlan) Prologue() Result { return p.prologue }

func (p *AddPlan) NoCandidates() error {
	if len(p.candidates) != 0 {
		return nil
	}
	return fmt.Errorf("%w in %s", ErrNoSkillsFound, p.arg)
}

func (p *AddPlan) NoSelection() error {
	if len(p.prologue.Failed) != 0 {
		return ErrOperationFailed
	}
	return nil
}

func (p *AddPlan) Install(selected []Candidate) (AddOutcome, error) {
	outcome := AddOutcome{Reconcile: p.prologue}
	if p.prepared == nil {
		return outcome, errors.New("add plan is closed")
	}
	if p.installed {
		return outcome, errors.New("add plan was already installed")
	}
	p.installed = true
	available := make(map[string]bool, len(p.candidates))
	for _, candidate := range p.candidates {
		available[candidate.Name+"\x00"+candidate.Subdir+"\x00"+candidate.Digest] = true
	}
	for _, candidate := range selected {
		if !available[candidate.Name+"\x00"+candidate.Subdir+"\x00"+candidate.Digest] {
			return outcome, fmt.Errorf("candidate %s at %s was not part of this source inspection", candidate.Name, candidate.Subdir)
		}
	}
	fields := func(candidate Candidate) map[string]string {
		return p.source.EncodeFields(candidate.Subdir, p.prepared.Lock())
	}
	result, added, skipped, unattempted, operations, err := addSkillsDetailed(p.store, p.agents, p.prepared, selected, fields, p.hooks)
	mergeResult(&outcome.Reconcile, result)
	outcome.Added = added
	outcome.Skipped = skipped
	outcome.Unattempted = unattempted
	outcome.Operations = operations
	if err == nil && len(outcome.Reconcile.Failed) != 0 {
		err = ErrOperationFailed
	}
	return outcome, err
}

func (p *AddPlan) Close() error {
	if p.prepared == nil {
		return nil
	}
	if err := p.prepared.Close(); err != nil {
		return err
	}
	p.prepared = nil
	return nil
}
