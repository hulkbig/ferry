package nativewire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const agEndpoint = "https://daily-cloudcode-pa.googleapis.com"

type antigravityTransport struct {
	base    http.RoundTripper
	project string
}

// NewAntigravityTransport must be constructed with the project bound to the
// current OAuth token, not a project supplied by the inference caller.
func NewAntigravityTransport(base http.RoundTripper, project string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &antigravityTransport{base: base, project: project}
}
func agHTTPResponse(r *http.Request, status int, body []byte, kind string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}
}
func agErrorResponse(r *http.Request, status int, message string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "antigravity_error", "code": status}})
	return agHTTPResponse(r, status, b, "application/json")
}
func (t *antigravityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	if r.URL == nil || r.Method != "POST" || r.URL.Scheme != "https" || r.URL.Host != "daily-cloudcode-pa.googleapis.com" || r.URL.Path != "/chat/completions" || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || r.URL.User != nil || r.Host != "" && r.Host != r.URL.Host || !agProject.MatchString(t.project) {
		return nil, errors.New("invalid Antigravity endpoint or account project")
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(auth) <= 7 || strings.ContainsAny(auth[7:], " \r\n\t") {
		return nil, errors.New("Antigravity bearer credential required")
	}
	if r.Body == nil {
		return agErrorResponse(r, 400, agWireInvalid.Error()), nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 8<<20 {
		return agErrorResponse(r, 413, "Antigravity request too large"), nil
	}
	chat, body, err := agEncodeRequest(raw, t.project)
	if err != nil {
		return agErrorResponse(r, 400, agWireInvalid.Error()), nil
	}
	endpoint := agEndpoint + "/v1internal:generateContent"
	if chat.Stream {
		endpoint = agEndpoint + "/v1internal:streamGenerateContent?alt=sse"
	}
	next, err := http.NewRequestWithContext(r.Context(), "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	next.Header.Set("Authorization", auth)
	next.Header.Set("Content-Type", "application/json")
	next.Header.Set("User-Agent", "Ferry/native-antigravity")
	if chat.Stream {
		next.Header.Set("Accept", "text/event-stream")
	}
	resp, err := t.base.RoundTrip(next)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, errors.New("Antigravity upstream transport failed")
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("Antigravity upstream response missing")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = 502
		}
		out := agErrorResponse(r, status, "Antigravity upstream request failed")
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			out.Header.Set("Retry-After", retry)
		}
		return out, nil
	}
	state := agResponseState{model: chat.Model, id: "chatcmpl-" + agID(), toolIDs: map[string]bool{}}
	if chat.Stream {
		return agStreamResponse(r, resp, &state, chat.StreamOptions != nil && chat.StreamOptions.IncludeUsage), nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(b) > 16<<20 {
		return agErrorResponse(r, 502, "invalid Antigravity response"), nil
	}
	message, err := state.consume(b)
	if err != nil || state.finish == "" {
		return agErrorResponse(r, 502, "unsupported or incomplete Antigravity response"), nil
	}
	message["role"] = "assistant"
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, call := range calls {
			delete(call.(map[string]any), "index")
		}
	}
	out := state.chunk([]any{map[string]any{"index": 0, "message": message, "finish_reason": state.finish}}, false)
	out["usage"] = state.usage.chat()
	encoded, _ := json.Marshal(out)
	return agHTTPResponse(r, 200, encoded, "application/json"), nil
}
