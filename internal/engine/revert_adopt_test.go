// internal/engine/revert_adopt_test.go
package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// Reverting an adopt does not put back what adopt displaced, and the command
// must say so.
//
// adopt is the only operation that moves pre-existing user content aside: it
// archives the original under recovery/ and leaves a link into the store.
// Reverting it drops the store copy and reconcile removes the link, so the
// agent directory ends up *empty* -- the user is left worse off than before
// the adopt, with their content reachable only by hand.
//
// SPEC scenario 5 points at `fu revert` as the remedy for a committed mistake,
// and adopting the wrong directory is exactly such a mistake, so this is the
// moment the user most needs to be told where their files went. Reverting a
// `new` or an `add` has no such cost: nothing was displaced, and removing the
// link is the whole of the undo.
func TestRevertReportsTheAdoptsItCannotPutBack(t *testing.T) {
	_, st, dirs := scenarioEnv(t)
	claudeDir := dirs["claude"]
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: d\n---\n")

	if _, err := Adopt(st, agent.Detected(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(claudeDir, "notes")); err != nil {
		t.Fatalf("adopt must leave an entry behind: %v", err)
	}

	outcome, err := RevertOperations(st, agent.Detected(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// The behaviour this reports, pinned so the report cannot drift from it.
	if _, err := os.Lstat(filepath.Join(claudeDir, "notes")); !os.IsNotExist(err) {
		t.Fatalf("reverting an adopt currently empties the agent entry; if that changed, this report must change with it: %v", err)
	}
	if len(outcome.DisplacedByRevertedAdopt) != 1 || outcome.DisplacedByRevertedAdopt[0] != "notes" {
		t.Fatalf("revert must name the adopt it undid without restoring: %+v", outcome.DisplacedByRevertedAdopt)
	}
}

// A revert that undid no adopt says nothing, or the warning becomes noise
// attached to every revert and stops being read.
func TestRevertIsSilentWhenNoAdoptWasUndone(t *testing.T) {
	_, st, _ := scenarioEnv(t)
	if _, err := NewSkill(st, agent.Detected(), "writer"); err != nil {
		t.Fatal(err)
	}
	outcome, err := RevertOperations(st, agent.Detected(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.DisplacedByRevertedAdopt) != 0 {
		t.Fatalf("reverting a `new` displaced nothing and must report nothing: %+v", outcome.DisplacedByRevertedAdopt)
	}
}

// A commit fu wrote on its own account must not consume one of the n slots.
//
// `fu revert n` counts *operations*; a sweep, an `init: store` and a recovery
// compensation all carry Ordinal 0 and are stepped over. Reading the window as
// n *commits* instead puts the boundary in a different place, and the first
// thing that falls out of it is the adopt this warning exists for -- a silent
// false negative, which is the harmful direction: the user is told nothing and
// their content sits in recovery/ unmentioned.
//
// Two shapes, both ordinary. A sweep from a hand edit this very call records
// before reverting, and a sweep an earlier command already left in history.
func TestRevertFindsTheAdoptBehindASweepCommit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		// setup runs after the adopt and before the revert.
		setup func(t *testing.T, st *store.Store, agents []agent.Agent)
	}{
		{"a sweep this call records", 1, func(t *testing.T, st *store.Store, _ []agent.Agent) {
			// Uncommitted when revert runs, so revert's own sweep commits it
			// and it lands between HEAD and the adopt.
			if err := os.WriteFile(filepath.Join(st.SkillsDir(), "notes", "extra.md"), []byte("hand edited"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a sweep an earlier command left", 2, func(t *testing.T, st *store.Store, agents []agent.Agent) {
			if err := os.WriteFile(filepath.Join(st.SkillsDir(), "notes", "extra.md"), []byte("hand edited"), 0o644); err != nil {
				t.Fatal(err)
			}
			// This command's prologue sweeps the edit into its own commit,
			// then adds an operation of its own above it.
			if _, err := NewSkill(st, agents, "writer"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st, dirs := scenarioEnv(t)
			claudeDir := dirs["claude"]
			if err := os.MkdirAll(claudeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: d\n---\n")
			agents := agent.Detected()
			if _, err := Adopt(st, agents, ""); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, st, agents)

			outcome, err := RevertOperations(st, agents, tc.count)
			if err != nil {
				t.Fatal(err)
			}
			// The adopt really is among what was undone.
			if _, err := os.Lstat(filepath.Join(claudeDir, "notes")); !os.IsNotExist(err) {
				t.Fatalf("precondition: the adopt must have been reverted, got %v", err)
			}
			if len(outcome.DisplacedByRevertedAdopt) != 1 || outcome.DisplacedByRevertedAdopt[0] != "notes" {
				t.Fatalf("a commit fu wrote on its own account pushed the adopt out of the window: %+v",
					outcome.DisplacedByRevertedAdopt)
			}
		})
	}
}

// An adopt one operation beyond the window must not be named. The window's
// two edges fail differently and only one of them was held: an adopt inside it
// that goes unnamed is a user left hunting, and an adopt outside it that gets
// named is a user told to restore something still in place.
func TestRevertDoesNotNameAnAdoptBeyondTheWindow(t *testing.T) {
	_, st, dirs := scenarioEnv(t)
	claudeDir := dirs["claude"]
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: d\n---\n")
	agents := agent.Detected()
	if _, err := Adopt(st, agents, ""); err != nil {
		t.Fatal(err)
	}
	// One operation above the adopt, so `revert 1` stops short of it.
	if _, err := NewSkill(st, agents, "writer"); err != nil {
		t.Fatal(err)
	}

	outcome, err := RevertOperations(st, agents, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.DisplacedByRevertedAdopt) != 0 {
		t.Fatalf("the adopt sits at ordinal 2 and revert 1 does not reach it: %+v",
			outcome.DisplacedByRevertedAdopt)
	}
	// And it really is untouched, which is what makes naming it wrong.
	if _, err := os.Lstat(filepath.Join(claudeDir, "notes")); err != nil {
		t.Fatalf("precondition: the adopt must still be in place: %v", err)
	}
}

// A refused revert undid nothing, so it must name nothing.
//
// The names are read before the revert runs. Reporting them after a refusal
// tells a user their adopted skill was rolled back and should be copied out of
// recovery/ by hand, while the link and the store copy are both still there --
// following that advice overwrites fu's own link.
func TestRevertNamesNothingWhenItRefusedToAct(t *testing.T) {
	_, st, dirs := scenarioEnv(t)
	claudeDir := dirs["claude"]
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: d\n---\n")
	agents := agent.Detected()
	if _, err := Adopt(st, agents, ""); err != nil {
		t.Fatal(err)
	}

	// Further back than the history goes.
	outcome, err := RevertOperations(st, agents, 5)
	if err == nil {
		t.Fatal("reverting past the start of history must fail")
	}
	if len(outcome.DisplacedByRevertedAdopt) != 0 {
		t.Fatalf("a refused revert undid nothing and must name nothing: %+v",
			outcome.DisplacedByRevertedAdopt)
	}
	if _, err := os.Lstat(filepath.Join(claudeDir, "notes")); err != nil {
		t.Fatalf("the adopt must be untouched after a refused revert: %v", err)
	}
}

// A cancelled adopt must not be named: it was never an operation, so the
// revert never counted it and never undid it.
//
// This is what makes the Ordinal == 0 skip load-bearing, and it is the case
// that disproves the reasoning offered when that skip was first written. A
// sweep carries Ordinal 0 *and* fails the "adopt: " subject match, so dropping
// the skip costs nothing there. A recovery compensation is different: it gives
// the interrupted operation it cancels Ordinal 0 while that commit keeps its
// original subject, so the prefix still matches. Without the skip, fu names an
// adopt that was rolled back rather than reverted -- and points the user at an
// archive for content that is already back where it belongs.
func TestRevertDoesNotNameAnAdoptThatWasCancelledNotReverted(t *testing.T) {
	_, st, dirs := scenarioEnv(t)
	claudeDir := dirs["claude"]
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: d\n---\n")
	agents := agent.Detected()
	if _, err := Adopt(st, agents, ""); err != nil {
		t.Fatal(err)
	}

	// The compensation that cancels it, written the way a recovery pass
	// writes one: the prefix plus the cancelled commit's own subject, and no
	// trailing newline, which is the shape fu's own messages carry.
	repo, err := git.PlainOpen(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit(store.RecoveryCompensationPrefix+"adopt: notes",
		&git.CommitOptions{AllowEmptyCommits: true, Author: &object.Signature{Name: "t", Email: "t@t"}}); err != nil {
		t.Fatal(err)
	}
	// One real operation above the pair, which is what `revert 1` undoes.
	if _, err := NewSkill(st, agents, "writer"); err != nil {
		t.Fatal(err)
	}

	outcome, err := RevertOperations(st, agents, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.DisplacedByRevertedAdopt) != 0 {
		t.Fatalf("a cancelled adopt is not an operation and this revert did not reach it: %+v",
			outcome.DisplacedByRevertedAdopt)
	}
}

// A revert that converges part of the way and then fails names only the part
// it got through.
//
// This is the path the whole gate exists for and the one nothing covered: the
// apply reports the paths it did move alongside its error, so a revert of two
// adopts that fails on the second leaves one genuinely undone and one entirely
// untouched. Keeping both is the same false statement a refused revert used to
// make -- a user told to copy an archive over a skill whose link and store copy
// are still live -- reached through a mid-apply failure instead of a refusal.
//
// The failure is injected by making one skill's directory unwritable, so its
// own file cannot be unlinked. The deletion pass walks names in reverse, so
// the alphabetically-first skill is the one that fails, after the other has
// already been removed.
//
// The two names are `note` and `notes` on purpose: one is a prefix of the
// other, so a rule that matched skills/<name> without the trailing separator
// would keep the untouched `note` on the strength of `notes` having moved.
func TestRevertNamesOnlyTheAdoptsItActuallyMoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not refused the unlink")
	}
	_, st, dirs := scenarioEnv(t)
	claudeDir := dirs["claude"]
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"note", "notes"} {
		writeSkillTree(t, claudeDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	agents := agent.Detected()
	if _, err := Adopt(st, agents, ""); err != nil {
		t.Fatal(err)
	}

	stuck := filepath.Join(st.SkillsDir(), "note")
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	outcome, err := RevertOperations(st, agents, 2)
	if err == nil {
		t.Fatal("the unlink of an unwritable directory's file must fail the revert")
	}
	// Precondition: the run really did stop halfway.
	// Fatal, not Skip: the deletion pass walks names in reverse, which is
	// fu's own documented order rather than a platform property, so a run that
	// does not reach this state is a change to that order and not an
	// environment fu has to tolerate. A skip here would leave the test passing
	// while distinguishing nothing -- with no partial state, every candidate
	// rule gives the same answer.
	if _, statErr := os.Stat(filepath.Join(st.SkillsDir(), "notes")); !os.IsNotExist(statErr) {
		t.Fatalf("the deletion pass must remove notes before failing on note; got %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(stuck, "SKILL.md")); statErr != nil {
		t.Fatalf("precondition: note must be untouched, got %v", statErr)
	}

	for _, name := range outcome.DisplacedByRevertedAdopt {
		if name == "note" {
			t.Fatalf("note was not reverted -- its store copy and link are both live -- and naming it "+
				"tells the user to copy an archive over them: %+v", outcome.DisplacedByRevertedAdopt)
		}
	}
	if len(outcome.DisplacedByRevertedAdopt) != 1 || outcome.DisplacedByRevertedAdopt[0] != "notes" {
		t.Fatalf("the half that was undone must still be named: %+v", outcome.DisplacedByRevertedAdopt)
	}
}

// A successful revert names every adopt in its window, including one whose own
// prefix saw no change.
//
// Adopt a skill, remove it, do something else, then revert past all three: the
// tree at HEAD and the tree being restored both lack that skill, so nothing
// under its prefix moves. The adopt was undone all the same, and the user's
// pre-adopt original is still sitting in recovery/ with no command to restore
// it -- which is the whole of what this warning is for.
//
// The per-name filter that guards the failure path must not reach here. It
// exists because a failed apply may have converged only part of the window;
// a successful one converged all of it, and filtering by moved paths would
// turn the over-claim it prevents into the opposite mistake.
func TestRevertNamesAnAdoptWhoseOwnPathsDidNotMove(t *testing.T) {
	_, st, dirs := scenarioEnv(t)
	claudeDir := dirs["claude"]
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: d\n---\n")
	agents := agent.Detected()
	if _, err := Adopt(st, agents, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(st, agents, "notes"); err != nil {
		t.Fatal(err)
	}
	// So the tree being restored differs from HEAD's and the revert has work
	// to do -- otherwise it refuses as a no-op and the question never arises.
	if _, err := NewSkill(st, agents, "writer"); err != nil {
		t.Fatal(err)
	}

	outcome, err := RevertOperations(st, agents, 3)
	if err != nil {
		t.Fatal(err)
	}
	var movedNotes bool
	for _, path := range outcome.Changed {
		if strings.HasPrefix(path, "skills/notes/") {
			movedNotes = true
		}
	}
	if movedNotes {
		t.Fatalf("precondition: nothing under skills/notes/ may move here, got %v", outcome.Changed)
	}
	if len(outcome.DisplacedByRevertedAdopt) != 1 || outcome.DisplacedByRevertedAdopt[0] != "notes" {
		t.Fatalf("the adopt was undone and its original is still only in recovery/, "+
			"so it must be named even though its own paths did not move: %+v",
			outcome.DisplacedByRevertedAdopt)
	}
}

// A revert that leaves the skill in the store must not be named, even though
// its paths moved and its adopt really was undone.
//
// Adopt a skill, remove it, adopt it again, then revert past the second adopt:
// the revert restores the first store copy and the reconcile relinks it, so
// the agent entry is not empty and fu is managing it. Telling the user to copy
// the recovery archive over that name lands on a live fu link -- the same
// mistake a refused revert used to invite, reached through a revert that
// succeeded.
//
// This is what a rule keyed on "did this revert move anything under the
// skill's prefix" gets wrong: here the paths did move.
func TestRevertDoesNotNameAnAdoptWhoseSkillSurvived(t *testing.T) {
	_, st, dirs := scenarioEnv(t)
	claudeDir := dirs["claude"]
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agents := agent.Detected()

	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: first\n---\n")
	if _, err := Adopt(st, agents, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSkill(st, agents, "notes"); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, claudeDir, "notes", "---\nname: notes\ndescription: second\n---\n")
	if _, err := Adopt(st, agents, ""); err != nil {
		t.Fatal(err)
	}

	// Back past the second adopt, which restores the first copy.
	outcome, err := RevertOperations(st, agents, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Precondition: the skill survived and the entry is live.
	if _, statErr := os.Stat(filepath.Join(st.SkillsDir(), "notes")); statErr != nil {
		t.Fatalf("precondition: the earlier copy must have been restored: %v", statErr)
	}
	var movedNotes bool
	for _, path := range outcome.Changed {
		if strings.HasPrefix(path, "skills/notes/") {
			movedNotes = true
		}
	}
	if !movedNotes {
		t.Fatalf("precondition: this revert must have moved the skill's own paths, got %v", outcome.Changed)
	}

	if len(outcome.DisplacedByRevertedAdopt) != 0 {
		t.Fatalf("the skill is still in the store and fu is linking it; naming it sends the user "+
			"to copy an archive over a live link: %+v", outcome.DisplacedByRevertedAdopt)
	}
}
