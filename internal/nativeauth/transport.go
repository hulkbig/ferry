package nativeauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"ferry/internal/nativewire"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type nativeTransport struct {
	manager *Manager
	ref     string
	base    http.RoundTripper
}

func (t *nativeTransport) RoundTrip(req *http.Request) (out *http.Response, outErr error) {
	driver, _, ok := ParseRef(t.ref)
	if !ok {
		return nil, ErrLoginRequired
	}
	info, err := t.manager.boundInfo(t.ref)
	if err != nil {
		return nil, ErrLoginRequired
	}
	// Validate before materializing credentials. Paths may only be descendants of
	// the registered API root; redirect following is disabled by ClientFor.
	root := strings.TrimRight(info.BaseURL, "/")
	if req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" || !strings.HasPrefix(req.URL.String(), root+"/") {
		return nil, fmt.Errorf("native request escaped provider endpoint")
	}
	token, err := t.manager.Identity(req.Context(), t.ref, "")
	if err != nil {
		return nil, err
	}
	// Run outside the native wire adapter and after every return path, including
	// a retained first 401 or a second attempt that echoes either credential.
	attempted := []string{token.AccessToken}
	defer func() { out, outErr = redactNativeOutcome(out, outErr, attempted) }()
	if d, ok := t.manager.Driver(driver); ok {
		if dynamic, ok := d.(EndpointBinding); ok {
			bound, _, err := dynamic.Binding(token)
			if err != nil || bound != info.BaseURL {
				return nil, fmt.Errorf("native account endpoint changed")
			}
		}
	}
	next := req.Clone(req.Context())
	next.Header = req.Header.Clone()
	next.Header.Del("X-Api-Key")
	next.Header.Del("Cookie")
	next.Header.Set("Authorization", "Bearer "+token.AccessToken)
	next.Header.Set("User-Agent", "Ferry/native")
	if driver == "kimi" || driver == "kimi-ai" {
		next.Header.Set("X-Msh-Device-Id", token.Extra["device_id"])
		next.Header.Set("X-Msh-Platform", "Ferry")
		next.Header.Set("X-Msh-Version", "native")
		next.Header.Set("X-Msh-Device-Name", "Ferry")
		next.Header.Set("X-Msh-Device-Model", runtime.GOOS+" "+runtime.GOARCH)
	}
	if driver == "claude" {
		next.Header.Set("Anthropic-Version", "2023-06-01")
		beta := next.Header.Get("Anthropic-Beta")
		if !strings.Contains(beta, "oauth-2025-04-20") {
			if beta != "" {
				beta += ","
			}
			beta += "oauth-2025-04-20"
		}
		next.Header.Set("Anthropic-Beta", beta)
		target := *next.URL
		query := target.Query()
		query.Set("beta", "true")
		target.RawQuery = query.Encode()
		next.URL = &target
	}
	wire := t.base
	if driver == "antigravity" {
		wire = nativewire.NewAntigravityTransport(wire, token.Extra["project_id"])
	}
	if driver == "devin" {
		next.Header.Set(nativewire.DevinClientNameHeader, token.Extra["client_name"])
		next.Header.Set(nativewire.DevinClientVersionHeader, token.Extra["client_version"])
		next.Header.Set(nativewire.DevinClientOSHeader, token.Extra["client_os"])
		next.Header.Set(nativewire.DevinFingerprintHeader, token.Extra["device_fingerprint"])
		wire = nativewire.NewDevinTransport(wire)
	}
	response, err := wire.RoundTrip(next)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized || req.GetBody == nil {
		return response, err
	}
	refreshed, refreshErr := t.manager.Identity(req.Context(), t.ref, token.AccessToken)
	if refreshErr != nil || refreshed.BindingID != token.BindingID {
		return response, nil
	}
	body, bodyErr := req.GetBody()
	if bodyErr != nil {
		return response, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	retry := next.Clone(req.Context())
	retry.Body = body
	retry.Header = next.Header.Clone()
	retry.Header.Set("Authorization", "Bearer "+refreshed.AccessToken)
	attempted = append(attempted, refreshed.AccessToken)
	return wire.RoundTrip(retry)
}

const nativeErrorBodyLimit = 64 << 10

// Keep useful diagnostics and standard cancellation/timeout semantics without
// retaining an unwrap-able upstream error containing credential material.
type nativeRedactedError struct {
	message            string
	cause              error
	timeout, temporary bool
}

func (e *nativeRedactedError) Error() string   { return e.message }
func (e *nativeRedactedError) Unwrap() error   { return e.cause }
func (e *nativeRedactedError) Timeout() bool   { return e.timeout }
func (e *nativeRedactedError) Temporary() bool { return e.temporary }

func redactNativeError(err error, replacer *strings.Replacer) error {
	if err == nil || err == io.EOF {
		return err
	}
	clean := &nativeRedactedError{message: replacer.Replace(err.Error())}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		if errors.Is(err, cause) {
			clean.cause = cause
			break
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		clean.timeout, clean.temporary = networkError.Timeout(), networkError.Temporary()
	}
	return clean
}

type nativeRedactedBody struct {
	body     io.ReadCloser
	replacer *strings.Replacer
}

func (b *nativeRedactedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	return n, redactNativeError(err, b.replacer)
}
func (b *nativeRedactedBody) Close() error { return redactNativeError(b.body.Close(), b.replacer) }

func nativeTokenReplacer(tokens []string) *strings.Replacer {
	variants := map[string]bool{}
	for _, token := range tokens {
		if token == "" {
			continue
		}
		// Devin currently sends a literal token-token Basic value. Also cover
		// standard Basic base64(token:token) and encoded echoes of the wire value.
		for _, material := range []string{token, token + "-" + token, base64.StdEncoding.EncodeToString([]byte(token + ":" + token)), base64.StdEncoding.EncodeToString([]byte(token + "-" + token))} {
			encoded, _ := json.Marshal(material)
			for _, value := range []string{material, url.QueryEscape(material), url.PathEscape(material), string(encoded[1 : len(encoded)-1])} {
				variants[value] = true
			}
		}
	}
	values := make([]string, 0, len(variants))
	for value := range variants {
		values = append(values, value)
	}
	// A rotated token may contain the old one as a prefix. Replace the longest
	// match first and never run replacements recursively over the marker.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pairs := make([]string, 0, 2*len(values))
	for _, value := range values {
		pairs = append(pairs, value, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...)
}

func redactNativeOutcome(resp *http.Response, err error, tokens []string) (*http.Response, error) {
	replacer := nativeTokenReplacer(tokens)
	if err != nil {
		// RoundTripper callers must ignore a response returned alongside an error.
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, redactNativeError(err, replacer)
	}
	if resp == nil {
		return nil, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Preserve successful bytes and streaming/backpressure; only sanitize
		// deferred read/close errors, which otherwise bypass the RoundTrip guard.
		if resp.Body == nil {
			return resp, nil
		}
		clean := *resp
		clean.Body = &nativeRedactedBody{body: resp.Body, replacer: replacer}
		return &clean, nil
	}
	var raw []byte
	var readErr error
	if resp.Body != nil {
		raw, readErr = io.ReadAll(io.LimitReader(resp.Body, nativeErrorBodyLimit+1))
		_ = resp.Body.Close()
	}
	clean := *resp
	clean.Header = resp.Header.Clone()
	clean.Trailer = resp.Trailer.Clone()
	for _, headers := range []http.Header{clean.Header, clean.Trailer} {
		for name, values := range headers {
			for i, value := range values {
				values[i] = replacer.Replace(value)
			}
			headers[name] = values
		}
	}
	clean.Status = replacer.Replace(resp.Status)
	body := ""
	switch {
	case resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity":
		body = `{"error":{"message":"native upstream error body encoding is unsupported"}}`
	case readErr != nil:
		body = `{"error":{"message":"native upstream error body could not be read"}}`
	case len(raw) > nativeErrorBodyLimit:
		// Never expose a truncated credential prefix at the read boundary.
		body = `{"error":{"message":"native upstream error body exceeded limit"}}`
	default:
		body = replacer.Replace(string(raw))
	}
	if clean.Header == nil {
		clean.Header = make(http.Header)
	}
	if readErr != nil || len(raw) > nativeErrorBodyLimit || (resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		clean.Header.Set("Content-Type", "application/json")
	}
	clean.Body = io.NopCloser(strings.NewReader(body))
	clean.ContentLength = int64(len(body))
	clean.TransferEncoding = nil
	clean.Header.Del("Transfer-Encoding")
	clean.Header.Del("Content-Encoding")
	clean.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return &clean, nil
}

// ClientFor preserves the caller's transport/timeouts but never redirects an
// OAuth request. Each attempt resolves its credential immediately before I/O.
func (m *Manager) ClientFor(ref string, client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &nativeTransport{manager: m, ref: ref, base: base}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
