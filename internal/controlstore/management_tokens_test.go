package controlstore

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManagementTokenLifecycleAndHighRiskGate(t *testing.T) {
	store := managementTestStore(t)
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)

	token, secret, err := store.CreateManagementToken("automation", "Ferry Skill", &expires, false,
		"admin", "127.0.0.1", now)
	if err != nil {
		t.Fatal(err)
	}
	if token.ID == "" || !strings.HasPrefix(secret, "smt_") || token.Preview == "" || token.AllowHighRisk {
		t.Fatalf("created token=%+v secret=%q", token, secret)
	}
	encoded, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "secret_hash") {
		t.Fatalf("safe token view leaked credential material: %s", encoded)
	}

	authenticated, err := store.AuthenticateManagementToken(secret, "127.0.0.2", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != token.ID || authenticated.LastUsedAt == nil {
		t.Fatalf("authenticated token=%+v", authenticated)
	}
	if _, err := store.AuthenticateManagementToken("smt_invalid", "127.0.0.2", now); !errors.Is(err, ErrManagementTokenInvalid) {
		t.Fatalf("invalid credential error=%v", err)
	}

	allowHighRisk := true
	disabled := false
	updated, err := store.UpdateManagementToken(token.ID, nil, nil, &disabled, &allowHighRisk, nil, false,
		"admin", "127.0.0.1", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || !updated.AllowHighRisk {
		t.Fatalf("updated token=%+v", updated)
	}
	if _, err := store.AuthenticateManagementToken(secret, "127.0.0.2", now.Add(3*time.Minute)); !errors.Is(err, ErrManagementTokenInvalid) {
		t.Fatalf("disabled credential error=%v", err)
	}
	enabled := true
	if _, err := store.UpdateManagementToken(token.ID, nil, nil, &enabled, nil, nil, false,
		"admin", "127.0.0.1", now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rotated, rotatedSecret, err := store.RotateManagementToken(token.ID, "admin", "127.0.0.1", now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rotatedSecret == secret || rotated.Preview == token.Preview {
		t.Fatal("rotation did not replace the token secret")
	}
	if _, err := store.AuthenticateManagementToken(secret, "127.0.0.2", now.Add(6*time.Minute)); !errors.Is(err, ErrManagementTokenInvalid) {
		t.Fatalf("old credential remained valid: %v", err)
	}
	if _, err := store.AuthenticateManagementToken(rotatedSecret, "127.0.0.2", now.Add(6*time.Minute)); err != nil {
		t.Fatalf("rotated credential: %v", err)
	}

	revoked, err := store.RevokeManagementToken(token.ID, "admin", "127.0.0.1", now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Enabled || revoked.RevokedAt == nil {
		t.Fatalf("revoked token=%+v", revoked)
	}
	if _, err := store.AuthenticateManagementToken(rotatedSecret, "127.0.0.2", now.Add(8*time.Minute)); !errors.Is(err, ErrManagementTokenInvalid) {
		t.Fatalf("revoked credential error=%v", err)
	}
	if _, _, err := store.RotateManagementToken(token.ID, "admin", "127.0.0.1", now.Add(9*time.Minute)); !errors.Is(err, ErrManagementTokenRevoked) {
		t.Fatalf("rotate revoked error=%v", err)
	}
	if _, err := store.UpdateManagementToken(token.ID, nil, nil, &enabled, nil, nil, false,
		"admin", "127.0.0.1", now.Add(9*time.Minute)); !errors.Is(err, ErrManagementTokenRevoked) {
		t.Fatalf("re-enable revoked error=%v", err)
	}
	if err := store.DeleteManagementToken(token.ID, "admin", "127.0.0.1", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ManagementToken(token.ID); err != nil || got != nil {
		t.Fatalf("deleted token=%+v err=%v", got, err)
	}
}

func TestManagementTokenExpiryAndUseAuditAreBounded(t *testing.T) {
	store := managementTestStore(t)
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	expires := now.Add(30 * time.Minute)
	token, secret, err := store.CreateManagementToken("short-lived", "", &expires, false,
		"admin", "127.0.0.1", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateManagementToken(secret, "127.0.0.2", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateManagementToken(secret, "127.0.0.2", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateManagementToken(secret, "127.0.0.2", expires); !errors.Is(err, ErrManagementTokenInvalid) {
		t.Fatalf("expired credential error=%v", err)
	}

	events, err := store.ManagementSecurityEvents(50)
	if err != nil {
		t.Fatal(err)
	}
	uses := 0
	for _, event := range events {
		if event.EventType == "management_token_used" && event.Detail["token_id"] == token.ID {
			uses++
		}
	}
	if uses != 1 {
		t.Fatalf("management_token_used events=%d, want 1: %+v", uses, events)
	}
}

func TestManagementTokenMetadataValidation(t *testing.T) {
	store := managementTestStore(t)
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	token, _, err := store.CreateManagementToken("Skill Operator", "trusted LAN automation", nil, false,
		"admin", "127.0.0.1", now)
	if err != nil {
		t.Fatal(err)
	}
	tooLong := strings.Repeat("x", 241)
	if _, err := store.UpdateManagementToken(token.ID, nil, &tooLong, nil, nil, nil, false,
		"admin", "127.0.0.1", now.Add(time.Minute)); err == nil {
		t.Fatal("expected oversized description to be rejected")
	}
}
