package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"ferry/internal/config"
	"ferry/internal/domain"
)

func TestDiscoverProviderModelsPaginatesAnthropicCatalog(t *testing.T) {
	var calls int
	client := &http.Client{Transport: modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/v1/models" || request.URL.Query().Get("limit") != "1000" ||
			request.Header.Get("X-Catalog-Key") != "Token secret" || request.Header.Get("anthropic-version") == "" {
			t.Fatalf("request=%s headers=%v", request.URL, request.Header)
		}
		switch calls {
		case 1:
			if request.URL.Query().Get("after_id") != "" {
				t.Fatalf("first cursor=%q", request.URL.Query().Get("after_id"))
			}
			return modelCatalogResponse(http.StatusOK, `{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`), nil
		case 2:
			if request.URL.Query().Get("after_id") != "claude-a" {
				t.Fatalf("second cursor=%q", request.URL.Query().Get("after_id"))
			}
			return modelCatalogResponse(http.StatusOK, `{"data":[{"id":"claude-b"}],"has_more":false,"last_id":"claude-b"}`), nil
		default:
			t.Fatalf("unexpected page %d", calls)
			return nil, nil
		}
	})}
	models, entries, err := discoverProviderModelsWithCredential(context.Background(), client, "https://anthropic.test", domain.ProtocolAnthropic, "secret", domain.CredentialRef{
		Header: "X-Catalog-Key", Prefix: "Token ",
	})
	if err != nil || !reflect.DeepEqual(models, []string{"claude-a", "claude-b"}) || calls != 2 || entries != nil {
		t.Fatalf("models=%v calls=%d entries=%v err=%v", models, calls, entries, err)
	}
}

func TestDiscoverProviderModelsCanonicalizesOpenRouterAnthropicCatalogIDs(t *testing.T) {
	client := &http.Client{Transport: modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://openrouter.ai/api/v1/models?limit=1000" ||
			request.Header.Get("x-api-key") != "secret" {
			t.Fatalf("request=%s headers=%v", request.URL, request.Header)
		}
		return modelCatalogResponse(http.StatusOK, `{"data":[
			{"id":"anthropic/meta/muse-spark-1.3-contributor [1m]","context_length":1048576,
			 "top_provider":{"max_completion_tokens":943718},
			 "supported_parameters":["tools","reasoning"]},
			{"id":"anthropic/claude-sonnet-4","supported_parameters":["tools"]}
		]}`), nil
	})}
	models, entries, err := discoverProviderModelsWithCredential(context.Background(), client,
		"https://openrouter.ai/api", domain.ProtocolAnthropic, "secret", domain.CredentialRef{})
	if err != nil {
		t.Fatal(err)
	}
	wantModels := []string{"meta/muse-spark-1.3-contributor", "anthropic/claude-sonnet-4"}
	if !reflect.DeepEqual(models, wantModels) {
		t.Fatalf("models=%v want=%v", models, wantModels)
	}
	spark, ok := entries["meta/muse-spark-1.3-contributor"]
	if !ok || !spark.Authoritative || !spark.Capabilities.Stream || !spark.Capabilities.Tools ||
		!spark.Capabilities.Reasoning || spark.Capabilities.ContextWindow != 1048576 || spark.Capabilities.MaxOutput != 943718 {
		t.Fatalf("canonical capability entry=%+v entries=%v", spark, entries)
	}
	if _, ok := entries["anthropic/claude-sonnet-4"]; !ok {
		t.Fatalf("real anthropic vendor id was rewritten: %v", entries)
	}
}

func TestDiscoverProviderModelsRejectsUnboundedAndTruncatedCatalogs(t *testing.T) {
	t.Run("page limit", func(t *testing.T) {
		var calls int
		client := &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			cursor := "cursor-" + strconv.Itoa(calls)
			return modelCatalogResponse(http.StatusOK, `{"data":[{"id":"same"}],"has_more":true,"last_id":"`+cursor+`"}`), nil
		})}
		_, err := discoverProviderModels(context.Background(), client, "https://anthropic.test", domain.ProtocolAnthropic, "secret")
		if err == nil || !strings.Contains(err.Error(), "exceeds 100 pages") || calls != maxModelCatalogPages {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})

	t.Run("model limit", func(t *testing.T) {
		items := make([]map[string]string, maxDiscoveredModels+1)
		for index := range items {
			items[index] = map[string]string{"id": "model-" + strconv.Itoa(index)}
		}
		body, err := json.Marshal(map[string]any{"data": items})
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return modelCatalogResponse(http.StatusOK, string(body)), nil
		})}
		_, err = discoverProviderModels(context.Background(), client, "https://openai.test/v1", domain.ProtocolOpenAIChat, "secret")
		if err == nil || !strings.Contains(err.Error(), "exceeds 2000 models") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestDiscoverProviderModelsRejectsSameHostHTTPSDowngrade(t *testing.T) {
	var calls int
	client := &http.Client{Transport: modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		response := modelCatalogResponse(http.StatusFound, "")
		response.Header.Set("Location", "http://catalog.test/v1/models")
		response.Request = request
		return response, nil
	})}
	_, err := discoverProviderModels(context.Background(), client, "https://catalog.test/v1", domain.ProtocolOpenAIChat, "secret")
	if err == nil || calls != 1 {
		t.Fatalf("downgrade calls=%d err=%v", calls, err)
	}
}

func writeOpenAIChatProbeSSE(w http.ResponseWriter, content string) {
	chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}}}})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", chunk)
}

func writeOpenAIChatProbeResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{},
	})
}

type retryingTextProbeCaller struct {
	maxTokens []int64
}

func (c *retryingTextProbeCaller) Invoke(context.Context, *domain.RequestContract) (*domain.Response, *domain.Failure) {
	return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "unexpected invoke"}
}

func (c *retryingTextProbeCaller) Stream(_ context.Context, contract *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	c.maxTokens = append(c.maxTokens, contract.MaxTokens)
	events := make(chan domain.StreamEvent, 2)
	if len(c.maxTokens) > 1 {
		events <- domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: "OK"}
	}
	events <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: "stop"}
	close(events)
	return events, nil, nil
}

func TestProviderTextProbeRetriesEmptyStreamWithBoundedBudget(t *testing.T) {
	caller := &retryingTextProbeCaller{}
	failure, requests := probeProviderTextFeatures(context.Background(), caller, domain.ProtocolAnthropic,
		"reasoning-model", providerTextProbeFeatures{}, providerProbeStream)
	if failure != nil || requests != 2 ||
		!reflect.DeepEqual(caller.maxTokens, []int64{providerTextProbeMaxTokens, providerTextProbeRetryTokens}) {
		t.Fatalf("failure=%+v requests=%d max_tokens=%v", failure, requests, caller.maxTokens)
	}
}

func writeOpenAIChatToolProbeResponse(w http.ResponseWriter, parallel bool) {
	toolCalls := []any{map[string]any{
		"id": "cap-1", "type": "function",
		"function": map[string]any{"name": "ferry_capability_probe_one", "arguments": "{\"ok\":true}"},
	}}
	if parallel {
		toolCalls = append(toolCalls, map[string]any{
			"id": "cap-2", "type": "function",
			"function": map[string]any{"name": "ferry_capability_probe_two", "arguments": "{\"ok\":true}"},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{},
	})
}

func writeOpenAIResponsesProbeSSE(w http.ResponseWriter, content string) {
	chunk, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": "item-1", "delta": content})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{}}}\n\n", chunk)
}

func providerProbeUsesTools(body map[string]any) bool {
	tools, ok := body["tools"].([]any)
	return ok && len(tools) > 0
}

func providerProbeUsesParallelTools(body map[string]any) bool {
	tools, _ := body["tools"].([]any)
	return len(tools) > 1
}

func providerProbeUsesVision(body map[string]any) bool {
	payload, _ := json.Marshal(body)
	return strings.Contains(string(payload), "data:image/png;base64,")
}

func writeProviderToolProbeSSE(w http.ResponseWriter, protocol domain.Protocol, parallel bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	switch protocol {
	case domain.ProtocolOpenAIChat:
		if parallel {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"cap-1","function":{"name":"ferry_capability_probe_one"}},{"index":1,"id":"cap-2","function":{"name":"ferry_capability_probe_two"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ok\":true}"}},{"index":1,"function":{"arguments":"{\"ok\":true}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`))
			return
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"cap-1","function":{"name":"ferry_capability_probe_one"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ok\":true}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`))
	case domain.ProtocolOpenAIResponses:
		if parallel {
			_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","item_id":"fc-1","item":{"type":"function_call","call_id":"cap-1","name":"ferry_capability_probe_one"}}

data: {"type":"response.output_item.added","item_id":"fc-2","item":{"type":"function_call","call_id":"cap-2","name":"ferry_capability_probe_two"}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc-1","delta":"{\"ok\":true}"}

data: {"type":"response.function_call_arguments.delta","item_id":"fc-2","delta":"{\"ok\":true}"}

data: {"type":"response.output_item.done","item_id":"fc-1","item":{"type":"function_call","arguments":"{\"ok\":true}"}}

data: {"type":"response.output_item.done","item_id":"fc-2","item":{"type":"function_call","arguments":"{\"ok\":true}"}}

data: {"type":"response.completed","response":{"status":"completed","usage":{}}}

`))
			return
		}
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","item_id":"fc-1","item":{"type":"function_call","call_id":"cap-1","name":"ferry_capability_probe_one"}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc-1","delta":"{\"ok\":true}"}

data: {"type":"response.output_item.done","item_id":"fc-1","item":{"type":"function_call","arguments":"{\"ok\":true}"}}

data: {"type":"response.completed","response":{"status":"completed","usage":{}}}

`))
	case domain.ProtocolAnthropic:
		if parallel {
			_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"usage":{}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"cap-1","name":"ferry_capability_probe_one","input":{"ok":true}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"cap-2","name":"ferry_capability_probe_two","input":{"ok":true}}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{}}

event: message_stop
data: {"type":"message_stop"}

`))
			return
		}
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"usage":{}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"cap-1","name":"ferry_capability_probe_one","input":{"ok":true}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{}}

event: message_stop
data: {"type":"message_stop"}

`))
	}
}

func providerDiscoveryModelResult(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	protocols, ok := response["protocols"].([]any)
	if !ok || len(protocols) != 1 {
		t.Fatalf("protocol results=%v", response["protocols"])
	}
	models, ok := protocols[0].(map[string]any)["model_results"].([]any)
	if !ok || len(models) != 1 {
		t.Fatalf("model results=%v", protocols[0])
	}
	return models[0].(map[string]any)
}

func providerDiscoveryCapabilityEvidence(t *testing.T, model map[string]any, capability string) map[string]any {
	t.Helper()
	evidence, ok := model["capability_evidence"].([]any)
	if !ok {
		t.Fatalf("capability evidence=%v", model["capability_evidence"])
	}
	for _, raw := range evidence {
		item := raw.(map[string]any)
		if item["capability"] == capability {
			return item
		}
	}
	t.Fatalf("capability %q not found in evidence=%v", capability, evidence)
	return nil
}

func TestProviderCapabilityRegistryCoversDomainCapabilities(t *testing.T) {
	typeOfCapabilities := reflect.TypeOf(domain.Capabilities{})
	domainCapabilities := make([]string, 0, typeOfCapabilities.NumField())
	fields := make(map[string]reflect.StructField, typeOfCapabilities.NumField())
	for index := 0; index < typeOfCapabilities.NumField(); index++ {
		field := typeOfCapabilities.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("capability field %s has no JSON evidence identity", field.Name)
		}
		domainCapabilities = append(domainCapabilities, name)
		fields[name] = field
	}
	if registry := providerCapabilityRegistry(); !reflect.DeepEqual(registry, domainCapabilities) {
		t.Fatalf("provider capability registry=%v, domain capabilities=%v", registry, domainCapabilities)
	}

	for capability, field := range fields {
		switch field.Type.Kind() {
		case reflect.Bool:
			var capabilities domain.Capabilities
			setProviderCapability(&capabilities, capability, true)
			if !reflect.ValueOf(capabilities).FieldByIndex(field.Index).Bool() {
				t.Fatalf("boolean capability %q has no setProviderCapability mapping", capability)
			}
		case reflect.Int64:
			if capability != providerCapabilityContextWindow && capability != providerCapabilityMaxOutput {
				t.Fatalf("numeric capability %q needs an explicit evidence strategy", capability)
			}
		default:
			t.Fatalf("capability %q uses unsupported field kind %s", capability, field.Type.Kind())
		}
	}
}

func TestProviderProbeFailureMessageExtractsStructuredError(t *testing.T) {
	failure := &domain.Failure{Type: domain.FailBadResponse, Message: `{"error":{"message":"model is only supported on /v1/images/generations"}}`}
	if got := providerProbeFailureMessage(failure); got != "model is only supported on /v1/images/generations" {
		t.Fatalf("probe failure message=%q", got)
	}
}

func TestDeterministicProviderCapabilityFailureClassification(t *testing.T) {
	for _, failureType := range []domain.FailureType{domain.FailUnsupportedParam, domain.FailProtocolConversion} {
		if !deterministicProviderCapabilityFailure(&domain.Failure{Type: failureType}) {
			t.Fatalf("%s must be deterministic capability evidence", failureType)
		}
	}
	for _, failureType := range []domain.FailureType{
		domain.FailBadResponse, domain.FailStreamBroken, domain.FailProvider5xx, domain.FailRateLimit,
	} {
		if deterministicProviderCapabilityFailure(&domain.Failure{Type: failureType}) {
			t.Fatalf("%s must remain inconclusive capability evidence", failureType)
		}
	}
}

func TestProviderCapabilitySemanticValidationClassifiesUnsupported(t *testing.T) {
	structured := validateProviderStructuredProbeContent(`{"ok":false}`)
	if structured == nil || structured.Type != domain.FailUnsupportedParam {
		t.Fatalf("structured semantic rejection=%+v", structured)
	}
	tools := validateProviderToolProbeResponse(&domain.Response{
		Content: []domain.Block{{Kind: domain.BlockText, Text: "I cannot call tools"}},
	}, map[string]struct{}{"ferry_capability_probe_one": {}}, false)
	if tools == nil || tools.Type != domain.FailUnsupportedParam {
		t.Fatalf("tool semantic rejection=%+v", tools)
	}
	parallel := validateProviderToolProbeResponse(&domain.Response{
		Content: []domain.Block{{Kind: domain.BlockToolUse, Name: "ferry_capability_probe_one", Input: json.RawMessage(`{}`)}},
	}, map[string]struct{}{
		"ferry_capability_probe_one": {},
		"ferry_capability_probe_two": {},
	}, true)
	if parallel == nil || parallel.Type != domain.FailUnsupportedParam {
		t.Fatalf("parallel-tool semantic rejection=%+v", parallel)
	}
	malformed := validateProviderTextProbeResponse(nil, providerTextProbeFeatures{})
	if malformed == nil || malformed.Type != domain.FailBadResponse || deterministicProviderCapabilityFailure(malformed) {
		t.Fatalf("malformed response must stay inconclusive: %+v", malformed)
	}
}

func TestProviderModelDiscoveryFetchesAndDeduplicatesUpstreamCatalog(t *testing.T) {
	var requestPath, authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		authorization = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]string{"id": "model-b"}, map[string]string{"id": "model-a"},
			map[string]string{"id": "model-b"}, "model-c",
		}})
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-models", map[string]any{
		"type": "custom", "base_url": upstream.URL + "/v1/", "protocol": "openai-chat", "secret": "discovery-secret",
	})
	requireStatus(t, rec, http.StatusOK)
	if requestPath != "/v1/models" || authorization != "Bearer discovery-secret" {
		t.Fatalf("upstream request path=%q authorization=%q", requestPath, authorization)
	}
	models, _ := response["models"].([]any)
	if len(models) != 3 || models[0] != "model-b" || models[1] != "model-a" || models[2] != "model-c" {
		t.Fatalf("discovered models=%v", response["models"])
	}
}

func TestProviderModelDiscoveryUsesAnthropicPathAndHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "anthropic-secret" || r.Header.Get("anthropic-version") == "" {
			t.Fatalf("request path=%q headers=%v", r.URL.Path, r.Header)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-test"}]}`))
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-models", map[string]any{
		"type": "custom", "base_url": upstream.URL, "protocol": "anthropic", "secret": "anthropic-secret",
	})
	requireStatus(t, rec, http.StatusOK)
	if response["source"] != "upstream" {
		t.Fatalf("response=%v", response)
	}
}

func TestProviderModelDiscoveryReturnsBuiltInCodexCatalog(t *testing.T) {
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-models", map[string]any{
		"type": "cliproxy-codex", "base_url": "https://chatgpt.com/backend-api/codex", "protocol": "openai-responses",
	})
	requireStatus(t, rec, http.StatusOK)
	models, _ := response["models"].([]any)
	foundSol, foundImage := false, false
	for _, model := range models {
		foundSol = foundSol || model == "gpt-5.6-sol"
		foundImage = foundImage || model == "gpt-image-2"
	}
	if response["source"] != "built-in-pre-auth" || len(models) == 0 || !foundSol || !foundImage {
		t.Fatalf("response=%v", response)
	}
}

func TestProviderProtocolDiscoveryProbesNativeModelsPerEndpoint(t *testing.T) {
	supported := map[string]string{
		"/v1/chat/completions":   "chat-native",
		"/v1/responses":          "responses-native",
		"/v1/messages":           "anthropic-native",
		"/v1/images/generations": "image-native",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]string{"id": "chat-native"},
				map[string]string{"id": "responses-native"},
				map[string]string{"id": "anthropic-native"},
				map[string]string{"id": "image-native"},
			}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != supported[r.URL.Path] {
			http.Error(w, "model is not supported on this protocol", http.StatusNotFound)
			return
		}
		probeProtocol := map[string]domain.Protocol{
			"/v1/chat/completions": domain.ProtocolOpenAIChat,
			"/v1/responses":        domain.ProtocolOpenAIResponses,
			"/v1/messages":         domain.ProtocolAnthropic,
		}[r.URL.Path]
		if providerProbeUsesTools(body) {
			writeProviderToolProbeSSE(w, probeProtocol, providerProbeUsesParallelTools(body))
			return
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			if body["stream"] != true {
				t.Fatalf("chat protocol probe must verify streaming: %v", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if body["response_format"] != nil {
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"ok\\\":true}\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
				return
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		case "/v1/responses":
			if body["stream"] != true {
				t.Fatalf("responses protocol probe must verify streaming: %v", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if body["text"] != nil {
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"item-1\",\"delta\":\"{\\\"ok\\\":true}\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{}}}\n\n"))
				return
			}
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"item-1\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{}}}\n\n"))
		case "/v1/messages":
			if body["stream"] != true {
				t.Fatalf("anthropic protocol probe must verify streaming: %v", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		case "/v1/images/generations":
			_, _ = w.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}],"usage":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
		"base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{
			{"protocol": "openai-chat", "models": []string{"chat-native"}},
			{"protocol": "openai-responses", "models": []string{"responses-native"}},
			{"protocol": "anthropic", "models": []string{"anthropic-native"}},
			{"protocol": "openai-images", "models": []string{"image-native"}},
		},
	})
	requireStatus(t, rec, http.StatusOK)
	protocols, _ := response["protocols"].([]any)
	if len(protocols) != 4 {
		t.Fatalf("protocol results=%v", response["protocols"])
	}
	for _, raw := range protocols {
		result := raw.(map[string]any)
		models := result["models"].([]any)
		if result["status"] != "verified" || len(models) != 1 {
			t.Fatalf("probe result=%v", result)
		}
		protocol := result["protocol"].(string)
		want := map[string]string{
			"openai-chat":      "chat-native",
			"openai-responses": "responses-native",
			"anthropic":        "anthropic-native",
			"openai-images":    "image-native",
		}[protocol]
		if models[0] != want {
			t.Fatalf("%s native models=%v want %s", protocol, models, want)
		}
		if _, hasError := result["error"]; hasError {
			t.Fatalf("reachable protocol must not expose per-model probe errors: %v", result)
		}
	}
}

func TestProviderProtocolDiscoveryCachesCombinedStructuredProbe(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			postCount := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "structured-model"}}})
					return
				}
				postCount++
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["stream"] != true {
					t.Fatalf("combined probe must stream: %v", body)
				}
				if providerProbeUsesTools(body) {
					if body["parallel_tool_calls"] != true {
						t.Fatalf("combined tool probe omitted parallel contract: %v", body)
					}
					writeProviderToolProbeSSE(w, protocol, providerProbeUsesParallelTools(body))
					return
				}
				switch protocol {
				case domain.ProtocolOpenAIChat:
					if body["response_format"] == nil {
						t.Fatalf("chat combined probe omitted response_format: %v", body)
					}
					writeOpenAIChatProbeSSE(w, "{\"ok\":true}")
				case domain.ProtocolOpenAIResponses:
					if body["text"] == nil {
						t.Fatalf("responses combined probe omitted text.format: %v", body)
					}
					writeOpenAIResponsesProbeSSE(w, "{\"ok\":true}")
				}
			}))
			defer upstream.Close()

			f := newV2APIFixture(t)
			body := map[string]any{
				"provider_id": "provider-capabilities", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
				"protocols": []map[string]any{{"protocol": protocol, "models": []string{"structured-model"}}},
			}
			firstRec, first := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
			requireStatus(t, firstRec, http.StatusOK)
			firstModel := providerDiscoveryModelResult(t, first)
			capabilities := firstModel["capabilities"].(map[string]any)
			contextEvidence := providerDiscoveryCapabilityEvidence(t, firstModel, providerCapabilityContextWindow)
			outputEvidence := providerDiscoveryCapabilityEvidence(t, firstModel, providerCapabilityMaxOutput)
			if postCount != 2 || firstModel["probe_requests"] != float64(2) ||
				capabilities["text"] != true || capabilities["stream"] != true || capabilities["structured_output"] != true ||
				capabilities["reasoning"] != true || capabilities["vision"] != true ||
				capabilities["tools"] != true || capabilities["parallel_tools"] != true {
				t.Fatalf("first combined probe posts=%d result=%v", postCount, firstModel)
			}
			if contextEvidence["status"] != "unknown" || contextEvidence["source"] != "authoritative-config-required" ||
				outputEvidence["status"] != "unknown" || outputEvidence["source"] != "authoritative-config-required" {
				t.Fatalf("token limit evidence context=%v output=%v", contextEvidence, outputEvidence)
			}

			secondRec, second := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
			requireStatus(t, secondRec, http.StatusOK)
			secondModel := providerDiscoveryModelResult(t, second)
			if postCount != 2 || secondModel["cache_hit"] != true {
				t.Fatalf("cached probe posts=%d result=%v", postCount, secondModel)
			}
		})
	}
}

func TestProviderProtocolDiscoverySeparatesTextFromStreamingAndCachesFallback(t *testing.T) {
	postCount, streamCount, invokeCount := 0, 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]string{"id": "invoke-only-model"}},
			})
			return
		}
		postCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			streamCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("{\"error\":{\"message\":\"stream is not supported\"}}"))
			return
		}
		invokeCount++
		if providerProbeUsesTools(body) {
			writeOpenAIChatToolProbeResponse(w, providerProbeUsesParallelTools(body))
			return
		}
		writeOpenAIChatProbeResponse(w, "{\"ok\":true}")
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	body := map[string]any{
		"provider_id": "provider-no-stream", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIChat, "models": []string{"invoke-only-model"}}},
	}
	firstRec, first := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, firstRec, http.StatusOK)
	firstModel := providerDiscoveryModelResult(t, first)
	capabilities := firstModel["capabilities"].(map[string]any)
	if postCount != 4 || streamCount != 2 || invokeCount != 2 || firstModel["probe_requests"] != float64(4) {
		t.Fatalf("fallback probe posts=%d stream=%d invoke=%d result=%v", postCount, streamCount, invokeCount, firstModel)
	}
	if firstModel["status"] != "verified" || capabilities["text"] != true || capabilities["stream"] != false ||
		capabilities["structured_output"] != true || capabilities["reasoning"] != true || capabilities["vision"] != true ||
		capabilities["tools"] != true || capabilities["parallel_tools"] != true {
		t.Fatalf("invoke-only capabilities=%v result=%v", capabilities, firstModel)
	}

	secondRec, second := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, secondRec, http.StatusOK)
	secondModel := providerDiscoveryModelResult(t, second)
	if postCount != 4 || secondModel["cache_hit"] != true {
		t.Fatalf("invoke-only evidence was not cached: posts=%d result=%v", postCount, secondModel)
	}
	if _, exists := secondModel["probe_requests"]; exists {
		t.Fatalf("cached model unexpectedly reported probe requests: %v", secondModel)
	}
}

func TestProviderProtocolDiscoveryCachesDeterministicStructuredOutputRejection(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			postCount, structuredCount, basicCount, toolCount := 0, 0, 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "plain-model"}}})
					return
				}
				postCount++
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if providerProbeUsesTools(body) {
					toolCount++
					writeProviderToolProbeSSE(w, protocol, providerProbeUsesParallelTools(body))
					return
				}
				structured := body["response_format"] != nil || body["text"] != nil
				if structured {
					structuredCount++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("{\"error\":{\"message\":\"structured output is not supported\"}}"))
					return
				}
				basicCount++
				if protocol == domain.ProtocolOpenAIChat {
					writeOpenAIChatProbeSSE(w, "ok")
				} else {
					writeOpenAIResponsesProbeSSE(w, "ok")
				}
			}))
			defer upstream.Close()

			f := newV2APIFixture(t)
			body := map[string]any{
				"provider_id": "provider-plain", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
				"protocols": []map[string]any{{"protocol": protocol, "models": []string{"plain-model"}}},
			}
			firstRec, first := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
			requireStatus(t, firstRec, http.StatusOK)
			firstModel := providerDiscoveryModelResult(t, first)
			capabilities := firstModel["capabilities"].(map[string]any)
			structuredEvidence := providerDiscoveryCapabilityEvidence(t, firstModel, providerCapabilityStructuredOutput)
			if postCount != 6 || structuredCount != 2 || basicCount != 3 || toolCount != 1 || firstModel["probe_requests"] != float64(6) {
				t.Fatalf("first fallback probe posts=%d structured=%d basic=%d tools=%d result=%v", postCount, structuredCount, basicCount, toolCount, firstModel)
			}
			if capabilities["text"] != true || capabilities["stream"] != true || capabilities["structured_output"] != false ||
				capabilities["reasoning"] != true || capabilities["vision"] != true || capabilities["tools"] != true ||
				capabilities["parallel_tools"] != true || structuredEvidence["status"] != "unsupported" {
				t.Fatalf("structured rejection capabilities=%v evidence=%v", capabilities, structuredEvidence)
			}

			secondRec, second := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
			requireStatus(t, secondRec, http.StatusOK)
			secondModel := providerDiscoveryModelResult(t, second)
			if postCount != 6 || secondModel["cache_hit"] != true {
				t.Fatalf("negative evidence was not cached: posts=%d result=%v", postCount, secondModel)
			}
		})
	}
}

func TestProviderProtocolDiscoveryDoesNotAssumeAnthropicStructuredOutput(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "claude-test"}}})
			return
		}
		postCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if providerProbeUsesTools(body) {
			writeProviderToolProbeSSE(w, domain.ProtocolAnthropic, providerProbeUsesParallelTools(body))
			return
		}
		if body["stream"] != true || body["response_format"] != nil || body["text"] != nil {
			t.Fatalf("anthropic core probe must use native text streaming without structured output: %v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
		"provider_id": "provider-anthropic", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolAnthropic, "models": []string{"claude-test"}}},
	})
	requireStatus(t, rec, http.StatusOK)
	model := providerDiscoveryModelResult(t, response)
	evidence := providerDiscoveryCapabilityEvidence(t, model, providerCapabilityStructuredOutput)
	if postCount != 6 || model["probe_requests"] != float64(6) || evidence["status"] != "unsupported" || evidence["source"] != "probe" {
		t.Fatalf("anthropic probe posts=%d evidence=%v result=%v", postCount, evidence, model)
	}
}

func TestProviderProtocolDiscoveryReprobesWhenEndpointChangesOrRefreshIsForced(t *testing.T) {
	postCount := 0
	newUpstream := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "model-a"}}})
				return
			}
			postCount++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if providerProbeUsesTools(body) {
				writeProviderToolProbeSSE(w, domain.ProtocolOpenAIChat, providerProbeUsesParallelTools(body))
				return
			}
			writeOpenAIChatProbeSSE(w, "{\"ok\":true}")
		}))
	}
	firstUpstream, secondUpstream := newUpstream(), newUpstream()
	defer firstUpstream.Close()
	defer secondUpstream.Close()
	f := newV2APIFixture(t)
	probe := func(baseURL string, refresh bool) map[string]any {
		rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
			"provider_id": "provider-a", "base_url": baseURL + "/v1", "secret": "secret", "refresh": refresh,
			"protocols": []map[string]any{{"protocol": "openai-chat", "models": []string{"model-a"}}},
		})
		requireStatus(t, rec, http.StatusOK)
		return providerDiscoveryModelResult(t, response)
	}
	probe(firstUpstream.URL, false)
	if cached := probe(firstUpstream.URL, false); postCount != 2 || cached["cache_hit"] != true {
		t.Fatalf("same endpoint was not cached: posts=%d result=%v", postCount, cached)
	}
	if changed := probe(secondUpstream.URL, false); postCount != 4 || changed["probe_requests"] != float64(2) {
		t.Fatalf("changed endpoint did not re-probe: posts=%d result=%v", postCount, changed)
	}
	if refreshed := probe(secondUpstream.URL, true); postCount != 6 || refreshed["probe_requests"] != float64(2) {
		t.Fatalf("forced refresh did not re-probe: posts=%d result=%v", postCount, refreshed)
	}
}

func TestProviderProtocolDiscoveryFallsBackFromParallelToolsToSingleTool(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "single-tool-model"}}})
			return
		}
		postCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !providerProbeUsesTools(body) {
			writeOpenAIChatProbeSSE(w, "{\"ok\":true}")
			return
		}
		if providerProbeUsesParallelTools(body) {
			// Some OpenAI-compatible providers accept the parallel flag but
			// silently produce only one call. That is valid tools evidence, not
			// parallel-tools evidence.
			writeProviderToolProbeSSE(w, domain.ProtocolOpenAIChat, false)
			return
		}
		writeProviderToolProbeSSE(w, domain.ProtocolOpenAIChat, false)
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	body := map[string]any{
		"provider_id": "provider-single-tools", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIChat, "models": []string{"single-tool-model"}}},
	}
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, rec, http.StatusOK)
	model := providerDiscoveryModelResult(t, response)
	capabilities := model["capabilities"].(map[string]any)
	parallelEvidence := providerDiscoveryCapabilityEvidence(t, model, providerCapabilityParallelTools)
	if postCount != 3 || model["probe_requests"] != float64(3) || capabilities["tools"] != true ||
		capabilities["parallel_tools"] != false || parallelEvidence["status"] != "unsupported" {
		t.Fatalf("parallel tool fallback posts=%d evidence=%v result=%v", postCount, parallelEvidence, model)
	}

	secondRec, second := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, secondRec, http.StatusOK)
	if cached := providerDiscoveryModelResult(t, second); postCount != 3 || cached["cache_hit"] != true {
		t.Fatalf("tool fallback evidence was not cached: posts=%d result=%v", postCount, cached)
	}
}

func TestProviderProtocolDiscoveryDoesNotCacheTransientProbeFailure(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "temporarily-unavailable"}}})
			return
		}
		postCount++
		http.Error(w, "{\"error\":{\"message\":\"temporary upstream outage\"}}", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	body := map[string]any{
		"provider_id": "provider-transient", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIChat, "models": []string{"temporarily-unavailable"}}},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
		requireStatus(t, rec, http.StatusOK)
		model := providerDiscoveryModelResult(t, response)
		if postCount != attempt || model["probe_requests"] != float64(1) || model["cache_hit"] == true {
			t.Fatalf("transient attempt=%d posts=%d result=%v", attempt, postCount, model)
		}
		for _, capability := range []string{providerCapabilityText, providerCapabilityStream, providerCapabilityTools, providerCapabilityParallelTools, providerCapabilityReasoning, providerCapabilityStructuredOutput, providerCapabilityVision} {
			evidence := providerDiscoveryCapabilityEvidence(t, model, capability)
			if evidence["status"] != "unknown" || evidence["failure_type"] != string(domain.FailProvider5xx) {
				t.Fatalf("transient %s evidence=%v", capability, evidence)
			}
		}
	}
}

func TestProviderProtocolDiscoveryDoesNotCacheMalformedResponse(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "malformed-model"}}})
			return
		}
		postCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	body := map[string]any{
		"provider_id": "provider-malformed", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIChat, "models": []string{"malformed-model"}}},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
		requireStatus(t, rec, http.StatusOK)
		model := providerDiscoveryModelResult(t, response)
		if postCount != attempt || model["probe_requests"] != float64(1) || model["cache_hit"] == true {
			t.Fatalf("malformed attempt=%d posts=%d result=%v", attempt, postCount, model)
		}
		for _, capability := range []string{providerCapabilityText, providerCapabilityStream, providerCapabilityTools, providerCapabilityParallelTools, providerCapabilityReasoning, providerCapabilityStructuredOutput, providerCapabilityVision} {
			evidence := providerDiscoveryCapabilityEvidence(t, model, capability)
			failureType, _ := evidence["failure_type"].(string)
			if evidence["status"] != "unknown" || failureType == "" || deterministicProviderCapabilityFailure(&domain.Failure{Type: domain.FailureType(failureType)}) {
				t.Fatalf("malformed %s evidence=%v", capability, evidence)
			}
		}
	}
}

func TestProviderProtocolDiscoveryCachesStructuredSemanticRejection(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "free-form-model"}}})
			return
		}
		postCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if providerProbeUsesTools(body) {
			writeProviderToolProbeSSE(w, domain.ProtocolOpenAIChat, providerProbeUsesParallelTools(body))
			return
		}
		if body["response_format"] != nil {
			writeOpenAIChatProbeSSE(w, "plain text")
			return
		}
		writeOpenAIChatProbeSSE(w, "ok")
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	body := map[string]any{
		"provider_id": "provider-free-form", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIChat, "models": []string{"free-form-model"}}},
	}
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, rec, http.StatusOK)
	model := providerDiscoveryModelResult(t, response)
	evidence := providerDiscoveryCapabilityEvidence(t, model, providerCapabilityStructuredOutput)
	if evidence["status"] != "unsupported" || evidence["failure_type"] != string(domain.FailUnsupportedParam) {
		t.Fatalf("structured semantic evidence=%v result=%v", evidence, model)
	}
	postsAfterFirst := postCount
	secondRec, second := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, secondRec, http.StatusOK)
	if cached := providerDiscoveryModelResult(t, second); postCount != postsAfterFirst || cached["cache_hit"] != true {
		t.Fatalf("structured semantic rejection was not cached: posts=%d result=%v", postCount, cached)
	}
}

func TestProviderProtocolDiscoveryIsolatesRejectedVisionCapability(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "text-reasoning-model"}}})
			return
		}
		postCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if providerProbeUsesTools(body) {
			writeProviderToolProbeSSE(w, domain.ProtocolOpenAIChat, providerProbeUsesParallelTools(body))
			return
		}
		if providerProbeUsesVision(body) {
			http.Error(w, "{\"error\":{\"message\":\"image input is not supported\"}}", http.StatusBadRequest)
			return
		}
		if body["response_format"] != nil {
			writeOpenAIChatProbeSSE(w, "{\"ok\":true}")
			return
		}
		writeOpenAIChatProbeSSE(w, "ok")
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
		"provider_id": "provider-no-vision", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIChat, "models": []string{"text-reasoning-model"}}},
	})
	requireStatus(t, rec, http.StatusOK)
	model := providerDiscoveryModelResult(t, response)
	capabilities := model["capabilities"].(map[string]any)
	visionEvidence := providerDiscoveryCapabilityEvidence(t, model, providerCapabilityVision)
	if postCount != 6 || model["probe_requests"] != float64(6) || capabilities["text"] != true ||
		capabilities["stream"] != true || capabilities["structured_output"] != true || capabilities["reasoning"] != true ||
		capabilities["vision"] != false || capabilities["tools"] != true || capabilities["parallel_tools"] != true ||
		visionEvidence["status"] != "unsupported" {
		t.Fatalf("vision isolation posts=%d evidence=%v result=%v", postCount, visionEvidence, model)
	}
}

func TestProviderProtocolDiscoveryCachesImageCapability(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "image-model"}}})
			return
		}
		postCount++
		_, _ = w.Write([]byte("{\"data\":[{\"b64_json\":\"aW1hZ2U=\"}],\"usage\":{}}"))
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	body := map[string]any{
		"provider_id": "provider-images", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIImages, "models": []string{"image-model"}}},
	}
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, rec, http.StatusOK)
	model := providerDiscoveryModelResult(t, response)
	evidence := providerDiscoveryCapabilityEvidence(t, model, providerCapabilityImages)
	if postCount != 1 || model["probe_requests"] != float64(1) || model["capabilities"].(map[string]any)["images"] != true || evidence["status"] != "supported" {
		t.Fatalf("image evidence posts=%d evidence=%v result=%v", postCount, evidence, model)
	}
	secondRec, second := f.do(http.MethodPost, "/api/providers/discover-protocols", body)
	requireStatus(t, secondRec, http.StatusOK)
	if cached := providerDiscoveryModelResult(t, second); postCount != 1 || cached["cache_hit"] != true {
		t.Fatalf("image evidence was not cached: posts=%d result=%v", postCount, cached)
	}
}

func TestProviderProtocolDiscoveryRejectsHTTP200EmptyStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	models, results := probeProviderProtocolModels(
		context.Background(), upstream.Client(), upstream.URL, "", domain.ProtocolOpenAIChat, "secret", []string{"empty-stream"},
	)
	if len(models) != 0 || len(results) != 1 || !strings.Contains(results[0].Error, "ended before any content") {
		t.Fatalf("empty stream was accepted: models=%v results=%v", models, results)
	}
}

func TestProviderProtocolDiscoveryUsesExactFullURLAndReportsPartialModels(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "works"}, map[string]string{"id": "fails"}}})
			return
		}
		if r.URL.Path != "/nonstandard/chat" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "fails" {
			http.Error(w, `{"error":{"message":"model blocked on this endpoint"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
		"base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{
			"protocol": "openai-chat", "full_url": upstream.URL + "/nonstandard/chat",
			"models": []string{"works", "fails"},
		}},
	})
	requireStatus(t, rec, http.StatusOK)
	protocols := response["protocols"].([]any)
	result := protocols[0].(map[string]any)
	if result["status"] != "partial" || result["endpoint"] != upstream.URL+"/nonstandard/chat" {
		t.Fatalf("result=%v", result)
	}
	models := result["models"].([]any)
	modelResults := result["model_results"].([]any)
	if len(models) != 1 || models[0] != "works" || len(modelResults) != 2 {
		t.Fatalf("partial model evidence=%v", result)
	}
	for _, path := range paths {
		if strings.Contains(path, "chat/completions") {
			t.Fatalf("full URL was modified: paths=%v", paths)
		}
	}
}

func TestProviderProtocolDiscoveryDoesNotProbeUnselectedCatalog(t *testing.T) {
	postCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "catalog-only"}}})
			return
		}
		postCount++
		http.Error(w, "unexpected probe", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
		"base_url": upstream.URL + "/v1", "secret": "probe-secret",
		"protocols": []map[string]any{{"protocol": "openai-chat"}},
	})
	requireStatus(t, rec, http.StatusOK)
	result := response["protocols"].([]any)[0].(map[string]any)
	if postCount != 0 || result["status"] != "unconfigured" {
		t.Fatalf("catalog caused live probes: posts=%d result=%v", postCount, result)
	}
}

func TestProviderProtocolDiscoveryRefreshesRuntimeOnlyWhenEvidenceChanges(t *testing.T) {
	newImageUpstream := func(t *testing.T) (*httptest.Server, *int) {
		t.Helper()
		postCount := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "upstream-model"}}})
				return
			}
			postCount++
			_, _ = w.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}],"usage":{}}`))
		}))
		t.Cleanup(upstream.Close)
		return upstream, &postCount
	}
	probe := func(t *testing.T, f *v2APIFixture, upstream *httptest.Server, providerID, model string, refresh bool) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		return f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
			"provider_id": providerID, "base_url": upstream.URL + "/v1", "secret": "probe-secret", "refresh": refresh,
			"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIImages, "models": []string{model}}},
		})
	}
	configureActiveImageEndpoint := func(f *v2APIFixture, baseURL string) {
		f.server.deps.Config.Providers[0].BaseURL = baseURL
		model := &f.server.deps.Config.Connections[0].Models[0]
		model.Protocols = append(model.Protocols, domain.ProtocolOpenAIImages)
	}
	refreshStatus := func(t *testing.T, response map[string]any) map[string]any {
		t.Helper()
		status, ok := response["runtime_refresh"].(map[string]any)
		if !ok {
			t.Fatalf("runtime_refresh missing: %v", response)
		}
		return status
	}

	t.Run("active provider evidence change refreshes once", func(t *testing.T) {
		upstream, postCount := newImageUpstream(t)
		f := newV2APIFixture(t)
		configureActiveImageEndpoint(f, upstream.URL+"/v1")
		refreshCalls := 0
		f.server.deps.RefreshRuntime = func(*config.File) error {
			refreshCalls++
			return nil
		}
		firstRec, first := probe(t, f, upstream, "provider-a", "upstream-model", false)
		requireStatus(t, firstRec, http.StatusOK)
		if got := refreshStatus(t, first)["status"]; got != "refreshed" || refreshCalls != 1 || *postCount != 1 {
			t.Fatalf("first refresh status=%v calls=%d posts=%d", got, refreshCalls, *postCount)
		}

		cacheRec, cached := probe(t, f, upstream, "provider-a", "upstream-model", false)
		requireStatus(t, cacheRec, http.StatusOK)
		if got := refreshStatus(t, cached)["status"]; got != "not_needed" || refreshCalls != 1 || *postCount != 1 {
			t.Fatalf("cache refresh status=%v calls=%d posts=%d", got, refreshCalls, *postCount)
		}
		if model := providerDiscoveryModelResult(t, cached); model["cache_hit"] != true {
			t.Fatalf("second discovery did not use evidence cache: %v", model)
		}

		forcedRec, forced := probe(t, f, upstream, "provider-a", "upstream-model", true)
		requireStatus(t, forcedRec, http.StatusOK)
		if got := refreshStatus(t, forced)["status"]; got != "not_needed" || refreshCalls != 1 || *postCount != 2 {
			t.Fatalf("unchanged forced refresh status=%v calls=%d posts=%d", got, refreshCalls, *postCount)
		}
	})

	t.Run("inactive provider evidence is not applicable", func(t *testing.T) {
		upstream, _ := newImageUpstream(t)
		f := newV2APIFixture(t)
		refreshCalls := 0
		f.server.deps.RefreshRuntime = func(*config.File) error {
			refreshCalls++
			return nil
		}
		rec, response := probe(t, f, upstream, "provider-not-active", "upstream-model", false)
		requireStatus(t, rec, http.StatusOK)
		if got := refreshStatus(t, response)["status"]; got != "not_applicable" || refreshCalls != 0 {
			t.Fatalf("inactive provider refresh status=%v calls=%d", got, refreshCalls)
		}
	})

	t.Run("active provider endpoint mismatch is not applicable", func(t *testing.T) {
		upstream, _ := newImageUpstream(t)
		f := newV2APIFixture(t)
		configureActiveImageEndpoint(f, "https://different.example/v1")
		refreshCalls := 0
		f.server.deps.RefreshRuntime = func(*config.File) error {
			refreshCalls++
			return nil
		}
		rec, response := probe(t, f, upstream, "provider-a", "upstream-model", false)
		requireStatus(t, rec, http.StatusOK)
		if got := refreshStatus(t, response)["status"]; got != "not_applicable" || refreshCalls != 0 {
			t.Fatalf("endpoint mismatch refresh status=%v calls=%d", got, refreshCalls)
		}
	})

	t.Run("active provider model mismatch is not applicable", func(t *testing.T) {
		upstream, _ := newImageUpstream(t)
		f := newV2APIFixture(t)
		configureActiveImageEndpoint(f, upstream.URL+"/v1")
		refreshCalls := 0
		f.server.deps.RefreshRuntime = func(*config.File) error {
			refreshCalls++
			return nil
		}
		rec, response := probe(t, f, upstream, "provider-a", "different-model", false)
		requireStatus(t, rec, http.StatusOK)
		if got := refreshStatus(t, response)["status"]; got != "not_applicable" || refreshCalls != 0 {
			t.Fatalf("model mismatch refresh status=%v calls=%d", got, refreshCalls)
		}
	})

	t.Run("active provider protocol mismatch is not applicable", func(t *testing.T) {
		upstream, _ := newImageUpstream(t)
		f := newV2APIFixture(t)
		f.server.deps.Config.Providers[0].BaseURL = upstream.URL + "/v1"
		refreshCalls := 0
		f.server.deps.RefreshRuntime = func(*config.File) error {
			refreshCalls++
			return nil
		}
		rec, response := probe(t, f, upstream, "provider-a", "upstream-model", false)
		requireStatus(t, rec, http.StatusOK)
		if got := refreshStatus(t, response)["status"]; got != "not_applicable" || refreshCalls != 0 {
			t.Fatalf("protocol mismatch refresh status=%v calls=%d", got, refreshCalls)
		}
	})

	t.Run("refresh failure fails the management request", func(t *testing.T) {
		upstream, _ := newImageUpstream(t)
		f := newV2APIFixture(t)
		configureActiveImageEndpoint(f, upstream.URL+"/v1")
		f.server.deps.RefreshRuntime = func(*config.File) error { return errors.New("compile failed") }
		rec, response := probe(t, f, upstream, "provider-a", "upstream-model", false)
		requireStatus(t, rec, http.StatusInternalServerError)
		refresh := refreshStatus(t, response)
		if refresh["status"] != "failed" || !strings.Contains(refresh["error"].(string), "compile failed") {
			t.Fatalf("failed refresh response=%v", refresh)
		}
	})

	t.Run("evidence store failure skips probes and runtime refresh", func(t *testing.T) {
		upstream, postCount := newImageUpstream(t)
		f := newV2APIFixture(t)
		configureActiveImageEndpoint(f, upstream.URL+"/v1")
		refreshCalls := 0
		f.server.deps.RefreshRuntime = func(*config.File) error {
			refreshCalls++
			return nil
		}
		if err := f.control.Close(); err != nil {
			t.Fatal(err)
		}
		rec, response := probe(t, f, upstream, "provider-a", "upstream-model", false)
		requireStatus(t, rec, http.StatusInternalServerError)
		refresh := refreshStatus(t, response)
		if refresh["status"] != "failed" || refresh["error"] != "capability evidence persistence failed" ||
			refreshCalls != 0 || *postCount != 0 {
			t.Fatalf("store failure refresh=%v calls=%d posts=%d", refresh, refreshCalls, *postCount)
		}
		model := providerDiscoveryModelResult(t, response)
		if model["status"] != "failed" || model["evidence_error"] == nil {
			t.Fatalf("store failure model result=%v", model)
		}
	})

	t.Run("evidence write failure stops remaining probes and runtime refresh", func(t *testing.T) {
		f := newV2APIFixture(t)
		postCount := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": []any{map[string]string{"id": "upstream-model"}},
				})
				return
			}
			postCount++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if postCount == 1 {
				if err := f.control.Close(); err != nil {
					t.Errorf("close evidence store: %v", err)
				}
			}
			writeOpenAIChatProbeSSE(w, "{\"ok\":true}")
		}))
		defer upstream.Close()
		refreshCalls := 0
		f.server.deps.RefreshRuntime = func(*config.File) error {
			refreshCalls++
			return nil
		}
		rec, response := f.do(http.MethodPost, "/api/providers/discover-protocols", map[string]any{
			"provider_id": "provider-a", "base_url": upstream.URL + "/v1", "secret": "probe-secret",
			"protocols": []map[string]any{{"protocol": domain.ProtocolOpenAIChat, "models": []string{"upstream-model"}}},
		})
		requireStatus(t, rec, http.StatusInternalServerError)
		refresh := refreshStatus(t, response)
		model := providerDiscoveryModelResult(t, response)
		if refresh["status"] != "failed" || refreshCalls != 0 || postCount != 1 || model["probe_requests"] != float64(1) {
			t.Fatalf("write failure refresh=%v calls=%d posts=%d model=%v", refresh, refreshCalls, postCount, model)
		}
		if model["status"] != "failed" || model["evidence_error"] == nil {
			t.Fatalf("write failure model result=%v", model)
		}
	})
}
