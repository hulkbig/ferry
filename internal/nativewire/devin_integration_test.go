package nativewire_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/nativeauth"
	"ferry/internal/nativewire"
)

type devinIntegrationRT func(*http.Request) (*http.Response, error)

func (f devinIntegrationRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func devinIntegrationEnvelope(flag byte, p []byte) []byte {
	size := len(p)
	if size < 0 || size > 16<<20 {
		return nil
	}
	b := make([]byte, 5)
	b[0] = flag
	binary.BigEndian.PutUint32(b[1:], uint32(size))
	return append(b, p...)
}

// Exercises the actual existing caller, not a substitute Chat parser. Every
// endpoint, including login, is intercepted in memory and never reaches DNS.
func TestDevinDriverThroughExistingChatCaller(t *testing.T) {
	ctx := context.Background()
	driver := nativeauth.NewDevinDriver()
	session, e := driver.Begin(ctx, nil, nativeauth.LoginOptions{State: "state", Verifier: strings.Repeat("v", 43)})
	if e != nil {
		t.Fatal(e)
	}
	loginClient := &http.Client{Transport: devinIntegrationRT(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.devin.ai" {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"token":"offline-account"}`))}, nil
	})}
	token, e := driver.Complete(ctx, loginClient, session, "offline-code")
	if e != nil {
		t.Fatal(e)
	}
	headers := map[string]string{
		nativewire.DevinClientNameHeader: token.Extra["client_name"], nativewire.DevinClientVersionHeader: token.Extra["client_version"], nativewire.DevinClientOSHeader: token.Extra["client_os"], nativewire.DevinFingerprintHeader: token.Extra["device_fingerprint"],
	}
	// Field 3 text, field 7 usage {2:7,3:2}, field 5 STOP(2), then trailer.
	payload := []byte{0x1a, 2, 'o', 'k', 0x3a, 4, 0x10, 7, 0x18, 2, 0x28, 2}
	wire := append(devinIntegrationEnvelope(0, payload), devinIntegrationEnvelope(2, []byte(`{}`))...)
	calls := 0
	transport := nativewire.NewDevinTransport(devinIntegrationRT(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "server.codeium.com" || !strings.HasSuffix(r.URL.Path, "/GetChatMessage") {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/connect+proto"}}, Body: io.NopCloser(bytes.NewReader(wire))}, nil
	}))
	caller := egress.NewOpenAIChat(egress.Base{HTTP: &http.Client{Transport: transport}, Endpoint: &domain.Endpoint{BaseURL: driver.Info().BaseURL, Protocol: domain.ProtocolOpenAIChat}, Secret: token.AccessToken, ExtraHeaders: headers, Model: "exact-catalog-uid"})
	rc := &domain.RequestContract{Origin: domain.ProtocolOpenAIChat, Protocol: domain.ProtocolOpenAIChat, Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hello"}}}}}
	response, failure := caller.Invoke(ctx, rc)
	if failure != nil {
		t.Fatal(failure)
	}
	if !bytes.Contains(response.NativeBody, []byte(`"content":"ok"`)) || len(response.ConversionIssues) != 0 {
		t.Fatalf("response mismatch: %+v", response)
	}
	rc.Stream = true
	events, cancel, failure := caller.Stream(ctx, rc)
	if failure != nil {
		t.Fatal(failure)
	}
	defer cancel()
	sawEnd := false
	sawUsage := false
	for event := range events {
		if event.Kind == domain.EvError {
			t.Fatal(event.Err)
		}
		if event.Kind == domain.EvEnd {
			sawEnd = true
		}
		if event.Usage != nil {
			sawUsage = true
		}
	}
	if !sawEnd || !sawUsage || calls != 2 {
		t.Fatalf("end=%v usage=%v calls=%d", sawEnd, sawUsage, calls)
	}
	// A passed-through unsupported field must not reach even the fake upstream.
	rc.Stream = false
	rc.Native = json.RawMessage(`{"model":"x","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema"}}`)
	if _, f := caller.Invoke(ctx, rc); f == nil || calls != 2 {
		t.Fatalf("unsupported request was sent: failure=%v calls=%d", f, calls)
	}
}
