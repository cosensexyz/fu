package engine

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/cosensexyz/fu/internal/store"
)

// PruneOutcome reports what one gc run removed from the recovery directory:
// completed transaction families, and the total files taken with them --
// journal entries plus the config exchange bookkeeping swept alongside them.
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
		problems = append(problems, configErr)
	}
	// Every recovery payload name a pending transaction claims, computed once
	// for the whole run and before any deletion. A payload name identifies
	// content, never the transaction that owns it: two rm transactions of the
	// same skill at the same HEAD derive the same name, and each hop between
	// the skills root and the recovery directory is a rename, so device, inode
	// and content are carried across all of them. A completed family's
	// manifest can therefore match a pending family's live payload exactly,
	// and matching it is not owning it.
	//
	// Reading the pending records is not recovering them: `fu gc` still never
	// drives another command's transaction to a terminal state, which is why
	// PruneRecovery deliberately runs without RecoverPending.
	claimed, claimErr := pendingRecoveryPayloadClaims(st)
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
	// Everything already accumulated is owed to the caller even when a write
	// below stops the run outright: the config exchange sweep's failure and
	// every family problem named so far are unrelated to whatever stopped it,
	// and nothing else reports them.
	abort := func(err error) (PruneOutcome, error) {
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
					settled, presentErr := recoveryPayloadAbsent(st, payload)
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
	return outcome, errors.Join(problems...)
}

// pendingRecoveryPayloadClaims collects the recovery payload name every
// pending transaction claims. The name is derived from all pending records,
// not just the rm ones: rmPayloadName is a pure function of the skill name and
// the start HEAD, so any pending transaction whose derivation lands on a name
// is one gc cannot prove does not own the object sitting there. The two
// mistakes are not symmetric -- collecting a name too many only skips a
// deletion, collecting one too few deletes content another transaction is
// still counting on.
func pendingRecoveryPayloadClaims(st *store.Store) (map[string]bool, error) {
	pending, err := PendingTxns(st)
	if err != nil {
		return nil, err
	}
	claims := make(map[string]bool, len(pending))
	for _, record := range pending {
		claims[rmPayloadName(record)] = true
	}
	return claims, nil
}

// recoveryPayloadAbsent reports whether a transaction payload name holds
// nothing under the recovery directory. It answers the one ownership question
// that does not need the pending set: a name holding no object cannot be
// claimed by any transaction, so the family that named it has nothing left to
// settle and may be pruned even while ownership is otherwise unknowable.
func recoveryPayloadAbsent(st *store.Store, name string) (bool, error) {
	root, err := st.RecoveryRoot()
	if err != nil {
		return false, err
	}
	present, err := txnPathPresent(root, name)
	if err != nil {
		return false, err
	}
	return !present, nil
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
