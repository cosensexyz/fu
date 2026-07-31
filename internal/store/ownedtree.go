package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
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
	if err := scanOwnedDirectory(file, "", &tree.Entries); err != nil {
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

func scanOwnedDirectory(dir *os.File, prefix string, entries *[]OwnedTreeEntry) error {
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
			if err := scanOwnedDirectory(child, rel, entries); err != nil {
				_ = child.Close()
				return err
			}
			if err := child.Close(); err != nil {
				return err
			}
		case ownedFile:
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
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.staging.dir == nil {
		return OwnedTree{}, errors.New("store is not attached to a checked staging root")
	}
	if !validLogicalEntry(name) {
		return OwnedTree{}, fmt.Errorf("create staged root requires a single-component name: %q", name)
	}
	staging := s.writeRoots.staging
	parentFD := int(staging.dir.Fd())
	if err := reclaimAbandonedStagedRoots(staging); err != nil {
		return OwnedTree{}, err
	}
	private, err := privateStagedRootName()
	if err != nil {
		return OwnedTree{}, err
	}
	if err := unix.Mkdirat(parentFD, private, uint32(perm.Perm())); err != nil {
		return OwnedTree{}, fmt.Errorf("create private staging root %s/%s exclusively: %w", staging.display, private, err)
	}
	// Only ever removes a directory that is still empty, which is the one
	// state proving nothing was adopted into it -- rmdir refuses the rest.
	discard := func() {
		_ = unix.Unlinkat(parentFD, private, unix.AT_REMOVEDIR)
	}
	tree, err := snapshotOwnedTree(staging, private)
	if err != nil {
		discard()
		return OwnedTree{}, err
	}
	if len(tree.Entries) != 0 {
		return OwnedTree{}, fmt.Errorf("%w: private staging root %s/%s was created empty but already holds %d entries",
			ErrOwnedTreeChanged, staging.display, private, len(tree.Entries))
	}
	if err := renameNoReplace(parentFD, private, parentFD, name); err != nil {
		discard()
		return OwnedTree{}, fmt.Errorf("rename %s/%s to unoccupied %s/%s: %w", staging.display, private, staging.display, name, err)
	}
	if err := validateOwnedTreeAt(staging, name, tree); err != nil {
		return OwnedTree{}, err
	}
	return tree, nil
}

// reclaimAbandonedStagedRoots clears private staging roots left behind by a
// process that died between creating one and renaming it into place. Such a
// directory is mentioned by no transaction record -- the WAL is still at
// "started" with no payload, and recovery only inspects skills/<name> and
// staging/<name> -- so nothing else would ever report or remove it.
//
// Reclamation is rmdir and nothing else. That can only succeed on a directory
// that is still empty, which is the one state proving nothing was written into
// it, so this can never destroy content fu cannot account for. The write lock
// is held here, so an empty private root is abandoned by definition rather
// than in flight.
// DeclaredEntry is a regular file a transaction has committed to creating but
// has not created yet.
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
type DeclaredEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Digest string `json:"digest"`
}

// NewDeclaredFile describes the exact file a transaction is about to create.
func NewDeclaredFile(name string, perm os.FileMode, data []byte) DeclaredEntry {
	sum := sha256.Sum256(data)
	return DeclaredEntry{
		Path:   filepath.ToSlash(name),
		Mode:   uint32(perm.Perm()),
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
	}
}

// Validate rejects a declaration that could not describe a file fu creates.
// Only single-component names are accepted: the settle below addresses the
// entry relative to the staged root's descriptor, and nothing in this plan
// declares anything deeper.
func (d DeclaredEntry) Validate() error {
	if !validLogicalEntry(d.Path) {
		return fmt.Errorf("declared transaction entry %q must be a single-component name", d.Path)
	}
	if os.FileMode(d.Mode).Type() != 0 {
		return fmt.Errorf("declared transaction entry %q is not described as a regular file", d.Path)
	}
	if len(d.Digest) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(d.Digest, "sha256:") {
		return fmt.Errorf("declared transaction entry %q has an invalid content digest", d.Path)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(d.Digest, "sha256:")); err != nil {
		return fmt.Errorf("declared transaction entry %q has an invalid content digest: %w", d.Path, err)
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
		observed, statErr := statAt(int(dir.Fd()), entry.Path)
		if errors.Is(statErr, unix.ENOENT) {
			continue
		}
		if statErr != nil {
			return OwnedTree{}, statErr
		}
		mode, kind, err := modeAndKind(&observed)
		if err != nil {
			return OwnedTree{}, fmt.Errorf("%w: declared entry %q: %v", ErrOwnedTreeChanged, entry.Path, err)
		}
		if kind != ownedFile || uint32(mode) != entry.Mode {
			return OwnedTree{}, fmt.Errorf("%w: declared entry %q is not the file the transaction described", ErrOwnedTreeChanged, entry.Path)
		}
		digest, opened, err := hashFileAt(int(dir.Fd()), entry.Path, identityFromStat(&observed))
		if err != nil {
			return OwnedTree{}, err
		}
		if digest != entry.Digest {
			return OwnedTree{}, fmt.Errorf("%w: declared entry %q does not hold the content the transaction described", ErrOwnedTreeChanged, entry.Path)
		}
		openedMode, _, err := modeAndKind(&opened)
		if err != nil {
			return OwnedTree{}, err
		}
		settled.Entries = append(settled.Entries, OwnedTreeEntry{
			Path:     entry.Path,
			Kind:     ownedFile,
			Mode:     uint32(openedMode),
			Identity: identityFromStat(&opened),
			Digest:   digest,
		})
	}
	sort.Slice(settled.Entries, func(i, j int) bool { return settled.Entries[i].Path < settled.Entries[j].Path })
	if err := settled.Validate(); err != nil {
		return OwnedTree{}, err
	}
	return settled, nil
}

// ReclaimEmptyStagedRoot removes a staged root only while it is still empty,
// and reports whether it did.
//
// This is the residue a crash between the exclusive create and the WriteTxn
// that records it leaves behind: the manifest does not exist yet, so recovery
// cannot prove ownership the usual way, and its bidirectional exact equality
// then reads fu's own half-written work as foreign interference -- blocking
// every write command, not just the one that died. rmdir is the proof that is
// available instead: it can only succeed on a directory that is still empty,
// which is the one state showing nothing was ever written into it. Anything
// with content in it returns ENOTEMPTY and is preserved and reported, exactly
// as before.
func (s *Store) ReclaimEmptyStagedRoot(name string) (bool, error) {
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.staging.dir == nil {
		return false, errors.New("store is not attached to a checked staging root")
	}
	if !validLogicalEntry(name) {
		return false, fmt.Errorf("reclaim staged root requires a single-component name: %q", name)
	}
	err := unix.Unlinkat(int(s.writeRoots.staging.dir.Fd()), name, unix.AT_REMOVEDIR)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return true, nil
	case errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTDIR):
		return false, nil
	default:
		return false, fmt.Errorf("reclaim empty staged root %s/%s: %w", s.writeRoots.staging.display, name, err)
	}
}

func reclaimAbandonedStagedRoots(staging *checkedRoot) error {
	dir, err := openDirFresh(staging, ".")
	if err != nil {
		return err
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		_ = dir.Close()
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, stagedRootPrefix) {
			continue
		}
		// ENOTEMPTY and ENOTDIR are the ordinary answers for something fu must
		// leave alone, so a failure here is never escalated.
		_ = unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR)
	}
	return dir.Close()
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

// PublishStagedOwned moves a staged tree into the live skills root only while
// its exact authoritative manifest remains stable. A post-rename mismatch is
// restored to staging instead of being accepted as transaction content.
func (s *Store) PublishStagedOwned(name string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.skills == nil {
		return errors.New("store is not attached to checked staging and skills roots")
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
	return moveOwnedTreeToRecovery(s.writeRoots.staging, name, s.writeRoots.recovery, payload, expected)
}

// QuarantineSkillOwned is the live-store counterpart of
// QuarantineStagedOwned.
func (s *Store) QuarantineSkillOwned(name, payload string, expected OwnedTree) error {
	if s.writeRoots == nil || s.writeRoots.skills == nil || s.writeRoots.recovery == nil {
		return errors.New("store is not attached to checked skills and recovery roots")
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
// payload: POSIX has no portable identity-conditioned unlink, so retaining the
// validated object is the only way to guarantee that a replacement racing the
// final namespace operation is not deleted.
func (s *Store) ArchiveRecoveryPayloadOwned(name string, expected OwnedTree) error {
	return archiveRecoveryPayloadOwned(s, name, expected, ownedCleanupHooks{})
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
