package management

import (
	"ferry/internal/backuppolicy"
	"ferry/internal/secretstore"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackupSettingsPersistAndRejectUnsafeChanges(t *testing.T) {
	f := newV2APIFixture(t)
	p := backuppolicy.Default()
	p.IntervalHours = 168
	p.S3Prefix = "ferry/configuration"
	p.Enabled = false
	rec, _ := f.do(http.MethodPatch, "/api/settings", map[string]any{"backup": p})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch %d: %s", rec.Code, rec.Body.String())
	}
	actual, err := backuppolicy.Load(f.control)
	if err != nil || actual != p {
		t.Fatalf("persisted %v %v", actual, err)
	}
	p.S3Prefix = "another-service"
	rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{"backup": p, "debug_request_response": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatal("invalid prefix accepted")
	}
	if f.ledger.DebugCaptureEnabled() {
		t.Fatal("invalid request partially applied")
	}
	actual, err = backuppolicy.Load(f.control)
	if err != nil || actual.S3Prefix != "ferry/configuration" {
		t.Fatal("invalid policy persisted")
	}
}

func TestBackupS3CredentialsAreWriteOnlyAndPreserved(t *testing.T) {
	f := newV2APIFixture(t)
	store, err := secretstore.OpenSQLite(f.controlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f.server.deps.Store = store
	p := backuppolicy.Default()
	p.S3 = backuppolicy.Connection{Endpoint: "https://storage.example.test", Region: "eu-west-1", Bucket: "my-backups", PathStyle: true}
	send := func(credentials any) (*httptest.ResponseRecorder, map[string]any) {
		body := map[string]any{"enabled": true, "interval_hours": 168, "include_history": false, "s3_prefix": p.S3Prefix, "local_copies": 2, "remote_copies": 30, "s3": p.S3}
		if credentials != nil {
			body["s3_credentials"] = credentials
		}
		return f.do(http.MethodPatch, "/api/settings", map[string]any{"backup": body})
	}
	secret := backuppolicy.Credentials{AccessKey: "test-access-key", SecretKey: "test-secret-value"}
	rec, _ := send(secret)
	if rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret.AccessKey) || strings.Contains(rec.Body.String(), secret.SecretKey) {
		t.Fatal("credentials leaked in response")
	}
	saved, err := backuppolicy.Load(f.control)
	if err != nil || saved.S3.CredentialsRef == "" {
		t.Fatal("missing saved reference", err)
	}
	raw, _, _ := f.control.RuntimeSetting(backuppolicy.SettingKey)
	if strings.Contains(raw, secret.SecretKey) {
		t.Fatal("secret leaked into public policy")
	}
	if _, ok := store.Get(saved.S3.CredentialsRef); !ok {
		t.Fatal("missing credential")
	}
	rec, _ = send(nil)
	if rec.Code != 200 {
		t.Fatalf("preserve: %s", rec.Body.String())
	}
	kept, _ := backuppolicy.Load(f.control)
	if kept.S3.CredentialsRef != saved.S3.CredentialsRef {
		t.Fatal("blank credentials did not preserve secret")
	}
	// Older clients that omit s3 must not silently revert the destination.
	rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{"backup": map[string]any{"enabled": true, "interval_hours": 168, "s3_prefix": p.S3Prefix, "local_copies": 2, "remote_copies": 30}})
	kept, _ = backuppolicy.Load(f.control)
	if rec.Code != 200 || kept.S3 != saved.S3 {
		t.Fatal("legacy update lost S3 settings")
	}
	p.S3.Endpoint = "https://different.example.test"
	rec, _ = send(nil)
	if rec.Code != 400 {
		t.Fatal("credentials reused at new endpoint")
	}
	kept, _ = backuppolicy.Load(f.control)
	if kept.S3 != saved.S3 {
		t.Fatal("invalid connection was saved")
	}
	p.S3.Endpoint = saved.S3.Endpoint
	rec, _ = send(backuppolicy.Credentials{AccessKey: "only-one-field"})
	if rec.Code != 400 {
		t.Fatal("partial credentials accepted")
	}
}
