package ingress

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
)

// AnthropicHandler serves POST /v1/messages.
type AnthropicHandler struct{ Eng Engine }

func (h *AnthropicHandler) Protocol() domain.Protocol { return domain.ProtocolAnthropic }

// ---------------------------------------------------------------------------
// wire types (request side mirrors the Anthropic Messages schema)

type anContentBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`
	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	// tool_use / tool_result
	ID           string          `json:"id,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"` // tool_result spelling
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"` // tool_result content
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// image
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source,omitempty"`
}

type anMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anMessage     `json:"messages"`
	MaxTokens     int64           `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int64          `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anTool        `json:"tools,omitempty"`
	ToolChoice    *struct {
		Type               string `json:"type"`
		Name               string `json:"name,omitempty"`
		DisableParallelUse *bool  `json:"disable_parallel_tool_use,omitempty"`
	} `json:"tool_choice,omitempty"`
	Thinking *struct {
		Type         string `json:"type"`
		BudgetTokens int64  `json:"budget_tokens"`
	} `json:"thinking,omitempty"`
	Metadata *struct {
		UserID string `json:"user_id,omitempty"`
	} `json:"metadata,omitempty"`
	// ContextManagement and OutputConfig are auxiliary Anthropic features:
	// context editing optimizes token usage without changing request
	// content, and output_config carries verbosity and structured output. Both are translated
	// instead of blocking cross-protocol conversion.
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
	OutputConfig      *struct {
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format,omitempty"`
		Text *struct {
			Verbosity string `json:"verbosity,omitempty"`
		} `json:"text,omitempty"`
	} `json:"output_config,omitempty"`
}

type anTool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

func anthropicNestedConversionIssues(body []byte) []string {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	var issues []string
	if raw := root["system"]; len(raw) > 0 && string(raw) != "null" {
		var systemText string
		var blocks []json.RawMessage
		switch {
		case json.Unmarshal(raw, &systemText) == nil:
		case json.Unmarshal(raw, &blocks) == nil:
			for index, rawBlock := range blocks {
				issues = append(issues, unknownObjectFields(rawBlock,
					fmt.Sprintf("system[%d]", index), "type", "text", "cache_control")...)
			}
		default:
			issues = append(issues, "field system has a native-only shape")
		}
	}
	var messages []json.RawMessage
	_ = json.Unmarshal(root["messages"], &messages)
	for index, rawMessage := range messages {
		path := fmt.Sprintf("messages[%d]", index)
		issues = append(issues, unknownObjectFields(rawMessage, path, "role", "content")...)
		var message map[string]json.RawMessage
		_ = json.Unmarshal(rawMessage, &message)
		var blocks []json.RawMessage
		if json.Unmarshal(message["content"], &blocks) != nil {
			continue
		}
		for blockIndex, rawBlock := range blocks {
			blockPath := fmt.Sprintf("%s.content[%d]", path, blockIndex)
			issues = append(issues, unknownObjectFields(rawBlock, blockPath,
				"type", "text", "thinking", "signature", "data", "id", "tool_use_id",
				"name", "input", "content", "is_error", "cache_control", "source")...)
			var block map[string]json.RawMessage
			_ = json.Unmarshal(rawBlock, &block)
			issues = append(issues, unknownObjectFields(block["source"], blockPath+".source",
				"type", "media_type", "data")...)
		}
	}
	var tools []json.RawMessage
	_ = json.Unmarshal(root["tools"], &tools)
	for index, rawTool := range tools {
		issues = append(issues, unknownObjectFields(rawTool, fmt.Sprintf("tools[%d]", index),
			"type", "name", "description", "input_schema", "cache_control")...)
	}
	if raw := root["tool_choice"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "tool_choice",
			"type", "name", "disable_parallel_tool_use")...)
	}
	if raw := root["thinking"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "thinking", "type", "budget_tokens")...)
	}
	if raw := root["metadata"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "metadata", "user_id")...)
	}
	if raw := root["output_config"]; len(raw) > 0 && string(raw) != "null" {
		issues = append(issues, unknownObjectFields(raw, "output_config", "text", "format")...)
		var config map[string]json.RawMessage
		_ = json.Unmarshal(raw, &config)
		issues = append(issues, unknownObjectFields(config["format"], "output_config.format", "type", "schema")...)
		issues = append(issues, unknownObjectFields(config["text"], "output_config.text", "verbosity")...)
	}

	return issues
}

// toContract converts the wire request into canonical form.
func (h *AnthropicHandler) toContract(body []byte) (*domain.RequestContract, error) {
	var req anRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, perrf("invalid JSON body: %v", err)
	}
	if req.Model == "" {
		return nil, perrf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, perrf("messages is required")
	}
	rc := &domain.RequestContract{
		Protocol:      domain.ProtocolAnthropic,
		Origin:        domain.ProtocolAnthropic,
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
		Native:        json.RawMessage(body),
		ConversionIssues: unknownTopLevelFields(body,
			"model", "system", "messages", "max_tokens", "temperature", "top_p", "top_k",
			"stop_sequences", "stream", "tools", "tool_choice", "thinking", "metadata",
			"context_management", "output_config"),
	}
	rc.ConversionIssues = append(rc.ConversionIssues, anthropicNestedConversionIssues(body)...)
	if len(req.ContextManagement) > 0 && string(req.ContextManagement) != "null" {
		// Context editing clears stale content to save tokens; the target
		// preserves full history, which keeps every semantic input the client
		// sent.
		rc.MappingNotes = append(rc.MappingNotes,
			"context_management edits omitted (full conversation history preserved)")
	}
	if oc := req.OutputConfig; oc != nil {
		if format := oc.Format; format != nil {
			if format.Type != "json_schema" {
				rc.ConversionIssues = append(rc.ConversionIssues, "output_config.format type requires native Anthropic passthrough")
			} else {
				if !domain.ValidJSONObject(format.Schema) {
					return nil, perrf("output_config.format.schema must be a JSON object")
				}
				strict := true
				rc.ResponseFormat = &domain.ResponseFormat{Schema: format.Schema, Strict: &strict}
			}
		}
		if oc.Text != nil {
			if verbosity := strings.TrimSpace(oc.Text.Verbosity); verbosity != "" {
				rc.Verbosity = verbosity
			}
		}
	}
	if sys := decodeAnSystem(req.System); sys != "" {
		rc.Messages = append(rc.Messages, domain.Message{
			Role:   domain.RoleSystem,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: sys}},
		})
	}
	if strings.Contains(string(body), `"cache_control"`) {
		// Cache-control breakpoints are a prompt-caching optimization hint on
		// system prompts, messages, and tools. OpenAI-style targets cache
		// automatically, so the markers map to nothing without changing
		// request content.
		rc.MappingNotes = append(rc.MappingNotes,
			"cache_control omitted (target applies automatic prompt caching)")
	}
	for _, m := range req.Messages {
		if m.Role != string(domain.RoleUser) && m.Role != string(domain.RoleAssistant) {
			rc.ConversionIssues = append(rc.ConversionIssues,
				"message role "+m.Role+" is native-only")
		}
		blocks, toolID, err := decodeAnBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		if toolID == "" {
			rc.Messages = append(rc.Messages, domain.Message{Role: domain.Role(m.Role), Blocks: blocks})
			continue
		}
		// Anthropic carries tool results in a user turn and permits ordinary
		// user content after the result blocks. Split that wire turn into its
		// two canonical messages so Chat/Responses can preserve both the tool
		// correlation and the following user text without degradation.
		var resultBlocks, userBlocks []domain.Block
		for _, block := range blocks {
			if block.Kind == domain.BlockToolResult {
				resultBlocks = append(resultBlocks, block)
			} else {
				userBlocks = append(userBlocks, block)
			}
		}
		if len(resultBlocks) > 0 {
			rc.Messages = append(rc.Messages, domain.Message{
				Role: domain.RoleTool, Blocks: resultBlocks, ToolID: toolID,
			})
		}
		if len(userBlocks) > 0 {
			rc.Messages = append(rc.Messages, domain.Message{
				Role: domain.Role(m.Role), Blocks: userBlocks,
			})
		}
	}
	if len(req.Tools) > 0 {
		// Native Anthropic tools may not have a canonical function-tool shape,
		// but every declared tool still needs a tool-capable upstream model.
		rc.NativeRequirements.Tools = true
	}
	for _, t := range req.Tools {
		if t.Type != "" || t.Name == "" {
			rc.ConversionIssues = append(rc.ConversionIssues, "tool type "+t.Type+" is native-only")
			continue
		}
		rc.Tools = append(rc.Tools, domain.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	if tc := req.ToolChoice; tc != nil {
		if tc.DisableParallelUse != nil {
			parallel := !*tc.DisableParallelUse
			rc.ParallelTools = &parallel
		}
		switch tc.Type {
		case "auto":
			rc.ToolChoice = "auto"
		case "any":
			rc.ToolChoice = "required"
		case "none":
			rc.ToolChoice = "none"
		case "tool":
			if tc.Name == "" {
				return nil, perrf("tool_choice.tool requires name")
			}
			rc.ToolChoice = tc.Name
		default:
			return nil, perrf("unsupported tool_choice.type %q", tc.Type)
		}
	}
	if th := req.Thinking; th != nil && th.Type == "enabled" {
		if th.BudgetTokens <= 0 {
			return nil, perrf("thinking.budget_tokens must be positive")
		}
		rc.Reasoning = &domain.ReasoningSpec{BudgetTokens: th.BudgetTokens}
	} else if th := req.Thinking; th != nil && th.Type != "disabled" {
		rc.NativeRequirements.Reasoning = true
		rc.ConversionIssues = append(rc.ConversionIssues, "thinking."+th.Type+" is native-only")
	}
	if md := req.Metadata; md != nil {
		rc.Metadata = map[string]string{"user_id": md.UserID}
	}
	markOpaqueConversionIssues(rc)
	return rc, nil
}

func decodeAnSystem(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb []byte
		for _, b := range blocks {
			if b.Type == "text" {
				sb = append(sb, b.Text...)
			}
		}
		return string(sb)
	}
	return ""
}

func decodeAnBlocks(raw json.RawMessage) ([]domain.Block, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, "", nil
		}
		return []domain.Block{{Kind: domain.BlockText, Text: s}}, "", nil
	}
	var wire []json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, "", perrf("message.content: invalid shape")
	}
	var out []domain.Block
	var toolID string
	for _, rawBlock := range wire {
		var wb anContentBlock
		if err := json.Unmarshal(rawBlock, &wb); err != nil {
			return nil, "", perrf("message.content: invalid block")
		}
		switch wb.Type {
		case "text":
			out = append(out, domain.Block{Kind: domain.BlockText, Text: wb.Text})
		case "thinking":
			out = append(out, domain.Block{Kind: domain.BlockThinking, Text: wb.Thinking, Signature: wb.Signature})
		case "redacted_thinking":
			out = append(out, domain.Block{Kind: domain.BlockRedactedThinking, Data: wb.Data})
		case "image":
			if wb.Source == nil || wb.Source.Type != "base64" {
				out = append(out, domain.Block{Kind: domain.BlockOpaque, OpaqueType: wb.Type,
					Opaque: append(json.RawMessage(nil), rawBlock...)})
				continue
			}
			data, err := base64Decode(wb.Source.Data)
			if err != nil {
				out = append(out, domain.Block{Kind: domain.BlockOpaque, OpaqueType: wb.Type,
					Opaque: append(json.RawMessage(nil), rawBlock...)})
				continue
			}
			out = append(out, domain.Block{
				Kind: domain.BlockImage, ImageMIME: wb.Source.MediaType, ImageData: data})
		case "tool_use":
			input := wb.Input
			out = append(out, domain.Block{
				Kind: domain.BlockToolUse, ID: wb.ID, Name: wb.Name, Input: input})
		case "tool_result":
			resultID := wb.ID
			if resultID == "" {
				resultID = wb.ToolUseID
			}
			if toolID == "" {
				toolID = resultID
			}
			result := wb.Content
			out = append(out, domain.Block{Kind: domain.BlockToolResult, ID: resultID,
				Result: append(json.RawMessage(nil), result...), IsError: wb.IsError})
		default:
			out = append(out, domain.Block{Kind: domain.BlockOpaque, OpaqueType: wb.Type,
				Opaque: append(json.RawMessage(nil), rawBlock...)})
		}
		// cache_control markers are prompt-caching hints; one body-level
		// mapping note covers every occurrence (system, content, tools).
	}
	return out, toolID, nil
}

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// ---------------------------------------------------------------------------
// response side

type anResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []anContentBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anUsage          `json:"usage"`
}

type anUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens"`
}

func anUsageFrom(u domain.Usage) anUsage {
	return anUsage{
		InputTokens:              u.InputTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		OutputTokens:             u.OutputTokens,
	}
}

func anStopReason(canonical string) string {
	switch canonical {
	case "tool_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func anBlocksFrom(bs []domain.Block) []anContentBlock {
	var out []anContentBlock
	for _, b := range bs {
		switch b.Kind {
		case domain.BlockText:
			out = append(out, anContentBlock{Type: "text", Text: b.Text})
		case domain.BlockThinking:
			out = append(out, anContentBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
		case domain.BlockRedactedThinking:
			out = append(out, anContentBlock{Type: "redacted_thinking", Data: b.Data})
		case domain.BlockToolUse:
			out = append(out, anContentBlock{
				Type: "tool_use", ID: b.ID, Name: b.Name, Input: normalizeArgs(b.Input)})
		}
	}
	if out == nil {
		out = []anContentBlock{}
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP entry

func (h *AnthropicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w, r, finishObservation := observeIngress(h.Eng, domain.ProtocolAnthropic, w, r)
	defer finishObservation()

	if r.Method != http.MethodPost {
		writeAnError(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST only")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeAnError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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
		writeAnError(w, status, "invalid_request_error", err.Error())
		return
	}
	captureProtocolHeaders(r, rc)
	meta := requestMeta(r, rc)

	res, f := h.Eng.Execute(r.Context(), rc, meta)
	if f != nil {
		writeAnFailure(w, f)
		return
	}
	if rc.Stream {
		streamMessages(w, res)
		return
	}
	if conversionFailure := responseConversionFailure(res, domain.ProtocolAnthropic); conversionFailure != nil {
		writeAnFailure(w, conversionFailure)
		return
	}
	writeResponseWarnings(w, res)
	if writeNativeResponse(w, res, domain.ProtocolAnthropic) {
		return
	}
	writeJSON(w, http.StatusOK, anResponse{
		ID: res.RequestID, Type: "message", Role: "assistant",
		Model:      res.Model,
		Content:    anBlocksFrom(res.Response.Content),
		StopReason: anStopReason(res.Response.FinishReason),
		Usage:      anUsageFrom(res.Response.Usage),
	})
}

// streamMessages pumps canonical events into Anthropic SSE events.
func streamMessages(w http.ResponseWriter, res *sched.Result) {
	sw := newSSEWriter(w)
	nativeMode := false
	nativeTerminal := false
	type activeBlock struct {
		kind        domain.BlockKind
		sourceIndex int
		wireIndex   int
	}
	var active *activeBlock
	nextBlock := 0
	type bufferedTool struct {
		id, name  string
		arguments strings.Builder
		ended     bool
	}
	tools := map[int]*bufferedTool{}
	toolOrder := []int{}
	emit := func(name string, payload any) { sw.event(name, payload) }
	closeBlock := func() {
		if active == nil {
			return
		}
		emit("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": active.wireIndex,
		})
		active = nil
	}
	openBlock := func(kind domain.BlockKind, sourceIndex int, content map[string]any) int {
		closeBlock()
		wireIndex := nextBlock
		nextBlock++
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": wireIndex, "content_block": content,
		})
		active = &activeBlock{kind: kind, sourceIndex: sourceIndex, wireIndex: wireIndex}
		return wireIndex
	}
	ensureBlock := func(kind domain.BlockKind, sourceIndex int) int {
		if active != nil && active.kind == kind && active.sourceIndex == sourceIndex {
			return active.wireIndex
		}
		content := map[string]any{"type": string(kind)}
		if kind == domain.BlockThinking {
			content["thinking"] = ""
			content["signature"] = ""
		} else {
			content["text"] = ""
		}
		return openBlock(kind, sourceIndex, content)
	}
	flushTools := func() bool {
		for len(toolOrder) > 0 {
			sourceIndex := toolOrder[0]
			tool := tools[sourceIndex]
			if tool == nil || !tool.ended {
				return false
			}
			wireIndex := openBlock(domain.BlockToolUse, sourceIndex, map[string]any{
				"type": "tool_use", "id": tool.id, "name": tool.name, "input": map[string]any{},
			})
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": wireIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tool.arguments.String()},
			})
			closeBlock()
			delete(tools, sourceIndex)
			toolOrder = toolOrder[1:]
		}
		return true
	}
	allToolsEnded := func() bool {
		for _, sourceIndex := range toolOrder {
			if tool := tools[sourceIndex]; tool == nil || !tool.ended {
				return false
			}
		}
		return true
	}
	flushToolsBeforeContent := func() bool {
		if len(toolOrder) == 0 {
			return true
		}
		if !allToolsEnded() {
			emit("error", map[string]any{
				"type": "error", "error": map[string]any{
					"type":    string(domain.FailProtocolConversion),
					"message": "cannot interleave Anthropic content with unfinished parallel tool calls",
				},
			})
			return false
		}
		return flushTools()
	}

	started := false
	startSynthetic := func() {
		if started {
			return
		}
		started = true
		emit("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": res.RequestID, "type": "message", "role": "assistant",
				"model": res.Model, "content": []any{},
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
	}
	for ev := range res.Events {
		if ev.Kind == domain.EvNative {
			if ev.NativeProtocol == domain.ProtocolAnthropic && ev.NativeFrame != nil {
				nativeMode = true
				nativeTerminal = nativeTerminal || ev.NativeFrame.Terminal
				writeNativeFrame(sw, res, domain.ProtocolAnthropic, ev.NativeFrame)
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
		} else {
			startSynthetic()
		}
		switch ev.Kind {
		case domain.EvTextDelta:
			if !flushToolsBeforeContent() {
				return
			}
			wi := ensureBlock(domain.BlockText, ev.Index)
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": wi,
				"delta": map[string]any{"type": "text_delta", "text": ev.TextDelta},
			})
		case domain.EvThinkingDelta:
			if !flushToolsBeforeContent() {
				return
			}
			wi := ensureBlock(domain.BlockThinking, ev.Index)
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": wi,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ev.ThinkingDelta},
			})
		case domain.EvSignatureDelta:
			if !flushToolsBeforeContent() {
				return
			}
			wi := ensureBlock(domain.BlockThinking, ev.Index)
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": wi,
				"delta": map[string]any{"type": "signature_delta", "signature": ev.SignatureDelta},
			})
		case domain.EvRedactedThinking:
			if !flushToolsBeforeContent() {
				return
			}
			openBlock(domain.BlockRedactedThinking, ev.Index, map[string]any{
				"type": "redacted_thinking", "data": ev.RedactedThinkingData,
			})
			closeBlock()
		case domain.EvToolStart:
			closeBlock()
			if tools[ev.Index] != nil {
				emit("error", map[string]any{
					"type": "error", "error": map[string]any{
						"type": "api_error", "message": "streamed tool call reused an active index",
					},
				})
				return
			}
			tools[ev.Index] = &bufferedTool{id: ev.ToolID, name: ev.ToolName}
			toolOrder = append(toolOrder, ev.Index)
		case domain.EvToolDelta:
			tool := tools[ev.Index]
			if tool == nil {
				emit("error", map[string]any{
					"type": "error", "error": map[string]any{
						"type": "api_error", "message": "streamed tool arguments arrived before their tool call",
					},
				})
				return
			}
			tool.arguments.WriteString(ev.ArgsDelta)
		case domain.EvToolEnd:
			tool := tools[ev.Index]
			if tool == nil {
				emit("error", map[string]any{
					"type": "error", "error": map[string]any{
						"type": "api_error", "message": "streamed tool call ended before it started",
					},
				})
				return
			}
			tool.ended = true
			flushTools()
		case domain.EvEnd:
			if !flushTools() {
				emit("error", map[string]any{
					"type": "error", "error": map[string]any{
						"type": "api_error", "message": "stream completed with unfinished tool calls",
					},
				})
				return
			}
			closeBlock()
			emit("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": anStopReason(ev.FinishReason), "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": usageOut(ev.Usage)},
			})
			emit("message_stop", map[string]any{"type": "message_stop"})
			return
		case domain.EvError:
			closeBlock()
			msg := "upstream stream failed"
			typ := "api_error"
			if ev.Err != nil {
				msg = ev.Err.Message
				typ = anErrType(ev.Err.Type)
			}
			emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": typ, "message": msg},
			})
			return
		}
	}
}

func usageOut(u *domain.Usage) int64 {
	if u == nil {
		return 0
	}
	return u.OutputTokens
}

func anErrType(t domain.FailureType) string {
	switch t {
	case domain.FailAuth:
		return "authentication_error"
	case domain.FailRateLimit, domain.FailQuotaExhausted:
		return "rate_limit_error"
	case domain.FailUnsupportedParam, domain.FailContextOverflow:
		return "invalid_request_error"
	case domain.FailClientCancel:
		return "request_too_long"
	default:
		return "api_error"
	}
}

// writeAnError emits an Anthropic-shaped error object.
func writeAnError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func writeAnFailure(w http.ResponseWriter, f *domain.Failure) {
	setRetryAfter(w, f)
	status := failureStatus(f)
	if status == 499 {
		status = http.StatusRequestTimeout
	}
	writeAnError(w, status, anErrType(f.Type), f.Message)
}
