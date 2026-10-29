package controlstore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func managementTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestManagementSecurityLockoutSessionsAndPasswordRevocation(t *testing.T) {
	store := managementTestStore(t)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := store.InitializeManagementAdministrator("admin", "hash", "127.0.0.1", now); err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeManagementAdministrator("other", "hash", "127.0.0.1", now); !errors.Is(err, ErrAdministratorAlreadyInitialized) {
		t.Fatalf("second init: %v", err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		count, locked, err := store.RecordManagementLoginFailure("127.0.0.1", now.Add(time.Duration(attempt)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if count != attempt {
			t.Fatalf("attempts=%d", count)
		}
		if attempt < 5 && locked != nil {
			t.Fatal("locked early")
		}
		if attempt == 5 && (locked == nil || locked.Sub(now) < 14*time.Minute) {
			t.Fatalf("lock=%v", locked)
		}
	}
	if err := store.RecordManagementLoginSuccess("admin", "127.0.0.1", now.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	session := ManagementSession{ID: "s1", Username: "admin", TokenHash: []byte("token-hash"), CSRFHash: []byte("csrf-hash"), CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := store.CreateManagementSession(session, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ManagementSession(session.TokenHash, now.Add(90*time.Minute), 2*time.Hour); err != nil {
		t.Fatalf("live session: %v", err)
	}
	if _, err := store.ManagementSession(session.TokenHash, now.Add(4*time.Hour), 2*time.Hour); !errors.Is(err, ErrManagementSessionInvalid) {
		t.Fatalf("idle session: %v", err)
	}
	session.ID = "s2"
	session.TokenHash = []byte("token-hash-2")
	session.LastSeenAt = now
	session.ExpiresAt = now.Add(24 * time.Hour)
	if err := store.CreateManagementSession(session, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateManagementAdministratorPassword("admin", "new-hash", "password_changed", "127.0.0.1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ManagementSession(session.TokenHash, now.Add(time.Hour), 2*time.Hour); !errors.Is(err, ErrManagementSessionInvalid) {
		t.Fatalf("password change retained session: %v", err)
	}
}

func TestManagementAlertsTriggerDedupeAcknowledgeRecoverAndCleanup(t *testing.T) {
	store := managementTestStore(t)
	now := time.Now().UTC()
	spec := ManagementAlertSpec{Key: "eval:test", Type: "runtime_unavailable", Title: "Runtime unavailable", Severity: "critical"}
	if err := store.ObserveManagementAlert(spec, true, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	alerts, _ := store.ManagementAlerts(10)
	if len(alerts) != 0 {
		t.Fatal("threshold ignored")
	}
	if err := store.ObserveManagementAlert(spec, true, time.Minute, now.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveManagementAlert(spec, true, time.Minute, now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	alerts, _ = store.ManagementAlerts(10)
	if len(alerts) != 1 || alerts[0].State != "ACTIVE" {
		t.Fatalf("alerts=%+v", alerts)
	}
	if err := store.AcknowledgeManagementAlert(alerts[0].ID, now.Add(63*time.Second)); err != nil {
		t.Fatal(err)
	}
	alerts, _ = store.ManagementAlerts(10)
	if alerts[0].State != "ACKNOWLEDGED" || alerts[0].Unread {
		t.Fatalf("ack=%+v", alerts[0])
	}
	if err := store.ObserveManagementAlert(spec, false, time.Minute, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	alerts, _ = store.ManagementAlerts(10)
	if alerts[0].State != "RECOVERED" || !alerts[0].Unread {
		t.Fatalf("recovery=%+v", alerts[0])
	}
	if err := store.CleanupManagementAlerts(now.Add(31 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	alerts, _ = store.ManagementAlerts(10)
	if len(alerts) != 0 {
		t.Fatal("expired alert retained")
	}
}
