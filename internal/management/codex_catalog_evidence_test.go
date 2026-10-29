package management

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

func TestCodexSyncSeparatesVisibilityAndPriorityEvidence(t *testing.T) {
	f := newCodexPrioritySyncFixture(t)
	original, _, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	var catalog atomic.Value
	catalog.Store(`{"models":[{"slug":"gpt-6-astra","visibility":"hide"}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("client_version") != domain.CodexClientVersion ||
			r.Header.Get("Version") != domain.CodexClientVersion || r.Header.Get("User-Agent") != domain.CodexUserAgent {
			t.Error("catalog read did not use the pinned client contract")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(catalog.Load().(string)))
	}))
	defer server.Close()
	previousURL := oauth.CurrentModelCatalogURL()
	oauth.SetModelCatalogURL(server.URL)
	defer oauth.SetModelCatalogURL(previousURL)

	// A successful account catalog with no visible models must not fall back
	// to the static plan list or reveal hidden models through discovery.
	rec, discovery := f.do(http.MethodPost, "/api/providers/discover-models", map[string]any{
		"type": "cliproxy-codex", "credential_ref": domain.OAuthRef("work"),
	})
	requireStatus(t, rec, http.StatusOK)
	if models, ok := discovery["models"].([]any); !ok || len(models) != 0 || discovery["source"] != "oauth-catalog" {
		t.Fatalf("hidden catalog leaked into discovery/fallback: %v", discovery)
	}

	var previousRevision int64
	for _, tc := range []struct {
		name, astra, account string
		priority             bool
		updated              int
	}{
		{"unknown", `{"slug":"gpt-6-astra","visibility":"hide"}`, "unknown", false, 0},
		{"supported", `{"slug":"gpt-6-astra","visibility":"hide","service_tiers":[{"id":"priority"}]}`, "supported", true, 1},
		{"repeat", `{"slug":"gpt-6-astra","visibility":"hide","service_tiers":[{"id":"priority"}]}`, "supported", true, 0},
		{"absent tiers", `{"slug":"gpt-6-astra","visibility":"hide"}`, "unknown", true, 0},
		{"null tiers", `{"slug":"gpt-6-astra","visibility":"hide","service_tiers":null}`, "unknown", true, 0},
		{"revoked", `{"slug":"gpt-6-astra","visibility":"hide","service_tiers":[]}`, "unsupported", false, 1},
		{"missing model", ``, "unknown", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := `{"slug":"hidden-new","visibility":"hide","service_tiers":[{"id":"priority"}]}`
			if tc.astra != "" {
				entries += "," + tc.astra
			}
			catalog.Store(`{"models":[` + entries + `]}`)
			rec, response := f.do(http.MethodPost, "/api/providers/provider-a/models/sync", nil)
			requireStatus(t, rec, http.StatusOK)
			if response["status"] != "succeeded" || response["discovered_models"] != float64(0) ||
				response["added_models"] != float64(0) || response["updated_models"] != float64(tc.updated) {
				t.Fatalf("sync result=%v", response)
			}
			active, revision, err := f.control.ActiveConfig()
			if err != nil {
				t.Fatal(err)
			}
			if previousRevision != 0 && tc.updated == 0 && revision != previousRevision {
				t.Fatalf("unchanged declarations changed revision %d -> %d", previousRevision, revision)
			}
			previousRevision = revision
			if len(active.Connections[0].Models) != len(original.Connections[0].Models) || logicalModelIndex(active, "hidden-new") >= 0 {
				t.Fatal("unconfigured hidden model was added")
			}
			for index, model := range active.Connections[0].Models {
				if model.UpstreamModel == "gpt-6-astra" {
					if model.Capabilities.Priority != tc.priority {
						t.Fatalf("stored Astra priority=%v want=%v", model.Capabilities.Priority, tc.priority)
					}
				} else if !reflect.DeepEqual(model, original.Connections[0].Models[index]) {
					t.Fatalf("unrelated model changed: %+v", model)
				}
			}
			before := original.LogicalModels[logicalModelIndex(original, "smart")].DefaultRoute
			after := active.LogicalModels[logicalModelIndex(active, "smart")].DefaultRoute
			if !reflect.DeepEqual(before, after) {
				t.Fatal("custom route changed during hidden-model reconciliation")
			}
			evidence, ok := response["codex_catalog"].(map[string]any)
			if !ok || evidence["client_version"] != domain.CodexClientVersion ||
				evidence["baseline_source"] != domain.CodexFastBaselineSource || evidence["baseline_sha256"] != domain.CodexFastBaselineSHA256 {
				t.Fatalf("missing catalog provenance: %v", evidence)
			}
			found := false
			for _, raw := range evidence["models"].([]any) {
				row := raw.(map[string]any)
				if row["model"] == "hidden-new" {
					t.Fatal("unconfigured hidden model leaked into sync evidence")
				}
				if row["model"] != "gpt-6-astra" {
					if row["model_priority"] != "unknown" || row["account_priority"] != "unknown" {
						t.Fatalf("missing metadata reported as unsupported: %v", row)
					}
					continue
				}
				found = true
				if row["model_priority"] != "supported" || row["model_minimal_client_version"] != "0.153.0" ||
					row["account_priority"] != tc.account || row["connection_priority"] != tc.priority {
					t.Fatalf("model, account and connection evidence conflated: %v", row)
				}
			}
			if !found || strings.Contains(rec.Body.String(), "test-account") || strings.Contains(rec.Body.String(), "test-refresh") {
				t.Fatal("sync evidence must contain configured Astra without account credentials")
			}
		})
	}
}
