package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/skill"
)

// ErrOwnedTreeChanged means transaction-owned archive content no longer
// matches the identity and manifest recorded before it entered recovery.
var ErrOwnedTreeChanged = errors.New("transaction-owned tree changed externally")

var errUnsupportedOwnedType = errors.New("unsupported transaction-owned filesystem type")

// FileIdentity is a persistent identity for one filesystem entry on the
// supported POSIX platforms. Renames preserve it; replacement does not.
type FileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// OwnedTreeEntry records one descendant of a transaction-owned directory.
// Paths always use slash separators and are relative to the tree root.
type OwnedTreeEntry struct {
	Path     string       `json:"path"`
	Kind     string       `json:"kind"`
	Mode     uint32       `json:"mode"`
	Identity FileIdentity `json:"identity"`
	Digest   string       `json:"digest,omitempty"`
	Target   string       `json:"target,omitempty"`
}

// OwnedTree is the archival authority for one recovery payload. Every recorded
// entry must be present and match exactly, and unknown entries are never
// consumed: the manifest describes the payload, not a subset of it.
type OwnedTree struct {
	RootIdentity FileIdentity     `json:"root_identity"`
	RootMode     uint32           `json:"root_mode"`
	Entries      []OwnedTreeEntry `json:"entries"`
}

const (
	ownedDirectory = "directory"
	ownedFile      = "file"
	ownedSymlink   = "symlink"
)

func (id FileIdentity) valid() bool {
	return id.Inode != 0
}

func (tree OwnedTree) Validate() error {
	if !tree.RootIdentity.valid() {
		return errors.New("owned tree has no root identity")
	}
	if os.FileMode(tree.RootMode).Type() != os.ModeDir {
		return errors.New("owned tree root is not recorded as a directory")
	}
	byPath := make(map[string]OwnedTreeEntry, len(tree.Entries))
	previous := ""
	for i, entry := range tree.Entries {
		if entry.Path == "" || entry.Path == "." || path.IsAbs(entry.Path) || path.Clean(entry.Path) != entry.Path ||
			entry.Path == ".." || strings.HasPrefix(entry.Path, "../") {
			return fmt.Errorf("owned tree entry %d has unsafe path %q", i, entry.Path)
		}
		if previous != "" && entry.Path <= previous {
			return fmt.Errorf("owned tree entries are not strictly sorted at %q", entry.Path)
		}
		previous = entry.Path
		if !entry.Identity.valid() {
			return fmt.Errorf("owned tree entry %q has no filesystem identity", entry.Path)
		}
		mode := os.FileMode(entry.Mode)
		switch entry.Kind {
		case ownedDirectory:
			if mode.Type() != os.ModeDir || entry.Digest != "" || entry.Target != "" {
				return fmt.Errorf("owned tree directory %q has inconsistent metadata", entry.Path)
			}
		case ownedFile:
			if mode.Type() != 0 || len(entry.Digest) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(entry.Digest, "sha256:") || entry.Target != "" {
				return fmt.Errorf("owned tree file %q has inconsistent metadata", entry.Path)
			}
			if _, err := hex.DecodeString(strings.TrimPrefix(entry.Digest, "sha256:")); err != nil {
				return fmt.Errorf("owned tree file %q has invalid digest: %w", entry.Path, err)
			}
		case ownedSymlink:
			if mode.Type() != os.ModeSymlink || entry.Digest != "" {
				return fmt.Errorf("owned tree symlink %q has inconsistent metadata", entry.Path)
			}
		default:
			return fmt.Errorf("owned tree entry %q has unknown kind %q", entry.Path, entry.Kind)
		}
		byPath[entry.Path] = entry
	}
	for _, entry := range tree.Entries {
		parent := path.Dir(entry.Path)
		if parent == "." {
			continue
		}
		parentEntry, ok := byPath[parent]
		if !ok || parentEntry.Kind != ownedDirectory {
			return fmt.Errorf("owned tree entry %q has no recorded parent directory", entry.Path)
		}
	}
	return nil
}

func identityFromStat(stat *unix.Stat_t) FileIdentity {
	return FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
}

func modeAndKind(stat *unix.Stat_t) (os.FileMode, string, error) {
	raw := uint32(stat.Mode)
	mode := os.FileMode(raw & 0o777)
	if raw&uint32(unix.S_ISUID) != 0 {
		mode |= os.ModeSetuid
	}
	if raw&uint32(unix.S_ISGID) != 0 {
		mode |= os.ModeSetgid
	}
	if raw&uint32(unix.S_ISVTX) != 0 {
		mode |= os.ModeSticky
	}
	switch raw & uint32(unix.S_IFMT) {
	case uint32(unix.S_IFDIR):
		return mode | os.ModeDir, ownedDirectory, nil
	case uint32(unix.S_IFREG):
		return mode, ownedFile, nil
	case uint32(unix.S_IFLNK):
		return mode | os.ModeSymlink, ownedSymlink, nil
	default:
		return 0, "", fmt.Errorf("%w: mode %#o", errUnsupportedOwnedType, raw)
	}
}

func statAt(parentFD int, name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	return stat, err
}

func readlinkAt(parentFD int, name string) (string, error) {
	size := 128
	for size <= 1<<20 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(parentFD, name, buf)
		if err != nil {
			return "", err
		}
		if n < len(buf) {
			return string(buf[:n]), nil
		}
		size *= 2
	}
	return "", fmt.Errorf("symlink target at %q exceeds 1 MiB", name)
}

func hashFileAt(parentFD int, name string, expected FileIdentity) (string, unix.Stat_t, error) {
	return hashFileAtWithHooks(parentFD, name, expected, regularFileReadHooks{})
}

func hashFileAtWithHooks(parentFD int, name string, expected FileIdentity, hooks regularFileReadHooks) (string, unix.Stat_t, error) {
	file, stat, err := openRegularFileAt(parentFD, name)
	if err != nil {
		if expected.valid() && (errors.Is(err, errRegularFileChanged) || errors.Is(err, errUnsupportedOwnedType)) {
			return "", unix.Stat_t{}, fmt.Errorf("%w: file %q changed type while being inspected: %v", ErrOwnedTreeChanged, name, err)
		}
		return "", unix.Stat_t{}, err
	}
	if expected.valid() && identityFromStat(&stat) != expected {
		_ = file.Close()
		return "", unix.Stat_t{}, fmt.Errorf("%w: file %q was replaced while being inspected", ErrOwnedTreeChanged, name)
	}
	h := sha256.New()
	byteCount, copyErr := io.Copy(h, file)
	if copyErr != nil {
		_ = file.Close()
		return "", unix.Stat_t{}, copyErr
	}
	if err := finishRegularFileRead(file, name, stat, byteCount, hooks); err != nil {
		if errors.Is(err, errRegularFileChanged) {
			return "", unix.Stat_t{}, fmt.Errorf("%w: file %q changed while being hashed: %v", ErrOwnedTreeChanged, name, err)
		}
		return "", unix.Stat_t{}, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), stat, nil
}

func snapshotOwnedTree(root *checkedRoot, name string) (OwnedTree, error) {
	defer keepDescriptorOwnersAlive(root)
	if root == nil || root.dir == nil {
		return OwnedTree{}, errors.New("checked logical root is unavailable")
	}
	if !validLogicalEntry(name) {
		return OwnedTree{}, fmt.Errorf("snapshot beneath checked root requires a single-component name: %q", name)
	}
	fd, err := unix.Openat(int(root.dir.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return OwnedTree{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return OwnedTree{}, errors.New("invalid transaction-owned root descriptor")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return OwnedTree{}, err
	}
	rootMode, rootKind, err := modeAndKind(&stat)
	if err != nil {
		_ = file.Close()
		return OwnedTree{}, err
	}
	if rootKind != ownedDirectory {
		_ = file.Close()
		return OwnedTree{}, fmt.Errorf("transaction-owned root %q is not a directory", name)
	}
	tree := OwnedTree{RootIdentity: identityFromStat(&stat), RootMode: uint32(rootMode)}
	if err := scanOwnedDirectory(file, "", &tree.Entries, 0); err != nil {
		_ = file.Close()
		return OwnedTree{}, err
	}
	if err := file.Close(); err != nil {
		return OwnedTree{}, err
	}
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Path < tree.Entries[j].Path })
	if err := tree.Validate(); err != nil {
		return OwnedTree{}, err
	}
	return tree, nil
}

// SnapshotRootOwned captures an exact identity-and-content manifest for a
// directory already pinned by os.Root. Unlike a skill projection it includes
// every entry, including .git, and records the root directory mode.
func SnapshotRootOwned(root *os.Root, rel string) (OwnedTree, error) {
	return snapshotRootOwned(root, rel, 0)
}

// SnapshotRootOwnedForCopy captures the same exact manifest while applying
// the copy primitive's regular-file size limit before hashing. Adopt uses it
// before retiring a user's directory so a deterministic copy failure remains
// isolatable.
func SnapshotRootOwnedForCopy(root *os.Root, rel string) (OwnedTree, error) {
	return snapshotRootOwned(root, rel, skill.MaxSourceFileBytes)
}

func snapshotRootOwned(root *os.Root, rel string, maxFileBytes int64) (OwnedTree, error) {
	if root == nil {
		return OwnedTree{}, errors.New("snapshot rooted tree: root is nil")
	}
	dir, err := root.Open(rel)
	if err != nil {
		return OwnedTree{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		_ = dir.Close()
		return OwnedTree{}, err
	}
	rootMode, rootKind, err := modeAndKind(&stat)
	if err != nil {
		_ = dir.Close()
		return OwnedTree{}, err
	}
	if rootKind != ownedDirectory {
		_ = dir.Close()
		return OwnedTree{}, fmt.Errorf("rooted source %q is not a directory", rel)
	}
	tree := OwnedTree{RootIdentity: identityFromStat(&stat), RootMode: uint32(rootMode)}
	if err := scanOwnedDirectory(dir, "", &tree.Entries, maxFileBytes); err != nil {
		_ = dir.Close()
		return OwnedTree{}, err
	}
	if err := dir.Close(); err != nil {
		return OwnedTree{}, err
	}
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Path < tree.Entries[j].Path })
	if err := tree.Validate(); err != nil {
		return OwnedTree{}, err
	}
	return tree, nil
}

// ValidateRootOwned requires the current rooted source to match a previously
// captured exact manifest in both directions.
func ValidateRootOwned(root *os.Root, rel string, expected OwnedTree) error {
	actual, err := SnapshotRootOwned(root, rel)
	if err != nil {
		return err
	}
	return compareOwnedTreeExact(actual, expected)
}

func scanOwnedDirectory(dir *os.File, prefix string, entries *[]OwnedTreeEntry, maxFileBytes int64) error {
	defer keepDescriptorOwnersAlive(dir)
	dirEntries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name() < dirEntries[j].Name() })
	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()
		rel := name
		if prefix != "" {
			rel = path.Join(prefix, name)
		}
		stat, err := statAt(int(dir.Fd()), name)
		if err != nil {
			return err
		}
		mode, kind, err := modeAndKind(&stat)
		if err != nil {
			return fmt.Errorf("inspect transaction-owned entry %q: %w", rel, err)
		}
		entry := OwnedTreeEntry{Path: rel, Kind: kind, Mode: uint32(mode), Identity: identityFromStat(&stat)}
		switch kind {
		case ownedDirectory:
			fd, err := unix.Openat(int(dir.Fd()), name,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			child := os.NewFile(uintptr(fd), rel)
			if child == nil {
				_ = unix.Close(fd)
				return errors.New("invalid transaction-owned directory descriptor")
			}
			var opened unix.Stat_t
			if err := unix.Fstat(fd, &opened); err != nil {
				_ = child.Close()
				return err
			}
			if identityFromStat(&opened) != entry.Identity {
				_ = child.Close()
				return fmt.Errorf("%w: directory %q was replaced while being inspected", ErrOwnedTreeChanged, rel)
			}
			*entries = append(*entries, entry)
			if err := scanOwnedDirectory(child, rel, entries, maxFileBytes); err != nil {
				_ = child.Close()
				return err
			}
			if err := child.Close(); err != nil {
				return err
			}
		case ownedFile:
			if maxFileBytes > 0 && stat.Size > maxFileBytes {
				return fmt.Errorf("file %q size %d exceeds the %d-byte copy limit", rel, stat.Size, maxFileBytes)
			}
			digest, opened, err := hashFileAt(int(dir.Fd()), name, entry.Identity)
			if err != nil {
				return err
			}
			openedMode, _, err := modeAndKind(&opened)
			if err != nil {
				return err
			}
			entry.Mode = uint32(openedMode)
			entry.Digest = digest
			*entries = append(*entries, entry)
		case ownedSymlink:
			target, err := readlinkAt(int(dir.Fd()), name)
			if err != nil {
				return err
			}
			entry.Target = target
			*entries = append(*entries, entry)
		}
	}
	return nil
}

func compareOwnedEntry(actual, expected OwnedTreeEntry) error {
	if actual.Kind != expected.Kind || actual.Mode != expected.Mode || actual.Identity != expected.Identity ||
		actual.Digest != expected.Digest || actual.Target != expected.Target {
		return fmt.Errorf("%w: recovery entry %q no longer matches its recorded identity and content", ErrOwnedTreeChanged, expected.Path)
	}
	return nil
}

func compareOwnedTreeExact(actual, expected OwnedTree) error {
	if actual.RootIdentity != expected.RootIdentity || actual.RootMode != expected.RootMode {
		return fmt.Errorf("%w: transaction-owned root no longer matches its recorded identity and mode", ErrOwnedTreeChanged)
	}
	want := make(map[string]OwnedTreeEntry, len(expected.Entries))
	for _, entry := range expected.Entries {
		want[entry.Path] = entry
	}
	for _, entry := range actual.Entries {
		expectedEntry, ok := want[entry.Path]
		if !ok {
			return fmt.Errorf("%w: transaction-owned tree gained unknown entry %q", ErrOwnedTreeChanged, entry.Path)
		}
		if err := compareOwnedEntry(entry, expectedEntry); err != nil {
			return err
		}
		delete(want, entry.Path)
	}
	if len(want) != 0 {
		missing := make([]string, 0, len(want))
		for name := range want {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf("%w: transaction-owned tree lost recorded entries %q", ErrOwnedTreeChanged, missing)
	}
	return nil
}

func validateOwnedTreeAt(root *checkedRoot, name string, expected OwnedTree) error {
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("invalid transaction-owned tree manifest: %w", err)
	}
	actual, err := snapshotOwnedTree(root, name)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, errUnsupportedOwnedType) {
			return fmt.Errorf("%w: transaction-owned tree %q changed type: %v", ErrOwnedTreeChanged, name, err)
		}
		return err
	}
	return compareOwnedTreeExact(actual, expected)
}

func moveOwnedTreeToRecovery(src *checkedRoot, srcName string, recovery *checkedRoot, payloadName string, expected OwnedTree) error {
	if err := validateOwnedTreeAt(src, srcName, expected); err != nil {
		return err
	}
	if err := renameChecked(src, srcName, recovery, payloadName); err != nil {
		return err
	}
	movedErr := validateOwnedTreeAt(recovery, payloadName, expected)
	if movedErr == nil {
		return nil
	}
	if restoreErr := renameChecked(recovery, payloadName, src, srcName); restoreErr != nil {
		return fmt.Errorf("%w (the moved object is preserved at %s/%s because restoring %s/%s failed: %v)",
			movedErr, recovery.display, payloadName, src.display, srcName, restoreErr)
	}
	return movedErr
}

func (s *Store) SnapshotStagedPayload(name string) (OwnedTree, error) {
	if s.writeRoots == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked write session")
	}
	return snapshotOwnedTree(s.writeRoots.staging, name)
}

// stagedRootPrefix names the transaction-private directory a staging root is
// built under before it takes the name the operation works with. The dot
// prefix keeps it outside the grammar a skill name can have.
const stagedRootPrefix = ".fu-new-"

// StagedRootReservation is the durable ownership proof for a private staging
// directory before it is published at a user-visible skill name.
type StagedRootReservation struct {
	Name     string    `json:"name"`
	Manifest OwnedTree `json:"manifest"`
}

func (r StagedRootReservation) Validate() error {
	if !validLogicalEntry(r.Name) || !strings.HasPrefix(r.Name, stagedRootPrefix) || len(r.Name) == len(stagedRootPrefix) {
		return fmt.Errorf("invalid private staged-root name %q", r.Name)
	}
	if err := r.Manifest.Validate(); err != nil {
		return err
	}
	if len(r.Manifest.Entries) != 0 {
		return fmt.Errorf("private staged-root reservation %q is not empty", r.Name)
	}
	return nil
}

// CreateStagedRootOwned creates one transaction-owned staging root and returns
// the manifest bound to the directory it created.
//
// Creating the root at its final name and then reopening that name to record
// what it holds is not ownership. POSIX has no mkdir that hands back a
// descriptor, so the two steps are joined only by a pathname, and between them
// another same-user writer can replace the directory or drop a file inside it.
// The manifest would then record their work as fu's, and everything downstream
// -- the exclusive scaffold write, the exact validation, the digest, the
// publication, the commit -- would agree, because all of it is checked against
// that manifest.
//
// Building under an unpredictable private name narrows that window; it does
// not abolish it, and the boundary is worth stating exactly. A same-UID
// process can read this directory and learn the name once it exists, so the
// guarantee is not secrecy. What the name buys is that it cannot be
// pre-created or pre-targeted, and that a racer wanting to swap the root must
// win an enumerate-then-swap race inside the few syscalls between the mkdirat
// and the snapshot below. One that did win it could still substitute an empty
// directory of its own -- what it cannot do is contribute content, because the
// manifest is required to be empty. So the residual exposure is the provenance
// of an empty inode, not the adoption of foreign bytes.
//
// The no-replace rename that moves it into place carries the inode along, so
// the identity recorded here is the identity the final name holds, and every
// later step re-checks it. The manifest is required to be empty for the same
// reason it is captured at all: the directory was created two lines above, so
// anything already inside it is somebody else's.
func (s *Store) CreateStagedRootOwned(name string, perm os.FileMode) (OwnedTree, error) {
	if !validPublicLogicalEntry(name) {
		return OwnedTree{}, fmt.Errorf("create staged root requires a public single-component name outside the .fu- namespace: %q", name)
	}
	reservation, err := s.ReserveStagedRootOwned(perm)
	if err != nil {
		return OwnedTree{}, err
	}
	return s.PublishStagedRootOwned(reservation, name)
}

// ReserveStagedRootOwned exclusively creates a private staging directory and
// returns its identity manifest without publishing a final name. Callers must
// persist this reservation before PublishStagedRootOwned.
func (s *Store) ReserveStagedRootOwned(perm os.FileMode) (StagedRootReservation, error) {
	return s.reserveStagedRootOwnedWithHooks(perm, stagedRootReservationHooks{})
}

type stagedRootReservationHooks struct {
	afterMkdir func(string) error
}

func (s *Store) reserveStagedRootOwnedWithHooks(perm os.FileMode, hooks stagedRootReservationHooks) (_ StagedRootReservation, retErr error) {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.staging.dir == nil {
		return StagedRootReservation{}, errors.New("store is not attached to a checked staging root")
	}
	staging := s.writeRoots.staging
	parentFD := int(staging.dir.Fd())
	private, err := privateStagedRootName()
	if err != nil {
		return StagedRootReservation{}, err
	}
	if err := unix.Mkdirat(parentFD, private, uint32(perm.Perm())); err != nil {
		return StagedRootReservation{}, fmt.Errorf("create private staging root %s/%s exclusively: %w", staging.display, private, err)
	}
	observed, err := statAt(parentFD, private)
	if err != nil {
		cleanupErr := unix.Unlinkat(parentFD, private, unix.AT_REMOVEDIR)
		return StagedRootReservation{}, errors.Join(err, cleanupErr)
	}
	mode, kind, err := modeAndKind(&observed)
	if err != nil || kind != ownedDirectory {
		cleanupErr := unix.Unlinkat(parentFD, private, unix.AT_REMOVEDIR)
		return StagedRootReservation{}, errors.Join(err, cleanupErr)
	}
	expectedIdentity := identityFromStat(&observed)
	expectedMode := uint32(mode)
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		retErr = errors.Join(retErr, retireOwnedDirectoryAt(staging.dir, private, ".fu-retired-staging-", expectedIdentity, expectedMode))
	}()
	if hooks.afterMkdir != nil {
		if err := hooks.afterMkdir(private); err != nil {
			return StagedRootReservation{}, err
		}
	}
	tree, err := snapshotOwnedTree(staging, private)
	if err != nil {
		return StagedRootReservation{}, err
	}
	if len(tree.Entries) != 0 {
		return StagedRootReservation{}, fmt.Errorf("%w: private staging root %s/%s was created empty but already holds %d entries",
			ErrOwnedTreeChanged, staging.display, private, len(tree.Entries))
	}
	reservation := StagedRootReservation{Name: private, Manifest: tree}
	if err := reservation.Validate(); err != nil {
		return StagedRootReservation{}, err
	}
	succeeded = true
	return reservation, nil
}

// PublishStagedRootOwned moves a persisted private reservation to its final
// staging name without replacement and post-validates the same inode.
func (s *Store) PublishStagedRootOwned(reservation StagedRootReservation, name string) (OwnedTree, error) {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.staging.dir == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked staging root")
	}
	if err := reservation.Validate(); err != nil {
		return OwnedTree{}, err
	}
	if !validPublicLogicalEntry(name) {
		return OwnedTree{}, fmt.Errorf("publish staged root requires a public single-component name outside the .fu- namespace: %q", name)
	}
	staging := s.writeRoots.staging
	if err := validateOwnedTreeAt(staging, reservation.Name, reservation.Manifest); err != nil {
		return OwnedTree{}, err
	}
	if err := renameNoReplace(int(staging.dir.Fd()), reservation.Name, int(staging.dir.Fd()), name); err != nil {
		return OwnedTree{}, fmt.Errorf("rename %s/%s to unoccupied %s/%s: %w", staging.display, reservation.Name, staging.display, name, err)
	}
	if err := validateOwnedTreeAt(staging, name, reservation.Manifest); err != nil {
		restoreErr := renameNoReplace(int(staging.dir.Fd()), name, int(staging.dir.Fd()), reservation.Name)
		if restoreErr != nil {
			return OwnedTree{}, errors.Join(err, fmt.Errorf("restore mismatched staged reservation: %w", restoreErr))
		}
		return OwnedTree{}, err
	}
	return reservation.Manifest, nil
}

// DeclaredEntry describes one entry a transaction has committed to creating
// but has not created yet.
//
// Every create->record pair has a window where the live tree is a strict
// superset of the manifest, and recovery deliberately demands bidirectional
// exact equality -- that is what makes "fu never deletes what it does not own"
// provable. Without a declaration, fu's own half-written work in that window is
// indistinguishable from foreign interference, so a clean crash blocked every
// later write command. Declaring the exact path, mode and content digest
// *before* creating closes it: recovery can then accept either "declared and
// absent" or "declared and present and matching", and still refuse anything
// else.
//
// The identity cannot be part of the declaration -- the inode does not exist
// yet -- so the proof here is path, mode and content rather than dev/ino. For a
// file fu is about to write, that is what fu actually knows.
//
// Paths may be nested relative paths within the staged root (a copy of an
// arbitrary skill tree declares entries several components deep; plan D8).
// File declarations carry content digests, symlinks carry raw targets, and
// directory declarations carry the mode applied through their opened
// descriptor.
type DeclaredEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind,omitempty"`
	Mode   uint32 `json:"mode"`
	Digest string `json:"digest,omitempty"`
	Target string `json:"target,omitempty"`
}

const (
	declaredFile    = "file"
	declaredSymlink = "symlink"
	declaredDir     = "directory"
)

// NewDeclaredDir describes the exact directory a transaction is about to
// create. Directories carry only a mode: they are implied by the entries
// beneath them, and no manifest records their content.
func NewDeclaredDir(name string, perm os.FileMode) DeclaredEntry {
	return DeclaredEntry{
		Path: filepath.ToSlash(name),
		Kind: declaredDir,
		Mode: uint32(os.ModeDir | perm.Perm()),
	}
}

// NewDeclaredFile describes the exact file a transaction is about to create.
func NewDeclaredFile(name string, perm os.FileMode, data []byte) DeclaredEntry {
	sum := sha256.Sum256(data)
	return DeclaredEntry{
		Path:   filepath.ToSlash(name),
		Kind:   declaredFile,
		Mode:   uint32(perm.Perm()),
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
	}
}

// NewDeclaredSymlink describes the exact symlink a transaction is about to
// create: its raw target text, never resolved.
func NewDeclaredSymlink(name, target string) DeclaredEntry {
	return DeclaredEntry{
		Path:   filepath.ToSlash(name),
		Kind:   declaredSymlink,
		Mode:   uint32(os.ModeSymlink),
		Target: target,
	}
}

// Validate rejects a declaration that could not describe an entry fu
// creates. Paths must be clean relative paths beneath the staged root: no
// leading slash, no "." or ".." component, no empty components. A legacy
// zero-Kind declaration (path, mode, digest only) is accepted as a file so
// recovery stays compatible with records written before the Kind field.
func (d DeclaredEntry) Validate() error {
	if d.Path == "" || d.Path == "." || path.IsAbs(d.Path) ||
		path.Clean(d.Path) != d.Path || d.Path == ".." || strings.HasPrefix(d.Path, "../") {
		return fmt.Errorf("declared transaction entry path %q must be a clean relative path within the staged root", d.Path)
	}
	for _, component := range strings.Split(d.Path, "/") {
		if component == "." || component == ".." || component == "" {
			return fmt.Errorf("declared transaction entry path %q has an unsafe component", d.Path)
		}
	}
	switch d.Kind {
	case "", declaredFile:
		if os.FileMode(d.Mode).Type() != 0 {
			return fmt.Errorf("declared transaction entry %q is not described as a regular file", d.Path)
		}
		if len(d.Digest) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(d.Digest, "sha256:") {
			return fmt.Errorf("declared transaction entry %q has an invalid content digest", d.Path)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(d.Digest, "sha256:")); err != nil {
			return fmt.Errorf("declared transaction entry %q has an invalid content digest: %w", d.Path, err)
		}
		if d.Target != "" {
			return fmt.Errorf("declared transaction entry %q is a file but carries a symlink target", d.Path)
		}
	case declaredSymlink:
		if os.FileMode(d.Mode).Type() != os.ModeSymlink {
			return fmt.Errorf("declared transaction entry %q is not described as a symlink", d.Path)
		}
		if d.Digest != "" {
			return fmt.Errorf("declared transaction entry %q is a symlink but carries a content digest", d.Path)
		}
	case declaredDir:
		if os.FileMode(d.Mode).Type() != os.ModeDir {
			return fmt.Errorf("declared transaction entry %q is not described as a directory", d.Path)
		}
		if d.Digest != "" || d.Target != "" {
			return fmt.Errorf("declared transaction entry %q is a directory but carries content metadata", d.Path)
		}
	default:
		return fmt.Errorf("declared transaction entry %q has unknown kind %q", d.Path, d.Kind)
	}
	return nil
}

// SettleDeclaredStagedEntries turns declarations into manifest entries for the
// ones that were actually created, and refuses anything that does not match
// what was declared. A declared entry that is absent is simply dropped: the
// transaction died before creating it.
func (s *Store) SettleDeclaredStagedEntries(name string, base OwnedTree, declared []DeclaredEntry) (OwnedTree, error) {
	if s.writeRoots == nil || s.writeRoots.staging == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked staging root")
	}
	if len(declared) == 0 {
		return base, nil
	}
	dir, err := openDirFresh(s.writeRoots.staging, name)
	if err != nil {
		return OwnedTree{}, err
	}
	defer dir.Close()
	settled := base
	settled.Entries = append([]OwnedTreeEntry(nil), base.Entries...)
	for _, entry := range declared {
		if err := entry.Validate(); err != nil {
			return OwnedTree{}, err
		}
		if err := withParentFD(dir, entry.Path, true, func(parentFD int, leaf string) error {
			return settleDeclaredAt(s, parentFD, leaf, entry, &settled)
		}); err != nil {
			if errors.Is(err, errDeclaredParentMissing) {
				continue // the parent never existed: drop like an absent leaf
			}
			return OwnedTree{}, err
		}
	}
	sort.Slice(settled.Entries, func(i, j int) bool { return settled.Entries[i].Path < settled.Entries[j].Path })
	if err := settled.Validate(); err != nil {
		return OwnedTree{}, err
	}
	return settled, nil
}

// settleDeclaredAt inspects one declared entry through the parent descriptor
// and, when present and matching, extends the settled manifest with it. An
// absent entry is dropped: the transaction died before creating it.
func settleDeclaredAt(s *Store, parentFD int, leaf string, entry DeclaredEntry, settled *OwnedTree) error {
	observed, statErr := statAt(parentFD, leaf)
	if errors.Is(statErr, unix.ENOENT) {
		return nil
	}
	if statErr != nil {
		return statErr
	}
	switch entry.Kind {
	case "", declaredFile:
		mode, kind, err := modeAndKind(&observed)
		if err != nil {
			return fmt.Errorf("%w: declared entry %q: %v", ErrOwnedTreeChanged, entry.Path, err)
		}
		if kind != ownedFile || uint32(mode) != entry.Mode {
			return fmt.Errorf("%w: declared entry %q is not the file the transaction described", ErrOwnedTreeChanged, entry.Path)
		}
		digest, opened, err := hashFileAt(parentFD, leaf, identityFromStat(&observed))
		if err != nil {
			return err
		}
		if digest != entry.Digest {
			return fmt.Errorf("%w: declared entry %q does not hold the content the transaction described", ErrOwnedTreeChanged, entry.Path)
		}
		openedMode, _, err := modeAndKind(&opened)
		if err != nil {
			return err
		}
		settled.Entries = append(settled.Entries, OwnedTreeEntry{
			Path:     entry.Path,
			Kind:     ownedFile,
			Mode:     uint32(openedMode),
			Identity: identityFromStat(&opened),
			Digest:   digest,
		})
	case declaredSymlink:
		mode, kind, err := modeAndKind(&observed)
		if err != nil {
			return fmt.Errorf("%w: declared entry %q: %v", ErrOwnedTreeChanged, entry.Path, err)
		}
		// Symlink permission bits are meaningless (a link is always 0777 on
		// creation), so the comparison is on the type bits alone.
		if kind != ownedSymlink || mode.Type() != os.ModeSymlink {
			return fmt.Errorf("%w: declared entry %q is not the symlink the transaction described", ErrOwnedTreeChanged, entry.Path)
		}
		target, err := readlinkAt(parentFD, leaf)
		if err != nil {
			return err
		}
		if target != entry.Target {
			return fmt.Errorf("%w: declared entry %q does not hold the target the transaction described", ErrOwnedTreeChanged, entry.Path)
		}
		settled.Entries = append(settled.Entries, OwnedTreeEntry{
			Path:     entry.Path,
			Kind:     ownedSymlink,
			Mode:     uint32(mode),
			Identity: identityFromStat(&observed),
			Target:   target,
		})
	case declaredDir:
		mode, kind, err := modeAndKind(&observed)
		if err != nil {
			return fmt.Errorf("%w: declared entry %q: %v", ErrOwnedTreeChanged, entry.Path, err)
		}
		if kind != ownedDirectory || uint32(mode) != entry.Mode {
			return fmt.Errorf("%w: declared entry %q is not the directory the transaction described", ErrOwnedTreeChanged, entry.Path)
		}
		settled.Entries = append(settled.Entries, OwnedTreeEntry{
			Path:     entry.Path,
			Kind:     ownedDirectory,
			Mode:     uint32(mode),
			Identity: identityFromStat(&observed),
		})
	default:
		return fmt.Errorf("declared transaction entry %q has unknown kind %q", entry.Path, entry.Kind)
	}
	return nil
}

// errDeclaredParentMissing reports that an intermediate component of a
// declared entry's path does not exist. The settle path treats it exactly
// like the absent leaf: the transaction died before creating that part of
// the tree (round 4 finding I1). The copy paths keep treating it as a
// conflict -- there a missing parent means the source changed mid-copy.
var errDeclaredParentMissing = errors.New("declared entry parent is missing")

// withParentFD walks rel's directory components beneath dir with no-follow
// opens and calls fn on the parent directory's descriptor with the leaf
// component. The opened descriptors live for the duration of fn. When
// parentMissingOK is set, an ENOENT on an intermediate component reports
// errDeclaredParentMissing instead of ErrOwnedTreeChanged.
func withParentFD(dir *os.File, rel string, parentMissingOK bool, fn func(parentFD int, leaf string) error) error {
	defer keepDescriptorOwnersAlive(dir)
	parts := strings.Split(rel, "/")
	held := []*os.File{dir}
	parentFD := int(dir.Fd())
	closeHeld := func() {
		for _, f := range held[1:] {
			_ = f.Close()
		}
	}
	for _, component := range parts[:len(parts)-1] {
		fd, err := unix.Openat(parentFD, component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			closeHeld()
			if parentMissingOK && errors.Is(err, unix.ENOENT) {
				return errDeclaredParentMissing
			}
			return fmt.Errorf("%w: declared entry %q: %v", ErrOwnedTreeChanged, rel, err)
		}
		child := os.NewFile(uintptr(fd), component)
		if child == nil {
			_ = unix.Close(fd)
			closeHeld()
			return errors.New("invalid directory descriptor while settling declared entries")
		}
		held = append(held, child)
		parentFD = fd
	}
	defer closeHeld()
	return fn(parentFD, parts[len(parts)-1])
}

func privateStagedRootName() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate private staging root name: %w", err)
	}
	return stagedRootPrefix + hex.EncodeToString(raw[:]), nil
}

// CreateStagedFileOwned exclusively creates one regular file beneath an
// already-authorized staged root and returns the identity and content proof
// for extending that authority. The expected tree is validated before the
// create, and the root descriptor is checked again before any bytes are
// written, so an unexpected descendant or root replacement is never adopted.
func (s *Store) CreateStagedFileOwned(name, rel string, data []byte, perm os.FileMode, expected OwnedTree) (OwnedTreeEntry, error) {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.staging == nil {
		return OwnedTreeEntry{}, errors.New("store is not attached to a checked staging root")
	}
	if !validLogicalEntry(name) || !validLogicalEntry(rel) {
		return OwnedTreeEntry{}, fmt.Errorf("create staged file requires single-component root and file names: %q/%q", name, rel)
	}
	if err := validateOwnedTreeAt(s.writeRoots.staging, name, expected); err != nil {
		return OwnedTreeEntry{}, err
	}
	dir, err := openDirFresh(s.writeRoots.staging, name)
	if err != nil {
		return OwnedTreeEntry{}, err
	}
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &rootStat); err != nil {
		_ = dir.Close()
		return OwnedTreeEntry{}, err
	}
	rootMode, rootKind, err := modeAndKind(&rootStat)
	if err != nil {
		_ = dir.Close()
		return OwnedTreeEntry{}, err
	}
	if rootKind != ownedDirectory || identityFromStat(&rootStat) != expected.RootIdentity || uint32(rootMode) != expected.RootMode {
		_ = dir.Close()
		return OwnedTreeEntry{}, fmt.Errorf("%w: staged root %q changed before file creation", ErrOwnedTreeChanged, name)
	}

	fd, err := unix.Openat(int(dir.Fd()), rel,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		_ = dir.Close()
		return OwnedTreeEntry{}, err
	}
	file := os.NewFile(uintptr(fd), rel)
	if file == nil {
		_ = unix.Close(fd)
		_ = dir.Close()
		return OwnedTreeEntry{}, errors.New("invalid staged-file descriptor")
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Chmod(perm)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	var created unix.Stat_t
	if writeErr == nil {
		writeErr = unix.Fstat(fd, &created)
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = dir.Close()
		return OwnedTreeEntry{}, writeErr
	}
	if closeErr != nil {
		_ = dir.Close()
		return OwnedTreeEntry{}, closeErr
	}
	if err := requireRegularStat(rel, &created); err != nil {
		_ = dir.Close()
		return OwnedTreeEntry{}, err
	}
	identity := identityFromStat(&created)
	digest, opened, err := hashFileAt(int(dir.Fd()), rel, identity)
	if err != nil {
		_ = dir.Close()
		return OwnedTreeEntry{}, err
	}
	if err := dir.Close(); err != nil {
		return OwnedTreeEntry{}, err
	}
	wantSum := sha256.Sum256(data)
	wantDigest := "sha256:" + hex.EncodeToString(wantSum[:])
	if stampRegularFile(&created) != stampRegularFile(&opened) || digest != wantDigest {
		return OwnedTreeEntry{}, fmt.Errorf("%w: staged file %q changed before ownership could be recorded", ErrOwnedTreeChanged, rel)
	}
	mode, kind, err := modeAndKind(&opened)
	if err != nil {
		return OwnedTreeEntry{}, err
	}
	return OwnedTreeEntry{
		Path:     filepath.ToSlash(rel),
		Kind:     kind,
		Mode:     uint32(mode),
		Identity: identityFromStat(&opened),
		Digest:   digest,
	}, nil
}

// ValidateStagedOwned checks that a staged tree contains exactly the entries
// Fu recorded while exclusively creating them.
func (s *Store) ValidateStagedOwned(name string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.staging == nil {
		return errors.New("store is not attached to a checked staging root")
	}
	return validateOwnedTreeAt(s.writeRoots.staging, name, expected)
}

// CopyStagedTreeOwned copies the subtree at srcRel beneath src into the
// already-created staged root name, creating every entry exactly as
// expected declares it, and returns the authoritative manifest of what was
// created.
//
// The copy is the creation half of the declaration protocol (plan D3): the
// caller has already projected the source tree (skill.ProjectDir) and
// written the projection into the transaction record as Declared entries
// before calling this, so a crash at any point leaves a staged tree that is
// a subset of a declared set -- which recovery can settle and quarantine
// instead of refusing. The returned manifest is captured from the live
// staged tree after the last create, so everything downstream (digest,
// publication, recovery) works from one authoritative snapshot.
//
// Source entries are walked with no-follow classification, .git is excluded
// by name at any depth (the digest projection's rule), symlinks are copied
// as symlinks with their raw target text, and special files are refused
// before any open that could block. A file is read through src's checked
// root, which refuses escaping symlinks; the residual in-root replacement
// race (a same-user writer swapping a file between classification and open)
// is the same boundary DigestFS already documents and is caught at the
// content-digest check below. Each created entry must match its declaration
// exactly (mode, digest or target); an undeclared source entry is refused.
func (s *Store) CopyStagedTreeOwned(name string, base OwnedTree, src *os.Root, srcRel string, expected []DeclaredEntry) (OwnedTree, error) {
	if s.writeRoots == nil || s.writeRoots.staging == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked staging root")
	}
	if err := validateOwnedTreeAt(s.writeRoots.staging, name, base); err != nil {
		return OwnedTree{}, err
	}
	return copyTreeOwned(s.writeRoots.staging, name, base, src, srcRel, expected, false)
}

// copyTreeOwned is the shared implementation of the copy primitives: it
// creates every entry of the subtree at srcRel beneath src inside the
// already-created root name of dst, each exactly as expected declares it,
// and returns the authoritative manifest captured from the live destination
// tree after the last create.
//
// Source entries are walked with no-follow classification, .git is excluded
// by name at any depth (the digest projection's rule), symlinks are copied
// as symlinks with their raw target text, and special files are refused
// before any open that could block. A file is read through src's checked
// root, which refuses escaping symlinks; the residual in-root replacement
// race (a same-user writer swapping a file between classification and open)
// is the same boundary DigestFS already documents and is caught at the
// content-digest check below. Each created entry must match its declaration
// exactly (mode, digest or target); an undeclared source entry is refused.
func copyTreeOwned(dst *checkedRoot, name string, base OwnedTree, src *os.Root, srcRel string, expected []DeclaredEntry, includeGit bool) (OwnedTree, error) {
	return copyTreeOwnedWithHooks(dst, name, base, src, srcRel, expected, includeGit, copyTreeHooks{})
}

type copyTreeHooks struct {
	beforeOpen     func() error
	beforeSnapshot func() error
}

func copyTreeOwnedWithHooks(dst *checkedRoot, name string, base OwnedTree, src *os.Root, srcRel string, expected []DeclaredEntry, includeGit bool, hooks copyTreeHooks) (OwnedTree, error) {
	expectedByPath := make(map[string]DeclaredEntry, len(expected))
	for _, e := range expected {
		if err := e.Validate(); err != nil {
			return OwnedTree{}, err
		}
		if _, dup := expectedByPath[e.Path]; dup {
			return OwnedTree{}, fmt.Errorf("copy declarations contain duplicate path %q", e.Path)
		}
		expectedByPath[e.Path] = e
	}
	if hooks.beforeOpen != nil {
		if err := hooks.beforeOpen(); err != nil {
			return OwnedTree{}, err
		}
	}
	dir, err := openDirFresh(dst, name)
	if err != nil {
		return OwnedTree{}, err
	}
	defer dir.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &rootStat); err != nil {
		return OwnedTree{}, err
	}
	rootMode, rootKind, err := modeAndKind(&rootStat)
	if err != nil || rootKind != ownedDirectory || identityFromStat(&rootStat) != base.RootIdentity || uint32(rootMode) != base.RootMode {
		return OwnedTree{}, fmt.Errorf("%w: copy destination root %q changed before creation", ErrOwnedTreeChanged, name)
	}

	err = fsWalkDir(src.FS(), srcRel, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !includeGit && d.Name() == ".git" {
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%s is a .git symlink and cannot be part of a copied skill projection", p)
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := relSlashPath(srcRel, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		declared, ok := expectedByPath[rel]
		if !ok {
			return fmt.Errorf("%w: source entry %q was not declared by the copy projection", ErrOwnedTreeChanged, rel)
		}
		switch {
		case d.IsDir():
			if declared.Kind != declaredDir {
				return fmt.Errorf("source directory %q was declared as %q", rel, declared.Kind)
			}
			return mkdirDeclared(dir, rel, declared)
		case d.Type()&fs.ModeSymlink != 0:
			if declared.Kind != declaredSymlink {
				return fmt.Errorf("source symlink %q was not declared as a symlink", rel)
			}
			target, err := fs.ReadLink(src.FS(), p)
			if err != nil {
				return err
			}
			if target != declared.Target {
				return fmt.Errorf("source symlink %q target %q does not match its declaration %q", rel, target, declared.Target)
			}
			return createDeclaredSymlink(dir, rel, declared)
		default:
			if d.Type()&^fs.ModeSymlink&^fs.ModeDir != 0 {
				return fmt.Errorf("source entry %q has unsupported mode %v", rel, d.Type())
			}
			if declared.Kind != declaredFile && declared.Kind != "" {
				return fmt.Errorf("source file %q was declared as %q", rel, declared.Kind)
			}
			return copyDeclaredFile(src, p, dir, rel, declared)
		}
	})
	if err != nil {
		return OwnedTree{}, err
	}
	if hooks.beforeSnapshot != nil {
		if err := hooks.beforeSnapshot(); err != nil {
			return OwnedTree{}, err
		}
	}
	manifest, err := snapshotOwnedOpenDirectory(dir)
	if err != nil {
		return OwnedTree{}, fmt.Errorf("snapshot copied tree: %w", err)
	}
	if err := validateOwnedTreeAt(dst, name, manifest); err != nil {
		return OwnedTree{}, fmt.Errorf("%w: copied destination root %q changed identity: %v", ErrOwnedTreeChanged, name, err)
	}
	// Bidirectional verification against the declaration set (finding I4).
	// The manifest alone cannot vouch for the copy: it is captured from the
	// very tree it describes, so a same-user stray landing in the staged
	// root between the last create and this snapshot would otherwise pass
	// every downstream check against itself and be published as fu's
	// content. And a declared entry the walk never visited -- a source file
	// deleted between the projection and the copy -- would silently vanish
	// from the skill. Both must be refused here.
	present := make(map[string]bool, len(expectedByPath))
	for _, entry := range manifest.Entries {
		present[entry.Path] = true
		declared, ok := expectedByPath[entry.Path]
		if !ok {
			return OwnedTree{}, fmt.Errorf("%w: copy produced undeclared entry %q", ErrOwnedTreeChanged, entry.Path)
		}
		if err := compareCopiedEntryToDeclaration(entry, declared); err != nil {
			return OwnedTree{}, err
		}
	}
	for path := range expectedByPath {
		if !present[path] {
			return OwnedTree{}, fmt.Errorf("%w: declared source entry %q is missing from the copy", ErrOwnedTreeChanged, path)
		}
	}
	return manifest, nil
}

func compareCopiedEntryToDeclaration(actual OwnedTreeEntry, declared DeclaredEntry) error {
	wantKind := declared.Kind
	if wantKind == "" {
		wantKind = declaredFile
	}
	if actual.Kind != wantKind {
		return fmt.Errorf("%w: copied entry %q has kind %q, declared %q", ErrOwnedTreeChanged, actual.Path, actual.Kind, wantKind)
	}
	if wantKind == declaredSymlink {
		if os.FileMode(actual.Mode).Type() != os.ModeSymlink || actual.Target != declared.Target {
			return fmt.Errorf("%w: copied symlink %q no longer matches its declaration", ErrOwnedTreeChanged, actual.Path)
		}
		return nil
	}
	if actual.Mode != declared.Mode || actual.Digest != declared.Digest || actual.Target != declared.Target {
		return fmt.Errorf("%w: copied entry %q no longer matches its declared mode and content", ErrOwnedTreeChanged, actual.Path)
	}
	return nil
}

// fsWalkDir is fs.WalkDir with an os.Root-agnostic signature so the copy
// walks through the source's own checked root.
func fsWalkDir(fsys fs.FS, dir string, fn fs.WalkDirFunc) error {
	return fs.WalkDir(fsys, dir, fn)
}

// relSlashPath computes rel's slash-separated relative path beneath root,
// handling the "." root the way fs.WalkDir reports it.
func relSlashPath(root, p string) (string, error) {
	if root == "." {
		return path.Clean(p), nil
	}
	if p == root {
		return ".", nil
	}
	prefix := root + "/"
	if len(p) > len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):], nil
	}
	return "", fmt.Errorf("path %q is not beneath %q", p, root)
}

// mkdirDeclared creates one directory component beneath the staged root.
// Each projected directory is visited once, so any existing entry is foreign
// state and must be rejected. The mode is applied through a no-follow
// directory descriptor because path-based chmod could follow a raced symlink.
func mkdirDeclared(dir *os.File, rel string, declared DeclaredEntry) error {
	return withParentFD(dir, rel, false, func(parentFD int, leaf string) error {
		if err := unix.Mkdirat(parentFD, leaf, uint32(os.FileMode(declared.Mode).Perm())); err != nil {
			return fmt.Errorf("create staged directory %q: %w", rel, err)
		}
		var pathStat unix.Stat_t
		if err := unix.Fstatat(parentFD, leaf, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("inspect staged directory %q after creation: %w", rel, err)
		}
		fd, err := unix.Openat(parentFD, leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open staged directory %q after creation: %w", rel, err)
		}
		defer unix.Close(fd)
		var openedStat unix.Stat_t
		if err := unix.Fstat(fd, &openedStat); err != nil {
			return fmt.Errorf("inspect opened staged directory %q: %w", rel, err)
		}
		if identityFromStat(&pathStat) != identityFromStat(&openedStat) || openedStat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("staged directory %q was replaced after creation", rel)
		}
		if err := unix.Fchmod(fd, uint32(os.FileMode(declared.Mode).Perm())); err != nil {
			return fmt.Errorf("set staged directory mode for %q: %w", rel, err)
		}
		if err := unix.Fstatat(parentFD, leaf, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("reinspect staged directory %q after chmod: %w", rel, err)
		}
		if identityFromStat(&pathStat) != identityFromStat(&openedStat) {
			return fmt.Errorf("staged directory %q was replaced while applying its mode", rel)
		}
		return nil
	})
}

// createDeclaredSymlink creates a symlink whose raw target text is exactly
// the declared one.
func createDeclaredSymlink(dir *os.File, rel string, declared DeclaredEntry) error {
	return withParentFD(dir, rel, false, func(parentFD int, leaf string) error {
		if err := unix.Symlinkat(declared.Target, parentFD, leaf); err != nil {
			return fmt.Errorf("create staged symlink %q: %w", rel, err)
		}
		return nil
	})
}

// copyDeclaredFile streams a source file into the staged tree, verifies the
// written content against the declaration, and returns nothing: the
// authoritative manifest is captured by the final snapshot. The source is
// opened through the checked source root with no-follow type re-verification
// after open, so a type swap between classification and open cannot turn
// the copy into a symlink-following read.
func copyDeclaredFile(src *os.Root, srcPath string, dir *os.File, rel string, declared DeclaredEntry) error {
	content, err := readSourceFile(src, srcPath)
	if err != nil {
		return fmt.Errorf("read source file %q: %w", srcPath, err)
	}
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if digest != declared.Digest {
		return fmt.Errorf("source file %q does not match its declared content digest", rel)
	}
	return withParentFD(dir, rel, false, func(parentFD int, leaf string) error {
		fd, err := unix.Openat(parentFD, leaf,
			unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			uint32(os.FileMode(declared.Mode).Perm()))
		if err != nil {
			return fmt.Errorf("create staged file %q: %w", rel, err)
		}
		file := os.NewFile(uintptr(fd), leaf)
		if file == nil {
			_ = unix.Close(fd)
			return errors.New("invalid staged-file descriptor while copying")
		}
		written, writeErr := file.Write(content)
		if writeErr == nil && written != len(content) {
			writeErr = io.ErrShortWrite
		}
		if writeErr == nil {
			writeErr = file.Chmod(os.FileMode(declared.Mode).Perm())
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("write staged file %q: %w", rel, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close staged file %q: %w", rel, closeErr)
		}
		return nil
	})
}

// readSourceFile reads one source file through the checked root with a size
// cap and post-open type verification.
func readSourceFile(src *os.Root, path string) ([]byte, error) {
	f, err := src.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > skill.MaxSourceFileBytes {
		_ = f.Close()
		return nil, fmt.Errorf("%s size %d exceeds the %d-byte copy limit", path, info.Size(), skill.MaxSourceFileBytes)
	}
	content, readErr := io.ReadAll(io.LimitReader(f, skill.MaxSourceFileBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(content)) > skill.MaxSourceFileBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte copy limit while being read", path, skill.MaxSourceFileBytes)
	}
	return content, nil
}

// SnapshotSkillPayload captures the authoritative manifest of a live skill
// directory in the skills root (the rm-side counterpart of
// SnapshotStagedPayload).
func (s *Store) SnapshotSkillPayload(name string) (OwnedTree, error) {
	if s.writeRoots == nil || s.writeRoots.skills == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked skills root")
	}
	return snapshotOwnedTree(s.writeRoots.skills, name)
}

// RestoreRecoveryPayloadToSkills moves a quarantined payload back into the
// skills root under name, validating on both sides of the move (the
// uncommitted-rm rollback path).
func (s *Store) RestoreRecoveryPayloadToSkills(payload, name string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.recovery == nil || s.writeRoots.skills == nil {
		return errors.New("store is not attached to checked recovery and skills roots")
	}
	if !validPublicLogicalEntry(name) {
		return fmt.Errorf("restore recovery payload requires a public destination name outside the .fu- namespace: %q", name)
	}
	return moveOwnedTreeToRecovery(s.writeRoots.recovery, payload, s.writeRoots.skills, name, expected)
}

// CreateRecoveryRootOwned creates one exclusive directory under the recovery
// root (adopt's agent-original archive destination) and returns the manifest
// bound to the directory it created. Unlike the staged variant there is no
// private-name hop: the payload name is caller-chosen with a random suffix,
// so a collision means another fu process's content is already there and
// must be left alone (Mkdirat refuses an existing name).
func (s *Store) CreateRecoveryRootOwned(payload string, perm os.FileMode) (OwnedTree, error) {
	return s.createRecoveryRootOwnedWithHooks(payload, perm, createRecoveryRootHooks{})
}

type createRecoveryRootHooks struct {
	afterOpen func() error
}

func (s *Store) createRecoveryRootOwnedWithHooks(payload string, perm os.FileMode, hooks createRecoveryRootHooks) (OwnedTree, error) {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.recovery == nil || s.writeRoots.recovery.dir == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked recovery root")
	}
	if !validPublicLogicalEntry(payload) {
		return OwnedTree{}, fmt.Errorf("create recovery root requires a public single-component name outside the .fu- namespace: %q", payload)
	}
	recovery := s.writeRoots.recovery
	parentFD := int(recovery.dir.Fd())
	if err := unix.Mkdirat(parentFD, payload, uint32(perm.Perm())); err != nil {
		return OwnedTree{}, fmt.Errorf("create recovery root %s/%s exclusively: %w", recovery.display, payload, err)
	}
	dir, err := openDirFresh(recovery, payload)
	if err != nil {
		return OwnedTree{}, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &opened); err != nil {
		_ = dir.Close()
		return OwnedTree{}, err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(parentFD, payload, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		identityFromStat(&named) != identityFromStat(&opened) || opened.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = dir.Close()
		return OwnedTree{}, fmt.Errorf("%w: recovery root %s/%s changed while being opened", ErrOwnedTreeChanged, recovery.display, payload)
	}
	if hooks.afterOpen != nil {
		if err := hooks.afterOpen(); err != nil {
			_ = dir.Close()
			return OwnedTree{}, err
		}
	}
	if err := unix.Fchmod(int(dir.Fd()), uint32(perm.Perm())); err != nil {
		_ = dir.Close()
		return OwnedTree{}, fmt.Errorf("set recovery root mode %s/%s: %w", recovery.display, payload, err)
	}
	if err := unix.Fstatat(parentFD, payload, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil || identityFromStat(&named) != identityFromStat(&opened) {
		_ = dir.Close()
		return OwnedTree{}, fmt.Errorf("%w: recovery root %s/%s was replaced while applying its mode", ErrOwnedTreeChanged, recovery.display, payload)
	}
	tree, err := snapshotOwnedOpenDirectory(dir)
	if err != nil {
		_ = dir.Close()
		return OwnedTree{}, err
	}
	if err := validateOwnedTreeAt(recovery, payload, tree); err != nil {
		_ = dir.Close()
		return OwnedTree{}, err
	}
	if err := dir.Close(); err != nil {
		return OwnedTree{}, err
	}
	if len(tree.Entries) != 0 {
		return OwnedTree{}, fmt.Errorf("%w: recovery root %s/%s was created empty but already holds %d entries",
			ErrOwnedTreeChanged, recovery.display, payload, len(tree.Entries))
	}
	return tree, nil
}

// CopyTreeToRecoveryExactOwned copies every entry from an exact rooted-source
// manifest into recovery. It includes .git and revalidates the pinned source
// after the copy, so the archive is a faithful recovery image rather than the
// normalized installation projection.
//
// "Exact" is bounded, and the boundary is stated here because nothing fails
// when it is crossed: the returned manifest is captured from what was actually
// created, so it is self-consistent by construction and a dropped attribute
// leaves no trace (round 18 finding M6). What is preserved: tree shape, entry
// kind, permission bits, file content and symlink targets. What is not:
// setuid/setgid/sticky bits (mkdirDeclared and copyDeclaredFile apply
// .Perm() only), mtime, ownership, and hard-link identity -- two names for one
// inode come back as two independent files.
func (s *Store) CopyTreeToRecoveryExactOwned(payload string, base OwnedTree, src *os.Root, srcRel string, source OwnedTree) (OwnedTree, error) {
	if s.writeRoots == nil || s.writeRoots.recovery == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked recovery root")
	}
	if err := validateOwnedTreeAt(s.writeRoots.recovery, payload, base); err != nil {
		return OwnedTree{}, err
	}
	if err := ValidateRootOwned(src, srcRel, source); err != nil {
		return OwnedTree{}, fmt.Errorf("validate exact archive source: %w", err)
	}
	declared := declaredFromOwnedTree(source)
	manifest, err := copyTreeOwned(s.writeRoots.recovery, payload, base, src, srcRel, declared, true)
	if err != nil {
		return OwnedTree{}, err
	}
	if err := ValidateRootOwned(src, srcRel, source); err != nil {
		return OwnedTree{}, fmt.Errorf("revalidate exact archive source: %w", err)
	}
	return manifest, nil
}

func declaredFromOwnedTree(tree OwnedTree) []DeclaredEntry {
	declared := make([]DeclaredEntry, 0, len(tree.Entries))
	for _, entry := range tree.Entries {
		item := DeclaredEntry{Path: entry.Path}
		switch entry.Kind {
		case ownedDirectory:
			item.Kind = declaredDir
			item.Mode = uint32(os.ModeDir | os.FileMode(entry.Mode).Perm())
		case ownedFile:
			item.Kind = declaredFile
			item.Mode = uint32(os.FileMode(entry.Mode).Perm())
			item.Digest = entry.Digest
		case ownedSymlink:
			item.Kind = declaredSymlink
			item.Mode = uint32(os.ModeSymlink)
			item.Target = entry.Target
		}
		declared = append(declared, item)
	}
	return declared
}

// PublishStagedOwned moves a staged tree into the live skills root only while
// its exact authoritative manifest remains stable. A post-rename mismatch is
// restored to staging instead of being accepted as transaction content.
func (s *Store) PublishStagedOwned(name string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.skills == nil {
		return errors.New("store is not attached to checked staging and skills roots")
	}
	if !validPublicLogicalEntry(name) {
		return fmt.Errorf("publish staged tree requires a public single-component name outside the .fu- namespace: %q", name)
	}
	return moveOwnedTreeToRecovery(s.writeRoots.staging, name, s.writeRoots.skills, name, expected)
}

// QuarantineStagedOwned moves a staged tree only when its complete ownership
// manifest still matches. The destination move is exclusive, and the moved
// object is validated again so a source replacement racing the first check is
// restored rather than deleted as transaction content.
func (s *Store) QuarantineStagedOwned(name, payload string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.recovery == nil {
		return errors.New("store is not attached to checked staging and recovery roots")
	}
	if !validPublicLogicalEntry(name) {
		return fmt.Errorf("quarantine staged tree requires a public source name outside the .fu- namespace: %q", name)
	}
	if !validPublicLogicalEntry(payload) {
		return fmt.Errorf("quarantine staged tree requires a public recovery payload name outside the .fu- namespace: %q", payload)
	}
	return moveOwnedTreeToRecovery(s.writeRoots.staging, name, s.writeRoots.recovery, payload, expected)
}

// QuarantineSkillOwned is the live-store counterpart of
// QuarantineStagedOwned.
func (s *Store) QuarantineSkillOwned(name, payload string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.skills == nil || s.writeRoots.recovery == nil {
		return errors.New("store is not attached to checked skills and recovery roots")
	}
	if !validPublicLogicalEntry(name) {
		return fmt.Errorf("quarantine skill requires a public single-component name outside the .fu- namespace: %q", name)
	}
	if !validPublicLogicalEntry(payload) {
		return fmt.Errorf("quarantine skill requires a public recovery payload name outside the .fu- namespace: %q", payload)
	}
	return moveOwnedTreeToRecovery(s.writeRoots.skills, name, s.writeRoots.recovery, payload, expected)
}

// ValidateSkillOwned checks a live skill against the transaction manifest
// through the pinned skills descriptor without mutating either namespace.
func (s *Store) ValidateSkillOwned(name string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.skills == nil {
		return errors.New("store is not attached to a checked skills root")
	}
	return validateOwnedTreeAt(s.writeRoots.skills, name, expected)
}

// ValidateRecoveryPayloadOwned checks an already quarantined payload against
// the same identities and content recorded while it was staged.
func (s *Store) ValidateRecoveryPayloadOwned(name string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.recovery == nil {
		return errors.New("store is not attached to a checked recovery root")
	}
	return validateOwnedTreeAt(s.writeRoots.recovery, name, expected)
}

// ArchiveRecoveryPayloadOwned retires a validated transaction payload under a
// deterministic machine-local archive name. It deliberately never unlinks the
// payload, so a rolled-back new/add install keeps the quarantined content for
// the user. That retention is a scope decision, not a limit of the primitives:
// POSIX has indeed no portable identity-conditioned unlink, but safe deletion
// does not need one, as ReclaimRecoveryPayloadOwned below does by retiring the
// name and revalidating the moved object before it unlinks.
func (s *Store) ArchiveRecoveryPayloadOwned(name string, expected OwnedTree) error {
	return archiveRecoveryPayloadOwned(s, name, expected, ownedCleanupHooks{})
}

// ReclaimRecoveryPayloadOwned disposes of a transaction payload whose owning
// transaction has already reached its terminal marker. It is the disposal
// counterpart of ArchiveRecoveryPayloadOwned: the manifest binds every entry,
// each leaf is retired to its deterministic sibling and revalidated before it
// is unlinked, and an interrupted removal resumes when the same manifest is
// replayed. An already absent payload is not an error -- the caller may be a
// replay of a removal that already finished.
func (s *Store) ReclaimRecoveryPayloadOwned(name string, expected OwnedTree) error {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.recovery == nil || s.writeRoots.recovery.dir == nil {
		return errors.New("store is not attached to a checked recovery-root session")
	}
	if !validPublicLogicalEntry(name) {
		return fmt.Errorf("reclaim recovery payload requires a public single-component name outside the .fu- namespace: %q", name)
	}
	return RemoveOwnedTreeAt(s.writeRoots.recovery.dir, name, expected)
}

type ownedCleanupHooks struct {
	beforeEntryRemoval      func(OwnedTreeEntry)
	beforeRootRemoval       func()
	beforeEntryFinalization func(OwnedTreeEntry)
	beforeRootFinalization  func()
}

const ownedArchivePrefix = ".fu-archive-"

func ownedCleanupToken(kind, name string, identity FileIdentity) string {
	h := sha256.New()
	_, _ = io.WriteString(h, kind)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, name)
	_, _ = io.WriteString(h, fmt.Sprintf("\x00%d\x00%d", identity.Device, identity.Inode))
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func ownedArchiveName(name string, expected OwnedTree) string {
	return ownedArchivePrefix + ownedCleanupToken("archive", name, expected.RootIdentity)
}

func pathPresentAt(parentFD int, name string) (bool, error) {
	_, err := statAt(parentFD, name)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func archiveRecoveryPayloadOwned(s *Store, name string, expected OwnedTree, hooks ownedCleanupHooks) error {
	defer keepDescriptorOwnersAlive(s)
	if s.writeRoots == nil || s.writeRoots.recovery == nil || s.writeRoots.recovery.dir == nil {
		return errors.New("store is not attached to a checked recovery-root session")
	}
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("invalid transaction-owned tree manifest: %w", err)
	}
	parentFD := int(s.writeRoots.recovery.dir.Fd())
	archive := ownedArchiveName(name, expected)
	candidates := []string{name, archive}
	active := ""
	for _, candidate := range candidates {
		present, err := pathPresentAt(parentFD, candidate)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if active != "" {
			return fmt.Errorf("%w: recovery payload exists at both %q and %q", ErrOwnedTreeChanged, active, candidate)
		}
		active = candidate
	}
	if active == "" {
		return fmt.Errorf("%w: recovery payload %q is absent from both its original and archive names", ErrOwnedTreeChanged, name)
	}
	// Exact set equality, in both directions and at both boundaries. Cleanup
	// is one rename, so there is no half-cleaned payload for a looser rule to
	// describe: a recorded entry that is missing means the payload was changed
	// under fu, and the archive would otherwise claim to have retained content
	// that is gone.
	validateAt := func(candidate string) error {
		actual, err := snapshotOwnedTree(s.writeRoots.recovery, candidate)
		if err != nil {
			return fmt.Errorf("%w: inspect recovery payload %q: %v", ErrOwnedTreeChanged, candidate, err)
		}
		return compareOwnedTreeExact(actual, expected)
	}
	if err := validateAt(active); err != nil {
		return err
	}
	if active == archive {
		return nil
	}
	for _, entry := range expected.Entries {
		if hooks.beforeEntryRemoval != nil {
			hooks.beforeEntryRemoval(entry)
		}
		if hooks.beforeEntryFinalization != nil {
			hooks.beforeEntryFinalization(entry)
		}
	}
	if hooks.beforeRootRemoval != nil {
		hooks.beforeRootRemoval()
	}
	if hooks.beforeRootFinalization != nil {
		hooks.beforeRootFinalization()
	}
	if err := renameNoReplace(parentFD, active, parentFD, archive); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: archive recovery payload %q as %q: %v", ErrOwnedTreeChanged, active, archive, err)
		}
		return err
	}
	if err := validateAt(archive); err != nil {
		if restoreErr := renameNoReplace(parentFD, archive, parentFD, active); restoreErr != nil {
			return fmt.Errorf("%w (the mismatching payload is preserved at archive name %q because restoring %q failed: %v)", err, archive, active, restoreErr)
		}
		return err
	}
	return nil
}
