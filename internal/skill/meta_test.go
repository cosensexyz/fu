package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
