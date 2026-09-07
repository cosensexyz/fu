package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// ErrNoRemote is the answer push and pull give before `fu remote <url>`.
var ErrNoRemote = errors.New("no remote configured; set one with `fu remote <url>`")

// ErrNoRemoteBranch means the remote has commits but none on the branch this
// store's HEAD points at, so there is nothing a fast-forward could follow.
var ErrNoRemoteBranch = errors.New("remote has no branch matching the store's branch")

// remoteSyncTimeout bounds every network transfer of the store itself. It is
// wider than the two minutes a skill source gets (internal/source) because
// this is the user's own history rather than a third-party repository, and
// no byte budget applies for the same reason.
const remoteSyncTimeout = 10 * time.Minute

type RemoteOutcome struct {
	URL        string
	Configured bool
	Previous   string // url SetRemote replaced; "" when there was none
}

type PushOutcome struct {
	Result    Result
	URL       string
	Branch    string
	Head      string // short hash of the commit pushed
	UpToDate  bool
	Completed bool // Push succeeded, including UpToDate, independently of reconcile.
}

type PullOutcome struct {
	Result      Result
	URL         string
	Branch      string
	From, To    string // short hashes; equal when UpToDate
	UpToDate    bool
	EmptyRemote bool
	Changed     []string
	Completed   bool // FastForward succeeded, including UpToDate, independently of reconcile.
}

type CloneOutcome struct {
	Result Result
	Home   string
	URL    string
	Branch string
	Skills int
}

func shortHash(h plumbing.Hash) string {
	if h.IsZero() {
		return ""
	}
	return h.String()[:7]
}

// Remote reports the store's remote. Read-only: no lock, nothing written
// (SPEC §9).
func (a *Application) Remote() (RemoteOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return RemoteOutcome{}, err
	}
	url, configured, err := st.Remote()
	return RemoteOutcome{URL: url, Configured: configured}, err
}

func (a *Application) SetRemote(url string) (RemoteOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return RemoteOutcome{}, err
	}
	return SetRemote(st, url)
}

// SetRemote takes fu.lock but neither recovers nor sweeps: it changes
// store/.git/config only, never store content, so it is no operation in
// SPEC §5.3's sense and records no commit -- the same footing `fu gc` has.
func SetRemote(st *store.Store, url string) (outcome RemoteOutcome, retErr error) {
	session, err := st.BeginWrite()
	if err != nil {
		return outcome, fmt.Errorf("open checked remote session: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, session.Close()) }()
	homeRoot, err := session.Store.Root()
	if err != nil {
		return outcome, err
	}
	retErr = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		previous, err := session.Store.SetRemote(url)
		if err != nil {
			return err
		}
		storedURL, configured, err := session.Store.Remote()
		outcome = RemoteOutcome{URL: storedURL, Configured: configured, Previous: previous}
		if err != nil {
			return err
		}
		return nil
	})
	return outcome, retErr
}

func (a *Application) Push() (PushOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return PushOutcome{}, err
	}
	return PushOperations(st, a.detectedAgents())
}

// PushOperations records pending hand edits and pushes the branch. The
// prologue holds fu.lock for the recovery, the sweep and the reconcile, and
// releases it before the transfer, so a slow network never blocks another fu
// command -- the boundary update draws as well (DESIGN §6). A commit landing
// between the two is fu's own and simply rides along.
func PushOperations(st *store.Store, agents []agent.Agent) (PushOutcome, error) {
	url, configured, err := st.Remote()
	if err != nil {
		return PushOutcome{}, err
	}
	if !configured {
		return PushOutcome{}, ErrNoRemote
	}
	outcome := PushOutcome{URL: url}
	prologue, err := writeCommandPrologue(st, agents)
	mergeResult(&outcome.Result, prologue)
	if err != nil {
		return outcome, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteSyncTimeout)
	defer cancel()
	pushed, err := st.Push(ctx)
	outcome.Branch, outcome.Head, outcome.UpToDate = pushed.Branch, shortHash(pushed.Head), pushed.UpToDate
	if errors.Is(err, store.ErrRemoteDiverged) {
		return outcome, fmt.Errorf("%w; run fu pull first", err)
	}
	if err != nil {
		// Push leaves Branch empty when its local HEAD checks fail before
		// the transport starts; those errors need no remote context.
		if pushed.Branch == "" {
			return outcome, err
		}
		return outcome, fmt.Errorf("push to %s: %w", url, err)
	}
	outcome.Completed = true
	// The prologue's failures are this command's to report. writeCommandPrologue
	// deliberately does not raise ErrOperationFailed for a per-agent reconcile
	// failure -- it carries the finding in the Result and leaves the exit
	// status to the caller (pipeline.go) -- and push has no later boundary of
	// its own that would raise it. The push above already succeeded, so the
	// durable part is done; this only keeps $? honest, the same way update
	// and adopt end (application.go, adopt.go).
	if len(outcome.Result.Failed) != 0 {
		return outcome, ErrOperationFailed
	}
	return outcome, nil
}

func (a *Application) Pull() (PullOutcome, error) {
	st, err := a.openStore()
	if err != nil {
		return PullOutcome{}, err
	}
	return PullOperations(st, a.detectedAgents())
}

// PullOperations fetches outside the lock, then under it recovers, sweeps,
// fast-forwards and reconciles -- RevertOperations' shape. The sweep comes
// first because SPEC §5.3 wants hand edits in history before pull acts; when
// the remote moved as well, that sweep commit is exactly what makes the two
// branches diverge, and the error says so rather than merging.
func PullOperations(st *store.Store, agents []agent.Agent) (outcome PullOutcome, retErr error) {
	url, configured, err := st.Remote()
	if err != nil {
		return outcome, err
	}
	if !configured {
		return outcome, ErrNoRemote
	}
	outcome.URL = url
	ctx, cancel := context.WithTimeout(context.Background(), remoteSyncTimeout)
	defer cancel()
	if _, err := st.Fetch(ctx); err != nil {
		if errors.Is(err, store.ErrRemoteEmpty) {
			outcome.EmptyRemote = true
			return outcome, nil
		}
		return outcome, fmt.Errorf("fetch from %s: %w", url, err)
	}
	session, err := st.BeginWrite()
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = errors.Join(retErr, session.Close()) }()
	checked := session.Store
	homeRoot, err := checked.Root()
	if err != nil {
		return outcome, fmt.Errorf("use checked pull root: %w", err)
	}
	storeRoot, err := checked.StoreRoot()
	if err != nil {
		return outcome, fmt.Errorf("use checked store root for pull: %w", err)
	}
	retErr = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		recoveryResult, err := RecoverPendingReporting(checked)
		mergeResult(&outcome.Result, recoveryResult)
		if err != nil {
			return fmt.Errorf("recover pending transactions before pull: %w", err)
		}
		cfg, err := store.LoadConfigRoot(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("load config %s for pull: %w", st.ConfigPath(), err)
		}
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check config writable before pull: %w", err)
		}
		if err := session.CheckCanonicalPath(); err != nil {
			return err
		}
		if err := checked.Sweep(); err != nil {
			return err
		}
		ff, err := checked.FastForward()
		outcome.Branch, outcome.From, outcome.To, outcome.Changed = ff.Branch, shortHash(ff.From), shortHash(ff.To), ff.Changed
		if errors.Is(err, store.ErrRemoteDiverged) {
			return fmt.Errorf("%w; fu does not merge. Resolve it in %s with git (for example: git -C %s rebase origin/%s), then run fu restore",
				err, st.Dir(), st.Dir(), ff.Branch)
		}
		if err != nil {
			return err
		}
		if ff.NoRemoteBranch {
			return fmt.Errorf("%w: %s has no branch %s", ErrNoRemoteBranch, url, ff.Branch)
		}
		outcome.UpToDate = ff.UpToDate
		outcome.Completed = true
		cfg, err = store.LoadConfigRoot(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("reload config %s after pull: %w", st.ConfigPath(), err)
		}
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check pulled config writable: %w", err)
		}
		reconcileResult, err := reconcileChecked(checked, cfg, agents, nil)
		mergeResult(&outcome.Result, reconcileResult)
		return err
	})
	return outcome, retErr
}

func (a *Application) Clone(url string) (CloneOutcome, error) {
	home, err := a.home()
	if err != nil {
		return CloneOutcome{}, err
	}
	return CloneStore(home, url, a.detectedAgents())
}

// CloneStore is SPEC scenario 4: clone the store, then let the ordinary
// reconcile rebuild every link from the fu.yaml that arrived with it. Agent
// detection is the reconcile's own (SPEC rule 4), so the new machine gets
// exactly the agents it has, and the switch matrix is whatever the store
// recorded -- fu.yaml is the expectation and it travelled whole.
func CloneStore(home, url string, agents []agent.Agent) (CloneOutcome, error) {
	outcome := CloneOutcome{Home: home, URL: url}
	ctx, cancel := context.WithTimeout(context.Background(), remoteSyncTimeout)
	defer cancel()
	st, err := store.Clone(ctx, home, url)
	if err != nil {
		return outcome, err
	}
	if head, err := st.Repo.Head(); err == nil {
		outcome.Branch = head.Name().Short()
	}
	res, err := Reconcile(st, agents)
	mergeResult(&outcome.Result, res)
	if err != nil {
		// The store is fully in place by now; only the link layer is
		// unfinished, and fu restore finishes it. Saying so is what stops a
		// user from re-running clone into "store already exists" -- the same
		// reason revert names the state it leaves behind (store/revert.go).
		return outcome, fmt.Errorf("store cloned to %s; rebuilding the agent links failed, run fu restore to finish: %w", st.Dir(), err)
	}
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		return outcome, err
	}
	outcome.Skills = len(cfg.SkillNames())
	return outcome, nil
}
