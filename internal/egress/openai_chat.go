package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"ferry/internal/domain"
)

// OpenAIChat adapts the /v1/chat/completions upstream protocol.
type OpenAIChat struct{ Base }

// NewOpenAIChat builds an adapter for one target.
func NewOpenAIChat(b Base) *OpenAIChat { return &OpenAIChat{Base: b} }

// ---------------------------------------------------------------------------
// wire types

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []oaToolCall    `json:"tool_calls,omitempty"`
	Reasoning  string          `json:"reasoning_content,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type oaRequest struct {
	Model           string          `json:"model"`
	Messages        []oaMessage     `json:"messages"`
	Tools           []oaTool        `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	ResponseFormat  *oaRespFormat   `json:"response_format,omitempty"`
	Stream          bool            `json:"stream"`
	ServiceTier     string          `json:"service_tier,omitempty"`
	StreamOptions   *oaStreamOpts   `json:"stream_options,omitempty"`
	MaxTokens       int64           `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	TopK            *int64          `json:"top_k,omitempty"`
	ParallelTools   *bool           `json:"parallel_tool_calls,omitempty"`
	Stop            any             `json:"stop,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        json.RawMessage `json:"thinking,omitempty"`
	PromptCacheKey  string          `json:"prompt_cache_key,omitempty"`
	Verbosity       string          `json:"verbosity,omitempty"`
	Native          map[string]any  `json:"-"`
}

type oaStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaRespFormat struct {
	Type       string        `json:"type"`
	JSONSchema *oaJSONSchema `json:"json_schema,omitempty"`
}

type oaJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict *bool           `json:"strict,omitempty"`
}

// buildRequest converts canonical → OpenAI Chat wire format.
// Returns compatibility notes describing every non-exact mapping.
func (o *OpenAIChat) buildRequest(rc *domain.RequestContract) ([]byte, []string, error) {
	body, notes, err := o.buildProtocolRequest(rc)
	if err != nil {
		return nil, notes, err
	}
	body, err = normalizeProviderRequest(o.Base, domain.ProtocolOpenAIChat, rc, body)
	return body, notes, err
}

func (o *OpenAIChat) buildProtocolRequest(rc *domain.RequestContract) ([]byte, []string, error) {
	_, zhipu := providerAdapter(o.Endpoint, o.Model, domain.ProtocolOpenAIChat).(zhipuProviderAdapter)
	if zhipu {
		if body, applicable, err := zhipuNativeChatBody(rc, o.Model); applicable {
			return body, nil, err
		}
	}
	// Phase 3 exact passthrough: a chat-ingress request aimed at a native
	// chat target replays the client's body verbatim (model and stream
	// fields patched to this target), so provider-specific fields the
	// canonical contract does not model survive untouched.
	if body := passthroughBody(rc, domain.ProtocolOpenAIChat, o.Model, nil); body != nil {
		return body, nil, nil
	}
	if err := rejectLossyConversion(rc, domain.ProtocolOpenAIChat); err != nil {
		return nil, nil, err
	}
	notes := canonicalRequestNotes(rc, domain.ProtocolOpenAIChat)
	req := oaRequest{
		Model:       o.Model,
		Stream:      rc.Stream,
		ServiceTier: rc.ServiceTier,
	}
	if rc.Stream && !zhipu {
		req.StreamOptions = &oaStreamOpts{IncludeUsage: true}
	}
	for _, m := range rc.Messages {
		hasToolResult := false
		hasRegularBlock := false
		for _, block := range m.Blocks {
			if block.Kind == domain.BlockToolResult {
				hasToolResult = true
				req.Messages = append(req.Messages, oaMessage{Role: "tool", ToolCallID: block.ID,
					Content: json.RawMessage(mustJSON(toolResultText(block.Result)))})
			} else {
				hasRegularBlock = true
			}
		}
		wm := oaMessage{Role: string(m.Role), Name: m.Name}
		for _, block := range m.Blocks {
			if block.Kind == domain.BlockThinking {
				wm.Reasoning += block.Text
			}
		}
		switch m.Role {
		case domain.RoleTool:
			wm.ToolCallID = m.ToolID
			wm.Content = textContent(m)
		default:
			if len(m.Blocks) == 1 && m.Blocks[0].Kind == domain.BlockText {
				wm.Content = json.RawMessage(mustJSON(m.Blocks[0].Text))
			} else {
				parts := make([]map[string]any, 0, len(m.Blocks))
				hasNonText := false
				for _, b := range m.Blocks {
					switch b.Kind {
					case domain.BlockText:
						parts = append(parts, map[string]any{"type": "text", "text": b.Text})
					case domain.BlockThinking:
						// Carried by the widely supported reasoning_content extension.
					case domain.BlockImage:
						hasNonText = true
						parts = append(parts, map[string]any{
							"type": "image_url",
							"image_url": map[string]string{
								"url": fmt.Sprintf("data:%s;base64,%s", b.ImageMIME, b64Encode(b.ImageData)),
							},
						})
					case domain.BlockToolUse:
						wm.ToolCalls = append(wm.ToolCalls, oaToolCallFromBlock(b))
					case domain.BlockToolResult:
						// Emitted above as one OpenAI tool message per result.
					}
				}
				if hasNonText || len(parts) > 1 || len(wm.ToolCalls) > 0 {
					if len(parts) > 0 {
						wm.Content = mustJSON(parts)
					} else if len(wm.ToolCalls) > 0 {
						wm.Content = nil
					}
				} else if len(parts) == 1 {
					wm.Content = json.RawMessage(mustJSON(parts[0]["text"]))
				}
			}
		}
		if hasToolResult && !hasRegularBlock {
			continue
		}
		req.Messages = append(req.Messages, wm)
	}
	for _, t := range rc.Tools {
		if zhipu && rc.ToolChoice == "none" {
			break
		}
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		req.Tools = append(req.Tools, ot)
	}
	switch rc.ToolChoice {
	case "", "auto":
	default:
		if zhipu {
			if rc.ToolChoice == "none" {
				break
			}
			return nil, nil, fmt.Errorf("zhipu chat compatibility: tool_choice=%q is unsupported; Zhipu Chat documents only auto", rc.ToolChoice)
		}
		if rc.ToolChoice == "none" || rc.ToolChoice == "required" {
			req.ToolChoice = rc.ToolChoice
		} else {
			req.ToolChoice = map[string]any{
				"type":     "function",
				"function": map[string]string{"name": rc.ToolChoice},
			}
		}
	}
	if rf := rc.ResponseFormat; rf != nil {
		if len(rf.Schema) > 0 {
			if zhipu {
				return nil, nil, fmt.Errorf("zhipu chat compatibility: response_format json_schema is unsupported; use json_object")
			}
			req.ResponseFormat = &oaRespFormat{Type: "json_schema",
				JSONSchema: &oaJSONSchema{Name: orDefault(rf.Name, "response"), Schema: rf.Schema, Strict: rf.Strict}}
		} else {
			req.ResponseFormat = &oaRespFormat{Type: "json_object"}
		}
	}
	if rc.Reasoning != nil {
		req.ReasoningEffort = rc.Reasoning.Effort
		if rc.Reasoning.BudgetTokens > 0 {
			req.ReasoningEffort = reasoningEffortForBudget(rc.Reasoning.BudgetTokens)
		}
		if zhipu && req.ReasoningEffort != "" {
			normalized := map[string]json.RawMessage{"reasoning_effort": mustJSON(req.ReasoningEffort)}
			if err := normalizeZhipuReasoning(normalized, o.Model); err != nil {
				return nil, nil, fmt.Errorf("zhipu chat compatibility: %w", err)
			}
			if raw, ok := normalized["reasoning_effort"]; ok {
				_ = json.Unmarshal(raw, &req.ReasoningEffort)
			} else {
				req.ReasoningEffort = ""
				if thinking, ok := normalized["thinking"]; ok {
					req.Thinking = thinking
				}
			}
		}
	}
	if rc.Lineage != nil && !zhipu {
		req.PromptCacheKey = rc.Lineage.PromptCacheKey
	}
	if verbosity := strings.TrimSpace(rc.Verbosity); verbosity != "" {
		req.Verbosity = verbosity
	}
	if len(rc.Metadata) > 0 {
		notes = append(notes, "metadata dropped (openai-chat has no standard metadata field)")
	}
	req.MaxTokens = rc.MaxTokens
	req.Temperature = rc.Temperature
	req.TopP = rc.TopP
	if zhipu && rc.TopK != nil {
		return nil, nil, fmt.Errorf("zhipu chat compatibility: top_k is unsupported by the documented Zhipu Chat API")
	}
	req.TopK = rc.TopK
	if zhipu && rc.ParallelTools != nil {
		if *rc.ParallelTools {
			return nil, nil, fmt.Errorf("zhipu chat compatibility: parallel_tool_calls=true is unsupported by this Zhipu Chat dialect")
		}
	} else {
		req.ParallelTools = rc.ParallelTools
	}
	if len(rc.StopSequences) == 1 {
		if zhipu {
			req.Stop = rc.StopSequences
		} else {
			req.Stop = rc.StopSequences[0]
		}
	} else if len(rc.StopSequences) > 1 {
		req.Stop = rc.StopSequences
	}
	return mustJSON(req), notes, nil
}

func oaToolCallFromBlock(b domain.Block) oaToolCall {
	var tc oaToolCall
	tc.ID = b.ID
	tc.Type = "function"
	tc.Function.Name = b.Name
	tc.Function.Arguments = string(b.Input)
	return tc
}

func textContent(m domain.Message) json.RawMessage {
	var sb strings2Builder
	for _, b := range m.Blocks {
		if b.Kind == domain.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return json.RawMessage(mustJSON(sb.String()))
}

func chatResponseFieldIssues(body []byte) []string {
	// Decode the envelope once. Field inspection does not interpret numeric values
	// or reserialize this tree; native bytes and the typed response stay unchanged.
	root, ok := decodeChatEnvelope(body)
	if !ok {
		return nil
	}
	return chatResponseObjectFieldIssues(root)
}

func decodeChatEnvelope(body []byte) (map[string]any, bool) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return root, true
}

func standardChatContent(root map[string]any, containerKey string) bool {
	choices, _ := root["choices"].([]any)
	for _, value := range choices {
		choice, _ := value.(map[string]any)
		container, _ := choice[containerKey].(map[string]any)
		content := container["content"]
		if _, isString := content.(string); content != nil && !isString {
			return false
		}
	}
	return true
}

// inspectChatResponse shares the decoded standard envelope between content
// validation and field inspection. Nonstandard content retains the original
// compatibility transformation and inspects the transformed bytes.
func inspectChatResponse(body []byte) ([]byte, bool, []string) {
	if root, ok := decodeChatEnvelope(body); ok && standardChatContent(root, "message") {
		return body, false, chatResponseObjectFieldIssues(root)
	}
	normalized, changed := normalizeNonstandardChatContent(body, "message")
	return normalized, changed, chatResponseFieldIssues(normalized)
}

func chatResponseObjectFieldIssues(root map[string]any) []string {
	object := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	array := func(v any) []any { a, _ := v.([]any); return a }
	issues := unknownObjectFieldIssues(root, "", "id", "object", "created", "model", "choices", "usage", "error", "service_tier", "system_fingerprint")
	usage := object(root["usage"])
	issues = append(issues, unknownObjectFieldIssues(usage, "usage", "prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details", "completion_tokens_details")...)
	issues = append(issues, unknownObjectFieldIssues(object(usage["prompt_tokens_details"]), "usage.prompt_tokens_details", "cached_tokens", "cache_write_tokens", "cache_creation_tokens", "audio_tokens", "image_tokens", "text_tokens")...)
	issues = append(issues, unknownObjectFieldIssues(object(usage["completion_tokens_details"]), "usage.completion_tokens_details", "reasoning_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens", "audio_tokens", "text_tokens", "compute_units")...)
	for index, value := range array(root["choices"]) {
		path := fmt.Sprintf("choices[%d]", index)
		choice := object(value)
		issues = append(issues, unknownObjectFieldIssues(choice, path, "index", "message", "finish_reason", "logprobs")...)
		if choice["logprobs"] != nil {
			issues = append(issues, "response field "+path+".logprobs is native-only")
		}
		message := object(choice["message"])
		issues = append(issues, unknownObjectFieldIssues(message, path+".message", "role", "content", "reasoning_content", "refusal", "audio", "annotations", "tool_calls")...)
		for partIndex, part := range array(message["content"]) {
			issues = append(issues, unknownObjectFieldIssues(object(part), fmt.Sprintf("%s.message.content[%d]", path, partIndex), "type", "text")...)
		}
		for callIndex, rawCall := range array(message["tool_calls"]) {
			callPath := fmt.Sprintf("%s.message.tool_calls[%d]", path, callIndex)
			call := object(rawCall)
			issues = append(issues, unknownObjectFieldIssues(call, callPath, "id", "type", "function")...)
			issues = append(issues, unknownObjectFieldIssues(object(call["function"]), callPath+".function", "name", "arguments")...)
		}
	}
	return issues
}

// normalizeChatChoiceContentShape repairs provider compatibility gaps in
// otherwise valid OpenAI-compatible Chat envelopes. Text wrappers are reduced
// to their text. Any other valid JSON content value is serialized as compact
// JSON text so a provider extension cannot make the entire response unusable.
func normalizeChatChoiceContentShape(body []byte, containerKey string) ([]byte, bool) {
	// The normal case needs no normalization. Inspect one decoded envelope,
	// using exact field names and preserving large numbers. Nonstandard values
	// still use the compatibility path below, including its original JSON text.
	if envelope, ok := decodeChatEnvelope(body); ok && standardChatContent(envelope, containerKey) {
		return body, false
	}
	return normalizeNonstandardChatContent(body, containerKey)
}

func normalizeNonstandardChatContent(body []byte, containerKey string) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	var choices []json.RawMessage
	if json.Unmarshal(root["choices"], &choices) != nil {
		return body, false
	}
	changed := false
	for index, rawChoice := range choices {
		var choice map[string]json.RawMessage
		if json.Unmarshal(rawChoice, &choice) != nil {
			continue
		}
		var container map[string]json.RawMessage
		if json.Unmarshal(choice[containerKey], &container) != nil {
			continue
		}
		rawContent, present := container["content"]
		if !present {
			continue
		}
		normalized, ok := normalizedChatTextContent(rawContent)
		if !ok {
			continue
		}
		container["content"] = normalized
		normalizedContainer, err := json.Marshal(container)
		if err != nil {
			continue
		}
		choice[containerKey] = normalizedContainer
		normalizedChoice, err := json.Marshal(choice)
		if err != nil {
			continue
		}
		choices[index] = normalizedChoice
		changed = true
	}
	if !changed {
		return body, false
	}
	normalizedChoices, err := json.Marshal(choices)
	if err != nil {
		return body, false
	}
	root["choices"] = normalizedChoices
	normalizedBody, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return normalizedBody, true
}

func normalizedChatTextContent(raw json.RawMessage) (json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, false
	}
	if value == nil {
		return nil, false
	}
	if _, isString := value.(string); isString {
		return nil, false
	}
	if text, ok := chatTextValue(value); ok {
		return mustJSON(text), true
	}

	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil, false
	}
	return mustJSON(compact.String()), true
}

func chatTextValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		return "", true
	case []any:
		if len(typed) == 0 {
			return "", false
		}
		var text bytes.Buffer
		for _, fragment := range typed {
			part, ok := chatTextValue(fragment)
			if !ok {
				return "", false
			}
			text.WriteString(part)
		}
		return text.String(), true
	case map[string]any:
		if contentType, present := typed["type"]; present {
			name, ok := contentType.(string)
			if !ok || (name != "text" && name != "output_text" && name != "input_text") {
				return "", false
			}
		}
		for _, key := range []string{"text", "content", "value", "parts"} {
			if nested, present := typed[key]; present {
				return chatTextValue(nested)
			}
		}
	}
	return "", false
}

func normalizeChatResponseContentShape(body []byte) ([]byte, bool) {
	return normalizeChatChoiceContentShape(body, "message")
}

func normalizeChatStreamContentShape(body []byte) ([]byte, bool) {
	return normalizeChatChoiceContentShape(body, "delta")
}

type strings2Builder struct{ b []byte }

func (s *strings2Builder) WriteString(p string) { s.b = append(s.b, p...) }
func (s *strings2Builder) String() string       { return string(s.b) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Invoke performs a non-streaming upstream call.
func (o *OpenAIChat) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	payload, _, err := o.buildRequest(rc)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "build request: " + err.Error()}
	}
	resp, f := o.doSend(ctx, "/chat/completions", payload,
		protocolHeaders(rc, domain.ProtocolOpenAIChat, nil))
	if f != nil {
		return nil, f
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Role        string          `json:"role"`
				Content     json.RawMessage `json:"content"`
				Reasoning   string          `json:"reasoning_content"`
				Refusal     string          `json:"refusal"`
				Audio       json.RawMessage `json:"audio"`
				Annotations json.RawMessage `json:"annotations"`
				ToolCalls   []oaToolCall    `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage       json.RawMessage `json:"usage"`
		Model       string          `json:"model"`
		ServiceTier string          `json:"service_tier"`
		Error       *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "read response: " + err.Error()}
	}
	var contentNormalized bool
	var fieldIssues []string
	rawBody, contentNormalized, fieldIssues = inspectChatResponse(rawBody)
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "decode response: " + err.Error()}
	}
	if out.Error != nil {
		message := "upstream error: " + out.Error.Message
		failureType := domain.ClassifyHTTP(http.StatusBadRequest, out.Error.Type+" "+out.Error.Message)
		return nil, upstreamFailure(failureType, 0, message)
	}
	r := &domain.Response{Model: out.Model, ServiceTier: out.ServiceTier, NativeBody: rawBody,
		NativeContentType: resp.Header.Get("Content-Type"), NativeProtocol: domain.ProtocolOpenAIChat}
	if contentNormalized {
		appendResponseWarning(r, "chat response content normalized from a non-standard JSON shape")
	}
	r.ConversionIssues = append(r.ConversionIssues, fieldIssues...)
	if len(out.Choices) == 0 {
		appendResponseValidationIssue(r, "chat response contains no choices")
	} else if len(out.Choices) > 1 {
		r.ConversionIssues = append(r.ConversionIssues, "multiple chat choices are native-only")
	}
	if len(out.Choices) > 0 {
		ch := out.Choices[0]
		if ch.Message.Role != "" && ch.Message.Role != "assistant" {
			appendResponseValidationIssue(r, "chat response message role is not assistant")
		}
		if ch.FinishReason == "" {
			appendResponseValidationIssue(r, "chat response is missing finish_reason")
		}
		var text string
		if json.Unmarshal(ch.Message.Content, &text) == nil {
			if text != "" {
				r.Content = append(r.Content, domain.Block{Kind: domain.BlockText, Text: text})
			}
		} else if len(ch.Message.Content) > 0 && string(ch.Message.Content) != "null" {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(ch.Message.Content, &parts) == nil {
				for _, part := range parts {
					if (part.Type == "text" || part.Type == "output_text") && part.Text != "" {
						r.Content = append(r.Content, domain.Block{Kind: domain.BlockText, Text: part.Text})
					} else {
						r.ConversionIssues = append(r.ConversionIssues, "chat content type "+part.Type+" is native-only")
					}
				}
			} else {
				appendResponseValidationIssue(r, "chat response content has an invalid shape")
			}
		}
		if ch.Message.Reasoning != "" {
			r.Content = append(r.Content, domain.Block{Kind: domain.BlockThinking, Text: ch.Message.Reasoning})
			if rc.Origin == domain.ProtocolAnthropic {
				r.ConversionIssues = append(r.ConversionIssues,
					"unsigned chat reasoning cannot be represented as Anthropic thinking")
			}
		}
		if ch.Message.Refusal != "" {
			r.Content = append(r.Content, domain.Block{Kind: domain.BlockText, Text: ch.Message.Refusal})
			r.ConversionIssues = append(r.ConversionIssues, "chat refusal is native-only")
		}
		if len(ch.Message.Audio) > 0 && string(ch.Message.Audio) != "null" {
			r.ConversionIssues = append(r.ConversionIssues, "chat audio response is native-only")
		}
		if len(ch.Message.Annotations) > 0 && string(ch.Message.Annotations) != "null" && string(ch.Message.Annotations) != "[]" {
			r.ConversionIssues = append(r.ConversionIssues, "chat annotations are native-only")
		}
		for _, tc := range ch.Message.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				r.ConversionIssues = append(r.ConversionIssues,
					"chat tool call type "+tc.Type+" is native-only")
			}
			r.Content = append(r.Content, domain.Block{
				Kind: domain.BlockToolUse, ID: tc.ID, Name: tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			})
		}
		r.FinishReason = mapFinish(ch.FinishReason)
		if ch.FinishReason != "" && ch.FinishReason != "stop" && ch.FinishReason != "length" &&
			ch.FinishReason != "tool_calls" && ch.FinishReason != "function_call" {
			r.ConversionIssues = append(r.ConversionIssues,
				"chat finish reason "+ch.FinishReason+" is native-only")
		}
	}
	r.Usage = decodeUsage(out.Usage)
	markResponseConversionIssues(r)
	return r, nil
}

func mapFinish(s string) string {
	switch s {
	case "tool_calls", "function_call":
		return "tool_call"
	case "length":
		return "length"
	default:
		return "stop"
	}
}

// Stream performs a streaming upstream call.
func (o *OpenAIChat) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	rc.Stream = true
	payload, _, err := o.buildRequest(rc)
	if err != nil {
		return nil, nil, &domain.Failure{Type: domain.FailBadResponse, Message: "build request: " + err.Error()}
	}
	resp, f := o.doSend(ctx, "/chat/completions", payload,
		protocolHeaders(rc, domain.ProtocolOpenAIChat, map[string]string{"Accept": "text/event-stream"}))
	if f != nil {
		return nil, nil, f
	}
	ch := make(chan domain.StreamEvent, 64)
	cancel := func() { resp.Body.Close() } // #nosec G104 -- best-effort close of a finished or abandoned upstream body
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		finish := ""
		ended := false
		type streamTool struct {
			id, name, arguments string
			started, ended      bool
		}
		activeTools := map[int]*streamTool{}
		activeOrder := []int{}
		choiceIndex := 0
		choiceSeen := false
		var responseModel, responseTier string
		var lastUsage *domain.Usage
		nativePass := rc.Origin == domain.ProtocolOpenAIChat && len(rc.Native) > 0
		finishTools := func() *domain.Failure {
			if len(activeOrder) == 0 && (finish == "tool_calls" || finish == "function_call") {
				return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
					Message: "chat stream finished with tool_calls but emitted no tool call"}
			}
			if len(activeOrder) > 0 && finish != "tool_calls" && finish != "function_call" {
				return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
					Message: "chat stream emitted tool calls without a tool_calls finish reason"}
			}
			for _, toolIndex := range activeOrder {
				tool := activeTools[toolIndex]
				if tool == nil || !tool.started {
					return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
						Message: "chat tool call is missing its id or name"}
				}
				if tool.arguments == "" {
					return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
						Message: "chat tool call is missing its arguments"}
				}
				if !domain.ValidJSONObject(json.RawMessage(tool.arguments)) {
					return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
						Message: "chat tool call arguments are not a JSON object"}
				}
				if !tool.ended {
					tool.ended = true
					ch <- domain.StreamEvent{Kind: domain.EvToolEnd, Index: toolIndex}
				}
			}
			return nil
		}
		err := readSSE(resp.Body, func(ev sseEvent) error {
			if ev.data == "[DONE]" {
				if finish == "" {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
						Message: "chat stream ended without a finish_reason"}}
					return ioEnd
				}
				if failure := finishTools(); failure != nil {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: failure}
					return ioEnd
				}
				if nativePass {
					ch <- nativeStreamEvent(domain.ProtocolOpenAIChat, ev, true)
				}
				if !ended {
					ch <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: mapFinish(finish), Model: responseModel, ServiceTier: responseTier, Usage: lastUsage}
					ended = true
				}
				return ioEnd
			}
			normalizedData, _ := normalizeChatStreamContentShape([]byte(ev.data))
			ev.data = string(normalizedData)
			var chunk struct {
				Model       string `json:"model"`
				ServiceTier string `json:"service_tier"`
				Choices     []struct {
					Index int `json:"index"`
					Delta struct {
						Role              string          `json:"role"`
						Content           string          `json:"content"`
						Reasoning         string          `json:"reasoning_content"`
						ExtendedReasoning string          `json:"reasoning"`
						ReasoningDetails  json.RawMessage `json:"reasoning_details"`
						Refusal           string          `json:"refusal"`
						Audio             json.RawMessage `json:"audio"`
						ToolCalls         []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *json.RawMessage `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
					Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
					Message: "chat stream emitted invalid JSON"}}
				return ioEnd
			}
			if chunk.Model != "" {
				responseModel = chunk.Model
			}
			if chunk.ServiceTier != "" {
				responseTier = chunk.ServiceTier
			}
			if chunk.Model != "" || chunk.ServiceTier != "" {
				ch <- domain.StreamEvent{Kind: domain.EvStart, Model: responseModel, ServiceTier: responseTier}
			}
			terminal := chunk.Error != nil
			// Chat reasoning extensions have no lossless canonical equivalent (notably
			// encrypted details). Recognize progress while forwarding the original
			// frame, rather than counting valid reasoning as empty precommit data.
			meaningfulReasoning := false
			nativeProgressSafe := true
			for _, choice := range chunk.Choices {
				if choice.Delta.Role != "" && choice.Delta.Role != "assistant" {
					nativeProgressSafe = false
					continue // Role validation below still fails closed.
				}
				if len(choice.Delta.ToolCalls) > 0 {
					// Tool identity and lifecycle must pass canonical validation first.
					nativeProgressSafe = false
				}
				if domain.MeaningfulChatReasoning(choice.Delta.ExtendedReasoning, choice.Delta.ReasoningDetails) {
					meaningfulReasoning = true
				}
			}
			if !terminal && meaningfulReasoning && !nativePass {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
					Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
					Message: "Chat reasoning extensions require native Chat passthrough"}}
				return ioEnd
			}
			if nativePass {
				native := nativeStreamEvent(domain.ProtocolOpenAIChat, ev, terminal)
				native.NativeFrame.Meaningful = !terminal && meaningfulReasoning && nativeProgressSafe
				ch <- native
			}
			if chunk.Error != nil {
				failureType := domain.ClassifyHTTP(0, chunk.Error.Message)
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: upstreamFailure(failureType, 0, chunk.Error.Message)}
				return ioEnd
			}
			if len(chunk.Choices) > 1 && !nativePass {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
					Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
					Message: "multiple Chat completion choices cannot be losslessly converted"}}
				return ioEnd
			}
			for _, c := range chunk.Choices {
				if c.Delta.Role != "" && c.Delta.Role != "assistant" {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
						Message: "chat stream delta role is not assistant"}}
					return ioEnd
				}
				if !nativePass {
					if choiceSeen && choiceIndex != c.Index {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "multiple Chat completion choices cannot be losslessly converted"}}
						return ioEnd
					}
					choiceIndex = c.Index
					choiceSeen = true
				}
				if c.Delta.Reasoning != "" {
					if rc.Origin == domain.ProtocolAnthropic && !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "unsigned chat reasoning cannot be represented as Anthropic thinking"}}
						return ioEnd
					}
					ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta, ThinkingDelta: c.Delta.Reasoning}
				}
				if c.Delta.Content != "" {
					ch <- domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: c.Delta.Content}
				}
				if c.Delta.Refusal != "" {
					if !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "chat refusal cannot be losslessly converted"}}
						return ioEnd
					}
				}
				if len(c.Delta.Audio) > 0 && string(c.Delta.Audio) != "null" && !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "chat audio stream cannot be losslessly converted"}}
					return ioEnd
				}
				for _, tc := range c.Delta.ToolCalls {
					tool := activeTools[tc.Index]
					if tool == nil {
						tool = &streamTool{}
						activeTools[tc.Index] = tool
						activeOrder = append(activeOrder, tc.Index)
					}
					if tc.ID != "" {
						if tool.started && tc.ID != tool.id && !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "chat tool call id changed after the call started"}}
							return ioEnd
						}
						tool.id = tc.ID
					}
					if tc.Function.Name != "" {
						if tool.started && tc.Function.Name != tool.name && !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "chat tool name changed after the call started"}}
							return ioEnd
						}
						tool.name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						tool.arguments += tc.Function.Arguments
					}
					if !tool.started && tool.id != "" && tool.name != "" {
						tool.started = true
						ch <- domain.StreamEvent{Kind: domain.EvToolStart, Index: tc.Index,
							ToolID: tool.id, ToolName: tool.name}
						if tool.arguments != "" {
							ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: tc.Index,
								ArgsDelta: tool.arguments}
						}
					} else if tool.started && tc.Function.Arguments != "" {
						ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: tc.Index,
							ArgsDelta: tc.Function.Arguments}
					}
				}
				if c.FinishReason != nil {
					finish = *c.FinishReason
					if finish != "stop" && finish != "length" && finish != "tool_calls" &&
						finish != "function_call" && !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "chat finish reason " + finish + " cannot be losslessly converted"}}
						return ioEnd
					}
					if failure := finishTools(); failure != nil {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: failure}
						return ioEnd
					}
				}
			}
			if chunk.Usage != nil {
				u := decodeUsage(*chunk.Usage)
				lastUsage = &u
				if !nativePass && !ended {
					if finish == "" {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
							Message: "chat stream emitted usage before a terminal finish reason"}}
						return ioEnd
					}
					ch <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: mapFinish(finish), Model: responseModel, ServiceTier: responseTier, Usage: &u}
					ended = true
					return ioEnd
				}
			}
			return nil
		})
		if err == ioEnd {
			return
		}
		if err != nil {
			ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
				Type: domain.FailStreamBroken, Message: err.Error()}}
		} else if !ended && finish == "" {
			ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
				Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
				Message: "chat stream closed without [DONE] or a terminal finish reason"}}
		} else if !ended {
			if failure := finishTools(); failure != nil {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: failure}
				return
			}
			ch <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: mapFinish(finish), Model: responseModel, ServiceTier: responseTier, Usage: lastUsage}
		}
	}()
	return ch, cancel, nil
}

var ioEnd = errEOF{}

type errEOF struct{}

func (errEOF) Error() string { return "eof" }

var _ = http.StatusOK
