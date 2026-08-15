package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

func PrepareAdd(st *store.Store, arg, ref string) (AddPreparation, error) {
	src, err := parseAddSource(arg, ref)
	if err != nil {
		return AddPreparation{}, err
	}
	plan, prologue, err := prepareAddSource(st, arg, src, agent.Detected(), hooks{})
	preparation := AddPreparation{Prologue: prologue}
	if err == nil {
		preparation.Session = plan
	}
	return preparation, err
}

func TestPrepareAddOwnsSourceInspectionAndInstallation(t *testing.T) {
	fuHome := t.TempDir()
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	sourceDir := t.TempDir()
	writeSkillTree(t, sourceDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")

	preparation, err := PrepareAdd(s, sourceDir, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := preparation.Session
	defer plan.Close()
	candidates := plan.Candidates()
	if len(candidates) != 1 || candidates[0].Name != "alpha" {
		t.Fatalf("candidates = %+v", candidates)
	}
	outcome, err := plan.Install(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Added) != 1 || outcome.Added[0] != "alpha" {
		t.Fatalf("outcome = %+v", outcome)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	fields := cfg.SourceFields("alpha")
	resolved, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if fields["type"] != "local" || fields["path"] != resolved {
		t.Fatalf("source fields = %v", fields)
	}
	if _, err := os.Stat(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md")); err != nil {
		t.Fatalf("installed skill missing: %v", err)
	}
}

func TestPrepareAddPinsLocalSourceAcrossPathReplacement(t *testing.T) {
	fuHome := t.TempDir()
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	container := t.TempDir()
	sourceDir := filepath.Join(container, "source")
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, sourceDir, "alpha", "---\nname: alpha\ndescription: original\n---\n")
	preparation, err := PrepareAdd(s, sourceDir, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := preparation.Session
	defer plan.Close()
	candidates := plan.Candidates()
	if err := os.Rename(sourceDir, sourceDir+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, sourceDir, "alpha", "---\nname: alpha\ndescription: replacement\n---\n")
	if _, err := plan.Install(candidates); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"))
	if err != nil || !strings.Contains(string(got), "description: original") {
		t.Fatalf("install must read the pinned original source: %q, %v", got, err)
	}
}

func TestPrepareAddPinsGitCloneAcrossPathReplacement(t *testing.T) {
	fuHome := t.TempDir()
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	url := makeGitSourceNamed(t, false, "alpha")
	preparation, err := PrepareAdd(s, url, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := preparation.Session.(*AddPlan)
	candidates := plan.Candidates()
	clone := plan.prepared.Dir()
	if err := os.Rename(clone, clone+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, clone, "alpha", "---\nname: alpha\ndescription: replacement\n---\n")
	if _, err := plan.Install(candidates); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"))
	if err != nil || !strings.Contains(string(got), "description: d") || strings.Contains(string(got), "description: replacement") {
		t.Fatalf("install must read the pinned clone: %q, %v", got, err)
	}
	if err := plan.Close(); err == nil {
		t.Fatal("closing a replaced scratch pathname must report the conflict")
	}
}

func TestAddPlanCloseRetriesPreparedCleanupFailure(t *testing.T) {
	s, _ := setupStore(t)
	t.Setenv("HOME", t.TempDir())
	url := makeGitSourceNamed(t, false, "alpha")
	preparation, err := PrepareAdd(s, url, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := preparation.Session.(*AddPlan)
	clone := plan.prepared.Dir()
	probe := filepath.Join(clone, "unsupported-fifo")
	if err := unix.Mkfifo(probe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plan.Close(); err == nil {
		t.Fatal("unsupported scratch content must make the first cleanup fail")
	}
	entries, err := os.ReadDir(s.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	quarantine := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".fu-src-clean-") {
			quarantine = filepath.Join(s.StagingDir(), entry.Name())
			break
		}
	}
	if quarantine == "" {
		t.Fatalf("failed close did not preserve a retryable quarantine: %v", entries)
	}
	if err := os.Remove(filepath.Join(quarantine, filepath.Base(probe))); err != nil {
		t.Fatal(err)
	}
	if err := plan.Close(); err != nil {
		t.Fatalf("retry close after removing the obstruction: %v", err)
	}
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatalf("successful retry must remove the owned scratch quarantine: %v", err)
	}
}

func TestPrepareAddFailureReturnsRecoveryPrologue(t *testing.T) {
	s, _ := setupStore(t)
	interrupted := errors.New("stop after operation commit")
	if _, err := newSkill(s, nil, "alpha", hooks{
		afterCommit: func() error { return interrupted },
	}); !errors.Is(err, interrupted) {
		t.Fatalf("setup must leave a committed transaction pending, got %v", err)
	}
	agentDir := t.TempDir()
	writeSkillTree(t, agentDir, "alpha", "---\nname: alpha\ndescription: foreign\n---\n")
	agents := []agent.Agent{fakeAgent{"claude", agentDir}}

	src, err := parseAddSource("file:///definitely/missing/fu-source.git", "")
	if err != nil {
		t.Fatal(err)
	}
	plan, prologue, err := prepareAddSource(s, "file:///definitely/missing/fu-source.git", src, agents, hooks{})
	if err == nil {
		t.Fatal("the missing git source must fail after the recovery prologue")
	}
	if plan != nil {
		t.Fatalf("a failed preparation must not return a session: %#v", plan)
	}
	if len(prologue.Foreign) != 1 || prologue.Foreign[0].Skill != "alpha" {
		t.Fatalf("the durable recovery finding must survive source failure: %+v", prologue)
	}
}

func TestPrepareAddClassifiesMalformedRefs(t *testing.T) {
	for _, ref := range []string{
		"refs/heads/main",
		strings.Repeat("0", 40),
		"bad ref",
	} {
		_, err := parseAddSource("https://example.invalid/repo.git", ref)
		if !errors.Is(err, ErrInvalidAddRef) {
			t.Fatalf("--ref %q error = %v, want ErrInvalidAddRef", ref, err)
		}
	}
}

func TestAddPlanNoSelectionSurfacesPrologueFailure(t *testing.T) {
	p := &AddPlan{prologue: Result{Failed: []FailedAction{{
		Action: Action{AgentName: "broken"},
		Err:    errors.New("scan failed"),
	}}}}
	if err := p.NoSelection(); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("no-selection verdict = %v, want ErrOperationFailed", err)
	}
	if err := (&AddPlan{}).NoSelection(); err != nil {
		t.Fatalf("a clean prologue with no selection must be an ordinary no-op: %v", err)
	}
}
