package skill

import (
	"errors"
	"fmt"
	"os"
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

// nameRe encodes the Agent Skills naming rules: lowercase alphanumerics
// and single hyphens, no leading/trailing/consecutive hyphens.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// describeEntry names a filesystem entry kind in prose. A raw fs.FileMode
// renders as "L---------", which is accurate and unreadable.
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
	case mode.IsRegular():
		return "a file"
	default:
		return "not a directory"
	}
}

// ParseMeta reads dir/SKILL.md and returns its frontmatter.
//
// dir must be a real directory, checked with Lstat before it is opened, and
// SKILL.md is read through an os.Root anchored there. Pathname-based reading
// followed a symlink at either component out of the store: with
// store/skills/<name> replaced by a link, `fu show <name>` printed content from
// outside the store and exited 0. SPEC rule 7's full path-safety check is still
// future work, but a read command resolving a store-internal link outward is
// not something that can wait for it.
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
	raw, err := root.ReadFile("SKILL.md")
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

// frontmatter extracts the YAML block between the leading "---" fences.
func frontmatter(s string) (string, error) {
	lines := strings.Split(s, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", errors.New("missing frontmatter opening fence")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), nil
		}
	}
	return "", errors.New("missing frontmatter closing fence")
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
// check against symlink escape inside a skill directory: no command that
// imports external content (add, adopt) ships in this plan, so nothing
// exercises it yet, but it must be added before one does (DESIGN §6
// records this as a known gap). dirName must equal m.Name per the Agent
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
