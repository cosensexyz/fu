package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedReservationRecoveryJournalsTheLiveManifest(t *testing.T) {
	s, _ := setupStore(t)
	session, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	st := session.Store
	reservation, err := st.ReserveStagedRootOwned(0o755)
	if err != nil {
		t.Fatal(err)
	}
	live, err := st.PublishStagedRootOwned(reservation, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	// Distinguish admission evidence from the returned observation on both
	// handle-exporting filesystems and platforms where handles are absent.
	if live.RootIdentity.Handle == "" {
		reservation.Manifest.RootIdentity.Handle = "1:aa"
	} else {
		reservation.Manifest.RootIdentity.Handle = ""
	}
	config, err := os.ReadFile(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	head, err := st.Repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	record := TxnRecord{Op: "new", Name: "alpha", Stage: "started", StartHead: head.Hash().String(), ConfigBefore: config, StagingReservation: &reservation}
	if err := WriteTxn(st, &record); err != nil {
		t.Fatal(err)
	}
	// Force a later, independent conflict so the promoted payload remains in
	// the WAL and can be inspected before any cleanup or completion.
	if err := os.Mkdir(filepath.Join(st.SkillsDir(), "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	storeRoot, err := st.StoreRoot()
	if err != nil {
		t.Fatal(err)
	}
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		t.Fatal(err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		t.Fatal(err)
	}
	recoveryRoot, err := st.RecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	err = rollBackUncommittedInstall(st, storeRoot, skillsRoot, stagingRoot, recoveryRoot, record, nil)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("expected the later dual-location conflict, got %v", err)
	}
	pending, err := PendingTxns(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Payload == nil || pending[0].StagingReservation != nil {
		t.Fatalf("promoted WAL = %+v", pending)
	}
	if got := pending[0].Payload.RootIdentity; got != live.RootIdentity {
		t.Fatalf("journaled identity = %+v, want live %+v", got, live.RootIdentity)
	}
}
