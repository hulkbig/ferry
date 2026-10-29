package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

type tierTransport func(*http.Request) (*http.Response, error)

func (f tierTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTierEvidenceFollowsActualAttemptAcrossProviderFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, finalCodex := range []bool{false, true} {
			for _, returned := range []string{"default", "priority", ""} {
				t.Run(fmt.Sprintf("stream=%v/codex=%v/echo=%s", stream, finalCodex, returned), func(t *testing.T) {
					f := newV2Fixture(t)
					f.publish(func(s *domain.CompiledSnapshot) {
						s.Targets["p1-b"].Disabled = true
						for _, target := range s.Targets {
							target.NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
							target.Endpoint.Protocol = domain.ProtocolOpenAIResponses
							target.Capabilities.Priority = true
							target.Endpoint.BaseURL = "https://api.openai.com/v1"
							codex := (target.ProviderID == "provider-2") == finalCodex
							if codex {
								s.Providers[target.ProviderID].Type = domain.ProviderCliproxyCodex
								target.Endpoint.BaseURL = "https://chatgpt.com/backend-api/codex"
							}
						}
					})
					w := f.sch.world.Load()
					for id, target := range w.snap.Targets {
						first := target.ProviderID == "provider-1"
						client := &http.Client{Transport: tierTransport(func(r *http.Request) (*http.Response, error) {
							var payload map[string]any
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								return nil, err
							}
							if payload["service_tier"] != "priority" {
								return nil, fmt.Errorf("unexpected wire tier: %v", payload["service_tier"])
							}
							code, contentType := http.StatusOK, "application/json"
							body := fmt.Sprintf(`{"id":"resp-test","object":"response","status":"completed","model":"actual","service_tier":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`, returned)
							if first {
								code, body = http.StatusServiceUnavailable, `{"error":{"message":"temporarily unavailable"}}`
							} else if payload["stream"] == true {
								contentType = "text/event-stream"
								body = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-test\",\"service_tier\":\"priority\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
							}
							return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
						})}
						base := egress.Base{HTTP: client, Endpoint: target.Endpoint, Model: "upstream", Secret: "fixture"}
						if w.snap.Providers[target.ProviderID].Type == domain.ProviderCliproxyCodex {
							w.callers[id] = egress.NewCodexCaller(base, "fixture-account")
						} else {
							w.callers[id] = egress.NewOpenAIResponses(base)
						}
					}
					rc := chatReq("smart")
					rc.Protocol, rc.Origin = domain.ProtocolOpenAIResponses, domain.ProtocolOpenAIResponses
					rc.ServiceTier, rc.Stream = "priority", stream
					result, failure := f.sch.Execute(context.Background(), rc, RequestMeta{BearerToken: "sk"})
					if failure != nil {
						t.Fatal(failure)
					}
					wantAuthority, wantConfirmation := domain.TierAuthoritative, "confirmed"
					if returned == "default" {
						wantConfirmation = "downgraded"
					}
					if finalCodex {
						wantAuthority, wantConfirmation = domain.TierInformational, "unconfirmed"
					}
					if returned == "" {
						wantConfirmation = "unconfirmed"
					}
					if result.Target != "p2-a" || result.ServiceTierEvidence == nil || result.ServiceTierEvidence.ResponseTierAuthority != wantAuthority {
						t.Fatalf("committed result=%+v", result)
					}
					if stream {
						if result.ServiceTierEvidence.PriorityConfirmation != "unconfirmed" {
							t.Fatal("confirmed priority before terminal event")
						}
						for ev := range result.Events {
							if ev.Err != nil {
								t.Fatal(ev.Err)
							}
						}
					}
					db := f.ledger.reopen(t)
					record, err := db.QueryRequest(result.LedgerRequestID)
					if err != nil || record == nil {
						t.Fatalf("request=%+v err=%v", record, err)
					}
					attempts, err := db.QueryAttempts(record.ID)
					if err != nil || len(attempts) != 2 {
						t.Fatalf("attempts=%+v err=%v", attempts, err)
					}
					for _, evidence := range []*domain.ServiceTierEvidence{record.ServiceTierEvidence, attempts[1].ServiceTierEvidence} {
						if evidence == nil || evidence.WireServiceTier != "priority" || evidence.ResponseTierAuthority != wantAuthority || evidence.PriorityConfirmation != wantConfirmation {
							t.Fatalf("final evidence=%+v want=%s/%s", evidence, wantAuthority, wantConfirmation)
						}
					}
					first := attempts[0].ServiceTierEvidence
					wantEcho := returned
					if returned == "" && (stream || finalCodex) {
						wantEcho = "priority" // retain the observed initial echo without confirming it
					}
					if first == nil || first.ResponseTierAuthority == wantAuthority || first.PriorityConfirmation != "unconfirmed" || attempts[1].ResponseServiceTier != wantEcho || record.ResponseServiceTier != wantEcho {
						t.Fatalf("attempt isolation failed: first=%+v final=%+v", first, record)
					}
				})
			}
		}
	}
}
