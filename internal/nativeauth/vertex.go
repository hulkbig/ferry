package nativeauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	vertexExchangeEndpoint = "https://oauth2.googleapis.com/token"
	vertexScope            = "https://www.googleapis.com/auth/cloud-platform"
	vertexJSONLimit        = 64 << 10
	vertexKeyLimit         = 16 << 10
)

var (
	vertexProjectPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	vertexLocationPattern = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)+[1-9][0-9]*$`)
	vertexEmailPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}@[a-z][a-z0-9-]{4,28}[a-z0-9]\.iam\.gserviceaccount\.com$`)
	vertexKeyIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

type vertexDriver struct{}

// Refresh signs a new JWT with the same service-account private key. Token
// issuance does not consume or rotate that key, including on ambiguous failure.
func (vertexDriver) RefreshIsNonRotating() bool { return true }

// NewVertexDriver imports explicitly supplied Google service-account credentials.
func NewVertexDriver() Driver { return vertexDriver{} }

// An empty BaseURL deliberately requires the optional Binding(Token) method.
func (vertexDriver) Info() Info {
	return Info{ID: "vertex", Name: "Google Vertex AI", Mode: "import", Protocol: "openai-chat"}
}

type vertexAccount struct {
	Type     string `json:"type"`
	Project  string `json:"project_id"`
	KeyID    string `json:"private_key_id,omitempty"`
	Key      string `json:"private_key"`
	Email    string `json:"client_email"`
	TokenURI string `json:"token_uri"`
	Universe string `json:"universe_domain,omitempty"`
}

// vertexDecodeObject rejects duplicate members, trailing data and oversized JSON.
// Parser errors are deliberately not wrapped: excerpts can contain private keys.
func vertexDecodeObject(raw string, out any) error {
	if len(raw) == 0 || len(raw) > vertexJSONLimit {
		return errors.New("Vertex JSON is missing or exceeds 64 KiB")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("Vertex JSON must be an object")
	}
	seen := map[string]bool{}
	for d.More() {
		member, err := d.Token()
		if err != nil {
			return errors.New("invalid Vertex JSON")
		}
		name, ok := member.(string)
		if !ok || seen[name] {
			return errors.New("invalid or duplicate Vertex JSON field")
		}
		seen[name] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return errors.New("invalid Vertex JSON")
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return errors.New("invalid Vertex JSON")
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("unexpected trailing Vertex JSON")
	}
	if err = json.Unmarshal([]byte(raw), out); err != nil {
		return errors.New("invalid Vertex JSON field type")
	}
	return nil
}

func vertexImport(raw, project, location string) (vertexAccount, *rsa.PrivateKey, error) {
	var a vertexAccount
	if err := vertexDecodeObject(raw, &a); err != nil {
		return a, nil, err
	}
	if a.Type != "service_account" {
		return a, nil, errors.New("Vertex requires type service_account")
	}
	if a.TokenURI != vertexExchangeEndpoint {
		return a, nil, errors.New("Vertex token_uri must be https://oauth2.googleapis.com/token")
	}
	if a.Universe != "" && a.Universe != "googleapis.com" {
		return a, nil, errors.New("Vertex requires the googleapis.com universe")
	}
	if !vertexProjectPattern.MatchString(a.Project) {
		return a, nil, errors.New("Vertex service account requires a valid project_id")
	}
	if !vertexProjectPattern.MatchString(project) {
		return a, nil, errors.New("Vertex requires a valid target project_id")
	}
	if !vertexValidLocation(location) {
		return a, nil, errors.New("Vertex requires an explicit valid location")
	}
	if len(a.Email) > 254 || !vertexEmailPattern.MatchString(a.Email) {
		return a, nil, errors.New("Vertex requires a service-account client_email")
	}
	if a.KeyID != "" && !vertexKeyIDPattern.MatchString(a.KeyID) {
		return a, nil, errors.New("invalid Vertex private_key_id")
	}
	if len(a.Key) == 0 || len(a.Key) > vertexKeyLimit {
		return a, nil, errors.New("Vertex private_key is missing or exceeds 16 KiB")
	}
	keyText := strings.TrimSpace(a.Key)
	if !strings.HasPrefix(keyText, "-----BEGIN PRIVATE KEY-----") && !strings.HasPrefix(keyText, "-----BEGIN RSA PRIVATE KEY-----") {
		return a, nil, errors.New("Vertex requires a PEM RSA private key")
	}
	block, rest := pem.Decode([]byte(keyText))
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 || len(block.Headers) != 0 {
		return a, nil, errors.New("invalid Vertex PEM private key")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return a, nil, errors.New("invalid Vertex PKCS8 private key")
		}
		key, _ = parsed.(*rsa.PrivateKey)
	case "RSA PRIVATE KEY":
		var err error
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return a, nil, errors.New("invalid Vertex PKCS1 private key")
		}
	}
	if key == nil || key.N.BitLen() < 2048 || key.N.BitLen() > 4096 || key.E != 65537 || len(key.Primes) != 2 {
		return a, nil, errors.New("Vertex requires a two-prime 2048–4096 bit RSA key with exponent 65537")
	}
	if err := key.Validate(); err != nil {
		return a, nil, errors.New("invalid Vertex RSA private key")
	}
	return a, key, nil
}

func (vertexDriver) Begin(ctx context.Context, _ *http.Client, o LoginOptions) (Session, error) {
	if err := vertexContext(ctx); err != nil {
		return Session{}, err
	}
	raw := o.Fields["service_account"]
	// A project override is explicit; otherwise use the imported account project.
	var account vertexAccount
	if err := vertexDecodeObject(raw, &account); err != nil {
		return Session{}, err
	}
	project := o.Fields["project_id"]
	if project == "" {
		project = account.Project
	}
	location := o.Fields["location"]
	account, _, err := vertexImport(raw, project, location)
	if err != nil {
		return Session{}, err
	}
	// Retain only fields needed for re-signing, never arbitrary imported URLs.
	canonical, _ := json.Marshal(account)
	return Session{State: o.State, Verifier: o.Verifier, ExpiresAt: time.Now().Add(10 * time.Minute), Extra: map[string]string{
		"service_account": string(canonical), "project": project, "location": location,
	}}, nil
}

func (v vertexDriver) Complete(ctx context.Context, c *http.Client, s Session, _ string) (Token, error) {
	if s.ExpiresAt.IsZero() || !time.Now().Before(s.ExpiresAt) {
		return Token{}, ErrLoginRequired
	}
	return v.Refresh(ctx, c, Token{Extra: s.Extra})
}

// Binding is an optional extension; callers must use this for both configured
// endpoint validation and every authenticated inference request. Extra is secret.
func (vertexDriver) Binding(t Token) (string, string, error) {
	_, _, err := vertexImport(t.Extra["service_account"], t.Extra["project"], t.Extra["location"])
	if err != nil {
		return "", "", err
	}
	return vertexBaseURL(t.Extra["project"], t.Extra["location"]), "openai-chat", nil
}

// ValidateEndpoint validates configuration before secret-store access. It does
// not authorize a project: runtime must compare with Binding of the saved token.
func (vertexDriver) ValidateEndpoint(baseURL, protocol string) error {
	if protocol != "openai-chat" {
		return errors.New("Vertex requires openai-chat")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return errors.New("invalid Vertex OpenAPI endpoint")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 8 || parts[0] != "" || parts[1] != "v1" || parts[2] != "projects" || parts[4] != "locations" || parts[6] != "endpoints" || parts[7] != "openapi" || !vertexProjectPattern.MatchString(parts[3]) || !vertexValidLocation(parts[5]) {
		return errors.New("invalid Vertex OpenAPI resource path")
	}
	// Exact reconstruction rejects ports, case variants, empty fragments, escapes,
	// mismatched regional hosts, suffix tricks and noncanonical trailing slashes.
	if baseURL != vertexBaseURL(parts[3], parts[5]) {
		return errors.New("untrusted Vertex OpenAPI endpoint")
	}
	return nil
}

func vertexValidLocation(location string) bool {
	return len(location) <= 63 && (location == "global" || vertexLocationPattern.MatchString(location))
}

func vertexBaseURL(project, location string) string {
	host := location + "-aiplatform.googleapis.com"
	if location == "global" {
		host = "aiplatform.googleapis.com"
	}
	return "https://" + host + "/v1/projects/" + project + "/locations/" + location + "/endpoints/openapi"
}

func vertexContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Vertex requires a context")
	}
	return ctx.Err()
}

func (vertexDriver) Refresh(ctx context.Context, c *http.Client, t Token) (Token, error) {
	if err := vertexContext(ctx); err != nil {
		return Token{}, err
	}
	if c == nil {
		return Token{}, errors.New("Vertex requires an HTTP client")
	}
	a, key, err := vertexImport(t.Extra["service_account"], t.Extra["project"], t.Extra["location"])
	if err != nil {
		return Token{}, err
	}
	issued := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if a.KeyID != "" {
		header["kid"] = a.KeyID
	}
	h, _ := json.Marshal(header)
	claims, _ := json.Marshal(struct {
		Issuer   string `json:"iss"`
		Scope    string `json:"scope"`
		Audience string `json:"aud"`
		Issued   int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}{a.Email, vertexScope, vertexExchangeEndpoint, issued.Unix(), issued.Unix() + 3600})
	unsigned := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return Token{}, errors.New("Vertex assertion signing failed")
	}
	assertion := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vertexExchangeEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, errors.New("Vertex token request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Ferry/nativeauth")
	safe := *c
	safe.Jar = nil
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safe.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Token{}, ctx.Err()
		}
		return Token{}, errors.New("Vertex token transport failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, vertexJSONLimit+1))
	if err != nil {
		return Token{}, errors.New("Vertex token response read failed")
	}
	if len(raw) > vertexJSONLimit {
		return Token{}, errors.New("Vertex token response exceeds 64 KiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Token{}, fmt.Errorf("Vertex token HTTP status %d", resp.StatusCode)
	}
	var p struct {
		Access  string          `json:"access_token"`
		Type    string          `json:"token_type"`
		Expires int64           `json:"expires_in"`
		Error   json.RawMessage `json:"error"`
	}
	if err = vertexDecodeObject(string(raw), &p); err != nil {
		return Token{}, err
	}
	if len(p.Error) > 0 || !vertexValidBearer(p.Access) || !strings.EqualFold(p.Type, "Bearer") || p.Expires <= 0 || p.Expires > 3600 {
		return Token{}, errors.New("invalid Vertex token response")
	}
	canonical, _ := json.Marshal(a)
	return Token{AccessToken: p.Access, ExpiresAt: issued.Add(time.Duration(p.Expires) * time.Second), Extra: map[string]string{
		"service_account": string(canonical), "project": t.Extra["project"], "location": t.Extra["location"],
	}}, nil
}

func vertexValidBearer(token string) bool {
	if len(token) == 0 || len(token) > 16<<10 {
		return false
	}
	for i := range token {
		if token[i] <= 32 || token[i] >= 127 {
			return false
		}
	}
	return true
}
