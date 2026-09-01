// internal/engine/update_txn.go
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

// The stages an update transaction passes through. The exchange is the pivot;
// everything before it is reversible by discarding staging, everything after it
// is reversible only by exchanging back.
const (
	updateTxnSnapshotted = "snapshotted"
	updateTxnStaged      = "staged"
	// updateTxnExchanged is written for the record, not for a decision. The
	// pipeline overwrites it with "published" immediately (run, pipeline.go),
	// updatePastStaging treats the two identically, and recoverUpdate decides
	// the exchange side by matching manifests rather than by stage -- as its own
	// doc insists it must, since update journals the exchange after performing
	// it. It stays because a post-mortem reader of a journal family should be
	// able to see that the exchange itself was reached (round 2, Minor #8).
	updateTxnExchanged = "exchanged"
)

func init() {
	RegisterRecoverHandler("update", recoverUpdate)
}

// updatePastStaging reports whether stage is at or past updateTxnStaged, the
// revision from which both manifests are journalled and the exchange becomes
// possible.
//
// The stages do not run in the order this file declares them: the pipeline
// writes "config-saved" between updateTxnStaged and updateTxnExchanged, and
// "published" after the exchange (pipeline.go). What they share is the only
// property recovery reads them for -- both manifests are on record.
func updatePastStaging(stage string) bool {
	switch stage {
	case updateTxnStaged, updateTxnExchanged, "config-saved", "published":
		return true
	default:
		return false
	}
}

// validateUpdateRecord rejects an update journal entry that cannot drive
// recovery. Past updateTxnStaged both manifests must be present: with either
// one missing, recovery cannot tell which side of the exchange it is looking at
// and would have to guess at a destructive step.
func validateUpdateRecord(r TxnRecord) error {
	if r.Op != "update" {
		return fmt.Errorf("not an update transaction: %q", r.Op)
	}
	if r.Name == "" {
		return errors.New("update transaction has no skill name")
	}
	switch {
	case r.Stage == "started" || r.Stage == updateTxnSnapshotted:
		// Before the staged revision the manifests are still being assembled,
		// so neither is required here. Both can nonetheless be present:
		// update journals the tree it is about to replace at
		// updateTxnSnapshotted (update.go), and createTxnStagedRoot records
		// the staging root it published under Payload while that same stage is
		// still in force (staging.go) -- a root-only manifest describing a
		// directory the copy has not filled in yet. A non-nil Payload is
		// therefore not evidence that the staged tree exists.
		//
		// Recovery asks two questions and they take their answers from
		// different places, which is worth keeping apart: whether the staged
		// tree exists at all is decided from the stage and never from
		// Payload's nil-ness, while which side of the exchange the published
		// name is on is decided by matching a manifest against it and never
		// from the stage (recoverUpdate says why).
	case updatePastStaging(r.Stage):
		if r.Payload == nil {
			return errors.New("update transaction past staging has no staged manifest")
		}
		if err := r.Payload.Validate(); err != nil {
			return fmt.Errorf("update transaction has invalid staged manifest: %w", err)
		}
		if r.PreviousPayload == nil {
			return errors.New("update transaction past staging has no replaced manifest")
		}
		if err := r.PreviousPayload.Validate(); err != nil {
			return fmt.Errorf("update transaction has invalid replaced manifest: %w", err)
		}
	default:
		return fmt.Errorf("update transaction has unknown stage %q", r.Stage)
	}
	return nil
}

// recoverUpdate drives an interrupted update to a terminal state: completed
// (the operation commit exists, the exchanged tree validates against the staged
// manifest, the WAL is cleared and the replaced tree reclaimed after it) or
// rolled back (the replaced tree published again, the staged replacement
// discarded, fu.yaml restored).
//
// The transaction turns on one filesystem pivot -- the single atomic
// ExchangeStagedWithSkillOwned in Publish (update.go) -- so recovery has two
// questions to answer: did the commit land, and which of the two recorded trees
// is at skills/<name> now -- or neither of them. That crosses to six states,
// five of them reachable, decided by four rows because the last covers two:
//
//	commit landed | skills/<name> holds    | action
//	no            | the replaced tree      | discard staging/<name>, restore fu.yaml
//	no            | the staged replacement | exchange back, then as above
//	yes           | the staged replacement | reclaim staging/<name>, clear the WAL
//	any           | neither                | refuse, keep the WAL, report
//
// The one state no row names is the committed transaction whose skills/<name>
// still holds the tree it replaced, and what makes it unreachable is committed
// implies exchanged *by fu*: the pipeline runs Publish strictly before it
// prepares and writes the operation commit (pipeline.go), so no commit of this
// transaction can exist unless the exchange completed first. It is not
// unreachable against an outside writer -- one that swaps the two real trees
// back reproduces it exactly, the inode-preserving move StagingRootMatches's
// own doc warns about -- and neither is the state where skills/<name> matches
// nothing. Both are the same event with different aim, and both land on the
// last row, which is why that row is written to hold from either side of the
// commit: a published name fu cannot account for is the same refusal whatever
// HEAD says.
//
// Both manifests still identify the right trees after the exchange because
// renameExchange preserves each directory's inode: a manifest names an
// *object*, and the object travels with the rename rather than staying with
// the name. Which name holds which manifest therefore depends on which side of
// the swap the state is on. Before it, Payload is the staged replacement at
// staging/<name> and PreviousPayload the published tree at skills/<name>;
// after it the two names have traded contents, so Payload is what sits at
// skills/<name> and PreviousPayload what sits at staging/<name>. So naming a
// manifest against a staging path means knowing which side of the swap the
// state is on, and no consumer here assumes one pairing for both: each
// establishes the side first, and they do not all land on the same answer.
// txn_prune.go's staging arm and status.go's update arm see only completed
// families, where the exchange has happened, so staging/<name> is
// PreviousPayload's. The two in this file read the other way round, and are
// right to: restoreExchangedUpdate settles the side at skills/<name> first --
// matching there, never at the staging name, is what tells the two apart -- and
// by the time it names staging, the pre-exchange arrangement holds again,
// either because the exchange never ran or because it has just been undone, so
// what it discards there is Payload's tree. discardUpdateStagingResidue reads
// Payload for the same reason from the other end: it runs on a record whose
// exchange demonstrably never happened.
//
// Naming an object rather than a location is what makes matching a manifest a
// decision procedure rather than a guess -- and it is why the exchange side is
// decided by matching rather than by the recorded stage. Update journals the
// exchange only after performing it, so a crash in that window leaves a record
// whose stage says "not yet" about a swap that already happened; the manifests
// say what actually happened.
func recoverUpdate(st *store.Store, record TxnRecord) error {
	if err := validateUpdateRecord(record); err != nil {
		return err
	}
	if err := skill.ValidateName(record.Name); err != nil {
		return fmt.Errorf("invalid skill name in update transaction: %w", err)
	}
	if !plumbing.IsHash(record.StartHead) {
		return fmt.Errorf("update transaction has invalid start HEAD %q", record.StartHead)
	}
	if record.Message != "update: "+record.Name {
		return fmt.Errorf("update transaction message %q does not match skill %q", record.Message, record.Name)
	}
	wantTargets := []string{
		filepath.Join("staging", record.Name),
		filepath.Join("store", "skills", record.Name),
	}
	if len(record.Targets) != len(wantTargets) || record.Targets[0] != wantTargets[0] || record.Targets[1] != wantTargets[1] {
		return fmt.Errorf("update transaction targets %q do not match skill %q", record.Targets, record.Name)
	}
	if len(record.ConfigBefore) == 0 {
		return errors.New("update transaction has no starting config snapshot")
	}
	storeRoot, err := st.StoreRoot()
	if err != nil {
		return fmt.Errorf("recover update transaction through checked repository root: %w", err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return fmt.Errorf("recover update transaction through checked staging root: %w", err)
	}
	startHash := plumbing.NewHash(record.StartHead)
	startCommit, err := st.Repo.CommitObject(startHash)
	if err != nil {
		return fmt.Errorf("load update transaction start commit %s: %w", record.StartHead, err)
	}
	startFile, err := startCommit.File("fu.yaml")
	if err != nil {
		return fmt.Errorf("load fu.yaml from update transaction start commit: %w", err)
	}
	startConfig, err := startFile.Contents()
	if err != nil {
		return fmt.Errorf("read fu.yaml from update transaction start commit: %w", err)
	}
	if !bytes.Equal([]byte(startConfig), record.ConfigBefore) {
		return fmt.Errorf("%w: recorded starting config does not match %s", ErrTxnConflict, record.StartHead)
	}

	currentHead, err := st.Repo.Head()
	if err != nil {
		return fmt.Errorf("read HEAD while recovering update transaction: %w", err)
	}
	if currentHead.Hash() == startHash {
		return rollBackUncommittedUpdate(st, storeRoot, stagingRoot, record)
	}
	return finishCommittedUpdate(st, storeRoot, record, startHash, currentHead.Hash())
}

// expectedUpdatedConfig reconstructs the config a completed update wrote:
// ConfigBefore with this skill's baseline advanced to the staged tree and its
// source record replaced, which is exactly what the operation's Mutate does
// (update.go).
//
// The two sides produce identical bytes for two different reasons, and only one
// of them comes free. Keys already recorded under source are rewritten in
// place, in the order the file already held them, so both sides agree on those
// simply by starting from the same ConfigBefore bytes. Keys that have to be
// appended are gathered by ranging over the fields map, and Go does not promise
// that order repeats from one iteration to the next -- so on those, this
// comparison does depend on the sort SetSourceFields applies before appending
// them. Its own doc comment (config.go) names this comparison as the reason
// that sort is there; keep the two in step rather than re-deriving the argument
// here.
//
// The dependency is defensive, not live: nothing reachable through `fu update`
// makes the order observable today. A bare-commit pin would -- EncodeFields
// records ref, ref_kind and commit independently (source.go), so advancing one
// to a resolved branch appends two keys at once -- but no such row is ever
// updated. judgeGitUpdate (outdated.go) leaves any ref_kind other than "branch"
// non-comparable, both arms of selectUpdateTargets (application.go) require
// Comparable, and SPEC rule 9 requires exactly that of a fixed lock. For a
// comparable git row the keys update writes are the keys already recorded, so
// SetSourceFields appends nothing at all.
//
// Recorded rather than dropped because nothing in this function's contract
// restricts SourceFields to that subset, and the failure the order prevents is
// silent and permanent: a committed transaction whose config can never be
// reconstructed stays pending forever. TestRecoveredConfigMatchesOperationBytes
// (invariant_test.go) is the only place enforcing it for update, and it builds
// the bare-commit shape deliberately for that reason.
//
// The digest is taken from the record when it is there and derived from the
// staged manifest when it is not. Update sets txn.Digest in memory while
// staging and only makes it durable in the pipeline's "config-saved" revision,
// so a record that stopped at updateTxnStaged carries the manifest but not the
// digest -- and the two agree by construction, because the operation itself
// computes the digest with digestOwnedPayload over this same manifest.
//
// Only meaningful past staging, where the staged manifest exists.
func expectedUpdatedConfig(record TxnRecord) ([]byte, error) {
	cfg, err := store.LoadConfigBytes(record.ConfigBefore, "fu.yaml at transaction start")
	if err != nil {
		return nil, fmt.Errorf("parse update transaction starting config: %w", err)
	}
	if !cfg.HasSkill(record.Name) {
		return nil, fmt.Errorf("%w: skill %q did not exist at transaction start", ErrTxnConflict, record.Name)
	}
	digest := record.Digest
	if digest == "" {
		digest, err = digestOwnedPayload(*record.Payload)
		if err != nil {
			return nil, fmt.Errorf("digest the staged tree recorded by the update transaction: %w", err)
		}
	}
	cfg.SetDigest(record.Name, digest)
	cfg.SetSourceFields(record.Name, record.SourceFields)
	out, err := cfg.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode reconstructed update transaction config: %w", err)
	}
	return out, nil
}

// rollBackUncommittedUpdate restores the starting state: the replaced tree back
// at skills/<name> (either it never left, or it comes back through a second
// exchange), the staged replacement discarded, config restored, WAL cleared.
//
// Each step is idempotent and ordered so a crash between any two of them leaves
// a state this same function converges from: the filesystem is settled first
// and the config last, and once the trees are back where they started the
// remaining work is recognisable from the first row of the table again.
func rollBackUncommittedUpdate(st *store.Store, storeRoot, stagingRoot *os.Root, record TxnRecord) error {
	currentConfig, err := store.ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		return err
	}
	if updatePastStaging(record.Stage) {
		if record.Payload == nil || record.PreviousPayload == nil {
			return fmt.Errorf("%w: staged update transaction has no staged and replaced manifests", ErrTxnConflict)
		}
		expectedConfig, err := expectedUpdatedConfig(record)
		if err != nil {
			return err
		}
		if !bytes.Equal(currentConfig, record.ConfigBefore) && !bytes.Equal(currentConfig, expectedConfig) {
			return fmt.Errorf("%w: fu.yaml is neither the starting nor the expected updated-skill config", ErrTxnConflict)
		}
		if err := restoreExchangedUpdate(st, stagingRoot, record); err != nil {
			return err
		}
	} else {
		// Mutate only returns once it has journalled updateTxnStaged, and the
		// config is saved after it returns, so a record that stopped earlier
		// cannot have had its config written. Anything else in fu.yaml arrived
		// from outside this transaction and is not recovery's to overwrite.
		if !bytes.Equal(currentConfig, record.ConfigBefore) {
			return fmt.Errorf("%w: fu.yaml changed before the interrupted update could save it", ErrTxnConflict)
		}
		if err := discardUpdateStagingResidue(st, stagingRoot, record); err != nil {
			return err
		}
	}
	if err := restoreTxnConfig(st, currentConfig, record.ConfigBefore); err != nil {
		return err
	}
	return ClearTxn(st, record)
}

// restoreExchangedUpdate settles the published name and then the staging name
// for a transaction that got as far as journalling both manifests.
//
// Matching is the whole decision. The exchange is one atomic rename pair, so
// skills/<name> holds one tree or the other and never a mixture, and each
// manifest identifies its own tree by identity, mode and content together, so
// a match says the object here is one of the two fu recorded. That is the
// property, and it is weaker than unforgeability: a same-UID writer can make
// either manifest match by moving one of the two real trees onto the other's
// name -- both are real objects with real inodes. The decision stays safe
// under that, because exchanging them back merely undoes a swap this function
// correctly detected. Matching neither is the one case fu must not act on:
// both possible actions are destructive, and choosing between them would be a
// guess about content fu did not put there.
func restoreExchangedUpdate(st *store.Store, stagingRoot *os.Root, record TxnRecord) error {
	replacedErr := st.ValidateSkillOwned(record.Name, *record.PreviousPayload)
	if replacedErr != nil {
		stagedErr := st.ValidateSkillOwned(record.Name, *record.Payload)
		if stagedErr != nil {
			return fmt.Errorf(
				"%w: %s matches neither the tree the update replaced (%v) nor the replacement it staged (%v)",
				ErrTxnConflict, filepath.Join(st.SkillsDir(), record.Name), replacedErr, stagedErr)
		}
		// The exchange ran, the commit did not. Undo it with the same call that
		// performed it, arguments swapped: ExchangeStagedWithSkillOwned binds
		// each manifest to a location rather than to a role, validating the
		// second against the staging root and the third against the skills
		// root, and the two trees have traded places since the forward call.
		if err := st.ExchangeStagedWithSkillOwned(record.Name, *record.PreviousPayload, *record.Payload); err != nil {
			return mapInstallOwnershipError("exchange the replaced tree back into the store", err)
		}
	}
	// Either way staging/<name> now holds the staged replacement, which is what
	// this rollback exists to throw away: the tree that was published when the
	// transaction started is the one the user keeps.
	return discardStagedUpdateTree(st, stagingRoot, record.Name, *record.Payload)
}

// discardUpdateStagingResidue clears whatever an update left under staging
// before it journalled updateTxnStaged. No exchange can have happened yet, so
// the published tree is untouched and staging is the only place to clean up.
//
// Three shapes can be found there, one per durable boundary createTxnStagedRoot
// crosses (staging.go): nothing at all, a private reservation not yet renamed
// to the skill's name, and a published root the copy may have partly filled in.
// The last two are settled into one exact manifest before anything is removed,
// as an interrupted install settles its own declarations
// (rollBackUncommittedInstall).
//
// It is deliberately three checks shorter than that install path, because each
// of them is doing work update does not need, and restoring them would break
// what the shortness buys:
//
//   - install refuses when the reservation is present at both names or at
//     neither. Both is already impossible to act on wrongly here:
//     PublishStagedRootOwned renames without replacing, so it fails rather than
//     landing on top of the final name. Neither is benign for update in a way
//     it is not for install -- no exchange can have run this early, so the
//     published tree is untouched and the residue is simply already gone.
//   - install validates the already-published branch with ValidateStagedOwned.
//     RemoveOwnedTreeAt performs the same comparison in its own all-or-nothing
//     preflight before it moves anything, so the check would only be run twice.
//   - install writes the settled manifest back to the journal. Nothing here
//     reads it afterwards, and a later pass re-derives the same manifest from
//     the same record. Adding the write-back would also have to make the live
//     tree a precondition of settling, which is exactly what the absent-name
//     path below must not require: the root-only manifest is what lets
//     RemoveOwnedTreeAt resume from its retired name.
func discardUpdateStagingResidue(st *store.Store, stagingRoot *os.Root, record TxnRecord) error {
	payload := record.Payload
	if record.StagingReservation != nil {
		reservation := *record.StagingReservation
		privatePresent, err := txnPathPresent(stagingRoot, reservation.Name)
		if err != nil {
			return err
		}
		manifest := reservation.Manifest
		if privatePresent {
			manifest, err = st.PublishStagedRootOwned(reservation, record.Name)
			if err != nil {
				return mapInstallOwnershipError("recover the update's staged-root reservation", err)
			}
		}
		payload = &manifest
	}
	if payload == nil {
		return nil // nothing was ever created under staging
	}
	settled := *payload
	if len(record.Declared) > 0 {
		present, err := txnPathPresent(stagingRoot, record.Name)
		if err != nil {
			return err
		}
		// Settling reads the live tree, so it only applies while there is one.
		// With the name already gone the root-only manifest is what the removal
		// below wants anyway: it is enough to recognise and finish a removal
		// interrupted after the emptied root was retired (retire.go).
		if present {
			settled, err = st.SettleDeclaredStagedEntries(record.Name, *payload, record.Declared)
			if err != nil {
				return mapInstallOwnershipError("settle the update's declared staging entries", err)
			}
		}
	}
	return discardStagedUpdateTree(st, stagingRoot, record.Name, settled)
}

// discardStagedUpdateTree removes one manifested tree from the staging root.
//
// It is called unconditionally rather than only when the name is occupied:
// RemoveOwnedTreeAt returns without complaint when there is nothing left, and
// resumes from the deterministic retired name an interrupted removal parks the
// root at -- a state only it can recognise (retire.go).
func discardStagedUpdateTree(st *store.Store, stagingRoot *os.Root, name string, expected store.OwnedTree) error {
	dir, err := stagingRoot.Open(".")
	if err != nil {
		return fmt.Errorf("open the checked staging root to discard %s: %w", filepath.Join(st.StagingDir(), name), err)
	}
	defer func() { _ = dir.Close() }()
	if err := store.RemoveOwnedTreeAt(dir, name, expected); err != nil {
		return mapInstallOwnershipError("discard the update's staged content", err)
	}
	return nil
}

// finishCommittedUpdate validates the operation commit and the state it must
// have published, clears the WAL, and reclaims the replaced tree the exchange
// left in staging. A committed update is its own end state: there is no
// compensation commit, because the content it replaced stays recoverable from
// the store's git history (SPEC rule 3).
func finishCommittedUpdate(st *store.Store, storeRoot *os.Root, record TxnRecord, startHash, currentHash plumbing.Hash) error {
	currentCommit, err := st.Repo.CommitObject(currentHash)
	if err != nil {
		return err
	}
	if len(currentCommit.ParentHashes) != 1 || currentCommit.ParentHashes[0] != startHash || currentCommit.Message != record.Message {
		return fmt.Errorf("%w: commit %s is not the recorded update commit based on %s", ErrTxnConflict, currentHash, startHash)
	}
	if record.CommitTree == "" {
		return fmt.Errorf("%w: committed update transaction has no prepared operation-tree fingerprint", ErrTxnConflict)
	}
	fingerprint, err := st.CommitTreeFingerprint(currentHash)
	if err != nil {
		return fmt.Errorf("fingerprint recorded update commit %s: %w", currentHash, err)
	}
	if fingerprint != record.CommitTree {
		return fmt.Errorf("%w: commit %s tree %s does not match prepared operation tree %s", ErrTxnConflict, currentHash, fingerprint, record.CommitTree)
	}
	// The commit is this transaction's, and the pipeline writes it strictly
	// after Publish, so both manifests were journalled long before it. A record
	// reaching here without them is not a state fu can produce.
	if record.Payload == nil || record.PreviousPayload == nil {
		return fmt.Errorf("%w: committed update transaction has no staged and replaced manifests", ErrTxnConflict)
	}
	expectedConfig, err := expectedUpdatedConfig(record)
	if err != nil {
		return err
	}
	currentConfig, err := store.ReadConfigFileRoot(storeRoot, "fu.yaml")
	if err != nil {
		return err
	}
	if !bytes.Equal(currentConfig, expectedConfig) {
		return fmt.Errorf("%w: committed update transaction has unexpected config", ErrTxnConflict)
	}
	if err := st.ValidateSkillOwned(record.Name, *record.Payload); err != nil {
		return fmt.Errorf("%w: %s does not hold the replacement the committed update published: %v",
			ErrTxnConflict, filepath.Join(st.SkillsDir(), record.Name), err)
	}
	if err := ClearTxn(st, record); err != nil {
		return err
	}
	// Strictly after the terminal marker, and with its error dropped, for the
	// reasons the operation's own reclaim documents (update.go): the update is
	// durably committed, so nothing this does may turn it into a failure, and
	// nothing is waiting on the tree it disposes of.
	reclaimExchangedUpdatePayload(st, record)
	return nil
}
