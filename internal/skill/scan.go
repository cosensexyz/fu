package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Candidate is a directory holding a valid SKILL.md, found by Scan.
type Candidate struct {
	Dir  string // absolute path
	Meta Meta
}

// Scan walks root and returns every directory containing a SKILL.md.
// root is resolved to an absolute path before walking, so every
// Candidate.Dir is absolute regardless of what the caller passed in.
// .git is skipped entirely; skills are not searched inside skills.
//
// A problem with root itself (missing, unreadable, or not a directory)
// is fatal and returned as err, since there is nothing left to scan. A
// problem reading a descendant is instead recorded into the invalid
// map, keyed by that path, and the walk continues with the rest of the
// tree: one unreadable item must not prevent the others from being
// processed.
func Scan(root string) (valid []Candidate, invalid map[string]error, err error) {
	invalid = map[string]error{}
	absRoot, aerr := filepath.Abs(root)
	if aerr != nil {
		return valid, invalid, aerr
	}

	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if p == absRoot {
				return werr // root itself is unusable: fatal, abort the scan
			}
			invalid[p] = werr // descendant is unusable: isolate it, keep walking
			return nil
		}
		if p == absRoot && !d.IsDir() {
			return fmt.Errorf("%s: not a directory", p)
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		m, perr := ParseMeta(p)
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
			return filepath.SkipDir
		}
		if verr := Validate(m, filepath.Base(p)); verr != nil {
			invalid[p] = verr
			return filepath.SkipDir
		}
		valid = append(valid, Candidate{Dir: p, Meta: m})
		return filepath.SkipDir
	})
	return valid, invalid, err
}
