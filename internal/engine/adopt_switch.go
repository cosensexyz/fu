// internal/engine/adopt_switch.go
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

var errAdoptArchiveBoundary = errors.New("adopt archive boundary hook failed")
var errAdoptObjectGone = errors.New("recorded adopt object is absent at both owned names")

// switchAdoptedEntries delivers every agent that held an adopted skill to
// its switched state: the original entry archived into recovery and gone,
// the store link left for the trailing reconcile to create. The function is
// idempotent and re-runnable -- recovery calls it again after a crash (plan
// D6). A per-agent failure (content changed since the inventory, an
// unreadable entry) isolates the agent by removing it from the transaction's
// agent list -- durably, so recovery will not retry it -- and the trailing
// reconcile then reports the remaining foreign entry as a conflict.
func switchAdoptedEntries(st *store.Store, agents []agent.Agent, name string, txn *TxnRecord) error {
	return switchAdoptedEntriesWithHooks(st, agents, name, txn, hooks{})
}

func switchAdoptedEntriesWithHooks(st *store.Store, agents []agent.Agent, name string, txn *TxnRecord, h hooks) error {
	_, err := switchAdoptedEntriesReporting(st, agents, name, txn, h)
	return err
}

type adoptIsolation struct {
	agent string
	err   error
}

// switchAdoptedEntriesReporting returns the reasons for agents it safely
// isolates before retirement. Recovery needs only the durable transaction
// edits, while the initiating adopt uses these details to tell the user how
// to repair an installed-but-unswitched skill.
func switchAdoptedEntriesReporting(st *store.Store, agents []agent.Agent, name string, txn *TxnRecord, h hooks) ([]adoptIsolation, error) {
	first := true
	var isolated []adoptIsolation
	for _, a := range agents {
		if err := switchAdoptEntryWithHooks(st, a, name, txn, h); err != nil {
			if errors.Is(err, errAdoptObjectGone) && txn.Archive != nil &&
				txn.Archive.Agent == a.Name() {
				// Neither recorded name is reachable through the identity-bound
				// parent. Keeping the archive plan cannot protect or recover an
				// object fu can no longer address, so isolate the agent durably.
				txn.Archive = nil
				txn.Agents = withoutAgent(txn.Agents, a.Name())
				if writeErr := WriteTxn(st, txn); writeErr != nil {
					return isolated, errors.Join(err, writeErr)
				}
				isolated = append(isolated, adoptIsolation{agent: a.Name(), err: err})
				continue
			}
			isolatableTarget := errors.Is(err, errAdoptTargetChanged) &&
				(txn.Archive == nil || (txn.Archive.Agent == a.Name() && txn.Archive.Stage == "planned"))
			if (errors.Is(err, ErrTxnConflict) && !isolatableTarget) || errors.Is(err, errAdoptArchiveBoundary) {
				return isolated, err
			}
			// Isolation is only safe while nothing of the user's has moved.
			// Past stage "planned" the original has been renamed to
			// .fu-adopt-retired-<hex>, so dropping the agent here would let
			// PostCommit succeed, ClearTxn close the WAL, and the command exit
			// 0 with the user's tree hidden under a random dot-name and the
			// recovery archive empty -- with nothing left to finish or report
			// it (round 18 finding C1). Propagate instead: the transaction
			// stays open and the next write command's recovery resumes it.
			if txn.Archive != nil {
				if abandonErr := abandonPlannedAdoptArchive(txn, a.Name()); abandonErr != nil {
					return isolated, errors.Join(err, abandonErr)
				}
			}
			// The failure is durable knowledge, not a transient error:
			// record it by removing the agent from the transaction's list so
			// recovery stops here and the reconcile reports the entry.
			txn.Agents = withoutAgent(txn.Agents, a.Name())
			if err := WriteTxn(st, txn); err != nil {
				return isolated, err
			}
			isolated = append(isolated, adoptIsolation{agent: a.Name(), err: err})
			continue
		}
		if first && h.afterAdoptSwitch != nil {
			first = false
			if err := h.afterAdoptSwitch(); err != nil {
				return isolated, err
			}
		}
	}
	return isolated, nil
}

func abandonPlannedAdoptArchive(txn *TxnRecord, agentName string) error {
	archive := txn.Archive
	if archive == nil {
		return nil
	}
	if archive.Agent != agentName {
		return fmt.Errorf("%w: archive plan belongs to agent %q, not %q", ErrTxnConflict, archive.Agent, agentName)
	}
	if archive.Stage != "planned" {
		return fmt.Errorf("%w: archive for agent %q has advanced to %q", ErrTxnConflict, agentName, archive.Stage)
	}
	// Nothing was moved yet, so the plan is abandoned with the agent rather
	// than left for a resume that will never come.
	txn.Archive = nil
	return nil
}

// switchAdoptEntry brings one agent's entry to the switched state:
//
//	fu link  -> nothing to do
//	absent   -> nothing to archive (reconcile will create the link)
//	foreign  -> archive the original to recovery, then delete it
//
// The parent's form is re-verified here, at execution time (round 7 finding
// I1): the classification happened earlier -- and for recovery, in a
// previous process -- so the skills directory may have become a symlink
// since. Switching through one would archive and delete the entry from the
// user's target (SPEC rule 10); such an agent is refused and left for the
// isolation path to report.
func switchAdoptEntry(st *store.Store, a agent.Agent, name string, txn *TxnRecord) error {
	return switchAdoptEntryWithHooks(st, a, name, txn, hooks{})
}

func switchAdoptEntryWithHooks(st *store.Store, a agent.Agent, name string, txn *TxnRecord, h hooks) error {
	target, err := targetForAgent(txn, a.Name())
	if err != nil {
		return err
	}
	// A whole-directory agent belongs to the directory swap, never to this
	// path: archiving its entry would delete the original from the user's own
	// target through the parent symlink (SPEC rule 10). The refusal is stated
	// here rather than left to pairBoundAdoptRoot's identity comparison, which
	// rejects it only incidentally (round 18 finding I2).
	if target.WholeDir {
		return fmt.Errorf("%w: agent %s uses the whole-directory form and must not be switched per entry", ErrTxnConflict, a.Name())
	}
	parent, err := openBoundAdoptParent(target)
	if err != nil {
		if txn.Archive != nil && txn.Archive.Agent == a.Name() {
			return fmt.Errorf("%w; archived original is recorded at %s", err,
				filepath.Join(target.SkillsDir, txn.Archive.Retired))
		}
		return err
	}
	defer parent.Close()
	parentRoot, err := pairBoundAdoptRoot(target.SkillsDir, parent, target.ParentIdentity)
	if err != nil {
		return asTargetConflict(err)
	}
	defer parentRoot.Close()
	if txn.Archive != nil {
		if txn.Archive.Agent != a.Name() {
			return fmt.Errorf("%w: archive for agent %q is still active while switching %q", ErrTxnConflict, txn.Archive.Agent, a.Name())
		}
		return archiveAdoptEntry(st, a, target, parent, parentRoot, name, txn, h, nil, nil)
	}
	entry, err := statAdoptEntry(int(parent.Fd()), name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if uint32(entry.Mode)&uint32(unix.S_IFMT) == uint32(unix.S_IFLNK) {
		raw, err := readAdoptLink(int(parent.Fd()), name)
		if err != nil {
			return err
		}
		if ownsLink(st.SkillsDir(), name, raw) {
			return nil
		}
	}
	validatedIdentity, validatedStat, err := validateCurrentAdoptEntryStat(parent, target, name)
	if err != nil {
		return err
	}
	return archiveAdoptEntry(st, a, target, parent, parentRoot, name, txn, h, &validatedIdentity, &validatedStat)
}

// archiveAdoptEntry copies one agent's original entry into recovery and
// deletes it. The copy lands directly in recovery (never staging) under a
// random-suffixed payload name recorded in the transaction, so a crash at
// any point leaves either nothing (re-copy to a fresh name), a partial
// payload (junk in recovery, ignored), or a complete validated one -- and
// the original is deleted only after the complete one is proven.
func archiveAdoptEntry(st *store.Store, a agent.Agent, target AdoptTarget, parent *os.File, parentRoot *os.Root, name string, txn *TxnRecord, h hooks, validatedIdentity *store.FileIdentity, validatedStat *unix.Stat_t) error {
	if txn.Archive == nil {
		if validatedIdentity == nil || validatedStat == nil {
			return fmt.Errorf("%w: cannot plan an adopt archive without a sealed entry stat", ErrTxnConflict)
		}
		if err := freshArchivePlan(st, a, target, parent, parentRoot, name, txn, *validatedIdentity, *validatedStat); err != nil {
			return err
		}
	}
	return resumeAdoptArchive(st, target, parent, parentRoot, name, txn, h)
}

// freshArchivePlan records all authority needed to retire and archive the
// original before the first namespace mutation. Recovery can therefore find
// the exact retired name even if the process stops immediately after rename.
func freshArchivePlan(st *store.Store, a agent.Agent, target AdoptTarget, parent *os.File, parentRoot *os.Root, name string, txn *TxnRecord, observedIdentity store.FileIdentity, entry unix.Stat_t) error {
	defer keepDescriptorOwnersAlive(parent)
	dir := filepath.Join(target.SkillsDir, name)
	srcRoot, err := openBoundAdoptSource(target)
	if err != nil {
		return fmt.Errorf("open recorded adopt source %s: %w", target.SourcePath, err)
	}
	defer srcRoot.Close()
	proj, err := skill.ProjectDir(srcRoot.FS(), ".")
	if err != nil {
		return fmt.Errorf("project original entry %s: %w", dir, err)
	}
	currentDigest, err := skill.DigestManifest(proj)
	if err != nil {
		return err
	}
	if txn.Digest != "" && currentDigest != txn.Digest {
		return fmt.Errorf("original entry %s changed since the inventory; refusing to archive it", dir)
	}
	retired, err := store.NewRetiredName(".fu-adopt-retired-")
	if err != nil {
		return err
	}
	txn.Archive = &AdoptArchive{
		Agent:            a.Name(),
		Retired:          retired,
		Stage:            "planned",
		OriginalIdentity: observedIdentity,
		OriginalMode:     uint32(checkedAgentFileMode(uint32(entry.Mode))),
		OriginalKind:     target.EntryKind,
		OriginalTarget:   target.LinkTarget,
	}
	if target.EntryKind == adoptEntrySymlink {
		record := perEntryAdoptLinkArchive(target, name, txn.Archive)
		archiveName, err := ensureAdoptLinkArchive(st, record)
		if err != nil {
			txn.Archive = nil
			return err
		}
		txn.Archive.LinkArchive = archiveName
	}
	if target.EntryKind == adoptEntryDirectory {
		exact, err := store.SnapshotRootOwnedForCopy(srcRoot, ".")
		if err != nil {
			txn.Archive = nil
			return fmt.Errorf("snapshot exact original entry %s: %w", dir, err)
		}
		payload, err := adoptArchivePayloadName(a.Name(), name)
		if err != nil {
			txn.Archive = nil
			return err
		}
		txn.Archive.Payload = payload
		txn.Archive.SourceManifest = &exact
	}
	return WriteTxn(st, txn)
}

func resumeAdoptArchive(st *store.Store, target AdoptTarget, parent *os.File, parentRoot *os.Root, name string, txn *TxnRecord, h hooks) error {
	archive := txn.Archive
	if archive == nil {
		return fmt.Errorf("%w: adopt archive state is absent", ErrTxnConflict)
	}
	if archive.Agent != target.Agent {
		return fmt.Errorf("%w: adopt archive agent %q does not match target %q", ErrTxnConflict, archive.Agent, target.Agent)
	}
	if archive.OriginalKind == adoptEntrySymlink {
		return resumeAdoptSymlinkRemoval(st, target, parent, name, txn, h)
	}
	if archive.SourceManifest == nil {
		return fmt.Errorf("%w: adopt archive has no exact source manifest", ErrTxnConflict)
	}

	switch archive.Stage {
	case "planned":
		retiredIdentity, err := ensureAdoptOriginalRetired(target, parent, name, archive, h.beforeAdoptRetire, h.entryIdentityAt)
		if err != nil {
			return err
		}
		archive.Stage = "retired"
		if err := WriteTxn(st, txn); err != nil {
			return err
		}
		if err := h.fire(h.afterAdoptRetiredJournal); err != nil {
			return pastRetireConflict(err)
		}
		return resumeRetiredAdoptArchive(st, target, parent, parentRoot, name, txn, retiredIdentity, h)
	case "retired":
		retiredIdentity, err := ensureAdoptOriginalRetired(target, parent, name, archive, nil, h.entryIdentityAt)
		if err != nil {
			return err
		}
		return resumeRetiredAdoptArchive(st, target, parent, parentRoot, name, txn, retiredIdentity, h)
	case "copied":
		// No rename or archive copy occurred in this invocation. The removal
		// primitive admits this persisted identity and retains its own live proof.
		return resumeCopiedAdoptArchive(st, target, parent, name, txn, archive.OriginalIdentity, h)
	case "cleaned":
		return completeAdoptArchive(st, parent, txn)
	default:
		return fmt.Errorf("%w: unknown adopt archive stage %q", ErrTxnConflict, archive.Stage)
	}
}

// resumeRetiredAdoptArchive carries the admitted live identity through archive
// copying. It must not re-admit the WAL: a local rename and a resumed retirement
// both arrive here with their proof supplied by the caller.
func resumeRetiredAdoptArchive(st *store.Store, target AdoptTarget, parent *os.File, parentRoot *os.Root, name string, txn *TxnRecord, retiredIdentity store.FileIdentity, h hooks) error {
	if err := h.fire(h.afterAdoptRetire); err != nil {
		return err
	}
	if err := copyExactAdoptArchive(st, target, parentRoot, txn); err != nil {
		return err
	}
	if err := h.fire(h.afterAdoptArchiveCopy); err != nil {
		return fmt.Errorf("%w: %w", errAdoptArchiveBoundary, err)
	}
	return resumeCopiedAdoptArchive(st, target, parent, name, txn, retiredIdentity, h)
}

func resumeCopiedAdoptArchive(st *store.Store, target AdoptTarget, parent *os.File, name string, txn *TxnRecord, retiredIdentity store.FileIdentity, h hooks) error {
	archive := txn.Archive
	if archive.Manifest == nil {
		return errors.New("adopt archive recorded as copied without a manifest")
	}
	if err := st.ValidateRecoveryPayloadOwned(archive.Payload, *archive.Manifest); err != nil {
		return fmt.Errorf("validate completed adopt archive: %w", err)
	}
	retiredPresent, err := adoptNamePresent(parent, archive.Retired)
	if err != nil {
		return err
	}
	if retiredPresent {
		if err := removeRetiredAdoptOriginal(st, target, parent, name, archive, retiredIdentity, h.entryIdentityAt); err != nil {
			return err
		}
	} else {
		originalPresent, err := adoptNamePresent(parent, name)
		if err != nil {
			return err
		}
		if originalPresent {
			return fmt.Errorf("%w: retired adopt entry disappeared while original name %s/%s is occupied", ErrTxnConflict, target.Agent, name)
		}
	}
	archive.Stage = "cleaned"
	if err := WriteTxn(st, txn); err != nil {
		return err
	}
	return completeAdoptArchive(st, parent, txn)
}

func completeAdoptArchive(st *store.Store, parent *os.File, txn *TxnRecord) error {
	archive := txn.Archive
	if archive.Manifest == nil {
		return errors.New("cleaned adopt archive has no recovery manifest")
	}
	if err := st.ValidateRecoveryPayloadOwned(archive.Payload, *archive.Manifest); err != nil {
		return fmt.Errorf("validate adopt archive before completion: %w", err)
	}
	retiredPresent, err := adoptNamePresent(parent, archive.Retired)
	if err != nil {
		return err
	}
	if retiredPresent {
		return fmt.Errorf("%w: cleaned adopt archive still has retired entry %q", ErrTxnConflict, archive.Retired)
	}
	txn.Archive = nil
	return WriteTxn(st, txn)
}

// resumeAdoptSymlinkRemoval removes only the link entry. Its identity, mode,
// and raw target are sufficient ownership evidence; copying or revalidating
// the external target would turn unrelated repository changes into permanent
// recovery dependencies.
func resumeAdoptSymlinkRemoval(st *store.Store, target AdoptTarget, parent *os.File, name string, txn *TxnRecord, h hooks) error {
	archive := txn.Archive
	if archive == nil || archive.OriginalKind != adoptEntrySymlink {
		return fmt.Errorf("%w: symlink removal has invalid archive state", ErrTxnConflict)
	}
	if err := validateAdoptLinkArchive(st, archive.LinkArchive, perEntryAdoptLinkArchive(target, name, archive)); err != nil {
		return err
	}
	// The record admits an already-retired link only. Every path that retires
	// it in this invocation must replace this value with the pre-move identity.
	retiredIdentity := archive.OriginalIdentity
	switch archive.Stage {
	case "planned":
		retiredPresent, err := adoptNamePresent(parent, archive.Retired)
		if err != nil {
			return err
		}
		originalPresent, err := adoptNamePresent(parent, name)
		if err != nil {
			return err
		}
		if retiredPresent && originalPresent {
			return fmt.Errorf("%w: original and retired adopt symlinks both exist for %s/%s", ErrTxnConflict, target.Agent, name)
		}
		if originalPresent {
			if retiredIdentity, err = ensureAdoptOriginalRetired(target, parent, name, archive, h.beforeAdoptRetire, h.entryIdentityAt); err != nil {
				return err
			}
			retiredPresent = true
		}
		if !retiredPresent {
			// Both names absent means an older process completed the unlink but
			// stopped before recording it. Persist the terminal phase and resume
			// through the ordinary cleaned validation.
			archive.Stage = "cleaned"
			if err := WriteTxn(st, txn); err != nil {
				return err
			}
			return resumeAdoptSymlinkRemoval(st, target, parent, name, txn, h)
		}
		// Record the namespace mutation immediately, as the directory form
		// does. From this point onward isolation is structurally forbidden even
		// if a later syscall reports a plain errno.
		archive.Stage = "retired"
		if err := WriteTxn(st, txn); err != nil {
			return err
		}
		if err := h.fire(h.afterAdoptRetire); err != nil {
			return pastRetireConflict(err)
		}
		fallthrough
	case "retired":
		originalPresent, err := adoptNamePresent(parent, name)
		if err != nil {
			return pastRetireConflict(err)
		}
		if originalPresent {
			return fmt.Errorf("%w: original adopt symlink reappeared for %s/%s after retirement", ErrTxnConflict, target.Agent, name)
		}
		retiredPresent, err := adoptNamePresent(parent, archive.Retired)
		if err != nil {
			return pastRetireConflict(err)
		}
		if retiredPresent {
			if err := removeRetiredAdoptOriginal(st, target, parent, name, archive, retiredIdentity, h.entryIdentityAt); err != nil {
				return pastRetireConflict(err)
			}
		}
		// A missing retired name is complete too: a crash may have happened
		// after RemoveOwnedSymlinkAt and before this revision was appended.
		archive.Stage = "cleaned"
		if err := WriteTxn(st, txn); err != nil {
			return err
		}
		fallthrough
	case "cleaned":
		for _, candidate := range []string{name, archive.Retired} {
			present, err := adoptNamePresent(parent, candidate)
			if err != nil {
				return err
			}
			if present {
				return fmt.Errorf("%w: cleaned adopt symlink still exists at %q", ErrTxnConflict, candidate)
			}
		}
		txn.Archive = nil
		return WriteTxn(st, txn)
	default:
		return fmt.Errorf("%w: symlink adopt has invalid archive stage %q", ErrTxnConflict, archive.Stage)
	}
}

func ensureAdoptOriginalRetired(target AdoptTarget, parent *os.File, name string, archive *AdoptArchive, beforeRetire func() error, capture func(int, string) (store.FileIdentity, unix.Stat_t, error)) (store.FileIdentity, error) {
	if capture == nil {
		capture = store.EntryIdentityAt
	}
	retiredPresent, err := adoptNamePresent(parent, archive.Retired)
	if err != nil {
		return store.FileIdentity{}, err
	}
	originalPresent, err := adoptNamePresent(parent, name)
	if err != nil {
		return store.FileIdentity{}, err
	}
	if retiredPresent {
		if originalPresent {
			originalPath := filepath.Join(target.SkillsDir, name)
			retiredPath := filepath.Join(target.SkillsDir, archive.Retired)
			return store.FileIdentity{}, fmt.Errorf("%w: original adopt entry %s and retired adopt entry %s both exist; move one aside and retry", ErrTxnConflict, originalPath, retiredPath)
		}
		// The rename already happened, in this run or a previous process. Any
		// failure from here is past the point where isolation is safe, so it
		// says so structurally rather than leaving switchAdoptedEntriesWithHooks
		// to infer it from archive.Stage. A plain error escaping here let the
		// isolation arm clear txn.Archive and ClearTxn close the WAL with the
		// user's entry still parked under .fu-adopt-retired-<hex> -- exactly
		// the shape round 18 finding C1 closed one level up.
		observed, err := observeRetiredAdoptOriginalWithCapture(parent, target, archive, archive.OriginalIdentity, capture)
		return observed, pastRetireConflict(err)
	}
	if !originalPresent {
		return store.FileIdentity{}, asTargetConflict(fmt.Errorf("%w: %w: both original adopt entry %s and retired adopt entry %s are absent",
			ErrTxnConflict, errAdoptObjectGone,
			filepath.Join(target.SkillsDir, name), filepath.Join(target.SkillsDir, archive.Retired)))
	}
	observedIdentity, _, err := validateCurrentAdoptEntryStatWithCapture(parent, target, name, target.EntryIdentity, capture)
	if err != nil {
		return store.FileIdentity{}, err
	}
	if beforeRetire != nil {
		if err := beforeRetire(); err != nil {
			return store.FileIdentity{}, fmt.Errorf("%w: %w", errAdoptArchiveBoundary, err)
		}
	}
	if err := store.RenameNoReplaceAt(parent, name, parent, archive.Retired); err != nil {
		return store.FileIdentity{}, fmt.Errorf("retire adopted original %s/%s: %w", target.Agent, name, err)
	}
	if _, err := observeRetiredAdoptOriginalWithCapture(parent, target, archive, observedIdentity, capture); err != nil {
		restoreErr := store.RestoreRetiredAt(parent, archive.Retired, name)
		if restoreErr != nil {
			// The restore failed, so the user's entry is still under the
			// retired name: past the rename, and isolating the agent here
			// would close the WAL over it.
			return store.FileIdentity{}, pastRetireConflict(errors.Join(err, fmt.Errorf("restore mismatched retired original: %w", restoreErr)))
		}
		// Restored: nothing of the user's remains under the retired name, but
		// the artifact mismatch is still an ErrTxnConflict hard stop because
		// the transaction's ownership evidence is no longer trustworthy.
		return store.FileIdentity{}, err
	}
	return observedIdentity, nil
}

// pastRetireConflict marks an error raised after the adopted original has been
// renamed aside and not put back. switchAdoptedEntriesWithHooks may isolate an
// agent only while nothing of the user's has moved; it infers that today from
// archive.Stage, and these two paths could return past the rename with the
// stage still "planned". Tagging the error makes the property structural
// instead of inferred -- the one invariant round 18 finding C1's fix rests on.
func pastRetireConflict(err error) error {
	if err == nil || errors.Is(err, ErrTxnConflict) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrTxnConflict, err)
}

func observeRetiredAdoptOriginalWithCapture(parent *os.File, target AdoptTarget, archive *AdoptArchive, expectedIdentity store.FileIdentity, capture func(int, string) (store.FileIdentity, unix.Stat_t, error)) (store.FileIdentity, error) {
	if capture == nil {
		capture = store.EntryIdentityAt
	}
	defer keepDescriptorOwnersAlive(parent)
	entryIdentity, entry, err := capture(int(parent.Fd()), archive.Retired)
	if err != nil {
		return store.FileIdentity{}, err
	}
	mode := checkedAgentFileMode(uint32(entry.Mode))
	if !entryIdentity.Same(expectedIdentity) || uint32(mode) != archive.OriginalMode {
		return store.FileIdentity{}, fmt.Errorf("%w: retired adopt entry for %s changed identity or mode", ErrTxnConflict, target.Agent)
	}
	switch archive.OriginalKind {
	case adoptEntryDirectory:
		if mode.Type() != os.ModeDir {
			return store.FileIdentity{}, fmt.Errorf("%w: retired adopt entry for %s changed type", ErrTxnConflict, target.Agent)
		}
	case adoptEntrySymlink:
		if mode.Type() != os.ModeSymlink {
			return store.FileIdentity{}, fmt.Errorf("%w: retired adopt entry for %s changed type", ErrTxnConflict, target.Agent)
		}
		raw, err := readAdoptLink(int(parent.Fd()), archive.Retired)
		if err != nil {
			return store.FileIdentity{}, err
		}
		if raw != archive.OriginalTarget {
			return store.FileIdentity{}, fmt.Errorf("%w: retired adopt symlink for %s changed target", ErrTxnConflict, target.Agent)
		}
	default:
		return store.FileIdentity{}, fmt.Errorf("%w: unknown retired adopt entry kind %q", ErrTxnConflict, archive.OriginalKind)
	}
	return entryIdentity, nil
}

func copyExactAdoptArchive(st *store.Store, target AdoptTarget, parentRoot *os.Root, txn *TxnRecord) error {
	archive := txn.Archive
	if archive.Base != nil {
		payload, err := adoptArchivePayloadName(archive.Agent, txn.Name)
		if err != nil {
			return err
		}
		archive.Payload = payload
		archive.Base = nil
		archive.Manifest = nil
		if err := WriteTxn(st, txn); err != nil {
			return err
		}
	}
	source, err := openRetiredAdoptSource(target, parentRoot, archive)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := store.ValidateRootOwned(source, ".", *archive.SourceManifest); err != nil {
		return fmt.Errorf("validate retired adopt source: %w", err)
	}
	var base store.OwnedTree
	for {
		base, err = st.CreateRecoveryRootOwned(archive.Payload, os.FileMode(archive.SourceManifest.RootMode).Perm())
		if !errors.Is(err, fs.ErrExist) {
			break
		}
		payload, nameErr := adoptArchivePayloadName(archive.Agent, txn.Name)
		if nameErr != nil {
			return nameErr
		}
		archive.Payload = payload
		archive.Base = nil
		archive.Manifest = nil
		if writeErr := WriteTxn(st, txn); writeErr != nil {
			return fmt.Errorf("record fresh recovery payload after collision: %w", writeErr)
		}
	}
	if err != nil {
		return fmt.Errorf("create recovery payload %s: %w", archive.Payload, err)
	}
	archive.Base = &base
	if err := WriteTxn(st, txn); err != nil {
		return err
	}
	manifest, err := st.CopyTreeToRecoveryExactOwned(archive.Payload, base, source, ".", *archive.SourceManifest)
	if err != nil {
		return fmt.Errorf("copy exact original entry into recovery: %w", err)
	}
	archive.Base = nil
	archive.Manifest = &manifest
	archive.Stage = "copied"
	return WriteTxn(st, txn)
}

func openRetiredAdoptSource(target AdoptTarget, parentRoot *os.Root, archive *AdoptArchive) (*os.Root, error) {
	if archive.OriginalKind == adoptEntryDirectory {
		root, err := parentRoot.OpenRoot(archive.Retired)
		if err != nil {
			return nil, fmt.Errorf("%w: open retired adopt directory: %v", ErrTxnConflict, err)
		}
		return root, nil
	}
	return openBoundAdoptSource(target)
}

func removeRetiredAdoptOriginal(st *store.Store, target AdoptTarget, parent *os.File, name string, archive *AdoptArchive, expectedIdentity store.FileIdentity, capture func(int, string) (store.FileIdentity, unix.Stat_t, error)) error {
	observedIdentity, err := observeRetiredAdoptOriginalWithCapture(parent, target, archive, expectedIdentity, capture)
	if err != nil {
		return err
	}
	switch archive.OriginalKind {
	case adoptEntryDirectory:
		if archive.SourceManifest == nil {
			return fmt.Errorf("%w: retired directory has no exact manifest", ErrTxnConflict)
		}
		manifest := *archive.SourceManifest
		manifest.RootIdentity = observedIdentity
		if err := store.RemoveOwnedTreeAt(parent, archive.Retired, manifest); err != nil {
			return fmt.Errorf("remove retired adopted original %s/%s: %w", target.Agent, name, err)
		}
	case adoptEntrySymlink:
		if err := validateAdoptLinkArchive(st, archive.LinkArchive, perEntryAdoptLinkArchive(target, name, archive)); err != nil {
			return err
		}
		if err := store.RemoveOwnedSymlinkAt(parent, archive.Retired, observedIdentity, archive.OriginalMode, archive.OriginalTarget); err != nil {
			return fmt.Errorf("remove retired adopted symlink %s/%s: %w", target.Agent, name, err)
		}
	}
	return nil
}

func perEntryAdoptLinkArchive(target AdoptTarget, name string, archive *AdoptArchive) adoptLinkArchiveRecord {
	return newAdoptLinkArchiveRecord(
		adoptLinkArchiveEntry, archive.Agent, name, filepath.Join(target.SkillsDir, name),
		archive.OriginalTarget, archive.OriginalMode, archive.OriginalIdentity,
	)
}

func adoptNamePresent(parent *os.File, name string) (bool, error) {
	defer keepDescriptorOwnersAlive(parent)
	_, err := statAdoptEntry(int(parent.Fd()), name)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// adoptArchivePayloadName builds the recovery payload name for one agent's
// archived original. The random suffix makes an interrupted attempt's
// partial payload unreachable by later attempts (each re-copy uses a fresh
// name); the prefix carries the owning transaction's identity via the
// record, not the filename.
func adoptArchivePayloadName(agentName, skillName string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate adopt archive payload name: %w", err)
	}
	return fmt.Sprintf("adopt-archive-%s-%s-%s", agentName, skillName, hex.EncodeToString(raw[:])), nil
}
