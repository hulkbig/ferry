package nativeauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStandbyNativeGrantsNeverRefresh(t *testing.T) {
	active, driver := testManager(t)
	ref := Ref("fixture", "a")
	record := accountRecord{BindingID: "stable-binding", Token: &Token{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ExpiresAt: time.Now().Add(time.Hour)}}
	if err := active.save(ref, record); err != nil {
		t.Fatal(err)
	}
	m := NewManagerWithOptions(active.store, Options{Standby: true}, driver)
	ctx := context.Background()
	if token, err := m.Identity(ctx, ref, ""); err != nil || token.BindingID != "stable-binding" {
		t.Fatalf("valid copied grant: %v", err)
	}
	if _, err := m.Identity(ctx, ref, "fixture-access"); !errors.Is(err, ErrStandby) {
		t.Fatalf("401 refresh: %v", err)
	}
	record.Token.ExpiresAt = time.Now().Add(-time.Minute)
	if err := active.save(ref, record); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Identity(ctx, ref, ""); !errors.Is(err, ErrStandby) {
		t.Fatalf("expired grant: %v", err)
	}
	if _, err := m.Begin(ctx, "fixture", "a", nil); !errors.Is(err, ErrStandby) {
		t.Fatalf("begin: %v", err)
	}
	if _, err := m.Complete(ctx, "fixture", "a", ""); !errors.Is(err, ErrStandby) {
		t.Fatalf("complete: %v", err)
	}
	if driver.refreshes.Load() != 0 {
		t.Fatal("standby called the upstream refresh driver")
	}
	stored, err := m.read(ref)
	if err != nil || stored.RefreshInFlight || stored.Token.RefreshToken != "fixture-refresh" {
		t.Fatal("standby altered the grant or rotation journal")
	}
}
