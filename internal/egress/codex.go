package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ferry/internal/domain"
)

// CodexCaller serves the ChatGPT/Codex subscription backend (Phase 4). The
// wire format is OpenAI Responses, but the backend demands an identity
// envelope (Originator/User-Agent/Chatgpt-Account-Id) and a stateless,
// stripped request shape, so it wraps the Responses adapter rather than
// reusing it bare.
type CodexCaller struct {
	OpenAIResponses

	accountID string // chatgpt_account_id claim; empty = omit header
}

// codexResponseStreamItemText separates client-visible reasoning from the
// encrypted state the gateway already owns for Codex tool-loop replay. Chat's
// reasoning_content extension can carry a Responses summary, while encrypted
// content remains internal and is reinserted on the next provider turn.
// Anthropic still fails closed because its replayable thinking blocks require
// provider signatures that an unsigned Responses item cannot supply.
func codexResponseStreamItemText(item *responseStreamItem, origin domain.Protocol) (string, []string) {
	return responseStreamItemTextForOrigin(item, origin)
}

// NewCodexCaller builds the Codex-flavored Responses adapter. accountID may
// be empty (header omitted) — it only feeds the backend's identity check.
func NewCodexCaller(b Base, accountID string) *CodexCaller {
	return &CodexCaller{OpenAIResponses: OpenAIResponses{Base: b}, accountID: accountID}
}

// codexStripFields are client/conversation-state fields the Codex backend
// rejects or ignores; the gateway owns statelessness, so they never leave.
var codexStripFields = []string{
	"previous_response_id", "prompt", "generate",
	"prompt_cache_retention", "safety_identifier", "stream_options",
	"prompt_cache_options", "max_completion_tokens", "top_p", "truncation",
	// The ChatGPT Codex backend rejects the public Responses API output-token
	// limit. Keep client-facing limits gateway-owned instead of turning every
	// otherwise valid Codex request into an upstream 400.
	"max_output_tokens",
	// The ChatGPT Codex backend rejects sampling temperature altogether.
	// Keep the canonical value for request/debug accounting, but remove it only
	// at this provider boundary instead of weakening the other Responses
	// adapters or waiting for a predictable upstream 400.
	"temperature",
}

// codexGatewayOwnedFields are deliberately normalized by Ferry rather than
// forwarded to the ChatGPT Codex backend. They remain part of the canonical
// request (for capacity planning and public response handling), so stripping
// them from the upstream wire shape is not a protocol-conversion loss.
var codexGatewayOwnedFields = map[string]struct{}{
	"max_output_tokens":     {},
	"temperature":           {},
	"top_p":                 {},
	"max_completion_tokens": {},
	"prompt_cache_options":  {},
}

// codexPassthroughTransform strips Codex-incompatible fields from a
// passthrough body and forces streaming (the backend is stream-first; the
// gateway reassembles non-streaming responses from the event flow).
func codexPassthroughTransform(m map[string]json.RawMessage) error {
	if err := normalizeCodexServiceTier(m); err != nil {
		return err
	}
	for _, k := range codexStripFields {
		delete(m, k)
	}
	// The public Responses API accepts a string shorthand for one user input,
	// while the ChatGPT Codex backend requires the canonical item list.
	if raw, ok := m["input"]; ok {
		var inputText string
		if json.Unmarshal(raw, &inputText) == nil {
			normalized, err := json.Marshal([]rsInputItem{{
				Type: "message", Role: "user",
				Content: []rsContentPart{{Type: "input_text", Text: inputText}},
			}})
			if err != nil {
				return fmt.Errorf("normalize Codex string input: %w", err)
			}
			m["input"] = normalized
		}
	}
	stripCodexCacheBreakpoints(m)
	m["stream"] = json.RawMessage("true")
	if raw, ok := m["instructions"]; !ok || string(raw) == "null" {
		m["instructions"] = json.RawMessage(`""`)
	}
	return nil
}

// buildRequest converts canonical → Codex wire format. Passthrough requests
// (responses ingress aimed at a codex target) replay the client body minus
// the stripped fields; everything else re-encodes canonically, then gets the
// same normalization.
func (c *CodexCaller) buildRequest(rc *domain.RequestContract) ([]byte, []string, error) {
	if rc == nil {
		return nil, nil, fmt.Errorf("nil request contract")
	}
	if _, err := domain.NormalizeServiceTier(rc.ServiceTier); err != nil {
		return nil, nil, err
	}
	// Validate native tiers before passthrough: its transform fallback must not
	// turn an unsupported paid tier into a canonical request without a tier.
	if rc.Origin == domain.ProtocolOpenAIResponses && len(rc.Native) > 0 {
		var native map[string]json.RawMessage
		if json.Unmarshal(rc.Native, &native) == nil {
			if err := normalizeCodexServiceTier(native); err != nil {
				return nil, nil, err
			}
		}
	}
	notes := []string(nil)
	if body := passthroughBody(rc, domain.ProtocolOpenAIResponses, c.Model,
		codexPassthroughTransform); body != nil {
		seen := map[string]bool{}
		var native map[string]json.RawMessage
		if json.Unmarshal(rc.Native, &native) == nil {
			for _, field := range codexStripFields {
				if _, gatewayOwned := codexGatewayOwnedFields[field]; gatewayOwned {
					continue
				}
				if raw := native[field]; len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
					note := "Codex OAuth cannot preserve Responses field " + field
					if !seen[note] {
						notes = append(notes, note)
						seen[note] = true
					}
				}
			}
		}
		// store stays gateway-owned even on exact passthrough
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err == nil {
			m["store"] = json.RawMessage("false")
			if out, err := json.Marshal(m); err == nil {
				return out, notes, nil
			}
		}
		return body, notes, nil
	}
	payload, notes, err := c.OpenAIResponses.buildRequest(rc)
	if err != nil {
		return nil, notes, err
	}
	if rc.Origin == domain.ProtocolOpenAIChat && rc.ProviderSessionKey != "" {
		// A Chat client may echo the visible reasoning summary from the prior
		// Codex tool-call turn. The provider needs its encrypted item instead;
		// applyCodexReasoningReplay inserts that gateway-owned state immediately
		// before the tool output, so omitting the echoed summary is lossless for
		// this stable conversation.
		filtered := notes[:0]
		for _, note := range notes {
			if note != "thinking block dropped (responses adapter keeps only encrypted/summary forms)" {
				filtered = append(filtered, note)
			}
		}
		notes = filtered
	}
	out, err := codexNormalize(payload)
	return out, notes, err
}

// Prepare overrides the embedded Responses preview because Codex applies a
// stricter payload normalization after ordinary Responses encoding. Without
// this method the scheduler would incorrectly label a same-wire request
// exact even when Codex strips a native Responses field.
func (c *CodexCaller) Prepare(rc *domain.RequestContract) ([]string, error) {
	prepared, err := c.PrepareDetailed(rc)
	return prepared.Notes, err
}

func (c *CodexCaller) PrepareDetailed(rc *domain.RequestContract) (Preparation, error) {
	if rc == nil {
		return Preparation{}, fmt.Errorf("nil request contract")
	}
	copy := *rc
	copy.Stream = true // Codex streams internally even for Invoke reassembly.
	payload, notes, err := c.buildRequest(&copy)
	return Preparation{Notes: notes, ToolTranscriptPreserved: err == nil &&
		nativeResponsesInputPreserved(&copy, payload)}, err
}

// codexNormalize applies the same shape rules to canonically-encoded bodies:
// stream forced on, client-only fields dropped, instructions defaulted.
func codexNormalize(payload []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("codex normalize: %w", err)
	}
	if err := codexPassthroughTransform(m); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// Invoke performs a non-streaming call: the Codex backend has no
// non-streaming mode, so the request is forced to stream (buildRequest sets
// stream=true) and the event flow reassembled into one response.
func (c *CodexCaller) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	rc.Stream = true
	payload, _, err := c.buildRequest(rc)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "build request: " + err.Error()}
	}
	sessionKey := codexSessionKey(rc, c.accountID)
	promptCacheKey := codexPromptCacheKey(rc, c.accountID, sessionKey)
	payload = applyCodexIdentityPayload(payload, c.accountID, sessionKey, promptCacheKey)
	payload = applyCodexReasoningReplay(payload, c.Model, sessionKey, time.Now())
	resp, f := c.doSend(ctx, "/responses", payload,
		codexRequestHeaders(rc, sessionKey))
	if f != nil {
		return nil, f
	}
	defer resp.Body.Close()
	var reasoning []json.RawMessage
	res := &domain.Response{NativeProtocol: domain.ProtocolOpenAIResponses}
	var sawTool bool
	toolIndexes := map[string]int{}
	toolDone := map[string]bool{}
	contentByItem := map[string]string{}
	done := false
	observedOutput := false
	err = readSSE(resp.Body, func(ev sseEvent) error {
		if failure := codexFailureFromEvent([]byte(c.Base.redact(ev.data))); failure != nil {
			return &codexReadFailure{failure: failure}
		}
		if item := collectReasoningItem([]byte(ev.data)); item != nil {
			reasoning = append(reasoning, item)
		}
		var body responseStreamEnvelope
		if json.Unmarshal([]byte(ev.data), &body) != nil {
			return fmt.Errorf("codex stream emitted invalid JSON")
		}
		// Invoke still reads SSE upstream. Stop the first-output watchdog as
		// soon as generation produces content, not when reassembly finishes.
		if !observedOutput && domain.MeaningfulSSEOutput(ev.name, []byte(ev.data)) {
			observedOutput = true
			domain.ObserveFirstOutput(ctx)
		}
		if body.Response != nil {
			if body.Response.Model != "" {
				res.Model = body.Response.Model
			}
			if body.Response.ServiceTier != "" {
				res.ServiceTier = body.Response.ServiceTier
			}
		}
		switch body.Type {
		case "response.output_text.delta":
			contentByItem[responseStreamKey(body.ItemID, body.OutputIndex, "message")] += body.Delta
			res.Content = append(res.Content, domain.Block{Kind: domain.BlockText, Text: body.Delta})
		case "response.refusal.delta":
			res.Content = append(res.Content, domain.Block{Kind: domain.BlockText, Text: body.Delta})
			appendResponseConversionIssue(res, "Responses refusal is native-only")
		case "response.reasoning_text.delta":
			contentByItem[responseStreamKey(body.ItemID, body.OutputIndex, "reasoning")] += body.Delta
			res.Content = append(res.Content, domain.Block{Kind: domain.BlockThinking, Text: body.Delta})
			if rc.Origin == domain.ProtocolAnthropic {
				appendResponseConversionIssue(res,
					"unsigned Responses reasoning cannot be represented as Anthropic thinking")
			}
		case "response.reasoning_summary_text.delta":
			contentByItem[responseStreamKey(body.ItemID, body.OutputIndex, "reasoning_summary")] += body.Delta
			res.Content = append(res.Content, domain.Block{Kind: domain.BlockThinking, Text: body.Delta})
			recordResponseReasoningSummary(res, rc)
		case "response.output_item.added":
			if it := body.Item; it != nil {
				if it.Type == "function_call" {
					sawTool = true
					itemID := body.ItemID
					if itemID == "" {
						itemID = it.ID
					}
					if itemID == "" {
						appendResponseConversionIssue(res, "Responses tool item is missing its item id")
					}
					args := json.RawMessage(it.Arguments)
					toolIndexes[itemID] = len(res.Content)
					toolDone[itemID] = false
					res.Content = append(res.Content, domain.Block{
						Kind: domain.BlockToolUse, ID: it.CallID, Name: it.Name, Input: args})
				} else if it.Type == "message" || it.Type == "reasoning" {
					text, issues := codexResponseStreamItemText(it, rc.Origin)
					for _, issue := range issues {
						appendResponseConversionIssue(res, issue)
					}
					if text != "" {
						itemID := firstNonEmpty(body.ItemID, it.ID)
						contentByItem[responseStreamKey(itemID, body.OutputIndex, it.Type)] += text
						kind := domain.BlockText
						if it.Type == "reasoning" {
							kind = domain.BlockThinking
							if rc.Origin == domain.ProtocolAnthropic {
								appendResponseConversionIssue(res,
									"unsigned Responses reasoning cannot be represented as Anthropic thinking")
							}
						}
						res.Content = append(res.Content, domain.Block{Kind: kind, Text: text})
					}
				} else {
					appendResponseConversionIssue(res,
						"Responses output type "+it.Type+" is native-only")
				}
			}
		case "response.function_call_arguments.delta":
			index, ok := toolIndexes[body.ItemID]
			if !ok || index < 0 || index >= len(res.Content) {
				appendResponseConversionIssue(res, "Responses tool arguments arrived before their tool item")
				break
			}
			res.Content[index].Input = json.RawMessage(string(res.Content[index].Input) + body.Delta)
		case "response.function_call_arguments.done":
			index, ok := toolIndexes[body.ItemID]
			if !ok || index < 0 || index >= len(res.Content) {
				appendResponseConversionIssue(res, "Responses tool arguments completed before their tool item")
				break
			}
			current := string(res.Content[index].Input)
			if current == "" && body.Arguments != "" {
				res.Content[index].Input = json.RawMessage(body.Arguments)
			} else if body.Arguments != "" && current != body.Arguments {
				appendResponseConversionIssue(res, "Responses final tool arguments do not match their deltas")
			}
		case "response.output_item.done":
			if it := body.Item; it != nil && it.Type == "function_call" {
				if it.Status != "" && it.Status != "completed" {
					appendResponseValidationIssue(res, "Responses output item is not completed")
				}
				itemID := body.ItemID
				if itemID == "" {
					itemID = it.ID
				}
				index, ok := toolIndexes[itemID]
				if !ok || index < 0 || index >= len(res.Content) {
					appendResponseConversionIssue(res, "Responses tool item completed before it started")
					break
				}
				current := string(res.Content[index].Input)
				if current == "" && it.Arguments != "" {
					res.Content[index].Input = json.RawMessage(it.Arguments)
				} else if it.Arguments != "" && current != it.Arguments {
					appendResponseConversionIssue(res, "Responses completed tool arguments do not match their deltas")
				}
				toolDone[itemID] = true
			} else if it := body.Item; it != nil && (it.Type == "message" || it.Type == "reasoning") {
				if it.Status != "" && it.Status != "completed" {
					appendResponseValidationIssue(res, "Responses output item is not completed")
				}
				full, issues := codexResponseStreamItemText(it, rc.Origin)
				for _, issue := range issues {
					appendResponseConversionIssue(res, issue)
				}
				itemID := firstNonEmpty(body.ItemID, it.ID)
				key := responseStreamKey(itemID, body.OutputIndex, it.Type)
				current := contentByItem[key]
				if current == "" && full != "" {
					contentByItem[key] = full
					kind := domain.BlockText
					if it.Type == "reasoning" {
						kind = domain.BlockThinking
						if rc.Origin == domain.ProtocolAnthropic {
							appendResponseConversionIssue(res,
								"unsigned Responses reasoning cannot be represented as Anthropic thinking")
						}
					}
					res.Content = append(res.Content, domain.Block{Kind: kind, Text: full})
				} else if full != "" && current != full {
					appendResponseConversionIssue(res,
						"Responses completed item does not match its streamed deltas")
				}
				if summary := responseStreamItemSummaryText(it); summary != "" {
					recordResponseReasoningSummary(res, rc)
					summaryKey := responseStreamKey(itemID, body.OutputIndex, "reasoning_summary")
					currentSummary := contentByItem[summaryKey]
					if currentSummary == "" {
						contentByItem[summaryKey] = summary
						res.Content = append(res.Content, domain.Block{Kind: domain.BlockThinking, Text: summary})
					} else if currentSummary != summary {
						appendResponseConversionIssue(res,
							"Responses completed reasoning summary does not match its streamed deltas")
					}
				}
			}
		case "response.output_text.done", "response.reasoning_text.done",
			"response.reasoning_summary_text.done":
			kindName := "message"
			blockKind := domain.BlockText
			if body.Type == "response.reasoning_text.done" {
				kindName = "reasoning"
				blockKind = domain.BlockThinking
			} else if body.Type == "response.reasoning_summary_text.done" {
				kindName = "reasoning_summary"
				blockKind = domain.BlockThinking
				recordResponseReasoningSummary(res, rc)
			}
			key := responseStreamKey(body.ItemID, body.OutputIndex, kindName)
			current := contentByItem[key]
			if current == "" && body.Text != "" {
				contentByItem[key] = body.Text
				res.Content = append(res.Content, domain.Block{Kind: blockKind, Text: body.Text})
				if kindName == "reasoning" && rc.Origin == domain.ProtocolAnthropic {
					appendResponseConversionIssue(res,
						"unsigned Responses reasoning cannot be represented as Anthropic thinking")
				}
			} else if body.Text != "" && current != body.Text {
				appendResponseConversionIssue(res, "Responses done text does not match its streamed deltas")
			}
		case "response.completed", "response.done":
			if body.Response == nil || body.Response.Status != "completed" {
				appendResponseValidationIssue(res, "Responses completed event is missing a completed response")
			} else if body.Response.IncompleteDetails != nil && body.Response.IncompleteDetails.Reason != "" {
				appendResponseValidationIssue(res, "Responses completed event contains incomplete details")
			} else {
				res.NativeResponseID = body.Response.ID
				u := decodeUsage(body.Response.Usage)
				res.Usage = u
			}
			done = true
			return ioEnd
		case "response.incomplete":
			res.FinishReason = "length"
			if body.Response == nil || body.Response.Status != "incomplete" {
				appendResponseValidationIssue(res, "Responses incomplete event is missing an incomplete response")
			} else if body.Response.IncompleteDetails == nil || body.Response.IncompleteDetails.Reason == "" {
				appendResponseValidationIssue(res, "Responses incomplete event is missing incomplete_details.reason")
			} else {
				res.NativeResponseID = body.Response.ID
				u := decodeUsage(body.Response.Usage)
				res.Usage = u
				if body.Response.IncompleteDetails.Reason != "max_output_tokens" {
					appendResponseConversionIssue(res,
						"Responses incomplete reason "+body.Response.IncompleteDetails.Reason+" is native-only")
				}
			}
			done = true
			return ioEnd
		case "response.created", "response.in_progress", "response.queued",
			"response.content_part.added", "response.content_part.done",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
			// Lifecycle/completion frames carry no extra canonical delta.
		case "response.metadata":
			if codexDiagnosticMetadata([]byte(ev.data)) {
				appendResponseWarning(res, "Codex moderation diagnostics omitted from non-streaming response")
			} else {
				appendResponseConversionIssue(res, "Responses metadata event contains unrecognized native-only data")
			}
		default:
			appendResponseConversionIssue(res, "Responses event type "+body.Type+" is native-only")
		}
		return nil
	})
	if err != nil && err != ioEnd {
		var terminal *codexReadFailure
		if errors.As(err, &terminal) {
			return nil, terminal.failure
		}
		ft := domain.ClassifyHTTP(0, err.Error())
		return nil, &domain.Failure{Type: ft, Message: truncate(err.Error(), 512)}
	}
	if !done {
		return nil, &domain.Failure{Type: domain.FailBadResponse,
			Message: "codex: event stream ended without a terminal response event"}
	}
	for itemID, complete := range toolDone {
		if !complete {
			appendResponseConversionIssue(res, "Responses tool item "+itemID+" did not complete")
		}
	}
	res.FinishReason = orDefaultStr(res.FinishReason, finishOf(sawTool))
	markResponseConversionIssues(res)
	rememberCodexReasoning(c.Model, sessionKey, reasoning, time.Now())
	return res, nil
}

type codexReadFailure struct{ failure *domain.Failure }

func (e *codexReadFailure) Error() string {
	if e == nil || e.failure == nil {
		return "codex upstream failed"
	}
	return e.failure.Error()
}

// Stream uses the Codex-specific request shape, identity envelope and
// terminal error decoder. The embedded OpenAIResponses implementation cannot
// be reused here because its statically-bound sender would omit Codex OAuth
// identity headers.
func (c *CodexCaller) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	rc.Stream = true
	payload, _, err := c.buildRequest(rc)
	if err != nil {
		return nil, nil, &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeRequest,
			Message: "build request: " + err.Error()}
	}
	sessionKey := codexSessionKey(rc, c.accountID)
	promptCacheKey := codexPromptCacheKey(rc, c.accountID, sessionKey)
	payload = applyCodexIdentityPayload(payload, c.accountID, sessionKey, promptCacheKey)
	payload = applyCodexReasoningReplay(payload, c.Model, sessionKey, time.Now())
	resp, failure := c.doSend(ctx, "/responses", payload,
		codexRequestHeaders(rc, sessionKey))
	if failure != nil {
		return nil, nil, failure
	}
	ch := make(chan domain.StreamEvent, 64)
	cancel := func() { _ = resp.Body.Close() }
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		nativePass := rc.Origin == domain.ProtocolOpenAIResponses && len(rc.Native) > 0
		var reasoning []json.RawMessage
		type codexStreamTool struct {
			index     int
			arguments string
		}
		activeTools := map[string]*codexStreamTool{}
		var itemIndexes streamItemIndexer
		contentByItem := map[string]string{}
		sawTool := false
		readErr := readSSE(resp.Body, func(ev sseEvent) error {
			if terminal := codexFailureFromEvent([]byte(c.Base.redact(ev.data))); terminal != nil {
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: terminal}
				return ioEnd
			}
			if item := collectReasoningItem([]byte(ev.data)); item != nil {
				reasoning = append(reasoning, item)
			}
			var body responseStreamEnvelope
			if err := json.Unmarshal([]byte(ev.data), &body); err != nil {
				if nativePass {
					ch <- nativeStreamEvent(domain.ProtocolOpenAIResponses, ev, false)
					return nil
				}
				ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
					Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
					Message: "Codex stream emitted invalid JSON"}}
				return ioEnd
			}
			terminal := body.Type == "response.completed" || body.Type == "response.done" ||
				body.Type == "response.incomplete" || body.Type == "response.failed" ||
				body.Type == "response.canceled" || body.Type == "response.cancelled" ||
				body.Type == "response.error" || body.Type == "error"
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
				contentByItem[responseStreamKey(body.ItemID, body.OutputIndex, "message")] += body.Delta
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
				contentByItem[responseStreamKey(body.ItemID, body.OutputIndex, "reasoning")] += body.Delta
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
					contentByItem[responseStreamKey(body.ItemID, body.OutputIndex, "reasoning_summary")] += body.Delta
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
				if item := body.Item; item != nil {
					if item.Type == "function_call" {
						itemID := body.ItemID
						if itemID == "" {
							itemID = item.ID
						}
						if (itemID == "" || item.CallID == "" || item.Name == "") && !nativePass {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: "Responses tool item is missing its item id, call id, or name"}}
							return ioEnd
						}
						index := itemIndexes.index(itemID, body.OutputIndex)
						activeTools[itemID] = &codexStreamTool{index: index, arguments: item.Arguments}
						sawTool = true
						ch <- domain.StreamEvent{Kind: domain.EvToolStart, Index: index,
							ToolID: item.CallID, ToolName: item.Name}
						if item.Arguments != "" {
							ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: index, ArgsDelta: item.Arguments}
						}
					} else if item.Type == "message" || item.Type == "reasoning" {
						text, issues := codexResponseStreamItemText(item, rc.Origin)
						if !nativePass && len(issues) > 0 {
							ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
								Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
								Message: strings.Join(issues, "; ")}}
							return ioEnd
						}
						if !nativePass && text != "" {
							itemID := firstNonEmpty(body.ItemID, item.ID)
							contentByItem[responseStreamKey(itemID, body.OutputIndex, item.Type)] += text
							index := itemIndexes.index(itemID, body.OutputIndex)
							if item.Type == "message" {
								ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: index, TextDelta: text}
							} else {
								if rc.Origin == domain.ProtocolAnthropic {
									ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
										Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
										Message: "unsigned Responses reasoning cannot be represented as Anthropic thinking"}}
									return ioEnd
								}
								ch <- domain.StreamEvent{Kind: domain.EvThinkingDelta, Index: index, ThinkingDelta: text}
							}
						}
					} else if !nativePass {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "Responses output type " + item.Type + " cannot be losslessly converted"}}
						return ioEnd
					}
				}
			case "response.output_item.done":
				if item := body.Item; item != nil && item.Type == "function_call" {
					if item.Status != "" && item.Status != "completed" {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
							Message: "Responses output item is not completed"}}
						return ioEnd
					}
					itemID := body.ItemID
					if itemID == "" {
						itemID = item.ID
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
					if tool.arguments == "" && item.Arguments != "" {
						tool.arguments = item.Arguments
						ch <- domain.StreamEvent{Kind: domain.EvToolDelta, Index: tool.index, ArgsDelta: item.Arguments}
					} else if item.Arguments != "" && tool.arguments != item.Arguments && !nativePass {
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
				} else if item := body.Item; item != nil && (item.Type == "message" || item.Type == "reasoning") {
					if item.Status != "" && item.Status != "completed" {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
							Message: "Responses output item is not completed"}}
						return ioEnd
					}
					full, issues := codexResponseStreamItemText(item, rc.Origin)
					if !nativePass && len(issues) > 0 {
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: strings.Join(issues, "; ")}}
						return ioEnd
					}
					if !nativePass {
						itemID := firstNonEmpty(body.ItemID, item.ID)
						key := responseStreamKey(itemID, body.OutputIndex, item.Type)
						current := contentByItem[key]
						if current == "" && full != "" {
							contentByItem[key] = full
							index := itemIndexes.index(itemID, body.OutputIndex)
							if item.Type == "message" {
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
						if summary := responseStreamItemSummaryText(item); summary != "" {
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
				end := domain.StreamEvent{Kind: domain.EvEnd, FinishReason: finish}
				if body.Response != nil {
					end.Model = body.Response.Model
					end.ServiceTier = body.Response.ServiceTier
					end.NativeResponseID = body.Response.ID
					end.NativeConversationID = nativeConversationID(body.Response.Conversation)
					usage := decodeUsage(body.Response.Usage)
					end.Usage = &usage
				}
				ch <- end
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
				end := domain.StreamEvent{Kind: domain.EvEnd, FinishReason: finish}
				if body.Response != nil {
					end.Model = body.Response.Model
					end.ServiceTier = body.Response.ServiceTier
					end.NativeResponseID = body.Response.ID
					end.NativeConversationID = nativeConversationID(body.Response.Conversation)
					usage := decodeUsage(body.Response.Usage)
					end.Usage = &usage
				}
				ch <- end
				return ioEnd
			case "response.created", "response.in_progress", "response.queued",
				"response.content_part.added", "response.content_part.done",
				"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
				// Lifecycle/completion frames carry no extra canonical delta.
			case "response.metadata":
				// Exact Responses clients already received the original frame.
				// Other protocols may omit only the known diagnostic envelope.
				if !nativePass && !codexDiagnosticMetadata([]byte(ev.data)) {
					ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
						Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "Responses metadata event contains unrecognized native-only data"}}
					return ioEnd
				}
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
		if readErr != nil && readErr != ioEnd {
			ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
				Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream, Message: readErr.Error()}}
			return
		}
		// Only a stream that ran to its terminal event describes a turn the
		// next request can build on.
		rememberCodexReasoning(c.Model, sessionKey, reasoning, time.Now())
	}()
	return ch, cancel, nil
}

// orDefaultStr returns fallback when s is empty.
func orDefaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// finishOf maps presence of tool calls to the canonical finish reason.
func finishOf(sawTool bool) string {
	if sawTool {
		return "tool_call"
	}
	return "stop"
}

const (
	codexOriginator = domain.CodexOriginator
	codexUserAgent  = domain.CodexUserAgent
)

var (
	_ Caller   = (*CodexCaller)(nil)
	_ Preparer = (*CodexCaller)(nil)
)

// headers returns the identity envelope merged over the shared Base extras.
func (c *CodexCaller) headers() map[string]string {
	return codexProviderHeaders(c.ExtraHeaders, c.accountID, c.CodexClientVersion)
}

func codexProviderHeaders(base map[string]string, accountID string, versions ...string) map[string]string {
	h := make(map[string]string, len(base)+4)
	for k, v := range base {
		h[k] = v
	}
	h["Originator"] = codexOriginator
	version := ""
	if len(versions) > 0 {
		version = versions[0]
	}
	h["User-Agent"] = domain.CodexUserAgentForVersion(version)
	h["Version"] = domain.EffectiveCodexClientVersion(version)
	h["Connection"] = "Keep-Alive"
	if accountID != "" {
		h["Chatgpt-Account-Id"] = accountID
	}
	return h
}

// doSend overrides Base.doSend to attach the Codex identity envelope.
func (c *CodexCaller) doSend(ctx context.Context, path string, payload []byte,
	extra map[string]string) (*http.Response, *domain.Failure) {
	if len(extra) > 0 {
		h := c.headers()
		for k, v := range extra {
			h[k] = v // caller extras on top of the envelope
		}
		extra = h
	} else {
		extra = c.headers()
	}
	return c.Base.doSendClassified(ctx, path, payload, extra, classifyCodexHTTPFailure)
}
