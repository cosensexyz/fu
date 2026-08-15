package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

// ErrTxnConflict means recovery found state that is neither the transaction's
// starting snapshot nor a state the recorded operation could have produced.
var ErrTxnConflict = errors.New("pending transaction conflicts with current store state")

const (
	installTxnCompensating          = "compensating"
	installTxnCompensationReady     = "compensation-ready"
	installTxnCompensationCommitted = "compensation-committed"
)

type installRecoveryHook func(*store.Store, TxnRecord) error

// installRecoveryHooks exposes the durable compensation boundaries to subprocess
// tests. Production recovery always supplies the zero value.
type installRecoveryHooks struct {
	afterCompensationStarted installRecoveryHook
	afterConfigRestore       installRecoveryHook
	afterQuarantine          installRecoveryHook
	beforeQuarantineCleanup  installRecoveryHook
	afterCompensationCommit  installRecoveryHook
	beforeWALClear           installRecoveryHook
}

func (h installRecoveryHooks) fire(hook installRecoveryHook, st *store.Store, record TxnRecord) error {
	if hook == nil {
		return nil
	}
	return hook(st, record)
}

func init() {
	RegisterRecoverHandler("new", recoverInstallSkill)
	RegisterRecoverHandler("add", recoverInstallSkill)
}

func recoverInstallSkill(st *store.Store, record TxnRecord) error {
	return recoverInstallSkillWithHooks(st, record, installRecoveryHooks{})
}

func recoverInstallSkillWithHooks(st *store.Store, record TxnRecord, h installRecoveryHooks) error {
	if err := validateInstallTxn(record); err != nil {
		return err
	}
	storeRoot, err := st.StoreRoot()
	if err != nil {
		return fmt.Errorf("recover %s transaction through checked repository root: %w", record.Op, err)
	}
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return fmt.Errorf("recover %s transaction through checked skills root: %w", record.Op, err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return fmt.Errorf("recover %s transaction through checked staging root: %w", record.Op, err)
	}
	recoveryRoot, err := st.RecoveryRoot()
	if err != nil {
		return fmt.Errorf("recover %s transaction through checked recovery root: %w", record.Op, err)
	}
	startHash := plumbing.NewHash(record.StartHead)
	startCommit, err := st.Repo.CommitObject(startHash)
	if err != nil {
		return fmt.Errorf("load %s transaction start commit %s: %w", record.Op, record.StartHead, err)
	}
	startFile, err := startCommit.File("fu.yaml")
	if err != nil {
		return fmt.Errorf("load fu.yaml from transaction start commit: %w", err)
	}
	startConfig, err := startFile.Contents()
	if err != nil {
		return fmt.Errorf("read fu.yaml from transaction start commit: %w", err)
	}
	if !bytes.Equal([]byte(startConfig), record.ConfigBefore) {
		return fmt.Errorf("%w: recorded starting config does not match %s", ErrTxnConflict, record.StartHead)
	}

	expectedConfig, err := expectedInstallConfig(record)
	if err != nil {
		return err
	}
	currentHead, err := st.Repo.Head()
	if err != nil {
		return fmt.Errorf("read HEAD while recovering %s transaction: %w", record.Op, err)
	}
	if currentHead.Hash() == startHash {
		if isInstallCompensationStage(record.Stage) {
			return fmt.Errorf("%w: compensation stage %q cannot point at the transaction start commit", ErrTxnConflict, record.Stage)
		}
		return rollBackUncommittedInstall(st, storeRoot, skillsRoot, stagingRoot, recoveryRoot, record, expectedConfig)
	}
	return rollBackCommittedInstall(st, storeRoot, skillsRoot, stagingRoot, recoveryRoot,
		record, startHash, currentHead.Hash(), expectedConfig, h)
}

func isInstallCompensationStage(stage string) bool {
	switch stage {
	case installTxnCompensating, installTxnCompensationReady, installTxnCompensationCommitted:
		return true
	default:
		return false
	}
}

func validateInstallTxn(record TxnRecord) error {
	if record.Op != "new" && record.Op != "add" {
		return fmt.Errorf("install recovery received operation %q", record.Op)
	}
	if err := skill.ValidateName(record.Name); err != nil {
		return fmt.Errorf("invalid skill name in %s transaction: %w", record.Op, err)
	}
	if !plumbing.IsHash(record.StartHead) {
		return fmt.Errorf("%s transaction has invalid start HEAD %q", record.Op, record.StartHead)
	}
	if record.Message != record.Op+": "+record.Name {
		return fmt.Errorf("%s transaction message %q does not match skill %q", record.Op, record.Message, record.Name)
	}
	wantTargets := []string{
		filepath.Join("staging", record.Name),
		filepath.Join("store", "skills", record.Name),
	}
	if len(record.Targets) != len(wantTargets) || record.Targets[0] != wantTargets[0] || record.Targets[1] != wantTargets[1] {
		return fmt.Errorf("%s transaction targets %q do not match skill %q", record.Op, record.Targets, record.Name)
	}
	switch record.Stage {
	case "started", "prepared", "config-saved", "published",
		installTxnCompensating, installTxnCompensationReady, installTxnCompensationCommitted:
	default:
		return fmt.Errorf("%s transaction has unknown stage %q", record.Op, record.Stage)
	}
	if record.Stage != "started" && record.Digest == "" {
		return fmt.Errorf("%s transaction stage %q has no prepared-content digest", record.Op, record.Stage)
	}
	if record.Payload != nil {
		if err := record.Payload.Validate(); err != nil {
			return fmt.Errorf("%s transaction has invalid payload ownership manifest: %w", record.Op, err)
		}
	} else if record.Stage != "started" {
		return fmt.Errorf("%s transaction stage %q has no payload ownership manifest", record.Op, record.Stage)
	}
	if record.StagingReservation != nil {
		if err := record.StagingReservation.Validate(); err != nil {
			return fmt.Errorf("%s transaction has an invalid staging reservation: %w", record.Op, err)
		}
		if record.Payload != nil {
			return fmt.Errorf("%w: %s transaction records both a private staging reservation and a published payload", ErrTxnConflict, record.Op)
		}
	}
	// Declarations describe work not yet done, so they cannot outlive the stage
	// that does it: a later stage carrying one would mean the manifest and the
	// tree disagree about what has been created.
	if len(record.Declared) > 0 && record.Stage != "started" {
		return fmt.Errorf("%s transaction stage %q still declares uncreated entries", record.Op, record.Stage)
	}
	for _, declared := range record.Declared {
		if err := declared.Validate(); err != nil {
			return fmt.Errorf("%s transaction has an invalid declared entry: %w", record.Op, err)
		}
	}
	if len(record.ConfigBefore) == 0 {
		return fmt.Errorf("%s transaction has no starting config snapshot", record.Op)
	}
	return nil
}

func expectedInstallConfig(record TxnRecord) ([]byte, error) {
	if record.Digest == "" {
		return nil, nil
	}
	cfg, err := store.LoadConfigBytes(record.ConfigBefore, "fu.yaml at transaction start")
	if err != nil {
		return nil, fmt.Errorf("parse %s transaction starting config: %w", record.Op, err)
	}
	if cfg.HasSkill(record.Name) {
		return nil, fmt.Errorf("%w: skill %q already existed at transaction start", ErrTxnConflict, record.Name)
	}
	if err := cfg.AddSkill(record.Name, record.Digest); err != nil {
		return nil, fmt.Errorf("reconstruct %s transaction config: %w", record.Op, err)
	}
	// An add operation wrote a source record alongside the entry; the
	// reconstructed expected config must carry it or it cannot validate a
	// committed state.
	if len(record.SourceFields) > 0 {
		cfg.SetSourceFields(record.Name, record.SourceFields)
	}
	out, err := cfg.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode reconstructed %s transaction config: %w", record.Op, err)
	}
	return out, nil
}

func rollBackUncommittedInstall(st *store.Store, storeRoot, skillsRoot, stagingRoot, recoveryRoot *os.Root, record TxnRecord, expectedConfig []byte) error {
	const configPath = "fu.yaml"
	currentConfig, err := store.ReadConfigFileRoot(storeRoot, configPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(currentConfig, record.ConfigBefore) && (expectedConfig == nil || !bytes.Equal(currentConfig, expectedConfig)) {
		return fmt.Errorf("%w: fu.yaml is neither the starting nor expected %s-skill config", ErrTxnConflict, record.Op)
	}
	if record.StagingReservation != nil {
		reservation := *record.StagingReservation
		privatePresent, err := txnPathPresent(stagingRoot, reservation.Name)
		if err != nil {
			return err
		}
		finalPresent, err := txnPathPresent(stagingRoot, record.Name)
		if err != nil {
			return err
		}
		if privatePresent == finalPresent {
			return fmt.Errorf("%w: staged reservation is present at both or neither of its private and final names", ErrTxnConflict)
		}
		manifest := reservation.Manifest
		if privatePresent {
			manifest, err = st.PublishStagedRootOwned(reservation, record.Name)
		} else {
			err = st.ValidateStagedOwned(record.Name, manifest)
		}
		if err != nil {
			return mapInstallOwnershipError("recover staged-root reservation", err)
		}
		record.Payload = &manifest
		record.StagingReservation = nil
		if err := WriteTxn(st, &record); err != nil {
			return fmt.Errorf("record recovered staged-root reservation: %w", err)
		}
	}
	// Declarations are resolved once, against staging, before anything is
	// moved. They can only be outstanding at the "started" stage, where nothing
	// has been published or quarantined yet, and the settled manifest is written
	// back so every step below -- and every later recovery -- works from an
	// ordinary complete manifest.
	if len(record.Declared) > 0 {
		if record.Payload == nil {
			return fmt.Errorf("%w: %s transaction declares entries without a root manifest", ErrTxnConflict, record.Op)
		}
		settled, err := st.SettleDeclaredStagedEntries(record.Name, *record.Payload, record.Declared)
		if err != nil {
			return mapInstallOwnershipError("settle declared transaction entries", err)
		}
		record.Payload = &settled
		record.Declared = nil
		if err := WriteTxn(st, &record); err != nil {
			return fmt.Errorf("record settled transaction entries: %w", err)
		}
	}
	if record.Payload == nil {
		if !pathAbsent(skillsRoot, record.Name) {
			return fmt.Errorf("%w: transaction path exists without an ownership manifest", ErrTxnConflict)
		}
		if !pathAbsent(stagingRoot, record.Name) {
			return fmt.Errorf("%w: transaction path exists without an ownership manifest", ErrTxnConflict)
		}
		if err := restoreTxnConfig(st, currentConfig, record.ConfigBefore); err != nil {
			return err
		}
		return ClearTxn(st, record)
	}

	skillPresent, err := txnPathPresent(skillsRoot, record.Name)
	if err != nil {
		return err
	}
	stagedPresent, err := txnPathPresent(stagingRoot, record.Name)
	if err != nil {
		return err
	}
	payload := installUncommittedPayloadName(record)
	payloadPresent, err := txnPathPresent(recoveryRoot, payload)
	if err != nil {
		return err
	}
	if (skillPresent && stagedPresent) || (payloadPresent && (skillPresent || stagedPresent)) {
		return fmt.Errorf("%w: uncommitted %s transaction has content in more than one ownership location", ErrTxnConflict, record.Op)
	}
	if !payloadPresent {
		switch {
		case skillPresent:
			err = st.QuarantineSkillOwned(record.Name, payload, *record.Payload)
		case stagedPresent:
			err = st.QuarantineStagedOwned(record.Name, payload, *record.Payload)
		}
		if err != nil {
			return mapInstallOwnershipError("quarantine uncommitted transaction content", err)
		}
		payloadPresent = skillPresent || stagedPresent
	}
	if err := st.ArchiveRecoveryPayloadOwned(payload, *record.Payload); err != nil {
		return recoveryPayloadConflict(st, record, "archive uncommitted transaction content", payload,
			"the install was not committed", err)
	}
	if err := restoreTxnConfig(st, currentConfig, record.ConfigBefore); err != nil {
		return err
	}
	return ClearTxn(st, record)
}

// restoreTxnConfig puts the transaction's starting config back only while
// fu.yaml still holds the bytes recovery observed when it decided to. Reading,
// comparing and then replacing left the decision and the write separated by a
// window in which an external edit could arrive and be destroyed by the
// restore -- the same defect the operation's own install already closed.
func restoreTxnConfig(st *store.Store, observed, want []byte) error {
	if bytes.Equal(observed, want) {
		return nil
	}
	if err := st.InstallConfigExpecting(observed, want); err != nil {
		if errors.Is(err, store.ErrConfigChangedExternally) {
			return fmt.Errorf("%w: fu.yaml changed while the interrupted transaction was being rolled back", ErrTxnConflict)
		}
		return fmt.Errorf("restore config for interrupted transaction: %w", err)
	}
	return nil
}

type committedInstallRecovery struct {
	st              *store.Store
	storeRoot       *os.Root
	skillsRoot      *os.Root
	stagingRoot     *os.Root
	recoveryRoot    *os.Root
	record          TxnRecord
	startHash       plumbing.Hash
	expectedConfig  []byte
	recoveryMessage string
	hooks           installRecoveryHooks
}

func rollBackCommittedInstall(
	st *store.Store,
	storeRoot, skillsRoot, stagingRoot, recoveryRoot *os.Root,
	record TxnRecord,
	startHash, currentHash plumbing.Hash,
	expectedConfig []byte,
	h installRecoveryHooks,
) error {
	recovery := &committedInstallRecovery{
		st:              st,
		storeRoot:       storeRoot,
		skillsRoot:      skillsRoot,
		stagingRoot:     stagingRoot,
		recoveryRoot:    recoveryRoot,
		record:          record,
		startHash:       startHash,
		expectedConfig:  expectedConfig,
		recoveryMessage: "recover: roll back interrupted " + record.Message,
		hooks:           h,
	}
	return recovery.run(currentHash)
}

func (r *committedInstallRecovery) run(currentHash plumbing.Hash) error {
	currentCommit, err := r.st.Repo.CommitObject(currentHash)
	if err != nil {
		return err
	}
	if currentCommit.Message == r.recoveryMessage {
		if err := r.validateRecoveryCommit(currentCommit); err != nil {
			return err
		}
		return r.finish()
	}
	if err := r.validateOperationCommit(currentCommit); err != nil {
		return err
	}

	switch r.record.Stage {
	case installTxnCompensationReady:
		return r.commit()
	case installTxnCompensating:
		return r.resume()
	case installTxnCompensationCommitted:
		return fmt.Errorf("%w: WAL says compensation committed while HEAD still names the operation commit", ErrTxnConflict)
	}

	if err := r.validateInitialState(); err != nil {
		return err
	}
	r.record.Stage = installTxnCompensating
	if err := WriteTxn(r.st, &r.record); err != nil {
		return fmt.Errorf("record interrupted-%s compensation start: %w", r.record.Op, err)
	}
	if err := r.hooks.fire(r.hooks.afterCompensationStarted, r.st, r.record); err != nil {
		return err
	}
	return r.resume()
}

func (r *committedInstallRecovery) validateOperationCommit(commit *object.Commit) error {
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0] != r.startHash || commit.Message != r.record.Message {
		return fmt.Errorf("%w: commit %s is not the recorded %s-skill commit based on %s", ErrTxnConflict, commit.Hash, r.record.Op, r.startHash)
	}
	if r.record.CommitTree == "" {
		return fmt.Errorf("%w: committed %s transaction has no prepared operation-tree fingerprint", ErrTxnConflict, r.record.Op)
	}
	fingerprint, err := r.st.CommitTreeFingerprint(commit.Hash)
	if err != nil {
		return fmt.Errorf("fingerprint recorded %s-skill commit %s: %w", r.record.Op, commit.Hash, err)
	}
	if fingerprint != r.record.CommitTree {
		return fmt.Errorf("%w: commit %s tree %s does not match prepared operation tree %s", ErrTxnConflict, commit.Hash, fingerprint, r.record.CommitTree)
	}
	return nil
}

func (r *committedInstallRecovery) validateRecoveryCommit(commit *object.Commit) error {
	if len(commit.ParentHashes) != 1 {
		return fmt.Errorf("%w: recovery commit %s does not have exactly one parent", ErrTxnConflict, commit.Hash)
	}
	operationCommit, err := r.st.Repo.CommitObject(commit.ParentHashes[0])
	if err != nil {
		return fmt.Errorf("load interrupted %s-skill commit behind recovery commit: %w", r.record.Op, err)
	}
	if err := r.validateOperationCommit(operationCommit); err != nil {
		return err
	}
	if r.record.CompensationTree == "" {
		return fmt.Errorf("%w: recovery commit %s has no prepared compensation-tree fingerprint", ErrTxnConflict, commit.Hash)
	}
	fingerprint, err := r.st.CommitTreeFingerprint(commit.Hash)
	if err != nil {
		return fmt.Errorf("fingerprint interrupted-%s recovery commit %s: %w", r.record.Op, commit.Hash, err)
	}
	if fingerprint != r.record.CompensationTree {
		return fmt.Errorf("%w: recovery commit %s tree %s does not match prepared compensation tree %s", ErrTxnConflict, commit.Hash, fingerprint, r.record.CompensationTree)
	}
	return nil
}

func (r *committedInstallRecovery) validateInitialState() error {
	configExpected, configBefore, err := r.configState()
	if err != nil {
		return err
	}
	if !configExpected && !configBefore {
		return fmt.Errorf("%w: committed %s transaction has unexpected config", ErrTxnConflict, r.record.Op)
	}
	present, err := txnPathPresent(r.skillsRoot, r.record.Name)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: committed %s transaction has no published content", ErrTxnConflict, r.record.Op)
	}
	if err := r.st.ValidateSkillOwned(r.record.Name, *r.record.Payload); err != nil {
		return mapInstallOwnershipError("validate committed transaction content", err)
	}
	if err := requireTxnPathAbsent(r.stagingRoot, r.record.Name, "staging content"); err != nil {
		return err
	}
	if err := requireTxnPathAbsent(r.recoveryRoot, installCompensationPayloadName(r.record), "compensation payload"); err != nil {
		return err
	}
	return validateInstallWorktreeChanges(r.st, r.record.Name, configBefore, false)
}

func (r *committedInstallRecovery) resume() error {
	configExpected, configBefore, err := r.configState()
	if err != nil {
		return err
	}
	skillPresent, err := txnPathPresent(r.skillsRoot, r.record.Name)
	if err != nil {
		return err
	}
	payload := installCompensationPayloadName(r.record)
	payloadPresent, err := txnPathPresent(r.recoveryRoot, payload)
	if err != nil {
		return err
	}
	if err := requireTxnPathAbsent(r.stagingRoot, r.record.Name, "staging content"); err != nil {
		return err
	}

	validState := (configExpected || configBefore) && (skillPresent != payloadPresent)
	if !validState {
		return fmt.Errorf("%w: compensation state does not match a durable pre-state or post-state", ErrTxnConflict)
	}
	if skillPresent {
		if err := r.st.ValidateSkillOwned(r.record.Name, *r.record.Payload); err != nil {
			return mapInstallOwnershipError("validate published content before compensation", err)
		}
	} else if err := r.st.ValidateRecoveryPayloadOwned(payload, *r.record.Payload); err != nil {
		return mapInstallOwnershipError("validate quarantined compensation content", err)
	}
	if err := validateInstallWorktreeChanges(r.st, r.record.Name, configBefore, !skillPresent); err != nil {
		return err
	}

	if skillPresent {
		if err := r.st.QuarantineSkillOwned(r.record.Name, payload, *r.record.Payload); err != nil {
			return mapInstallOwnershipError("quarantine published content for committed "+r.record.Op+" transaction", err)
		}
	}
	if err := r.hooks.fire(r.hooks.afterQuarantine, r.st, r.record); err != nil {
		return err
	}
	if !configBefore {
		if err := restoreTxnConfig(r.st, r.expectedConfig, r.record.ConfigBefore); err != nil {
			return err
		}
	}
	if err := r.hooks.fire(r.hooks.afterConfigRestore, r.st, r.record); err != nil {
		return err
	}

	r.record.Stage = installTxnCompensationReady
	if err := WriteTxn(r.st, &r.record); err != nil {
		return fmt.Errorf("record interrupted-%s compensation worktree: %w", r.record.Op, err)
	}
	return r.commit()
}

func (r *committedInstallRecovery) commit() error {
	configExpected, configBefore, err := r.configState()
	if err != nil {
		return err
	}
	if configExpected || !configBefore {
		return fmt.Errorf("%w: compensation-ready transaction has unexpected config", ErrTxnConflict)
	}
	if err := requireTxnPathAbsent(r.skillsRoot, r.record.Name, "published content"); err != nil {
		return err
	}
	if err := requireTxnPathAbsent(r.stagingRoot, r.record.Name, "staging content"); err != nil {
		return err
	}
	payload := installCompensationPayloadName(r.record)
	payloadPresent, err := txnPathPresent(r.recoveryRoot, payload)
	if err != nil {
		return err
	}
	if !payloadPresent {
		return fmt.Errorf("%w: compensation-ready transaction has no quarantine payload", ErrTxnConflict)
	}
	if err := r.st.ValidateRecoveryPayloadOwned(payload, *r.record.Payload); err != nil {
		return mapInstallOwnershipError("validate compensation payload before commit", err)
	}
	if err := validateInstallWorktreeChanges(r.st, r.record.Name, true, true); err != nil {
		return err
	}

	prepared, err := r.st.PrepareCommit()
	if err != nil {
		return fmt.Errorf("prepare interrupted-%s compensation commit: %w", r.record.Op, err)
	}
	compensationOp := Op{
		AllowedChanges: []string{"fu.yaml", filepath.ToSlash(filepath.Join("skills", r.record.Name))},
		ValidatePrepared: func(st *store.Store, prepared store.PreparedCommit) error {
			return st.ValidatePreparedPathAbsent(prepared, filepath.ToSlash(filepath.Join("skills", r.record.Name)))
		},
	}
	// Every exit before publication abandons the immutable candidate through one
	// boundary. Candidate staging is private, so a direct Git process never sees
	// the compensation while validation and WAL updates are still in progress.
	abandon := func(cause error) error {
		if err := r.st.RestorePreparedIndex(prepared); err != nil {
			return errors.Join(cause, fmt.Errorf("abandon the prepared Git candidate: %w", err))
		}
		return cause
	}
	if err := validatePreparedOperation(r.st, compensationOp, prepared, r.record.ConfigBefore); err != nil {
		return abandon(fmt.Errorf("validate interrupted-%s compensation commit: %w", r.record.Op, err))
	}
	r.record.CompensationTree = prepared.TreeFingerprint()
	if err := WriteTxn(r.st, &r.record); err != nil {
		return abandon(fmt.Errorf("record prepared interrupted-%s compensation tree: %w", r.record.Op, err))
	}
	outcome, err := r.st.CommitPrepared(r.recoveryMessage, prepared)
	if err != nil {
		if outcome.Written {
			return fmt.Errorf("commit interrupted-%s compensation (commit %s was written): %w", r.record.Op, outcome.Hash, err)
		}
		return abandon(fmt.Errorf("commit interrupted-%s compensation: %w", r.record.Op, err))
	}
	if !outcome.Written {
		return fmt.Errorf("%w: compensation worktree unexpectedly produced no commit", ErrTxnConflict)
	}
	if err := r.hooks.fire(r.hooks.afterCompensationCommit, r.st, r.record); err != nil {
		return err
	}
	r.record.Stage = installTxnCompensationCommitted
	if err := WriteTxn(r.st, &r.record); err != nil {
		return fmt.Errorf("record interrupted-%s compensation commit: %w", r.record.Op, err)
	}
	return r.finish()
}

func (r *committedInstallRecovery) finish() error {
	if !bytesEqualConfigRoot(r.storeRoot, "fu.yaml", r.record.ConfigBefore) ||
		!pathAbsent(r.stagingRoot, r.record.Name) || !pathAbsent(r.skillsRoot, r.record.Name) {
		return fmt.Errorf("%w: recovery commit exists but its worktree state changed", ErrTxnConflict)
	}
	if err := validateInstallWorktreeChanges(r.st, r.record.Name, false, false); err != nil {
		return err
	}

	switch r.record.Stage {
	case "started", "prepared", "config-saved", "published":
		if err := requireTxnPathAbsent(r.recoveryRoot, installCompensationPayloadName(r.record), "compensation payload"); err != nil {
			return err
		}
		if err := r.hooks.fire(r.hooks.beforeWALClear, r.st, r.record); err != nil {
			return err
		}
		return ClearTxn(r.st, r.record)
	case installTxnCompensationReady:
		r.record.Stage = installTxnCompensationCommitted
		if err := WriteTxn(r.st, &r.record); err != nil {
			return fmt.Errorf("record detected interrupted-%s compensation commit: %w", r.record.Op, err)
		}
	case installTxnCompensationCommitted:
	default:
		return fmt.Errorf("%w: recovery commit is paired with transaction stage %q", ErrTxnConflict, r.record.Stage)
	}

	if err := r.hooks.fire(r.hooks.beforeQuarantineCleanup, r.st, r.record); err != nil {
		return err
	}
	if err := r.st.ArchiveRecoveryPayloadOwned(installCompensationPayloadName(r.record), *r.record.Payload); err != nil {
		return recoveryPayloadConflict(r.st, r.record, "archive committed-"+r.record.Op+" compensation payload",
			installCompensationPayloadName(r.record), "the compensation commit is already durable", err)
	}
	if err := r.hooks.fire(r.hooks.beforeWALClear, r.st, r.record); err != nil {
		return err
	}
	return ClearTxn(r.st, r.record)
}

func (r *committedInstallRecovery) configState() (expected, before bool, err error) {
	current, err := store.ReadConfigFileRoot(r.storeRoot, "fu.yaml")
	if err != nil {
		return false, false, err
	}
	return bytes.Equal(current, r.expectedConfig), bytes.Equal(current, r.record.ConfigBefore), nil
}

func installCompensationPayloadName(record TxnRecord) string {
	return "rollback-" + record.Op + "-" + record.Name + "-" + record.StartHead
}

func installUncommittedPayloadName(record TxnRecord) string {
	return installCompensationPayloadName(record) + "-uncommitted"
}

func mapInstallOwnershipError(action string, err error) error {
	if errors.Is(err, store.ErrOwnedTreeChanged) || errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s: %w", ErrTxnConflict, action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func recoveryPayloadConflict(st *store.Store, record TxnRecord, action, payload, state string, err error) error {
	conflict := mapInstallOwnershipError(action, err)
	payloadPath := filepath.Join(st.RecoveryDir(), payload)
	return addRecoveryConflictRemedy(st, record, fmt.Errorf("%w; %s; recorded recovery payload: %s", conflict, state, payloadPath))
}

func txnPathPresent(root *os.Root, name string) (bool, error) {
	_, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func requireTxnPathAbsent(root *os.Root, name, what string) error {
	_, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: committed transaction still has %s at %s", ErrTxnConflict, what, name)
}

func validateInstallWorktreeChanges(st *store.Store, name string, allowConfig, allowSkill bool) error {
	changed, err := st.ChangedPathsIncludingIgnored()
	if err != nil {
		return err
	}
	skillPath := filepath.ToSlash(filepath.Join("skills", name))
	for _, path := range changed {
		if allowConfig && path == "fu.yaml" {
			continue
		}
		if allowSkill && (path == skillPath || strings.HasPrefix(path, skillPath+"/")) {
			continue
		}
		return fmt.Errorf("%w: store changed outside the interrupted transaction at %s", ErrTxnConflict, path)
	}
	return nil
}

func bytesEqualConfigRoot(root *os.Root, name string, want []byte) bool {
	got, err := store.ReadConfigFileRoot(root, name)
	return err == nil && bytes.Equal(got, want)
}

func pathAbsent(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return errors.Is(err, fs.ErrNotExist)
}
