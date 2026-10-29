package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func TestCodexNormalizesCacheHintsWithoutChangingToolData(t *testing.T) {
	rc := &domain.RequestContract{Origin: domain.ProtocolOpenAIResponses, ServiceTier: " FAST ", Native: json.RawMessage(`{
		"model":"alias","service_tier":" FAST ","prompt_cache_options":{"retention":"24h"},
		"max_completion_tokens":100,"top_p":0.9,"truncation":"auto",
		"tools":[{"type":"function","name":"tool","parameters":{"properties":{"prompt_cache_breakpoint":{"type":"boolean"}}}}],
		"input":[
		  {"role":"user","prompt_cache_breakpoint":true,"content":[{"type":"input_text","text":"hi","prompt_cache_breakpoint":true}]},
		  {"type":"function_call","name":"tool","call_id":"call-1","arguments":"{\"prompt_cache_breakpoint\":true}","prompt_cache_breakpoint":true},
		  {"type":"function_call_output","call_id":"call-1","prompt_cache_breakpoint":true,"output":[{"type":"input_text","text":"{\"prompt_cache_breakpoint\":true}","prompt_cache_breakpoint":true}]},
		  {"type":"function_call_output","call_id":"call-2","output":{"prompt_cache_breakpoint":"user-data"}}
		]}`)}
	c := NewCodexCaller(testBase(nil, ""), "account")
	body, notes, err := c.buildRequest(rc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"prompt_cache_options", "max_completion_tokens", "top_p", "truncation"} {
		if _, ok := got[key]; ok {
			t.Errorf("unsupported field survived: %s", key)
		}
	}
	if got["service_tier"] != "priority" || got["stream"] != true || got["store"] != false {
		t.Fatalf("envelope=%s", body)
	}
	items := got["input"].([]any)
	for _, raw := range items {
		item := raw.(map[string]any)
		if _, ok := item["prompt_cache_breakpoint"]; ok {
			t.Fatal("item cache hint survived")
		}
		for _, key := range []string{"content", "output"} {
			if parts, ok := item[key].([]any); ok {
				for _, part := range parts {
					if _, ok := part.(map[string]any)["prompt_cache_breakpoint"]; ok {
						t.Fatal("part cache hint survived")
					}
				}
			}
		}
	}
	if items[1].(map[string]any)["arguments"] != `{"prompt_cache_breakpoint":true}` ||
		items[3].(map[string]any)["output"].(map[string]any)["prompt_cache_breakpoint"] != "user-data" ||
		!strings.Contains(string(body), `"properties":{"prompt_cache_breakpoint"`) {
		t.Fatalf("tool data changed: %s", body)
	}
	if !strings.Contains(strings.Join(notes, ";"), "truncation") {
		t.Fatalf("missing conversion note: %v", notes)
	}
	for _, key := range []string{"prompt_cache_options", "max_completion_tokens", "top_p"} {
		if strings.Contains(strings.Join(notes, ";"), key) {
			t.Fatalf("gateway-owned option marked lossy: %v", notes)
		}
	}
	converted := chatRC()
	converted.ServiceTier = " FAST "
	body, _, err = c.buildRequest(converted)
	if err != nil || !strings.Contains(string(body), `"service_tier":"priority"`) {
		t.Fatalf("converted request=%s err=%v", body, err)
	}
}

func TestCodexRejectsUnsupportedNativeTiersBeforeFallback(t *testing.T) {
	c := NewCodexCaller(testBase(nil, ""), "account")
	for _, value := range []string{`"ultrafast"`, `"unknown"`, `42`, `{}`} {
		rc := chatRC()
		rc.Origin = domain.ProtocolOpenAIResponses
		rc.Native = json.RawMessage(`{"model":"alias","input":"hello","service_tier":` + value + `}`)
		if _, _, err := c.buildRequest(rc); err == nil {
			t.Errorf("native tier silently dropped: %s", value)
		}
	}
}

func TestCodexTurnStateIsScopedAndIdentityIsGatewayOwned(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, allowed := range []bool{false, true} {
			cap := newCodexCapture(codexDoneSSE)
			rc := chatRC()
			rc.Protocol, rc.Origin = domain.ProtocolOpenAIResponses, domain.ProtocolOpenAIResponses
			rc.Stream, rc.CodexTurnStateAllowed = stream, allowed
			rc.Lineage = &domain.ResponsesLineage{PromptCacheKey: "thread"}
			rc.ProtocolHeaders = map[string]string{"X-Codex-Turn-State": "opaque-state", "Session-Id": "forged", "Thread-Id": "forged", "Chatgpt-Account-Id": "forged"}
			c := NewCodexCaller(codexBase(t, cap), "account")
			if stream {
				ch, cancel, failure := c.Stream(context.Background(), rc)
				mustNoFail(t, failure)
				drain(t, ch)
				cancel()
			} else {
				_, failure := c.Invoke(context.Background(), rc)
				mustNoFail(t, failure)
			}
			cap.close()
			headers := cap.requests[0]
			wantState := ""
			if allowed {
				wantState = "opaque-state"
			}
			if headers.Get("X-Codex-Turn-State") != wantState || headers.Get("Chatgpt-Account-Id") != "account" ||
				headers.Get("Session-Id") != codexSessionKey(rc, "account") || headers.Get("Session_id") != "" ||
				headers.Get("Thread-Id") != headers.Get("Session-Id") {
				t.Fatalf("headers=%v", headers)
			}
			ordinary := http.Header{}
			for key, value := range protocolHeaders(rc, domain.ProtocolOpenAIResponses, nil) {
				ordinary.Set(key, value)
			}
			if ordinary.Get("X-Codex-Turn-State") != "" {
				t.Fatal("opaque state leaked to a generic provider")
			}
		}
	}
}
