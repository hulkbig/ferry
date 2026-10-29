package management

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"ferry/internal/domain"
)

// Opt-in local visual fixture. Neither data nor authentication is shared with
// production. Mutations other than fixture login/logout are rejected.
func TestConsoleNextPreview(t *testing.T) {
	if os.Getenv("FERRY_CONSOLE_PREVIEW") != "1" {
		t.Skip("local visual preview only")
	}
	f := newV2APIFixture(t)
	now := time.Now()
	for i := 0; i < 96; i++ {
		id := fmt.Sprintf("preview-%d", i)
		ts := now.Add(-time.Duration(i*863+10) * time.Second)
		status := domain.ReqSuccess
		outcome := domain.OutcomeDirectSuccess
		if i%19 == 0 {
			status = domain.ReqFailed
			outcome = domain.OutcomeFailed
		}
		latency := int64(240 + i%33*41)
		f.ledger.BeginRequest(domain.RequestRecord{ID: id, Ts: ts, LogicalModelID: "smart", ModelRequested: "smart", ProviderID: "provider-a", Status: status, Outcome: outcome, TTFTMS: latency, DurationMS: latency * 5})
		f.ledger.AddAttempt(domain.AttemptRecord{RequestID: id, Seq: 1, Ts: ts, ProviderID: "provider-a", ConnectionID: "connection-a", Protocol: domain.ProtocolOpenAIChat, Status: status, PlanKind: domain.PlanPAYG, RateCardVersion: "preview-only", EstCashCost: float64(i%13+1) * .014, TTFTMS: latency, LatencyMS: latency * 5, Usage: domain.Usage{InputTokens: 1500, CacheReadTokens: 500, OutputTokens: 800}})
		f.ledger.FinishRequest(id, nil)
	}
	f.server.deps.Security = f.control
	init, _ := f.do(http.MethodPost, "/api/auth/initialize", map[string]string{"username": "preview", "password": "local-preview-only-2026"})
	if init.Code != 201 {
		t.Fatalf("initialize preview: %d", init.Code)
	}
	handler := f.server.Routes()
	server := &http.Server{Addr: "127.0.0.1:4187", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.URL.Path != "/api/auth/login" && r.URL.Path != "/api/auth/logout" {
			http.Error(w, "local preview is read only", http.StatusMethodNotAllowed)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	t.Cleanup(func() { _ = server.Close() })
	t.Log("Local fixture: http://localhost:4187 — preview / local-preview-only-2026")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
