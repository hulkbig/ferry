package management

import (
	"net/http"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestV2RequestMetadataSurvivesListAndDetailProjection(t *testing.T) {
	f := newV2APIFixture(t)
	// Request windows use an exclusive millisecond upper bound. Keep this
	// fixture inside the window even when writes and reads finish in one ms.
	id, now := "req-returned-model", time.Now().Add(-time.Second)
	evidence := &domain.ServiceTierEvidence{WireServiceTier: "priority", ResponseTierAuthority: domain.TierInformational, Source: "codex", PriorityConfirmation: "unconfirmed"}
	f.ledger.BeginRequest(domain.RequestRecord{
		ID: id, Ts: now, LogicalModelID: "smart", Protocol: domain.ProtocolOpenAIResponses,
		ProviderID: "provider-a", ConnectionID: "connection-a", UpstreamModel: "configured-alias",
		Status: domain.ReqSuccess, Outcome: domain.OutcomeRecoveredSuccess, Attempts: 2,
		ServiceTier: "fast", ResponseModel: "observed-model", ResponseServiceTier: "priority",
		ServiceTierEvidence: evidence,
	})
	f.ledger.AddAttempt(domain.AttemptRecord{
		RequestID: id, Seq: 1, Ts: now, Protocol: domain.ProtocolOpenAIResponses,
		Model: "configured-alias", Status: domain.ReqFailed, ServiceTier: "fast",
	})
	f.ledger.AddAttempt(domain.AttemptRecord{
		RequestID: id, Seq: 2, Ts: now, Protocol: domain.ProtocolOpenAIResponses,
		Model: "configured-alias", Status: domain.ReqSuccess, ServiceTier: "fast",
		ResponseModel: "observed-model", ResponseServiceTier: "priority",
		ServiceTierEvidence: evidence,
	})
	f.ledger.FinishRequest(id, nil)

	var detail map[string]any
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		rec, body := f.do(http.MethodGet, "/api/requests/"+id, nil)
		attempts, _ := body["attempts"].([]any)
		if rec.Code == http.StatusOK && len(attempts) == 2 {
			detail = body
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if detail == nil {
		t.Fatal("request and attempts were not flushed")
	}
	rec, list := f.do(http.MethodGet, "/api/requests?window=24h", nil)
	requireStatus(t, rec, http.StatusOK)
	var summary map[string]any
	for _, raw := range list["requests"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == id {
			summary = row
			break
		}
	}
	if summary == nil {
		t.Fatal("request missing from list")
	}
	attempts := detail["attempts"].([]any)
	for _, row := range []map[string]any{summary, detail["client_request"].(map[string]any), attempts[1].(map[string]any)} {
		metadata, ok := row["service_tier_evidence"].(map[string]any)
		if !ok || metadata["response_tier_authority"] != "informational" || metadata["priority_confirmation"] != "unconfirmed" || metadata["wire_service_tier"] != "priority" {
			t.Fatalf("evidence missing: %v", row)
		}
		if row["upstream_model"] != "configured-alias" || row["response_model"] != "observed-model" ||
			row["service_tier"] != "fast" || row["response_service_tier"] != "priority" {
			t.Fatalf("configured and returned metadata not preserved: %v", row)
		}
	}
	first := attempts[0].(map[string]any)
	if first["service_tier_evidence"] != nil {
		t.Fatalf("unobserved evidence invented: %v", first)
	}
	if first["response_model"] != "" || first["response_service_tier"] != "" || first["service_tier"] != "fast" {
		t.Fatalf("unobserved attempt metadata was invented: %v", first)
	}
}
