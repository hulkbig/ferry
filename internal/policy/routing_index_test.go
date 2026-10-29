package policy

import (
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestRoutingIndexSelectsFirstEnabledMatch(t *testing.T) {
	index, err := CompileRoutingIndex([]domain.RoutingPolicySpec{
		{
			ID:       "glm-afternoon",
			Priority: 10,
			Enabled:  true,
			Match: domain.RoutingPolicyMatch{
				Models:     []string{"glm-5"},
				TimeWindow: &domain.TimeWindow{Start: "12:00", End: "18:00"},
				Timezone:   "Asia/Shanghai",
			},
			Route: routeGroup("non-glm"),
		},
		{
			ID:       "disabled-shadow",
			Priority: 20,
			Enabled:  false,
			Route:    routeGroup("must-not-match"),
		},
		{ID: "catch-all", Priority: 30, Enabled: true, Route: routeGroup("default")},
	})
	if err != nil {
		t.Fatal(err)
	}

	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	ctx := RoutingContext{
		Consumer: "personal",
		Model:    "glm-5",
		Protocol: domain.ProtocolOpenAIChat,
		Now:      time.Date(2026, 8, 24, 15, 0, 0, 0, shanghai),
	}
	selected := index.Select(ctx)
	if selected == nil || selected.ID != "glm-afternoon" {
		t.Fatalf("selected %#v, want glm-afternoon", selected)
	}

	ctx.Now = time.Date(2026, 8, 24, 20, 0, 0, 0, shanghai)
	selected = index.Select(ctx)
	if selected == nil || selected.ID != "catch-all" {
		t.Fatalf("selected %#v, want catch-all", selected)
	}
}

func TestRoutingIndexHashAndCacheAreContentAddressed(t *testing.T) {
	specs := []domain.RoutingPolicySpec{{ID: "catch-all", Priority: 10, Enabled: true, Route: routeGroup("default")}}
	cache := NewRoutingIndexCache()
	a, firstHit, err := cache.CompileWithStatus(specs)
	if err != nil {
		t.Fatal(err)
	}
	b, secondHit, err := cache.CompileWithStatus(specs)
	if err != nil {
		t.Fatal(err)
	}
	if firstHit || !secondHit {
		t.Fatalf("cache status first=%v second=%v, want false then true", firstHit, secondHit)
	}
	if a != b {
		t.Fatal("same active policy set did not reuse compiled index")
	}
	if len(a.Hash) != 64 {
		t.Fatalf("index hash length = %d, want full SHA-256", len(a.Hash))
	}
}

func routeGroup(group string) domain.RoutingPolicyRoute {
	return domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: group}}}
}
