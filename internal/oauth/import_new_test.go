package oauth

import (
	"context"
	"ferry/internal/secretstore"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestNewImportDormantAndCreateOnly(t *testing.T) {
	store, _ := secretstore.Open(filepath.Join(t.TempDir(), "secrets.json"))
	m := NewManager(store)
	raw := []byte(`{"type":"codex","access_token":"access","refresh_token":"refresh"}`)
	if _, e := m.ImportNewAuthJSON("imported", raw); e != nil {
		t.Fatal(e)
	}
	if _, e := m.ImportNewAuthJSON("imported", raw); e == nil {
		t.Fatal("overwrote existing grant")
	}
	if due := m.accountsDueForRefresh(time.Now()); len(due) != 0 {
		t.Fatal("new import scheduled refresh")
	}
	m = NewManager(store)
	if due := m.accountsDueForRefresh(time.Now()); len(due) != 0 {
		t.Fatal("restart scheduled import refresh")
	}
	standby := NewManagerWithOptions(store, Options{Standby: true})
	if _, e := standby.ImportNewAuthJSON("standby", raw); e != ErrStandby {
		t.Fatal("standby accepted import")
	}
}

func TestImportedCodexFirstUseActivatesRefresh(t *testing.T) {
	m, endpoint, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	if _, err := m.ImportNewAuthJSON("imported", []byte(`{"type":"codex","access_token":"access","refresh_token":"refresh"}`)); err != nil {
		t.Fatal(err)
	}
	before := endpoint.callCount()
	m.refreshDueAccounts(context.Background())
	if endpoint.callCount() != before {
		t.Fatal("unused import contacted upstream")
	}
	if _, err := m.AccessToken(context.Background(), "imported"); err != nil {
		t.Fatal(err)
	}
	if endpoint.callCount() != before+1 || m.tokens["imported"].ImportedDormant {
		t.Fatal("first use did not activate refresh")
	}
	restarted := NewManager(m.store)
	if restarted.tokens["imported"].ImportedDormant {
		t.Fatal("activation was not persisted")
	}
}
