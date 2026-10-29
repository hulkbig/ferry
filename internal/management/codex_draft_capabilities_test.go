package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

func TestCodexModelEditsResolveAccountCapabilitiesBeforeReview(t *testing.T) {
	f := newCodexPrioritySyncFixture(t)
	original, revision, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"models":[
			{"slug":"future-text","visibility":"list","input_modalities":["text","image"],"context_window":872000,"service_tiers":[{"id":"priority"}]},
			{"slug":"future-image","visibility":"list","input_modalities":["image"]}
		]}`)
	}))
	defer server.Close()
	previous := oauth.CurrentModelCatalogURL()
	oauth.SetModelCatalogURL(server.URL)
	defer oauth.SetModelCatalogURL(previous)

	models := append([]domain.ConnectionModel(nil), original.Connections[0].Models...)
	for _, id := range []string{"future-text", "future-image", "unknown-manual"} {
		models = append(models, domain.ConnectionModel{UpstreamModel: id,
			Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true}})
	}
	for _, mode := range []string{"connection", "provider", "draft-create", "draft-patch"} {
		t.Run(mode, func(t *testing.T) {
			var draftID string
			var rec *httptest.ResponseRecorder
			var response map[string]any
			switch mode {
			case "connection":
				rec, response = f.do(http.MethodPatch, "/api/providers/provider-a/connections/connection-a", map[string]any{"models": models})
			case "provider":
				rec, response = f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{
					"connections": []map[string]any{{"id": "connection-a", "models": models}},
				})
			default:
				intent := v2IntentOf(original)
				// Round-trip the public DTO exactly as an API consumer would.
				raw, _ := json.Marshal(intent)
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				body["connections"].([]any)[0].(map[string]any)["models"] = models
				if mode == "draft-create" {
					rec, response = f.do(http.MethodPost, "/api/drafts", map[string]any{"base_revision": revision, "intent": body})
					requireStatus(t, rec, http.StatusCreated)
					draftID = response["id"].(string)
				} else {
					created, out := f.do(http.MethodPost, "/api/drafts", map[string]any{"base_revision": revision})
					requireStatus(t, created, http.StatusCreated)
					draftID = out["id"].(string)
					rec, response = f.do(http.MethodPatch, "/api/drafts/"+draftID, body)
					requireStatus(t, rec, http.StatusOK)
				}
			}
			if draftID == "" {
				requireStatus(t, rec, http.StatusAccepted)
				draftID = response["draft_id"].(string)
			}
			draft, err := f.control.Draft(draftID)
			if err != nil {
				t.Fatal(err)
			}
			for _, model := range draft.Intent.Connections[0].Models {
				caps := model.Capabilities
				switch model.UpstreamModel {
				case "future-text":
					if !caps.Stream || !caps.Tools || !caps.Reasoning || !caps.Vision || !caps.Priority || caps.ContextWindow != 872000 {
						t.Fatalf("new text model lacks account contract: %+v", model)
					}
				case "future-image":
					if !reflect.DeepEqual(model.Protocols, []domain.Protocol{domain.ProtocolOpenAIImages}) || caps != (domain.Capabilities{Images: true}) {
						t.Fatalf("image-only contract was guessed from name: %+v", model)
					}
				case "unknown-manual":
					if caps != (domain.Capabilities{Text: true}) {
						t.Fatalf("catalog-absent model gained unsupported features: %+v", model)
					}
				}
			}
			active, activeRevision, _ := f.control.ActiveConfig()
			if activeRevision != revision || !reflect.DeepEqual(active, original) {
				t.Fatal("draft enrichment changed live configuration")
			}
		})
	}
	fail.Store(true)
	rec, _ := f.do(http.MethodPatch, "/api/providers/provider-a/connections/connection-a", map[string]any{"models": models})
	if rec.Code < 400 {
		t.Fatal("catalog failure silently accepted incomplete model capabilities")
	}
	count := requests.Load()
	rec, _ = f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{"name": "renamed"})
	requireStatus(t, rec, http.StatusAccepted)
	if requests.Load() != count {
		t.Fatal("unrelated provider edit unnecessarily fetched account metadata")
	}
}
