package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
)

// codexServer captures the wire request and replays a canned Responses SSE
// flow, so tests can assert the exact envelope CodexCaller sends.
type codexCapture struct {
	srv      *httptest.Server
	auth     string
	headers  map[string]string
	requests []http.Header
	bodyRaw  string
	bodies   []string
	sse      string
	status   int
	nonSSE   bool // serve one JSON object instead of an event stream
	jsonBody string
}

func newCodexCapture(sse string) *codexCapture {
	c := &codexCapture{sse: sse, status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		c.auth = r.Header.Get("Authorization")
		c.headers = map[string]string{
			"Originator":         r.Header.Get("Originator"),
			"User-Agent":         r.Header.Get("User-Agent"),
			"Chatgpt-Account-Id": r.Header.Get("Chatgpt-Account-Id"),
		}
		c.requests = append(c.requests, r.Header.Clone())
		c.bodyRaw = string(b)
		c.bodies = append(c.bodies, c.bodyRaw)
		if c.nonSSE {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.jsonBody))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(c.sse))
	})
	c.srv = httptest.NewServer(mux)
	return c
}

func (c *codexCapture) close() { c.srv.Close() }

func codexBase(t *testing.T, c *codexCapture) Base {
	t.Helper()
	return Base{
		HTTP: c.srv.Client(),
		Endpoint: &domain.Endpoint{
			ID: "codex", BaseURL: c.srv.URL,
			Protocol: domain.ProtocolOpenAIResponses,
		},
		Secret: "at-test", CredRef: "oauth:codex:work", Model: "gpt-5.5",
	}
}

const codexDoneSSE = "event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"," +
	"\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"

func TestCodexCallerEnvelopeAndNormalize(t *testing.T) {
	cap := newCodexCapture(codexDoneSSE)
	defer cap.close()

	cc := NewCodexCaller(codexBase(t, cap), "acct-9")
	rc := &domain.RequestContract{
		Model:  "alias-55",
		Origin: domain.ProtocolOpenAIResponses, // passthrough path
		Native: json.RawMessage(`{"model":"alias-55","stream":false,"store":true,
			"instructions":"be brief","previous_response_id":"resp_1",
			"safety_identifier":"user-1","prompt_cache_retention":"24h",
			"max_output_tokens":8,"temperature":0,
			"future_setting":{"mode":"preserve"},
			"input":"hello"}`),
		ConversionIssues: []string{"field future_setting is native-only"},
		Messages: []domain.Message{{Role: domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hello"}}}},
	}
	notes, err := cc.Prepare(rc)
	if err != nil || !strings.Contains(strings.Join(notes, ";"), "previous_response_id") {
		t.Fatalf("Codex normalization was not exposed to scheduler preflight: notes=%v err=%v", notes, err)
	}
	if strings.Contains(strings.Join(notes, ";"), "future_setting") {
		t.Fatalf("preserved passthrough field was incorrectly treated as stripped: %v", notes)
	}
	if strings.Contains(strings.Join(notes, ";"), "max_output_tokens") {
		t.Fatalf("gateway-owned output limit was incorrectly treated as degraded: %v", notes)
	}
	if strings.Contains(strings.Join(notes, ";"), "temperature") {
		t.Fatalf("Codex-specific temperature normalization was incorrectly treated as degraded: %v", notes)
	}
	res, f := cc.Invoke(context.Background(), rc)
	if f != nil {
		t.Fatalf("Invoke failure: %+v", f)
	}
	if res == nil || len(res.Content) != 1 || res.Content[0].Text != "hi" {
		t.Fatalf("reassembled response wrong: %+v", res)
	}
	if res.Usage.InputTokens != 3 || res.Usage.OutputTokens != 2 {
		t.Fatalf("usage not reassembled: %+v", res.Usage)
	}

	// identity envelope present
	if got := cap.headers["Originator"]; got != "codex-tui" {
		t.Errorf("Originator = %q", got)
	}
	if !strings.HasPrefix(cap.headers["User-Agent"], "codex-tui/") {
		t.Errorf("User-Agent = %q", cap.headers["User-Agent"])
	}
	if got := cap.headers["Chatgpt-Account-Id"]; got != "acct-9" {
		t.Errorf("Chatgpt-Account-Id = %q", got)
	}
	if cap.auth != "Bearer at-test" {
		t.Errorf("auth = %q", cap.auth)
	}

	// body normalization on the passthrough path
	var m map[string]any
	if err := json.Unmarshal([]byte(cap.bodyRaw), &m); err != nil {
		t.Fatalf("wire body not JSON: %s", cap.bodyRaw)
	}
	for _, gone := range []string{"previous_response_id", "prompt", "generate",
		"prompt_cache_retention", "safety_identifier", "stream_options", "max_output_tokens", "temperature"} {
		if _, ok := m[gone]; ok {
			t.Errorf("field %q survived normalization", gone)
		}
	}
	if m["stream"] != true {
		t.Errorf("stream = %v, want forced true", m["stream"])
	}
	if m["store"] != false {
		t.Errorf("store = %v, want gateway-forced false", m["store"])
	}
	input, ok := m["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("Codex string input was not normalized to one input item: %v", m["input"])
	}
	item, ok := input[0].(map[string]any)
	if !ok || item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("normalized Codex input item has the wrong envelope: %v", input[0])
	}
	content, ok := item["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("normalized Codex input content is not one part: %v", item["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "input_text" || part["text"] != "hello" {
		t.Fatalf("normalized Codex input content is wrong: %v", content[0])
	}
	if m["instructions"] != "be brief" {
		t.Errorf("instructions = %v", m["instructions"])
	}
	if future, ok := m["future_setting"].(map[string]any); !ok || future["mode"] != "preserve" {
		t.Errorf("future_setting was not preserved: %v", m["future_setting"])
	}
	if m["model"] != "gpt-5.5" {
		t.Errorf("model not patched to upstream name: %v", m["model"])
	}
}

func TestCodexCallerKeepsCacheEnvelopeStableAcrossInternalConversations(t *testing.T) {
	cap := newCodexCapture(codexDoneSSE)
	defer cap.close()

	cc := NewCodexCaller(codexBase(t, cap), "acct-9")
	request := func(providerSession, turnID string) *domain.RequestContract {
		turnMetadata, err := json.Marshal(`{"prompt_cache_key":"client-cache","turn_id":"` + turnID +
			`","window_id":"client-cache:0"}`)
		if err != nil {
			t.Fatal(err)
		}
		return &domain.RequestContract{
			Model: "alias-55", Origin: domain.ProtocolOpenAIResponses,
			ProviderSessionKey: providerSession,
			Lineage:            &domain.ResponsesLineage{PromptCacheKey: "client-cache"},
			Native: json.RawMessage(`{"model":"alias-55","stream":true,"input":"hello",` +
				`"prompt_cache_key":"client-cache","client_metadata":{` +
				`"x-codex-window-id":"client-cache:0","x-codex-turn-metadata":` +
				string(turnMetadata) + `}}`),
			Messages: []domain.Message{{Role: domain.RoleUser,
				Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hello"}}}},
		}
	}

	for _, rc := range []*domain.RequestContract{
		request("internal-conversation-1", "turn-1"),
		request("internal-conversation-2", "turn-2"),
	} {
		if response, failure := cc.Invoke(context.Background(), rc); failure != nil || response == nil {
			t.Fatalf("response=%+v failure=%+v", response, failure)
		}
	}
	if len(cap.requests) != 2 || len(cap.bodies) != 2 {
		t.Fatalf("captured headers=%d bodies=%d", len(cap.requests), len(cap.bodies))
	}
	for _, key := range []string{"Session-Id", "Thread-Id", "X-Codex-Window-Id"} {
		first, second := cap.requests[0].Get(key), cap.requests[1].Get(key)
		if first == "" || first != second {
			t.Fatalf("%s changed across one cache thread: %q != %q", key, first, second)
		}
	}
	if first, second := cap.requests[0].Get("X-Client-Request-Id"), cap.requests[1].Get("X-Client-Request-Id"); first == "" || second == "" || first == second {
		t.Fatalf("per-call request ids were not unique: %q %q", first, second)
	}
	var firstBody, secondBody struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		ClientMetadata struct {
			WindowID string `json:"x-codex-window-id"`
		} `json:"client_metadata"`
	}
	if err := json.Unmarshal([]byte(cap.bodies[0]), &firstBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(cap.bodies[1]), &secondBody); err != nil {
		t.Fatal(err)
	}
	if firstBody.PromptCacheKey == "" || firstBody.PromptCacheKey != secondBody.PromptCacheKey {
		t.Fatalf("prompt cache key changed: %q != %q", firstBody.PromptCacheKey, secondBody.PromptCacheKey)
	}
	if firstBody.ClientMetadata.WindowID == "" ||
		firstBody.ClientMetadata.WindowID != secondBody.ClientMetadata.WindowID {
		t.Fatalf("payload window id changed: %q != %q",
			firstBody.ClientMetadata.WindowID, secondBody.ClientMetadata.WindowID)
	}
}

func TestCodexInvokeUsesFinalToolArgumentsAndRequiresItemCompletion(t *testing.T) {
	stream := "data: {\"type\":\"response.output_item.added\",\"item_id\":\"fc_alpha\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call-a\",\"name\":\"alpha\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_alpha\",\"arguments\":\"{\\\"x\\\":1}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item_id\":\"fc_alpha\",\"item\":{\"type\":\"function_call\",\"arguments\":\"{\\\"x\\\":1}\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{}}}\n\n"
	cap := newCodexCapture(stream)
	defer cap.close()

	caller := NewCodexCaller(codexBase(t, cap), "")
	response, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Model: "gpt-5.5", Messages: []domain.Message{{Role: domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "use a tool"}}}},
	})
	if failure != nil {
		t.Fatalf("Invoke failure: %+v", failure)
	}
	if len(response.Content) != 1 || response.Content[0].Kind != domain.BlockToolUse ||
		string(response.Content[0].Input) != `{"x":1}` || len(response.ConversionIssues) != 0 {
		t.Fatalf("tool response was not assembled losslessly: %+v", response)
	}
}

func TestCodexInvokeMapsReasoningSummaryToChat(t *testing.T) {
	stream := "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[]}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_part.added\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"part\":{\"type\":\"summary_text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"delta\":\"pondering\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.done\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"text\":\"pondering\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_part.done\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"part\":{\"type\":\"summary_text\",\"text\":\"pondering\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"pondering\"}]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-5.5\",\"usage\":{}}}\n\n"
	cap := newCodexCapture(stream)
	defer cap.close()

	caller := NewCodexCaller(codexBase(t, cap), "")
	response, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Model: "gpt-5.5", Protocol: domain.ProtocolOpenAIChat, Origin: domain.ProtocolOpenAIChat,
		Messages: []domain.Message{{Role: domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "think"}}}},
	})
	if failure != nil {
		t.Fatalf("Invoke failure: %+v", failure)
	}
	if len(response.ConversionIssues) != 0 || len(response.Content) != 1 ||
		response.Content[0].Kind != domain.BlockThinking || response.Content[0].Text != "pondering" {
		t.Fatalf("reasoning summary was not mapped to chat reasoning_content: %+v", response)
	}
}

func TestCodexCanonicalPathDefaultsInstructions(t *testing.T) {
	cap := newCodexCapture(codexDoneSSE)
	defer cap.close()

	cc := NewCodexCaller(codexBase(t, cap), "")
	temperature := 0.0
	rc := &domain.RequestContract{
		Model: "gpt-5.5", Origin: domain.ProtocolOpenAIChat, MaxTokens: 8, Temperature: &temperature,
		Messages: []domain.Message{{Role: domain.RoleSystem, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "sys"}}},
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "yo"}}}},
	}
	res, f := cc.Invoke(context.Background(), rc)
	if f != nil {
		t.Fatalf("Invoke failure: %+v", f)
	}
	if res == nil || res.Content[0].Text != "hi" {
		t.Fatalf("canonical-path response wrong: %+v", res)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(cap.bodyRaw), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["instructions"]; !ok {
		t.Error("instructions field missing (must be explicitly empty)")
	} else if m["instructions"] != "sys" {
		t.Errorf("instructions = %v, want system text", m["instructions"])
	}
	if _, ok := m["max_output_tokens"]; ok {
		t.Errorf("Codex-incompatible max_output_tokens survived canonical normalization: %v", m)
	}
	if _, ok := m["temperature"]; ok {
		t.Errorf("Codex-incompatible temperature survived canonical normalization: %v", m)
	}
	if got := cap.headers["Chatgpt-Account-Id"]; got != "" {
		t.Errorf("account header set without account id: %q", got)
	}
}

func TestCodexTemperatureNormalizationCoversEveryIngressProtocol(t *testing.T) {
	temperature := 0.0
	caller := NewCodexCaller(Base{Model: "gpt-5.6-luna"}, "")
	canonicalMessages := []domain.Message{{Role: domain.RoleUser,
		Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hello"}}}}
	tests := []struct {
		name     string
		contract *domain.RequestContract
	}{
		{name: "openai chat", contract: &domain.RequestContract{
			Model: "hermes-agent", Origin: domain.ProtocolOpenAIChat,
			Temperature: &temperature, Messages: canonicalMessages,
		}},
		{name: "anthropic", contract: &domain.RequestContract{
			Model: "hermes-agent", Origin: domain.ProtocolAnthropic,
			Temperature: &temperature, Messages: canonicalMessages,
		}},
		{name: "openai responses passthrough", contract: &domain.RequestContract{
			Model: "hermes-agent", Origin: domain.ProtocolOpenAIResponses,
			Temperature: &temperature, Messages: canonicalMessages,
			Native: json.RawMessage(`{"model":"hermes-agent","input":"hello","temperature":0}`),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, notes, err := caller.buildRequest(test.contract)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.Join(notes, ";"), "temperature") {
				t.Fatalf("temperature normalization was reported as lossy: %v", notes)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Fatalf("payload is not JSON: %v", err)
			}
			if _, ok := body["temperature"]; ok {
				t.Fatalf("Codex-incompatible temperature survived %s normalization: %s", test.name, payload)
			}
		})
	}
}

func TestCodexStreamPassthrough(t *testing.T) {
	cap := newCodexCapture(codexDoneSSE)
	defer cap.close()

	cc := NewCodexCaller(codexBase(t, cap), "acct-9")
	rc := &domain.RequestContract{
		Model: "gpt-5.5", Stream: true,
		Origin: domain.ProtocolOpenAIResponses,
		Native: json.RawMessage(`{"model":"gpt-5.5","stream":true,"input":[]}`),
		Messages: []domain.Message{{Role: domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "x"}}}},
	}
	ch, cancel, f := cc.Stream(context.Background(), rc)
	if f != nil {
		t.Fatalf("Stream failure: %+v", f)
	}
	defer cancel()
	var gotText, gotEnd bool
	for ev := range ch {
		switch ev.Kind {
		case domain.EvTextDelta:
			gotText = ev.TextDelta == "hi"
		case domain.EvEnd:
			gotEnd = ev.FinishReason == "stop"
		case domain.EvError:
			t.Fatalf("stream error: %+v", ev.Err)
		}
	}
	if !gotText || !gotEnd {
		t.Fatalf("stream incomplete: text=%v end=%v", gotText, gotEnd)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(cap.bodyRaw), &m)
	if m["stream"] != true || m["store"] != false {
		t.Fatalf("stream/store flags wrong: %v/%v", m["stream"], m["store"])
	}
	if cap.auth != "Bearer at-test" || cap.headers["Chatgpt-Account-Id"] != "acct-9" ||
		cap.headers["Originator"] != "codex-tui" {
		t.Fatalf("stream omitted Codex OAuth envelope: auth=%q headers=%v", cap.auth, cap.headers)
	}
}

func TestCodexPrepareNotesEmptyForGatewayOwnedOutputLimit(t *testing.T) {
	cap := newCodexCapture("")
	defer cap.close()
	cc := NewCodexCaller(codexBase(t, cap), "a")
	rc := &domain.RequestContract{
		Model:     "gpt-5.5",
		MaxTokens: 16,
		Origin:    domain.ProtocolOpenAIResponses,
		Native:    json.RawMessage(`{"model":"gpt-5.5","input":[],"max_output_tokens":16}`),
		Messages: []domain.Message{{Role: domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "x"}}}},
	}
	notes, err := cc.Prepare(rc)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Fatalf("gateway-owned output limit must not degrade Codex compatibility, notes=%v", notes)
	}
}

func TestCodexConvertsChatAndAnthropicIngressToResponses(t *testing.T) {
	for _, ingressProtocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolAnthropic} {
		t.Run(string(ingressProtocol), func(t *testing.T) {
			capture := newCodexCapture(codexDoneSSE)
			defer capture.close()
			caller := NewCodexCaller(codexBase(t, capture), "acct-9")
			response, failure := caller.Invoke(context.Background(), &domain.RequestContract{
				Protocol: ingressProtocol, Origin: ingressProtocol, Model: "public-model",
				Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "convert me"}}}},
			})
			if failure != nil || response == nil {
				t.Fatalf("response=%+v failure=%+v", response, failure)
			}
			var wire map[string]any
			if err := json.Unmarshal([]byte(capture.bodyRaw), &wire); err != nil {
				t.Fatal(err)
			}
			if wire["model"] != "gpt-5.5" || wire["stream"] != true || wire["store"] != false {
				t.Fatalf("Codex Responses wire=%v", wire)
			}
			if _, ok := wire["input"].([]any); !ok {
				t.Fatalf("converted request has no Responses input: %v", wire)
			}
			if capture.headers["Chatgpt-Account-Id"] != "acct-9" {
				t.Fatalf("converted request lost OAuth identity: %v", capture.headers)
			}
		})
	}
}

func TestCodexChatKeepsEncryptedReasoningInternalAndMapsSummary(t *testing.T) {
	stream := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"brief\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item_id\":\"rs_1\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"brief\"}],\"encrypted_content\":\"cipher\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item_id\":\"fc_1\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item_id\":\"fc_1\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"arguments\":\"{}\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"
	request := func() *domain.RequestContract {
		return &domain.RequestContract{
			Protocol: domain.ProtocolOpenAIChat, Origin: domain.ProtocolOpenAIChat,
			Model: "public-model", Reasoning: &domain.ReasoningSpec{Effort: "high"},
			Lineage: &domain.ResponsesLineage{PromptCacheKey: "chat-session"},
			Messages: []domain.Message{{Role: domain.RoleUser,
				Blocks: []domain.Block{{Kind: domain.BlockText, Text: "use a tool"}}}},
			Tools: []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
		}
	}
	continuation := request()
	continuation.ProviderSessionKey = "smart-session"
	continuation.Messages = append(continuation.Messages,
		domain.Message{Role: domain.RoleAssistant, Blocks: []domain.Block{
			{Kind: domain.BlockThinking, Text: "brief"},
			{Kind: domain.BlockToolUse, ID: "call_1", Name: "lookup", Input: json.RawMessage(`{}`)},
		}},
		domain.Message{Role: domain.RoleTool, ToolID: "call_1", Blocks: []domain.Block{{
			Kind: domain.BlockToolResult, ID: "call_1", Result: json.RawMessage(`"ok"`),
		}}},
	)
	if notes, err := NewCodexCaller(Base{Model: "gpt-5.5"}, "acct-9").Prepare(continuation); err != nil || len(notes) != 0 {
		t.Fatalf("stable Chat tool continuation was rejected: notes=%v err=%v", notes, err)
	}

	t.Run("invoke", func(t *testing.T) {
		resetReasoningReplay()
		capture := newCodexCapture(stream)
		defer capture.close()
		caller := NewCodexCaller(codexBase(t, capture), "acct-9")
		response, failure := caller.Invoke(context.Background(), request())
		if failure != nil {
			t.Fatalf("Chat conversion rejected gateway-owned reasoning: %v", failure)
		}
		if len(response.ConversionIssues) != 0 || response.FinishReason != "tool_call" ||
			len(response.Content) != 2 || response.Content[0].Kind != domain.BlockThinking ||
			response.Content[0].Text != "brief" || response.Content[1].Kind != domain.BlockToolUse {
			t.Fatalf("converted response=%+v", response)
		}
		session := codexSessionKey(request(), "acct-9")
		if items := takeCodexReasoning("gpt-5.5", session, time.Now()); len(items) != 1 ||
			!strings.Contains(string(items[0]), "cipher") {
			t.Fatalf("encrypted reasoning was not retained for the tool continuation: %s", items)
		}
	})

	t.Run("stream", func(t *testing.T) {
		resetReasoningReplay()
		capture := newCodexCapture(stream)
		defer capture.close()
		caller := NewCodexCaller(codexBase(t, capture), "acct-9")
		events, cancel, failure := caller.Stream(context.Background(), request())
		if failure != nil {
			t.Fatal(failure)
		}
		defer cancel()
		var thinking string
		var sawTool, sawEnd bool
		for event := range events {
			switch event.Kind {
			case domain.EvThinkingDelta:
				thinking += event.ThinkingDelta
			case domain.EvToolStart:
				sawTool = event.ToolID == "call_1" && event.ToolName == "lookup"
			case domain.EvEnd:
				sawEnd = event.FinishReason == "tool_call"
			case domain.EvError:
				t.Fatalf("Chat stream rejected gateway-owned reasoning: %v", event.Err)
			}
		}
		if thinking != "brief" || !sawTool || !sawEnd {
			t.Fatalf("thinking=%q tool=%v end=%v", thinking, sawTool, sawEnd)
		}
	})
}
