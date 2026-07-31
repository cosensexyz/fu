package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Entry type tags and field separator for the record encoding below.
const (
	entryFile = "F"
	// "D" was the directory tag, retired in round 6 when directories left
	// the projection (see Digest). Not reused for anything else: an old
	// digest computed with it must never accidentally collide with a new
	// one.
	entrySymlink = "L"
	fieldSep     = "\x00"
)

// ManifestEntry is one already-authorized filesystem entry projected into a
// skill digest. File digests use the same "sha256:<hex>" form as the store's
// ownership manifest; directories are ignored because Git cannot persist
// them independently.
type ManifestEntry struct {
	Path   string
	Mode   fs.FileMode
	Digest string
	Target string
}

func digestRecords(records []string) string {
	sort.Strings(records)
	sum := sha256.New()
	for _, record := range records {
		_, _ = sum.Write([]byte(record))
		_, _ = sum.Write([]byte(fieldSep))
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

func excludedGitPath(name string) bool {
	for _, component := range strings.Split(name, "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

// DigestManifest computes the canonical projection from an authoritative
// entry set instead of enumerating a live directory. This prevents content
// introduced by another writer from entering a transaction's baseline merely
// because it appeared beneath a directory the transaction created.
func DigestManifest(entries []ManifestEntry) (string, error) {
	records := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Path == "" || entry.Path == "." || !fs.ValidPath(entry.Path) || path.Clean(entry.Path) != entry.Path {
			return "", fmt.Errorf("invalid manifest path %q", entry.Path)
		}
		if _, ok := seen[entry.Path]; ok {
			return "", fmt.Errorf("duplicate manifest path %q", entry.Path)
		}
		seen[entry.Path] = struct{}{}
		if excludedGitPath(entry.Path) {
			continue
		}
		switch entry.Mode.Type() {
		case 0:
			digestHex := strings.TrimPrefix(entry.Digest, "sha256:")
			if len(digestHex) != sha256.Size*2 || "sha256:"+digestHex != entry.Digest {
				return "", fmt.Errorf("manifest file %q has invalid SHA-256 digest", entry.Path)
			}
			if _, err := hex.DecodeString(digestHex); err != nil {
				return "", fmt.Errorf("manifest file %q has invalid SHA-256 digest: %w", entry.Path, err)
			}
			exec := "0"
			if entry.Mode&0o100 != 0 {
				exec = "1"
			}
			records = append(records, strings.Join([]string{entryFile, entry.Path, digestHex, exec}, fieldSep))
		case fs.ModeDir:
			continue
		case fs.ModeSymlink:
			records = append(records, strings.Join([]string{entrySymlink, entry.Path, entry.Target}, fieldSep))
		default:
			return "", fmt.Errorf("manifest entry %q has unsupported mode %v", entry.Path, entry.Mode)
		}
	}
	return digestRecords(records), nil
}

// Digest computes the canonical snapshot digest of a skill directory.
// This projection is the single source of truth shared by copying and
// digesting (DESIGN §3): what gets copied is exactly what gets hashed.
//
// The projection is defined in terms git can persist (round 6 finding):
// files and symlinks only, .git excluded on name alone at any depth. A
// directory contributes no record of its own -- git stores none either, so
// counting them made a skill digest differently in the store's worktree
// than in a fresh clone of that same store, breaking the one agreement this
// projection exists to provide. The consequence, stated plainly: a change
// consisting solely of adding or removing an empty directory is invisible
// to fu, exactly as it is to git.
//
// Each entry is encoded as a NUL-delimited record led by a one-byte type
// tag that fixes the number of fields that follow:
//
//	F <rel> <hash-hex> <exec-flag>   file: content hash + owner-exec bit
//	L <rel> <target>                 symlink: raw target, never resolved
//
// A POSIX path or symlink target cannot contain NUL, so NUL is a safe,
// unambiguous delimiter both between fields and between records in the
// final hashed stream: the tag fixes how many fields follow, so the
// stream can always be split back into the exact original records. A
// human-readable separator (e.g. " -> ") cannot make that guarantee — a
// symlink literally named "link1 -> a" targeting "b" would render
// identically to a symlink named "link1" targeting "a -> b".
func Digest(dir string) (string, error) {
	return DigestFS(os.DirFS(dir), ".")
}

// DigestFS computes the same projection within an fs.FS. It lets write
// operations digest content through an already checked os.Root rather than
// reopening a mutable pathname.
func DigestFS(fsys fs.FS, dir string) (string, error) {
	var records []string
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.Name() == ".git" {
			// Excluded on name alone, at any depth, whatever kind of entry
			// it is -- directory (normal clone), regular file
			// (worktree/submodule pointer), or symlink. Round 6 finding: the
			// symlink case used to fall through to the symlink arm below and
			// enter the digest, while store.stageAll excludes .git by name
			// regardless of type. That disagreement is permanent, since it
			// makes digest(store) differ from digest(clone) for a skill
			// nobody has touched.
			if d.IsDir() {
				return fs.SkipDir
			}
			// nil, not filepath.SkipDir: SkipDir on a non-directory entry
			// skips the rest of its *containing* directory, not just this
			// entry. nil simply excludes this one.
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := fs.ReadLink(fsys, p)
			if err != nil {
				return err
			}
			records = append(records, strings.Join([]string{entrySymlink, rel, target}, fieldSep))
		case d.IsDir():
			// Directories contribute nothing of their own (round 6 finding).
			// Git stores no directory entries: a directory exists in history
			// exactly when some file or symlink under it does, and an empty
			// one cannot be represented at all. Recording a "D" record per
			// directory therefore made a skill holding an empty directory
			// digest differently in the store's worktree than in a fresh
			// clone of that same store -- permanently, and for content
			// nobody had touched. A directory that does hold something is
			// already implied by that content's own record and its relative
			// path, so nothing is lost here that git itself would have kept.
			return nil
		default:
			// Classified before opening. A blocking open of a FIFO with no
			// writer never returns, and DigestFS is designated as the shared
			// projection for add/adopt/update -- the first caller to reach one
			// would hang while holding fu.lock, wedging every other fu process.
			// DigestManifest already refuses unsupported modes; this is the
			// same rule on the walking side.
			if d.Type()&^fs.ModeSymlink&^fs.ModeDir != 0 {
				return fmt.Errorf("%s has unsupported mode %v and cannot be part of a skill digest", rel, d.Type())
			}
			f, err := fsys.Open(p)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			closeErr := f.Close() // explicit close (not defer): this runs in a walk loop, not a single-shot function
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			exec := "0"
			if info.Mode()&0o100 != 0 {
				exec = "1"
			}
			records = append(records, strings.Join([]string{entryFile, rel, hex.EncodeToString(h.Sum(nil)), exec}, fieldSep))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return digestRecords(records), nil
}
