package config

import (
	"fmt"
	"testing"

	"ferry/internal/domain"
)

func TestLogicalModelLimitOnlyAppliesToListedHosts(t *testing.T) {
	for _, tc := range []struct {
		url     string
		limited bool
	}{
		{"https://openrouter.ai/api/v1", true},
		{"https://OPENROUTER.AI.:443/api/v1", true},
		{"http://openrouter.ai:8080/v1", true},
		{"https://openrouter.ai.evil.test/v1", false},
		{"https://example.test/openrouter.ai", false},
		{"https://openrouter.ai@evil.test", false},
		{"https://chatgpt.com/backend-api/codex", false},
		{"https://open.bigmodel.cn/api/paas/v4", false},
		{"https://custom-gateway.test/v1", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			zero := 0
			f := &File{AutoLogicalModelLimit: &zero, Providers: []domain.Provider{{ID: "p", BaseURL: tc.url, Enabled: true}}, Connections: []domain.Connection{{ID: "c", ProviderID: "p", Enabled: true, Models: []domain.ConnectionModel{{UpstreamModel: "gpt-6.1-terra"}}}}}
			created := EnsureSameNameLogicalModels(f)
			if (len(created) == 0) != tc.limited {
				t.Fatalf("created=%v limited=%v", created, tc.limited)
			}
		})
	}
}

func TestLogicalModelLimitConnectionOverride(t *testing.T) {
	for _, full := range []bool{false, true} {
		c := domain.Connection{ID: "c", ProviderID: "p", Enabled: true, Models: []domain.ConnectionModel{{UpstreamModel: "model"}}}
		if full {
			c.FullURL = "https://openrouter.ai/api/v1/chat/completions"
		} else {
			c.BaseURL = "https://openrouter.ai/api/v1"
		}
		zero := 0
		f := &File{AutoLogicalModelLimit: &zero, Providers: []domain.Provider{{ID: "p", Enabled: true}}, Connections: []domain.Connection{c}}
		if got := EnsureSameNameLogicalModels(f); len(got) != 0 {
			t.Fatalf("override escaped limit: %v", got)
		}
	}
}

func TestLogicalModelDirectProviderRepairsMissingTerraOverLimit(t *testing.T) {
	for _, typ := range []domain.ProviderType{domain.ProviderCliproxyCodex, domain.ProviderCustom} {
		f := &File{Providers: []domain.Provider{{ID: "direct", Type: typ, Enabled: true}}, Connections: []domain.Connection{{ID: "c", ProviderID: "direct", Enabled: true}}}
		for i := 0; i < 12; i++ {
			id := fmt.Sprintf("old-%d", i)
			f.Connections[0].Models = append(f.Connections[0].Models, domain.ConnectionModel{UpstreamModel: id})
		}
		EnsureSameNameLogicalModels(f)
		f.Connections[0].Models = append(f.Connections[0].Models, domain.ConnectionModel{UpstreamModel: "gpt-6.1-terra"})
		got := EnsureSameNameLogicalModels(f)
		if len(got) != 1 || got[0] != "gpt-6.1-terra" {
			t.Fatalf("missing terra: %v", got)
		}
		if got := EnsureSameNameLogicalModels(f); len(got) != 0 {
			t.Fatalf("not idempotent: %v", got)
		}
	}
}

func TestLogicalModelLimitedAggregatorDoesNotMaskDirectProvider(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		zero := 0
		connections := []domain.Connection{
			{ID: "a", ProviderID: "aggregator", Enabled: true, Models: []domain.ConnectionModel{{UpstreamModel: "shared"}}},
			{ID: "d", ProviderID: "direct", Enabled: true, Models: []domain.ConnectionModel{{UpstreamModel: "shared"}}},
		}
		if reverse {
			connections[0], connections[1] = connections[1], connections[0]
		}
		f := &File{AutoLogicalModelLimit: &zero, Providers: []domain.Provider{{ID: "aggregator", Enabled: true, BaseURL: "https://openrouter.ai"}, {ID: "direct", Enabled: true}}, Connections: connections}
		if got := EnsureSameNameLogicalModels(f); len(got) != 1 || f.LogicalModels[0].DefaultRoute.Steps[0].ProviderID != "direct" {
			t.Fatalf("blocked direct model: %+v", f.LogicalModels)
		}
		if got := EnsureSameNameLogicalModels(f); len(got) != 0 || f.LogicalModels[0].DefaultRoute.Steps[0].ProviderID != "direct" {
			t.Fatalf("reconciliation changed selected source: %+v", f.LogicalModels)
		}
	}
}
