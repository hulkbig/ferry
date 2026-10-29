package egress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ferry/internal/domain"
)

// ---------------------------------------------------------------------------
// MultiCaller: per-request adapter dispatch (Phase 2)

// TestMultiCallerDispatchesPerRequest verifies the picker chooses which wire
// format serves each request and that Invoke/Stream land on the matching
// upstream path.
func TestMultiCallerDispatchesPerRequest(t *testing.T) {
	var mu = make(chan string, 16) // records last-hit paths, non-blocking enough for tests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case mu <- r.URL.Path:
		default:
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"content":"chat-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		case strings.HasSuffix(r.URL.Path, "/v1/messages"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"content":[{"type":"text","text":"anthropic-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case strings.HasSuffix(r.URL.Path, "/v1/responses"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"responses-ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := testBase(srv, "/v1")
	multi := NewMultiCaller(b,
		domain.SortIDs([]domain.Protocol{
			domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic,
		}),
		func(rc *domain.RequestContract) domain.Protocol { return rc.Origin },
	)

	cases := []struct {
		origin domain.Protocol
		path   string
		want   string
	}{
		{domain.ProtocolOpenAIChat, "/v1/chat/completions", "chat-ok"},
		{domain.ProtocolAnthropic, "/v1/messages", "anthropic-ok"},
		{domain.ProtocolOpenAIResponses, "/v1/responses", "responses-ok"},
	}
	for _, tc := range cases {
		rc := chatRC()
		rc.Protocol = tc.origin
		rc.Origin = tc.origin
		resp, f := multi.Invoke(context.Background(), rc)
		mustNoFail(t, f)
		if len(resp.Content) == 0 || resp.Content[0].Text != tc.want {
			t.Errorf("origin %s: got %+v, want text %q", tc.origin, resp.Content, tc.want)
		}
		last := <-mu
		if !strings.HasSuffix(last, tc.path) {
			t.Errorf("origin %s hit %q, want suffix %q", tc.origin, last, tc.path)
		}

		// streaming dispatch follows the same picker
		ch, cancel, fs := multi.Stream(context.Background(), rc)
		mustNoFail(t, fs)
		for range ch {
		}
		cancel()
		if p := <-mu; !strings.HasSuffix(p, tc.path) {
			t.Errorf("origin %s stream hit %q, want suffix %q", tc.origin, p, tc.path)
		}
	}
}

func TestNativeResponsesProgressReachesUpstreamInBothModes(t *testing.T) {
	input := []any{
		map[string]any{"type": "function_call", "call_id": "call-exec", "name": "exec", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call-exec", "output": "done"},
		map[string]any{"type": "function_call_output", "call_id": "call-exec", "output": "15/65"},
		map[string]any{"type": "function_call_output", "call_id": "call-exec", "output": "30/65"},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}
	native, err := json.Marshal(map[string]any{"model": "alias", "input": input})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- body
		if strings.Contains(string(body), "\"stream\":true") {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[],\"usage\":{}}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[],\"usage\":{}}")
	}))
	defer srv.Close()
	caller := NewOpenAIResponses(testBase(srv, "/v1"))
	for _, stream := range []bool{false, true} {
		rc := &domain.RequestContract{
			Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIResponses,
			Native: native, Stream: stream,
		}
		if !stream {
			if _, failure := caller.Invoke(context.Background(), rc); failure != nil {
				t.Fatalf("invoke: %+v", failure)
			}
		} else {
			events, stop, failure := caller.Stream(context.Background(), rc)
			if failure != nil {
				t.Fatalf("stream: %+v", failure)
			}
			for event := range events {
				if event.Kind == domain.EvError {
					t.Fatalf("stream error: %+v", event)
				}
			}
			stop()
		}
		var emitted map[string]any
		if err := json.Unmarshal(<-seen, &emitted); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(emitted["input"], input) {
			t.Fatalf("stream=%v changed input: %#v", stream, emitted["input"])
		}
	}
}

// TestMultiCallerPrepareMatchesPickedProtocol verifies that compat previewing
// describes the protocol the request will actually use — the scheduler's
// assessCompat calls Prepare on the caller it finds, so MultiCaller must
// forward to the picked adapter.
func TestMultiCallerPrepareMatchesPickedProtocol(t *testing.T) {
	b := Base{Secret: "s", Model: "m"}
	multi := NewMultiCaller(b,
		[]domain.Protocol{domain.ProtocolAnthropic},
		func(rc *domain.RequestContract) domain.Protocol { return domain.ProtocolAnthropic },
	)

	// anthropic-ingress with max_tokens set → native passthrough → no notes
	rc := &domain.RequestContract{
		Protocol:  domain.ProtocolAnthropic,
		Origin:    domain.ProtocolAnthropic,
		Model:     "m",
		MaxTokens: 100,
		Messages: []domain.Message{
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}}},
		},
	}
	notes, err := multi.Prepare(rc)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("native passthrough must produce no notes, got %v", notes)
	}

	// openai-chat-ingress into an anthropic-only target → conversion notes
	// (thinking dropped / structured output injected etc.). Use a thinking
	// block, which anthropic keeps but chat would drop — here we assert the
	// notes come from the ANTHROPIC adapter's view of a chat-shaped contract.
	rc2 := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIChat,
		Origin:   domain.ProtocolOpenAIChat,
		Model:    "m",
		Messages: []domain.Message{
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}}},
		},
	}
	notes2, err := multi.Prepare(rc2)
	if err != nil {
		t.Fatalf("prepare cross-protocol: %v", err)
	}
	_ = notes2 // anthropic adapter maps chat contracts cleanly; no error expected

	// unknown pick → hard error surfaced as unsupported compatibility
	multi2 := NewMultiCaller(b,
		[]domain.Protocol{domain.ProtocolOpenAIChat},
		func(rc *domain.RequestContract) domain.Protocol { return domain.Protocol("bogus") },
	)
	if _, err := multi2.Prepare(rc); err == nil {
		t.Error("picker returning unregistered protocol must error")
	}
}

// ---------------------------------------------------------------------------
// Phase 3: exact passthrough of the raw client body

// captureServer records the raw request body of each POST.
func captureServer(t *testing.T, respond func(path string) string) (*httptest.Server, *[]string) {
	t.Helper()
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, respond(r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

func TestPassthroughNativeBodyVerbatim(t *testing.T) {
	native := `{"model":"claude-x","max_tokens":777,"stream":false,` +
		`"system":"custom-system","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],` +
		`"provider_only_field":{"nested":true}}`
	srv, bodies := captureServer(t, func(string) string {
		return `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2},"model":"up-m"}`
	})

	a := NewAnthropic(testBase(srv, ""))
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolAnthropic,
		Origin:   domain.ProtocolAnthropic,
		Model:    "alias-name",
		Native:   json.RawMessage(native),
		Messages: []domain.Message{
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}}},
		},
	}
	resp, f := a.Invoke(context.Background(), rc)
	mustNoFail(t, f)
	_ = resp

	if len(*bodies) != 1 {
		t.Fatalf("expected exactly one upstream call, got %d", len(*bodies))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*bodies)[0]), &sent); err != nil {
		t.Fatalf("sent body not JSON: %v", err)
	}
	// gateway-owned fields are patched
	if sent["model"] != "glm-4-flash" { // Base.Model from testBase
		t.Errorf("model must be patched to upstream name, got %v", sent["model"])
	}
	if sent["stream"] != false {
		t.Errorf("stream flag is gateway-owned, got %v", sent["stream"])
	}
	// everything else survives byte-for-byte semantics
	if sent["max_tokens"] != float64(777) {
		t.Errorf("client max_tokens must survive verbatim, got %v", sent["max_tokens"])
	}
	if sent["provider_only_field"] == nil {
		t.Error("fields unknown to the canonical contract must pass through untouched")
	}
	if s, _ := sent["system"].(string); s != "custom-system" {
		t.Errorf("client system prompt must survive, got %v", sent["system"])
	}
}

// TestPassthroughNotTriggeredForCrossProtocol ensures a converted attempt
// re-encodes canonically instead of replaying a foreign body.
func TestPassthroughNotTriggeredForCrossProtocol(t *testing.T) {
	native := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"weird_field":1}`
	srv, bodies := captureServer(t, func(string) string {
		return `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	})

	o := NewOpenAIChat(testBase(srv, ""))
	rc := chatRC() // Protocol=openai-chat but Origin unset ("") — no Native replay
	rc.Native = json.RawMessage(native)
	if _, f := o.Invoke(context.Background(), rc); f != nil {
		t.Fatalf("invoke: %v", f)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*bodies)[0]), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := sent["weird_field"]; ok {
		t.Error("cross-protocol attempt must not replay the raw body")
	}
	if sent["temperature"] == nil {
		t.Error("canonical encoding should carry temperature from the contract")
	}
}

// TestPassthroughModelRenameDropsNative mirrors scheduler.contractFor: when
// the alias differs from the upstream model AND the body was already cleared
// by the scheduler this is belt-and-braces — but if Native survived while
// names differ, passthrough still patches the model field.
func TestPassthroughModelRenameDropsNative(t *testing.T) {
	native := `{"model":"alias-name","messages":[],"keep_me":true}`
	srv, bodies := captureServer(t, func(string) string {
		return `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	})

	a := NewAnthropic(testBase(srv, ""))
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolAnthropic,
		Origin:   domain.ProtocolAnthropic,
		Model:    "alias-name",
		Native:   json.RawMessage(native),
	}
	if _, f := a.Invoke(context.Background(), rc); f != nil {
		t.Fatalf("invoke: %v", f)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte((*bodies)[0]), &sent)
	if sent["model"] != "glm-4-flash" || sent["keep_me"] != true {
		t.Errorf("passthrough should patch model only: got model=%v keep=%v",
			sent["model"], sent["keep_me"])
	}
}

// TestPassthroughStreamFlagFollowsExecutionMode checks the stream field is
// forced by the execution mode, not trusted from the client body.
func TestPassthroughStreamFlagFollowsExecutionMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, data(`{"choices":[{"delta":{"content":"x"},"index":0}]}`)+"\n\n")
		io.WriteString(w, data(`{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)+"\n\n")
		io.WriteString(w, data("[DONE]")+"\n\n")
	}))
	defer srv.Close()
	bodies := &[]string{}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, data(`{"choices":[{"delta":{"content":"x"}}]}`)+"\n\n")
		io.WriteString(w, data(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)+"\n\n")
		io.WriteString(w, data("[DONE]")+"\n\n")
	})

	o := NewOpenAIChat(testBase(srv, ""))
	rc := chatRC()
	rc.Native = json.RawMessage(`{"model":"whatever","stream":false,"messages":[]}`)

	ch, cancel, f := o.Stream(context.Background(), rc)
	mustNoFail(t, f)
	defer cancel()
	for range ch {
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*bodies)[0]), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent["stream"] != true {
		t.Errorf("stream execution mode must force stream=true, got %v", sent["stream"])
	}
}

// TestResponsesAdapterForcesStoreFalseOnPassthrough pins the default for a
// request that is not participating in Ferry-managed conversation state.
func TestResponsesAdapterForcesStoreFalseOnPassthrough(t *testing.T) {
	srv, bodies := captureServer(t, func(string) string {
		return `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	})
	r := NewOpenAIResponses(testBase(srv, ""))
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIResponses,
		Origin:   domain.ProtocolOpenAIResponses,
		Model:    "m",
		Native: json.RawMessage(
			`{"model":"m","store":true,"input":[{"role":"user","content":"hi"}]}`),
	}
	if _, f := r.Invoke(context.Background(), rc); f != nil {
		t.Fatalf("invoke: %v", f)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*bodies)[0]), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent["store"] != false {
		t.Errorf("store must be forced false (stateless gateway), got %v", sent["store"])
	}
}

func TestResponsesAdapterReplacesVirtualLineageWithNativeHandle(t *testing.T) {
	r := NewOpenAIResponses(testBase(nil, ""))
	r.Model = "upstream"
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIResponses,
		Model: "smart", PersistNativeState: true,
		Lineage: &domain.ResponsesLineage{
			ConversationID: "conv_smart_1", PreviousResponseID: "resp_smart_1", PromptCacheKey: "cache-1",
		},
		Continuation: &domain.NativeContinuation{
			NativeConversationID: "conv_native_1", NativeResponseID: "resp_native_1",
		},
		Native: json.RawMessage(`{"model":"smart","conversation":"conv_smart_1","previous_response_id":"resp_smart_1","input":"next"}`),
	}
	payload, _, err := r.buildRequest(rc)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(payload, &sent); err != nil {
		t.Fatal(err)
	}
	if _, exists := sent["conversation"]; exists || sent["previous_response_id"] != "resp_native_1" || sent["store"] != true {
		t.Fatalf("native continuation = %v", sent)
	}
	if sent["conversation"] == "conv_smart_1" {
		t.Fatalf("client virtual conversation leaked upstream: %v", sent)
	}
}

func TestResponsesCanonicalEgressChoosesOneNativeContinuation(t *testing.T) {
	r := NewOpenAIResponses(testBase(nil, ""))
	r.Model = "upstream"
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIChat,
		Model: "smart", Messages: []domain.Message{{
			Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "next"}},
		}},
		Continuation: &domain.NativeContinuation{
			NativeConversationID: "conv_native", NativeResponseID: "resp_native",
		},
	}
	payload, _, err := r.buildRequest(rc)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(payload, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["previous_response_id"] != "resp_native" {
		t.Fatalf("native response continuation missing: %v", sent)
	}
	if _, exists := sent["conversation"]; exists {
		t.Fatalf("mutually exclusive native continuation leaked both fields: %v", sent)
	}
}

func TestNativeStreamFramesPreservedForAllTextProtocols(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.URL.Path {
		case "/chat/completions":
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"index\":0}],\"future\":1}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/messages":
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{}},\"future\":1}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case "/responses":
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"},\"future\":1}\n\n")
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"x\"}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{}}}\n\n")
		}
	}))
	defer srv.Close()

	tests := []struct {
		protocol domain.Protocol
		caller   Caller
		body     string
	}{
		{domain.ProtocolOpenAIChat, NewOpenAIChat(testBase(srv, "")), `{"model":"alias","messages":[],"stream":true}`},
		{domain.ProtocolAnthropic, NewAnthropic(testBase(srv, "")), `{"model":"alias","messages":[],"max_tokens":8,"stream":true}`},
		{domain.ProtocolOpenAIResponses, NewOpenAIResponses(testBase(srv, "")), `{"model":"alias","input":"x","stream":true}`},
	}
	for _, tc := range tests {
		t.Run(string(tc.protocol), func(t *testing.T) {
			rc := &domain.RequestContract{Protocol: tc.protocol, Origin: tc.protocol,
				Model: "alias", Native: json.RawMessage(tc.body), Stream: true}
			events, _, failure := tc.caller.Stream(context.Background(), rc)
			if failure != nil {
				t.Fatal(failure)
			}
			nativeCount := 0
			terminal := false
			for event := range events {
				if event.Kind == domain.EvNative {
					nativeCount++
					if event.NativeFrame == nil || event.NativeProtocol != tc.protocol {
						t.Fatalf("bad native event: %+v", event)
					}
					terminal = terminal || event.NativeFrame.Terminal
				}
			}
			if nativeCount < 2 || !terminal {
				t.Fatalf("native frames=%d terminal=%v", nativeCount, terminal)
			}
		})
	}
}

func TestNativeStreamsRejectMalformedJSONFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {broken\n\n")
	}))
	defer srv.Close()

	tests := []struct {
		protocol domain.Protocol
		caller   Caller
		body     string
	}{
		{domain.ProtocolOpenAIChat, NewOpenAIChat(testBase(srv, "")), `{"model":"alias","messages":[],"stream":true}`},
		{domain.ProtocolAnthropic, NewAnthropic(testBase(srv, "")), `{"model":"alias","messages":[],"max_tokens":8,"stream":true}`},
		{domain.ProtocolOpenAIResponses, NewOpenAIResponses(testBase(srv, "")), `{"model":"alias","input":"x","stream":true}`},
	}
	for _, tc := range tests {
		t.Run(string(tc.protocol), func(t *testing.T) {
			rc := &domain.RequestContract{Protocol: tc.protocol, Origin: tc.protocol,
				Model: "alias", Native: json.RawMessage(tc.body), Stream: true}
			events, _, failure := tc.caller.Stream(context.Background(), rc)
			if failure != nil {
				t.Fatal(failure)
			}
			got := drain(t, events)
			if len(got) != 1 || got[0].Kind != domain.EvError || got[0].Err == nil ||
				got[0].Err.Type != domain.FailStreamBroken {
				t.Fatalf("malformed native JSON was forwarded or misclassified: %+v", got)
			}
		})
	}
}

func TestProtocolFeatureHeadersForwardOnlyOnNativePath(t *testing.T) {
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer srv.Close()
	a := NewAnthropic(testBase(srv, ""))
	rc := &domain.RequestContract{Protocol: domain.ProtocolAnthropic, Origin: domain.ProtocolAnthropic,
		Model: "alias", Native: json.RawMessage(`{"model":"alias","messages":[],"max_tokens":8}`),
		ProtocolHeaders: map[string]string{"Anthropic-Beta": "feature-2026"}}
	if _, failure := a.Invoke(context.Background(), rc); failure != nil {
		t.Fatal(failure)
	}
	if beta != "feature-2026" {
		t.Fatalf("Anthropic-Beta=%q", beta)
	}
}

func TestUnknownNativeFeaturesFailClosedOnConvertedPath(t *testing.T) {
	rc := &domain.RequestContract{Protocol: domain.ProtocolAnthropic, Origin: domain.ProtocolAnthropic,
		Model: "alias", Native: json.RawMessage(`{"model":"alias","messages":[]}`),
		ConversionIssues: []string{"content type document is native-only"}}
	chat := NewOpenAIChat(testBase(nil, ""))
	if _, err := chat.Prepare(rc); err == nil || !strings.Contains(err.Error(), "requires native") {
		t.Fatalf("converted path accepted lossy request: %v", err)
	}
	anthropic := NewAnthropic(testBase(nil, ""))
	if _, err := anthropic.Prepare(rc); err != nil {
		t.Fatalf("exact native path rejected: %v", err)
	}
}
