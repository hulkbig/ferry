package management

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

func TestCodexSyncHealsExistingCatalogCapabilities(t *testing.T) {
	f := newCodexPrioritySyncFixture(t)
	next, err := cloneConfig(f.server.currentConfig())
	if err != nil {
		t.Fatal(err)
	}
	stale := domain.Capabilities{Text: true, Priority: true}
	for _, id := range []string{"gpt-6.1-sol", "hidden-configured", "absent-from-catalog"} {
		next.Connections[0].Models = append(next.Connections[0].Models, domain.ConnectionModel{
			UpstreamModel: id, Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}, Capabilities: stale,
		})
	}
	next.Connections[0].Models = append(next.Connections[0].Models, domain.ConnectionModel{
		UpstreamModel: "future-image", Protocols: []domain.Protocol{domain.ProtocolOpenAIImages},
		Capabilities: domain.Capabilities{Images: true},
	})
	if err := f.server.publishConfig(next); err != nil {
		t.Fatal(err)
	}
	original, _, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	var catalog atomic.Value
	catalog.Store(`{"models":[
		{"slug":"gpt-6.1-sol","visibility":"list","context_window":872000,"input_modalities":["text","image"]},
		{"slug":"hidden-configured","visibility":"hide","input_modalities":["text"],"service_tiers":[]},
		{"slug":"hidden-new","visibility":"hide","input_modalities":["text"]},
		{"slug":"future-image","visibility":"list","input_modalities":["image"]}
	]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("catalog sync issued %s instead of metadata GET", r.Method)
		}
		fmt.Fprint(w, catalog.Load().(string))
	}))
	defer server.Close()
	previous := oauth.CurrentModelCatalogURL()
	oauth.SetModelCatalogURL(server.URL)
	defer oauth.SetModelCatalogURL(previous)

	var healedRevision int64
	for run := 0; run < 3; run++ {
		if run == 2 {
			// Loss of optional metadata must not erase an established contract.
			catalog.Store(`{"models":[{"slug":"gpt-6.1-sol","visibility":"list","input_modalities":["text"]}]}`)
		}
		rec, result := f.do(http.MethodPost, "/api/providers/provider-a/models/sync", nil)
		requireStatus(t, rec, http.StatusOK)
		wantUpdates := float64(0)
		if run == 0 {
			wantUpdates = 2
		}
		if result["updated_models"] != wantUpdates || result["added_models"] != float64(0) {
			t.Fatalf("run %d: unexpected sync result %v", run, result)
		}
		active, revision, err := f.control.ActiveConfig()
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			healedRevision = revision
		} else if revision != healedRevision {
			t.Fatalf("unchanged evidence published revision %d instead of %d", revision, healedRevision)
		}
		for index, model := range active.Connections[0].Models {
			caps := model.Capabilities
			switch model.UpstreamModel {
			case "gpt-6.1-sol", "hidden-configured":
				if !caps.Text || !caps.Stream || !caps.Tools || !caps.ParallelTools || !caps.Reasoning || !caps.StructuredOutput {
					t.Fatalf("existing model was not healed: %+v", model)
				}
				if model.UpstreamModel == "gpt-6.1-sol" && (!caps.Priority || !caps.Vision || caps.ContextWindow != 872000) {
					t.Fatalf("catalog evidence/unknown priority not preserved: %+v", model)
				}
				if model.UpstreamModel == "hidden-configured" && caps.Priority {
					t.Fatal("explicit priority revocation was ignored")
				}
			default:
				if !reflect.DeepEqual(model, original.Connections[0].Models[index]) {
					t.Fatalf("unrelated, image-only or absent model changed: %+v", model)
				}
			}
		}
		if !reflect.DeepEqual(active.LogicalModels, original.LogicalModels) ||
			len(active.Connections[0].Models) != len(original.Connections[0].Models) {
			t.Fatal("capability healing changed routing or added a hidden model")
		}
	}
}
