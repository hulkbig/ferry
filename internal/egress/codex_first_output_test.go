package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/domain"
)

const codexEmptyCompletion = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"output_tokens\":1}}}\n\n"

func TestCodexInvokeObservesOutputBeforeReassemblyCompletes(t *testing.T) {
	tests := []struct {
		name, output, remaining string
	}{
		{"text", `{"type":"response.output_text.delta","delta":"answer"}`, ""},
		{"reasoning", `{"type":"response.reasoning_text.delta","delta":"thinking"}`, ""},
		{"reasoning summary", `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, ""},
		{"refusal", `{"type":"response.refusal.delta","delta":"refused"}`, ""},
		{"tool identity", `{"type":"response.output_item.added","item":{"type":"function_call","id":"fc","call_id":"call","name":"search","arguments":""}}`,
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc\",\"call_id\":\"call\",\"name\":\"search\",\"arguments\":\"{}\",\"status\":\"completed\"}}\n\n"},
		{"opaque reasoning", `{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs","encrypted_content":"opaque","summary":[],"status":"completed"}}`, ""},
		{"done-only text", `{"type":"response.output_text.done","text":"answer"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finish := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp\"}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp\"}}\n\n")
				fmt.Fprintf(w, "data: %s\n\n", tt.output)
				w.(http.Flusher).Flush()
				// Keep the non-streaming Invoke pending after upstream output.
				// An observer fired only on completion cannot release this gate.
				select {
				case <-finish:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, tt.remaining, codexEmptyCompletion)
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			observed := make(chan struct{})
			var calls atomic.Int32
			ctx = domain.WithFirstOutputObserver(ctx, func() {
				if calls.Add(1) == 1 {
					close(observed)
				}
			})
			caller := NewCodexCaller(Base{
				HTTP: server.Client(), Model: "gpt-6-astra",
				Endpoint: &domain.Endpoint{BaseURL: server.URL, Protocol: domain.ProtocolOpenAIResponses},
			}, "")
			type result struct {
				response *domain.Response
				failure  *domain.Failure
			}
			completed := make(chan result, 1)
			go func() {
				response, failure := caller.Invoke(ctx, &domain.RequestContract{
					Model: "gpt-6-astra", Origin: domain.ProtocolOpenAIResponses,
					Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "test"}}}},
				})
				completed <- result{response, failure}
			}()
			select {
			case <-observed:
			case <-ctx.Done():
				t.Fatal("meaningful SSE output did not notify before response completion")
			case got := <-completed:
				t.Fatalf("Invoke ended before first output was observed: %v", got.failure)
			}
			select {
			case <-completed:
				t.Fatal("Invoke returned before the terminal frame")
			default:
			}
			close(finish)
			select {
			case got := <-completed:
				if got.failure != nil || got.response == nil {
					t.Fatalf("Invoke failed after output observation: %v", got.failure)
				}
			case <-ctx.Done():
				t.Fatal("Invoke failed to finish after the terminal frame")
			}
			if calls.Load() != 1 {
				t.Fatalf("observed output %d times, want once", calls.Load())
			}
		})
	}
}

func TestCodexInvokeDoesNotObserveHeadersLifecycleOrEmptyOutput(t *testing.T) {
	tests := []struct {
		name, stream string
		wantFailure  bool
	}{
		{"headers only", "", true},
		{"created", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp\"}}\n\n" + codexEmptyCompletion, false},
		{"in progress", "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp\"}}\n\n" + codexEmptyCompletion, false},
		{"empty delta", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n" + codexEmptyCompletion, false},
		{"empty reasoning", "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"\"}\n\n" + codexEmptyCompletion, false},
		{"empty item", "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"m\",\"role\":\"assistant\",\"content\":[]}}\n\n" + codexEmptyCompletion, false},
		{"usage and completion", codexEmptyCompletion, false},
		{"error", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"upstream_error\",\"message\":\"failed\"}}}\n\n", true},
		{"invalid JSON", "data: not-json\n\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := newCodexCapture(tt.stream)
			defer capture.close()
			ctx := domain.WithFirstOutputObserver(context.Background(), func() { t.Error("non-output observed") })
			caller := NewCodexCaller(codexBase(t, capture), "")
			_, failure := caller.Invoke(ctx, &domain.RequestContract{
				Model: "gpt-6-astra", Origin: domain.ProtocolOpenAIResponses,
				Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "test"}}}},
			})
			if (failure != nil) != tt.wantFailure {
				t.Fatalf("failure=%v, wantFailure=%v", failure, tt.wantFailure)
			}
		})
	}
}
