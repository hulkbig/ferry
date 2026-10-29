package nativewire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var agBadResponse = errors.New("unsupported or incomplete Antigravity response")

type agUsage struct {
	Prompt   int64 `json:"promptTokenCount"`
	Output   int64 `json:"candidatesTokenCount"`
	Thoughts int64 `json:"thoughtsTokenCount"`
	Total    int64 `json:"totalTokenCount"`
	Cached   int64 `json:"cachedContentTokenCount"`
}

func (u agUsage) chat() map[string]any {
	total := u.Total
	if total == 0 {
		total = u.Prompt + u.Output + u.Thoughts
	}
	return map[string]any{"prompt_tokens": u.Prompt, "completion_tokens": u.Output + u.Thoughts, "total_tokens": total, "prompt_tokens_details": map[string]any{"cached_tokens": u.Cached}, "completion_tokens_details": map[string]any{"reasoning_tokens": u.Thoughts}}
}

type agResponseState struct {
	model, id, finish string
	usage             agUsage
	toolIDs           map[string]bool
	toolIndex         int
	started           bool
}

func (s *agResponseState) chunk(choices []any, stream bool) map[string]any {
	object := "chat.completion"
	if stream {
		object += ".chunk"
	}
	return map[string]any{"id": s.id, "object": object, "created": time.Now().Unix(), "model": s.model, "choices": choices}
}
func (s *agResponseState) consume(b []byte) (map[string]any, error) {
	var envelope struct {
		Response json.RawMessage `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &envelope) != nil || len(envelope.Error) > 0 && string(envelope.Error) != "null" || len(envelope.Response) == 0 {
		return nil, agBadResponse
	}
	var v struct {
		Candidates []struct {
			Index   int `json:"index"`
			Content struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
			Finish string `json:"finishReason"`
		} `json:"candidates"`
		Usage          *agUsage `json:"usageMetadata"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(envelope.Response, &v) != nil || len(v.Error) > 0 && string(v.Error) != "null" || len(v.Candidates) > 1 {
		return nil, agBadResponse
	}
	if v.Usage != nil {
		u := *v.Usage
		if u.Prompt < 0 || u.Output < 0 || u.Thoughts < 0 || u.Total < 0 || u.Cached < 0 {
			return nil, agBadResponse
		}
		// Wrapped streaming counts are cumulative; later usage-only frames may omit fields.
		s.usage.Prompt = max(s.usage.Prompt, u.Prompt)
		s.usage.Output = max(s.usage.Output, u.Output)
		s.usage.Thoughts = max(s.usage.Thoughts, u.Thoughts)
		s.usage.Total = max(s.usage.Total, u.Total)
		s.usage.Cached = max(s.usage.Cached, u.Cached)
	}
	delta := map[string]any{}
	if v.PromptFeedback.BlockReason != "" {
		s.finish = "content_filter"
		return delta, nil
	}
	if len(v.Candidates) == 0 {
		if v.Usage == nil {
			return nil, agBadResponse
		}
		return delta, nil
	}
	c := v.Candidates[0]
	if c.Index != 0 {
		return nil, agBadResponse
	}
	text, reasoning := "", ""
	calls := []any{}
	for _, raw := range c.Content.Parts {
		var part struct {
			Text      *string `json:"text"`
			Thought   bool    `json:"thought"`
			Signature string  `json:"thoughtSignature"`
			Call      *struct {
				ID   string         `json:"id"`
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			} `json:"functionCall"`
		}
		if agDecode(raw, &part) != nil || part.Signature != "" || part.Text != nil && part.Call != nil {
			return nil, agBadResponse
		}
		if part.Text != nil {
			if part.Thought {
				reasoning += *part.Text
			} else {
				text += *part.Text
			}
			continue
		}
		if part.Call == nil || part.Thought || !agName.MatchString(part.Call.Name) || part.Call.Args == nil {
			return nil, agBadResponse
		}
		id := part.Call.ID
		if id == "" {
			id = "call_" + agID()
		}
		if !agModel.MatchString(id) || s.toolIDs[id] {
			return nil, agBadResponse
		}
		s.toolIDs[id] = true
		args, _ := json.Marshal(part.Call.Args)
		calls = append(calls, map[string]any{"index": s.toolIndex, "id": id, "type": "function", "function": map[string]any{"name": part.Call.Name, "arguments": string(args)}})
		s.toolIndex++
	}
	if s.finish != "" && (text != "" || reasoning != "" || len(calls) > 0) {
		return nil, agBadResponse
	}
	if text != "" {
		delta["content"] = text
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if len(calls) > 0 {
		delta["tool_calls"] = calls
	}
	if c.Finish != "" {
		previousFinish := s.finish
		switch c.Finish {
		case "STOP":
			s.finish = "stop"
			if s.toolIndex > 0 {
				s.finish = "tool_calls"
			}
		case "MAX_TOKENS":
			s.finish = "length"
		case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
			s.finish = "content_filter"
		default:
			return nil, agBadResponse
		}
		if previousFinish != "" && previousFinish != s.finish {
			return nil, agBadResponse
		}
	}
	return delta, nil
}

// Closing the translated body also closes the upstream stream, unblocking a
// scanner even when the caller stops reading before cancellation.
type agStreamBody struct {
	*io.PipeReader
	upstream io.ReadCloser
	once     sync.Once
}

func (b *agStreamBody) Close() error {
	b.once.Do(func() { b.upstream.Close() })
	return b.PipeReader.Close()
}
func agStreamResponse(r *http.Request, upstream *http.Response, state *agResponseState, includeUsage bool) *http.Response {
	reader, writer := io.Pipe()
	body := &agStreamBody{PipeReader: reader, upstream: upstream.Body}
	done := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			body.Close()
		case <-done:
		}
	}()
	go func() {
		defer close(done)
		defer writer.Close()
		defer body.once.Do(func() { upstream.Body.Close() })
		emit := func(v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(writer, "data: %s\n\n", b)
			return err
		}
		fail := func() {
			_ = emit(map[string]any{"error": map[string]string{"type": "antigravity_error", "message": agBadResponse.Error()}})
		}
		scanner := bufio.NewScanner(upstream.Body)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		data := []string{}
		size := 0
		dispatch := func() error {
			if len(data) == 0 {
				return nil
			}
			raw := strings.Join(data, "\n")
			data = nil
			size = 0
			if raw == "[DONE]" {
				if state.finish == "" {
					return agBadResponse
				}
				return nil
			}
			delta, err := state.consume([]byte(raw))
			if err != nil {
				return err
			}
			if len(delta) > 0 {
				if !state.started {
					delta["role"] = "assistant"
					state.started = true
				}
				return emit(state.chunk([]any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}, true))
			}
			return nil
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if dispatch() != nil {
					fail()
					return
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				line = strings.TrimPrefix(line, "data:")
				line = strings.TrimPrefix(line, " ")
				size += len(line)
				if size > 2<<20 {
					fail()
					return
				}
				data = append(data, line)
			}
		}
		if scanner.Err() != nil || dispatch() != nil || state.finish == "" {
			fail()
			return
		}
		terminal := state.chunk([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": state.finish}}, true)
		if emit(terminal) != nil {
			return
		}
		if includeUsage {
			usage := state.chunk([]any{}, true)
			usage["usage"] = state.usage.chat()
			if emit(usage) != nil {
				return
			}
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}()
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, ContentLength: -1, Request: r}
}
