// Package ingress adapts the supported client protocols to the
// canonical contract and back (PRD §5, §6.1): /v1/chat/completions,
// /v1/responses, /v1/messages — each covering text/stream/tools/reasoning/
// usage in both directions, plus protocol-shaped error mapping.
package ingress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"

	"ferry/internal/buildinfo"
	"ferry/internal/domain"
	"ferry/internal/policy"
	sched "ferry/internal/scheduler"
)

// Handler serves one ingress protocol against the scheduler.
type Handler interface {
	Protocol() domain.Protocol
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// Engine is the scheduler surface the ingress layer needs. Kept as an
// interface so protocol tests can drive handlers without a live world.
type Engine interface {
	Execute(ctx context.Context, rc *domain.RequestContract, meta sched.RequestMeta) (*sched.Result, *domain.Failure)
	DryRun(rc *domain.RequestContract, meta sched.RequestMeta) (*policy.PlanResult, *domain.CompiledPolicy, *domain.Failure)
}

// Server routes the ingress endpoints to their adapters.
type Server struct {
	Chat      *OpenAIChatHandler
	Responses *ResponsesHandler
	Images    *OpenAIImagesHandler
	Messages  *AnthropicHandler
	Models    *ModelsHandler
}

// NewServer builds the three protocol handlers over one engine.
func NewServer(eng Engine) *Server {
	return &Server{
		Chat:      &OpenAIChatHandler{Eng: eng},
		Responses: &ResponsesHandler{Eng: eng},
		Images:    &OpenAIImagesHandler{Eng: eng},
		Messages:  &AnthropicHandler{Eng: eng},
		Models:    &ModelsHandler{Source: eng},
	}
}

func (s *Server) HandlerFor(proto domain.Protocol) http.Handler {
	switch proto {
	case domain.ProtocolAnthropic:
		return s.Messages
	case domain.ProtocolOpenAIResponses:
		return s.Responses
	case domain.ProtocolOpenAIImages:
		return s.Images
	default:
		return s.Chat
	}
}

// Register mounts all supported protocols on mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/v1/chat/completions", s.Chat)
	mux.Handle("/v1/responses", s.Responses)
	mux.Handle("/v1/messages", s.Messages)
	mux.Handle("/v1/images/generations", s.Images)
	mux.Handle("/v1/images/edits", s.Images)
	mux.Handle("/v1/models", s.Models)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		info := buildinfo.Current()
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": info.Version})
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, buildinfo.Current())
	})
}

// parseError is a 4xx-shaped problem with the request itself.
type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

func perrf(format string, args ...any) error {
	return &parseError{msg: fmt.Sprintf(format, args...)}
}

// decodeBody reads and unmarshals the JSON request body.
func decodeBody(r *http.Request) (json.RawMessage, error) {
	body, err := readAll(r)
	if err != nil {
		return nil, perrf("read body: %v", err)
	}
	if len(body) == 0 {
		return nil, perrf("empty request body")
	}
	return json.RawMessage(body), nil
}

// bearerOf extracts the client credential.
func bearerOf(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.Header.Get("X-Api-Key") // anthropic style
}

func requestMeta(r *http.Request, rc *domain.RequestContract) sched.RequestMeta {
	meta := sched.RequestMeta{PresentedCredential: bearerOf(r), ClientIP: clientIP(r)}
	add := func(source string, scope domain.AffinityScope, raw string, confidence domain.Confidence) {
		if strings.TrimSpace(raw) != "" {
			meta.AffinityEvidence = append(meta.AffinityEvidence,
				domain.NewAffinityEvidence(source, scope, raw, confidence))
		}
	}
	if rc != nil && rc.Lineage != nil {
		add("responses.conversation", domain.AffinityConversation, rc.Lineage.ConversationID, domain.ConfHigh)
		add("responses.previous_response_id", domain.AffinityConversation, rc.Lineage.PreviousResponseID, domain.ConfHigh)
		add("prompt_cache_key", domain.AffinityCache, rc.Lineage.PromptCacheKey, domain.ConfHigh)
	}
	if rc != nil && rc.Metadata != nil {
		// Anthropic explicitly defines metadata.user_id as a user/caller hint;
		// it must never be promoted to conversation evidence.
		add("anthropic.metadata.user_id", domain.AffinityCaller, rc.Metadata["user_id"], domain.ConfMedium)
	}
	for _, item := range []struct {
		headers []string
		source  string
		scope   domain.AffinityScope
	}{
		{[]string{"X-Claude-Code-Session-Id"}, "claude-code.session", domain.AffinityConversation},
		{[]string{"Thread-Id", "X-Codex-Thread-Id"}, "codex.thread", domain.AffinityConversation},
		// A Codex process session can span multiple threads, so it is useful
		// cache affinity but is deliberately weaker than conversation evidence.
		{[]string{"Session-Id", "X-Codex-Session-Id"}, "codex.session", domain.AffinityCache},
		{[]string{"X-Hermes-Session-Id"}, "hermes.session_id", domain.AffinityConversation},
		{[]string{"X-Hermes-Session-Key"}, "hermes.session_key", domain.AffinityCache},
		{[]string{"X-OpenClaw-Session-Key"}, "openclaw.session_key", domain.AffinityConversation},
	} {
		for _, header := range item.headers {
			if raw := r.Header.Get(header); strings.TrimSpace(raw) != "" {
				add(item.source, item.scope, raw, domain.ConfHigh)
				break
			}
		}
	}
	return meta
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sseWriter frames canonical events into an SSE response stream.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	return &sseWriter{w: w}
}

func (s *sseWriter) begin() {
	if !s.started {
		s.started = true
		s.w.WriteHeader(http.StatusOK)
		if f, ok := s.w.(http.Flusher); ok {
			s.flusher = f
			f.Flush()
		}
	}
}

func (s *sseWriter) event(name string, payload any) {
	s.begin()
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	if s.flusher != nil {
		s.flusher.Flush()
	}
	s.observeOutput(name, b)
}

func (s *sseWriter) data(payload any) {
	s.begin()
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.flusher != nil {
		s.flusher.Flush()
	}
	s.observeOutput("", b)
}

func (s *sseWriter) done() {
	s.begin()
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// rawFrame preserves the encoded SSE payload. Multi-line data is reframed
// according to the SSE specification.
func (s *sseWriter) rawFrame(frame *domain.NativeSSEFrame) {
	if frame == nil {
		return
	}
	s.begin()
	if frame.Event != "" {
		fmt.Fprintf(s.w, "event: %s\n", frame.Event)
	}
	for _, line := range strings.Split(frame.Data, "\n") {
		fmt.Fprintf(s.w, "data: %s\n", line)
	}
	fmt.Fprint(s.w, "\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
	s.observeOutput(frame.Event, []byte(frame.Data))
}

func (s *sseWriter) observeOutput(event string, payload []byte) {
	if observer, ok := s.w.(interface{ observeSSEOutput(string, []byte) }); ok {
		observer.observeSSEOutput(event, payload)
	}
}

func captureProtocolHeaders(r *http.Request, rc *domain.RequestContract) {
	if r == nil || rc == nil {
		return
	}
	allowed := []string{"Anthropic-Version", "Anthropic-Beta", "OpenAI-Beta"}
	if rc.Origin == domain.ProtocolOpenAIResponses {
		allowed = append(allowed, "X-Codex-Turn-State")
	}
	for _, name := range allowed {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			if rc.ProtocolHeaders == nil {
				rc.ProtocolHeaders = map[string]string{}
			}
			rc.ProtocolHeaders[name] = value
			if strings.EqualFold(name, "Anthropic-Beta") || strings.EqualFold(name, "OpenAI-Beta") {
				rc.ConversionIssues = append(rc.ConversionIssues,
					name+" feature negotiation is native-only")
			}
		}
	}
}

func unknownTopLevelFields(body []byte, known ...string) []string {
	return unknownObjectFields(body, "", known...)
}

func unknownObjectFields(body []byte, path string, known ...string) []string {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(known))
	for _, field := range known {
		allowed[field] = struct{}{}
	}
	issues := make([]string, 0)
	for field := range object {
		if _, ok := allowed[field]; !ok {
			qualified := field
			if path != "" {
				qualified = path + "." + field
			}
			issues = append(issues, "field "+qualified+" is native-only")
		}
	}
	sort.Strings(issues)
	return issues
}

func markOpaqueConversionIssues(rc *domain.RequestContract) {
	if rc == nil {
		return
	}
	seen := map[string]bool{}
	for _, issue := range rc.ConversionIssues {
		seen[issue] = true
	}
	for _, message := range rc.Messages {
		for _, block := range message.Blocks {
			// Known canonical fields are checked by the target-specific Preparer.
			// For example, signed/redacted thinking and structured tool results
			// can be losslessly re-encoded to Anthropic, but not to Chat. Only an
			// opaque block fundamentally requires the original native body.
			if block.Kind != domain.BlockOpaque {
				continue
			}
			typ := block.OpaqueType
			if typ == "" {
				typ = "unknown"
			}
			issue := "content type " + typ + " is native-only"
			if !seen[issue] {
				rc.ConversionIssues = append(rc.ConversionIssues, issue)
				seen[issue] = true
			}
		}
	}
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
	for _, block := range blocks {
		if block.Type != "text" {
			return false
		}
	}
	return true
}

func writeNativeResponse(w http.ResponseWriter, res *sched.Result, protocol domain.Protocol) bool {
	if res == nil || res.Response == nil || res.Response.NativeProtocol != protocol || len(res.Response.NativeBody) == 0 {
		return false
	}
	body := patchNativeJSON(protocol, res.Response.NativeBody, res)
	contentType := res.Response.NativeContentType
	if contentType == "" {
		contentType = "application/json"
	}
	return writeNativeJSONResponse(w, body, contentType)
}

func writeNativeJSONResponse(w http.ResponseWriter, body []byte, contentType string) bool {
	if !json.Valid(body) {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body) // #nosec G705 -- body is valid JSON and the response is forced to a nosniff JSON media type above.
	return true
}

func responseConversionFailure(res *sched.Result, protocol domain.Protocol) *domain.Failure {
	if res == nil || res.Response == nil {
		return nil
	}
	if len(res.Response.ValidationIssues) > 0 {
		return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
			Message: "upstream response violates its protocol: " + strings.Join(res.Response.ValidationIssues, "; ")}
	}
	if len(res.Response.ConversionIssues) == 0 {
		return nil
	}
	if res.Response.NativeProtocol == protocol && len(res.Response.NativeBody) > 0 {
		return nil
	}
	return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
		Message: "upstream response cannot be losslessly converted: " + strings.Join(res.Response.ConversionIssues, "; ")}
}

func writeResponseWarnings(w http.ResponseWriter, res *sched.Result) {
	if w == nil || res == nil || res.Response == nil {
		return
	}
	for _, warning := range res.Response.Warnings {
		if warning != "" {
			w.Header().Add("X-SmartAPI-Warning", warning)
			w.Header().Add("X-Ferry-Warning", warning)
		}
	}
}

func writeNativeFrame(sw *sseWriter, res *sched.Result, protocol domain.Protocol, frame *domain.NativeSSEFrame) {
	if frame == nil {
		return
	}
	copy := *frame
	if copy.Data != "[DONE]" && json.Valid([]byte(copy.Data)) {
		copy.Data = string(patchNativeJSON(protocol, []byte(copy.Data), res))
	}
	sw.rawFrame(&copy)
}

// patchNativeJSON keeps unknown provider fields intact while replacing the
// gateway-owned public model/response identifiers. Responses API continuity
// must never expose an upstream response or conversation id to the client.
func patchNativeJSON(protocol domain.Protocol, raw []byte, res *sched.Result) []byte {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return raw
	}
	set := func(target map[string]json.RawMessage, key string, value any) {
		encoded, err := json.Marshal(value)
		if err == nil {
			target[key] = encoded
		}
	}
	patchIdentity := func(target map[string]json.RawMessage, includeConversation bool) {
		set(target, "id", res.RequestID)
		set(target, "model", res.Model)
		if includeConversation {
			if res.ConversationID != "" {
				set(target, "conversation", map[string]string{"id": res.ConversationID})
			} else {
				delete(target, "conversation")
			}
		}
	}

	switch protocol {
	case domain.ProtocolOpenAIChat:
		patchIdentity(object, false)
	case domain.ProtocolAnthropic:
		var typ string
		_ = json.Unmarshal(object["type"], &typ)
		if typ == "message_start" {
			var message map[string]json.RawMessage
			if json.Unmarshal(object["message"], &message) == nil {
				patchIdentity(message, false)
				set(object, "message", message)
			}
		} else if typ == "message" {
			patchIdentity(object, false)
		}
	case domain.ProtocolOpenAIResponses:
		var response map[string]json.RawMessage
		if json.Unmarshal(object["response"], &response) == nil && response != nil {
			patchIdentity(response, true)
			set(object, "response", response)
		}
		var objectType string
		_ = json.Unmarshal(object["object"], &objectType)
		if objectType == "response" {
			patchIdentity(object, true)
		}
		if _, ok := object["response_id"]; ok {
			set(object, "response_id", res.RequestID)
		}
	}
	patched, err := json.Marshal(object)
	if err != nil {
		return raw
	}
	return patched
}

// ---------------------------------------------------------------------------
// shared wire fragments

type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func oaUsageFrom(u domain.Usage) oaUsage {
	var out oaUsage
	out.PromptTokens = u.UncachedInput() + u.CacheReadTokens + u.CacheWriteTokens
	out.CompletionTokens = u.OutputTokens
	out.TotalTokens = out.PromptTokens + out.CompletionTokens
	out.PromptTokensDetails.CachedTokens = u.CacheReadTokens
	out.PromptTokensDetails.CacheWriteTokens = u.CacheWriteTokens
	out.CompletionTokensDetails.ReasoningTokens = u.ReasoningTokens
	return out
}

func blocksText(bs []domain.Block) string {
	var sb strings.Builder
	for _, b := range bs {
		if b.Kind == domain.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func hasToolCall(bs []domain.Block) bool {
	for _, b := range bs {
		if b.Kind == domain.BlockToolUse {
			return true
		}
	}
	return false
}

func timeNowUnix() int64 { return time.Now().Unix() }

// parseDataURL splits a data: URL into MIME type and decoded payload.
// Only base64 payloads are accepted, e.g. "data:image/png;base64,<payload>".
func parseDataURL(u string) (mime string, data []byte, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(u, prefix) {
		return "", nil, false
	}
	rest := u[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", nil, false
	}
	head, payload := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(head, ";base64") {
		return "", nil, false
	}
	mime = strings.TrimSuffix(head, ";base64")
	if mime == "" {
		mime = "application/octet-stream"
	}
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return mime, b, true
}

func readAll(r *http.Request) (body []byte, err error) {
	defer r.Body.Close()
	observeIngressPhase(r.Context(), "BODY_RECEIVE_STARTED", nil)
	defer func() { finishIngressPhase(r.Context(), "BODY_RECEIVE", err) }()
	if err = r.Context().Err(); err != nil {
		return nil, err
	}
	const maxBody = 64 << 20 // 64 MiB guard
	body, err = io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if canceled := r.Context().Err(); canceled != nil {
		return nil, canceled
	}
	return body, err
}
