package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

const (
	adoptEntryDirectory = "directory"
	adoptEntrySymlink   = "symlink"
)

func adoptIdentity(stat *unix.Stat_t) store.FileIdentity {
	return store.FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
}

func adoptIdentityValid(id store.FileIdentity) bool {
	return id.Inode != 0
}

func openAdoptDirectory(path string) (*os.File, store.FileIdentity, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, store.FileIdentity{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, store.FileIdentity{}, fmt.Errorf("open %s: invalid directory descriptor", path)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, store.FileIdentity{}, err
	}
	return file, adoptIdentity(&stat), nil
}

func statAdoptEntry(parentFD int, name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	return stat, err
}

func readAdoptLink(parentFD int, name string) (string, error) {
	for size := 128; size <= 1<<20; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(parentFD, name, buf)
		if err != nil {
			return "", err
		}
		if n < len(buf) {
			return string(buf[:n]), nil
		}
	}
	return "", unix.ENAMETOOLONG
}

func captureAdoptTargets(agents, wholeDirAgents []agent.Agent, name, digest string) ([]AdoptTarget, error) {
	return captureAdoptTargetsWithHooks(agents, wholeDirAgents, name, digest, nil, nil)
}

func captureAdoptTargetsWithHooks(agents, wholeDirAgents []agent.Agent, name, digest string, beforeSourcePair, afterLinkRead func() error) ([]AdoptTarget, error) {
	whole := make(map[string]bool, len(wholeDirAgents))
	for _, a := range wholeDirAgents {
		whole[a.Name()] = true
	}
	targets := make([]AdoptTarget, 0, len(agents))
	for _, a := range agents {
		target, err := captureAdoptTargetWithHooks(a, name, digest, whole[a.Name()], beforeSourcePair, afterLinkRead)
		if err != nil {
			return nil, fmt.Errorf("capture adopt target for %s: %w", a.Name(), err)
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func captureAdoptTarget(a agent.Agent, name, digest string, wholeDir bool) (AdoptTarget, error) {
	return captureAdoptTargetWithHooks(a, name, digest, wholeDir, nil, nil)
}

func captureAdoptTargetWithHooks(a agent.Agent, name, digest string, wholeDir bool, beforeSourcePair, afterLinkRead func() error) (AdoptTarget, error) {
	skillsDir := filepath.Clean(a.SkillsDir())
	if !filepath.IsAbs(skillsDir) {
		return AdoptTarget{}, fmt.Errorf("skills directory is not absolute: %s", skillsDir)
	}
	parentPath, entryName := skillsDir, name
	if wholeDir {
		parentPath, entryName = filepath.Dir(skillsDir), filepath.Base(skillsDir)
	}
	parent, parentIdentity, err := openAdoptDirectory(parentPath)
	if err != nil {
		return AdoptTarget{}, err
	}
	defer parent.Close()
	entry, err := statAdoptEntry(int(parent.Fd()), entryName)
	if err != nil {
		return AdoptTarget{}, err
	}
	target := AdoptTarget{
		Agent:          a.Name(),
		SkillsDir:      skillsDir,
		WholeDir:       wholeDir,
		ParentIdentity: parentIdentity,
		EntryIdentity:  adoptIdentity(&entry),
		Digest:         digest,
	}
	mode := uint32(entry.Mode) & uint32(unix.S_IFMT)
	switch mode {
	case uint32(unix.S_IFDIR):
		if wholeDir {
			return AdoptTarget{}, fmt.Errorf("whole-directory target %s is not a symlink", skillsDir)
		}
		target.EntryKind = adoptEntryDirectory
		target.SourcePath = filepath.Join(skillsDir, name)
		target.SourceIdentity = target.EntryIdentity
	case uint32(unix.S_IFLNK):
		target.EntryKind = adoptEntrySymlink
		target.LinkTarget, err = readAdoptLink(int(parent.Fd()), entryName)
		if err != nil {
			return AdoptTarget{}, err
		}
		if afterLinkRead != nil {
			if err := afterLinkRead(); err != nil {
				return AdoptTarget{}, err
			}
		}
		sourcePath := target.LinkTarget
		if !filepath.IsAbs(sourcePath) {
			sourcePath = filepath.Join(parentPath, sourcePath)
		}
		target.SourcePath, err = filepath.EvalSymlinks(sourcePath)
		if err != nil {
			return AdoptTarget{}, err
		}
		source, sourceIdentity, openErr := openAdoptDirectory(target.SourcePath)
		if openErr != nil {
			return AdoptTarget{}, openErr
		}
		target.SourceIdentity = sourceIdentity
		if wholeDir {
			if beforeSourcePair != nil {
				if hookErr := beforeSourcePair(); hookErr != nil {
					_ = source.Close()
					return AdoptTarget{}, hookErr
				}
			}
			root, rootErr := pairBoundAdoptRoot(target.SourcePath, source, sourceIdentity)
			if rootErr != nil {
				_ = source.Close()
				return AdoptTarget{}, rootErr
			}
			manifest, projectErr := scanDirSwitchEntries(root)
			target.TargetManifest = manifest
			rootCloseErr := root.Close()
			if projectErr != nil {
				_ = source.Close()
				return AdoptTarget{}, projectErr
			}
			for _, entry := range manifest {
				if entry.Name == "SKILL.md" {
					_ = source.Close()
					// Same wording as the scan-time refusal in scanAdoptEntries,
					// hint included: this copy is reached only when the file
					// appeared mid-run, and a user racing it deserves the way
					// out just as much as one who had it there all along.
					return AdoptTarget{}, fmt.Errorf(
						"%w: agent %s target %s contains a root SKILL.md; move that skill into a named child directory or install it with `fu add`",
						ErrWholeDirRootSkillUnsupported, a.Name(), target.SourcePath)
				}
			}
			if rootCloseErr != nil {
				_ = source.Close()
				return AdoptTarget{}, rootCloseErr
			}
		}
		if closeErr := source.Close(); closeErr != nil {
			return AdoptTarget{}, closeErr
		}
	default:
		return AdoptTarget{}, fmt.Errorf("entry has unsupported mode %#o", entry.Mode)
	}
	return target, nil
}

func scanDirSwitchEntries(root *os.Root) ([]DirSwitchEntry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	manifest := make([]DirSwitchEntry, 0, len(entries))
	for _, entry := range entries {
		stat, err := statAdoptEntry(int(dir.Fd()), entry.Name())
		if err != nil {
			_ = dir.Close()
			return nil, err
		}
		mode := checkedAgentFileMode(uint32(stat.Mode))
		item := DirSwitchEntry{Name: entry.Name(), Mode: uint32(mode.Type()), Identity: adoptIdentity(&stat)}
		if mode&fs.ModeSymlink != 0 {
			item.LinkTarget, err = readAdoptLink(int(dir.Fd()), entry.Name())
			if err != nil {
				_ = dir.Close()
				return nil, err
			}
		}
		manifest = append(manifest, item)
	}
	if err := dir.Close(); err != nil {
		return nil, err
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Name < manifest[j].Name })
	return manifest, nil
}

// sameDirSwitchEntries is the strict comparison, identity included. It is
// correct only for objects fu created and can therefore prove it owns: the
// replacement sibling and the archived backup. See
// sameDirSwitchTargetEntries for why it must never be applied to the user's
// own target directory.
func sameDirSwitchEntries(left, right []DirSwitchEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// sameDirSwitchTargetEntries compares the user's whole-directory target
// against its inventory by name set and entry type only -- never by inode
// identity, and never by a child's own link target.
//
// The check exists for one reason: the replacement sibling's passthrough
// links mirror the target's children, so the *name set* has to hold. A
// passthrough link is filepath.Join(target.SourcePath, entry.Name) and
// resolves through whatever currently lives at that name, so replacing a
// child does not invalidate it.
//
// Identity did not just over-refuse, it over-refused irreversibly. fu does
// not own those inodes, and replacing a direct child is what a `git pull`, an
// editor's atomic save, or an rsync does -- all renames, all new inodes. A
// digest mismatch can be undone by restoring the bytes; an inode mismatch
// cannot be undone at all, so the conflict became permanent, and in the
// swapped-vacant window it became permanent with the agent's skills directory
// missing entirely.
func sameDirSwitchTargetEntries(left, right []DirSwitchEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name || left[i].Mode != right[i].Mode {
			return false
		}
	}
	return true
}

func targetForAgent(record *TxnRecord, agentName string) (AdoptTarget, error) {
	for _, target := range record.AdoptTargets {
		if target.Agent == agentName {
			return target, nil
		}
	}
	return AdoptTarget{}, fmt.Errorf("%w: adopt transaction has no filesystem target for agent %q", ErrTxnConflict, agentName)
}

func openBoundAdoptParent(target AdoptTarget) (*os.File, error) {
	parentPath := target.SkillsDir
	if target.WholeDir {
		parentPath = filepath.Dir(target.SkillsDir)
	}
	parent, identity, err := openAdoptDirectory(parentPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, asTargetConflict(fmt.Errorf("%w: reopen adopt parent %s: %w", ErrTxnConflict, parentPath, err))
		}
		return nil, asTargetConflict(fmt.Errorf("%w: reopen adopt parent %s: %w", ErrTxnConflict, parentPath, err))
	}
	if identity != target.ParentIdentity {
		_ = parent.Close()
		return nil, asTargetConflict(fmt.Errorf("%w: adopt parent %s was replaced", ErrTxnConflict, parentPath))
	}
	return parent, nil
}

func pairBoundAdoptRoot(path string, dir *os.File, expected store.FileIdentity) (*os.Root, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open rooted adopt parent %s: %v", ErrTxnConflict, path, err)
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	dirInfo, err := dir.Stat()
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !os.SameFile(rootInfo, dirInfo) || !adoptIdentityValid(expected) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: adopt parent %s changed while its descriptor was being paired", ErrTxnConflict, path)
	}
	return root, nil
}

func openBoundAdoptSource(target AdoptTarget) (*os.Root, error) {
	root, err := os.OpenRoot(target.SourcePath)
	if err != nil {
		return nil, asTargetConflict(fmt.Errorf("%w: open recorded adopt source %s: %v", ErrTxnConflict, target.SourcePath, err))
	}
	opened, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, asTargetConflict(err)
	}
	var stat unix.Stat_t
	statErr := unix.Fstat(int(opened.Fd()), &stat)
	closeErr := opened.Close()
	if statErr != nil {
		_ = root.Close()
		return nil, asTargetConflict(statErr)
	}
	if closeErr != nil {
		_ = root.Close()
		return nil, asTargetConflict(closeErr)
	}
	if adoptIdentity(&stat) != target.SourceIdentity {
		_ = root.Close()
		return nil, asTargetConflict(fmt.Errorf("%w: recorded adopt source %s was replaced", ErrTxnConflict, target.SourcePath))
	}
	return root, nil
}

func validateCurrentAdoptEntry(parent *os.File, target AdoptTarget, name string) error {
	defer keepDescriptorOwnersAlive(parent)
	entryName := name
	if target.WholeDir {
		entryName = filepath.Base(target.SkillsDir)
	}
	entry, err := statAdoptEntry(int(parent.Fd()), entryName)
	if errors.Is(err, unix.ENOENT) {
		return asTargetConflict(fs.ErrNotExist)
	}
	if err != nil {
		return asTargetConflict(err)
	}
	if adoptIdentity(&entry) != target.EntryIdentity {
		return asTargetConflict(fmt.Errorf("%w: adopt entry %s/%s was replaced", ErrTxnConflict, target.Agent, name))
	}
	mode := uint32(entry.Mode) & uint32(unix.S_IFMT)
	switch target.EntryKind {
	case adoptEntryDirectory:
		if mode != uint32(unix.S_IFDIR) {
			return asTargetConflict(fmt.Errorf("%w: adopt entry %s/%s changed type", ErrTxnConflict, target.Agent, name))
		}
	case adoptEntrySymlink:
		if mode != uint32(unix.S_IFLNK) {
			return asTargetConflict(fmt.Errorf("%w: adopt entry %s/%s changed type", ErrTxnConflict, target.Agent, name))
		}
		raw, err := readAdoptLink(int(parent.Fd()), entryName)
		if err != nil {
			return asTargetConflict(err)
		}
		if raw != target.LinkTarget {
			return asTargetConflict(fmt.Errorf("%w: adopt symlink %s/%s changed target", ErrTxnConflict, target.Agent, name))
		}
	default:
		return fmt.Errorf("%w: adopt target for %s has unknown entry kind %q", ErrTxnConflict, target.Agent, target.EntryKind)
	}
	return nil
}
