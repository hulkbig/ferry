package nativeauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"ferry/internal/secretstore"
)

type redactionDriver struct {
	id, root, next string
	refreshErr     error
}

func (d *redactionDriver) Info() Info {
	return Info{ID: d.id, BaseURL: d.root, Protocol: "openai-chat"}
}
func (*redactionDriver) Begin(context.Context, *http.Client, LoginOptions) (Session, error) {
	return Session{}, errors.New("unused")
}
func (*redactionDriver) Complete(context.Context, *http.Client, Session, string) (Token, error) {
	return Token{}, errors.New("unused")
}
func (d *redactionDriver) Refresh(_ context.Context, _ *http.Client, t Token) (Token, error) {
	t.AccessToken = d.next
	return t, d.refreshErr
}

type redactionRT func(*http.Request) (*http.Response, error)

func (f redactionRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func redactionFixture(t *testing.T, id, old, next string, rt redactionRT) (*Manager, *redactionDriver, *http.Client, *http.Request) {
	t.Helper()
	root := "https://native.example/v1"
	if id == "devin" {
		root = "https://server.codeium.com"
	}
	d := &redactionDriver{id: id, root: root, next: next}
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, d)
	tok := Token{AccessToken: old, ExpiresAt: time.Now().Add(time.Hour), Extra: map[string]string{
		"client_name": "ferry", "client_version": "test-1", "client_os": "fixture-os", "device_fingerprint": strings.Repeat("ab", 366),
	}}
	if err = m.save(Ref(id, "account"), accountRecord{Token: &tok, BindingID: "same-account"}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", root+"/chat/completions", strings.NewReader(`{"model":"exact-uid","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return m, d, m.ClientFor(Ref(id, "account"), &http.Client{Transport: rt}), req
}
func redactionResponse(status int, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d fixture", status), Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"12"}, "X-Request-Id": {"keep-id"}}, Body: body}
}
func redactionEcho(token string) string {
	encoded, _ := json.Marshal(token)
	return strings.Join([]string{token, url.QueryEscape(token), url.PathEscape(token), string(encoded[1 : len(encoded)-1]), token + "-" + token, base64.StdEncoding.EncodeToString([]byte(token + ":" + token)), base64.StdEncoding.EncodeToString([]byte(token + "-" + token))}, " | ")
}
func assertRedactionSafe(t *testing.T, text string, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		for _, variant := range strings.Split(redactionEcho(token), " | ") {
			if strings.Contains(text, variant) {
				t.Fatal("credential material escaped redaction")
			}
		}
	}
}

type redactionBody struct {
	reader       io.Reader
	read, closes int
	closeErr     error
}

func (b *redactionBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}
func (b *redactionBody) Close() error { b.closes++; return b.closeErr }

func TestNativeRedactionBothAttemptsAndMetadata(t *testing.T) {
	old := "secret-prefix+/&"
	next := old + "rotated"
	calls := 0
	first := &redactionBody{reader: strings.NewReader("first " + old)}
	second := &redactionBody{reader: strings.NewReader("quota exhausted; " + redactionEcho(old) + "; " + redactionEcho(next))}
	var original *http.Response
	_, _, client, req := redactionFixture(t, "fixture", old, next, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return redactionResponse(401, first), nil
		}
		if r.Header.Get("Authorization") != "Bearer "+next {
			t.Fatal("retry did not rotate")
		}
		original = redactionResponse(429, second)
		original.Header.Set("Content-Length", "9999")
		original.Header.Set("X-Diagnostic", redactionEcho(next))
		original.Trailer = http.Header{"X-Diagnostic": {old}}
		return original, nil
	})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	assertRedactionSafe(t, string(raw)+fmt.Sprint(resp.Header)+fmt.Sprint(resp.Trailer), old, next)
	if calls != 2 || first.closes != 1 || second.closes != 1 || first.read > 4096 {
		t.Fatal("attempt body lifecycle changed")
	}
	if !strings.Contains(string(raw), "quota exhausted") || resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "12" || resp.Header.Get("X-Request-Id") != "keep-id" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatal("useful failure metadata lost")
	}
	if resp.ContentLength != int64(len(raw)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(raw)) {
		t.Fatal("stale response length")
	}
	if original.Header.Get("X-Diagnostic") != redactionEcho(next) {
		t.Fatal("mutated shared response headers")
	}
}

func TestNativeRedactionRetainedUnauthorized(t *testing.T) {
	for _, mode := range []string{"no-replay", "refresh-failure", "replay-failure", "account-changed"} {
		t.Run(mode, func(t *testing.T) {
			old := "private-original"
			calls := 0
			b := &redactionBody{reader: strings.NewReader("auth rejected " + old)}
			var m *Manager
			m, d, c, req := redactionFixture(t, "fixture", old, "private-next", func(*http.Request) (*http.Response, error) {
				calls++
				if mode == "account-changed" {
					if err := m.save(Ref("fixture", "account"), accountRecord{Token: &Token{AccessToken: "other-account", ExpiresAt: time.Now().Add(time.Hour)}, BindingID: "other"}); err != nil {
						t.Fatal(err)
					}
				}
				return redactionResponse(401, b), nil
			})
			switch mode {
			case "no-replay":
				req.GetBody = nil
			case "refresh-failure":
				d.refreshErr = errors.New("refresh unavailable")
			case "replay-failure":
				req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("cannot replay") }
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			assertRedactionSafe(t, string(raw), old)
			if calls != 1 || b.closes != 1 || resp.StatusCode != 401 {
				t.Fatal("retained 401 not safely returned")
			}
		})
	}
}

type redactionTimeoutError struct{ text string }

func (e redactionTimeoutError) Error() string { return e.text }
func (redactionTimeoutError) Timeout() bool   { return true }
func (redactionTimeoutError) Temporary() bool { return true }
func TestNativeRedactionTransportErrors(t *testing.T) {
	old, next := "old-error-secret", "new-error-secret"
	for _, retry := range []bool{false, true} {
		calls := 0
		attached := &redactionBody{reader: strings.NewReader("must not read")}
		_, _, c, req := redactionFixture(t, "fixture", old, next, func(*http.Request) (*http.Response, error) {
			calls++
			if retry && calls == 1 {
				return redactionResponse(401, io.NopCloser(strings.NewReader("unauthorized"))), nil
			}
			return redactionResponse(502, attached), redactionTimeoutError{"dial failed: " + redactionEcho(old) + " " + redactionEcho(next)}
		})
		// Only attempted tokens are protected. The first-attempt error includes only
		// the original credential; both must be protected on the retry path.
		if !retry {
			c.Transport.(*nativeTransport).base = redactionRT(func(*http.Request) (*http.Response, error) {
				return redactionResponse(502, attached), redactionTimeoutError{"dial failed: " + redactionEcho(old)}
			})
		}
		_, err := c.Do(req)
		if err == nil {
			t.Fatal("lost transport error")
		}
		assertRedactionSafe(t, err.Error(), old)
		if retry {
			assertRedactionSafe(t, err.Error(), next)
		}
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() || !ne.Temporary() {
			t.Fatal("timeout classification lost")
		}
		for e := err; e != nil; e = errors.Unwrap(e) {
			assertRedactionSafe(t, e.Error(), old)
			if retry {
				assertRedactionSafe(t, e.Error(), next)
			}
		}
		if attached.read != 0 || attached.closes != 1 {
			t.Fatal("error-associated body was not closed unread")
		}
	}
}

type redactionFailReader struct {
	data string
	err  error
}

func (r *redactionFailReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}
func TestNativeRedactionBodyBoundsAndReadFailures(t *testing.T) {
	token := "boundary-secret"
	for _, mode := range []string{"oversize", "read-error", "encoded", "exact-limit"} {
		t.Run(mode, func(t *testing.T) {
			data := strings.Repeat("x", nativeErrorBodyLimit-len(token)/2) + token + strings.Repeat("y", 1024)
			var reader io.Reader = strings.NewReader(data)
			if mode == "read-error" {
				reader = &redactionFailReader{data: "partial " + token[:5], err: errors.New(token)}
			}
			if mode == "exact-limit" {
				reader = strings.NewReader(strings.Repeat("x", nativeErrorBodyLimit-len(token)) + token)
			}
			b := &redactionBody{reader: reader, closeErr: errors.New(token)}
			_, _, c, req := redactionFixture(t, "fixture", token, "unused", func(*http.Request) (*http.Response, error) {
				resp := redactionResponse(503, b)
				if mode == "encoded" {
					resp.Header.Set("Content-Encoding", "gzip")
				}
				return resp, nil
			})
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err = resp.Body.Close(); err != nil {
				t.Fatal("raw close error escaped")
			}
			assertRedactionSafe(t, string(raw), token)
			if b.read > nativeErrorBodyLimit+1 || b.closes != 1 || resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "12" {
				t.Fatal("unbounded read or lost status")
			}
			if mode != "exact-limit" && (strings.Contains(string(raw), token[:5]) || len(raw) > 200) {
				t.Fatal("truncated secret prefix exposed")
			}
			if mode == "exact-limit" && !strings.HasSuffix(string(raw), "[REDACTED]") {
				t.Fatal("exact cap was discarded")
			}
		})
	}
}

func TestNativeRedactionSuccessStreamingReadErrors(t *testing.T) {
	old, next := "stream-old-secret", "stream-new-secret"
	for _, failure := range []error{io.EOF, fmt.Errorf("%s: %w", redactionEcho(old)+redactionEcho(next), context.Canceled), redactionTimeoutError{redactionEcho(old) + redactionEcho(next)}, fmt.Errorf("%s: %w", old, io.ErrUnexpectedEOF)} {
		b := &redactionBody{reader: &redactionFailReader{data: "unchanged stream bytes", err: failure}, closeErr: errors.New(redactionEcho(old) + redactionEcho(next))}
		calls := 0
		_, _, c, req := redactionFixture(t, "fixture", old, next, func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return redactionResponse(401, io.NopCloser(strings.NewReader("retry"))), nil
			}
			r := redactionResponse(200, b)
			r.Header.Set("Content-Type", "text/event-stream")
			r.ContentLength = -1
			return r, nil
		})
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if b.read != 0 || b.closes != 0 || resp.ContentLength != -1 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal("stream buffered or modified before read")
		}
		buf := make([]byte, 128)
		n, readErr := resp.Body.Read(buf)
		if string(buf[:n]) != "unchanged stream bytes" {
			t.Fatal("stream content changed")
		}
		if failure == io.EOF {
			if readErr != io.EOF {
				t.Fatal("EOF identity lost")
			}
		} else {
			if readErr == nil {
				t.Fatal("read error swallowed")
			}
			assertRedactionSafe(t, readErr.Error(), old, next)
			if errors.Is(failure, context.Canceled) && !errors.Is(readErr, context.Canceled) {
				t.Fatal("cancellation identity lost")
			}
			if errors.Is(failure, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				t.Fatal("unexpected EOF identity lost")
			}
			var want net.Error
			if errors.As(failure, &want) {
				var got net.Error
				if !errors.As(readErr, &got) || !got.Timeout() {
					t.Fatal("read timeout lost")
				}
			}
		}
		closeErr := resp.Body.Close()
		if closeErr == nil {
			t.Fatal("close error swallowed")
		}
		assertRedactionSafe(t, closeErr.Error(), old, next)
		if b.closes != 1 {
			t.Fatal("stream not closed")
		}
	}
}

func TestNativeRedactionDevinWireTrailer(t *testing.T) {
	old, next := "devin-session-token$old-private", "devin-session-token$new-private"
	calls := 0
	var bodies []*redactionBody
	_, _, c, req := redactionFixture(t, "devin", old, next, func(r *http.Request) (*http.Response, error) {
		calls++
		token, code := old, "unauthenticated"
		if calls == 2 {
			token, code = next, "resource_exhausted"
		}
		if r.URL.Path != "/exa.api_server_pb.ApiServerService/GetChatMessage" || r.Header.Get("Authorization") != "Basic "+token+"-"+token {
			t.Fatal("test did not exercise actual Devin wire")
		}
		// The wire adapter turns this successful HTTP Connect trailer into a Chat
		// error response. Redaction must occur outside that adapter, after mapping.
		trailer, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": "quota diagnostic: " + redactionEcho(old) + " " + redactionEcho(next)}})
		length := len(trailer)
		if length < 0 || length > math.MaxUint32 {
			return nil, errors.New("Connect trailer exceeds the uint32 frame length")
		}
		frame := make([]byte, 5+length)
		frame[0] = 2
		binary.BigEndian.PutUint32(frame[1:5], uint32(length))
		copy(frame[5:], trailer)
		b := &redactionBody{reader: bytes.NewReader(frame)}
		bodies = append(bodies, b)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/connect+proto"}}, Body: b}, nil
	})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	assertRedactionSafe(t, string(raw), old, next)
	if calls != 2 || resp.StatusCode != 429 || !strings.Contains(string(raw), "quota diagnostic") || !strings.Contains(string(raw), "resource_exhausted") {
		t.Fatal("wire status or useful diagnostics lost")
	}
	for _, b := range bodies {
		if b.closes == 0 {
			t.Fatal("wire body leaked")
		}
	}
}
