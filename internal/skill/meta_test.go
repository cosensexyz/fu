package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeSkill(t *testing.T, dir, frontmatter string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n\n# body\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseMeta(t *testing.T) {
	dir := writeSkill(t, filepath.Join(t.TempDir(), "pdf-tools"),
		"name: pdf-tools\ndescription: Handle PDFs.\nlicense: MIT")
	m, err := ParseMeta(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Name != "pdf-tools" || m.Description != "Handle PDFs." {
		t.Fatalf("unexpected meta: %+v", m)
	}
}

func TestParseMetaMissingFile(t *testing.T) {
	if _, err := ParseMeta(t.TempDir()); err != ErrNoSkillFile {
		t.Fatalf("want ErrNoSkillFile, got %v", err)
	}
}

func TestDescribeEntryNamesDirectory(t *testing.T) {
	if got := describeEntry(os.ModeDir); got != "a directory" {
		t.Fatalf("describeEntry(directory) = %q, want %q", got, "a directory")
	}
}

func TestValidate(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		label, name, desc, dir string
		wantErr                bool
	}{
		{"valid", "pdf-tools", "d", "pdf-tools", false},
		{"uppercase", "PDF", "d", "PDF", true},
		{"leading-hyphen", "-pdf", "d", "-pdf", true},
		{"trailing-hyphen", "pdf-", "d", "pdf-", true},
		{"double-hyphen", "pdf--x", "d", "pdf--x", true},
		{"too-long", long, "d", long, true},
		{"dir-mismatch", "pdf-tools", "d", "other", true},
		{"empty-desc", "pdf-tools", "", "pdf-tools", true},
		{"long-desc", "pdf-tools", strings.Repeat("x", 1025), "pdf-tools", true},
		{"empty-name", "", "d", "", true},
		{"max-length-name", strings.Repeat("a", 64), "d", strings.Repeat("a", 64), false},
		{"max-length-desc", "pdf-tools", strings.Repeat("x", 1024), "pdf-tools", false},
		{"chinese-desc-1024", "pdf-tools", strings.Repeat("中", 1024), "pdf-tools", false},
	}
	for _, c := range cases {
		err := Validate(Meta{Name: c.name, Description: c.desc}, c.dir)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.label, err, c.wantErr)
		}
	}
}

// TestValidateRejectsBlankDescription: SPEC rule 7 requires a nonempty
// description, but the length check counted runes without trimming, so a
// description of nothing but spaces satisfied it (round 6 finding). Such a
// skill tells an agent nothing about when to use it, which is the entire
// purpose of the field.
func TestValidateRejectsBlankDescription(t *testing.T) {
	for _, desc := range []string{"", " ", "   ", "\t", "\n", " \t\n ", " "} {
		if err := Validate(Meta{Name: "alpha", Description: desc}, "alpha"); err == nil {
			t.Errorf("a description of %q carries no information and must be refused", desc)
		}
	}
	// A description with real content keeps passing, leading/trailing
	// whitespace and all.
	if err := Validate(Meta{Name: "alpha", Description: "  does a thing  "}, "alpha"); err != nil {
		t.Errorf("a description with actual content must pass: %v", err)
	}
}

// TestParseMetaRejectsOversizedSkillFile pins round 18 finding I8. SKILL.md
// was the only uncapped external read in the codebase: MaxConfigBytes bounds
// the config at 8 MiB, MaxSourceFileBytes bounds projected files at 64 MiB, and
// readlinkAt bounds a link target at 1 MiB, but ParseMeta/ParseMetaFS/
// ValidateSkillDir read SKILL.md whole. ScanFS calls ParseMetaFS for every
// directory in a source tree, and `fu add <git-url>` is precisely the command
// whose input is third-party, so an oversized SKILL.md exhausted memory during
// the scan -- before the copy cap could ever refuse it.
func TestParseMetaRejectsOversizedSkillFile(t *testing.T) {
	dir := t.TempDir()
	oversized := make([]byte, maxSkillFileBytes+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), oversized, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ParseMeta(dir); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("ParseMeta must refuse an oversized SKILL.md, got %v", err)
	}
	if _, err := ParseMetaFS(os.DirFS(dir), "."); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("ParseMetaFS must refuse an oversized SKILL.md, got %v", err)
	}
	if err := ValidateSkillDir(os.DirFS(dir), "."); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("ValidateSkillDir must refuse an oversized SKILL.md, got %v", err)
	}
}

// TestFrontmatterIgnoresFenceInsideBlockScalar pins round 18 finding M3. The
// closing fence was matched after TrimSpace, so a "---" line indented inside a
// YAML block scalar ended the frontmatter early: the description was silently
// truncated, and in the reverse field order the name came back empty and the
// skill was rejected with the misleading "name length 0 out of range 1-64".
// A fence is a document marker and only counts at column 0.
func TestFrontmatterIgnoresFenceInsideBlockScalar(t *testing.T) {
	raw := "---\ndescription: |\n  first line\n  ---\n  after the inner fence\nname: alpha\n---\n\nbody\n"
	fm, err := frontmatter(raw)
	if err != nil {
		t.Fatalf("frontmatter: %v", err)
	}
	m := Meta{}
	if err := yamlUnmarshalForTest(fm, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Name != "alpha" {
		t.Fatalf("name = %q; the block scalar's inner fence must not end the frontmatter", m.Name)
	}
	if !strings.Contains(m.Description, "after the inner fence") {
		t.Fatalf("description was truncated at the inner fence: %q", m.Description)
	}
}

// TestFrontmatterAcceptsByteOrderMark: a UTF-8 BOM is invisible in every
// editor, and rejecting the file with "missing frontmatter opening fence"
// gives the user nothing to look at (round 18 finding M3).
func TestFrontmatterAcceptsByteOrderMark(t *testing.T) {
	raw := "\ufeff---\nname: alpha\ndescription: d\n---\n"
	if _, err := frontmatter(raw); err != nil {
		t.Fatalf("a leading UTF-8 BOM must not hide the opening fence: %v", err)
	}
}

func yamlUnmarshalForTest(fm string, m *Meta) error {
	return yaml.Unmarshal([]byte(fm), m)
}

// TestFrontmatterFenceToleratesTrailingWhitespace pins the marker rule against
// YAML's own. `---` followed by spaces or tabs is a valid document-start
// marker, and gopkg.in/yaml.v3 parses such a document; fu refused it and
// reported a missing opening fence for a file whose first line is visibly
// `---`. A trailing double space is a Markdown hard-line-break idiom, so this
// is ordinary input, not a contrived one.
func TestFrontmatterFenceToleratesTrailingWhitespace(t *testing.T) {
	for _, raw := range []string{
		"---  \nname: a\ndescription: d\n---\n",
		"---\t\nname: a\ndescription: d\n---\n",
		"---\nname: a\ndescription: d\n---  \n",
		"---  \r\nname: a\ndescription: d\n---  \r\n",
	} {
		fm, err := frontmatter(raw)
		if err != nil {
			t.Fatalf("frontmatter(%q) = %v; a fence with trailing whitespace is still a fence", raw, err)
		}
		if !strings.Contains(fm, "name: a") {
			t.Fatalf("frontmatter(%q) = %q; want the block between the fences", raw, fm)
		}
	}
	// Leading whitespace is still content, not a marker: the column-0 rule is
	// what keeps a `---` inside a block scalar from ending the block.
	if _, err := frontmatter("  ---\nname: a\n---\n"); err == nil {
		t.Fatal("an indented fence must still be content, not a marker")
	}
}
