// internal/engine/txn.go
package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"fu/internal/store"
)

// TxnRecord is the command-level write-ahead record (DESIGN §2). Every stage
// is appended under an immutable transaction ID and sequence; completion is
// another exclusively created marker, never deletion of a mutable pathname.
type TxnRecord struct {
	Op             string           `json:"op"`
	TxnID          string           `json:"txn_id"`
	Sequence       uint64           `json:"sequence"`
	PreviousDigest string           `json:"previous_digest,omitempty"`
	StartHead      string           `json:"start_head"`
	Stage          string           `json:"stage"`
	Targets        []string         `json:"targets"`
	Name           string           `json:"name,omitempty"`
	Digest         string           `json:"digest,omitempty"`
	Message        string           `json:"message,omitempty"`
	ConfigBefore   []byte           `json:"config_before,omitempty"`
	Payload        *store.OwnedTree `json:"payload,omitempty"`
	// Declared names entries the operation has committed to creating but has
	// not created yet, so a crash inside a create->record window leaves a state
	// recovery can classify instead of one it must refuse. Cleared by the
	// revision that records them as real manifest entries.
	Declared         []store.DeclaredEntry `json:"declared,omitempty"`
	CommitTree       string                `json:"commit_tree,omitempty"`
	CompensationTree string                `json:"compensation_tree,omitempty"`
	revisionDigest   string
}

// opNamePattern is the grammar an operation identifier must satisfy before
// it is allowed anywhere near a path (round 6 finding). Lowercase
// alphanumerics with single internal hyphens -- deliberately the same shape
// as a skill name, and deliberately narrower than "whatever the caller
// passed": the identifier is concatenated into a filename, so anything
// containing a separator or a dot-dot wrote the record outside the recovery
// directory, and anything else unusual produced a name PendingTxns would
// never list again, leaving a transaction recorded and permanently
// unrecoverable.
//
// Op names are fu's own constants, not user input, which is why this had
// gone unnoticed -- and exactly why the check is cheap: no legitimate
// caller is inconvenienced by it, and a future one that would have
// introduced the bug is stopped at the point it is still obvious.
var opNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validateOpName(op string) error {
	if !opNamePattern.MatchString(op) {
		return fmt.Errorf("transaction operation name %q must be lowercase alphanumerics "+
			"separated by single hyphens: it becomes part of a filename under the recovery directory", op)
	}
	return nil
}

const (
	txnIDBytes        = 16
	txnIDHexLength    = txnIDBytes * 2
	txnSequenceWidth  = 20
	txnDigestHexWidth = sha256.Size * 2
	maxTxnRecordBytes = int64(16 << 20)
)

var (
	txnIDPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	txnDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func newTxnID() (string, error) {
	var raw [txnIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate transaction ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validateTxnID(id string) error {
	if !txnIDPattern.MatchString(id) {
		return fmt.Errorf("transaction ID %q must be %d lowercase hexadecimal characters", id, txnIDHexLength)
	}
	return nil
}

func txnDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func txnDigestFilenamePart(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}

func txnRecordName(r TxnRecord) string {
	digest := r.revisionDigest
	if digest == "" {
		// TxnRecord contains only JSON-marshallable fields. This fallback keeps
		// fixture construction honest: a hand-authored name still commits to
		// the canonical bytes the record would have had on disk.
		raw, _ := json.Marshal(r)
		digest = txnDigest(raw)
	}
	return fmt.Sprintf("txn-%s-%s-%0*d-%s.json", r.Op, r.TxnID, txnSequenceWidth, r.Sequence, txnDigestFilenamePart(digest))
}

func txnCompletionName(r TxnRecord) string {
	return fmt.Sprintf("txn-%s-%s.done", r.Op, r.TxnID)
}

func txnDisplayPath(st *store.Store, name string) string {
	return filepath.Join(st.RecoveryDir(), name)
}

func marshalTxnPayload(kind string, value any) ([]byte, error) {
	out, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", kind, err)
	}
	if int64(len(out)) > maxTxnRecordBytes {
		return nil, fmt.Errorf("%s serialized size %d exceeds transaction journal limit %d", kind, len(out), maxTxnRecordBytes)
	}
	return out, nil
}

func writeTxnFileNoReplace(st *store.Store, name string, data []byte) error {
	if int64(len(data)) > maxTxnRecordBytes {
		return fmt.Errorf("transaction journal file %s size %d exceeds limit %d", name, len(data), maxTxnRecordBytes)
	}
	if _, err := st.Root(); err == nil {
		recoveryRoot, err := st.RecoveryRoot()
		if err != nil {
			return err
		}
		return store.WriteFileAtomicNoReplaceRoot(recoveryRoot, name, data, 0o644)
	}
	return store.WriteFileAtomicNoReplace(txnDisplayPath(st, name), data, 0o644)
}

func WriteTxn(st *store.Store, r *TxnRecord) error {
	if r == nil {
		return errors.New("transaction record is nil")
	}
	if err := validateOpName(r.Op); err != nil {
		return err
	}
	if r.TxnID == "" {
		id, err := newTxnID()
		if err != nil {
			return err
		}
		r.TxnID = id
	} else if err := validateTxnID(r.TxnID); err != nil {
		return err
	}
	if r.Sequence == math.MaxUint64 {
		return fmt.Errorf("transaction %s/%s exhausted its sequence space", r.Op, r.TxnID)
	}
	previousDigest := ""
	if r.Sequence == 0 {
		if r.PreviousDigest != "" || r.revisionDigest != "" {
			return fmt.Errorf("new transaction %s/%s carries persisted revision state", r.Op, r.TxnID)
		}
	} else {
		journal, err := scanTxnJournal(st)
		if err != nil {
			return err
		}
		key := txnKey{op: r.Op, id: r.TxnID}
		if completionName, ok := journal.completed[key]; ok {
			return fmt.Errorf("transaction %s/%s is already completed at %s", r.Op, r.TxnID, txnDisplayPath(st, completionName))
		}
		latest, err := validateTxnChain(st, key, journal.revisions[key])
		if err != nil {
			return fmt.Errorf("validate transaction %s/%s before append: %w", r.Op, r.TxnID, err)
		}
		if latest.Sequence != r.Sequence || r.revisionDigest == "" || latest.revisionDigest != r.revisionDigest {
			return fmt.Errorf("transaction %s/%s latest persisted revision changed before append", r.Op, r.TxnID)
		}
		previousDigest = latest.revisionDigest
	}
	next := *r
	next.Sequence++
	next.PreviousDigest = previousDigest
	next.revisionDigest = ""
	out, err := marshalTxnPayload(fmt.Sprintf("transaction record %q", r.Op), next)
	if err != nil {
		return err
	}
	next.revisionDigest = txnDigest(out)
	name := txnRecordName(next)
	if err := writeTxnFileNoReplace(st, name, out); err != nil {
		return fmt.Errorf("append transaction record %s: %w", txnDisplayPath(st, name), err)
	}
	r.Sequence = next.Sequence
	r.PreviousDigest = next.PreviousDigest
	r.revisionDigest = next.revisionDigest
	return nil
}

type txnCompletion struct {
	Op             string `json:"op"`
	TxnID          string `json:"txn_id"`
	Sequence       uint64 `json:"sequence"`
	RevisionDigest string `json:"revision_digest"`
}

// ClearTxn completes a transaction by exclusively creating an immutable
// terminal marker. Prior revisions are retained so no pathname is ever
// removed without proof that it is still transaction-owned.
func ClearTxn(st *store.Store, r TxnRecord) error {
	if err := validateOpName(r.Op); err != nil {
		return err
	}
	if err := validateTxnID(r.TxnID); err != nil {
		return err
	}
	if r.Sequence == 0 {
		return fmt.Errorf("transaction %s/%s has no persisted revision to complete", r.Op, r.TxnID)
	}
	journal, err := scanTxnJournal(st)
	if err != nil {
		return err
	}
	key := txnKey{op: r.Op, id: r.TxnID}
	latest, err := validateTxnChain(st, key, journal.revisions[key])
	if err != nil {
		return fmt.Errorf("validate transaction %s/%s before completion: %w", r.Op, r.TxnID, err)
	}
	if latest.Sequence != r.Sequence || r.revisionDigest == "" || latest.revisionDigest != r.revisionDigest {
		return fmt.Errorf("transaction %s/%s latest persisted revision changed before completion", r.Op, r.TxnID)
	}
	marker := txnCompletion{
		Op:             r.Op,
		TxnID:          r.TxnID,
		Sequence:       r.Sequence,
		RevisionDigest: r.revisionDigest,
	}
	if completionName, ok := journal.completed[key]; ok {
		persisted, err := decodeTxnCompletion(st, key, completionName)
		if err != nil {
			return fmt.Errorf("validate existing transaction completion: %w", err)
		}
		if persisted != marker {
			return fmt.Errorf("transaction completion %s does not match the latest revision", txnDisplayPath(st, completionName))
		}
		return nil
	}
	out, err := marshalTxnPayload(fmt.Sprintf("completed transaction %q", r.Op), marker)
	if err != nil {
		return err
	}
	name := txnCompletionName(r)
	if err := writeTxnFileNoReplace(st, name, out); err != nil {
		return fmt.Errorf("complete transaction at %s: %w", txnDisplayPath(st, name), err)
	}
	return nil
}

type txnKey struct {
	op string
	id string
}

type txnRevision struct {
	key      txnKey
	sequence uint64
	digest   string
	name     string
}

func parseTxnRecordName(name string) (txnRevision, error) {
	body := strings.TrimSuffix(strings.TrimPrefix(name, "txn-"), ".json")
	digestSep := strings.LastIndexByte(body, '-')
	if digestSep < 1 || len(body)-digestSep-1 != txnDigestHexWidth {
		return txnRevision{}, fmt.Errorf("transaction record name %q has no canonical revision digest", name)
	}
	digestHex := body[digestSep+1:]
	if _, err := hex.DecodeString(digestHex); err != nil || strings.ToLower(digestHex) != digestHex {
		return txnRevision{}, fmt.Errorf("transaction record name %q has invalid revision digest", name)
	}
	body = body[:digestSep]
	sequenceSep := strings.LastIndexByte(body, '-')
	if sequenceSep < 1 || len(body)-sequenceSep-1 != txnSequenceWidth {
		return txnRevision{}, fmt.Errorf("transaction record name %q has no canonical sequence", name)
	}
	sequence, err := strconv.ParseUint(body[sequenceSep+1:], 10, 64)
	if err != nil || sequence == 0 {
		return txnRevision{}, fmt.Errorf("transaction record name %q has invalid sequence", name)
	}
	owner := body[:sequenceSep]
	idSep := strings.LastIndexByte(owner, '-')
	if idSep < 1 {
		return txnRevision{}, fmt.Errorf("transaction record name %q has no transaction ID", name)
	}
	op, id := owner[:idSep], owner[idSep+1:]
	if err := validateOpName(op); err != nil {
		return txnRevision{}, fmt.Errorf("transaction record name %q: %w", name, err)
	}
	if err := validateTxnID(id); err != nil {
		return txnRevision{}, fmt.Errorf("transaction record name %q: %w", name, err)
	}
	return txnRevision{
		key:      txnKey{op: op, id: id},
		sequence: sequence,
		digest:   "sha256:" + digestHex,
		name:     name,
	}, nil
}

func parseTxnCompletionName(name string) (txnKey, error) {
	body := strings.TrimSuffix(strings.TrimPrefix(name, "txn-"), ".done")
	idSep := strings.LastIndexByte(body, '-')
	if idSep < 1 {
		return txnKey{}, fmt.Errorf("transaction completion name %q has no transaction ID", name)
	}
	op, id := body[:idSep], body[idSep+1:]
	if err := validateOpName(op); err != nil {
		return txnKey{}, fmt.Errorf("transaction completion name %q: %w", name, err)
	}
	if err := validateTxnID(id); err != nil {
		return txnKey{}, fmt.Errorf("transaction completion name %q: %w", name, err)
	}
	return txnKey{op: op, id: id}, nil
}

func readTxnFile(st *store.Store, name string) ([]byte, error) {
	if _, err := st.Root(); err == nil {
		recoveryRoot, err := st.RecoveryRoot()
		if err != nil {
			return nil, err
		}
		return store.ReadRegularFileRoot(recoveryRoot, name, maxTxnRecordBytes)
	}
	return store.ReadRegularFile(txnDisplayPath(st, name), maxTxnRecordBytes)
}

func decodeTxnFile(st *store.Store, revision txnRevision) (TxnRecord, error) {
	path := txnDisplayPath(st, revision.name)
	raw, err := readTxnFile(st, revision.name)
	if err != nil {
		return TxnRecord{}, fmt.Errorf("read transaction record %s: %w", path, err)
	}
	actualDigest := txnDigest(raw)
	if actualDigest != revision.digest {
		return TxnRecord{}, fmt.Errorf("transaction record %s has digest %s; filename commits to %s",
			path, actualDigest, revision.digest)
	}
	var r TxnRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return TxnRecord{}, fmt.Errorf("parse transaction record %s: %w", path, err)
	}
	if r.Op != revision.key.op || r.TxnID != revision.key.id || r.Sequence != revision.sequence {
		return TxnRecord{}, fmt.Errorf("transaction record %s identifies %q/%q/%d in its contents; filename identifies %q/%q/%d",
			path, r.Op, r.TxnID, r.Sequence, revision.key.op, revision.key.id, revision.sequence)
	}
	r.revisionDigest = actualDigest
	return r, nil
}

func decodeTxnCompletion(st *store.Store, key txnKey, name string) (txnCompletion, error) {
	path := txnDisplayPath(st, name)
	raw, err := readTxnFile(st, name)
	if err != nil {
		return txnCompletion{}, fmt.Errorf("read transaction completion %s: %w", path, err)
	}
	var marker txnCompletion
	if err := json.Unmarshal(raw, &marker); err != nil {
		return txnCompletion{}, fmt.Errorf("parse transaction completion %s: %w", path, err)
	}
	if marker.Op != key.op || marker.TxnID != key.id {
		return txnCompletion{}, fmt.Errorf("transaction completion %s identifies %q/%q; filename identifies %q/%q",
			path, marker.Op, marker.TxnID, key.op, key.id)
	}
	if marker.Sequence == 0 || !txnDigestPattern.MatchString(marker.RevisionDigest) {
		return txnCompletion{}, fmt.Errorf("transaction completion %s has invalid revision identity", path)
	}
	return marker, nil
}

type txnJournal struct {
	revisions map[txnKey][]txnRevision
	completed map[txnKey]string
}

func scanTxnJournal(st *store.Store) (txnJournal, error) {
	_, rootErr := st.Root()
	var ents []fs.DirEntry
	var err error
	if rootErr == nil {
		recoveryRoot, rootErr := st.RecoveryRoot()
		if rootErr != nil {
			return txnJournal{}, rootErr
		}
		ents, err = fs.ReadDir(recoveryRoot.FS(), ".")
	} else {
		ents, err = os.ReadDir(st.RecoveryDir())
	}
	if err != nil {
		return txnJournal{}, fmt.Errorf("scan recovery directory %s: %w", st.RecoveryDir(), err)
	}
	journal := txnJournal{
		revisions: make(map[txnKey][]txnRevision),
		completed: make(map[txnKey]string),
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "txn-") {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".json"):
			revision, err := parseTxnRecordName(name)
			if err != nil {
				return txnJournal{}, fmt.Errorf("transaction record %s: %w", txnDisplayPath(st, name), err)
			}
			journal.revisions[revision.key] = append(journal.revisions[revision.key], revision)
		case strings.HasSuffix(name, ".done"):
			key, err := parseTxnCompletionName(name)
			if err != nil {
				return txnJournal{}, fmt.Errorf("transaction completion %s: %w", txnDisplayPath(st, name), err)
			}
			journal.completed[key] = name
		}
	}
	return journal, nil
}

func validateTxnChain(st *store.Store, key txnKey, revisions []txnRevision) (TxnRecord, error) {
	if len(revisions) == 0 {
		return TxnRecord{}, fmt.Errorf("transaction %s/%s has no immutable revisions", key.op, key.id)
	}
	sort.Slice(revisions, func(i, j int) bool {
		if revisions[i].sequence != revisions[j].sequence {
			return revisions[i].sequence < revisions[j].sequence
		}
		return revisions[i].name < revisions[j].name
	})
	previousDigest := ""
	var latest TxnRecord
	for i, revision := range revisions {
		expectedSequence := uint64(i + 1)
		if revision.sequence != expectedSequence {
			return TxnRecord{}, fmt.Errorf("transaction %s/%s revision chain expected sequence %d, found %d at %s",
				key.op, key.id, expectedSequence, revision.sequence, txnDisplayPath(st, revision.name))
		}
		record, err := decodeTxnFile(st, revision)
		if err != nil {
			return TxnRecord{}, err
		}
		if record.PreviousDigest != previousDigest {
			return TxnRecord{}, fmt.Errorf("transaction record %s links to revision digest %q; expected %q",
				txnDisplayPath(st, revision.name), record.PreviousDigest, previousDigest)
		}
		latest = record
		previousDigest = revision.digest
	}
	return latest, nil
}

// PendingTxns validates every digest-chained revision and returns the latest
// record for each transaction that has no terminal marker bound to that exact
// revision. Completion never makes a revision exempt from validation.
func PendingTxns(st *store.Store) ([]TxnRecord, error) {
	journal, err := scanTxnJournal(st)
	if err != nil {
		return nil, err
	}
	keys := make(map[txnKey]struct{}, len(journal.revisions)+len(journal.completed))
	for key := range journal.revisions {
		keys[key] = struct{}{}
	}
	for key := range journal.completed {
		keys[key] = struct{}{}
	}
	orderedKeys := make([]txnKey, 0, len(keys))
	for key := range keys {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Slice(orderedKeys, func(i, j int) bool {
		if orderedKeys[i].op != orderedKeys[j].op {
			return orderedKeys[i].op < orderedKeys[j].op
		}
		return orderedKeys[i].id < orderedKeys[j].id
	})

	out := make([]TxnRecord, 0, len(journal.revisions))
	for _, key := range orderedKeys {
		revisions := journal.revisions[key]
		if len(revisions) == 0 {
			return nil, fmt.Errorf("transaction completion %s has no matching immutable revision",
				txnDisplayPath(st, journal.completed[key]))
		}
		latest, err := validateTxnChain(st, key, revisions)
		if err != nil {
			return nil, err
		}
		completionName, completed := journal.completed[key]
		if !completed {
			out = append(out, latest)
			continue
		}
		marker, err := decodeTxnCompletion(st, key, completionName)
		if err != nil {
			return nil, fmt.Errorf("validate transaction completion: %w", err)
		}
		if marker.Sequence != latest.Sequence || marker.RevisionDigest != latest.revisionDigest {
			return nil, fmt.Errorf("transaction completion %s names revision %d/%s but latest revision is %d/%s",
				txnDisplayPath(st, completionName), marker.Sequence, marker.RevisionDigest, latest.Sequence, latest.revisionDigest)
		}
	}
	return out, nil
}

// RecoverHandler drives one pending transaction to a terminal state:
// complete, rolled back, or safe-conflict. Handlers append a terminal marker.
type RecoverHandler func(*store.Store, TxnRecord) error

var (
	// recoverHandlersMu guards recoverHandlers. Registration normally
	// happens during init, before any command runs, but the lock keeps
	// that from being a correctness requirement callers must remember.
	recoverHandlersMu sync.RWMutex
	recoverHandlers   = map[string]RecoverHandler{}
)

// RegisterRecoverHandler is called by op implementations (adopt/update
// in later plans).
func RegisterRecoverHandler(op string, h RecoverHandler) {
	recoverHandlersMu.Lock()
	defer recoverHandlersMu.Unlock()
	recoverHandlers[op] = h
}

var ErrUnknownTxn = errors.New("pending transaction with no registered handler")

// deleteRecoverHandler is an unexported test helper for safe removal of
// registered handlers after tests.
func deleteRecoverHandler(op string) {
	recoverHandlersMu.Lock()
	defer recoverHandlersMu.Unlock()
	delete(recoverHandlers, op)
}

// RecoverPending is the unified recovery entry — the mandatory first
// step of every write command after taking the lock (DESIGN §2).
func RecoverPending(st *store.Store) error {
	if err := st.RecoverConfigExchanges(); err != nil {
		return fmt.Errorf("recover pending config exchanges: %w", err)
	}
	pend, err := PendingTxns(st)
	if err != nil {
		return fmt.Errorf("list pending transactions: %w", err)
	}
	for _, r := range pend {
		recoverHandlersMu.RLock()
		h, ok := recoverHandlers[r.Op]
		recoverHandlersMu.RUnlock()
		if !ok {
			return fmt.Errorf("%w: %q (newer fu required, or resolve manually under %s)",
				ErrUnknownTxn, r.Op, st.RecoveryDir())
		}
		if err := h(st, r); err != nil {
			return fmt.Errorf("recover transaction %q: %w", r.Op, err)
		}
	}
	return nil
}
