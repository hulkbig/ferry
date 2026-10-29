package management

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ferry/internal/billing/pricingsync"
	"ferry/internal/domain"
)

func TestAnalyticsWindowAndAuthorization(t *testing.T) {
	f := newV2APIFixture(t)
	rec, body := f.do(http.MethodGet, "/api/analytics?window=24h&model=absent&provider=absent", nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || body["basis"] != "attempts" {
		t.Fatalf("analytics: %d %v", rec.Code, body)
	}
	for _, query := range []string{"window=0h", "window=2200h", "to=bad", "from=2026-09-18T00:00:00Z", "from=2026-09-18T00:00:00Z&to=2026-09-17T00:00:00Z"} {
		rec, _ := f.do(http.MethodGet, "/api/analytics?"+query, nil)
		if rec.Code != 400 {
			t.Fatalf("query %s: %d", query, rec.Code)
		}
	}
	// The console uses durations, not unsupported "7d" / "30d" strings.
	for _, window := range []string{"168h", "720h"} {
		to := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
		rec, body := f.do(http.MethodGet, "/api/analytics?window="+window+"&to="+to.Format(time.RFC3339Nano), nil)
		requireStatus(t, rec, http.StatusOK)
		duration, _ := time.ParseDuration(window)
		if body["from"] != to.Add(-duration).Format(time.RFC3339Nano) || body["to"] != to.Format(time.RFC3339Nano) {
			t.Fatalf("window %s: %v", window, body)
		}
	}
	// Latest-window UI requests must not use a potentially fast client clock.
	// Exact report timestamps remain strict; do not silently clamp them.
	for _, window := range []string{"168h", "720h"} {
		future := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
		rec, body := f.do(http.MethodGet, "/api/analytics?window="+window+"&to="+future, nil)
		requireStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "to must not be in the future") {
			t.Fatalf("window %s: %v", window, body)
		}
		rec, _ = f.do(http.MethodGet, "/api/analytics?window="+window, nil)
		requireStatus(t, rec, http.StatusOK)
	}
	f.server.deps.Security = f.control
	handler := f.server.Routes()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(method, "https://ferry.test"+path, strings.NewReader(body)))
		return r
	}
	if r := call("GET", "/api/analytics", ""); r.Code != 428 {
		t.Fatalf("uninitialized=%d", r.Code)
	}
	init := call("POST", "/api/auth/initialize", `{"username":"admin","password":"correct horse battery staple"}`)
	if init.Code != 201 {
		t.Fatalf("init=%d %s", init.Code, init.Body)
	}
	if r := call("GET", "/api/analytics", ""); r.Code != 401 {
		t.Fatalf("anonymous=%d", r.Code)
	}
	req := httptest.NewRequest("GET", "https://ferry.test/api/analytics", nil)
	for _, c := range init.Result().Cookies() {
		req.AddCookie(c)
	}
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, req)
	if result.Code != 200 {
		t.Fatalf("authenticated=%d %s", result.Code, result.Body)
	}
}

func TestAnalyticsValuesEveryPlanUsingAttemptPrices(t *testing.T) {
	f := newV2APIFixture(t)
	now := time.Now().UTC()
	readRate, tierRead := 1.0, 2.0
	rates := pricingsync.Rates{Input: 10, Output: 50, Read: &readRate}
	result := pricingsync.Result{Source: "litellm", Version: "fixture:v1", FetchedAt: now.Add(-48 * time.Hour), Prices: []pricingsync.Price{
		{Provider: "openai", Model: "shared", Rates: rates},
		{Provider: "other", Model: "shared", Rates: pricingsync.Rates{Input: 99, Output: 99}},
		{Provider: "catalog-vendor", Model: "unique", Rates: rates, Tiers: []pricingsync.Tier{{Above: 200000, Rates: pricingsync.Rates{Input: 20, Output: 100, Read: &tierRead}}}},
		{Provider: "catalog-vendor", Model: "free", Rates: pricingsync.Rates{}},
		{Provider: "catalog-vendor", Model: "no-cache-price", Rates: pricingsync.Rates{Input: 10, Output: 50}},
	}}
	snapshot, err := json.Marshal(map[string]any{"result": result, "last_attempt": now})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.control.SetRuntimeSetting("reference_pricing_snapshot", string(snapshot)); err != nil {
		t.Fatal(err)
	}
	f.server.pricing = pricingsync.NewCache(f.control)
	cfg := *f.server.currentConfig()
	cfg.Providers = append(append([]domain.Provider(nil), cfg.Providers...), domain.Provider{ID: "codex", Type: domain.ProviderCliproxyCodex})
	f.server.deps.Config = &cfg
	cases := []struct {
		id, model           string
		plan                domain.PlanKind
		protocol            domain.Protocol
		input, read, output int64
		cost                float64
		known               bool
	}{
		{"codex", "shared", domain.PlanSubscription, domain.ProtocolOpenAIResponses, 1000, 900, 100, .0069, true},
		{"api", "unique", domain.PlanPAYG, domain.ProtocolOpenAIChat, 1000, 900, 100, .0069, true},
		{"anthropic", "unique", domain.PlanSubscription, domain.ProtocolAnthropic, 100, 900, 100, .0069, true},
		{"legacy", "unique", "", "", 100, 900, 100, .0069, true},
		{"free-plan", "unique", domain.PlanFree, domain.ProtocolOpenAIChat, 1000, 900, 100, .0069, true},
		{"long", "unique", domain.PlanSubscription, domain.ProtocolOpenAIResponses, 300000, 0, 0, 6, true},
		{"short", "unique", domain.PlanSubscription, domain.ProtocolOpenAIResponses, 100000, 0, 0, 1, true},
		{"free-model", "free", domain.PlanPAYG, domain.ProtocolOpenAIChat, 100, 0, 0, 0, true},
		{"unknown", "missing", domain.PlanSubscription, domain.ProtocolOpenAIChat, 100, 0, 0, 0, false},
		{"cache-missing", "no-cache-price", domain.PlanPAYG, domain.ProtocolOpenAIChat, 1000, 900, 100, 0, false},
		{"no-usage", "unique", domain.PlanPAYG, domain.ProtocolOpenAIChat, 0, 0, 0, 0, false},
		{"ambiguous", "shared", domain.PlanSubscription, domain.ProtocolOpenAIChat, 100, 0, 0, 0, false},
	}
	for _, tc := range cases {
		recorded, allocated := 0.0, 0.0
		if tc.id == "api" {
			recorded = 9
		}
		if tc.id == "codex" {
			allocated = .25
		}
		usage := domain.Usage{InputTokens: tc.input, CacheReadTokens: tc.read, OutputTokens: tc.output}
		f.ledger.BeginRequest(domain.RequestRecord{ID: tc.id, Ts: now.Add(-time.Minute), LogicalModelID: "logical-alias", UpstreamModel: tc.model, ProviderID: tc.id, Protocol: tc.protocol, Status: domain.ReqSuccess, Usage: usage, EstCashCost: recorded, AllocatedSubCost: allocated})
		f.ledger.AddAttempt(domain.AttemptRecord{RequestID: tc.id, Seq: 1, Ts: now, ProviderID: tc.id, Model: tc.model, PlanKind: tc.plan, Protocol: tc.protocol, Status: domain.ReqSuccess, Usage: usage, EstCashCost: recorded, AllocatedSubCost: allocated})
		f.ledger.FinishRequest(tc.id, nil)
	}
	var report map[string]any
	for i := 0; i < 100; i++ {
		rec, body := f.do(http.MethodGet, "/api/analytics?window=24h", nil)
		requireStatus(t, rec, http.StatusOK)
		report = body
		if body["summary"].(map[string]any)["attempts"] == float64(len(cases)) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s := report["summary"].(map[string]any)
	if math.Abs(s["theoretical_cost"].(float64)-7.0345) > 1e-9 || s["theoretical_priced_attempts"] != float64(8) || s["theoretical_unpriced_attempts"] != float64(4) || s["cost"] != 9.25 {
		t.Fatalf("summary=%v", s)
	}
	metadata := report["reference_pricing"].(map[string]any)
	if metadata["version"] != result.Version || metadata["source"] != result.Source || metadata["stale"] != true || metadata["updated_at"] != result.FetchedAt.Format(time.RFC3339Nano) {
		t.Fatalf("pricing provenance=%v", metadata)
	}
	for _, tc := range cases {
		rec, body := f.do(http.MethodGet, "/api/analytics?window=24h&model=logical-alias&provider="+tc.id, nil)
		requireStatus(t, rec, http.StatusOK)
		s := body["summary"].(map[string]any)
		if math.Abs(s["theoretical_cost"].(float64)-tc.cost) > 1e-9 || (s["theoretical_priced_attempts"] == float64(1)) != tc.known || s["attempts"] != float64(1) {
			t.Fatalf("%s=%v", tc.id, s)
		}
	}
	view := map[string]any{}
	f.server.addReferencePricing(view, &domain.RequestRecord{ProviderID: "codex", UpstreamModel: "shared", Protocol: domain.ProtocolOpenAIResponses, Usage: domain.Usage{InputTokens: 1000, CacheReadTokens: 900, OutputTokens: 100, CacheSubsetOfInput: true}})
	if e := view["reference_pricing"].(pricingsync.Estimate); e.Cost == nil || math.Abs(*e.Cost-.0069) > 1e-9 {
		t.Fatalf("Codex request and report disagree: %+v", e)
	}
}
