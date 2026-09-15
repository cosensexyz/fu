package engine

import (
	"errors"
	"fmt"
	"os"

	"github.com/cosensexyz/fu/internal/store"
)

// writeSession is the opening every write command shares.
//
// Six entry points -- run and writeCommandPrologue (pipeline.go),
// commitOperationsWithHooks (commit.go), Reconcile (reconcile.go),
// RevertOperations (restore.go) and PullOperations (remote.go) -- each began
// with the same seven steps: open a checked session, join its close into the
// returned error, take the two roots, take fu.lock, and recover whatever a
// previous run left pending. Only the error wording differed.
//
// What is deliberately *not* here is everything after that. Whether a command
// sweeps, where it checks the canonical path relative to that sweep, whether it
// reloads the config after mutating, and what it does with ErrOperationFailed
// all differ between commands, and the differences are load-bearing (SPEC §5.3
// requires restore not to sweep; batch 2 requires a named commit not to sweep
// store-wide; pull and revert must reload because the thing they just did can
// replace fu.yaml). Folding those into a single callback would turn each
// difference into a parameter -- the same divergence, one level further from
// the call site, and invisible to anyone reading the command.
type writeSession struct {
	session   *store.WriteSession
	checked   *store.Store
	homeRoot  *os.Root
	storeRoot *os.Root

	st   *store.Store
	what string
}

// beginWrite opens the checked session and resolves both roots. what names the
// command for error messages and changes nothing else.
//
// A failure after the session is open closes it here rather than leaving it to
// the caller, because the caller has nothing to close: it never received a
// handle. The close error is joined rather than dropped for the same reason
// every call site joins it -- a session that did not release its pinned
// descriptors cleanly is not something a failing command should hide.
func beginWrite(st *store.Store, what string) (*writeSession, error) {
	session, err := st.BeginWrite()
	if err != nil {
		return nil, fmt.Errorf("open checked %s session: %w", what, err)
	}
	checked := session.Store
	homeRoot, err := checked.Root()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("use checked %s root: %w", what, err), session.Close())
	}
	storeRoot, err := checked.StoreRoot()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("use checked store root for %s: %w", what, err), session.Close())
	}
	return &writeSession{
		session: session, checked: checked,
		homeRoot: homeRoot, storeRoot: storeRoot,
		st: st, what: what,
	}, nil
}

// close releases the session. Callers defer it and join its error into their
// own; calling it twice is harmless, which matters because an early return may
// have closed it already.
func (w *writeSession) close() error {
	if w == nil || w.session == nil {
		return nil
	}
	return w.session.Close()
}

// underLock runs fn holding $FU_HOME/fu.lock, which is what serialises this
// command against every other fu process.
//
// The lock is not reentrant: taking it again inside fn blocks forever. That is
// why pull and revert reload their config inside this body rather than calling
// back through a public entry point that would take it again.
func (w *writeSession) underLock(fn func() error) error {
	return withLock(w.homeRoot, "fu.lock", w.st.LockPath(), fn)
}

// recoverPending settles whatever a previous run left behind and merges what
// that recovery reported into res.
//
// The merge is done here, once, rather than at each call site, because
// forgetting it is invisible: a recovery that repairs something and says
// nothing looks exactly like a command with nothing to report. Dropping the
// merge from a single prologue left the entire engine package green.
func (w *writeSession) recoverPending(res *Result) error {
	recovered, err := RecoverPendingReporting(w.checked)
	mergeResult(res, recovered)
	if err != nil {
		return fmt.Errorf("recover pending transactions before %s: %w", w.what, err)
	}
	return nil
}

// loadConfig reads fu.yaml through the pinned store root and refuses one this
// build cannot write.
//
// Writability is checked here because it is a precondition of every write: a
// config newer than this build understands must be refused before anything is
// written, including a sweep commit, which would otherwise absorb the very
// content the guard exists to refuse under an unrelated message.
func (w *writeSession) loadConfig() (*store.Config, error) {
	cfg, err := store.LoadConfigRoot(w.storeRoot, "fu.yaml", w.st.ConfigPath())
	if err != nil {
		return nil, fmt.Errorf("load config %s for %s: %w", w.st.ConfigPath(), w.what, err)
	}
	if err := cfg.CheckWritable(); err != nil {
		return nil, fmt.Errorf("check config writable before %s: %w", w.what, err)
	}
	return cfg, nil
}

// loadConfigBytes is loadConfig plus the bytes the config was parsed from.
//
// Separate from loadConfig, and not a flag on it, because the bytes are a
// baseline a later publish compares against: run and commit hold them so the
// parsed config and the baseline cannot describe different versions. Reading
// the file a second time to get them is what let those two disagree -- the
// second read could straddle an external edit -- which is the defect batch 2
// closed.
func (w *writeSession) loadConfigBytes() (*store.Config, []byte, error) {
	cfg, loaded, err := store.LoadConfigRootBytes(w.storeRoot, "fu.yaml", w.st.ConfigPath())
	if err != nil {
		return nil, nil, fmt.Errorf("load config %s for %s: %w", w.st.ConfigPath(), w.what, err)
	}
	if err := cfg.CheckWritable(); err != nil {
		return nil, nil, fmt.Errorf("check config writable before %s: %w", w.what, err)
	}
	return cfg, loaded, nil
}
