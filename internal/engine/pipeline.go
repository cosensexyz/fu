// internal/engine/pipeline.go
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// Op is one write command's mutation. Run wraps it with the invariant
// pipeline every write command shares (DESIGN §6):
// lock → recover pending txns → load config → check writable →
// sweep external edits → record txn → mutate → save config → publish →
// commit → clear txn → reconcile links.
type Op struct {
	Message string // commit message, e.g. "new: pdf-tools"
	// AllowedChanges declares the only Git paths this operation may stage.
	// Each value admits the exact path and descendants below it.
	AllowedChanges []string
	// ValidatePrepared performs operation-specific validation against the
	// frozen Git candidate, after the generic config and path checks.
	ValidatePrepared func(st *store.Store, prepared store.PreparedCommit) error
	// Preflight performs read-only operation-specific checks while the write
	// lock and pinned roots are held, but before a transaction record exists.
	// Preconditions that can reject an untouched starting state belong here so
	// a crash cannot turn their ordinary refusal into a permanent WAL conflict.
	Preflight func(st *store.Store, cfg *store.Config) error
	Mutate    func(st *store.Store, cfg *store.Config) error
	// Txn is persisted before Mutate for multi-stage operations and updated at
	// durable boundaries. The pointed record may be enriched by Mutate.
	Txn *TxnRecord
	// Publish, when non-nil, moves content prepared during Mutate from
	// staging into the store. It runs after the config has been saved and
	// before the commit (round 7 finding, DESIGN §6's "准备 → 落盘 store →
	// commit"). Ops that only edit the config leave it nil.
	//
	// The order matters, and is chosen for which half-finished state is the
	// better one to be left in. Publishing before the config was saved is
	// what the store used to do, and it produced content fu.yaml had no
	// record of: nothing reported it, the next write swept it in as an
	// "external modification", and a retry of the same command refused
	// because the directory was already there -- neither finished nor
	// repeatable. Publishing after the save inverts the residue: the config
	// knows about a skill whose content did not arrive, which Reconcile
	// already reports (Result.Missing) and which a later attempt can
	// complete. One is silent and stuck; the other is visible and
	// recoverable.
	Publish func(st *store.Store) error
	// Cleanup removes staging content for a non-transaction observed rollback.
	Cleanup func(st *store.Store) error
}

func Run(st *store.Store, agents []agent.Agent, op Op) (Result, error) {
	return run(st, agents, op, hooks{})
}

// hooks is a test-only seam: each non-nil function runs at the durable
// boundary it names, so a test can inject the failures that actually happen
// there (a full disk, a permission change, a crash) and assert what the
// store is left holding. Production code always calls Run, which passes a
// zero value -- the same arrangement reconcile's beforeApply and
// Store.commitWithHook already use.
type hooks struct {
	afterTxnStart         func() error
	afterStagingCreate    func() error
	afterStagingOwnership func() error
	afterStagingScaffold  func() error
	afterMutate           func() error
	afterSave             func() error
	beforePublish         func() error
	afterPublish          func() error
	afterCommit           func() error
	commit                func(*store.Store, string, store.PreparedCommit) (store.CommitOutcome, error)
}

func (h hooks) fire(f func() error) error {
	if f == nil {
		return nil
	}
	return f()
}

func (h hooks) commitStore(st *store.Store, message string, prepared store.PreparedCommit) (store.CommitOutcome, error) {
	if h.commit != nil {
		return h.commit(st, message, prepared)
	}
	return st.CommitPrepared(message, prepared)
}

func run(st *store.Store, agents []agent.Agent, op Op, h hooks) (res Result, err error) {
	session, err := st.BeginWrite()
	if err != nil {
		return res, fmt.Errorf("open checked write session: %w", err)
	}
	// Joined rather than discarded, matching reconcileChecked: a close error
	// here means the pinned descriptors did not release cleanly, which is not
	// something a successful command should hide.
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	checked := session.Store
	homeRoot, err := checked.Root()
	if err != nil {
		return res, fmt.Errorf("use checked write root: %w", err)
	}
	storeRoot, err := checked.StoreRoot()
	if err != nil {
		return res, fmt.Errorf("use checked store root: %w", err)
	}
	err = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		if err := RecoverPending(checked); err != nil {
			return fmt.Errorf("recover pending transactions: %w", err)
		}
		// The bytes come back with the parsed config so the two cannot
		// disagree. Reading the file a second time later to establish a
		// baseline is what let them: the second read could straddle an
		// external edit, leaving cfg modelling one version while the baseline
		// named another, and the conditional install below then matched the
		// baseline and destroyed the edit.
		cfg, configLoaded, err := store.LoadConfigRootBytes(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("load config %s: %w", st.ConfigPath(), err)
		}
		// Writability is a precondition, checked before Sweep or Mutate run
		// (finding I1, hardened by round 2 finding 4): a config newer than
		// this build supports must be refused before anything is written --
		// including the sweep commit below, which is itself a commit and
		// would otherwise absorb the very content this guard exists to
		// refuse, under the unrelated message "external: manual
		// modifications". LoadConfig reads straight from disk regardless of
		// git state, so checking here (before Sweep ever runs) sees exactly
		// the same version a check after Sweep would have -- nothing is lost
		// by moving it earlier, and a refused write now leaves the store
		// exactly as the user left it, not partially committed.
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check config writable: %w", err)
		}
		if err := checked.Sweep(); err != nil {
			return fmt.Errorf("sweep external edits: %w", err)
		}
		// The baseline is the snapshot cfg was parsed from, established the
		// moment the sweep is done and before any operation code runs. Sweep
		// commits what it found and does not itself rewrite fu.yaml, so the
		// file must still hold those exact bytes; if it does not, an external
		// writer arrived during the sweep window. That edit belongs to the
		// next external commit, so the command stops here and leaves it alone
		// rather than continuing from a model that no longer describes it.
		configBefore, err := store.ReadConfigFileRoot(storeRoot, "fu.yaml")
		if err != nil {
			return fmt.Errorf("read config %s: %w", st.ConfigPath(), err)
		}
		if !bytes.Equal(configBefore, configLoaded) {
			return fmt.Errorf("%w: %s changed while the operation was starting", ErrConcurrentStoreChange, st.ConfigPath())
		}
		if op.Preflight != nil {
			if err := op.Preflight(checked, cfg); err != nil {
				return fmt.Errorf("check operation preconditions: %w", err)
			}
		}
		var configExpected []byte
		if op.Txn != nil {
			head, err := checked.Repo.Head()
			if err != nil {
				return fmt.Errorf("read transaction start HEAD: %w", err)
			}
			op.Txn.StartHead = head.Hash().String()
			op.Txn.Stage = "started"
			op.Txn.Message = op.Message
			op.Txn.ConfigBefore = append(op.Txn.ConfigBefore[:0], configBefore...)
			if err := WriteTxn(checked, op.Txn); err != nil {
				return fmt.Errorf("write transaction before mutation: %w", err)
			}
			if err := h.fire(h.afterTxnStart); err != nil {
				return rollback(checked, op, configBefore, configExpected, nil, err)
			}
		}
		if err := op.Mutate(checked, cfg); err != nil {
			return rollback(checked, op, configBefore, configExpected, nil, fmt.Errorf("execute mutation: %w", err))
		}
		if err := h.fire(h.afterMutate); err != nil {
			return rollback(checked, op, configBefore, configExpected, nil, err)
		}
		// From here on the store changes durably, and every failure has to
		// undo what got that far. Publishing before saving would invert the
		// residue but not remove it (see Op.Publish); rolling back is what
		// makes the command as a whole either happen or not.
		configExpected, err = cfg.Bytes()
		if err != nil {
			return rollback(checked, op, configBefore, configExpected, nil, fmt.Errorf("encode expected config: %w", err))
		}
		// Installed against the bytes captured after the sweep, not over
		// whatever happens to be there now. An external edit arriving in
		// between belongs to the next external commit or stays in the
		// worktree (DESIGN §6): this command must neither absorb it nor
		// destroy it, and a plain replace-rename did the latter silently.
		if err := checked.SaveConfigExpecting(cfg, configBefore); err != nil {
			return rollback(checked, op, configBefore, configExpected, nil, fmt.Errorf("save config: %w", err))
		}
		if op.Txn != nil {
			op.Txn.Stage = "config-saved"
			if err := WriteTxn(checked, op.Txn); err != nil {
				return rollback(checked, op, configBefore, configExpected, nil, fmt.Errorf("record saved config: %w", err))
			}
		}
		if err := h.fire(h.afterSave); err != nil {
			return rollback(checked, op, configBefore, configExpected, nil, err)
		}
		if op.Publish != nil {
			if err := h.fire(h.beforePublish); err != nil {
				return rollback(checked, op, configBefore, configExpected, nil, err)
			}
			if err := op.Publish(checked); err != nil {
				return rollback(checked, op, configBefore, configExpected, nil, fmt.Errorf("publish into store: %w", err))
			}
			if op.Txn != nil {
				op.Txn.Stage = "published"
				if err := WriteTxn(checked, op.Txn); err != nil {
					return rollback(checked, op, configBefore, configExpected, nil, fmt.Errorf("record published content: %w", err))
				}
			}
			if err := h.fire(h.afterPublish); err != nil {
				return rollback(checked, op, configBefore, configExpected, nil, err)
			}
		}
		prepared, err := checked.PrepareCommit()
		if err != nil {
			return rollback(checked, op, configBefore, configExpected, nil, fmt.Errorf("prepare operation commit: %w", err))
		}
		if err := validatePreparedOperation(checked, op, prepared, configExpected); err != nil {
			return rollback(checked, op, configBefore, configExpected, &prepared, err)
		}
		if op.Txn != nil {
			op.Txn.CommitTree = prepared.TreeFingerprint()
			if err := WriteTxn(checked, op.Txn); err != nil {
				return rollback(checked, op, configBefore, configExpected, &prepared, fmt.Errorf("record prepared operation tree: %w", err))
			}
		}
		outcome, err := h.commitStore(checked, op.Message, prepared)
		if err != nil {
			commitErr := fmt.Errorf("commit to store: %w", err)
			if outcome.Written {
				return fmt.Errorf("%w (commit %s was written; preserving its config, content, and transaction record for recovery)", commitErr, outcome.Hash.String())
			}
			return rollback(checked, op, configBefore, configExpected, &prepared, commitErr)
		}
		if err := h.fire(h.afterCommit); err != nil {
			return err
		}
		if op.Txn != nil {
			if err := ClearTxn(checked, *op.Txn); err != nil {
				return fmt.Errorf("clear completed transaction: %w", err)
			}
		}
		if err := session.CheckCanonicalPath(); err != nil {
			return err
		}
		res, err = reconcileChecked(checked, cfg, agents, nil)
		if err != nil {
			return fmt.Errorf("reconcile links: %w", err)
		}
		return nil
	})
	return res, err
}

// ErrConcurrentStoreChange means content outside the operation's declared and
// validated delta appeared after the initial sweep.
var ErrConcurrentStoreChange = errors.New("store changed while the operation was in progress")

func validatePreparedOperation(st *store.Store, op Op, prepared store.PreparedCommit, expectedConfig []byte) error {
	root, err := st.StoreRoot()
	if err != nil {
		return err
	}
	currentConfig, err := store.ReadConfigFileRoot(root, "fu.yaml")
	if err != nil {
		return err
	}
	if !bytes.Equal(currentConfig, expectedConfig) {
		return fmt.Errorf("%w: fu.yaml no longer contains the operation's expected bytes", ErrConcurrentStoreChange)
	}
	if err := st.ValidatePreparedFile(prepared, "fu.yaml", expectedConfig); err != nil {
		return fmt.Errorf("%w: %v", ErrConcurrentStoreChange, err)
	}
	for _, changed := range prepared.ChangedPaths() {
		allowed := false
		for _, root := range op.AllowedChanges {
			root = path.Clean(root)
			if root != "." && (changed == root || strings.HasPrefix(changed, root+"/")) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%w: prepared commit contains undeclared path %q", ErrConcurrentStoreChange, changed)
		}
	}
	if op.ValidatePrepared != nil {
		if err := op.ValidatePrepared(st, prepared); err != nil {
			return fmt.Errorf("%w: %v", ErrConcurrentStoreChange, err)
		}
	}
	unstaged, err := st.UnstagedPathsIncludingIgnored(prepared)
	if err != nil {
		return err
	}
	if len(unstaged) != 0 {
		return fmt.Errorf("%w: filesystem has unprepared paths %q", ErrConcurrentStoreChange, unstaged)
	}
	return nil
}

// rollback undoes the durable half of a failed write so the command as a
// whole either happened or did not (round 8 finding).
//
// Before this, the pipeline's failure modes were durable and asymmetric: a
// publish failure left a registered but absent skill, and a commit failure
// left content and config with no commit. Neither could be completed, and
// retrying the same command hit the "already exists" guard instead -- so
// `fu new` was neither recoverable nor safely repeatable, while the CLI
// reported plain failure.
//
// Transaction operations route failures the running process observes through
// the same manifest-backed RecoverPending handler used after a process crash.
// Operations without a transaction record retain callback/config rollback.
//
// A rollback that itself fails is reported alongside the original error
// rather than swallowed: at that point the store genuinely is in a state fu
// could not restore, and saying so is the only honest option.
func rollback(st *store.Store, op Op, configBefore, configExpected []byte, prepared *store.PreparedCommit, cause error) error {
	// Abandon the immutable candidate before rolling back durable operation
	// state. Candidates are private today, so this has no public-index side
	// effect; retaining one explicit boundary keeps rollback independent of the
	// staging implementation.
	var indexErr error
	if prepared != nil {
		indexErr = st.RestorePreparedIndex(*prepared)
	}
	if op.Txn != nil {
		if err := RecoverPending(st); err != nil {
			return fmt.Errorf("%w (transaction-backed rollback stopped: %w)", cause, err)
		}
		if indexErr != nil {
			return fmt.Errorf("%w (and the prepared Git candidate could not be abandoned: %w)", cause, indexErr)
		}
		return cause
	}
	var undoErrs []error
	if indexErr != nil {
		undoErrs = append(undoErrs, fmt.Errorf("abandon the prepared Git candidate: %w", indexErr))
	}
	if op.Cleanup != nil {
		if err := op.Cleanup(st); err != nil {
			undoErrs = append(undoErrs, fmt.Errorf("clean staging content: %w", err))
		}
	}
	// The restore is conditional in the same sense as the install: it puts the
	// starting config back only while the file still holds the bytes this
	// command wrote. Reading, comparing and then replacing left exactly the
	// window the comparison was meant to close -- an edit landing in between
	// was overwritten by the rollback instead of by the command.
	root, rootErr := st.StoreRoot()
	if rootErr != nil {
		undoErrs = append(undoErrs, fmt.Errorf("open checked store root for config rollback: %w", rootErr))
	} else if current, err := store.ReadConfigFileRoot(root, "fu.yaml"); err != nil {
		undoErrs = append(undoErrs, fmt.Errorf("inspect %s before rollback: %w", st.ConfigPath(), err))
	} else if bytes.Equal(current, configBefore) {
		// Nothing to restore.
	} else if configExpected == nil {
		undoErrs = append(undoErrs, fmt.Errorf("%w: preserving unexpected fu.yaml instead of overwriting it during rollback", ErrConcurrentStoreChange))
	} else if err := st.InstallConfigExpecting(configExpected, configBefore); err != nil {
		if errors.Is(err, store.ErrConfigChangedExternally) {
			undoErrs = append(undoErrs, fmt.Errorf("%w: preserving unexpected fu.yaml instead of overwriting it during rollback", ErrConcurrentStoreChange))
		} else {
			undoErrs = append(undoErrs, fmt.Errorf("restore %s: %w", st.ConfigPath(), err))
		}
	}
	if len(undoErrs) > 0 {
		return fmt.Errorf("%w (and the store could not be restored: %w)", cause, errors.Join(undoErrs...))
	}
	return cause
}
