package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	configExchangeRecordVersion  = 1
	maxConfigExchangeRecordBytes = int64(64 << 10)
	configCandidatePrefix        = ".fu-config-candidate-"
	configExchangeRecordPrefix   = ".fu-config-exchange-"
)

type configExchangeRecord struct {
	Version      int          `json:"version"`
	Candidate    string       `json:"candidate"`
	Previous     FileIdentity `json:"previous"`
	Staged       FileIdentity `json:"staged"`
	ExpectDigest string       `json:"expect_digest"`
	DataDigest   string       `json:"data_digest"`
}

type configExchangeCompletion struct {
	Version      int    `json:"version"`
	RecordDigest string `json:"record_digest"`
	Outcome      string `json:"outcome"`
}

type configObjectState struct {
	exists   bool
	identity FileIdentity
	digest   string
}

func digestConfigExchangeBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func configCandidateSuffix(name string) (string, error) {
	if !strings.HasPrefix(name, configCandidatePrefix) {
		return "", fmt.Errorf("config candidate %q has an invalid name", name)
	}
	suffix := strings.TrimPrefix(name, configCandidatePrefix)
	if len(suffix) != 16 {
		return "", fmt.Errorf("config candidate %q has an invalid random suffix", name)
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		return "", fmt.Errorf("config candidate %q has an invalid random suffix: %w", name, err)
	}
	return suffix, nil
}

func configExchangeRecordName(candidate string) (string, error) {
	suffix, err := configCandidateSuffix(candidate)
	if err != nil {
		return "", err
	}
	return configExchangeRecordPrefix + suffix + ".json", nil
}

func configExchangeDoneName(candidate string) (string, error) {
	suffix, err := configCandidateSuffix(candidate)
	if err != nil {
		return "", err
	}
	return configExchangeRecordPrefix + suffix + ".done", nil
}

func validateConfigExchangeDigest(digest string) error {
	if len(digest) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("invalid SHA-256 digest %q", digest)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err != nil {
		return fmt.Errorf("invalid SHA-256 digest %q: %w", digest, err)
	}
	return nil
}

func validateConfigExchangeRecord(name string, record configExchangeRecord) error {
	if record.Version != configExchangeRecordVersion {
		return fmt.Errorf("config exchange record %s has unsupported version %d", name, record.Version)
	}
	wantName, err := configExchangeRecordName(record.Candidate)
	if err != nil {
		return fmt.Errorf("config exchange record %s: %w", name, err)
	}
	if name != wantName {
		return fmt.Errorf("config exchange record %s names candidate %q belonging to %s", name, record.Candidate, wantName)
	}
	if !record.Previous.valid() || !record.Staged.valid() || record.Previous == record.Staged {
		return fmt.Errorf("config exchange record %s has invalid file identities", name)
	}
	if err := validateConfigExchangeDigest(record.ExpectDigest); err != nil {
		return fmt.Errorf("config exchange record %s expected bytes: %w", name, err)
	}
	if err := validateConfigExchangeDigest(record.DataDigest); err != nil {
		return fmt.Errorf("config exchange record %s staged bytes: %w", name, err)
	}
	return nil
}

func marshalConfigExchangeRecord(record configExchangeRecord) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxConfigExchangeRecordBytes {
		return nil, fmt.Errorf("config exchange record is too large: %d bytes", len(raw))
	}
	return raw, nil
}

func writeConfigExchangeRecord(archive *checkedRoot, record configExchangeRecord) ([]byte, error) {
	name, err := configExchangeRecordName(record.Candidate)
	if err != nil {
		return nil, err
	}
	if err := validateConfigExchangeRecord(name, record); err != nil {
		return nil, err
	}
	raw, err := marshalConfigExchangeRecord(record)
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomicNoReplaceRoot(archive.root, name, raw, 0o600); err != nil {
		return nil, fmt.Errorf("persist config exchange record %s/%s: %w", archive.display, name, err)
	}
	return raw, nil
}

func inspectConfigObject(root *checkedRoot, name string) (configObjectState, error) {
	defer keepDescriptorOwnersAlive(root)
	file, stat, err := openRegularFileAt(int(root.dir.Fd()), name)
	if errors.Is(err, unix.ENOENT) {
		return configObjectState{}, nil
	}
	if err != nil {
		return configObjectState{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if readErr != nil {
		_ = file.Close()
		return configObjectState{}, readErr
	}
	if int64(len(raw)) > MaxConfigBytes {
		_ = file.Close()
		return configObjectState{}, fmt.Errorf("regular file %q exceeds config limit %d", name, MaxConfigBytes)
	}
	if err := finishRegularFileRead(file, name, stat, int64(len(raw)), regularFileReadHooks{}); err != nil {
		return configObjectState{}, err
	}
	return configObjectState{
		exists:   true,
		identity: identityFromStat(&stat),
		digest:   digestConfigExchangeBytes(raw),
	}, nil
}

func configObjectMatches(state configObjectState, identity FileIdentity, digest string) bool {
	return state.exists && state.identity == identity && state.digest == digest
}

func completeConfigExchange(archive *checkedRoot, record configExchangeRecord, raw []byte, outcome string) error {
	defer keepDescriptorOwnersAlive(archive)
	doneName, err := configExchangeDoneName(record.Candidate)
	if err != nil {
		return err
	}
	recordName, err := configExchangeRecordName(record.Candidate)
	if err != nil {
		return err
	}
	completion := configExchangeCompletion{
		Version:      configExchangeRecordVersion,
		RecordDigest: digestConfigExchangeBytes(raw),
		Outcome:      outcome,
	}
	doneRaw, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	if err := WriteFileAtomicNoReplaceRoot(archive.root, doneName, doneRaw, 0o600); err != nil {
		if existing, readErr := readRegularFileAt(int(archive.dir.Fd()), doneName, maxConfigExchangeRecordBytes); readErr == nil && bytes.Equal(existing, doneRaw) {
			reclaimConfigExchangeResidue(archive, record, recordName, doneName)
			return nil
		}
		return fmt.Errorf("persist config exchange completion %s/%s: %w", archive.display, doneName, err)
	}
	reclaimConfigExchangeResidue(archive, record, recordName, doneName)
	return nil
}

// reclaimConfigExchangeResidue disposes of one completed exchange's bookkeeping
// once its terminal marker is durable. Order is load-bearing: the record goes
// first, because readPendingConfigExchangeRecords enumerates records and treats
// a record without its marker as pending work. Removing the marker first would
// make a finished exchange look interrupted and send recovery through it again.
// A stranded marker is invisible to that scan, so every intermediate state here
// is safe.
//
// Each removal retires the name to an unpredictable sibling and proves the
// moved object is the one this exchange is entitled to delete before unlinking
// it, so a replacement racing the final namespace operation is preserved rather
// than deleted. What "entitled" means differs per name: reclaimConfigExchangeOwnName
// states it for the two files the exchange wrote for itself, and the archives are
// covered at their call below. Errors are dropped: the exchange itself has
// already completed durably, so a reclamation failure must not turn it into a
// reported failure.
//
// Collecting what a failed reclaim leaves behind belongs to `fu gc`, and gc
// cannot get there by replaying this record: the record is retired first, so
// every state an interruption here can leave holds a bare marker or a bare
// archive with nothing left to derive either from. gc has to collect them by
// prefix instead. Both prefixes carry enough on their own to make that safe --
// a `.done` whose record is gone is finished by construction, and an archive
// name states the device/inode of the object it must hold, so gc can prove that
// identity before unlinking exactly as this function does.
// ReclaimCompletedConfigExchanges is that collector.
// reclaimConfigExchangeOrderHook observes the order the two bookkeeping names
// are disposed in. It is nil in production and set only by the test that pins
// that order.
//
// A seam is needed because the order is observable only at a crash. The loop
// below has no early exit -- a failed removal does not stop the next one -- so
// in any completed run both names are gone regardless of sequence, and the
// whole internal/store suite stayed green with the two swapped. gc's
// implementation of the same invariant is pinned by
// TestReclaimCompletedConfigExchangesKeepsAMarkerWhoseRecordSurvives, which
// can observe it because that sweep really does condition one removal on the
// other; this one cannot, and had no guard at all. Injecting a crash into
// completeConfigExchange would be the faithful reproduction and is by some
// distance the riskiest change available here, so this records the sequence
// instead: it cannot alter behaviour, and it kills the swap.
var reclaimConfigExchangeOrderHook func(name string)

func reclaimConfigExchangeResidue(archive *checkedRoot, record configExchangeRecord, recordName, doneName string) {
	defer keepDescriptorOwnersAlive(archive)
	for _, name := range []string{recordName, doneName} {
		if reclaimConfigExchangeOrderHook != nil {
			reclaimConfigExchangeOrderHook(name)
		}
		reclaimConfigExchangeOwnName(archive, name)
	}
	// The archives are the opposite case: their names *are* recorded identities,
	// and archiveNamedConfigEntry already proved the named inode arrived under
	// each. Passing that recorded identity is what makes these two removals
	// provably safe -- a name is unlinked only if it still resolves to the object
	// the record says it holds. Which one exists depends on the outcome (the
	// previous file for an installed exchange, the staged one for a withdrawn
	// one); the name never created is absent and skipped, and a name some other
	// writer has taken over fails the identity check and is preserved.
	//
	// Preserved without being moved first, because both go through
	// reclaimConfigExchangeStatedArchive: this is the path whose regenerated
	// archive names an unrelated object can occupy, so it is the path that most
	// needs a foreign occupant kept out of the retire-and-restore window
	// entirely rather than walked through it.
	reclaimConfigExchangeStatedArchive(archive, configArchiveName(record.Previous), record.Previous)
	reclaimConfigExchangeStatedArchive(archive, configArchiveName(record.Staged), record.Staged)
}

// reclaimConfigExchangeFile unlinks name only if it still resolves to expected,
// a regular file. The retirement rename plus revalidation is what turns that
// into an atomic check: POSIX has no identity-conditioned unlink, so the name is
// first moved to an unpredictable sibling no other writer can address, and the
// moved object is unlinked only once it proves to be expected. Anything else is
// moved back to name untouched -- unless that restoring rename itself fails, in
// which case the object stays under the retirement name and both failures are
// returned together.
//
// Callers reclaiming residue drop the returned error: a name already collected,
// or one some other writer has taken over, is not a failure of the operation
// that produced it. They read it only to count what actually went away. So a
// failed restore is dropped with it, and the object is left parked at
// .fu-retired-entry-<random>. Nothing collects that name -- the retirement
// prefix here is random, unlike RemoveOwnedTreeAt's deterministic one, so no
// later run can attribute it back to fu, and ReclaimCompletedConfigExchanges
// sweeps only records, markers and archives. The same is true of a crash
// between the retirement rename and the unlink.
func reclaimConfigExchangeFile(archive *checkedRoot, name string, expected FileIdentity) error {
	if !validLogicalEntry(name) {
		return fmt.Errorf("reclaim config exchange entry: invalid name %q", name)
	}
	if !expected.valid() {
		return fmt.Errorf("reclaim config exchange entry %q: invalid expected identity", name)
	}
	return retireOwnedLeafAt(archive.dir, name, ".fu-retired-entry-", expected, unix.S_IFREG)
}

// reclaimConfigExchangeOwnName removes one of the two bookkeeping files an
// exchange writes for itself, a record or a terminal marker. Both are fu's own,
// written by WriteFileAtomicNoReplaceRoot under names derived from a random
// suffix nothing outside fu can predict, and no identity for either is recorded
// anywhere. There is therefore nothing to check them against but what they
// resolve to right now, and reading that here buys only the stat-to-rename race
// -- which is all these two names need. A name holding anything but a regular
// file is left where it is rather than taken through the retirement rename: fu
// never writes one, so it is not fu's to move.
//
// ReclaimCompletedConfigExchanges stands on less. It found the name by prefix,
// with no record and nothing else on disk saying fu wrote what answers to it,
// so only half of the argument above transfers: the suffix is still one
// nothing outside fu can predict, which makes the name fu's, but the object
// under it need not still be fu's object. What the sweep leans on for the rest
// is the regular-file test below -- fu writes nothing else under these two
// names, so anything else there is someone's replacement and is left alone --
// and the retirement proof, which unlinks the object that was stat'd or
// nothing at all.
// reclaimConfigExchangeOwnNameHook observes whether the regular-file pre-flight
// above admitted or rejected a name. It is nil in production and set only by the
// test that pins the check, and it exists for the same reason its archive twin
// does: a completed run cannot tell the two apart.
//
// Delete the requireRegularStat term and a directory at one of these two names
// is retired to an unpredictable sibling, then handed straight back by
// retireOwnedLeafAt's own S_IFREG revalidation -- so contents, inode and a
// collected count of zero all still hold, and the whole package stays green.
// The difference appears only if the process dies inside that window or
// RestoreRetiredAt loses an EEXIST race, and then the user's object is parked
// under a .fu-retired-entry- name this repository documents as permanently
// unattributable.
var reclaimConfigExchangeOwnNameHook func(name string, admitted bool)

func reclaimConfigExchangeOwnName(archive *checkedRoot, name string) bool {
	defer keepDescriptorOwnersAlive(archive)
	if !validLogicalEntry(name) {
		return false
	}
	stat, err := statAt(int(archive.dir.Fd()), name)
	admitted := err == nil && requireRegularStat(name, &stat) == nil
	if reclaimConfigExchangeOwnNameHook != nil {
		reclaimConfigExchangeOwnNameHook(name, admitted)
	}
	if !admitted {
		return false
	}
	return reclaimConfigExchangeFile(archive, name, identityFromStat(&stat)) == nil
}

// reclaimConfigExchangeStatedArchive removes an archive name only while it
// still resolves to the identity that name states. `fu gc` reaches it with an
// identity decoded from the name itself, which is all a sweep by prefix ever
// has to go on; the exchange path reaches it with the identity its own record
// binds, which that name restates. Either way the retirement rename plus
// revalidation is what proves the statement true before anything is unlinked.
// The stat here decides nothing that proof does not decide again; it keeps an
// object the name plainly does not describe -- an unrelated occupant of a
// regenerated name, which the exchange path can produce and does preserve --
// from being walked through a window where an interruption would leave it
// parked under an unpredictable name no evidence anywhere leads back to.
// reclaimConfigExchangeStatedArchiveHook observes whether the pre-flight
// identity check above admitted or rejected a name. It is nil in production
// and set only by the test that pins the check.
//
// Needed because a completed run cannot distinguish the two. When the check is
// deleted, an object the name does not describe is retired and then restored
// by the revalidation, so every end-state assertion -- contents equal,
// os.SameFile, collected == 0 -- still holds and the whole suite stays green.
// The difference only appears if the process dies inside that window, or if
// RestoreRetiredAt loses an EEXIST race: the user's file is then parked under
// a random name this repository documents as permanently unattributable. The
// same two conditions in CollectableConfigArchiveNames are pinned; without
// this the two copies can drift apart in the dangerous direction.
var reclaimConfigExchangeStatedArchiveHook func(name string, admitted bool)

func reclaimConfigExchangeStatedArchive(archive *checkedRoot, name string, stated FileIdentity) bool {
	defer keepDescriptorOwnersAlive(archive)
	stat, err := statAt(int(archive.dir.Fd()), name)
	admitted := err == nil && identityFromStat(&stat) == stated && requireRegularStat(name, &stat) == nil
	if reclaimConfigExchangeStatedArchiveHook != nil {
		reclaimConfigExchangeStatedArchiveHook(name, admitted)
	}
	if !admitted {
		return false
	}
	return reclaimConfigExchangeFile(archive, name, stated) == nil
}

func readPendingConfigExchangeRecords(archive *checkedRoot) ([]struct {
	record configExchangeRecord
	raw    []byte
}, error) {
	defer keepDescriptorOwnersAlive(archive)
	names, err := readCheckedRootNames(archive)
	if err != nil {
		return nil, err
	}
	var pending []struct {
		record configExchangeRecord
		raw    []byte
	}
	for _, name := range names {
		doneName, ok := configExchangeRecordMarkerName(name)
		if !ok {
			continue
		}
		// The terminal marker's name is derivable from the record's, so a
		// completed exchange is recognised with one fstatat instead of two
		// bounded reads, two JSON parses and a digest comparison. Every write
		// command runs this scan, and three files accumulate per config write
		// with nothing pruning them, so verifying every historical record made
		// the cost of a write grow with the store's whole history.
		//
		// The evidence this scan acts on is therefore the marker's *existence*,
		// never its contents, and nothing downstream re-checks them either. An
		// earlier version of this loop called a configExchangeCompleted helper
		// below, whose own version/digest/outcome validation read as a safety
		// net -- but the continue above has already proved the marker absent by
		// the time that call was reached, so it could only ever return
		// (false, nil) on ENOENT and the validation inside it never ran. Helper
		// and call are both gone rather than left in place looking
		// load-bearing; the same derivation
		// is stated honestly at ReclaimCompletedConfigExchanges, which says
		// outright that a `.done` whose record is gone is finished by
		// construction.
		if _, statErr := statAt(int(archive.dir.Fd()), doneName); statErr == nil {
			continue
		} else if !errors.Is(statErr, unix.ENOENT) {
			return nil, fmt.Errorf("inspect config exchange completion %s/%s: %w", archive.display, doneName, statErr)
		}
		raw, err := readRegularFileAt(int(archive.dir.Fd()), name, maxConfigExchangeRecordBytes)
		if err != nil {
			return nil, fmt.Errorf("read config exchange record %s/%s: %w", archive.display, name, err)
		}
		var record configExchangeRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, fmt.Errorf("decode config exchange record %s/%s: %w", archive.display, name, err)
		}
		if err := validateConfigExchangeRecord(name, record); err != nil {
			return nil, err
		}
		pending = append(pending, struct {
			record configExchangeRecord
			raw    []byte
		}{record: record, raw: raw})
	}
	return pending, nil
}

func recoverPendingConfigExchanges(target, scratch, archive *checkedRoot) error {
	pending, err := readPendingConfigExchangeRecords(archive)
	if err != nil {
		return err
	}
	for _, item := range pending {
		if err := recoverConfigExchange(target, scratch, archive, item.record, item.raw); err != nil {
			return err
		}
	}
	return nil
}

func recoverConfigExchange(target, scratch, archive *checkedRoot, record configExchangeRecord, raw []byte) error {
	defer keepDescriptorOwnersAlive(target, scratch, archive)
	candidate, err := inspectConfigObject(scratch, record.Candidate)
	if err != nil {
		return fmt.Errorf("inspect config candidate %s/%s during recovery: %w", scratch.display, record.Candidate, err)
	}
	active, err := inspectConfigObject(scratch, configSwapName)
	if err != nil {
		return fmt.Errorf("inspect active config exchange %s/%s during recovery: %w", scratch.display, configSwapName, err)
	}
	current, err := inspectConfigObject(target, "fu.yaml")
	if err != nil {
		return fmt.Errorf("inspect fu.yaml during config exchange recovery: %w", err)
	}
	previousArchiveName := configArchiveName(record.Previous)
	previousArchive, err := inspectConfigObject(archive, previousArchiveName)
	if err != nil {
		return fmt.Errorf("inspect previous-config archive during recovery: %w", err)
	}
	stagedArchiveName := configArchiveName(record.Staged)
	stagedArchive, err := inspectConfigObject(archive, stagedArchiveName)
	if err != nil {
		return fmt.Errorf("inspect staged-config archive during recovery: %w", err)
	}

	if configObjectMatches(candidate, record.Staged, record.DataDigest) {
		if err := archiveNamedConfigEntry(scratch, record.Candidate, archive, record.Staged); err != nil {
			return fmt.Errorf("archive unpublished config candidate during recovery: %w", err)
		}
		return completeConfigExchange(archive, record, raw, "withdrawn-before-publication")
	}
	if configObjectMatches(active, record.Staged, record.DataDigest) &&
		current.exists && current.identity == record.Previous {
		outcome := configExchangeWithdrawalOutcome(current, record)
		if err := archiveNamedConfigEntry(scratch, configSwapName, archive, record.Staged); err != nil {
			return fmt.Errorf("archive unpublished config exchange during recovery: %w", err)
		}
		return completeConfigExchange(archive, record, raw, outcome)
	}
	if configObjectMatches(active, record.Previous, record.ExpectDigest) &&
		configObjectMatches(current, record.Staged, record.DataDigest) {
		if err := archiveNamedConfigEntry(scratch, configSwapName, archive, record.Previous); err != nil {
			return fmt.Errorf("finish interrupted config exchange: %w", err)
		}
		return completeConfigExchange(archive, record, raw, "installed")
	}
	if active.exists && active.identity == record.Previous &&
		configObjectMatches(current, record.Staged, record.DataDigest) {
		if err := revalidateConfigExchangePair(target, scratch, record.Staged, record.Previous); err != nil {
			return err
		}
		if err := renameExchange(int(target.dir.Fd()), "fu.yaml", int(scratch.dir.Fd()), configSwapName); err != nil {
			return fmt.Errorf("restore displaced config during exchange recovery: %w", err)
		}
		if err := revalidateConfigExchangePair(target, scratch, record.Previous, record.Staged); err != nil {
			return fmt.Errorf("config exchange recovery changed state while restoring: %w", err)
		}
		if err := archiveNamedConfigEntry(scratch, configSwapName, archive, record.Staged); err != nil {
			return fmt.Errorf("archive withdrawn config after recovery: %w", err)
		}
		return completeConfigExchange(archive, record, raw, "withdrawn-after-precondition-mismatch")
	}
	if configObjectMatches(previousArchive, record.Previous, record.ExpectDigest) &&
		configObjectMatches(current, record.Staged, record.DataDigest) && !candidate.exists && !active.exists {
		return completeConfigExchange(archive, record, raw, "installed")
	}
	if configObjectMatches(stagedArchive, record.Staged, record.DataDigest) &&
		current.exists && current.identity == record.Previous && !candidate.exists && !active.exists {
		return completeConfigExchange(archive, record, raw, "withdrawn")
	}
	return configExchangeConflictError(target, scratch, archive, record)
}

// configExchangeWithdrawalOutcome labels the withdrawal recovery performs when
// fu's staged object is still parked at the active scratch name and fu.yaml
// still holds the recorded previous inode. Both readings withdraw and both take
// the same actions; they differ in what the label records about why. Bytes that
// still match the recorded expectation mean the exchange simply never took
// effect, while changed bytes under the same inode mean an external writer
// rewrote fu.yaml in place, so the precondition this exchange was conditioned
// on no longer holds.
//
// The label reaches the terminal marker, which reclaimConfigExchangeResidue
// disposes of as soon as it is durable, so nothing can read the decision back
// off disk afterwards. It is a pure function of the two inputs precisely so a
// test can assert it without a marker to read or a seam to install.
func configExchangeWithdrawalOutcome(current configObjectState, record configExchangeRecord) string {
	if current.digest == record.ExpectDigest {
		return "withdrawn-with-previous-current"
	}
	return "withdrawn-after-precondition-mismatch"
}

func configExchangeConflictError(target, scratch, archive *checkedRoot, record configExchangeRecord) error {
	recordName, err := configExchangeRecordName(record.Candidate)
	if err != nil {
		recordName = "<invalid-config-exchange-record>"
	}
	paths := []string{
		filepath.Join(target.display, "fu.yaml"),
		filepath.Join(scratch.display, record.Candidate),
		filepath.Join(scratch.display, configSwapName),
		filepath.Join(archive.display, recordName),
		filepath.Join(archive.display, configArchiveName(record.Previous)),
		filepath.Join(archive.display, configArchiveName(record.Staged)),
	}
	return fmt.Errorf("config exchange cannot be recovered safely because recorded objects changed or occupy conflicting locations; preserve these versions, compare them, move changed or conflicting entries aside, then retry: %s", strings.Join(paths, ", "))
}

func revalidateConfigExchangePair(target, scratch *checkedRoot, targetIdentity, scratchIdentity FileIdentity) error {
	defer keepDescriptorOwnersAlive(target, scratch)
	targetStat, err := statAt(int(target.dir.Fd()), "fu.yaml")
	if err != nil {
		return err
	}
	scratchStat, err := statAt(int(scratch.dir.Fd()), configSwapName)
	if err != nil {
		return err
	}
	if identityFromStat(&targetStat) != targetIdentity || identityFromStat(&scratchStat) != scratchIdentity {
		return errors.New("config exchange names changed identity during recovery")
	}
	return nil
}

// ReclaimCompletedConfigExchanges collects the config exchange bookkeeping a
// crash stranded between an exchange's durable terminal marker and the inline
// reclamation that follows it, and reports how many entries it removed. Nothing
// else ever collects it: no transaction journal describes an exchange, so `fu
// gc`'s per-family prune loop cannot reach it, and the exchange that wrote it
// has already run to completion.
//
// It works by prefix because it has to. Inline reclamation retires the record
// first -- readPendingConfigExchangeRecords enumerates records and treats one
// without its marker as pending work, so removing the marker first would make a
// finished exchange look interrupted -- which means every state an interruption
// can leave holds a bare marker or a bare archive with no record left to replay.
// Three rules make collecting by prefix safe, and only these three names are
// touched at all:
//
//   - A record whose marker is present is finished, on exactly the evidence
//     readPendingConfigExchangeRecords finishes it on: the marker's existence.
//     A record without a marker is that scan's authority to complete or withdraw
//     an interrupted exchange and is never touched.
//   - A marker is removed only once its record is gone, preserving the order
//     inline reclamation depends on. A record the sweep could not remove keeps
//     its marker, so no interrupted sweep can resurrect a finished exchange as
//     pending work.
//   - An archive is removed only if its name still resolves to the device and
//     inode that name states, and only while no record is pending at all. The
//     identity proves what the object is; it cannot prove whose it is, and only
//     an unfinished exchange can still claim one. Recovery archives an object
//     and only then writes its terminal marker, so a crash in between leaves an
//     archive whose record is pending and which is the sole remaining copy of
//     the object recovery is about to converge on.
//
// Blocking the archives on any pending record, rather than on the two names a
// particular record spans, costs nothing worth having: a write command recovers
// every pending exchange before it starts, so at most one can exist, and the
// states this sweep exists to collect are precisely the ones where that one has
// already reached its marker. It buys the sweep out of parsing another command's
// authority to decide what to delete -- it only ever stats names -- and out of
// having a failure mode at all when a record is unreadable. A record whose bytes
// are damaged wedges every write command with a remedy of its own; gc quietly
// declining to collect around it is not hiding anything.
//
// Everything else in the recovery directory belongs to someone else. Retirement
// names in particular are never swept: a rename parks whatever it found under
// .fu-retired-entry-<random>, so an object fu does not own can be sitting there
// with nothing on disk to attribute the name back to fu.
func (s *Store) ReclaimCompletedConfigExchanges() (int, error) {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.recovery == nil || s.writeRoots.recovery.dir == nil {
		return 0, errors.New("store is not attached to a checked recovery-root session")
	}
	archive := s.writeRoots.recovery
	names, err := readCheckedRootNames(archive)
	if err != nil {
		return 0, err
	}
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	collected := 0
	for _, marker := range names {
		record, ok := configExchangeMarkedRecordName(marker)
		if !ok {
			continue
		}
		if present[record] && reclaimConfigExchangeOwnName(archive, record) {
			collected++
		}
		stillRecorded, err := pathPresentAt(int(archive.dir.Fd()), record)
		if err != nil || stillRecorded {
			continue
		}
		if reclaimConfigExchangeOwnName(archive, marker) {
			collected++
		}
	}
	// Judged on the listing, which is the state before this sweep removed
	// anything, and unchanged by what it removed: only a record beside its
	// marker is collected, and a marker only once its record is gone, so no
	// removal above can turn a finished exchange into a pending one.
	if configExchangePending(names, present) {
		return collected, nil
	}
	for _, name := range names {
		identity, ok := parseConfigArchiveName(name)
		if !ok {
			continue
		}
		if reclaimConfigExchangeStatedArchive(archive, name, identity) {
			collected++
		}
	}
	return collected, nil
}

// CollectableConfigArchiveNames returns the subset of names that the archive
// sweep in ReclaimCompletedConfigExchanges would actually unlink, judged by
// the same two conditions that sweep applies: the name must round-trip through
// the archive naming grammar, and it must still resolve to the very
// device/inode it states it holds.
//
// It exists so the read-only inventory can report an archive as collectable
// only when gc will really collect it. Neither condition is a formality. A
// malformed .fu-config-archive-* name is skipped by parseConfigArchiveName and
// never collected at all; a well-formed name whose inode has since drifted is
// preserved on every run by design -- that preservation is what
// TestReclaimCompletedConfigExchangesPreservesAnArchiveNameItDoesNotDescribe
// pins -- so both are permanent residue, and reporting them as collectable is
// the same empty promise an unclaimed removed- payload was.
//
// This reads names and inode metadata only; no archive is opened or parsed.
// Callers still have to decide the freeze question separately
// (PendingConfigExchangeRecords), since a pending exchange stops the sweep
// before it reaches any of these.
func (s *Store) CollectableConfigArchiveNames(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	// Pinned when a write session is open, by pathname when it is not, the
	// same way scanTxnJournalReport (engine/txn.go) reaches this directory.
	// `fu status` takes no lock and opens no session by design, so the
	// pathname branch is its ordinary route; the worst a swap under it costs a
	// read-only report is a stale count.
	archive := s.checkedRecovery()
	for _, name := range names {
		stated, ok := parseConfigArchiveName(name)
		if !ok {
			continue
		}
		var stat unix.Stat_t
		var err error
		if archive != nil {
			stat, err = statAt(int(archive.dir.Fd()), name)
		} else {
			err = unix.Lstat(filepath.Join(s.RecoveryDir(), name), &stat)
		}
		if err != nil || identityFromStat(&stat) != stated || requireRegularStat(name, &stat) != nil {
			continue
		}
		out[name] = true
	}
	if archive != nil {
		keepDescriptorOwnersAlive(archive)
	}
	return out
}

// checkedRecovery returns the pinned recovery root when this store is attached
// to a write session, and nil when it is not. Read-only callers use the nil
// case deliberately: `fu status` neither opens a session nor takes fu.lock.
func (s *Store) checkedRecovery() *checkedRoot {
	if s.writeRoots == nil {
		return nil
	}
	return s.writeRoots.recovery
}

// RecoveryNames lists the recovery directory, through the pinned descriptor
// when one is held and by pathname otherwise. Same fallback as
// scanTxnJournalReport's, kept in one place so a read-only reporter does not
// have to restate it.
func (s *Store) RecoveryNames() ([]string, error) {
	return s.logicalRootNames(s.checkedRecovery(), s.RecoveryDir())
}

// StagingNames is RecoveryNames for the staging directory.
func (s *Store) StagingNames() ([]string, error) {
	var staging *checkedRoot
	if s.writeRoots != nil {
		staging = s.writeRoots.staging
	}
	return s.logicalRootNames(staging, s.StagingDir())
}

func (s *Store) logicalRootNames(root *checkedRoot, dir string) ([]string, error) {
	if root != nil {
		return readCheckedRootNames(root)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// CollectableConfigExchangeNames returns the subset of names that
// ReclaimCompletedConfigExchanges would actually unlink from the record/marker
// side of its sweep, judged by that sweep's own strict grammar.
//
// It exists because "not pending" and "collectable" are not complements, and
// treating them as such made the inventory promise collections that never
// happen. The pending scan uses the loose grammar
// (configExchangeRecordMarkerName: prefix plus ".json", hex unchecked); the
// collector uses the strict one (configExchangeMarkedRecordName, which
// round-trips through configCandidateSuffix and requires exactly 16 hex
// digits). Anything carrying the prefix but failing the strict form -- a
// ".json.bak" an editor left, a non-hex suffix -- was counted Collectable and
// then walked past by every `fu gc` run forever.
//
// fu never writes such a name itself, but a user can reach the state by hand,
// which is the same reachability argument that hardened the neighbouring
// .fu-config-archive- case. Keeping the judgement here rather than in the
// reporter is the point: the grammar was written twice, in two packages, at
// two strictnesses, and that is what let them disagree.
//
// Names only; nothing is opened or stat'd. Callers must still apply the freeze
// separately (PendingConfigExchangeRecords), since one pending exchange stops
// the sweep before it reaches any archive.
func CollectableConfigExchangeNames(names []string) map[string]bool {
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	out := make(map[string]bool, len(names))
	for _, marker := range names {
		record, ok := configExchangeMarkedRecordName(marker)
		if !ok {
			continue
		}
		// Both, when both are there. The sweep removes the record because its
		// marker exists, then re-checks the record and removes the marker
		// because it is now gone -- so a complete pair is fully collected
		// within a single run, not one name per run.
		if present[record] {
			out[record] = true
		}
		out[marker] = true
	}
	return out
}

// PendingConfigExchangeRecords returns the names in a recovery-directory
// listing that are config exchange records with no completion marker beside
// them -- the exchanges this package's sweep will not collect, and which freeze
// its archive sweep along with them (ReclaimCompletedConfigExchanges). It
// exists so a read-only reporter can say what `fu gc` would and would not
// collect without restating the naming grammar: the answer comes from
// pendingConfigExchangeRecords below, the same derivation gc and recovery
// already share. Names alone are read; nothing is opened, parsed or stat'd.
func PendingConfigExchangeRecords(names []string) []string {
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	return pendingConfigExchangeRecords(names, present)
}

// PendingConfigExchangeStagingNames returns every entry under staging that the
// given pending exchange records still govern: each record's own candidate,
// plus the single active swap name they all share. A caller holding the record
// names could take these apart itself, and deliberately does not -- the naming
// scheme would then live in two places, which is the disagreement between
// recovery and its readers that PendingConfigExchangeRecords already exists to
// prevent.
//
// Records that do not parse are skipped rather than reported. The only caller
// is a read-only inventory, and it passes names this package just selected as
// pending, so a malformed one is not a state this function can usefully raise:
// the write path fails loudly on it long before a count would matter.
func PendingConfigExchangeStagingNames(pendingRecords []string) []string {
	if len(pendingRecords) == 0 {
		return nil
	}
	names := make([]string, 0, len(pendingRecords)+1)
	for _, record := range pendingRecords {
		suffix := strings.TrimSuffix(strings.TrimPrefix(record, configExchangeRecordPrefix), ".json")
		candidate := configCandidatePrefix + suffix
		if _, err := configCandidateSuffix(candidate); err != nil {
			continue
		}
		names = append(names, candidate)
	}
	return append(names, configSwapName)
}

// configExchangePending reports whether any exchange is still unfinished, by
// the same predicate readPendingConfigExchangeRecords selects pending work
// with -- configExchangeRecordMarkerName. The two reach it differently and
// deliberately: this side derives the marker name and asks whether the listing
// already holds it, while readPendingConfigExchangeRecords derives the same
// name and settles it with one fstatat, because it runs on every write command
// and must not grow a listing. Sharing the derivation, not the lookup, is what
// keeps gc from ever disagreeing with recovery about which records still have
// authority -- including over a record recovery can only fail on, which stays
// pending precisely because nothing can finish it.
func configExchangePending(names []string, present map[string]bool) bool {
	return len(pendingConfigExchangeRecords(names, present)) != 0
}

func pendingConfigExchangeRecords(names []string, present map[string]bool) []string {
	var pending []string
	for _, name := range names {
		doneName, ok := configExchangeRecordMarkerName(name)
		if !ok {
			continue
		}
		if !present[doneName] {
			pending = append(pending, name)
		}
	}
	return pending
}

// configExchangeRecordMarkerName reports whether name is a config exchange
// record and, when it is, returns the name of the terminal marker that would
// finish it. This is the one place that grammar is stated: recovery selects
// pending work with it in readPendingConfigExchangeRecords, and gc gates the
// archive sweep on it in configExchangePending. The sweep's whole safety
// argument is that the two never disagree about which records still have
// authority, so a second copy of the test -- which is what these two carried
// before -- is exactly the drift that argument cannot survive.
func configExchangeRecordMarkerName(name string) (string, bool) {
	if !strings.HasPrefix(name, configExchangeRecordPrefix) || !strings.HasSuffix(name, ".json") {
		return "", false
	}
	return strings.TrimSuffix(name, ".json") + ".done", true
}

// configExchangeMarkedRecordName returns the record name of the family a
// terminal marker names. The derivation runs through the same functions that
// produced both names, so a name outside that exact grammar is rejected here
// rather than judged on its prefix alone.
func configExchangeMarkedRecordName(marker string) (string, bool) {
	body, ok := strings.CutSuffix(marker, ".done")
	if !ok || !strings.HasPrefix(body, configExchangeRecordPrefix) {
		return "", false
	}
	record, err := configExchangeRecordName(configCandidatePrefix + strings.TrimPrefix(body, configExchangeRecordPrefix))
	if err != nil {
		return "", false
	}
	return record, true
}

// parseConfigArchiveName decodes the identity an archive name states it holds.
// The round-trip is the validation: a name is accepted only if configArchiveName
// would produce exactly it for the identity decoded, which leaves no second
// grammar to keep in step with the one that writes these names.
func parseConfigArchiveName(name string) (FileIdentity, bool) {
	if !strings.HasPrefix(name, configArchivePrefix) {
		return FileIdentity{}, false
	}
	deviceHex, inodeHex, found := strings.Cut(strings.TrimPrefix(name, configArchivePrefix), "-")
	if !found {
		return FileIdentity{}, false
	}
	device, err := strconv.ParseUint(deviceHex, 16, 64)
	if err != nil {
		return FileIdentity{}, false
	}
	inode, err := strconv.ParseUint(inodeHex, 16, 64)
	if err != nil {
		return FileIdentity{}, false
	}
	identity := FileIdentity{Device: device, Inode: inode}
	return identity, identity.valid() && configArchiveName(identity) == name
}
