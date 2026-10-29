package egress

import (
	"context"
	"ferry/internal/domain"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCodexUsageGuardBlocksAllInferencePathsAndObservesErrors(t *testing.T) {
	sent := 0
	observed := 0
	block := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"code":"usage_limit_reached"}}`))
	}))
	defer srv.Close()
	base := Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL, Protocol: domain.ProtocolOpenAIResponses}, Model: "gpt-test",
		BeforeSend: func(context.Context) *domain.Failure {
			if block {
				return &domain.Failure{Type: domain.FailQuotaExhausted, LocalQuotaProtection: true}
			}
			return nil
		},
		AfterResponse: func(h http.Header) {
			if h.Get("X-Codex-Primary-Used-Percent") != "100" {
				t.Error("missing quota header")
			}
			observed++
		},
	}
	text := NewCodexCaller(base, "acct")
	rc := &domain.RequestContract{Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIResponses, Native: []byte(`{"model":"gpt-test","input":"hi"}`)}
	if _, f := text.Invoke(context.Background(), rc); f == nil || !f.LocalQuotaProtection {
		t.Fatal(f)
	}
	if _, _, f := text.Stream(context.Background(), rc); f == nil || !f.LocalQuotaProtection {
		t.Fatal(f)
	}
	imageBase := base
	imageBase.Endpoint = &domain.Endpoint{BaseURL: srv.URL, Protocol: domain.ProtocolOpenAIImages}
	images := NewCodexImages(imageBase, "acct")
	if _, f := images.Invoke(context.Background(), &domain.RequestContract{Protocol: domain.ProtocolOpenAIImages}); f == nil || !f.LocalQuotaProtection {
		t.Fatal(f)
	}
	if sent != 0 {
		t.Fatal("blocked request reached upstream")
	}
	block = false
	_, f := text.Invoke(context.Background(), rc)
	if f == nil || sent != 1 || observed != 1 {
		t.Fatalf("sent=%d observed=%d failure=%+v", sent, observed, f)
	}
}
