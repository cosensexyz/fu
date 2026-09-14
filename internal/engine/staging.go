package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cosensexyz/fu/internal/store"
)

// createTxnStagedRoot reserves a private root, journals its inode, publishes
// the final staging name, and journals that same manifest before exposing the
// test boundary. Declared entries must already be attached to txn.
func createTxnStagedRoot(st *store.Store, txn *TxnRecord, name string, perm os.FileMode, h hooks) (store.OwnedTree, error) {
	reservation, lease, err := st.ReserveStagedRootOwned(perm)
	if err != nil {
		return store.OwnedTree{}, fmt.Errorf("reserve staging area for %s: %w", name, err)
	}
	txn.StagingReservation = &reservation
	if err := WriteTxn(st, txn); err != nil {
		// The private root is still on disk and nothing here removes it, so
		// the record must stay with it: dropping the lease would manufacture
		// exactly the unbacked private root this lease was taken to prevent.
		return store.OwnedTree{}, errors.Join(
			fmt.Errorf("record private staging-root ownership: %w", err),
			lease.ReleaseIfObjectGone(st.StagingDir()))
	}
	// The journal now names the root, so recovery owns it and the lease's
	// window is over. Releasing it here rather than later is what keeps the
	// two from both claiming the same name: the lease accounts for exactly
	// the stretch in which no record could.
	if err := lease.Release(st.StagingDir()); err != nil {
		return store.OwnedTree{}, err
	}
	root, err := st.PublishStagedRootOwned(reservation, name)
	if err != nil {
		return store.OwnedTree{}, fmt.Errorf("publish staging area %s: %w", filepath.Join(st.StagingDir(), name), err)
	}
	txn.StagingReservation = nil
	txn.Payload = &root
	if err := WriteTxn(st, txn); err != nil {
		return store.OwnedTree{}, fmt.Errorf("record published staging-root ownership: %w", err)
	}
	if err := h.fire(h.afterStagingCreate); err != nil {
		return store.OwnedTree{}, err
	}
	return root, nil
}
