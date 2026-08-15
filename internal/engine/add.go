// internal/engine/add.go
package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

// Candidate is one installable skill found inside a prepared source.
type Candidate struct {
	Name        string
	Description string
	// Subdir is the candidate's path relative to the source root; "." when
	// the candidate is the source root itself (recorded as no subdir).
	Subdir string
	// Digest binds the selection to the exact normalized tree inspected by
	// ScanSource; installation rechecks it through the same Prepared root.
	Digest string
}

// ScanSource inventories every valid skill inside a prepared source.
// invalid maps each rejected candidate's path -- relative to the source root
// -- to why it was rejected (name/description violations, unreadable
// subtrees); err is non-nil only when the source root itself is unusable.
//
// The key used to be the absolute location the candidate was read from. For a
// git source that is the clone directory under staging, so `fu add
// file:///…/bare` reported `invalid: /private/tmp/…/staging/.fu-src-<hex>/broken`
// -- a path that no longer exists by the time the user reads it. The source as
// the user typed it lives on AddPlan (AddSession.SourceArg); pairing the two is
// what makes the diagnostic actionable.
func ScanSource(p *source.Prepared) ([]Candidate, map[string]error, error) {
	valid, relativeInvalid, err := skill.ScanFS(p.FS())
	if err != nil {
		return nil, nil, err
	}
	invalid := make(map[string]error, len(relativeInvalid))
	for rel, scanErr := range relativeInvalid {
		invalid[filepath.FromSlash(rel)] = scanErr
	}
	cands := make([]Candidate, 0, len(valid))
	for _, c := range valid {
		proj, projectErr := skill.ProjectDir(p.FS(), c.Dir)
		if projectErr != nil {
			invalid[filepath.FromSlash(c.Dir)] = projectErr
			continue
		}
		// SPEC rule 7's path-safety half, applied here rather than only at
		// install time. The projection this check consumes has just been
		// computed, so running it costs nothing -- and skipping it meant an
		// escaping candidate was offered to the user as installable and then
		// aborted the batch mid-way, after earlier skills had committed and
		// with the remaining ones never attempted (round 18 finding I7).
		if linkErr := skill.ValidateLinks(proj); linkErr != nil {
			invalid[filepath.FromSlash(c.Dir)] = linkErr
			continue
		}
		digest, digestErr := skill.DigestManifest(proj)
		if digestErr != nil {
			invalid[filepath.FromSlash(c.Dir)] = digestErr
			continue
		}
		cands = append(cands, Candidate{
			Name:        c.Meta.Name,
			Description: c.Meta.Description,
			Subdir:      c.Dir,
			Digest:      digest,
		})
	}
	byName := make(map[string][]int, len(cands))
	for i, candidate := range cands {
		byName[candidate.Name] = append(byName[candidate.Name], i)
	}
	keep := make([]Candidate, 0, len(cands))
	for _, candidate := range cands {
		indexes := byName[candidate.Name]
		if len(indexes) == 1 {
			keep = append(keep, candidate)
			continue
		}
		subdirs := make([]string, 0, len(indexes))
		for _, index := range indexes {
			subdirs = append(subdirs, cands[index].Subdir)
		}
		sort.Strings(subdirs)
		invalid[filepath.FromSlash(candidate.Subdir)] = fmt.Errorf(
			"duplicate skill name %q found at source subdirectories %s; choose one copy: for a local source, add its desired subdirectory directly; for a git source, remove or rename one duplicate in the repository and retry",
			candidate.Name, strings.Join(subdirs, ", "))
	}
	return keep, invalid, nil
}

// ErrSkillExists marks the one retryable refusal in a batch: the name is
// already registered, so the item is skipped (SPEC rule 1) rather than
// aborting the batch.
var ErrSkillExists = errors.New("skill already exists")

// checkAddAvailable is add's preflight: the name must be free in the config
// and at both store and staging positions, and the candidate must still
// validate at install time (the source may have changed since ScanSource).
func checkAddAvailable(st *store.Store, cfg *store.Config, name string) error {
	if cfg.HasSkill(name) {
		// SPEC rule 1 requires the duplicate-name refusal to point at `fu rm`.
		// It could not before that command existed; it can now, and DESIGN §6
		// records naming the way out as a standing rule for any refusal or
		// skip (round 18 finding M12).
		return fmt.Errorf("%w: %q; run `fu rm %s` first to install a different copy", ErrSkillExists, name, name)
	}
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return fmt.Errorf("use checked skills root: %w", err)
	}
	if _, err := skillsRoot.Lstat(name); err == nil {
		return fmt.Errorf("store already holds content at %s (not registered in fu.yaml); move it aside or remove it before running `fu add` again", filepath.Join(st.SkillsDir(), name))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check existing store content at %s: %w", filepath.Join(st.SkillsDir(), name), err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return fmt.Errorf("use checked staging root: %w", err)
	}
	if _, err := stagingRoot.Lstat(name); err == nil {
		return fmt.Errorf("staging already holds unmatched content at %s; move it aside or remove it before running `fu add` again", filepath.Join(st.StagingDir(), name))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check existing staging content at %s: %w", filepath.Join(st.StagingDir(), name), err)
	}
	return nil
}

// declaredFromProjection converts a source projection into the declaration
// set the copy primitive creates from and recovery settles against.
//
// Modes deliberately carry only Perm(): setuid/setgid/sticky bits are
// stripped on import and never published (import hygiene; the digest
// projection hashes the same perm-only mode, so the strip cannot create a
// manifest mismatch).
func declaredFromProjection(proj []skill.ManifestEntry) []store.DeclaredEntry {
	declared := make([]store.DeclaredEntry, 0, len(proj))
	for _, e := range proj {
		switch {
		case e.Mode&fs.ModeDir != 0:
			declared = append(declared, store.NewDeclaredDir(e.Path, e.Mode.Perm()))
		case e.Mode&fs.ModeSymlink != 0:
			declared = append(declared, store.NewDeclaredSymlink(e.Path, e.Target))
		default:
			declared = append(declared, store.DeclaredEntry{
				Path:   e.Path,
				Kind:   "file",
				Mode:   uint32(e.Mode.Perm()),
				Digest: e.Digest,
			})
		}
	}
	return declared
}

// manifestEntries projects a store ownership manifest back into the skill
// digest projection so the shared safety and digest helpers can consume it.
func manifestEntries(tree store.OwnedTree) []skill.ManifestEntry {
	out := make([]skill.ManifestEntry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		out = append(out, skill.ManifestEntry{
			Path:   e.Path,
			Mode:   fs.FileMode(e.Mode),
			Digest: e.Digest,
			Target: e.Target,
		})
	}
	return out
}

// addSkillDefault installs one candidate from a prepared source into the store and
// registers it enabled everywhere, recording the source lock (SPEC §5.1
// add; scenario 1).
func addSkillDefault(st *store.Store, agents []agent.Agent, p *source.Prepared, cand Candidate, fields map[string]string) (Result, error) {
	return addSkill(st, agents, p, cand, fields, hooks{})
}

// addSkill carries AddSkill's implementation plus the pipeline's test-only
// hooks at the add-specific durable boundaries.
func addSkill(st *store.Store, agents []agent.Agent, p *source.Prepared, cand Candidate, fields map[string]string, h hooks) (Result, error) {
	return addSkillTracked(st, agents, p, cand, fields, h, nil)
}

func addSkillTracked(st *store.Store, agents []agent.Agent, p *source.Prepared, cand Candidate, fields map[string]string, h hooks, outcome *OperationOutcome) (Result, error) {
	name := cand.Name
	if outcome != nil {
		outcome.Name = name
	}
	if err := skill.ValidateName(name); err != nil {
		return Result{}, err
	}
	txn := &TxnRecord{
		Op:           "add",
		Name:         name,
		SourceFields: fields,
		Targets: []string{
			filepath.Join("staging", name),
			filepath.Join("store", "skills", name),
		},
	}
	return run(st, agents, Op{
		Message:        "add: " + name,
		Txn:            txn,
		outcome:        outcome,
		AllowedChanges: []string{"fu.yaml", filepath.ToSlash(filepath.Join("skills", name))},
		ValidatePrepared: func(st *store.Store, prepared store.PreparedCommit) error {
			if txn.Payload == nil {
				return errors.New("add transaction has no payload manifest at commit preparation")
			}
			if err := st.ValidateSkillOwned(name, *txn.Payload); err != nil {
				return fmt.Errorf("validate published skill before commit: %w", err)
			}
			return st.ValidatePreparedOwnedTree(prepared, filepath.ToSlash(filepath.Join("skills", name)), *txn.Payload)
		},
		Preflight: func(st *store.Store, cfg *store.Config) error {
			return checkAddAvailable(st, cfg, name)
		},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			if err := checkAddAvailable(st, cfg, name); err != nil {
				return err
			}
			srcRoot, err := p.Root()
			if err != nil {
				return fmt.Errorf("use prepared source %s: %w", p.Dir(), err)
			}
			proj, err := skill.ProjectDir(srcRoot.FS(), cand.Subdir)
			if err != nil {
				return fmt.Errorf("project source %s: %w", cand.Subdir, err)
			}
			observedDigest, err := skill.DigestManifest(proj)
			if err != nil {
				return fmt.Errorf("digest source %s: %w", cand.Subdir, err)
			}
			if cand.Digest == "" || observedDigest != cand.Digest {
				return fmt.Errorf("prepared source candidate %s changed since inspection", cand.Subdir)
			}
			declared := declaredFromProjection(proj)
			txn.Declared = declared
			rootPayload, err := createTxnStagedRoot(st, txn, name, 0o755, h)
			if err != nil {
				return err
			}
			if err := h.fire(h.afterDeclaredTxn); err != nil {
				return err
			}
			payload, err := st.CopyStagedTreeOwned(name, rootPayload, srcRoot, cand.Subdir, declared)
			if err != nil {
				return fmt.Errorf("copy %s into staging: %w", cand.Subdir, err)
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
				return fmt.Errorf("record copied skill: %w", err)
			}
			if err := st.ValidateStagedOwned(name, payload); err != nil {
				return fmt.Errorf("validate exact staged skill: %w", err)
			}
			stagedRoot, err := st.StagingRoot()
			if err != nil {
				return err
			}
			if err := skill.ValidateSkillDir(stagedRoot.FS(), name); err != nil {
				return fmt.Errorf("validate staged skill: %w", err)
			}
			d, err := digestOwnedPayload(payload)
			if err != nil {
				return fmt.Errorf("digest staged skill ownership: %w", err)
			}
			if d != observedDigest {
				return fmt.Errorf("copied skill %s does not match the source digest verified before staging", cand.Subdir)
			}
			txn.Digest = d
			txn.Stage = "prepared"
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record prepared skill: %w", err)
			}
			if err := cfg.AddSkill(name, d); err != nil {
				return err
			}
			cfg.SetSourceFields(name, fields)
			return nil
		},
		Publish: func(st *store.Store) error {
			if txn.Payload == nil {
				return errors.New("add transaction has no staged ownership manifest")
			}
			skillsRoot, err := st.SkillsRoot()
			if err != nil {
				return fmt.Errorf("use checked skills root: %w", err)
			}
			if _, err := skillsRoot.Lstat(name); err == nil {
				return fmt.Errorf("%s appeared while %q was being added; refusing to replace it", filepath.Join(st.SkillsDir(), name), name)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return st.PublishStagedOwned(name, *txn.Payload)
		},
	}, h)
}

// addSkills installs every listed candidate in one batch: each candidate
// runs its own transaction (a batch therefore commits once per skill), a
// name collision skips the item with ErrSkillExists reported in skipped,
// and an operation-local failure aborts the batch. A trailing per-agent
// reconcile failure is accumulated while independent candidates continue.
// res accumulates the trailing reconcile findings of every operation.
func addSkills(st *store.Store, agents []agent.Agent, p *source.Prepared, cands []Candidate, fields func(Candidate) map[string]string) (res Result, added, skipped []string, err error) {
	res, added, skipped, _, _, err = addSkillsDetailed(st, agents, p, cands, fields, hooks{})
	return res, added, skipped, err
}

func addSkillsDetailed(st *store.Store, agents []agent.Agent, p *source.Prepared, cands []Candidate, fields func(Candidate) map[string]string, h hooks) (res Result, added, skipped, unattempted []string, operations []OperationOutcome, err error) {
	reconcileFailed := false
	candidateFailed := false
	for index, cand := range cands {
		outcome := OperationOutcome{Name: cand.Name}
		opRes, err := addSkillTracked(st, agents, p, cand, fields(cand), h, &outcome)
		operations = append(operations, outcome)
		mergeResult(&res, opRes)
		if err != nil {
			batchFatal := isOperationSetupError(err) || isOperationStoreError(err) || outcome.RecoveryPending ||
				(outcome.Committed && !outcome.CanonicalChecked) ||
				errors.Is(err, ErrTxnConflict) || errors.Is(err, ErrConcurrentStoreChange)
			// SPEC rule 1 states two different rules for the same collision:
			// a targeted add refuses a duplicate, while a batch skips and
			// reports that item. Applying the batch rule to a
			// single candidate made `fu add <dir>` install nothing, print
			// nothing to stdout and exit 0 -- indistinguishable from success
			// to a script (round 18 finding I18).
			if errors.Is(err, ErrSkillExists) && len(cands) > 1 && !batchFatal {
				skipped = append(skipped, cand.Name)
				continue
			}
			if !batchFatal && errors.Is(err, ErrOperationFailed) {
				if outcome.Committed && !outcome.RecoveryPending {
					added = append(added, cand.Name)
				}
				reconcileFailed = true
				continue
			}
			if len(cands) > 1 && !batchFatal {
				res.Failed = append(res.Failed, FailedAction{Action: Action{Skill: cand.Name}, Err: err})
				candidateFailed = true
				continue
			}
			res.Failed = append(res.Failed, FailedAction{Action: Action{Skill: cand.Name}, Err: err})
			// Whatever stopped the batch, the candidates after this one were
			// never attempted. Saying so is the difference between "these
			// three are not installed" and silence (round 18 finding I19).
			unattempted = append(unattempted, remainingNames(cands, index+1)...)
			if reconcileFailed || candidateFailed {
				err = errors.Join(err, ErrOperationFailed)
			}
			return res, added, skipped, unattempted, operations, err
		}
		added = append(added, cand.Name)
	}
	if reconcileFailed || candidateFailed {
		return res, added, skipped, unattempted, operations, ErrOperationFailed
	}
	return res, added, skipped, unattempted, operations, nil
}

// remainingNames lists the candidate names from index onward.
func remainingNames(cands []Candidate, from int) []string {
	if from >= len(cands) {
		return nil
	}
	names := make([]string, 0, len(cands)-from)
	for _, cand := range cands[from:] {
		names = append(names, cand.Name)
	}
	return names
}
