package ingress

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func TestOpenAIUsageSerializationPreservesCacheAccounting(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, subset := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/subset=%v", protocol, stream, subset), func(t *testing.T) {
					eng := newStub()
					u := domain.Usage{InputTokens: 50, OutputTokens: 200, CacheReadTokens: 800, CacheWriteTokens: 150, ReasoningTokens: 20, CacheSubsetOfInput: subset}
					if subset {
						u.InputTokens = 1000
					}
					eng.resp.Usage, eng.resp.ServiceTier = u, "priority"
					eng.evs = []domain.StreamEvent{{Kind: domain.EvTextDelta, TextDelta: "hi"}, {Kind: domain.EvEnd, FinishReason: "stop", Usage: &u, ServiceTier: "priority"}}
					handler, path := eng.chat(), "/v1/chat/completions"
					input := `"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}`
					if protocol == "responses" {
						handler, path, input = eng.responses(), "/v1/responses", `"input":"hi"`
					}
					rec := postJSON(t, handler, path, fmt.Sprintf(`{"model":"logical",%s,"stream":%v,"service_tier":"fast"}`, input, stream))
					if rec.Code != http.StatusOK {
						t.Fatalf("%d %s", rec.Code, rec.Body)
					}
					var usage map[string]any
					var tier string
					if stream {
						for _, line := range strings.Split(rec.Body.String(), "\n") {
							if !strings.HasPrefix(line, "data: ") {
								continue
							}
							var frame map[string]any
							if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
								continue
							}
							if response, ok := frame["response"].(map[string]any); ok {
								frame = response
							}
							if value, ok := frame["service_tier"].(string); ok {
								tier = value
							}
							if value, ok := frame["usage"].(map[string]any); ok {
								usage = value
							}
						}
					} else {
						frame := decode(t, rec)
						usage = sub(t, frame, "usage")
						tier, _ = frame["service_tier"].(string)
					}
					inputKey, detailsKey := "prompt_tokens", "prompt_tokens_details"
					if protocol == "responses" {
						inputKey, detailsKey = "input_tokens", "input_tokens_details"
					}
					if usage[inputKey] != float64(1000) || usage["total_tokens"] != float64(1200) || tier != "priority" {
						t.Fatalf("usage=%v tier=%q", usage, tier)
					}
					details := sub(t, usage, detailsKey)
					if details["cached_tokens"] != float64(800) || details["cache_write_tokens"] != float64(150) {
						t.Fatalf("cache=%v", details)
					}
				})
			}
		}
	}
}

func TestTierValidationPrecedesExecutionAndNormalizesCase(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, value := range []string{`" FAST "`, `" Priority "`, `"ultrafast"`, `"unknown"`, `42`, `{}`} {
			eng := newStub()
			handler, path, input := eng.chat(), "/v1/chat/completions", `"messages":[{"role":"user","content":"hi"}]`
			if protocol == "responses" {
				handler, path, input = eng.responses(), "/v1/responses", `"input":"hi"`
			}
			rec := postJSON(t, handler, path, `{"model":"m",`+input+`,"service_tier":`+value+`}`)
			if value == `" FAST "` || value == `" Priority "` {
				if rec.Code != http.StatusOK || eng.gotContract == nil || !eng.gotContract.ExtractRequirements().Priority || strings.TrimSpace(strings.ToLower(eng.gotContract.ServiceTier)) != eng.gotContract.ServiceTier {
					t.Fatalf("normalization: %s %s", value, rec.Body)
				}
				if strings.Contains(rec.Body.String(), `"service_tier"`) {
					t.Fatal("unobserved response tier was invented from the request")
				}
			} else if rec.Code != http.StatusBadRequest || eng.gotContract != nil {
				t.Fatalf("invalid tier reached execution: %s code=%d", value, rec.Code)
			}
		}
	}
}

func TestTurnStateIngressHeaderScope(t *testing.T) {
	for _, origin := range []domain.Protocol{domain.ProtocolOpenAIResponses, domain.ProtocolOpenAIChat, domain.ProtocolAnthropic} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Codex-Turn-State", "opaque-state")
		r.Header.Set("Chatgpt-Account-Id", "forged")
		rc := &domain.RequestContract{Origin: origin}
		captureProtocolHeaders(r, rc)
		want := ""
		if origin == domain.ProtocolOpenAIResponses {
			want = "opaque-state"
		}
		if rc.ProtocolHeaders["X-Codex-Turn-State"] != want || rc.ProtocolHeaders["Chatgpt-Account-Id"] != "" || rc.CodexTurnStateAllowed {
			t.Fatalf("captured contract=%+v", rc)
		}
	}
}
