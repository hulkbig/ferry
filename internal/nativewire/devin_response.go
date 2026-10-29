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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

type devinFailure struct {
	status        int
	code, message string
}

func (e *devinFailure) Error() string { return "devin: " + e.code + ": " + e.message }
func devinBad(message string) *devinFailure {
	return &devinFailure{502, "invalid_upstream_response", message}
}
func devinTrailer(b []byte) *devinFailure {
	var t struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Metadata map[string][]string `json:"metadata"`
	}
	if len(b) == 0 || json.Unmarshal(b, &t) != nil || strings.TrimSpace(string(b)) == "null" {
		return devinBad("malformed Connect trailer")
	}
	if t.Error == nil {
		return nil
	}
	status := 502
	switch t.Error.Code {
	case "invalid_argument", "out_of_range", "failed_precondition":
		status = 400
	case "unauthenticated":
		status = 401
	case "permission_denied":
		status = 403
	case "resource_exhausted":
		status = 429
	case "unavailable":
		status = 503
	case "deadline_exceeded":
		status = 504
	case "canceled":
		status = 499
	}
	if t.Error.Code == "failed_precondition" {
		s := strings.ToLower(t.Error.Message)
		for _, v := range []string{"quota", "credit", "acu", "exhaust", "limit"} {
			if strings.Contains(s, v) {
				status = 429
				break
			}
		}
	}
	return &devinFailure{status, t.Error.Code, t.Error.Message}
}

type devinToolState struct{ id, name, args string }
type devinOutput struct {
	text, thinking               strings.Builder
	textPending, thinkingPending []byte
	tools                        []devinToolState
	byID                         map[string]int
	stop                         uint64
	counts                       map[int]uint64
	dimensions                   map[int]uint64
	signature                    []byte
	signatureType                string
}

func devinUTF8(pending *[]byte, b []byte) (string, error) {
	*pending = append(*pending, b...)
	n := 0
	for n < len(*pending) {
		if !utf8.FullRune((*pending)[n:]) {
			break
		}
		r, k := utf8.DecodeRune((*pending)[n:])
		if r == utf8.RuneError && k == 1 {
			return "", errors.New("invalid UTF-8 upstream text")
		}
		n += k
	}
	s := string((*pending)[:n])
	*pending = append((*pending)[:0], (*pending)[n:]...)
	return s, nil
}
func (o *devinOutput) frame(b []byte) (map[string]any, error) {
	fs, err := devinFields(b)
	if err != nil {
		return nil, err
	}
	delta := map[string]any{}
	for _, f := range fs {
		switch f.num {
		case 3, 6, 7, 9, 10, 21, 28:
			if f.typ != 2 {
				return nil, devinBad("wrong protobuf type for response field")
			}
		case 5:
			if f.typ != 0 {
				return nil, devinBad("wrong protobuf type for stop reason")
			}
		}
		switch {
		case f.num == 3 && f.typ == 2:
			s, e := devinUTF8(&o.textPending, f.b)
			if e != nil {
				return nil, e
			}
			o.text.WriteString(s)
			if s != "" {
				old, _ := delta["content"].(string)
				delta["content"] = old + s
			}
		case f.num == 9 && f.typ == 2:
			s, e := devinUTF8(&o.thinkingPending, f.b)
			if e != nil {
				return nil, e
			}
			o.thinking.WriteString(s)
			if s != "" {
				old, _ := delta["reasoning_content"].(string)
				delta["reasoning_content"] = old + s
			}
		case f.num == 5 && f.typ == 0:
			o.stop = f.n
		case f.num == 6 && f.typ == 2:
			td, e := o.tool(f.b)
			if e != nil {
				return nil, e
			}
			list, _ := delta["tool_calls"].([]any)
			delta["tool_calls"] = append(list, td)
		case f.num == 7 && f.typ == 2:
			if e := o.usageField(f.b); e != nil {
				return nil, e
			}
		case f.num == 28 && f.typ == 2:
			if e := o.dimension(f.b); e != nil {
				return nil, e
			}
		case f.num == 10 && f.typ == 2:
			o.signature = append(o.signature, f.b...)
		case f.num == 21 && f.typ == 2:
			o.signatureType = string(f.b)
		}
	}
	return delta, nil
}
func (o *devinOutput) tool(b []byte) (map[string]any, error) {
	fs, err := devinFields(b)
	if err != nil {
		return nil, err
	}
	var id, name, args string
	for _, f := range fs {
		switch {
		case f.num == 1 && f.typ == 2:
			id = string(f.b)
		case f.num == 2 && f.typ == 2:
			name = string(f.b)
		case f.num == 3 && f.typ == 2:
			args += string(f.b)
		case f.num == 4 && f.typ == 2:
			if args == "" {
				args = string(f.b)
			}
		case f.num == 5 && f.typ == 2:
			if len(f.b) > 0 {
				return nil, devinBad("upstream reported invalid tool JSON")
			}
		case f.num == 6 && f.typ == 0:
			if f.n != 0 {
				return nil, devinBad("custom tool calls are not Chat functions")
			}
		}
	}
	if !utf8.ValidString(id + name + args) {
		return nil, devinBad("invalid UTF-8 tool delta")
	}
	if o.byID == nil {
		o.byID = map[string]int{}
	}
	idx, found := o.byID[id]
	if id == "" && len(o.tools) > 0 {
		idx = len(o.tools) - 1
		found = true
	}
	fresh := !found
	if fresh {
		if id == "" || name == "" || len(o.tools) >= 128 {
			return nil, devinBad("tool delta lacks identity or exceeds limit")
		}
		idx = len(o.tools)
		o.tools = append(o.tools, devinToolState{id: id, name: name})
		o.byID[id] = idx
	}
	t := &o.tools[idx]
	if name != "" && name != t.name {
		return nil, devinBad("tool identity changed")
	}
	t.args += args
	fn := map[string]any{"arguments": args}
	d := map[string]any{"index": idx, "function": fn}
	if fresh {
		d["id"] = t.id
		d["type"] = "function"
		fn["name"] = t.name
	}
	return d, nil
}
func (o *devinOutput) usageField(b []byte) error {
	fs, e := devinFields(b)
	if e != nil {
		return e
	}
	if o.counts == nil {
		o.counts = map[int]uint64{}
	}
	for _, f := range fs {
		if f.typ == 0 && f.num >= 2 && f.num <= 5 {
			if f.n > 1<<40 {
				return devinBad("usage exceeds bound")
			}
			o.counts[f.num] = f.n
		}
		if f.typ == 0 && f.num == 6 {
			if f.n < 400 || f.n > 599 {
				return devinBad("invalid upstream error status")
			}
			return &devinFailure{int(f.n), "upstream_status", "upstream usage carries error status"}
		}
	}
	return nil
}
func (o *devinOutput) dimension(b []byte) error {
	fs, e := devinFields(b)
	if e != nil {
		return e
	}
	title := ""
	for _, f := range fs {
		if f.num == 1 && f.typ == 2 {
			title = string(f.b)
		}
	}
	if !strings.EqualFold(title, "Token Usage") {
		return nil
	}
	if o.dimensions == nil {
		o.dimensions = map[int]uint64{}
	}
	for _, f := range fs {
		if f.num != 2 || f.typ != 2 {
			continue
		}
		items, e := devinFields(f.b)
		if e != nil {
			return e
		}
		key := ""
		var v float64
		has := false
		for _, it := range items {
			if it.num == 5 && it.typ == 2 {
				key = string(it.b)
			}
			if it.num == 4 && it.typ == 2 {
				ds, e := devinFields(it.b)
				if e != nil {
					return e
				}
				for _, d := range ds {
					if d.num == 2 && d.typ == 5 {
						if d.n > math.MaxUint32 {
							return devinBad("dimension fixed32 value exceeds bound")
						}
						v = float64(math.Float32frombits(uint32(d.n)))
						has = true
					}
				}
			}
		}
		n := 0
		switch key {
		case "input_tokens":
			n = 2
		case "output_tokens":
			n = 3
		case "cached_input_tokens":
			n = 5
		}
		if n > 0 && has {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1<<40 || math.Trunc(v) != v {
				return devinBad("invalid dimension token count")
			}
			o.dimensions[n] = uint64(v)
		}
	}
	return nil
}
func (o *devinOutput) usage() map[string]any {
	if len(o.counts) == 0 && len(o.dimensions) == 0 {
		return nil
	}
	value := func(n int) uint64 {
		if v, ok := o.counts[n]; ok {
			return v
		}
		return o.dimensions[n]
	}
	cached, write := value(5), value(4)
	input, output := value(2)+cached+write, value(3)
	return map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output, "prompt_tokens_details": map[string]any{"cached_tokens": cached, "cache_creation_tokens": write}}
}
func (o *devinOutput) finish() (string, error) {
	if len(o.textPending)+len(o.thinkingPending) > 0 {
		return "", devinBad("truncated UTF-8 at end of stream")
	}
	for _, t := range o.tools {
		if !devinJSONObject([]byte(t.args)) {
			return "", devinBad("incomplete function arguments")
		}
	}
	switch o.stop {
	case 1, 3:
		return "length", nil
	case 11:
		return "content_filter", nil
	case 0, 2:
	default:
		return "", devinBad(fmt.Sprintf("unknown stop reason %d", o.stop))
	}
	if len(o.tools) > 0 {
		return "tool_calls", nil
	}
	return "stop", nil
}
func (o *devinOutput) message() map[string]any {
	m := map[string]any{"role": "assistant", "content": o.text.String()}
	if o.thinking.Len() > 0 {
		m["reasoning_content"] = o.thinking.String()
	}
	if len(o.tools) > 0 {
		ts := make([]any, 0, len(o.tools))
		for _, t := range o.tools {
			ts = append(ts, map[string]any{"id": t.id, "type": "function", "function": map[string]any{"name": t.name, "arguments": t.args}})
		}
		m["tool_calls"] = ts
	}
	// Provider signatures are not plain Chat reasoning. Preserve as a namespaced
	// extension; replaying them requires a separate explicit input contract.
	if len(o.signature) > 0 {
		m["devin_signature"] = base64.StdEncoding.EncodeToString(o.signature)
		m["devin_signature_type"] = o.signatureType
	}
	return m
}
