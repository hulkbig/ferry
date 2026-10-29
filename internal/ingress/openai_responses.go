package ingress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
)

// ResponsesHandler serves POST /v1/responses (OpenAI Responses API).
type ResponsesHandler struct{ Eng Engine }

func (h *ResponsesHandler) Protocol() domain.Protocol { return domain.ProtocolOpenAIResponses }

// ---------------------------------------------------------------------------
// wire types (request side)

type rsInputItem struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
	// message items
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// function_call items
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output items
	Output json.RawMessage `json:"output,omitempty"`
}

type rsRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Tools           []rsTool        `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	ServiceTier     string          `json:"service_tier,omitempty"`
	MaxOutputTokens int64           `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	ParallelTools   *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning       *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
	Text *struct {
		Format *struct {
			Type   string          `json:"type"`
			Name   string          `json:"name,omitempty"`
			Schema json.RawMessage `json:"schema,omitempty"`
			Strict *bool           `json:"strict,omitempty"`
		} `json:"format,omitempty"`
		Verbosity string `json:"verbosity,omitempty"`
	} `json:"text,omitempty"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
	Conversation       json.RawMessage `json:"conversation,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	Store              bool            `json:"store,omitempty"`
	// Include and Truncation are auxiliary Responses features: include
	// controls extra response fields and truncation drops old content to
	// save tokens. Both translate to mapping notes instead of blocking.
	Include    json.RawMessage `json:"include,omitempty"`
	Truncation string          `json:"truncation,omitempty"`
}

type rsTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func responsesNestedConversionIssues(body []byte) []string {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	var issues []string
	var input []json.RawMessage
	if json.Unmarshal(root["input"], &input) == nil {
		for index, rawItem := range input {
			path := fmt.Sprintf("input[%d]", index)
			issues = append(issues, unknownObjectFields(rawItem, path,
				"type", "id", "status", "role", "content", "call_id", "name", "arguments", "output")...)
			var item map[string]json.RawMessage
			_ = json.Unmarshal(rawItem, &item)
			var parts []json.RawMessage
			if json.Unmarshal(item["content"], &parts) == nil {
				for partIndex, rawPart := range parts {
					partPath := fmt.Sprintf("%s.content[%d]", path, partIndex)
					issues = append(issues, unknownObjectFields(rawPart, partPath,
						"type", "text", "image_url")...)
				}
			}
		}
	}
	var tools []json.RawMessage
	_ = json.Unmarshal(root["tools"], &tools)
	for index, rawTool := range tools {
		issues = append(issues, unknownObjectFields(rawTool, fmt.Sprintf("tools[%d]", index),
			"type", "name", "description", "parameters")...)
	}
	if raw := root["tool_choice"]; len(raw) > 0 && strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		issues = append(issues, unknownObjectFields(raw, "tool_choice", "type", "name", "function")...)
		var choice map[string]json.RawMessage
		_ = json.Unmarshal(raw, &choice)
		issues = append(issues, unknownObjectFields(choice["function"], "tool_choice.function", "name")...)
	}
	if raw := root["reasoning"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "reasoning", "effort")...)
	}
	if raw := root["text"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "text", "format", "verbosity")...)
		var text map[string]json.RawMessage
		_ = json.Unmarshal(raw, &text)
		issues = append(issues, unknownObjectFields(text["format"], "text.format",
			"type", "name", "schema", "strict")...)
	}
	if raw := root["conversation"]; len(raw) > 0 && strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		issues = append(issues, unknownObjectFields(raw, "conversation", "id")...)
	}
	return issues
}

// toContract converts the wire request into canonical form.
func (h *ResponsesHandler) toContract(body []byte) (*domain.RequestContract, error) {
	var req rsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, perrf("invalid JSON body: %v", err)
	}
	if req.Model == "" {
		return nil, perrf("model is required")
	}
	tier, err := domain.NormalizeServiceTier(req.ServiceTier)
	if err != nil {
		return nil, perrf("%s", err)
	}
	rc := &domain.RequestContract{
		Protocol:           domain.ProtocolOpenAIResponses,
		Origin:             domain.ProtocolOpenAIResponses,
		Model:              req.Model,
		MaxTokens:          req.MaxOutputTokens,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		ParallelTools:      req.ParallelTools,
		Stream:             req.Stream,
		PersistNativeState: req.Store,
		Native:             json.RawMessage(body),
		ServiceTier:        tier,
		ConversionIssues: unknownTopLevelFields(body,
			"model", "input", "instructions", "tools", "tool_choice", "stream",
			"max_output_tokens", "temperature", "top_p", "parallel_tool_calls", "reasoning", "text",
			"metadata", "conversation", "previous_response_id", "prompt_cache_key", "store",
			"include", "truncation", "service_tier"),
	}
	if req.Text != nil {
		if verbosity := strings.TrimSpace(req.Text.Verbosity); verbosity != "" {
			rc.Verbosity = verbosity
		}
	}
	if len(req.Include) > 0 {
		rc.MappingNotes = append(rc.MappingNotes, "include extras omitted (no target equivalent)")
	}
	if req.Truncation != "" {
		rc.MappingNotes = append(rc.MappingNotes,
			"truncation strategy omitted (full conversation history preserved)")
	}
	rc.ConversionIssues = append(rc.ConversionIssues, responsesNestedConversionIssues(body)...)
	conversationID, err := parseResponsesConversation(req.Conversation)
	if err != nil {
		return nil, err
	}
	if conversationID != "" && req.PreviousResponseID != "" {
		return nil, perrf("conversation and previous_response_id are mutually exclusive")
	}
	if conversationID != "" || req.PreviousResponseID != "" || req.PromptCacheKey != "" {
		rc.Lineage = &domain.ResponsesLineage{
			ConversationID: conversationID, PreviousResponseID: req.PreviousResponseID,
			PromptCacheKey: req.PromptCacheKey,
		}
	}
	if ins := strings.TrimSpace(req.Instructions); ins != "" {
		rc.Messages = append(rc.Messages, domain.Message{
			Role:   domain.RoleSystem,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: ins}},
		})
	}
	items, inputIssues, err := decodeRsInput(req.Input)
	if err != nil {
		return nil, err
	}
	rc.Messages = append(rc.Messages, items...)
	rc.ConversionIssues = append(rc.ConversionIssues, inputIssues...)
	if len(req.Tools) > 0 {
		// Responses hosted/built-in tools are intentionally preserved in Native
		// instead of being rewritten as function tools, but they still require a
		// tool-capable target.
		rc.NativeRequirements.Tools = true
	}
	for _, t := range req.Tools {
		if t.Type != "function" {
			rc.ConversionIssues = append(rc.ConversionIssues, "tool type "+t.Type+" is native-only")
			continue
		}
		rc.Tools = append(rc.Tools, domain.ToolDef{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var s string
		if err := json.Unmarshal(req.ToolChoice, &s); err == nil {
			rc.ToolChoice = s
		} else {
			var obj struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &obj); err != nil {
				return nil, perrf("unsupported tool_choice shape")
			}
			name := obj.Name
			if name == "" {
				name = obj.Function.Name
			}
			if obj.Type != "function" {
				if obj.Type == "" {
					return nil, perrf("unsupported tool_choice shape")
				}
				rc.NativeRequirements.Tools = true
				rc.ConversionIssues = append(rc.ConversionIssues,
					"tool_choice type "+obj.Type+" is native-only")
			} else if name == "" {
				return nil, perrf("unsupported tool_choice shape")
			} else {
				rc.ToolChoice = name
			}
		}
	}
	if rf := req.Text; rf != nil && rf.Format != nil {
		switch rf.Format.Type {
		case "text":
		case "json_object":
			rc.ResponseFormat = &domain.ResponseFormat{Name: "response"}
		case "json_schema":
			if !domain.ValidJSONObject(rf.Format.Schema) {
				return nil, perrf("text.format.schema must be a JSON object")
			}
			name := rf.Format.Name
			if name == "" {
				name = "response"
			}
			rc.ResponseFormat = &domain.ResponseFormat{Name: name, Schema: rf.Format.Schema, Strict: rf.Format.Strict}
		default:
			return nil, perrf("unsupported text.format.type %q", rf.Format.Type)
		}
	}
	if rz := req.Reasoning; rz != nil {
		rc.Reasoning = &domain.ReasoningSpec{Effort: rz.Effort}
	}
	if len(req.Metadata) > 0 {
		md := map[string]string{}
		for k, v := range req.Metadata {
			if s, ok := v.(string); ok {
				md[k] = s
			} else {
				rc.ConversionIssues = append(rc.ConversionIssues,
					"metadata."+k+" is not representable outside Responses")
			}
		}
		rc.Metadata = md
	}
	markOpaqueConversionIssues(rc)
	return rc, nil
}

func parseResponsesConversation(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var object struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &object); err != nil || object.ID == "" {
		return "", perrf("conversation must be a string id or object with id")
	}
	return object.ID, nil
}

// decodeRsInput accepts a plain string or an array of input items.
func decodeRsInput(raw json.RawMessage) ([]domain.Message, []string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, perrf("input is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []domain.Message{{
			Role:   domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: s}},
		}}, nil, nil
	}
	var wire []json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, nil, perrf("input must be a string or array of items")
	}
	var out []domain.Message
	var issues []string
	appendParallelCall := func(block domain.Block) {
		if len(out) > 0 && out[len(out)-1].Role == domain.RoleAssistant {
			callsOnly := len(out[len(out)-1].Blocks) > 0
			for _, existing := range out[len(out)-1].Blocks {
				callsOnly = callsOnly && existing.Kind == domain.BlockToolUse
			}
			if callsOnly {
				out[len(out)-1].Blocks = append(out[len(out)-1].Blocks, block)
				return
			}
		}
		out = append(out, domain.Message{Role: domain.RoleAssistant, Blocks: []domain.Block{block}})
	}
	appendParallelResult := func(block domain.Block) {
		if len(out) > 0 && out[len(out)-1].Role == domain.RoleTool {
			out[len(out)-1].Blocks = append(out[len(out)-1].Blocks, block)
			return
		}
		out = append(out, domain.Message{Role: domain.RoleTool, ToolID: block.ID,
			Blocks: []domain.Block{block}})
	}
	for _, rawItem := range wire {
		var it rsInputItem
		if err := json.Unmarshal(rawItem, &it); err != nil {
			return nil, nil, perrf("input contains an invalid item")
		}
		switch it.Type {
		case "", "message":
			if it.Status != "" && it.Status != "completed" {
				issues = append(issues, "input message status "+it.Status+" is native-only")
			}
			blocks, err := decodeRsContent(it.Content)
			if err != nil {
				return nil, nil, err
			}
			role := domain.Role(it.Role)
			if it.Role == "developer" {
				role = domain.RoleSystem
				issues = append(issues, "developer input role is native-only")
			} else if it.Role != string(domain.RoleSystem) && it.Role != string(domain.RoleUser) &&
				it.Role != string(domain.RoleAssistant) {
				issues = append(issues, "input message role "+it.Role+" is native-only")
			}
			out = append(out, domain.Message{Role: role, Blocks: blocks})
		case "function_call":
			if it.Status != "" && it.Status != "completed" {
				issues = append(issues, "function_call status "+it.Status+" is native-only")
			}
			appendParallelCall(domain.Block{
				Kind: domain.BlockToolUse, ID: it.CallID,
				Name: it.Name, Input: json.RawMessage(it.Arguments),
			})
		case "function_call_output":
			if it.Status != "" && it.Status != "completed" {
				issues = append(issues, "function_call_output status "+it.Status+" is native-only")
			}
			if len(it.Output) == 0 || !json.Valid(it.Output) {
				return nil, nil, perrf("function_call_output %q has invalid output", it.CallID)
			}
			appendParallelResult(domain.Block{
				Kind: domain.BlockToolResult, ID: it.CallID,
				Result: append(json.RawMessage(nil), it.Output...),
			})
		case "reasoning":
			out = append(out, domain.Message{Role: domain.RoleAssistant, Blocks: []domain.Block{{
				Kind: domain.BlockOpaque, OpaqueType: it.Type,
				Opaque: append(json.RawMessage(nil), rawItem...),
			}}})
		default:
			out = append(out, domain.Message{Role: domain.Role(it.Role), Blocks: []domain.Block{{
				Kind: domain.BlockOpaque, OpaqueType: it.Type,
				Opaque: append(json.RawMessage(nil), rawItem...),
			}}})
		}
	}
	return out, issues, nil
}

func decodeRsContent(raw json.RawMessage) ([]domain.Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []domain.Block{{Kind: domain.BlockText, Text: s}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, perrf("message.content: invalid shape")
	}
	var blocks []domain.Block
	for _, rawPart := range parts {
		var p struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL string `json:"image_url"`
		}
		if err := json.Unmarshal(rawPart, &p); err != nil {
			return nil, perrf("message.content: invalid part")
		}
		switch p.Type {
		case "", "input_text", "output_text":
			blocks = append(blocks, domain.Block{Kind: domain.BlockText, Text: p.Text})
		case "input_image":
			mime, data, ok := parseDataURL(p.ImageURL)
			if !ok {
				blocks = append(blocks, domain.Block{Kind: domain.BlockOpaque,
					OpaqueType: p.Type, Opaque: append(json.RawMessage(nil), rawPart...)})
				continue
			}
			blocks = append(blocks, domain.Block{
				Kind: domain.BlockImage, ImageMIME: mime, ImageData: data})
		default:
			blocks = append(blocks, domain.Block{Kind: domain.BlockOpaque,
				OpaqueType: p.Type, Opaque: append(json.RawMessage(nil), rawPart...)})
		}
	}
	return blocks, nil
}

// ---------------------------------------------------------------------------
// response side

type rsOutputContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rsOutputItem struct {
	Type    string                `json:"type"`
	ID      string                `json:"id,omitempty"`
	Status  string                `json:"status,omitempty"`
	Role    string                `json:"role,omitempty"`
	Content []rsOutputContentPart `json:"content,omitempty"`
	// function_call fields
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// summary for reasoning items
	Summary []any `json:"summary,omitempty"`
}

type rsUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	} `json:"input_tokens_details"`
	OutputTokens        int64 `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int64 `json:"total_tokens"`
}

func rsUsageFrom(u domain.Usage) rsUsage {
	var out rsUsage
	out.InputTokens = u.UncachedInput() + u.CacheReadTokens + u.CacheWriteTokens
	out.InputTokensDetails.CachedTokens = u.CacheReadTokens
	out.InputTokensDetails.CacheWriteTokens = u.CacheWriteTokens
	out.OutputTokens = u.OutputTokens
	out.OutputTokensDetails.ReasoningTokens = u.ReasoningTokens
	out.TotalTokens = out.InputTokens + out.OutputTokens
	return out
}

func rsResponseFrom(res *sched.Result, r *domain.Response) map[string]any {
	var items []rsOutputItem
	messageNumber, reasoningNumber, toolNumber := 0, 0, 0
	var textParts []rsOutputContentPart
	var reasoningText strings.Builder
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		messageNumber++
		id := res.RequestID + "-msg"
		if messageNumber > 1 {
			id = fmt.Sprintf("%s-msg-%d", res.RequestID, messageNumber)
		}
		items = append(items, rsOutputItem{
			Type: "message", ID: id, Status: "completed", Role: "assistant",
			Content: textParts,
		})
		textParts = nil
	}
	flushReasoning := func() {
		if reasoningText.Len() == 0 {
			return
		}
		reasoningNumber++
		id := res.RequestID + "-rs"
		if reasoningNumber > 1 {
			id = fmt.Sprintf("%s-rs-%d", res.RequestID, reasoningNumber)
		}
		items = append(items, rsOutputItem{
			Type: "reasoning", ID: id, Status: "completed", Summary: []any{},
			Content: []rsOutputContentPart{{Type: "reasoning_text", Text: reasoningText.String()}},
		})
		reasoningText.Reset()
	}
	for _, b := range r.Content {
		switch b.Kind {
		case domain.BlockThinking:
			flushText()
			reasoningText.WriteString(b.Text)
		case domain.BlockText:
			flushReasoning()
			textParts = append(textParts, rsOutputContentPart{Type: "output_text", Text: b.Text})
		case domain.BlockToolUse:
			flushReasoning()
			flushText()
			toolNumber++
			items = append(items, rsOutputItem{
				Type: "function_call", ID: fmt.Sprintf("%s-fc-%d", res.RequestID, toolNumber),
				Status: "completed", CallID: b.ID, Name: b.Name,
				Arguments: string(normalizeArgs(b.Input)),
			})
		}
	}
	flushReasoning()
	flushText()
	if items == nil {
		items = []rsOutputItem{}
	}
	status := "completed"
	if r.FinishReason == "length" {
		status = "incomplete"
	}
	out := map[string]any{
		"id":                  res.RequestID,
		"object":              "response",
		"created_at":          timeNowUnix(),
		"status":              status,
		"model":               res.Model,
		"output":              items,
		"parallel_tool_calls": toolNumber > 1,
		"usage":               rsUsageFrom(r.Usage),
	}
	if r.ServiceTier != "" {
		out["service_tier"] = r.ServiceTier
	}
	if res.ConversationID != "" {
		out["conversation"] = map[string]string{"id": res.ConversationID}
	}
	if status == "incomplete" {
		out["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}
	return out
}

// ---------------------------------------------------------------------------
// streaming wire types

type rsEvent struct {
	Type           string               `json:"type"`
	SequenceNumber int64                `json:"sequence_number"`
	ItemID         string               `json:"item_id,omitempty"`
	OutputIndex    *int                 `json:"output_index,omitempty"`
	ContentIndex   *int                 `json:"content_index,omitempty"`
	Delta          string               `json:"delta,omitempty"`
	Text           string               `json:"text,omitempty"`
	Arguments      string               `json:"arguments,omitempty"`
	Response       map[string]any       `json:"response,omitempty"`
	Item           *rsOutputItem        `json:"item,omitempty"`
	Part           *rsOutputContentPart `json:"part,omitempty"`
}

// ---------------------------------------------------------------------------
// HTTP entry

func (h *ResponsesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w, r, finishObservation := observeIngress(h.Eng, domain.ProtocolOpenAIResponses, w, r)
	defer finishObservation()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST only")
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
		status := http.StatusBadRequest
		var pe *parseError
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
		streamResponses(w, res)
		return
	}
	if conversionFailure := responseConversionFailure(res, domain.ProtocolOpenAIResponses); conversionFailure != nil {
		writeFailure(w, conversionFailure)
		return
	}
	writeResponseWarnings(w, res)
	writeServiceTierHeaders(w, res)
	if !writeNativeResponse(w, res, domain.ProtocolOpenAIResponses) {
		writeJSON(w, http.StatusOK, rsResponseFrom(res, res.Response))
	}
}

// streamResponses pumps canonical events into Responses-API SSE events.
func streamResponses(w http.ResponseWriter, res *sched.Result) {
	sw := newSSEWriter(w)
	nativeMode := false
	nativeTerminal := false
	seq := int64(0)
	next := func() int64 { seq++; return seq }
	indexPointer := func(index int) *int { return &index }
	nextOutputIndex := 0
	messageNumber, reasoningNumber, toolNumber := 0, 0, 0
	completedItems := map[int]rsOutputItem{}
	type sequentialItem struct {
		kind, id  string
		source    int
		wireIndex int
		text      strings.Builder
	}
	type toolItem struct {
		id, callID, name string
		wireIndex        int
		arguments        strings.Builder
	}
	var current *sequentialItem
	tools := map[int]*toolItem{}
	closeCurrent := func() {
		if current == nil {
			return
		}
		index := current.wireIndex
		text := current.text.String()
		var item rsOutputItem
		if current.kind == "message" {
			part := rsOutputContentPart{Type: "output_text", Text: text}
			sw.data(rsEvent{Type: "response.output_text.done", SequenceNumber: next(),
				ItemID: current.id, OutputIndex: indexPointer(index), ContentIndex: indexPointer(0), Text: text})
			sw.data(rsEvent{Type: "response.content_part.done", SequenceNumber: next(),
				ItemID: current.id, OutputIndex: indexPointer(index), ContentIndex: indexPointer(0), Part: &part})
			item = rsOutputItem{Type: "message", ID: current.id, Status: "completed", Role: "assistant",
				Content: []rsOutputContentPart{part}}
		} else {
			sw.data(rsEvent{Type: "response.reasoning_text.done", SequenceNumber: next(),
				ItemID: current.id, OutputIndex: indexPointer(index), ContentIndex: indexPointer(0), Text: text})
			item = rsOutputItem{Type: "reasoning", ID: current.id, Status: "completed", Summary: []any{},
				Content: []rsOutputContentPart{{Type: "reasoning_text", Text: text}}}
		}
		sw.data(rsEvent{Type: "response.output_item.done", SequenceNumber: next(),
			OutputIndex: indexPointer(index), Item: &item})
		completedItems[index] = item
		current = nil
	}
	openSequential := func(kind string, source int) *sequentialItem {
		if current != nil && current.kind == kind && current.source == source {
			return current
		}
		closeCurrent()
		index := nextOutputIndex
		nextOutputIndex++
		id := ""
		if kind == "message" {
			messageNumber++
			id = res.RequestID + "-msg"
			if messageNumber > 1 {
				id = fmt.Sprintf("%s-msg-%d", res.RequestID, messageNumber)
			}
		} else {
			reasoningNumber++
			id = res.RequestID + "-rs"
			if reasoningNumber > 1 {
				id = fmt.Sprintf("%s-rs-%d", res.RequestID, reasoningNumber)
			}
		}
		current = &sequentialItem{kind: kind, id: id, source: source, wireIndex: index}
		item := &rsOutputItem{Type: kind, ID: id, Status: "in_progress"}
		if kind == "message" {
			item.Role = "assistant"
			item.Content = []rsOutputContentPart{}
		} else {
			item.Summary = []any{}
		}
		sw.data(rsEvent{Type: "response.output_item.added", SequenceNumber: next(),
			OutputIndex: indexPointer(index), Item: item})
		if kind == "message" {
			sw.data(rsEvent{Type: "response.content_part.added", SequenceNumber: next(),
				ItemID: id, OutputIndex: indexPointer(index), ContentIndex: indexPointer(0),
				Part: &rsOutputContentPart{Type: "output_text"}})
		}
		return current
	}
	writeErrorEvent := func(message, code string) {
		sw.data(rsEvent{Type: "response.failed", SequenceNumber: next(),
			Response: map[string]any{
				"id": res.RequestID, "object": "response", "status": "failed",
				"error": map[string]any{"code": code, "message": message},
			}})
	}

	for ev := range res.Events {
		if ev.Kind == domain.EvNative {
			if ev.NativeProtocol == domain.ProtocolOpenAIResponses && ev.NativeFrame != nil {
				nativeMode = true
				nativeTerminal = nativeTerminal || ev.NativeFrame.Terminal
				writeNativeFrame(sw, res, domain.ProtocolOpenAIResponses, ev.NativeFrame)
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
		case domain.EvThinkingDelta:
			item := openSequential("reasoning", ev.Index)
			item.text.WriteString(ev.ThinkingDelta)
			sw.data(rsEvent{Type: "response.reasoning_text.delta", SequenceNumber: next(),
				ItemID: item.id, OutputIndex: indexPointer(item.wireIndex), ContentIndex: indexPointer(0),
				Delta: ev.ThinkingDelta})
		case domain.EvTextDelta:
			item := openSequential("message", ev.Index)
			item.text.WriteString(ev.TextDelta)
			sw.data(rsEvent{Type: "response.output_text.delta", SequenceNumber: next(),
				ItemID: item.id, OutputIndex: indexPointer(item.wireIndex), ContentIndex: indexPointer(0),
				Delta: ev.TextDelta})
		case domain.EvToolStart:
			closeCurrent()
			if tools[ev.Index] != nil {
				writeErrorEvent("streamed tool call reused an active index", string(domain.FailProtocolConversion))
				return
			}
			toolNumber++
			item := &toolItem{id: fmt.Sprintf("%s-fc-%d", res.RequestID, toolNumber),
				callID: ev.ToolID, name: ev.ToolName, wireIndex: nextOutputIndex}
			nextOutputIndex++
			tools[ev.Index] = item
			wire := &rsOutputItem{Type: "function_call", ID: item.id,
				Status: "in_progress", CallID: ev.ToolID, Name: ev.ToolName}
			sw.data(rsEvent{Type: "response.output_item.added", SequenceNumber: next(),
				OutputIndex: indexPointer(item.wireIndex), Item: wire})
		case domain.EvToolDelta:
			item := tools[ev.Index]
			if item == nil {
				writeErrorEvent("streamed tool arguments arrived before their tool call", string(domain.FailProtocolConversion))
				return
			}
			item.arguments.WriteString(ev.ArgsDelta)
			sw.data(rsEvent{Type: "response.function_call_arguments.delta", SequenceNumber: next(),
				ItemID: item.id, OutputIndex: indexPointer(item.wireIndex),
				Delta: ev.ArgsDelta})
		case domain.EvToolEnd:
			item := tools[ev.Index]
			if item == nil {
				writeErrorEvent("streamed tool call ended before it started", string(domain.FailProtocolConversion))
				return
			}
			arguments := item.arguments.String()
			sw.data(rsEvent{Type: "response.function_call_arguments.done", SequenceNumber: next(),
				ItemID: item.id, OutputIndex: indexPointer(item.wireIndex), Arguments: arguments})
			wire := rsOutputItem{Type: "function_call", ID: item.id, Status: "completed",
				CallID: item.callID, Name: item.name, Arguments: arguments}
			sw.data(rsEvent{Type: "response.output_item.done", SequenceNumber: next(),
				OutputIndex: indexPointer(item.wireIndex), Item: &wire})
			completedItems[item.wireIndex] = wire
			delete(tools, ev.Index)
		case domain.EvEnd:
			closeCurrent()
			if len(tools) > 0 {
				writeErrorEvent("stream completed with unfinished tool calls", string(domain.FailProtocolConversion))
				return
			}
			indexes := make([]int, 0, len(completedItems))
			for index := range completedItems {
				indexes = append(indexes, index)
			}
			sort.Ints(indexes)
			output := make([]rsOutputItem, 0, len(indexes))
			for _, index := range indexes {
				output = append(output, completedItems[index])
			}
			final := map[string]any{
				"id": res.RequestID, "object": "response",
				"created_at": timeNowUnix(),
				"status": func() string {
					if ev.FinishReason == "length" {
						return "incomplete"
					}
					return "completed"
				}(),
				"model":               res.Model,
				"output":              output,
				"parallel_tool_calls": toolNumber > 1,
				"usage": func() any {
					if ev.Usage != nil {
						return rsUsageFrom(*ev.Usage)
					}
					return nil
				}(),
			}
			if ev.ServiceTier != "" {
				final["service_tier"] = ev.ServiceTier
			}
			if res.ConversationID != "" {
				final["conversation"] = map[string]string{"id": res.ConversationID}
			}
			typ := "response.completed"
			if final["status"] == "incomplete" {
				typ = "response.incomplete"
				final["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
			}
			sw.data(rsEvent{Type: typ, SequenceNumber: next(), Response: final})
			return
		case domain.EvError:
			msg := "upstream stream failed"
			code := "upstream_error"
			if ev.Err != nil {
				msg = ev.Err.Message
				code = string(ev.Err.Type)
			}
			writeErrorEvent(msg, code)
			return
		}
	}
}
