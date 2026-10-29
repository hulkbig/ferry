package nativeauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type xmTransport func(*http.Request) (*http.Response, error)

func (f xmTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func xmResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func xmClient(f xmTransport) *http.Client { return &http.Client{Transport: f} }
func xmLiveSession() Session {
	const exchangeEndpoint = "https://auth.x.ai/oauth2/token"
	return Session{DeviceCode: "device-secret", ExpiresAt: time.Now().Add(time.Minute), Extra: map[string]string{"token_endpoint": exchangeEndpoint}}
}

const xmDiscoveryFixture = `{"issuer":"https://auth.x.ai","device_authorization_endpoint":"https://auth.x.ai/oauth2/device","token_endpoint":"https://auth.x.ai/oauth2/token"}`

func TestXMDeviceFlows(t *testing.T) {
	for _, driver := range []Driver{NewXAIDriver(), NewMetaDriver()} {
		t.Run(driver.Info().ID, func(t *testing.T) {
			xai := driver.Info().ID == "xai"
			polls := 0
			client := xmClient(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("User-Agent") != "Ferry/nativeauth" {
					t.Fatal("unexpected client identity")
				}
				if r.URL.String() == xaiDiscoveryURL {
					return xmResponse(200, xmDiscoveryFixture), nil
				}
				if r.URL.String() == metaMintURL {
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body["dca_token"] != "dca:secret" || r.Header.Get("Authorization") != "Bearer dca:secret" {
						t.Fatal("mint credential missing")
					}
					return xmResponse(200, `{"api_key":"LLM|secret","base_url":"https://api.meta.ai/v1","user_email":"real@example.test"}`), nil
				}
				if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
					t.Fatal("invalid form request")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				wantID := metaClientID
				if xai {
					wantID = xaiClientID
				}
				if r.Form.Get("client_id") != wantID {
					t.Fatal("wrong client ID")
				}
				if r.Form.Get("grant_type") == "" {
					host := "auth.meta.com"
					if xai {
						host = "accounts.x.ai"
						if r.Form.Get("scope") != xaiScope {
							t.Fatal("wrong scope")
						}
					}
					return xmResponse(200, `{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://`+host+`/device","verification_uri_complete":"https://`+host+`/device?user_code=ABCD","expires_in":600,"interval":7}`), nil
				}
				if r.Form.Get("grant_type") != xmDeviceGrant || r.Form.Get("device_code") != "device-secret" {
					t.Fatal("wrong device grant")
				}
				polls++
				if polls == 1 {
					return xmResponse(400, `{"error":"authorization_pending"}`), nil
				}
				if polls == 2 {
					return xmResponse(400, `{"error":"slow_down"}`), nil
				}
				if xai {
					return xmResponse(200, `{"access_token":"access-secret","refresh_token":"refresh-secret","expires_in":3600,"token_type":"Bearer","id_token":"unverified"}`), nil
				}
				return xmResponse(200, `{"access_token":"dca:secret","expires_in":3600}`), nil
			})
			s, err := driver.Begin(context.Background(), client, LoginOptions{State: "state", Verifier: "verifier"})
			if err != nil {
				t.Fatal(err)
			}
			if s.State != "state" || s.Verifier != "verifier" || s.IntervalSeconds != 7 || !strings.Contains(s.URL, "user_code=ABCD") {
				t.Fatal("session state not preserved")
			}
			for _, want := range []error{ErrPending, ErrSlowDown, nil} {
				tok, err := driver.Complete(context.Background(), client, s, "")
				if !errors.Is(err, want) {
					t.Fatalf("unexpected poll error: %v", err)
				}
				if want == nil {
					if xai {
						if tok.AccessToken != "access-secret" || tok.RefreshToken != "refresh-secret" || tok.ExpiresAt.IsZero() || tok.Identity != "" {
							t.Fatal("bad xAI token")
						}
					} else {
						if tok.AccessToken != "LLM|secret" || tok.RefreshToken != "dca:secret" || !tok.ExpiresAt.IsZero() || tok.Extra["dca_expires_at"] == "" || tok.Identity != "real@example.test" {
							t.Fatal("bad Meta lifecycle")
						}
					}
				}
			}
		})
	}
}

func TestXAIRefreshAndEndpointValidation(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		c := xmClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() == xaiDiscoveryURL {
				return xmResponse(200, xmDiscoveryFixture), nil
			}
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old" {
				t.Fatal("wrong refresh grant")
			}
			if rotate {
				return xmResponse(200, `{"access_token":"new","refresh_token":"rotated","expires_in":100}`), nil
			}
			return xmResponse(200, `{"access_token":"new"}`), nil
		})
		tok, err := NewXAIDriver().Refresh(context.Background(), c, Token{RefreshToken: "old", Identity: "known"})
		if err != nil || tok.Identity != "known" {
			t.Fatalf("refresh failed: %v", err)
		}
		want := "old"
		if rotate {
			want = "rotated"
		}
		if tok.RefreshToken != want {
			t.Fatal("rotation lost")
		}
	}
	bad := []string{"http://auth.x.ai/token", "https://auth.x.ai.evil.test/token", "https://evil.x.ai/token", "https://user:secret@auth.x.ai/token", "https://auth.x.ai:444/token", "https://auth.x.ai/token?secret=x", "https://auth.x.ai/token#fragment", "https://auth.x.ai/%74oken", "https://auth.x.ai./token"}
	for _, endpoint := range bad {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			c := xmClient(func(r *http.Request) (*http.Response, error) {
				calls++
				return xmResponse(200, `{"device_authorization_endpoint":"https://auth.x.ai/device","token_endpoint":"`+endpoint+`"}`), nil
			})
			if _, err := NewXAIDriver().Begin(context.Background(), c, LoginOptions{}); err == nil || calls != 1 {
				t.Fatal("unsafe discovery accepted")
			}
			calls = 0
			s := xmLiveSession()
			s.Extra["token_endpoint"] = endpoint
			if _, err := NewXAIDriver().Complete(context.Background(), c, s, ""); err == nil || calls != 0 {
				t.Fatal("unsafe session accepted")
			}
			if _, err := NewXAIDriver().Refresh(context.Background(), c, Token{RefreshToken: "secret", Extra: s.Extra}); err == nil || calls != 0 {
				t.Fatal("unsafe refresh accepted")
			}
		})
	}
}

func TestXMResponseSafety(t *testing.T) {
	for _, driver := range []Driver{NewXAIDriver(), NewMetaDriver()} {
		for _, tc := range []struct {
			name   string
			status int
			body   string
			want   error
		}{
			{"denied", 400, `{"error":"access_denied","error_description":"SECRET"}`, ErrLoginRequired},
			{"expired", 400, `{"error":"expired_token"}`, ErrLoginRequired},
			{"invalid grant", 400, `{"error":"invalid_grant"}`, ErrLoginRequired},
			{"server", 500, `SECRET`, nil},
			{"malformed", 200, `{"access_token":"SECRET",`, nil},
			{"unknown", 200, `{"error":"SECRET","access_token":"SECRET"}`, nil},
			{"missing", 200, `{}`, nil},
			{"oversize", 200, strings.Repeat("S", xmBodyLimit+1), nil},
			{"overflow", 200, `{"access_token":"SECRET","expires_in":9223372036854775807}`, nil},
			{"wrong type", 200, `{"access_token":"SECRET","token_type":"MAC"}`, nil},
		} {
			t.Run(driver.Info().ID+"/"+tc.name, func(t *testing.T) {
				c := xmClient(func(*http.Request) (*http.Response, error) { return xmResponse(tc.status, tc.body), nil })
				_, err := driver.Complete(context.Background(), c, xmLiveSession(), "")
				if err == nil || strings.Contains(err.Error(), "SECRET") {
					t.Fatal("unsafe response handling")
				}
				if tc.want != nil && !errors.Is(err, tc.want) {
					t.Fatalf("wrong error %v", err)
				}
			})
		}
		for _, status := range []int{301, 302, 303, 307, 308} {
			calls := 0
			c := xmClient(func(*http.Request) (*http.Response, error) {
				calls++
				r := xmResponse(status, "SECRET")
				r.Header.Set("Location", "https://evil.test/SECRET")
				return r, nil
			})
			c.CheckRedirect = func(*http.Request, []*http.Request) error { t.Fatal("caller redirect invoked"); return nil }
			if _, err := driver.Complete(context.Background(), c, xmLiveSession(), ""); err == nil || calls != 1 || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("redirect followed or leaked")
			}
		}
		c := xmClient(func(*http.Request) (*http.Response, error) { return nil, errors.New("SECRET transport error") })
		if _, err := driver.Complete(context.Background(), c, xmLiveSession(), ""); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("transport leak")
		}
		if _, err := driver.Complete(context.Background(), nil, xmLiveSession(), ""); err == nil {
			t.Fatal("nil client allowed")
		}
		c = xmClient(func(*http.Request) (*http.Response, error) { t.Fatal("expired session made request"); return nil, nil })
		if _, err := driver.Complete(context.Background(), c, Session{}, ""); !errors.Is(err, ErrLoginRequired) {
			t.Fatal("missing session accepted")
		}
	}
}

func TestMetaMintLifecycle(t *testing.T) {
	d := NewMetaDriver()
	original := Token{AccessToken: "old-key", RefreshToken: "dca:secret", Extra: map[string]string{"dca_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}
	c := xmClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != metaMintURL {
			t.Fatal("unexpected mint destination")
		}
		return xmResponse(200, `{"api_key":"new-key"}`), nil
	})
	next, err := d.Refresh(context.Background(), c, original)
	if err != nil || next.AccessToken != "new-key" || !next.ExpiresAt.IsZero() || original.AccessToken != "old-key" {
		t.Fatal("remint failed")
	}
	for _, expiry := range []string{"invalid", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)} {
		original.Extra["dca_expires_at"] = expiry
		if _, err := d.Refresh(context.Background(), c, original); !errors.Is(err, ErrLoginRequired) {
			t.Fatal("expired DCA reminted")
		}
	}
	for _, body := range []string{`{"api_key":"key","base_url":"https://evil.test/v1"}`, `{"api_key":"key","base_url":"https://api.meta.ai:443/v1"}`, `{"api_key":"dca:wrong"}`, `{}`} {
		c = xmClient(func(*http.Request) (*http.Response, error) { return xmResponse(200, body), nil })
		if _, err := d.Refresh(context.Background(), c, Token{RefreshToken: "dca:secret"}); err == nil {
			t.Fatal("bad mint accepted")
		}
	}
	calls := 0
	c = xmClient(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return xmResponse(200, `{"access_token":"dca:secret","expires_in":3600}`), nil
		}
		return xmResponse(503, "SECRET"), nil
	})
	recovery, err := d.Complete(context.Background(), c, xmLiveSession(), "")
	if err == nil || recovery.AccessToken != "" || recovery.RefreshToken != "dca:secret" || recovery.Extra["dca_expires_at"] == "" {
		t.Fatal("mint failure lost recovery credential")
	}
}

func TestXMDeviceValidationAndCancellation(t *testing.T) {
	for _, d := range []xmDevice{
		{}, {DeviceCode: "d", UserCode: "u", URI: "https://evil.test", ExpiresIn: 10},
		{DeviceCode: "d", UserCode: "u", URI: "https://auth.x.ai/device", CompleteURI: "https://evil.test", ExpiresIn: 10},
		{DeviceCode: "d", UserCode: "u", URI: "https://auth.x.ai/device", ExpiresIn: -1},
	} {
		if _, err := xmSession(LoginOptions{}, d, 1800, "auth.x.ai"); err == nil {
			t.Fatal("bad device response accepted")
		}
	}
	s, err := xmSession(LoginOptions{}, xmDevice{DeviceCode: "d", UserCode: "u", URI: "https://auth.x.ai/device", ExpiresIn: 9223372036854775807}, 1800, "auth.x.ai")
	if err != nil || s.IntervalSeconds != 5 || time.Until(s.ExpiresAt) > 30*time.Minute {
		t.Fatal("device lifetime not bounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := xmClient(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if _, err := NewXAIDriver().Complete(ctx, c, xmLiveSession(), ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}

type xmTrackedBody struct {
	remaining int
	read      int
	closed    bool
	fail      bool
}

func (b *xmTrackedBody) Read(p []byte) (int, error) {
	if b.fail {
		return 0, errors.New("SECRET read failure")
	}
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.remaining {
		n = b.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	b.read += n
	b.remaining -= n
	return n, nil
}
func (b *xmTrackedBody) Close() error { b.closed = true; return errors.New("SECRET close failure") }

func TestXMBodyBoundAndClosure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		b := &xmTrackedBody{remaining: 10 * xmBodyLimit, fail: fail}
		c := xmClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: b}, nil
		})
		_, err := NewXAIDriver().Complete(context.Background(), c, xmLiveSession(), "")
		if err == nil || strings.Contains(err.Error(), "SECRET") || !b.closed || b.read > xmBodyLimit+1 {
			t.Fatal("body boundary violated")
		}
	}
}

func TestXAIRejectsDiscoveryAndVerification(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"issuer":"https://evil.test","device_authorization_endpoint":"https://auth.x.ai/device","token_endpoint":"https://auth.x.ai/token"}`,
		`{"device_authorization_endpoint":"https://evil.test/device","token_endpoint":"https://auth.x.ai/token"}`,
		`{"device_authorization_endpoint":"https://auth.x.ai/device","token_endpoint":"https://evil.test/token"}`,
	} {
		calls := 0
		c := xmClient(func(*http.Request) (*http.Response, error) { calls++; return xmResponse(200, body), nil })
		if _, err := NewXAIDriver().Begin(context.Background(), c, LoginOptions{}); err == nil || calls != 1 {
			t.Fatal("discovery validation failed")
		}
	}
	for _, driver := range []Driver{NewXAIDriver(), NewMetaDriver()} {
		c := xmClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() == xaiDiscoveryURL {
				return xmResponse(200, xmDiscoveryFixture), nil
			}
			return xmResponse(200, `{"device_code":"d","user_code":"u","expires_in":600,"verification_uri":"https://evil.test/device"}`), nil
		})
		if _, err := driver.Begin(context.Background(), c, LoginOptions{}); err == nil {
			t.Fatal("untrusted browser URL accepted")
		}
	}
}
