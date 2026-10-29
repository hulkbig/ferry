package ingress

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func TestTierAuthorityHeadersDescribeCommittedResponseWithoutChangingEcho(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, observed := range []bool{false, true} {
				for _, failed := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%v/observed=%v/failed=%v", protocol, stream, observed, failed), func(t *testing.T) {
						eng := newStub()
						if observed {
							eng.tierEvidence = &domain.ServiceTierEvidence{WireServiceTier: "priority", ResponseTierAuthority: domain.TierInformational, Source: "codex", PriorityConfirmation: "unconfirmed"}
						}
						if failed {
							eng.fail = &domain.Failure{Type: domain.FailProvider5xx}
						}
						eng.resp.ServiceTier = "default"
						eng.evs = []domain.StreamEvent{{Kind: domain.EvTextDelta, TextDelta: "ok"}, {Kind: domain.EvEnd, FinishReason: "stop", ServiceTier: "default"}}
						handler, path, input := eng.chat(), "/v1/chat/completions", `"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}`
						if protocol == "responses" {
							handler, path, input = eng.responses(), "/v1/responses", `"input":"hi"`
						}
						rec := postJSON(t, handler, path, fmt.Sprintf(`{"model":"m",%s,"stream":%v,"service_tier":"priority"}`, input, stream))
						if !failed && rec.Code != http.StatusOK {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
						}
						wantAuthority, wantSource := "", ""
						if observed && !failed {
							wantAuthority, wantSource = "informational", "codex"
						}
						// Result().Header is the header snapshot at commit, not the
						// recorder's mutable header map after SSE writes.
						if rec.Result().Header.Get("X-SmartAPI-Response-Tier-Authority") != wantAuthority || rec.Result().Header.Get("X-SmartAPI-Response-Tier-Source") != wantSource {
							t.Fatalf("headers=%v", rec.Result().Header)
						}
						if rec.Result().Header.Get("X-Ferry-Response-Tier-Authority") != wantAuthority || rec.Result().Header.Get("X-Ferry-Response-Tier-Source") != wantSource {
							t.Fatalf("headers=%v", rec.Result().Header)
						}
						if !failed && !strings.Contains(rec.Body.String(), `"service_tier":"default"`) {
							t.Fatal("echo changed")
						}
					})
				}
			}
		}
	}
}
