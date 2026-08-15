package skill

import (
	"fmt"
	"io/fs"
	"path"
)

// ProjectDir walks the subtree at dir beneath fsys and returns the read-only
// projection of every entry it holds: files with their content digest and
// mode, symlinks with their raw target, directories with their mode. .git is
// excluded by name at any depth, exactly as DigestFS excludes it -- the
// projection and the digest projection must always see the same set
// (DESIGN §3: copying and digesting see the same set, so their excluded
// sets cannot diverge).
//
// This is the source-side twin of the store's copy step: the expected entry
// set is computed here before anything is created, so a transaction can
// declare exactly what it is about to copy and recovery can classify any
// interrupted state (plan D3).
//
// Files are opened through fsys after classification by DirEntry type, the
// same discipline DigestFS documents: a FIFO with no writer must be refused
// before any open attempt, never opened. Symlinked directories are reported
// as symlink entries and never descended into.
func ProjectDir(fsys fs.FS, dir string) ([]ManifestEntry, error) {
	var entries []ManifestEntry
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.Name() == ".git" {
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%s is a .git symlink; skill projections may exclude only real .git directories or files", p)
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := pathRel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		entry := ManifestEntry{Path: rel, Mode: d.Type()}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := fs.ReadLink(fsys, p)
			if err != nil {
				return err
			}
			entry.Target = target
			entries = append(entries, entry)
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			entry.Mode = info.Mode()
			entries = append(entries, entry)
		default:
			if d.Type()&^fs.ModeSymlink&^fs.ModeDir != 0 {
				return fmt.Errorf("%s has unsupported mode %v and cannot be part of a skill", rel, d.Type())
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			digest, err := digestProjectedFile(fsys, p, info)
			if err != nil {
				return err
			}
			entry.Mode = info.Mode()
			entry.Digest = "sha256:" + digest
			entries = append(entries, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// pathRel mirrors filepath.Rel in slash space. fs.WalkDir paths are slash
// separated, so filepath.Rel would mis-handle them on Windows; fu's v1
// platforms are POSIX, but the projection must stay path-separator agnostic
// because fs.FS paths are always slash separated.
func pathRel(dir, p string) (string, error) {
	rel, err := relSlash(dir, p)
	if err != nil {
		return "", err
	}
	return rel, nil
}

func relSlash(dir, p string) (string, error) {
	if dir == "." {
		return path.Clean(p), nil
	}
	if p == dir {
		return ".", nil
	}
	prefix := dir + "/"
	if len(p) > len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):], nil
	}
	return "", fmt.Errorf("path %q is not beneath %q", p, dir)
}
