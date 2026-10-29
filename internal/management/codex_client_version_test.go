package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

func TestCodexVersionPublishAndCatalogReconciliation(t *testing.T) {
	f := newCodexPrioritySyncFixture(t)
	original, revision, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := r.URL.Query().Get("client_version")
		if r.Header.Get("Version") != version || r.Header.Get("User-Agent") != domain.CodexUserAgentForVersion(version) {
			t.Error("inconsistent version envelope")
		}
		w.Header().Set("Content-Type", "application/json")
		if version == "0.154.0" {
			fmt.Fprint(w, `{"models":[{"slug":"new-release","visibility":"list"},{"slug":"hidden-release","visibility":"hide"}]}`)
		} else {
			fmt.Fprint(w, `{"models":[{"slug":"gpt-6-astra","visibility":"hide"}]}`)
		}
	}))
	defer server.Close()
	previous := oauth.CurrentModelCatalogURL()
	oauth.SetModelCatalogURL(server.URL)
	defer oauth.SetModelCatalogURL(previous)
	rec, _ := f.do(http.MethodPost, "/api/providers/provider-a/models/sync", nil)
	requireStatus(t, rec, http.StatusOK)
	beforeFingerprint := providerModelSyncFingerprint(original, "provider-a")
	rec, draft := f.do(http.MethodPost, "/api/drafts", map[string]any{"base_revision": revision, "intent": map[string]any{"codex_client_version": "0.154.0"}})
	requireStatus(t, rec, http.StatusCreated)
	id := draft["id"].(string)
	active, _, _ := f.control.ActiveConfig()
	if active.CodexClientVersion != "" {
		t.Fatal("draft changed active config")
	}
	rec, validation := f.do(http.MethodPost, "/api/drafts/"+id+"/validate", map[string]any{})
	requireStatus(t, rec, http.StatusOK)
	if validation["valid"] != true {
		t.Fatalf("validation=%v", validation)
	}
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+id+"/apply", map[string]any{"expected_revision": revision + 1})
	requireStatus(t, rec, http.StatusConflict)
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+id+"/apply", map[string]any{"expected_revision": revision})
	requireStatus(t, rec, http.StatusOK)
	active, _, _ = f.control.ActiveConfig()
	if active.CodexClientVersion != "0.154.0" || beforeFingerprint == providerModelSyncFingerprint(active, "provider-a") {
		t.Fatal("version not persisted or schedule not invalidated")
	}
	rec, settings := f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, rec, http.StatusOK)
	if settings["codex_effective_client_version"] != "0.154.0" {
		t.Fatalf("settings=%v", settings)
	}
	for i := 0; i < 2; i++ {
		rec, result := f.do(http.MethodPost, "/api/providers/provider-a/models/sync", nil)
		requireStatus(t, rec, http.StatusOK)
		if result["added_models"] != float64(1-i) {
			t.Fatalf("sync=%v", result)
		}
	}
	active, afterRevision, _ := f.control.ActiveConfig()
	for _, old := range original.Connections[0].Models {
		found := false
		for _, current := range active.Connections[0].Models {
			if reflect.DeepEqual(old, current) {
				found = true
			}
		}
		if !found {
			t.Fatalf("old model lost: %v", old)
		}
	}
	if !reflect.DeepEqual(original.LogicalModels[logicalModelIndex(original, "smart")], active.LogicalModels[logicalModelIndex(active, "smart")]) {
		t.Fatal("custom route changed")
	}
	if logicalModelIndex(active, "new-release") < 0 || logicalModelIndex(active, "hidden-release") >= 0 {
		t.Fatal("visible/hidden addition policy violated")
	}
	rec, restore := f.do(http.MethodPost, fmt.Sprintf("/api/revisions/%d/restore-draft", revision), map[string]any{})
	requireStatus(t, rec, http.StatusCreated)
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+restore["id"].(string)+"/apply", map[string]any{"expected_revision": afterRevision})
	requireStatus(t, rec, http.StatusOK)
	active, _, _ = f.control.ActiveConfig()
	if active.CodexClientVersion != "" {
		t.Fatal("restore did not restore bundled version")
	}
}

func TestCodexVersionChangeRejectsInFlightCatalog(t *testing.T) {
	f := newCodexPrioritySyncFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"models":[{"slug":"stale-release","visibility":"list"}]}`)
	}))
	defer server.Close()
	previous := oauth.CurrentModelCatalogURL()
	oauth.SetModelCatalogURL(server.URL)
	defer oauth.SetModelCatalogURL(previous)
	resultCh := make(chan providerModelSyncResult, 1)
	go func() { resultCh <- f.server.syncProviderModels(context.Background(), "provider-a") }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("catalog not started")
	}
	next, err := cloneConfig(f.server.currentConfig())
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	next.CodexClientVersion = "0.154.0"
	if err = f.server.publishConfig(next); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	result := <-resultCh
	if result.HTTPStatus != http.StatusConflict || result.State.LastErrorCode != "configuration_changed" {
		t.Fatalf("stale result=%+v", result)
	}
	active, _, _ := f.control.ActiveConfig()
	if logicalModelIndex(active, "stale-release") >= 0 || active.CodexClientVersion != "0.154.0" {
		t.Fatal("stale catalog overwrote new revision")
	}
}
