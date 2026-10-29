package ingress

import (
	"context"
	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type observingEngine struct {
	stubEngine
	observed bool
	status   int
}

func (e *observingEngine) ObserveIngress(ctx context.Context, _ domain.Protocol, _ sched.RequestMeta) (context.Context, func(int)) {
	e.observed = true
	return ctx, func(status int) { e.status = status }
}

type checkingBody struct {
	io.Reader
	engine *observingEngine
	t      *testing.T
}

func (b checkingBody) Read(p []byte) (int, error) {
	if !b.engine.observed {
		b.t.Fatal("request body read before request observation")
	}
	return b.Reader.Read(p)
}
func (b checkingBody) Close() error { return nil }
func TestObservationPrecedesBodyReadAndCapturesParseFailures(t *testing.T) {
	for _, kind := range []string{"chat", "responses", "images", "anthropic"} {
		t.Run(kind, func(t *testing.T) {
			e := &observingEngine{}
			var handler http.Handler
			switch kind {
			case "chat":
				handler = &OpenAIChatHandler{Eng: e}
			case "responses":
				handler = &ResponsesHandler{Eng: e}
			case "images":
				handler = &OpenAIImagesHandler{Eng: e}
			case "anthropic":
				handler = &AnthropicHandler{Eng: e}
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
			req.Header.Set("Content-Type", "application/json")
			req.Body = checkingBody{Reader: strings.NewReader("{"), engine: e, t: t}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if !e.observed || e.status != 400 || rec.Code != 400 {
				t.Fatalf("observation=%v status=%d response=%d", e.observed, e.status, rec.Code)
			}
		})
	}
}

type streamingObservationEngine struct {
	*stubEngine
	open    atomic.Int32
	outputs atomic.Int32
	output  chan struct{}
	events  chan domain.StreamEvent
	started chan struct{}
}

type notifyingStreamWriter struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (w *notifyingStreamWriter) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func (e *streamingObservationEngine) ObserveIngress(ctx context.Context, _ domain.Protocol, _ sched.RequestMeta) (context.Context, func(int)) {
	e.open.Add(1)
	return ctx, func(int) { e.open.Add(-1) }
}

func (e *streamingObservationEngine) Execute(_ context.Context, _ *domain.RequestContract, _ sched.RequestMeta) (*sched.Result, *domain.Failure) {
	close(e.started)
	return &sched.Result{RequestID: "stream-request", Model: "test-model", Events: e.events}, nil
}

func (e *streamingObservationEngine) ObserveFirstOutput(context.Context) {
	e.outputs.Add(1)
	if e.output != nil {
		close(e.output)
	}
}

func TestObservationRemainsOpenUntilStreamHandlerReturns(t *testing.T) {
	tests := []struct {
		name, path, body string
		handler          func(Engine) http.Handler
	}{
		{"chat", "/v1/chat/completions", "{\"model\":\"test-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true}", func(e Engine) http.Handler { return &OpenAIChatHandler{Eng: e} }},
		{"responses", "/v1/responses", "{\"model\":\"test-model\",\"input\":\"hi\",\"stream\":true}", func(e Engine) http.Handler { return &ResponsesHandler{Eng: e} }},
		{"anthropic", "/v1/messages", "{\"model\":\"test-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":8,\"stream\":true}", func(e Engine) http.Handler { return &AnthropicHandler{Eng: e} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &streamingObservationEngine{stubEngine: newStub(), events: make(chan domain.StreamEvent), started: make(chan struct{}), output: make(chan struct{})}
			writer := &notifyingStreamWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 16)}
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.handler(e).ServeHTTP(writer, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			}()
			select {
			case <-e.started:
			case <-time.After(time.Second):
				t.Fatal("stream execution did not start")
			}
			if got := e.open.Load(); got != 1 {
				t.Fatalf("open requests before stream end = %d, want 1", got)
			}
			if e.outputs.Load() != 0 {
				t.Fatal("recorded first output before stream content")
			}
			select {
			case e.events <- domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: "hello"}:
			case <-time.After(time.Second):
				t.Fatal("stream handler did not read text event")
			}
			// The SSE writer flushes once for headers and again for the first frame.
			for range 2 {
				select {
				case <-writer.flushed:
				case <-time.After(time.Second):
					t.Fatal("stream frame was not flushed")
				}
			}
			if got := e.open.Load(); got != 1 {
				t.Fatalf("open requests during stream = %d, want 1", got)
			}
			select {
			case <-e.output:
			case <-time.After(time.Second):
				t.Fatal("first output was not observed during the live stream")
			}
			select {
			case <-done:
				t.Fatal("handler returned before stream ended")
			default:
			}
			select {
			case e.events <- domain.StreamEvent{Kind: domain.EvEnd}:
			case <-time.After(time.Second):
				t.Fatal("stream handler did not read end event")
			}
			close(e.events)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not return after stream end")
			}
			if got := e.open.Load(); got != 0 {
				t.Fatalf("open requests after stream end = %d, want 0", got)
			}
			if got := e.outputs.Load(); got != 1 {
				t.Fatalf("first output observations = %d, want 1", got)
			}
		})
	}
}
