package ingress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
)

type phaseObservation struct {
	name string
	at   time.Time
	err  error
}

type phaseObservingEngine struct {
	*stubEngine
	phases []phaseObservation
	posted chan phaseObservation
}

func (e *phaseObservingEngine) ObserveIngressPhase(_ context.Context, phase string, at time.Time, err error) {
	event := phaseObservation{phase, at, err}
	e.phases = append(e.phases, event)
	if e.posted != nil {
		e.posted <- event
	}
}

func (e *phaseObservingEngine) Execute(ctx context.Context, rc *domain.RequestContract, meta sched.RequestMeta) (*sched.Result, *domain.Failure) {
	e.phases = append(e.phases, phaseObservation{name: "EXECUTE", at: time.Now()})
	return e.stubEngine.Execute(ctx, rc, meta)
}

func TestIngressPhasesAcrossProtocols(t *testing.T) {
	tests := []struct {
		name, path, body string
		handler          func(Engine) http.Handler
	}{
		{"chat", "/v1/chat/completions", `{"model":"test","messages":[{"role":"user","content":"hello"}]}`, func(e Engine) http.Handler { return &OpenAIChatHandler{Eng: e} }},
		{"responses", "/v1/responses", `{"model":"test","input":"hello"}`, func(e Engine) http.Handler { return &ResponsesHandler{Eng: e} }},
		{"anthropic", "/v1/messages", `{"model":"test","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`, func(e Engine) http.Handler { return &AnthropicHandler{Eng: e} }},
		{"images", "/v1/images/generations", `{"model":"test","prompt":"hello"}`, func(e Engine) http.Handler { return &OpenAIImagesHandler{Eng: e} }},
		{"image edits", "/v1/images/edits", "--test-boundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntest\r\n--test-boundary--\r\n", func(e Engine) http.Handler { return &OpenAIImagesHandler{Eng: e} }},
	}
	for _, tc := range tests {
		for _, invalid := range []bool{false, true} {
			name := tc.name + "/valid"
			body := tc.body
			if invalid {
				name, body = tc.name+"/invalid", "{"
			}
			t.Run(name, func(t *testing.T) {
				e := &phaseObservingEngine{stubEngine: newStub()}
				e.fail = &domain.Failure{Type: domain.FailConnect, Message: "test upstream failure"}
				r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if tc.name == "image edits" {
					r.Header.Set("Content-Type", "multipart/form-data; boundary=test-boundary")
				}
				before := time.Now()
				tc.handler(e).ServeHTTP(httptest.NewRecorder(), r)
				after := time.Now()
				want := []string{"BODY_RECEIVE_STARTED", "BODY_RECEIVE_COMPLETED", "BODY_PARSE_STARTED", "BODY_PARSE_COMPLETED", "EXECUTE"}
				if invalid {
					want = []string{"BODY_RECEIVE_STARTED", "BODY_RECEIVE_COMPLETED", "BODY_PARSE_STARTED", "BODY_PARSE_FAILED"}
				}
				var names []string
				previous := before
				for _, event := range e.phases {
					names = append(names, event.name)
					if event.at.Before(previous) || event.at.After(after) {
						t.Fatalf("phase %s has a fabricated/out-of-order timestamp", event.name)
					}
					previous = event.at
				}
				if !reflect.DeepEqual(names, want) {
					t.Fatalf("phases = %v, want %v", names, want)
				}
				if invalid && e.phases[len(e.phases)-1].err == nil {
					t.Fatal("parse failure was marked successful")
				}
			})
		}
	}
}

type gatedPhaseBody struct {
	io.Reader
	entered chan time.Time
	release <-chan struct{}
}

func (b *gatedPhaseBody) Read(p []byte) (int, error) {
	if b.entered != nil {
		b.entered <- time.Now()
		b.entered = nil
		<-b.release
	}
	return b.Reader.Read(p)
}

func (*gatedPhaseBody) Close() error { return nil }

func TestIngressReceiveStartVisibleWhileBodyPending(t *testing.T) {
	e := &phaseObservingEngine{stubEngine: newStub(), posted: make(chan phaseObservation, 8)}
	release := make(chan struct{})
	entered := make(chan time.Time, 1)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Body = &gatedPhaseBody{Reader: strings.NewReader("{"), entered: entered, release: release}
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&ResponsesHandler{Eng: e}).ServeHTTP(httptest.NewRecorder(), r)
	}()
	defer func() { close(release); <-done }()
	select {
	case readAt := <-entered:
		select {
		case phase := <-e.posted:
			if phase.name != "BODY_RECEIVE_STARTED" || phase.at.After(readAt) {
				t.Fatalf("reading began before its observation: %+v", phase)
			}
		default:
			t.Fatal("body is blocked with no live receive-start phase")
		}
	case <-time.After(time.Second):
		t.Fatal("body read did not start")
	}
	select {
	case phase := <-e.posted:
		t.Fatalf("premature phase while receiving body: %+v", phase)
	default:
	}
}

type cancelingPhaseBody struct{ cancel context.CancelFunc }

func (b cancelingPhaseBody) Read(p []byte) (int, error) {
	b.cancel()
	return copy(p, `{"model":"test","input":"hello"}`), io.EOF
}
func (cancelingPhaseBody) Close() error { return nil }

func TestIngressReceiveCancellationDoesNotStartParsing(t *testing.T) {
	e := &phaseObservingEngine{stubEngine: newStub()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	r.Body = cancelingPhaseBody{cancel: cancel}
	(&ResponsesHandler{Eng: e}).ServeHTTP(httptest.NewRecorder(), r)
	if len(e.phases) != 2 || e.phases[1].name != "BODY_RECEIVE_FAILED" || !errors.Is(e.phases[1].err, context.Canceled) {
		t.Fatalf("cancel phases = %+v", e.phases)
	}
	if e.gotContract != nil {
		t.Fatal("canceled body entered execution")
	}
}
