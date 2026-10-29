/*
Native protocol integration based on CLIProxyAPI d946c4f, internal/runtime/executor/devin_executor.go.
Source: https://github.com/router-for-me/CLIProxyAPI/tree/d946c4f
Narrow adaptation; no upstream SDK/runtime dependency.

MIT License

Copyright (c) 2025-2005.9 Luis Pater
Copyright (c) 2025.9-present Router-For.ME

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/
// Package nativewire contains in-process native upstream wire adapters.
package nativewire

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type devinTransport struct{ base http.RoundTripper }

// NewDevinTransport adapts only POST https://server.codeium.com/chat/completions.
// Wrap it INSIDE the credential-injecting transport. The request must carry a
// Devin Bearer session token and the Ferry-owned metadata headers in devin_request.go.
// The model must be an exact catalog UID. No external proxy, catalog guesses,
// borrowed client identity, background token calls or prompt rewriting is used.
// Unsupported Chat fields fail locally. Text, function tools/results, sampling,
// token limits and usage are supported; images, strict schemas, forced tools,
// reasoning controls and server-side continuation are deliberately rejected.
func NewDevinTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &devinTransport{base: base}
}
func (t *devinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("devin: nil request")
	}
	if req.Body != nil {
		defer req.Body.Close()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if req.URL == nil || req.Method != "POST" || req.URL.Scheme != "https" || req.URL.Host != "server.codeium.com" || req.URL.Path != "/chat/completions" || req.URL.RawQuery != "" || req.URL.Fragment != "" || req.URL.User != nil || req.URL.RawPath != "" {
		return nil, errors.New("devin: unexpected native endpoint")
	}
	auth := req.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth || !strings.HasPrefix(token, "devin-session-token$") || len(token) <= len("devin-session-token$") || len(token) > 16384 || strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r == 127 }) >= 0 {
		return nil, errors.New("devin: normalized Bearer session token required")
	}
	chat, body, err := devinBuild(req, token)
	if err != nil {
		return nil, err
	}
	upstream, err := http.NewRequestWithContext(req.Context(), "POST", "https://server.codeium.com/exa.api_server_pb.ApiServerService/GetChatMessage", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	upstream.Header.Set("Authorization", "Basic "+token+"-"+token)
	upstream.Header.Set("Content-Type", "application/connect+proto")
	upstream.Header.Set("Connect-Protocol-Version", "1")
	upstream.Header.Set("Accept", "*/*")
	upstream.Header.Set("Connect-Accept-Encoding", "gzip")
	resp, err := t.base.RoundTrip(upstream)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, errors.New("devin: upstream transport failed")
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("devin: missing upstream response")
	}
	// Close the body to interrupt blocked reads on cancellation, even for custom
	// round trippers. Streaming also closes its output pipe when cancelled.
	stopCancel := context.AfterFunc(req.Context(), func() { resp.Body.Close() })
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		defer stopCancel()
		return devinErrorResponse(req, &devinFailure{resp.StatusCode, "upstream_http_error", fmt.Sprintf("Devin HTTP status %d", resp.StatusCode)}), nil
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "application/connect+proto") {
		resp.Body.Close()
		stopCancel()
		return devinErrorResponse(req, devinBad("unexpected upstream content type")), nil
	}
	// Prime one frame so immediate trailer/auth errors remain HTTP failures and
	// can use the existing same-account refresh/relogin machinery.
	flag, first, err := devinReadFrame(resp.Body)
	if err != nil {
		resp.Body.Close()
		stopCancel()
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return devinErrorResponse(req, devinBad("missing or truncated first Connect frame")), nil
	}
	if flag&2 != 0 {
		if fail := devinTrailer(first); fail != nil {
			resp.Body.Close()
			stopCancel()
			return devinErrorResponse(req, fail), nil
		}
	}
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	state := &devinOutput{}
	if !chat.Stream {
		defer resp.Body.Close()
		defer stopCancel()
		failure := devinConsume(req.Context(), resp.Body, state, flag, first, nil)
		if failure != nil {
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			return devinErrorResponse(req, devinAsFailure(failure)), nil
		}
		reason, e := state.finish()
		if e != nil {
			return devinErrorResponse(req, devinAsFailure(e)), nil
		}
		result := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": chat.Model, "choices": []any{map[string]any{"index": 0, "message": state.message(), "finish_reason": reason}}}
		if usage := state.usage(); usage != nil {
			result["usage"] = usage
		}
		return devinJSONResponse(req, 200, result), nil
	}
	reader, writer := io.Pipe()
	stopPipe := context.AfterFunc(req.Context(), func() { writer.CloseWithError(req.Context().Err()); resp.Body.Close() })
	go func() {
		defer stopCancel()
		defer stopPipe()
		defer resp.Body.Close()
		defer func() {
			if err := req.Context().Err(); err != nil {
				_ = writer.CloseWithError(err)
			} else {
				_ = writer.Close()
			}
		}()
		send := func(v any) error {
			b, e := json.Marshal(v)
			if e != nil {
				return e
			}
			_, e = fmt.Fprintf(writer, "data: %s\n\n", b)
			return e
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": chat.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		emitError := func(e error) {
			f := devinAsFailure(e)
			_ = send(map[string]any{"error": map[string]any{"message": f.message, "type": "upstream_error", "code": f.code, "status": f.status}})
		}
		// No synthetic role/content event before validating the first frame.
		failure := devinConsume(req.Context(), resp.Body, state, flag, first, func(d map[string]any) error {
			if len(d) == 0 {
				return nil
			}
			return send(chunk(d, nil))
		})
		if failure != nil {
			if req.Context().Err() == nil {
				emitError(failure)
			}
			return
		}
		reason, e := state.finish()
		if e != nil {
			emitError(e)
			return
		}
		if len(state.signature) > 0 {
			if send(chunk(map[string]any{"devin_signature": base64.StdEncoding.EncodeToString(state.signature), "devin_signature_type": state.signatureType}, nil)) != nil {
				return
			}
		}
		if send(chunk(map[string]any{}, reason)) != nil {
			return
		}
		if chat.StreamOptions != nil && chat.StreamOptions.IncludeUsage {
			if usage := state.usage(); usage != nil {
				if send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": chat.Model, "choices": []any{}, "usage": usage}) != nil {
					return
				}
			}
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}()
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &devinStreamBody{PipeReader: reader, upstream: resp.Body}, ContentLength: -1, Request: req}, nil
}

type devinStreamBody struct {
	*io.PipeReader
	upstream io.Closer
}

func (b *devinStreamBody) Close() error { e := b.PipeReader.Close(); b.upstream.Close(); return e }
func devinConsume(ctx context.Context, r io.Reader, state *devinOutput, flag byte, b []byte, emit func(map[string]any) error) error {
	total, frames := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frames++
		total += len(b)
		if total > devinMaxTotal || frames > 65536 {
			return devinBad("response limit exceeded")
		}
		if flag&2 != 0 {
			if f := devinTrailer(b); f != nil {
				return f
			}
			return nil
		}
		d, err := state.frame(b)
		if err != nil {
			return err
		}
		if emit != nil {
			if err := emit(d); err != nil {
				return err
			}
		}
		flag, b, err = devinReadFrame(r)
		if err != nil {
			return devinBad("Connect stream ended before valid terminal trailer")
		}
	}
}
func devinAsFailure(e error) *devinFailure {
	var f *devinFailure
	if errors.As(e, &f) {
		return f
	}
	return devinBad("malformed native response")
}
func devinErrorResponse(req *http.Request, f *devinFailure) *http.Response {
	return devinJSONResponse(req, f.status, map[string]any{"error": map[string]any{"message": f.message, "type": "upstream_error", "code": f.code}})
}
func devinJSONResponse(req *http.Request, status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b)), Request: req}
}
