package store

import (
	"path/filepath"
	"testing"

	"github.com/cosensexyz/fu/internal/testenv"
)

// requireReusedWithNewHandle is the tripwire every same-inode replacement
// test carries under the required Linux coverage: the replacement must hold
// the original's inode number with a different handle, or the handle path
// was never the discriminator.
func requireReusedWithNewHandle(t *testing.T, before, after FileIdentity, reuse testenv.InodeReuse) {
	t.Helper()
	if !testenv.FileHandlesRequired() {
		return
	}
	if !reuse.Reused || before.Device != after.Device || before.Inode != after.Inode || before.Handle == "" || after.Handle == "" || before.Handle == after.Handle {
		t.Fatalf("replacement coverage did not reuse the inode with a new handle: reuse=%+v before=%+v after=%+v", reuse, before, after)
	}
}

// A logical root replaced on the same inode between Open (which releases its
// descriptors) and BeginWrite must be refused: the identity Open remembered
// carries the handle, and the replacement's handle differs.
func TestBeginWriteRefusesASameInodeReplacementOfALogicalRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(s *Store) string
	}{
		{"staging", func(s *Store) string { return s.StagingDir() }},
		{"recovery", func(s *Store) string { return s.RecoveryDir() }},
		{"skills", func(s *Store) string { return s.SkillsDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if _, err := Init(home); err != nil {
				t.Fatal(err)
			}
			s, err := Open(home)
			if err != nil {
				t.Fatal(err)
			}
			skipUnlessReplacementDetectable(t, home)
			path := tc.path(s)
			before, _, err := entryIdentityAt(unixAtFDCWD, path)
			if err != nil {
				t.Fatal(err)
			}
			reuse, err := testenv.ReplaceOnSameInode(filepath.Dir(path), filepath.Base(path), true)
			if err != nil {
				t.Fatal(err)
			}
			after, _, err := entryIdentityAt(unixAtFDCWD, path)
			if err != nil {
				t.Fatal(err)
			}

			session, err := s.BeginWrite()
			if err == nil {
				_ = session.Close()
				t.Fatalf("BeginWrite must refuse a %s root replaced since Open", tc.name)
			}
			requireReusedWithNewHandle(t, before, after, reuse)
		})
	}
}

// StagingIdentity hands internal/source the validated staging directory; a
// same-inode replacement since Open must not be handed out as validated.
func TestStagingIdentityRefusesASameInodeReplacement(t *testing.T) {
	home := t.TempDir()
	if _, err := Init(home); err != nil {
		t.Fatal(err)
	}
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	skipUnlessReplacementDetectable(t, home)
	before, _, err := entryIdentityAt(unixAtFDCWD, s.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	reuse, err := testenv.ReplaceOnSameInode(home, "staging", true)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := entryIdentityAt(unixAtFDCWD, s.StagingDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.StagingIdentity(); err == nil {
		t.Fatal("a replaced staging directory must not be reported as the validated one")
	}
	requireReusedWithNewHandle(t, before, after, reuse)
}
