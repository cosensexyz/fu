// internal/engine/rm.go
package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// RemoveOutcome reports both the durable removal phases and its trailing
// reconciliation. Callers must use Operation.Committed rather than infer
// whether the removal happened from the error alone.
type RemoveOutcome struct {
	Name      string
	Operation OperationOutcome
}

// RemoveSkill unregisters a skill and removes its store entity, recording the
// removal in git history (SPEC §5.1 rm): the removed content stays recoverable
// from that history, and the copy quarantined for recovery is reclaimed once
// the transaction completes. Agent links are reclaimed by the pipeline's
// trailing reconcile: the name leaves the desired set, so Diff's leftover
// removal deletes the links.
func RemoveSkill(st *store.Store, agents []agent.Agent, name string) (RemoveOutcome, error) {
	return removeSkill(st, agents, name, hooks{})
}

// removeSkill carries RemoveSkill's implementation plus the pipeline's
// test-only hooks at the rm-specific durable boundaries.
func removeSkill(st *store.Store, agents []agent.Agent, name string, h hooks) (RemoveOutcome, error) {
	operation := OperationOutcome{Name: name}
	txn := &TxnRecord{
		Op:   "rm",
		Name: name,
		// Same [staging, store] order as new/add/adopt, so a validator
		// copied from the install pattern accepts rm's own records too
		// (round 6 finding M5).
		Targets: []string{
			filepath.Join("staging", name),
			filepath.Join("store", "skills", name),
		},
	}
	_, err := run(st, agents, Op{
		Message:        "rm: " + name,
		Txn:            txn,
		outcome:        &operation,
		AllowedChanges: []string{"fu.yaml", filepath.ToSlash(filepath.Join("skills", name))},
		ValidatePrepared: func(st *store.Store, prepared store.PreparedCommit) error {
			// The committed tree must no longer carry the skill.
			return st.ValidatePreparedPathAbsent(prepared, filepath.ToSlash(filepath.Join("skills", name)))
		},
		Preflight: func(st *store.Store, cfg *store.Config) error {
			if err := checkRemoveAvailable(st, cfg, name); err != nil {
				return err
			}
			return checkRemoveStoreEntry(st, name)
		},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			if err := checkRemoveAvailable(st, cfg, name); err != nil {
				return err
			}
			// Repeat the preflight check at mutation time so a replacement that
			// races the WAL boundary is still refused before it can be
			// quarantined.
			if err := checkRemoveStoreEntry(st, name); err != nil {
				return err
			}
			// Snapshot the live content first so a crash at any later point
			// leaves recovery with the exact manifest to restore or reclaim.
			payload, err := st.SnapshotSkillPayload(name)
			switch {
			case err == nil:
				txn.Payload = &payload
			case errors.Is(err, fs.ErrNotExist):
				// Registered but contentless (e.g. a failed publish from an
				// older run): nothing to quarantine, the removal itself is
				// still the right operation.
			default:
				return fmt.Errorf("snapshot skill %s: %w", name, err)
			}
			txn.Stage = rmTxnSnapshotted
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record rm snapshot: %w", err)
			}
			if err := h.fire(h.afterSnapshot); err != nil {
				return err
			}
			if txn.Payload != nil {
				if err := st.QuarantineSkillOwned(name, rmPayloadName(*txn), *txn.Payload); err != nil {
					return fmt.Errorf("quarantine %s: %w", name, err)
				}
			}
			txn.Stage = rmTxnQuarantined
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record rm quarantine: %w", err)
			}
			if err := h.fire(h.afterQuarantine); err != nil {
				return err
			}
			cfg.RemoveSkill(name)
			return nil
		},
		// The pipeline clears an operation's WAL itself once the commit lands,
		// so a plain uncrashed rm never reaches finishCommittedRemove's own
		// reclaim. This runs the same disposal at the same point of the same
		// ordering -- strictly after ClearTxn, never before it, since
		// reclaiming on an open WAL would destroy the payload
		// rollBackUncommittedRemove needs to restore.
		afterTxnCleared: func(st *store.Store) {
			// h.beforeReclaim is a test-only crash seam (production always
			// passes the zero hooks value, so this is a no-op there): it lets a
			// test terminate the process at the exact entry of this callback,
			// leaving the state `fu gc` must independently reclaim -- committed,
			// WAL cleared, payload still at its quarantine name.
			if err := h.fire(h.beforeReclaim); err != nil {
				return
			}
			reclaimCommittedRemovePayload(st, *txn)
		},
	}, h)
	return RemoveOutcome{Name: name, Operation: operation}, err
}

func checkRemoveAvailable(st *store.Store, cfg *store.Config, name string) error {
	if !cfg.HasSkill(name) {
		for _, invalid := range cfg.InvalidNames() {
			if invalid.Name == name {
				return fmt.Errorf("skill name %q fails validation (%s) and is ignored; edit %s to fix or remove it", invalid.Name, invalid.Reason, st.ConfigPath())
			}
		}
		return fmt.Errorf("unknown skill %q", name)
	}
	return nil
}

func checkRemoveStoreEntry(st *store.Store, name string) error {
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return fmt.Errorf("use checked skills root: %w", err)
	}
	if info, statErr := skillsRoot.Lstat(name); statErr == nil && !info.IsDir() {
		entry := filepath.Join(st.SkillsDir(), name)
		return fmt.Errorf("store entry %s is not a directory; move or remove it, then retry `fu rm %s`", entry, name)
	} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect store entry %s: %w", filepath.Join(st.SkillsDir(), name), statErr)
	}
	return nil
}

// rmPayloadName is the deterministic recovery name for removed skill
// content, mirroring the install compensation naming.
func rmPayloadName(record TxnRecord) string {
	return "removed-" + record.Name + "-" + record.StartHead
}
