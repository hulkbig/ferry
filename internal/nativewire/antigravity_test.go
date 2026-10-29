package nativewire

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type agFixtureRT func(*http.Request) (*http.Response, error)

func (f agFixtureRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func agFixtureResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func agFixtureRequest(body string) *http.Request {
	r, _ := http.NewRequest("POST", agEndpoint+"/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer fixture-access")
	return r
}

const agBasic = `{"model":"fixture-model","messages":[{"role":"user","content":"hello"}]}`
const agAnswer = `{"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":2,"totalTokenCount":10,"cachedContentTokenCount":1}}}`

func TestAntigravityRequestAndNonStream(t *testing.T) {
	transport := NewAntigravityTransport(agFixtureRT(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != agEndpoint+"/v1internal:generateContent" || r.Header.Get("Authorization") != "Bearer fixture-access" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Fatal("unsafe upstream request")
		}
		var envelope map[string]any
		if json.NewDecoder(r.Body).Decode(&envelope) != nil {
			t.Fatal("invalid body")
		}
		if envelope["project"] != "project-one" || envelope["model"] != "fixture-model" || envelope["requestType"] != "agent" {
			t.Fatal(envelope)
		}
		req := envelope["request"].(map[string]any)
		if req["sessionId"] == "" || len(req["contents"].([]any)) != 1 {
			t.Fatal(req)
		}
		return agFixtureResponse(agAnswer), nil
	}), "project-one")
	r := agFixtureRequest(agBasic)
	r.Header.Set("Cookie", "private")
	r.Header.Set("X-Api-Key", "private")
	resp, err := transport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if json.NewDecoder(resp.Body).Decode(&result) != nil || resp.StatusCode != 200 {
		t.Fatal(result)
	}
	choice := result["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "hello" || choice["finish_reason"] != "stop" {
		t.Fatal(choice)
	}
	usage := result["usage"].(map[string]any)
	if usage["completion_tokens"] != float64(5) || usage["total_tokens"] != float64(10) {
		t.Fatal(usage)
	}
}
func TestAntigravityToolsAndResults(t *testing.T) {
	raw := `{"model":"fixture-model","messages":[{"role":"system","content":"rules"},{"role":"user","content":[{"type":"text","text":"weather?"}]},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","description":"forecast","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"tool_choice":{"type":"function","function":{"name":"weather"}},"max_tokens":100,"temperature":0.5,"top_p":0.9,"stop":["END"]}`
	_, body, err := agEncodeRequest([]byte(raw), "project-one")
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	json.Unmarshal(body, &wire)
	r := wire["request"].(map[string]any)
	turns := r["contents"].([]any)
	result := turns[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if result["name"] != "weather" || result["id"] != "call_1" || result["response"].(map[string]any)["result"] != "sunny" {
		t.Fatal(result)
	}
	if r["tools"] == nil || r["systemInstruction"] == nil || r["generationConfig"] == nil || r["toolConfig"] == nil {
		t.Fatal(r)
	}
	transport := NewAntigravityTransport(agFixtureRT(func(*http.Request) (*http.Response, error) {
		return agFixtureResponse(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}]}}`), nil
	}), "project-one")
	resp, err := transport.RoundTrip(agFixtureRequest(agBasic))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"finish_reason":"tool_calls"`) || !strings.Contains(string(b), `\"city\":\"Paris\"`) {
		t.Fatal(string(b))
	}
}
func TestAntigravityRejectsBeforeUpstream(t *testing.T) {
	calls := 0
	base := agFixtureRT(func(*http.Request) (*http.Response, error) { calls++; return agFixtureResponse(agAnswer), nil })
	for _, raw := range []string{
		`{"model":"../model","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"assistant","reasoning_content":"private","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid"}}]}]}`,
		`{"model":"m","messages":[{"role":"tool","tool_call_id":"unknown","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"},"thought_signature":"sig"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"previous_response_id":"id"}`,
		agBasic + ` {}`, strings.Replace(agBasic, `"model"`, `"n":2,"model"`, 1),
	} {
		resp, err := NewAntigravityTransport(base, "project-one").RoundTrip(agFixtureRequest(raw))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, target := range []string{"https://evil.test/chat/completions", agEndpoint + "/chat/completions?key=x", agEndpoint + "/other", agEndpoint + "/%63hat/completions", agEndpoint + "/chat/completions#frag"} {
		r := agFixtureRequest(agBasic)
		u, _ := http.NewRequest("POST", target, nil)
		r.URL = u.URL
		if _, err := NewAntigravityTransport(base, "project-one").RoundTrip(r); err == nil {
			t.Fatal("escaped endpoint")
		}
	}
	if _, err := NewAntigravityTransport(base, "../project").RoundTrip(agFixtureRequest(agBasic)); err == nil {
		t.Fatal("bad project")
	}
	if calls != 0 {
		t.Fatal("rejected request reached network")
	}
}
func TestAntigravityStreamUsageAndErrors(t *testing.T) {
	text := `{"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}}`
	end := `{"response":{"candidates":[{"finishReason":"STOP"}]}}`
	usage := `{"response":{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":2,"totalTokenCount":10}}}`
	for _, tc := range []struct {
		name, stream string
		success      bool
	}{
		{"usage-tail", "data: " + text + "\n\ndata: " + end + "\n\ndata: " + usage + "\n\n", true},
		{"truncated", "data: " + text + "\n\n", false},
		{"midstream-error", "data: " + text + "\n\ndata: {\"error\":{\"message\":\"private-upstream\"}}\n\n", false},
		{"signature", "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\",\"thoughtSignature\":\"private-signature\"}]},\"finishReason\":\"STOP\"}]}}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := NewAntigravityTransport(agFixtureRT(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
					t.Fatal(r.URL)
				}
				return agFixtureResponse(tc.stream), nil
			}), "project-one")
			resp, err := transport.RoundTrip(agFixtureRequest(strings.Replace(agBasic, `"model"`, `"stream":true,"stream_options":{"include_usage":true},"model"`, 1)))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			if strings.Contains(s, "[DONE]") != tc.success || strings.Contains(s, `"error"`) == tc.success || strings.Contains(s, "private-") {
				t.Fatal(s)
			}
			if tc.success && (!strings.Contains(s, `"completion_tokens":5`) || !strings.Contains(s, `"content":"hello"`)) {
				t.Fatal(s)
			}
		})
	}
}
func TestAntigravityHTTPFailuresSanitized(t *testing.T) {
	for _, status := range []int{302, 401, 403, 429, 500} {
		transport := NewAntigravityTransport(agFixtureRT(func(*http.Request) (*http.Response, error) {
			r := agFixtureResponse("private-credential")
			r.StatusCode = status
			r.Header.Set("Retry-After", "10")
			return r, nil
		}), "project-one")
		resp, err := transport.RoundTrip(agFixtureRequest(agBasic))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := status
		if status == 302 {
			want = 502
		}
		if resp.StatusCode != want || strings.Contains(string(b), "private-") || resp.Header.Get("Retry-After") != "10" {
			t.Fatal(resp, string(b))
		}
	}
}

type agBlockingBody struct {
	closed chan struct{}
	count  atomic.Int32
}

func (b *agBlockingBody) Read([]byte) (int, error) { <-b.closed; return 0, io.EOF }
func (b *agBlockingBody) Close() error {
	if b.count.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}
func TestAntigravityStreamCancellationClosesUpstream(t *testing.T) {
	upstream := &agBlockingBody{closed: make(chan struct{})}
	tr := NewAntigravityTransport(agFixtureRT(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: upstream}, nil
	}), "project-one")
	ctx, cancel := context.WithCancel(context.Background())
	r := agFixtureRequest(strings.Replace(agBasic, `"model"`, `"stream":true,"model"`, 1)).WithContext(ctx)
	resp, err := tr.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	defer resp.Body.Close()
	select {
	case <-upstream.closed:
	case <-time.After(time.Second):
		t.Fatal("cancel leaked upstream body")
	}
}

func TestAntigravityStreamToolsAndSplitUsage(t *testing.T) {
	stream := `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"weather","args":{"city":"Paris"}}}]}}]}}

data: {"response":{"candidates":[{"finishReason":"STOP"}]}}

data: {"response":{"candidates":[{"content":{"parts":[{"text":""}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,"totalTokenCount":33}}}

`
	tr := NewAntigravityTransport(agFixtureRT(func(*http.Request) (*http.Response, error) { return agFixtureResponse(stream), nil }), "project-one")
	r := agFixtureRequest(strings.Replace(agBasic, `"model"`, `"stream":true,"stream_options":{"include_usage":true},"model"`, 1))
	resp, err := tr.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) || !strings.Contains(s, `"completion_tokens":22`) || !strings.Contains(s, `"id":"call_1"`) || !strings.Contains(s, "[DONE]") {
		t.Fatal(s)
	}
	if strings.Count(s, `"finish_reason":"tool_calls"`) != 1 {
		t.Fatal("duplicate terminal")
	}
}
func TestAntigravityResponseFailureShapes(t *testing.T) {
	for _, body := range []string{
		`{"response":{"candidates":[{"content":{"parts":[{"text":"x","thoughtSignature":"opaque"}]},"finishReason":"STOP"}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"inlineData":{"data":"xxx"}}]},"finishReason":"STOP"}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"x"}]}]}}`,
		`{"error":{"message":"private"}}`,
	} {
		tr := NewAntigravityTransport(agFixtureRT(func(*http.Request) (*http.Response, error) { return agFixtureResponse(body), nil }), "project-one")
		resp, err := tr.RoundTrip(agFixtureRequest(agBasic))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 502 || strings.Contains(string(b), "opaque") || strings.Contains(string(b), "private") {
			t.Fatal(resp.StatusCode, string(b))
		}
	}
}

func TestValidateAntigravityChatWithoutTransport(t *testing.T) {
	// Call the public validator directly: no transport, credentials, or project
	// configuration is needed to decide compatibility before dispatch.
	if err := ValidateAntigravityChat([]byte(agBasic)); err != nil {
		t.Fatalf("supported text request: %v", err)
	}
	for _, tc := range []struct{ name, payload string }{
		{"reasoning", `{"model":"m","messages":[{"role":"assistant","content":"x","reasoning_content":"hidden"}]}`},
		{"signature", `{"model":"m","messages":[{"role":"assistant","content":"x","thoughtSignature":"opaque"}]}`},
		{"state", `{"model":"m","messages":[{"role":"user","content":"x"}],"previous_response_id":"previous"}`},
		{"image", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image"}}]}]}`},
		{"strict-tools", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","strict":true}}]}`},
		{"orphan-result", `{"model":"m","messages":[{"role":"tool","tool_call_id":"missing","content":"result"}]}`},
		{"multiple-candidates", `{"model":"m","messages":[{"role":"user","content":"x"}],"n":2}`},
		{"response-format", `{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`},
		{"trailing-json", agBasic + ` {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAntigravityChat([]byte(tc.payload)); err == nil {
				t.Fatal("unsupported request accepted")
			}
		})
	}
}
