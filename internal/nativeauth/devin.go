/*
Native protocol integration based on CLIProxyAPI d946c4f, internal/auth/devin/devin_auth.go and record.go.
Source: https://github.com/router-for-me/CLIProxyAPI/tree/d946c4f
Narrow adaptation; no upstream SDK/runtime dependency.

# MIT License

Copyright (c) 2025-2005.9 Luis Pater
Copyright (c) 2025.9-present Router-For.ME

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/
package nativeauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"ferry/internal/buildinfo"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// Devin's native PKCE contract was inspected in CLIProxyAPI d946c4f,
// internal/auth/devin/devin_auth.go. This driver does not start a browser or
// listener. The manager owns one-shot session storage and callback submission.
type devinDriver struct{}

func NewDevinDriver() Driver { return &devinDriver{} }
func (*devinDriver) Info() Info {
	return Info{ID: "devin", Name: "Devin", Mode: "browser", BaseURL: "https://server.codeium.com", Protocol: "openai-chat"}
}
func devinSafe(s string) bool {
	if s == "" || len(s) > 16384 {
		return false
	}
	for _, c := range s {
		if c <= 32 || c == 127 {
			return false
		}
	}
	return true
}
func devinVerifier(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
			return false
		}
	}
	return true
}
func (*devinDriver) Begin(ctx context.Context, _ *http.Client, opts LoginOptions) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if !devinSafe(opts.State) || !devinVerifier(opts.Verifier) {
		return Session{}, errors.New("devin: state and valid PKCE verifier required")
	}
	hash := sha256.Sum256([]byte(opts.Verifier))
	q := url.Values{"state": {opts.State}, "prompt": {"select_account"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "cli_pkce_marker": {"1"}}
	extra, err := devinOwnMetadata(nil)
	if err != nil {
		return Session{}, err
	}

	return Session{State: opts.State, Verifier: opts.Verifier, URL: "https://app.devin.ai/auth/cli/continue?" + q.Encode(), ExpiresAt: time.Now().Add(10 * time.Minute), Extra: extra}, nil
}
func (*devinDriver) Complete(ctx context.Context, client *http.Client, s Session, callback string) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	if !s.ExpiresAt.After(time.Now()) || !devinVerifier(s.Verifier) || !devinSafe(s.State) {
		return Token{}, ErrLoginRequired
	}
	code := strings.TrimSpace(callback)
	// This driver uses manual code mode (no redirect_uri). A bare code is bound
	// to the stored verifier; query-form completion must additionally match state.
	if strings.HasPrefix(code, "code=") || strings.HasPrefix(code, "state=") || strings.Contains(code, "&") || strings.HasPrefix(code, "?") {
		q, err := url.ParseQuery(strings.TrimPrefix(code, "?"))
		if err != nil || len(q["code"]) != 1 || len(q["state"]) != 1 || q.Has("error") || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(s.State)) != 1 {
			return Token{}, errors.New("devin: invalid authorization completion")
		}
		code = q.Get("code")
	}
	if !devinSafe(code) || strings.Contains(code, "://") {
		return Token{}, errors.New("devin: invalid authorization code")
	}
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": s.Verifier})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.devin.ai/auth/cli/token", bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Token{}, ctx.Err()
		}
		return Token{}, errors.New("devin: token exchange transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Token{}, fmt.Errorf("devin: token exchange status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return Token{}, errors.New("devin: invalid token response")
	}
	var result struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &result) != nil || !devinSafe(result.Token) {
		return Token{}, errors.New("devin: missing session token")
	}
	token := result.Token
	if !strings.HasPrefix(token, "devin-session-token$") {
		token = "devin-session-token$" + token
	}
	if token == "devin-session-token$" {
		return Token{}, errors.New("devin: empty session token")
	}
	sum := sha256.Sum256([]byte(token))
	extra, err := devinOwnMetadata(s.Extra)
	if err != nil {
		return Token{}, err
	}

	// A stable secret-derived local identity avoids account collisions when no
	// profile call is made. It is not represented as a provider user ID.
	return Token{AccessToken: token, Identity: "devin:" + hex.EncodeToString(sum[:16]), Extra: extra}, nil
}

// Devin provides no refresh grant. Do not return a stale token as a successful
// refresh after a 401; the caller must prompt for a new login.
func (*devinDriver) Refresh(ctx context.Context, _ *http.Client, _ Token) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	return Token{}, ErrLoginRequired
}

// Token.Extra contract for main's trusted transport injection:
// client_name -> X-Smartapi-Devin-Client-Name
// client_version -> X-Smartapi-Devin-Client-Version
// client_os -> X-Smartapi-Devin-Client-OS
// device_fingerprint -> X-Smartapi-Devin-Device-Fingerprint
// This is a self-owned random protocol identifier, NOT provider attestation or
// another application's fingerprint. Generate once per login session; persist
// unchanged with the account token and reuse across inference requests.
func devinOwnMetadata(existing map[string]string) (map[string]string, error) {
	fp := existing["device_fingerprint"]
	if fp == "" {
		var id [366]byte
		if _, err := rand.Read(id[:]); err != nil {
			return nil, errors.New("devin: generate local device identity failed")
		}
		fp = hex.EncodeToString(id[:])
	} else {
		if len(fp) != 732 {
			return nil, errors.New("devin: corrupt persisted device identity")
		}
		if _, err := hex.DecodeString(fp); err != nil {
			return nil, errors.New("devin: corrupt persisted device identity")
		}
	}
	info := buildinfo.Current()
	if !devinSafe(info.Version) {
		return nil, errors.New("devin: invalid Ferry build version")
	}
	name := existing["client_name"]
	if name == "" {
		name = info.Service
	}
	if name != "ferry" && name != "smart-api" {
		return nil, errors.New("devin: invalid persisted client identity")
	}
	return map[string]string{"client_name": name, "client_version": info.Version, "client_os": runtime.GOOS, "device_fingerprint": fp}, nil
}
