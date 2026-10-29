package scheduler

import (
	"context"
	"ferry/internal/domain"
	"sync/atomic"
	"time"
)

type ingressRequestKey struct{}
type ingressRequest struct {
	id        string
	startedAt time.Time
	claimed   atomic.Bool
	finished  atomic.Bool
	preflight preflightTrace
}

// ObserveIngress makes even decoding/authentication failures visible without
// recording credentials or request bodies. Execute reuses the same logical ID.
func (s *Scheduler) ObserveIngress(ctx context.Context, protocol domain.Protocol, meta RequestMeta) (context.Context, func(int)) {
	request := &ingressRequest{id: newRequestID(), startedAt: time.Now()}
	rec := domain.RequestRecord{ID: request.id, Ts: request.startedAt, Protocol: protocol, ClientIP: meta.ClientIP, Status: "received"}
	world := s.world.Load()
	if world != nil && world.snap != nil {
		rec.RuntimeRevision = world.snap.Version
	}
	if key, secretID := s.resolveKeyInWorld(world, meta.credential(), meta.CallerID); key != nil {
		rec.ConsumerKey, rec.APIKeySecretID = key.Identity.Name, secretID
	}
	s.openIngress.Add(1)
	s.led.BeginIngressRequest(rec)
	ctx = context.WithValue(ctx, ingressRequestKey{}, request)
	s.observePreflightPhase(ctx, domain.ExecutionTraceEvent{
		At: request.startedAt, Category: domain.TraceRoute, Type: "REQUEST_RECEIVED", Protocol: protocol,
	})
	return ctx, func(status int) {
		if !request.finished.CompareAndSwap(false, true) {
			return
		}
		defer s.openIngress.Add(-1)
		defer s.led.EndIngressRequest(request.id)
		if request.claimed.Load() {
			return
		}
		failureType := domain.FailUnsupportedParam
		if status == 401 || status == 403 {
			failureType = domain.FailAuth
		}
		phase := "INGRESS_FAILED"
		if ctx.Err() != nil {
			failureType, phase = domain.FailClientCancel, "INGRESS_CANCELED"
		}
		s.observePreflightPhase(ctx, domain.ExecutionTraceEvent{
			At: time.Now(), Category: domain.TraceRoute, Type: phase,
			FailureType: failureType, FailureScope: domain.FailureScopeRequest,
		})
		s.led.FinishRequest(request.id, func(r *domain.RequestRecord) {
			r.Status, r.Outcome = domain.ReqFailed, domain.OutcomeFailed
			r.FailureType = failureType
			if failureType == domain.FailClientCancel {
				r.Status = domain.ReqCanceled
			}
			r.FailureMsg = "request ended before execution completed"
			r.DurationMS = time.Since(request.startedAt).Milliseconds()
		})
	}
}

// ObserveFirstOutput is called by ingress after a meaningful downstream write
// (and flush for SSE), never for lifecycle frames or upstream-only output.
func (s *Scheduler) ObserveFirstOutput(ctx context.Context) {
	if request, ok := ctx.Value(ingressRequestKey{}).(*ingressRequest); ok {
		s.led.RecordFirstOutput(request.id, time.Now())
	}
}

// OpenIngressRequests counts HTTP handlers that have entered inference and
// have not yet returned, including streams still being written to clients.
func (s *Scheduler) OpenIngressRequests() int64 {
	if s == nil {
		return 0
	}
	return s.openIngress.Load()
}
