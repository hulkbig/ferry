package nativeauth

import (
	"context"
	"ferry/internal/secretstore"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

type importClaudeDriver struct{ calls int }

func (d *importClaudeDriver) Info() Info {
	return Info{ID: "claude", BaseURL: "https://api.anthropic.com/v1", Protocol: "anthropic"}
}
func (d *importClaudeDriver) Begin(context.Context, *http.Client, LoginOptions) (Session, error) {
	panic("unexpected login")
}
func (d *importClaudeDriver) Complete(context.Context, *http.Client, Session, string) (Token, error) {
	panic("unexpected completion")
}
func (d *importClaudeDriver) Refresh(_ context.Context, _ *http.Client, old Token) (Token, error) {
	d.calls++
	old.AccessToken = "refreshed"
	old.ExpiresAt = time.Now().Add(time.Hour)
	return old, nil
}
func TestImportedClaudeDormantAcrossRestartAndCreateOnly(t *testing.T) {
	store, _ := secretstore.Open(filepath.Join(t.TempDir(), "secrets.json"))
	driver := &importClaudeDriver{}
	m := NewManager(store, driver)
	token := Token{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if _, e := m.ImportNewClaude("imported", token); e != nil {
		t.Fatal(e)
	}
	if _, e := m.ImportNewClaude("imported", token); e == nil {
		t.Fatal("overwrote existing account")
	}
	m = NewManager(store, driver)
	ref := Ref("claude", "imported")
	if _, e := m.identity(context.Background(), ref, "", true); e != nil {
		t.Fatal(e)
	}
	if driver.calls != 0 {
		t.Fatal("background refreshed unused import")
	}
	if _, e := m.Identity(context.Background(), ref, ""); e != nil {
		t.Fatal(e)
	}
	r, _ := m.read(ref)
	if driver.calls != 1 || r.ImportedDormant {
		t.Fatal("first use did not activate refresh")
	}
	standby := NewManagerWithOptions(store, Options{Standby: true}, driver)
	if _, e := standby.ImportNewClaude("standby", token); e != ErrStandby {
		t.Fatal("standby accepted import")
	}
}
