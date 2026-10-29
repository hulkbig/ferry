package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ferry/internal/buildinfo"
	"ferry/internal/domain"
	"ferry/internal/policy"
	sched "ferry/internal/scheduler"
)

// protocol helpers keep the call sites short
func chatH(eng *stubEngine) *OpenAIChatHandler     { return &OpenAIChatHandler{Eng: eng} }
func rsH(eng *stubEngine) *ResponsesHandler        { return &ResponsesHandler{Eng: eng} }
func anH(eng *stubEngine) *AnthropicHandler        { return &AnthropicHandler{Eng: eng} }
func imagesH(eng *stubEngine) *OpenAIImagesHandler { return &OpenAIImagesHandler{Eng: eng} }

// stubEngine implements ingress.Engine, recording the contract it was given
// and replaying a scripted response or failure.
type stubEngine struct {
	gotContract *domain.RequestContract
	gotMeta     sched.RequestMeta

	resp *domain.Response
	evs  []domain.StreamEvent
	fail *domain.Failure

	dryPlan *policy.PlanResult
	dryPol  *domain.CompiledPolicy
	dryFail *domain.Failure

	conversationID string
	snapshot       *domain.CompiledSnapshot
	authenticated  bool
	tierEvidence   *domain.ServiceTierEvidence
}

func (e *stubEngine) Execute(_ context.Context, rc *domain.RequestContract, meta sched.RequestMeta) (*sched.Result, *domain.Failure) {
	e.gotContract = rc
	e.gotMeta = meta
	if e.fail != nil {
		return nil, e.fail
	}
	res := &sched.Result{
		RequestID:           "req-stub",
		Model:               "glm-4-flash",
		ConversationID:      e.conversationID,
		ServiceTierEvidence: e.tierEvidence,
	}
	if rc.Stream {
		ch := make(chan domain.StreamEvent, len(e.evs)+1)
		for _, ev := range e.evs {
			ch <- ev
		}
		close(ch)
		res.Events = ch
	} else {
		res.Response = e.resp
	}
	return res, nil
}

func (e *stubEngine) DryRun(rc *domain.RequestContract, _ sched.RequestMeta) (*policy.PlanResult, *domain.CompiledPolicy, *domain.Failure) {
	e.gotContract = rc
	if e.dryFail != nil {
		return nil, nil, e.dryFail
	}
	return e.dryPlan, e.dryPol, nil
}

func (e *stubEngine) Snapshot() *domain.CompiledSnapshot  { return e.snapshot }
func (e *stubEngine) Authenticate(sched.RequestMeta) bool { return e.authenticated }

// direct handler accessors (bypassing Server) for unit-level tests
func (e *stubEngine) chat() http.Handler      { return chatH(e) }
func (e *stubEngine) responses() http.Handler { return rsH(e) }
func (e *stubEngine) messages() http.Handler  { return anH(e) }
func (e *stubEngine) images() http.Handler    { return imagesH(e) }

func newStub() *stubEngine {
	return &stubEngine{
		authenticated: true,
		resp: &domain.Response{
			Content:      []domain.Block{{Kind: domain.BlockText, Text: "hello"}},
			FinishReason: "stop",
			Usage:        domain.Usage{InputTokens: 10, OutputTokens: 5},
		},
		dryPlan: &policy.PlanResult{Primary: "t1", Order: []domain.ResourceTargetID{"t1"}},
		dryPol:  &domain.CompiledPolicy{Name: "std"},
	}
}

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response not JSON: %v\n%s", err, rec.Body.String())
	}
	return m
}

func sub(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("key %q is not an object: %v", key, m)
	}
	return v
}

func TestManagementSessionCookieCannotAuthenticateDataPlane(t *testing.T) {
	engine := newStub()
	engine.authenticated = false
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.AddCookie(&http.Cookie{Name: "smartapi_admin_session", Value: "management-session"})
	rec := httptest.NewRecorder()
	(&ModelsHandler{Source: engine}).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("management cookie authenticated data plane: %d %s", rec.Code, rec.Body.String())
	}
}

// jsonEqual compares raw JSON bytes against an expected value by decoding both.
func jsonEqual(raw json.RawMessage, want any) bool {
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	wantJSON, _ := json.Marshal(want)
	var wantDec any
	_ = json.Unmarshal(wantJSON, &wantDec)
	gotJSON, _ := json.Marshal(got)
	return string(gotJSON) == string(wantJSON) && fmt.Sprint(got) == fmt.Sprint(wantDec)
}

// ---------------------------------------------------------------------------
// request parsing → canonical contract

func TestChatToContract(t *testing.T) {
	body := `{
		"model": "glm-4-flash",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": [{"type": "text", "text": "hi"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}]},
			{"role": "assistant", "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "get", "arguments": "{\"q\":\"x\"}"}}]},
			{"role": "tool", "tool_call_id": "c1", "content": "42"}
		],
		"tools": [{"type": "function", "function": {"name": "get", "description": "d", "parameters": {"type": "object"}}}],
		"tool_choice": {"type": "function", "function": {"name": "get"}},
		"response_format": {"type": "json_schema", "json_schema": {"name": "out", "schema": {"type": "object"}}},
		"reasoning_effort": "high",
		"max_tokens": 256,
		"temperature": 0.5,
		"top_p": 0.9,
		"stop": ["END"],
		"stream": true
	}`
	eng := newStub()
	rec := postJSON(t, chatH(eng), "/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rc := eng.gotContract
	if rc == nil {
		t.Fatal("engine never received a contract")
	}
	if rc.Protocol != domain.ProtocolOpenAIChat || rc.Model != "glm-4-flash" || !rc.Stream {
		t.Errorf("header fields: %+v", rc)
	}
	if len(rc.Messages) != 4 {
		t.Fatalf("messages=%d want 4", len(rc.Messages))
	}
	if rc.Messages[0].Role != domain.RoleSystem || blocksText(rc.Messages[0].Blocks) != "be brief" {
		t.Errorf("system: %+v", rc.Messages[0])
	}
	u := rc.Messages[1]
	if len(u.Blocks) != 2 || u.Blocks[0].Text != "hi" ||
		u.Blocks[1].Kind != domain.BlockImage || string(u.Blocks[1].ImageData) != "hello" ||
		u.Blocks[1].ImageMIME != "image/png" {
		t.Errorf("user content parts: %+v", u.Blocks)
	}
	a := rc.Messages[2]
	if a.Blocks[0].Kind != domain.BlockToolUse || a.Blocks[0].ID != "c1" ||
		a.Blocks[0].Name != "get" || string(a.Blocks[0].Input) != `{"q":"x"}` {
		t.Errorf("assistant tool call: %+v", a.Blocks)
	}
	tl := rc.Messages[3]
	if tl.Role != domain.RoleTool || tl.ToolID != "c1" || blocksText(tl.Blocks) != "42" {
		t.Errorf("tool turn: %+v", tl)
	}
	if len(rc.Tools) != 1 || rc.Tools[0].Name != "get" || rc.Tools[0].Description != "d" {
		t.Errorf("tools: %+v", rc.Tools)
	}
	if rc.ToolChoice != "get" {
		t.Errorf("tool_choice=%q want tool name", rc.ToolChoice)
	}
	if rc.ResponseFormat == nil || rc.ResponseFormat.Name != "out" ||
		!jsonEqual(rc.ResponseFormat.Schema, map[string]any{"type": "object"}) {
		t.Errorf("response_format: %+v", rc.ResponseFormat)
	}
	if rc.Reasoning == nil || rc.Reasoning.Effort != "high" {
		t.Errorf("reasoning: %+v", rc.Reasoning)
	}
	if rc.MaxTokens != 256 || rc.Temperature == nil || *rc.Temperature != 0.5 ||
		rc.TopP == nil || *rc.TopP != 0.9 || len(rc.StopSequences) != 1 || rc.StopSequences[0] != "END" {
		t.Errorf("sampling fields: %+v", rc)
	}
}

func TestChatToolResultNameIsRedundantCrossProtocolMetadata(t *testing.T) {
	rc, err := (&OpenAIChatHandler{}).toContract([]byte(`{
		"model":"hermes-agent",
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"session_search","arguments":"{}"}}]},
			{"role":"tool","name":"session_search","tool_call_id":"call-1","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"session_search","parameters":{"type":"object"}}}],
		"reasoning_effort":"high",
		"stream":true,
		"stream_options":{"include_usage":true}
	}`))
	if err != nil {
		t.Fatalf("parse Hermes-style tool continuation: %v", err)
	}
	if len(rc.ConversionIssues) != 0 {
		t.Fatalf("redundant tool-result name blocked conversion: %v", rc.ConversionIssues)
	}
	if len(rc.Messages) != 2 || rc.Messages[1].Role != domain.RoleTool ||
		rc.Messages[1].ToolID != "call-1" || rc.Messages[1].Name != "" {
		t.Fatalf("tool result was not canonically correlated by id: %+v", rc.Messages)
	}
}

func TestChatParticipantNameIsMappedNotBlocking(t *testing.T) {
	rc, err := (&OpenAIChatHandler{}).toContract([]byte(`{
		"model":"m","messages":[{"role":"user","name":"operator-a","content":"hello"}]
	}`))
	if err != nil {
		t.Fatalf("parse named participant: %v", err)
	}
	// The participant name is auxiliary sender metadata: it is omitted with a
	// mapping note instead of blocking cross-protocol conversion.
	if len(rc.ConversionIssues) != 0 {
		t.Fatalf("participant identity must not block conversion: %v", rc.ConversionIssues)
	}
	if len(rc.MappingNotes) != 1 || rc.MappingNotes[0] != "message sender name omitted" {
		t.Fatalf("participant identity mapping note=%v", rc.MappingNotes)
	}
}

func TestChatParseErrors(t *testing.T) {
	cases := []struct{ body, wantSub string }{
		{`not json`, "invalid JSON body"},
		{`{"model":"m","messages":[]}`, "messages is required"},
		{`{"model":"m","messages":[{"role":"user"}],"tool_choice":{"type":"x"}}`, "unsupported tool_choice shape"},
		{`{"model":"m","messages":[{"role":"user"}],"response_format":{"type":"json_schema"}}`, "json_schema.schema is required"},
		{`{"model":"m","messages":[{"role":"user"}],"response_format":{"type":"yaml"}}`, "unsupported response_format.type"},
		{`{"model":"m","messages":[{"role":"user"}],"stop":[1]}`, "stop must be string or array"},
	}
	for _, tc := range cases {
		_, err := (&OpenAIChatHandler{}).toContract([]byte(tc.body))
		if err == nil {
			t.Errorf("%s: expected error", tc.body)
			continue
		}
		var pe *parseError
		if !isParseErr(err, &pe) {
			t.Errorf("%s: error must be parseError, got %T", tc.body, err)
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: error %q missing %q", tc.body, err.Error(), tc.wantSub)
		}
	}
}

func TestChatNativeOnlyContentIsAcceptedForExactPassthrough(t *testing.T) {
	rc, err := (&OpenAIChatHandler{}).toContract([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x"}}]}]}`))
	if err != nil || len(rc.ConversionIssues) == 0 || rc.Messages[0].Blocks[0].Kind != domain.BlockOpaque {
		t.Fatalf("native-only chat content: rc=%+v err=%v", rc, err)
	}
	if requirements := rc.ExtractRequirements(); requirements.Tools || requirements.Reasoning || requirements.Vision {
		t.Fatalf("unknown input_audio was guessed as an existing capability: %+v", requirements)
	}
}

func TestChatRemoteImageRequiresVision(t *testing.T) {
	rc, err := (&OpenAIChatHandler{}).toContract([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}]}`))
	if err != nil || !rc.ExtractRequirements().Vision {
		t.Fatalf("remote Chat image did not require vision: rc=%+v err=%v", rc, err)
	}
}

func isParseErr(err error, target **parseError) bool {
	return errors.As(err, target)
}

func TestResponsesToContract(t *testing.T) {
	body := `{
		"model": "glm-4.6",
		"instructions": "sys text",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "function_call", "call_id": "f1", "name": "get", "arguments": "{\"a\":1}"},
			{"type": "function_call_output", "call_id": "f1", "output": "ok"},
			{"type": "reasoning", "summary": []}
		],
		"tools": [{"type": "function", "name": "get", "parameters": {}}],
		"tool_choice": "auto",
		"text": {"format": {"type": "json_object"}},
		"reasoning": {"effort": "low"},
		"max_output_tokens": 128,
		"metadata": {"session": "s1", "num": 7}
	}`
	eng := newStub()
	rec := postJSON(t, rsH(eng), "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rc := eng.gotContract
	if rc == nil {
		t.Fatal("engine never received a contract")
	}
	if rc.Protocol != domain.ProtocolOpenAIResponses || rc.Model != "glm-4.6" {
		t.Errorf("header: %+v", rc)
	}
	// Reasoning is retained as an opaque native-only item so exact Responses
	// passthrough remains possible while conversions fail closed.
	if len(rc.Messages) != 5 {
		t.Fatalf("messages=%d want 5: %+v", len(rc.Messages), rc.Messages)
	}
	if rc.Messages[0].Role != domain.RoleSystem || blocksText(rc.Messages[0].Blocks) != "sys text" {
		t.Errorf("instructions: %+v", rc.Messages[0])
	}
	if rc.Messages[2].Blocks[0].Kind != domain.BlockToolUse ||
		rc.Messages[2].Blocks[0].ID != "f1" || rc.Messages[2].Blocks[0].Name != "get" {
		t.Errorf("function_call item: %+v", rc.Messages[2])
	}
	if rc.Messages[3].Role != domain.RoleTool || rc.Messages[3].ToolID != "f1" ||
		len(rc.Messages[3].Blocks) != 1 || rc.Messages[3].Blocks[0].Kind != domain.BlockToolResult ||
		string(rc.Messages[3].Blocks[0].Result) != `"ok"` {
		t.Errorf("function_call_output item: %+v", rc.Messages[3])
	}
	if rc.ToolChoice != "auto" {
		t.Errorf("tool_choice=%q", rc.ToolChoice)
	}
	if rc.ResponseFormat == nil || rc.ResponseFormat.Name != "response" {
		t.Errorf("text.format json_object: %+v", rc.ResponseFormat)
	}
	if rc.Reasoning == nil || rc.Reasoning.Effort != "low" {
		t.Errorf("reasoning: %+v", rc.Reasoning)
	}
	if rc.MaxTokens != 128 {
		t.Errorf("max_output_tokens=%d", rc.MaxTokens)
	}
	if rc.Metadata["session"] != "s1" || len(rc.Metadata) != 1 {
		t.Errorf("metadata (string values only): %+v", rc.Metadata)
	}
	if rc.Messages[4].Blocks[0].Kind != domain.BlockOpaque || len(rc.ConversionIssues) == 0 {
		t.Errorf("reasoning item must be native-only: %+v issues=%v", rc.Messages[4], rc.ConversionIssues)
	}
}

func TestResponsesParallelToolTranscriptRemainsOneAssistantAndResultTurn(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"go"},
			{"type":"function_call","call_id":"f1","name":"first","arguments":"{\"x\":1}"},
			{"type":"function_call","call_id":"f2","name":"second","arguments":"{\"y\":2}"},
			{"type":"function_call_output","call_id":"f1","output":{"ok":1}},
			{"type":"function_call_output","call_id":"f2","output":"done"}
		],
		"tools":[
			{"type":"function","name":"first","parameters":{}},
			{"type":"function","name":"second","parameters":{}}
		]
	}`)
	rc, err := (&ResponsesHandler{}).toContract(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Messages) != 3 || len(rc.Messages[1].Blocks) != 2 || len(rc.Messages[2].Blocks) != 2 {
		t.Fatalf("parallel transcript was split into sequential turns: %+v", rc.Messages)
	}
	if rc.Messages[1].Blocks[0].ID != "f1" || rc.Messages[1].Blocks[1].ID != "f2" ||
		rc.Messages[2].Blocks[0].ID != "f1" || rc.Messages[2].Blocks[1].ID != "f2" {
		t.Fatalf("parallel tool IDs were not preserved: %+v", rc.Messages)
	}
	if err := rc.ValidateToolSequence(); err != nil {
		t.Fatalf("valid parallel Responses transcript was rejected: %v", err)
	}
}

func TestResponsesParseErrors(t *testing.T) {
	cases := []struct{ body, wantSub string }{
		{`{"model":""}`, "model is required"},
		{`{"model":"m"}`, "input is required"},
		{`{"model":"m","input":"x","text":{"format":{"type":"xml"}}}`, "unsupported text.format.type"},
	}
	for _, tc := range cases {
		_, err := (&ResponsesHandler{}).toContract([]byte(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: got %v want ~%q", tc.body, err, tc.wantSub)
		}
	}
}

func TestResponsesNativeOnlyItemIsAcceptedForExactPassthrough(t *testing.T) {
	rc, err := (&ResponsesHandler{}).toContract([]byte(`{"model":"m","input":[{"type":"computer_call","id":"x"}]}`))
	if err != nil || len(rc.ConversionIssues) == 0 || rc.Messages[0].Blocks[0].Kind != domain.BlockOpaque {
		t.Fatalf("native-only Responses item: rc=%+v err=%v", rc, err)
	}
	if !rc.ExtractRequirements().Tools {
		t.Fatal("native computer_call did not require tools")
	}
}

func TestResponsesNativeCapabilitiesAreGated(t *testing.T) {
	cases := []struct {
		name string
		body string
		want func(domain.Requirements) bool
	}{
		{"remote image", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.test/image.png"}]}]}`, func(r domain.Requirements) bool { return r.Vision }},
		{"reasoning item", `{"model":"m","input":[{"type":"reasoning","summary":[]}]}`, func(r domain.Requirements) bool { return r.Reasoning }},
		{"empty reasoning object", `{"model":"m","input":"x","reasoning":{}}`, func(r domain.Requirements) bool { return r.Reasoning }},
		{"built-in tool", `{"model":"m","input":"x","tools":[{"type":"web_search_preview"}]}`, func(r domain.Requirements) bool { return r.Tools }},
		{"built-in tool choice", `{"model":"m","input":"x","tool_choice":{"type":"web_search_preview"}}`, func(r domain.Requirements) bool { return r.Tools }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := (&ResponsesHandler{}).toContract([]byte(tc.body))
			if err != nil || !tc.want(rc.ExtractRequirements()) {
				t.Fatalf("native capability was not gated: rc=%+v err=%v", rc, err)
			}
		})
	}
}

func TestResponsesFunctionToolChoiceUsesTopLevelName(t *testing.T) {
	rc, err := (&ResponsesHandler{}).toContract([]byte(`{"model":"m","input":"x","tool_choice":{"type":"function","name":"lookup"}}`))
	if err != nil || rc.ToolChoice != "lookup" {
		t.Fatalf("Responses function tool choice: rc=%+v err=%v", rc, err)
	}
}

func TestResponsesIncompleteReplayStatusRemainsNativeOnly(t *testing.T) {
	rc, err := (&ResponsesHandler{}).toContract([]byte(`{
		"model":"m",
		"input":[{"type":"function_call","id":"fc-1","status":"incomplete","call_id":"call-1","name":"lookup","arguments":"{}"}]
	}`))
	if err != nil {
		t.Fatalf("parse incomplete replay item: %v", err)
	}
	if len(rc.ConversionIssues) != 1 || rc.ConversionIssues[0] != "function_call status incomplete is native-only" {
		t.Fatalf("incomplete replay state must stay on Responses: %v", rc.ConversionIssues)
	}
}

func TestNestedNativeOnlyFieldsBlockCrossProtocolConversion(t *testing.T) {
	rc, err := (&ResponsesHandler{}).toContract([]byte(`{"model":"m","input":"x","tools":[{"type":"function","name":"lookup","parameters":{},"strict":true}]}`))
	if err != nil || len(rc.ConversionIssues) == 0 ||
		!strings.Contains(strings.Join(rc.ConversionIssues, ";"), "tools[0].strict") {
		t.Fatalf("nested Responses field was silently dropped: rc=%+v err=%v", rc, err)
	}
}

func TestAnthropicToContract(t *testing.T) {
	body := `{
		"model": "claude-x",
		"system": [{"type": "text", "text": "be "}, {"type": "text", "text": "kind"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}, {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "aGVsbG8="}}]},
			{"role": "assistant", "content": [{"type": "thinking", "thinking": "hmm", "signature": "sig-1"}, {"type": "redacted_thinking", "data": "opaque-1"}, {"type": "tool_use", "id": "tu1", "name": "get", "input": {"k": 1}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "tu1", "content": "res"}]}
		],
		"max_tokens": 512,
		"tools": [{"name": "get", "description": "d", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "any"},
		"thinking": {"type": "enabled", "budget_tokens": 4096},
		"stop_sequences": ["S"]
	}`
	eng := newStub()
	rec := postJSON(t, anH(eng), "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rc := eng.gotContract
	if rc == nil {
		t.Fatal("engine never received a contract")
	}
	if rc.Protocol != domain.ProtocolAnthropic || rc.Model != "claude-x" || rc.MaxTokens != 512 {
		t.Errorf("header: %+v", rc)
	}
	// system + 3 turns
	if len(rc.Messages) != 4 {
		t.Fatalf("messages=%d want 4: %+v", len(rc.Messages), rc.Messages)
	}
	if blocksText(rc.Messages[0].Blocks) != "be kind" {
		t.Errorf("system join: %+v", rc.Messages[0])
	}
	if rc.Messages[1].Blocks[1].Kind != domain.BlockImage || string(rc.Messages[1].Blocks[1].ImageData) != "hello" {
		t.Errorf("image block: %+v", rc.Messages[1].Blocks[1])
	}
	if rc.Messages[2].Blocks[0].Kind != domain.BlockThinking || rc.Messages[2].Blocks[0].Text != "hmm" || rc.Messages[2].Blocks[0].Signature != "sig-1" {
		t.Errorf("thinking block: %+v", rc.Messages[2])
	}
	if rc.Messages[2].Blocks[1].Kind != domain.BlockRedactedThinking || rc.Messages[2].Blocks[1].Data != "opaque-1" {
		t.Errorf("redacted thinking block: %+v", rc.Messages[2])
	}
	if rc.Messages[2].Blocks[2].Kind != domain.BlockToolUse || rc.Messages[2].Blocks[2].ID != "tu1" {
		t.Errorf("tool_use block: %+v", rc.Messages[2])
	}
	tr := rc.Messages[3]
	var toolResult string
	_ = json.Unmarshal(tr.Blocks[0].Result, &toolResult)
	if tr.Role != domain.RoleTool || tr.ToolID != "tu1" || tr.Blocks[0].Kind != domain.BlockToolResult ||
		tr.Blocks[0].ID != "tu1" || toolResult != "res" {
		t.Errorf("tool_result via tool_use_id: %+v", tr)
	}
	if rc.ToolChoice != "required" {
		t.Errorf("tool_choice any→required, got %q", rc.ToolChoice)
	}
	if rc.Reasoning == nil || rc.Reasoning.BudgetTokens != 4096 {
		t.Errorf("thinking: %+v", rc.Reasoning)
	}
	if len(rc.StopSequences) != 1 || rc.StopSequences[0] != "S" {
		t.Errorf("stop_sequences: %v", rc.StopSequences)
	}
	if len(rc.ConversionIssues) != 0 {
		t.Errorf("fully captured Anthropic semantics were marked opaque: %v", rc.ConversionIssues)
	}
}

func TestAnthropicToolResultAndFollowingUserTextSplitLosslessly(t *testing.T) {
	rc, err := (&AnthropicHandler{}).toContract([]byte(`{
		"model":"m",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"lookup","input":{}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call-1","content":"found"},
				{"type":"text","text":"Use that result."}
			]}
		],
		"max_tokens":128
	}`))
	if err != nil {
		t.Fatalf("parse mixed tool-result turn: %v", err)
	}
	if len(rc.ConversionIssues) != 0 {
		t.Fatalf("representable mixed turn was marked native-only: %v", rc.ConversionIssues)
	}
	if len(rc.Messages) != 3 || rc.Messages[1].Role != domain.RoleTool ||
		rc.Messages[1].ToolID != "call-1" || len(rc.Messages[1].Blocks) != 1 ||
		rc.Messages[2].Role != domain.RoleUser || blocksText(rc.Messages[2].Blocks) != "Use that result." {
		t.Fatalf("tool result and user continuation were not split in order: %+v", rc.Messages)
	}
}

func TestAnthropicParseErrors(t *testing.T) {
	cases := []struct{ body, wantSub string }{
		{`{"messages":[]}`, "model is required"},
		{`{"model":"m"}`, "messages is required"},
		{`{"model":"m","messages":[{"role":"u"}],"tool_choice":{"type":"wat"}}`, "unsupported tool_choice.type"},
		{`{"model":"m","messages":[{"role":"u"}],"tool_choice":{"type":"tool"}}`, "requires name"},
		{`{"model":"m","messages":[{"role":"u"}],"thinking":{"type":"enabled","budget_tokens":0}}`, "must be positive"},
	}
	for _, tc := range cases {
		_, err := (&AnthropicHandler{}).toContract([]byte(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: got %v want ~%q", tc.body, err, tc.wantSub)
		}
	}
}

func TestAnthropicNonBase64ImageStaysNativeOnly(t *testing.T) {
	rc, err := (&AnthropicHandler{}).toContract([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}}]}]}`))
	if err != nil || len(rc.ConversionIssues) == 0 ||
		len(rc.Messages) != 1 || len(rc.Messages[0].Blocks) != 1 ||
		rc.Messages[0].Blocks[0].Kind != domain.BlockOpaque {
		t.Fatalf("native-only remote image: rc=%+v err=%v", rc, err)
	}
	if !rc.ExtractRequirements().Vision {
		t.Fatal("native Anthropic image did not require vision")
	}
}

func TestAnthropicNativeThinkingAndToolsAreGated(t *testing.T) {
	rc, err := (&AnthropicHandler{}).toContract([]byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","input_schema":{}}],
		"thinking":{"type":"adaptive"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	requirements := rc.ExtractRequirements()
	if !requirements.Tools || !requirements.Reasoning {
		t.Fatalf("native Anthropic capabilities were not gated: %+v", requirements)
	}
}

func TestAnthropicNativeOnlyBlockIsAcceptedForExactPassthrough(t *testing.T) {
	rc, err := (&AnthropicHandler{}).toContract([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","data":"eA=="}}]}],"max_tokens":8}`))
	if err != nil || len(rc.ConversionIssues) == 0 || rc.Messages[0].Blocks[0].Kind != domain.BlockOpaque {
		t.Fatalf("native-only Anthropic block: rc=%+v err=%v", rc, err)
	}
}

func TestBearerOfAndClientIP(t *testing.T) {
	r1, _ := http.NewRequest(http.MethodPost, "/", nil)
	r1.Header.Set("Authorization", "Bearer tok-1")
	if got := bearerOf(r1); got != "tok-1" {
		t.Errorf("bearer: %q", got)
	}
	r2, _ := http.NewRequest(http.MethodPost, "/", nil)
	r2.Header.Set("X-Api-Key", "tok-2")
	if got := bearerOf(r2); got != "tok-2" {
		t.Errorf("x-api-key: %q", got)
	}
	r3, _ := http.NewRequest(http.MethodPost, "/", nil)
	if got := bearerOf(r3); got != "" {
		t.Errorf("none: %q", got)
	}
	r4 := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := clientIP(r4); got == "" || strings.Contains(got, ":") {
		t.Errorf("clientIP must strip port: %q", got)
	}
}

func TestProtocolFeatureHeadersAreCapturedWithoutCredentials(t *testing.T) {
	eng := newStub()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req.Header.Set("Anthropic-Version", "2024-10-01")
	req.Header.Set("Anthropic-Beta", "tools-2025-01-01")
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	anH(eng).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := eng.gotContract.ProtocolHeaders["Anthropic-Beta"]; got != "tools-2025-01-01" {
		t.Fatalf("Anthropic-Beta=%q", got)
	}
	if _, leaked := eng.gotContract.ProtocolHeaders["Authorization"]; leaked {
		t.Fatal("client credential must never enter protocol headers")
	}
}

func TestCompatibilityWarningsPassThroughAsResponseHeaders(t *testing.T) {
	engine := newStub()
	engine.resp.Warnings = []string{
		"provider caching metadata omitted during protocol conversion",
		"Responses reasoning summary mapped to Chat reasoning_content",
	}
	recorder := postJSON(t, engine.chat(), "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("warning response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Values("X-SmartAPI-Warning"); len(got) != 2 || got[0] != engine.resp.Warnings[0] || got[1] != engine.resp.Warnings[1] {
		t.Fatalf("warning headers = %v", got)
	}
	if got := recorder.Header().Values("X-Ferry-Warning"); len(got) != 2 || got[0] != engine.resp.Warnings[0] || got[1] != engine.resp.Warnings[1] {
		t.Fatalf("warning headers = %v", got)
	}
}

func TestWriteNativeJSONResponseRejectsInvalidBodyAndNormalizesMediaType(t *testing.T) {
	invalid := httptest.NewRecorder()
	if writeNativeJSONResponse(invalid, []byte("not-json"), "text/html") {
		t.Fatal("invalid native JSON must be rejected before response commit")
	}
	if invalid.Code != http.StatusOK || invalid.Body.Len() != 0 {
		t.Fatalf("invalid response was committed: status=%d body=%q", invalid.Code, invalid.Body.String())
	}

	valid := httptest.NewRecorder()
	if !writeNativeJSONResponse(valid, []byte(`{"ok":true}`), "text/html") {
		t.Fatal("valid native JSON was rejected")
	}
	if valid.Header().Get("Content-Type") != "application/json" || valid.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("safe response headers = %v", valid.Header())
	}
}

func TestNativeResponsesPreserveUnknownFieldsAndPatchPublicIdentity(t *testing.T) {
	tests := []struct {
		name     string
		protocol domain.Protocol
		path     string
		request  string
		handler  func(*stubEngine) http.Handler
		body     string
	}{
		{"chat", domain.ProtocolOpenAIChat, "/v1/chat/completions", `{"model":"alias","messages":[{"role":"user","content":"hi"}]}`, func(e *stubEngine) http.Handler { return chatH(e) }, `{"id":"upstream","model":"actual","choices":[],"future_field":{"x":1}}`},
		{"responses", domain.ProtocolOpenAIResponses, "/v1/responses", `{"model":"alias","input":"hi"}`, func(e *stubEngine) http.Handler { return rsH(e) }, `{"id":"resp_upstream","object":"response","model":"actual","output":[],"future_field":{"x":1}}`},
		{"anthropic", domain.ProtocolAnthropic, "/v1/messages", `{"model":"alias","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`, func(e *stubEngine) http.Handler { return anH(e) }, `{"id":"msg_upstream","type":"message","model":"actual","content":[],"future_field":{"x":1}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng := newStub()
			eng.resp = &domain.Response{NativeProtocol: tc.protocol, NativeBody: []byte(tc.body),
				NativeContentType: "application/json"}
			rec := postJSON(t, tc.handler(eng), tc.path, tc.request)
			var got map[string]any
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got["id"] != "req-stub" || got["model"] != "glm-4-flash" || got["future_field"] == nil {
				t.Fatalf("patched native response=%v", got)
			}
		})
	}
}

func TestNativeStreamsPreserveRawFramesForAllTextProtocols(t *testing.T) {
	tests := []struct {
		name     string
		protocol domain.Protocol
		path     string
		request  string
		handler  func(*stubEngine) http.Handler
		frames   []domain.StreamEvent
		contains []string
	}{
		{"chat", domain.ProtocolOpenAIChat, "/v1/chat/completions", `{"model":"alias","messages":[{"role":"user","content":"hi"}],"stream":true}`, func(e *stubEngine) http.Handler { return chatH(e) }, []domain.StreamEvent{
			{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat, NativeFrame: &domain.NativeSSEFrame{Data: `{"id":"up","model":"actual","choices":[],"future":1}`}},
			{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat, NativeFrame: &domain.NativeSSEFrame{Data: "[DONE]", Terminal: true}}, {Kind: domain.EvEnd}}, []string{`"id":"req-stub"`, `"future":1`, "data: [DONE]"}},
		{"responses", domain.ProtocolOpenAIResponses, "/v1/responses", `{"model":"alias","input":"hi","stream":true}`, func(e *stubEngine) http.Handler { return rsH(e) }, []domain.StreamEvent{
			{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIResponses, NativeFrame: &domain.NativeSSEFrame{Event: "response.created", Data: `{"type":"response.created","response":{"id":"up","model":"actual","output":[],"future":1}}`}},
			{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIResponses, NativeFrame: &domain.NativeSSEFrame{Event: "response.completed", Data: `{"type":"response.completed","response":{"id":"up","model":"actual","output":[]}}`, Terminal: true}}, {Kind: domain.EvEnd}}, []string{"event: response.created", `"id":"req-stub"`, `"future":1`}},
		{"anthropic", domain.ProtocolAnthropic, "/v1/messages", `{"model":"alias","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":true}`, func(e *stubEngine) http.Handler { return anH(e) }, []domain.StreamEvent{
			{Kind: domain.EvNative, NativeProtocol: domain.ProtocolAnthropic, NativeFrame: &domain.NativeSSEFrame{Event: "message_start", Data: `{"type":"message_start","message":{"id":"up","model":"actual","content":[],"future":1}}`}},
			{Kind: domain.EvNative, NativeProtocol: domain.ProtocolAnthropic, NativeFrame: &domain.NativeSSEFrame{Event: "message_stop", Data: `{"type":"message_stop"}`, Terminal: true}}, {Kind: domain.EvEnd}}, []string{"event: message_start", `"id":"req-stub"`, `"future":1`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng := newStub()
			eng.evs = tc.frames
			rec := postJSON(t, tc.handler(eng), tc.path, tc.request)
			for _, want := range tc.contains {
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("stream missing %q:\n%s", want, rec.Body.String())
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// HTTP plumbing + non-streaming responses

func TestHealthzAndMethodGuard(t *testing.T) {
	old := buildinfo.Current()
	buildinfo.Version, buildinfo.Commit = "main-123456789abc", "123456789abcdef0123456789abcdef012345678"
	buildinfo.BuildTime, buildinfo.Source = "2026-08-23T01:02:03Z", "https://example.org/ferry/source"
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit = old.Version, old.Commit
		buildinfo.BuildTime, buildinfo.Source = old.BuildTime, old.Source
	})
	mux := http.NewServeMux()
	NewServer(newStub()).Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body.String())
	}
	for path, want := range map[string]string{
		"/health":  `"version":"main-123456789abc"`,
		"/version": `"commit":"123456789abcdef0123456789abcdef012345678"`,
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s: %d %s, want %s", path, rec.Code, rec.Body.String(), want)
		}
	}

	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/images/generations", "/v1/images/edits"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d want 405", path, rec.Code)
		}
	}
}

func TestOpenAIImagesGenerationIngressReturnsNativeResponse(t *testing.T) {
	engine := newStub()
	engine.resp = &domain.Response{NativeBody: []byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`), NativeContentType: "application/json; charset=utf-8"}
	rec := postJSON(t, engine.images(), "/v1/images/generations", `{"model":"gpt-image-2","prompt":"draw a lighthouse","size":"1024x1024"}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status=%d content-type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	rc := engine.gotContract
	if rc == nil || rc.Protocol != domain.ProtocolOpenAIImages || rc.Operation != "generation" || rc.Model != "gpt-image-2" || len(rc.Native) == 0 {
		t.Fatalf("generation contract=%+v", rc)
	}
	if rec.Body.String() != `{"data":[{"b64_json":"aW1hZ2U="}]}` {
		t.Fatalf("native response changed: %s", rec.Body.String())
	}
}

func TestOpenAIImagesEditIngressPreservesMultipartBody(t *testing.T) {
	engine := newStub()
	engine.resp = &domain.Response{NativeBody: []byte(`{"data":[{"url":"https://example.test/edit.png"}]}`), NativeContentType: "application/json"}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "gpt-image-2")
	_ = writer.WriteField("prompt", "make it blue")
	part, _ := writer.CreateFormFile("image", "input.png")
	_, _ = io.WriteString(part, "PNG-BYTES")
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	engine.images().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	rc := engine.gotContract
	if rc == nil || rc.Operation != "edit" || rc.Model != "gpt-image-2" || !strings.HasPrefix(rc.NativeContentType, "multipart/form-data;") || !bytes.Contains(rc.Native, []byte("PNG-BYTES")) {
		t.Fatalf("edit contract=%+v", rc)
	}
}

func TestModelsListsOnlyEnabledLogicalModels(t *testing.T) {
	eng := newStub()
	eng.snapshot = &domain.CompiledSnapshot{
		LogicalModels: map[string]*domain.LogicalModel{
			"z-disabled": {ID: "z-disabled", Enabled: false},
			"smart":      {ID: "smart", Enabled: true},
			"analysis":   {ID: "analysis", Enabled: true},
		},
		Providers: map[string]*domain.Provider{
			"private-provider": {ID: "private-provider", Enabled: true},
		},
	}
	mux := http.NewServeMux()
	NewServer(eng).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "list" || len(response.Data) != 2 ||
		response.Data[0].ID != "analysis" || response.Data[1].ID != "smart" {
		t.Fatalf("models response=%+v", response)
	}
	if body := rec.Body.String(); strings.Contains(body, "disabled") || strings.Contains(body, "private-provider") {
		t.Fatalf("models leaked disabled/provider resources: %s", body)
	}
}

func TestModelsUsesDataPlaneAuthentication(t *testing.T) {
	eng := newStub()
	eng.authenticated = false
	eng.snapshot = &domain.CompiledSnapshot{LogicalModels: map[string]*domain.LogicalModel{
		"smart": {ID: "smart", Enabled: true},
	}}
	rec := httptest.NewRecorder()
	NewServer(eng).Models.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Fatalf("unauthenticated models response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChatNonStreamingResponseShape(t *testing.T) {
	eng := newStub()
	eng.resp = &domain.Response{
		Content: []domain.Block{
			{Kind: domain.BlockThinking, Text: "think"},
			{Kind: domain.BlockText, Text: "answer"},
			{Kind: domain.BlockToolUse, ID: "tc9", Name: "get", Input: json.RawMessage(`{"a":1}`)},
		},
		FinishReason: "tool_call",
		Usage:        domain.Usage{InputTokens: 12, OutputTokens: 34, CacheReadTokens: 2, CacheSubsetOfInput: true},
	}
	rec := postJSON(t, eng.chat(), "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["object"] != "chat.completion" || m["model"] != "glm-4-flash" {
		t.Errorf("envelope: %v", m)
	}
	choices := m["choices"].([]any)
	c0 := choices[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "answer" {
		t.Errorf("message content: %v", msg)
	}
	if msg["reasoning_content"] != "think" {
		t.Errorf("reasoning_content: %v", msg)
	}
	tcs := msg["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != "tc9" || fn["name"] != "get" {
		t.Errorf("tool_calls: %v", tc)
	}
	var argsStr string
	var okArgs bool
	if argsStr, okArgs = fn["arguments"].(string); !okArgs || argsStr != `{"a":1}` {
		t.Errorf("arguments must be a JSON string wrapping the object: %v", fn["arguments"])
	}
	if c0["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason mapping tool_call→tool_calls: %v", c0["finish_reason"])
	}
	usage := m["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 12 || usage["completion_tokens"].(float64) != 34 ||
		usage["total_tokens"].(float64) != 46 {
		t.Errorf("usage: %v", usage)
	}
}

func TestResponsesNonStreamingShape(t *testing.T) {
	eng := newStub()
	eng.conversationID = "conv_smart_1"
	eng.resp = &domain.Response{
		Content: []domain.Block{
			{Kind: domain.BlockThinking, Text: "ponder"},
			{Kind: domain.BlockText, Text: "final"},
		},
		FinishReason: "length",
		Usage:        domain.Usage{InputTokens: 8, OutputTokens: 16, ReasoningTokens: 4, CacheReadTokens: 3, CacheSubsetOfInput: true},
	}
	rec := postJSON(t, eng.responses(), "/v1/responses", `{"model":"m","input":"hi"}`)
	m := decode(t, rec)
	if m["object"] != "response" || m["status"] != "incomplete" {
		t.Errorf("envelope/status: %v", m)
	}
	if conversation := m["conversation"].(map[string]any); conversation["id"] != "conv_smart_1" {
		t.Errorf("virtual conversation: %v", conversation)
	}
	if d := m["incomplete_details"]; d == nil || d.(map[string]any)["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details: %v", m["incomplete_details"])
	}
	items := m["output"].([]any)
	first := items[0].(map[string]any)
	if first["type"] != "reasoning" || first["id"] != "req-stub-rs" {
		t.Errorf("reasoning item first: %v", first)
	}
	reasoningParts := first["content"].([]any)
	if reasoningParts[0].(map[string]any)["type"] != "reasoning_text" ||
		reasoningParts[0].(map[string]any)["text"] != "ponder" {
		t.Errorf("reasoning content was dropped: %v", first)
	}
	last := items[len(items)-1].(map[string]any)
	if last["type"] != "message" || last["role"] != "assistant" {
		t.Errorf("message item: %v", last)
	}
	parts := last["content"].([]any)
	if parts[0].(map[string]any)["text"] != "final" {
		t.Errorf("output_text part: %v", parts)
	}
	u := m["usage"].(map[string]any)
	if u["total_tokens"].(float64) != 24 || u["output_tokens"].(float64) != 16 {
		t.Errorf("usage: %v", u)
	}
	inDetails := u["input_tokens_details"].(map[string]any)
	outDetails := u["output_tokens_details"].(map[string]any)
	if inDetails["cached_tokens"].(float64) != 3 || outDetails["reasoning_tokens"].(float64) != 4 {
		t.Errorf("usage details: %v %v", inDetails, outDetails)
	}
}

func TestAnthropicNonStreamingShape(t *testing.T) {
	eng := newStub()
	eng.resp = &domain.Response{
		Content: []domain.Block{
			{Kind: domain.BlockText, Text: "hey"},
			{Kind: domain.BlockToolUse, ID: "tu2", Name: "act", Input: json.RawMessage(`{}`)},
		},
		FinishReason: "length",
		Usage:        domain.Usage{InputTokens: 7, OutputTokens: 9, CacheWriteTokens: 5, CacheReadTokens: 6},
	}
	rec := postJSON(t, eng.messages(), "/v1/messages", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	m := decode(t, rec)
	if m["type"] != "message" || m["role"] != "assistant" || m["model"] != "glm-4-flash" {
		t.Errorf("envelope: %v", m)
	}
	blocks := m["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "text" {
		t.Errorf("text block: %v", blocks[0])
	}
	tu := blocks[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "tu2" || tu["name"] != "act" {
		t.Errorf("tool_use block: %v", tu)
	}
	if tu["input"] != "{}" && fmt.Sprint(tu["input"]) != "map[]" {
		t.Errorf("empty object input changed: %v", tu["input"])
	}
	if m["stop_reason"] != "max_tokens" {
		t.Errorf("length→max_tokens: %v", m["stop_reason"])
	}
	u := m["usage"].(map[string]any)
	if u["cache_creation_input_tokens"].(float64) != 5 || u["cache_read_input_tokens"].(float64) != 6 {
		t.Errorf("cache usage: %v", u)
	}
}

// ---------------------------------------------------------------------------
// streaming shapes

func sseFrames(rec *httptest.ResponseRecorder) []string {
	raw := rec.Body.String()
	parts := strings.Split(raw, "\n\n")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dataOf(frame string) string {
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	return ""
}

func TestChatStreamFraming(t *testing.T) {
	eng := newStub()
	eng.evs = []domain.StreamEvent{
		{Kind: domain.EvTextDelta, TextDelta: "he"},
		{Kind: domain.EvTextDelta, TextDelta: "y"},
		{Kind: domain.EvEnd, FinishReason: "stop",
			Usage: &domain.Usage{InputTokens: 3, OutputTokens: 2}},
	}
	rec := postJSON(t, eng.chat(), "/v1/chat/completions",
		`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type: %s", ct)
	}
	frames := sseFrames(rec)
	// role chunk, 2 deltas, finish chunk, usage chunk, [DONE]
	if len(frames) != 6 {
		t.Fatalf("frames=%d:\n%s", len(frames), rec.Body.String())
	}
	first := dataOf(frames[0])
	if !strings.Contains(first, `"role":"assistant"`) {
		t.Errorf("first chunk must carry role: %s", first)
	}
	if !strings.Contains(dataOf(frames[1]), `"content":"he"`) ||
		!strings.Contains(dataOf(frames[2]), `"content":"y"`) {
		t.Errorf("delta chunks: %s | %s", frames[1], frames[2])
	}
	fin := dataOf(frames[3])
	if !strings.Contains(fin, `"finish_reason":"stop"`) || strings.Contains(fin, `"usage"`) {
		t.Errorf("terminal chunk: %s", fin)
	}
	usage := dataOf(frames[4])
	if !strings.Contains(usage, `"choices":[]`) ||
		!strings.Contains(usage, `"prompt_tokens":3`) || !strings.Contains(usage, `"completion_tokens":2`) {
		t.Errorf("usage chunk: %s", usage)
	}
	if frames[5] != "data: [DONE]" {
		t.Errorf("terminator: %q", frames[5])
	}
}

func TestChatStreamOmitsUsageUnlessRequested(t *testing.T) {
	eng := newStub()
	eng.evs = []domain.StreamEvent{
		{Kind: domain.EvTextDelta, TextDelta: "ok"},
		{Kind: domain.EvEnd, FinishReason: "stop",
			Usage: &domain.Usage{InputTokens: 3, OutputTokens: 2}},
	}
	rec := postJSON(t, eng.chat(), "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("usage leaked without stream_options.include_usage: %s", rec.Body.String())
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	eng := newStub()
	eng.conversationID = "conv_smart_stream"
	eng.evs = []domain.StreamEvent{
		{Kind: domain.EvTextDelta, TextDelta: "ok"},
		{Kind: domain.EvEnd, FinishReason: "stop", Usage: &domain.Usage{OutputTokens: 1}},
	}
	rec := postJSON(t, eng.responses(), "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`)
	frames := sseFrames(rec)
	types := make([]string, 0, len(frames))
	for _, f := range frames {
		var ev struct {
			Type string `json:"type"`
			Seq  int64  `json:"sequence_number"`
		}
		if err := json.Unmarshal([]byte(dataOf(f)), &ev); err != nil {
			t.Fatalf("frame %q: %v", f, err)
		}
		types = append(types, ev.Type)
	}
	want := []string{
		"response.output_item.added",  // message opens
		"response.content_part.added", // text part opens once
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(types) != len(want) {
		t.Fatalf("events=%v want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("event[%d]=%s want %s", i, types[i], want[i])
		}
	}
	// sequence numbers strictly increase
	prev := int64(0)
	for _, f := range frames {
		var ev struct {
			Seq int64 `json:"sequence_number"`
		}
		json.Unmarshal([]byte(dataOf(f)), &ev)
		if ev.Seq <= prev {
			t.Errorf("sequence_number not increasing at %q (%d after %d)", f, ev.Seq, prev)
		}
		prev = ev.Seq
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("Responses API must NOT emit data: [DONE]")
	}
	final := decodeMap(t, dataOf(frames[len(frames)-1]))
	respObj := final["response"].(map[string]any)
	if respObj["status"] != "completed" {
		t.Errorf("final response status: %v", respObj["status"])
	}
	if conversation := respObj["conversation"].(map[string]any); conversation["id"] != "conv_smart_stream" {
		t.Errorf("virtual conversation: %v", conversation)
	}
	output := respObj["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["type"] != "message" {
		t.Errorf("terminal response omitted streamed output: %v", output)
	}
}

func TestResponsesStreamKeepsParallelToolsDistinct(t *testing.T) {
	eng := newStub()
	eng.evs = []domain.StreamEvent{
		{Kind: domain.EvToolStart, Index: 4, ToolID: "call-a", ToolName: "alpha"},
		{Kind: domain.EvToolStart, Index: 9, ToolID: "call-b", ToolName: "beta"},
		{Kind: domain.EvToolDelta, Index: 9, ArgsDelta: `{"b":2}`},
		{Kind: domain.EvToolDelta, Index: 4, ArgsDelta: `{"a":1}`},
		{Kind: domain.EvToolEnd, Index: 9},
		{Kind: domain.EvToolEnd, Index: 4},
		{Kind: domain.EvEnd, FinishReason: "tool_call"},
	}
	rec := postJSON(t, eng.responses(), "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`)
	frames := sseFrames(rec)
	added := map[string]float64{}
	for _, frame := range frames {
		var event map[string]any
		if json.Unmarshal([]byte(dataOf(frame)), &event) != nil || event["type"] != "response.output_item.added" {
			continue
		}
		item := event["item"].(map[string]any)
		if item["type"] == "function_call" {
			added[item["id"].(string)] = event["output_index"].(float64)
		}
	}
	if len(added) != 2 {
		t.Fatalf("parallel tools reused an item identity: %v\n%s", added, rec.Body.String())
	}
	final := decodeMap(t, dataOf(frames[len(frames)-1]))["response"].(map[string]any)
	output := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("terminal response lost parallel tools: %v", output)
	}
	seen := map[string]string{}
	for _, raw := range output {
		item := raw.(map[string]any)
		seen[item["call_id"].(string)] = item["arguments"].(string)
	}
	if seen["call-a"] != `{"a":1}` || seen["call-b"] != `{"b":2}` {
		t.Fatalf("parallel tool arguments crossed: %v", seen)
	}
}

func TestAnthropicStreamBuffersParallelToolsIntoValidBlocks(t *testing.T) {
	eng := newStub()
	eng.evs = []domain.StreamEvent{
		{Kind: domain.EvToolStart, Index: 4, ToolID: "call-a", ToolName: "alpha"},
		{Kind: domain.EvToolStart, Index: 9, ToolID: "call-b", ToolName: "beta"},
		{Kind: domain.EvToolDelta, Index: 9, ArgsDelta: `{"b":2}`},
		{Kind: domain.EvToolDelta, Index: 4, ArgsDelta: `{"a":1}`},
		{Kind: domain.EvToolEnd, Index: 9},
		{Kind: domain.EvToolEnd, Index: 4},
		{Kind: domain.EvEnd, FinishReason: "tool_call"},
	}
	rec := postJSON(t, eng.messages(), "/v1/messages",
		`{"model":"m","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	type blockFrame struct {
		typeName string
		index    int
		id       string
		args     string
	}
	var blocks []blockFrame
	for _, frame := range sseFrames(rec) {
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(dataOf(frame)), &event) != nil {
			continue
		}
		if event.Type == "content_block_start" && event.Block != nil && event.Block.Type == "tool_use" {
			blocks = append(blocks, blockFrame{typeName: "start", index: event.Index, id: event.Block.ID})
		}
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "input_json_delta" {
			blocks = append(blocks, blockFrame{typeName: "delta", index: event.Index, args: event.Delta.PartialJSON})
		}
		if event.Type == "content_block_stop" {
			blocks = append(blocks, blockFrame{typeName: "stop", index: event.Index})
		}
	}
	if len(blocks) != 6 || blocks[0].typeName != "start" || blocks[0].id != "call-a" ||
		blocks[1].typeName != "delta" || blocks[1].index != blocks[0].index || blocks[1].args != `{"a":1}` ||
		blocks[2].typeName != "stop" || blocks[2].index != blocks[0].index ||
		blocks[3].typeName != "start" || blocks[3].id != "call-b" ||
		blocks[4].typeName != "delta" || blocks[4].index != blocks[3].index || blocks[4].args != `{"b":2}` ||
		blocks[5].typeName != "stop" || blocks[5].index != blocks[3].index {
		t.Fatalf("parallel tools produced interleaved Anthropic blocks: %+v\n%s", blocks, rec.Body.String())
	}
}

func TestAnthropicStreamRejectsContentInterleavedWithUnfinishedTools(t *testing.T) {
	eng := newStub()
	eng.evs = []domain.StreamEvent{
		{Kind: domain.EvToolStart, Index: 4, ToolID: "call-a", ToolName: "alpha"},
		{Kind: domain.EvToolDelta, Index: 4, ArgsDelta: `{"a":`},
		{Kind: domain.EvTextDelta, Index: 5, TextDelta: "must not jump ahead"},
	}
	rec := postJSON(t, eng.messages(), "/v1/messages",
		`{"model":"m","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	if !strings.Contains(body, string(domain.FailProtocolConversion)) ||
		!strings.Contains(body, "unfinished parallel tool calls") {
		t.Fatalf("interleaved tool/content stream did not fail closed: %s", body)
	}
	if strings.Contains(body, "must not jump ahead") {
		t.Fatalf("content was emitted ahead of an unfinished tool block: %s", body)
	}
}

func decodeMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return m
}

func TestAnthropicStreamFraming(t *testing.T) {
	eng := newStub()
	eng.evs = []domain.StreamEvent{
		{Kind: domain.EvThinkingDelta, Index: 0, ThinkingDelta: "hmm"},
		{Kind: domain.EvSignatureDelta, Index: 0, SignatureDelta: "sig-1"},
		{Kind: domain.EvToolStart, Index: 1, ToolID: "t9", ToolName: "get"},
		{Kind: domain.EvToolDelta, Index: 1, ArgsDelta: `{"a"`},
		{Kind: domain.EvToolEnd, Index: 1},
		{Kind: domain.EvEnd, FinishReason: "tool_call",
			Usage: &domain.Usage{OutputTokens: 11}},
	}
	rec := postJSON(t, eng.messages(), "/v1/messages",
		`{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	frames := sseFrames(rec)
	names := make([]string, 0, len(frames))
	for _, f := range frames {
		lines := strings.Split(f, "\n")
		name := ""
		data := ""
		for _, l := range lines {
			if strings.HasPrefix(l, "event: ") {
				name = strings.TrimPrefix(l, "event: ")
			}
			if strings.HasPrefix(l, "data: ") {
				data = strings.TrimPrefix(l, "data: ")
			}
		}
		if name == "" || data == "" {
			t.Fatalf("malformed frame %q", f)
		}
		names = append(names, name)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "content_block_start",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(names) != len(want) {
		t.Fatalf("events=%v want %v\n%s", names, want, rec.Body.String())
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("event[%d]=%s want %s", i, names[i], want[i])
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"thinking_delta"`) ||
		!strings.Contains(body, `"type":"signature_delta"`) ||
		!strings.Contains(body, `"partial_json":"{\"a\""`) {
		t.Errorf("delta payloads: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) ||
		!strings.Contains(body, `"output_tokens":11`) {
		t.Errorf("message_delta payload: %s", body)
	}
	// message_start must be the very first frame
	if names[0] != "message_start" {
		t.Errorf("message_start must come first, got %s", names[0])
	}
}

func TestStreamErrorTravelsInBand(t *testing.T) {
	chatErr := newStub()
	chatErr.evs = []domain.StreamEvent{
		{Kind: domain.EvError, Err: &domain.Failure{Type: domain.FailRateLimit, Message: "429 upstream"}},
	}
	rec := postJSON(t, chatErr.chat(), "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("committed stream errors ride a 200 SSE channel: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `event: error`) ||
		!strings.Contains(rec.Body.String(), `"rate_limit"`) {
		t.Errorf("chat error event: %s", rec.Body.String())
	}
	if !strings.HasSuffix(strings.TrimSpace(rec.Body.String()), "[DONE]") {
		t.Errorf("chat error must be followed by [DONE]: %s", rec.Body.String())
	}

	anErr := newStub()
	anErr.evs = []domain.StreamEvent{
		{Kind: domain.EvError, Err: &domain.Failure{Type: domain.FailRateLimit, Message: "429 upstream"}},
	}
	rec = postJSON(t, anErr.messages(), "/v1/messages", `{"model":"m","max_tokens":1,"stream":true,"messages":[{"role":"u","content":"x"}]}`)
	if !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
		t.Errorf("anthropic error event: %s", rec.Body.String())
	}

	rsErr := newStub()
	rsErr.evs = []domain.StreamEvent{
		{Kind: domain.EvError, Err: &domain.Failure{Type: domain.FailProvider5xx, Message: "boom"}},
	}
	rec = postJSON(t, rsErr.responses(), "/v1/responses", `{"model":"m","stream":true,"input":"x"}`)
	if !strings.Contains(rec.Body.String(), `"type":"response.failed"`) ||
		!strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Errorf("responses error event: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"type":"response.error"`) {
		t.Errorf("responses error used a non-terminal event type: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// failure mapping

func TestFailureStatusMapping(t *testing.T) {
	cases := []struct {
		ft     domain.FailureType
		status int
	}{
		{domain.FailAuth, 401},
		{domain.FailRateLimit, 429},
		{domain.FailQuotaExhausted, 429},
		{domain.FailUnsupportedParam, 422},
		{domain.FailContextOverflow, 413},
		{domain.FailClientCancel, 499}, // wire handlers rewrite this to 408
		{domain.FailConnect, 502},
		{domain.FailProvider5xx, 502},
		{domain.FailProtocolConversion, 502},
		{domain.FailBadResponse, 502},
	}
	for _, tc := range cases {
		if got := failureStatus(&domain.Failure{Type: tc.ft}); got != tc.status {
			t.Errorf("%s → %d want %d", tc.ft, got, tc.status)
		}
	}
	// explicit upstream HTTP status wins over the default table
	if got := failureStatus(&domain.Failure{Type: domain.FailBadResponse, HTTPStatus: 503}); got != 503 {
		t.Errorf("HTTPStatus override: %d", got)
	}
}

func TestFailureWritesRetryAfterHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeFailure(recorder, &domain.Failure{Type: domain.FailConnect, HTTPStatus: 503, Message: "temporarily unavailable", RetryAfter: 2500 * time.Millisecond})
	if got := recorder.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After=%q want 3", got)
	}
}

func TestExecuteFailureWireShapes(t *testing.T) {
	eng := newStub()
	eng.fail = &domain.Failure{Type: domain.FailAuth, Message: "bad key"}

	rec := postJSON(t, eng.chat(), "/v1/chat/completions", `{"model":"m","messages":[{"role":"u","content":"x"}]}`)
	m := decode(t, rec)
	errObj := sub(t, m, "error")
	if rec.Code != 401 || errObj["type"] != "authentication_error" {
		t.Errorf("chat auth failure: %d %v", rec.Code, errObj)
	}

	rec = postJSON(t, eng.responses(), "/v1/responses", `{"model":"m","input":"x"}`)
	if rec.Code != 401 {
		t.Errorf("responses failure status: %d", rec.Code)
	}

	rec = postJSON(t, eng.messages(), "/v1/messages", `{"model":"m","max_tokens":1,"messages":[{"role":"u","content":"x"}]}`)
	m = decode(t, rec)
	errAn := sub(t, m, "error")
	if rec.Code != 401 || m["type"] != "error" || errAn["type"] != "authentication_error" {
		t.Errorf("anthropic auth failure: %d %v", rec.Code, m)
	}

	// cancel → 408 with protocol-specific type spelling
	eng.fail = &domain.Failure{Type: domain.FailClientCancel, Message: "client canceled"}
	rec = postJSON(t, eng.chat(), "/v1/chat/completions", `{"model":"m","messages":[{"role":"u","content":"x"}]}`)
	if rec.Code != 408 || !strings.Contains(rec.Body.String(), `"request_canceled"`) {
		t.Errorf("cancel mapping chat: %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, eng.messages(), "/v1/messages", `{"model":"m","max_tokens":1,"messages":[{"role":"u","content":"x"}]}`)
	if rec.Code != 408 || !strings.Contains(rec.Body.String(), `"request_too_long"`) {
		t.Errorf("cancel mapping anthropic: %d %s", rec.Code, rec.Body.String())
	}

	// quota exhaustion → 429 rate_limit_error
	eng.fail = &domain.Failure{Type: domain.FailQuotaExhausted, Message: "bucket empty"}
	rec = postJSON(t, eng.chat(), "/v1/chat/completions", `{"model":"m","messages":[{"role":"u","content":"x"}]}`)
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
		t.Errorf("quota mapping: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBodyAndParseGuards(t *testing.T) {
	eng := newStub()
	empty := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	rec := httptest.NewRecorder()
	eng.chat().ServeHTTP(rec, empty)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "empty request body") {
		t.Errorf("empty body: %d %s", rec.Code, rec.Body.String())
	}

	rec = postJSON(t, eng.chat(), "/v1/chat/completions", `{broken`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid JSON body") {
		t.Errorf("bad json: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// metadata pass-through into Execute

func TestMetaPassedThrough(t *testing.T) {
	eng := newStub()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.RemoteAddr = "10.0.0.7:55555"
	rec := httptest.NewRecorder()
	eng.chat().ServeHTTP(rec, req)

	if eng.gotMeta.PresentedCredential != "sk-test" {
		t.Errorf("presented credential: %q", eng.gotMeta.PresentedCredential)
	}
	if eng.gotMeta.ClientIP != "10.0.0.7" {
		t.Errorf("client ip: %q", eng.gotMeta.ClientIP)
	}
	if eng.gotContract == nil || eng.gotContract.Model != "m" {
		t.Errorf("contract: %+v", eng.gotContract)
	}
	if eng.gotContract.Protocol != domain.ProtocolOpenAIChat {
		t.Errorf("protocol stamped by ingress: %s", eng.gotContract.Protocol)
	}
}

func TestStandardLineageAndKnownClientEvidenceAreTypedAndHashed(t *testing.T) {
	eng := newStub()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"m","input":"hi","conversation":{"id":"conv-client-1"},
		"prompt_cache_key":"cache-1"
	}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Codex-Thread-Id", "thread-1")
	rec := httptest.NewRecorder()
	eng.responses().ServeHTTP(rec, req)
	if eng.gotContract == nil || eng.gotContract.Lineage == nil ||
		eng.gotContract.Lineage.ConversationID != "conv-client-1" ||
		eng.gotContract.Lineage.PromptCacheKey != "cache-1" {
		t.Fatalf("lineage = %+v", eng.gotContract)
	}
	if len(eng.gotMeta.AffinityEvidence) != 3 {
		t.Fatalf("evidence = %+v", eng.gotMeta.AffinityEvidence)
	}
	wantScopes := map[domain.AffinityScope]int{
		domain.AffinityConversation: 2,
		domain.AffinityCache:        1,
	}
	for _, evidence := range eng.gotMeta.AffinityEvidence {
		wantScopes[evidence.Scope]--
		for _, raw := range []string{"conv-client-1", "cache-1", "thread-1"} {
			if evidence.KeyHash == raw {
				t.Fatalf("raw affinity value leaked: %+v", evidence)
			}
		}
	}
	for scope, remaining := range wantScopes {
		if remaining != 0 {
			t.Fatalf("scope %s count delta = %d", scope, remaining)
		}
	}
}

func TestResponsesRejectsConversationAndPreviousResponseTogether(t *testing.T) {
	eng := newStub()
	rec := postJSON(t, eng.responses(), "/v1/responses", `{
		"model":"m","input":"hi","conversation":"conv_smart_1",
		"previous_response_id":"resp_smart_1"
	}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "mutually exclusive") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if eng.gotContract != nil {
		t.Fatalf("invalid lineage reached scheduler: %+v", eng.gotContract)
	}
}

func TestKnownClientAffinityHeadersUseDistinctScopes(t *testing.T) {
	eng := newStub()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	for header, value := range map[string]string{
		"X-Claude-Code-Session-Id": "claude", "Thread-Id": "thread",
		"Session-Id": "codex-session", "X-Hermes-Session-Id": "hermes-id",
		"X-Hermes-Session-Key": "hermes-key", "X-OpenClaw-Session-Key": "openclaw-key",
	} {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	eng.chat().ServeHTTP(rec, req)
	want := map[string]domain.AffinityScope{
		"claude-code.session":  domain.AffinityConversation,
		"codex.thread":         domain.AffinityConversation,
		"codex.session":        domain.AffinityCache,
		"hermes.session_id":    domain.AffinityConversation,
		"hermes.session_key":   domain.AffinityCache,
		"openclaw.session_key": domain.AffinityConversation,
	}
	if len(eng.gotMeta.AffinityEvidence) != len(want) {
		t.Fatalf("evidence = %+v", eng.gotMeta.AffinityEvidence)
	}
	for _, evidence := range eng.gotMeta.AffinityEvidence {
		if evidence.Scope != want[evidence.Source] {
			t.Fatalf("source %s scope=%s want=%s", evidence.Source, evidence.Scope, want[evidence.Source])
		}
		delete(want, evidence.Source)
	}
	if len(want) != 0 {
		t.Fatalf("missing sources: %v", want)
	}
}

func TestChatPromptCacheAndAnthropicUserEvidenceDoNotBecomeConversation(t *testing.T) {
	chat := newStub()
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"cache-chat"}`))
	chatRec := httptest.NewRecorder()
	chat.chat().ServeHTTP(chatRec, chatReq)
	if chat.gotContract.Lineage == nil || chat.gotContract.Lineage.PromptCacheKey != "cache-chat" ||
		len(chat.gotMeta.AffinityEvidence) != 1 || chat.gotMeta.AffinityEvidence[0].Scope != domain.AffinityCache {
		t.Fatalf("chat affinity = contract:%+v meta:%+v", chat.gotContract, chat.gotMeta)
	}

	anthropic := newStub()
	anReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}],
		"metadata":{"user_id":"user-1"}
	}`))
	anRec := httptest.NewRecorder()
	anthropic.messages().ServeHTTP(anRec, anReq)
	if len(anthropic.gotMeta.AffinityEvidence) != 1 ||
		anthropic.gotMeta.AffinityEvidence[0].Scope != domain.AffinityCaller {
		t.Fatalf("anthropic user evidence = %+v", anthropic.gotMeta.AffinityEvidence)
	}
}

func TestAnthropicAuxiliaryFieldsMapInsteadOfBlocking(t *testing.T) {
	rc, err := (&AnthropicHandler{}).toContract([]byte(`{
		"model":"claude-x","max_tokens":1024,
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"output_config":{"text":{"verbosity":"low"}},
		"system":[{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"name":"probe","description":"probe","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]
	}`))
	if err != nil {
		t.Fatalf("parse anthropic aux body: %v", err)
	}
	if len(rc.ConversionIssues) != 0 {
		t.Fatalf("auxiliary anthropic fields must not block conversion: %v", rc.ConversionIssues)
	}
	if rc.Verbosity != "low" {
		t.Fatalf("verbosity=%q", rc.Verbosity)
	}
	joined := strings.Join(rc.MappingNotes, "\n")
	for _, want := range []string{"cache_control omitted", "context_management edits omitted"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mapping notes missing %q: %v", want, rc.MappingNotes)
		}
	}
}

func TestChatAuxiliaryFieldsMapInsteadOfBlocking(t *testing.T) {
	rc, err := (&OpenAIChatHandler{}).toContract([]byte(`{
		"model":"gpt-x","verbosity":"medium","seed":7,"logprobs":false,"top_logprobs":2,
		"user":"u-1","store":false,"frequency_penalty":0.1,"presence_penalty":0.2,
		"logit_bias":{"50256":-100},"metadata":{"a":"b"},"service_tier":"auto",
		"prediction":{"type":"content","content":"draft"},
		"web_search_options":{},"messages":[{"role":"developer","content":"be brief"}]
	}`))
	if err != nil {
		t.Fatalf("parse chat aux body: %v", err)
	}
	if len(rc.ConversionIssues) != 0 {
		t.Fatalf("auxiliary chat fields must not block conversion: %v", rc.ConversionIssues)
	}
	if rc.Verbosity != "medium" {
		t.Fatalf("verbosity=%q", rc.Verbosity)
	}
	joined := strings.Join(rc.MappingNotes, "\n")
	for _, want := range []string{
		"developer role mapped to system", "seed omitted", "logprobs request omitted",
		"client identity metadata omitted", "provider-side request options omitted",
		"frequency/presence penalties omitted", "logit_bias omitted",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mapping notes missing %q: %v", want, rc.MappingNotes)
		}
	}
}

func TestResponsesAuxiliaryFieldsMapInsteadOfBlocking(t *testing.T) {
	rc, err := (&ResponsesHandler{}).toContract([]byte(`{
		"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"text":{"verbosity":"high"},"include":["reasoning.encrypted_content"],"truncation":"auto"
	}`))
	if err != nil {
		t.Fatalf("parse responses aux body: %v", err)
	}
	if len(rc.ConversionIssues) != 0 {
		t.Fatalf("auxiliary responses fields must not block conversion: %v", rc.ConversionIssues)
	}
	if rc.Verbosity != "high" {
		t.Fatalf("verbosity=%q", rc.Verbosity)
	}
	joined := strings.Join(rc.MappingNotes, "\n")
	for _, want := range []string{"include extras omitted", "truncation strategy omitted"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mapping notes missing %q: %v", want, rc.MappingNotes)
		}
	}
}
