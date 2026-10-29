package nativeauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type vertexTestTransport func(*http.Request) (*http.Response, error)

func (f vertexTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func vertexTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

var vertexTestOnce sync.Once
var vertexTestKey *rsa.PrivateKey
var vertexTestKeyError error

func vertexTestAccount(t *testing.T) (map[string]any, *rsa.PrivateKey) {
	t.Helper()
	vertexTestOnce.Do(func() { vertexTestKey, vertexTestKeyError = rsa.GenerateKey(rand.Reader, 2048) })
	if vertexTestKeyError != nil {
		t.Fatal(vertexTestKeyError)
	}
	der, err := x509.MarshalPKCS8PrivateKey(vertexTestKey)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": "service_account", "project_id": "valid-project", "private_key_id": "test-key-id", "client_email": "test-account@valid-project.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": vertexExchangeEndpoint, "universe_domain": "googleapis.com"}, vertexTestKey
}
func vertexTestJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func vertexTestSession(t *testing.T, location string) Session {
	t.Helper()
	a, _ := vertexTestAccount(t)
	s, err := NewVertexDriver().Begin(context.Background(), nil, LoginOptions{State: "state", Verifier: "verifier", Fields: map[string]string{"service_account": vertexTestJSON(t, a), "location": location}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func vertexVerifyRequest(t *testing.T, r *http.Request, key *rsa.PrivateKey) {
	t.Helper()
	if r.URL.String() != vertexExchangeEndpoint || r.Method != "POST" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		t.Fatal("unexpected credential destination or headers")
	}
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "Ferry/nativeauth" {
		t.Fatal("unexpected token headers")
	}
	if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 30*time.Second {
		t.Fatal("missing bounded deadline")
	}
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if len(r.Form) != 2 || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatal("wrong assertion grant")
	}
	assertion := r.Form.Get("assertion")
	if strings.Contains(assertion, "PRIVATE KEY") {
		t.Fatal("raw private key transmitted")
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatal("invalid JWT")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]string
	if err = json.Unmarshal(header, &h); err != nil {
		t.Fatal(err)
	}
	if h["alg"] != "RS256" || h["typ"] != "JWT" || h["kid"] != "test-key-id" || len(h) != 3 {
		t.Fatal("wrong JWT header")
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err = json.Unmarshal(claims, &p); err != nil {
		t.Fatal(err)
	}
	if len(p) != 5 || p["iss"] != "test-account@valid-project.iam.gserviceaccount.com" || p["aud"] != vertexExchangeEndpoint || p["scope"] != vertexScope {
		t.Fatal("wrong JWT claims")
	}
	issued := int64(p["iat"].(float64))
	expiry := int64(p["exp"].(float64))
	if expiry-issued != 3600 || issued > time.Now().Unix() || issued < time.Now().Add(-time.Minute).Unix() {
		t.Fatal("wrong assertion lifetime")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err = rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatal("JWT signature did not verify")
	}
}

func TestVertexImportExchangeRefreshAndBinding(t *testing.T) {
	d := NewVertexDriver()
	info := d.Info()
	if info.ID != "vertex" || info.Mode != "import" || info.Protocol != "openai-chat" || info.BaseURL != "" {
		t.Fatal("unexpected driver info")
	}
	binding, ok := d.(interface {
		Binding(Token) (string, string, error)
	})
	if !ok {
		t.Fatal("missing optional binding")
	}
	for _, location := range []string{"us-central1", "europe-west4", "global"} {
		t.Run(location, func(t *testing.T) {
			s := vertexTestSession(t, location)
			if s.State != "state" || s.Verifier != "verifier" || s.URL != "" || s.DeviceCode != "" || s.IntervalSeconds != 0 || time.Until(s.ExpiresAt) > 10*time.Minute {
				t.Fatal("invalid import session")
			}
			_, key := vertexTestAccount(t)
			calls := 0
			client := &http.Client{Transport: vertexTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				vertexVerifyRequest(t, r, key)
				return vertexTestResponse(200, `{"access_token":"actual-bearer","token_type":"Bearer","expires_in":3600}`), nil
			})}
			token, err := d.Complete(context.Background(), client, s, "")
			if err != nil {
				t.Fatal(err)
			}
			if token.AccessToken != "actual-bearer" || token.RefreshToken != "" || token.Identity != "" || token.ExpiresAt.IsZero() || token.Extra["service_account"] == "" {
				t.Fatal("wrong secret token layout")
			}
			base, protocol, err := binding.Binding(token)
			host := location + "-aiplatform.googleapis.com"
			if location == "global" {
				host = "aiplatform.googleapis.com"
			}
			if err != nil || protocol != "openai-chat" || base != "https://"+host+"/v1/projects/valid-project/locations/"+location+"/endpoints/openapi" {
				t.Fatal("wrong native Chat binding")
			}
			token.AccessToken = "expired"
			token.ExpiresAt = time.Now().Add(-time.Hour)
			refreshed, err := d.Refresh(context.Background(), client, token)
			if err != nil || refreshed.AccessToken != "actual-bearer" || calls != 2 {
				t.Fatalf("refresh failed: %v", err)
			}
			refreshed.Extra["location"] = "changed"
			if token.Extra["location"] != location || s.Extra["location"] != location {
				t.Fatal("returned metadata aliases input")
			}
		})
	}
	a, _ := vertexTestAccount(t)
	s, err := d.Begin(context.Background(), nil, LoginOptions{Fields: map[string]string{"service_account": vertexTestJSON(t, a), "project_id": "target-project", "location": "global"}})
	if err != nil || s.Extra["project"] != "target-project" {
		t.Fatal("explicit target project override failed")
	}
}

func TestVertexImportRejectsUnsafeFields(t *testing.T) {
	cases := []struct {
		field string
		value any
	}{
		{"type", "external_account"}, {"type", "authorized_user"}, {"type", ""},
		{"project_id", ""}, {"project_id", "../escape"}, {"project_id", "project/path"}, {"project_id", "UPPERCASE"}, {"project_id", "short"},
		{"token_uri", ""}, {"token_uri", "http://oauth2.googleapis.com/token"}, {"token_uri", "https://oauth2.googleapis.com.evil.test/token"},
		{"token_uri", "https://evil.test/token"}, {"token_uri", "https://oauth2.googleapis.com:443/token"}, {"token_uri", "https://oauth2.googleapis.com/token?x=1"},
		{"token_uri", "https://oauth2.googleapis.com/token#x"}, {"token_uri", "https://secret@oauth2.googleapis.com/token"}, {"token_uri", "https://oauth2.googleapis.com/%74oken"},
		{"universe_domain", "evil.test"}, {"client_email", ""}, {"client_email", "user@example.test"}, {"client_email", "secret\n@valid-project.iam.gserviceaccount.com"},
		{"private_key_id", "bad\nkey"}, {"private_key", ""}, {"private_key", "SECRET malformed key"}, {"private_key", strings.Repeat("S", vertexKeyLimit+1)},
	}
	calls := 0
	client := &http.Client{Transport: vertexTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		t.Fatal("invalid import reached network")
		return nil, nil
	})}
	for _, tc := range cases {
		t.Run(tc.field+"/"+fmtVertexLabel(tc.value), func(t *testing.T) {
			a, _ := vertexTestAccount(t)
			a[tc.field] = tc.value
			raw := vertexTestJSON(t, a)
			if _, err := NewVertexDriver().Begin(context.Background(), client, LoginOptions{Fields: map[string]string{"service_account": raw, "location": "us-central1"}}); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid import accepted or leaked")
			}
			extra := map[string]string{"service_account": raw, "project": "valid-project", "location": "us-central1"}
			if _, err := NewVertexDriver().Refresh(context.Background(), client, Token{Extra: extra}); err == nil {
				t.Fatal("tampered stored credential accepted")
			}
		})
	}
	for _, location := range []string{"", "../global", "us-central1.evil.test", "us-central1/../../", "global?x=y", "US-CENTRAL1", "us-central1-a", strings.Repeat("a", 64)} {
		a, _ := vertexTestAccount(t)
		if _, err := NewVertexDriver().Begin(context.Background(), client, LoginOptions{Fields: map[string]string{"service_account": vertexTestJSON(t, a), "location": location}}); err == nil {
			t.Fatal("unsafe location accepted")
		}
	}
	if calls != 0 {
		t.Fatal("unexpected network")
	}
	s := vertexTestSession(t, "global")
	bind := NewVertexDriver().(interface {
		Binding(Token) (string, string, error)
	})
	for _, field := range []string{"project", "location", "service_account"} {
		extra := map[string]string{}
		for k, v := range s.Extra {
			extra[k] = v
		}
		extra[field] = "https://evil.test"
		if _, _, err := bind.Binding(Token{Extra: extra}); err == nil {
			t.Fatal("unsafe binding accepted")
		}
	}
}
func fmtVertexLabel(v any) string {
	s, _ := v.(string)
	if len(s) > 100 {
		return "oversize"
	}
	return s
}

func TestVertexJSONAndKeyBoundaries(t *testing.T) {
	a, key := vertexTestAccount(t)
	raw := vertexTestJSON(t, a)
	for _, bad := range []string{"", `null`, `[]`, `{`, raw + ` {}`, strings.Repeat(" ", vertexJSONLimit+1), `{"type":"service_account","type":"external_account"}`, `{"type":123}`} {
		if _, err := NewVertexDriver().Begin(context.Background(), nil, LoginOptions{Fields: map[string]string{"service_account": bad, "location": "global"}}); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	// Both standard PEM encodings work; encrypted, non-RSA and weak keys do not.
	a["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if _, err := NewVertexDriver().Begin(context.Background(), nil, LoginOptions{Fields: map[string]string{"service_account": vertexTestJSON(t, a), "location": "global"}}); err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)})),
		string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("SECRET")})),
		a["private_key"].(string) + a["private_key"].(string),
		"SECRET garbage" + a["private_key"].(string),
	} {
		a["private_key"] = bad
		if _, err := NewVertexDriver().Begin(context.Background(), nil, LoginOptions{Fields: map[string]string{"service_account": vertexTestJSON(t, a), "location": "global"}}); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("invalid key accepted or leaked")
		}
	}
}

type vertexTestJar struct{ used bool }

func (j *vertexTestJar) SetCookies(*url.URL, []*http.Cookie) { j.used = true }
func (j *vertexTestJar) Cookies(*url.URL) []*http.Cookie {
	j.used = true
	return []*http.Cookie{{Name: "secret", Value: "SECRET"}}
}
func TestVertexNoRedirectCookiesOrErrorLeaks(t *testing.T) {
	s := vertexTestSession(t, "global")
	for _, status := range []int{301, 302, 303, 307, 308} {
		calls := 0
		jar := &vertexTestJar{}
		client := &http.Client{Jar: jar, Transport: vertexTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			r := vertexTestResponse(status, "SECRET")
			r.Header.Set("Location", "https://evil.test/SECRET")
			return r, nil
		}), CheckRedirect: func(*http.Request, []*http.Request) error { t.Fatal("caller redirect policy used"); return nil }}
		if _, err := NewVertexDriver().Complete(context.Background(), client, s, ""); err == nil || strings.Contains(err.Error(), "SECRET") || calls != 1 || jar.used || client.Jar != jar {
			t.Fatal("redirect or cookie isolation failed")
		}
	}
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":"invalid_grant","error_description":"SECRET"}`}, {500, "SECRET"},
		{200, `{"access_token":"SECRET",`}, {200, `{"access_token":"SECRET","token_type":"Bearer","expires_in":0}`},
		{200, `{"access_token":"SECRET","token_type":"Bearer","expires_in":3601}`},
		{200, `{"access_token":"SECRET","token_type":"Bearer","expires_in":-1}`},
		{200, `{"access_token":"SECRET","token_type":"Bearer","expires_in":9223372036854775807}`},
		{200, `{"access_token":"SECRET","token_type":"MAC","expires_in":3600}`},
		{200, `{"access_token":"SECRET","token_type":"Bearer","expires_in":3600,"error":"SECRET"}`},
		{200, `{"access_token":"SECRET","access_token":"other","token_type":"Bearer","expires_in":3600}`},
		{200, `{"access_token":"SECRET\r\n","token_type":"Bearer","expires_in":3600}`},
		{200, `{}`}, {200, strings.Repeat("S", vertexJSONLimit+1)},
	} {
		client := &http.Client{Transport: vertexTestTransport(func(*http.Request) (*http.Response, error) { return vertexTestResponse(tc.status, tc.body), nil })}
		token, err := NewVertexDriver().Complete(context.Background(), client, s, "")
		if err == nil || strings.Contains(err.Error(), "SECRET") || token.AccessToken != "" || token.Extra != nil {
			t.Fatal("response error leaked credentials")
		}
	}
	client := &http.Client{Transport: vertexTestTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("SECRET transport error") })}
	if _, err := NewVertexDriver().Complete(context.Background(), client, s, ""); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("transport leak")
	}
}

type vertexTestBody struct {
	read   int
	closed bool
	fail   bool
}

func (b *vertexTestBody) Read(p []byte) (int, error) {
	if b.fail {
		return 0, errors.New("SECRET read error")
	}
	for i := range p {
		p[i] = 'x'
	}
	b.read += len(p)
	return len(p), nil
}
func (b *vertexTestBody) Close() error { b.closed = true; return errors.New("SECRET close error") }
func TestVertexBodyLimitCancellationAndNoEnvironment(t *testing.T) {
	s := vertexTestSession(t, "global")
	for _, fail := range []bool{false, true} {
		b := &vertexTestBody{fail: fail}
		c := &http.Client{Transport: vertexTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: b}, nil
		})}
		if _, err := NewVertexDriver().Complete(context.Background(), c, s, ""); err == nil || strings.Contains(err.Error(), "SECRET") || !b.closed || b.read > vertexJSONLimit+1 {
			t.Fatal("body boundary failed")
		}
	}
	blocked := &http.Client{Transport: vertexTestTransport(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected request"); return nil, nil })}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewVertexDriver().Complete(ctx, blocked, s, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if _, err := NewVertexDriver().Complete(context.Background(), nil, s, ""); err == nil {
		t.Fatal("nil client allowed")
	}
	if _, err := NewVertexDriver().Complete(nil, blocked, s, ""); err == nil {
		t.Fatal("nil context allowed")
	}
	s.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := NewVertexDriver().Complete(context.Background(), blocked, s, ""); !errors.Is(err, ErrLoginRequired) {
		t.Fatal("expired import accepted")
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/must-not-read-host-credentials")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "dummy-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "global")
	if _, err := NewVertexDriver().Begin(context.Background(), blocked, LoginOptions{}); err == nil {
		t.Fatal("host credential fallback")
	}
}

func TestVertexStaticEndpointValidation(t *testing.T) {
	d := NewVertexDriver()
	validator, ok := d.(interface{ ValidateEndpoint(string, string) error })
	if !ok {
		t.Fatal("missing static endpoint interface")
	}
	for _, location := range []string{"us-central1", "europe-west4", "global"} {
		s := vertexTestSession(t, location)
		base, protocol, err := d.(interface {
			Binding(Token) (string, string, error)
		}).Binding(Token{Extra: s.Extra})
		if err != nil || validator.ValidateEndpoint(base, protocol) != nil {
			t.Fatal("binding rejected by static validation")
		}
	}
	good := "https://us-central1-aiplatform.googleapis.com/v1/projects/valid-project/locations/us-central1/endpoints/openapi"
	for _, endpoint := range []string{
		"", strings.Replace(good, "https:", "http:", 1), strings.Replace(good, "googleapis.com", "googleapis.com.evil.test", 1),
		strings.Replace(good, "googleapis.com", "googleapis.com:443", 1), strings.Replace(good, "https://", "https://user:secret@", 1),
		strings.Replace(good, "us-central1-aiplatform", "europe-west4-aiplatform", 1), strings.Replace(good, "us-central1-aiplatform", "aiplatform", 1),
		strings.Replace(good, "/us-central1/", "/global/", 1), strings.Replace(good, "/valid-project/", "/../", 1),
		strings.Replace(good, "/valid-project/", "/123456789/", 1), strings.Replace(good, "/valid-project/", "/UPPERCASE/", 1),
		strings.Replace(good, "/v1/", "/v1beta1/", 1), strings.Replace(good, "/openapi", "/custom", 1),
		strings.Replace(good, "/openapi", "/%6fpenapi", 1), strings.Replace(good, "/projects/", "//projects/", 1),
		good + "/", good + "/chat/completions", good + "?", good + "?key=secret", good + "#", good + "#fragment",
		strings.Replace(good, "googleapis.com", "GOOGLEAPIS.COM", 1), strings.Replace(good, "googleapis.com", "googleapis.com.", 1),
	} {
		if err := validator.ValidateEndpoint(endpoint, "openai-chat"); err == nil {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	for _, protocol := range []string{"", "openai-responses", "anthropic", "OPENAI-CHAT"} {
		if err := validator.ValidateEndpoint(good, protocol); err == nil {
			t.Fatal("wrong protocol accepted")
		}
	}
	// Static validation intentionally allows another valid project. Only runtime
	// equality with Binding(saved Token) can authorize the configured account.
	if err := validator.ValidateEndpoint(strings.Replace(good, "valid-project", "other-project", 1), "openai-chat"); err != nil {
		t.Fatal("static validation unexpectedly depends on an account")
	}
}

// Optional contracts are explicit without forcing static-endpoint drivers to
// implement dynamic binding or weakening the manager's credential boundary.
var _ EndpointBinding = vertexDriver{}
var _ EndpointValidator = vertexDriver{}
