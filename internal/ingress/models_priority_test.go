package ingress

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ferry/internal/domain"
)

func TestModelsPublishOnlyRoutablePriorityDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.CompiledSnapshot, *domain.LogicalModel, *domain.CompiledTarget)
		want   bool
	}{
		{"declared", func(*domain.CompiledSnapshot, *domain.LogicalModel, *domain.CompiledTarget) {}, true},
		{"no-name-inference", func(_ *domain.CompiledSnapshot, _ *domain.LogicalModel, target *domain.CompiledTarget) {
			target.Capabilities.Priority = false
		}, false},
		{"provider-disabled", func(snapshot *domain.CompiledSnapshot, _ *domain.LogicalModel, _ *domain.CompiledTarget) {
			snapshot.Providers["private"].Enabled = false
		}, false},
		{"connection-disabled", func(snapshot *domain.CompiledSnapshot, _ *domain.LogicalModel, _ *domain.CompiledTarget) {
			snapshot.Connections["c"].Enabled = false
		}, false},
		{"target-disabled", func(_ *domain.CompiledSnapshot, _ *domain.LogicalModel, target *domain.CompiledTarget) {
			target.Disabled = true
		}, false},
		{"protocol-isolation", func(_ *domain.CompiledSnapshot, _ *domain.LogicalModel, target *domain.CompiledTarget) {
			target.ProtocolCapabilities = map[domain.Protocol]domain.Capabilities{domain.ProtocolOpenAIResponses: {Text: true}}
		}, false},
		{"outside-route", func(_ *domain.CompiledSnapshot, _ *domain.LogicalModel, target *domain.CompiledTarget) {
			target.Model = "unrelated"
		}, false},
		{"custom-override", func(_ *domain.CompiledSnapshot, model *domain.LogicalModel, _ *domain.CompiledTarget) {
			model.Overrides = []domain.OverrideSpec{{Enabled: true, Mode: domain.OverrideCustomRoute, Route: &domain.RouteSpec{}}}
		}, false},
		{"avoided", func(_ *domain.CompiledSnapshot, model *domain.LogicalModel, _ *domain.CompiledTarget) {
			model.Overrides = []domain.OverrideSpec{{Enabled: true, Mode: domain.OverrideAdjustDefault, Avoid: model.DefaultRoute.Steps}}
		}, false},
		{"unrelated-override", func(_ *domain.CompiledSnapshot, model *domain.LogicalModel, _ *domain.CompiledTarget) {
			model.Overrides = []domain.OverrideSpec{{Enabled: true, Protocols: []domain.Protocol{domain.ProtocolAnthropic}, Mode: domain.OverrideCustomRoute, Route: &domain.RouteSpec{}}}
		}, true},
		{"converted-chat", func(_ *domain.CompiledSnapshot, _ *domain.LogicalModel, target *domain.CompiledTarget) {
			target.NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIChat}
		}, true},
		{"conversion-disabled", func(_ *domain.CompiledSnapshot, _ *domain.LogicalModel, target *domain.CompiledTarget) {
			target.NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIChat}
			target.ProtocolConversion = map[domain.Protocol]bool{}
		}, false},
		{"exact-only", func(_ *domain.CompiledSnapshot, model *domain.LogicalModel, target *domain.CompiledTarget) {
			model.ExactOnly = true
			target.NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIChat}
		}, false},
		{"anthropic-is-not-openai-priority", func(_ *domain.CompiledSnapshot, _ *domain.LogicalModel, target *domain.CompiledTarget) {
			target.NativeProtocols = []domain.Protocol{domain.ProtocolAnthropic}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &domain.LogicalModel{ID: "gpt-6-astra", Enabled: true, DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "private", UpstreamModel: "upstream"}}}}
			target := &domain.CompiledTarget{ID: "t", ProviderID: "private", ConnectionID: "c", Model: "upstream", NativeProtocols: []domain.Protocol{domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true, Priority: true}}
			snapshot := &domain.CompiledSnapshot{
				LogicalModels: map[string]*domain.LogicalModel{model.ID: model},
				Providers:     map[string]*domain.Provider{"private": {ID: "private", Enabled: true}},
				Connections:   map[string]*domain.Connection{"c": {ID: "c", Enabled: true}},
				Targets:       map[domain.ResourceTargetID]*domain.CompiledTarget{"t": target},
			}
			tc.change(snapshot, model, target)
			engine := newStub()
			engine.snapshot = snapshot
			rec := httptest.NewRecorder()
			(&ModelsHandler{Source: engine}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", rec.Code, rec.Body)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			var rows []map[string]json.RawMessage
			if err := json.Unmarshal(payload["data"], &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("models=%s", payload["data"])
			}
			var tiers map[domain.Protocol][]string
			var authority map[domain.Protocol]domain.ResponseTierAuthority
			if err := json.Unmarshal(rows[0]["response_tier_authority"], &authority); err != nil || len(authority) != 2 {
				t.Fatalf("authority=%v err=%v", authority, err)
			}
			if err := json.Unmarshal(rows[0]["supported_service_tiers"], &tiers); err != nil {
				t.Fatal(err)
			}
			values, exists := tiers[domain.ProtocolOpenAIResponses]
			if !exists || values == nil {
				t.Fatal("metadata must explicitly distinguish no support from unknown server schema")
			}
			got := len(values) == 1 && values[0] == "priority"
			if got != tc.want {
				t.Fatalf("tiers=%v want priority=%v", tiers, tc.want)
			}
		})
	}
}
