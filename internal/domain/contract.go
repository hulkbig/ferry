// Package domain defines Ferry's canonical contracts and cross-package types.
//
// All protocol adapters (ingress and egress) translate between wire formats
// and these canonical types. The scheduler operates exclusively on them.
package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Protocol identifies a wire protocol, both on ingress (client side) and
// egress (upstream provider side).
type Protocol string

const (
	ProtocolOpenAIChat      Protocol = "openai-chat"      // /v1/chat/completions
	ProtocolOpenAIResponses Protocol = "openai-responses" // /v1/responses
	ProtocolOpenAIImages    Protocol = "openai-images"    // /v1/images/generations, /v1/images/edits
	ProtocolAnthropic       Protocol = "anthropic"        // /v1/messages
)

// CompatLevel is the result of any protocol conversion (PRD §6.10).
type CompatLevel string

const (
	CompatExact       CompatLevel = "exact"       // lossless round-trip
	CompatMapped      CompatLevel = "mapped"      // semantically equivalent mapping
	CompatDegraded    CompatLevel = "degraded"    // information lost
	CompatUnsupported CompatLevel = "unsupported" // cannot represent
)

// DegradationAllowed controls the default best-effort route policy. Exact and
// semantically mapped requests are accepted, and degraded requests may proceed
// with their compatibility notes recorded for operators. Unsupported requests
// remain blocked. Callers that require strict fidelity enforce exact_only
// separately before consulting this policy.
func DegradationAllowed(_ bool, level CompatLevel) bool {
	return level != CompatUnsupported
}

// Capabilities describes what a target can do (F-P0-06).
type Capabilities struct {
	Text             bool `json:"text" yaml:"text"`
	Images           bool `json:"images" yaml:"images"`
	Stream           bool `json:"stream" yaml:"stream"`
	Tools            bool `json:"tools" yaml:"tools"`
	ParallelTools    bool `json:"parallel_tools" yaml:"parallel_tools"`
	Reasoning        bool `json:"reasoning" yaml:"reasoning"`
	StructuredOutput bool `json:"structured_output" yaml:"structured_output"`
	Vision           bool `json:"vision" yaml:"vision"`
	// Priority means the endpoint explicitly supports OpenAI priority processing.
	// It is account/endpoint evidence, never inferred from the model name.
	Priority      bool  `json:"priority" yaml:"priority"`
	ContextWindow int64 `json:"context_window" yaml:"context_window"` // tokens
	MaxOutput     int64 `json:"max_output" yaml:"max_output"`         // tokens
}

// NormalizeProtocolCapabilities supplies capability bits that are inherent to
// a native API family. It is intentionally additive: one upstream model may
// expose both a text protocol and the Images API, so normalization must not
// erase explicitly declared support for either family.
func NormalizeProtocolCapabilities(capabilities Capabilities, protocols []Protocol) Capabilities {
	for _, protocol := range protocols {
		if protocol == ProtocolOpenAIImages {
			capabilities.Images = true
		}
	}
	return capabilities
}

// Satisfies reports whether caps fulfill every requirement in req.
// Context window requirement of 0 means "no minimum".
func (c Capabilities) Satisfies(req Requirements) bool {
	for _, capability := range BooleanCapabilityRegistry() {
		required, _ := RequirementCapability(req, capability)
		supported, _ := BooleanCapability(c, capability)
		if required && !supported {
			return false
		}
	}
	if req.MinContext > 0 && c.ContextWindow > 0 && c.ContextWindow < req.MinContext {
		return false
	}
	if req.MinOutput > 0 && c.MaxOutput > 0 && c.MaxOutput < req.MinOutput {
		return false
	}
	return true
}

// Requirements is the capability demand extracted from a request contract
// (PRD §12.2 "提取 Required Capability").
type Requirements struct {
	Text             bool  `json:"text"`
	Images           bool  `json:"images"`
	Stream           bool  `json:"stream"`
	Tools            bool  `json:"tools"`
	ParallelTools    bool  `json:"parallel_tools"`
	Reasoning        bool  `json:"reasoning"`
	StructuredOutput bool  `json:"structured_output"`
	Vision           bool  `json:"vision"`
	Priority         bool  `json:"priority"`
	MinContext       int64 `json:"min_context"` // estimated input size + margin
	MinOutput        int64 `json:"min_output"`  // requested maximum output tokens
}

// ---------------------------------------------------------------------------
// Canonical request contract (F-P0-04)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type BlockKind string

const (
	BlockText             BlockKind = "text"
	BlockThinking         BlockKind = "thinking"
	BlockRedactedThinking BlockKind = "redacted_thinking"
	BlockToolUse          BlockKind = "tool_use"
	BlockToolResult       BlockKind = "tool_result"
	BlockImage            BlockKind = "image"
	BlockOpaque           BlockKind = "opaque"
)

// Block is one piece of message content in canonical form.
type Block struct {
	Kind       BlockKind
	Text       string          // text & thinking content
	Signature  string          // opaque Anthropic thinking integrity signature
	Data       string          // opaque redacted thinking payload
	ID         string          // tool_use id
	Name       string          // tool_use name
	Input      json.RawMessage // tool_use arguments object
	Result     json.RawMessage // tool_result native content
	IsError    bool            // tool_result error marker
	ImageMIME  string          // image media type
	ImageData  []byte          // image bytes (base64-decoded)
	OpaqueType string          // native-only content/item type
	Opaque     json.RawMessage // preserved native-only content/item payload
}

// Message is one canonical conversation turn.
type Message struct {
	Role   Role
	Blocks []Block
	Name   string // optional sender name
	ToolID string // role=tool: id of the tool_use this answers
}

// ToolDef declares a callable tool.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema object
}

// ResponseFormat requests structured output (F-P0-06 structured_output).
type ResponseFormat struct {
	Strict *bool // nil preserves the source default; Anthropic native schema output is strict
	Name   string
	Schema json.RawMessage // JSON Schema
}

// ReasoningSpec carries reasoning/thinking demand across protocols.
type ReasoningSpec struct {
	Effort       string // "low" | "medium" | "high" ("" = unspecified)
	BudgetTokens int64  // anthropic-style budget; 0 = unspecified
}

// RequestContract is the canonical form of any supported ingress request.
type RequestContract struct {
	Protocol       Protocol
	Model          string // alias or concrete model name as requested
	Messages       []Message
	Tools          []ToolDef
	ToolChoice     string // "", "auto", "none", "required", or a tool name
	ResponseFormat *ResponseFormat
	Reasoning      *ReasoningSpec
	MaxTokens      int64 // 0 = provider default
	Temperature    *float64
	TopP           *float64
	TopK           *int64
	ParallelTools  *bool
	StopSequences  []string
	Stream         bool
	// ServiceTier preserves the OpenAI processing tier across Chat/Responses.
	// Other protocol families do not share these values.
	ServiceTier string
	// StreamIncludeUsage is the Chat Completions response-framing preference
	// from stream_options.include_usage. It does not change model capability
	// requirements; ingress uses it only when rendering a converted Chat stream.
	StreamIncludeUsage bool

	// Metadata: session/conversation identity for affinity (F-P1-08),
	// plus protocol-specific ids worth carrying through.
	Metadata map[string]string
	Lineage  *ResponsesLineage
	// Continuation is scheduler-owned attempt metadata. Clients can never set
	// provider-native handles directly.
	Continuation       *NativeContinuation
	PersistNativeState bool
	// ProviderSessionKey is scheduler-owned continuity identity for upstreams
	// that need a stable session even when the client protocol has no native
	// conversation field (for example Chat/Anthropic clients routed to Codex).
	// It is never accepted from or emitted to clients.
	ProviderSessionKey string `json:"-"`
	// CodexTurnStateAllowed is set only for a conversation still bound to the
	// same Codex provider connection. Opaque state cannot follow a rebind.
	CodexTurnStateAllowed bool `json:"-"`

	// Native preserves the raw ingress body so egress adapters of the same
	// protocol can achieve Exact compatibility without re-encoding.
	Native json.RawMessage
	// NativeContentType and Operation preserve non-JSON protocol variants.
	// OpenAI Images edits, for example, use multipart/form-data while image
	// generations use application/json under the same protocol.
	NativeContentType string `json:"-"`
	Operation         string `json:"-"`
	// ProtocolHeaders preserves feature-negotiation headers which are part of
	// the wire contract (for example anthropic-beta and OpenAI-Beta). Secrets
	// and tenant identity headers are deliberately never captured here.
	ProtocolHeaders map[string]string `json:"-"`
	// ConversionIssues lists native request features without a lossless
	// canonical representation. Same-protocol passthrough remains exact;
	// cross-protocol attempts fail closed instead of dropping them silently.
	ConversionIssues []string `json:"-"`
	// MappingNotes lists auxiliary native features (caching hints, context
	// editing directives, verbosity, provider sampling extras) that were
	// translated to their closest target equivalent or intentionally omitted
	// because they optimize rather than change request content. They are
	// informational: unlike ConversionIssues they never block cross-protocol
	// conversion, and they surface to the client as mapped compatibility
	// notes.
	MappingNotes []string `json:"-"`
	// Verbosity is the canonical request verbosity ("low"|"medium"|"high")
	// carried by Anthropic output_config, Chat verbosity, and Responses
	// text.verbosity. Egress adapters map it back onto their native knob.
	Verbosity string `json:"-"`
	// NativeRequirements carries capability demands from native-only request
	// features that cannot be represented by the canonical fields above. It is
	// routing metadata only: exact same-protocol passthrough still uses Native,
	// while cross-protocol conversion continues to fail closed.
	NativeRequirements Requirements `json:"-"`

	// Origin records the wire protocol the client actually used, kept
	// separate from Protocol (the egress format chosen for the current
	// attempt) so adapters know when the preserved Native body is replayable
	// verbatim versus a cross-protocol conversion.
	Origin Protocol `json:"-"`
}

// ValidJSONObject reports whether raw is a non-null JSON object. Function
// schemas and function-call arguments use object semantics in every supported
// text protocol; accepting another JSON kind would create a request that some
// adapters can serialize but no longer means the same thing.
func ValidJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// EstimateContext returns a conservative token estimate of the prompt,
// used for MinContext gating. ~4 bytes per token heuristic.
func (rc *RequestContract) EstimateContext() int64 {
	var chars int64
	for _, m := range rc.Messages {
		for _, b := range m.Blocks {
			chars += int64(len(b.Text))
			chars += int64(len(b.Input)) / 2
			if len(b.ImageData) > 0 {
				chars += 2000 // ~1.5k tokens for a typical image
			}
		}
	}
	tokens := chars / 3
	// margin for output + template overhead; clamp to something sane
	margin := rc.MaxTokens
	if margin <= 0 {
		margin = 8192
	}
	tokens += margin
	if tokens < 1024 {
		tokens = 1024
	}
	return tokens
}

// ExtractRequirements derives the capability gate input (PRD §6.6).
func (rc *RequestContract) ExtractRequirements() Requirements {
	// Images is a native-only API family, not a text-generation model with a
	// vision input. Treating its prompt as a Text requirement makes a correctly
	// declared image endpoint ineligible; treating it as Vision would claim the
	// endpoint can understand images in chat. Keep the contract independent.
	if rc.Protocol == ProtocolOpenAIImages {
		return Requirements{Images: true}
	}
	req := Requirements{
		Text:   true,
		Stream: rc.Stream,
		// OpenAI also accepts fast as an alias; it must not bypass this gate.
		Priority: strings.EqualFold(strings.TrimSpace(rc.ServiceTier), "priority") ||
			strings.EqualFold(strings.TrimSpace(rc.ServiceTier), "fast") ||
			strings.EqualFold(strings.TrimSpace(rc.ServiceTier), "ultrafast"),
	}
	if len(rc.Tools) > 0 {
		req.Tools = true
	}
	if choice := strings.TrimSpace(rc.ToolChoice); choice != "" && !strings.EqualFold(choice, "none") {
		req.Tools = true
	}
	if rc.ParallelTools != nil && *rc.ParallelTools {
		req.Tools = true
		req.ParallelTools = true
	}
	// Both json_object and json_schema are structured-output contracts. The
	// former intentionally has no schema bytes, but must still never be routed
	// to a target that only promises free-form text.
	if rc.ResponseFormat != nil {
		req.StructuredOutput = true
	}
	if rc.Reasoning != nil {
		req.Reasoning = true
	}
	for _, m := range rc.Messages {
		if m.Role == RoleTool {
			req.Tools = true
		}
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolUse, BlockToolResult:
				req.Tools = true
			case BlockThinking, BlockRedactedThinking:
				req.Reasoning = true
			case BlockImage:
				req.Vision = true
			case BlockOpaque:
				req = MergeRequirements(req, nativeOpaqueRequirements(b.OpaqueType))
			}
		}
	}
	req = MergeRequirements(req, rc.NativeRequirements)
	req.MinContext = rc.EstimateContext()
	req.MinOutput = rc.MaxTokens
	return req
}

// nativeOpaqueRequirements maps only request semantics whose capability is
// unambiguous even though their wire payload is not losslessly canonicalized.
// Unknown native types deliberately stay unmapped: same-protocol passthrough
// remains available, and the upstream may return a precise protocol error.
func nativeOpaqueRequirements(opaqueType string) Requirements {
	typ := strings.ToLower(strings.TrimSpace(opaqueType))
	switch typ {
	case "image_url", "input_image", "image":
		return Requirements{Vision: true}
	case "reasoning", "thinking", "redacted_thinking":
		return Requirements{Reasoning: true}
	case "tool_use", "tool_result", "server_tool_use":
		return Requirements{Tools: true}
	}
	if strings.HasSuffix(typ, "_call") || strings.HasSuffix(typ, "_call_output") ||
		strings.HasSuffix(typ, "_tool_result") || strings.HasPrefix(typ, "mcp_") {
		return Requirements{Tools: true}
	}
	return Requirements{}
}

// ValidateToolSequence checks the protocol-independent tool transcript after
// the scheduler has chosen native continuation or rehydrated full history.
// It deliberately runs at final-attempt time: a Responses continuation may
// begin with function_call_output because the matching call lives behind a
// provider-native previous_response_id, while a rehydrated route must carry
// the call explicitly.
func (rc *RequestContract) ValidateToolSequence() error {
	if err := rc.ValidateToolShape(); err != nil {
		return err
	}
	pending := map[string]bool{}
	callIDs := map[string]bool{}
	allowLeadingResults := rc.Continuation != nil
	seenConversationItem := false
	for messageIndex, message := range rc.Messages {
		results := map[string]bool{}
		if message.Role == RoleTool && strings.TrimSpace(message.ToolID) == "" {
			return fmt.Errorf("tool result message %d is missing its tool call id", messageIndex)
		}
		var calls []Block
		for _, block := range message.Blocks {
			switch block.Kind {
			case BlockToolUse:
				if callIDs[block.ID] {
					return fmt.Errorf("tool call id %q is duplicated", block.ID)
				}
				calls = append(calls, block)
			case BlockToolResult:
				id := block.ID
				if id == "" {
					id = message.ToolID
				}
				if results[id] {
					return fmt.Errorf("tool result id %q is duplicated", id)
				}
				results[id] = true
			}
		}
		if message.Role == RoleTool {
			if len(results) == 0 {
				results[message.ToolID] = true
			} else if !results[message.ToolID] {
				return fmt.Errorf("tool result message %d id %q does not match its content",
					messageIndex, message.ToolID)
			}
		}
		if len(pending) > 0 {
			if len(results) == 0 {
				return fmt.Errorf("tool calls %s require immediate tool results before message %d",
					strings.Join(sortedBoolKeys(pending), ", "), messageIndex)
			}
			for id := range results {
				if !pending[id] {
					return fmt.Errorf("tool result %q does not match a pending tool call", id)
				}
				delete(pending, id)
			}
		} else if len(results) > 0 {
			if !allowLeadingResults || seenConversationItem {
				return fmt.Errorf("tool result %q has no preceding tool call", sortedBoolKeys(results)[0])
			}
		}
		if len(calls) > 0 {
			if len(pending) > 0 || len(results) > 0 {
				return fmt.Errorf("message %d starts new tool calls before prior results complete", messageIndex)
			}
			for _, call := range calls {
				pending[call.ID] = true
				callIDs[call.ID] = true
			}
		}
		if len(results) == 0 {
			seenConversationItem = true
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("tool calls %s are missing their tool results",
			strings.Join(sortedBoolKeys(pending), ", "))
	}
	return nil
}

// ValidateToolShape checks malformed tool fields independently of transcript
// ordering. Native Responses input can be replayed without imposing canonical
// one-result-per-call ordering, but its decoded fields still need validation.
func (rc *RequestContract) ValidateToolShape() error {
	if rc == nil {
		return fmt.Errorf("request contract is nil")
	}
	if rc.ResponseFormat != nil && len(rc.ResponseFormat.Schema) > 0 &&
		!ValidJSONObject(rc.ResponseFormat.Schema) {
		return fmt.Errorf("response format schema must be a JSON object")
	}
	toolNames := map[string]bool{}
	callIDs := map[string]bool{}
	for _, tool := range rc.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("tool definition is missing its name")
		}
		if toolNames[tool.Name] {
			return fmt.Errorf("tool definition %q is duplicated", tool.Name)
		}
		toolNames[tool.Name] = true
		if !ValidJSONObject(tool.Parameters) {
			return fmt.Errorf("tool %q parameters must be a JSON object", tool.Name)
		}
	}
	for messageIndex, message := range rc.Messages {
		results := map[string]bool{}
		if message.Role == RoleTool {
			if strings.TrimSpace(message.ToolID) == "" {
				return fmt.Errorf("tool result message %d is missing its tool call id", messageIndex)
			}
		}
		for _, block := range message.Blocks {
			switch block.Kind {
			case BlockToolUse:
				if message.Role != RoleAssistant {
					return fmt.Errorf("tool call %q appears outside an assistant message", block.ID)
				}
				if strings.TrimSpace(block.ID) == "" || strings.TrimSpace(block.Name) == "" {
					return fmt.Errorf("tool call in message %d is missing its id or name", messageIndex)
				}
				if callIDs[block.ID] {
					return fmt.Errorf("tool call id %q is duplicated", block.ID)
				}
				callIDs[block.ID] = true
				if !ValidJSONObject(block.Input) {
					return fmt.Errorf("tool call %q arguments must be a JSON object", block.ID)
				}
			case BlockToolResult:
				if message.Role != RoleTool {
					return fmt.Errorf("tool result %q appears outside a tool message", block.ID)
				}
				id := block.ID
				if id == "" {
					id = message.ToolID
				}
				if strings.TrimSpace(id) == "" {
					return fmt.Errorf("tool result in message %d is missing its tool call id", messageIndex)
				}
				if len(block.Result) == 0 || !json.Valid(block.Result) {
					return fmt.Errorf("tool result %q content is not valid JSON", id)
				}
				results[id] = true
			}
		}
		if message.Role == RoleTool && len(results) > 0 && !results[message.ToolID] {
			return fmt.Errorf("tool result message %d id %q does not match its content", messageIndex, message.ToolID)
		}
	}
	return nil
}

func sortedBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Canonical response & usage

// Usage is immutable raw consumption (PRD §15.2/15.3).
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	// CacheSubsetOfInput marks upstream dialects whose reported input total
	// already contains the cached tokens (OpenAI-style subset semantics).
	// Anthropic-style independent semantics leave it false. Cost accounting
	// must subtract the cached portion so it is never priced twice.
	CacheSubsetOfInput bool `json:"cache_subset_of_input"`
}

// Total returns input+output as providers commonly bill them.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// UncachedInput returns the input token count excluding cached tokens under
// the usage's own semantics: subset dialects subtract the cached portion,
// independent dialects report input without cache already.
func (u Usage) UncachedInput() int64 {
	if !u.CacheSubsetOfInput {
		return u.InputTokens
	}
	uncached := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if uncached < 0 {
		return 0
	}
	return uncached
}

// BillableTokens returns every token a quota window counts: uncached input,
// cached reads and writes (Claude plans count cache toward usage), and output.
func (u Usage) BillableTokens() int64 {
	return u.UncachedInput() + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
}

// Response is a complete non-streaming canonical response.
type Response struct {
	Content              []Block
	FinishReason         string // canonical: "stop", "tool_call", "length"
	Usage                Usage
	Model                string // what the upstream reported
	ServiceTier          string // what the upstream reported; never inferred from the request
	NativeResponseID     string
	NativeConversationID string
	// NativeBody is returned verbatim by ingress adapters for protocols whose
	// response cannot be represented by the text-oriented canonical blocks.
	NativeBody        []byte   `json:"-"`
	NativeContentType string   `json:"-"`
	NativeProtocol    Protocol `json:"-"`
	ConversionIssues  []string `json:"-"`
	// Warnings describe safe, client-visible compatibility mappings or omitted
	// provider metadata. They are diagnostic only and never block a response.
	Warnings []string `json:"-"`
	// ValidationIssues are violations of the selected upstream protocol's
	// known response grammar. Unlike native-only extension fields, raw
	// same-protocol passthrough cannot make a malformed terminal/tool shape
	// successful; the scheduler must fail or recover before commit.
	ValidationIssues []string `json:"-"`
}

// ---------------------------------------------------------------------------
// Canonical stream event contract

type EventKind string

const (
	EvStart            EventKind = "start"             // message started (target known)
	EvTextDelta        EventKind = "text_delta"        // TextDelta appended
	EvThinkingDelta    EventKind = "thinking_delta"    // ThinkingDelta appended
	EvSignatureDelta   EventKind = "signature_delta"   // SignatureDelta appended to thinking
	EvRedactedThinking EventKind = "redacted_thinking" // complete opaque redacted thinking block
	EvToolStart        EventKind = "tool_start"        // begin tool call: Index/ToolID/ToolName
	EvToolDelta        EventKind = "tool_delta"        // args chunk: Index/ArgsDelta
	EvToolEnd          EventKind = "tool_end"          // tool call complete: Index
	EvNative           EventKind = "native"            // same-protocol upstream SSE frame
	EvEnd              EventKind = "end"               // FinishReason + Usage
	EvError            EventKind = "error"             // terminal upstream error mid-stream
)

// NativeSSEFrame preserves an upstream event for same-protocol clients. The
// canonical event emitted beside it remains available to the scheduler and to
// cross-protocol ingress adapters; native ingress writes this frame instead.
type NativeSSEFrame struct {
	KeepAlive bool // Validated heartbeat; discard before commit without buffering.
	// Meaningful marks adapter-validated native-only output, not lifecycle or
	// keepalive frames. It commits same-protocol output without fabricating a
	// canonical representation of opaque provider reasoning.
	Meaningful bool
	Event      string
	Data       string
	Terminal   bool
}

// StreamEvent is the canonical streaming unit. Adapters translate both ways.
// Field relevance depends on Kind; unused fields stay zero.
type StreamEvent struct {
	Model                string // upstream metadata; terminal values override earlier observations
	ServiceTier          string
	Kind                 EventKind
	Index                int // tool/block ordinal within the message
	TextDelta            string
	ThinkingDelta        string
	SignatureDelta       string
	RedactedThinkingData string
	ToolID               string
	ToolName             string
	ArgsDelta            string
	FinishReason         string
	Usage                *Usage
	Err                  *Failure
	NativeResponseID     string
	NativeConversationID string
	NativeProtocol       Protocol
	NativeFrame          *NativeSSEFrame
}
