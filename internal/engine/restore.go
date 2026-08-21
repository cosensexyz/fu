// internal/engine/restore.go
package engine

import (
	"errors"
	"fmt"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// RestoreOutcome reports both layers, with the store-worktree half split by
// what the user can actually do about each path.
//
// The split is not cosmetic. Refused and Left are both "uncommitted content
// restore did not touch", but only Refused is within `--hard`'s reach:
// ResetWorktreeToHead walks union(index, HEAD), so a path in neither is never
// in its path set and `--hard` will not remove it no matter how many times it
// runs. The dividing line is membership in that set and nothing else --
// notably not gitignore status, since fu's stageAll commits ignored content on
// purpose, so an ignored file that has survived one write command is tracked
// and lands in Refused with every other tracked path. Reporting the two
// together under one remedy produced
// advice that could not be followed -- `fu restore` named an untracked file
// and suggested `--hard`, `--hard` changed nothing, and the next `fu restore`
// printed the identical suggestion.
type RestoreOutcome struct {
	Result Result
	// Refused lists tracked store paths holding uncommitted content that this
	// run reported instead of discarding. It is filled only when hard is
	// false; with hard set these are the paths that got reset instead.
	Refused []string
	// Left lists uncommitted store paths outside union(index, HEAD): untracked
	// content, including ignored content no write command has swept in yet.
	// They are reported on both paths,
	// including the hard one: a user who explicitly asked to discard deserves
	// to be told what was deliberately kept rather than left to infer it from
	// silence.
	Left []string
	// Reset lists the store paths --hard put back. It is empty on the default
	// path, where Refused carries the tracked paths as a report instead.
	Reset []string
}

// Restore repairs the link layer unconditionally, then either reports or --
// with hard set -- discards uncommitted content in the store's own worktree.
// The link layer is Reconcile unchanged: it already takes the lock, recovers
// pending transactions, reloads the config and reconciles every agent, and it
// deliberately does not sweep.
//
// Not sweeping is what SPEC §5.3 asks of restore, stated there as the property
// this code actually has: restore adds no commit of its own, and folds no hand
// edit into history behind the user's back. It is not a promise that HEAD
// cannot move. Reconcile begins with RecoverPendingReporting, and *rolling
// back* an interrupted new, add or adopt writes that transaction's own
// compensation commit (committedInstallRecovery, new_txn.go). Rolling back, not
// completing: the completion branch writes no commit at all, and saying
// "completing" for both obscured which of the terminal states actually moves
// HEAD. It is the correct behaviour either way, and the only way to settle the
// very state `fu status` tells the user restore will settle. The
// commit belongs to that interrupted transaction, not to restore. Before this
// package had a production caller for Reconcile the distinction never came up,
// which is how the stronger claim survived.
//
// A sweep added to Reconcile would break the property that does hold;
// TestRestoreDoesNotSweepHandEditedStoreContent is the guard, since it dirties
// the store's own worktree, which is what Sweep/IsDirty actually inspect,
// rather than an agent-side link. hard does not touch this property either:
// it discards uncommitted content instead of committing it, so it still adds
// no commit of restore's own -- TestRestoreHardResetsTheStoreWorktree pins
// exactly that discard, not a fold into history.
//
// hard is the discard half this doc comment used to say restore would never
// have. It calls store.Store.ResetWorktreeToHead, never go-git's
// Worktree.Reset(HardReset): TestGoGitV519HardResetDeletesUntrackedAndIgnoredFiles
// (internal/store/revert_test.go) still pins that that primitive deletes
// untracked and ignored content, unlike system `git reset --hard`, which is
// why an earlier version of this discard half -- built on that primitive --
// was removed again rather than shipped. ResetWorktreeToHead is not that
// primitive under a new name; it is a path-scoped updater that only ever
// touches paths named by the index or the target tree
// (internal/store/worktree_apply.go), so an untracked path's name never
// reaches it and it cannot delete what it never looked at. That is also how
// it closes the five preconditions DESIGN §6 lists for a worktree reset,
// which the removed version left open: archiving untracked entries before
// the reset has no subject here, since its path set holds nothing untracked
// to archive; it moves the index and worktree but never rewrites the branch
// ref; its index rewrite runs under .git/index.lock; it runs on the write
// session's pinned descriptors below, refusing outside one; and it calls
// checkNoAbsoluteSymlinks before writing anything. See ResetWorktreeToHead's
// own doc comment for the full detail behind each of those five.
func Restore(st *store.Store, agents []agent.Agent, hard bool) (outcome RestoreOutcome, retErr error) {
	// The link layer runs first and unconditionally: repairing it is what the
	// user asked for, and reporting on the store's own worktree must not cost
	// them that.
	result, reconcileErr := Reconcile(st, agents)
	outcome.Result = result
	// A per-agent failure is isolated by Reconcile itself and surfaces as
	// ErrOperationFailed; it is not a reason to abandon the store worktree
	// layer. Returning here on it delivered the exact inverse of the ordering
	// argument above: one unreadable agent directory silently cancelled the
	// whole second layer, including a --hard the user had explicitly asked
	// for, with nothing said about the store paths still standing. The error
	// is carried to the end instead.
	//
	// Anything else -- BeginWrite, the lock, an unreadable or unwritable
	// config, a canonical-path check -- is a genuine abort: the second layer
	// would be operating on a store this call could not establish anything
	// about, so it still returns immediately.
	if reconcileErr != nil && !errors.Is(reconcileErr, ErrOperationFailed) {
		return outcome, reconcileErr
	}
	if !hard {
		// Reconcile takes fu.lock for its own duration and releases it before
		// returning, so this read runs unlocked. That is fine on this path and
		// only on this path: nothing here acts on what it finds, it only names
		// paths, so a concurrent write can make the report stale but cannot
		// make it destructive. The hard path below takes the lock, because it
		// deletes.
		dirty, err := st.ChangedPathsIncludingIgnored()
		if err != nil {
			return outcome, errors.Join(reconcileErr, err)
		}
		if len(dirty) == 0 {
			return outcome, reconcileErr
		}
		// Report and stop, the way git refuses a checkout that would overwrite
		// local changes. See the doc comment above for what hard does instead.
		outcome.Refused, outcome.Left, err = st.SplitResettablePaths(dirty)
		return outcome, errors.Join(reconcileErr, err)
	}
	// hard runs the discard in its own checked write session: the reset writes
	// through pinned descriptors, which guards against the logical root being
	// swapped out from under this call.
	// reconcileErr is joined into every exit below, including the three
	// failures on the way into the write session. It is an ErrOperationFailed
	// by this point -- anything else already returned above -- and dropping it
	// meant that when BeginWrite, Root or StoreRoot then failed for their own
	// reasons, the per-agent failures the first layer had already found
	// vanished from the error the user saw.
	session, err := st.BeginWrite()
	if err != nil {
		return outcome, errors.Join(reconcileErr, err)
	}
	// Every other BeginWrite call site in this package folds session.Close's
	// error into the returned error with errors.Join rather than discarding
	// it (Reconcile, writeCommandPrologue, pruneCompletedTransactions); a
	// named return is what makes that actually reach the caller here, since a
	// bare `return outcome, resetErr` would let the deferred assignment land
	// on a value nothing downstream reads.
	defer func() { retErr = errors.Join(retErr, session.Close()) }()
	checked := session.Store
	homeRoot, err := checked.Root()
	if err != nil {
		return outcome, errors.Join(reconcileErr, fmt.Errorf("use checked restore root: %w", err))
	}
	storeRoot, err := checked.StoreRoot()
	if err != nil {
		return outcome, errors.Join(reconcileErr, fmt.Errorf("use checked store root for restore: %w", err))
	}
	// Pinned descriptors are not a lock: they guarantee this call keeps
	// writing to the store it opened, not that no other fu process is writing
	// to it at the same time. The prior round's Restore could leave that gap
	// open because its second layer only reported; this one deletes, and every
	// other command in this package that changes state -- run (pipeline.go),
	// RevertOperations below, Reconcile itself -- holds fu.lock for the whole
	// of its change. So does this one now.
	retErr = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		dirty, err := checked.ChangedPathsIncludingIgnored()
		if err != nil {
			return err
		}
		if len(dirty) == 0 {
			return nil
		}
		// The reset's own path set is union(index, HEAD), so whatever falls
		// outside it survives this command untouched however it was invoked.
		// Recording it is what turns "restored agent links" with nothing else
		// printed into an actual account of what --hard did and did not do.
		_, left, err := checked.SplitResettablePaths(dirty)
		if err != nil {
			return err
		}
		outcome.Left = left
		reset, err := checked.ResetWorktreeToHead()
		outcome.Reset = reset
		if err != nil {
			return err
		}
		if len(reset) == 0 {
			return nil
		}
		// The link layer above ran against the config as it was before this
		// reset discarded part of it, so anything it decided from fu.yaml is
		// now decided from a file that no longer exists. Reconciling once more
		// -- against what the store actually holds now -- is what keeps the
		// one command whose purpose is removing drift from finishing by
		// creating some: without it, discarding an uncommitted `enabled:
		// false` left the config saying enabled and the link deleted, and
		// repairing hand-deleted store content took two runs, though SPEC §3
		// scenario 5, rule 6 and §10 all describe it as one.
		//
		// Ordering the link layer first stays deliberate (see the doc comment
		// above): it must not be held hostage to the store worktree. This is a
		// second pass, not a reordering -- the shape RevertOperations already
		// has below.
		cfg, err := store.LoadConfigRoot(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("reload config %s after reset: %w", st.ConfigPath(), err)
		}
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check restored config writable: %w", err)
		}
		// Every other production route into reconcileChecked performs this
		// check first -- Reconcile, RevertOperations, reconcileWithHooks -- and
		// this one is inside a different session from the first pass, whose own
		// check was made and closed with that session. Its purpose (store.go)
		// is to confirm the user-facing FU_HOME pathname still reaches this
		// session's identities before any agent-side reconcile writes a
		// canonical store path as a link target, which is exactly what runs
		// next.
		if err := session.CheckCanonicalPath(); err != nil {
			return err
		}
		reconcileResult, err := reconcileChecked(checked, cfg, agents, nil)
		// Replaced, not merged. The first pass ran before the store worktree
		// was final, so its link-layer findings describe a state this reset has
		// since changed -- and on the flagship case (store content deleted by
		// hand, link broken, --hard) the second pass resolves them outright.
		// Appending left the command reporting a missing skill three lines
		// above the output saying it had restored it. Only what the second pass
		// cannot regenerate is carried across: Warnings come from
		// RecoverPendingReporting, which runs once, inside the first pass.
		//
		// Defence in depth rather than a live loss, and worth saying so plainly
		// because a reader will otherwise go looking for the test. The only
		// Warnings any recovery handler produces today are the adopt isolation
		// notices at adopt_txn.go:306 and :326, and both sit behind
		// finishCommittedAdopt's preconditions, which require the store
		// worktree to be byte-exact -- config equal to the recorded bytes, the
		// owned tree unchanged, and validateInstallWorktreeChanges finding no
		// other changed path. A run that reached those warnings therefore has
		// nothing for the reset below to move, len(reset) == 0 returns before
		// this line, and no reachable state exercises it. It stays because the
		// cost is one append and the failure mode it prevents -- a future
		// recovery finding silently dropped by a replacement it knows nothing
		// about -- is silent by construction.
		reconcileResult.Warnings = carryWarningsForward(outcome.Result.Warnings, reconcileResult.Warnings)
		outcome.Result = reconcileResult
		return err
	})
	// Deduplicated on the same principle that replaces Result above: the second
	// pass supersedes the first, and the error has to follow the report it
	// belongs to. Both passes return the identical ErrOperationFailed sentinel,
	// so joining them printed "error: one or more agent operations failed"
	// followed by a bare second copy of the same sentence -- a fault introduced
	// by two correct fixes meeting.
	if errors.Is(reconcileErr, ErrOperationFailed) && errors.Is(retErr, ErrOperationFailed) {
		return outcome, retErr
	}
	return outcome, errors.Join(reconcileErr, retErr)
}

// carryWarningsForward joins the first reconcile pass's warnings ahead of the
// second's, so replacing the first pass's Result with the second's does not
// take them with it.
//
// A named function rather than the inline append it used to be, because the
// branch that calls it cannot be reached from production (review round 27
// finding 3, and the long comment at the call site). The only Warnings any
// recovery handler produces today sit behind preconditions that force the reset
// to move nothing, and a run that moves nothing returns before the merge. That
// argument is the reason the merge is defence in depth -- and also the reason no
// end-to-end test can exercise it without inventing a recovery handler that
// does not exist. Pinning the contract here is what a test can honestly do: if
// a future handler ever emits a Warning on a path the reset reaches, this is the
// only thing standing between that finding and silence.
//
// The copy is deliberate. Appending onto first would write into the caller's own
// backing array when it has spare capacity, mutating outcome.Result.Warnings --
// the very slice being read -- rather than building a new one beside it.
func carryWarningsForward(first, second []string) []string {
	return append(append([]string(nil), first...), second...)
}

// RevertOutcome reports what a revert did, in the same two parts `fu restore
// --hard` reports its own work in: the reconcile findings, and the store paths
// that actually moved.
//
// Changed exists because "reverted 2 operation(s)" is not an account of
// anything a user can check. Store.Revert has held this list at its
// applyTreeToWorktree call all along and discarded it; surfacing it is also
// what would have made the stale-config defect visible, since a revert that
// rewrote fu.yaml while the link layer stayed put now says so on the first
// line.
type RevertOutcome struct {
	Result  Result
	Changed []string
}

// RevertOperations rolls the store back n operations. Unlike Restore it is a
// write command, so it takes the standard route every other write command
// takes: the lock, pending-transaction recovery, and a sweep that records any
// hand edits as their own commit before this command does its own work. That
// sweep is why revert needs no archive -- nothing it rolls past is
// uncommitted by the time it acts.
//
// This is the divergence from real `git revert` recorded in DESIGN: git
// refuses outright on a dirty worktree ("Your local changes ... would be
// overwritten by merge"), because a textual merge of the reverted diff against
// unrelated uncommitted edits can conflict. fu's revert is not a merge; it is
// store.Store.Revert converging the worktree to a past tree snapshot and
// republishing it, which cannot conflict with anything -- there is no patch
// application step for a hand edit to collide with. Refusing here would just
// make the user run a write command (which sweeps) and then retry, for no
// safety gained, so the sweep runs inline instead. SPEC §5.3/§5 already
// require every write command to fold pending hand edits into history before
// doing its own work; this is that rule applied here, not an exception to it.
//
// Sweeping first is necessary but not sufficient for getting "n operations
// back" right. store.Store.Revert (Task 7) is a tree checkout: it converges
// every path the current index or the target commit names to the target's
// exact content, with no notion of "newer than the target". A hand edit this
// call's own sweep just committed sits strictly above the n operations being
// undone, so counting raw commits back from HEAD undershoots -- it lands on
// the sweep's own parent, the most recent real operation, not the one before
// it.
//
// That is resolved where the counting happens rather than here.
// store.Store.Revert walks first-parent history counting operations, not
// commits (resolveOperationsBack), and what counts as an operation is decided
// by a whitelist rather than by skipping known bookkeeping: isOperationMessage
// admits exactly the eight verbs SPEC §5.3 enumerates. Three things therefore
// do not count -- a sweep's "external: manual modifications", a recovery
// compensation together with the operation it cancels (the pair nets to zero,
// so neither is one the user completed), and "init: store", which is not in
// SPEC's list and could never be a revert target anyway.
//
// The direction matters and the earlier single-entry blacklist had it wrong.
// Under a blacklist an unrecognised message silently becomes a user operation,
// which is how a recovery compensation came to be counted as one, and
// `fu revert 1` after a crash undid fu's own rollback and put back the very
// content it had just removed. Under a whitelist an unrecognised message is
// simply not counted, which costs a reach that is one operation short and is
// visible rather than destructive. An earlier version
// corrected for it here instead, by measuring how many commits this call's own
// sweep had added and asking for that many extra; it worked for the sweep this
// call performs and for no other, so a sweep left behind by any earlier
// command still consumed one of the user's n.
// TestRevertSweepsHandEditsIntoHistoryWithoutReplayingThem pins that this call
// reverts exactly n real operations despite a sweep sitting in between, and
// TestRevertCountsOperationsNotRawCommits pins the case that adjustment could
// not reach.
//
// What this call does not do: replay the swept content back onto the
// worktree once Revert has landed. An earlier version did -- captured the
// dirty content before the sweep committed it, checked it back out after
// Revert landed, and folded that back in as a second, trailing sweep -- so a
// hand edit to a path the reverted operations never touched would still be
// sitting in the worktree afterwards, the way git's own patch-based revert
// would leave it. That is gone: after Revert, the worktree simply holds the
// target tree, with nothing replayed on top of it. The edit is not
// discarded -- the sweep above already gave it its own "external: manual
// modifications" commit, so it is in git history and reachable with `fu
// log` -- it just no longer reappears in the worktree. What this call
// guarantees is that the edit is recorded in history, not that it is still
// in your working tree.
func RevertOperations(st *store.Store, agents []agent.Agent, n int) (outcome RevertOutcome, retErr error) {
	var res Result
	defer func() { outcome.Result = res }()
	if n < 1 {
		return RevertOutcome{}, fmt.Errorf("revert count must be >= 1, got %d", n)
	}
	session, err := st.BeginWrite()
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = errors.Join(retErr, session.Close()) }()
	checked := session.Store
	homeRoot, err := checked.Root()
	if err != nil {
		return outcome, fmt.Errorf("use checked revert root: %w", err)
	}
	storeRoot, err := checked.StoreRoot()
	if err != nil {
		return outcome, fmt.Errorf("use checked store root for revert: %w", err)
	}
	// Mirrors Reconcile's own shape (reconcile.go): recovery, the config
	// load/writable/canonical-path checks, this command's own mutation, and
	// the closing internal reconcile pass all run inside the single lock
	// acquisition below -- the same boundary every other write command gets
	// from run (pipeline.go) and Reconcile gets on its own (review round
	// finding 1). Before this, the lock was never taken here at all --
	// BeginWrite above only pins descriptors against a logical root being
	// swapped out from under the call, which is a different guarantee from
	// serializing against another fu process's own write -- and
	// RecoverPendingReporting only ran inside a separate, later call to the
	// public Reconcile below, after Sweep and Revert had already mutated the
	// store unlocked and without recovering anything first. A leftover
	// pending transaction's in-flight content could then be folded by Sweep
	// into an "external: manual modifications" commit instead of being
	// recovered the way every other write command recovers it before doing
	// its own work.
	retErr = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		recoveryResult, err := RecoverPendingReporting(checked)
		mergeResult(&res, recoveryResult)
		if err != nil {
			return fmt.Errorf("recover pending transactions before revert: %w", err)
		}
		cfg, err := store.LoadConfigRoot(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("load config %s for revert: %w", st.ConfigPath(), err)
		}
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check config writable before revert: %w", err)
		}
		if err := session.CheckCanonicalPath(); err != nil {
			return err
		}

		if err := checked.Sweep(); err != nil {
			return err
		}

		// n is passed through untouched, and both arguments are the same
		// value now. There used to be a skip adjustment here: Log(1) before
		// the sweep, a forward count afterwards, and HEAD~(n+skip) so that
		// this call's own sweep commit did not consume one of the operations
		// the user asked to undo. It compensated for the right thing in too
		// narrow a window -- only the sweep this very call performed -- so a
		// sweep left in history by any earlier command still counted as an
		// operation, and `fu revert 2` over such a history undid one real
		// operation and one of fu's own bookkeeping entries.
		//
		// Store.Revert resolves the target by counting operations rather than
		// commits (resolveOperationsBack, store/revert.go), which subsumes the
		// adjustment: this call's own sweep is skipped for the same reason
		// every other sweep is, because it is not an operation. Nothing is
		// left for the caller to correct.
		// Not guarded against a concurrent direct-git writer the way run is.
		// run compares bytes after its sweep and raises ErrConcurrentStoreChange
		// (pipeline.go); revert has no equivalent, and applyTreeToWorktree
		// overwrites any path where the worktree differs from the target before
		// the tree fingerprint is taken. That fingerprint catches content the
		// target does not name -- it cannot catch *different* content on a path
		// the target does name, because the updater has already converged it.
		//
		// The window is narrow: fu.lock excludes other fu processes, so it takes
		// a direct-git write landing inside this locked section. It is recorded
		// rather than closed because closing it means giving revert run's whole
		// baseline-comparison apparatus, which is a larger change than this
		// round; it does mean DESIGN's "sweeping loses not one byte on fu's
		// side" is, for revert, stated more strongly than the code supports.
		//
		// "Recorded" means recorded in DESIGN §6's known-gap list, which is the
		// only place that survives. It used to cite the batch design document
		// under docs/superpowers/ instead -- a directory .gitignore excludes,
		// so the sole durable trace of the gap was this comment asserting it
		// was written down somewhere else.
		changed, err := checked.Revert(n)
		outcome.Changed = changed
		if err != nil {
			return err
		}

		// Reloaded, because the revert just rewrote fu.yaml on disk and cfg
		// above is the copy from before it ran. reconcileChecked does not
		// re-read the file -- Desired(cfg, a) (reconcile.go) is a pure
		// function of the *Config handed to it -- so passing the stale copy
		// reconciles the link layer against a config that no longer exists,
		// and SPEC:148's "链接随之重建" silently does not happen: reverting a
		// disable leaves fu.yaml enabled with no link, and reverting an enable
		// leaves a live link under a config that says the skill is off, which
		// the agent loads every session while fu reports it disabled.
		// Reconcile itself already reloads at this point in its own sequence
		// (reconcile.go); this is that same read, at the one place revert can
		// perform it -- after its own mutation, inside the same lock.
		cfg, err = store.LoadConfigRoot(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("reload config %s after revert: %w", st.ConfigPath(), err)
		}
		// Re-checked for the same reason it is loaded again: the reverted-to
		// config is a different file, and nothing else has established that
		// this version is one this build can write. A revert landing on a
		// fu.yaml from a future schema would otherwise be discovered only by
		// the next command.
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check reverted config writable: %w", err)
		}

		// The internal reconcile entry, not the public Reconcile: that one
		// opens its own write session and takes fu.lock itself, which would
		// try to re-acquire the lock this call already holds.
		reconcileResult, err := reconcileChecked(checked, cfg, agents, nil)
		mergeResult(&res, reconcileResult)
		return err
	})
	return outcome, retErr
}
