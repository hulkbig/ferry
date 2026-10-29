package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

func writeAnthropicStructuredProbe(w http.ResponseWriter, stream bool, text string) {
	if !stream {
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": map[string]any{}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "type": "message", "role": "assistant", "content": []any{}, "usage": map[string]any{}}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{}},
		map[string]any{"type": "message_stop"},
	} {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
}

func TestAnthropicStructuredProbeSendsNativeFormatAndValidatesOutput(t *testing.T) {
	for _, mode := range []providerProbeMode{providerProbeInvoke, providerProbeStream} {
		for _, result := range []string{`{"ok":true}`, `{"ok":false}`, `not json`, `{"ok":`} {
			t.Run(fmt.Sprintf("%v/%s", mode, result), func(t *testing.T) {
				requests := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if r.URL.Path != "/exact/messages" {
						t.Errorf("wrong FullURL: %s", r.URL.Path)
					}
					var wire struct {
						OutputConfig struct {
							Format struct {
								Type   string
								Schema json.RawMessage
							}
						} `json:"output_config"`
						System string
						Stream bool
					}
					if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
						t.Error(err)
					}
					if wire.OutputConfig.Format.Type != "json_schema" || !json.Valid(wire.OutputConfig.Format.Schema) || wire.System != "" {
						t.Errorf("probe used wrong format or degraded system: %+v", wire)
					}
					writeAnthropicStructuredProbe(w, wire.Stream, result)
				}))
				defer upstream.Close()
				caller := egress.NewAnthropic(egress.Base{HTTP: upstream.Client(), Endpoint: &domain.Endpoint{BaseURL: "https://not-used.invalid", FullURL: upstream.URL + "/exact/messages"}, Model: "claude-test"})
				failure, count := probeProviderTextFeatures(context.Background(), caller, domain.ProtocolAnthropic, "claude-test", providerTextProbeFeatures{StructuredOutput: true}, mode)
				if requests != 1 || count != 1 || (failure == nil) != (result == `{"ok":true}`) {
					t.Fatalf("requests=%d count=%d failure=%v", requests, count, failure)
				}
			})
		}
	}
}

func TestAnthropicDiscoveryStoresSupportedStructuredEvidence(t *testing.T) {
	seenFormat := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "claude-test"}}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if providerProbeUsesTools(body) {
			writeProviderToolProbeSSE(w, domain.ProtocolAnthropic, providerProbeUsesParallelTools(body))
			return
		}
		if config, ok := body["output_config"].(map[string]any); ok {
			format, ok := config["format"].(map[string]any)
			if !ok || format["type"] != "json_schema" {
				t.Errorf("bad native format: %v", config)
			}
			seenFormat = true
		}
		stream, _ := body["stream"].(bool)
		writeAnthropicStructuredProbe(w, stream, `{"ok":true}`)
	}))
	defer upstream.Close()
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
		"provider_id": "native-schema", "base_url": upstream.URL + "/v1", "secret": "fixture",
		"protocols": []map[string]any{{"protocol": domain.ProtocolAnthropic, "models": []string{"claude-test"}}},
	})
	requireStatus(t, rec, http.StatusOK)
	model := providerDiscoveryModelResult(t, response)
	evidence := providerDiscoveryCapabilityEvidence(t, model, providerCapabilityStructuredOutput)
	if !seenFormat || evidence["status"] != "supported" || evidence["source"] != "probe" {
		t.Fatalf("format=%v evidence=%v model=%v", seenFormat, evidence, model)
	}
}

type strictSchemaProbeCaller struct {
	egress.Caller
	t *testing.T
}

func (c strictSchemaProbeCaller) Invoke(_ context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	if rc.ResponseFormat == nil || rc.ResponseFormat.Strict == nil || !*rc.ResponseFormat.Strict {
		c.t.Fatalf("internal schema probe must explicitly request strict enforcement: %+v", rc.ResponseFormat)
	}
	notes, err := c.Caller.(egress.Preparer).Prepare(rc)
	if err != nil || len(notes) != 0 {
		c.t.Fatalf("strict native probe unexpectedly degraded: notes=%v err=%v", notes, err)
	}
	return &domain.Response{Content: []domain.Block{{Kind: domain.BlockText, Text: `{"ok":true}`}}}, nil
}

func TestInternalStructuredProbesExplicitlyRequestStrictEnforcement(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic} {
		caller := strictSchemaProbeCaller{Caller: egress.NewCaller(egress.Base{Model: "m"}, protocol), t: t}
		failure, _ := probeProviderTextFeatures(context.Background(), caller, protocol, "m", providerTextProbeFeatures{StructuredOutput: true}, providerProbeInvoke)
		if failure != nil {
			t.Fatal(failure)
		}
	}
}
