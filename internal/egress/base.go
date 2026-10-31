// Package egress implements upstream provider adapters. Each adapter builds
// a wire request from the canonical contract, parses SSE streams into
// canonical events, and classifies failures into typed failures
// (F-P0-16, PRD §6.8).
package egress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ferry/internal/domain"
)

// Caller executes one upstream attempt and reports canonical results.
type Caller interface {
	// Invoke performs a non-streaming call.
	Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure)
	// Stream performs a streaming call; the returned channel yields canonical
	// events until EvEnd/EvError or ctx cancellation.
	Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure)
}

// Base bundles what every adapter needs.
type Base struct {
	CodexClientVersion string // pinned to the compiled configuration revision
	HTTP               *http.Client
	Endpoint           *domain.Endpoint
	Secret             string
	CredRef            string // for ledger display only
	// Credential controls non-default credential placement. With neither
	// Header nor QueryParam set, adapters retain the historical compatible
	// defaults (Authorization: Bearer plus x-api-key).
	Credential domain.CredentialRef
	Model      string
	// ExtraHeaders are set on every request this target sends (after the
	// protocol defaults, so they can override them) — e.g. the Codex
	// backend's mandatory Originator/Chatgpt-Account-Id identity headers.
	ExtraHeaders map[string]string
	// BeforeSend checks account usage after admission, immediately before inference.
	BeforeSend func(context.Context) *domain.Failure
	// AfterResponse observes quota headers before body parsing, including error responses.
	AfterResponse func(http.Header)
}

func newRequest(ctx context.Context, method, url string, body []byte, headers map[string]string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// passthroughBody replays the preserved raw client body verbatim when this
// attempt is same-protocol (Phase 3 exact fidelity): the client's wire format
// reaches upstream untouched instead of being round-tripped through the
// canonical contract. The gateway patches the upstream model name and stream
// flag, and normalizes service_tier for OpenAI requests. transform, when non-nil,
// rewrites the decoded body afterwards (Codex strips client-only fields);
// a transform error falls back to canonical re-encoding.
func passthroughBody(rc *domain.RequestContract, origin domain.Protocol, upstreamModel string,
	transform func(map[string]json.RawMessage) error) []byte {
	if rc == nil || rc.Origin != origin || len(rc.Native) == 0 || upstreamModel == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rc.Native, &m); err != nil {
		return nil // malformed capture; re-encode from canonical state
	}
	if _, ok := m["model"]; !ok {
		return nil // not a model-bearing body; re-encode
	}
	model, _ := json.Marshal(upstreamModel)
	stream, _ := json.Marshal(rc.Stream)
	m["model"] = model
	m["stream"] = stream
	if rc.ServiceTier != "" {
		tier, err := domain.NormalizeServiceTier(rc.ServiceTier)
		if err != nil {
			return nil
		}
		if tier == "" {
			delete(m, "service_tier")
		} else {
			m["service_tier"], _ = json.Marshal(tier)
		}
	} else if origin == domain.ProtocolOpenAIChat || origin == domain.ProtocolOpenAIResponses {
		// Ingress normalizes a blank tier to empty but preserves the raw body.
		var value string
		if raw, ok := m["service_tier"]; ok && json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) == "" {
			delete(m, "service_tier")
		}
	}
	if transform != nil {
		if err := transform(m); err != nil {
			return nil
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// protocolHeaders forwards only feature-negotiation headers captured by the
// matching ingress protocol. Authentication and tenant identity always come
// from the configured provider credential instead of the client request.
func protocolHeaders(rc *domain.RequestContract, protocol domain.Protocol, defaults map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range defaults {
		out[key] = value
	}
	if rc == nil || rc.Origin != protocol {
		return out
	}
	for key, value := range rc.ProtocolHeaders {
		switch strings.ToLower(key) {
		case "anthropic-version", "anthropic-beta", "openai-beta":
			out[key] = value
		}
	}
	return out
}

func rejectLossyConversion(rc *domain.RequestContract, protocol domain.Protocol) error {
	if rc != nil && rc.ServiceTier != "" && protocol != domain.ProtocolOpenAIChat && protocol != domain.ProtocolOpenAIResponses {
		return fmt.Errorf("OpenAI service_tier cannot be represented by %s", protocol)
	}
	if rc == nil || len(rc.ConversionIssues) == 0 {
		return nil
	}
	// A valid same-protocol native body is handled before this guard by each
	// adapter's passthroughBody branch.
	return fmt.Errorf("request requires native %s passthrough: %s",
		rc.Origin, strings.Join(rc.ConversionIssues, "; "))
}

func canonicalRequestNotes(rc *domain.RequestContract, protocol domain.Protocol) []string {
	if rc == nil {
		return nil
	}
	var notes []string
	seen := map[string]bool{}
	add := func(note string) {
		if note != "" && !seen[note] {
			notes = append(notes, note)
			seen[note] = true
		}
	}
	if rc.PersistNativeState && protocol != domain.ProtocolOpenAIResponses {
		add("Responses store semantics cannot be represented by " + string(protocol))
	}
	systemMessages := 0
	for _, tool := range rc.Tools {
		if tool.Name == "" {
			add("tool definition without a name cannot be converted")
		}
		if !domain.ValidJSONObject(tool.Parameters) {
			add("tool parameters must be a JSON object")
		}
	}
	for _, message := range rc.Messages {
		if message.Role == domain.RoleSystem {
			systemMessages++
		}
		if message.Role != domain.RoleSystem && message.Role != domain.RoleUser &&
			message.Role != domain.RoleAssistant && message.Role != domain.RoleTool {
			add("message role " + string(message.Role) + " cannot be represented by " + string(protocol))
		}
		// A tool result is correlated by ToolID in all three protocols. Some
		// OpenAI-compatible clients also echo its function name; that duplicate
		// label carries no additional request semantics and is safe to omit.
		if message.Name != "" && protocol != domain.ProtocolOpenAIChat &&
			!(message.Role == domain.RoleTool && message.ToolID != "") {
			add("message name cannot be represented by " + string(protocol))
		}
		if message.Role == domain.RoleTool && message.ToolID == "" {
			add("tool result is missing its tool call id")
		}
		for _, block := range message.Blocks {
			if message.Role == domain.RoleSystem && block.Kind != domain.BlockText {
				add("non-text system content cannot be represented by " + string(protocol))
			}
			if message.Role == domain.RoleTool && block.Kind != domain.BlockText &&
				block.Kind != domain.BlockToolResult {
				add("non-text tool-role content cannot be represented by " + string(protocol))
			}
			switch block.Kind {
			case domain.BlockText, domain.BlockImage:
			case domain.BlockThinking:
				if block.Signature != "" && protocol != domain.ProtocolAnthropic {
					add("thinking signature cannot be represented by " + string(protocol))
				}
				if block.Signature == "" && protocol == domain.ProtocolAnthropic && rc.Origin != domain.ProtocolAnthropic {
					add("unsigned thinking history cannot be sent to anthropic")
				}
			case domain.BlockRedactedThinking:
				if protocol != domain.ProtocolAnthropic {
					add("redacted thinking cannot be represented by " + string(protocol))
				}
			case domain.BlockToolUse:
				if block.ID == "" || block.Name == "" {
					add("tool call without an id and name cannot be converted")
				}
				if !domain.ValidJSONObject(block.Input) {
					add("tool call arguments must be a JSON object")
				}
			case domain.BlockToolResult:
				if block.ID == "" && message.ToolID == "" {
					add("tool result is missing its tool call id")
				}
				if block.IsError && protocol != domain.ProtocolAnthropic {
					add("tool result error state cannot be represented by " + string(protocol))
				}
				if protocol != domain.ProtocolAnthropic && !simpleToolResult(block.Result) {
					add("structured tool result cannot be represented by " + string(protocol))
				}
			case domain.BlockOpaque:
				kind := block.OpaqueType
				if kind == "" {
					kind = "unknown"
				}
				add("content type " + kind + " cannot be represented by " + string(protocol))
			default:
				add("unknown canonical content cannot be represented by " + string(protocol))
			}
		}
	}
	for _, message := range rc.Messages {
		if message.Role != domain.RoleTool {
			continue
		}
		hasResult, hasOther := false, false
		for _, block := range message.Blocks {
			if block.Kind == domain.BlockToolResult {
				hasResult = true
			} else {
				hasOther = true
			}
		}
		if hasResult && hasOther {
			add("tool result message mixes explicit result blocks with other content")
		}
	}
	if systemMessages > 1 && protocol != domain.ProtocolOpenAIChat {
		add("multiple system messages would be merged by " + string(protocol))
	}
	return notes
}

func simpleToolResult(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	if len(blocks) != 1 {
		return false
	}
	for _, block := range blocks {
		if block.Type != "text" {
			return false
		}
	}
	return true
}

// reasoningEffortForBudget maps the Anthropic token-budget control to the
// shared low/medium/high reasoning control understood by the OpenAI-shaped
// protocols. This is a request-control mapping, not a promise about the
// provider-native reasoning state returned later; actual response fidelity is
// checked separately before commit.
func reasoningEffortForBudget(budget int64) string {
	switch {
	case budget <= 0:
		return ""
	case budget <= 2048:
		return "low"
	case budget >= 16384:
		return "high"
	default:
		return "medium"
	}
}

// reasoningBudgetForEffort is the inverse request-control mapping used by
// Anthropic Messages, whose thinking API requires an explicit token budget.
func reasoningBudgetForEffort(effort string) int64 {
	switch effort {
	case "low":
		return 2048
	case "high":
		return 16384
	default:
		return 8192
	}
}

func appendResponseConversionIssue(response *domain.Response, issue string) {
	if response == nil || issue == "" {
		return
	}
	for _, existing := range response.ConversionIssues {
		if existing == issue {
			return
		}
	}
	response.ConversionIssues = append(response.ConversionIssues, issue)
}

func appendResponseWarning(response *domain.Response, warning string) {
	if response == nil || warning == "" {
		return
	}
	for _, existing := range response.Warnings {
		if existing == warning {
			return
		}
	}
	response.Warnings = append(response.Warnings, warning)
}

func appendResponseValidationIssue(response *domain.Response, issue string) {
	if response == nil || issue == "" {
		return
	}
	for _, existing := range response.ValidationIssues {
		if existing == issue {
			return
		}
	}
	response.ValidationIssues = append(response.ValidationIssues, issue)
}

func unknownJSONFieldIssues(raw []byte, path string, known ...string) []string {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return unknownObjectFieldIssues(object, path, known...)
}

// Reuse already decoded objects instead of parsing their potentially large payload again.
func unknownObjectFieldIssues[T any](object map[string]T, path string, known ...string) []string {
	allowed := make(map[string]bool, len(known))
	for _, field := range known {
		allowed[field] = true
	}
	var issues []string
	for field := range object {
		if allowed[field] {
			continue
		}
		qualified := field
		if path != "" {
			qualified = path + "." + field
		}
		issues = append(issues, "response field "+qualified+" is native-only")
	}
	sort.Strings(issues)
	return issues
}

// markResponseConversionIssues validates the canonical response contract
// shared by all text adapters. Same-protocol callers still receive the raw
// native body; cross-protocol callers fail before commit instead of emitting
// malformed tools or silently dropping native-only blocks.
func markResponseConversionIssues(response *domain.Response) {
	if response == nil {
		return
	}
	sawTool := false
	switch response.FinishReason {
	case "stop", "tool_call", "length":
	case "":
		appendResponseValidationIssue(response, "response is missing its finish reason")
	default:
		appendResponseConversionIssue(response, "response finish reason "+response.FinishReason+" is native-only")
	}
	for _, block := range response.Content {
		switch block.Kind {
		case domain.BlockText:
		case domain.BlockThinking:
			if block.Signature != "" {
				appendResponseConversionIssue(response, "thinking signature is native-only")
			}
		case domain.BlockToolUse:
			sawTool = true
			if block.ID == "" {
				appendResponseValidationIssue(response, "tool call is missing its id")
			}
			if block.Name == "" {
				appendResponseValidationIssue(response, "tool call is missing its name")
			}
			if len(block.Input) == 0 {
				appendResponseValidationIssue(response, "tool call is missing its arguments")
			} else if !domain.ValidJSONObject(block.Input) {
				appendResponseValidationIssue(response, "tool call arguments are not a JSON object")
			}
		case domain.BlockRedactedThinking:
			appendResponseConversionIssue(response, "redacted thinking is native-only")
		case domain.BlockImage:
			appendResponseConversionIssue(response, "image response block is native-only")
		case domain.BlockToolResult:
			appendResponseConversionIssue(response, "tool result response block is native-only")
		case domain.BlockOpaque:
			kind := block.OpaqueType
			if kind == "" {
				kind = "unknown"
			}
			appendResponseConversionIssue(response, "content type "+kind+" is native-only")
		default:
			appendResponseConversionIssue(response, "unknown canonical response block is native-only")
		}
	}
	if sawTool && response.FinishReason != "tool_call" {
		appendResponseValidationIssue(response, "tool calls are inconsistent with the response finish reason")
	}
	if !sawTool && response.FinishReason == "tool_call" {
		appendResponseValidationIssue(response, "tool-call finish reason has no matching tool call")
	}
}

func nativeStreamEvent(protocol domain.Protocol, ev sseEvent, terminal bool) domain.StreamEvent {
	return domain.StreamEvent{
		Kind: domain.EvNative, NativeProtocol: protocol,
		NativeFrame: &domain.NativeSSEFrame{Event: ev.name, Data: ev.data, Terminal: terminal,
			Meaningful: !terminal && nativeOnlyOutput(protocol, ev),
			KeepAlive:  !terminal && nativeKeepAlive(protocol, ev)},
	}
}

// streamItemIndexer assigns collision-free canonical indexes to opaque
// provider item IDs. Responses IDs are not numeric ordinals (and often have
// no digits at all), so deriving an index from their spelling can merge two
// parallel tool calls into one canonical block.
type streamItemIndexer struct {
	indexes map[string]int
	next    int
}

func (s *streamItemIndexer) index(itemID string, outputIndex *int) int {
	if s.indexes == nil {
		s.indexes = map[string]int{}
	}
	key := itemID
	if key == "" && outputIndex != nil {
		key = fmt.Sprintf("output:%d", *outputIndex)
	}
	if key != "" {
		if index, ok := s.indexes[key]; ok {
			return index
		}
	}
	index := s.next
	s.next++
	if key != "" {
		s.indexes[key] = index
	}
	return index
}

func toolResultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var builder strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				builder.WriteString(block.Text)
			}
		}
		return builder.String()
	}
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

// doSend executes the request and returns the response or a typed failure.
func (b *Base) doSend(ctx context.Context, path string, payload []byte, extra map[string]string) (*http.Response, *domain.Failure) {
	return b.doSendClassified(ctx, path, payload, extra, nil)
}

type responseFailureClassifier func(status int, body string, headers http.Header) *domain.Failure

func upstreamFailure(failureType domain.FailureType, status int, message string) *domain.Failure {
	failure := &domain.Failure{Type: failureType, HTTPStatus: status, Message: message}
	if failureType == domain.FailUnsupportedParam {
		failure.Scope = domain.FailureScopeTarget
	}
	return failure
}

// doSendClassified lets provider adapters preserve provider-specific error
// semantics while reusing the common credential placement, redaction,
// transport, response limiting, and Retry-After handling.
func (b *Base) doSendClassified(ctx context.Context, path string, payload []byte, extra map[string]string,
	classifier responseFailureClassifier) (*http.Response, *domain.Failure) {
	requestURL, err := b.Endpoint.RequestURL(path)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailConnect, Message: b.redact(err.Error())}
	}
	headers := map[string]string{
		"Authorization": "Bearer " + b.Secret,
		"x-api-key":     b.Secret,
	}
	if b.Credential.Header != "" || b.Credential.QueryParam != "" {
		headers = map[string]string{}
	}
	if b.Credential.Header != "" {
		headers[b.Credential.Header] = b.Credential.Prefix + b.Secret
	}
	if b.Credential.QueryParam != "" {
		u, err := url.Parse(requestURL)
		if err != nil {
			return nil, &domain.Failure{Type: domain.FailConnect, Message: b.redact(err.Error())}
		}
		q := u.Query()
		q.Set(b.Credential.QueryParam, b.Secret)
		u.RawQuery = q.Encode()
		requestURL = u.String()
	}
	for k, v := range b.ExtraHeaders {
		headers[k] = v
	}
	for k, v := range extra {
		headers[k] = v
	}
	req, err := newRequest(ctx, http.MethodPost, requestURL, payload, headers)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailConnect, Message: b.redact(err.Error())}
	}
	if b.BeforeSend != nil {
		if failure := b.BeforeSend(ctx); failure != nil {
			return nil, failure
		}
	}
	observeServiceTierRequest(ctx, req, payload)
	resp, err := b.HTTP.Do(req)
	if err != nil {
		f := &domain.Failure{Type: domain.FailConnect, Message: b.redact(err.Error())}
		if ctx.Err() != nil && !strings.Contains(err.Error(), "context deadline") {
			f.Type = domain.FailClientCancel
		}
		return nil, f
	}
	if b.AfterResponse != nil {
		b.AfterResponse(resp.Header)
	}
	observeServiceTierResponse(ctx, req, resp.Request)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close() // #nosec G104 -- best-effort close after a bounded, redacted read of an error body
		safeBody := b.redact(string(body))
		var f *domain.Failure
		if classifier != nil {
			f = classifier(resp.StatusCode, safeBody, resp.Header)
		}
		if f == nil {
			ft := domain.ClassifyHTTP(resp.StatusCode, safeBody)
			f = &domain.Failure{Type: ft, HTTPStatus: resp.StatusCode, Message: truncate(safeBody, 512)}
		}
		if f.Type == domain.FailUnsupportedParam && f.Scope == "" {
			// The upstream, rather than the ingress parser, rejected this
			// parameter set. Another target may support it.
			f.Scope = domain.FailureScopeTarget
		}
		if f.HTTPStatus == 0 {
			f.HTTPStatus = resp.StatusCode
		}
		if f.Message == "" {
			f.Message = truncate(safeBody, 512)
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if f.RetryAfter == 0 {
				if d, err := time.ParseDuration(ra + "s"); err == nil && d > 0 && d < 10*time.Minute {
					f.RetryAfter = d
				} else if secs := parseRetryAfter(ra); secs > 0 {
					f.RetryAfter = time.Duration(secs) * time.Second
				}
			}
		}
		return nil, f
	}
	return resp, nil
}

func (b *Base) redact(s string) string {
	if b.Secret == "" {
		return s
	}
	for _, material := range []string{b.Secret, url.QueryEscape(b.Secret), url.PathEscape(b.Secret)} {
		if material != "" {
			s = strings.ReplaceAll(s, material, "[REDACTED]")
		}
	}
	return s
}

func parseRetryAfter(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// SSE reading shared by all adapters

type sseEvent struct {
	name string
	data string
}

func readSSE(r io.Reader, fn func(ev sseEvent) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var dataLines []string
	eventName := ""
	flush := func() error {
		if len(dataLines) == 0 && eventName == "" {
			return nil
		}
		ev := sseEvent{name: eventName, data: strings.Join(dataLines, "\n")}
		dataLines = dataLines[:0]
		eventName = ""
		if ev.data == "" && ev.name == "" {
			return nil
		}
		return fn(ev)
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if err := flush(); err != nil {
					return err
				}
			case strings.HasPrefix(line, ":"):
				// comment / keepalive
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}

// decodeUsage merges provider usage JSON into canonical Usage (best effort).
func decodeUsage(raw json.RawMessage) domain.Usage {
	type inputDetails struct {
		CachedTokens        int64  `json:"cached_tokens"`
		CacheWriteTokens    *int64 `json:"cache_write_tokens"`
		CacheCreationTokens int64  `json:"cache_creation_tokens"`
	}
	var u struct {
		PromptTokens      int64  `json:"prompt_tokens"`
		CompletionTokens  int64  `json:"completion_tokens"`
		InputTokens       int64  `json:"input_tokens"`
		OutputTokens      int64  `json:"output_tokens"`
		CacheRead         *int64 `json:"cache_read_input_tokens"`
		CacheCreation     *int64 `json:"cache_creation_input_tokens"`
		Reasoning         int64  `json:"reasoning_tokens"`
		CompletionDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		OutputDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
		PromptDetails *inputDetails `json:"prompt_tokens_details"`
		InputDetails  *inputDetails `json:"input_tokens_details"`
	}
	_ = json.Unmarshal(raw, &u)
	out := domain.Usage{}
	if u.PromptTokens != 0 {
		out.InputTokens = u.PromptTokens
	} else {
		out.InputTokens = u.InputTokens
	}
	if u.CompletionTokens != 0 {
		out.OutputTokens = u.CompletionTokens
	} else {
		out.OutputTokens = u.OutputTokens
	}
	out.InputTokens = max(0, out.InputTokens)
	out.OutputTokens = max(0, out.OutputTokens)
	reasoningDetails := u.CompletionDetails.ReasoningTokens
	if reasoningDetails == 0 {
		reasoningDetails = u.OutputDetails.ReasoningTokens
	}
	// Independent Anthropic fields take precedence over OpenAI detail aliases.
	// Never add two representations of the same cache usage together.
	if u.CacheRead != nil || u.CacheCreation != nil {
		if u.CacheRead != nil {
			out.CacheReadTokens = max(0, *u.CacheRead)
		}
		if u.CacheCreation != nil {
			out.CacheWriteTokens = max(0, *u.CacheCreation)
		}
	} else {
		details := u.PromptDetails
		if details == nil {
			details = u.InputDetails
		}
		if details != nil {
			out.CacheSubsetOfInput = true
			out.CacheReadTokens = min(out.InputTokens, max(0, details.CachedTokens))
			write := details.CacheCreationTokens
			if details.CacheWriteTokens != nil {
				write = *details.CacheWriteTokens
			}
			// Clamp sequentially to avoid overflow for malformed detail totals.
			out.CacheWriteTokens = min(out.InputTokens-out.CacheReadTokens, max(0, write))
		}
	}
	out.ReasoningTokens = u.Reasoning + reasoningDetails
	return out
}
