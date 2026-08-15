package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
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

func configExchangeCompleted(archive *checkedRoot, record configExchangeRecord, raw []byte) (bool, error) {
	defer keepDescriptorOwnersAlive(archive)
	doneName, err := configExchangeDoneName(record.Candidate)
	if err != nil {
		return false, err
	}
	doneRaw, err := readRegularFileAt(int(archive.dir.Fd()), doneName, maxConfigExchangeRecordBytes)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read config exchange completion %s/%s: %w", archive.display, doneName, err)
	}
	var completion configExchangeCompletion
	if err := json.Unmarshal(doneRaw, &completion); err != nil {
		return false, fmt.Errorf("decode config exchange completion %s/%s: %w", archive.display, doneName, err)
	}
	wantDigest := digestConfigExchangeBytes(raw)
	if completion.Version != configExchangeRecordVersion || completion.RecordDigest != wantDigest || completion.Outcome == "" {
		return false, fmt.Errorf("config exchange completion %s/%s does not match its record", archive.display, doneName)
	}
	return true, nil
}

func completeConfigExchange(archive *checkedRoot, record configExchangeRecord, raw []byte, outcome string) error {
	defer keepDescriptorOwnersAlive(archive)
	doneName, err := configExchangeDoneName(record.Candidate)
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
			return nil
		}
		return fmt.Errorf("persist config exchange completion %s/%s: %w", archive.display, doneName, err)
	}
	return nil
}

func readPendingConfigExchangeRecords(archive *checkedRoot) ([]struct {
	record configExchangeRecord
	raw    []byte
}, error) {
	defer keepDescriptorOwnersAlive(archive)
	dir, err := reopenDirNoFollow(int(archive.dir.Fd()), ".", archive.display, false, 0o755)
	if err != nil {
		return nil, err
	}
	names, readErr := dir.Readdirnames(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	sort.Strings(names)
	var pending []struct {
		record configExchangeRecord
		raw    []byte
	}
	for _, name := range names {
		if !strings.HasPrefix(name, configExchangeRecordPrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		// The terminal marker's name is derivable from the record's, so a
		// completed exchange is recognised with one fstatat instead of two
		// bounded reads, two JSON parses and a digest comparison. Every write
		// command runs this scan, and three files accumulate per config write
		// with nothing pruning them, so verifying every historical record made
		// the cost of a write grow with the store's whole history. The marker's
		// contents are still verified whenever a record is actually acted on --
		// which is the only time it decides anything.
		doneName := strings.TrimSuffix(name, ".json") + ".done"
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
		completed, err := configExchangeCompleted(archive, record, raw)
		if err != nil {
			return nil, err
		}
		if !completed {
			pending = append(pending, struct {
				record configExchangeRecord
				raw    []byte
			}{record: record, raw: raw})
		}
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
		outcome := "withdrawn-after-precondition-mismatch"
		if current.digest == record.ExpectDigest {
			outcome = "withdrawn-with-previous-current"
		}
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
