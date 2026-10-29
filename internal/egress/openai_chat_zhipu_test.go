package egress

import (
	"encoding/json"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func zhipuChatForTest(endpoint string) *OpenAIChat {
	return NewOpenAIChat(Base{
		Endpoint: &domain.Endpoint{BaseURL: endpoint, Protocol: domain.ProtocolOpenAIChat},
		Model:    "glm-5.3-flash",
	})
}

func zhipuNativeContract(body string, stream bool) *domain.RequestContract {
	return &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIChat,
		Origin:   domain.ProtocolOpenAIChat,
		Stream:   stream,
		Native:   json.RawMessage(body),
	}
}

func TestZhipuChatNormalizesOpenAIAgentPayload(t *testing.T) {
	native := `{
		"model":"logical-alias",
		"messages":[
			{"role":"developer","content":"You are a coding agent."},
			{"role":"user","content":[{"type":"text","text":"debug this"}]}
		],
		"max_completion_tokens":16384,
		"reasoning_effort":"medium",
		"store":false,
		"stream":true,
		"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"shell","description":"run a command","parameters":{"type":"object"},"strict":false}}]
	}`
	caller := zhipuChatForTest("https://open.bigmodel.cn/api/coding/paas/v4")
	body, notes, err := caller.buildRequest(zhipuNativeContract(native, true))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("documented compatibility mappings must be lossless, got notes %v", notes)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	for _, removed := range []string{"max_completion_tokens", "store", "stream_options", "prompt_cache_key"} {
		if _, exists := got[removed]; exists {
			t.Errorf("%s should have been removed: %s", removed, body)
		}
	}
	var model, effort string
	var maxTokens int64
	_ = json.Unmarshal(got["model"], &model)
	_ = json.Unmarshal(got["reasoning_effort"], &effort)
	_ = json.Unmarshal(got["max_tokens"], &maxTokens)
	if model != "glm-5.3-flash" || effort != "high" || maxTokens != 16384 {
		t.Fatalf("unexpected aliases: model=%q effort=%q max_tokens=%d body=%s", model, effort, maxTokens, body)
	}

	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(got["messages"], &messages); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	var userText string
	_ = json.Unmarshal(messages[1].Content, &userText)
	if len(messages) != 2 || messages[0].Role != "system" || userText != "debug this" {
		t.Fatalf("unexpected normalized messages: %+v", messages)
	}

	var tools []map[string]json.RawMessage
	_ = json.Unmarshal(got["tools"], &tools)
	var function map[string]json.RawMessage
	_ = json.Unmarshal(tools[0]["function"], &function)
	if _, exists := function["strict"]; exists {
		t.Fatalf("strict=false should be removed: %s", got["tools"])
	}
}

func TestZhipuChatPreservesOfficialExtensionsAndMapsAliases(t *testing.T) {
	native := `{
		"model":"alias",
		"messages":[{"role":"user","content":"hello"}],
		"stream":false,
		"request_id":"request-123",
		"user":"user-123",
		"do_sample":false,
		"thinking":{"type":"enabled"},
		"tool_stream":true,
		"stop":"END",
		"n":1,
		"frequency_penalty":0,
		"presence_penalty":0,
		"logprobs":false,
		"modalities":["text"]
	}`
	body, _, err := zhipuChatForTest("https://api.z.ai/api/coding/paas/v4").buildRequest(
		zhipuNativeContract(native, false),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(body, &got)
	for _, removed := range []string{"user", "n", "frequency_penalty", "presence_penalty", "logprobs", "modalities"} {
		if _, exists := got[removed]; exists {
			t.Errorf("default OpenAI field %s should be removed", removed)
		}
	}
	var userID string
	var stop []string
	_ = json.Unmarshal(got["user_id"], &userID)
	_ = json.Unmarshal(got["stop"], &stop)
	if userID != "user-123" || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("alias mapping failed: user_id=%q stop=%v body=%s", userID, stop, body)
	}
	for _, preserved := range []string{"request_id", "do_sample", "thinking", "tool_stream"} {
		if _, exists := got[preserved]; !exists {
			t.Errorf("official Zhipu field %s was lost", preserved)
		}
	}
}

func TestZhipuChatRejectsOpenAIFeaturesWithoutEquivalent(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"stored completion", `{"model":"m","messages":[{"role":"user","content":"x"}],"store":true}`, "store=true"},
		{"strict tool", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"},"strict":true}}]}`, "strict=true"},
		{"required tool", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[],"tool_choice":"required"}`, "tool_choice=\"required\""},
		{"json schema", `{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`, "json_schema"},
		{"parallel tools", `{"model":"m","messages":[{"role":"user","content":"x"}],"parallel_tool_calls":true}`, "parallel_tool_calls=true"},
		{"seed", `{"model":"m","messages":[{"role":"user","content":"x"}],"seed":1}`, "field seed"},
		{"usage suppression", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream_options":{"include_usage":false}}`, "include_usage=false"},
		{"mixed priorities", `{"model":"m","messages":[{"role":"system","content":"a"},{"role":"developer","content":"b"},{"role":"user","content":"x"}]}`, "cannot both be represented"},
		{"conflicting token limits", `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":8,"max_completion_tokens":9}`, "disagree"},
		{"undocumented field", `{"model":"m","messages":[{"role":"user","content":"x"}],"future_magic":true}`, "not documented"},
	}
	caller := zhipuChatForTest("https://open.bigmodel.cn/api/paas/v4")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := caller.buildRequest(zhipuNativeContract(test.body, false))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestZhipuChatCanonicalEncodingUsesDocumentedDialect(t *testing.T) {
	serial := false
	rc := chatRC()
	rc.Stream = true
	rc.Reasoning = &domain.ReasoningSpec{Effort: "medium"}
	rc.Lineage = &domain.ResponsesLineage{PromptCacheKey: "cache-affinity"}
	rc.ParallelTools = &serial
	rc.StopSequences = []string{"END"}
	body, notes, err := zhipuChatForTest("https://open.bigmodel.cn/api/coding/paas/v4").buildRequest(rc)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected compatibility notes: %v", notes)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(body, &got)
	for _, absent := range []string{"stream_options", "prompt_cache_key", "parallel_tool_calls"} {
		if _, exists := got[absent]; exists {
			t.Errorf("canonical Zhipu request should omit %s: %s", absent, body)
		}
	}
	var effort string
	var stop []string
	_ = json.Unmarshal(got["reasoning_effort"], &effort)
	_ = json.Unmarshal(got["stop"], &stop)
	if effort != "high" || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("unexpected canonical mapping: effort=%q stop=%v body=%s", effort, stop, body)
	}
}

func TestZhipuChatNormalizationIsScopedToOfficialEndpoints(t *testing.T) {
	native := `{"model":"alias","messages":[{"role":"developer","content":"x"}],"store":false,"stream_options":{"include_usage":true}}`
	rc := zhipuNativeContract(native, false)
	caller := NewOpenAIChat(Base{
		Endpoint: &domain.Endpoint{BaseURL: "https://compatible.example/api/coding/paas/v4"},
		Model:    "upstream-model",
	})
	body, _, err := caller.buildRequest(rc)
	if err != nil {
		t.Fatalf("generic passthrough: %v", err)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(body, &got)
	if _, exists := got["store"]; !exists {
		t.Fatal("generic provider lost its native store field")
	}
	var messages []map[string]json.RawMessage
	_ = json.Unmarshal(got["messages"], &messages)
	var role string
	_ = json.Unmarshal(messages[0]["role"], &role)
	if role != "developer" {
		t.Fatalf("generic provider role was rewritten to %q", role)
	}
}

func TestZhipuChatEndpointDetection(t *testing.T) {
	tests := []struct {
		endpoint domain.Endpoint
		want     bool
	}{
		{domain.Endpoint{BaseURL: "https://open.bigmodel.cn/api/paas/v4"}, true},
		{domain.Endpoint{BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4"}, true},
		{domain.Endpoint{FullURL: "https://api.z.ai/api/coding/paas/v4/chat/completions"}, true},
		{domain.Endpoint{BaseURL: "https://open.bigmodel.cn.example/api/paas/v4"}, false},
		{domain.Endpoint{BaseURL: "https://compatible.example/api/paas/v4"}, false},
		{domain.Endpoint{BaseURL: "https://open.bigmodel.cn/other"}, false},
	}
	for _, test := range tests {
		if got := isZhipuOpenAIChatEndpoint(&test.endpoint); got != test.want {
			t.Errorf("endpoint %+v detected=%v, want %v", test.endpoint, got, test.want)
		}
	}
}
