// internal/cli/status_test.go
package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/engine"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/store"
)

type fakeStatusApplication struct {
	outcome engine.StatusOutcome
	err     error
}

func (f fakeStatusApplication) Status() (engine.StatusOutcome, error) { return f.outcome, f.err }

// TestStatusCommandExitsZeroWhenItFindsDrift pins that a finding is not a
// failure. `git status` reports and exits 0; DESIGN §7 keeps three exit codes,
// and drift is none of them.
func TestStatusCommandExitsZeroWhenItFindsDrift(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Agents: []engine.AgentStatus{{
			Name:  "claude",
			Drift: []engine.Action{{Type: engine.CreateLink, AgentName: "claude", Skill: "alpha"}},
		}},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("reporting drift must not be an error: %v", err)
	}
	if !strings.Contains(stdout.String(), "alpha") {
		t.Fatalf("status output %q does not name the drifting skill", stdout.String())
	}
}

// Self-review addition: driftLabel is the one piece of real translation logic
// in this command (engine terms -> user terms). A table test pins each
// mapping directly, cheaper than routing every case through the full command.
func TestDriftLabelNamesEveryActionTypeStatusCanProduce(t *testing.T) {
	cases := map[engine.ActionType]string{
		engine.CreateLink:            "missing link",
		engine.RemoveLink:            "stale link",
		engine.ReportConflict:        "occupied by unmanaged content",
		engine.ReportDisabledForeign: "off, but occupied by unmanaged content",
		engine.ReportMissing:         "enabled, but the store no longer holds it",
		engine.ReportReserved:        "reserved name, never linked",
		engine.ReportInvalid:         "invalid name, never linked",
		engine.ReportForeign:         "unmanaged",
	}
	for actionType, want := range cases {
		if got := driftLabel(engine.Action{Type: actionType}); got != want {
			t.Errorf("driftLabel(%v) = %q, want %q", actionType, got, want)
		}
	}
}

// Self-review addition: the store section (uncommitted paths and unfinished
// transactions) is rendered by neither the brief's own test nor any other
// command's test -- pin its exact wording here.
func TestStatusCommandReportsStoreDirtyAndPending(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Store: engine.StoreStatus{
			DirtyPaths: []string{"skills/alpha/SKILL.md"},
			Pending:    []engine.PendingOperation{{Op: "rm", Name: "beta"}},
		},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		"store",
		"uncommitted  skills/alpha/SKILL.md",
		// Hedged deliberately. A pending transaction can also be a
		// safe-conflict every write command fails on at the recovery entry
		// point until it is repaired by hand, and status cannot tell the two
		// apart without running recovery -- which it must not do.
		"unfinished   rm beta (the next write command settles it, or says what needs repairing)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusCommandFilesAgentDriftUnderItsOwnHeading pins that the agent
// section is a section. Its lines were two-space indented with no heading of
// their own, directly under the store heading printed above them, so every
// drifting link read as store content.
func TestStatusCommandFilesAgentDriftUnderItsOwnHeading(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Store: engine.StoreStatus{DirtyPaths: []string{"skills/alpha/SKILL.md"}},
		Agents: []engine.AgentStatus{{
			Name:  "claude",
			Drift: []engine.Action{{Type: engine.CreateLink, AgentName: "claude", Skill: "alpha"}},
		}},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	headingIdx := strings.Index(out, "\nagents\n")
	driftIdx := strings.Index(out, "missing link   claude/alpha")
	if headingIdx < 0 {
		t.Fatalf("the agent section needs a heading of its own:\n%s", out)
	}
	if driftIdx < headingIdx {
		t.Fatalf("agent drift must be filed under that heading, not the store's:\n%s", out)
	}
}

// TestStatusCommandOmitsTheAgentHeadingWhenNoAgentHasAnythingToSay is the
// other half: a heading with nothing under it is scaffold, not a report.
func TestStatusCommandOmitsTheAgentHeadingWhenNoAgentHasAnythingToSay(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Store:  engine.StoreStatus{DirtyPaths: []string{"skills/alpha/SKILL.md"}},
		Agents: []engine.AgentStatus{{Name: "claude"}},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out := stdout.String(); strings.Contains(out, "agents") {
		t.Fatalf("a clean agent must not produce an empty section:\n%s", out)
	}
}

// Self-review addition: pins the three agent-precondition lines (ScanErr,
// DirIsSymlink, DirMissing), each worded differently, from one report.
func TestStatusCommandReportsAgentPreconditions(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Agents: []engine.AgentStatus{
			{Name: "claude", DirMissing: true},
			{Name: "codex", DirIsSymlink: true},
			{Name: "cursor", ScanErr: "permission denied"},
		},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		"claude: detected, nothing projected yet",
		"codex: skills dir is a symlink; run `fu adopt` to convert it",
		"cursor: could not be inspected: permission denied",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

// Pins that all three agent preconditions suppress per-skill drift, and why.
//
// DirMissing used to be the exception: it printed its own header line and
// then fell through to list Drift as well. That was deliberate but wrong at
// scale -- a newly detected agent drifts on every enabled skill, all for the
// one reason the header line already states, so a store with fifty skills
// produced fifty-one lines saying the same thing. SPEC rule 4 asks a
// read-only command to say a new agent is awaiting projection ("仅提示待投放")
// and stop. ScanErr and DirIsSymlink already suppressed drift, for the
// neighbouring reason that an uninspectable or write-refused directory has no
// meaningful per-skill classification to show.
//
// TestStatusCommandReportsAgentPreconditions above does not exercise Drift at
// all, so it cannot tell suppression from fall-through; this test drives all
// three preconditions with non-empty Drift attached.
func TestStatusCommandDirMissingSuppressesPerSkillDriftLikeTheOtherPreconditions(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Agents: []engine.AgentStatus{
			{
				Name:       "claude",
				DirMissing: true,
				Drift:      []engine.Action{{Type: engine.CreateLink, AgentName: "claude", Skill: "alpha"}},
			},
			{
				Name:    "codex",
				ScanErr: "boom",
				Drift:   []engine.Action{{Type: engine.CreateLink, AgentName: "codex", Skill: "beta"}},
			},
			{
				Name:         "cursor",
				DirIsSymlink: true,
				Drift:        []engine.Action{{Type: engine.CreateLink, AgentName: "cursor", Skill: "gamma"}},
			},
		},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if !strings.Contains(out, "claude: detected, nothing projected yet") {
		t.Fatalf("a newly detected agent must be reported as awaiting projection: %q", out)
	}
	if strings.Contains(out, "alpha") {
		t.Fatalf("the pending-projection line is the one cause of every skill's drift; listing them again says nothing new: %q", out)
	}
	if strings.Contains(out, "beta") {
		t.Fatalf("an agent that could not be inspected must not also list stale drift: %q", out)
	}
	if strings.Contains(out, "gamma") {
		t.Fatalf("a symlinked skills dir must not also list stale drift: %q", out)
	}
}

// Self-review addition: pins the recovery section's exact wording, and the
// explicit product constraint carried in this task's brief -- the
// Uncollectable line must not read as an invitation to delete. README.md
// states only removed-* may be removed by hand, and an orphaned rollback-
// now lands in this bucket; "no command collects it yet" must stay
// deliberately inactionable rather than suggesting manual cleanup.
func TestStatusCommandRecoverySectionWordingDoesNotInviteDeletion(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Recovery: engine.RecoveryInventory{Collectable: 2, Uncollectable: 3},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		"recovery",
		"2 collectable (run `fu gc`)",
		"3 that no command collects yet",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
	lower := strings.ToLower(out)
	for _, verb := range []string{"delete", "remove it", "rm -", "safe to", "clean up", "may remove"} {
		if strings.Contains(lower, verb) {
			t.Fatalf("uncollectable wording must not invite deletion (found %q): %q", verb, out)
		}
	}
}

// TestStatusCommandSeparatesWhatGCCannotCollectYet pins that the two counts
// carry different remedies and never merge: `fu gc` collects the first, and
// leaves the second exactly where it is until a recovery pass settles the
// unfinished write behind it. Sending a user to `fu gc` for the second is
// sending them to watch a count not move.
func TestStatusCommandSeparatesWhatGCCannotCollectYet(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Recovery: engine.RecoveryInventory{Collectable: 2, Blocked: 1},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		"recovery",
		"2 collectable (run `fu gc`)",
		"1 waiting on an unfinished write (run `fu restore`, which settles it or says what needs repairing, then `fu gc`)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusCommandReportsRetainedWithoutInvitingDeletion pins that the count
// engine.Status computes for adopt-archive-* and adopt-link-*.json is
// rendered. It was computed and tested but excluded from the section header
// gate, so it reached no reader at all: SPEC §9 promises this content is kept
// for restoring an adopted entry in place, and how much of it is sitting there
// is exactly the sort of accumulation this section exists to report. It has no
// remedy, though, so its wording must stay as inactionable as the
// uncollectable line's.
func TestStatusCommandReportsRetainedWithoutInvitingDeletion(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Recovery: engine.RecoveryInventory{Retained: 5},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{"recovery", "5 kept on purpose (what `fu adopt` set aside)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
	lower := strings.ToLower(out)
	for _, verb := range []string{"delete", "remove it", "rm -", "safe to", "clean up", "may remove"} {
		if strings.Contains(lower, verb) {
			t.Fatalf("retained wording must not invite deletion (found %q): %q", verb, out)
		}
	}
}

// TestStatusCommandSaysSoWhenFullyClean pins the confirmation. A fully clean
// report used to print nothing at all, which is also what a command that did
// nothing prints -- a reader could not tell "everything matches" from "this
// silently failed to look".
func TestStatusCommandSaysSoWhenFullyClean(t *testing.T) {
	cmd := newStatusCmd(fakeStatusApplication{outcome: engine.StatusOutcome{}})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := "nothing to report: what fu.yaml asks for is what is on disk\n"; stdout.String() != want {
		t.Fatalf("a clean report must say so, got %q, want %q", stdout.String(), want)
	}
}

// Self-review addition: pins the section ordering (store, then agents, then
// recovery) end to end from one report, which no single-section test above
// exercises.
// TestStatusCommandReportsStagingWithoutSendingUserToGC pins the staging
// section's wording and the single way it must differ from the recovery
// section's. `fu gc` never looks at staging, so the waiting line stops at
// `fu restore` instead of repeating recovery's "then `fu gc`" -- naming gc here
// would send a reader to watch a count not move, the exact failure the split
// into buckets exists to prevent. The uncollectable line carries the same
// deliberately inactionable wording as recovery's, and for the same reason:
// nothing here has the ownership evidence that would justify deleting it.
func TestStatusCommandReportsStagingWithoutSendingUserToGC(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Staging: engine.StagingInventory{Blocked: 2, Uncollectable: 3},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		"staging",
		"2 waiting on an unfinished write (run `fu restore`)",
		"3 that no command collects yet",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fu gc") {
		t.Fatalf("staging holds nothing `fu gc` collects, so it must not be named: %q", out)
	}
	lower := strings.ToLower(out)
	for _, verb := range []string{"delete", "remove it", "rm -", "safe to", "clean up", "may remove"} {
		if strings.Contains(lower, verb) {
			t.Fatalf("staging wording must not invite deletion (found %q): %q", verb, out)
		}
	}
}

// The two machine-local sections come last and in the order a reader acts on
// them: recovery before staging, because recovery is where the collectable
// work is and staging holds only what a recovery pass or nothing at all will
// settle.
func TestStatusCommandOrdersStoreAgentsThenRecoveryThenStaging(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Store: engine.StoreStatus{DirtyPaths: []string{"skills/alpha/SKILL.md"}},
		Agents: []engine.AgentStatus{{
			Name:  "claude",
			Drift: []engine.Action{{Type: engine.CreateLink, AgentName: "claude", Skill: "alpha"}},
		}},
		Recovery: engine.RecoveryInventory{Collectable: 2},
		Staging:  engine.StagingInventory{Uncollectable: 1},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	storeIdx := strings.Index(out, "store")
	agentIdx := strings.Index(out, "claude")
	recoveryIdx := strings.Index(out, "recovery")
	stagingIdx := strings.Index(out, "staging")
	if storeIdx < 0 || agentIdx < 0 || recoveryIdx < 0 || stagingIdx < 0 {
		t.Fatalf("status output missing a section: %q", out)
	}
	if storeIdx >= agentIdx || agentIdx >= recoveryIdx || recoveryIdx >= stagingIdx {
		t.Fatalf("sections out of order (store=%d, agent=%d, recovery=%d, staging=%d): %q",
			storeIdx, agentIdx, recoveryIdx, stagingIdx, out)
	}
}

// Self-review addition: mirrors gc_test.go's
// TestGCCommandDoesNotClaimSuccessOnFailure -- an Application-level error
// must reach the caller unmodified and must not be swallowed into a
// misleading report. The assertion checks for the absence of report content
// rather than a wholly empty buffer: cobra's own Execute() writes its
// default "Error: ...\nUsage:\n..." to cmd.ErrOrStderr() whenever RunE
// returns a non-nil error and SilenceErrors/SilenceUsage are not set on this
// standalone command (NewRootCmd sets both on the real root, but a bare
// newStatusCmd built directly by a test does not) -- gc_test.go's own
// version avoids asserting emptiness for the same reason.
func TestStatusCommandPropagatesApplicationError(t *testing.T) {
	failure := errors.New("boom")
	cmd := newStatusCmd(fakeStatusApplication{err: failure})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); !errors.Is(err, failure) {
		t.Fatalf("status error = %v, want %v", err, failure)
	}
	for _, notWant := range []string{"uncommitted", "unfinished", "collectable", "missing link"} {
		if strings.Contains(output.String(), notWant) {
			t.Fatalf("an empty report must print nothing at all (found %q): %q", notWant, output.String())
		}
	}
	// Least of all the clean confirmation: a failed status assembled some
	// section or other not at all, so it cannot vouch for what is on disk.
	if strings.Contains(output.String(), "nothing to report") {
		t.Fatalf("a failed status must not claim a clean report: %q", output.String())
	}
}

// TestStatusCommandPrintsThePartialReportBesideTheError is the rendering half
// of the isolation engine.Status builds: it reads the store-side facts after
// the agents precisely so a failure there costs the user one section rather
// than the report. Returning before printing threw that away again at the last
// step -- and `fu status` is the command a user runs *because* the store looks
// damaged. The error still reaches the caller, so the exit code is unchanged.
func TestStatusCommandPrintsThePartialReportBesideTheError(t *testing.T) {
	failure := errors.New("boom")
	outcome := engine.StatusOutcome{
		Report: engine.StatusReport{
			Agents: []engine.AgentStatus{{
				Name:  "claude",
				Drift: []engine.Action{{Type: engine.CreateLink, AgentName: "claude", Skill: "alpha"}},
			}},
		},
		Diagnostics: engine.ReadDiagnostics{
			ConfigPath:   "/home/u/.fu/store/fu.yaml",
			InvalidNames: []engine.InvalidConfigName{{Name: "Beta", Reason: "uppercase"}},
		},
	}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome, err: failure})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); !errors.Is(err, failure) {
		t.Fatalf("status error = %v, want %v", err, failure)
	}
	if !strings.Contains(stdout.String(), "missing link   claude/alpha") {
		t.Fatalf("the part of the report the engine did assemble must still reach the user: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Beta") {
		t.Fatalf("the diagnostics must reach the user too: %q", stderr.String())
	}
}

// Self-review addition: proves printVersionWarning and printInvalidNames are
// actually wired into newStatusCmd (the brief's context calls this out as a
// hard requirement) and land on stderr, never stdout -- the same discipline
// TestReadOnlyDiagnosticsGoToStderrOnly pins for list/show.
func TestStatusCommandPrintsVersionWarningAndInvalidNamesToStderrOnly(t *testing.T) {
	outcome := engine.StatusOutcome{
		Diagnostics: engine.ReadDiagnostics{
			ConfigPath:    "/home/u/.fu/store/fu.yaml",
			VersionTooNew: true,
			InvalidNames:  []engine.InvalidConfigName{{Name: "Beta", Reason: "uppercase"}},
		},
	}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "/home/u/.fu/store/fu.yaml") || !strings.Contains(stderr.String(), "warning:") {
		t.Fatalf("version warning must reach stderr naming fu.yaml: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Beta") || !strings.Contains(stderr.String(), "invalid:") {
		t.Fatalf("invalid-name diagnostic must reach stderr: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "warning:") || strings.Contains(stdout.String(), "Beta") {
		t.Fatalf("diagnostics must never leak onto stdout: %q", stdout.String())
	}
}

// Self-review addition: everything above drives newStatusCmd through a fake
// Application, which is what the brief's own test does too -- so nothing yet
// exercises the production glue Step 3 adds (Application.Status, wired
// through NewRootCmd). This integration test runs the real store and a real
// detected agent end to end (init, new, delete the projected link by hand),
// confirming the wiring reaches the true engine.Status computation and that
// the command remains read-only under SPEC's "只读命令...不修改 store 内容与
// agent 目录" -- the same property TestListAndShowAreReadOnly pins for list
// and show, extended here to cover the newly wired command.
func TestStatusCommandIntegrationReportsDriftAndIsReadOnly(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	mustMkdirAll(t, claudeDir)
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	link := filepath.Join(claudeDir, "skills", "alpha")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	log, err := st.Log(50)
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := st.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	storeDigest, err := skill.Digest(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	claudeDigest, err := skill.Digest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "status")
	if err != nil {
		t.Fatalf("status must succeed even with drift present: %v", err)
	}
	if !strings.Contains(out, "claude/alpha") || !strings.Contains(out, "missing link") {
		t.Fatalf("status must report the deleted link as drift: %q", out)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("status must not recreate the missing link")
	}

	afterLog, err := st.Log(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != len(afterLog) {
		t.Fatalf("commit count changed: %d -> %d", len(log), len(afterLog))
	}
	afterDirty, err := st.IsDirty()
	if err != nil {
		t.Fatal(err)
	}
	if dirty != afterDirty {
		t.Fatal("status changed the store's dirty state")
	}
	afterStoreDigest, err := skill.Digest(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if storeDigest != afterStoreDigest {
		t.Fatal("status changed store content")
	}
	afterClaudeDigest, err := skill.Digest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	if claudeDigest != afterClaudeDigest {
		t.Fatal("status changed the claude skills directory")
	}
}

// TestStatusCommandIntegrationReportsADeletedStoreEntityAsOneLine drives SPEC
// rule 6's headline example -- the store entity removed by hand -- through the
// real store, agent and command. Diff answers this state with the pair that
// repairs it, so the report used to print `stale link   claude/alpha`
// immediately followed by `missing link   claude/alpha`: two lines
// contradicting each other about one entry, neither of them saying what is
// actually wrong, while driftLabel's accurate wording for it sat unreachable
// because Diff never emits ReportMissing.
func TestStatusCommandIntegrationReportsADeletedStoreEntityAsOneLine(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	if err := os.RemoveAll(filepath.Join(fuHome, "store", "skills", "alpha")); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "status")
	if err != nil {
		t.Fatalf("status must survive a deleted store entity: %v", err)
	}
	if !strings.Contains(out, "enabled, but the store no longer holds it") || !strings.Contains(out, "claude/alpha") {
		t.Fatalf("status must name what is actually wrong: %q", out)
	}
	for _, notWant := range []string{"stale link", "missing link"} {
		if strings.Contains(out, notWant) {
			t.Fatalf("one broken link must not also be reported as %q: %q", notWant, out)
		}
	}
}

// Self-review addition: the integration-level counterpart of
// TestStatusCommandPrintsVersionWarningAndInvalidNamesToStderrOnly, driven
// through the real Application/store the way
// TestListAndShowWarnOnVersionTooNew and
// TestReadOnlyDiagnosticsGoToStderrOnly drive list/show -- proving
// engine.readDiagnostics' output actually reaches these two functions
// through Application.Status, not just that the functions behave correctly
// when handed a diagnostics value by hand.
func TestStatusCommandIntegrationWarnsOnVersionTooNewAndInvalidName(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	runCmd(t, "init")
	runCmd(t, "new", "alpha")

	cfgPath := filepath.Join(fuHome, "store", "fu.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "version: 1", "version: 99", 1)
	edited = strings.Replace(edited, "skills:\n  alpha:",
		"skills:\n  Beta:\n    digest: sha256:bad\n    enabled: true\n  alpha:", 1)
	if edited == string(raw) {
		t.Fatal("setup check: fu.yaml did not contain the expected anchors this test edits")
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCmdSplit(t, "status")
	if err != nil {
		t.Fatalf("status must survive a too-new version and an invalid name: %v", err)
	}
	if !strings.Contains(stderr, cfgPath) || !strings.Contains(stderr, "warning:") {
		t.Fatalf("version warning must reach stderr naming fu.yaml, got stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "Beta") || !strings.Contains(stderr, "invalid:") {
		t.Fatalf("invalid-name diagnostic must reach stderr, got stderr=%q", stderr)
	}
	if strings.Contains(stdout, "warning:") || strings.Contains(stdout, "Beta") {
		t.Fatalf("diagnostics must not leak onto stdout, got stdout=%q", stdout)
	}
}

// Self-review addition: the flip side of the brief's own exit-0-on-drift
// test, at the real process-exit-code boundary (execute(), DESIGN §7) rather
// than the newStatusCmd unit level -- a genuine operation failure (no store
// to read at all) must still exit 1, so status's exit-0 behavior is about
// drift specifically, not about the command never failing.
func TestStatusCommandOnUninitializedStoreIsOperationFailure(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	code, out := runExitCode(t, "status")
	if code != 1 {
		t.Fatalf("status on an uninitialized store must exit 1, got %d; output: %s", code, out)
	}
	if !strings.Contains(out, "error:") {
		t.Fatalf("output must still report the error, got %q", out)
	}
}

// TestStatusCommandPrintsTheUnmatchedStagingLine pins the output half of the
// same bucket: the line, like the count, shipped with nothing that fails when
// it is removed.
func TestStatusCommandPrintsTheUnmatchedStagingLine(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Staging: engine.StagingInventory{Unmatched: 2},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if !strings.Contains(out, "staging") || !strings.Contains(out, "2 staging entr") {
		t.Fatalf("the unmatched staging count must be reported:\n%s", out)
	}
	// Deliberately inactionable in the same way the other residue lines are:
	// this content is not fu's to delete.
	for _, notWant := range []string{"delete", "remove them", "safe to"} {
		if strings.Contains(out, notWant) {
			t.Fatalf("the staging line must not invite deletion (%q):\n%s", notWant, out)
		}
	}
}

// TestStatusCommandReportsNonProjectionFindingsForAPendingAgent pins the
// boundary of the DirMissing suppression added in the previous round.
//
// Suppressing per-skill "missing link" lines is right: a newly detected agent
// drifts on every enabled skill for the one reason the pending-projection line
// already states. But the suppression dropped the whole Drift slice, and
// Status puts Desired's own ReportReserved / ReportInvalid findings in that
// same slice -- while readDiagnostics suppresses the stderr `invalid:` line on
// the explicit premise that the per-agent finding covers it. So a reserved,
// invalid name in fu.yaml was reported on neither stream, for this command
// only; every write command still reports it, because createAndScanAgentDir
// creates the directory and DirMissing never applies.
//
// ReportMissing is swallowed the same way, and it is a store-side fact with
// nothing to do with the agent's directory at all. SPEC rule 4's "仅提示待投放"
// is about projection, not about configuration diagnostics.
func TestStatusCommandReportsNonProjectionFindingsForAPendingAgent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agent      engine.AgentStatus
		wantHidden string
		wantShown  []string
	}{
		{
			name: "DirMissing",
			agent: engine.AgentStatus{
				Name:       "codex",
				DirMissing: true,
				Drift: []engine.Action{
					{Type: engine.CreateLink, AgentName: "codex", Skill: "alpha"},
					{Type: engine.ReportReserved, AgentName: "codex", Skill: ".system"},
					{Type: engine.ReportMissing, AgentName: "codex", Skill: "beta"},
				},
			},
			wantHidden: "alpha",
			wantShown:  []string{"detected, nothing projected yet", ".system", "beta"},
		},
		{
			name: "DirIsSymlink",
			agent: engine.AgentStatus{
				Name:         "codex",
				DirIsSymlink: true,
				Drift: []engine.Action{
					{Type: engine.CreateLink, AgentName: "codex", Skill: "alpha"},
					// ReportReserved rather than ReportInvalid: LoadConfig
					// already keeps an invalid name out of SkillNames(), so
					// Desired's ReportInvalid arm is unreachable from
					// `fu status` and this subtest would drive a state the
					// engine cannot produce. A reserved name reaches here for
					// real -- codex's own ".system", printed beneath the
					// symlink line.
					{Type: engine.ReportReserved, AgentName: "codex", Skill: ".system"},
				},
			},
			wantHidden: "alpha",
			wantShown:  []string{"skills dir is a symlink", ".system"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := engine.StatusOutcome{Report: engine.StatusReport{
				Agents: []engine.AgentStatus{tc.agent},
			}}
			cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stdout)
			cmd.SetArgs([]string{})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			out := stdout.String()
			for _, want := range tc.wantShown {
				if !strings.Contains(out, want) {
					t.Fatalf("a finding this directory did not cause must still be reported (%q):\n%s", want, out)
				}
			}
			if strings.Contains(out, tc.wantHidden) {
				t.Fatalf("per-skill projection drift must still be suppressed (%q):\n%s", tc.wantHidden, out)
			}
		})
	}
}

// TestStatusDriftColumnAlignsToTheWidestLabelPrinted pins the alignment
// itself. The existing long-label test asserts the label and the skill name
// with two separate Contains calls and never observes the column, so forcing
// driftColumnWidth to return its floor left the whole package green.
//
// Alignment is the entire purpose of the function: with a fixed 14, only three
// of the eight labels fitted, and any report containing "enabled, but the
// store no longer holds it" (41 characters) came out ragged.
func TestStatusDriftColumnAlignsToTheWidestLabelPrinted(t *testing.T) {
	outcome := engine.StatusOutcome{Report: engine.StatusReport{
		Agents: []engine.AgentStatus{{
			Name: "claude",
			Drift: []engine.Action{
				{Type: engine.RemoveLink, AgentName: "claude", Skill: "alpha"},
				{Type: engine.ReportMissing, AgentName: "claude", Skill: "beta"},
			},
		}},
	}}
	cmd := newStatusCmd(fakeStatusApplication{outcome: outcome})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var short, long string
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.Contains(line, "claude/alpha"):
			short = line
		case strings.Contains(line, "claude/beta"):
			long = line
		}
	}
	if short == "" || long == "" {
		t.Fatalf("both drift lines must be printed:\n%s", stdout.String())
	}
	if strings.Index(short, "claude/") != strings.Index(long, "claude/") {
		t.Fatalf("the skill column must line up across labels of different length:\n%q\n%q", short, long)
	}
	// And the widening must be real, not the 14-character floor.
	if strings.Index(long, "claude/") <= len("  ")+14 {
		t.Fatalf("a 41-character label must widen the column beyond the floor:\n%q", long)
	}
}

// TestStatusReportsAReservedInvalidNameWhenItsAgentCouldNotBeScanned pins the
// last door the reserved/invalid suppression left open.
//
// readDiagnostics stands the config-level `invalid:` line down whenever
// alreadyReportedAsReserved says some agent's own ReportReserved covers the
// name. That predicate asks the agent *list*, not the report -- so it stood
// the line down even when engine.Status never got as far as Desired for that
// agent, because ScanAgent had already failed and status.go returned early.
// The result was the one command that exits 0 and says nothing at all: `fu
// list` still printed the `invalid:` line for the same config and `fu enable`
// still failed loudly, while `fu status` reported only the scan failure.
func TestStatusReportsAReservedInvalidNameWhenItsAgentCouldNotBeScanned(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	// codex detected, but its skills directory is a plain file, so ScanAgent
	// fails for it -- the state in which no ReportReserved can be produced.
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "skills"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}
	// ".system" is codex's own reserved entry and fails name validation, so it
	// is exactly the name both channels can claim the other covers.
	st, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.ConfigPath(), []byte("version: 1\nskills:\n  .system:\n    enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCmdSplit(t, "status")
	if err != nil {
		t.Fatalf("status must still succeed: %v (%s%s)", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "could not be inspected") {
		t.Fatalf("the scan failure must still be reported:\n%s%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "invalid:") || !strings.Contains(stderr, ".system") {
		t.Fatalf("an invalid name no per-agent finding could cover must reach stderr:\nstdout=%q\nstderr=%q", stdout, stderr)
	}
}
