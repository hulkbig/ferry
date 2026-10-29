package ingress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
)

// OpenAIChatHandler serves POST /v1/chat/completions.
type OpenAIChatHandler struct{ Eng Engine }

func (h *OpenAIChatHandler) Protocol() domain.Protocol { return domain.ProtocolOpenAIChat }

// ---------------------------------------------------------------------------
// wire types (request side mirrors the OpenAI Chat Completions schema)

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	Reasoning  string          `json:"reasoning_content,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Tools          []chatTool      `json:"tools,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat *chatRespFormat `json:"response_format,omitempty"`
	Stream         bool            `json:"stream"`
	StreamOptions  *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	MaxTokens       int64           `json:"max_tokens,omitempty"`
	MaxCompletion   int64           `json:"max_completion_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	TopK            *int64          `json:"top_k,omitempty"`
	ParallelTools   *bool           `json:"parallel_tool_calls,omitempty"`
	Stop            json.RawMessage `json:"stop,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	PromptCacheKey  string          `json:"prompt_cache_key,omitempty"`
	Verbosity       string          `json:"verbosity,omitempty"`
	ServiceTier     string          `json:"service_tier,omitempty"`
	// Aux sampling, logging, and optimization knobs have no canonical
	// equivalent; they are translated to the closest target support and
	// reported as mapping notes instead of blocking cross-protocol routing.
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	LogitBias        map[string]any  `json:"logit_bias,omitempty"`
	Logprobs         *bool           `json:"logprobs,omitempty"`
	TopLogprobs      *int64          `json:"top_logprobs,omitempty"`
	User             string          `json:"user,omitempty"`
	Store            *bool           `json:"store,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	Prediction       json.RawMessage `json:"prediction,omitempty"`
	WebSearchOptions json.RawMessage `json:"web_search_options,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRespFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict,omitempty"`
	} `json:"json_schema,omitempty"`
}

func chatNestedConversionIssues(body []byte) []string {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	var issues []string
	var messages []json.RawMessage
	_ = json.Unmarshal(root["messages"], &messages)
	for index, rawMessage := range messages {
		path := fmt.Sprintf("messages[%d]", index)
		issues = append(issues, unknownObjectFields(rawMessage, path,
			"role", "content", "name", "tool_call_id", "tool_calls", "reasoning_content")...)
		var message map[string]json.RawMessage
		_ = json.Unmarshal(rawMessage, &message)
		var parts []json.RawMessage
		if json.Unmarshal(message["content"], &parts) == nil {
			for partIndex, rawPart := range parts {
				partPath := fmt.Sprintf("%s.content[%d]", path, partIndex)
				issues = append(issues, unknownObjectFields(rawPart, partPath,
					"type", "text", "image_url")...)
				var part map[string]json.RawMessage
				_ = json.Unmarshal(rawPart, &part)
				if image := part["image_url"]; len(image) > 0 {
					issues = append(issues, unknownObjectFields(image, partPath+".image_url", "url")...)
				}
			}
		}
		var calls []json.RawMessage
		_ = json.Unmarshal(message["tool_calls"], &calls)
		for callIndex, rawCall := range calls {
			callPath := fmt.Sprintf("%s.tool_calls[%d]", path, callIndex)
			issues = append(issues, unknownObjectFields(rawCall, callPath, "id", "type", "function")...)
			var call map[string]json.RawMessage
			_ = json.Unmarshal(rawCall, &call)
			issues = append(issues, unknownObjectFields(call["function"], callPath+".function",
				"name", "arguments")...)
		}
	}
	var tools []json.RawMessage
	_ = json.Unmarshal(root["tools"], &tools)
	for index, rawTool := range tools {
		path := fmt.Sprintf("tools[%d]", index)
		issues = append(issues, unknownObjectFields(rawTool, path, "type", "function")...)
		var tool map[string]json.RawMessage
		_ = json.Unmarshal(rawTool, &tool)
		issues = append(issues, unknownObjectFields(tool["function"], path+".function",
			"name", "description", "parameters")...)
	}
	if raw := root["tool_choice"]; len(raw) > 0 && raw[0] == '{' {
		issues = append(issues, unknownObjectFields(raw, "tool_choice", "type", "function")...)
		var choice map[string]json.RawMessage
		_ = json.Unmarshal(raw, &choice)
		issues = append(issues, unknownObjectFields(choice["function"], "tool_choice.function", "name")...)
	}
	if raw := root["response_format"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "response_format", "type", "json_schema")...)
		var format map[string]json.RawMessage
		_ = json.Unmarshal(raw, &format)
		issues = append(issues, unknownObjectFields(format["json_schema"],
			"response_format.json_schema", "name", "schema", "strict")...)
	}
	if raw := root["stream_options"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "stream_options", "include_usage")...)
	}
	return issues
}

// toContract converts the wire request into canonical form. Native keeps the
// raw body so same-protocol egress achieves Exact compatibility.
func (h *OpenAIChatHandler) toContract(body []byte) (*domain.RequestContract, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, perrf("invalid JSON body: %v", err)
	}
	if len(req.Messages) == 0 {
		return nil, perrf("messages is required")
	}
	tier, err := domain.NormalizeServiceTier(req.ServiceTier)
	if err != nil {
		return nil, perrf("%s", err)
	}
	rc := &domain.RequestContract{
		Protocol:    domain.ProtocolOpenAIChat,
		Origin:      domain.ProtocolOpenAIChat,
		Model:       req.Model,
		Stream:      req.Stream,
		Native:      json.RawMessage(body),
		ServiceTier: tier,
		ConversionIssues: unknownTopLevelFields(body,
			"model", "messages", "tools", "tool_choice", "response_format", "stream", "stream_options",
			"max_tokens", "max_completion_tokens", "temperature", "top_p", "top_k", "parallel_tool_calls",
			"stop", "reasoning_effort", "prompt_cache_key", "verbosity",
			"frequency_penalty", "presence_penalty", "seed", "logit_bias", "logprobs", "top_logprobs",
			"user", "store", "metadata", "prediction", "service_tier", "web_search_options"),
	}
	if verbosity := strings.TrimSpace(req.Verbosity); verbosity != "" {
		rc.Verbosity = verbosity
	}
	if req.FrequencyPenalty != nil || req.PresencePenalty != nil {
		rc.MappingNotes = append(rc.MappingNotes,
			"frequency/presence penalties omitted (target sampling keeps defaults)")
	}
	if req.Seed != nil {
		rc.MappingNotes = append(rc.MappingNotes, "seed omitted (target does not accept deterministic seeds)")
	}
	if req.LogitBias != nil {
		rc.MappingNotes = append(rc.MappingNotes, "logit_bias omitted (no target equivalent)")
	}
	if req.Logprobs != nil || req.TopLogprobs != nil {
		rc.MappingNotes = append(rc.MappingNotes, "logprobs request omitted (no target equivalent)")
	}
	if req.User != "" || len(req.Metadata) > 0 {
		rc.MappingNotes = append(rc.MappingNotes, "client identity metadata omitted (no target equivalent)")
	}
	if req.Store != nil || len(req.Prediction) > 0 || len(req.WebSearchOptions) > 0 {
		rc.MappingNotes = append(rc.MappingNotes, "provider-side request options omitted (no target equivalent)")
	}
	if req.StreamOptions != nil {
		rc.StreamIncludeUsage = req.StreamOptions.IncludeUsage
	}
	rc.ConversionIssues = append(rc.ConversionIssues, chatNestedConversionIssues(body)...)
	if req.PromptCacheKey != "" {
		rc.Lineage = &domain.ResponsesLineage{PromptCacheKey: req.PromptCacheKey}
	}
	for _, m := range req.Messages {
		role := domain.Role(m.Role)
		if m.Role == "developer" {
			role = domain.RoleSystem
			rc.MappingNotes = append(rc.MappingNotes, "developer role mapped to system")
		} else if m.Role != string(domain.RoleSystem) && m.Role != string(domain.RoleUser) &&
			m.Role != string(domain.RoleAssistant) && m.Role != string(domain.RoleTool) {
			rc.ConversionIssues = append(rc.ConversionIssues,
				"message role "+m.Role+" is native-only")
		}
		// Some OpenAI-compatible clients echo the function name on a tool
		// result in addition to tool_call_id. The Chat schema associates tool
		// results by tool_call_id, and Responses/Anthropic use the same call ID
		// as their authoritative correlation key. Treat the echoed name as
		// redundant metadata so it cannot make an otherwise lossless tool
		// continuation ineligible for cross-protocol routing.
		canonicalName := m.Name
		if m.Role == string(domain.RoleTool) && m.ToolCallID != "" {
			canonicalName = ""
		} else if m.Name != "" {
			rc.MappingNotes = append(rc.MappingNotes, "message sender name omitted")
		}
		cm := domain.Message{Role: role, Name: canonicalName, ToolID: m.ToolCallID}
		if err := decodeChatContent(m.Content, &cm); err != nil {
			return nil, err
		}
		if m.Reasoning != "" {
			cm.Blocks = append(cm.Blocks, domain.Block{Kind: domain.BlockThinking, Text: m.Reasoning})
		}
		for _, tc := range m.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				rc.ConversionIssues = append(rc.ConversionIssues,
					"tool call type "+tc.Type+" is native-only")
			}
			args := tc.Function.Arguments
			// OpenAI carries arguments as a JSON-encoded string; unwrap it so
			// the canonical contract always holds object-shaped input.
			var argStr string
			if err := json.Unmarshal(args, &argStr); err == nil {
				args = json.RawMessage(argStr)
			}
			cm.Blocks = append(cm.Blocks, domain.Block{
				Kind: domain.BlockToolUse, ID: tc.ID,
				Name: tc.Function.Name, Input: args,
			})
		}
		rc.Messages = append(rc.Messages, cm)
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			rc.ConversionIssues = append(rc.ConversionIssues,
				"tool type "+t.Type+" is native-only")
		}
		rc.Tools = append(rc.Tools, domain.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var s string
		if err := json.Unmarshal(req.ToolChoice, &s); err == nil {
			rc.ToolChoice = s
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
				return nil, perrf("unsupported tool_choice shape")
			}
			rc.ToolChoice = obj.Function.Name
		}
	}
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "text":
		case "json_object":
			rc.ResponseFormat = &domain.ResponseFormat{Name: "response"}
		case "json_schema":
			if rf.JSONSchema == nil || len(rf.JSONSchema.Schema) == 0 {
				return nil, perrf("response_format.json_schema.schema is required")
			}
			name := rf.JSONSchema.Name
			if name == "" {
				name = "response"
			}
			rc.ResponseFormat = &domain.ResponseFormat{Name: name, Schema: rf.JSONSchema.Schema, Strict: rf.JSONSchema.Strict}
		default:
			return nil, perrf("unsupported response_format.type %q", rf.Type)
		}
	}
	if req.ReasoningEffort != "" {
		rc.Reasoning = &domain.ReasoningSpec{Effort: req.ReasoningEffort}
	}
	rc.MaxTokens = req.MaxTokens
	if rc.MaxTokens == 0 {
		rc.MaxTokens = req.MaxCompletion
	} else if req.MaxCompletion > 0 && req.MaxCompletion != req.MaxTokens {
		rc.ConversionIssues = append(rc.ConversionIssues,
			"max_tokens and max_completion_tokens disagree")
	}
	rc.Temperature = req.Temperature
	rc.TopP = req.TopP
	rc.TopK = req.TopK
	rc.ParallelTools = req.ParallelTools
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		var one string
		if err := json.Unmarshal(req.Stop, &one); err == nil {
			rc.StopSequences = []string{one}
		} else {
			var many []string
			if err := json.Unmarshal(req.Stop, &many); err != nil {
				return nil, perrf("stop must be string or array of strings")
			}
			rc.StopSequences = many
		}
	}
	markOpaqueConversionIssues(rc)
	return rc, nil
}

// decodeChatContent accepts string or content-part-array forms.
func decodeChatContent(raw json.RawMessage, cm *domain.Message) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			cm.Blocks = append(cm.Blocks, domain.Block{Kind: domain.BlockText, Text: s})
		}
		return nil
	}
	var rawParts []json.RawMessage
	if err := json.Unmarshal(raw, &rawParts); err != nil {
		return perrf("message.content: invalid shape")
	}
	for _, rawPart := range rawParts {
		var p struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(rawPart, &p); err != nil {
			return perrf("message.content: invalid part")
		}
		switch p.Type {
		case "text":
			cm.Blocks = append(cm.Blocks, domain.Block{Kind: domain.BlockText, Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				cm.Blocks = append(cm.Blocks, domain.Block{Kind: domain.BlockOpaque,
					OpaqueType: p.Type, Opaque: append(json.RawMessage(nil), rawPart...)})
				continue
			}
			mime, data, ok := parseDataURL(p.ImageURL.URL)
			if !ok {
				cm.Blocks = append(cm.Blocks, domain.Block{Kind: domain.BlockOpaque,
					OpaqueType: p.Type, Opaque: append(json.RawMessage(nil), rawPart...)})
				continue
			}
			cm.Blocks = append(cm.Blocks, domain.Block{
				Kind: domain.BlockImage, ImageMIME: mime, ImageData: data})
		default:
			cm.Blocks = append(cm.Blocks, domain.Block{Kind: domain.BlockOpaque,
				OpaqueType: p.Type, Opaque: append(json.RawMessage(nil), rawPart...)})
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// response side

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatRespMsg `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatRespMsg struct {
	Role      string         `json:"role"`
	Content   any            `json:"content"` // string or null when tool calls
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
	Reasoning string         `json:"reasoning_content,omitempty"`
}

type chatResponse struct {
	ServiceTier string       `json:"service_tier,omitempty"`
	ID          string       `json:"id"`
	Object      string       `json:"object"`
	Created     int64        `json:"created"`
	Model       string       `json:"model"`
	Choices     []chatChoice `json:"choices"`
	Usage       oaUsage      `json:"usage"`
}

func chatResponseFrom(res *sched.Result, r *domain.Response) chatResponse {
	msg := chatRespMsg{Role: "assistant"}
	var text []byte
	var reasoning string
	var tools []chatToolCall
	for _, b := range r.Content {
		switch b.Kind {
		case domain.BlockText:
			text = append(text, b.Text...)
		case domain.BlockThinking:
			reasoning += b.Text
		case domain.BlockToolUse:
			tc := chatToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name = b.Name
			// re-encode object input as the JSON string the wire expects
			argsJSON, err := json.Marshal(string(b.Input))
			if err != nil {
				argsJSON = []byte(`"{}"`)
			}
			tc.Function.Arguments = argsJSON
			tools = append(tools, tc)
		}
	}
	if len(text) > 0 {
		msg.Content = string(text)
	} else if len(tools) > 0 {
		msg.Content = nil
	} else {
		msg.Content = ""
	}
	msg.Reasoning = reasoning
	msg.ToolCalls = tools
	return chatResponse{
		ID: res.RequestID, Object: "chat.completion",
		Created: timeNowUnix(), Model: res.Model,
		Choices: []chatChoice{{Index: 0, Message: msg, FinishReason: mapFinishOut(r.FinishReason)}},
		Usage:   oaUsageFrom(r.Usage), ServiceTier: r.ServiceTier,
	}
}

func mapFinishOut(canonical string) string {
	switch canonical {
	case "tool_call":
		return "tool_calls"
	case "length":
		return "length"
	default:
		return "stop"
	}
}

func normalizeArgs(raw json.RawMessage) json.RawMessage {
	return raw
}

// ---------------------------------------------------------------------------
// streaming wire types

type chatChunk struct {
	ServiceTier string   `json:"service_tier,omitempty"`
	ID          string   `json:"id"`
	Object      string   `json:"object"`
	Created     int64    `json:"created"`
	Model       string   `json:"model"`
	Choices     []chatD  `json:"choices"`
	Usage       *oaUsage `json:"usage,omitempty"`
}

type chatD struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []chatDeltaTC `json:"tool_calls,omitempty"`
}

type chatDeltaTC struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// ---------------------------------------------------------------------------
// HTTP entry

func (h *OpenAIChatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w, r, finishObservation := observeIngress(h.Eng, domain.ProtocolOpenAIChat, w, r)
	defer finishObservation()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	observeIngressPhase(r.Context(), "BODY_PARSE_STARTED", nil)
	rc, err := h.toContract(body)
	finishIngressPhase(r.Context(), "BODY_PARSE", err)
	if err != nil {
		var pe *parseError
		status := http.StatusBadRequest
		if !errors.As(err, &pe) {
			status = http.StatusInternalServerError
		}
		writeError(w, status, "invalid_request_error", err.Error())
		return
	}
	captureProtocolHeaders(r, rc)
	meta := requestMeta(r, rc)

	res, f := h.Eng.Execute(r.Context(), rc, meta)
	if f != nil {
		writeFailure(w, f)
		return
	}
	if rc.Stream {
		writeServiceTierHeaders(w, res)
		streamChat(w, res, rc.StreamIncludeUsage)
		return
	}
	if conversionFailure := responseConversionFailure(res, domain.ProtocolOpenAIChat); conversionFailure != nil {
		writeFailure(w, conversionFailure)
		return
	}
	writeResponseWarnings(w, res)
	writeServiceTierHeaders(w, res)
	if !writeNativeResponse(w, res, domain.ProtocolOpenAIChat) {
		writeJSON(w, http.StatusOK, chatResponseFrom(res, res.Response))
	}
}

// streamChat pumps canonical events into OpenAI-style SSE chunks.
func streamChat(w http.ResponseWriter, res *sched.Result, includeUsage bool) {
	sw := newSSEWriter(w)
	nativeMode := false
	nativeTerminal := false
	first := true
	idxByTool := map[int]int{} // canonical block index -> wire tool index
	nextTool := 0
	for ev := range res.Events {
		if ev.Kind == domain.EvNative {
			if ev.NativeProtocol == domain.ProtocolOpenAIChat && ev.NativeFrame != nil {
				nativeMode = true
				nativeTerminal = nativeTerminal || ev.NativeFrame.Terminal
				writeNativeFrame(sw, res, domain.ProtocolOpenAIChat, ev.NativeFrame)
			}
			continue
		}
		if nativeMode {
			if ev.Kind == domain.EvEnd || (ev.Kind == domain.EvError && nativeTerminal) {
				return
			}
			if ev.Kind != domain.EvError {
				continue
			}
		}
		switch ev.Kind {
		case domain.EvTextDelta:
			if first {
				first = false
				sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
					Created: timeNowUnix(), Model: res.Model,
					Choices: []chatD{{Delta: chatDelta{Role: "assistant"}}}})
			}
			sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
				Created: timeNowUnix(), Model: res.Model,
				Choices: []chatD{{Delta: chatDelta{Content: ev.TextDelta}}}})
		case domain.EvThinkingDelta:
			if first {
				first = false
				sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
					Created: timeNowUnix(), Model: res.Model,
					Choices: []chatD{{Delta: chatDelta{Role: "assistant"}}}})
			}
			sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
				Created: timeNowUnix(), Model: res.Model,
				Choices: []chatD{{Delta: chatDelta{ReasoningContent: ev.ThinkingDelta}}}})
		case domain.EvToolStart:
			wi := nextTool
			nextTool++
			idxByTool[ev.Index] = wi
			if first {
				first = false
				sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
					Created: timeNowUnix(), Model: res.Model,
					Choices: []chatD{{Delta: chatDelta{Role: "assistant"}}}})
			}
			d := chatDelta{}
			tc := chatDeltaTC{Index: wi, ID: ev.ToolID, Type: "function"}
			tc.Function.Name = ev.ToolName
			d.ToolCalls = []chatDeltaTC{tc}
			sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
				Created: timeNowUnix(), Model: res.Model,
				Choices: []chatD{{Delta: d}}})
		case domain.EvToolDelta:
			wi, ok := idxByTool[ev.Index]
			if !ok {
				wi = nextTool
				nextTool++
				idxByTool[ev.Index] = wi
			}
			d := chatDelta{}
			tc := chatDeltaTC{Index: wi}
			tc.Function.Arguments = ev.ArgsDelta
			d.ToolCalls = []chatDeltaTC{tc}
			sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
				Created: timeNowUnix(), Model: res.Model,
				Choices: []chatD{{Delta: d}}})
		case domain.EvEnd:
			fr := mapFinishOut(ev.FinishReason)
			chunk := chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
				Created: timeNowUnix(), Model: res.Model,
				Choices: []chatD{{FinishReason: &fr}}, ServiceTier: ev.ServiceTier,
			}
			sw.data(chunk)
			if includeUsage && ev.Usage != nil {
				u := oaUsageFrom(*ev.Usage)
				sw.data(chatChunk{ID: res.RequestID, Object: "chat.completion.chunk",
					Created: timeNowUnix(), Model: res.Model, Choices: []chatD{}, Usage: &u})
			}
			sw.done()
			return
		case domain.EvError:
			// committed stream: error travels in-band as a terminal chunk
			msg := "upstream stream failed"
			code := "upstream_error"
			if ev.Err != nil {
				msg = ev.Err.Message
				code = string(ev.Err.Type)
			}
			sw.event("error", map[string]any{
				"error": map[string]any{
					"message": msg,
					"type":    code,
				},
			})
			sw.done()
			return
		}
	}
	if !nativeMode {
		sw.done()
	}
}

// writeError emits an OpenAI-shaped error object.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}

// writeFailure maps a typed scheduler failure onto protocol-shaped errors.
func writeFailure(w http.ResponseWriter, f *domain.Failure) {
	setRetryAfter(w, f)
	status := failureStatus(f)
	typ := "api_error"
	switch f.Type {
	case domain.FailAuth:
		typ = "authentication_error"
	case domain.FailRateLimit, domain.FailQuotaExhausted:
		typ = "rate_limit_error"
	case domain.FailUnsupportedParam:
		typ = "invalid_request_error"
	case domain.FailContextOverflow:
		typ = "invalid_request_error"
	case domain.FailClientCancel:
		status = http.StatusRequestTimeout
		typ = "request_canceled"
	case domain.FailConnect, domain.FailProvider5xx, domain.FailBadResponse,
		domain.FailStreamBroken, domain.FailProtocolConversion:
		typ = "api_error"
	}
	writeError(w, status, typ, f.Message)
}

func setRetryAfter(w http.ResponseWriter, f *domain.Failure) {
	if f != nil && f.RetryAfter > 0 {
		seconds := int64((f.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
}

func failureStatus(f *domain.Failure) int {
	if f.HTTPStatus >= 400 {
		return f.HTTPStatus
	}
	switch f.Type {
	case domain.FailAuth:
		return http.StatusUnauthorized
	case domain.FailRateLimit:
		return http.StatusTooManyRequests
	case domain.FailQuotaExhausted:
		return http.StatusTooManyRequests
	case domain.FailUnsupportedParam:
		return http.StatusUnprocessableEntity
	case domain.FailContextOverflow:
		return http.StatusRequestEntityTooLarge
	case domain.FailClientCancel:
		return 499
	default:
		return http.StatusBadGateway
	}
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}
