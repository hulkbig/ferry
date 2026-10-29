package management

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"ferry/internal/billing/pricingsync"
	"ferry/internal/domain"
)

func TestReferencePricingSettingsPersistAndValidate(t *testing.T) {
	f := newV2APIFixture(t)
	rec, status := f.do(http.MethodGet, "/api/usage-billing/pricing", nil)
	requireStatus(t, rec, http.StatusOK)
	settings := status["settings"].(map[string]any)
	if settings["enabled"] != true || settings["interval_hours"] != float64(24) || settings["source"] != "litellm" {
		t.Fatalf("defaults=%v", settings)
	}
	for _, bad := range []any{
		map[string]any{"enabled": true, "interval_hours": 0, "source": "litellm"},
		map[string]any{"enabled": true, "interval_hours": 169, "source": "litellm"},
		map[string]any{"enabled": true, "interval_hours": 24, "source": "https://untrusted.test"},
	} {
		rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{"pricing_sync": bad})
		requireStatus(t, rec, http.StatusBadRequest)
	}
	want := pricingsync.Settings{Enabled: false, IntervalHours: 6, Source: "models.dev"}
	rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{"pricing_sync": want})
	requireStatus(t, rec, http.StatusOK)
	f.server.pricing = pricingsync.NewCache(f.control)
	if got := f.server.pricing.Status(time.Now()).Settings; got != want {
		t.Fatalf("restart settings=%+v", got)
	}
	rec, status = f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, rec, http.StatusOK)
	if status["pricing_sync"].(map[string]any)["settings"].(map[string]any)["enabled"] != false {
		t.Fatal("settings/status diverged")
	}
}

func TestRequestReferencePricingPreservesRecordedCostsAcrossProtocols(t *testing.T) {
	f := newV2APIFixture(t)
	now := time.Now().UTC()
	readRate := 1.0
	result := pricingsync.Result{Source: "litellm", Version: "fixture:sha256:v1", FetchedAt: now, Prices: []pricingsync.Price{{Provider: "openai", Model: "upstream-model", Rates: pricingsync.Rates{Input: 10, Output: 50, Read: &readRate}}}}
	snapshot, _ := json.Marshal(map[string]any{"result": result, "last_attempt": now})
	if err := f.control.SetRuntimeSetting("reference_pricing_snapshot", string(snapshot)); err != nil {
		t.Fatal(err)
	}
	f.server.pricing = pricingsync.NewCache(f.control)
	cases := []struct {
		id                    string
		client, upstream      domain.Protocol
		input, read, eligible int64
		cost                  float64
		subset                bool
	}{
		{"anthropic-upstream", domain.ProtocolOpenAIChat, domain.ProtocolAnthropic, 100, 900, 1000, .0069, false},
		{"openai-upstream", domain.ProtocolAnthropic, domain.ProtocolOpenAIResponses, 1000, 900, 1000, .0069, true},
		{"openai-independent", domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, 1000, 900, 1900, .0159, false},
		{"legacy-independent", domain.ProtocolOpenAIChat, "", 100, 900, 1000, .0069, false},
	}
	for _, tc := range cases {
		f.ledger.BeginRequest(domain.RequestRecord{ID: tc.id, Ts: now, Protocol: tc.client, Attempts: 1, ProviderID: "provider-a", ConnectionID: "connection-a", LogicalModelID: "smart", UpstreamModel: "upstream-model", Status: domain.ReqSuccess, Usage: domain.Usage{InputTokens: tc.input, CacheReadTokens: tc.read, OutputTokens: 100, CacheSubsetOfInput: tc.subset}, EstCashCost: 9})
		f.ledger.AddAttempt(domain.AttemptRecord{RequestID: tc.id, Seq: 1, Ts: now, Protocol: tc.upstream, Status: domain.ReqSuccess})
		f.ledger.FinishRequest(tc.id, nil)
	}
	var recStatus int
	for i := 0; i < 100; i++ {
		rec, _ := f.do(http.MethodGet, "/api/requests/legacy-independent", nil)
		recStatus = rec.Code
		if recStatus == http.StatusOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if recStatus != http.StatusOK {
		t.Fatal("ledger did not flush")
	}
	rec, list := f.do(http.MethodGet, "/api/requests?window=24h", nil)
	requireStatus(t, rec, http.StatusOK)
	rows := map[string]map[string]any{}
	for _, v := range list["requests"].([]any) {
		row := v.(map[string]any)
		rows[row["id"].(string)] = row
	}
	for _, tc := range cases {
		rec, detail := f.do(http.MethodGet, "/api/requests/"+tc.id, nil)
		requireStatus(t, rec, http.StatusOK)
		for _, row := range []map[string]any{rows[tc.id], detail["client_request"].(map[string]any)} {
			if row["cache_eligible_input_tokens"] != float64(tc.eligible) || row["total_cost"] != float64(9) || row["cost_observed"] != true {
				t.Fatalf("normalization/bill changed: %v", row)
			}
			ref := row["reference_pricing"].(map[string]any)
			if ref["status"] != "available" || math.Abs(ref["cost"].(float64)-tc.cost) > 1e-12 || ref["version"] != result.Version {
				t.Fatalf("reference=%v", ref)
			}
			wantProtocol := tc.upstream
			if wantProtocol == "" {
				wantProtocol = tc.client
			}
			if row["usage_protocol"] != string(wantProtocol) {
				t.Fatalf("protocol=%v want=%v", row["usage_protocol"], wantProtocol)
			}
		}
	}
}
