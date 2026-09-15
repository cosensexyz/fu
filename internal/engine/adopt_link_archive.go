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
	// adoptLinkArchiveVersion is what this build writes. Version 1 recorded an
	// identity with the file handle stripped, so that the archive's bytes --
	// and therefore its content-addressed name -- could be reproduced by
	// binaries predating handles. The cost was that every archive fell back to
	// device and inode, which a deleted file's replacement can reuse; version 2
	// keeps the strongest identity the platform supplied.
	//
	// Nothing migrates. Existing archives keep their bytes and their names, and
	// are read at the version they were written at (marshalAdoptLinkArchiveAt).
	adoptLinkArchiveVersion = 2
	// adoptLinkArchiveVersionLegacy is the handle-free encoding. Still written
	// by no one and still readable by everyone.
	adoptLinkArchiveVersionLegacy  = 1
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
	Version      int    `json:"version"`
	Kind         string `json:"kind"`
	Agent        string `json:"agent"`
	Skill        string `json:"skill"`
	OriginalPath string `json:"original_path"`
	RawTarget    string `json:"raw_target"`
	Mode         uint32 `json:"mode"`
	// Identity is the strongest identity available when the record was
	// written. What it proves depends on the version it was written at, which
	// is what evidence() answers; FileIdentity.Same is not that answer, because
	// it degrades to device and inode whenever either side lacks a handle and
	// says nothing about which side that was.
	Identity store.FileIdentity `json:"identity"`
}

// adoptLinkEvidence is how much a record's identity proves about the object it
// describes. A future restore command asks this rather than asking Same, whose
// true means "consistent as far as both sides can tell" and not "the same
// object".
type adoptLinkEvidence int

const (
	// adoptLinkEvidenceUnknown: a version 1 record. It carries no handle, and
	// that says nothing -- version 1 recorded none whatever the platform could
	// supply, so the absence is a property of the format, not of the object.
	//
	// At iota 0 deliberately. A strength read out of an unset field must come
	// back as the weakest answer; twice in this repo a zero value that meant a
	// real verdict turned "nothing was decided" into a confident wrong one.
	adoptLinkEvidenceUnknown adoptLinkEvidence = iota
	// adoptLinkEvidenceDeviceInode: a current record whose platform exported no
	// handle. Weaker than a handle, but the weakness is known and bounded.
	adoptLinkEvidenceDeviceInode
	// adoptLinkEvidenceHandle: a current record with a handle, which survives
	// inode reuse.
	adoptLinkEvidenceHandle
)

func (e adoptLinkEvidence) String() string {
	switch e {
	case adoptLinkEvidenceUnknown:
		return "unknown (archive predates recorded handles)"
	case adoptLinkEvidenceDeviceInode:
		return "device and inode only (no handle available when written)"
	case adoptLinkEvidenceHandle:
		return "file handle"
	}
	return fmt.Sprintf("adoptLinkEvidence(%d)", int(e))
}

// evidence reports what this record's identity proves.
//
// Nothing in production calls this yet: the restore command that would consult
// it is deliberately out of scope for this batch. So the prohibition it exists
// to serve -- never read a version 1 record as handle-backed proof -- is today
// a tested type and a documented rule rather than an enforced one. A future
// restore reaching for FileIdentity.Same instead would compile, pass vet, and
// be wrong, because Same degrades to device and inode whenever either side
// lacks a handle and says nothing about which side that was.
func (r adoptLinkArchiveRecord) evidence() adoptLinkEvidence {
	if r.Version <= adoptLinkArchiveVersionLegacy {
		return adoptLinkEvidenceUnknown
	}
	if r.Identity.Handle == "" {
		return adoptLinkEvidenceDeviceInode
	}
	return adoptLinkEvidenceHandle
}

func newAdoptLinkArchiveRecord(kind, agentName, skillName, originalPath, rawTarget string, mode uint32, identity store.FileIdentity) adoptLinkArchiveRecord {
	return adoptLinkArchiveRecord{
		Version: adoptLinkArchiveVersion, Kind: kind, Agent: agentName, Skill: skillName,
		OriginalPath: filepath.Clean(originalPath), RawTarget: rawTarget, Mode: mode, Identity: identity,
	}
}

func (r adoptLinkArchiveRecord) validate() error {
	if r.Version != adoptLinkArchiveVersion && r.Version != adoptLinkArchiveVersionLegacy {
		return fmt.Errorf("adopt link archive has unsupported version %d", r.Version)
	}
	// fu never wrote a version 1 record carrying a handle -- that encoding
	// strips it -- so one here would mean the record was built wrong.
	//
	// Unreachable from the marshal path, which sets the version and strips the
	// handle before calling this, and nothing yet decodes an on-disk archive
	// into a record: a hand-edited file is caught by the byte comparison
	// instead. This is pre-positioning for the decoder a restore command would
	// need, so that the rule is already here when something can reach it.
	if r.Version == adoptLinkArchiveVersionLegacy && r.Identity.Handle != "" {
		return errors.New("adopt link archive version 1 cannot carry a handle")
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
	if !r.Identity.Valid() {
		return errors.New("adopt link archive has an invalid identity")
	}
	if os.FileMode(r.Mode).Type() != fs.ModeSymlink {
		return fmt.Errorf("adopt link archive mode %#o is not a symlink", r.Mode)
	}
	return nil
}

// marshalAdoptLinkArchive encodes a record at the version this build writes.
func marshalAdoptLinkArchive(record adoptLinkArchiveRecord) ([]byte, string, error) {
	return marshalAdoptLinkArchiveAt(record, adoptLinkArchiveVersion)
}

// marshalAdoptLinkArchiveAt encodes a record at a given version, which is what
// makes an archive written by an earlier build reproducible from a record built
// today.
//
// The record handed to validation is rebuilt from the journal, and since batch 4
// that journal identity carries a handle. Encoding it at the current version
// would give a different digest and therefore a different name, so a perfectly
// intact version 1 archive would fail validation and an interrupted adopt would
// become unresumable. Encoding at the file's own version keeps it byte for byte
// what it was -- without touching what is on disk, which is the one thing format
// compatibility here may not do.
func marshalAdoptLinkArchiveAt(record adoptLinkArchiveRecord, version int) ([]byte, string, error) {
	record.Version = version
	if version == adoptLinkArchiveVersionLegacy {
		// The one handle strip in the engine, and the reason identity_test.go's
		// downgrade allowlist names this function. Version 1 never recorded a
		// handle, so reproducing a version 1 archive means dropping one.
		//
		// The allowlist is function-scoped, not branch-scoped: a strip added
		// anywhere else in this function would be permitted too. Nothing
		// enforces that this stays the only one -- it is why the line says so.
		record.Identity.Handle = ""
	}
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

// validateAdoptLinkArchive confirms the archive at name still describes the
// symlink the caller reconstructed.
//
// The file is read before the expectation is built, because the expectation
// depends on the version the file was written at: an archive from before
// handles must be re-encoded without one to reproduce, and encoding it the way
// this build writes would fail an intact record. Nothing on disk is touched or
// rewritten -- the file's own version decides how it is read.
func validateAdoptLinkArchive(st *store.Store, name string, record adoptLinkArchiveRecord) error {
	if !adoptLinkArchiveNamePattern.MatchString(name) {
		return fmt.Errorf("%w: adopt link archive name %q is not a valid archive name", ErrTxnConflict, name)
	}
	if st == nil {
		return fmt.Errorf("%w: cannot validate durable adopt link archive %q without a checked store", ErrTxnConflict, name)
	}
	var err error
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
	version, err := peekAdoptLinkArchiveVersion(raw)
	if err != nil {
		return fmt.Errorf("%w: read durable adopt link archive %s: %v", ErrTxnConflict, txnDisplayPath(st, name), err)
	}
	expectedRaw, expectedName, err := marshalAdoptLinkArchiveAt(record, version)
	if err != nil {
		return fmt.Errorf("%w: invalid durable adopt link archive metadata: %v", ErrTxnConflict, err)
	}
	if name != expectedName {
		return fmt.Errorf("%w: adopt link archive name %q does not match its recorded content %q", ErrTxnConflict, name, expectedName)
	}
	if !bytes.Equal(raw, expectedRaw) {
		return fmt.Errorf("%w: durable adopt link archive %s no longer matches the removed symlink", ErrTxnConflict, txnDisplayPath(st, name))
	}
	return nil
}

// peekAdoptLinkArchiveVersion reads only the version out of an archive's bytes,
// so the rest can be decoded by the rules that version implies.
func peekAdoptLinkArchiveVersion(raw []byte) (int, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return 0, fmt.Errorf("decode adopt link archive version: %w", err)
	}
	if header.Version != adoptLinkArchiveVersion && header.Version != adoptLinkArchiveVersionLegacy {
		return 0, fmt.Errorf("adopt link archive has unsupported version %d", header.Version)
	}
	return header.Version, nil
}

func validAdoptLinkArchiveName(name string) bool {
	return adoptLinkArchiveNamePattern.MatchString(name)
}
