package engine

import (
	"errors"
	"testing"

	"github.com/gofrs/flock"

	"github.com/cosensexyz/fu/internal/store"
)

// Every write command begins by recovering whatever a previous run left
// pending, and whatever that recovery has to say must reach the command's own
// result. A recovery that repairs something silently is a command that tells
// the user nothing happened while it quietly put their store back together.
//
// Nothing pinned this. Dropping the merge in writeCommandPrologue left the
// whole engine package green, which is how a shared prologue can lose a
// finding for every command that goes through it at once.
//
// The handler is borrowed rather than contrived: production recovery handlers
// only emit warnings behind preconditions that keep the recovery from moving
// anything (see carryWarningsForward's note in restore.go), so the seam the
// repo already uses for observing dispatch is the way to reach the merge.
func TestWriteCommandsCarryRecoveryFindingsIntoTheirResult(t *testing.T) {
	commands := map[string]func(st *store.Store) (Result, error){
		"writeCommandPrologue": func(st *store.Store) (Result, error) { return writeCommandPrologue(st, nil) },
		"Reconcile":            func(st *store.Store) (Result, error) { return Reconcile(st, nil) },
	}
	// Both halves. A recovery that fails is exactly when its findings matter
	// most -- the user is about to be told the command stopped, and what
	// recovery managed to say on the way is the only account of why -- and
	// merging after the error check instead of before loses precisely that
	// half while leaving every success-path test green.
	for _, failing := range []bool{false, true} {
		for name, command := range commands {
			label := name
			if failing {
				label += "/recovery fails"
			}
			t.Run(label, func(t *testing.T) {
				s, _ := setupStore(t)
				if err := WriteTxn(s, &TxnRecord{Op: "test-op", Stage: "started"}); err != nil {
					t.Fatal(err)
				}
				const finding = "recovery said something worth repeating"
				restore := swapRecoverReporter("test-op", func(st *store.Store, r TxnRecord) (Result, error) {
					if failing {
						return Result{Warnings: []string{finding}}, errors.New("recovery could not finish")
					}
					return Result{Warnings: []string{finding}}, ClearTxn(st, r)
				})
				t.Cleanup(restore)

				res, err := command(s)
				if failing && err == nil {
					t.Fatal("a recovery that fails must fail the command")
				}
				if !failing && err != nil {
					t.Fatal(err)
				}
				var carried bool
				for _, warning := range res.Warnings {
					if warning == finding {
						carried = true
					}
				}
				if !carried {
					t.Fatalf("warnings = %v, want the recovery's own finding carried into the command's result", res.Warnings)
				}
			})
		}
	}
}

// The shell must actually hold fu.lock, not merely run its body. This is the
// property the prologue exists for -- every write command serialises against
// every other fu process -- and it is the one an extraction is most likely to
// lose, because losing it changes nothing anyone can see until two commands
// run at once.
func TestUnderLockHoldsTheHomeLock(t *testing.T) {
	s, _ := setupStore(t)
	ws, err := beginWrite(s, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.close()

	var heldDuringBody bool
	if err := ws.underLock(func() error {
		// A second acquisition must fail while the body runs.
		other := flock.New(s.LockPath())
		got, err := other.TryLock()
		if err != nil {
			return err
		}
		if got {
			_ = other.Unlock()
			return nil
		}
		heldDuringBody = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !heldDuringBody {
		t.Fatal("the body ran without fu.lock held; two fu processes could write at once")
	}

	// And it is released afterwards, or the next command would hang.
	after := flock.New(s.LockPath())
	got, err := after.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("fu.lock was not released when the body returned")
	}
	_ = after.Unlock()
}

// A session that cannot finish opening must not leak: the caller has no
// handle to close, because it never got one.
func TestBeginWriteReportsWhatItCouldNotOpen(t *testing.T) {
	s, _ := setupStore(t)
	ws, err := beginWrite(s, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ws.checked == nil || ws.homeRoot == nil || ws.storeRoot == nil {
		t.Fatalf("beginWrite must hand back everything the body needs: %+v", ws)
	}
	if err := ws.close(); err != nil {
		t.Fatal(err)
	}
	// Closing twice is what a deferred close does after an early return path
	// has already closed; it must stay harmless.
	if err := ws.close(); err != nil {
		t.Fatalf("a second close must be harmless: %v", err)
	}
}
