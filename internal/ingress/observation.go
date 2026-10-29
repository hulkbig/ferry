package ingress

import (
	"context"
	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
	"net/http"
	"strings"
	"time"
)

type ingressObserver interface {
	ObserveIngress(context.Context, domain.Protocol, sched.RequestMeta) (context.Context, func(int))
}
type firstOutputObserver interface {
	ObserveFirstOutput(context.Context)
}
type ingressPhaseObserver interface {
	ObserveIngressPhase(context.Context, string, time.Time, error)
}
type ingressPhaseObserverKey struct{}

func observeIngressPhase(ctx context.Context, phase string, err error) {
	if observer, ok := ctx.Value(ingressPhaseObserverKey{}).(ingressPhaseObserver); ok {
		observer.ObserveIngressPhase(ctx, phase, time.Now(), err)
	}
}

func finishIngressPhase(ctx context.Context, phase string, err error) {
	if err != nil {
		observeIngressPhase(ctx, phase+"_FAILED", err)
	} else {
		observeIngressPhase(ctx, phase+"_COMPLETED", nil)
	}
}

type observedWriter struct {
	http.ResponseWriter
	status        int
	writeFailed   bool
	firstOutput   bool
	onFirstOutput func()
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil || n != len(b) {
		w.writeFailed = true
	}
	if n > 0 && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.observeOutput()
	}
	return n, err
}
func (w *observedWriter) Flush() {
	_ = w.FlushError()
}
func (w *observedWriter) FlushError() error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.writeFailed = true
	}
	return err
}
func (w *observedWriter) observeOutput() {
	if w.firstOutput || w.writeFailed || w.status < 200 || w.status >= 300 || w.onFirstOutput == nil {
		return
	}
	w.firstOutput = true
	w.onFirstOutput()
}
func (w *observedWriter) observeSSEOutput(event string, payload []byte) {
	if !w.firstOutput && !w.writeFailed && w.onFirstOutput != nil && meaningfulSSEOutput(event, payload) {
		w.observeOutput()
	}
}

func observeIngress(eng Engine, protocol domain.Protocol, w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, func()) {
	if observer, ok := eng.(ingressPhaseObserver); ok {
		r = r.WithContext(context.WithValue(r.Context(), ingressPhaseObserverKey{}, observer))
	}
	observer, ok := eng.(ingressObserver)
	if !ok {
		return w, r, func() {}
	}
	ctx, finish := observer.ObserveIngress(r.Context(), protocol, requestMeta(r, nil))
	writer := &observedWriter{ResponseWriter: w}
	if output, ok := eng.(firstOutputObserver); ok {
		writer.onFirstOutput = func() { output.ObserveFirstOutput(ctx) }
	}
	return writer, r.WithContext(ctx), func() { finish(writer.status) }
}
