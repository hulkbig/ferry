package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
)

// ---------------------------------------------------------------------------
// helpers

func testBase(srv *httptest.Server, path string) Base {
	b := Base{
		Endpoint: &domain.Endpoint{
			ID:      "ep-test",
			BaseURL: srvURL(srv) + path,
			// protocol filled by the caller where it matters
		},
		Secret:  "sk-secret-value",
		CredRef: "env:TEST_SECRET",
		Model:   "glm-4-flash",
	}
	if srv != nil {
		b.HTTP = srv.Client()
	}
	return b
}

func srvURL(srv *httptest.Server) string {
	if srv == nil {
		return "http://prepare-only.invalid"
	}
	return srv.URL
}

func chatRC() *domain.RequestContract {
	temp := 0.7
	return &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIChat,
		Model:    "glm-flash",
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "be brief"}}},
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}}},
		},
		Temperature: &temp,
		MaxTokens:   512,
	}
}

func sse(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		io.WriteString(w, f+"\n\n")
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func data(d string) string { return "data: " + d }

// drain collects every event from a stream channel.
func drain(t *testing.T, ch <-chan domain.StreamEvent) []domain.StreamEvent {
	t.Helper()
	var out []domain.StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func mustNoFail(t *testing.T, f *domain.Failure) {
	t.Helper()
	if f != nil {
		t.Fatalf("unexpected failure: %v", f)
	}
}

// ---------------------------------------------------------------------------
// shared transport behavior (via OpenAIChat)

func TestTransportHeadersAndPath(t *testing.T) {
	var gotPath string
	var gotAuth, gotKey, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	c := NewOpenAIChat(Base{
		HTTP:     srv.Client(),
		Endpoint: &domain.Endpoint{ID: "ep", BaseURL: srv.URL + "/v1"},
		Secret:   "sk-secret-value", CredRef: "env:T", Model: "glm-4-flash",
	})
	resp, f := c.Invoke(context.Background(), chatRC())
	mustNoFail(t, f)
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path=%q want /v1/chat/completions (BaseURL carries /v1)", gotPath)
	}
	if gotAuth != "Bearer sk-secret-value" || gotKey != "sk-secret-value" {
		t.Errorf("auth headers: Authorization=%q x-api-key=%q", gotAuth, gotKey)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("content-type=%q", gotCT)
	}
	if resp == nil || len(resp.Content) != 1 || resp.Content[0].Text != "ok" {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestTransportUsesFullURLWithoutAppendingProtocolPath(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()

	c := NewOpenAIChat(Base{
		HTTP: srv.Client(), Endpoint: &domain.Endpoint{
			ID: "ep", BaseURL: srv.URL + "/ignored", FullURL: srv.URL + "/api/plan/v3/chat?tenant=one",
		},
		Secret: "secret", Model: "deepseek-v4-flash",
	})
	_, failure := c.Invoke(context.Background(), chatRC())
	mustNoFail(t, failure)
	if gotPath != "/api/plan/v3/chat" || gotQuery != "tenant=one" {
		t.Fatalf("request URL=%s?%s, want exact Full URL", gotPath, gotQuery)
	}
}

func TestTransportHonorsCustomCredentialPlacement(t *testing.T) {
	var gotHeader, gotQuery, gotDefaultAuth, gotDefaultKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Gateway-Key")
		gotQuery = r.URL.Query().Get("api_key")
		gotDefaultAuth = r.Header.Get("Authorization")
		gotDefaultKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := NewOpenAIChat(Base{
		HTTP: srv.Client(), Endpoint: &domain.Endpoint{ID: "ep", BaseURL: srv.URL + "/v1"},
		Secret: "custom-secret", Model: "m",
		Credential: domain.CredentialRef{
			Header: "X-Gateway-Key", Prefix: "Token ", QueryParam: "api_key",
		},
	})
	_, failure := c.Invoke(context.Background(), chatRC())
	mustNoFail(t, failure)
	if gotHeader != "Token custom-secret" || gotQuery != "custom-secret" {
		t.Fatalf("custom credential placement header=%q query=%q", gotHeader, gotQuery)
	}
	if gotDefaultAuth != "" || gotDefaultKey != "" {
		t.Fatalf("default auth headers remained alongside custom placement: Authorization=%q x-api-key=%q", gotDefaultAuth, gotDefaultKey)
	}
}

func TestTransportFailureRedactsCredentialMaterial(t *testing.T) {
	const secret = "never/leak+this key"
	encodedSecret := url.QueryEscape(secret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"credential `+secret+` (`+encodedSecret+`) rejected"}`)
	}))
	defer srv.Close()

	c := NewOpenAIChat(Base{
		HTTP: srv.Client(), Endpoint: &domain.Endpoint{ID: "ep", BaseURL: srv.URL + "/v1"},
		Secret: secret, Model: "m",
		Credential: domain.CredentialRef{QueryParam: "api_key"},
	})
	_, failure := c.Invoke(context.Background(), chatRC())
	if failure == nil {
		t.Fatal("want authentication failure")
	}
	if strings.Contains(failure.Message, secret) || strings.Contains(failure.Error(), secret) ||
		strings.Contains(failure.Message, encodedSecret) || strings.Contains(failure.Error(), encodedSecret) {
		t.Fatalf("failure leaked credential: %+v", failure)
	}
}

func TestNon200ClassificationAndRetryAfter(t *testing.T) {
	cases := []struct {
		status int
		body   string
		retry  string
		want   domain.FailureType
		wantRA time.Duration
	}{
		{401, `{"error":{"message":"bad key"}}`, "", domain.FailAuth, 0},
		{429, `rate limited`, "90", domain.FailRateLimit, 90 * time.Second},
		// "90" parses as 90 seconds; "30s" fails ParseDuration("30s"+"s") and
		// parseRetryAfter rejects non-digits, so it is not honored.
		{429, `rate limited`, "30s", domain.FailRateLimit, 0},
		{500, `boom`, "", domain.FailProvider5xx, 0},
		{400, `{"error":{"message":"maximum context length exceeded"}}`, "", domain.FailContextOverflow, 0},
		{400, `{"error":{"message":"insufficient balance"}}`, "", domain.FailQuotaExhausted, 0},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.retry != "" {
				w.Header().Set("Retry-After", tc.retry)
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		c := NewOpenAIChat(testBase(srv, "/v1"))
		_, f := c.Invoke(context.Background(), chatRC())
		srv.Close()
		if f == nil {
			t.Fatalf("status %d: want failure", tc.status)
		}
		if f.Type != tc.want {
			t.Errorf("status %d body %q: type=%s want %s (msg=%q)", tc.status, tc.body, f.Type, tc.want, f.Message)
		}
		if f.HTTPStatus != tc.status {
			t.Errorf("status %d: recorded %d", tc.status, f.HTTPStatus)
		}
		if f.RetryAfter != tc.wantRA {
			t.Errorf("retry-after header %q: got %v want %v", tc.retry, f.RetryAfter, tc.wantRA)
		}
	}
}

func TestRetryAfterDigitFormUncapped(t *testing.T) {
	// The 10-minute sanity cap applies only to duration-form values ("90s");
	// the plain-digits fallback path is honored as-is, even for large values.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	_, f := c.Invoke(context.Background(), chatRC())
	if f == nil {
		t.Fatal("want a 429 failure")
	}
	if f.RetryAfter != time.Hour {
		t.Errorf("digit-form Retry-After is honored uncapped, got %v want %v", f.RetryAfter, time.Hour)
	}
	if f.Type != domain.FailRateLimit {
		t.Errorf("type=%s want rate_limit", f.Type)
	}
}

func TestConnectFailureOnDeadUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens now

	c := NewOpenAIChat(Base{HTTP: &http.Client{Timeout: 2 * time.Second},
		Endpoint: &domain.Endpoint{ID: "ep", BaseURL: url + "/v1"},
		Secret:   "s", Model: "m"})
	_, f := c.Invoke(context.Background(), chatRC())
	if f == nil || f.Type != domain.FailConnect {
		t.Fatalf("want connect_failure, got %+v", f)
	}

	// canceled context -> client_cancel
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, f2 := c.Invoke(ctx, chatRC())
	if f2 == nil || f2.Type != domain.FailClientCancel {
		t.Errorf("canceled ctx must yield client_cancel, got %+v", f2)
	}
}

func TestInvalidJSONBodyIsBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{not json`)
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	_, f := c.Invoke(context.Background(), chatRC())
	if f == nil || f.Type != domain.FailBadResponse || !strings.Contains(f.Message, "decode response:") {
		t.Fatalf("want bad_response decode error, got %+v", f)
	}
}

func TestErrorObjectIn200BodyIsBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":{"message":"model overloaded","type":"server_error"}}`)
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	_, f := c.Invoke(context.Background(), chatRC())
	if f == nil || f.Type != domain.FailBadResponse || !strings.Contains(f.Message, "upstream error: model overloaded") {
		t.Fatalf("got %+v", f)
	}
}

func TestChatResponseNormalizesSingletonTextObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","model":"glm-5.3-flash","choices":[{"index":0,"message":{"role":"assistant","content":{"type":"text","text":"hello"}},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer srv.Close()

	response, failure := NewOpenAIChat(testBase(srv, "/v1")).Invoke(context.Background(), chatRC())
	if failure != nil {
		t.Fatalf("singleton text object was rejected: %+v", failure)
	}
	if len(response.ValidationIssues) != 0 || len(response.Content) != 1 || response.Content[0].Text != "hello" {
		t.Fatalf("unexpected normalized response: %+v", response)
	}
	if len(response.Warnings) != 1 || !strings.Contains(response.Warnings[0], "non-standard JSON shape") {
		t.Fatalf("normalization warning missing: %v", response.Warnings)
	}
	var native struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(response.NativeBody, &native); err != nil ||
		len(native.Choices) != 1 || native.Choices[0].Message.Content != "hello" {
		t.Fatalf("native response was not normalized for the client: %s (%v)", response.NativeBody, err)
	}
}

func TestChatResponseNormalizesTextFragmentArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"glm-5.3-flash","choices":[{"message":{"role":"assistant","content":["hello",{"type":"text","text":{"value":" world"},"index":1,"annotations":[]}]},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	response, failure := NewOpenAIChat(testBase(srv, "/v1")).Invoke(context.Background(), chatRC())
	if failure != nil || len(response.ValidationIssues) != 0 || len(response.Content) != 1 || response.Content[0].Text != "hello world" {
		t.Fatalf("text fragments were not normalized: response=%+v failure=%+v", response, failure)
	}
	if len(response.Warnings) != 1 {
		t.Fatalf("normalization warning missing: %v", response.Warnings)
	}
}

func TestChatResponsePreservesUnknownContentObjectAsJSONText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"glm-5.3-flash","choices":[{"message":{"role":"assistant","content":{"type":"image","url":"https://example.test/image"}},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	response, failure := NewOpenAIChat(testBase(srv, "/v1")).Invoke(context.Background(), chatRC())
	if failure != nil {
		t.Fatalf("valid provider JSON content should pass: %+v", failure)
	}
	want := `{"type":"image","url":"https://example.test/image"}`
	if len(response.ValidationIssues) != 0 || len(response.Content) != 1 || response.Content[0].Text != want {
		t.Fatalf("unknown content was not preserved as JSON text: %+v", response)
	}
	if len(response.Warnings) != 1 || !strings.Contains(response.Warnings[0], "normalized") {
		t.Fatalf("normalization warning missing: %v", response.Warnings)
	}
}

func TestNormalizedChatTextContentAcceptsEveryValidJSONShape(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		changed bool
	}{
		{name: "standard string", input: `"hello"`, want: `"hello"`},
		{name: "null", input: `null`, want: `null`},
		{name: "nested text wrapper", input: `{"type":"text","text":{"value":"hello"},"index":0}`, want: `"hello"`, changed: true},
		{name: "mixed text fragments", input: `["hello",{"type":"output_text","text":" world"}]`, want: `"hello world"`, changed: true},
		{name: "unknown object", input: ` { "type": "image", "url": "x" } `, want: `"{\"type\":\"image\",\"url\":\"x\"}"`, changed: true},
		{name: "mixed extension array", input: `["hello",{"type":"image","url":"x"}]`, want: `"[\"hello\",{\"type\":\"image\",\"url\":\"x\"}]"`, changed: true},
		{name: "number", input: `42`, want: `"42"`, changed: true},
		{name: "boolean", input: `true`, want: `"true"`, changed: true},
		{name: "empty array", input: `[]`, want: `"[]"`, changed: true},
		{name: "invalid trailing data", input: `{"type":"text","text":"hello"} trailing`, want: `{"type":"text","text":"hello"} trailing`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed := normalizedChatTextContent(json.RawMessage(test.input))
			if changed != test.changed {
				t.Fatalf("changed=%v want=%v output=%s", changed, test.changed, got)
			}
			if !changed {
				got = json.RawMessage(test.input)
			}
			if string(got) != test.want {
				t.Fatalf("output=%s want=%s", got, test.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Prepare / compatibility notes

func TestPrepareNotesAndReasoningControlMappings(t *testing.T) {
	chat := NewOpenAIChat(testBase(nil, "/v1"))
	thinkingMsg := domain.Message{Role: domain.RoleAssistant,
		Blocks: []domain.Block{{Kind: domain.BlockThinking, Text: "hmm"}}}

	rc := chatRC()
	rc.Messages = append(rc.Messages, thinkingMsg)
	rc.Reasoning = &domain.ReasoningSpec{Effort: "high"}
	notes, err := chat.Prepare(rc)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("chat reasoning_content/reasoning_effort mapping must be lossless, notes: %#v", notes)
	}

	rs := NewOpenAIResponses(testBase(nil, ""))
	rc2 := chatRC()
	rc2.Messages = append(rc2.Messages, thinkingMsg)
	n2, err := rs.Prepare(rc2)
	if err != nil {
		t.Fatal(err)
	}
	if len(n2) != 1 || n2[0] != "thinking block dropped (responses adapter keeps only encrypted/summary forms)" {
		t.Errorf("responses notes: %#v", n2)
	}

	an := NewAnthropic(testBase(nil, ""))
	rc3 := chatRC()
	rc3.MaxTokens = 0 // anthropic requires max_tokens
	p3, n3, err := an.buildRequest(rc3)
	if err != nil {
		t.Fatal(err)
	}
	if len(n3) != 0 || !strings.Contains(string(p3), `"max_tokens":4096`) {
		t.Errorf("anthropic required default must be a clean mapping: notes=%#v payload=%s", n3, p3)
	}

	// Reasoning controls are mapped deterministically across API families.
	// Actual provider-native reasoning state is checked on the response path,
	// so a possible future signature/summary must not block this request.
	rc3.Reasoning = &domain.ReasoningSpec{Effort: "low"}
	p3, n3, err = an.buildRequest(rc3)
	if err != nil || len(n3) != 0 || !strings.Contains(string(p3), `"budget_tokens":2048`) {
		t.Errorf("effort→budget mapping: notes=%#v err=%v payload=%s", n3, err, p3)
	}

	// Explicit budget remains exact on Anthropic.
	rc3.Reasoning = &domain.ReasoningSpec{BudgetTokens: 4096}
	n3, _ = an.Prepare(rc3)
	if len(n3) != 0 {
		t.Errorf("explicit budget must not produce notes: %#v", n3)
	}

	// Explicit strict schema output is native; json_object remains degraded.
	strict := true
	rc3.ResponseFormat = &domain.ResponseFormat{Name: "out", Strict: &strict, Schema: json.RawMessage(`{"type":"object"}`)}
	n3, _ = an.Prepare(rc3)
	if len(n3) != 0 {
		t.Fatalf("native structured output has degradation notes: %v", n3)
	}
	rc3.ResponseFormat.Schema = nil
	n3, _ = an.Prepare(rc3)
	if len(n3) != 1 || !strings.Contains(n3[0], "json_object has no native") {
		t.Fatalf("JSON mode degradation missing: %v", n3)
	}

	// Anthropic budget maps to the shared Responses effort control without
	// pretending that response-native encrypted/summary state is guaranteed.
	rcr := chatRC()
	rcr.Origin = domain.ProtocolAnthropic
	rcr.Reasoning = &domain.ReasoningSpec{BudgetTokens: 1024} // <=2048 -> low
	pr, nr, err := rs.buildRequest(rcr)
	if err != nil || len(nr) != 0 || !strings.Contains(string(pr), `"reasoning":{"effort":"low"}`) {
		t.Errorf("budget→effort mapping: notes=%#v err=%v payload=%s", nr, err, pr)
	}

	pc, nc, err := chat.buildRequest(rcr)
	if err != nil || len(nc) != 0 || !strings.Contains(string(pc), `"reasoning_effort":"low"`) {
		t.Errorf("budget→chat effort mapping: notes=%#v err=%v payload=%s", nc, err, pc)
	}
}

func TestSharedCapabilityPrepareMatrix(t *testing.T) {
	chat := NewOpenAIChat(Base{Model: "upstream"})
	responses := NewOpenAIResponses(Base{Model: "upstream"})
	anthropic := NewAnthropic(Base{Model: "upstream"})
	adapters := []struct {
		name    string
		prepare func(*domain.RequestContract) ([]string, error)
	}{
		{"openai-chat", chat.Prepare},
		{"openai-responses", responses.Prepare},
		{"anthropic", anthropic.Prepare},
	}
	origins := []domain.Protocol{
		domain.ProtocolOpenAIChat,
		domain.ProtocolOpenAIResponses,
		domain.ProtocolAnthropic,
	}
	boolPtr := func(value bool) *bool { return &value }
	floatPtr := func(value float64) *float64 { return &value }
	base := func(origin domain.Protocol) *domain.RequestContract {
		return &domain.RequestContract{
			Protocol: origin, Origin: origin, Model: "client-model",
			Messages: []domain.Message{{Role: domain.RoleUser,
				Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hello"}}}},
		}
	}
	features := []struct {
		name  string
		apply func(*domain.RequestContract)
	}{
		{"text with provider default output limit", func(*domain.RequestContract) {}},
		{"stream", func(rc *domain.RequestContract) { rc.Stream = true }},
		{"tools", func(rc *domain.RequestContract) {
			rc.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
			rc.ToolChoice = "lookup"
		}},
		{"tool continuation with echoed source name", func(rc *domain.RequestContract) {
			rc.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
			rc.Messages = []domain.Message{
				{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "look it up"}}},
				{Role: domain.RoleAssistant, Blocks: []domain.Block{{
					Kind: domain.BlockToolUse, ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"q":"x"}`),
				}}},
				{Role: domain.RoleTool, Name: "lookup", ToolID: "call-1",
					Blocks: []domain.Block{{Kind: domain.BlockText, Text: "result"}}},
			}
		}},
		{"parallel tools", func(rc *domain.RequestContract) {
			rc.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
			rc.ParallelTools = boolPtr(true)
		}},
		{"reasoning control", func(rc *domain.RequestContract) {
			if rc.Origin == domain.ProtocolAnthropic {
				rc.Reasoning = &domain.ReasoningSpec{BudgetTokens: 4096}
			} else {
				rc.Reasoning = &domain.ReasoningSpec{Effort: "high"}
			}
		}},
		{"vision", func(rc *domain.RequestContract) {
			rc.Messages[0].Blocks = append(rc.Messages[0].Blocks,
				domain.Block{Kind: domain.BlockImage, ImageMIME: "image/png", ImageData: []byte("png")})
		}},
		{"temperature and top_p", func(rc *domain.RequestContract) {
			rc.Temperature = floatPtr(0.4)
			rc.TopP = floatPtr(0.9)
		}},
	}

	for _, origin := range origins {
		for _, feature := range features {
			for _, adapter := range adapters {
				name := string(origin) + "/" + feature.name + "→" + adapter.name
				t.Run(name, func(t *testing.T) {
					rc := base(origin)
					feature.apply(rc)
					notes, err := adapter.prepare(rc)
					if err != nil || len(notes) != 0 {
						t.Fatalf("shared capability was not cleanly mapped: notes=%v err=%v", notes, err)
					}
				})
			}
		}
	}
}

func TestPrepareMatrixRetainsNativeOnlyBoundaries(t *testing.T) {
	chat := NewOpenAIChat(Base{Model: "upstream"})
	responses := NewOpenAIResponses(Base{Model: "upstream"})
	anthropic := NewAnthropic(Base{Model: "upstream"})
	base := func() *domain.RequestContract {
		return &domain.RequestContract{
			Protocol: domain.ProtocolOpenAIChat, Origin: domain.ProtocolOpenAIChat,
			Messages: []domain.Message{{Role: domain.RoleUser,
				Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hello"}}}},
		}
	}

	structured := base()
	strict := true
	structured.ResponseFormat = &domain.ResponseFormat{Name: "out", Strict: &strict, Schema: json.RawMessage(`{"type":"object"}`)}
	if notes, err := chat.Prepare(structured); err != nil || len(notes) != 0 {
		t.Fatalf("Chat structured output: notes=%v err=%v", notes, err)
	}
	if notes, err := responses.Prepare(structured); err != nil || len(notes) != 0 {
		t.Fatalf("Responses structured output: notes=%v err=%v", notes, err)
	}
	if notes, err := anthropic.Prepare(structured); err != nil || len(notes) != 0 {
		t.Fatalf("Anthropic structured-output boundary: notes=%v err=%v", notes, err)
	}

	responsesOnlyBoundary := base()
	responsesOnlyBoundary.StopSequences = []string{"END"}
	topK := int64(20)
	responsesOnlyBoundary.TopK = &topK
	if notes, err := responses.Prepare(responsesOnlyBoundary); err != nil || len(notes) != 2 ||
		notes[0] != "stop sequences dropped (Responses has no standard stop field)" ||
		notes[1] != "top_k dropped (responses has no equivalent)" {
		t.Fatalf("Responses stop/top_k boundary: notes=%v err=%v", notes, err)
	}

	unsignedHistory := base()
	unsignedHistory.Messages = append(unsignedHistory.Messages, domain.Message{Role: domain.RoleAssistant,
		Blocks: []domain.Block{{Kind: domain.BlockThinking, Text: "private"}}})
	if notes, err := responses.Prepare(unsignedHistory); err != nil || len(notes) != 1 ||
		notes[0] != "thinking block dropped (responses adapter keeps only encrypted/summary forms)" {
		t.Fatalf("Responses reasoning-history boundary: notes=%v err=%v", notes, err)
	}
	if notes, err := anthropic.Prepare(unsignedHistory); err != nil || len(notes) == 0 ||
		!strings.Contains(strings.Join(notes, ";"), "unsigned thinking history") {
		t.Fatalf("Anthropic unsigned-history boundary: notes=%v err=%v", notes, err)
	}
}

func TestAnthropicPreparePreservesCanonicalNativeSemantics(t *testing.T) {
	an := NewAnthropic(testBase(nil, ""))
	rc := &domain.RequestContract{
		Protocol:  domain.ProtocolAnthropic,
		Origin:    domain.ProtocolAnthropic,
		Model:     "claude-native",
		MaxTokens: 512,
		Messages: []domain.Message{
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "run it"}}},
			{Role: domain.RoleAssistant, Blocks: []domain.Block{
				{Kind: domain.BlockThinking, Text: "private", Signature: "sig-native"},
				{Kind: domain.BlockRedactedThinking, Data: "redacted-native"},
				{Kind: domain.BlockToolUse, ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"q":1}`)},
			}},
			{Role: domain.RoleTool, ToolID: "call-1", Blocks: []domain.Block{{
				Kind: domain.BlockToolResult, ID: "call-1",
				Result: json.RawMessage(`[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"eA=="}}]`),
			}}},
		},
	}
	payload, notes, err := an.buildRequest(rc)
	if err != nil || len(notes) != 0 {
		t.Fatalf("canonical Anthropic history was not lossless: notes=%v err=%v", notes, err)
	}
	if !strings.Contains(string(payload), `"signature":"sig-native"`) ||
		!strings.Contains(string(payload), `"redacted_thinking"`) ||
		!strings.Contains(string(payload), `"type":"image"`) {
		t.Fatalf("canonical Anthropic semantics were dropped: %s", payload)
	}
}

// ---------------------------------------------------------------------------
// OpenAIChat Invoke mapping

func TestOpenAIChatInvokeHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		io.WriteString(w, `{
			"model": "glm-4-flash-upstream",
			"service_tier":"default","system_fingerprint":"fp_test",
			"choices":[{
				"message":{
					"content":"hello there",
					"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"sf\"}"}}]
				},
			"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,
				"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":2,
					"audio_tokens":0,"image_tokens":0,"text_tokens":7},
				"completion_tokens_details":{"reasoning_tokens":3,"accepted_prediction_tokens":0,
					"rejected_prediction_tokens":0,"audio_tokens":0,"text_tokens":4,"compute_units":0}}
		}`)
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	r, f := c.Invoke(context.Background(), chatRC())
	mustNoFail(t, f)
	if r.Model != "glm-4-flash-upstream" {
		t.Errorf("model echoed: %q", r.Model)
	}
	if r.FinishReason != "tool_call" {
		t.Errorf("finish_reason=%q want tool_call", r.FinishReason)
	}
	if len(r.Content) != 2 ||
		r.Content[0].Kind != domain.BlockText || r.Content[0].Text != "hello there" ||
		r.Content[1].Kind != domain.BlockToolUse || r.Content[1].Name != "weather" ||
		string(r.Content[1].Input) != `{"city":"sf"}` {
		t.Fatalf("content: %+v", r.Content)
	}
	u := r.Usage
	if u.InputTokens != 11 || u.OutputTokens != 7 || u.CacheReadTokens != 4 ||
		u.CacheWriteTokens != 2 || u.ReasoningTokens != 3 {
		t.Errorf("usage merged wrong: %+v", u)
	}
	if len(r.ConversionIssues) != 0 {
		t.Errorf("standard Chat envelope was treated as content loss: %v", r.ConversionIssues)
	}
}

func TestOpenAIChatInvokeLengthFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"cut"},"finish_reason":"length"}],"usage":{}}`)
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	r, f := c.Invoke(context.Background(), chatRC())
	mustNoFail(t, f)
	if r.FinishReason != "length" {
		t.Errorf("finish=%q want length", r.FinishReason)
	}
	if len(r.Content) != 1 || r.Content[0].Text != "cut" {
		t.Errorf("content=%+v", r.Content)
	}
}

func TestOpenAIChatRequestShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))

	temp := 0.5
	rc := &domain.RequestContract{
		Model: "glm-flash",
		Messages: []domain.Message{
			{Role: domain.RoleUser, Name: "alice", Blocks: []domain.Block{{Kind: domain.BlockText, Text: "q"}}},
			{Role: domain.RoleTool, ToolID: "call_9",
				Blocks: []domain.Block{{Kind: domain.BlockText, Text: "42"}}},
			{Role: domain.RoleAssistant, Blocks: []domain.Block{
				{Kind: domain.BlockText, Text: "answer"},
				{Kind: domain.BlockToolUse, ID: "call_9", Name: "calc", Input: json.RawMessage(`{"x":1}`)},
			}},
		},
		Tools: []domain.ToolDef{{Name: "calc", Description: "math",
			Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:     "calc",
		ResponseFormat: &domain.ResponseFormat{Name: "result", Schema: json.RawMessage(`{"type":"object"}`)},
		StopSequences:  []string{"END", "STOP"},
		Temperature:    &temp,
		MaxTokens:      100,
	}
	if _, f := c.Invoke(context.Background(), rc); f != nil {
		t.Fatal(f)
	}
	if got["model"] != "glm-4-flash" {
		t.Errorf("adapter model used, not alias: %v", got["model"])
	}
	msgs := got["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["name"] != "alice" || m0["content"] != "q" {
		t.Errorf("msg0: %v", m0)
	}
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "tool" || m1["tool_call_id"] != "call_9" || m1["content"] != "42" {
		t.Errorf("tool msg: %v", m1)
	}
	m2 := msgs[2].(map[string]any)
	if m2["content"] == nil {
		t.Errorf("assistant with tool_use must keep content parts: %v", m2)
	}
	tcs := m2["tool_calls"].([]any)
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "call_9" {
		t.Errorf("tool_call id: %v", tc0)
	}
	if got["tool_choice"] == nil {
		t.Error("named tool_choice dropped")
	} else {
		tcm := got["tool_choice"].(map[string]any)
		fn := tcm["function"].(map[string]any)
		if fn["name"] != "calc" {
			t.Errorf("tool_choice: %v", got["tool_choice"])
		}
	}
	rf := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("response_format: %v", rf)
	}
	stop := got["stop"].([]any)
	if len(stop) != 2 || stop[0] != "END" {
		t.Errorf("stop array: %v", got["stop"])
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Error("no reasoning requested but reasoning_effort sent")
	}
}

func TestOpenAIChatStreamHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			t.Errorf("stream flag lost")
		}
		so := req["stream_options"].(map[string]any)
		if so["include_usage"] != true {
			t.Error("include_usage required for usage in stream")
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Error("Accept header")
		}
		sse(w,
			data(`{"choices":[{"delta":{"reasoning_content":"think"}}]}`),
			data(`{"choices":[{"delta":{"content":"Hel"}}]}`),
			data(`{"choices":[{"delta":{"content":"lo"}}]}`),
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f"}}]}}]}`),
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`),
			data(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
			data(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":6}}`),
			data(`[DONE]`),
		)
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	ch, cancel, f := c.Stream(context.Background(), chatRC())
	mustNoFail(t, f)
	defer cancel()

	evs := drain(t, ch)
	var kinds []domain.EventKind
	var text, think, args string
	var end *domain.StreamEvent
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
		switch ev.Kind {
		case domain.EvTextDelta:
			text += ev.TextDelta
		case domain.EvThinkingDelta:
			think += ev.ThinkingDelta
		case domain.EvToolStart:
			if ev.ToolName != "f" || ev.ToolID != "c1" || ev.Index != 0 {
				t.Errorf("tool start: %+v", ev)
			}
		case domain.EvToolDelta:
			args += ev.ArgsDelta
		case domain.EvEnd:
			e := ev
			end = &e
		case domain.EvError:
			t.Fatalf("unexpected stream error: %v", ev.Err)
		}
	}
	if text != "Hello" || think != "think" || args != `{"a":1}` {
		t.Errorf("text=%q think=%q args=%q", text, think, args)
	}
	if end == nil {
		t.Fatal("no EvEnd")
	}
	if end.FinishReason != "tool_call" {
		t.Errorf("stream finish=%q want tool_call", end.FinishReason)
	}
	if end.Usage == nil || end.Usage.InputTokens != 5 || end.Usage.OutputTokens != 6 {
		t.Errorf("end usage: %+v", end.Usage)
	}
}

func TestOpenAIChatStreamNormalizesNonStandardContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			data(`{"choices":[{"index":0,"delta":{"content":{"type":"text","text":{"value":"hello"},"index":0}}}]}`),
			data(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
			data(`[DONE]`),
		)
	}))
	defer srv.Close()

	rc := chatRC()
	rc.Origin = domain.ProtocolOpenAIChat
	rc.Native = json.RawMessage(`{"model":"alias","messages":[],"stream":true}`)
	ch, cancel, failure := NewOpenAIChat(testBase(srv, "/v1")).Stream(context.Background(), rc)
	mustNoFail(t, failure)
	defer cancel()

	events := drain(t, ch)
	var text string
	var normalizedNative bool
	for _, event := range events {
		switch event.Kind {
		case domain.EvTextDelta:
			text += event.TextDelta
		case domain.EvNative:
			if event.NativeFrame != nil && strings.Contains(event.NativeFrame.Data, `"content":"hello"`) {
				normalizedNative = true
			}
		case domain.EvError:
			t.Fatalf("non-standard stream content failed: %+v", event.Err)
		}
	}
	if text != "hello" || !normalizedNative {
		t.Fatalf("stream normalization failed: text=%q normalized_native=%v events=%+v", text, normalizedNative, events)
	}
}

func TestOpenAIChatStreamErrorChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, data(`{"error":{"message":"quota gone"}}`))
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	ch, cancel, f := c.Stream(context.Background(), chatRC())
	mustNoFail(t, f)
	defer cancel()
	evs := drain(t, ch)
	// An in-stream error is already terminal and must not be followed by a
	// fabricated successful end event.
	if len(evs) != 1 || evs[0].Kind != domain.EvError ||
		evs[0].Err.Type != domain.FailBadResponse || evs[0].Err.Message != "quota gone" {
		t.Fatalf("events: %+v", evs)
	}
}

func TestOpenAIChatStreamBrokenMidway(t *testing.T) {
	// A clean TCP close reads as io.EOF (treated as a normal stream end), so
	// exercise the error path directly: readSSE must surface a non-EOF read
	// error after delivering the already-buffered event.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, data(`{"choices":[{"delta":{"content":"par"}}]}`)+"\n\n")
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	ch, cancel, f := c.Stream(context.Background(), chatRC())
	mustNoFail(t, f)
	defer cancel()
	evs := drain(t, ch)
	// A clean TCP close without [DONE] or a finish reason is still truncated.
	if len(evs) != 2 || evs[0].Kind != domain.EvTextDelta || evs[0].TextDelta != "par" ||
		evs[1].Kind != domain.EvError || evs[1].Err == nil || evs[1].Err.Type != domain.FailStreamBroken {
		t.Fatalf("clean truncated stream was accepted: %+v", evs)
	}
}

func TestOpenAIChatNativeDoneFrameIsWithheldWhenFinishReasonIsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, data(`[DONE]`))
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Protocol = domain.ProtocolOpenAIChat
	rc.Origin = domain.ProtocolOpenAIChat
	rc.Native = json.RawMessage(`{"model":"alias","messages":[],"stream":true}`)
	ch, cancel, failure := c.Stream(context.Background(), rc)
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 1 || events[0].Kind != domain.EvError || events[0].Err == nil ||
		events[0].Err.Type != domain.FailBadResponse {
		t.Fatalf("invalid [DONE] frame leaked or was accepted: %+v", events)
	}
}

type errAfterData struct {
	given bool
}

func (e *errAfterData) Read(p []byte) (int, error) {
	if !e.given {
		e.given = true
		n := copy(p, "data: {\"ok\":true}\n\n")
		return n, nil
	}
	return 0, errors.New("connection reset by peer")
}

func TestReadSSEReturnsReadError(t *testing.T) {
	var seen []sseEvent
	err := readSSE(&errAfterData{}, func(ev sseEvent) error {
		seen = append(seen, ev)
		return nil
	})
	if err == nil || err.Error() != "connection reset by peer" {
		t.Fatalf("read error must propagate, got %v", err)
	}
	if len(seen) != 1 || seen[0].data != `{"ok":true}` {
		t.Errorf("flushed event before error: %+v", seen)
	}
}

// ---------------------------------------------------------------------------
// OpenAIResponses adapter

func TestOpenAIResponsesInvoke(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path=%q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{
			"id":"resp_native_1","conversation":{"id":"conv_native_1"},"model":"rs-model","status":"completed",
			"created_at":1,"completed_at":2,"background":false,"service_tier":"default","store":false,
			"instructions":"be brief","max_output_tokens":512,"parallel_tool_calls":true,
			"temperature":0.7,"top_p":1,"truncation":"disabled","tools":[],"tool_choice":"auto",
			"output":[
				{"type":"reasoning","content":[{"type":"reasoning_text","text":"why"}]},
				{"type":"message","content":[{"type":"output_text","text":"answer"}]},
				{"type":"function_call","call_id":"fc1","name":"search","arguments":"{\"q\":\"x\"}"}
			],
			"incomplete_details":null,
			"usage":{"input_tokens":9,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":2}}
		}`)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	rc := chatRC()
	// The responses adapter only emits a reasoning object when an explicit
	// effort is present; budget_tokens alone is not serialized.
	rc.Reasoning = &domain.ReasoningSpec{Effort: "low", BudgetTokens: 100000} // budget >=16384 -> high
	r, f := c.Invoke(context.Background(), rc)
	mustNoFail(t, f)

	if got["store"] != false {
		t.Error("store must be forced false")
	}
	if got["instructions"] != "be brief" {
		t.Errorf("system folded into instructions: %v", got["instructions"])
	}
	rq := got["reasoning"].(map[string]any)
	if rq["effort"] != "high" {
		t.Errorf("budget 100000 must map to high, got %v", rq)
	}
	if r.FinishReason != "tool_call" {
		t.Errorf("finish=%q", r.FinishReason)
	}
	if r.NativeResponseID != "resp_native_1" {
		t.Errorf("native response id = %q", r.NativeResponseID)
	}
	if r.NativeConversationID != "conv_native_1" {
		t.Errorf("native conversation id = %q", r.NativeConversationID)
	}
	if len(r.Content) != 3 ||
		r.Content[0].Kind != domain.BlockThinking || r.Content[0].Text != "why" ||
		r.Content[1].Kind != domain.BlockText || r.Content[1].Text != "answer" ||
		r.Content[2].Kind != domain.BlockToolUse || r.Content[2].ID != "fc1" {
		t.Fatalf("content: %+v", r.Content)
	}
	if r.Usage.ReasoningTokens != 2 || r.Usage.InputTokens != 9 {
		t.Errorf("usage: %+v", r.Usage)
	}
	if len(r.ConversionIssues) != 0 {
		t.Errorf("standard Responses envelope was treated as content loss: %v", r.ConversionIssues)
	}
}

func TestOpenAIResponsesChatConversionPassesWithAdvisoryWarnings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"id":"resp-advisory","model":"glm-5.3-flash","status":"completed",
			"caching":{"type":"provider_managed"},
			"output":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"brief rationale"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
			],
			"usage":{"input_tokens":9,"output_tokens":4,"input_tokens_details":{"cached_tokens":8}}
		}`)
	}))
	defer srv.Close()

	rc := chatRC()
	rc.Origin = domain.ProtocolOpenAIChat
	rc.Protocol = domain.ProtocolOpenAIResponses
	response, failure := NewOpenAIResponses(testBase(srv, "/v1")).Invoke(context.Background(), rc)
	if failure != nil {
		t.Fatalf("advisory provider metadata blocked the response: %+v", failure)
	}
	if len(response.ConversionIssues) != 0 {
		t.Fatalf("advisory fields remained blocking: %v", response.ConversionIssues)
	}
	if len(response.Warnings) != 2 ||
		!strings.Contains(strings.Join(response.Warnings, "; "), "caching metadata") ||
		!strings.Contains(strings.Join(response.Warnings, "; "), "reasoning summary") {
		t.Fatalf("warnings = %v", response.Warnings)
	}
	if len(response.Content) != 2 || response.Content[0].Kind != domain.BlockThinking ||
		response.Content[0].Text != "brief rationale" || response.Content[1].Text != "answer" {
		t.Fatalf("canonical response = %+v", response.Content)
	}
}

func TestOpenAIResponsesIncompleteMeansLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{}}`)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	r, f := c.Invoke(context.Background(), chatRC())
	mustNoFail(t, f)
	if r.FinishReason != "length" {
		t.Errorf("finish=%q want length", r.FinishReason)
	}
}

func TestOpenAIResponsesErrorObjectClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	_, f := c.Invoke(context.Background(), chatRC())
	if f == nil || f.Type != domain.FailRateLimit || f.HTTPStatus != 429 {
		t.Fatalf("got %+v", f)
	}
}

func TestOpenAIResponsesStreamEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			data(`{"type":"response.output_text.delta","item_id":"msg_7","delta":"he"}`),
			data(`{"type":"response.output_text.delta","item_id":"msg_7","delta":"y"}`),
			data(`{"type":"response.reasoning_text.delta","delta":"pondering"}`),
			data(`{"type":"response.output_item.added","item_id":"fc_1","item":{"type":"function_call","call_id":"callA","name":"get"}}`),
			data(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`),
			data(`{"type":"response.output_item.done","item_id":"fc_1","item":{"type":"function_call"}}`),
			data(`{"type":"response.completed","response":{"id":"resp_native_stream","conversation":"conv_native_stream","status":"completed","usage":{"input_tokens":3,"output_tokens":8}}}`),
		)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	ch, cancel, f := c.Stream(context.Background(), chatRC())
	mustNoFail(t, f)
	defer cancel()
	evs := drain(t, ch)

	var text string
	var sawThink, sawStart, sawDelta, sawEnd bool
	var end domain.StreamEvent
	for _, ev := range evs {
		switch ev.Kind {
		case domain.EvTextDelta:
			text += ev.TextDelta
			if ev.Index != 0 {
				t.Errorf("first Responses item index: %d", ev.Index)
			}
		case domain.EvThinkingDelta:
			sawThink = true
		case domain.EvToolStart:
			sawStart = true
			if ev.ToolID != "callA" || ev.ToolName != "get" || ev.Index != 2 {
				t.Errorf("start: %+v", ev)
			}
		case domain.EvToolDelta:
			sawDelta = true
		case domain.EvToolEnd:
			sawEnd = true
		case domain.EvEnd:
			end = ev
		case domain.EvError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if text != "hey" || !sawThink || !sawStart || !sawDelta || !sawEnd {
		t.Errorf("text=%q think=%v start=%v delta=%v end=%v", text, sawThink, sawStart, sawDelta, sawEnd)
	}
	if end.FinishReason != "tool_call" || end.Usage == nil || end.Usage.OutputTokens != 8 {
		t.Errorf("end: %+v", end)
	}
	if end.NativeResponseID != "resp_native_stream" {
		t.Errorf("stream native response id = %q", end.NativeResponseID)
	}
	if end.NativeConversationID != "conv_native_stream" {
		t.Errorf("stream native conversation id = %q", end.NativeConversationID)
	}
}

func TestOpenAIResponsesStreamMapsReasoningSummaryToChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			data(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","content":[],"summary":[]}}`),
			data(`{"type":"response.reasoning_text.delta","item_id":"rs_1","output_index":0,"content_index":0,"delta":"analysis"}`),
			data(`{"type":"response.reasoning_text.done","item_id":"rs_1","output_index":0,"content_index":0,"text":"analysis"}`),
			data(`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`),
			data(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"pondering"}`),
			data(`{"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"pondering"}`),
			data(`{"type":"response.reasoning_summary_part.done","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"pondering"}}`),
			data(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","content":[{"type":"reasoning_text","text":"analysis"}],"summary":[{"type":"summary_text","text":"pondering"}]}}`),
			data(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"answer"}`),
			data(`{"type":"response.completed","response":{"status":"completed","usage":{}}}`),
		)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Origin = domain.ProtocolOpenAIChat
	ch, cancel, failure := c.Stream(context.Background(), rc)
	mustNoFail(t, failure)
	defer cancel()

	var reasoning, text string
	var ended bool
	for _, event := range drain(t, ch) {
		switch event.Kind {
		case domain.EvThinkingDelta:
			reasoning += event.ThinkingDelta
		case domain.EvTextDelta:
			text += event.TextDelta
		case domain.EvEnd:
			ended = true
		case domain.EvError:
			t.Fatalf("reasoning summary stream failed: %v", event.Err)
		}
	}
	if reasoning != "analysispondering" || text != "answer" || !ended {
		t.Fatalf("reasoning=%q text=%q ended=%v", reasoning, text, ended)
	}
}

func TestOpenAIResponsesInvokeMapsReasoningSummaryToChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"status":"completed",
			"output":[{"type":"reasoning","status":"completed",
				"summary":[{"type":"summary_text","text":"pondering"}]}],
			"usage":{}
		}`)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Origin = domain.ProtocolOpenAIChat
	response, failure := c.Invoke(context.Background(), rc)
	mustNoFail(t, failure)
	if len(response.ConversionIssues) != 0 || len(response.Content) != 1 ||
		response.Content[0].Kind != domain.BlockThinking || response.Content[0].Text != "pondering" {
		t.Fatalf("reasoning summary was not mapped to chat reasoning_content: %+v", response)
	}
}

func TestOpenAIResponsesStreamRejectsReasoningSummaryForAnthropic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			data(`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`),
			data(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"pondering"}`),
		)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Origin = domain.ProtocolAnthropic
	ch, cancel, failure := c.Stream(context.Background(), rc)
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 1 || events[0].Kind != domain.EvError || events[0].Err == nil ||
		events[0].Err.Type != domain.FailProtocolConversion ||
		!strings.Contains(events[0].Err.Message, "Anthropic thinking") {
		t.Fatalf("Anthropic reasoning-summary conversion was not rejected: %+v", events)
	}
}

func TestOpenAIResponsesParallelToolItemsUseCollisionFreeIndexes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			data(`{"type":"response.output_item.added","item_id":"fc_alpha","item":{"type":"function_call","call_id":"call-a","name":"alpha"}}`),
			data(`{"type":"response.output_item.added","item_id":"fc_beta","item":{"type":"function_call","call_id":"call-b","name":"beta"}}`),
			data(`{"type":"response.function_call_arguments.delta","item_id":"fc_alpha","delta":"{\"a\":"}`),
			data(`{"type":"response.function_call_arguments.delta","item_id":"fc_beta","delta":"{\"b\":2}"}`),
			data(`{"type":"response.function_call_arguments.delta","item_id":"fc_alpha","delta":"1}"}`),
			data(`{"type":"response.output_item.done","item_id":"fc_alpha","item":{"type":"function_call","arguments":"{\"a\":1}"}}`),
			data(`{"type":"response.output_item.done","item_id":"fc_beta","item":{"type":"function_call","arguments":"{\"b\":2}"}}`),
			data(`{"type":"response.completed","response":{"status":"completed","usage":{}}}`),
		)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	ch, cancel, failure := c.Stream(context.Background(), chatRC())
	mustNoFail(t, failure)
	defer cancel()

	indexes := map[string]int{}
	arguments := map[int]string{}
	for _, event := range drain(t, ch) {
		switch event.Kind {
		case domain.EvToolStart:
			indexes[event.ToolID] = event.Index
		case domain.EvToolDelta:
			arguments[event.Index] += event.ArgsDelta
		case domain.EvError:
			t.Fatalf("parallel tool stream failed: %v", event.Err)
		}
	}
	if indexes["call-a"] == indexes["call-b"] {
		t.Fatalf("parallel tool indexes collided: %v", indexes)
	}
	if arguments[indexes["call-a"]] != `{"a":1}` || arguments[indexes["call-b"]] != `{"b":2}` {
		t.Fatalf("parallel arguments crossed: indexes=%v arguments=%v", indexes, arguments)
	}
}

func TestOpenAIResponsesStreamRecoversDoneOnlyTextWithoutFabrication(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			data(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`),
			data(`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hello"}`),
			data(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}}`),
			data(`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{}}}`),
		)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	ch, cancel, failure := c.Stream(context.Background(), chatRC())
	mustNoFail(t, failure)
	defer cancel()
	var text string
	var ended bool
	for _, event := range drain(t, ch) {
		switch event.Kind {
		case domain.EvTextDelta:
			text += event.TextDelta
		case domain.EvEnd:
			ended = true
		case domain.EvError:
			t.Fatalf("done-only stream failed: %v", event.Err)
		}
	}
	if text != "hello" || !ended {
		t.Fatalf("done-only stream text=%q ended=%v", text, ended)
	}
}

func TestOpenAIResponsesStreamRejectsDoneTextThatDisagreesWithDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			data(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"hello"}`),
			data(`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"text":"different"}`),
		)
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	ch, cancel, failure := c.Stream(context.Background(), chatRC())
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 2 || events[1].Kind != domain.EvError || events[1].Err == nil ||
		events[1].Err.Type != domain.FailProtocolConversion {
		t.Fatalf("mismatched done text events=%+v", events)
	}
}

func TestOpenAIResponsesStreamRequiresTerminalResponseStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, data(`{"type":"response.completed"}`))
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	ch, cancel, failure := c.Stream(context.Background(), chatRC())
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 1 || events[0].Kind != domain.EvError || events[0].Err == nil ||
		events[0].Err.Type != domain.FailBadResponse {
		t.Fatalf("missing terminal status events=%+v", events)
	}
}

func TestOpenAIResponsesNativeMalformedTerminalIsNotForwarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, data(`{"type":"response.completed","response":{"id":"r1"}}`))
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Protocol = domain.ProtocolOpenAIResponses
	rc.Origin = domain.ProtocolOpenAIResponses
	rc.Native = json.RawMessage(`{"model":"alias","input":"x","stream":true}`)
	ch, cancel, failure := c.Stream(context.Background(), rc)
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 1 || events[0].Kind != domain.EvError || events[0].Err == nil ||
		events[0].Err.Type != domain.FailBadResponse {
		t.Fatalf("malformed native terminal frame leaked or was accepted: %+v", events)
	}
}

func TestOpenAIResponsesStreamFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, data(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"melted"}}}`))
	}))
	defer srv.Close()
	c := NewOpenAIResponses(testBase(srv, "/v1"))
	ch, cancel, f := c.Stream(context.Background(), chatRC())
	mustNoFail(t, f)
	defer cancel()
	evs := drain(t, ch)
	if len(evs) != 1 || evs[0].Kind != domain.EvError || evs[0].Err.Message != "melted" {
		t.Fatalf("events: %+v", evs)
	}
}

// ---------------------------------------------------------------------------
// Anthropic adapter

func TestAnthropicInvoke(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path=%q want /v1/messages", r.URL.Path)
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("anthropic-version header missing")
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{
			"model":"an-model",
			"content":[
				{"type":"thinking","thinking":"deep","signature":"sig-native"},
				{"type":"text","text":"final"},
				{"type":"tool_use","id":"tu1","name":"run","input":{"cmd":"ls"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":20,"output_tokens":9,
				"cache_read_input_tokens":5,"cache_creation_input_tokens":2}
		}`)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))
	rc := chatRC()
	rc.MaxTokens = 0 // triggers the default note
	r, f := c.Invoke(context.Background(), rc)
	mustNoFail(t, f)

	if mt, ok := got["max_tokens"].(float64); !ok || int(mt) != 4096 {
		t.Errorf("max_tokens defaulted: %v", got["max_tokens"])
	}
	if got["system"] != "be brief" {
		t.Errorf("system: %v", got["system"])
	}
	if r.FinishReason != "tool_call" {
		t.Errorf("finish=%q want tool_call", r.FinishReason)
	}
	if len(r.Content) != 3 ||
		r.Content[0].Kind != domain.BlockThinking || r.Content[0].Text != "deep" ||
		r.Content[1].Kind != domain.BlockText || r.Content[1].Text != "final" ||
		r.Content[2].Kind != domain.BlockToolUse || r.Content[2].Name != "run" {
		t.Fatalf("content: %+v", r.Content)
	}
	if r.Usage.CacheReadTokens != 5 || r.Usage.CacheWriteTokens != 2 {
		t.Errorf("cache usage: %+v", r.Usage)
	}
	if len(r.ConversionIssues) != 2 ||
		!strings.Contains(strings.Join(r.ConversionIssues, " "), "thinking blocks") ||
		!strings.Contains(strings.Join(r.ConversionIssues, " "), "signature") {
		t.Fatalf("native thinking semantics were not marked conversion-only: %+v", r.ConversionIssues)
	}
}

func TestAnthropicRequestShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))

	rc := &domain.RequestContract{
		Model: "glm-flash",
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "sys1"}}},
			{Role: domain.RoleSystem, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "sys2"}}},
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "only text here"}}},
			{Role: domain.RoleAssistant, Blocks: []domain.Block{
				{Kind: domain.BlockToolUse, ID: "t1", Name: "n", Input: json.RawMessage(`{}`)},
				{Kind: domain.BlockToolUse, ID: "t2", Name: "n", Input: json.RawMessage(`{"x":2}`)},
			}},
			{Role: domain.RoleTool, ToolID: "t1",
				Blocks: []domain.Block{{Kind: domain.BlockText, Text: "done"}}},
			{Role: domain.RoleTool, ToolID: "t2",
				Blocks: []domain.Block{{Kind: domain.BlockText, Text: "also done"}}},
		},
		Tools:         []domain.ToolDef{{Name: "n", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:    "required",
		StopSequences: []string{"END"},
	}
	if _, f := c.Invoke(context.Background(), rc); f != nil {
		t.Fatal(f)
	}
	if sys, ok := got["system"].(string); !ok || sys != "sys1\n\nsys2" {
		t.Errorf("joined system: %v", got["system"])
	}
	msgs := got["messages"].([]any)
	mu := msgs[0].(map[string]any)
	if mu["role"] != "user" || mu["content"] != "only text here" {
		t.Errorf("single-text message must serialize as plain string, got %v", mu)
	}
	ma := msgs[1].(map[string]any)
	blocks := ma["content"].([]any)
	b0 := blocks[0].(map[string]any)
	if b0["type"] != "tool_use" {
		t.Errorf("assistant tool block: %v", b0)
	}
	inp := b0["input"].(map[string]any)
	if len(inp) != 0 {
		t.Errorf("empty object input changed: %v", inp)
	}
	mt := msgs[2].(map[string]any)
	if mt["role"] != "user" {
		t.Errorf("tool result rides as user turn: %v", mt)
	}
	toolResults := mt["content"].([]any)
	if len(toolResults) != 2 {
		t.Fatalf("consecutive tool results must merge into one user turn: %v", mt)
	}
	tr := toolResults[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "t1" {
		t.Errorf("tool_result block: %v", tr)
	}
	if got["tool_choice"] == nil {
		t.Fatal("required tool_choice lost")
	}
	if tc := got["tool_choice"].(map[string]any); tc["type"] != "any" {
		t.Errorf("required maps to any: %v", tc)
	}
	if sq := got["stop_sequences"].([]any); len(sq) != 1 || sq[0] != "END" {
		t.Errorf("stop_sequences: %v", got["stop_sequences"])
	}
}

func TestAnthropicRequestRejectsMissingToolInputInsteadOfFabricatingObject(t *testing.T) {
	c := NewAnthropic(testBase(nil, "/v1"))
	rc := &domain.RequestContract{
		Model: "model",
		Messages: []domain.Message{{Role: domain.RoleAssistant, Blocks: []domain.Block{{
			Kind: domain.BlockToolUse, ID: "t1", Name: "run",
		}}}},
	}
	if _, _, err := c.buildRequest(rc); err == nil || !strings.Contains(err.Error(), "input must be a JSON object") {
		t.Fatalf("missing tool input was not rejected: %v", err)
	}
}

func TestAnthropicConvertedMessagesAlternateRoles(t *testing.T) {
	c := NewAnthropic(testBase(nil, "/v1"))
	rc := &domain.RequestContract{Messages: []domain.Message{
		{Role: domain.RoleAssistant, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "prefill"}}},
		{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "one"}}},
		{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "two"}}},
	}}
	body, _, err := c.buildRequest(rc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%v", msgs)
	}
	if first := msgs[0].(map[string]any); first["role"] != "user" || first["content"] != "..." {
		t.Fatalf("leading assistant requires a user prelude: %v", first)
	}
	merged := msgs[2].(map[string]any)
	parts := merged["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "one" || parts[1].(map[string]any)["text"] != "two" {
		t.Fatalf("consecutive user turns were not merged: %v", merged)
	}
}

func TestAnthropicParallelToolResultsMergeWithoutDegradation(t *testing.T) {
	c := NewAnthropic(testBase(nil, "/v1"))
	rc := &domain.RequestContract{
		Origin: domain.ProtocolOpenAIChat, MaxTokens: 64,
		Messages: []domain.Message{
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "run"}}},
			{Role: domain.RoleAssistant, Blocks: []domain.Block{
				{Kind: domain.BlockToolUse, ID: "a", Name: "alpha", Input: json.RawMessage(`{}`)},
				{Kind: domain.BlockToolUse, ID: "b", Name: "beta", Input: json.RawMessage(`{}`)},
			}},
			{Role: domain.RoleTool, ToolID: "a", Blocks: []domain.Block{{Kind: domain.BlockText, Text: "one"}}},
			{Role: domain.RoleTool, ToolID: "b", Blocks: []domain.Block{{Kind: domain.BlockText, Text: "two"}}},
		},
	}
	_, notes, err := c.buildRequest(rc)
	if err != nil || len(notes) != 0 {
		t.Fatalf("lossless parallel tool results were rejected: notes=%v err=%v", notes, err)
	}
}

func TestResponsesJSONObjectFormatOmitsSchemaFields(t *testing.T) {
	r := NewOpenAIResponses(testBase(nil, "/v1"))
	rc := chatRC()
	rc.ResponseFormat = &domain.ResponseFormat{}
	body, _, err := r.buildRequest(rc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	format := got["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("format=%v", format)
	}
	if _, ok := format["name"]; ok {
		t.Errorf("json_object must not carry json_schema name: %v", format)
	}
	if _, ok := format["schema"]; ok {
		t.Errorf("json_object must not carry schema: %v", format)
	}
}

func TestAnthropicStructuredOutputUsesNativeFormat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))
	rc := chatRC()
	rc.ResponseFormat = &domain.ResponseFormat{Schema: json.RawMessage(`{"type":"object"}`)}
	if _, f := c.Invoke(context.Background(), rc); f != nil {
		t.Fatal(f)
	}
	config, ok := got["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("missing native output config: %v", got)
	}
	format := config["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("invalid native format: %v", format)
	}
	if sys, _ := got["system"].(string); strings.Contains(sys, "Respond ONLY") {
		t.Fatalf("schema degraded to prompt: %s", sys)
	}
	if _, ok := got["response_format"]; ok {
		t.Error("anthropic wire request must not carry response_format")
	}
}

func TestAnthropicStreamAccumulatesUsageAcrossEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			"event: ping\ndata: "+`{"type":"ping"}`,
			"event: message_start\ndata: "+`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":3}}}`,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
			"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque-1"}}`,
			"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":1}`,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":2,"content_block":{"type":"text"}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Hi"}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"!"}}`,
			"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":2}`,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"tt","name":"act","input":{}}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"k\":1}"}}`,
			"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":3}`,
			"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}`,
			"event: message_stop\ndata: "+`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Protocol = domain.ProtocolAnthropic
	rc.Origin = domain.ProtocolAnthropic
	rc.Native = json.RawMessage(`{"model":"glm-flash","max_tokens":512,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	ch, cancel, f := c.Stream(context.Background(), rc)
	mustNoFail(t, f)
	defer cancel()
	evs := drain(t, ch)

	var text, thinking, signature, redacted string
	var args string
	var starts, ends int
	var end domain.StreamEvent
	for _, ev := range evs {
		switch ev.Kind {
		case domain.EvTextDelta:
			text += ev.TextDelta
			if ev.Index != 2 {
				t.Errorf("text index=%d want 2", ev.Index)
			}
		case domain.EvThinkingDelta:
			thinking += ev.ThinkingDelta
		case domain.EvSignatureDelta:
			signature += ev.SignatureDelta
		case domain.EvRedactedThinking:
			redacted = ev.RedactedThinkingData
		case domain.EvToolDelta:
			args += ev.ArgsDelta
		case domain.EvToolStart:
			starts++
			if ev.ToolID != "tt" || ev.ToolName != "act" || ev.Index != 3 {
				t.Errorf("tool start: %+v", ev)
			}
		case domain.EvToolEnd:
			ends++
			if ev.Index != 3 {
				t.Errorf("tool end idx: %d", ev.Index)
			}
		case domain.EvEnd:
			end = ev
		case domain.EvError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if text != "Hi!" || thinking != "hmm" || signature != "sig-1" || redacted != "opaque-1" || args != `{"k":1}` || starts != 1 || ends != 1 {
		t.Errorf("text=%q thinking=%q signature=%q redacted=%q args=%q starts=%d ends=%d", text, thinking, signature, redacted, args, starts, ends)
	}
	if end.FinishReason != "tool_call" {
		t.Errorf("finish=%q", end.FinishReason)
	}
	if end.Usage == nil || end.Usage.InputTokens != 12 || end.Usage.OutputTokens != 15 ||
		end.Usage.CacheReadTokens != 3 {
		t.Errorf("accumulated usage: %+v", end.Usage)
	}
}

func TestAnthropicCrossProtocolSignatureFailsExplicitly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			"event: message_start\ndata: "+`{"type":"message_start","message":{"usage":{}}}`,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"partial"}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))
	ch, cancel, failure := c.Stream(context.Background(), chatRC())
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 1 || events[0].Kind != domain.EvError || events[0].Err == nil ||
		events[0].Err.Type != domain.FailProtocolConversion ||
		events[0].Err.Scope != domain.FailureScopeTarget {
		t.Fatalf("cross-protocol signature was silently dropped: %+v", events)
	}
}

func TestAnthropicCanonicalThinkingWithoutSignatureFailsExplicitly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			"event: message_start\ndata: "+`{"type":"message_start","message":{"usage":{}}}`,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"partial"}}`,
			"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`,
		)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Protocol = domain.ProtocolAnthropic
	rc.Origin = domain.ProtocolAnthropic
	rc.Native = nil // exercise canonical re-encoding after a fallback/rehydration
	ch, cancel, failure := c.Stream(context.Background(), rc)
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	last := events[len(events)-1]
	if last.Kind != domain.EvError || last.Err == nil ||
		last.Err.Type != domain.FailProtocolConversion ||
		last.Err.Scope != domain.FailureScopeTarget ||
		!strings.Contains(last.Err.Message, "without a signature") {
		t.Fatalf("unsigned canonical thinking was silently accepted: %+v", events)
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, "event: error\ndata: "+`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))
	ch, cancel, f := c.Stream(context.Background(), chatRC())
	mustNoFail(t, f)
	defer cancel()
	evs := drain(t, ch)
	if len(evs) != 1 || evs[0].Kind != domain.EvError || evs[0].Err.Message != "overloaded" {
		t.Fatalf("events: %+v", evs)
	}
}

func TestAnthropicNativeMessageStopIsWithheldUntilTerminalDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			"event: message_start\ndata: "+`{"type":"message_start","message":{"usage":{}}}`,
			"event: message_stop\ndata: "+`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()
	c := NewAnthropic(testBase(srv, "/v1"))
	rc := chatRC()
	rc.Protocol = domain.ProtocolAnthropic
	rc.Origin = domain.ProtocolAnthropic
	rc.Native = json.RawMessage(`{"model":"alias","messages":[],"max_tokens":8,"stream":true}`)
	ch, cancel, failure := c.Stream(context.Background(), rc)
	mustNoFail(t, failure)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 2 || events[0].Kind != domain.EvNative ||
		events[0].NativeFrame == nil || events[0].NativeFrame.Terminal ||
		events[1].Kind != domain.EvError || events[1].Err == nil ||
		events[1].Err.Type != domain.FailBadResponse {
		t.Fatalf("invalid Anthropic terminal frame leaked or was accepted: %+v", events)
	}
}

// ---------------------------------------------------------------------------
// factory wiring

func TestFactorySelectsProtocolAdapter(t *testing.T) {
	b := Base{Secret: "s", Model: "m"}
	if _, ok := NewCaller(b, domain.ProtocolAnthropic).(*Anthropic); !ok {
		t.Error("anthropic protocol must build Anthropic adapter")
	}
	if _, ok := NewCaller(b, domain.ProtocolOpenAIResponses).(*OpenAIResponses); !ok {
		t.Error("openai-responses protocol must build responses adapter")
	}
	if _, ok := NewCaller(b, domain.ProtocolOpenAIChat).(*OpenAIChat); !ok {
		t.Error("openai-chat protocol must build chat adapter")
	}
	if _, ok := NewCaller(b, domain.ProtocolOpenAIImages).(*OpenAIImages); !ok {
		t.Error("openai-images protocol must build images adapter")
	}
	if _, ok := NewCaller(b, domain.Protocol("weird")).(*OpenAIChat); !ok {
		t.Error("unknown protocol falls back to chat adapter")
	}
}

func TestOpenAIImagesGenerationPreservesBodyAndUsesGenerationEndpoint(t *testing.T) {
	var gotPath, gotModel, gotPrompt, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotContentType = r.URL.Path, r.Header.Get("Content-Type")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		gotPrompt, _ = body["prompt"].(string)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}],"usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	defer srv.Close()

	caller := NewOpenAIImages(Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL + "/v1"}, Secret: "secret", Model: "gpt-image-2"})
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: "image-alias", Operation: "generation", Native: json.RawMessage(`{"model":"image-alias","prompt":"draw a lighthouse","size":"1024x1024"}`),
	}
	response, failure := caller.Invoke(context.Background(), rc)
	mustNoFail(t, failure)
	if gotPath != "/v1/images/generations" || gotModel != "gpt-image-2" || gotPrompt != "draw a lighthouse" {
		t.Fatalf("request path=%q model=%q prompt=%q", gotPath, gotModel, gotPrompt)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Fatalf("content-type=%q", gotContentType)
	}
	if response.NativeContentType != "application/json; charset=utf-8" || !bytes.Contains(response.NativeBody, []byte(`"b64_json"`)) {
		t.Fatalf("native image response=%+v", response)
	}
}

func TestOpenAIImagesRejectsNonJSONUpstreamResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<script>alert("unexpected")</script>`)
	}))
	defer srv.Close()

	caller := NewOpenAIImages(Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL + "/v1"}, Secret: "secret", Model: "gpt-image-2"})
	response, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: "image-alias", Operation: "generation", Native: json.RawMessage(`{"model":"image-alias","prompt":"draw a lighthouse"}`),
	})
	if response != nil || failure == nil || failure.Type != domain.FailBadResponse {
		t.Fatalf("response=%+v failure=%+v, want bad_response", response, failure)
	}
}

func TestCodexImagesGenerationUsesOAuthEnvelope(t *testing.T) {
	var gotPath, gotAuth, gotAPIKey, gotOriginator, gotAccount, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth, gotAPIKey = r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		gotOriginator, gotAccount = r.Header.Get("Originator"), r.Header.Get("Chatgpt-Account-Id")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}]}`)
	}))
	defer srv.Close()

	caller := NewCodexImages(Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{
		BaseURL: srv.URL, Protocol: domain.ProtocolOpenAIImages,
	}, Secret: "oauth-token", Model: "gpt-image-2"}, "account-7")
	response, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: "image-alias", Operation: "generation",
		Native: json.RawMessage(`{"model":"image-alias","prompt":"draw a lighthouse"}`),
	})
	mustNoFail(t, failure)
	if response == nil || gotPath != "/images/generations" || gotModel != "gpt-image-2" ||
		gotAuth != "Bearer oauth-token" || gotAPIKey != "" || gotOriginator != codexOriginator || gotAccount != "account-7" {
		t.Fatalf("response=%+v path=%q model=%q auth=%q apiKey=%q originator=%q account=%q",
			response, gotPath, gotModel, gotAuth, gotAPIKey, gotOriginator, gotAccount)
	}
}

func TestCodexImagesEditPreservesMultipartFileAndOAuthEnvelope(t *testing.T) {
	var gotPath, gotModel, gotPrompt, gotFile, gotAuth, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth, gotAccount = r.Header.Get("Authorization"), r.Header.Get("Chatgpt-Account-Id")
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		gotModel, gotPrompt = r.FormValue("model"), r.FormValue("prompt")
		file, _, err := r.FormFile("image")
		if err != nil {
			t.Fatalf("image part: %v", err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		gotFile = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"url":"https://example.test/image.png"}]}`)
	}))
	defer srv.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "image-alias")
	_ = writer.WriteField("prompt", "make it blue")
	imagePart, _ := writer.CreateFormFile("image", "input.png")
	_, _ = io.WriteString(imagePart, "PNG-BYTES")
	_ = writer.Close()

	caller := NewCodexImages(Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL + "/v1"}, Secret: "oauth-token", Model: "gpt-image-2"}, "account-8")
	response, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: "image-alias", Operation: "edit", Native: body.Bytes(), NativeContentType: writer.FormDataContentType(),
	})
	mustNoFail(t, failure)
	if gotPath != "/v1/images/edits" || gotModel != "gpt-image-2" || gotPrompt != "make it blue" ||
		gotFile != "PNG-BYTES" || gotAuth != "Bearer oauth-token" || gotAccount != "account-8" {
		t.Fatalf("edit path=%q model=%q prompt=%q file=%q auth=%q account=%q",
			gotPath, gotModel, gotPrompt, gotFile, gotAuth, gotAccount)
	}
	if response == nil || len(response.NativeBody) == 0 {
		t.Fatal("missing native edit response")
	}
}

// ---------------------------------------------------------------------------
// SSE framing robustness

func TestSSEMultiLineDataAndComments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": keepalive comment\n\n")
		// one JSON doc split across two data lines; readSSE joins them with
		// \n, which parses because it lands between tokens
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\n")
		io.WriteString(w, "data: \"content\":\"a\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewOpenAIChat(testBase(srv, "/v1"))
	ch, cancel, f := c.Stream(context.Background(), chatRC())
	mustNoFail(t, f)
	defer cancel()
	evs := drain(t, ch)
	var text string
	for _, ev := range evs {
		if ev.Kind == domain.EvTextDelta {
			text += ev.TextDelta
		}
	}
	if text != "a" {
		t.Errorf("multi-data frame should join into one JSON doc; deltas: %+v", evs)
	}
}
