package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"ferry/internal/domain"
)

// Anthropic adapts the /v1/messages upstream protocol.
type Anthropic struct{ Base }

// NewAnthropic builds an adapter for one target.
func NewAnthropic(b Base) *Anthropic { return &Anthropic{Base: b} }

const anthropicVersion = "2023-06-01"

// ---------------------------------------------------------------------------
// wire types

type anBlock struct {
	Type string `json:"type"`
	// text / thinking
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	// image
	Source *anImageSource `json:"source,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anImageSource struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anMessage struct {
	Role    string    `json:"role"`
	Content any       `json:"-"` // encoded manually: string vs []anBlock
	blocks  []anBlock `json:"-"`
	isText  bool
	text    string
}

func (m anMessage) MarshalJSON() ([]byte, error) {
	type alias struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var raw json.RawMessage
	if m.isText {
		raw = json.RawMessage(mustJSON(m.text))
	} else {
		if m.blocks == nil {
			m.blocks = []anBlock{}
		}
		raw = mustJSONAny(m.blocks)
	}
	return json.Marshal(alias{Role: m.Role, Content: raw})
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anToolChoice struct {
	Type               string `json:"type"` // auto | any | none | tool
	Name               string `json:"name,omitempty"`
	DisableParallelUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type anThinking struct {
	Type         string `json:"type"` // enabled
	BudgetTokens int64  `json:"budget_tokens"`
}

type anOutputConfigText struct {
	Verbosity string `json:"verbosity,omitempty"`
}

type anOutputConfig struct {
	Text   *anOutputConfigText `json:"text,omitempty"`
	Format *anOutputFormat     `json:"format,omitempty"`
}

type anOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anRequest struct {
	Model         string            `json:"model"`
	MaxTokens     int64             `json:"max_tokens"`
	System        json.RawMessage   `json:"system,omitempty"`
	Messages      []anMessage       `json:"messages"`
	Tools         []anTool          `json:"tools,omitempty"`
	ToolChoice    *anToolChoice     `json:"tool_choice,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	TopK          *int64            `json:"top_k,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Stream        bool              `json:"stream"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Thinking      *anThinking       `json:"thinking,omitempty"`
	OutputConfig  *anOutputConfig   `json:"output_config,omitempty"`
}

func mustJSONAny(v any) json.RawMessage { return mustJSON(v) }

// buildRequest converts canonical → Anthropic Messages wire format.
func (a *Anthropic) buildRequest(rc *domain.RequestContract) ([]byte, []string, error) {
	body, notes, err := a.buildProtocolRequest(rc)
	if err != nil {
		return nil, notes, err
	}
	body, err = normalizeProviderRequest(a.Base, domain.ProtocolAnthropic, rc, body)
	return body, notes, err
}

func (a *Anthropic) buildProtocolRequest(rc *domain.RequestContract) ([]byte, []string, error) {
	// Phase 3 exact passthrough: an anthropic-ingress request aimed at a
	// native anthropic target replays the client's body verbatim (model and
	// stream fields patched to this target), so every upstream-only field
	// survives the round trip untouched.
	if body := passthroughBody(rc, domain.ProtocolAnthropic, a.Model, nil); body != nil {
		return body, nil, nil
	}
	if err := rejectLossyConversion(rc, domain.ProtocolAnthropic); err != nil {
		return nil, nil, err
	}
	notes := canonicalRequestNotes(rc, domain.ProtocolAnthropic)
	req := anRequest{
		Model:  a.Model,
		Stream: rc.Stream,
	}
	if verbosity := strings.TrimSpace(rc.Verbosity); verbosity != "" {
		req.OutputConfig = &anOutputConfig{Text: &anOutputConfigText{Verbosity: verbosity}}
	}
	req.MaxTokens = rc.MaxTokens
	if req.MaxTokens <= 0 {
		req.MaxTokens = 4096 // anthropic requires max_tokens
	}

	var systemParts []string
	for _, m := range rc.Messages {
		switch m.Role {
		case domain.RoleSystem:
			for _, b := range m.Blocks {
				if b.Kind == domain.BlockText {
					systemParts = append(systemParts, b.Text)
				}
			}
			continue
		case domain.RoleTool:
			// tool results travel as user turns carrying tool_result blocks
			resultBlocks := make([]anBlock, 0, len(m.Blocks))
			for _, block := range m.Blocks {
				if block.Kind != domain.BlockToolResult {
					continue
				}
				toolID := block.ID
				if toolID == "" {
					toolID = m.ToolID
				}
				if len(block.Result) == 0 || !json.Valid(block.Result) {
					return nil, nil, fmt.Errorf("tool result %q content must be valid JSON", toolID)
				}
				resultBlocks = append(resultBlocks, anBlock{
					Type: "tool_result", ToolUseID: toolID,
					Content: block.Result, IsError: block.IsError,
				})
			}
			if len(resultBlocks) == 0 {
				resultBlocks = append(resultBlocks, anBlock{
					Type: "tool_result", ToolUseID: m.ToolID,
					Content: mustJSON(anTextContent(m)),
				})
			}
			req.Messages = append(req.Messages, anMessage{Role: "user", blocks: resultBlocks})
			continue
		}
		wm := anMessage{Role: string(m.Role)}
		for _, b := range m.Blocks {
			switch b.Kind {
			case domain.BlockText:
				wm.blocks = append(wm.blocks, anBlock{Type: "text", Text: b.Text})
			case domain.BlockThinking:
				wm.blocks = append(wm.blocks, anBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
			case domain.BlockRedactedThinking:
				wm.blocks = append(wm.blocks, anBlock{Type: "redacted_thinking", Data: b.Data})
			case domain.BlockImage:
				wm.blocks = append(wm.blocks, anBlock{
					Type:   "image",
					Source: &anImageSource{Type: "base64", MediaType: b.ImageMIME, Data: b64Encode(b.ImageData)},
				})
			case domain.BlockToolUse:
				if !domain.ValidJSONObject(b.Input) {
					return nil, nil, fmt.Errorf("tool call %q input must be a JSON object", b.ID)
				}
				wm.blocks = append(wm.blocks, anBlock{
					Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input,
				})
			case domain.BlockToolResult:
				if len(b.Result) == 0 || !json.Valid(b.Result) {
					return nil, nil, fmt.Errorf("tool result %q content must be valid JSON", b.ID)
				}
				wm.blocks = append(wm.blocks, anBlock{Type: "tool_result", ToolUseID: b.ID,
					Content: b.Result, IsError: b.IsError})
			}
		}
		if len(wm.blocks) == 1 && wm.blocks[0].Type == "text" {
			wm.isText = true
			wm.text = wm.blocks[0].Text
		}
		req.Messages = append(req.Messages, wm)
	}
	var normalizationNotes []string
	req.Messages, normalizationNotes = normalizeAnthropicMessages(req.Messages)
	notes = append(notes, normalizationNotes...)
	if len(systemParts) > 0 {
		req.System = json.RawMessage(mustJSON(strings.Join(systemParts, "\n\n")))
	}

	for _, t := range rc.Tools {
		req.Tools = append(req.Tools, anTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	if rc.ToolChoice != "" && len(rc.Tools) > 0 {
		tc := &anToolChoice{}
		switch rc.ToolChoice {
		case "auto":
			tc.Type = "auto"
		case "none":
			tc.Type = "none"
		case "required":
			tc.Type = "any"
		default:
			tc.Type = "tool"
			tc.Name = rc.ToolChoice
		}
		req.ToolChoice = tc
	}
	if rc.ParallelTools != nil {
		if req.ToolChoice == nil {
			req.ToolChoice = &anToolChoice{Type: "auto"}
		}
		disable := !*rc.ParallelTools
		req.ToolChoice.DisableParallelUse = &disable
	}
	if rf := rc.ResponseFormat; rf != nil {
		if len(rf.Schema) > 0 {
			if !domain.ValidJSONObject(rf.Schema) {
				return nil, nil, fmt.Errorf("structured output schema must be a JSON object")
			}
			// Routing eligibility is based on endpoint/model capability evidence,
			// not on the existence of this protocol encoder. Do not weaken schemas.
			if req.OutputConfig == nil {
				req.OutputConfig = &anOutputConfig{}
			}
			req.OutputConfig.Format = &anOutputFormat{Type: "json_schema", Schema: rf.Schema}
			origin := rc.Origin
			if origin == "" {
				origin = rc.Protocol
			}
			// OpenAI defaults omitted strict to false. Anthropic's native
			// schema enforcement cannot preserve that best-effort contract.
			if rf.Strict != nil && !*rf.Strict || rf.Strict == nil &&
				(origin == domain.ProtocolOpenAIChat || origin == domain.ProtocolOpenAIResponses) {
				notes = append(notes, "structured output strict=false (explicit or OpenAI default) upgraded to Anthropic native schema enforcement")
			}
		} else {
			// Anthropic has no equivalent of unconstrained OpenAI json_object.
			notes = append(notes, "json_object has no native Anthropic equivalent; JSON instruction injected into system prompt")
			systemParts = append(systemParts, "Respond ONLY with a JSON object.")
			req.System = mustJSON(strings.Join(systemParts, "\n\n"))
		}
	}
	if userID := rc.Metadata["user_id"]; userID != "" {
		req.Metadata = map[string]string{"user_id": userID}
	}
	if len(rc.Metadata) > 0 && (len(rc.Metadata) > 1 || rc.Metadata["user_id"] == "") {
		notes = append(notes, "metadata fields other than user_id dropped")
	}
	if rc.Lineage != nil && rc.Lineage.PromptCacheKey != "" {
		notes = append(notes, "prompt_cache_key dropped (anthropic requires cache_control blocks)")
	}
	if rc.Reasoning != nil {
		budget := rc.Reasoning.BudgetTokens
		if budget <= 0 {
			budget = reasoningBudgetForEffort(rc.Reasoning.Effort)
		}
		if budget < 1024 {
			budget = 1024
		}
		req.Thinking = &anThinking{Type: "enabled", BudgetTokens: budget}
	}
	req.Temperature = rc.Temperature
	req.TopP = rc.TopP
	req.TopK = rc.TopK
	req.StopSequences = rc.StopSequences
	return mustJSON(req), notes, nil
}

// normalizeAnthropicMessages produces the alternating user/assistant turns
// required by the Messages API. A tool result is a user content block, so it
// must be merged into an adjacent user turn rather than emitted as a second
// consecutive user message. This is especially important when converting a
// Chat or Responses tool loop to Anthropic.
func normalizeAnthropicMessages(in []anMessage) ([]anMessage, []string) {
	var notes []string
	if len(in) == 0 {
		return []anMessage{{Role: "user", isText: true, text: "..."}},
			[]string{"empty conversation replaced with an anthropic placeholder user message"}
	}
	out := make([]anMessage, 0, len(in)+1)
	if in[0].Role != "user" {
		out = append(out, anMessage{Role: "user", isText: true, text: "..."})
		notes = append(notes, "leading non-user turn requires an anthropic placeholder user message")
	}
	for _, m := range in {
		m = anthropicMessageAsBlocks(m)
		if len(m.blocks) == 0 {
			m.blocks = []anBlock{{Type: "text", Text: "..."}}
			notes = append(notes, "empty message replaced with anthropic placeholder text")
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			prev := anthropicMessageAsBlocks(out[len(out)-1])
			losslessToolResultMerge := prev.Role == "user" &&
				anthropicToolResultsOnly(prev.blocks) && anthropicToolResultsOnly(m.blocks)
			prev.blocks = append(prev.blocks, m.blocks...)
			out[len(out)-1] = prev
			if !losslessToolResultMerge {
				notes = append(notes, "consecutive same-role messages merged for anthropic alternation")
			}
			continue
		}
		out = append(out, m)
	}
	for i := range out {
		if len(out[i].blocks) == 1 && out[i].blocks[0].Type == "text" {
			out[i].isText = true
			out[i].text = out[i].blocks[0].Text
		}
	}
	return out, notes
}

func anthropicToolResultsOnly(blocks []anBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.Type != "tool_result" {
			return false
		}
	}
	return true
}

func anthropicMessageAsBlocks(m anMessage) anMessage {
	if m.isText {
		m.blocks = []anBlock{{Type: "text", Text: m.text}}
		m.isText = false
		m.text = ""
	}
	return m
}

func anTextContent(m domain.Message) []map[string]any {
	parts := make([]map[string]any, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		if b.Kind == domain.BlockText {
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		}
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"type": "text", "text": ""})
	}
	return parts
}

// ---------------------------------------------------------------------------
// non-streaming

func (a *Anthropic) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	payload, _, err := a.buildRequest(rc)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "build request: " + err.Error()}
	}
	resp, f := a.doSend(ctx, "/messages", payload, protocolHeaders(rc, domain.ProtocolAnthropic, map[string]string{
		"anthropic-version": anthropicVersion,
	}))
	if f != nil {
		return nil, f
	}
	defer resp.Body.Close()
	var out struct {
		Type         string            `json:"type"`
		Role         string            `json:"role"`
		Content      []json.RawMessage `json:"content"`
		StopReason   string            `json:"stop_reason"`
		StopSequence *string           `json:"stop_sequence"`
		Usage        json.RawMessage   `json:"usage"`
		Model        string            `json:"model"`
		Error        *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "read response: " + err.Error()}
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "decode response: " + err.Error()}
	}
	if out.Error != nil {
		ft := domain.ClassifyHTTP(resp.StatusCode, out.Error.Type+" "+out.Error.Message)
		return nil, upstreamFailure(ft, resp.StatusCode, out.Error.Message)
	}
	r := &domain.Response{Model: out.Model, NativeBody: rawBody,
		NativeContentType: resp.Header.Get("Content-Type"), NativeProtocol: domain.ProtocolAnthropic}
	r.ConversionIssues = append(r.ConversionIssues, unknownJSONFieldIssues(rawBody, "",
		"id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "usage", "error")...)
	var responseObject map[string]json.RawMessage
	_ = json.Unmarshal(rawBody, &responseObject)
	r.ConversionIssues = append(r.ConversionIssues, unknownJSONFieldIssues(responseObject["usage"], "usage",
		"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
		"service_tier")...)
	if out.Type != "" && out.Type != "message" {
		appendResponseValidationIssue(r, "Anthropic response type is not message")
	}
	if out.Role != "" && out.Role != "assistant" {
		appendResponseValidationIssue(r, "Anthropic response role is not assistant")
	}
	for index, rawBlock := range out.Content {
		r.ConversionIssues = append(r.ConversionIssues, unknownJSONFieldIssues(rawBlock,
			fmt.Sprintf("content[%d]", index), "type", "text", "thinking", "signature", "data",
			"source", "id", "name", "input", "tool_use_id", "content", "is_error")...)
		var b anBlock
		if err := json.Unmarshal(rawBlock, &b); err != nil {
			appendResponseValidationIssue(r, "invalid Anthropic content block")
			continue
		}
		switch b.Type {
		case "text":
			r.Content = append(r.Content, domain.Block{Kind: domain.BlockText, Text: b.Text})
		case "thinking":
			r.Content = append(r.Content, domain.Block{Kind: domain.BlockThinking, Text: b.Thinking, Signature: b.Signature})
			if b.Signature == "" {
				appendResponseValidationIssue(r, "Anthropic thinking block is missing its signature")
			}
			if rc.Origin != domain.ProtocolAnthropic {
				r.ConversionIssues = append(r.ConversionIssues,
					"Anthropic thinking blocks require a native Anthropic response path")
			}
		case "redacted_thinking":
			r.Content = append(r.Content, domain.Block{Kind: domain.BlockRedactedThinking, Data: b.Data})
		case "tool_use":
			r.Content = append(r.Content, domain.Block{
				Kind: domain.BlockToolUse, ID: b.ID, Name: b.Name, Input: b.Input,
			})
		default:
			r.Content = append(r.Content, domain.Block{Kind: domain.BlockOpaque, OpaqueType: b.Type,
				Opaque: append(json.RawMessage(nil), rawBlock...)})
			r.ConversionIssues = append(r.ConversionIssues, "content type "+b.Type+" is native-only")
		}
	}
	r.FinishReason = mapAnthropicStop(out.StopReason)
	if out.StopReason == "" {
		appendResponseValidationIssue(r, "Anthropic response is missing stop_reason")
	}
	if out.StopSequence != nil {
		r.ConversionIssues = append(r.ConversionIssues, "Anthropic stop_sequence is native-only")
	}
	if out.StopReason != "" && out.StopReason != "end_turn" && out.StopReason != "stop_sequence" &&
		out.StopReason != "tool_use" && out.StopReason != "max_tokens" && out.StopReason != "pause_turn" &&
		out.StopReason != "refusal" {
		r.ConversionIssues = append(r.ConversionIssues,
			"Anthropic stop reason "+out.StopReason+" is native-only")
	}
	if out.StopReason == "pause_turn" || out.StopReason == "refusal" {
		r.ConversionIssues = append(r.ConversionIssues,
			"Anthropic stop reason "+out.StopReason+" is native-only")
	}
	r.Usage = decodeUsage(out.Usage)
	markResponseConversionIssues(r)
	return r, nil
}

func mapAnthropicStop(s string) string {
	switch s {
	case "tool_use":
		return "tool_call"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// ---------------------------------------------------------------------------
// streaming

func (a *Anthropic) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	rc.Stream = true
	payload, _, err := a.buildRequest(rc)
	if err != nil {
		return nil, nil, &domain.Failure{Type: domain.FailBadResponse, Message: "build request: " + err.Error()}
	}
	resp, f := a.doSend(ctx, "/messages", payload, protocolHeaders(rc, domain.ProtocolAnthropic, map[string]string{
		"anthropic-version": anthropicVersion,
		"Accept":            "text/event-stream",
	}))
	if f != nil {
		return nil, nil, f
	}
	ch := make(chan domain.StreamEvent, 64)
	cancel := func() { resp.Body.Close() } // #nosec G104 -- best-effort close of a finished or abandoned upstream body
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		usage := domain.Usage{}
		responseModel := ""
		finish := ""
		blockKinds := map[int]string{}
		toolArguments := map[int]string{}
		toolStartInputs := map[int]string{}
		toolStartComplete := map[int]bool{}
		toolSawDelta := map[int]bool{}
		thinkingSignatures := map[int]bool{}
		messageStarted := false
		messageDeltaSeen := false
		nativePass := rc.Origin == domain.ProtocolAnthropic && len(rc.Native) > 0
		err := readSSE(resp.Body, func(ev sseEvent) error {
			var body struct {
				Type    string `json:"type"`
				Message *struct {
					Model string           `json:"model"`
					Usage *json.RawMessage `json:"usage"`
				} `json:"message"`
				Index        int `json:"index"`
				ContentBlock *struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Text      string          `json:"text"`
					Thinking  string          `json:"thinking"`
					Signature string          `json:"signature"`
					Data      string          `json:"data"`
					Input     json.RawMessage `json:"input"`
				} `json:"content_block"`
				Delta *struct {
					Type         string  `json:"type"`
					Text         string  `json:"text"`
					Thinking     string  `json:"thinking"`
					Signature    string  `json:"signature"`
					PartialJSON  string  `json:"partial_json"`
					StopReason   string  `json:"stop_reason"`
					StopSequence *string `json:"stop_sequence"`
				} `json:"delta"`
				Usage *json.RawMessage `json:"usage"`
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(ev.data), &body); err != nil {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
					Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
					Message: "Anthropic stream emitted invalid JSON"}}
				return ioEnd
			}
			validationFailure := func(message string) error {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
					Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget, Message: message}}
				return ioEnd
			}
			switch body.Type {
			case "message_start":
				if messageStarted || body.Message == nil {
					return validationFailure("Anthropic stream emitted an invalid or duplicate message_start")
				}
			case "content_block_start":
				if !messageStarted || messageDeltaSeen || body.ContentBlock == nil {
					return validationFailure("Anthropic content block started outside an active message")
				}
				if _, exists := blockKinds[body.Index]; exists {
					return validationFailure("Anthropic stream reused an open content block index")
				}
			case "content_block_delta":
				if !messageStarted || messageDeltaSeen || body.Delta == nil {
					return validationFailure("Anthropic content delta arrived outside an active message")
				}
				if _, exists := blockKinds[body.Index]; !exists {
					return validationFailure("Anthropic content delta arrived outside an open content block")
				}
			case "content_block_stop":
				if !messageStarted || messageDeltaSeen {
					return validationFailure("Anthropic content block stopped outside an active message")
				}
				if _, exists := blockKinds[body.Index]; !exists {
					return validationFailure("Anthropic stream closed an unknown content block")
				}
			case "message_delta":
				if !messageStarted || messageDeltaSeen || len(blockKinds) > 0 || body.Delta == nil || body.Delta.StopReason == "" {
					return validationFailure("Anthropic message_delta is missing its terminal stop state")
				}
			case "message_stop":
				if !messageStarted || !messageDeltaSeen || len(blockKinds) > 0 || finish == "" {
					return validationFailure("Anthropic message_stop arrived before a complete terminal message_delta")
				}
			}
			terminal := body.Type == "message_stop" || body.Type == "error"
			if nativePass {
				ch <- nativeStreamEvent(domain.ProtocolAnthropic, ev, terminal)
			}
			switch body.Type {
			case "ping":
				return nil
			case "message_start":
				messageStarted = true
				responseModel = body.Message.Model
				if responseModel != "" {
					ch <- domain.StreamEvent{Kind: domain.EvStart, Model: responseModel}
				}
				if body.Message != nil && body.Message.Usage != nil {
					u := decodeUsage(*body.Message.Usage)
					usage.InputTokens += u.InputTokens
					usage.CacheReadTokens += u.CacheReadTokens
					usage.CacheWriteTokens += u.CacheWriteTokens
				}
			case "content_block_start":
				if cb := body.ContentBlock; cb != nil {
					blockKinds[body.Index] = cb.Type
					switch cb.Type {
					case "tool_use":
						if cb.ID == "" || cb.Name == "" {
							return validationFailure("Anthropic tool block is missing its id or name")
						}
						input := strings.TrimSpace(string(cb.Input))
						if input == "" || input == "null" || !domain.ValidJSONObject(cb.Input) {
							return validationFailure("Anthropic tool block input must start as a JSON object")
						}
						var inputObject map[string]json.RawMessage
						_ = json.Unmarshal(cb.Input, &inputObject)
						toolStartInputs[body.Index] = input
						toolStartComplete[body.Index] = len(inputObject) > 0
						ch <- domain.StreamEvent{Kind: domain.EvToolStart, Index: body.Index,
							ToolID: cb.ID, ToolName: cb.Name}
						if toolStartComplete[body.Index] {
							toolArguments[body.Index] = input
							ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: body.Index,
								ArgsDelta: input}
						}
					case "redacted_thinking":
						if rc.Origin != domain.ProtocolAnthropic {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Anthropic redacted thinking cannot be losslessly converted"}}
							return ioEnd
						}
						if cb.Data == "" {
							return validationFailure("Anthropic redacted thinking block is missing data")
						}
						ch <- domain.StreamEvent{Kind: domain.EvRedactedThinking, Index: body.Index,
							RedactedThinkingData: cb.Data}
					case "thinking":
						if rc.Origin != domain.ProtocolAnthropic {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Anthropic thinking blocks require a native Anthropic response path"}}
							return ioEnd
						}
						if cb.Thinking != "" {
							ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta, Index: body.Index,
								ThinkingDelta: cb.Thinking}
						}
						if cb.Signature != "" {
							thinkingSignatures[body.Index] = true
							ch <- domain.StreamEvent{Kind: domain.EvSignatureDelta, Index: body.Index,
								SignatureDelta: cb.Signature}
						}
					case "text":
						if cb.Text != "" {
							ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: body.Index, TextDelta: cb.Text}
						}
					default:
						if !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Anthropic content type " + cb.Type + " cannot be losslessly converted"}}
							return ioEnd
						}
					}
				}
			case "content_block_delta":
				if body.Delta == nil {
					return nil
				}
				switch body.Delta.Type {
				case "text_delta":
					if blockKinds[body.Index] != "text" && !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Anthropic text delta arrived outside a text block"}}
						return ioEnd
					}
					ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: body.Index, TextDelta: body.Delta.Text}
				case "thinking_delta":
					if blockKinds[body.Index] != "thinking" && !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Anthropic thinking delta arrived outside a thinking block"}}
						return ioEnd
					}
					ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta, Index: body.Index, ThinkingDelta: body.Delta.Thinking}
				case "signature_delta":
					if rc.Origin != domain.ProtocolAnthropic {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Anthropic thinking signature cannot be losslessly converted"}}
						return ioEnd
					}
					if (blockKinds[body.Index] != "thinking" || body.Delta.Signature == "") && !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Anthropic thinking signature is missing or outside a thinking block"}}
						return ioEnd
					}
					if body.Delta.Signature != "" {
						thinkingSignatures[body.Index] = true
					}
					ch <- domain.StreamEvent{Kind: domain.EvSignatureDelta, Index: body.Index, SignatureDelta: body.Delta.Signature}
				case "input_json_delta":
					if blockKinds[body.Index] != "tool_use" {
						return validationFailure("Anthropic tool arguments arrived outside a tool block")
					}
					if toolStartComplete[body.Index] {
						return validationFailure("Anthropic tool block mixed a complete input object with argument deltas")
					}
					toolSawDelta[body.Index] = true
					toolArguments[body.Index] += body.Delta.PartialJSON
					ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: body.Index,
						ArgsDelta: body.Delta.PartialJSON}
				default:
					if !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Anthropic delta type " + body.Delta.Type + " cannot be losslessly converted"}}
						return ioEnd
					}
				}
			case "content_block_stop":
				kind := blockKinds[body.Index]
				if kind == "thinking" && !thinkingSignatures[body.Index] && !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Anthropic thinking block ended without a signature"}}
					return ioEnd
				}
				if blockKinds[body.Index] == "tool_use" {
					arguments := toolArguments[body.Index]
					if !toolSawDelta[body.Index] && arguments == "" {
						arguments = toolStartInputs[body.Index]
						if arguments != "" {
							toolArguments[body.Index] = arguments
							ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: body.Index,
								ArgsDelta: arguments}
						}
					}
					if !domain.ValidJSONObject(json.RawMessage(arguments)) {
						return validationFailure("Anthropic tool call arguments are missing or not a JSON object")
					}
					ch <- domain.StreamEvent{Kind: domain.EvToolEnd, Index: body.Index}
				}
				delete(blockKinds, body.Index)
				delete(toolArguments, body.Index)
				delete(toolStartInputs, body.Index)
				delete(toolStartComplete, body.Index)
				delete(toolSawDelta, body.Index)
				delete(thinkingSignatures, body.Index)
			case "message_delta":
				messageDeltaSeen = true
				if body.Delta != nil {
					stopReason := body.Delta.StopReason
					if stopReason != "" && stopReason != "end_turn" && stopReason != "stop_sequence" &&
						stopReason != "tool_use" && stopReason != "max_tokens" && !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Anthropic stop reason " + stopReason + " is native-only"}}
						return ioEnd
					}
					if body.Delta.StopSequence != nil && !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Anthropic stop_sequence is native-only"}}
						return ioEnd
					}
					finish = mapAnthropicStop(body.Delta.StopReason)
				}
				if body.Usage != nil {
					u := decodeUsage(*body.Usage)
					usage.OutputTokens += u.OutputTokens
				}
			case "message_stop":
				u := usage
				ch <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: finish, Usage: &u, Model: responseModel}
				return ioEnd
			case "error":
				msg := "upstream stream error"
				typ := ""
				if body.Error != nil {
					msg = body.Error.Message
					typ = body.Error.Type
				}
				ft := domain.ClassifyHTTP(0, typ+" "+msg)
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: upstreamFailure(ft, 0, msg)}
				return ioEnd
			default:
				if !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Anthropic event type " + body.Type + " cannot be losslessly converted"}}
					return ioEnd
				}
			}
			return nil
		})
		if err != nil && err != ioEnd {
			ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
				Type: domain.FailStreamBroken, Message: err.Error()}}
		}
	}()
	return ch, cancel, nil
}
