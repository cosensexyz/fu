package engine

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cosensexyz/fu/internal/store"
)

// PruneOutcome reports what one gc run removed from the recovery directory:
// completed transaction families, and the total files taken with them --
// journal entries plus the config exchange bookkeeping swept alongside them.
//
// Files counts files, and only some of the ones under the recovery directory:
// the journal entries a family is made of and the config exchange bookkeeping.
// The .pruned record each family is deleted through is not among them -- it is
// written by this run and removed by it, so counting it would report a file the
// user never had. The two trees a run can also reclaim -- a completed rm
// family's quarantined payload under recovery/, and the tree a completed update
// family replaced under staging/ -- are deliberately not tallied either. They
// are the same thing as each other: a directory removed whole against its
// manifest, by a deletion
// primitive whose signature is not worth widening to return a count for one
// line of output. Counting one of the two and not the other, which this type
// briefly did, left `fu gc` reporting a staging directory tree as a "recovery
// journal and bookkeeping file". Neither reclaim goes unreported: each happens
// only on the way to pruning the family that describes it, so Transactions
// always moves with it.
type PruneOutcome struct {
	Transactions int
	Files        int
}

type txnPrune struct {
	Op             string        `json:"op"`
	TxnID          string        `json:"txn_id"`
	CompletionName string        `json:"completion_name"`
	Completion     txnCompletion `json:"completion"`
	Revisions      []string      `json:"revisions"`
}

type pruneHooks struct {
	afterMarker func() error
	afterRemove func(string) error
}

func txnPruneName(record txnPrune, raw []byte) string {
	return fmt.Sprintf("txn-%s-%s-%s.pruned", record.Op, record.TxnID, txnDigestFilenamePart(txnDigest(raw)))
}

func parseTxnPruneName(name string) (txnKey, string, error) {
	body := strings.TrimSuffix(strings.TrimPrefix(name, "txn-"), ".pruned")
	digestSep := strings.LastIndexByte(body, '-')
	if digestSep < 1 || len(body)-digestSep-1 != txnDigestHexWidth {
		return txnKey{}, "", fmt.Errorf("transaction prune record name %q has no canonical digest", name)
	}
	digestHex := body[digestSep+1:]
	if _, err := hex.DecodeString(digestHex); err != nil || strings.ToLower(digestHex) != digestHex {
		return txnKey{}, "", fmt.Errorf("transaction prune record name %q has invalid digest", name)
	}
	owner := body[:digestSep]
	idSep := strings.LastIndexByte(owner, '-')
	if idSep < 1 {
		return txnKey{}, "", fmt.Errorf("transaction prune record name %q has no transaction ID", name)
	}
	key := txnKey{op: owner[:idSep], id: owner[idSep+1:]}
	if err := validateOpName(key.op); err != nil {
		return txnKey{}, "", fmt.Errorf("transaction prune record name %q: %w", name, err)
	}
	if err := validateTxnID(key.id); err != nil {
		return txnKey{}, "", fmt.Errorf("transaction prune record name %q: %w", name, err)
	}
	return key, "sha256:" + digestHex, nil
}

func decodeTxnPrune(st *store.Store, key txnKey, name string) (txnPrune, error) {
	parsedKey, expectedDigest, err := parseTxnPruneName(name)
	if err != nil {
		return txnPrune{}, err
	}
	if parsedKey != key {
		return txnPrune{}, fmt.Errorf("transaction prune record %s has mismatched filename identity", txnDisplayPath(st, name))
	}
	raw, err := readTxnFile(st, name)
	if err != nil {
		return txnPrune{}, fmt.Errorf("read transaction prune record %s: %w", txnDisplayPath(st, name), err)
	}
	if actual := txnDigest(raw); actual != expectedDigest {
		return txnPrune{}, fmt.Errorf("transaction prune record %s has digest %s; filename commits to %s", txnDisplayPath(st, name), actual, expectedDigest)
	}
	var record txnPrune
	if err := json.Unmarshal(raw, &record); err != nil {
		return txnPrune{}, fmt.Errorf("parse transaction prune record %s: %w", txnDisplayPath(st, name), err)
	}
	if err := validateTxnPruneRecord(key, record); err != nil {
		return txnPrune{}, fmt.Errorf("validate transaction prune record %s: %w", txnDisplayPath(st, name), err)
	}
	return record, nil
}

func validateTxnPruneRecord(key txnKey, record txnPrune) error {
	if record.Op != key.op || record.TxnID != key.id {
		return fmt.Errorf("contents identify %q/%q; filename identifies %q/%q", record.Op, record.TxnID, key.op, key.id)
	}
	if record.CompletionName != fmt.Sprintf("txn-%s-%s.done", key.op, key.id) {
		return fmt.Errorf("completion name %q is not canonical", record.CompletionName)
	}
	if record.Completion.Op != key.op || record.Completion.TxnID != key.id || record.Completion.Sequence == 0 || !txnDigestPattern.MatchString(record.Completion.RevisionDigest) {
		return errors.New("completion identity is invalid")
	}
	if uint64(len(record.Revisions)) != record.Completion.Sequence {
		return fmt.Errorf("revision count %d does not match completion sequence %d", len(record.Revisions), record.Completion.Sequence)
	}
	seen := make(map[string]bool, len(record.Revisions))
	for index, name := range record.Revisions {
		if seen[name] {
			return fmt.Errorf("revision %q is listed more than once", name)
		}
		seen[name] = true
		revision, err := parseTxnRecordName(name)
		if err != nil {
			return err
		}
		if revision.key != key || revision.sequence != uint64(index+1) {
			return fmt.Errorf("revision %q is not sequence %d of %s/%s", name, index+1, key.op, key.id)
		}
		if index == len(record.Revisions)-1 && revision.digest != record.Completion.RevisionDigest {
			return fmt.Errorf("newest revision %q does not match completion digest", name)
		}
	}
	return nil
}

func validatePrunedTxn(st *store.Store, key txnKey, pruneName string, journal txnJournal) (txnPrune, error) {
	record, err := decodeTxnPrune(st, key, pruneName)
	if err != nil {
		return txnPrune{}, err
	}
	allowed := make(map[string]bool, len(record.Revisions))
	for _, name := range record.Revisions {
		allowed[name] = true
	}
	for _, revision := range journal.revisions[key] {
		if !allowed[revision.name] {
			return txnPrune{}, fmt.Errorf("transaction %s/%s gained revision %s after pruning began", key.op, key.id, txnDisplayPath(st, revision.name))
		}
	}
	if _, present := journal.completed[key]; !present && len(journal.revisions[key]) > 0 {
		return txnPrune{}, fmt.Errorf(
			"transaction %s/%s has prune record %s but no completion marker while %d revisions remain",
			key.op, key.id, txnDisplayPath(st, pruneName), len(journal.revisions[key]),
		)
	}
	if completionName, present := journal.completed[key]; present {
		if completionName != record.CompletionName {
			return txnPrune{}, fmt.Errorf("transaction %s/%s completion changed after pruning began", key.op, key.id)
		}
		completion, err := decodeTxnCompletion(st, key, completionName)
		if err != nil {
			return txnPrune{}, err
		}
		if completion != record.Completion {
			return txnPrune{}, fmt.Errorf("transaction completion %s changed after pruning began", txnDisplayPath(st, completionName))
		}
	}
	return record, nil
}

// PruneCompletedTransactions safely removes completed immutable journal
// families. A content-addressed prune record is durable before the first
// deletion, making every interrupted deletion prefix resumable.
func PruneCompletedTransactions(st *store.Store) (PruneOutcome, error) {
	return pruneCompletedTransactions(st, pruneHooks{})
}

func pruneCompletedTransactions(st *store.Store, hooks pruneHooks) (outcome PruneOutcome, retErr error) {
	session, err := st.BeginWrite()
	if err != nil {
		return outcome, fmt.Errorf("open checked recovery-prune session: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, session.Close()) }()
	homeRoot, err := session.Store.Root()
	if err != nil {
		return outcome, err
	}
	retErr = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		var err error
		outcome, err = pruneCompletedTransactionsLocked(session.Store, hooks)
		return err
	})
	return outcome, retErr
}

func pruneCompletedTransactionsLocked(st *store.Store, hooks pruneHooks) (PruneOutcome, error) {
	journal, err := scanTxnJournalReport(st)
	if err != nil {
		return PruneOutcome{}, err
	}
	keys := make(map[txnKey]bool, len(journal.completed)+len(journal.pruned))
	for key := range journal.completed {
		keys[key] = true
	}
	for key := range journal.pruned {
		keys[key] = true
	}
	ordered := make([]txnKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].op != ordered[j].op {
			return ordered[i].op < ordered[j].op
		}
		return ordered[i].id < ordered[j].id
	})
	outcome := PruneOutcome{}
	var problems []error
	if len(journal.problems) != 0 {
		problems = append(problems, addJournalScanRemedy(st, errors.Join(journal.problems...)))
	}
	// Config exchange bookkeeping is not described by any transaction journal,
	// so it is swept once per run rather than per family, and a failure here
	// must not stop journal pruning: the two share only the directory they live
	// in.
	configFiles, configErr := st.ReclaimCompletedConfigExchanges()
	outcome.Files += configFiles
	if configErr != nil {
		// Named the way every other problem in this function is. The only way
		// this call fails is listing the recovery directory, and the bare
		// "list logical root: ..." it produced matched neither `fu gc` nor the
		// config exchange scan it came from, leaving the one unwrapped error
		// in the whole run.
		problems = append(problems, fmt.Errorf("reclaim config exchange bookkeeping under %s: %w", st.RecoveryDir(), configErr))
	}
	// Every name a pending transaction claims, in both directories this run
	// removes things from, computed once for the whole run and before any
	// deletion. A name identifies content, never the transaction that owns it:
	// two rm transactions of the same skill at the same HEAD derive the same
	// removed- name, and each hop between the skills root and the recovery
	// directory is a rename, so device, inode and content are carried across
	// all of them. A completed family's manifest can therefore match a pending
	// family's live payload exactly, and matching it is not owning it.
	// staging/<name> is weaker still -- it is the bare skill name, which every
	// operation that stages content publishes at.
	//
	// Reading the pending records is not recovering them: `fu gc` still never
	// drives another command's transaction to a terminal state, which is why
	// PruneRecovery deliberately runs without RecoverPending.
	claimed, stagingClaimed, claimErr := pendingTxnClaims(st)
	if claimErr != nil && len(journal.problems) == 0 && len(journal.invalid) == 0 {
		// When the scan above found anything wrong, this is that same failure
		// said twice: the claims read rescans the same directory under the
		// same lock, and scanTxnJournal rejects a malformed name before it
		// validates a single family and rejects an invalid family as it goes,
		// so claimErr repeats what the loop below already reports per family.
		// Wrapping it again printed the identical multi-line remedy a second
		// time for one broken name.
		//
		// Both halves of that scan have to be consulted, not just the first.
		// A family that is itself invalid -- two prune records for one
		// transaction, say -- lands in journal.invalid while journal.problems
		// stays empty, and it fails the claims read just the same, so testing
		// only problems still let one damaged family be reported twice.
		//
		// A clean scan with a failing claims read is the opposite case: it
		// reports something no per-family error can, because the failure is
		// in a *pending* family's chain and the prune loop only visits
		// completed ones. Suppressing on invalid does give up one narrow
		// case -- a pending family broken independently of an invalid
		// completed one, whose error scanTxnJournal happens to return first.
		// That is a diagnostic delay, not a loss: the next write command
		// recovers pending transactions before doing anything else and fails
		// loudly on exactly that chain.
		problems = append(problems, addJournalScanRemedy(st, claimErr))
	}
	// A reclaim that failed its manifest check is held here rather than
	// reported where it happened, and released once every family has been
	// visited (releaseStagingPayloadProblems below).
	var heldStagingProblems []heldStagingProblem
	// releaseStagingPayloadProblems decides which held failures are real. A
	// failed reclaim means the entry at staging/<name> is not the tree *that*
	// family recorded -- which is news when the entry is the user's own
	// directory, and is not news at all when a later family in this same run
	// collected the tree as its own.
	//
	// Two completed, unpruned update families can name one staging name:
	// families are ordered by (op, id) and id is random, so the one whose
	// manifest no longer describes the tree may be visited first. It fails
	// here, the other one collects the tree a moment later, and gc used to
	// exit 1 telling the user to "restore it to its recorded content" about an
	// entry that was already gone. Re-asking after the loop is what tells the
	// two apart; the family itself is still skipped either way, so the next
	// run prunes it exactly as before -- only the spurious message goes.
	//
	// A clear name is necessary but not sufficient to drop the failure: it must
	// also be a failure a later collection could account for. Only
	// ErrOwnedTreeChanged is -- it is what compareOwnedTreeCleanupState raises
	// about a tree that is really there and is no longer the recorded one
	// (retire.go), which is exactly the state another family collecting it
	// resolves. Every other class fails before reaching the tree at all:
	// reclaimUpdateStagingPayload refuses a Name that is not a public skill name
	// before it opens staging (update.go), and RemoveOwnedTreeAt refuses an
	// invalid manifest the same way -- both leave the two candidate names
	// untouched and so answer "settled", and releasing on that answer alone
	// dropped them. Dropped, gc reports "nothing to prune" and exits 0 while the
	// family is skipped run after run and `fu status` goes on counting its files
	// collectable: the "run a command and watch a count not move" incoherence
	// this change exists to end, and the one PruneOutcome's own doc says cannot
	// happen.
	// Grouped by name, one remedy each. Two completed families can name one
	// staging name -- it is the bare skill name and the families are
	// independent -- and when the entry there matches neither manifest both fail
	// here. The remedy is a long instruction about a single directory, so one
	// copy per family told the user to move the same entry aside twice. The
	// causes are joined rather than dropped: which family failed is still
	// information, it just does not need its own copy of the instructions.
	releaseStagingPayloadProblems := func() []error {
		var order []string
		causes := make(map[string][]error)
		for _, held := range heldStagingProblems {
			settled, settledErr := updateStagingPayloadSettled(st, held.name, held.expected)
			var cause error
			switch {
			case settledErr != nil:
				cause = errors.Join(held.err, settledErr)
			case !settled || !errors.Is(held.err, store.ErrOwnedTreeChanged):
				cause = held.err
			default:
				continue
			}
			if _, seen := causes[held.name]; !seen {
				order = append(order, held.name)
			}
			causes[held.name] = append(causes[held.name], cause)
		}
		released := make([]error, 0, len(order))
		for _, name := range order {
			released = append(released, addStagingPayloadRemedy(st, name, errors.Join(dedupeErrorText(causes[name])...)))
		}
		return released
	}
	// Everything already accumulated is owed to the caller even when a write
	// below stops the run outright: the config exchange sweep's failure and
	// every family problem named so far are unrelated to whatever stopped it,
	// and nothing else reports them.
	abort := func(err error) (PruneOutcome, error) {
		problems = append(problems, releaseStagingPayloadProblems()...)
		return outcome, errors.Join(errors.Join(problems...), err)
	}
	for _, key := range ordered {
		if familyProblems := journal.invalid[key]; len(familyProblems) != 0 {
			problems = append(problems, addPruneFamilyRemedy(st, key, errors.Join(familyProblems...)))
			continue
		}
		pruneName := journal.pruned[key]
		var record txnPrune
		if pruneName != "" {
			// A resumed prune reclaims nothing, and needs to reclaim nothing:
			// the prune record is written strictly after this family's payload
			// has been settled below -- either reclaimed, or shown to be some
			// pending transaction's -- so a record on disk is proof that step
			// already ran to a conclusion. (The weaker argument, that the
			// revisions carrying the manifest may already be gone, explains
			// only why a resumed prune *could not* reclaim, not why it *need
			// not*.)
			record, err = validatePrunedTxn(st, key, pruneName, journal)
			if err != nil {
				problems = append(problems, addPruneFamilyRemedy(st, key, err))
				continue
			}
		} else {
			completionName := journal.completed[key]
			completion, err := decodeTxnCompletion(st, key, completionName)
			if err != nil {
				problems = append(problems, addPruneFamilyRemedy(st, key, err))
				continue
			}
			latest, err := validateTxnChain(st, key, journal.revisions[key])
			if err != nil {
				problems = append(problems, addPruneFamilyRemedy(st, key, err))
				continue
			}
			if latest.Sequence != completion.Sequence || latest.revisionDigest != completion.RevisionDigest {
				problems = append(problems, addPruneFamilyRemedy(st, key,
					fmt.Errorf("transaction completion %s does not match its validated revision chain", txnDisplayPath(st, completionName))))
				continue
			}
			// Reclaim before the journal goes: the payload's manifest lives in
			// the revision files this prune is about to delete. Reclaiming
			// after them would leave content that can never be verified again,
			// so a failure here skips the family entirely and the next gc run
			// retries with the manifest still in place.
			if latest.Op == "rm" && latest.Payload != nil {
				payload := rmPayloadName(latest)
				switch {
				case claimErr != nil:
					// The pending set could not be read, so no name under the
					// recovery directory can be shown to be unclaimed. Pruning
					// a family whose payload is still there would delete the
					// manifest that payload might still need, so that family
					// waits for a run that can read the pending set. The scan
					// failure itself is already among the problems.
					//
					// A family whose payload is already gone waits for
					// nothing, though: there is no object left for any
					// transaction to claim, so ownership cannot be in
					// question and the manifest has nothing left to prove.
					// Checking that first keeps one malformed journal filename
					// from pinning every rm family ever settled -- which is
					// most of them, since the inline reclaim collects the
					// payload the moment its transaction completes.
					// "Gone" has to mean gone from both names disposal uses,
					// which is why the question is the store's to answer rather
					// than a stat of the payload name here. Disposal empties the
					// tree, renames the root to a sibling derived from this very
					// manifest, and only then unlinks it, so a crash between the
					// last two frees the payload name while the emptied root
					// remains -- and pruning on the strength of that free name
					// destroys the only manifest the root could ever be resumed
					// or collected by.
					settled, presentErr := st.RecoveryPayloadSettled(payload, *latest.Payload)
					if presentErr != nil {
						problems = append(problems, addRecoveryPayloadRemedy(st, payload, presentErr))
						continue
					}
					if !settled {
						continue
					}
				case claimed[payload]:
					// Not this family's to collect, and this family has nothing
					// left to collect anywhere -- so pruning it, judged on its
					// own merits, is still right.
					//
					// Only a rolled-back rm reaches this branch. Its rollback
					// moved the content back under the skills root before it
					// wrote its terminal marker, so it owns nothing in the
					// recovery directory. A committed rm cannot be here: its
					// commit moved HEAD, so no later transaction derives the
					// same StartHead, and no earlier one can still be pending,
					// because every write command recovers pending transactions
					// before it starts.
					//
					// Nothing is stranded either. The object at this name stays
					// provable from the claiming transaction's own manifest,
					// and gc never prunes a pending family.
				default:
					if err := st.ReclaimRecoveryPayloadOwned(payload, *latest.Payload); err != nil {
						problems = append(problems, addRecoveryPayloadRemedy(st, payload, err))
						continue
					}
				}
			}
			// The same reclaim-before-prune rule applies to update's own
			// residue, and for the same reason: the tree the update replaced
			// is left orphaned at staging/<Name> whenever this operation's own
			// afterTxnCleared reclaim (reclaimExchangedUpdatePayload,
			// update.go) does not complete -- a crash before it runs, or that
			// reclaim running and failing, since it drops its own error -- and
			// PreviousPayload, the only manifest that proves what that tree is,
			// lives in the very revisions this loop is about to delete.
			// Reclaiming after pruning would make the tree permanently
			// unverifiable, so it happens here, before the journal goes,
			// exactly like the rm payload above. It is not counted in
			// outcome.Files: a tree removed whole against a manifest is not a
			// journal file, and rm's is not counted either (PruneOutcome).
			//
			// The claims test is the rm arm's, applied to the weaker name.
			// removed-<name>-<StartHead> at least says which manifest produced
			// it; staging/<Name> is the bare skill name, so a user's own
			// directory, an abandoned install's staged tree and a pending
			// transaction's staged content all sit on it just as legitimately
			// as this family's orphan. That every operation publishing there
			// refuses an occupied name (checkNewSkillAvailable in ops.go,
			// checkAddAvailable in add.go, shared by adopt's own Preflight and
			// Mutate in adopt.go, checkUpdateAvailable in update.go) proves
			// something about the *pending* transaction's starting state, not
			// about the object sitting here now -- an argument this code once
			// ran backwards, and acted on. What a claim settles is only that
			// this name is no evidence of ownership -- and nothing else about
			// the object is evidence of it either, which is why the arm below
			// stops at the claim rather than going on to inspect the tree.
			if latest.Op == "update" && latest.PreviousPayload != nil {
				name := latest.Name
				switch {
				case claimErr != nil:
					// The pending set could not be read, so no staging name can
					// be shown to be unclaimed -- and this is the rm arm's
					// reasoning verbatim. A family with nothing left at either
					// of its own candidate names waits for nothing: there is no
					// object for any transaction to claim, so ownership cannot
					// be in question and the manifest has nothing left to
					// prove. Asking that first keeps one malformed journal
					// filename from pinning every update family ever settled,
					// which is nearly all of them -- the inline reclaim clears
					// staging/<Name> the moment its transaction completes.
					settled, settledErr := updateStagingPayloadSettled(st, name, *latest.PreviousPayload)
					if settledErr != nil {
						problems = append(problems, fmt.Errorf(
							"check the tree an interrupted update may have left at %s: %w",
							filepath.Join(st.StagingDir(), name), settledErr))
						continue
					}
					if !settled {
						continue
					}
				case stagingClaimed[name]:
					// Claimed: leave the tree where it is, whatever it holds,
					// and prune the family anyway. Same call the rm arm makes
					// above, and it has to be the same call for a second
					// reason: leaving the journal instead would have
					// `fu status` counting these files Collectable (status.go's
					// own settled-family rule) while gc walked past them run
					// after run, which is the disagreement this whole change
					// exists to end.
					//
					// The claim alone decides it. An earlier revision tried to
					// reclaim when the object still carried the root identity
					// this family recorded, on the grounds that a claimant's
					// staged content is a fresh copy with a fresh inode. That
					// is false, and destructively so: the exchange is a rename,
					// so it preserves inodes. A rolled-back update leaves a
					// completed family whose PreviousPayload names the tree
					// restoreExchangedUpdate put back at skills/<name>
					// (update_txn.go), the next update exchanges that very
					// inode out to staging/<name>, and if it is interrupted
					// before its commit the object there matches the older
					// family's manifest by identity, mode and content -- it is
					// the same object. Reclaiming it destroys the only copy
					// that update's own rollback can exchange back, leaving
					// recovery unable to reach a terminal state and every write
					// command blocked at its prologue.
					// TestPruneKeepsATreeAPendingUpdateNeedsWhoseIdentityMatches
					// AnOlderFamily builds exactly that. It is the staging-side
					// case of the rule DESIGN §2 states for recovery payloads:
					// a name plus a manifest that matches it is not ownership.
					//
					// The cost is a known limitation, not a second defect.
					// pendingStagingClaims takes record.Name from every pending
					// record whatever its op, deliberately, and `fu rm` stages
					// nothing -- its preflight reads fu.yaml and skills/<name>
					// and never looks at staging (checkRemoveAvailable,
					// checkRemoveStoreEntry, rm.go). So an update whose inline
					// reclaim did not complete, followed by an rm of the same
					// skill that died mid-transaction, leaves this family's own
					// orphan under a name the rm claims.
					//
					// Losing the tree takes a third condition, and it is this
					// command's own timing: `fu gc` has to run while that rm is
					// still pending. Then gc leaves the tree, prunes the
					// manifest, and the tree becomes uncollectable and is
					// reported under Unmatched. Run any write command first and
					// it recovers the rm before doing anything else; a rm in a
					// terminal state claims nothing, so the next `fu gc` takes
					// the default arm below and collects the orphan as usual.
					// Two faults and a window, then, with the replaced content
					// still in the store's git history (SPEC rule 3), against
					// the alternative of a wedged store.
					// The asymmetry this file states everywhere decides it:
					// collecting a name too many only skips a deletion,
					// collecting one too few destroys content another
					// transaction is still counting on.
					//
					// Only the bare name is tested against the claims set, and
					// the retired sibling deliberately is not: the claims set
					// holds skill names and StagingReservation names only, a
					// skill name cannot begin with "." (skill.nameRe, meta.go)
					// and a reservation is ".fu-new-", so nothing ever claims a
					// ".fu-retired-dir-" name.
					//
					// The effect, stated plainly: this arm leaves *both*
					// candidate names alone and prunes anyway, so a retired
					// sibling under a claimed name loses its manifest here and
					// nothing collects it afterwards -- the same double-fault
					// limitation this comment already accepts for the live name.
					// status.go's own update arm is the counterpart and mirrors
					// it exactly: under a claim it promises neither name.
				default:
					if err := reclaimUpdateStagingPayload(st, name, *latest.PreviousPayload); err != nil {
						// Held, not reported: another family later in this run
						// may own the tree that failed this one's manifest
						// check (releaseStagingPayloadProblems).
						heldStagingProblems = append(heldStagingProblems,
							heldStagingProblem{name: name, expected: *latest.PreviousPayload, err: err})
						continue
					}
				}
			}
			revisions := append([]txnRevision(nil), journal.revisions[key]...)
			sort.Slice(revisions, func(i, j int) bool { return revisions[i].sequence < revisions[j].sequence })
			record = txnPrune{Op: key.op, TxnID: key.id, CompletionName: completionName, Completion: completion}
			for _, revision := range revisions {
				record.Revisions = append(record.Revisions, revision.name)
			}
			raw, err := marshalTxnPayload(fmt.Sprintf("transaction prune record %q", key.op), record)
			if err != nil {
				return abort(err)
			}
			pruneName = txnPruneName(record, raw)
			if err := writeTxnFileNoReplace(st, pruneName, raw); err != nil {
				return abort(fmt.Errorf("record transaction prune at %s: %w", txnDisplayPath(st, pruneName), err))
			}
			if hooks.afterMarker != nil {
				if err := hooks.afterMarker(); err != nil {
					return abort(err)
				}
			}
		}

		for _, name := range append(append([]string(nil), record.Revisions...), record.CompletionName) {
			removed, err := removeTxnJournalFile(st, name)
			if err != nil {
				return abort(err)
			}
			if removed {
				outcome.Files++
				if hooks.afterRemove != nil {
					if err := hooks.afterRemove(name); err != nil {
						return abort(err)
					}
				}
			}
		}
		_, err = removeTxnJournalFile(st, pruneName)
		if err != nil {
			return abort(err)
		}
		outcome.Transactions++
	}
	problems = append(problems, releaseStagingPayloadProblems()...)
	return outcome, errors.Join(problems...)
}

// dedupeErrorText drops causes that read identically, preserving order. Two
// completed families on one staging name usually fail the same way for the same
// reason -- one entry failed both manifests -- so joining both copies would put
// the same sentence into the remedy twice, which is the noise grouping by name
// exists to remove.
func dedupeErrorText(errs []error) []error {
	seen := make(map[string]bool, len(errs))
	out := make([]error, 0, len(errs))
	for _, err := range errs {
		if text := err.Error(); !seen[text] {
			seen[text] = true
			out = append(out, err)
		}
	}
	return out
}

// heldStagingProblem is one update family's failed staging reclaim, kept with
// the manifest it failed against so the failure can be re-asked once the whole
// run is over. See releaseStagingPayloadProblems for why the question is worth
// asking twice.
type heldStagingProblem struct {
	name     string
	expected store.OwnedTree
	err      error
}

// pendingTxnClaims collects both sets of names a pending transaction claims:
// the recovery payload names, and the staging names. Both are derived from all
// pending records rather than from the ops that produce each form -- rm's
// rmPayloadName is a pure function of the skill name and the start HEAD, and
// update's staging name is just the skill name -- so any pending transaction
// whose derivation lands on a name is one gc cannot prove does not own the
// object sitting there. The two mistakes are not symmetric: collecting a name
// too many only skips a deletion, collecting one too few destroys content
// another transaction is still counting on.
//
// The derivations themselves are pendingPayloadClaims and pendingStagingClaims
// (status.go), shared with the read-only inventory so the two can never
// disagree about what a pending transaction claims. Retyping the rule instead
// of sharing it is exactly how the staging half came to be missing here while
// status already had it. pendingPayloadClaims also derives the two install
// compensation names, which this caller never looks up -- an over-claim on a
// distinct prefix, which by the asymmetry above is the harmless direction.
//
// One journal read serves both sets. They used to be one set and one read;
// making it two reads would have doubled the scan and reported a damaged
// journal twice, the same duplication Status was already fixed for.
func pendingTxnClaims(st *store.Store) (payloads, staging map[string]bool, err error) {
	pending, err := PendingTxns(st)
	if err != nil {
		return nil, nil, err
	}
	return pendingPayloadClaims(pending), pendingStagingClaims(pending), nil
}

func removeTxnJournalFile(st *store.Store, name string) (bool, error) {
	root, err := st.RecoveryRoot()
	if err != nil {
		return false, err
	}
	if err := root.Remove(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("remove transaction journal %s: %w", txnDisplayPath(st, name), err)
	}
	return true, nil
}

// addStagingPayloadRemedy is addRecoveryPayloadRemedy's counterpart for
// update's own orphaned residue: it names the staging path that failed to
// reclaim and, like its recovery-side sibling, prescribes nothing about the
// journal family. The family is not damaged -- only the tree under the
// staging directory is -- and its revisions carry the one manifest
// (PreviousPayload) this tree can ever be verified and reclaimed by.
// addPruneFamilyRemedy's advice, moving the family out of the recovery
// directory, would destroy that manifest and strand the tree for good.
//
// It is worded for the one thing gc actually knows at this point, which is
// less than it used to claim: the entry at this name failed the manifest
// check, so it is not the tree the transaction recorded. The earlier wording
// told the reader to "move that directory out of staging to abandon the copy",
// which described content fu had just proved was not its own copy at all --
// most likely the reader's own directory on a name that is, after all, only
// the skill's name. Both readings now get an instruction that fits them, and
// neither is told to abandon anything.
func addStagingPayloadRemedy(st *store.Store, name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(
		"%w; the entry at %s is not the tree this transaction recorded, so fu left it exactly as it is and kept the transaction journal intact for the next `fu gc` to retry: if the entry is yours, move it out of %s and the next run finishes this family; if it is meant to be the tree the update replaced, restore it to its recorded content -- that content also remains in the store's git history; leave this transaction's txn-* records where they are, they hold the only manifest that can verify and reclaim this tree",
		err, filepath.Join(st.StagingDir(), name), st.StagingDir(),
	)
}
