package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cosensexyz/fu/internal/store"
)

// createTxnStagedRoot reserves a private root, journals its inode, publishes
// the final staging name, and journals that same manifest before exposing the
// test boundary. Declared entries must already be attached to txn.
func createTxnStagedRoot(st *store.Store, txn *TxnRecord, name string, perm os.FileMode, h hooks) (store.OwnedTree, error) {
	reservation, err := st.ReserveStagedRootOwned(perm)
	if err != nil {
		return store.OwnedTree{}, fmt.Errorf("reserve staging area for %s: %w", name, err)
	}
	txn.StagingReservation = &reservation
	if err := WriteTxn(st, txn); err != nil {
		return store.OwnedTree{}, fmt.Errorf("record private staging-root ownership: %w", err)
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
