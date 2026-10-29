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

type ckRoundTrip func(*http.Request) (*http.Response, error)

func (f ckRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func ckResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func ckClient(t *testing.T, handle func(*http.Request) (*http.Response, error)) *http.Client {
	t.Helper()
	return &http.Client{Transport: ckRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > ckTimeout {
			t.Error("request lacks bounded deadline")
		}
		if r.Method != "POST" || r.URL.Scheme != "https" || r.Header.Get("User-Agent") != "Ferry/nativeauth" || r.Header.Get("Accept") != "application/json" {
			t.Error("unexpected request metadata")
		}
		return handle(r)
	})}
}
func ckOptions() LoginOptions {
	return LoginOptions{State: "manager-state", Verifier: strings.Repeat("a", 43)}
}

func TestClaudeNativeFlow(t *testing.T) {
	d := NewClaudeDriver()
	if d.Info().BaseURL != "https://api.anthropic.com/v1" {
		t.Fatal("incorrect Claude API root")
	}
	opts := ckOptions()
	s, err := d.Begin(context.Background(), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(s.URL)
	q := u.Query()
	sum := sha256.Sum256([]byte(opts.Verifier))
	if s.State != opts.State || s.Verifier != opts.Verifier || u.Host != "claude.ai" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("state") != opts.State || q.Get("redirect_uri") != claudeRedirectURI || q.Get("scope") != claudeScope || q.Get("code_challenge_method") != "S256" {
		t.Fatal("invalid PKCE session")
	}
	calls := 0
	c := ckClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != claudeExchangeEndpoint || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong endpoint/encoding")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatal("bad JSON")
		}
		if body["client_id"] != claudeClientID {
			t.Error("wrong client")
		}
		if calls == 1 {
			if body["code"] != "code" || body["state"] != opts.State || body["code_verifier"] != opts.Verifier || body["redirect_uri"] != claudeRedirectURI || body["grant_type"] != "authorization_code" {
				t.Error("bad code exchange")
			}
			return ckResponse(200, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"account":{"uuid":"account-id","email_address":"email@example.test"},"organization":{"uuid":"org-id"}}`), nil
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "refresh" || body["scope"] != claudeScope {
			t.Error("bad refresh")
		}
		return ckResponse(200, `{"access_token":"rotated","refresh_token":"new-refresh","expires_in":7200}`), nil
	})
	token, err := d.Complete(context.Background(), c, s, claudeRedirectURI+"?code=code&state="+opts.State)
	if err != nil || token.Identity != "email@example.test" || token.Extra["account_uuid"] != "account-id" || time.Until(token.ExpiresAt) < 59*time.Minute {
		t.Fatal("bad token", err)
	}
	next, err := d.Refresh(context.Background(), c, token)
	if err != nil || next.AccessToken != "rotated" || next.RefreshToken != "new-refresh" || next.Identity != token.Identity || next.Extra["organization_uuid"] != "org-id" {
		t.Fatal("bad refresh result", err)
	}
	next.Extra["account_uuid"] = "changed"
	if token.Extra["account_uuid"] != "account-id" {
		t.Fatal("aliased token metadata")
	}
}

func TestClaudeCallbackRejectsBeforeNetwork(t *testing.T) {
	d := NewClaudeDriver()
	if d.Info().BaseURL != "https://api.anthropic.com/v1" {
		t.Fatal("incorrect Claude API root")
	}
	s, _ := d.Begin(context.Background(), nil, ckOptions())
	c := ckClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })
	for _, input := range []string{"bare-code", "code#wrong", "code=code&state=wrong", "code=code&state=manager-state&state=manager-state", "code=code&state=manager-state&error=denied", "https://evil.test/callback?code=code&state=manager-state", "http://localhost:54545/other?code=code&state=manager-state", "code#manager-state#extra"} {
		if _, err := d.Complete(context.Background(), c, s, input); err == nil {
			t.Errorf("accepted invalid callback %q", input)
		}
	}
	for _, input := range []string{"code#manager-state", "?code=code&state=manager-state"} {
		if code, err := claudeCallback(input, s.State); err != nil || code != "code" {
			t.Error("valid callback rejected")
		}
	}
	s.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := d.Complete(context.Background(), c, s, "code#manager-state"); err != ErrLoginRequired {
		t.Error("expired session accepted")
	}
	for _, opts := range []LoginOptions{{State: "state"}, {State: "", Verifier: strings.Repeat("a", 43)}, {State: "state", Verifier: strings.Repeat("!", 43)}} {
		if _, err := d.Begin(context.Background(), c, opts); err == nil {
			t.Error("bad options accepted")
		}
	}
}

func TestKimiNativeFlows(t *testing.T) {
	for _, international := range []bool{false, true} {
		t.Run(map[bool]string{false: "com", true: "ai"}[international], func(t *testing.T) {
			d := NewKimiDriver(international)
			domain := "kimi.com"
			if international {
				domain = "kimi.ai"
			}
			opts := ckOptions()
			opts.Fields = map[string]string{"device_id": "stable-device"}
			calls := 0
			c := ckClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "auth."+domain || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("X-Msh-Device-Id") != "stable-device" || r.Header.Get("X-Msh-Platform") != "Ferry" {
					t.Error("wrong Kimi request")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("client_id") != kimiClientID {
					t.Error("bad client id")
				}
				switch calls {
				case 1:
					if r.URL.Path != "/api/oauth/device_authorization" {
						t.Error("bad begin path")
					}
					return ckResponse(200, `{"device_code":"device-secret","user_code":"USER","verification_uri_complete":"https://`+domain+`/activate?user_code=USER","expires_in":1800,"interval":1}`), nil
				case 2, 3, 4:
					if r.URL.Path != "/api/oauth/token" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "device-secret" {
						t.Error("bad poll")
					}
					if calls == 2 {
						return ckResponse(200, `{"error":"authorization_pending"}`), nil
					}
					if calls == 3 {
						return ckResponse(400, `{"error":"slow_down"}`), nil
					}
					return ckResponse(200, `{"access_token":"access","refresh_token":"refresh","expires_in":60.5,"token_type":"Bearer","scope":"coding"}`), nil
				default:
					if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh" {
						t.Error("bad refresh")
					}
					return ckResponse(200, `{"access_token":"rotated","expires_in":120}`), nil
				}
			})
			s, err := d.Begin(context.Background(), c, opts)
			if err != nil || s.State != opts.State || s.Verifier != opts.Verifier || s.IntervalSeconds != 5 || time.Until(s.ExpiresAt) > 15*time.Minute {
				t.Fatal("bad session", err)
			}
			if d.Info().BaseURL != "https://api."+domain+"/coding/v1" || d.Info().Protocol != "openai-chat" {
				t.Fatal("bad info")
			}
			for _, want := range []error{ErrPending, ErrSlowDown} {
				if _, err := d.Complete(context.Background(), c, s, ""); err != want {
					t.Fatalf("got %v want %v", err, want)
				}
			}
			token, err := d.Complete(context.Background(), c, s, "")
			if err != nil || token.Extra["device_id"] != "stable-device" || token.Extra["domain"] != domain || token.Extra["scope"] != "coding" || time.Until(token.ExpiresAt) < time.Minute {
				t.Fatal("bad token", err)
			}
			next, err := d.Refresh(context.Background(), c, token)
			if err != nil || next.RefreshToken != "refresh" || next.AccessToken != "rotated" {
				t.Fatal("bad refresh result", err)
			}
			next.Extra["device_id"] = "changed"
			if token.Extra["device_id"] != "stable-device" {
				t.Fatal("aliased metadata")
			}
			if _, err := NewKimiDriver(!international).Refresh(context.Background(), c, token); err == nil {
				t.Fatal("cross-domain refresh accepted")
			}
		})
	}
}

func TestClaudeKimiHTTPFailuresAreClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"pending", 200, `{"error":"authorization_pending","error_description":"SECRET"}`, ErrPending},
		{"slow", 400, `{"error":"slow_down"}`, ErrSlowDown},
		{"denied", 400, `{"error":"access_denied"}`, ErrLoginRequired},
		{"expired", 400, `{"error":"expired_token"}`, ErrLoginRequired},
		{"invalid-grant", 400, `{"error":"invalid_grant"}`, ErrLoginRequired},
		{"401", 401, "SECRET", ErrLoginRequired}, {"403", 403, "SECRET", ErrLoginRequired}, {"429", 429, "SECRET", ErrSlowDown},
		{"server", 500, `{"error":"authorization_pending","access_token":"SECRET"}`, ckRequestFailed},
		{"unknown", 400, `{"error":"SECRET","error_description":"SECRET"}`, ckRequestFailed},
		{"malformed", 200, `{"access_token":"SECRET"`, ckInvalid},
		{"large", 200, strings.Repeat("s", ckBodyLimit+1), ckInvalid},
		{"redirect", 307, "SECRET", ckRequestFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := ckClient(t, func(*http.Request) (*http.Response, error) {
				calls++
				r := ckResponse(tc.status, tc.body)
				r.Header.Set("Location", "https://evil.test/SECRET")
				return r, nil
			})
			c.CheckRedirect = func(*http.Request, []*http.Request) error { t.Error("caller redirect hook used"); return nil }
			var out any
			err := ckPost(context.Background(), c, claudeExchangeEndpoint, "application/json", "{}", nil, &out)
			if err != tc.want || strings.Contains(err.Error(), "SECRET") || calls != 1 {
				t.Fatalf("unexpected safe error %v / calls %d", err, calls)
			}
			if c.Timeout != 0 || c.CheckRedirect == nil {
				t.Error("caller client mutated")
			}
		})
	}
	c := ckClient(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("SECRET transport details") })
	var out any
	if err := ckPost(context.Background(), c, claudeExchangeEndpoint, "application/json", "{}", nil, &out); err != ckRequestFailed {
		t.Fatal(err)
	}
	if err := ckPost(context.Background(), c, "https://evil.test", "application/json", "{}", nil, &out); err != ckInvalid {
		t.Fatal("unsafe endpoint accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c = ckClient(t, func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if err := ckPost(ctx, c, claudeExchangeEndpoint, "application/json", "{}", nil, &out); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}

func TestKimiRejectsUntrustedDeviceResponse(t *testing.T) {
	for _, link := range []string{"http://kimi.com/activate", "https://evil.test/activate", "https://kimi.com.evil.test/activate", "https://auth.kimi.ai/activate", "https://user@kimi.com/activate", "https://kimi.com:443/activate"} {
		c := ckClient(t, func(*http.Request) (*http.Response, error) {
			body, _ := json.Marshal(map[string]any{"device_code": "device", "user_code": "USER", "expires_in": 300, "verification_uri_complete": link})
			return ckResponse(200, string(body)), nil
		})
		if _, err := NewKimiDriver(false).Begin(context.Background(), c, ckOptions()); err == nil {
			t.Errorf("accepted %s", link)
		}
	}
}

func TestClaudeKimiRejectInvalidTokenPayloads(t *testing.T) {
	for _, d := range []Driver{NewClaudeDriver(), NewKimiDriver(false), NewKimiDriver(true)} {
		domain := "kimi.com"
		if d.Info().ID == "kimi-ai" {
			domain = "kimi.ai"
		}
		old := Token{RefreshToken: "refresh", Extra: map[string]string{"device_id": "id", "domain": domain}}
		for _, body := range []string{`null`, `{}`, `{"access_token":"SECRET","expires_in":-1}`, `{"access_token":"SECRET","expires_in":1e100}`, `{"access_token":"SECRET","token_type":"MAC"}`} {
			c := ckClient(t, func(*http.Request) (*http.Response, error) { return ckResponse(200, body), nil })
			token, err := d.Refresh(context.Background(), c, old)
			if err == nil || token.AccessToken != "" || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("unsafe response accepted")
			}
		}
	}
}

type ckBlockingBody struct {
	ctx    context.Context
	closed bool
}

func (b *ckBlockingBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, errors.New("SECRET read failure")
}
func (b *ckBlockingBody) Close() error { b.closed = true; return errors.New("SECRET close failure") }

func TestClaudeKimiBodyCancellationAndClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var body *ckBlockingBody
	c := ckClient(t, func(r *http.Request) (*http.Response, error) {
		body = &ckBlockingBody{ctx: r.Context()}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	var out any
	err := ckPost(ctx, c, claudeExchangeEndpoint, "application/json", "{}", nil, &out)
	if !errors.Is(err, context.DeadlineExceeded) || body == nil || !body.closed {
		t.Fatal("deadline or body closure lost", err)
	}
}

func TestKimiGeneratedDeviceAndExpiry(t *testing.T) {
	var device string
	c := ckClient(t, func(r *http.Request) (*http.Response, error) {
		device = r.Header.Get("X-Msh-Device-Id")
		return ckResponse(200, `{"device_code":"device","user_code":"USER","verification_uri":"https://auth.kimi.com/activate","expires_in":300}`), nil
	})
	d := NewKimiDriver(false)
	s, err := d.Begin(context.Background(), c, ckOptions())
	if err != nil || len(device) != 36 || device[14] != '4' || s.Extra["device_id"] != device || s.IntervalSeconds != 5 {
		t.Fatal("invalid generated device session", err)
	}
	c = ckClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected HTTP"); return nil, nil })
	s.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := d.Complete(context.Background(), c, s, ""); err != ErrLoginRequired {
		t.Fatal("expired session accepted")
	}
	if _, err := d.Refresh(context.Background(), c, Token{}); err != ErrLoginRequired {
		t.Fatal("missing refresh accepted")
	}
}
