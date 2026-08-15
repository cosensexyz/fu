// internal/engine/rm_txn.go
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

const (
	rmTxnSnapshotted = "snapshotted"
	rmTxnQuarantined = "quarantined"
)

func init() {
	RegisterRecoverHandler("rm", recoverRemoveSkill)
}

// removeRecoveryHooks exposes the rm recovery's durable boundaries to
// subprocess tests (round 8 finding C1). Production recovery always
// supplies the zero value.
type removeRecoveryHooks struct {
	// beforeWALClear fires after the committed payload was archived and
	// immediately before the WAL is cleared: the state a crash here leaves
	// behind -- payload at the archive name, WAL still open -- is exactly
	// what recovery must be idempotent over.
	beforeWALClear func(*store.Store, TxnRecord) error
}

// recoverRemoveSkill drives an interrupted rm to a terminal state: completed
// (the operation commit exists, validated, payload archived) or rolled back
// (content returned to the skills root, config restored).
func recoverRemoveSkill(st *store.Store, record TxnRecord) error {
	return recoverRemoveSkillWithHooks(st, record, removeRecoveryHooks{})
}

func recoverRemoveSkillWithHooks(st *store.Store, record TxnRecord, h removeRecoveryHooks) error {
	if err := validateRemoveTxn(record); err != nil {
		return err
	}
	storeRoot, err := st.StoreRoot()
	if err != nil {
		return fmt.Errorf("recover rm transaction through checked repository root: %w", err)
	}
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return fmt.Errorf("recover rm transaction through checked skills root: %w", err)
	}
	recoveryRoot, err := st.RecoveryRoot()
	if err != nil {
		return fmt.Errorf("recover rm transaction through checked recovery root: %w", err)
	}
	startHash := plumbing.NewHash(record.StartHead)
	startCommit, err := st.Repo.CommitObject(startHash)
	if err != nil {
		return fmt.Errorf("load rm transaction start commit %s: %w", record.StartHead, err)
	}
	startFile, err := startCommit.File("fu.yaml")
	if err != nil {
		return fmt.Errorf("load fu.yaml from rm transaction start commit: %w", err)
	}
	startConfig, err := startFile.Contents()
	if err != nil {
		return fmt.Errorf("read fu.yaml from rm transaction start commit: %w", err)
	}
	if !bytes.Equal([]byte(startConfig), record.ConfigBefore) {
		return fmt.Errorf("%w: recorded starting config does not match %s", ErrTxnConflict, record.StartHead)
	}

	expectedConfig, err := expectedRemovedConfig(record)
	if err != nil {
		return err
	}
	currentHead, err := st.Repo.Head()
	if err != nil {
		return fmt.Errorf("read HEAD while recovering rm transaction: %w", err)
	}
	if currentHead.Hash() == startHash {
		return rollBackUncommittedRemove(st, storeRoot, skillsRoot, recoveryRoot, record, expectedConfig)
	}
	return finishCommittedRemove(st, storeRoot, skillsRoot, recoveryRoot, record, startHash, currentHead.Hash(), expectedConfig, h)
}

func validateRemoveTxn(record TxnRecord) error {
	if record.Op != "rm" {
		return fmt.Errorf("rm recovery received operation %q", record.Op)
	}
	if err := skill.ValidateName(record.Name); err != nil {
		return fmt.Errorf("invalid skill name in rm transaction: %w", err)
	}
	if !plumbing.IsHash(record.StartHead) {
		return fmt.Errorf("rm transaction has invalid start HEAD %q", record.StartHead)
	}
	if record.Message != "rm: "+record.Name {
		return fmt.Errorf("rm transaction message %q does not match skill %q", record.Message, record.Name)
	}
	wantTargets := []string{
		filepath.Join("staging", record.Name),
		filepath.Join("store", "skills", record.Name),
	}
	if len(record.Targets) != len(wantTargets) || record.Targets[0] != wantTargets[0] || record.Targets[1] != wantTargets[1] {
		return fmt.Errorf("rm transaction targets %q do not match skill %q", record.Targets, record.Name)
	}
	if record.Payload != nil {
		if err := record.Payload.Validate(); err != nil {
			return fmt.Errorf("rm transaction has invalid payload ownership manifest: %w", err)
		}
	}
	if len(record.ConfigBefore) == 0 {
		return errors.New("rm transaction has no starting config snapshot")
	}
	switch record.Stage {
	case "started", rmTxnSnapshotted, rmTxnQuarantined, "config-saved":
	default:
		return fmt.Errorf("rm transaction has unknown stage %q", record.Stage)
	}
	return nil
}

// expectedRemovedConfig is ConfigBefore with the skill entry removed -- the
// config a completed rm must have written.
func expectedRemovedConfig(record TxnRecord) ([]byte, error) {
	cfg, err := store.LoadConfigBytes(record.ConfigBefore, "fu.yaml at transaction start")
	if err != nil {
		return nil, fmt.Errorf("parse rm transaction starting config: %w", err)
	}
	if !cfg.HasSkill(record.Name) {
		return nil, fmt.Errorf("%w: skill %q did not exist at transaction start", ErrTxnConflict, record.Name)
	}
	cfg.RemoveSkill(record.Name)
	out, err := cfg.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode reconstructed rm transaction config: %w", err)
	}
	return out, nil
}

// rollBackUncommittedRemove restores the starting state: content back in the
// skills root (either it never left, or it comes back from recovery), config
// restored, WAL cleared.
func rollBackUncommittedRemove(st *store.Store, storeRoot, skillsRoot, recoveryRoot *os.Root, record TxnRecord, expectedConfig []byte) error {
	currentConfig, err := store.ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		return err
	}
	if !bytes.Equal(currentConfig, record.ConfigBefore) && !bytes.Equal(currentConfig, expectedConfig) {
		return fmt.Errorf("%w: fu.yaml is neither the starting nor expected removed-skill config", ErrTxnConflict)
	}
	payload := rmPayloadName(record)
	skillPresent, err := txnPathPresent(skillsRoot, record.Name)
	if err != nil {
		return err
	}
	payloadPresent, err := txnPathPresent(recoveryRoot, payload)
	if err != nil {
		return err
	}
	if record.Payload == nil {
		// No snapshot was ever recorded. Two states share this shape: a
		// genuinely contentless skill (SnapshotSkillPayload returned
		// ENOENT), and a crash between the "started" WAL and the snapshot,
		// where the skill's own content is still untouched at skills/<name>
		// and nothing has been moved (round 4 finding I1). Both are benign;
		// only content appearing in *two* places is a conflict.
		switch {
		case skillPresent && payloadPresent:
			return fmt.Errorf("%w: contentless rm transaction found content in both skills/%s and %s", ErrTxnConflict, record.Name, payload)
		case payloadPresent:
			return fmt.Errorf("%w: contentless rm transaction found content at %s", ErrTxnConflict, payload)
		}
	} else {
		if skillPresent && payloadPresent {
			return fmt.Errorf("%w: uncommitted rm transaction has content in more than one ownership location", ErrTxnConflict)
		}
		if payloadPresent {
			if err := st.RestoreRecoveryPayloadToSkills(payload, record.Name, *record.Payload); err != nil {
				return mapInstallOwnershipError("restore quarantined rm content", err)
			}
			if err := st.ValidateSkillOwned(record.Name, *record.Payload); err != nil {
				return mapInstallOwnershipError("validate restored rm content", err)
			}
		} else if !skillPresent {
			return fmt.Errorf("%w: uncommitted rm transaction lost its content", ErrTxnConflict)
		} else if err := st.ValidateSkillOwned(record.Name, *record.Payload); err != nil {
			// The snapshot is recorded but the quarantine never ran: the
			// content at skills/<name> must still match the recorded
			// manifest. External tampering is a safe conflict with all
			// versions preserved, mirroring the install side's quarantine
			// validation (round 9 finding I-A).
			conflict := mapInstallOwnershipError("validate unquarantined rm content", err)
			return fmt.Errorf(
				"%w; restore %s to its recorded content, or move the changed entry aside",
				conflict, filepath.Join(st.SkillsDir(), record.Name))
		}
	}
	if err := restoreTxnConfig(st, currentConfig, record.ConfigBefore); err != nil {
		return err
	}
	return ClearTxn(st, record)
}

// finishCommittedRemove validates the operation commit and the final store
// state, archives the quarantined payload, and clears the WAL. A committed
// rm is its own end state -- no compensation commit exists for rm (plan D5).
func finishCommittedRemove(st *store.Store, storeRoot, skillsRoot, recoveryRoot *os.Root, record TxnRecord, startHash, currentHash plumbing.Hash, expectedConfig []byte, h removeRecoveryHooks) error {
	currentCommit, err := st.Repo.CommitObject(currentHash)
	if err != nil {
		return err
	}
	if len(currentCommit.ParentHashes) != 1 || currentCommit.ParentHashes[0] != startHash || currentCommit.Message != record.Message {
		return fmt.Errorf("%w: commit %s is not the recorded rm commit based on %s", ErrTxnConflict, currentHash, startHash)
	}
	if record.CommitTree == "" {
		return fmt.Errorf("%w: committed rm transaction has no prepared operation-tree fingerprint", ErrTxnConflict)
	}
	fingerprint, err := st.CommitTreeFingerprint(currentHash)
	if err != nil {
		return fmt.Errorf("fingerprint recorded rm commit %s: %w", currentHash, err)
	}
	if fingerprint != record.CommitTree {
		return fmt.Errorf("%w: commit %s tree %s does not match prepared operation tree %s", ErrTxnConflict, currentHash, fingerprint, record.CommitTree)
	}
	currentConfig, err := store.ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		return err
	}
	if !bytes.Equal(currentConfig, expectedConfig) {
		return fmt.Errorf("%w: committed rm transaction has unexpected config", ErrTxnConflict)
	}
	skillPresent, err := txnPathPresent(skillsRoot, record.Name)
	if err != nil {
		return err
	}
	if skillPresent {
		return fmt.Errorf("%w: committed rm transaction still has content at skills/%s", ErrTxnConflict, record.Name)
	}
	if record.Payload != nil {
		// No pre-archive presence check: ArchiveRecoveryPayloadOwned is
		// already idempotent over the [active, archive] position pair
		// (round 8 finding C1). Recovery itself can die after the archive
		// rename and before ClearTxn -- the payload then sits at the
		// archive name, which is exactly the state this call validates and
		// accepts. A payload absent from both names is refused by its
		// internal check.
		if err := st.ArchiveRecoveryPayloadOwned(rmPayloadName(record), *record.Payload); err != nil {
			return committedRemoveRecoveryConflict(st, record, err)
		}
	}
	if h.beforeWALClear != nil {
		if err := h.beforeWALClear(st, record); err != nil {
			return err
		}
	}
	return ClearTxn(st, record)
}

func committedRemoveRecoveryConflict(st *store.Store, record TxnRecord, err error) error {
	return recoveryPayloadConflict(st, record, "archive committed rm payload", rmPayloadName(record), "the rm is already committed", err)
}
