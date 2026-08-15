package skill

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Meta is the SKILL.md frontmatter subset fu understands. Unknown keys
// are ignored on parse; fu never rewrites skill content (SPEC rule 5).
type Meta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// ErrNoSkillFile is returned when SKILL.md is not found in a skill directory.
var ErrNoSkillFile = errors.New("SKILL.md not found")

// maxSkillFileBytes bounds every SKILL.md read. It was the one external read
// with no cap, and it is reached for every directory in a scanned source tree
// -- including a third-party repository handed to `fu add <git-url>`, whose
// content fu has not vetted and whose files the 64 MiB copy limit only bounds
// much later, after the scan has already read them (round 18 finding I8).
// 4 MiB is far above any real frontmatter-plus-prose skill file and far below
// what exhausts a process.
const maxSkillFileBytes = 4 << 20

// readSkillFile reads dir/SKILL.md through an already pinned filesystem,
// refusing anything past the cap without materializing it. The limit reader
// takes one byte more than the cap so a file exactly at the limit is accepted
// and the first byte over it is detected without a second stat.
//
// The entry is classified before it is opened, the same discipline DigestFS
// documents and ProjectDir implements: a blocking open of a FIFO with no
// writer never returns. This was the one reader that did not follow it, and it
// is reached for every directory of a source `fu add <local-dir>` scans and
// through skill.ParseMeta on every adopt candidate -- a mkfifo'd SKILL.md hung
// the process outright. Stat does not block on a FIFO, so classifying with it
// costs nothing.
func readSkillFile(fsys fs.FS, dir string) ([]byte, error) {
	name := path.Join(dir, "SKILL.md")
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is %s, not a file", name, describeEntry(info.Mode()))
	}
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSkillFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSkillFileBytes {
		return nil, fmt.Errorf("%s size exceeds the %d-byte limit", name, maxSkillFileBytes)
	}
	return raw, nil
}

// nameRe encodes the Agent Skills naming rules: lowercase alphanumerics
// and single hyphens, no leading/trailing/consecutive hyphens.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// describeEntry names a filesystem entry kind in prose. A raw fs.FileMode
// renders as "L---------", which is accurate and unreadable.
//
// Every arm names the kind positively, so the result reads correctly in any
// "%s is %s, not a <what was wanted>" sentence. The default arm used to say
// "not a directory", which was written for ParseMeta's one caller and became
// self-contradictory as soon as readSkillFile reused it: a directory produced
// "SKILL.md is not a directory, not a file".
func describeEntry(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "a symlink"
	case mode&os.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&os.ModeSocket != 0:
		return "a socket"
	case mode&os.ModeDevice != 0:
		return "a device"
	case mode.IsDir():
		return "a directory"
	case mode.IsRegular():
		return "a file"
	default:
		return "an unknown entry kind"
	}
}

// ParseMeta reads dir/SKILL.md and returns its frontmatter.
//
// dir must be a real directory, checked with Lstat before it is opened, and
// SKILL.md is read through an os.Root anchored there. Pathname-based reading
// followed a symlink at either component out of the store: with
// store/skills/<name> replaced by a link, `fu show <name>` printed content from
// outside the store and exited 0.
//
// That is a separate guarantee from SPEC rule 7's path-safety check, which has
// since landed as ValidateLinks and runs on every import path -- this anchoring
// protects a *read* command against a store-internal link resolving outward,
// which no import-time check can do.
func ParseMeta(dir string) (Meta, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Meta{}, ErrNoSkillFile
		}
		return Meta{}, err
	}
	if !info.IsDir() {
		return Meta{}, fmt.Errorf("%s is %s, not a skill directory", dir, describeEntry(info.Mode()))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Meta{}, err
	}
	defer root.Close()
	raw, err := readSkillFile(root.FS(), ".")
	if err != nil {
		if os.IsNotExist(err) {
			return Meta{}, ErrNoSkillFile
		}
		return Meta{}, err
	}
	fm, err := frontmatter(string(raw))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return Meta{}, fmt.Errorf("invalid frontmatter: %w", err)
	}
	return m, nil
}

// ParseMetaFS reads a skill's metadata through an already pinned filesystem.
// dir is a clean slash-relative directory such as "." or "nested/alpha".
func ParseMetaFS(fsys fs.FS, dir string) (Meta, error) {
	raw, err := readSkillFile(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Meta{}, ErrNoSkillFile
		}
		return Meta{}, err
	}
	fm, err := frontmatter(string(raw))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return Meta{}, fmt.Errorf("invalid frontmatter: %w", err)
	}
	return m, nil
}

// frontmatter extracts the YAML block between the leading "---" fences.
//
// A fence is recognised only at column 0. Matching it after TrimSpace ended
// the block at the first "---" line anywhere, including one indented inside a
// YAML block scalar: the description was silently truncated, and in the
// reverse field order the name came back empty and the skill was rejected with
// the misleading "name length 0 out of range 1-64" (round 18 finding M3). A
// fence is a document marker; indented text is content. Only a trailing \r is
// tolerated, so CRLF files still parse.
func frontmatter(s string) (string, error) {
	// A UTF-8 BOM is invisible in every editor, so reporting a missing opening
	// fence for one gives the user nothing to look at.
	s = strings.TrimPrefix(s, "\ufeff")
	lines := strings.Split(s, "\n")
	if !isFrontmatterFence(lines[0]) {
		return "", errors.New("missing frontmatter opening fence")
	}
	for i := 1; i < len(lines); i++ {
		if isFrontmatterFence(lines[i]) {
			return strings.Join(lines[1:i], "\n"), nil
		}
	}
	return "", errors.New("missing frontmatter closing fence")
}

// isFrontmatterFence recognises a document-start marker. YAML treats "---"
// followed by whitespace as a valid marker, and so does every front-matter
// reader -- verified against gopkg.in/yaml.v3, which parses such a document
// while fu reported "missing frontmatter opening fence" for a file whose first
// line is visibly `---`. A trailing double space is a Markdown hard-line-break
// idiom, so it is not far-fetched input. The column-0 rule is unaffected:
// leading whitespace still makes it content, not a marker.
func isFrontmatterFence(line string) bool {
	return strings.TrimRight(strings.TrimSuffix(line, "\r"), " \t") == "---"
}

// ValidateName checks the name field alone (used by `fu new` before any
// SKILL.md exists).
func ValidateName(name string) error {
	if l := utf8.RuneCountInString(name); l < 1 || l > 64 {
		return fmt.Errorf("name length %d out of range 1-64", l)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name %q violates naming rules", name)
	}
	return nil
}

// Validate enforces SPEC rule 7's naming and description constraints --
// name/description well-formedness and the name/dirName match; SKILL.md
// presence is ParseMeta's job. It does NOT implement rule 7's path-safety
// check against symlink escape inside a skill directory: that lives in
// ValidateLinks, which the import pipeline runs against the staged copy
// before it can be published. dirName must equal m.Name per the Agent
// Skills spec.
func Validate(m Meta, dirName string) error {
	if err := ValidateName(m.Name); err != nil {
		return err
	}
	if m.Name != dirName {
		return fmt.Errorf("name %q must match directory name %q", m.Name, dirName)
	}
	// Trimmed before counting (round 6 finding): the untrimmed count let a
	// description of nothing but whitespace satisfy SPEC rule 7's nonempty
	// requirement, and such a description tells an agent nothing about when
	// to use the skill -- which is the whole purpose of the field. The upper
	// bound is measured on the same trimmed text, so surrounding whitespace
	// neither creates nor consumes the budget.
	if l := utf8.RuneCountInString(strings.TrimSpace(m.Description)); l < 1 || l > 1024 {
		return fmt.Errorf("description length %d out of range 1-1024 (leading and trailing whitespace does not count)", l)
	}
	return nil
}

// ValidateSkillDir reads dir/SKILL.md through an already checked fs.FS and
// applies the full rule-7 meta validation against dir's own base name. It
// is the fs-side counterpart of ParseMeta, used by the import pipeline to
// re-validate a candidate through the pinned staging tree (the content that
// will actually be published) rather than reopening a mutable pathname.
func ValidateSkillDir(fsys fs.FS, dir string) error {
	raw, err := readSkillFile(fsys, dir)
	if err != nil {
		return fmt.Errorf("read %s/SKILL.md: %w", dir, err)
	}
	fm, err := frontmatter(string(raw))
	if err != nil {
		return fmt.Errorf("parse %s/SKILL.md: %w", dir, err)
	}
	var m Meta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return fmt.Errorf("invalid frontmatter in %s/SKILL.md: %w", dir, err)
	}
	return Validate(m, path.Base(dir))
}
