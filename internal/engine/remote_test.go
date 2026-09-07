package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

func newEmptyBareRepo(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	return bare
}

// storeWithBareRemote is setupStore plus a fresh bare remote already set.
func storeWithBareRemote(t *testing.T, skills ...string) (*store.Store, string) {
	t.Helper()
	s, _ := setupStore(t, skills...)
	bare := newEmptyBareRepo(t)
	if _, err := s.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	return s, bare
}

func TestSetRemoteRecordsTheURLAndReportsThePreviousOne(t *testing.T) {
	s, _ := setupStore(t)
	first := newEmptyBareRepo(t)
	out, err := SetRemote(s, first)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Configured || out.URL != first || out.Previous != "" {
		t.Fatalf("SetRemote outcome = %+v", out)
	}
	second := newEmptyBareRepo(t)
	out, err = SetRemote(s, second)
	if err != nil || out.Previous != first || out.URL != second {
		t.Fatalf("overwrite outcome = %+v err=%v", out, err)
	}
	url, configured, err := s.Remote()
	if err != nil || !configured || url != second {
		t.Fatalf("store must hold the new remote, got %q %v %v", url, configured, err)
	}
	entries, err := s.Log(1)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Message != "init: store" {
		t.Fatalf("SetRemote must record no commit, log head = %q", entries[0].Message)
	}
}

func TestSetRemoteRejectsAnEmptyURLWithoutTouchingTheStore(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := SetRemote(s, ""); err == nil {
		t.Fatal("an empty url must be refused")
	}
	if _, configured, _ := s.Remote(); configured {
		t.Fatal("a refused url must leave the remote unconfigured")
	}
}

// remoteHeadMessage reads the subject of the commit the remote's copy of the
// store's branch points at.
func remoteHeadMessage(t *testing.T, bare string, s *store.Store) string {
	t.Helper()
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(head.Name(), true)
	if err != nil {
		t.Fatalf("read %s on the remote: %v", head.Name(), err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(commit.Message)
}

func TestPushSweepsHandEditsBeforePushing(t *testing.T) {
	s, bare := storeWithBareRemote(t, "alpha")
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "notes.md"), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := PushOperations(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.UpToDate || out.URL != bare || out.Branch == "" || out.Head == "" {
		t.Fatalf("push outcome = %+v", out)
	}
	if msg := remoteHeadMessage(t, bare, s); msg != store.ExternalCommitMessage {
		t.Fatalf("the sweep commit must be what push sends, remote head = %q", msg)
	}
	cloned, err := store.Clone(context.Background(), t.TempDir(), bare)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(cloned.SkillsDir(), "alpha", "notes.md"))
	if err != nil || string(content) != "draft" {
		t.Fatalf("the hand edit must have reached the remote, got %q err=%v", content, err)
	}
	again, err := PushOperations(s, nil)
	if err != nil || !again.UpToDate {
		t.Fatalf("a second push must be up to date, got %+v err=%v", again, err)
	}
}

func TestPushWithoutARemoteFails(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := PushOperations(s, nil); !errors.Is(err, ErrNoRemote) {
		t.Fatalf("push without a remote must report ErrNoRemote, got %v", err)
	}
}

func TestPushReportsDivergenceAndPointsAtPull(t *testing.T) {
	a, bare := storeWithBareRemote(t)
	// Divergence has to be structural rather than a matter of timing: two Init
	// stores write the same bootstrap config and sign with the same fixed
	// identity, so roots created in the same second collide. Giving each side a
	// commit the other lacks makes the push a non-fast-forward either way.
	if _, err := NewSkill(a, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := setupStore(t)
	if _, err := b.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(b, nil, "beta"); err != nil {
		t.Fatal(err)
	}
	_, err := PushOperations(b, nil)
	if !errors.Is(err, store.ErrRemoteDiverged) || !strings.Contains(err.Error(), "run fu pull first") {
		t.Fatalf("a remote that is ahead must be reported with the remedy, got %v", err)
	}
}

func TestPullFastForwardsAndRebuildsLinks(t *testing.T) {
	a, bare := storeWithBareRemote(t)
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	b, err := store.Clone(context.Background(), t.TempDir(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(a, nil, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	claudeDir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}}
	out, err := PullOperations(b, agents)
	if err != nil {
		t.Fatal(err)
	}
	if out.UpToDate || out.EmptyRemote || out.URL != bare || len(out.Changed) == 0 || out.From == out.To {
		t.Fatalf("pull outcome = %+v", out)
	}
	target, err := os.Readlink(filepath.Join(claudeDir, "writer"))
	if err != nil {
		t.Fatalf("pull must rebuild the link for the skill that arrived: %v", err)
	}
	if !strings.HasPrefix(target, b.SkillsDir()) {
		t.Fatalf("link must point into the pulled store, got %s", target)
	}
	again, err := PullOperations(b, agents)
	if err != nil || !again.UpToDate || again.From != again.To {
		t.Fatalf("a second pull must be up to date, got %+v err=%v", again, err)
	}
}

func TestPullReportsDivergenceAndKeepsTheLocalSweep(t *testing.T) {
	a, bare := storeWithBareRemote(t)
	if _, err := NewSkill(a, nil, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	claudeDir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}}
	b, err := store.Clone(context.Background(), t.TempDir(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PullOperations(b, agents); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.SkillsDir(), "writer", "notes.md"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SetGlobal(a, nil, "writer", false); err != nil {
		t.Fatal(err)
	}
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	_, err = PullOperations(b, agents)
	if !errors.Is(err, store.ErrRemoteDiverged) {
		t.Fatalf("a hand edit beside a remote change must be reported as divergence, got %v", err)
	}
	// The SPEC row promises the store path and a suggested command, not merely
	// the refusal; pin the wording the design settled on.
	for _, want := range []string{b.Dir(), "git -C " + b.Dir() + " rebase origin/", "then run fu restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the divergence error must carry %q, got %q", want, err.Error())
		}
	}
	entries, err := b.Log(1)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Message != store.ExternalCommitMessage {
		t.Fatalf("the sweep must have recorded the hand edit before pull stopped, log head = %q", entries[0].Message)
	}
	if _, err := os.Readlink(filepath.Join(claudeDir, "writer")); err != nil {
		t.Fatal("a refused pull must leave the links as they were")
	}
}

func TestPullFromAnEmptyRemoteIsNotAnError(t *testing.T) {
	s, bare := storeWithBareRemote(t)
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(s.SkillsDir(), "alpha", "notes.md")
	if err := os.WriteFile(notes, []byte("pending hand edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	acquired := 0
	lockAcquiredHook = func(string) { acquired++ }
	t.Cleanup(func() { lockAcquiredHook = nil })
	out, err := PullOperations(s, nil)
	if err != nil || !out.EmptyRemote || out.URL != bare {
		t.Fatalf("an empty remote must be reported, not failed: %+v err=%v", out, err)
	}
	if acquired != 0 {
		t.Errorf("an empty-remote pull must not take fu.lock, acquisitions = %d", acquired)
	}
	after, err := s.Repo.Head()
	if err != nil || after.Hash() != before.Hash() {
		t.Errorf("an empty-remote pull must not sweep the hand edit, before=%v after=%v err=%v", before, after, err)
	}
	if content, err := os.ReadFile(notes); err != nil || string(content) != "pending hand edit" {
		t.Errorf("the pending hand edit must remain on disk: %q err=%v", content, err)
	}
}

func TestPushReportsLocalPreconditionFailureWithoutRemoteContext(t *testing.T) {
	s, _ := storeWithBareRemote(t)
	head, err := s.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, head.Hash())); err != nil {
		t.Fatal(err)
	}
	out, err := PushOperations(s, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "HEAD is detached;") {
		t.Fatalf("a detached HEAD must be reported as a local precondition failure, got %v", err)
	}
	if out.Completed {
		t.Fatalf("a local precondition failure must not complete the push: %+v", out)
	}
}

func TestPushTransportFailureStillNamesTheRemote(t *testing.T) {
	s, _ := setupStore(t)
	missing := filepath.Join(t.TempDir(), "missing.git")
	if _, err := s.SetRemote(missing); err != nil {
		t.Fatal(err)
	}
	out, err := PushOperations(s, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "push to "+missing+": ") {
		t.Fatalf("a transport failure must still name the remote, got %v", err)
	}
	if out.Completed || out.Branch == "" {
		t.Fatalf("a transport failure has branch metadata but is incomplete: %+v", out)
	}
}

func TestPullFailsWhenTheRemoteLacksTheStoresBranch(t *testing.T) {
	a, bare := storeWithBareRemote(t)
	head, err := a.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := a.Repo.Remote("origin")
	if err != nil {
		t.Fatal(err)
	}
	// The remote holds commits, but only under a name no store's HEAD uses.
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(head.Name().String() + ":refs/heads/elsewhere")},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := setupStore(t)
	if _, err := b.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := PullOperations(b, nil); !errors.Is(err, ErrNoRemoteBranch) {
		t.Fatalf("a remote without the store's branch must report ErrNoRemoteBranch, got %v", err)
	}
}

func TestPullWithoutARemoteFails(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := PullOperations(s, nil); !errors.Is(err, ErrNoRemote) {
		t.Fatalf("pull without a remote must report ErrNoRemote, got %v", err)
	}
}

func TestCloneStoreReconcilesDetectedAgents(t *testing.T) {
	a, bare := storeWithBareRemote(t)
	if _, err := NewSkill(a, nil, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := SetAgentSwitch(a, nil, "writer", "codex", false); err != nil {
		t.Fatal(err)
	}
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	homeB := t.TempDir()
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", claudeDir}, fakeAgent{"codex", codexDir}}
	out, err := CloneStore(homeB, bare, agents)
	if err != nil {
		t.Fatal(err)
	}
	if out.Skills != 1 || out.URL != bare || out.Home != homeB || out.Branch == "" {
		t.Fatalf("clone outcome = %+v", out)
	}
	if _, err := os.Readlink(filepath.Join(claudeDir, "writer")); err != nil {
		t.Fatalf("clone must link the enabled skill for claude: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(codexDir, "writer")); !os.IsNotExist(err) {
		t.Fatal("the codex override travelled with fu.yaml, so codex must get no link")
	}
	cloned, err := store.Open(homeB)
	if err != nil {
		t.Fatal(err)
	}
	url, configured, err := cloned.Remote()
	if err != nil || !configured || url != bare {
		t.Fatalf("the clone must carry its origin, got %q %v %v", url, configured, err)
	}
}

func TestCloneStoreRefusesAnExistingStore(t *testing.T) {
	s, bare := storeWithBareRemote(t)
	if _, err := PushOperations(s, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CloneStore(s.Home, bare, nil); !errors.Is(err, store.ErrStoreExists) {
		t.Fatalf("cloning into a home that has a store must report ErrStoreExists, got %v", err)
	}
}

func TestCloneScratchIsCountedAsUncollectableByStatus(t *testing.T) {
	s, cfg := setupStore(t)
	if err := os.MkdirAll(filepath.Join(s.StagingDir(), ".fu-clone-0011223344"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Status(s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (StagingInventory{Uncollectable: 1}); report.Staging != want {
		t.Fatalf("a clone scratch must land in the bucket nothing collects, got %+v want %+v", report.Staging, want)
	}
}

// TestPullRefusesAfterSwitchingToARemoteWithoutTheBranch pins that a
// tracking ref left behind by a previous remote cannot stand in for the
// current one. SetRemote rewrites only .git/config, so refs/remotes/origin/*
// from the first remote survive the switch; without pruning, FastForward
// would read the stale ref and report "up to date" against a remote that
// has no such branch at all.
func TestPullRefusesAfterSwitchingToARemoteWithoutTheBranch(t *testing.T) {
	a, _ := storeWithBareRemote(t)
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	second := newEmptyBareRepo(t)
	if _, err := a.SetRemote(second); err != nil {
		t.Fatal(err)
	}
	head, err := a.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := a.Repo.Remote("origin")
	if err != nil {
		t.Fatal(err)
	}
	// The second remote holds commits, but only under a name no store's HEAD
	// uses, so the store's branch has nothing to follow there.
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(head.Name().String() + ":refs/heads/elsewhere")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := PullOperations(a, nil); !errors.Is(err, ErrNoRemoteBranch) {
		t.Fatalf("after switching to a remote without the store's branch, pull must report ErrNoRemoteBranch, got %v", err)
	}
}

// failingSkillsAgent is an agent whose skills path is a plain file, so
// ScanAgent fails for it with a genuine (non-precondition) error and
// reconcile isolates it into Result.Failed -- the same fixture the CLI's
// exit-code tests use for the sibling commands.
func failingSkillsAgent(t *testing.T) agent.Agent {
	t.Helper()
	skills := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(skills, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fakeAgent{"claude", skills}
}

// TestPushReportsAPerAgentReconcileFailure pins the closing boundary every
// prologue-only command needs. writeCommandPrologue carries a per-agent
// reconcile failure in its Result and leaves the exit status to the caller
// (pipeline.go), and update and adopt each end with this same check; push
// without it printed "failed: claude: ..." and then "pushed ...", and exited
// 0, so a script reading $? saw success while an agent received nothing.
func TestPushReportsAPerAgentReconcileFailure(t *testing.T) {
	s, bare := storeWithBareRemote(t, "alpha")
	out, err := PushOperations(s, []agent.Agent{failingSkillsAgent(t)})
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("a per-agent reconcile failure must make push report ErrOperationFailed, got %v", err)
	}
	if len(out.Result.Failed) == 0 {
		t.Fatal("the failure must still be carried in the result for the CLI to print")
	}
	// The push itself still happened: the durable part is confirmed on the
	// remote, exactly as `fu new` confirms its commit beside the same failure.
	if out.UpToDate || out.Head == "" {
		t.Fatalf("the push must have completed before the failure is reported, got %+v", out)
	}
	if msg := remoteHeadMessage(t, bare, s); msg != store.ExternalCommitMessage {
		t.Fatalf("the swept commit must have reached the remote regardless, remote head = %q", msg)
	}
}

// TestCloneStoreNamesTheRemedyWhenTheLinkRebuildFails pins that a clone whose
// store landed but whose link rebuild failed says so: by then $FU_HOME/store
// is fully in place and fu restore finishes the job, and without the wrap a
// user re-running clone hits "store already exists" with no idea why.
func TestCloneStoreNamesTheRemedyWhenTheLinkRebuildFails(t *testing.T) {
	a, bare := storeWithBareRemote(t)
	if _, err := NewSkill(a, nil, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := PushOperations(a, nil); err != nil {
		t.Fatal(err)
	}
	homeB := t.TempDir()
	out, err := CloneStore(homeB, bare, []agent.Agent{failingSkillsAgent(t)})
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("a per-agent reconcile failure must surface as ErrOperationFailed, got %v", err)
	}
	for _, want := range []string{filepath.Join(homeB, "store"), "run fu restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must say the store landed and name the remedy; want %q in %q", want, err.Error())
		}
	}
	if out.Home != homeB {
		t.Fatalf("outcome must still carry the home, got %+v", out)
	}
	if _, err := store.Open(homeB); err != nil {
		t.Fatalf("the store must be fully in place despite the link failure: %v", err)
	}
}

// TestRemoteWithoutArgumentsTakesNoLock pins SPEC §9's read-only claim for
// the argument-less form the way restore_test pins it for the report: the
// lock hook must never fire.
func TestRemoteWithoutArgumentsTakesNoLock(t *testing.T) {
	fuHome, home := t.TempDir(), t.TempDir()
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", home)
	app := NewApplication()
	if _, err := app.Initialize(); err != nil {
		t.Fatal(err)
	}
	acquired := 0
	lockAcquiredHook = func(string) { acquired++ }
	t.Cleanup(func() { lockAcquiredHook = nil })
	out, err := app.Remote()
	if err != nil || out.Configured {
		t.Fatalf("a fresh store must report no remote, got %+v err=%v", out, err)
	}
	if acquired != 0 {
		t.Fatalf("fu remote without arguments is read-only and must not take fu.lock, acquisitions = %d", acquired)
	}
}
