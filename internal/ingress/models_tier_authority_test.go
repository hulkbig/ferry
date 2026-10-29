package ingress

import (
	"testing"

	"ferry/internal/domain"
)

func TestModelTierAuthorityIncludesFallbackAndConditionalRoutes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.CompiledSnapshot, *domain.LogicalModel)
		want   domain.ResponseTierAuthority
	}{
		{"official-only", func(*domain.CompiledSnapshot, *domain.LogicalModel) {}, domain.TierAuthoritative},
		{"codex-only", func(s *domain.CompiledSnapshot, _ *domain.LogicalModel) {
			s.Providers["p"].Type = domain.ProviderCliproxyCodex
		}, domain.TierInformational},
		{"custom-gateway", func(s *domain.CompiledSnapshot, _ *domain.LogicalModel) {
			s.Targets["t"].Endpoint.BaseURL = "https://gateway.example/v1"
		}, domain.TierUnknown},
		{"full-url-override", func(s *domain.CompiledSnapshot, _ *domain.LogicalModel) {
			s.Targets["t"].Endpoint.FullURL = "https://gateway.example/v1/responses"
		}, domain.TierUnknown},
		{"disabled-only", func(s *domain.CompiledSnapshot, _ *domain.LogicalModel) { s.Connections["c"].Enabled = false }, domain.TierUnknown},
		{"fallback-codex", func(_ *domain.CompiledSnapshot, m *domain.LogicalModel) {
			m.DefaultRoute.Steps = append(m.DefaultRoute.Steps, domain.RouteStep{ProviderID: "codex", UpstreamModel: "upstream"})
		}, domain.TierUnknown},
		{"conditional-codex", func(_ *domain.CompiledSnapshot, m *domain.LogicalModel) {
			m.Overrides = []domain.OverrideSpec{{Enabled: true, Mode: domain.OverrideCustomRoute, Route: &domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "codex", UpstreamModel: "upstream"}}}}}
		}, domain.TierUnknown},
		{"unrelated-condition", func(_ *domain.CompiledSnapshot, m *domain.LogicalModel) {
			m.Overrides = []domain.OverrideSpec{{Enabled: true, Protocols: []domain.Protocol{domain.ProtocolAnthropic}, Mode: domain.OverrideCustomRoute, Route: &domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "codex", UpstreamModel: "upstream"}}}}}
		}, domain.TierAuthoritative},
		{"conditional-empty", func(_ *domain.CompiledSnapshot, m *domain.LogicalModel) {
			m.Overrides = []domain.OverrideSpec{{Enabled: true, Mode: domain.OverrideAdjustDefault, Avoid: m.DefaultRoute.Steps}}
		}, domain.TierUnknown},
		{"converted-anthropic-fallback", func(s *domain.CompiledSnapshot, m *domain.LogicalModel) {
			m.DefaultRoute.Steps = append(m.DefaultRoute.Steps, domain.RouteStep{ProviderID: "codex", UpstreamModel: "upstream"})
			s.Targets["other"].NativeProtocols = []domain.Protocol{domain.ProtocolAnthropic}
		}, domain.TierUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &domain.LogicalModel{ID: "any-model", Enabled: true, DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "p", UpstreamModel: "upstream"}}}}
			snapshot := &domain.CompiledSnapshot{
				Providers:   map[string]*domain.Provider{"p": {Enabled: true}, "codex": {Type: domain.ProviderCliproxyCodex, Enabled: true}},
				Connections: map[string]*domain.Connection{"c": {Enabled: true}, "other": {Enabled: true}},
				Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{
					"t":     {ProviderID: "p", ConnectionID: "c", Model: "upstream", Endpoint: &domain.Endpoint{BaseURL: "https://api.openai.com/v1", Protocol: domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true}},
					"other": {ProviderID: "codex", ConnectionID: "other", Model: "upstream", NativeProtocols: []domain.Protocol{domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true}},
				},
			}
			tc.change(snapshot, model)
			for protocol, got := range modelResponseTierAuthority(snapshot, model) {
				if got != tc.want {
					t.Fatalf("%s authority=%s want=%s", protocol, got, tc.want)
				}
			}
		})
	}
}
