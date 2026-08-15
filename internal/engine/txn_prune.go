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

// PruneOutcome reports the completed journal history removed by one gc run.
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
	for _, key := range ordered {
		if familyProblems := journal.invalid[key]; len(familyProblems) != 0 {
			problems = append(problems, addPruneFamilyRemedy(st, key, errors.Join(familyProblems...)))
			continue
		}
		pruneName := journal.pruned[key]
		var record txnPrune
		if pruneName != "" {
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
			revisions := append([]txnRevision(nil), journal.revisions[key]...)
			sort.Slice(revisions, func(i, j int) bool { return revisions[i].sequence < revisions[j].sequence })
			record = txnPrune{Op: key.op, TxnID: key.id, CompletionName: completionName, Completion: completion}
			for _, revision := range revisions {
				record.Revisions = append(record.Revisions, revision.name)
			}
			raw, err := marshalTxnPayload(fmt.Sprintf("transaction prune record %q", key.op), record)
			if err != nil {
				return outcome, err
			}
			pruneName = txnPruneName(record, raw)
			if err := writeTxnFileNoReplace(st, pruneName, raw); err != nil {
				return outcome, fmt.Errorf("record transaction prune at %s: %w", txnDisplayPath(st, pruneName), err)
			}
			if hooks.afterMarker != nil {
				if err := hooks.afterMarker(); err != nil {
					return outcome, err
				}
			}
		}

		for _, name := range append(append([]string(nil), record.Revisions...), record.CompletionName) {
			removed, err := removeTxnJournalFile(st, name)
			if err != nil {
				return outcome, err
			}
			if removed {
				outcome.Files++
				if hooks.afterRemove != nil {
					if err := hooks.afterRemove(name); err != nil {
						return outcome, err
					}
				}
			}
		}
		_, err = removeTxnJournalFile(st, pruneName)
		if err != nil {
			return outcome, err
		}
		outcome.Transactions++
	}
	return outcome, errors.Join(problems...)
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
