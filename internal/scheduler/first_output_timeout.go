package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ferry/internal/domain"
)

// firstOutputWait cancels only the active upstream attempt, never the client
// request. A meaningful output wins against the timer under the same lock.
// Removing this timer after output does not shorten a healthy long response.
type firstOutputWait struct {
	ctx     context.Context
	parent  context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
	expired bool
}

func newFirstOutputWait(parent context.Context, timeout time.Duration) *firstOutputWait {
	ctx, cancel := context.WithCancel(parent)
	w := &firstOutputWait{ctx: ctx, parent: parent, cancel: cancel, timeout: timeout}
	w.ctx = domain.WithFirstOutputObserver(ctx, w.observe)
	if timeout > 0 {
		w.timer = time.AfterFunc(timeout, func() {
			w.mu.Lock()
			if w.stopped || parent.Err() != nil {
				w.mu.Unlock()
				return
			}
			w.expired = true
			w.stopped = true
			w.mu.Unlock()
			cancel()
		})
	}
	return w
}

func (w *firstOutputWait) observe() {
	w.acceptOutput()
}

// acceptOutput atomically arbitrates output/terminal commitment against expiry.
// Once the timer wins, a late event cannot reclaim ownership of the response.
func (w *firstOutputWait) acceptOutput() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
	return !w.expired && w.parent.Err() == nil
}

func (w *firstOutputWait) close() {
	w.observe()
	w.cancel()
}

func (w *firstOutputWait) failure() *domain.Failure {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A client cancellation must never become a new upstream attempt.
	if !w.expired || w.parent.Err() != nil {
		return nil
	}
	return &domain.Failure{Type: domain.FailTTFTTimeout, Scope: domain.FailureScopeConnection,
		Message: fmt.Sprintf("upstream produced no meaningful output within %s", w.timeout)}
}

func (d *decision) firstOutputTimeout() time.Duration {
	if d.logical == nil {
		return 0
	}
	return time.Duration(d.logical.FirstOutputTimeoutSeconds) * time.Second
}

func (d *decision) initializeRecoveryBudget(now, clientDeadline time.Time) {
	d.budget = domain.DefaultRecoveryBudget(now, clientDeadline, max(1, len(d.plan.OrderTiers)))
	timeout := d.firstOutputTimeout()
	if timeout <= 0 {
		return
	}
	// The legacy 30-second soft budget must not prohibit the fallback whose
	// first-output deadline is intentionally longer. Add a bounded waiting
	// allowance for actual candidates, retaining attempt/route limits and the
	// caller's deadline. This is not a whole-response execution deadline.
	candidates := 0
	for _, tier := range d.plan.OrderTiers {
		candidates += len(tier)
	}
	candidates = min(candidates, d.budget.AttemptLimit)
	d.budget.HardDeadline = now.Add(2*time.Minute + time.Duration(candidates)*timeout)
	d.budget.SoftRecoveryDeadline = now.Add(30*time.Second + time.Duration(max(0, candidates-1))*timeout)
	if !clientDeadline.IsZero() && clientDeadline.Before(d.budget.HardDeadline) {
		d.budget.HardDeadline = clientDeadline
	}
	if d.budget.HardDeadline.Before(d.budget.SoftRecoveryDeadline) {
		d.budget.SoftRecoveryDeadline = d.budget.HardDeadline
	}
}
