package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
)

const codexInvalidEffortMessage = "Invalid value: 'ultra'. Supported values are: 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', and 'max'."

func TestCodexFailureClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantType   domain.FailureType
		wantScope  domain.FailureScope
		wantStatus int
		wantRetry  time.Duration
	}{
		{
			name: "usage limit", body: `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"limit","resets_in_seconds":60}}}`,
			wantType: domain.FailQuotaExhausted, wantScope: domain.FailureScopeConnection,
			wantStatus: http.StatusTooManyRequests, wantRetry: time.Minute,
		},
		{
			name: "model capacity", body: `{"error":{"message":"The selected model is at capacity. Please try a different model.","retry_after":3}}`,
			wantType: domain.FailRateLimit, wantScope: domain.FailureScopeConnection,
			wantStatus: http.StatusTooManyRequests, wantRetry: 3 * time.Second,
		},
		{
			name: "terminal authentication", body: `{"type":"error","error":{"type":"authentication_error","code":"invalid_api_key","message":"expired"}}`,
			wantType: domain.FailAuth, wantScope: domain.FailureScopeConnection, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "embedded status", body: `{"type":"error","status":401,"message":"token rejected"}`,
			wantType: domain.FailAuth, wantScope: domain.FailureScopeConnection, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "context", status: http.StatusBadRequest, body: `{"error":{"code":"context_length_exceeded","message":"context window"}}`,
			wantType: domain.FailContextOverflow, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			name: "policy", status: http.StatusForbidden, body: `{"error":{"code":"cyber_policy","message":"blocked"}}`,
			wantType: domain.FailBadResponse, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			name: "provider", body: `{"type":"response.failed","response":{"error":{"type":"upstream_error","message":"melted"}}}`,
			wantType: domain.FailProvider5xx, wantScope: domain.FailureScopeConnection, wantStatus: http.StatusBadGateway,
		},
		{
			// Codex echoes request content into invalid_request_error messages.
			// Escalating that to auth scope would force a pointless refresh
			// token rotation and account switch on a pure request error.
			name: "echoed auth words stay request scope", status: http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid value for 'input': unauthorized access to the rate limit dashboard"}}`,
			wantType: domain.FailBadResponse, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			name: "echoed forbidden stays request scope", status: http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","message":"tool 'forbidden' is not defined"}}`,
			wantType: domain.FailBadResponse, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			// A transport-level 401 is the provider's own channel verdict and
			// still outranks an invalid_request_error body.
			name: "transport 401 outranks request error", status: http.StatusUnauthorized,
			body:     `{"error":{"type":"invalid_request_error","message":"missing field"}}`,
			wantType: domain.FailAuth, wantScope: domain.FailureScopeConnection, wantStatus: http.StatusUnauthorized,
		},
		{
			// Context overflow arrives as invalid_request_error; it must keep
			// its own request-scope type rather than collapsing to bad response.
			name: "context overflow inside request error", status: http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","message":"prompt is too long for this model"}}`,
			wantType: domain.FailContextOverflow, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			name: "unsupported parameter is target scoped", status: http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","message":"temperature is not supported"}}`,
			wantType: domain.FailUnsupportedParam, wantScope: domain.FailureScopeTarget, wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid reasoning effort is request scoped", status: http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"reasoning.effort","message":"` + codexInvalidEffortMessage + `"}}`,
			wantType: domain.FailUnsupportedParam, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			name: "plain invalid enum is request scoped", status: http.StatusBadRequest,
			body:     codexInvalidEffortMessage,
			wantType: domain.FailUnsupportedParam, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid enum overrides generic unsupported scope", status: http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","message":"` + codexInvalidEffortMessage + `"}}`,
			wantType: domain.FailUnsupportedParam, wantScope: domain.FailureScopeRequest, wantStatus: http.StatusBadRequest,
		},
		{
			name: "server status outranks enum message", status: http.StatusInternalServerError,
			body:     `{"error":{"message":"` + codexInvalidEffortMessage + `"}}`,
			wantType: domain.FailProvider5xx, wantScope: domain.FailureScopeConnection, wantStatus: http.StatusInternalServerError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := codexFailure(test.status, []byte(test.body))
			if failure.Type != test.wantType || failure.Scope != test.wantScope || failure.HTTPStatus != test.wantStatus {
				t.Fatalf("failure=%+v", failure)
			}
			if failure.RetryAfter != test.wantRetry {
				t.Fatalf("retry_after=%v want %v", failure.RetryAfter, test.wantRetry)
			}
		})
	}
}

func TestCodexInvalidEffortHTTPFailureDoesNotRetryOrLowerEffort(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "invoke"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			capture := newCodexCapture("")
			defer capture.close()
			capture.nonSSE = true
			capture.status = http.StatusBadRequest
			capture.jsonBody = `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"reasoning.effort","message":"` + codexInvalidEffortMessage + `"}}`
			caller := NewCodexCaller(codexBase(t, capture), "")
			caller.Model = "gpt-6-astra"
			ctx := domain.WithFirstOutputObserver(context.Background(), func() { t.Error("HTTP error counted as first output") })
			request := &domain.RequestContract{
				Model: "gpt-6-astra", Origin: domain.ProtocolOpenAIResponses,
				Native: json.RawMessage(`{"model":"gpt-6-astra","input":"test","reasoning":{"effort":"ultra"}}`),
			}
			var failure *domain.Failure
			if stream {
				_, cancel, f := caller.Stream(ctx, request)
				if cancel != nil {
					defer cancel()
				}
				failure = f
			} else {
				_, failure = caller.Invoke(ctx, request)
			}
			if failure == nil || failure.Type != domain.FailUnsupportedParam ||
				failure.Scope != domain.FailureScopeRequest || failure.HTTPStatus != http.StatusBadRequest ||
				failure.Message != codexInvalidEffortMessage || failure.RetryAfter != 0 {
				t.Fatalf("failure=%+v", failure)
			}
			if len(capture.requests) != 1 {
				t.Fatalf("HTTP requests=%d, want one without retry", len(capture.requests))
			}
			var body struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.Unmarshal([]byte(capture.bodyRaw), &body); err != nil || body.Reasoning.Effort != "ultra" {
				t.Fatalf("reasoning effort changed: effort=%q err=%v", body.Reasoning.Effort, err)
			}
		})
	}
}

func TestCodexTerminalDecoderIgnoresNonTerminalAndClassifiesCancellation(t *testing.T) {
	for _, event := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"type":"response.output_text.delta","delta":"hello"}`),
	} {
		if failure := codexFailureFromEvent(event); failure != nil {
			t.Fatalf("non-terminal event classified as failure: %+v", failure)
		}
	}
	failure := codexFailureFromEvent([]byte(`{"type":"response.canceled"}`))
	if failure == nil || failure.Type != domain.FailStreamBroken || failure.Scope != domain.FailureScopeStream {
		t.Fatalf("cancellation failure=%+v", failure)
	}
}

func TestCodexInvokeTerminalFailurePreservesQuotaSemantics(t *testing.T) {
	capture := newCodexCapture("event: response.failed\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"limit\",\"resets_in_seconds\":30}}}\n\n")
	defer capture.close()
	caller := NewCodexCaller(codexBase(t, capture), "acct")
	_, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Model: "gpt-5.5", Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "x"}}}},
	})
	if failure == nil || failure.Type != domain.FailQuotaExhausted ||
		failure.Scope != domain.FailureScopeConnection || failure.RetryAfter != 30*time.Second {
		t.Fatalf("failure=%+v", failure)
	}
}

func TestCodexTerminalFailureRedactsAccessToken(t *testing.T) {
	capture := newCodexCapture("event: response.failed\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"authentication_error\",\"message\":\"token at-test expired\"}}}\n\n")
	defer capture.close()
	caller := NewCodexCaller(codexBase(t, capture), "acct")
	_, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Model: "gpt-5.5", Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "x"}}}},
	})
	if failure == nil || strings.Contains(failure.Message, "at-test") || !strings.Contains(failure.Message, "[REDACTED]") {
		t.Fatalf("unsafe failure=%+v", failure)
	}
}

func TestCodexHTTPFailureUsesProviderDecoder(t *testing.T) {
	capture := newCodexCapture("")
	defer capture.close()
	capture.nonSSE = true
	capture.status = http.StatusTooManyRequests
	capture.jsonBody = `{"error":{"type":"usage_limit_reached","message":"limit","resets_in_seconds":45}}`
	caller := NewCodexCaller(codexBase(t, capture), "acct")
	_, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Model: "gpt-5.5", Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "x"}}}},
	})
	if failure == nil || failure.Type != domain.FailQuotaExhausted || failure.RetryAfter != 45*time.Second {
		t.Fatalf("failure=%+v", failure)
	}
}
