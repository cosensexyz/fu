// internal/engine/adopt_txn.go
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

func init() {
	RegisterRecoverReporter("adopt", recoverAdoptSkill)
}

// recoverAdoptSkill drives an interrupted adopt to a terminal state. Unlike
// new/add, a committed adopt is its own end state and is never compensated:
// recovery validates the operation commit and the store state, then
// CONTINUES the agent-side switching (phase 3 of AdoptPlan) before clearing
// the WAL. An uncommitted adopt rolls back exactly like an install (the
// agent-side switching never ran -- it only starts after the commit).
func recoverAdoptSkill(st *store.Store, record TxnRecord) (Result, error) {
	if err := validateAdoptTxn(record); err != nil {
		return Result{}, err
	}
	storeRoot, err := st.StoreRoot()
	if err != nil {
		return Result{}, fmt.Errorf("recover adopt transaction through checked repository root: %w", err)
	}
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return Result{}, fmt.Errorf("recover adopt transaction through checked skills root: %w", err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return Result{}, fmt.Errorf("recover adopt transaction through checked staging root: %w", err)
	}
	recoveryRoot, err := st.RecoveryRoot()
	if err != nil {
		return Result{}, fmt.Errorf("recover adopt transaction through checked recovery root: %w", err)
	}
	startHash := plumbing.NewHash(record.StartHead)
	startCommit, err := st.Repo.CommitObject(startHash)
	if err != nil {
		return Result{}, fmt.Errorf("load adopt transaction start commit %s: %w", record.StartHead, err)
	}
	startFile, err := startCommit.File("fu.yaml")
	if err != nil {
		return Result{}, fmt.Errorf("load fu.yaml from adopt transaction start commit: %w", err)
	}
	startConfig, err := startFile.Contents()
	if err != nil {
		return Result{}, fmt.Errorf("read fu.yaml from adopt transaction start commit: %w", err)
	}
	if !bytes.Equal([]byte(startConfig), record.ConfigBefore) {
		return Result{}, fmt.Errorf("%w: recorded starting config does not match %s", ErrTxnConflict, record.StartHead)
	}

	expectedConfig, err := expectedAdoptConfig(record)
	if err != nil {
		return Result{}, err
	}
	currentHead, err := st.Repo.Head()
	if err != nil {
		return Result{}, fmt.Errorf("read HEAD while recovering adopt transaction: %w", err)
	}
	if currentHead.Hash() == startHash {
		return Result{}, rollBackUncommittedInstall(st, storeRoot, skillsRoot, stagingRoot, recoveryRoot, record, expectedConfig)
	}
	result := Result{}
	err = finishCommittedAdopt(st, storeRoot, skillsRoot, stagingRoot, recoveryRoot, record, startHash, currentHead.Hash(), expectedConfig, &result)
	return result, err
}

func validateAdoptTxn(record TxnRecord) error {
	if record.Op != "adopt" {
		return fmt.Errorf("adopt recovery received operation %q", record.Op)
	}
	if err := skill.ValidateName(record.Name); err != nil {
		return fmt.Errorf("invalid skill name in adopt transaction: %w", err)
	}
	if !plumbing.IsHash(record.StartHead) {
		return fmt.Errorf("adopt transaction has invalid start HEAD %q", record.StartHead)
	}
	if record.Message != "adopt: "+record.Name {
		return fmt.Errorf("adopt transaction message %q does not match skill %q", record.Message, record.Name)
	}
	// The pipeline's first revision is "started" with a nil payload (adopt's
	// Mutate records the first ownership manifest at its own declared
	// revision); only later stages must carry one, mirroring
	// validateInstallTxn (round 6 finding C1).
	if record.Payload != nil {
		if err := record.Payload.Validate(); err != nil {
			return fmt.Errorf("adopt transaction has invalid payload ownership manifest: %w", err)
		}
	} else if record.Stage != "started" {
		return fmt.Errorf("%w: adopt transaction has no payload ownership manifest", ErrTxnConflict)
	}
	if record.StagingReservation != nil {
		if err := record.StagingReservation.Validate(); err != nil {
			return fmt.Errorf("adopt transaction has an invalid staging reservation: %w", err)
		}
		if record.Payload != nil {
			return fmt.Errorf("%w: adopt transaction records both a private staging reservation and a published payload", ErrTxnConflict)
		}
	}
	if len(record.ConfigBefore) == 0 {
		return errors.New("adopt transaction has no starting config snapshot")
	}
	switch record.Stage {
	case "started", "prepared", "config-saved", "published":
	default:
		return fmt.Errorf("adopt transaction has unknown stage %q", record.Stage)
	}
	if record.Stage != "started" && record.Digest == "" {
		return fmt.Errorf("adopt transaction stage %q has no prepared-content digest", record.Stage)
	}
	// Declarations describe work not yet done, so they cannot outlive the
	// stage that does it -- mirroring validateInstallTxn (round 8 finding
	// M2). The shared rollback settles Declared regardless of stage, so a
	// stale declaration would otherwise be settled as if the copy had only
	// partially run.
	if len(record.Declared) > 0 && record.Stage != "started" {
		return fmt.Errorf("adopt transaction stage %q still declares uncreated entries", record.Stage)
	}
	for _, declared := range record.Declared {
		if err := declared.Validate(); err != nil {
			return fmt.Errorf("adopt transaction has an invalid declared entry: %w", err)
		}
	}
	if record.Archive != nil {
		archive := record.Archive
		if archive.Agent == "" || archive.Retired == "" || !adoptIdentityValid(archive.OriginalIdentity) {
			return fmt.Errorf("adopt transaction has an incomplete archive record: %+v", record.Archive)
		}
		switch archive.Stage {
		case "planned", "retired", "copied", "cleaned":
		default:
			return fmt.Errorf("adopt transaction has unknown archive stage %q", archive.Stage)
		}
		if archive.OriginalKind != adoptEntryDirectory && archive.OriginalKind != adoptEntrySymlink {
			return fmt.Errorf("adopt transaction archive has unknown original kind %q", archive.OriginalKind)
		}
		if archive.OriginalKind == adoptEntrySymlink {
			if !validAdoptLinkArchiveName(archive.LinkArchive) {
				return fmt.Errorf("symlink adopt transaction has invalid durable link archive %q", archive.LinkArchive)
			}
			if archive.Payload != "" || archive.SourceManifest != nil || archive.Base != nil || archive.Manifest != nil {
				return errors.New("symlink adopt transaction must not carry a tree archive")
			}
			if archive.Stage != "planned" && archive.Stage != "retired" && archive.Stage != "cleaned" {
				return fmt.Errorf("symlink adopt transaction has invalid stage %q", archive.Stage)
			}
		} else {
			if archive.Payload == "" || archive.SourceManifest == nil {
				return errors.New("directory adopt transaction archive has no exact source manifest")
			}
			if err := archive.SourceManifest.Validate(); err != nil {
				return fmt.Errorf("adopt transaction has an invalid archive source manifest: %w", err)
			}
		}
		if archive.Base != nil {
			if err := archive.Base.Validate(); err != nil {
				return fmt.Errorf("adopt transaction has an invalid archive base manifest: %w", err)
			}
		}
		if archive.Manifest != nil {
			if err := archive.Manifest.Validate(); err != nil {
				return fmt.Errorf("adopt transaction has an invalid archive manifest: %w", err)
			}
		}
		if archive.OriginalKind == adoptEntryDirectory &&
			(archive.Stage == "copied" || archive.Stage == "cleaned") && archive.Manifest == nil {
			return fmt.Errorf("adopt transaction archive stage %q has no completed manifest", archive.Stage)
		}
	}
	if record.DirSwitch != nil {
		sw := record.DirSwitch
		if sw.Agent == "" || sw.Target == "" || sw.Sibling == "" {
			return fmt.Errorf("adopt transaction has an incomplete whole-directory switch record: %+v", sw)
		}
		switch sw.Stage {
		case "building", "swapped", "done":
		default:
			return fmt.Errorf("adopt transaction has unknown whole-directory switch stage %q", sw.Stage)
		}
		if (sw.Stage == "swapped" || sw.Stage == "done") && !validAdoptLinkArchiveName(sw.LinkArchive) {
			return fmt.Errorf("whole-directory switch has invalid durable link archive %q", sw.LinkArchive)
		}
	}
	return nil
}

// expectedAdoptConfig is ConfigBefore with the skill registered, its source
// record and per-agent overrides applied -- the config a completed adopt
// must have written.
func expectedAdoptConfig(record TxnRecord) ([]byte, error) {
	if record.Digest == "" {
		return nil, nil
	}
	cfg, err := store.LoadConfigBytes(record.ConfigBefore, "fu.yaml at transaction start")
	if err != nil {
		return nil, fmt.Errorf("parse adopt transaction starting config: %w", err)
	}
	if cfg.HasSkill(record.Name) {
		return nil, fmt.Errorf("%w: skill %q already existed at transaction start", ErrTxnConflict, record.Name)
	}
	if err := cfg.AddSkill(record.Name, record.Digest); err != nil {
		return nil, fmt.Errorf("reconstruct adopt transaction config: %w", err)
	}
	if len(record.SourceFields) > 0 {
		cfg.SetSourceFields(record.Name, record.SourceFields)
	}
	// Sorted iteration matches the operation side's own write order, so the
	// reconstructed bytes are the exact bytes the operation committed
	// (finding C1).
	for _, agentName := range sortedNames(record.Overrides) {
		cfg.SetAgent(record.Name, agentName, record.Overrides[agentName])
	}
	out, err := cfg.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode reconstructed adopt transaction config: %w", err)
	}
	return out, nil
}

// finishCommittedAdopt validates the operation commit and store state, then
// continues the agent-side switching for every remaining agent.
func finishCommittedAdopt(
	st *store.Store,
	storeRoot, skillsRoot, stagingRoot, recoveryRoot *os.Root,
	record TxnRecord,
	startHash, currentHash plumbing.Hash,
	expectedConfig []byte,
	report *Result,
) error {
	if err := validateAdoptTargetPaths(record.AdoptTargets); err != nil {
		return err
	}
	currentCommit, err := st.Repo.CommitObject(currentHash)
	if err != nil {
		return err
	}
	if len(currentCommit.ParentHashes) != 1 || currentCommit.ParentHashes[0] != startHash || currentCommit.Message != record.Message {
		return fmt.Errorf("%w: commit %s is not the recorded adopt commit based on %s", ErrTxnConflict, currentHash, startHash)
	}
	if record.CommitTree == "" {
		return fmt.Errorf("%w: committed adopt transaction has no prepared operation-tree fingerprint", ErrTxnConflict)
	}
	fingerprint, err := st.CommitTreeFingerprint(currentHash)
	if err != nil {
		return fmt.Errorf("fingerprint recorded adopt commit %s: %w", currentHash, err)
	}
	if fingerprint != record.CommitTree {
		return fmt.Errorf("%w: commit %s tree %s does not match prepared operation tree %s", ErrTxnConflict, currentHash, fingerprint, record.CommitTree)
	}
	currentConfig, err := store.ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		return err
	}
	if !bytes.Equal(currentConfig, expectedConfig) {
		return fmt.Errorf("%w: committed adopt transaction has unexpected config", ErrTxnConflict)
	}
	if err := st.ValidateSkillOwned(record.Name, *record.Payload); err != nil {
		return mapInstallOwnershipError("validate committed adopt content", err)
	}
	if err := validateInstallWorktreeChanges(st, record.Name, true, true); err != nil {
		return err
	}

	// Continue the interrupted agent-side switching. Whole-directory agents
	// are switched by the directory swap, never per entry: the per-entry
	// pass would archive and delete the original from the user's target
	// through the parent symlink (finding I1), the same exclusion the
	// normal path applies before its own switch.
	recordCopy := record
	var adoptAgents, adoptWholeDir []agent.Agent
	wholeDir := map[string]bool{}
	for _, n := range record.WholeDirAgents {
		wholeDir[n] = true
	}
	for _, name := range record.Agents {
		if wholeDir[name] {
			continue
		}
		a, ok := agent.ByName(name)
		if !ok {
			return fmt.Errorf("%w: adopt transaction names unknown agent %q", ErrTxnConflict, name)
		}
		adoptAgents = append(adoptAgents, a)
	}
	if recordCopy.Archive != nil {
		adoptAgents, err = recoveryOwnerFirst(adoptAgents, recordCopy.Archive.Agent, "archive")
		if err != nil {
			return err
		}
	}
	isolatedEntries, err := switchAdoptedEntriesReporting(st, adoptAgents, record.Name, &recordCopy, hooks{})
	for _, isolated := range isolatedEntries {
		report.Warnings = append(report.Warnings, adoptIsolationWarning(record.Name, isolated))
	}
	if err != nil {
		return fmt.Errorf("continue interrupted adopt switching: %w", err)
	}
	for _, name := range record.WholeDirAgents {
		a, ok := agent.ByName(name)
		if !ok {
			return fmt.Errorf("%w: adopt transaction names unknown whole-directory agent %q", ErrTxnConflict, name)
		}
		adoptWholeDir = append(adoptWholeDir, a)
	}
	if recordCopy.DirSwitch != nil {
		adoptWholeDir, err = recoveryOwnerFirst(adoptWholeDir, recordCopy.DirSwitch.Agent, "whole-directory switch")
		if err != nil {
			return err
		}
	}
	isolatedWholeDir, err := switchWholeDirAgentsReporting(st, adoptWholeDir, record.Name, &recordCopy, hooks{})
	for _, isolated := range isolatedWholeDir {
		report.Warnings = append(report.Warnings, adoptIsolationWarning(record.Name, isolated))
	}
	if err != nil {
		return fmt.Errorf("continue interrupted whole-directory switch: %w", err)
	}
	return ClearTxn(st, recordCopy)
}

func recoveryOwnerFirst(agents []agent.Agent, owner, operation string) ([]agent.Agent, error) {
	for index, candidate := range agents {
		if candidate.Name() != owner {
			continue
		}
		ordered := make([]agent.Agent, 0, len(agents))
		ordered = append(ordered, agents[index:]...)
		ordered = append(ordered, agents[:index]...)
		return ordered, nil
	}
	return nil, fmt.Errorf("%w: active %s owner %q is not in the recorded agent set", ErrTxnConflict, operation, owner)
}

// validateAdoptTargetPaths prevents an agent name from being rebound by a
// changed HOME between the original command and recovery. The recorded
// absolute path is authoritative; adapter lookup is used only to prove that
// the current process still resolves the same location.
func validateAdoptTargetPaths(targets []AdoptTarget) error {
	for _, target := range targets {
		if target.Agent == "" || !filepath.IsAbs(target.SkillsDir) {
			return fmt.Errorf("%w: adopt target has an invalid agent/path binding: %+v", ErrTxnConflict, target)
		}
		a, ok := agent.ByName(target.Agent)
		if !ok {
			return fmt.Errorf("%w: adopt transaction names unknown agent %q", ErrTxnConflict, target.Agent)
		}
		if filepath.Clean(a.SkillsDir()) != filepath.Clean(target.SkillsDir) {
			return fmt.Errorf("%w: adopt target for %s was recorded at %s but the current environment resolves it to %s",
				ErrTxnConflict, target.Agent, target.SkillsDir, a.SkillsDir())
		}
	}
	return nil
}
