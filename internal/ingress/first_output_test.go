package ingress

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ferry/internal/domain"
)

func TestFirstOutputIgnoresLifecycleAndObservesMeaningfulWireFrames(t *testing.T) {
	tests := []struct {
		name, lifecycle, output string
	}{
		{"chat text", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`, `{"choices":[{"delta":{"content":"hello"}}]}`},
		{"chat reasoning", `{"choices":[],"usage":{"output_tokens":1}}`, `{"choices":[{"delta":{"reasoning_content":"think"}}]}`},
		{"chat tool", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `{"choices":[{"delta":{"tool_calls":[{"id":"call-1","function":{"name":"search"}}]}}]}`},
		{"responses text", `{"type":"response.created","response":{"id":"r"}}`, `{"type":"response.output_text.delta","delta":"hello"}`},
		{"responses reasoning", `{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}`, `{"type":"response.reasoning_summary_text.delta","delta":"think"}`},
		{"responses tool", `{"type":"response.output_item.added","item":{"type":"message","id":"m","content":[]}}`, `{"type":"response.output_item.added","item":{"type":"function_call","id":"fc","name":"search"}}`},
		{"responses terminal content", `{"type":"response.completed","response":{"output":[]}}`, `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}`},
		{"anthropic text", `{"type":"message_start","message":{"id":"m","content":[]}}`, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`},
		{"anthropic reasoning", `{"type":"content_block_start","content_block":{"type":"thinking","thinking":""}}`, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"think"}}`},
		{"anthropic tool", `{"type":"ping"}`, `{"type":"content_block_start","content_block":{"type":"tool_use","id":"tool-1","name":"search","input":{}}}`},
		{"anthropic redacted", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, `{"type":"content_block_start","content_block":{"type":"redacted_thinking","data":"opaque"}}`},
	}
	for _, tc := range tests {
		for _, native := range []bool{false, true} {
			name := tc.name + "/converted"
			if native {
				name = tc.name + "/native"
			}
			t.Run(name, func(t *testing.T) {
				writer := httptest.NewRecorder()
				observed := 0
				w := &observedWriter{ResponseWriter: writer, onFirstOutput: func() {
					if !writer.Flushed {
						t.Error("observed before flush")
					}
					observed++
				}}
				sse := newSSEWriter(w)
				emit := func(payload string) {
					if native {
						sse.rawFrame(&domain.NativeSSEFrame{Data: payload})
					} else {
						sse.data(json.RawMessage(payload))
					}
				}
				emit(tc.lifecycle)
				emit(`{"type":"error","error":{"message":"failed"}}`)
				sse.done()
				if observed != 0 {
					t.Fatal("lifecycle/error/terminal counted as output")
				}
				emit(tc.output)
				emit(tc.output)
				if observed != 1 {
					t.Fatalf("first output observed %d times", observed)
				}
			})
		}
	}
}

type failedDeliveryWriter struct {
	*httptest.ResponseRecorder
	writeFailure, flushFailure bool
}

func (w *failedDeliveryWriter) Write(b []byte) (int, error) {
	if w.writeFailure {
		return 0, errors.New("client disconnected")
	}
	return w.ResponseRecorder.Write(b)
}
func (w *failedDeliveryWriter) FlushError() error {
	if w.flushFailure {
		return errors.New("client disconnected")
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestFirstOutputRequiresSuccessfulDelivery(t *testing.T) {
	for _, failure := range []string{"write", "flush"} {
		t.Run(failure, func(t *testing.T) {
			writer := &failedDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), writeFailure: failure == "write", flushFailure: failure == "flush"}
			w := &observedWriter{ResponseWriter: writer, onFirstOutput: func() { t.Error("failed write counted as client output") }}
			newSSEWriter(w).data(json.RawMessage(`{"choices":[{"delta":{"content":"hello"}}]}`))
		})
	}
}

func TestFirstOutputForNonstreamSuccessOnly(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		observed := 0
		w := &observedWriter{ResponseWriter: httptest.NewRecorder(), onFirstOutput: func() { observed++ }}
		writeJSON(w, status, map[string]string{"message": "response"})
		want := 0
		if status == http.StatusOK {
			want = 1
		}
		if observed != want {
			t.Fatalf("status %d: observations %d, want %d", status, observed, want)
		}
	}
}
