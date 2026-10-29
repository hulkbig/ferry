package nativewire

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type devinTestRT func(*http.Request) (*http.Response, error)

func (f devinTestRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func devinTestRequest(body string) *http.Request {
	r, _ := http.NewRequest("POST", "https://server.codeium.com/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer devin-session-token$fixture-token")
	// Deliberately synthetic OFFLINE FIXTURE data, never used with a real server.
	r.Header.Set(DevinClientNameHeader, "ferry")
	r.Header.Set(DevinClientVersionHeader, "test-1")
	r.Header.Set(DevinClientOSHeader, "fixture-os")
	r.Header.Set(DevinFingerprintHeader, strings.Repeat("ab", 366))
	return r
}
func devinTestResponse(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/connect+proto"}}, Body: io.NopCloser(bytes.NewReader(body))}
}
func devinTestRead(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func devinTestFrame(p []byte) []byte   { return devinEnvelope(0, p) }
func devinTestTrailer(s string) []byte { return devinEnvelope(2, []byte(s)) }
func devinTestGzip(p []byte) []byte {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write(p)
	z.Close()
	return b.Bytes()
}
func devinTestFind(t *testing.T, b []byte, n int) []devinField {
	t.Helper()
	f, e := devinFields(b)
	if e != nil {
		t.Fatal(e)
	}
	var out []devinField
	for _, x := range f {
		if x.num == n {
			out = append(out, x)
		}
	}
	return out
}

func TestDevinNonstreamWireAndUsage(t *testing.T) {
	// Literal protobuf bytes independently encode text field 3, usage field 7
	// with uncached=10, completion=3, cache-write=2, cache-read=4, stop field 5=2.
	payload := []byte{0x1a, 2, 'h', 'i', 0x3a, 8, 0x10, 10, 0x18, 3, 0x20, 2, 0x28, 4, 0x28, 2}
	wire := append(devinTestFrame(payload), devinTestTrailer(`{}`)...)
	tr := NewDevinTransport(devinTestRT(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://server.codeium.com/exa.api_server_pb.ApiServerService/GetChatMessage" || r.Header.Get("Authorization") != "Basic devin-session-token$fixture-token-devin-session-token$fixture-token" || r.Header.Get("Connect-Protocol-Version") != "1" || r.Header.Get("Content-Type") != "application/connect+proto" {
			t.Fatal("wrong upstream contract")
		}
		if r.Header.Get(DevinFingerprintHeader) != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("leaked caller headers")
		}
		flag, b, e := devinReadFrame(r.Body)
		if e != nil || flag != 0 {
			t.Fatal(e)
		}
		if string(devinTestFind(t, b, 21)[0].b) != "exact-uid" {
			t.Fatal("model rewritten")
		}
		meta := devinTestFind(t, b, 1)[0].b
		if string(devinTestFind(t, meta, 1)[0].b) != "ferry" || len(devinTestFind(t, meta, 31)[0].b) != 732 {
			t.Fatal("identity rewritten")
		}
		return devinTestResponse(wire), nil
	}))
	req := devinTestRequest(`{"model":"exact-uid","messages":[{"role":"user","content":"hello"}]}`)
	req.Header.Set("Cookie", "do-not-forward")
	r, e := tr.RoundTrip(req)
	if e != nil {
		t.Fatal(e)
	}
	body := devinTestRead(t, r)
	if r.StatusCode != 200 {
		t.Fatal(body)
	}
	var got struct {
		Choices []struct {
			Message struct{ Content string }
			Finish  string `json:"finish_reason"`
		}
		Usage map[string]json.RawMessage
	}
	if json.Unmarshal([]byte(body), &got) != nil || got.Choices[0].Message.Content != "hi" || got.Choices[0].Finish != "stop" || string(got.Usage["prompt_tokens"]) != "16" || string(got.Usage["completion_tokens"]) != "3" {
		t.Fatal(body)
	}
}
func TestDevinToolsResultsAndStrictPreflight(t *testing.T) {
	body := `{"model":"exact-uid","messages":[{"role":"system","content":"Do not rewrite my prompt."},{"role":"user","content":"call"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}}]},{"role":"tool","tool_call_id":"call-1","content":"answer"}],"tools":[{"type":"function","function":{"name":"lookup","description":"desc","parameters":{"type":"object"}}}],"temperature":0.3,"max_tokens":50}`
	calls := 0
	tr := NewDevinTransport(devinTestRT(func(r *http.Request) (*http.Response, error) {
		calls++
		_, b, e := devinReadFrame(r.Body)
		if e != nil {
			t.Fatal(e)
		}
		if string(devinTestFind(t, b, 2)[0].b) != "Do not rewrite my prompt." {
			t.Fatal("system rewritten")
		}
		prompts := devinTestFind(t, b, 3)
		if len(prompts) != 3 || devinTestFind(t, prompts[2].b, 2)[0].n != 4 || string(devinTestFind(t, prompts[2].b, 7)[0].b) != "call-1" {
			t.Fatal("tool result not encoded")
		}
		tc := devinTestFind(t, prompts[1].b, 6)[0].b
		if string(devinTestFind(t, tc, 3)[0].b) != `{"x":1}` {
			t.Fatal("arguments lost")
		}
		return devinTestResponse(devinTestTrailer(`{}`)), nil
	}))
	r, e := tr.RoundTrip(devinTestRequest(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if calls != 1 {
		t.Fatal(calls)
	}
	base := `{"model":"uid","messages":[{"role":"user","content":"x"}]`
	rejects := []string{base + `,"response_format":{"type":"json_object"}}`, base + `,"reasoning_effort":"high"}`, base + `,"parallel_tool_calls":false}`, base + `,"tool_choice":"required"}`, base + `,"n":2}`, base + `,"stop":["x"]}`, base + `,"temperature":3}`, base + `,"max_tokens":0}`, `{"model":"uid","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x"}}]}]}`, `{"model":"uid","messages":[{"role":"tool","tool_call_id":"orphan","content":"x"}]}`, `{"model":"uid","messages":[{"role":"user","content":"x","name":"unrepresentable"}]}`}
	tr = NewDevinTransport(devinTestRT(func(*http.Request) (*http.Response, error) { t.Fatal("network for invalid input"); return nil, nil }))
	for _, body := range rejects {
		if _, e := tr.RoundTrip(devinTestRequest(body)); e == nil {
			t.Fatal("accepted", body)
		}
	}
	for _, mutate := range []func(*http.Request){func(r *http.Request) { r.Header.Del(DevinFingerprintHeader) }, func(r *http.Request) { r.URL.Host = "evil.test" }, func(r *http.Request) { r.Header.Set("Authorization", "Bearer raw") }, func(r *http.Request) { r.URL.RawQuery = "x=1" }} {
		r := devinTestRequest(base + `}`)
		mutate(r)
		if _, e := tr.RoundTrip(r); e == nil {
			t.Fatal("accepted invalid envelope")
		}
	}
}
func TestDevinStreamToolsGzipUsageAndUTF8(t *testing.T) {
	c1 := devinString(nil, 1, "call-1")
	c1 = devinString(c1, 2, "lookup")
	c1 = devinString(c1, 3, `{"x":`)
	c2 := devinString(nil, 3, `1}`)
	wire := devinTestFrame(devinBytes(nil, 3, []byte{0xe4, 0xbd}))
	wire = append(wire, devinEnvelope(1, devinTestGzip(devinBytes(nil, 3, []byte{0xa0})))...)
	wire = append(wire, devinTestFrame(devinBytes(nil, 6, c1))...)
	wire = append(wire, devinTestFrame(devinBytes(nil, 6, c2))...)
	usage := devinInt(nil, 2, 20)
	usage = devinInt(usage, 3, 5)
	wire = append(wire, devinTestFrame(devinBytes(nil, 7, usage))...)
	wire = append(wire, devinEnvelope(3, devinTestGzip([]byte(`{}`)))...)
	tr := NewDevinTransport(devinTestRT(func(*http.Request) (*http.Response, error) { return devinTestResponse(wire), nil }))
	r, e := tr.RoundTrip(devinTestRequest(`{"model":"uid","messages":[{"role":"user","content":"x"}],"stream":true,"stream_options":{"include_usage":true}}`))
	if e != nil {
		t.Fatal(e)
	}
	s := devinTestRead(t, r)
	for _, want := range []string{`"content":"你"`, `"id":"call-1"`, `"name":"lookup"`, `"arguments":"1}"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":20`, `"completion_tokens":5`, `data: [DONE]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s: %s", want, s)
		}
	}
	// The same frames must aggregate identically in nonstream mode.
	r, e = tr.RoundTrip(devinTestRequest(`{"model":"uid","messages":[{"role":"user","content":"x"}]}`))
	if e != nil {
		t.Fatal(e)
	}
	s = devinTestRead(t, r)
	if !strings.Contains(s, `"arguments":"{\"x\":1}"`) || !strings.Contains(s, `"content":"你"`) {
		t.Fatal(s)
	}
}
func TestDevinFrameBoundsAndCorruption(t *testing.T) {
	var huge [5]byte
	binary.BigEndian.PutUint32(huge[1:], devinMaxFrame+1)
	bad := [][]byte{{0}, {0, 0, 0, 0, 2, 'x'}, {4, 0, 0, 0, 0}, huge[:], devinEnvelope(1, []byte("bad gzip")), devinEnvelope(1, devinTestGzip(bytes.Repeat([]byte{'x'}, devinMaxDecoded+1)))}
	for i, b := range bad {
		if _, _, e := devinReadFrame(bytes.NewReader(b)); e == nil {
			t.Fatal("accepted invalid frame", i)
		}
	}
	for _, b := range [][]byte{{0}, {0x1a, 3, 1}, {0x1b}, {0x10, 0x80}} {
		if _, e := devinFields(b); e == nil {
			t.Fatal("accepted bad proto", b)
		}
	}
}
func TestDevinTrailersTruncationAndHTTPFailures(t *testing.T) {
	prefix := devinTestFrame([]byte{0x1a, 1, 'x'})
	cases := []struct {
		name   string
		wire   []byte
		status int
	}{{"auth", devinTestTrailer(`{"error":{"code":"unauthenticated","message":"login"}}`), 401}, {"quota", devinTestTrailer(`{"error":{"code":"resource_exhausted","message":"quota"}}`), 429}, {"late", append(append([]byte{}, prefix...), devinTestTrailer(`{"error":{"code":"internal","message":"failed"}}`)...), 502}, {"truncated", prefix, 502}, {"bad-trailer", append(append([]byte{}, prefix...), devinTestTrailer(`not json`)...), 502}, {"bad-proto", append(devinTestFrame([]byte{0x1a, 9, 'x'}), devinTestTrailer(`{}`)...), 502}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewDevinTransport(devinTestRT(func(*http.Request) (*http.Response, error) { return devinTestResponse(tc.wire), nil }))
			for _, stream := range []bool{false, true} {
				body := `{"model":"uid","messages":[{"role":"user","content":"x"}]`
				if stream {
					body += `,"stream":true`
				}
				body += `}`
				r, e := tr.RoundTrip(devinTestRequest(body))
				if e != nil {
					t.Fatal(e)
				}
				s := devinTestRead(t, r)
				if !stream || tc.name == "auth" || tc.name == "quota" {
					if r.StatusCode != tc.status {
						t.Fatal(r.StatusCode, s)
					}
				}
				if !strings.Contains(s, `"error"`) || strings.Contains(s, "[DONE]") || strings.Contains(s, `"finish_reason":"stop"`) {
					t.Fatal("false success", s)
				}
			}
		})
	}
	tr := NewDevinTransport(devinTestRT(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("private token"))}, nil
	}))
	r, e := tr.RoundTrip(devinTestRequest(`{"model":"u","messages":[{"role":"user","content":"x"}]}`))
	if e != nil {
		t.Fatal(e)
	}
	s := devinTestRead(t, r)
	if r.StatusCode != 403 || strings.Contains(s, "private token") {
		t.Fatal(s)
	}
}
func TestDevinDimensionUsage(t *testing.T) {
	group := devinString(nil, 1, "Token Usage")
	for _, it := range []struct {
		k string
		v float32
	}{{"input_tokens", 9}, {"output_tokens", 3}, {"cached_input_tokens", 2}} {
		value := []byte{0x15}
		value = binary.LittleEndian.AppendUint32(value, math.Float32bits(it.v))
		item := devinBytes(nil, 4, value)
		item = devinString(item, 5, it.k)
		group = devinBytes(group, 2, item)
	}
	s := &devinOutput{}
	if _, e := s.frame(devinBytes(nil, 28, group)); e != nil {
		t.Fatal(e)
	}
	u := s.usage()
	if u["prompt_tokens"] != uint64(11) || u["completion_tokens"] != uint64(3) {
		t.Fatal(u)
	}
	// Explicit zero is present data, not an invitation to resurrect old metrics.
	if e := s.usageField(devinInt(nil, 3, 0)); e != nil {
		t.Fatal(e)
	}
	if s.usage()["completion_tokens"] != uint64(0) {
		t.Fatal(s.usage())
	}
}
func TestDevinCancellationAndEarlyClose(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pr, pw := io.Pipe()
			defer pw.Close()
			sent := make(chan struct{})
			go func() { pw.Write(devinTestFrame([]byte{0x1a, 1, 'x'})); close(sent) }()
			tr := NewDevinTransport(devinTestRT(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/connect+proto"}}, Body: pr}, nil
			}))
			r := devinTestRequest(fmtDevinRequest(stream)).WithContext(ctx)
			done := make(chan error, 1)
			go func() {
				resp, e := tr.RoundTrip(r)
				if e == nil {
					_, e = io.ReadAll(resp.Body)
					resp.Body.Close()
				}
				done <- e
			}()
			<-sent
			cancel()
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("cancel reported success")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation blocked")
			}
		})
	}
	// Closing a response while producer awaits a consumer must unblock producer
	// and close upstream. No unbounded goroutine survives the abandoned request.
	pr, pw := io.Pipe()
	closed := make(chan struct{})
	var once sync.Once
	body := &devinCloseBody{ReadCloser: pr, close: func() { once.Do(func() { close(closed) }) }}
	go func() {
		defer pw.Close()
		pw.Write(devinTestFrame([]byte{0x1a, 1, 'x'}))
		pw.Write(devinTestFrame([]byte{0x1a, 1, 'y'}))
	}()
	tr := NewDevinTransport(devinTestRT(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/connect+proto"}}, Body: body}, nil
	}))
	r, e := tr.RoundTrip(devinTestRequest(fmtDevinRequest(true)))
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream not closed")
	}
}

type devinCloseBody struct {
	io.ReadCloser
	close func()
}

func (b *devinCloseBody) Close() error { b.close(); return b.ReadCloser.Close() }
func fmtDevinRequest(stream bool) string {
	if stream {
		return `{"model":"u","messages":[{"role":"user","content":"x"}],"stream":true}`
	}
	return `{"model":"u","messages":[{"role":"user","content":"x"}]}`
}
func TestDevinPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := NewDevinTransport(devinTestRT(func(*http.Request) (*http.Response, error) { t.Fatal("network"); return nil, nil }))
	_, e := tr.RoundTrip(devinTestRequest(fmtDevinRequest(false)).WithContext(ctx))
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestValidateDevinChatPureAndMatchesTransport(t *testing.T) {
	valid := []byte(`{"model":"catalog-uid","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"test","parameters":{"type":"object"}}}]}`)
	copyOfInput := append([]byte{}, valid...)
	if e := ValidateDevinChat(valid); e != nil {
		t.Fatal(e)
	}
	_, a, e := devinMapChat(valid, false)
	if e != nil {
		t.Fatal(e)
	}
	_, b, e := devinMapChat(valid, false)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(a, b) || !bytes.Equal(valid, copyOfInput) {
		t.Fatal("validator mutated input or generated identifiers")
	}
	if len(devinTestFind(t, a, 1)) != 0 {
		t.Fatal("validator generated client metadata")
	}
	for _, p := range devinTestFind(t, a, 3) {
		if len(devinTestFind(t, p.b, 1)[0].b) != 0 {
			t.Fatal("validator generated message ID")
		}
	}
	for _, payload := range []string{`{}`, `{"model":"uid","messages":[{"role":"user","content":"x"}],"reasoning_effort":"high"}`, `{"model":"uid","messages":[{"role":"tool","content":"orphan","tool_call_id":"missing"}]}`, `{"model":"uid","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","strict":true}}]}`} {
		if ValidateDevinChat([]byte(payload)) == nil {
			t.Fatal("validation accepted", payload)
		}
		req := devinTestRequest(payload)
		defer req.Body.Close()
		if _, _, e := devinBuild(req, "devin-session-token$fixture"); e == nil {
			t.Fatal("transport disagrees", payload)
		}
	}
	// Mapping is independent of binding: absence of credentials/device metadata
	// must not make a valid Preparer payload unavailable.
	req := devinTestRequest(string(valid))
	req.Header.Del(DevinFingerprintHeader)
	defer req.Body.Close()
	if ValidateDevinChat(valid) != nil {
		t.Fatal("validator needs metadata")
	}
	if _, _, e := devinBuild(req, "devin-session-token$fixture"); e == nil {
		t.Fatal("transport did not enforce metadata")
	}
}

func TestDevinAcceptsOnlyCurrentAndLegacyOwnIdentity(t *testing.T) {
	for _, name := range []string{"ferry", "smart-api", "another-app", ""} {
		req := devinTestRequest(fmtDevinRequest(false))
		req.Header.Set(DevinClientNameHeader, name)
		_, err := devinMetadata(req.Header, "fixture")
		want := name == "ferry" || name == "smart-api"
		if (err == nil) != want {
			t.Fatalf("name %q accepted=%v", name, err == nil)
		}
	}
}
