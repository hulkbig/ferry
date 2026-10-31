package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMockScenarios(t *testing.T) {
	m := &Mock{cfg: Scenario{Bytes: 1024, Chunks: 16}}
	s := httptest.NewServer(m)
	defer s.Close()
	for _, stream := range []bool{false, true} {
		body := `{"model":"gpt-4o-mini","stream":false}`
		if stream {
			body = `{"model":"gpt-4o-mini","stream":true}`
		}
		r, e := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatal(r.Status)
		}
		if stream {
			if !strings.Contains(string(b), "[DONE]") || strings.Count(string(b), strings.Repeat("x", 64)) != 16 {
				t.Fatal("SSE payload mismatch")
			}
		} else {
			var v map[string]any
			if json.Unmarshal(b, &v) != nil {
				t.Fatal("bad JSON")
			}
			content := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
			if len(content) != 1024 {
				t.Fatal(len(content))
			}
		}
	}
	m.mu.Lock()
	m.cfg = Scenario{Bytes: 32, Chunks: 1, Status: 429}
	m.mu.Unlock()
	r, _ := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	r.Body.Close()
	if r.StatusCode != 429 {
		t.Fatal(r.Status)
	}
	m.mu.Lock()
	m.cfg = Scenario{Bytes: 32, Chunks: 1, DelayMS: 500}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	start := time.Now()
	_, e := http.DefaultClient.Do(req)
	if e == nil || time.Since(start) > 300*time.Millisecond {
		t.Fatal("cancel not prompt")
	}
	deadline := time.Now().Add(time.Second)
	for m.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.active.Load() != 0 || m.cancelled.Load() != 1 {
		t.Fatal("cancel not observed")
	}
}
