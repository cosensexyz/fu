package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

const (
	adoptLinkArchiveVersion        = 1
	adoptLinkArchivePrefix         = "adopt-link-"
	adoptLinkArchiveEntry          = "entry"
	adoptLinkArchiveWholeDirectory = "whole-directory"
	maxAdoptLinkArchiveBytes       = int64(64 << 10)
)

var adoptLinkArchiveNamePattern = regexp.MustCompile(`^adopt-link-[0-9a-f]{64}\.json$`)

// adoptLinkArchiveRecord is a durable, non-journal description of a symlink
// removed by adopt. Its content-addressed file remains after transaction GC so
// a future restore command can reconstruct the entry without dereferencing it.
type adoptLinkArchiveRecord struct {
	Version      int                `json:"version"`
	Kind         string             `json:"kind"`
	Agent        string             `json:"agent"`
	Skill        string             `json:"skill"`
	OriginalPath string             `json:"original_path"`
	RawTarget    string             `json:"raw_target"`
	Mode         uint32             `json:"mode"`
	Identity     store.FileIdentity `json:"identity"`
}

func newAdoptLinkArchiveRecord(kind, agentName, skillName, originalPath, rawTarget string, mode uint32, identity store.FileIdentity) adoptLinkArchiveRecord {
	return adoptLinkArchiveRecord{
		Version: adoptLinkArchiveVersion, Kind: kind, Agent: agentName, Skill: skillName,
		OriginalPath: filepath.Clean(originalPath), RawTarget: rawTarget, Mode: mode, Identity: identity,
	}
}

func (r adoptLinkArchiveRecord) validate() error {
	if r.Version != adoptLinkArchiveVersion {
		return fmt.Errorf("adopt link archive has unsupported version %d", r.Version)
	}
	if r.Kind != adoptLinkArchiveEntry && r.Kind != adoptLinkArchiveWholeDirectory {
		return fmt.Errorf("adopt link archive has invalid kind %q", r.Kind)
	}
	if r.Agent == "" || strings.ContainsAny(r.Agent, `/\\`) {
		return fmt.Errorf("adopt link archive has invalid agent %q", r.Agent)
	}
	if err := skill.ValidateName(r.Skill); err != nil {
		return fmt.Errorf("adopt link archive has invalid skill: %w", err)
	}
	if !filepath.IsAbs(r.OriginalPath) || filepath.Clean(r.OriginalPath) != r.OriginalPath {
		return fmt.Errorf("adopt link archive has invalid original path %q", r.OriginalPath)
	}
	if r.RawTarget == "" {
		return errors.New("adopt link archive has an empty raw target")
	}
	if !adoptIdentityValid(r.Identity) {
		return errors.New("adopt link archive has an invalid identity")
	}
	if os.FileMode(r.Mode).Type() != fs.ModeSymlink {
		return fmt.Errorf("adopt link archive mode %#o is not a symlink", r.Mode)
	}
	return nil
}

func marshalAdoptLinkArchive(record adoptLinkArchiveRecord) ([]byte, string, error) {
	if err := record.validate(); err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, "", err
	}
	if int64(len(raw)) > maxAdoptLinkArchiveBytes {
		return nil, "", fmt.Errorf("adopt link archive size %d exceeds limit %d", len(raw), maxAdoptLinkArchiveBytes)
	}
	digest := sha256.Sum256(raw)
	return raw, adoptLinkArchivePrefix + hex.EncodeToString(digest[:]) + ".json", nil
}

func ensureAdoptLinkArchive(st *store.Store, record adoptLinkArchiveRecord) (string, error) {
	raw, name, err := marshalAdoptLinkArchive(record)
	if err != nil {
		return "", err
	}
	if err := writeTxnFileNoReplace(st, name, raw); err == nil {
		return name, nil
	} else if !errors.Is(err, fs.ErrExist) && !errors.Is(err, unix.EEXIST) {
		return "", fmt.Errorf("write durable adopt link archive %s: %w", txnDisplayPath(st, name), err)
	}
	if err := validateAdoptLinkArchive(st, name, record); err != nil {
		return "", err
	}
	return name, nil
}

func validateAdoptLinkArchive(st *store.Store, name string, record adoptLinkArchiveRecord) error {
	expectedRaw, expectedName, err := marshalAdoptLinkArchive(record)
	if err != nil {
		return fmt.Errorf("%w: invalid durable adopt link archive metadata: %v", ErrTxnConflict, err)
	}
	if name != expectedName || !adoptLinkArchiveNamePattern.MatchString(name) {
		return fmt.Errorf("%w: adopt link archive name %q does not match its recorded content %q", ErrTxnConflict, name, expectedName)
	}
	if st == nil {
		return fmt.Errorf("%w: cannot validate durable adopt link archive %q without a checked store", ErrTxnConflict, name)
	}
	var raw []byte
	if _, rootErr := st.Root(); rootErr == nil {
		recoveryRoot, rootErr := st.RecoveryRoot()
		if rootErr != nil {
			return rootErr
		}
		raw, err = store.ReadRegularFileRoot(recoveryRoot, name, maxAdoptLinkArchiveBytes)
	} else {
		raw, err = store.ReadRegularFile(txnDisplayPath(st, name), maxAdoptLinkArchiveBytes)
	}
	if err != nil {
		return fmt.Errorf("%w: read durable adopt link archive %s: %v", ErrTxnConflict, txnDisplayPath(st, name), err)
	}
	if !bytes.Equal(raw, expectedRaw) {
		return fmt.Errorf("%w: durable adopt link archive %s no longer matches the removed symlink", ErrTxnConflict, txnDisplayPath(st, name))
	}
	return nil
}

func validAdoptLinkArchiveName(name string) bool {
	return adoptLinkArchiveNamePattern.MatchString(name)
}
