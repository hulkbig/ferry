/*
Native protocol integration based on CLIProxyAPI d946c4f, internal/runtime/executor/helps/devin_wire.go.
Source: https://github.com/router-for-me/CLIProxyAPI/tree/d946c4f
Narrow adaptation; no upstream SDK/runtime dependency.

# MIT License

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
package nativewire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The caller must supply Ferry-owned, account-bound metadata from Token.Extra. These internal-only
// headers are consumed here and never forwarded as HTTP headers. They must NOT
// be copied from untrusted ingress headers. The driver generates its own stable random device ID; this transport never
// invents another application identity or rotates device IDs per request.
const (
	DevinClientNameHeader    = "X-Smartapi-Devin-Client-Name"
	DevinClientVersionHeader = "X-Smartapi-Devin-Client-Version"
	DevinClientOSHeader      = "X-Smartapi-Devin-Client-OS"
	DevinFingerprintHeader   = "X-Smartapi-Devin-Device-Fingerprint"
)

type devinFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}
type devinTool struct {
	Type     string        `json:"type"`
	Function devinFunction `json:"function"`
}
type devinCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type devinMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []devinCall     `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}
type devinChat struct {
	Model         string         `json:"model"`
	Messages      []devinMessage `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Tools               []devinTool     `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	N                   *int            `json:"n,omitempty"`
}

func devinStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}
func devinText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := devinStrict(raw, &parts); err != nil {
		return "", errors.New("devin: only text content is representable")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", errors.New("devin: unsupported content part")
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}
func devinMetadata(h http.Header, token string) ([]byte, error) {
	name, version, osName, fp := h.Get(DevinClientNameHeader), h.Get(DevinClientVersionHeader), h.Get(DevinClientOSHeader), h.Get(DevinFingerprintHeader)
	if name != "ferry" && name != "smart-api" {
		return nil, errors.New("devin: client identity must be ferry")
	}
	for _, s := range []string{name, version, osName} {
		if s == "" || len(s) > 256 || strings.ContainsAny(s, "\r\n\x00") {
			return nil, errors.New("devin: Ferry client name, version, OS and persisted device identifier required")
		}
	}
	if len(fp) != 732 {
		return nil, errors.New("devin: persisted Ferry 732-hex device identifier required (generated once by DevinDriver)")
	}
	if _, err := hex.DecodeString(fp); err != nil {
		return nil, errors.New("devin: invalid device fingerprint")
	}
	var b []byte
	for _, x := range []struct {
		n int
		s string
	}{{1, name}, {2, version}, {3, token}, {4, "en"}, {5, osName}, {7, version}, {12, name}, {31, fp}} {
		b = devinString(b, x.n, x.s)
	}
	return b, nil
}

// ValidateDevinChat performs the same structural and representability checks
// as the transport, without account metadata, secrets, randomness or network.
// Egress Preparer must call this after constructing its final Chat payload.
// It does not prove model entitlement or provider acceptance of client identity.
func ValidateDevinChat(payload []byte) error {
	_, _, err := devinMapChat(payload, false)
	return err
}

func devinBuild(req *http.Request, token string) (devinChat, []byte, error) {
	var chat devinChat
	if req.Body == nil {
		return chat, nil, errors.New("devin: request body required")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, devinMaxFrame+1))
	if err != nil {
		return chat, nil, err
	}
	chat, body, err := devinMapChat(raw, true)
	if err != nil {
		return chat, nil, err
	}
	meta, err := devinMetadata(req.Header, token)
	if err != nil {
		return chat, nil, err
	}
	body = append(devinBytes(nil, 1, meta), body...)
	if len(body) > devinMaxFrame {
		return chat, nil, errors.New("devin: encoded request exceeds limit")
	}
	return chat, devinEnvelope(0, body), nil
}

// identifiers is false in validation: no UUIDs or device metadata are generated.
func devinMapChat(raw []byte, identifiers bool) (devinChat, []byte, error) {
	var chat devinChat
	requestID := func() string {
		if identifiers {
			return uuid.NewString()
		}
		return ""
	}
	if len(raw) > devinMaxFrame {
		return chat, nil, errors.New("devin: request exceeds limit")
	}
	if !utf8.Valid(raw) {
		return chat, nil, errors.New("devin: invalid UTF-8 request")
	}
	if err := devinStrict(raw, &chat); err != nil {
		return chat, nil, fmt.Errorf("devin: unrepresentable Chat request: %w", err)
	}
	if chat.Model == "" || len(chat.Model) > 256 || strings.ContainsAny(chat.Model, "\r\n\x00") || len(chat.Messages) == 0 {
		return chat, nil, errors.New("devin: exact catalog model UID and messages required")
	}
	// Model is an exact upstream UID supplied by the catalog/integration; do not
	// infer private aliases or reasoning variants from a marketing model name.
	if chat.N != nil && *chat.N != 1 {
		return chat, nil, errors.New("devin: n must be 1")
	}
	if len(chat.ToolChoice) > 0 && string(chat.ToolChoice) != "null" && string(chat.ToolChoice) != `"auto"` {
		return chat, nil, errors.New("devin: only automatic tool choice is representable")
	}
	if len(chat.Tools) > 128 {
		return chat, nil, errors.New("devin: tool limit exceeded")
	}
	maxTokens := 128000
	if chat.MaxTokens != nil {
		maxTokens = *chat.MaxTokens
	}
	if chat.MaxCompletionTokens != nil {
		if chat.MaxTokens != nil && *chat.MaxTokens != *chat.MaxCompletionTokens {
			return chat, nil, errors.New("devin: conflicting token limits")
		}
		maxTokens = *chat.MaxCompletionTokens
	}
	if maxTokens < 1 || maxTokens > 1<<24 {
		return chat, nil, errors.New("devin: invalid token limit")
	}
	temp := 1.0
	if chat.Temperature != nil {
		temp = *chat.Temperature
	}
	topP := float64(float32(.95))
	if chat.TopP != nil {
		topP = *chat.TopP
	}
	if temp < 0 || temp > 2 || topP <= 0 || topP > 1 {
		return chat, nil, errors.New("devin: invalid sampling parameters")
	}
	var b []byte
	var system []string
	started := false
	pending := map[string]bool{}
	seen := map[string]bool{}
	lastSource := 0
	for _, m := range chat.Messages {
		text, err := devinText(m.Content)
		if err != nil {
			return chat, nil, err
		}
		if m.Role == "system" || m.Role == "developer" {
			if started || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
				return chat, nil, errors.New("devin: only leading plain system/developer messages supported")
			}
			system = append(system, text)
			continue
		}
		started = true
		source := 0
		switch m.Role {
		case "user":
			source = 1
		case "assistant":
			source = 2
		case "tool":
			source = 4
		default:
			return chat, nil, errors.New("devin: unsupported role")
		}
		if len(pending) > 0 && source != 4 {
			return chat, nil, errors.New("devin: unresolved tool calls before next turn")
		}
		if source != 2 && len(m.ToolCalls) > 0 {
			return chat, nil, errors.New("devin: tool calls require assistant role")
		}
		if source != 4 && m.ToolCallID != "" {
			return chat, nil, errors.New("devin: tool_call_id requires tool role")
		}
		if source == 4 {
			if !pending[m.ToolCallID] {
				return chat, nil, errors.New("devin: orphan or duplicate tool result")
			}
			delete(pending, m.ToolCallID)
		}
		if source == 1 && text == "" {
			return chat, nil, errors.New("devin: empty user turn")
		}
		if len(m.ToolCalls) > 128 {
			return chat, nil, errors.New("devin: too many tool calls")
		}
		p := devinString(nil, 1, requestID())
		p = devinInt(p, 2, uint64(source))
		p = devinString(p, 3, text)
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || tc.Type != "function" || tc.Function.Name == "" || seen[tc.ID] || !devinJSONObject([]byte(tc.Function.Arguments)) {
				return chat, nil, errors.New("devin: invalid function call")
			}
			seen[tc.ID] = true
			pending[tc.ID] = true
			c := devinString(nil, 1, tc.ID)
			c = devinString(c, 2, tc.Function.Name)
			c = devinString(c, 3, tc.Function.Arguments)
			p = devinBytes(p, 6, c)
		}
		if source == 4 {
			p = devinString(p, 7, m.ToolCallID)
		}
		b = devinBytes(b, 3, p)
		lastSource = source
	}
	if !started || len(pending) > 0 || lastSource == 2 {
		return chat, nil, errors.New("devin: request must end in a user or completed tool-result turn")
	}
	if len(system) > 0 {
		b = devinString(b, 2, strings.Join(system, "\n\n"))
	}
	b = devinInt(b, 7, 5)
	config := devinInt(nil, 1, 1)
	config = devinInt(config, 2, uint64(maxTokens))
	config = devinInt(config, 3, 400)
	config = devinDouble(config, 5, math.Float64bits(temp))
	config = devinInt(config, 7, 40)
	config = devinDouble(config, 8, math.Float64bits(topP))
	b = devinBytes(b, 8, config)
	names := map[string]bool{}
	for _, t := range chat.Tools {
		if t.Type != "function" || t.Function.Name == "" || names[t.Function.Name] || t.Function.Strict != nil && *t.Function.Strict {
			return chat, nil, errors.New("devin: unsupported or duplicate tool declaration")
		}
		names[t.Function.Name] = true
		params := t.Function.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schema map[string]any
		if json.Unmarshal(params, &schema) != nil || schema == nil {
			return chat, nil, errors.New("devin: tool parameters must be an object")
		}
		tbytes := devinString(nil, 1, t.Function.Name)
		tbytes = devinString(tbytes, 2, t.Function.Description)
		tbytes = devinBytes(tbytes, 3, params)
		b = devinBytes(b, 10, tbytes)
	}
	// Chat requests carry complete history. Each invocation is an independent
	// provider session; no undocumented continuation/cache identity is invented.
	session := requestID()
	s := devinString(nil, 1, session)
	s = devinInt(s, 3, 4)
	if lastSource == 1 {
		s = devinInt(s, 4, 14)
	}
	b = devinBytes(b, 15, s)
	b = devinString(b, 16, session)
	b = devinInt(b, 20, 1)
	b = devinString(b, 21, chat.Model)
	if len(b) > devinMaxFrame {
		return chat, nil, errors.New("devin: encoded request exceeds limit")
	}
	return chat, b, nil
}

func devinJSONObject(b []byte) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(b, &obj) == nil && obj != nil
}
