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

	"golang.org/x/sys/unix"
)

// RetireNameAt atomically moves one descriptor-relative name to a fresh,
// unpredictable sibling without replacing anything already there. Callers
// must post-validate the moved object before treating it as owned.
func RetireNameAt(parent *os.File, name, prefix string) (string, error) {
	if parent == nil {
		return "", errors.New("retire entry: parent descriptor is nil")
	}
	if !validLogicalEntry(name) {
		return "", fmt.Errorf("retire entry: invalid name %q", name)
	}
	for range 100 {
		retired, err := randomRetiredName(prefix)
		if err != nil {
			return "", err
		}
		err = RenameNoReplaceAt(parent, name, parent, retired)
		if errors.Is(err, os.ErrExist) || errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		return retired, nil
	}
	return "", errors.New("retire entry: exhausted unique quarantine names")
}

// RestoreRetiredAt returns a retired entry to its original name without
// replacing a newer occupant.
func RestoreRetiredAt(parent *os.File, retired, original string) error {
	if parent == nil {
		return errors.New("restore retired entry: parent descriptor is nil")
	}
	if !validLogicalEntry(retired) || !validLogicalEntry(original) {
		return fmt.Errorf("restore retired entry: invalid names %q and %q", retired, original)
	}
	return RenameNoReplaceAt(parent, retired, parent, original)
}

func randomRetiredName(prefix string) (string, error) {
	if prefix == "" || !validLogicalEntry(prefix+"x") {
		return "", fmt.Errorf("invalid retirement prefix %q", prefix)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate retirement name: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// NewRetiredName reserves no filesystem object; it returns an unpredictable
// single-component name suitable for recording in a WAL before a no-replace
// retirement rename.
func NewRetiredName(prefix string) (string, error) {
	return randomRetiredName(prefix)
}

func retireOwnedLeafAt(parent *os.File, name, prefix string, expected FileIdentity, expectedType uint32) error {
	return retireOwnedLeafAtWithCapture(parent, name, prefix, expected, expectedType, entryIdentityAt)
}

func retireOwnedLeafAtWithCapture(
	parent *os.File,
	name, prefix string,
	expected FileIdentity,
	expectedType uint32,
	capture ownedLeafIdentityCapture,
) error {
	defer keepDescriptorOwnersAlive(parent)
	observed, stat, err := capture(int(parent.Fd()), name)
	if err != nil {
		return atomicIdentityMismatch(fmt.Sprintf("inspect owned leaf %q before retirement", name), err)
	}
	if !observed.Same(expected) || uint32(stat.Mode)&uint32(unix.S_IFMT) != expectedType {
		return fmt.Errorf("%w: entry %q did not match its recorded identity and type before retirement", ErrOwnedTreeChanged, name)
	}
	return retireObservedLeafAt(parent, name, prefix, observed, expectedType)
}

func retireObservedLeafAt(parent *os.File, name, prefix string, observed FileIdentity, expectedType uint32) error {
	defer keepDescriptorOwnersAlive(parent)
	retired, err := RetireNameAt(parent, name, prefix)
	if err != nil {
		return err
	}
	identity, stat, statErr := entryIdentityAt(int(parent.Fd()), retired)
	if statErr != nil || !identity.Same(observed) || uint32(stat.Mode)&uint32(unix.S_IFMT) != expectedType {
		restoreErr := RestoreRetiredAt(parent, retired, name)
		mismatch := fmt.Errorf("%w: retired entry %q did not match its recorded identity and type", ErrOwnedTreeChanged, retired)
		if statErr != nil {
			mismatch = retiredLeafIdentityMismatch(retired, statErr)
		}
		if restoreErr != nil {
			return errors.Join(mismatch, fmt.Errorf("restore mismatched retired entry %q: %w", retired, restoreErr))
		}
		return mismatch
	}
	return removeRetiredAt(parent, retired, name, 0, "unlinkat", true)
}

func retiredLeafIdentityMismatch(name string, cause error) error {
	return atomicIdentityMismatch(fmt.Sprintf("inspect retired entry %q", name), cause)
}

// RemoveOwnedTreeAt removes exactly one previously manifested directory tree
// relative to parent. Every leaf is first moved to a deterministic sibling
// derived from the manifest (ownedCleanupRetiredName), post-validated against
// that manifest, and only then unlinked; directories are removed bottom-up.
// The retired name is deliberately deterministic rather than random: it is what
// lets compareOwnedTreeCleanupState recognise an interrupted removal and resume
// it. Safety therefore does not rest on an unguessable name -- it rests on the
// all-or-nothing manifest preflight, the no-replace retirement rename, and the
// post-move revalidation. A replacement or unknown entry is preserved and
// reported. Directory roots forward the descriptor identity captured by the
// preflight. Leaves deliberately retain the manifest entry as their admission
// reference because retireOwnedEntryAtWithOps recaptures their live identity
// immediately before rename and promotes that observation for post-rename
// content and identity validation.
func RemoveOwnedTreeAt(parent *os.File, name string, expected OwnedTree) error {
	defer keepDescriptorOwnersAlive(parent)
	if parent == nil {
		return errors.New("remove owned tree: parent descriptor is nil")
	}
	if !validLogicalEntry(name) {
		return fmt.Errorf("remove owned tree: invalid name %q", name)
	}
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("remove owned tree: invalid manifest: %w", err)
	}
	rootRetired := ownedCleanupRetiredName(".fu-retired-dir-", name, expected.RootIdentity)
	livePresent, err := namePresentAt(parent, name)
	if err != nil {
		return err
	}
	retiredPresent, err := namePresentAt(parent, rootRetired)
	if err != nil {
		return err
	}
	if !livePresent {
		if !retiredPresent {
			return nil
		}
		return finishRetiredOwnedDirectory(parent, rootRetired, name, expected.RootIdentity, expected.RootMode, false)
	}
	if retiredPresent {
		return fmt.Errorf("%w: owned root exists at live path %s and retired path %s", ErrOwnedTreeChanged,
			ownedSiblingPath(parent, name), ownedSiblingPath(parent, rootRetired))
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	root := os.NewFile(uintptr(fd), ownedSiblingPath(parent, name))
	if root == nil {
		_ = unix.Close(fd)
		return errors.New("remove owned tree: invalid root descriptor")
	}
	// The full-tree hash is the all-or-nothing preflight: no removal starts
	// until every entry is accounted for. Each entry is intentionally hashed
	// again immediately before retirement because a same-user writer can
	// change file contents after this snapshot; reusing this digest would turn
	// the preflight/removal gap into an ownership bypass.
	actual, err := snapshotOwnedOpenDirectory(root)
	if err != nil {
		_ = root.Close()
		return err
	}
	if err := compareOwnedTreeCleanupState(actual, expected, root.Name()); err != nil {
		_ = root.Close()
		return err
	}
	if err := removeOwnedDirectoryContents(root, "", expected); err != nil {
		_ = root.Close()
		return err
	}
	if err := root.Close(); err != nil {
		return err
	}
	return retireOwnedDirectoryAtPath(parent, name, name, ".fu-retired-dir-", actual.RootIdentity, expected.RootMode)
}

// compareOwnedTreeCleanupState validates every state the bottom-up removal
// protocol may durably leave behind. An entry may still be present at its live
// name, may be present at its deterministic retired name, or may already have
// been removed. Whichever copy remains must match the original manifest
// exactly, and every actual path must be accounted for before cleanup resumes.
func compareOwnedTreeCleanupState(actual, expected OwnedTree, rootPath string) error {
	if !actual.RootIdentity.Same(expected.RootIdentity) || actual.RootMode != expected.RootMode {
		return fmt.Errorf("%w: transaction-owned root no longer matches its recorded identity and mode", ErrOwnedTreeChanged)
	}
	actualByPath := make(map[string]OwnedTreeEntry, len(actual.Entries))
	for _, entry := range actual.Entries {
		actualByPath[entry.Path] = entry
	}
	consumed := make(map[string]string, len(actual.Entries))
	for _, expectedEntry := range expected.Entries {
		prefix := ".fu-retired-entry-"
		if expectedEntry.Kind == ownedDirectory {
			prefix = ".fu-retired-dir-"
		}
		retiredPath := path.Join(path.Dir(expectedEntry.Path),
			ownedCleanupRetiredName(prefix, expectedEntry.Path, expectedEntry.Identity))
		live, livePresent := actualByPath[expectedEntry.Path]
		retired, retiredPresent := actualByPath[retiredPath]
		if livePresent && retiredPresent {
			return fmt.Errorf("%w: owned entry %q exists at live path %s and retired path %s",
				ErrOwnedTreeChanged, expectedEntry.Path,
				filepath.Join(rootPath, filepath.FromSlash(expectedEntry.Path)),
				filepath.Join(rootPath, filepath.FromSlash(retiredPath)))
		}
		if !livePresent && !retiredPresent {
			continue
		}
		actualPath := expectedEntry.Path
		entry := live
		if retiredPresent {
			actualPath = retiredPath
			entry = retired
		}
		if other, duplicate := consumed[actualPath]; duplicate {
			return fmt.Errorf("%w: cleanup path %q ambiguously represents entries %q and %q",
				ErrOwnedTreeChanged, actualPath, other, expectedEntry.Path)
		}
		if err := compareOwnedEntry(entry, expectedEntry); err != nil {
			return err
		}
		consumed[actualPath] = expectedEntry.Path
	}
	for _, entry := range actual.Entries {
		if _, ok := consumed[entry.Path]; !ok {
			return fmt.Errorf("%w: transaction-owned tree gained unknown entry %q", ErrOwnedTreeChanged, entry.Path)
		}
	}
	return nil
}

func namePresentAt(parent *os.File, name string) (bool, error) {
	defer keepDescriptorOwnersAlive(parent)
	_, err := statAt(int(parent.Fd()), name)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return err == nil, err
}

func ownedCleanupRetiredName(prefix, logicalPath string, identity FileIdentity) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", prefix, logicalPath, identity.Device, identity.Inode)))
	return prefix + hex.EncodeToString(sum[:12])
}

// RemoveOwnedContents removes all descendants of an already-open directory
// while retaining the directory itself. The exact pre-cleanup manifest binds
// every child identity and content; unknown or replaced entries are kept.
func RemoveOwnedContents(dir *os.File, expected OwnedTree) error {
	defer keepDescriptorOwnersAlive(dir)
	if dir == nil {
		return errors.New("remove owned contents: directory descriptor is nil")
	}
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("remove owned contents: invalid manifest: %w", err)
	}
	actual, err := snapshotOwnedOpenDirectory(dir)
	if err != nil {
		return err
	}
	if err := compareOwnedTreeExact(actual, expected); err != nil {
		return err
	}
	if err := removeOwnedDirectoryContents(dir, "", expected); err != nil {
		return err
	}
	// The descriptor has pinned this exact directory since the initial snapshot;
	// unlike a pathname revalidation, comparing it with the admitted record
	// cannot observe a same-name replacement.
	identity, stat, err := openIdentity(int(dir.Fd()))
	if err != nil {
		return err
	}
	mode, kind, err := modeAndKind(&stat)
	if err != nil || kind != ownedDirectory || !identity.Same(expected.RootIdentity) || uint32(mode) != expected.RootMode {
		return fmt.Errorf("%w: owned root changed during contents cleanup", ErrOwnedTreeChanged)
	}
	checkFD, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	check := os.NewFile(uintptr(checkFD), dir.Name())
	if check == nil {
		_ = unix.Close(checkFD)
		return errors.New("remove owned contents: invalid verification descriptor")
	}
	names, readErr := check.Readdirnames(1)
	closeErr := check.Close()
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(names) != 0 {
		return fmt.Errorf("%w: owned root gained an entry during cleanup", ErrOwnedTreeChanged)
	}
	return nil
}

func snapshotOwnedOpenDirectory(dir *os.File) (OwnedTree, error) {
	defer keepDescriptorOwnersAlive(dir)
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return OwnedTree{}, err
	}
	copyDir := os.NewFile(uintptr(fd), dir.Name())
	if copyDir == nil {
		_ = unix.Close(fd)
		return OwnedTree{}, errors.New("snapshot owned directory: invalid descriptor")
	}
	identity, stat, err := openIdentity(fd)
	if err != nil {
		_ = copyDir.Close()
		return OwnedTree{}, err
	}
	mode, kind, err := modeAndKind(&stat)
	if err != nil || kind != ownedDirectory {
		_ = copyDir.Close()
		return OwnedTree{}, fmt.Errorf("snapshot owned directory: root is not a directory")
	}
	tree := OwnedTree{RootIdentity: identity, RootMode: uint32(mode)}
	if err := scanOwnedDirectory(copyDir, "", &tree.Entries, 0); err != nil {
		_ = copyDir.Close()
		return OwnedTree{}, err
	}
	if err := copyDir.Close(); err != nil {
		return OwnedTree{}, err
	}
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Path < tree.Entries[j].Path })
	return tree, tree.Validate()
}

// RemoveOwnedSymlinkAt safely removes a retired symlink with its recorded
// identity, mode, and raw target text.
func RemoveOwnedSymlinkAt(parent *os.File, name string, expected FileIdentity, mode uint32, target string) error {
	entry := OwnedTreeEntry{Path: name, Kind: ownedSymlink, Mode: mode, Identity: expected, Target: target}
	return retireOwnedEntryAt(parent, name, ".fu-retired-link-", entry)
}

func removeOwnedDirectoryContents(dir *os.File, prefix string, expected OwnedTree) error {
	defer keepDescriptorOwnersAlive(dir)
	direct := make([]OwnedTreeEntry, 0)
	for _, entry := range expected.Entries {
		parentPath := path.Dir(entry.Path)
		if prefix == "" {
			if parentPath == "." {
				direct = append(direct, entry)
			}
			continue
		}
		if parentPath == prefix {
			direct = append(direct, entry)
		}
	}
	sort.Slice(direct, func(i, j int) bool { return direct[i].Path < direct[j].Path })
	for _, entry := range direct {
		name := path.Base(entry.Path)
		if entry.Kind != ownedDirectory {
			if err := retireOwnedEntryAt(dir, name, ".fu-retired-entry-", entry); err != nil {
				return err
			}
			continue
		}
		retired := ownedCleanupRetiredName(".fu-retired-dir-", entry.Path, entry.Identity)
		livePresent, err := namePresentAt(dir, name)
		if err != nil {
			return err
		}
		retiredPresent, err := namePresentAt(dir, retired)
		if err != nil {
			return err
		}
		if !livePresent {
			if retiredPresent {
				if err := finishRetiredOwnedDirectory(dir, retired, name, entry.Identity, entry.Mode, false); err != nil {
					return err
				}
			}
			continue
		}
		if retiredPresent {
			return fmt.Errorf("%w: owned directory %q exists at live path %s and retired path %s", ErrOwnedTreeChanged,
				entry.Path, ownedSiblingPath(dir, name), ownedSiblingPath(dir, retired))
		}
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		child := os.NewFile(uintptr(fd), ownedSiblingPath(dir, name))
		if child == nil {
			_ = unix.Close(fd)
			return errors.New("remove owned tree: invalid child descriptor")
		}
		openedIdentity, opened, err := openIdentity(fd)
		if err != nil {
			_ = child.Close()
			return err
		}
		mode, kind, err := modeAndKind(&opened)
		if err != nil || kind != ownedDirectory || !openedIdentity.Same(entry.Identity) || uint32(mode) != entry.Mode {
			_ = child.Close()
			return fmt.Errorf("%w: owned directory %q changed before cleanup", ErrOwnedTreeChanged, entry.Path)
		}
		if err := removeOwnedDirectoryContents(child, entry.Path, expected); err != nil {
			_ = child.Close()
			return err
		}
		if err := child.Close(); err != nil {
			return err
		}
		if err := retireOwnedDirectoryAtPath(dir, name, entry.Path, ".fu-retired-dir-", openedIdentity, entry.Mode); err != nil {
			return err
		}
	}
	return nil
}

func retireOwnedEntryAt(parent *os.File, name, prefix string, expected OwnedTreeEntry) error {
	return retireOwnedEntryAtWithOps(parent, name, prefix, expected, entryIdentityAt, snapshotOwnedLeafAt, RenameNoReplaceAt)
}

type ownedLeafIdentityCapture func(int, string) (FileIdentity, unix.Stat_t, error)
type ownedLeafSnapshotFunc func(int, string, string) (OwnedTreeEntry, error)
type descriptorRenameFunc func(*os.File, string, *os.File, string) error

func retireOwnedEntryAtWithOps(parent *os.File, name, prefix string, expected OwnedTreeEntry, capture ownedLeafIdentityCapture, snapshot ownedLeafSnapshotFunc, rename descriptorRenameFunc) error {
	defer keepDescriptorOwnersAlive(parent)
	retired := ownedCleanupRetiredName(prefix, expected.Path, expected.Identity)
	livePresent, err := namePresentAt(parent, name)
	if err != nil {
		return err
	}
	retiredPresent, err := namePresentAt(parent, retired)
	if err != nil {
		return err
	}
	if livePresent && retiredPresent {
		return fmt.Errorf("%w: owned entry %q exists at live path %s and retired path %s", ErrOwnedTreeChanged,
			expected.Path, ownedSiblingPath(parent, name), ownedSiblingPath(parent, retired))
	}
	if !livePresent && !retiredPresent {
		return nil
	}
	renamedHere := false
	postRenameExpected := expected
	if livePresent {
		observedIdentity, observedStat, inspectErr := capture(int(parent.Fd()), name)
		if inspectErr != nil {
			return ownedLeafIdentityMismatch(expected.Path, inspectErr)
		}
		observedMode, observedKind, inspectErr := modeAndKind(&observedStat)
		if inspectErr != nil {
			return ownedLeafIdentityMismatch(expected.Path, inspectErr)
		}
		if !observedIdentity.Same(expected.Identity) || observedKind != expected.Kind || uint32(observedMode) != expected.Mode {
			return fmt.Errorf("%w: owned entry %q changed identity, type, or mode before retirement", ErrOwnedTreeChanged, expected.Path)
		}
		if err := rename(parent, name, parent, retired); err != nil {
			return retirementRenameError(name, retired, err)
		}
		renamedHere = true
		postRenameExpected.Identity = observedIdentity
	}
	actual, inspectErr := snapshot(int(parent.Fd()), retired, expected.Path)
	if inspectErr != nil || compareOwnedEntry(actual, postRenameExpected) != nil {
		mismatch := fmt.Errorf("%w: retired entry %q changed before cleanup", ErrOwnedTreeChanged, expected.Path)
		if inspectErr != nil {
			mismatch = retiredLeafIdentityMismatch(expected.Path, inspectErr)
		}
		if renamedHere {
			if restoreErr := RestoreRetiredAt(parent, retired, name); restoreErr != nil {
				return errors.Join(mismatch, fmt.Errorf("restore mismatched retired entry %q: %w", retired, restoreErr))
			}
		}
		return mismatch
	}
	return removeRetiredAt(parent, retired, name, 0, "unlinkat", renamedHere)
}

func ownedLeafIdentityMismatch(path string, cause error) error {
	mismatch := fmt.Errorf("%w: inspect owned entry %q before retirement", ErrOwnedTreeChanged, path)
	if cause != nil {
		return errors.Join(mismatch, cause)
	}
	return mismatch
}

func snapshotOwnedLeafAt(parentFD int, name, logicalPath string) (OwnedTreeEntry, error) {
	identity, stat, err := entryIdentityAt(parentFD, name)
	if err != nil {
		return OwnedTreeEntry{}, err
	}
	mode, kind, err := modeAndKind(&stat)
	if err != nil {
		return OwnedTreeEntry{}, err
	}
	entry := OwnedTreeEntry{Path: logicalPath, Kind: kind, Mode: uint32(mode), Identity: identity}
	switch kind {
	case ownedFile:
		entry.Digest, _, _, err = hashFileAt(parentFD, name, entry.Identity)
	case ownedSymlink:
		entry.Target, err = readlinkAt(parentFD, name)
	case ownedDirectory:
		return OwnedTreeEntry{}, fmt.Errorf("owned leaf %q is a directory", logicalPath)
	}
	return entry, err
}

func retireOwnedDirectoryAt(parent *os.File, name, prefix string, expected FileIdentity, expectedMode uint32) error {
	return retireOwnedDirectoryAtPath(parent, name, name, prefix, expected, expectedMode)
}

func retireOwnedDirectoryAtPath(parent *os.File, name, logicalPath, prefix string, expected FileIdentity, expectedMode uint32) error {
	return retireOwnedDirectoryAtPathWithCapture(parent, name, logicalPath, prefix, expected, expectedMode, entryIdentityAt)
}

func retireOwnedDirectoryAtPathWithCapture(
	parent *os.File,
	name, logicalPath, prefix string,
	expected FileIdentity,
	expectedMode uint32,
	capture ownedLeafIdentityCapture,
) error {
	retired := ownedCleanupRetiredName(prefix, logicalPath, expected)
	livePresent, err := namePresentAt(parent, name)
	if err != nil {
		return err
	}
	retiredPresent, err := namePresentAt(parent, retired)
	if err != nil {
		return err
	}
	if livePresent && retiredPresent {
		return fmt.Errorf("%w: directory exists at live path %s and retired path %s", ErrOwnedTreeChanged,
			ownedSiblingPath(parent, name), ownedSiblingPath(parent, retired))
	}
	if !livePresent && !retiredPresent {
		return nil
	}
	renamedHere := false
	postRenameExpected := expected
	if livePresent {
		identity, stat, err := capture(int(parent.Fd()), name)
		if err != nil {
			return ownedDirectoryIdentityMismatch(logicalPath, err)
		}
		mode, kind, modeErr := modeAndKind(&stat)
		if modeErr != nil || kind != ownedDirectory || !identity.Same(expected) || uint32(mode) != expectedMode {
			return atomicIdentityMismatch(
				fmt.Sprintf("owned directory %q changed before retirement", name),
				modeErr,
			)
		}
		if err := RenameNoReplaceAt(parent, name, parent, retired); err != nil {
			return retirementRenameError(name, retired, err)
		}
		renamedHere = true
		postRenameExpected = identity
	}
	return finishRetiredOwnedDirectory(parent, retired, name, postRenameExpected, expectedMode, renamedHere)
}

// Once a manifest has admitted a live object for retirement, a failed identity
// recapture is an ownership conflict as well as an environmental error. Pure
// discovery paths, including archive recovery scans, have not admitted an
// object yet and preserve environmental errors without adding that sentinel.
func ownedDirectoryIdentityMismatch(path string, cause error) error {
	return atomicIdentityMismatch(fmt.Sprintf("inspect owned directory %q before retirement", path), cause)
}

func ownedSiblingPath(parent *os.File, name string) string {
	if parent == nil || parent.Name() == "" {
		return name
	}
	return filepath.Join(parent.Name(), name)
}

func retirementRenameError(live, retired string, err error) error {
	if errors.Is(err, os.ErrExist) || errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%w: retirement rename from %q to occupied %q: %w", ErrOwnedTreeChanged, live, retired, err)
	}
	return err
}

func finishRetiredOwnedDirectory(parent *os.File, retired, name string, expected FileIdentity, expectedMode uint32, renamedHere bool) error {
	defer keepDescriptorOwnersAlive(parent)
	identity, stat, statErr := entryIdentityAt(int(parent.Fd()), retired)
	mode, kind, modeErr := modeAndKind(&stat)
	if statErr != nil || modeErr != nil || kind != ownedDirectory || !identity.Same(expected) || uint32(mode) != expectedMode {
		mismatch := retiredDirectoryIdentityMismatch(name, statErr, modeErr)
		if renamedHere {
			if restoreErr := RestoreRetiredAt(parent, retired, name); restoreErr != nil {
				return errors.Join(mismatch, fmt.Errorf("restore mismatched retired directory %q: %w", retired, restoreErr))
			}
		}
		return mismatch
	}
	return removeRetiredAt(parent, retired, name, unix.AT_REMOVEDIR, "rmdir", renamedHere)
}

func retiredDirectoryIdentityMismatch(name string, statErr, modeErr error) error {
	return atomicIdentityMismatch(
		fmt.Sprintf("retired directory %q changed before finalization", name),
		errors.Join(statErr, modeErr),
	)
}

func removeRetiredAt(parent *os.File, retired, original string, flags int, operation string, renamedHere bool) error {
	defer keepDescriptorOwnersAlive(parent)
	if err := unix.Unlinkat(int(parent.Fd()), retired, flags); err != nil {
		removeErr := fmt.Errorf("%w: %w", ErrOwnedTreeChanged, &os.PathError{Op: operation, Path: retired, Err: err})
		if renamedHere {
			if restoreErr := RestoreRetiredAt(parent, retired, original); restoreErr != nil {
				return errors.Join(removeErr, fmt.Errorf("restore %q after failed %s: %w", retired, operation, restoreErr))
			}
		}
		return removeErr
	}
	return nil
}
