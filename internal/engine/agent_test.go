package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// undetectedAgent is an adapter fu knows about but that is not installed on
// this machine. `fu agent` lists it -- naming the agents fu supports is the
// point of the command -- but never inspects it.
type undetectedAgent struct{ name, dir string }

func (u undetectedAgent) Name() string       { return u.name }
func (u undetectedAgent) Detect() bool       { return false }
func (u undetectedAgent) SkillsDir() string  { return u.dir }
func (u undetectedAgent) Reserved() []string { return nil }

func overviewByName(t *testing.T, all []AgentOverview, name string) AgentOverview {
	t.Helper()
	for _, o := range all {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("agent %q not in %+v", name, all)
	return AgentOverview{}
}

// An agent fu supports but that is not installed is listed by name and
// nothing else: detection is what makes an agent managed (SPEC rule 4), so
// there is no directory to describe.
func TestAgentOverviewsListsAnUndetectedAgentByNameOnly(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	overviews := AgentOverviews(s, cfg, []agent.Agent{undetectedAgent{"codex", "/nonexistent"}})

	codex := overviewByName(t, overviews, "codex")
	if codex.Detected {
		t.Fatalf("codex = %+v, want not detected", codex)
	}
	if codex.SkillsDir != "" || codex.Pending != 0 || codex.Delivered != 0 {
		t.Fatalf("codex = %+v, want no inspection of an undetected agent", codex)
	}
}

// A detected agent whose skills directory does not exist yet has everything
// pending: SPEC rule 4 requires a read-only command to say so rather than
// create the directory.
func TestAgentOverviewsReportsAMissingDirAsAllPending(t *testing.T) {
	s, cfg := setupStore(t, "alpha", "beta")
	dir := filepath.Join(t.TempDir(), "skills")

	overviews := AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}})

	claude := overviewByName(t, overviews, "claude")
	if !claude.Detected || !claude.DirMissing {
		t.Fatalf("claude = %+v, want detected with a missing dir", claude)
	}
	if claude.Pending != 2 || claude.Delivered != 0 {
		t.Fatalf("claude = %+v, want both skills pending", claude)
	}
	if claude.SkillsDir != dir {
		t.Fatalf("claude.SkillsDir = %q, want %q", claude.SkillsDir, dir)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("a read-only command must not create the directory: %v", err)
	}
}

// The ordinary case: delivered links counted, an enabled skill with no link
// counted as pending, and the count of entries fu does not manage reported
// separately.
func TestAgentOverviewsCountsDeliveredPendingAndUnmanaged(t *testing.T) {
	s, cfg := setupStore(t, "alpha", "beta")
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "someone-elses"), 0o755); err != nil {
		t.Fatal(err)
	}

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if claude.Delivered != 1 || claude.Pending != 1 || claude.Unmanaged != 1 {
		t.Fatalf("claude = %+v, want 1 delivered, 1 pending, 1 unmanaged", claude)
	}
	if claude.Broken != 0 || claude.Uninspectable != 0 {
		t.Fatalf("claude = %+v, want no broken or uninspectable entries", claude)
	}
}

// A link whose store-side content is gone is counted as broken rather than
// delivered -- the name is projected, but nothing is behind it.
func TestAgentOverviewsCountsABrokenLinkAsBroken(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if claude.Broken != 1 || claude.Delivered != 0 {
		t.Fatalf("claude = %+v, want the link counted as broken, not delivered", claude)
	}
}

// A skills directory that is itself a symlink is refused wholesale by
// reconcile (SPEC rule 10), so the overview says that instead of counting
// entries fu will never touch.
func TestAgentOverviewsReportsASymlinkedSkillsDir(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "skills")
	if err := os.Symlink(real, dir); err != nil {
		t.Fatal(err)
	}

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if !claude.DirIsSymlink {
		t.Fatalf("claude = %+v, want the symlinked dir reported", claude)
	}
	if claude.Pending != 0 || claude.Delivered != 0 {
		t.Fatalf("claude = %+v, want no counts for a refused agent", claude)
	}
}

// An entry the scan cannot inspect is counted in its own column, not folded
// into unmanaged: fu does not know what it is (batch 5's KindUnknown).
func TestAgentOverviewsCountsAnUninspectableEntry(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	uninspectableFuLink(t, dir, s.SkillsDir(), "loop")

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if claude.Uninspectable != 1 {
		t.Fatalf("claude = %+v, want the uninspectable entry counted", claude)
	}
	if claude.Unmanaged != 0 || claude.Delivered != 0 {
		t.Fatalf("claude = %+v, want it in no other column", claude)
	}
}

// A skills directory that cannot be scanned at all keeps its failure on the
// agent, exactly as fu status does, and costs no other agent its row.
func TestAgentOverviewsIsolatesAnUnscannableAgent(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	broken := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(broken, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	healthy := t.TempDir()

	overviews := AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", broken}, fakeAgent{"codex", healthy}})
	if got := overviewByName(t, overviews, "claude"); got.ScanErr == "" {
		t.Fatalf("claude = %+v, want the scan failure recorded", got)
	}
	if got := overviewByName(t, overviews, "codex"); got.ScanErr != "" || got.Pending != 1 {
		t.Fatalf("codex = %+v, want an unaffected row", got)
	}
}

// The overview is read-only in the strict sense SPEC §9 requires: it takes no
// lock, so it runs while another command holds fu.lock.
//
// The lock has to be genuinely held for this to discriminate. Holding a write
// session is not enough -- BeginWrite only opens pinned descriptors, and
// withLock is what takes the flock -- so an earlier version of this test
// passed whether or not AgentOverviews locked.
func TestAgentOverviewsTakesNoLock(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	homeRoot, err := session.Store.Root()
	if err != nil {
		t.Fatal(err)
	}

	held, release := make(chan struct{}), make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- withLock(homeRoot, "fu.lock", s.LockPath(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		close(release)
		if err := <-locked; err != nil {
			t.Error(err)
		}
	}()
	// The test carries its own proof that the lock is really held, so it can
	// never silently degrade into asserting nothing: a non-blocking exclusive
	// acquisition of the same file must be refused right now.
	probe, err := unix.Open(s.LockPath(), unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(probe)
	if err := unix.Flock(probe, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("precondition: fu.lock must be held for this test to mean anything, got %v", err)
	}

	done := make(chan []AgentOverview, 1)
	go func() { done <- AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", t.TempDir()}}) }()
	select {
	case overviews := <-done:
		if len(overviews) != 1 {
			t.Fatalf("overviews = %+v, want one row", overviews)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AgentOverviews blocked on fu.lock")
	}
}

// Application.Agents lists every adapter fu knows, not only the detected
// ones, so `fu agent` can say an agent is supported but not installed.
func TestApplicationAgentsListsEveryKnownAdapter(t *testing.T) {
	home := t.TempDir()
	if _, err := store.Init(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FU_HOME", home)
	t.Setenv("HOME", t.TempDir())

	outcome, err := (&Application{}).Agents()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, o := range outcome.Agents {
		names[o.Name] = true
	}
	for _, a := range agent.All() {
		if !names[a.Name()] {
			t.Fatalf("agents = %+v, want %q listed", outcome.Agents, a.Name())
		}
	}
}

// Result counts what reconcile actually did to the agent directories, which
// is what lets a CLI say a change takes effect without claiming so after a
// pass that changed nothing.
func TestReconcileCountsLinksCreatedAndRemoved(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}

	res, err := Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 || res.Removed != 0 {
		t.Fatalf("first pass = %d created, %d removed; want 1 and 0", res.Created, res.Removed)
	}

	// Idempotent: the second pass has nothing left to do and must say so.
	res, err = Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Removed != 0 {
		t.Fatalf("second pass = %d created, %d removed; want a no-op", res.Created, res.Removed)
	}

	cfg.SetEnabled("alpha", false)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	res, err = Reconcile(s, agents)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Removed != 1 {
		t.Fatalf("after disabling = %d created, %d removed; want 0 and 1", res.Created, res.Removed)
	}
}

// A CreateLink the store cannot satisfy is reported, not counted: nothing was
// projected, so nothing takes effect in a new session.
func TestReconcileDoesNotCountALinkItCouldNotCreate(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}

	res, err := Reconcile(s, []agent.Agent{fakeAgent{"claude", t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 {
		t.Fatalf("created = %d, want 0 for a link the store could not supply", res.Created)
	}
	if len(res.Missing) != 1 {
		t.Fatalf("missing = %+v, want the one unsatisfiable link", res.Missing)
	}
}

// mergeResult accumulates the counters, so a batch command's total is the sum
// of its constituent passes rather than the last one's.
func TestMergeResultAccumulatesTheDeliveryCounters(t *testing.T) {
	var dst Result
	mergeResult(&dst, Result{Created: 2, Removed: 1})
	mergeResult(&dst, Result{Created: 3, Removed: 4})
	if dst.Created != 5 || dst.Removed != 5 {
		t.Fatalf("dst = %d created, %d removed; want 5 and 5", dst.Created, dst.Removed)
	}
}

// `restore --hard` reconciles twice and the second pass's Result replaces the
// first's, because the first describes a state the reset has since changed.
// The delivery counters are the exception: they record what this command did,
// not what the store now looks like, and the second pass cannot regenerate a
// link the first pass already created. Dropping them made a run that rebuilt
// a link report no effect at all.
func TestRestoreHardKeepsTheLinksTheFirstPassCreated(t *testing.T) {
	s, _ := setupStore(t)
	dir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", dir}}
	if _, err := NewSkill(s, agents, "alpha"); err != nil {
		t.Fatal(err)
	}
	// The link the first pass must rebuild.
	if err := os.Remove(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	// A tracked edit, so the reset moves a path and the second pass runs.
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "alpha", "SKILL.md"), []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(s, agents, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Reset) == 0 {
		t.Fatal("precondition: the reset must have moved a path, so the second pass runs")
	}
	if outcome.Result.Created != 1 {
		t.Fatalf("created = %d, want the link the first pass rebuilt still counted", outcome.Result.Created)
	}
}

// adopt drops its prologue's reconcile result once a per-skill pass has run,
// because the prologue describes the state before adoption and the later pass
// is authoritative about it. The delivery counters are not a description of
// that state: they record links the prologue itself created, which no later
// pass can regenerate. Dropping them under-reported a run that projected two
// links as having projected one.
func TestAdoptKeepsTheLinksItsPrologueCreated(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir := t.TempDir()
	writeSkillTree(t, dir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")

	res, err := Adopt(s, []agent.Agent{fakeAgent{"claude", dir}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 {
		t.Fatalf("precondition: pdf-tools must be adopted, got %+v", res.Adopted)
	}
	// alpha's link comes from the prologue, pdf-tools' from its own operation.
	if res.Reconcile.Created != 2 {
		t.Fatalf("created = %d, want both the prologue's link and the adopted one", res.Reconcile.Created)
	}
}

// A link fu intends to take away is as much outstanding work as one it
// intends to create. A skill disabled in fu.yaml whose link is still in
// place, and a residual link for a name fu.yaml no longer mentions, both
// leave the agent out of sync -- reporting them as delivered with nothing
// pending answered "is this agent up to date?" with yes.
func TestAgentOverviewsCountsALinkItIntendsToRemove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*testing.T, *store.Config)
	}{
		{"disabled in fu.yaml", func(t *testing.T, cfg *store.Config) {
			cfg.SetEnabled("alpha", false)
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
		}},
		{"no longer mentioned", func(t *testing.T, cfg *store.Config) {
			cfg.RemoveSkill("alpha")
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg := setupStore(t, "alpha")
			dir := t.TempDir()
			if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(dir, "alpha")); err != nil {
				t.Fatal(err)
			}
			tc.arrange(t, cfg)

			claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
			if claude.Pending != 1 {
				t.Fatalf("claude = %+v, want the removal counted as outstanding", claude)
			}
		})
	}
}

// A run that projected a link is not a run in which nothing happened. The two
// predicates answer different questions and only one of them is about the
// run: Result.Empty is about findings, so restore --hard's second pass can
// resolve everything and correctly report nothing, while AdoptResult.Empty
// decides whether to print "nothing to adopt" and must not say that after
// adopt's prologue put a link in place.
func TestAdoptResultIsNotEmptyWhenItChangedALink(t *testing.T) {
	if (AdoptResult{Reconcile: Result{Created: 1}}).Empty() {
		t.Fatal("a run that created a link has something to say")
	}
	if (AdoptResult{Reconcile: Result{Removed: 1}}).Empty() {
		t.Fatal("a run that removed a link has something to say")
	}
	if !(AdoptResult{}).Empty() {
		t.Fatal("a run that did nothing and found nothing is empty")
	}
	if !(Result{Created: 1}).Empty() {
		t.Fatal("Result.Empty is about findings; a link created is not one")
	}
}

// The same defect end to end: an agent detected after its skills were
// registered has a link waiting, adopt's prologue creates it, and adopt has
// no candidate of its own -- so the run both reports an effect and claims
// nothing happened.
func TestAdoptDoesNotCallAProjectingRunEmpty(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	dir := t.TempDir()

	res, err := Adopt(s, []agent.Agent{fakeAgent{"claude", dir}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reconcile.Created != 1 {
		t.Fatalf("precondition: the prologue must have created alpha's link, got %+v", res.Reconcile)
	}
	if res.Empty() {
		t.Fatal("a run that projected a link must not be reported as nothing to adopt")
	}
}

// A broken link is outstanding work even though its content is gone: the next
// write command retires the link and only then reports the store-side gap, so
// the name does change. A name the store cannot supply and that is not there
// to remove either is the case where nothing changes.
func TestAgentOverviewsCountsABrokenLinkAsPendingButNotAnUnsuppliableOne(t *testing.T) {
	s, cfg := setupStore(t, "alpha", "beta")
	dir := t.TempDir()
	// alpha: linked, then its store content deleted -- a broken link.
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "alpha")); err != nil {
		t.Fatal(err)
	}
	// beta: never linked, and its store content is gone too.
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "beta")); err != nil {
		t.Fatal(err)
	}

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if claude.Pending != 1 {
		t.Fatalf("claude = %+v, want only alpha's retirement counted", claude)
	}
	if claude.Broken != 1 {
		t.Fatalf("claude = %+v, want the broken link reported as such too", claude)
	}
}

// A rebuild is one link changing, not two: Diff emits RemoveLink+CreateLink
// for a link fu will respell, and counting both would say an agent with one
// stale link has two changes waiting.
func TestAgentOverviewsCountsARebuildOnce(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	dir := t.TempDir()
	// The state DESIGN describes: a dotfiles tool moved $FU_HOME and left a
	// link pointing back through the old spelling. The leaf is the skill's
	// own name and the directory resolves to this store, so fu still owns the
	// link -- it just no longer reads as the canonical path, so Diff rebuilds.
	alias := filepath.Join(t.TempDir(), "alias")
	fuHome := filepath.Dir(filepath.Dir(s.SkillsDir()))
	if err := os.Symlink(fuHome, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(alias, "store", "skills", "alpha"), filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if claude.Pending != 1 {
		t.Fatalf("claude = %+v, want the rebuild counted once", claude)
	}
}

// A desired link fu refuses to create is not pending -- fu will make no
// change -- but the row must not read as up to date because of it. Neither
// state shows in the counts a reader scans: a blocked name is one more
// Unmanaged entry, and one the store cannot supply appears nowhere.
func TestAgentOverviewsSeparatesBlockedAndUnavailableFromPending(t *testing.T) {
	s, cfg := setupStore(t, "alpha", "beta", "gamma")
	dir := t.TempDir()
	// alpha: delivered.
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	// beta: its path is held by content fu did not create.
	if err := os.MkdirAll(filepath.Join(dir, "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	// gamma: enabled, never linked, and the store no longer holds it.
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), "gamma")); err != nil {
		t.Fatal(err)
	}

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if claude.Blocked != 1 || claude.Unavailable != 1 {
		t.Fatalf("claude = %+v, want one blocked and one unavailable", claude)
	}
	if claude.Pending != 0 {
		t.Fatalf("claude = %+v, want neither counted as pending: fu will change nothing", claude)
	}
	if claude.Delivered != 1 {
		t.Fatalf("claude = %+v, want alpha still counted as delivered", claude)
	}
}

// The two adopt shapes must report the same outcome the same way. The
// whole-directory form materialises its links inside its own switch
// transaction, so the trailing reconcile finds a directory with nothing left
// to do -- it projected links and then reported no effect, while the
// per-entry form of the same adoption reported one.
func TestAdoptCountsAWholeDirectorySwitchAsAProjection(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	claude := fakeAgent{"claude", filepath.Join(homeDir, ".claude", "skills")}

	res, err := Adopt(s, []agent.Agent{claude}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("precondition: pdf-tools must be adopted through the switch, got %+v", res.Adopted)
	}
	if res.Reconcile.Created == 0 {
		t.Fatalf("a completed whole-directory switch projects links: %+v", res.Reconcile)
	}
}

// A name fu will never link is not outstanding work. Both kinds are excluded
// by Desired before Diff ever sees them -- reserved because the agent claims
// the name (SPEC rule 11), invalid because fu refuses to manage that spelling
// -- so neither can inflate the count of changes the next write command makes.
// `fu status` names them; the overview's business is the projection.
func TestAgentOverviewsCountsNeitherAReservedNorAnInvalidName(t *testing.T) {
	s, cfg := setupStore(t, "alpha")
	// fakeAgent reserves ".system"; "Bad_Name" fails skill name validation.
	// Written straight into the config: both are names AddSkill is entitled
	// to refuse, and what matters here is a config that already holds them.
	for _, name := range []string{".system", "Bad_Name"} {
		if err := cfg.AddSkill(name, "sha256:x"); err != nil {
			t.Skipf("config refuses %q outright, so this state is unreachable: %v", name, err)
		}
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}

	// Without this the test could pass because the config quietly dropped
	// both names, proving nothing about how the overview treats them.
	_, reserved, invalid := Desired(cfg, fakeAgent{"claude", dir})
	if len(reserved) != 1 || len(invalid) != 1 {
		t.Fatalf("precondition: want one reserved and one invalid name, got %+v and %+v", reserved, invalid)
	}

	claude := overviewByName(t, AgentOverviews(s, cfg, []agent.Agent{fakeAgent{"claude", dir}}), "claude")
	if claude.Pending != 0 {
		t.Fatalf("claude = %+v, want no pending work for names fu will never link", claude)
	}
	if claude.Delivered != 1 {
		t.Fatalf("claude = %+v, want alpha still delivered", claude)
	}
}
