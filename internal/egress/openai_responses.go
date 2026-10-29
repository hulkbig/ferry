package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"ferry/internal/domain"
)

// OpenAIResponses adapts the /v1/responses upstream protocol.
type OpenAIResponses struct{ Base }

// NewOpenAIResponses builds an adapter for one target.
func NewOpenAIResponses(b Base) *OpenAIResponses { return &OpenAIResponses{Base: b} }

// ---------------------------------------------------------------------------
// wire types

type rsInputItem struct {
	Type    string `json:"type,omitempty"` // message | function_call | function_call_output
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"` // string or []rsContentPart

	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Args   string `json:"arguments,omitempty"`
	Output string `json:"output,omitempty"`
	ItemID string `json:"id,omitempty"`
}

type rsContentPart struct {
	Type     string `json:"type"` // input_text | output_text | reasoning_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type rsTool struct {
	Type        string          `json:"type"` // function
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type rsReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type rsTextFormat struct {
	Type   string          `json:"type"` // json_schema
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

type rsRequest struct {
	Model              string            `json:"model"`
	Input              []rsInputItem     `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	Tools              []rsTool          `json:"tools,omitempty"`
	ToolChoice         any               `json:"tool_choice,omitempty"`
	Reasoning          *rsReasoning      `json:"reasoning,omitempty"`
	Text               *rsTextWrapper    `json:"text,omitempty"`
	MaxOutputTokens    int64             `json:"max_output_tokens,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	ParallelTools      *bool             `json:"parallel_tool_calls,omitempty"`
	Stream             bool              `json:"stream"`
	ServiceTier        string            `json:"service_tier,omitempty"`
	Store              bool              `json:"store"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Conversation       string            `json:"conversation,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	PromptCacheKey     string            `json:"prompt_cache_key,omitempty"`
}

type rsTextWrapper struct {
	Format    *rsTextFormat `json:"format,omitempty"`
	Verbosity string        `json:"verbosity,omitempty"`
}

// responseStreamItem is the semantic subset of output_item.added/done used
// to verify that the final item agrees with the deltas already emitted. The
// done item is authoritative in the Responses protocol; ignoring it can turn
// a done-only provider stream into an empty answer or hide annotations and
// reasoning state that another client protocol cannot represent.
type responseStreamItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Status           string `json:"status"`
	Role             string `json:"role"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type        string          `json:"type"`
		Text        string          `json:"text"`
		Annotations json.RawMessage `json:"annotations"`
		Logprobs    json.RawMessage `json:"logprobs"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

type responseStreamError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responseStreamResponse struct {
	ID                string          `json:"id"`
	Conversation      json.RawMessage `json:"conversation"`
	Status            string          `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage       json.RawMessage      `json:"usage"`
	Model       string               `json:"model"`
	ServiceTier string               `json:"service_tier"`
	Error       *responseStreamError `json:"error"`
}

type responseStreamEnvelope struct {
	Type        string                  `json:"type"`
	ItemID      string                  `json:"item_id"`
	Delta       string                  `json:"delta"`
	Text        string                  `json:"text"`
	Arguments   string                  `json:"arguments"`
	OutputIndex *int                    `json:"output_index"`
	Response    *responseStreamResponse `json:"response"`
	Item        *responseStreamItem     `json:"item"`
	Error       *responseStreamError    `json:"error"`
	Code        string                  `json:"code"`
	Message     string                  `json:"message"`
}

func responseStreamTerminalValidationFailure(body *responseStreamEnvelope) *domain.Failure {
	if body == nil {
		return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
			Message: "Responses stream emitted an empty terminal envelope"}
	}
	bad := func(message string) *domain.Failure {
		return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget, Message: message}
	}
	switch body.Type {
	case "response.completed", "response.done":
		if body.Response == nil || body.Response.Status != "completed" {
			return bad("Responses completed event is missing a completed response")
		}
		if body.Response.IncompleteDetails != nil && body.Response.IncompleteDetails.Reason != "" {
			return bad("Responses completed event contains incomplete details")
		}
	case "response.incomplete":
		if body.Response == nil || body.Response.Status != "incomplete" {
			return bad("Responses incomplete event is missing an incomplete response")
		}
		if body.Response.IncompleteDetails == nil || body.Response.IncompleteDetails.Reason == "" {
			return bad("Responses incomplete event is missing incomplete_details.reason")
		}
	case "response.failed":
		if body.Response == nil || body.Response.Status != "failed" {
			return bad("Responses failed event is missing a failed response")
		}
		if body.Response.Error == nil || body.Response.Error.Message == "" {
			return bad("Responses failed event is missing response.error.message")
		}
	case "response.canceled", "response.cancelled":
		if body.Response == nil || (body.Response.Status != "canceled" && body.Response.Status != "cancelled") {
			return bad("Responses canceled event is missing a canceled response")
		}
	case "response.error", "error":
		if body.Message == "" && (body.Error == nil || body.Error.Message == "") &&
			(body.Response == nil || body.Response.Error == nil || body.Response.Error.Message == "") {
			return bad("Responses error event is missing an error message")
		}
	}
	return nil
}

func responseStreamItemText(item *responseStreamItem) (string, []string) {
	if item == nil {
		return "", nil
	}
	var text strings.Builder
	var issues []string
	if item.Type == "message" && item.Role != "" && item.Role != "assistant" {
		issues = append(issues, "Responses output message role "+item.Role+" is native-only")
	}
	for _, content := range item.Content {
		switch {
		case item.Type == "message" && content.Type == "output_text":
			text.WriteString(content.Text)
		case item.Type == "reasoning" && content.Type == "reasoning_text":
			text.WriteString(content.Text)
		default:
			issues = append(issues, "Responses "+item.Type+" content type "+content.Type+" is native-only")
		}
		if len(content.Annotations) > 0 && string(content.Annotations) != "null" && string(content.Annotations) != "[]" {
			issues = append(issues, "Responses output annotations are native-only")
		}
		if len(content.Logprobs) > 0 && string(content.Logprobs) != "null" && string(content.Logprobs) != "[]" {
			issues = append(issues, "Responses output logprobs are native-only")
		}
	}
	for _, summary := range item.Summary {
		if item.Type != "reasoning" || summary.Type != "summary_text" {
			issues = append(issues, "Responses "+item.Type+" summary type "+summary.Type+" is native-only")
		}
	}
	if item.EncryptedContent != "" {
		issues = append(issues, "encrypted Responses reasoning is native-only")
	}
	return text.String(), issues
}

func responseStreamItemTextForOrigin(item *responseStreamItem, origin domain.Protocol) (string, []string) {
	text, issues := responseStreamItemText(item)
	if item == nil || item.Type != "reasoning" || origin != domain.ProtocolOpenAIChat {
		return text, issues
	}
	filtered := issues[:0]
	for _, issue := range issues {
		if issue == "Responses reasoning summary is native-only" ||
			issue == "encrypted Responses reasoning is native-only" {
			continue
		}
		filtered = append(filtered, issue)
	}
	return text, filtered
}

func demoteResponsesAdvisories(res *domain.Response, origin domain.Protocol) {
	if res == nil || origin == domain.ProtocolOpenAIResponses {
		return
	}
	blocking := res.ConversionIssues[:0]
	for _, issue := range res.ConversionIssues {
		switch issue {
		case "response field caching is native-only":
			appendResponseWarning(res, "provider caching metadata omitted during protocol conversion")
		case "Responses reasoning summary is native-only":
			if origin == domain.ProtocolOpenAIChat {
				appendResponseWarning(res, "Responses reasoning summary mapped to Chat reasoning_content")
				continue
			}
			blocking = append(blocking, issue)
		case "encrypted Responses reasoning is native-only", "encrypted reasoning is native-only":
			if origin == domain.ProtocolOpenAIChat {
				appendResponseWarning(res, "encrypted provider reasoning state omitted during Chat conversion")
				continue
			}
			blocking = append(blocking, issue)
		default:
			blocking = append(blocking, issue)
		}
	}
	res.ConversionIssues = blocking
}

func responseStreamItemSummaryText(item *responseStreamItem) string {
	if item == nil || item.Type != "reasoning" {
		return ""
	}
	var text strings.Builder
	for _, summary := range item.Summary {
		if summary.Type == "summary_text" {
			text.WriteString(summary.Text)
		}
	}
	return text.String()
}

func responseClientProtocol(rc *domain.RequestContract) domain.Protocol {
	if rc == nil {
		return ""
	}
	if rc.Origin != "" {
		return rc.Origin
	}
	return rc.Protocol
}

func responseReasoningSummaryConversionIssue(rc *domain.RequestContract) string {
	switch responseClientProtocol(rc) {
	case domain.ProtocolOpenAIChat:
		return ""
	case domain.ProtocolAnthropic:
		return "Responses reasoning summary cannot be represented as Anthropic thinking"
	default:
		return "Responses reasoning summary is native-only"
	}
}

func recordResponseReasoningSummary(res *domain.Response, rc *domain.RequestContract) {
	if issue := responseReasoningSummaryConversionIssue(rc); issue != "" {
		appendResponseConversionIssue(res, issue)
		return
	}
	appendResponseWarning(res, "Responses reasoning summary mapped to Chat reasoning_content")
}

func responseStreamKey(itemID string, outputIndex *int, kind string) string {
	if itemID != "" {
		return "id:" + itemID + ":" + kind
	}
	if outputIndex != nil {
		return fmt.Sprintf("output:%d:%s", *outputIndex, kind)
	}
	// Some compatible providers omit item identifiers. Retain a single
	// unindexed item per semantic kind while still keeping text and reasoning
	// independent; final-item validation catches any ambiguity.
	return "anonymous:" + kind
}

// buildRequest converts canonical → Responses wire format.
func (r *OpenAIResponses) buildRequest(rc *domain.RequestContract) ([]byte, []string, error) {
	body, notes, err := r.buildProtocolRequest(rc)
	if err != nil {
		return nil, notes, err
	}
	body, err = normalizeProviderRequest(r.Base, domain.ProtocolOpenAIResponses, rc, body)
	return body, notes, err
}

func (r *OpenAIResponses) buildProtocolRequest(rc *domain.RequestContract) ([]byte, []string, error) {
	// Phase 3 exact passthrough: a responses-ingress request aimed at a
	// native responses target replays the client's body verbatim (model and
	// stream patched to gateway policy), so client-side fields like
	// reasoning summaries or text formatting survive untouched.
	if body := passthroughBody(rc, domain.ProtocolOpenAIResponses, r.Model, nil); body != nil {
		// Client virtual lineage never reaches providers. The scheduler inserts
		// only a provider-native continuation handle when it is valid for this
		// exact connection; otherwise canonical history is rehydrated.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err == nil {
			delete(m, "conversation")
			delete(m, "previous_response_id")
			if rc.Continuation != nil {
				if rc.Continuation.NativeResponseID != "" {
					value, _ := json.Marshal(rc.Continuation.NativeResponseID)
					m["previous_response_id"] = value
				} else if rc.Continuation.NativeConversationID != "" {
					value, _ := json.Marshal(rc.Continuation.NativeConversationID)
					m["conversation"] = value
				}
			}
			store, _ := json.Marshal(rc.PersistNativeState)
			m["store"] = store
			if out, err := json.Marshal(m); err == nil {
				return out, nil, nil
			}
		}
		return body, nil, nil
	}
	if err := rejectLossyConversion(rc, domain.ProtocolOpenAIResponses); err != nil {
		return nil, nil, err
	}
	notes := canonicalRequestNotes(rc, domain.ProtocolOpenAIResponses)
	req := rsRequest{
		Model:       r.Model,
		Stream:      rc.Stream,
		Store:       rc.PersistNativeState,
		ServiceTier: rc.ServiceTier,
	}
	if rc.Continuation != nil {
		if rc.Continuation.NativeResponseID != "" {
			req.PreviousResponseID = rc.Continuation.NativeResponseID
		} else {
			req.Conversation = rc.Continuation.NativeConversationID
		}
	}
	if rc.Lineage != nil {
		req.PromptCacheKey = rc.Lineage.PromptCacheKey
	}
	var instructions []string
	for _, m := range rc.Messages {
		switch m.Role {
		case domain.RoleSystem:
			for _, b := range m.Blocks {
				if b.Kind == domain.BlockText {
					instructions = append(instructions, b.Text)
				}
			}
			continue
		case domain.RoleTool:
			emitted := false
			for _, block := range m.Blocks {
				if block.Kind != domain.BlockToolResult {
					continue
				}
				callID := block.ID
				if callID == "" {
					callID = m.ToolID
				}
				req.Input = append(req.Input, rsInputItem{
					Type: "function_call_output", CallID: callID,
					Output: toolResultText(block.Result),
				})
				emitted = true
			}
			if !emitted {
				req.Input = append(req.Input, rsInputItem{
					Type: "function_call_output", CallID: m.ToolID,
					Output: anTextContent2(m),
				})
			}
			continue
		}
		itemType := "message"
		if m.Role == domain.RoleAssistant {
			itemType = "" // assistant items default to message type in responses API
		}
		parts := make([]rsContentPart, 0, len(m.Blocks))
		var calls []rsInputItem
		for _, b := range m.Blocks {
			switch b.Kind {
			case domain.BlockText:
				pt := "output_text"
				if m.Role != domain.RoleAssistant {
					pt = "input_text"
				}
				parts = append(parts, rsContentPart{Type: pt, Text: b.Text})
			case domain.BlockThinking:
				notes = append(notes, "thinking block dropped (responses adapter keeps only encrypted/summary forms)")
			case domain.BlockImage:
				parts = append(parts, rsContentPart{
					Type:     "input_image",
					ImageURL: fmt.Sprintf("data:%s;base64,%s", b.ImageMIME, b64Encode(b.ImageData)),
				})
			case domain.BlockToolUse:
				calls = append(calls, rsInputItem{
					Type: "function_call", CallID: b.ID, Name: b.Name,
					Args: string(b.Input),
				})
			case domain.BlockToolResult:
				req.Input = append(req.Input, rsInputItem{Type: "function_call_output",
					CallID: b.ID, Output: toolResultText(b.Result)})
			}
		}
		if len(parts) > 0 {
			it := rsInputItem{Role: string(m.Role), Content: parts}
			if itemType != "" {
				it.Type = itemType
			}
			req.Input = append(req.Input, it)
		}
		req.Input = append(req.Input, calls...)
	}
	if len(instructions) > 0 {
		req.Instructions = joinNewlines(instructions)
	}
	for _, t := range rc.Tools {
		req.Tools = append(req.Tools, rsTool{
			Type: "function", Name: t.Name,
			Description: t.Description, Parameters: t.Parameters,
		})
	}
	switch rc.ToolChoice {
	case "", "auto":
	case "none", "required":
		req.ToolChoice = rc.ToolChoice
	default:
		req.ToolChoice = map[string]any{"type": "function", "name": rc.ToolChoice}
	}
	if rf := rc.ResponseFormat; rf != nil {
		if len(rf.Schema) > 0 {
			req.Text = &rsTextWrapper{Format: &rsTextFormat{
				Type: "json_schema", Name: orDefault(rf.Name, "response"), Schema: rf.Schema, Strict: rf.Strict}}
		} else {
			req.Text = &rsTextWrapper{Format: &rsTextFormat{Type: "json_object"}}
		}
	}
	if verbosity := strings.TrimSpace(rc.Verbosity); verbosity != "" {
		if req.Text == nil {
			req.Text = &rsTextWrapper{}
		}
		req.Text.Verbosity = verbosity
	}
	if rc.Reasoning != nil {
		rq := &rsReasoning{Effort: rc.Reasoning.Effort}
		if rc.Reasoning.BudgetTokens > 0 {
			rq.Effort = reasoningEffortForBudget(rc.Reasoning.BudgetTokens)
		}
		req.Reasoning = rq
	}
	if rc.MaxTokens > 0 {
		req.MaxOutputTokens = rc.MaxTokens
	}
	req.Temperature = rc.Temperature
	req.TopP = rc.TopP
	req.Metadata = rc.Metadata
	req.ParallelTools = rc.ParallelTools
	if len(rc.StopSequences) > 0 {
		notes = append(notes, "stop sequences dropped (Responses has no standard stop field)")
	}
	if rc.TopK != nil {
		notes = append(notes, "top_k dropped (responses has no equivalent)")
	}
	return mustJSON(req), notes, nil
}

func anTextContent2(m domain.Message) string {
	var sb strings2Builder
	for i, b := range m.Blocks {
		if b.Kind == domain.BlockText {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func responsesOutputFieldIssues(rawItem []byte, index int) []string {
	path := fmt.Sprintf("output[%d]", index)
	issues := unknownJSONFieldIssues(rawItem, path,
		"id", "type", "status", "role", "content", "summary", "encrypted_content",
		"call_id", "name", "arguments")
	var item map[string]json.RawMessage
	_ = json.Unmarshal(rawItem, &item)
	var content []json.RawMessage
	_ = json.Unmarshal(item["content"], &content)
	for contentIndex, rawContent := range content {
		contentPath := fmt.Sprintf("%s.content[%d]", path, contentIndex)
		issues = append(issues, unknownJSONFieldIssues(rawContent, contentPath,
			"type", "text", "annotations", "logprobs")...)
		var part map[string]json.RawMessage
		_ = json.Unmarshal(rawContent, &part)
		if logprobs := part["logprobs"]; len(logprobs) > 0 && string(logprobs) != "null" {
			issues = append(issues, "response field "+contentPath+".logprobs is native-only")
		}
	}
	var summary []json.RawMessage
	_ = json.Unmarshal(item["summary"], &summary)
	for summaryIndex, rawSummary := range summary {
		issues = append(issues, unknownJSONFieldIssues(rawSummary,
			fmt.Sprintf("%s.summary[%d]", path, summaryIndex), "type", "text")...)
	}
	return issues
}

func joinNewlines(parts []string) string {
	var sb strings2Builder
	for i, p := range parts {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(p)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// non-streaming

func (r *OpenAIResponses) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	payload, _, err := r.buildRequest(rc)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "build request: " + err.Error()}
	}
	resp, f := r.doSend(ctx, "/responses", payload,
		protocolHeaders(rc, domain.ProtocolOpenAIResponses, nil))
	if f != nil {
		return nil, f
	}
	defer resp.Body.Close()
	var out struct {
		ID           string          `json:"id"`
		Conversation json.RawMessage `json:"conversation"`
		Status       string          `json:"status"`
		Error        *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Output            []json.RawMessage `json:"output"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage       json.RawMessage `json:"usage"`
		Model       string          `json:"model"`
		ServiceTier string          `json:"service_tier"`
	}
	type outputItem struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Role    string `json:"role"`
		Content []struct {
			Type        string          `json:"type"`
			Text        string          `json:"text"`
			Annotations json.RawMessage `json:"annotations"`
		} `json:"content"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		EncryptedContent string `json:"encrypted_content"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
	}
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "read response: " + err.Error()}
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "decode response: " + err.Error()}
	}
	if out.Error != nil {
		ft := domain.ClassifyHTTP(resp.StatusCode, out.Error.Code+" "+out.Error.Message)
		return nil, upstreamFailure(ft, resp.StatusCode, out.Error.Message)
	}
	res := &domain.Response{
		Model: out.Model, ServiceTier: out.ServiceTier, NativeResponseID: out.ID,
		NativeConversationID: nativeConversationID(out.Conversation),
		NativeBody:           rawBody, NativeContentType: resp.Header.Get("Content-Type"),
		NativeProtocol: domain.ProtocolOpenAIResponses,
	}
	if out.Status == "" {
		appendResponseValidationIssue(res, "Responses response is missing status")
	}
	res.ConversionIssues = append(res.ConversionIssues, unknownJSONFieldIssues(rawBody, "",
		"id", "object", "created_at", "completed_at", "conversation", "status", "error", "output",
		"incomplete_details", "usage", "model", "background", "billing", "instructions",
		"max_output_tokens", "max_tool_calls", "metadata", "parallel_tool_calls", "previous_response_id",
		"prompt", "prompt_cache_key", "prompt_cache_retention", "reasoning", "safety_identifier",
		"service_tier", "store", "temperature", "text", "tool_choice", "tools", "top_logprobs",
		"top_p", "truncation", "user")...)
	var responseObject map[string]json.RawMessage
	_ = json.Unmarshal(rawBody, &responseObject)
	res.ConversionIssues = append(res.ConversionIssues, unknownJSONFieldIssues(responseObject["usage"], "usage",
		"input_tokens", "output_tokens", "total_tokens", "input_tokens_details", "output_tokens_details")...)
	var responseUsage map[string]json.RawMessage
	_ = json.Unmarshal(responseObject["usage"], &responseUsage)
	res.ConversionIssues = append(res.ConversionIssues, unknownJSONFieldIssues(responseUsage["input_tokens_details"],
		"usage.input_tokens_details", "cached_tokens", "cache_write_tokens", "cache_creation_tokens")...)
	res.ConversionIssues = append(res.ConversionIssues, unknownJSONFieldIssues(responseUsage["output_tokens_details"],
		"usage.output_tokens_details", "reasoning_tokens")...)
	if out.Status != "" && out.Status != "completed" && out.Status != "incomplete" {
		res.ConversionIssues = append(res.ConversionIssues,
			"Responses status "+out.Status+" is native-only")
	}
	for index, rawItem := range out.Output {
		res.ConversionIssues = append(res.ConversionIssues, responsesOutputFieldIssues(rawItem, index)...)
		var item outputItem
		if json.Unmarshal(rawItem, &item) != nil {
			appendResponseValidationIssue(res, "invalid Responses output item")
			continue
		}
		if item.Status != "" && item.Status != "completed" {
			appendResponseValidationIssue(res, "Responses output item is not completed")
		}
		if item.Type == "message" && item.Role != "" && item.Role != "assistant" {
			appendResponseValidationIssue(res, "Responses output message role is not assistant")
		}
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					res.Content = append(res.Content, domain.Block{Kind: domain.BlockText, Text: c.Text})
				} else if c.Type == "refusal" && c.Text != "" {
					res.Content = append(res.Content, domain.Block{Kind: domain.BlockText, Text: c.Text})
					res.ConversionIssues = append(res.ConversionIssues, "Responses refusal is native-only")
				} else if c.Type != "output_text" && c.Type != "refusal" {
					res.ConversionIssues = append(res.ConversionIssues, "Responses content type "+c.Type+" is native-only")
				}
				if len(c.Annotations) > 0 && string(c.Annotations) != "null" && string(c.Annotations) != "[]" {
					res.ConversionIssues = append(res.ConversionIssues, "Responses annotations are native-only")
				}
			}
		case "reasoning":
			for _, c := range item.Content {
				if (c.Type == "reasoning_text" || c.Type == "summary_text") && c.Text != "" {
					res.Content = append(res.Content, domain.Block{Kind: domain.BlockThinking, Text: c.Text})
					if c.Type == "summary_text" {
						recordResponseReasoningSummary(res, rc)
					} else if rc.Origin == domain.ProtocolAnthropic {
						res.ConversionIssues = append(res.ConversionIssues,
							"unsigned Responses reasoning cannot be represented as Anthropic thinking")
					}
				}
			}
			for _, c := range item.Summary {
				if c.Text != "" {
					res.Content = append(res.Content, domain.Block{Kind: domain.BlockThinking, Text: c.Text})
					recordResponseReasoningSummary(res, rc)
				}
			}
			if item.EncryptedContent != "" {
				res.ConversionIssues = append(res.ConversionIssues, "encrypted reasoning is native-only")
			}
		case "function_call":
			args := json.RawMessage(item.Arguments)
			res.Content = append(res.Content, domain.Block{
				Kind: domain.BlockToolUse, ID: item.CallID, Name: item.Name, Input: args,
			})
		default:
			res.Content = append(res.Content, domain.Block{Kind: domain.BlockOpaque, OpaqueType: item.Type,
				Opaque: append(json.RawMessage(nil), rawItem...)})
			res.ConversionIssues = append(res.ConversionIssues, "Responses output type "+item.Type+" is native-only")
		}
	}
	res.FinishReason = "stop"
	if out.Status == "incomplete" && (out.IncompleteDetails == nil || out.IncompleteDetails.Reason == "") {
		appendResponseValidationIssue(res, "incomplete Responses response is missing incomplete_details.reason")
	}
	if out.Status == "completed" && out.IncompleteDetails != nil && out.IncompleteDetails.Reason != "" {
		appendResponseValidationIssue(res, "completed Responses response contains incomplete details")
	}
	if out.IncompleteDetails != nil && out.IncompleteDetails.Reason != "" {
		if out.IncompleteDetails.Reason == "max_output_tokens" {
			res.FinishReason = "length"
		} else {
			res.ConversionIssues = append(res.ConversionIssues,
				"Responses incomplete reason "+out.IncompleteDetails.Reason+" is native-only")
		}
	}
	for _, b := range res.Content {
		if b.Kind == domain.BlockToolUse {
			res.FinishReason = "tool_call"
			break
		}
	}
	res.Usage = decodeUsage(out.Usage)
	markResponseConversionIssues(res)
	demoteResponsesAdvisories(res, rc.Origin)
	return res, nil
}

// ---------------------------------------------------------------------------
// streaming

func (r *OpenAIResponses) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	rc.Stream = true
	payload, _, err := r.buildRequest(rc)
	if err != nil {
		return nil, nil, &domain.Failure{Type: domain.FailBadResponse, Message: "build request: " + err.Error()}
	}
	resp, f := r.doSend(ctx, "/responses", payload,
		protocolHeaders(rc, domain.ProtocolOpenAIResponses, map[string]string{"Accept": "text/event-stream"}))
	if f != nil {
		return nil, nil, f
	}
	ch := make(chan domain.StreamEvent, 64)
	cancel := func() { resp.Body.Close() } // #nosec G104 -- best-effort close of a finished or abandoned upstream body
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		nativePass := rc.Origin == domain.ProtocolOpenAIResponses && len(rc.Native) > 0
		type responseStreamTool struct {
			index     int
			arguments string
		}
		activeTools := map[string]*responseStreamTool{}
		var itemIndexes streamItemIndexer
		contentByItem := map[string]string{}
		sawTool := false
		err := readSSE(resp.Body, func(ev sseEvent) error {
			var body responseStreamEnvelope
			if err := json.Unmarshal([]byte(ev.data), &body); err != nil {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
					Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
					Message: "Responses stream emitted invalid JSON"}}
				return ioEnd
			}
			terminal := body.Type == "response.completed" || body.Type == "response.done" ||
				body.Type == "response.incomplete" || body.Type == "response.failed" ||
				body.Type == "response.canceled" || body.Type == "response.cancelled" ||
				body.Type == "error" || body.Type == "response.error"
			if terminal {
				if failure := responseStreamTerminalValidationFailure(&body); failure != nil {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: failure}
					return ioEnd
				}
				if (body.Type == "response.completed" || body.Type == "response.done" ||
					body.Type == "response.incomplete") && len(activeTools) > 0 {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
						Message: "Responses terminal event arrived with unfinished tool items"}}
					return ioEnd
				}
			}
			if nativePass {
				ch <- nativeStreamEvent(domain.ProtocolOpenAIResponses, ev, terminal)
			}
			if body.Response != nil && (body.Response.Model != "" || body.Response.ServiceTier != "") {
				ch <- domain.StreamEvent{Kind: domain.EvStart, Model: body.Response.Model, ServiceTier: body.Response.ServiceTier}
			}
			switch body.Type {
			case "response.output_text.delta":
				key := responseStreamKey(body.ItemID, body.OutputIndex, "message")
				contentByItem[key] += body.Delta
				ch <- domain.StreamEvent{Kind: domain.EvTextDelta,
					Index: itemIndexes.index(body.ItemID, body.OutputIndex), TextDelta: body.Delta}
			case "response.refusal.delta":
				if !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses refusal cannot be losslessly converted"}}
					return ioEnd
				}
			case "response.reasoning_text.delta":
				if rc.Origin == domain.ProtocolAnthropic && !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "unsigned Responses reasoning cannot be represented as Anthropic thinking"}}
					return ioEnd
				}
				key := responseStreamKey(body.ItemID, body.OutputIndex, "reasoning")
				contentByItem[key] += body.Delta
				ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta,
					Index: itemIndexes.index(body.ItemID, body.OutputIndex), ThinkingDelta: body.Delta}
			case "response.reasoning_summary_text.delta":
				if !nativePass {
					if issue := responseReasoningSummaryConversionIssue(rc); issue != "" {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: issue}}
						return ioEnd
					}
					key := responseStreamKey(body.ItemID, body.OutputIndex, "reasoning_summary")
					contentByItem[key] += body.Delta
					ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta,
						Index: itemIndexes.index(body.ItemID, body.OutputIndex), ThinkingDelta: body.Delta}
				}
			case "response.function_call_arguments.delta":
				tool := activeTools[body.ItemID]
				if tool == nil {
					if nativePass {
						return nil
					}
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses tool arguments arrived before a complete tool item"}}
					return ioEnd
				}
				tool.arguments += body.Delta
				ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: tool.index, ArgsDelta: body.Delta}
			case "response.function_call_arguments.done":
				tool := activeTools[body.ItemID]
				if tool == nil {
					if nativePass {
						return nil
					}
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses tool arguments completed before their tool item"}}
					return ioEnd
				}
				if body.Arguments != "" && tool.arguments == "" {
					tool.arguments = body.Arguments
					ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: tool.index, ArgsDelta: body.Arguments}
				} else if body.Arguments != "" && tool.arguments != body.Arguments && !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses final tool arguments do not match their deltas"}}
					return ioEnd
				}
			case "response.output_item.added":
				if it := body.Item; it != nil {
					if it.Type == "function_call" {
						itemID := body.ItemID
						if itemID == "" {
							itemID = it.ID
						}
						if (itemID == "" || it.CallID == "" || it.Name == "") && !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Responses tool item is missing its item id, call id, or name"}}
							return ioEnd
						}
						index := itemIndexes.index(itemID, body.OutputIndex)
						activeTools[itemID] = &responseStreamTool{index: index, arguments: it.Arguments}
						sawTool = true
						ch <- domain.StreamEvent{Kind: domain.EvToolStart, Index: index,
							ToolID: it.CallID, ToolName: it.Name}
						if it.Arguments != "" {
							ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: index, ArgsDelta: it.Arguments}
						}
					} else if it.Type == "message" || it.Type == "reasoning" {
						if text, issues := responseStreamItemText(it); !nativePass && len(issues) > 0 {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: strings.Join(issues, "; ")}}
							return ioEnd
						} else if !nativePass && text != "" {
							key := responseStreamKey(firstNonEmpty(body.ItemID, it.ID), body.OutputIndex, it.Type)
							contentByItem[key] += text
							index := itemIndexes.index(firstNonEmpty(body.ItemID, it.ID), body.OutputIndex)
							if it.Type == "message" {
								ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: index, TextDelta: text}
							} else {
								ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta, Index: index, ThinkingDelta: text}
							}
						}
					} else if !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Responses output type " + it.Type + " cannot be losslessly converted"}}
						return ioEnd
					}
				}
			case "response.output_item.done":
				if it := body.Item; it != nil {
					if it.Status != "" && it.Status != "completed" {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
							Message: "Responses output item is not completed"}}
						return ioEnd
					}
					if it.Type == "function_call" {
						itemID := body.ItemID
						if itemID == "" {
							itemID = it.ID
						}
						tool := activeTools[itemID]
						if tool == nil {
							if nativePass {
								return nil
							}
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Responses tool item completed before it started"}}
							return ioEnd
						}
						if tool.arguments == "" && it.Arguments != "" {
							tool.arguments = it.Arguments
							ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: tool.index, ArgsDelta: it.Arguments}
						} else if it.Arguments != "" && tool.arguments != it.Arguments && !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Responses completed tool arguments do not match their deltas"}}
							return ioEnd
						}
						if tool.arguments == "" && !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Responses tool call is missing its arguments"}}
							return ioEnd
						}
						if tool.arguments != "" && !domain.ValidJSONObject(json.RawMessage(tool.arguments)) && !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Responses tool call arguments are not a JSON object"}}
							return ioEnd
						}
						ch <- domain.StreamEvent{Kind: domain.EvToolEnd, Index: tool.index}
						delete(activeTools, itemID)
					} else if it.Type == "message" || it.Type == "reasoning" {
						full, issues := responseStreamItemTextForOrigin(it, rc.Origin)
						if !nativePass && len(issues) > 0 {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: strings.Join(issues, "; ")}}
							return ioEnd
						}
						if !nativePass {
							itemID := firstNonEmpty(body.ItemID, it.ID)
							key := responseStreamKey(itemID, body.OutputIndex, it.Type)
							current := contentByItem[key]
							if current == "" && full != "" {
								contentByItem[key] = full
								index := itemIndexes.index(itemID, body.OutputIndex)
								if it.Type == "message" {
									ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: index, TextDelta: full}
								} else {
									if rc.Origin == domain.ProtocolAnthropic {
										ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
											Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
											Message: "unsigned Responses reasoning cannot be represented as Anthropic thinking"}}
										return ioEnd
									}
									ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta, Index: index, ThinkingDelta: full}
								}
							} else if full != "" && current != full {
								ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
									Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
									Message: "Responses completed item does not match its streamed deltas"}}
								return ioEnd
							}
							if summary := responseStreamItemSummaryText(it); summary != "" {
								if issue := responseReasoningSummaryConversionIssue(rc); issue != "" {
									ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
										Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
										Message: issue}}
									return ioEnd
								}
								summaryKey := responseStreamKey(itemID, body.OutputIndex, "reasoning_summary")
								currentSummary := contentByItem[summaryKey]
								if currentSummary == "" {
									contentByItem[summaryKey] = summary
									ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta,
										Index: itemIndexes.index(itemID, body.OutputIndex), ThinkingDelta: summary}
								} else if currentSummary != summary {
									ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
										Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
										Message: "Responses completed reasoning summary does not match its streamed deltas"}}
									return ioEnd
								}
							}
						}
					} else if !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Responses output type " + it.Type + " cannot be losslessly converted"}}
						return ioEnd
					}
				}
			case "response.output_text.done", "response.reasoning_text.done",
				"response.reasoning_summary_text.done":
				if !nativePass {
					kind := "message"
					if body.Type == "response.reasoning_text.done" {
						kind = "reasoning"
					} else if body.Type == "response.reasoning_summary_text.done" {
						if issue := responseReasoningSummaryConversionIssue(rc); issue != "" {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: issue}}
							return ioEnd
						}
						kind = "reasoning_summary"
					}
					key := responseStreamKey(body.ItemID, body.OutputIndex, kind)
					current := contentByItem[key]
					if current == "" && body.Text != "" {
						contentByItem[key] = body.Text
						index := itemIndexes.index(body.ItemID, body.OutputIndex)
						if kind == "message" {
							ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: index, TextDelta: body.Text}
						} else {
							if kind == "reasoning" && rc.Origin == domain.ProtocolAnthropic {
								ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
									Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
									Message: "unsigned Responses reasoning cannot be represented as Anthropic thinking"}}
								return ioEnd
							}
							ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta, Index: index, ThinkingDelta: body.Text}
						}
					} else if body.Text != "" && current != body.Text {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Responses done text does not match its streamed deltas"}}
						return ioEnd
					}
				}
			case "response.completed", "response.done":
				if body.Response == nil || body.Response.Status != "completed" {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses completed event is missing a completed response"}}
					return ioEnd
				}
				if len(activeTools) > 0 && !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses stream completed with unfinished tool items"}}
					return ioEnd
				}
				finish := "stop"
				if sawTool {
					finish = "tool_call"
				}
				sev := domain.StreamEvent{Kind: domain.EvEnd, FinishReason: finish}
				if body.Response != nil {
					sev.Model = body.Response.Model
					sev.ServiceTier = body.Response.ServiceTier
					sev.NativeResponseID = body.Response.ID
					sev.NativeConversationID = nativeConversationID(body.Response.Conversation)
					u := decodeUsage(body.Response.Usage)
					sev.Usage = &u
				}
				ch <- sev
				return ioEnd
			case "response.incomplete":
				if body.Response == nil || body.Response.Status != "incomplete" ||
					body.Response.IncompleteDetails == nil || body.Response.IncompleteDetails.Reason == "" {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
						Message: "Responses incomplete event is missing incomplete_details.reason"}}
					return ioEnd
				}
				if body.Response.IncompleteDetails.Reason != "max_output_tokens" && !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses incomplete reason " + body.Response.IncompleteDetails.Reason +
							" cannot be losslessly converted"}}
					return ioEnd
				}
				finish := "stop"
				if body.Response.IncompleteDetails.Reason == "max_output_tokens" {
					finish = "length"
				}
				sev := domain.StreamEvent{Kind: domain.EvEnd, FinishReason: finish}
				if body.Response != nil {
					sev.Model = body.Response.Model
					sev.ServiceTier = body.Response.ServiceTier
					sev.NativeResponseID = body.Response.ID
					sev.NativeConversationID = nativeConversationID(body.Response.Conversation)
					u := decodeUsage(body.Response.Usage)
					sev.Usage = &u
				}
				ch <- sev
				return ioEnd
			case "response.failed", "response.canceled", "response.cancelled", "response.error", "error":
				msg := "upstream response failed"
				typ := ""
				if body.Error != nil {
					msg = body.Error.Message
					typ = firstNonEmpty(body.Error.Code, body.Error.Type)
				} else if body.Message != "" {
					msg = body.Message
					typ = body.Code
				} else if body.Response != nil && body.Response.Error != nil {
					msg = body.Response.Error.Message
					typ = firstNonEmpty(body.Response.Error.Code, body.Response.Error.Type)
				}
				ft := domain.ClassifyHTTP(0, typ+" "+msg)
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: upstreamFailure(ft, 0, msg)}
				return ioEnd
			case "response.created", "response.in_progress", "response.queued",
				"response.content_part.added", "response.content_part.done",
				"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
				// Lifecycle/completion frames carry no additional canonical delta.
			default:
				if !nativePass {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses event type " + body.Type + " cannot be losslessly converted"}}
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

func nativeConversationID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var object struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.ID
	}
	return ""
}
