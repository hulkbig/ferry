package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ferry/internal/config"
	"ferry/internal/credentialimport"
	"ferry/internal/nativeauth"
	"ferry/internal/oauth"
	"ferry/internal/secretstore"
)

func importFixture(t *testing.T) *v2APIFixture {
	t.Helper()
	f := newV2APIFixture(t)
	store, err := secretstore.OpenSQLite(filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.server.deps.Store = store
	f.server.deps.OAuth = oauth.NewManager(store)
	f.server.deps.NativeOAuth = nativeauth.NewManager(store, nativeauth.NewClaudeDriver())
	return f
}
func importRequest(t *testing.T) credentialimport.Request {
	t.Helper()
	r := credentialimport.Request{Source: "cliproxyapi", Namespace: "test", Document: `{"api-keys":["client-private"],"openai-compatibility":[{"name":"Gateway","base-url":"https://example.test/v1","api-key-entries":[{"api-key":"upstream-private"}],"models":[{"name":"model-test"}]}]}`, AuthFiles: []json.RawMessage{json.RawMessage(`{"type":"codex","access_token":"access-private","refresh_token":"refresh-private"}`), json.RawMessage(`{"type":"claude","access_token":"claude-private","refresh_token":"claude-refresh-private"}`)}}
	p, e := credentialimport.Parse(r)
	if e != nil {
		t.Fatal(e)
	}
	for _, i := range p.Items {
		r.Selected = append(r.Selected, i.ID)
	}
	return r
}
func TestImportPreviewApplyPersistAndRepeat(t *testing.T) {
	f := importFixture(t)
	r := importRequest(t)
	rec, _ := f.do("POST", "/api/imports/preview", r)
	requireStatus(t, rec, 200)
	if len(f.server.deps.Store.Names()) != 0 {
		t.Fatal("preview stored secrets")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cache allowed")
	}
	for _, path := range []string{"/api/imports/preview", "/api/imports/apply"} {
		rec, _ = f.do("POST", path, r)
		requireStatus(t, rec, 200)
		for _, value := range []string{"client-private", "upstream-private", "access-private", "refresh-private", "claude-private"} {
			if strings.Contains(rec.Body.String(), value) {
				t.Fatal("response leaked secret")
			}
		}
	}
	cfg, _, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 4 || len(cfg.ConsumerKeys) != 2 {
		t.Fatalf("missing imported objects: %d providers, %d keys", len(cfg.Providers), len(cfg.ConsumerKeys))
	}
	for _, p := range cfg.Providers[1:] {
		if p.Enabled || p.ModelSyncInterval != "off" {
			t.Fatal("import activated provider")
		}
	}
	for _, c := range cfg.Connections[1:] {
		if c.Enabled {
			t.Fatal("import activated connection")
		}
	}
	importedKey := cfg.ConsumerKeys[findConsumerIndex(cfg, r.Selected[0])]
	if importedKey.Enabled == nil || *importedKey.Enabled || importedKey.Key != "" {
		t.Fatal("client key enabled or leaked into config")
	}
	before := len(f.server.deps.Store.Names())
	rec, _ = f.do("POST", "/api/imports/apply", r)
	requireStatus(t, rec, 200)
	if strings.Count(rec.Body.String(), `"status":"exists"`) != 4 || len(f.server.deps.Store.Names()) != before {
		t.Fatal("repeat wrote credentials")
	}
}
func TestImportPartialFailureAndRetry(t *testing.T) {
	f := importFixture(t)
	r := importRequest(t)
	r.Selected = r.Selected[1:2]
	publish := f.server.deps.Publish
	f.server.deps.Publish = func(*config.File) error { return errors.New("sensitive error should not escape") }
	rec, response := f.do("POST", "/api/imports/apply", r)
	requireStatus(t, rec, 207)
	if strings.Contains(rec.Body.String(), "sensitive error") || len(f.server.deps.Store.Names()) != 1 {
		t.Fatal("failed import boundary")
	}
	item := response["items"].([]any)[0].(map[string]any)
	if item["credential_ref"] == nil {
		t.Fatal("missing retained credential reference")
	}
	if len(f.server.currentConfig().Providers) != 1 {
		t.Fatal("failed publish mutated active config")
	}
	f.server.deps.Publish = publish
	rec, _ = f.do("POST", "/api/imports/apply", r)
	requireStatus(t, rec, 200)
	if len(f.server.currentConfig().Providers) != 2 {
		t.Fatal("retry did not publish")
	}
}

func TestImportLargeCatalogUsesLogicalModelPolicyAndStaysDisabled(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint string
		wantModels     int
	}{
		{"direct", "https://example.test/v1", 12},
		{"listed-aggregator", "https://openrouter.ai/api/v1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := importFixture(t)
			manualBefore, _ := json.Marshal(f.server.currentConfig().LogicalModels[0])
			models := make([]map[string]string, 12)
			for i := range models {
				models[i] = map[string]string{"name": fmt.Sprintf("catalog-%d", i)}
			}
			models[11]["name"] = "gpt-6.1-terra"
			document, err := json.Marshal(map[string]any{"openai-compatibility": []any{map[string]any{
				"name": "Catalog", "base-url": tc.endpoint,
				"api-key-entries": []any{map[string]string{"api-key": "catalog-test-upstream"}}, "models": models,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			r := credentialimport.Request{Source: "cliproxyapi", Namespace: "catalog", Document: string(document)}
			plan, err := credentialimport.Parse(r)
			if err != nil || len(plan.Items) != 1 {
				t.Fatalf("catalog parse: items=%d err=%v", len(plan.Items), err)
			}
			r.Selected = []string{plan.Items[0].ID}
			rec, _ := f.do(http.MethodPost, "/api/imports/apply", r)
			requireStatus(t, rec, http.StatusOK)
			active, _, err := f.control.ActiveConfig()
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, logical := range active.LogicalModels {
				if len(logical.DefaultRoute.Steps) == 1 && logical.DefaultRoute.Steps[0].ProviderID == r.Selected[0] {
					count++
					if logical.Enabled || !logical.AutoGenerated {
						t.Fatal("imported logical model was activated or lost automatic ownership")
					}
				}
			}
			if count != tc.wantModels || active.Providers[1].Enabled || active.Connections[1].Enabled {
				t.Fatalf("imported catalog: logical=%d want=%d", count, tc.wantModels)
			}
			manualAfter, _ := json.Marshal(active.LogicalModels[logicalModelIndex(active, "smart")])
			if string(manualBefore) != string(manualAfter) {
				t.Fatal("import changed existing manual route")
			}
		})
	}
}
func TestImportSelectionAndDuplicateClientKey(t *testing.T) {
	f := importFixture(t)
	r := importRequest(t)
	r.Selected = nil
	rec, _ := f.do("POST", "/api/imports/apply", r)
	requireStatus(t, rec, 400)
	r.Selected = []string{"missing"}
	rec, _ = f.do("POST", "/api/imports/apply", r)
	requireStatus(t, rec, 400)
	if len(f.server.deps.Store.Names()) != 0 {
		t.Fatal("invalid selection stored secrets")
	}
	r = credentialimport.Request{Source: "sub2api", Namespace: "duplicate", ClientKeys: []credentialimport.ClientKey{{Name: "existing", Key: "sk-ferry-initial"}}}
	plan, _ := credentialimport.Parse(r)
	r.Selected = []string{plan.Items[0].ID}
	rec, _ = f.do("POST", "/api/imports/apply", r)
	requireStatus(t, rec, 200)
	if len(f.server.currentConfig().ConsumerKeys) != 1 || !strings.Contains(rec.Body.String(), `"status":"exists"`) {
		t.Fatal("duplicated existing client credential")
	}
}
func TestConcurrentImportDoesNotReplaceCredentials(t *testing.T) {
	f := importFixture(t)
	r := importRequest(t)
	r.Selected = r.Selected[1:2]
	raw, _ := json.Marshal(r)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			f.server.Routes().ServeHTTP(rec, httptest.NewRequest("POST", "/api/imports/apply", strings.NewReader(string(raw))))
			if rec.Code != 200 {
				t.Errorf("status %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if len(f.server.currentConfig().Providers) != 2 || len(f.server.deps.Store.Names()) != 1 {
		t.Fatal("concurrent import duplicated objects")
	}
}
func TestImportRequiresPrivilegedManagementAccess(t *testing.T) {
	for _, path := range []string{"/api/imports/preview", "/api/imports/apply"} {
		if !isHighRiskManagementRequest(httptest.NewRequest("POST", path, nil)) {
			t.Fatal("import bypassed high-risk gate")
		}
	}
	f := importFixture(t)
	f.server.deps.Security = f.control
	rec, _ := f.do("POST", "/api/imports/apply", importRequest(t))
	if rec.Code == http.StatusOK || len(f.server.deps.Store.Names()) != 0 {
		t.Fatal("unauthenticated import succeeded")
	}
	for _, path := range []string{"/import.html", "/import.js", "/import.css"} {
		rec, _ := f.do("GET", path, nil)
		requireStatus(t, rec, 200)
	}
}
