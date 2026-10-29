package scheduler

import (
	"reflect"
	"testing"
	"time"

	"ferry/internal/domain"
	"ferry/internal/policy"
)

func routeStep(provider, model string) domain.RouteStep {
	return domain.RouteStep{ProviderID: provider, UpstreamModel: model}
}

func TestRetryDelayForExcludedConnectionUsesRuntimeRecoveryTime(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	targetID := domain.ResourceTargetID("target-a")
	delay, retryAt := retryDelayForExclusions(
		[]domain.ExclusionRecord{{Target: targetID, Reason: "connection offline"}},
		map[domain.ResourceTargetID]*domain.CompiledTarget{targetID: {ID: targetID, ConnectionID: "connection-a"}},
		map[string]domain.ConnectionState{"connection-a": {ConnectionID: "connection-a", RetryAt: now.Add(5 * time.Second)}}, now)
	if delay != 5*time.Second || !retryAt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("retry delay=%s retry_at=%s", delay, retryAt)
	}
}

func TestAdjustDefaultRemovesAvoidAndStablyPrefixesPrefer(t *testing.T) {
	base := []domain.RouteStep{
		routeStep("p", "a"), routeStep("p", "b"), routeStep("p", "c"), routeStep("p", "d"),
	}
	got := adjustRoute(base, []domain.RouteStep{routeStep("p", "c")}, []domain.RouteStep{routeStep("p", "b")})
	want := []domain.RouteStep{routeStep("p", "c"), routeStep("p", "a"), routeStep("p", "d")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effective route = %+v, want %+v", got, want)
	}
}

func TestOverrideWindowUsesCompiledFerryTimezone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	oldLocal := time.Local
	time.Local = time.FixedZone("host-local", -7*60*60)
	defer func() { time.Local = oldLocal }()
	model := &domain.LogicalModel{
		DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{routeStep("p", "default")}},
		Overrides: []domain.OverrideSpec{{
			ID: "business-hours", Enabled: true, Mode: domain.OverrideCustomRoute,
			Window: &domain.TimeWindow{Start: "09:00", End: "10:00"},
			Route:  &domain.RouteSpec{Steps: []domain.RouteStep{routeStep("p", "override")}},
		}},
	}
	now := time.Date(2026, 8, 26, 1, 30, 0, 0, time.UTC) // 09:30 Shanghai
	selected := selectV2Route(model, "caller-id", "caller", domain.ProtocolOpenAIChat, domain.Requirements{}, now, shanghai)
	if selected.OverrideID != "business-hours" || selected.Route.Steps[0].UpstreamModel != "override" {
		t.Fatalf("configured timezone did not match: %+v", selected)
	}
}

func TestOverrideMatchesProtocolWeekdayAndOwnTimezone(t *testing.T) {
	runtimeZone := time.FixedZone("runtime", -7*60*60)
	model := &domain.LogicalModel{
		DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{routeStep("p", "default")}},
		Overrides: []domain.OverrideSpec{{
			ID: "shanghai-weekday-chat", Enabled: true, Mode: domain.OverrideCustomRoute,
			Protocols: []domain.Protocol{domain.ProtocolOpenAIChat}, Weekdays: []string{"wed"},
			Timezone: "Asia/Shanghai", Window: &domain.TimeWindow{Start: "09:00", End: "10:00"},
			Route: &domain.RouteSpec{Steps: []domain.RouteStep{routeStep("p", "override")}},
		}},
	}
	now := time.Date(2026, 8, 26, 1, 30, 0, 0, time.UTC) // Wednesday 09:30 Shanghai
	selected := selectV2Route(model, "caller-id", "caller", domain.ProtocolOpenAIChat, domain.Requirements{}, now, runtimeZone)
	if selected.OverrideID != "shanghai-weekday-chat" {
		t.Fatalf("protocol/weekday/timezone override did not match: %+v", selected)
	}
	selected = selectV2Route(model, "caller-id", "caller", domain.ProtocolAnthropic, domain.Requirements{}, now, runtimeZone)
	if selected.OverrideID != "" {
		t.Fatalf("protocol mismatch selected override: %+v", selected)
	}
}

func TestOverrideMatchesStableCallerIDOrLegacyCallerName(t *testing.T) {
	model := &domain.LogicalModel{
		DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{routeStep("p", "default")}},
		Overrides: []domain.OverrideSpec{
			{ID: "legacy-name", Enabled: true, Callers: []string{"production"}, Mode: domain.OverrideCustomRoute,
				Route: &domain.RouteSpec{Steps: []domain.RouteStep{routeStep("p", "legacy")}}},
		},
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	selected := selectV2Route(model, "key_123", "production", domain.ProtocolOpenAIChat, domain.Requirements{}, now, time.UTC)
	if selected.OverrideID != "legacy-name" {
		t.Fatalf("legacy caller name did not match stable identity: %+v", selected)
	}
	model.Overrides[0].Callers = []string{"key_123"}
	selected = selectV2Route(model, "key_123", "production", domain.ProtocolOpenAIChat, domain.Requirements{}, now, time.UTC)
	if selected.OverrideID != "legacy-name" {
		t.Fatalf("stable caller id did not match: %+v", selected)
	}
}

func TestAffinityReorderRegeneratesEveryCandidateRank(t *testing.T) {
	result := &policy.PlanResult{
		OrderTiers: [][]domain.ResourceTargetID{{"a", "b", "c"}},
		Order:      []domain.ResourceTargetID{"a", "b", "c"},
		Candidates: []domain.CandidateRecord{
			{Target: "a", Tier: "route-01", Eligible: true, Rank: 1},
			{Target: "b", Tier: "route-01", Eligible: true, Rank: 2},
			{Target: "c", Tier: "route-01", Eligible: true, Rank: 3},
		},
	}
	snapshot := &domain.CompiledSnapshot{Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{
		"a": {ProviderID: "p", ConnectionID: "a", Model: "m"},
		"b": {ProviderID: "p", ConnectionID: "b", Model: "m"},
		"c": {ProviderID: "p", ConnectionID: "c", Model: "m"},
	}}
	if !applyAffinity(result, snapshot, &domain.AffinityBinding{
		Scope: domain.AffinityConversation, ProviderID: "p", ConnectionID: "b", UpstreamModel: "m",
	}) {
		t.Fatal("affinity was not reused")
	}
	if !reflect.DeepEqual(result.Order, []domain.ResourceTargetID{"b", "a", "c"}) {
		t.Fatalf("order = %v", result.Order)
	}
	wantRanks := map[domain.ResourceTargetID]int{"b": 1, "a": 2, "c": 3}
	for _, candidate := range result.Candidates {
		if candidate.Rank != wantRanks[candidate.Target] {
			t.Fatalf("candidate ranks stale after reorder: %+v", result.Candidates)
		}
	}
}

func TestV2NoCandidateFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name    string
		reasons []string
		want    domain.FailureType
	}{
		{"capability", []string{"connection model capability missing"}, domain.FailUnsupportedParam},
		{"authentication", []string{"connection authentication error"}, domain.FailAuth},
		{"quota", []string{"connection quota exhausted"}, domain.FailQuotaExhausted},
		{"availability", []string{"connection offline"}, domain.FailConnect},
		{"mixed", []string{"connection quota exhausted", "connection offline"}, domain.FailConnect},
	} {
		t.Run(test.name, func(t *testing.T) {
			exclusions := make([]domain.ExclusionRecord, len(test.reasons))
			for i, reason := range test.reasons {
				exclusions[i] = domain.ExclusionRecord{Reason: reason}
			}
			if got := classifyV2NoCandidate(exclusions); got != test.want {
				t.Fatalf("classification = %s, want %s", got, test.want)
			}
		})
	}
}
