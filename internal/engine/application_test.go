package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
)

func TestApplicationOwnsInitializationQueriesAndAgentSelection(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := NewApplication()
	initialized, err := app.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	wantHome, err := filepath.EvalSymlinks(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	if initialized.Home != wantHome {
		t.Fatalf("initialized home = %q, want %q", initialized.Home, wantHome)
	}
	created, err := app.NewSkill("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !created.Committed || !created.ReconcileComplete {
		t.Fatalf("new outcome is incomplete: %+v", created)
	}
	listed, err := app.ListSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Agents) != 1 || listed.Agents[0] != "claude" || len(listed.Skills) != 1 || listed.Skills[0].Name != "alpha" {
		t.Fatalf("list outcome = %+v", listed)
	}
	shown, err := app.ShowSkill("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if shown.Name != "alpha" || len(shown.Agents) != 1 || shown.Agents[0].Name != "claude" {
		t.Fatalf("show outcome = %+v", shown)
	}
}

func TestApplicationReturnsCommittedOutcomeWithPendingRecovery(t *testing.T) {
	fuHome := t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", t.TempDir())
	interrupted := errors.New("interrupted after commit")
	app := newApplication(hooks{afterCommit: func() error { return interrupted }})
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}

	outcome, err := app.NewSkill("alpha")
	if !errors.Is(err, interrupted) {
		t.Fatalf("new error = %v, want %v", err, interrupted)
	}
	if !outcome.Committed || !outcome.RecoveryPending || outcome.PostCommitComplete {
		t.Fatalf("application must expose the durable pending state: %+v", outcome)
	}
}

func TestToggleDeliveryBlockedUsesOnlyTargetedAgents(t *testing.T) {
	result := Result{
		Conflicts: []Action{{AgentName: "claude", Skill: "alpha"}},
		Missing:   []Action{{AgentName: "codex", Skill: "alpha"}},
	}
	if !toggleDeliveryBlocked(result, "alpha", []string{"claude"}) {
		t.Fatal("targeted conflict must block the delivery claim")
	}
	if toggleDeliveryBlocked(result, "alpha", []string{"other"}) {
		t.Fatal("another agent's finding must not block the delivery claim")
	}
}

func TestUserReportsOmitInformationalForeignEntries(t *testing.T) {
	result := Result{
		Foreign:   []Action{{AgentName: "claude", Skill: "unmanaged"}},
		Conflicts: []Action{{AgentName: "claude", Skill: "alpha"}},
		Failed:    []FailedAction{{Action: Action{AgentName: "codex"}, Err: errors.New("broken")}},
	}
	reports := result.UserReports()
	if len(reports) != 2 || reports[0].Kind != ReportConflict || reports[1].Kind != ReportFailed {
		t.Fatalf("user reports = %+v", reports)
	}
}

// TestPrepareAddReturnsUntypedNilOnFailure pins the interface-nil trap.
// Returning prepareAddSource's nil *AddPlan directly as AddSession would
// create a non-nil interface holding a nil pointer, so the idiomatic call --
// `if plan != nil { defer plan.Close() }` -- took the branch and panicked.
// internal/cli/add.go survives only because it checks err before the defer,
// and go vet does not catch this. This is the one interface-returning method
// on the boundary this branch exists to create for a second front end.
func TestPrepareAddReturnsUntypedNilOnFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	app := NewApplication()

	for name, arg := range map[string]string{
		"empty source argument": "",
		"missing local path":    filepath.Join(t.TempDir(), "nope"),
	} {
		t.Run(name, func(t *testing.T) {
			preparation, err := app.PrepareAdd(arg, "")
			if err == nil {
				t.Fatalf("%s must fail", name)
			}
			if preparation.Session != nil {
				t.Fatalf("a failed PrepareAdd must return an untyped nil session, got %#v", preparation.Session)
			}
			// The defensive shape a second front end would write must not panic.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the idiomatic nil check must not panic: %v", r)
				}
			}()
			if preparation.Session != nil {
				_ = preparation.Session.Close()
			}
		})
	}
}

func TestApplicationValidatesMalformedArgumentsBeforeOpeningStore(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-store")
	t.Setenv("FU_HOME", missing)
	t.Setenv("HOME", t.TempDir())
	app := NewApplication()

	if _, err := app.PrepareAdd("https://example.com/repo.git", "refs/heads/main"); !errors.Is(err, ErrInvalidAddRef) {
		t.Fatalf("PrepareAdd error = %v, want ErrInvalidAddRef before missing-store error", err)
	}
	if _, err := app.Adopt(AdoptScope{Agent: "not-an-agent"}); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("Adopt error = %v, want ErrUnknownAgent before missing-store error", err)
	}
	if _, err := app.SetAgent("alpha", "not-an-agent", true); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("SetAgent error = %v, want ErrUnknownAgent before missing-store error", err)
	}
}
