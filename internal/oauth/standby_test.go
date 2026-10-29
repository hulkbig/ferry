package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"ferry/internal/secretstore"
)

func TestStandbyUsesValidGrantWithoutRotatingOrSpendingCredits(t *testing.T) {
	active, endpoint, closeEndpoint := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer closeEndpoint()
	fullLogin(t, active, "work")
	before, _ := active.store.Get(storeKey("work"))
	m := NewManagerWithOptions(active.store, Options{Standby: true})
	if access, _, err := m.Identity(context.Background(), "work"); err != nil || access == "" {
		t.Fatalf("valid copied grant must serve: %v", err)
	}
	calls := endpoint.callCount()
	if _, _, err := m.RefreshIdentity(context.Background(), "work", ""); !errors.Is(err, ErrStandby) {
		t.Fatalf("401 refresh must be blocked: %v", err)
	}
	m.mu.Lock()
	m.tokens["work"].ExpiresAt = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	if _, err := m.AccessToken(context.Background(), "work"); !errors.Is(err, ErrStandby) {
		t.Fatalf("expired grant must fail closed: %v", err)
	}
	if _, err := m.StartLogin("work"); !errors.Is(err, ErrStandby) {
		t.Fatalf("browser login: %v", err)
	}
	if _, err := m.StartDeviceLogin(context.Background(), "work"); !errors.Is(err, ErrStandby) {
		t.Fatalf("device login: %v", err)
	}
	if err := m.CompleteLogin("work", "state", "code"); !errors.Is(err, ErrStandby) {
		t.Fatalf("callback: %v", err)
	}
	guard := func() (func(), error) { t.Fatal("standby must not reserve a reset intent"); return nil, nil }
	if _, err := m.ConfirmUsageReset(context.Background(), UsageResetBinding{}, "", "", "", guard); !errors.Is(err, ErrStandby) {
		t.Fatalf("manual credit redemption: %v", err)
	}
	if _, err := m.AutoUsageReset(context.Background(), UsageResetBinding{}, time.Hour, guard); !errors.Is(err, ErrStandby) {
		t.Fatalf("automatic credit redemption: %v", err)
	}
	m.refreshDueAccounts(context.Background())
	if endpoint.callCount() != calls {
		t.Fatal("standby contacted the token endpoint")
	}
	after, _ := active.store.Get(storeKey("work"))
	if after != before {
		t.Fatal("standby changed the persisted grant")
	}
}

func TestStandbyDoesNotResumeCopiedDeviceLogin(t *testing.T) {
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingLogin{Mode: loginModeDevice, DeviceAuthID: "fixture-id", UserCode: "fixture-code", Started: time.Now(), Status: loginStatusPending}
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(pendingStoreKey("work"), string(raw)); err != nil {
		t.Fatal(err)
	}
	m := NewManagerWithOptions(store, Options{Standby: true})
	if m.pending["work"] == nil || len(m.pendingCancels) != 0 {
		t.Fatal("copied login must remain visible without a polling worker")
	}
}
