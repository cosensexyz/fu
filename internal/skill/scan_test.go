package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanMultiSkillRepo(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "alpha"), "name: alpha\ndescription: d")
	writeSkill(t, filepath.Join(root, "nested", "beta"), "name: beta\ndescription: d")
	// invalid: name mismatch with directory
	writeSkill(t, filepath.Join(root, "gamma"), "name: wrong\ndescription: d")
	// .git must be skipped even if it contains a SKILL.md-like file
	writeSkill(t, filepath.Join(root, ".git", "fake"), "name: fake\ndescription: d")

	valid, invalid, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 2 {
		t.Fatalf("want 2 valid, got %d: %+v", len(valid), valid)
	}
	if len(invalid) != 1 {
		t.Fatalf("want 1 invalid, got %d: %v", len(invalid), invalid)
	}
}

func TestScanRootIsSkill(t *testing.T) {
	root := writeSkill(t, filepath.Join(t.TempDir(), "solo"), "name: solo\ndescription: d")
	valid, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 || valid[0].Meta.Name != "solo" {
		t.Fatalf("want solo, got %+v", valid)
	}
}

// TestScanSkillNotDescendedInto guards the filepath.SkipDir call after a
// valid candidate is recorded: a skill nested directly inside another
// valid skill must not itself be reported. Unlike the nested/beta case in
// TestScanMultiSkillRepo (which nests through a plain, non-skill
// directory), this nests a second SKILL.md directly under an
// already-valid skill directory, so deleting the SkipDir line would make
// this test fail.
func TestScanSkillNotDescendedInto(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "outer"), "name: outer\ndescription: d")
	writeSkill(t, filepath.Join(root, "outer", "inner"), "name: inner\ndescription: d")

	valid, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 || valid[0].Meta.Name != "outer" {
		t.Fatalf("want only outer, got %+v", valid)
	}
}

// TestScanRelativeRootYieldsAbsoluteDir checks that Candidate.Dir is
// absolute (per its documented contract) even when the caller passes a
// relative root.
func TestScanRelativeRootYieldsAbsoluteDir(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "alpha"), "name: alpha\ndescription: d")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	valid, _, err := Scan(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 {
		t.Fatalf("want 1 valid, got %d: %+v", len(valid), valid)
	}
	if !filepath.IsAbs(valid[0].Dir) {
		t.Fatalf("want absolute Dir for a relative root, got %q", valid[0].Dir)
	}
}

// TestScanMissingRootIsFatal checks that an error on root itself (as
// opposed to a descendant) still aborts the scan and is returned as err.
func TestScanMissingRootIsFatal(t *testing.T) {
	_, _, err := Scan(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("want non-nil err for a missing root")
	}
}

// TestScanRootNotDirectoryIsFatal checks the other root-is-unusable case
// named by the finding: root exists but is a plain file. filepath.WalkDir
// does not surface this as a werr (it just visits the single file and
// returns nil), so Scan must reject it explicitly.
func TestScanRootNotDirectoryIsFatal(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Scan(f); err == nil {
		t.Fatal("want non-nil err when root is not a directory")
	}
}

// TestScanUnreadableDirIsIsolated is the scenario from the review finding:
// a directory made fully inaccessible (0o000, no read/write/execute)
// elsewhere in the tree must not abort the whole scan.
//
// Note: with 0o000, the failure actually surfaces through ParseMeta, not
// through the walk callback's error parameter. Opening "bad/SKILL.md"
// needs execute permission on "bad" to even look up that name, and 0o000
// removes it, so os.ReadFile fails with "permission denied" -- a
// different error from ErrNoSkillFile -- and Scan's existing
// perr-handling branch already records it as invalid and skips it, before
// filepath.WalkDir ever attempts to read the directory's contents. So
// this case was already handled correctly even before this fix; it is
// kept because the review explicitly asked for it, and because it does
// verify the end-to-end contract (valid skills elsewhere are still
// found, the bad path is reported, err is nil). See
// TestScanUnreadableDescendantWalkErrorIsIsolated below for a scenario
// that actually exercises the walk callback's werr handling that this
// fix changes.
func TestScanUnreadableDirIsIsolated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "good"), "name: good\ndescription: d")

	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bad, 0o755) })

	valid, invalid, err := Scan(root)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if len(valid) != 1 || valid[0].Meta.Name != "good" {
		t.Fatalf("want good still found, got %+v", valid)
	}
	if _, ok := invalid[bad]; !ok {
		t.Fatalf("want %q recorded in invalid, got %v", bad, invalid)
	}
}

// TestScanUnreadableDescendantWalkErrorIsIsolated exercises the actual
// mechanism finding 1 is about: filepath.WalkDir's error-carrying second
// callback invocation for a directory it failed to read.
//
// "bad" is given execute-but-not-read permission (0o111): looking up a
// specific known filename (what ParseMeta does for SKILL.md) still
// resolves as "not found" via ErrNoSkillFile, so Scan decides to descend
// into "bad" -- but then enumerating "bad" via ReadDir fails with
// permission denied, and filepath.WalkDir reports that failure by
// invoking the walk callback a second time with a non-nil err for the
// same path. Before this fix, Scan propagated that error unconditionally
// and aborted the entire scan (verified: valid came back empty, "good"
// was lost, even though it does not live under "bad").
func TestScanUnreadableDescendantWalkErrorIsIsolated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "good"), "name: good\ndescription: d")

	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bad, 0o755) })

	valid, invalid, err := Scan(root)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if len(valid) != 1 || valid[0].Meta.Name != "good" {
		t.Fatalf("want good still found, got %+v", valid)
	}
	if _, ok := invalid[bad]; !ok {
		t.Fatalf("want %q recorded in invalid, got %v", bad, invalid)
	}
}
