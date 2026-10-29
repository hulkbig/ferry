package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/domain"
	"ferry/internal/oauth"
	"ferry/internal/secretstore"
)

func newCodexPrioritySyncFixture(t *testing.T) *v2APIFixture {
	t.Helper()
	f := newV2APIFixture(t)
	store, err := secretstore.Open(t.TempDir() + "/oauth.json")
	if err != nil {
		t.Fatal(err)
	}
	manager := oauth.NewManager(store)
	auth, _ := json.Marshal(map[string]any{"tokens": map[string]any{
		"access_token":  managementJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}),
		"refresh_token": "test-refresh", "account_id": "test-account",
	}})
	if _, err := manager.ImportAuthJSON("work", auth); err != nil {
		t.Fatal(err)
	}
	f.server.deps.OAuth = manager
	next, err := cloneConfig(f.server.deps.Config)
	if err != nil {
		t.Fatal(err)
	}
	next.Providers[0].Type = domain.ProviderCliproxyCodex
	next.Providers[0].BaseURL = domain.CodexBaseURL
	next.Connections[0].Credential.SecretRef = domain.OAuthRef("work")
	next.Connections[0].Models = append(next.Connections[0].Models, domain.ConnectionModel{
		UpstreamModel: "gpt-6-astra", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses},
		Capabilities: domain.Capabilities{Text: true},
	})
	if err := f.server.publishConfig(next); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCodexSyncRefreshesPriorityForExistingAndNewModels(t *testing.T) {
	f := newCodexPrioritySyncFixture(t)

	var catalog atomic.Value
	setCatalog := func(known, priority bool) {
		var models []map[string]any
		for _, id := range []string{"gpt-6-astra", "gpt-new"} {
			entry := map[string]any{"slug": id, "visibility": "list", "input_modalities": []string{"text"}}
			if known {
				tiers := []map[string]string{}
				if priority {
					tiers = append(tiers, map[string]string{"id": "priority", "name": "Fast"})
				}
				entry["service_tiers"] = tiers
			}
			models = append(models, entry)
		}
		body, _ := json.Marshal(map[string]any{"models": models})
		catalog.Store(body)
	}
	setCatalog(true, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("sync sent %s; expected metadata GET only", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(catalog.Load().([]byte))
	}))
	defer server.Close()
	previousURL := oauth.CurrentModelCatalogURL()
	oauth.SetModelCatalogURL(server.URL)
	defer oauth.SetModelCatalogURL(previousURL)

	syncAndCheck := func(want bool) int64 {
		t.Helper()
		rec, _ := f.do(http.MethodPost, "/api/providers/provider-a/models/sync", nil)
		requireStatus(t, rec, http.StatusOK)
		active, revision, err := f.control.ActiveConfig()
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, model := range active.Connections[0].Models {
			if model.UpstreamModel != "gpt-6-astra" && model.UpstreamModel != "gpt-new" {
				continue
			}
			found++
			if model.Capabilities.Priority != want {
				t.Fatalf("%s priority=%v want=%v", model.UpstreamModel, model.Capabilities.Priority, want)
			}
		}
		if found != 2 {
			t.Fatalf("found %d synchronized models", found)
		}
		return revision
	}
	first := syncAndCheck(true)
	if got := syncAndCheck(true); got != first {
		t.Fatalf("identical catalog changed revision %d -> %d", first, got)
	}
	setCatalog(false, false)
	if got := syncAndCheck(true); got != first {
		t.Fatalf("unknown metadata erased declaration: revision %d -> %d", first, got)
	}
	setCatalog(true, false)
	if got := syncAndCheck(false); got <= first {
		t.Fatalf("explicit revocation was not published: revision %d", got)
	}
}

func TestPriorityUsesAuthoritativeEvidenceInsteadOfInferenceProbes(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic, domain.ProtocolOpenAIImages} {
		result := providerProtocolModelProbeResult{}
		appendProviderNonProbeCapabilityEvidence(&result, protocol)
		found := false
		for _, evidence := range result.CapabilityEvidence {
			if evidence.Capability != domain.CapabilityPriority {
				continue
			}
			found = true
			wantStatus, wantSource := "unknown", "authoritative-config-required"
			if protocol != domain.ProtocolOpenAIChat && protocol != domain.ProtocolOpenAIResponses {
				wantStatus, wantSource = "unsupported", "protocol-contract"
			}
			if evidence.Status != wantStatus || evidence.Source != wantSource {
				t.Fatalf("%s: %+v", protocol, evidence)
			}
		}
		if !found {
			t.Fatalf("%s lacks priority evidence strategy", protocol)
		}
		for _, capability := range domain.ProbeCapabilitiesForProtocol(protocol) {
			if capability == domain.CapabilityPriority {
				t.Fatal("priority cannot be established by an inference probe")
			}
		}
	}
}

func TestGenericCatalogDoesNotErasePriorityDeclaration(t *testing.T) {
	connection := &domain.Connection{Models: []domain.ConnectionModel{{
		UpstreamModel: "model", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
		Capabilities: domain.Capabilities{Text: true, Priority: true},
	}}}
	updated := map[string]bool{}
	applySynchronizedModelCapabilities(connection, []int{0}, domain.ProviderCustom, "model",
		&providerCatalogModelEntry{Authoritative: true}, domain.Capabilities{Text: true, Tools: true}, updated)
	if !connection.Models[0].Capabilities.Priority || !connection.Models[0].Capabilities.Tools || !updated["model"] {
		t.Fatalf("generic catalog must refresh features without erasing account/operator priority: %+v", connection.Models[0])
	}
}
