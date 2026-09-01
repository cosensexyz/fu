package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exchangeSessionFixture opens a fresh store and pins its write session, the
// same way every other checked-root test in this package does (see
// ownedRecoveryFixture): ExchangeStagedWithSkillOwned reads staged and
// published content through the descriptors BeginWrite pins, so a test store
// needs a live write session before it can exercise it.
func exchangeSessionFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session.Store
}

// exchangeFixture stages two distinct trees under the same name and returns
// their manifests.
func exchangeFixture(t *testing.T, s *Store, name string) (staged, published OwnedTree) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.SkillsDir(), name, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), name, "old", "f.txt"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.StagingDir(), name, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.StagingDir(), name, "f.txt"), []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Nested on the staged side too, not just the published side above: the
	// property under test is that RENAME_SWAP/RENAME_EXCHANGE moves whole
	// trees in both directions, and only one direction was pinned before.
	if err := os.WriteFile(filepath.Join(s.StagingDir(), name, "new", "g.txt"), []byte("NEW-NESTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	published, err := s.SnapshotSkillPayload(name)
	if err != nil {
		t.Fatal(err)
	}
	staged, err = s.SnapshotStagedPayload(name)
	if err != nil {
		t.Fatal(err)
	}
	return staged, published
}

// The whole tree moves, not just its top level. This pins the property the
// design rests on: RENAME_SWAP exchanges directories entire.
func TestExchangeStagedWithSkillOwnedSwapsWholeTrees(t *testing.T) {
	s := exchangeSessionFixture(t)
	staged, published := exchangeFixture(t, s, "kit")

	if err := s.ExchangeStagedWithSkillOwned("kit", staged, published); err != nil {
		t.Fatalf("ExchangeStagedWithSkillOwned: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "f.txt"))
	if err != nil || string(got) != "NEW" {
		t.Fatalf("skills side = %q (err %v), want NEW", got, err)
	}
	gotNested, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "new", "g.txt"))
	if err != nil || string(gotNested) != "NEW-NESTED" {
		t.Fatalf("skills side nested staged content = %q (err %v), want the nested NEW-NESTED tree", gotNested, err)
	}
	if _, err := os.Stat(filepath.Join(s.SkillsDir(), "kit", "old")); !os.IsNotExist(err) {
		t.Fatalf("skills/kit/old must be gone after an exchange, not merged or copied alongside the staged content (stat err %v)", err)
	}
	back, err := os.ReadFile(filepath.Join(s.StagingDir(), "kit", "old", "f.txt"))
	if err != nil || string(back) != "OLD" {
		t.Fatalf("staging side = %q (err %v), want the nested OLD tree", back, err)
	}
}

// A manifest that no longer describes what is on disk means something outside
// fu changed it; the exchange must refuse and leave both sides alone.
func TestExchangeStagedWithSkillOwnedRefusesWhenThePublishedSideChanged(t *testing.T) {
	s := exchangeSessionFixture(t)
	staged, published := exchangeFixture(t, s, "kit")

	// An outside writer replaces the published content after the snapshot.
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "kit", "old", "f.txt"), []byte("FOREIGN"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.ExchangeStagedWithSkillOwned("kit", staged, published); err == nil {
		t.Fatal("expected a refusal when the published manifest no longer matches")
	}
	got, err := os.ReadFile(filepath.Join(s.StagingDir(), "kit", "f.txt"))
	if err != nil || string(got) != "NEW" {
		t.Fatalf("staging must be untouched by a refusal: %q (err %v)", got, err)
	}
	foreign, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "old", "f.txt"))
	if err != nil || string(foreign) != "FOREIGN" {
		t.Fatalf("skills must be untouched by a refusal: %q (err %v)", foreign, err)
	}
}

func TestExchangeStagedWithSkillOwnedRefusesWhenTheStagedSideChanged(t *testing.T) {
	s := exchangeSessionFixture(t)
	staged, published := exchangeFixture(t, s, "kit")

	if err := os.WriteFile(filepath.Join(s.StagingDir(), "kit", "f.txt"), []byte("FOREIGN"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.ExchangeStagedWithSkillOwned("kit", staged, published); err == nil {
		t.Fatal("expected a refusal when the staged manifest no longer matches")
	}
	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "kit", "old", "f.txt"))
	if err != nil || string(got) != "OLD" {
		t.Fatalf("skills must be untouched by a refusal: %q (err %v)", got, err)
	}
	// Both sides, like the published-side sibling above: "leave both sides
	// alone" is the promise, and checking only the far side would miss a
	// refusal that had already begun moving the near one.
	foreign, err := os.ReadFile(filepath.Join(s.StagingDir(), "kit", "f.txt"))
	if err != nil || string(foreign) != "FOREIGN" {
		t.Fatalf("staging must be untouched by a refusal: %q (err %v)", foreign, err)
	}
}

// TestExchangeStagedWithSkillOwnedRejectsReservedName is this primitive's
// share of the discipline every other owned-tree write already carries (see
// TestOwnedPublishSurfacesReserveFuNamespace in copy_tree_test.go and
// TestRecoveryPayloadSettledRejectsReservedName in ownedtree_test.go): gc and
// recovery can call a write with a name taken straight off a completed
// journal family's Name field, which nothing on the prune path validates
// against the public grammar, so this primitive must refuse a .fu- name
// itself rather than trust a caller to have filtered it first. Both sides
// hold real, mutually consistent content at the reserved name -- the guard
// must fire on the name, not on a manifest mismatch it would never see.
func TestExchangeStagedWithSkillOwnedRejectsReservedName(t *testing.T) {
	s := exchangeSessionFixture(t)
	const reserved = ".fu-attacker"
	staged, published := exchangeFixture(t, s, reserved)

	if err := s.ExchangeStagedWithSkillOwned(reserved, staged, published); err == nil || !strings.Contains(err.Error(), "public single-component name") {
		t.Fatalf("reserved name %q must be refused, got err=%v", reserved, err)
	}
}

// TestRenameExchangeRequiresBothSidesToExist pins the syscall property design
// §4.2 states and requires be pinned ("该性质要落成回归测试，不能只停留在一次性
// 验证"): RENAME_SWAP/RENAME_EXCHANGE fails when either name is missing and
// never creates the absent side. Fix round 2, Important #8 found it unpinned.
//
// It calls renameExchange rather than ExchangeStagedWithSkillOwned because
// that wrapper's own validation refuses a vanished side first, so the
// property is unobservable through it -- which is exactly why nothing here
// covered it. The whole "no absent-skill window" argument
// (ExchangeStagedWithSkillOwned's doc, update's recovery in
// internal/engine/update_txn.go) rests on this: an exchange that created
// instead of failing would leave update's rollback exchanging a freshly
// invented empty directory back over the user's content.
func TestRenameExchangeRequiresBothSidesToExist(t *testing.T) {
	for _, tc := range []struct {
		label            string
		skillName        string
		stagingName      string
		absoluteAbsent   string
		survivingContent string
	}{
		{
			label: "published side missing", skillName: "absent", stagingName: "kit",
			absoluteAbsent: "skills", survivingContent: "staging",
		},
		{
			label: "staged side missing", skillName: "kit", stagingName: "absent",
			absoluteAbsent: "staging", survivingContent: "skills",
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			s := exchangeSessionFixture(t)
			exchangeFixture(t, s, "kit")

			err := renameExchange(
				int(s.writeRoots.skills.dir.Fd()), tc.skillName,
				int(s.writeRoots.staging.dir.Fd()), tc.stagingName)
			if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("an exchange with a missing side must fail with ENOENT, got %v", err)
			}

			// Nothing was invented at the absent name.
			absent := filepath.Join(s.SkillsDir(), "absent")
			if tc.absoluteAbsent == "staging" {
				absent = filepath.Join(s.StagingDir(), "absent")
			}
			if _, err := os.Lstat(absent); !os.IsNotExist(err) {
				t.Fatalf("a failed exchange must create nothing at %s (stat err %v)", absent, err)
			}
			// And the side that did exist is untouched.
			want, path := "OLD", filepath.Join(s.SkillsDir(), "kit", "old", "f.txt")
			if tc.survivingContent == "staging" {
				want, path = "NEW", filepath.Join(s.StagingDir(), "kit", "f.txt")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != want {
				t.Fatalf("the existing side must be untouched: %s = %q (err %v), want %q", path, got, err, want)
			}
		})
	}
}
