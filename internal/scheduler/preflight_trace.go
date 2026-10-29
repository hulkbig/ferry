package scheduler

import (
	"context"
	"ferry/internal/domain"
	"strings"
	"sync"
	"time"
)

// Preflight stores only a fixed number of phase boundaries, never a request
// body, credential, or an error's potentially sensitive text. Published traces
// are immutable snapshots, so the live ledger can read them concurrently.
const maxPreflightTraceEvents = 24

type preflightTrace struct {
	mu     sync.Mutex
	events []domain.ExecutionTraceEvent
}

// ObserveIngressPhase is the optional ingress hook. at is captured at the
// actual read/parse boundary, before publishing the observation.
func (s *Scheduler) ObserveIngressPhase(ctx context.Context, phase string, at time.Time, err error) {
	switch phase {
	case "BODY_RECEIVE_STARTED", "BODY_RECEIVE_COMPLETED", "BODY_RECEIVE_FAILED",
		"BODY_PARSE_STARTED", "BODY_PARSE_COMPLETED", "BODY_PARSE_FAILED":
	default:
		return
	}
	event := domain.ExecutionTraceEvent{At: at, Category: domain.TraceRoute, Type: phase}
	if err != nil {
		event.FailureType = domain.FailUnsupportedParam
		event.FailureScope = domain.FailureScopeRequest
		if ctx.Err() != nil {
			event.FailureType = domain.FailClientCancel
		}
	}
	s.observePreflightPhase(ctx, event)
}

// observePreflightPhase publishes one phase immediately for an observed HTTP
// request. Call only before handing the completed trace to execution. Scheduler
// phase messages must contain fixed metadata/counts, never request content.
func (s *Scheduler) observePreflightPhase(ctx context.Context, event domain.ExecutionTraceEvent) {
	request, ok := ctx.Value(ingressRequestKey{}).(*ingressRequest)
	if !ok || event.At.IsZero() || event.Type == "" {
		return
	}
	trace := &request.preflight
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if len(trace.events) >= maxPreflightTraceEvents {
		return
	}
	// Defensive bounds also cover callers outside the ingress adapter.
	if len(event.Type) > 64 {
		event.Type = strings.Clone(event.Type[:64])
	}
	if len(event.Message) > 256 {
		event.Message = strings.Clone(event.Message[:256])
	}
	for _, existing := range trace.events {
		if existing.Type == event.Type {
			return
		}
	}
	trace.events = append(trace.events, event)
	s.led.SetTrace(request.id, &domain.TraceRecord{Events: append([]domain.ExecutionTraceEvent(nil), trace.events...)})
}

// preflightTraceEvents seeds initialTraceEvents with observed times, rather
// than inventing receipt/read/parse times after planning has already finished.
// The caller owns the returned slice and must not add a second REQUEST_RECEIVED.
func preflightTraceEvents(ctx context.Context) []domain.ExecutionTraceEvent {
	request, ok := ctx.Value(ingressRequestKey{}).(*ingressRequest)
	if !ok {
		return nil
	}
	request.preflight.mu.Lock()
	defer request.preflight.mu.Unlock()
	return append([]domain.ExecutionTraceEvent(nil), request.preflight.events...)
}
