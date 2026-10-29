package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"ferry/internal/domain"
)

func TestResponseMetadataAndCacheUsageAcrossAdapters(t *testing.T) {
	usage := `{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_creation_tokens":150}}`
	for _, adapter := range []string{"chat", "responses", "codex", "anthropic"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", adapter, stream), func(t *testing.T) {
				wantTier := "priority"
				if adapter == "anthropic" {
					wantTier = ""
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch adapter {
					case "chat":
						chatUsage := `{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":800,"cache_creation_tokens":150}}`
						if stream {
							sse(w, data(`{"model":"actual","service_tier":"priority","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
								data(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
								data(`{"choices":[],"usage":`+chatUsage+`}`))
						} else {
							fmt.Fprint(w, `{"model":"actual","service_tier":"priority","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":`+chatUsage+`}`)
						}
					case "responses", "codex":
						if stream || adapter == "codex" {
							sse(w, data(`{"type":"response.created","response":{"id":"r","model":"actual","service_tier":"priority"}}`),
								data(`{"type":"response.output_text.delta","item_id":"m","delta":"hi"}`),
								data(`{"type":"response.completed","response":{"id":"r","status":"completed","usage":`+usage+`}}`))
						} else {
							fmt.Fprint(w, `{"id":"r","status":"completed","model":"actual","service_tier":"priority","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":`+usage+`}`)
						}
					case "anthropic":
						if stream {
							sse(w, data(`{"type":"message_start","message":{"model":"actual","usage":{"input_tokens":50,"cache_read_input_tokens":800,"cache_creation_input_tokens":150}}}`),
								data(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
								data(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
								data(`{"type":"content_block_stop","index":0}`),
								data(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":200}}`),
								data(`{"type":"message_stop"}`))
						} else {
							fmt.Fprint(w, `{"type":"message","model":"actual","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":200,"cache_read_input_tokens":800,"cache_creation_input_tokens":150}}`)
						}
					}
				}))
				defer srv.Close()
				base := testBase(srv, "")
				var c Caller
				switch adapter {
				case "chat":
					c = NewOpenAIChat(base)
				case "responses":
					c = NewOpenAIResponses(base)
				case "codex":
					c = NewCodexCaller(base, "account")
				case "anthropic":
					c = NewAnthropic(base)
				}
				rc := chatRC()
				rc.Stream = stream
				var got domain.Response
				if stream {
					ch, cancel, failure := c.Stream(context.Background(), rc)
					mustNoFail(t, failure)
					defer cancel()
					for _, ev := range drain(t, ch) {
						if ev.Err != nil {
							t.Fatal(ev.Err)
						}
						if ev.Model != "" {
							got.Model = ev.Model
						}
						if ev.ServiceTier != "" {
							got.ServiceTier = ev.ServiceTier
						}
						if ev.Usage != nil {
							got.Usage = *ev.Usage
						}
					}
				} else {
					response, failure := c.Invoke(context.Background(), rc)
					mustNoFail(t, failure)
					got = *response
					if len(got.ConversionIssues) != 0 || len(got.ValidationIssues) != 0 {
						t.Fatalf("compatible metadata rejected: %+v", got)
					}
				}
				if got.Model != "actual" || got.ServiceTier != wantTier || got.Usage.UncachedInput() != 50 ||
					got.Usage.CacheReadTokens != 800 || got.Usage.CacheWriteTokens != 150 || got.Usage.OutputTokens != 200 {
					t.Fatalf("response=%+v", got)
				}
			})
		}
	}
}
