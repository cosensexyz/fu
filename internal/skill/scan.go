package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
)

// Candidate is a directory holding a valid SKILL.md.
type Candidate struct {
	Dir  string // slash-relative path rooted at the scanned fs.FS
	Meta Meta
}

// ScanFS is Scan's descriptor-friendly form. Candidate and invalid paths are
// clean slash-relative names, with "." representing the filesystem root.
func ScanFS(fsys fs.FS) (valid []Candidate, invalid map[string]error, err error) {
	invalid = map[string]error{}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if p == "." {
				return werr // root itself is unusable: fatal, abort the scan
			}
			invalid[p] = werr // descendant is unusable: isolate it, keep walking
			return nil
		}
		if p == "." && !d.IsDir() {
			return fmt.Errorf("%s: not a directory", p)
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return fs.SkipDir
		}
		m, perr := ParseMetaFS(fsys, p)
		// errors.Is rather than == : this is the only sentinel comparison
		// in production code that used ==. If ParseMeta ever wraps this
		// error, == would silently misclassify every directory lacking a
		// SKILL.md as invalid and trigger filepath.SkipDir below, making
		// Scan return zero skills for any nested layout.
		if errors.Is(perr, ErrNoSkillFile) {
			return nil // keep walking into children
		}
		if perr != nil {
			invalid[p] = perr
			return fs.SkipDir
		}
		if verr := Validate(m, dirNameFS(m, p)); verr != nil {
			invalid[p] = verr
			return fs.SkipDir
		}
		valid = append(valid, Candidate{Dir: p, Meta: m})
		return fs.SkipDir
	})
	return valid, invalid, err
}

func dirNameFS(m Meta, p string) string {
	// A root-level SKILL.md has no directory basename to validate against, so
	// its declared name supplies the identity. Nested skills must instead match
	// their actual source directory (SPEC rule 1).
	if p == "." {
		return m.Name
	}
	return path.Base(p)
}
