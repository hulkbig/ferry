package nativeauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type devinAuthRT func(*http.Request) (*http.Response, error)

func (f devinAuthRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDevinPKCEAndComplete(t *testing.T) {
	d := NewDevinDriver()
	i := d.Info()
	if i.BaseURL != "https://server.codeium.com" || i.Protocol != "openai-chat" {
		t.Fatal(i)
	}
	verifier := strings.Repeat("v", 43)
	s, e := d.Begin(context.Background(), nil, LoginOptions{State: "state-test", Verifier: verifier, Fields: map[string]string{"client_name": "fixture-client"}})
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(s.URL)
	hash := sha256.Sum256([]byte(verifier))
	q := u.Query()
	if u.Host != "app.devin.ai" || u.Path != "/auth/cli/continue" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) || q.Get("code_challenge_method") != "S256" || q.Get("cli_pkce_marker") != "1" || q.Has("redirect_uri") {
		t.Fatal(s.URL)
	}
	calls := 0
	c := &http.Client{Transport: devinAuthRT(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://api.devin.ai/auth/cli/token" || r.Method != "POST" {
			t.Fatal(r.URL)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "test-code" || body["code_verifier"] != verifier || len(body) != 2 {
			t.Fatal(body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"token":"fixture-token"}`)), Header: http.Header{}}, nil
	})}
	tok, e := d.Complete(context.Background(), c, s, "code=test-code&state=state-test")
	if e != nil {
		t.Fatal(e)
	}
	if tok.AccessToken != "devin-session-token$fixture-token" || tok.Identity == "" || !tok.ExpiresAt.IsZero() || tok.Extra["client_name"] != "ferry" {
		t.Fatal("incorrect token contract")
	}
	tok2, e := d.Complete(context.Background(), c, s, "test-code")
	if e != nil || tok.Identity != tok2.Identity || tok.Extra["device_fingerprint"] != tok2.Extra["device_fingerprint"] {
		t.Fatal("manual code or identity instability", e)
	}
	if _, e = d.Complete(context.Background(), c, s, "code=test-code&state=wrong"); e == nil {
		t.Fatal("accepted wrong state")
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	if _, e = d.Refresh(context.Background(), c, tok); !errors.Is(e, ErrLoginRequired) {
		t.Fatal(e)
	}
	if calls != 2 {
		t.Fatal("refresh used network")
	}
}
func TestDevinAuthRejectsBeforeNetwork(t *testing.T) {
	d := NewDevinDriver()
	c := &http.Client{Transport: devinAuthRT(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })}
	for _, o := range []LoginOptions{{State: "a", Verifier: "short"}, {State: "bad\nstate", Verifier: strings.Repeat("v", 43)}} {
		if _, e := d.Begin(context.Background(), c, o); e == nil {
			t.Fatal("accepted invalid session")
		}
	}
	s, _ := d.Begin(context.Background(), c, LoginOptions{State: "s", Verifier: strings.Repeat("v", 43)})
	for _, v := range []string{"", "x\ny", "https://evil.test/callback?code=x&state=s", "code=a&code=b&state=s", "code=a&state=s&error=denied"} {
		if _, e := d.Complete(context.Background(), c, s, v); e == nil {
			t.Fatal("accepted", v)
		}
	}
	s.ExpiresAt = time.Now().Add(-time.Second)
	if _, e := d.Complete(context.Background(), c, s, "code"); !errors.Is(e, ErrLoginRequired) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := d.Begin(ctx, c, LoginOptions{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestDevinAuthRedirectAndRedaction(t *testing.T) {
	d := NewDevinDriver()
	s, _ := d.Begin(context.Background(), nil, LoginOptions{State: "s", Verifier: strings.Repeat("v", 43)})
	for _, tc := range []struct {
		status int
		body   string
	}{{302, "secret-code"}, {401, "secret-code"}, {200, `{"token":""}`}, {200, `{"token":"devin-session-token$"}`}, {200, strings.Repeat("a", (1<<20)+1)}} {
		calls := 0
		c := &http.Client{Transport: devinAuthRT(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"https://evil.test/"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
		})}
		_, e := d.Complete(context.Background(), c, s, "secret-code")
		if e == nil || strings.Contains(e.Error(), "secret-code") || calls != 1 {
			t.Fatalf("bad error/redirect: %v %d", e, calls)
		}
	}
}

func TestDevinOwnDeviceIdentity(t *testing.T) {
	d := NewDevinDriver()
	options := LoginOptions{State: "s", Verifier: strings.Repeat("v", 43), Fields: map[string]string{"client_name": "another-app", "device_fingerprint": strings.Repeat("ff", 366)}}
	a, e := d.Begin(context.Background(), nil, options)
	if e != nil {
		t.Fatal(e)
	}
	b, e := d.Begin(context.Background(), nil, options)
	if e != nil {
		t.Fatal(e)
	}
	if a.Extra["client_name"] != "ferry" || a.Extra["client_version"] == "" || a.Extra["client_os"] == "" || len(a.Extra["device_fingerprint"]) != 732 {
		t.Fatal("missing own identity")
	}
	if a.Extra["device_fingerprint"] == b.Extra["device_fingerprint"] || a.Extra["device_fingerprint"] == options.Fields["device_fingerprint"] {
		t.Fatal("identity reused from another login/application")
	}
	persisted, e := devinOwnMetadata(a.Extra)
	if e != nil || persisted["device_fingerprint"] != a.Extra["device_fingerprint"] {
		t.Fatal("identity not stable", e)
	}
}

func TestDevinLegacyMetadataSurvivesRename(t *testing.T) {
	original := map[string]string{"client_name": "smart-api", "device_fingerprint": strings.Repeat("ab", 366)}
	got, err := devinOwnMetadata(original)
	if err != nil || got["client_name"] != original["client_name"] || got["device_fingerprint"] != original["device_fingerprint"] {
		t.Fatal("legacy identity changed", err)
	}
	original["client_name"] = "another-app"
	if _, err := devinOwnMetadata(original); err == nil {
		t.Fatal("foreign identity accepted")
	}
}
