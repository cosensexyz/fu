package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

// errAdoptTargetChanged marks a conflict on a user-owned adopt object rather
// than on an artifact fu created. The distinction decides whether isolation
// may run: a sibling, backup, retired entry or recovery payload changing under
// fu is unsafe state that must be preserved, while a user target changing
// before anything moved can be abandoned without risking their data.
//
// Every construction wraps ErrTxnConflict as well, so callers testing only
// for that sentinel are unaffected.
var errAdoptTargetChanged = errors.New("user-owned adopt target changed since inventory")

var errDirSwitchReplacementMissing = errors.New("whole-directory replacement is missing")

// dirSwitchTargetConflict names what changed, not just where. In the landed
// state the user's only way out is to restore the name set, and the directory
// path alone gives them nothing to act on; the diff costs one pass over two
// already-sorted manifests.
func dirSwitchTargetConflict(target AdoptTarget, observed []DirSwitchEntry) error {
	return fmt.Errorf("%w: %w: %s (%s)", ErrTxnConflict, errAdoptTargetChanged,
		target.SourcePath, describeDirSwitchDiff(target.TargetManifest, observed))
}

// describeDirSwitchDiff summarises the first few differences between the
// inventoried and observed child sets. Both are sorted by name.
func describeDirSwitchDiff(want, got []DirSwitchEntry) string {
	inventoried := make(map[string]uint32, len(want))
	for _, e := range want {
		inventoried[e.Name] = e.Mode
	}
	present := make(map[string]uint32, len(got))
	for _, e := range got {
		present[e.Name] = e.Mode
	}
	var notes []string
	for _, e := range want {
		if mode, ok := present[e.Name]; !ok {
			notes = append(notes, "removed "+e.Name)
		} else if mode != e.Mode {
			notes = append(notes, "changed type "+e.Name)
		}
	}
	for _, e := range got {
		if _, ok := inventoried[e.Name]; !ok {
			notes = append(notes, "added "+e.Name)
		}
	}
	if len(notes) == 0 {
		return "no entry difference found; re-inventory required"
	}
	const shown = 3
	if len(notes) > shown {
		return fmt.Sprintf("%s and %d more", strings.Join(notes[:shown], ", "), len(notes)-shown)
	}
	return strings.Join(notes, ", ")
}

func canIsolateDirSwitchTargetConflict(err error, sw *DirSwitchState, agentName string) bool {
	if !errors.Is(err, errAdoptTargetChanged) && !errors.Is(err, errDirSwitchReplacementMissing) {
		return false
	}
	if sw == nil {
		return true
	}
	if sw.Agent != agentName {
		return false
	}
	return sw.Stage == "building" || sw.Stage == "swapped" || sw.Stage == "done"
}

// switchWholeDirAgents runs the whole-directory switch for every agent whose
// skills directory was recorded as a symlink. A conflict on fu's own
// artifacts retains the WAL and stops immediately; a conflict on the user's
// target, and ordinary agent failures, are isolated only after fu proves and
// removes -- or restores -- its own in-flight artifacts.
func switchWholeDirAgents(st *store.Store, agents []agent.Agent, name string, txn *TxnRecord) error {
	return switchWholeDirAgentsWithHook(st, agents, name, txn, hooks{})
}

func switchWholeDirAgentsWithHook(st *store.Store, agents []agent.Agent, name string, txn *TxnRecord, h hooks) error {
	_, err := switchWholeDirAgentsReporting(st, agents, name, txn, h)
	return err
}

func switchWholeDirAgentsReporting(st *store.Store, agents []agent.Agent, name string, txn *TxnRecord, h hooks) ([]adoptIsolation, error) {
	var isolated []adoptIsolation
	for _, a := range agents {
		if err := switchWholeDirAgent(st, a, name, txn, h); err != nil {
			// Target conflicts are isolatable at every valid switch stage. At
			// building nothing user-owned has moved, so abandon removes only
			// fu's sibling. At swapped/done abandon either restores the archived
			// link or proves the replacement landed. Conflicts on fu-owned
			// artifacts remain hard stops.
			if errors.Is(err, ErrTxnConflict) &&
				!canIsolateDirSwitchTargetConflict(err, txn.DirSwitch, a.Name()) {
				return isolated, err
			}
			completed, cleanErr := abandonDirSwitchWithHooks(st, a, txn, h)
			if cleanErr != nil {
				return isolated, errors.Join(err, cleanErr)
			}
			if completed {
				// The abandon reached an already-landed state and finished the
				// switch rather than undoing it: this agent's skills directory
				// really was replaced. Dropping it from the lists would make
				// adoptOne filter it out of the summary and the CLI print
				// `adopted <name> (from ...)` without naming an agent it did
				// switch (round 18 finding M11).
				if err := WriteTxn(st, txn); err != nil {
					return isolated, err
				}
				continue
			}
			isolated = append(isolated, adoptIsolation{agent: a.Name(), err: err})
			// Both lists, not just WholeDirAgents: finishCommittedAdopt builds
			// the per-entry list as record.Agents minus record.WholeDirAgents,
			// so an agent left in the first alone is handed to the per-entry
			// switch on recovery -- the path adopt_txn.go documents as
			// forbidden for whole-directory agents (round 18 finding I2).
			txn.WholeDirAgents = withoutAgent(txn.WholeDirAgents, a.Name())
			txn.Agents = withoutAgent(txn.Agents, a.Name())
			if err := WriteTxn(st, txn); err != nil {
				return isolated, err
			}
		}
	}
	return isolated, nil
}

// withoutAgent returns names with one agent removed. It allocates rather than
// filtering in place: the transaction's Agents slice is the same backing array
// as the adopt summary's (adoptOne builds one from the other), so in-place
// compaction would rewrite a caller's slice behind its back.
func withoutAgent(names []string, drop string) []string {
	if names == nil {
		return nil
	}
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if name != drop {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

// abandonDirSwitch restores the original link when possible and removes only
// artifacts whose persisted identities and manifests still match. A changed
// sibling, backup, parent, or skills entry is foreign state and is preserved.
//
// completed reports that the switch was *finished* rather than undone. Two of
// the arms below are entered with the replacement already landed, where there
// is nothing left to restore -- the correct move is to remove the backup and
// close out. The caller has to know the difference: treating those as failures
// dropped the agent from the transaction's lists even though its skills
// directory had in fact been replaced (round 18 finding M11).
func abandonDirSwitch(a agent.Agent, txn *TxnRecord) (bool, error) {
	return abandonDirSwitchWithHooks(nil, a, txn, hooks{})
}

func abandonDirSwitchWithHooks(st *store.Store, a agent.Agent, txn *TxnRecord, h hooks) (completed bool, retErr error) {
	sw := txn.DirSwitch
	if sw == nil {
		return false, nil
	}
	target, err := targetForAgent(txn, a.Name())
	if err != nil {
		return false, err
	}
	parent, parentRoot, parentPath, err := openDirSwitchParent(target)
	if err != nil {
		return false, err
	}
	defer parent.Close()
	defer parentRoot.Close()
	if err := validateDirSwitchState(target, sw); err != nil {
		return false, err
	}

	skillsName := filepath.Base(target.SkillsDir)
	siblingName := filepath.Base(sw.Sibling)
	switch sw.Stage {
	case "building":
		// Nothing user-owned has moved at building. Even if the live entry
		// changed, removing the identity-bound sibling is sufficient to abandon
		// this agent; re-validating the user's entry here would recreate the
		// permanent conflict that routed us into abandon.
		if !adoptIdentityValid(sw.SiblingIdentity) {
			if err := reclaimUnjournalledDirSwitchSibling(parent, parentPath, siblingName); err != nil {
				return false, err
			}
		} else if err := removeDirSwitchSibling(parent, parentPath, siblingName, sw, true, h); err != nil {
			return false, err
		}
	case "swapped":
		entry, statErr := statAdoptEntry(int(parent.Fd()), skillsName)
		switch {
		case statErr == nil && adoptIdentity(&entry) == target.EntryIdentity:
			if err := validateCurrentAdoptEntry(parent, target, txn.Name); err != nil {
				return false, dirSwitchEntryConflict("original skills link", err)
			}
			if err := requireDirSwitchEntryAbsent(parent, filepath.Base(sw.Backup), "backup"); err != nil {
				return false, err
			}
			if err := removeDirSwitchSibling(parent, parentPath, siblingName, sw, false, h); err != nil {
				return false, err
			}
		case errors.Is(statErr, unix.ENOENT):
			if err := validateDirSwitchBackup(parent, target, sw); err != nil {
				return false, err
			}
			siblingErr := validateDirSwitchSibling(parent, parentPath, siblingName, sw, false)
			if siblingErr != nil && !errors.Is(siblingErr, errDirSwitchReplacementMissing) {
				return false, siblingErr
			}
			if err := renameDirSwitchEntry(parent, filepath.Base(sw.Backup), skillsName, "restore original whole-directory link"); err != nil {
				return false, err
			}
			if err := validateCurrentAdoptEntry(parent, target, txn.Name); err != nil {
				return false, dirSwitchEntryConflict("restored skills link", err)
			}
			if siblingErr == nil {
				if err := removeDirSwitchSibling(parent, parentPath, siblingName, sw, false, h); err != nil {
					return false, err
				}
			}
		case statErr == nil && statMode(entry.Mode) == unix.S_IFDIR:
			if err := validateDirSwitchSibling(parent, parentPath, skillsName, sw, false); err != nil {
				// A foreign directory at the still-live name before archive is a
				// user replacement, not a landed sibling. With no backup present,
				// nothing user-owned was moved and the recorded sibling can be
				// safely discarded.
				if absentErr := requireDirSwitchEntryAbsent(parent, filepath.Base(sw.Backup), "backup"); absentErr != nil {
					return false, err
				}
				if removeErr := removeDirSwitchSibling(parent, parentPath, siblingName, sw, false, h); removeErr != nil {
					return false, removeErr
				}
				break
			}
			// Landed: the target is not re-validated at all. The replacement is
			// in place and the only operation left is removing fu's own
			// backup, which validateDirSwitchBackup already proves by inode,
			// mode and raw link text. Checking the user's directory here
			// protected nothing and refused everything: first by digest
			// (round 18 I6), then by child inode (round 19), and even with
			// every failure mode tagged it still wedged, because the abandon's
			// own landed arm re-runs the same check and refuses identically.
			if err := removeDirSwitchBackup(st, parent, target, txn.Name, sw, h); err != nil {
				return false, err
			}
			// Nothing was undone here: the replacement is in place and the
			// backup is gone, so the switch is finished, not abandoned.
			completed = true
		default:
			if absentErr := requireDirSwitchEntryAbsent(parent, filepath.Base(sw.Backup), "backup"); absentErr != nil {
				return false, dirSwitchEntryConflict("skills entry", statErr)
			}
			if err := removeDirSwitchSibling(parent, parentPath, siblingName, sw, false, h); err != nil {
				return false, err
			}
		}
	case "done":
		if err := validateDirSwitchSibling(parent, parentPath, skillsName, sw, false); err != nil {
			return false, err
		}
		// Landed: the target is not re-validated at all. The replacement is
		// in place and the only operation left is removing fu's own
		// backup, which validateDirSwitchBackup already proves by inode,
		// mode and raw link text. Checking the user's directory here
		// protected nothing and refused everything: first by digest
		// (round 18 I6), then by child inode (round 19), and even with
		// every failure mode tagged it still wedged, because the abandon's
		// own landed arm re-runs the same check and refuses identically.
		if err := removeDirSwitchBackup(st, parent, target, txn.Name, sw, h); err != nil {
			return false, err
		}
		completed = true
	default:
		return false, fmt.Errorf("%w: whole-directory switch has unknown stage %q", ErrTxnConflict, sw.Stage)
	}
	txn.DirSwitch = nil
	return completed, nil
}

func switchWholeDirAgent(st *store.Store, a agent.Agent, name string, txn *TxnRecord, h hooks) error {
	if txn.DirSwitch != nil && txn.DirSwitch.Agent == a.Name() {
		return resumeDirSwitch(st, a, name, txn, h)
	}
	target, err := targetForAgent(txn, a.Name())
	if err != nil {
		return err
	}
	if !target.WholeDir {
		return fmt.Errorf("%w: adopt target for %s is not a whole-directory target", ErrTxnConflict, a.Name())
	}
	alreadySwitched, err := wholeDirAgentAlreadySwitched(st, target, name)
	if err != nil {
		return err
	}
	if alreadySwitched {
		return nil
	}
	return startDirSwitch(st, a, name, txn, h)
}

func wholeDirAgentAlreadySwitched(st *store.Store, target AdoptTarget, name string) (bool, error) {
	parent, parentRoot, parentPath, err := openDirSwitchParent(target)
	if err != nil {
		return false, err
	}
	defer parent.Close()
	defer parentRoot.Close()
	skillsName := filepath.Base(target.SkillsDir)
	stat, err := statAdoptEntry(int(parent.Fd()), skillsName)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil || statMode(stat.Mode) != unix.S_IFDIR {
		return false, err
	}
	dir, root, err := openDirSwitchDirectory(parent, parentPath, skillsName, adoptIdentity(&stat))
	if err != nil {
		return false, err
	}
	defer dir.Close()
	defer root.Close()
	actual, err := scanDirSwitchEntries(root)
	if err != nil {
		return false, err
	}
	expected := replacementDirManifest(st, target, name)
	if len(actual) != len(expected) {
		return false, nil
	}
	for index := range actual {
		if actual[index].Name != expected[index].Name || actual[index].Mode != expected[index].Mode || actual[index].LinkTarget != expected[index].LinkTarget {
			return false, nil
		}
	}
	return true, nil
}

// startDirSwitch declares and identity-binds the replacement sibling before
// populating it. The original link is not archived until the target manifest,
// selected skill digest, sibling identity, and sibling manifest all reverify.
func startDirSwitch(st *store.Store, a agent.Agent, name string, txn *TxnRecord, h hooks) error {
	target, err := targetForAgent(txn, a.Name())
	if err != nil {
		return err
	}
	if !target.WholeDir || len(target.TargetManifest) == 0 {
		return fmt.Errorf("%w: whole-directory target metadata for %s is incomplete", ErrTxnConflict, a.Name())
	}
	parent, parentRoot, parentPath, err := openDirSwitchParent(target)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer parentRoot.Close()
	if err := validateCurrentAdoptEntry(parent, target, name); err != nil {
		return dirSwitchEntryConflict("original skills link", err)
	}
	if err := validateWholeDirTarget(target); err != nil {
		return err
	}
	originalStat, err := statAdoptEntry(int(parent.Fd()), filepath.Base(target.SkillsDir))
	if err != nil {
		return err
	}

	sibling, err := dirSwitchSiblingName(parentPath)
	if err != nil {
		return err
	}
	cleanupID, err := dirSwitchCleanupID()
	if err != nil {
		return err
	}
	sw := &DirSwitchState{
		Agent:           a.Name(),
		Target:          target.SourcePath,
		Sibling:         sibling,
		SiblingManifest: replacementDirManifest(st, target, name),
		CleanupID:       cleanupID,
		Stage:           "building",
	}
	txn.DirSwitch = sw
	if err := WriteTxn(st, txn); err != nil {
		return err
	}
	siblingName := filepath.Base(sibling)
	if err := parentRoot.Mkdir(siblingName, 0o755); err != nil {
		return fmt.Errorf("create replacement skills directory %s: %w", sibling, err)
	}
	siblingStat, err := statAdoptEntry(int(parent.Fd()), siblingName)
	if err != nil {
		return err
	}
	if statMode(siblingStat.Mode) != unix.S_IFDIR {
		return fmt.Errorf("%w: replacement skills entry %s is not a directory", ErrTxnConflict, sibling)
	}
	sw.SiblingIdentity = adoptIdentity(&siblingStat)
	if err := WriteTxn(st, txn); err != nil {
		return err
	}
	siblingDir, siblingRoot, err := openDirSwitchDirectory(parent, parentPath, siblingName, sw.SiblingIdentity)
	if err != nil {
		return err
	}
	for i := range sw.SiblingManifest {
		entry := &sw.SiblingManifest[i]
		if err := siblingRoot.Symlink(entry.LinkTarget, entry.Name); err != nil {
			_ = siblingRoot.Close()
			_ = siblingDir.Close()
			return fmt.Errorf("create replacement entry %s/%s: %w", sibling, entry.Name, err)
		}
		if h.afterDirSwitchChildCreate != nil {
			if err := h.afterDirSwitchChildCreate(filepath.Join(sibling, entry.Name)); err != nil {
				_ = siblingRoot.Close()
				_ = siblingDir.Close()
				return err
			}
		}
		created, err := statAdoptEntry(int(siblingDir.Fd()), entry.Name)
		if err != nil {
			_ = siblingRoot.Close()
			_ = siblingDir.Close()
			return err
		}
		if statMode(created.Mode) != unix.S_IFLNK {
			_ = siblingRoot.Close()
			_ = siblingDir.Close()
			return fmt.Errorf("%w: replacement child %s/%s changed type", ErrTxnConflict, sibling, entry.Name)
		}
		entry.Identity = adoptIdentity(&created)
	}
	if err := WriteTxn(st, txn); err != nil {
		_ = siblingRoot.Close()
		_ = siblingDir.Close()
		return err
	}
	if err := siblingRoot.Close(); err != nil {
		_ = siblingDir.Close()
		return err
	}
	if err := siblingDir.Close(); err != nil {
		return err
	}
	if err := h.fire(h.afterDirSwitchBuild); err != nil {
		return err
	}
	if err := validateWholeDirTargetAndSkill(target, name); err != nil {
		return err
	}
	if err := validateDirSwitchSibling(parent, parentPath, siblingName, sw, false); err != nil {
		return err
	}
	backup, err := dirSwitchBackupName(parentPath)
	if err != nil {
		return err
	}
	sw.Backup = backup
	sw.BackupIdentity = target.EntryIdentity
	sw.BackupMode = uint32(checkedAgentFileMode(uint32(originalStat.Mode)))
	linkArchive, err := ensureAdoptLinkArchive(st, wholeDirAdoptLinkArchive(target, name, sw))
	if err != nil {
		return err
	}
	sw.LinkArchive = linkArchive
	sw.Stage = "swapped"
	if err := WriteTxn(st, txn); err != nil {
		return err
	}
	if err := h.fire(h.beforeDirSwitchArchive); err != nil {
		return err
	}
	return completeDirSwitch(st, target, name, txn, h)
}

func resumeDirSwitch(st *store.Store, a agent.Agent, name string, txn *TxnRecord, h hooks) error {
	target, err := targetForAgent(txn, a.Name())
	if err != nil {
		return err
	}
	sw := txn.DirSwitch
	if err := validateDirSwitchState(target, sw); err != nil {
		return err
	}
	if sw.Stage == "swapped" || sw.Stage == "done" {
		if err := validateAdoptLinkArchive(st, sw.LinkArchive, wholeDirAdoptLinkArchive(target, name, sw)); err != nil {
			return err
		}
	}
	switch sw.Stage {
	case "building":
		parent, parentRoot, parentPath, err := openDirSwitchParent(target)
		if err != nil {
			return err
		}
		defer parent.Close()
		defer parentRoot.Close()
		if err := validateCurrentAdoptEntry(parent, target, name); err != nil {
			return dirSwitchEntryConflict("original skills link", err)
		}
		siblingName := filepath.Base(sw.Sibling)
		_, statErr := statAdoptEntry(int(parent.Fd()), siblingName)
		retiredPresent, retiredErr := adoptNamePresent(parent, unjournalledDirSwitchRetiredName(siblingName))
		if retiredErr != nil {
			return retiredErr
		}
		if errors.Is(statErr, unix.ENOENT) && !retiredPresent {
			txn.DirSwitch = nil
			if err := WriteTxn(st, txn); err != nil {
				return err
			}
			return startDirSwitch(st, a, name, txn, h)
		}
		if statErr != nil {
			if !errors.Is(statErr, unix.ENOENT) {
				return statErr
			}
		}
		if !adoptIdentityValid(sw.SiblingIdentity) {
			if err := reclaimUnjournalledDirSwitchSibling(parent, parentPath, siblingName); err != nil {
				return err
			}
			txn.DirSwitch = nil
			if err := WriteTxn(st, txn); err != nil {
				return err
			}
			return startDirSwitch(st, a, name, txn, h)
		}
		if err := removeDirSwitchSibling(parent, parentPath, siblingName, sw, true, h); err != nil {
			return err
		}
		txn.DirSwitch = nil
		if err := WriteTxn(st, txn); err != nil {
			return err
		}
		return startDirSwitch(st, a, name, txn, h)
	case "swapped", "done":
		return completeDirSwitch(st, target, name, txn, h)
	default:
		return fmt.Errorf("%w: whole-directory switch has unknown stage %q", ErrTxnConflict, sw.Stage)
	}
}

// reclaimUnjournalledDirSwitchSibling removes a replacement directory whose
// identity was never recorded. startDirSwitch persists the sibling name,
// creates the directory, then persists its identity; a crash in that
// one-syscall window leaves an entry fu owns but cannot prove, and refusing it
// turned fu's own residue into a permanent self-inflicted conflict that failed
// every later write command (round 18 finding I1).
//
// Nothing is ever created inside the sibling before its identity is journalled,
// so emptiness is the proof of ownership -- and rmdir supplies it atomically,
// refusing any directory that has content. The entry is retired to an
// unpredictable name first, so a racing writer cannot drop content in at the
// original name between the check and the removal; anything that survives the
// rmdir is foreign state and is put back.
func unjournalledDirSwitchRetiredName(name string) string {
	sum := sha256.Sum256([]byte("unjournalled-dir-switch\x00" + name))
	return ".fu-skills-orphan-" + hex.EncodeToString(sum[:12])
}

func reclaimUnjournalledDirSwitchSibling(parent *os.File, parentPath, name string) error {
	defer keepDescriptorOwnersAlive(parent)
	display := filepath.Join(parentPath, name)
	retired := unjournalledDirSwitchRetiredName(name)
	livePresent, err := adoptNamePresent(parent, name)
	if err != nil {
		return err
	}
	retiredPresent, err := adoptNamePresent(parent, retired)
	if err != nil {
		return err
	}
	if livePresent && retiredPresent {
		return fmt.Errorf("%w: unrecorded replacement directory exists at live and retired names", ErrTxnConflict)
	}
	if !livePresent && !retiredPresent {
		return nil
	}
	active := name
	if retiredPresent {
		active = retired
	}
	observed, err := statAdoptEntry(int(parent.Fd()), active)
	if err != nil {
		return err
	}
	if statMode(observed.Mode) != unix.S_IFDIR {
		return fmt.Errorf("%w: replacement entry %s exists without a recorded identity and is not a directory", ErrTxnConflict, display)
	}
	if livePresent {
		if err := store.RenameNoReplaceAt(parent, name, parent, retired); err != nil {
			return fmt.Errorf("retire unrecorded replacement directory %s: %w", display, err)
		}
	}
	renamedHere := livePresent
	moved, err := statAdoptEntry(int(parent.Fd()), retired)
	if err != nil || adoptIdentity(&moved) != adoptIdentity(&observed) || statMode(moved.Mode) != unix.S_IFDIR {
		mismatch := fmt.Errorf("%w: unrecorded replacement directory %s changed at retirement", ErrTxnConflict, display)
		if renamedHere {
			if restoreErr := store.RestoreRetiredAt(parent, retired, name); restoreErr != nil {
				return errors.Join(mismatch, restoreErr)
			}
			return mismatch
		}
		return fmt.Errorf("%w; preserved at %s", mismatch, filepath.Join(parentPath, retired))
	}
	if err := unix.Unlinkat(int(parent.Fd()), retired, unix.AT_REMOVEDIR); err != nil {
		if !renamedHere {
			return fmt.Errorf("%w: unrecorded replacement directory is not empty and is preserved at %s; move it aside to continue", ErrTxnConflict, filepath.Join(parentPath, retired))
		}
		if restoreErr := store.RestoreRetiredAt(parent, retired, name); restoreErr != nil {
			// The restore failed, so the content is at the retired name and the
			// original name is free. Telling the user to move aside the original
			// names a path nothing is at, and renameNoReplace returns a bare
			// errno that carries no path of its own -- the joined error read
			// "file exists" with nothing to locate. store/retire.go names the
			// retired path in the same situation; mirror it (round 18 M10's
			// class, reintroduced here).
			return errors.Join(fmt.Errorf(
				"%w: replacement directory %s exists without a recorded identity and is not empty, so it is not fu's residue; it is now at %s -- move it aside to continue",
				ErrTxnConflict, display, filepath.Join(parentPath, retired)), restoreErr)
		}
		return fmt.Errorf("%w: replacement directory %s exists without a recorded identity and is not empty, so it is not fu's residue; move it aside to continue", ErrTxnConflict, display)
	}
	return nil
}

// completeDirSwitch rolls a persisted swapped/done state forward using only
// the recorded parent, entry, target, sibling, and backup identities.
func completeDirSwitch(st *store.Store, target AdoptTarget, name string, txn *TxnRecord, h hooks) error {
	sw := txn.DirSwitch
	parent, parentRoot, parentPath, err := openDirSwitchParent(target)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer parentRoot.Close()
	if err := validateDirSwitchState(target, sw); err != nil {
		return err
	}
	if sw.Stage == "swapped" || sw.Stage == "done" {
		if err := validateAdoptLinkArchive(st, sw.LinkArchive, wholeDirAdoptLinkArchive(target, name, sw)); err != nil {
			return err
		}
	}
	skillsName := filepath.Base(target.SkillsDir)
	siblingName := filepath.Base(sw.Sibling)
	backupName := filepath.Base(sw.Backup)

	if sw.Stage == "swapped" {
		entry, statErr := statAdoptEntry(int(parent.Fd()), skillsName)
		switch {
		case statErr == nil && adoptIdentity(&entry) == target.EntryIdentity:
			if err := validateCurrentAdoptEntry(parent, target, name); err != nil {
				return dirSwitchEntryConflict("original skills link", err)
			}
			if err := requireDirSwitchEntryAbsent(parent, backupName, "backup"); err != nil {
				return err
			}
			if err := validateWholeDirTargetAndSkill(target, name); err != nil {
				return err
			}
			if err := validateDirSwitchSibling(parent, parentPath, siblingName, sw, false); err != nil {
				return err
			}
			if err := renameDirSwitchEntry(parent, skillsName, backupName, "archive original whole-directory link"); err != nil {
				return err
			}
			if err := validateDirSwitchBackup(parent, target, sw); err != nil {
				return restoreUnexpectedDirSwitchMoveAfterError(parent, backupName, skillsName, err)
			}
			if err := h.fire(h.afterDirSwitchSwap); err != nil {
				return err
			}
		case errors.Is(statErr, unix.ENOENT):
			if err := validateDirSwitchBackup(parent, target, sw); err != nil {
				return err
			}
		case statErr == nil && statMode(entry.Mode) == unix.S_IFDIR:
			if err := validateDirSwitchSibling(parent, parentPath, skillsName, sw, false); err != nil {
				if absentErr := requireDirSwitchEntryAbsent(parent, backupName, "backup"); absentErr == nil {
					return dirSwitchEntryConflict("skills entry", err)
				}
				return err
			}
			if err := validateDirSwitchBackup(parent, target, sw); err != nil {
				return err
			}
			// Landed: the target is not re-validated at all. The replacement is
			// in place and the only operation left is removing fu's own
			// backup, which validateDirSwitchBackup already proves by inode,
			// mode and raw link text. Checking the user's directory here
			// protected nothing and refused everything: first by digest
			// (round 18 I6), then by child inode (round 19), and even with
			// every failure mode tagged it still wedged, because the abandon's
			// own landed arm re-runs the same check and refuses identically.
			goto landed
		default:
			return dirSwitchEntryConflict("skills entry", statErr)
		}

		if err := validateWholeDirTargetAndSkill(target, name); err != nil {
			return err
		}
		if err := validateDirSwitchSibling(parent, parentPath, siblingName, sw, false); err != nil {
			return err
		}
		if err := renameDirSwitchEntry(parent, siblingName, skillsName, "land replacement skills directory"); err != nil {
			return err
		}
		if err := validateDirSwitchSibling(parent, parentPath, skillsName, sw, false); err != nil {
			return restoreUnexpectedDirSwitchMoveAfterError(parent, skillsName, siblingName, err)
		}
		if err := h.fire(h.afterDirSwitchLand); err != nil {
			return err
		}
	landed:
		sw.Stage = "done"
		if err := WriteTxn(st, txn); err != nil {
			return err
		}
	}

	if err := validateDirSwitchSibling(parent, parentPath, skillsName, sw, false); err != nil {
		return err
	}
	// The target is deliberately not re-validated here. See the landed arms
	// above: once the replacement is in place, the only operation left is
	// removing fu's own backup, and removeDirSwitchBackup proves that object
	// by inode, mode and raw link text on its own. Three rounds of narrowing
	// this check (digest, then child inode, then every failure mode tagged)
	// each left a shape that refused an ordinary user action and wedged every
	// later write command; the check was never protecting anything the backup
	// validation does not already establish.
	if err := removeDirSwitchBackup(st, parent, target, name, sw, h); err != nil {
		return err
	}
	txn.DirSwitch = nil
	return WriteTxn(st, txn)
}

// asTargetConflict tags every failure of the two target validators, not one
// comparison inside them.
//
// Tagging a single comparison left three reachable failure modes untagged, and
// each is just as much a statement about the user's own directory: the
// target's inode changed or it is gone (openBoundAdoptSource compares
// SourceIdentity), it could not be read (scanDirSwitchEntries), or the adopted
// entry's content changed (the digest). Re-creating a dotfiles directory from
// a backup, an rsync into place, or an editor save during the switch produce
// exactly those -- and each still returned before the abandon, leaving
// ~/.claude/skills missing with every later write command re-entering recovery
// and refusing identically. The inode variant cannot be undone by any user
// action at all.
//
// The boundary is the right place because the *function* is the unit that
// answers a question about an object fu does not own. Enumerating call sites
// or comparisons is what produced two rounds of partial fixes.
func asTargetConflict(err error) error {
	if err == nil || errors.Is(err, errAdoptTargetChanged) {
		return err
	}
	if !errors.Is(err, ErrTxnConflict) {
		return fmt.Errorf("%w: %w: %w", ErrTxnConflict, errAdoptTargetChanged, err)
	}
	return fmt.Errorf("%w: %w", errAdoptTargetChanged, err)
}

func validateWholeDirTarget(target AdoptTarget) error {
	return asTargetConflict(validateWholeDirTargetInner(target))
}

func validateWholeDirTargetInner(target AdoptTarget) error {
	root, err := openBoundAdoptSource(target)
	if err != nil {
		return err
	}
	defer root.Close()
	manifest, err := scanDirSwitchEntries(root)
	if err != nil {
		return fmt.Errorf("%w: inspect whole-directory target %s: %v", ErrTxnConflict, target.SourcePath, err)
	}
	if !sameDirSwitchTargetEntries(manifest, target.TargetManifest) {
		return dirSwitchTargetConflict(target, manifest)
	}
	return nil
}

func validateWholeDirTargetAndSkill(target AdoptTarget, name string) error {
	return asTargetConflict(validateWholeDirTargetAndSkillInner(target, name))
}

func validateWholeDirTargetAndSkillInner(target AdoptTarget, name string) error {
	root, err := openBoundAdoptSource(target)
	if err != nil {
		return err
	}
	defer root.Close()
	manifest, err := scanDirSwitchEntries(root)
	if err != nil {
		return fmt.Errorf("%w: inspect whole-directory target %s: %v", ErrTxnConflict, target.SourcePath, err)
	}
	if !sameDirSwitchTargetEntries(manifest, target.TargetManifest) {
		return dirSwitchTargetConflict(target, manifest)
	}
	skillRoot, err := openBoundWholeDirSkill(target, name)
	if err != nil {
		return err
	}
	defer skillRoot.Close()
	projection, err := skill.ProjectDir(skillRoot.FS(), ".")
	if err != nil {
		return fmt.Errorf("%w: project adopted target entry %s: %v", ErrTxnConflict, name, err)
	}
	digest, err := skill.DigestManifest(projection)
	if err != nil {
		return fmt.Errorf("%w: digest adopted target entry %s: %v", ErrTxnConflict, name, err)
	}
	if digest != target.Digest {
		return fmt.Errorf("%w: whole-directory target entry %s changed since inventory", ErrTxnConflict, name)
	}
	return nil
}

func openBoundWholeDirSkill(target AdoptTarget, name string) (*os.Root, error) {
	path := filepath.Join(target.SourcePath, name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve whole-directory target entry %s: %v", ErrTxnConflict, name, err)
	}
	dir, identity, err := openAdoptDirectory(resolved)
	if err != nil {
		return nil, fmt.Errorf("%w: open whole-directory target entry %s: %v", ErrTxnConflict, name, err)
	}
	root, rootErr := pairBoundAdoptRoot(resolved, dir, identity)
	closeErr := dir.Close()
	if rootErr != nil {
		return nil, rootErr
	}
	if closeErr != nil {
		_ = root.Close()
		return nil, closeErr
	}
	return root, nil
}

func replacementDirManifest(st *store.Store, target AdoptTarget, name string) []DirSwitchEntry {
	manifest := make([]DirSwitchEntry, 0, len(target.TargetManifest))
	for _, entry := range target.TargetManifest {
		linkBase := target.LinkTarget
		if !filepath.IsAbs(linkBase) {
			linkBase = filepath.Join("..", linkBase)
		}
		linkTarget := filepath.Join(linkBase, entry.Name)
		if entry.Name == name {
			linkTarget = filepath.Join(st.SkillsDir(), name)
		}
		manifest = append(manifest, DirSwitchEntry{
			Name:       entry.Name,
			Mode:       uint32(fs.ModeSymlink),
			LinkTarget: linkTarget,
		})
	}
	return manifest
}

func openDirSwitchParent(target AdoptTarget) (*os.File, *os.Root, string, error) {
	parentPath := filepath.Dir(target.SkillsDir)
	parent, err := openBoundAdoptParent(target)
	if err != nil {
		return nil, nil, "", err
	}
	root, err := pairBoundAdoptRoot(parentPath, parent, target.ParentIdentity)
	if err != nil {
		_ = parent.Close()
		return nil, nil, "", asTargetConflict(err)
	}
	return parent, root, parentPath, nil
}

func openDirSwitchDirectory(parent *os.File, parentPath, name string, expected store.FileIdentity) (*os.File, *os.Root, error) {
	defer keepDescriptorOwnersAlive(parent)
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "openat", Path: filepath.Join(parentPath, name), Err: err}
	}
	dir := os.NewFile(uintptr(fd), filepath.Join(parentPath, name))
	if dir == nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("open replacement directory %s: invalid descriptor", name)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	if !adoptIdentityValid(expected) || adoptIdentity(&stat) != expected {
		_ = dir.Close()
		return nil, nil, fmt.Errorf("%w: whole-directory switch directory %s was replaced", ErrTxnConflict, filepath.Join(parentPath, name))
	}
	root, err := pairBoundAdoptRoot(filepath.Join(parentPath, name), dir, expected)
	if err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	return dir, root, nil
}

func validateDirSwitchSibling(parent *os.File, parentPath, name string, sw *DirSwitchState, partial bool) error {
	dir, root, err := openDirSwitchDirectory(parent, parentPath, name, sw.SiblingIdentity)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w: whole-directory replacement %s is missing; the preserved original is at %s", ErrTxnConflict, errDirSwitchReplacementMissing, filepath.Join(parentPath, name), sw.Backup)
		}
		return err
	}
	defer dir.Close()
	defer root.Close()
	manifest, err := scanDirSwitchEntries(root)
	if err != nil {
		return fmt.Errorf("%w: inspect replacement directory %s: %v", ErrTxnConflict, filepath.Join(parentPath, name), err)
	}
	if partial {
		if !dirSwitchManifestSubset(manifest, sw.SiblingManifest) {
			return fmt.Errorf("%w: partially built replacement directory %s contains an undeclared entry", ErrTxnConflict, filepath.Join(parentPath, name))
		}
	} else if !sameDirSwitchEntries(manifest, sw.SiblingManifest) {
		return fmt.Errorf("%w: replacement directory %s no longer matches its recorded manifest", ErrTxnConflict, filepath.Join(parentPath, name))
	}
	return nil
}

func removeDirSwitchSibling(parent *os.File, parentPath, name string, sw *DirSwitchState, partial bool, h hooks) error {
	defer keepDescriptorOwnersAlive(parent)
	rootRetired := dirSwitchRetiredName(sw, "root", name)
	originalPresent, err := adoptNamePresent(parent, name)
	if err != nil {
		return err
	}
	retiredPresent, err := adoptNamePresent(parent, rootRetired)
	if err != nil {
		return err
	}
	if originalPresent && retiredPresent {
		return fmt.Errorf("%w: replacement directory exists at both live and retired names", ErrTxnConflict)
	}
	if !originalPresent && !retiredPresent {
		return nil
	}
	active := name
	if retiredPresent {
		active = rootRetired
	}
	dir, root, err := openDirSwitchDirectory(parent, parentPath, active, sw.SiblingIdentity)
	if err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		_ = dir.Close()
		return err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		_ = dir.Close()
		return err
	}
	want := make(map[string]DirSwitchEntry, len(sw.SiblingManifest))
	retiredNames := make(map[string]DirSwitchEntry, len(sw.SiblingManifest))
	for _, entry := range sw.SiblingManifest {
		want[entry.Name] = entry
		retiredNames[dirSwitchRetiredName(sw, "child", entry.Name)] = entry
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		expected, ok := want[entry.Name()]
		if !ok {
			expected, ok = retiredNames[entry.Name()]
		}
		if !ok {
			_ = dir.Close()
			return fmt.Errorf("%w: replacement directory %s contains undeclared cleanup entry %q", ErrTxnConflict, filepath.Join(parentPath, active), entry.Name())
		}
		if seen[expected.Name] {
			_ = dir.Close()
			return fmt.Errorf("%w: replacement child %q exists at live and retired names", ErrTxnConflict, expected.Name)
		}
		seen[expected.Name] = true
	}
	if !partial {
		for _, entry := range sw.SiblingManifest {
			if !seen[entry.Name] {
				_ = dir.Close()
				return fmt.Errorf("%w: replacement directory lost child %q", ErrTxnConflict, entry.Name)
			}
		}
	}
	for _, entry := range sw.SiblingManifest {
		if !seen[entry.Name] {
			continue
		}
		var retireErr error
		if adoptIdentityValid(entry.Identity) {
			retireErr = retireDirSwitchLink(dir, filepath.Join(parentPath, active), entry, sw, h.beforeDirSwitchChildRetire)
		} else if partial {
			retireErr = retireUnjournalledDirSwitchLink(dir, filepath.Join(parentPath, active), entry, sw, h.beforeDirSwitchChildRetire)
		} else {
			retireErr = fmt.Errorf("%w: replacement child %q has no persisted identity", ErrTxnConflict, entry.Name)
		}
		if retireErr != nil {
			_ = dir.Close()
			return retireErr
		}
	}
	if err := dir.Close(); err != nil {
		return err
	}
	renamedHere := !retiredPresent
	if renamedHere {
		if h.beforeDirSwitchRootRetire != nil {
			if err := h.beforeDirSwitchRootRetire(filepath.Join(parentPath, name)); err != nil {
				return err
			}
		}
		if err := store.RenameNoReplaceAt(parent, name, parent, rootRetired); err != nil {
			return fmt.Errorf("retire replacement directory %s: %w", filepath.Join(parentPath, name), err)
		}
	}
	stat, err := statAdoptEntry(int(parent.Fd()), rootRetired)
	if err != nil || adoptIdentity(&stat) != sw.SiblingIdentity || statMode(stat.Mode) != unix.S_IFDIR {
		mismatch := fmt.Errorf("%w: replacement directory changed at retirement", ErrTxnConflict)
		if renamedHere {
			if restoreErr := store.RestoreRetiredAt(parent, rootRetired, name); restoreErr != nil {
				return errors.Join(mismatch, restoreErr)
			}
		}
		return mismatch
	}
	if err := unix.Unlinkat(int(parent.Fd()), rootRetired, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove retired replacement directory: %w", err)
	}
	return nil
}

// retireUnjournalledDirSwitchLink reclaims a child created after the last
// durable building revision. The random cleanup namespace and exact raw link
// target identify fu's residue; an inode snapshot closes the validation/rename
// race before unlinking it.
func retireUnjournalledDirSwitchLink(dir *os.File, display string, expected DirSwitchEntry, sw *DirSwitchState, before func(string) error) error {
	defer keepDescriptorOwnersAlive(dir)
	retired := dirSwitchRetiredName(sw, "child", expected.Name)
	livePresent, err := adoptNamePresent(dir, expected.Name)
	if err != nil {
		return err
	}
	retiredPresent, err := adoptNamePresent(dir, retired)
	if err != nil {
		return err
	}
	if livePresent && retiredPresent {
		return fmt.Errorf("%w: replacement child %q exists at live and retired names", ErrTxnConflict, expected.Name)
	}
	if !livePresent && !retiredPresent {
		return fmt.Errorf("%w: replacement child %q disappeared during cleanup", ErrTxnConflict, expected.Name)
	}
	active := expected.Name
	if retiredPresent {
		active = retired
	}
	observed, err := statAdoptEntry(int(dir.Fd()), active)
	if err != nil {
		return err
	}
	if statMode(observed.Mode) != unix.S_IFLNK {
		return fmt.Errorf("%w: unjournalled replacement child %q is not a symlink", ErrTxnConflict, expected.Name)
	}
	raw, err := readAdoptLink(int(dir.Fd()), active)
	if err != nil {
		return err
	}
	if raw != expected.LinkTarget {
		return fmt.Errorf("%w: unjournalled replacement child %q changed target", ErrTxnConflict, expected.Name)
	}
	if livePresent {
		if before != nil {
			if err := before(filepath.Join(display, expected.Name)); err != nil {
				return err
			}
		}
		if err := store.RenameNoReplaceAt(dir, expected.Name, dir, retired); err != nil {
			return err
		}
	}
	moved, err := statAdoptEntry(int(dir.Fd()), retired)
	if err != nil || adoptIdentity(&moved) != adoptIdentity(&observed) || statMode(moved.Mode) != unix.S_IFLNK {
		mismatch := fmt.Errorf("%w: unjournalled replacement child %q changed at retirement", ErrTxnConflict, expected.Name)
		if livePresent {
			if restoreErr := store.RestoreRetiredAt(dir, retired, expected.Name); restoreErr != nil {
				return errors.Join(mismatch, restoreErr)
			}
		}
		return mismatch
	}
	raw, err = readAdoptLink(int(dir.Fd()), retired)
	if err != nil || raw != expected.LinkTarget {
		mismatch := fmt.Errorf("%w: unjournalled replacement child %q changed target at retirement", ErrTxnConflict, expected.Name)
		if livePresent {
			if restoreErr := store.RestoreRetiredAt(dir, retired, expected.Name); restoreErr != nil {
				return errors.Join(mismatch, restoreErr)
			}
		}
		return mismatch
	}
	return unix.Unlinkat(int(dir.Fd()), retired, 0)
}

func retireDirSwitchLink(dir *os.File, display string, expected DirSwitchEntry, sw *DirSwitchState, before func(string) error) error {
	defer keepDescriptorOwnersAlive(dir)
	retired := dirSwitchRetiredName(sw, "child", expected.Name)
	livePresent, err := adoptNamePresent(dir, expected.Name)
	if err != nil {
		return err
	}
	retiredPresent, err := adoptNamePresent(dir, retired)
	if err != nil {
		return err
	}
	if livePresent && retiredPresent {
		return fmt.Errorf("%w: replacement child %q exists at live and retired names", ErrTxnConflict, expected.Name)
	}
	if !livePresent && !retiredPresent {
		return fmt.Errorf("%w: replacement child %q disappeared during cleanup", ErrTxnConflict, expected.Name)
	}
	if livePresent {
		if err := validateDirSwitchLink(dir, expected.Name, expected); err != nil {
			return err
		}
		if before != nil {
			if err := before(filepath.Join(display, expected.Name)); err != nil {
				return err
			}
		}
		if err := store.RenameNoReplaceAt(dir, expected.Name, dir, retired); err != nil {
			return err
		}
	}
	if err := validateDirSwitchLink(dir, retired, expected); err != nil {
		if livePresent {
			if restoreErr := store.RestoreRetiredAt(dir, retired, expected.Name); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		}
		return err
	}
	if err := unix.Unlinkat(int(dir.Fd()), retired, 0); err != nil {
		return err
	}
	return nil
}

func validateDirSwitchLink(parent *os.File, name string, expected DirSwitchEntry) error {
	defer keepDescriptorOwnersAlive(parent)
	stat, err := statAdoptEntry(int(parent.Fd()), name)
	if err != nil {
		return err
	}
	if adoptIdentity(&stat) != expected.Identity || statMode(stat.Mode) != unix.S_IFLNK {
		return fmt.Errorf("%w: replacement child %q changed identity or type", ErrTxnConflict, expected.Name)
	}
	raw, err := readAdoptLink(int(parent.Fd()), name)
	if err != nil {
		return err
	}
	if raw != expected.LinkTarget {
		return fmt.Errorf("%w: replacement child %q changed target", ErrTxnConflict, expected.Name)
	}
	return nil
}

func validateDirSwitchBackup(parent *os.File, target AdoptTarget, sw *DirSwitchState) error {
	defer keepDescriptorOwnersAlive(parent)
	if sw.Backup == "" {
		return fmt.Errorf("%w: whole-directory switch has no backup path", ErrTxnConflict)
	}
	name := filepath.Base(sw.Backup)
	stat, err := statAdoptEntry(int(parent.Fd()), name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("%w: whole-directory backup %s is missing", ErrTxnConflict, sw.Backup)
		}
		return err
	}
	if adoptIdentity(&stat) != sw.BackupIdentity || sw.BackupIdentity != target.EntryIdentity || statMode(stat.Mode) != unix.S_IFLNK {
		return fmt.Errorf("%w: whole-directory backup %s was replaced", ErrTxnConflict, sw.Backup)
	}
	raw, err := readAdoptLink(int(parent.Fd()), name)
	if err != nil {
		return err
	}
	if raw != target.LinkTarget {
		return fmt.Errorf("%w: whole-directory backup %s changed target", ErrTxnConflict, sw.Backup)
	}
	return nil
}

func removeDirSwitchBackup(st *store.Store, parent *os.File, target AdoptTarget, skillName string, sw *DirSwitchState, h hooks) error {
	defer keepDescriptorOwnersAlive(parent)
	if sw.Backup == "" {
		return nil
	}
	if err := validateAdoptLinkArchive(st, sw.LinkArchive, wholeDirAdoptLinkArchive(target, skillName, sw)); err != nil {
		return err
	}
	name := filepath.Base(sw.Backup)
	retired := dirSwitchRetiredName(sw, "backup", name)
	livePresent, err := adoptNamePresent(parent, name)
	if err != nil {
		return err
	}
	retiredPresent, err := adoptNamePresent(parent, retired)
	if err != nil {
		return err
	}
	if livePresent && retiredPresent {
		return fmt.Errorf("%w: whole-directory backup exists at live and retired names", ErrTxnConflict)
	}
	if !livePresent && !retiredPresent {
		return nil
	}
	if livePresent {
		if err := validateDirSwitchBackup(parent, target, sw); err != nil {
			return err
		}
		if h.beforeDirSwitchBackupRetire != nil {
			if err := h.beforeDirSwitchBackupRetire(sw.Backup); err != nil {
				return err
			}
		}
		if err := store.RenameNoReplaceAt(parent, name, parent, retired); err != nil {
			return fmt.Errorf("retire archived whole-directory link %s: %w", sw.Backup, err)
		}
	}
	retiredState := *sw
	retiredState.Backup = filepath.Join(filepath.Dir(sw.Backup), retired)
	if err := validateDirSwitchBackup(parent, target, &retiredState); err != nil {
		if livePresent {
			if restoreErr := store.RestoreRetiredAt(parent, retired, name); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		}
		return err
	}
	if err := unix.Unlinkat(int(parent.Fd()), retired, 0); err != nil {
		return fmt.Errorf("remove archived whole-directory link %s: %w", sw.Backup, err)
	}
	return nil
}

func validateDirSwitchState(target AdoptTarget, sw *DirSwitchState) error {
	if sw == nil || sw.Agent != target.Agent || filepath.Clean(sw.Target) != filepath.Clean(target.SourcePath) {
		return fmt.Errorf("%w: incomplete or mismatched whole-directory switch metadata", ErrTxnConflict)
	}
	parent := filepath.Dir(target.SkillsDir)
	if !validDirSwitchPath(sw.Sibling, parent, ".fu-skills-") {
		return fmt.Errorf("%w: whole-directory sibling %q escapes its recorded parent", ErrTxnConflict, sw.Sibling)
	}
	if len(sw.CleanupID) != 16 {
		return fmt.Errorf("%w: whole-directory switch has invalid cleanup identity", ErrTxnConflict)
	}
	if _, err := hex.DecodeString(sw.CleanupID); err != nil {
		return fmt.Errorf("%w: whole-directory switch has invalid cleanup identity", ErrTxnConflict)
	}
	if sw.Stage == "swapped" || sw.Stage == "done" {
		if !validDirSwitchPath(sw.Backup, parent, ".fu-skills-old-") {
			return fmt.Errorf("%w: whole-directory backup %q escapes its recorded parent", ErrTxnConflict, sw.Backup)
		}
		if !adoptIdentityValid(sw.SiblingIdentity) || sw.BackupIdentity != target.EntryIdentity ||
			os.FileMode(sw.BackupMode).Type() != os.ModeSymlink ||
			!validAdoptLinkArchiveName(sw.LinkArchive) || len(sw.SiblingManifest) == 0 {
			return fmt.Errorf("%w: whole-directory switch lacks persisted artifact ownership", ErrTxnConflict)
		}
		for _, entry := range sw.SiblingManifest {
			if !adoptIdentityValid(entry.Identity) {
				return fmt.Errorf("%w: whole-directory switch child %q lacks persisted identity", ErrTxnConflict, entry.Name)
			}
		}
	}
	return nil
}

func wholeDirAdoptLinkArchive(target AdoptTarget, skillName string, sw *DirSwitchState) adoptLinkArchiveRecord {
	return newAdoptLinkArchiveRecord(
		adoptLinkArchiveWholeDirectory, sw.Agent, skillName, target.SkillsDir,
		target.LinkTarget, sw.BackupMode, sw.BackupIdentity,
	)
}

func validDirSwitchPath(path, parent, prefix string) bool {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	return filepath.IsAbs(clean) && filepath.Dir(clean) == filepath.Clean(parent) && strings.HasPrefix(base, prefix) && len(base) > len(prefix)
}

func requireDirSwitchEntryAbsent(parent *os.File, name, label string) error {
	defer keepDescriptorOwnersAlive(parent)
	if name == "" || name == "." {
		return fmt.Errorf("%w: whole-directory switch has no %s name", ErrTxnConflict, label)
	}
	_, err := statAdoptEntry(int(parent.Fd()), name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: whole-directory %s %s already exists", ErrTxnConflict, label, name)
}

func renameDirSwitchEntry(parent *os.File, oldName, newName, action string) error {
	if err := store.RenameNoReplaceAt(parent, oldName, parent, newName); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s would replace existing entry %s", ErrTxnConflict, action, newName)
		}
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}

func restoreUnexpectedDirSwitchMove(parent *os.File, movedName, originalName string) error {
	defer keepDescriptorOwnersAlive(parent)
	if _, err := statAdoptEntry(int(parent.Fd()), originalName); err == nil {
		return fmt.Errorf("%w: cannot restore %s because %s is occupied", ErrTxnConflict, movedName, originalName)
	} else if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("inspect restore destination %s: %w", originalName, err)
	}
	return store.RenameNoReplaceAt(parent, movedName, parent, originalName)
}

func restoreUnexpectedDirSwitchMoveAfterError(parent *os.File, movedName, originalName string, primary error) error {
	return errors.Join(primary, restoreUnexpectedDirSwitchMove(parent, movedName, originalName))
}

func dirSwitchManifestSubset(actual, expected []DirSwitchEntry) bool {
	want := make(map[string]DirSwitchEntry, len(expected))
	for _, entry := range expected {
		want[entry.Name] = entry
	}
	for _, entry := range actual {
		if expectedEntry, ok := want[entry.Name]; !ok || expectedEntry != entry {
			return false
		}
	}
	return true
}

func dirSwitchEntryConflict(label string, err error) error {
	if err == nil {
		return asTargetConflict(fmt.Errorf("%w: %s has an unexpected identity or type", ErrTxnConflict, label))
	}
	return asTargetConflict(fmt.Errorf("%w: %s changed: %w", ErrTxnConflict, label, err))
}

func statMode[T ~uint16 | ~uint32](mode T) uint32 {
	return uint32(mode) & uint32(unix.S_IFMT)
}

func dirSwitchSiblingName(parent string) (string, error) {
	return dirSwitchName(parent, ".fu-skills-")
}

func dirSwitchBackupName(parent string) (string, error) {
	return dirSwitchName(parent, ".fu-skills-old-")
}

func dirSwitchName(parent, prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate whole-directory switch name: %w", err)
	}
	return filepath.Join(parent, prefix+hex.EncodeToString(raw[:])), nil
}

func dirSwitchCleanupID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate whole-directory cleanup identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func dirSwitchRetiredName(sw *DirSwitchState, kind, original string) string {
	sum := sha256.Sum256([]byte(sw.CleanupID + "\x00" + kind + "\x00" + original))
	return ".fu-clean-" + sw.CleanupID + "-" + kind + "-" + hex.EncodeToString(sum[:6])
}
