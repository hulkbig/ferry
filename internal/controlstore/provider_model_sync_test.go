package controlstore

import (
	"path/filepath"
	"testing"
	"time"
)

func TestProviderModelSyncStatePersistsAndRunningRecoversAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	if err := store.SaveProviderModelSyncState(ProviderModelSyncState{
		ProviderID: "provider-a", ConfigFingerprint: "fingerprint", Status: "running",
		LastStartedAt: &started, DiscoveredModels: 7, Revision: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.ProviderModelSyncState("provider-a")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Status != "failed" || state.LastErrorCode != "interrupted" ||
		state.LastCompletedAt == nil || state.NextRunAt == nil || state.LastStartedAt == nil ||
		!state.LastStartedAt.Equal(started) || state.Revision != 4 {
		t.Fatalf("recovered state=%+v", state)
	}

	success := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)
	next := success.Add(time.Hour)
	state.Status = "succeeded"
	state.LastCompletedAt, state.LastSuccessAt, state.NextRunAt = &success, &success, &next
	state.LastErrorCode, state.LastError = "", ""
	state.DiscoveredModels, state.AddedModels, state.UpdatedModels, state.CreatedLogicalModels = 9, 2, 1, 2
	if err := reopened.SaveProviderModelSyncState(*state); err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.ProviderModelSyncState("provider-a")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Status != "succeeded" || loaded.AddedModels != 2 ||
		loaded.UpdatedModels != 1 || loaded.CreatedLogicalModels != 2 || loaded.LastSuccessAt == nil || !loaded.LastSuccessAt.Equal(success) {
		t.Fatalf("persisted state=%+v", loaded)
	}
}
