package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSourceFieldsAccessors covers the generic scalar-mapping source record
// accessors: set, read, round-trip with unknown scalar keys preserved, and
// removal. Field-name semantics belong to the source package; Config only
// stores and validates the mapping shape (DESIGN §3).
func TestSourceFieldsAccessors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	c := NewConfig(p)
	if err := c.AddSkill("alpha", "sha256:x"); err != nil {
		t.Fatal(err)
	}

	if got := c.SourceFields("alpha"); len(got) != 0 {
		t.Fatalf("no source yet, got %v", got)
	}
	if got := c.SourceFields("ghost"); len(got) != 0 {
		t.Fatalf("unregistered skill must have no source, got %v", got)
	}

	fields := map[string]string{
		"type":     "git",
		"url":      "https://example.com/skills.git",
		"ref":      "refs/heads/main",
		"ref_kind": "branch",
		"commit":   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c",
		"subdir":   "pdf-tools",
		"custom":   "preserved",
	}
	c.SetSourceFields("alpha", fields)
	if got := c.SourceFields("alpha"); !reflect.DeepEqual(got, fields) {
		t.Fatalf("SourceFields = %v, want %v", got, fields)
	}

	// Save/load round trip: the source record survives, including the
	// unknown scalar key.
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.SourceFields("alpha"); !reflect.DeepEqual(got, fields) {
		t.Fatalf("after reload SourceFields = %v, want %v", got, fields)
	}

	// Setting an empty mapping removes the source key entirely.
	loaded.SetSourceFields("alpha", nil)
	if got := loaded.SourceFields("alpha"); len(got) != 0 {
		t.Fatalf("empty set must clear the source record, got %v", got)
	}
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.SourceFields("alpha"); len(got) != 0 {
		t.Fatalf("source must be gone after clearing and reload, got %v", got)
	}
}

// TestSourceFieldsTrueReplacement pins round 13 finding I3: SetSourceFields
// promises a full replacement of the source record, so keys present in the
// existing record but absent from the new field set must be dropped -- a
// partial update (e.g. update rewriting the lock fields) must not leave
// stale keys like an old ref_kind or superseded commit behind.
func TestSourceFieldsTrueReplacement(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	c := NewConfig(p)
	if err := c.AddSkill("alpha", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	c.SetSourceFields("alpha", map[string]string{
		"type": "git", "url": "https://example.com/skills.git",
		"ref": "refs/heads/main", "ref_kind": "branch", "commit": "old",
	})
	c.SetSourceFields("alpha", map[string]string{
		"type": "git", "url": "https://example.com/skills.git", "commit": "new",
	})
	got := c.SourceFields("alpha")
	if len(got) != 3 || got["commit"] != "new" {
		t.Fatalf("partial update must replace the record, got %v", got)
	}
	if _, ok := got["ref"]; ok {
		t.Fatalf("stale ref must be dropped, got %v", got)
	}
	if _, ok := got["ref_kind"]; ok {
		t.Fatalf("stale ref_kind must be dropped, got %v", got)
	}
}

// TestSourceFieldsShapeValidation pins the structural rules validateConfigTree
// enforces on a `source` entry: it must be a mapping of scalar strings.
func TestSourceFieldsShapeValidation(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{"scalar source", "source: git"},
		{"sequence source", "source: [a, b]"},
		{"non-string value", "source:\n  url: 123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "fu.yaml")
			raw := "version: 1\nskills:\n  alpha:\n    enabled: true\n    " + tc.source + "\n"
			if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(p)
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("want a malformed-config error, got %v", err)
			}
		})
	}

	// The valid shape passes and preserves the record.
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := "version: 1\nskills:\n  alpha:\n    enabled: true\n    source:\n      type: git\n      url: https://x/y.git\n"
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("valid source shape rejected: %v", err)
	}
	if got := c.SourceFields("alpha"); got["type"] != "git" || got["url"] != "https://x/y.git" {
		t.Fatalf("SourceFields = %v", got)
	}
}

// TestSourceFieldsDuplicateKeyRejected pins that a duplicated source key is
// refused like every other duplicate mapping key in fu.yaml.
func TestSourceFieldsDuplicateKeyRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	raw := "version: 1\nskills:\n  alpha:\n    enabled: true\n    source:\n      type: git\n      type: local\n"
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatal("duplicate source key must be refused")
	}
}

// TestSourceFieldsYamlTypeSafeValues pins round 14 finding I1: source
// values that YAML would parse as non-strings ("true", "123", "1.0", a
// date) are legitimate git ref/subdir names and must survive the
// save/load round trip as strings -- validateConfigTree requires the
// !!str tag, so an untagged node would serialize them unquoted and the
// next load would reject the whole store.
func TestSourceFieldsYamlTypeSafeValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fu.yaml")
	c := NewConfig(p)
	if err := c.AddSkill("alpha", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{
		"type":   "git",
		"url":    "https://example.com/skills.git",
		"ref":    "true",
		"subdir": "123",
	}
	c.SetSourceFields("alpha", fields)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("a type-safe source value must reload cleanly: %v", err)
	}
	got := loaded.SourceFields("alpha")
	if got["ref"] != "true" || got["subdir"] != "123" {
		t.Fatalf("SourceFields = %v, want string-typed values", got)
	}
}

// TestSourceFieldsShapeValidation pins the structural rules validateConfigTree
// enforces on a `source` entry: it must be a mapping of scalar strings.

// TestConfigYamlTypeSafeSkillNames pins round 15 finding I1: the *key* side
// of the same defect round 14 closed on the value side. Legal skill names
// that YAML would parse as non-strings ("true", "123", "null",
// "2026-01-01", "1e3") are all accepted by ValidateName, so a skill created
// with such a name must survive Save → LoadConfig -- an untagged key node
// serializes them bare and the reload rejects the whole store.
func TestConfigYamlTypeSafeSkillNames(t *testing.T) {
	for _, name := range []string{"true", "false", "123", "null", "2026-01-01", "1e3", "yes", "on", "pdf-tools"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "fu.yaml")
			c := NewConfig(p)
			if err := c.AddSkill(name, "sha256:x"); err != nil {
				t.Fatal(err)
			}
			if err := c.Save(); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(p)
			if err != nil {
				t.Fatalf("a type-safe skill name must reload cleanly: %v", err)
			}
			if !loaded.HasSkill(name) {
				t.Fatalf("skill %q must survive the round trip", name)
			}
		})
	}
}
