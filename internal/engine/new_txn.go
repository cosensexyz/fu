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

	"fu/internal/skill"
	"fu/internal/store"
)

// ErrTxnConflict means recovery found state that is neither the transaction's
// starting snapshot nor a state the recorded operation could have produced.
var ErrTxnConflict = errors.New("pending transaction conflicts with current store state")

const (
	newTxnCompensating          = "compensating"
	newTxnCompensationReady     = "compensation-ready"
	newTxnCompensationCommitted = "compensation-committed"
)

type newRecoveryHook func(*store.Store, TxnRecord) error

// newRecoveryHooks exposes the durable compensation boundaries to subprocess
// tests. Production recovery always supplies the zero value.
type newRecoveryHooks struct {
	afterCompensationStarted newRecoveryHook
	afterConfigRestore       newRecoveryHook
	afterQuarantine          newRecoveryHook
	beforeQuarantineCleanup  newRecoveryHook
	afterCompensationCommit  newRecoveryHook
	beforeWALClear           newRecoveryHook
}

func (h newRecoveryHooks) fire(hook newRecoveryHook, st *store.Store, record TxnRecord) error {
	if hook == nil {
		return nil
	}
	return hook(st, record)
}

func init() {
	RegisterRecoverHandler("new", recoverNewSkill)
}

func recoverNewSkill(st *store.Store, record TxnRecord) error {
	return recoverNewSkillWithHooks(st, record, newRecoveryHooks{})
}

func recoverNewSkillWithHooks(st *store.Store, record TxnRecord, h newRecoveryHooks) error {
	if err := validateNewTxn(record); err != nil {
		return err
	}
	storeRoot, err := st.StoreRoot()
	if err != nil {
		return fmt.Errorf("recover new transaction through checked repository root: %w", err)
	}
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return fmt.Errorf("recover new transaction through checked skills root: %w", err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return fmt.Errorf("recover new transaction through checked staging root: %w", err)
	}
	recoveryRoot, err := st.RecoveryRoot()
	if err != nil {
		return fmt.Errorf("recover new transaction through checked recovery root: %w", err)
	}
	startHash := plumbing.NewHash(record.StartHead)
	startCommit, err := st.Repo.CommitObject(startHash)
	if err != nil {
		return fmt.Errorf("load new transaction start commit %s: %w", record.StartHead, err)
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

	expectedConfig, err := expectedNewConfig(record)
	if err != nil {
		return err
	}
	currentHead, err := st.Repo.Head()
	if err != nil {
		return fmt.Errorf("read HEAD while recovering new transaction: %w", err)
	}
	if currentHead.Hash() == startHash {
		if isNewCompensationStage(record.Stage) {
			return fmt.Errorf("%w: compensation stage %q cannot point at the transaction start commit", ErrTxnConflict, record.Stage)
		}
		return rollBackUncommittedNew(st, storeRoot, skillsRoot, stagingRoot, recoveryRoot, record, expectedConfig)
	}
	return rollBackCommittedNew(st, storeRoot, skillsRoot, stagingRoot, recoveryRoot,
		record, startHash, currentHead.Hash(), expectedConfig, h)
}

func isNewCompensationStage(stage string) bool {
	switch stage {
	case newTxnCompensating, newTxnCompensationReady, newTxnCompensationCommitted:
		return true
	default:
		return false
	}
}

func validateNewTxn(record TxnRecord) error {
	if record.Op != "new" {
		return fmt.Errorf("new recovery received operation %q", record.Op)
	}
	if err := skill.ValidateName(record.Name); err != nil {
		return fmt.Errorf("invalid skill name in new transaction: %w", err)
	}
	if !plumbing.IsHash(record.StartHead) {
		return fmt.Errorf("new transaction has invalid start HEAD %q", record.StartHead)
	}
	if record.Message != "new: "+record.Name {
		return fmt.Errorf("new transaction message %q does not match skill %q", record.Message, record.Name)
	}
	wantTargets := []string{
		filepath.Join("staging", record.Name),
		filepath.Join("store", "skills", record.Name),
	}
	if len(record.Targets) != len(wantTargets) || record.Targets[0] != wantTargets[0] || record.Targets[1] != wantTargets[1] {
		return fmt.Errorf("new transaction targets %q do not match skill %q", record.Targets, record.Name)
	}
	switch record.Stage {
	case "started", "prepared", "config-saved", "published",
		newTxnCompensating, newTxnCompensationReady, newTxnCompensationCommitted:
	default:
		return fmt.Errorf("new transaction has unknown stage %q", record.Stage)
	}
	if record.Stage != "started" && record.Digest == "" {
		return fmt.Errorf("new transaction stage %q has no prepared-content digest", record.Stage)
	}
	if record.Payload != nil {
		if err := record.Payload.Validate(); err != nil {
			return fmt.Errorf("new transaction has invalid payload ownership manifest: %w", err)
		}
	} else if record.Stage != "started" {
		return fmt.Errorf("new transaction stage %q has no payload ownership manifest", record.Stage)
	}
	// Declarations describe work not yet done, so they cannot outlive the stage
	// that does it: a later stage carrying one would mean the manifest and the
	// tree disagree about what has been created.
	if len(record.Declared) > 0 && record.Stage != "started" {
		return fmt.Errorf("new transaction stage %q still declares uncreated entries", record.Stage)
	}
	for _, declared := range record.Declared {
		if err := declared.Validate(); err != nil {
			return fmt.Errorf("new transaction has an invalid declared entry: %w", err)
		}
	}
	if len(record.ConfigBefore) == 0 {
		return errors.New("new transaction has no starting config snapshot")
	}
	return nil
}

func expectedNewConfig(record TxnRecord) ([]byte, error) {
	if record.Digest == "" {
		return nil, nil
	}
	cfg, err := store.LoadConfigBytes(record.ConfigBefore, "fu.yaml at transaction start")
	if err != nil {
		return nil, fmt.Errorf("parse new transaction starting config: %w", err)
	}
	if cfg.HasSkill(record.Name) {
		return nil, fmt.Errorf("%w: skill %q already existed at transaction start", ErrTxnConflict, record.Name)
	}
	if err := cfg.AddSkill(record.Name, record.Digest); err != nil {
		return nil, fmt.Errorf("reconstruct new transaction config: %w", err)
	}
	out, err := cfg.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode reconstructed new transaction config: %w", err)
	}
	return out, nil
}

func rollBackUncommittedNew(st *store.Store, storeRoot, skillsRoot, stagingRoot, recoveryRoot *os.Root, record TxnRecord, expectedConfig []byte) error {
	const configPath = "fu.yaml"
	currentConfig, err := store.ReadConfigFileRoot(storeRoot, configPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(currentConfig, record.ConfigBefore) && (expectedConfig == nil || !bytes.Equal(currentConfig, expectedConfig)) {
		return fmt.Errorf("%w: fu.yaml is neither the starting nor expected new-skill config", ErrTxnConflict)
	}
	// Declarations are resolved once, against staging, before anything is
	// moved. They can only be outstanding at the "started" stage, where nothing
	// has been published or quarantined yet, and the settled manifest is written
	// back so every step below -- and every later recovery -- works from an
	// ordinary complete manifest.
	if len(record.Declared) > 0 {
		if record.Payload == nil {
			return fmt.Errorf("%w: new transaction declares entries without a root manifest", ErrTxnConflict)
		}
		settled, err := st.SettleDeclaredStagedEntries(record.Name, *record.Payload, record.Declared)
		if err != nil {
			return mapNewOwnershipError("settle declared transaction entries", err)
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
		// A crash between the exclusive create and the WriteTxn that records it
		// leaves exactly one shape: an empty staged root. Reclaiming it by rmdir
		// proves nothing was written into it, which is the same argument
		// reclaimAbandonedStagedRoots already relies on. A replacement carrying
		// content returns ENOTEMPTY and is still preserved and reported.
		if !pathAbsent(stagingRoot, record.Name) {
			reclaimed, err := st.ReclaimEmptyStagedRoot(record.Name)
			if err != nil {
				return err
			}
			if !reclaimed {
				return fmt.Errorf("%w: transaction path exists without an ownership manifest", ErrTxnConflict)
			}
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
	payload := newUncommittedPayloadName(record)
	payloadPresent, err := txnPathPresent(recoveryRoot, payload)
	if err != nil {
		return err
	}
	if (skillPresent && stagedPresent) || (payloadPresent && (skillPresent || stagedPresent)) {
		return fmt.Errorf("%w: uncommitted new transaction has content in more than one ownership location", ErrTxnConflict)
	}
	if !payloadPresent {
		switch {
		case skillPresent:
			err = st.QuarantineSkillOwned(record.Name, payload, *record.Payload)
		case stagedPresent:
			err = st.QuarantineStagedOwned(record.Name, payload, *record.Payload)
		}
		if err != nil {
			return mapNewOwnershipError("quarantine uncommitted transaction content", err)
		}
		payloadPresent = skillPresent || stagedPresent
	}
	if err := st.ArchiveRecoveryPayloadOwned(payload, *record.Payload); err != nil {
		return mapNewOwnershipError("archive uncommitted transaction content", err)
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
			return fmt.Errorf("%w: fu.yaml changed while the interrupted new transaction was being rolled back", ErrTxnConflict)
		}
		return fmt.Errorf("restore config for interrupted new transaction: %w", err)
	}
	return nil
}

type committedNewRecovery struct {
	st              *store.Store
	storeRoot       *os.Root
	skillsRoot      *os.Root
	stagingRoot     *os.Root
	recoveryRoot    *os.Root
	record          TxnRecord
	startHash       plumbing.Hash
	expectedConfig  []byte
	recoveryMessage string
	hooks           newRecoveryHooks
}

func rollBackCommittedNew(
	st *store.Store,
	storeRoot, skillsRoot, stagingRoot, recoveryRoot *os.Root,
	record TxnRecord,
	startHash, currentHash plumbing.Hash,
	expectedConfig []byte,
	h newRecoveryHooks,
) error {
	recovery := &committedNewRecovery{
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

func (r *committedNewRecovery) run(currentHash plumbing.Hash) error {
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
	case newTxnCompensationReady:
		return r.commit()
	case newTxnCompensating:
		return r.resume()
	case newTxnCompensationCommitted:
		return fmt.Errorf("%w: WAL says compensation committed while HEAD still names the operation commit", ErrTxnConflict)
	}

	if err := r.validateInitialState(); err != nil {
		return err
	}
	r.record.Stage = newTxnCompensating
	if err := WriteTxn(r.st, &r.record); err != nil {
		return fmt.Errorf("record interrupted-new compensation start: %w", err)
	}
	if err := r.hooks.fire(r.hooks.afterCompensationStarted, r.st, r.record); err != nil {
		return err
	}
	return r.resume()
}

func (r *committedNewRecovery) validateOperationCommit(commit *object.Commit) error {
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0] != r.startHash || commit.Message != r.record.Message {
		return fmt.Errorf("%w: commit %s is not the recorded new-skill commit based on %s", ErrTxnConflict, commit.Hash, r.startHash)
	}
	if r.record.CommitTree == "" {
		return fmt.Errorf("%w: committed new transaction has no prepared operation-tree fingerprint", ErrTxnConflict)
	}
	fingerprint, err := r.st.CommitTreeFingerprint(commit.Hash)
	if err != nil {
		return fmt.Errorf("fingerprint recorded new-skill commit %s: %w", commit.Hash, err)
	}
	if fingerprint != r.record.CommitTree {
		return fmt.Errorf("%w: commit %s tree %s does not match prepared operation tree %s", ErrTxnConflict, commit.Hash, fingerprint, r.record.CommitTree)
	}
	return nil
}

func (r *committedNewRecovery) validateRecoveryCommit(commit *object.Commit) error {
	if len(commit.ParentHashes) != 1 {
		return fmt.Errorf("%w: recovery commit %s does not have exactly one parent", ErrTxnConflict, commit.Hash)
	}
	operationCommit, err := r.st.Repo.CommitObject(commit.ParentHashes[0])
	if err != nil {
		return fmt.Errorf("load interrupted new-skill commit behind recovery commit: %w", err)
	}
	if err := r.validateOperationCommit(operationCommit); err != nil {
		return err
	}
	if r.record.CompensationTree == "" {
		return fmt.Errorf("%w: recovery commit %s has no prepared compensation-tree fingerprint", ErrTxnConflict, commit.Hash)
	}
	fingerprint, err := r.st.CommitTreeFingerprint(commit.Hash)
	if err != nil {
		return fmt.Errorf("fingerprint interrupted-new recovery commit %s: %w", commit.Hash, err)
	}
	if fingerprint != r.record.CompensationTree {
		return fmt.Errorf("%w: recovery commit %s tree %s does not match prepared compensation tree %s", ErrTxnConflict, commit.Hash, fingerprint, r.record.CompensationTree)
	}
	return nil
}

func (r *committedNewRecovery) validateInitialState() error {
	configExpected, configBefore, err := r.configState()
	if err != nil {
		return err
	}
	if !configExpected && !configBefore {
		return fmt.Errorf("%w: committed new transaction has unexpected config", ErrTxnConflict)
	}
	present, err := txnPathPresent(r.skillsRoot, r.record.Name)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: committed new transaction has no published content", ErrTxnConflict)
	}
	if err := r.st.ValidateSkillOwned(r.record.Name, *r.record.Payload); err != nil {
		return mapNewOwnershipError("validate committed transaction content", err)
	}
	if err := requireTxnPathAbsent(r.stagingRoot, r.record.Name, "staging content"); err != nil {
		return err
	}
	if err := requireTxnPathAbsent(r.recoveryRoot, newCompensationPayloadName(r.record), "compensation payload"); err != nil {
		return err
	}
	return validateNewWorktreeChanges(r.st, r.record.Name, configBefore, false)
}

func (r *committedNewRecovery) resume() error {
	configExpected, configBefore, err := r.configState()
	if err != nil {
		return err
	}
	skillPresent, err := txnPathPresent(r.skillsRoot, r.record.Name)
	if err != nil {
		return err
	}
	payload := newCompensationPayloadName(r.record)
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
			return mapNewOwnershipError("validate published content before compensation", err)
		}
	} else if err := r.st.ValidateRecoveryPayloadOwned(payload, *r.record.Payload); err != nil {
		return mapNewOwnershipError("validate quarantined compensation content", err)
	}
	if err := validateNewWorktreeChanges(r.st, r.record.Name, configBefore, !skillPresent); err != nil {
		return err
	}

	if skillPresent {
		if err := r.st.QuarantineSkillOwned(r.record.Name, payload, *r.record.Payload); err != nil {
			return mapNewOwnershipError("quarantine published content for committed new transaction", err)
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

	r.record.Stage = newTxnCompensationReady
	if err := WriteTxn(r.st, &r.record); err != nil {
		return fmt.Errorf("record interrupted-new compensation worktree: %w", err)
	}
	return r.commit()
}

func (r *committedNewRecovery) commit() error {
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
	payload := newCompensationPayloadName(r.record)
	payloadPresent, err := txnPathPresent(r.recoveryRoot, payload)
	if err != nil {
		return err
	}
	if !payloadPresent {
		return fmt.Errorf("%w: compensation-ready transaction has no quarantine payload", ErrTxnConflict)
	}
	if err := r.st.ValidateRecoveryPayloadOwned(payload, *r.record.Payload); err != nil {
		return mapNewOwnershipError("validate compensation payload before commit", err)
	}
	if err := validateNewWorktreeChanges(r.st, r.record.Name, true, true); err != nil {
		return err
	}

	prepared, err := r.st.PrepareCommit()
	if err != nil {
		return fmt.Errorf("prepare interrupted-new compensation commit: %w", err)
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
		return abandon(fmt.Errorf("validate interrupted-new compensation commit: %w", err))
	}
	r.record.CompensationTree = prepared.TreeFingerprint()
	if err := WriteTxn(r.st, &r.record); err != nil {
		return abandon(fmt.Errorf("record prepared interrupted-new compensation tree: %w", err))
	}
	outcome, err := r.st.CommitPrepared(r.recoveryMessage, prepared)
	if err != nil {
		if outcome.Written {
			return fmt.Errorf("commit interrupted-new compensation (commit %s was written): %w", outcome.Hash, err)
		}
		return abandon(fmt.Errorf("commit interrupted-new compensation: %w", err))
	}
	if !outcome.Written {
		return fmt.Errorf("%w: compensation worktree unexpectedly produced no commit", ErrTxnConflict)
	}
	if err := r.hooks.fire(r.hooks.afterCompensationCommit, r.st, r.record); err != nil {
		return err
	}
	r.record.Stage = newTxnCompensationCommitted
	if err := WriteTxn(r.st, &r.record); err != nil {
		return fmt.Errorf("record interrupted-new compensation commit: %w", err)
	}
	return r.finish()
}

func (r *committedNewRecovery) finish() error {
	if !bytesEqualConfigRoot(r.storeRoot, "fu.yaml", r.record.ConfigBefore) ||
		!pathAbsent(r.stagingRoot, r.record.Name) || !pathAbsent(r.skillsRoot, r.record.Name) {
		return fmt.Errorf("%w: recovery commit exists but its worktree state changed", ErrTxnConflict)
	}
	if err := validateNewWorktreeChanges(r.st, r.record.Name, false, false); err != nil {
		return err
	}

	switch r.record.Stage {
	case "started", "prepared", "config-saved", "published":
		if err := requireTxnPathAbsent(r.recoveryRoot, newCompensationPayloadName(r.record), "compensation payload"); err != nil {
			return err
		}
		if err := r.hooks.fire(r.hooks.beforeWALClear, r.st, r.record); err != nil {
			return err
		}
		return ClearTxn(r.st, r.record)
	case newTxnCompensationReady:
		r.record.Stage = newTxnCompensationCommitted
		if err := WriteTxn(r.st, &r.record); err != nil {
			return fmt.Errorf("record detected interrupted-new compensation commit: %w", err)
		}
	case newTxnCompensationCommitted:
	default:
		return fmt.Errorf("%w: recovery commit is paired with transaction stage %q", ErrTxnConflict, r.record.Stage)
	}

	if err := r.hooks.fire(r.hooks.beforeQuarantineCleanup, r.st, r.record); err != nil {
		return err
	}
	if err := r.st.ArchiveRecoveryPayloadOwned(newCompensationPayloadName(r.record), *r.record.Payload); err != nil {
		if errors.Is(err, store.ErrOwnedTreeChanged) {
			return fmt.Errorf("%w: compensation archive changed after it was quarantined: %v", ErrTxnConflict, err)
		}
		return fmt.Errorf("archive committed-new compensation payload: %w", err)
	}
	if err := r.hooks.fire(r.hooks.beforeWALClear, r.st, r.record); err != nil {
		return err
	}
	return ClearTxn(r.st, r.record)
}

func (r *committedNewRecovery) configState() (expected, before bool, err error) {
	current, err := store.ReadConfigFileRoot(r.storeRoot, "fu.yaml")
	if err != nil {
		return false, false, err
	}
	return bytes.Equal(current, r.expectedConfig), bytes.Equal(current, r.record.ConfigBefore), nil
}

func newCompensationPayloadName(record TxnRecord) string {
	hash := record.StartHead
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return "rollback-new-" + record.Name + "-" + hash
}

func newUncommittedPayloadName(record TxnRecord) string {
	return newCompensationPayloadName(record) + "-uncommitted"
}

func mapNewOwnershipError(action string, err error) error {
	if errors.Is(err, store.ErrOwnedTreeChanged) || errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s: %w", ErrTxnConflict, action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
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
	return fmt.Errorf("%w: committed new transaction still has %s at %s", ErrTxnConflict, what, name)
}

func validateNewWorktreeChanges(st *store.Store, name string, allowConfig, allowSkill bool) error {
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
		return fmt.Errorf("%w: store changed outside the interrupted new transaction at %s", ErrTxnConflict, path)
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
