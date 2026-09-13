package engine

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/cosensexyz/fu/internal/store"
)

func indexBlobHashes(t *testing.T, s *store.Store) map[string]plumbing.Hash {
	t.Helper()
	idx, err := s.Repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]plumbing.Hash{}
	for _, e := range idx.Entries {
		out[e.Name] = e.Hash
	}
	return out
}

func blobOf(content string) plumbing.Hash {
	return plumbing.ComputeHash(plumbing.BlobObject, []byte(content))
}

// A named commit takes the rest of the tree from HEAD, so paths the user
// staged outside the skill with direct git are neither recorded by it nor
// disturbed in the index (SPEC §5.1). The refusal this replaces named those
// paths and asked the user to unstage them first.
func TestCommitNamedRecordsDespiteStagedPathsOutsideTheSkill(t *testing.T) {
	s, _ := setupStore(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := NewSkill(s, nil, name); err != nil {
			t.Fatal(err)
		}
	}
	committedConfig, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	stagedConfig := string(committedConfig) + "# hand note\n"
	if err := os.WriteFile(s.ConfigPath(), []byte(stagedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	stageInStore(t, s, "fu.yaml")
	editSkillFile(t, s, "beta", "SKILL.md", "beta staged")
	stageInStore(t, s, "skills/beta/SKILL.md")
	editSkillFile(t, s, "alpha", "SKILL.md", "alpha edited")

	outcome, err := CommitOperations(s, nil, CommitScope{Name: "alpha"})
	if err != nil {
		t.Fatalf("staged paths outside the skill must not refuse the commit: %v", err)
	}
	if !outcome.Written || outcome.Subject != "commit: alpha" || !slices.Equal(outcome.Changed, []string{"skills/alpha/SKILL.md"}) {
		t.Fatalf("outcome = %+v, want alpha alone recorded", outcome)
	}
	if len(outcome.Result.Warnings) != 0 {
		t.Fatalf("no warning is due when the index was refreshed: %v", outcome.Result.Warnings)
	}
	if got := commitFileAt(t, s, 0, "fu.yaml"); got != string(committedConfig) {
		t.Fatalf("HEAD fu.yaml = %q, want the previously committed bytes", got)
	}
	if got := commitFileAt(t, s, 0, "skills/beta/SKILL.md"); got == "beta staged" {
		t.Fatal("beta's staged edit must not enter alpha's commit")
	}
	index := indexBlobHashes(t, s)
	if index["fu.yaml"] != blobOf(stagedConfig) || index["skills/beta/SKILL.md"] != blobOf("beta staged") {
		t.Fatalf("staged entries outside the skill must survive in the index: %v", index)
	}
	if index["skills/alpha/SKILL.md"] != blobOf("alpha edited") {
		t.Fatalf("alpha's entry must be refreshed to the committed blob: %v", index)
	}
}

// fu.yaml is read once, before the candidate is prepared, and the closing
// reconcile acts on it; a change arriving in between is refused before
// anything is published rather than acted on from a stale model. On the
// store-wide shape the external snapshot may already have landed by then,
// and is reported as such.
func TestCommitRefusesWhenConfigChangesAfterLoad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scope  CommitScope
		stage  bool
		before int
	}{
		{name: "named", scope: CommitScope{Name: "alpha"}},
		{name: "store-wide with a staged snapshot", scope: CommitScope{}, stage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupStore(t)
			if _, err := NewSkill(s, nil, "alpha"); err != nil {
				t.Fatal(err)
			}
			editSkillFile(t, s, "alpha", "SKILL.md", "alpha edited")
			if tc.stage {
				stageInStore(t, s, "skills/alpha/SKILL.md")
				editSkillFile(t, s, "alpha", "SKILL.md", "alpha edited again")
			}
			before := logMessages(t, s, 10)

			outcome, err := commitOperationsWithHooks(s, nil, tc.scope, hooks{afterCommitPrepare: func() error {
				raw, err := os.ReadFile(s.ConfigPath())
				if err != nil {
					return err
				}
				return os.WriteFile(s.ConfigPath(), append(raw, []byte("# edited meanwhile\n")...), 0o644)
			}})
			if !errors.Is(err, ErrConcurrentStoreChange) {
				t.Fatalf("err = %v, want ErrConcurrentStoreChange", err)
			}
			if outcome.Written {
				t.Fatalf("nothing may be published from a stale model: %+v", outcome)
			}
			after := logMessages(t, s, 10)
			if tc.stage {
				if !outcome.ExternalWritten || len(after) != len(before)+1 || after[0] != ExternalCommitMessage {
					t.Fatalf("the external snapshot that landed first must be reported: outcome=%+v log=%v", outcome, after)
				}
			} else if outcome.ExternalWritten || !slices.Equal(after, before) {
				t.Fatalf("a refused named commit must leave history alone: outcome=%+v log=%v", outcome, after)
			}
		})
	}
}

// When the skill's own index entries were restaged by direct git while fu
// was committing, the commit stands, the index is left alone, and the
// outcome warns so the user knows why `fu status` still lists the skill.
func TestCommitWarnsWhenTheIndexWasNotRefreshed(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	editSkillFile(t, s, "alpha", "SKILL.md", "alpha edited")

	outcome, err := commitOperationsWithHooks(s, nil, CommitScope{Name: "alpha"}, hooks{afterCommitPrepare: func() error {
		editSkillFile(t, s, "alpha", "SKILL.md", "alpha staged mid-commit")
		stageInStore(t, s, "skills/alpha/SKILL.md")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Written {
		t.Fatalf("the commit must stand: %+v", outcome)
	}
	if got := commitFileAt(t, s, 0, "skills/alpha/SKILL.md"); got != "alpha edited" {
		t.Fatalf("HEAD holds %q, want the frozen candidate", got)
	}
	if index := indexBlobHashes(t, s); index["skills/alpha/SKILL.md"] != blobOf("alpha staged mid-commit") {
		t.Fatalf("the concurrently staged entry must not be overwritten: %v", index)
	}
	joined := strings.Join(outcome.Result.Warnings, "\n")
	if !strings.Contains(joined, "index") || !strings.Contains(joined, "fu status") {
		t.Fatalf("the outcome must warn that the index was not refreshed and how it shows: %q", joined)
	}
}

// The no-op site warns too: a candidate whose tree equals HEAD still installs
// the index, and when that install is skipped the user hears why.
func TestCommitWarnsAtTheNoOpSiteWhenTheIndexWasNotRefreshed(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	committed := commitFileAt(t, s, 0, "skills/alpha/SKILL.md")
	// Stage a draft, then put the worktree back: the tree does not move.
	editSkillFile(t, s, "alpha", "SKILL.md", "draft")
	stageInStore(t, s, "skills/alpha/SKILL.md")
	editSkillFile(t, s, "alpha", "SKILL.md", committed)
	before := logMessages(t, s, 10)

	outcome, err := commitOperationsWithHooks(s, nil, CommitScope{Name: "alpha"}, hooks{afterCommitPrepare: func() error {
		editSkillFile(t, s, "alpha", "SKILL.md", "another draft")
		stageInStore(t, s, "skills/alpha/SKILL.md")
		editSkillFile(t, s, "alpha", "SKILL.md", committed)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Written || !slices.Equal(logMessages(t, s, 10), before) {
		t.Fatalf("no commit may be written when the tree does not move: %+v", outcome)
	}
	joined := strings.Join(outcome.Result.Warnings, "\n")
	if !strings.Contains(joined, "index") || !strings.Contains(joined, "fu status") {
		t.Fatalf("the no-op site must warn that the index was not refreshed: %q", joined)
	}
	if index := indexBlobHashes(t, s); index["skills/alpha/SKILL.md"] != blobOf("another draft") {
		t.Fatalf("the concurrently staged draft must not be overwritten: %v", index)
	}
}
