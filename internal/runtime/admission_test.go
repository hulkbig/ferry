package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
)

func admissionManager(t *testing.T, policy domain.ConcurrencyPolicy) (*Manager, domain.ResourceTargetID) {
	t.Helper()
	m := NewManager(DefaultHealthConfig())
	id := domain.ResourceTargetID("conn-a:model")
	m.RegisterExecutionTarget(id, "conn-a", true, true, nil, nil)
	m.SetConnectionConcurrency("conn-a", &policy)
	return m, id
}

func TestAdmissionCapsConcurrencyAndReleasesInFIFOOrder(t *testing.T) {
	m, id := admissionManager(t, domain.ConcurrencyPolicy{MaxConcurrent: 1, MaxWaitSeconds: 5})

	release, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatalf("first admit failed: %+v", failure)
	}

	admitted := make(chan int, 3)
	started := make(chan struct{})
	for i := 1; i <= 3; i++ {
		// Serialize enqueue so arrival order is deterministic; the gate
		// promises FIFO over arrivals, not over goroutine scheduling.
		queued := make(chan struct{})
		go func(seq int) {
			close(queued)
			next, f := m.Admit(context.Background(), id)
			if f != nil {
				admitted <- -seq
				return
			}
			admitted <- seq
			next()
		}(i)
		<-queued
		waitForQueueDepth(t, m, "conn-a", i)
	}
	close(started)
	<-started

	release()
	for want := 1; want <= 3; want++ {
		select {
		case got := <-admitted:
			if got != want {
				t.Fatalf("admitted out of order: got %d want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never admitted", want)
		}
	}
}

func TestAdmissionQueuesThroughThrottleInsteadOfFailing(t *testing.T) {
	m, id := admissionManager(t, domain.ConcurrencyPolicy{MaxConcurrent: 4, MaxWaitSeconds: 10})

	// A rate limit with a short Retry-After must hold the queue, not reject it.
	m.RecordFailure(id, &domain.Failure{
		Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection,
		RetryAfter: 150 * time.Millisecond,
	}, time.Now())

	start := time.Now()
	release, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatalf("throttled request was rejected instead of queued: %+v", failure)
	}
	defer release()
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("request was admitted after %s, before the cooldown elapsed", waited)
	}
}

func TestAdmissionSerializesWhileRecoveringThenRestoresLimit(t *testing.T) {
	m, id := admissionManager(t, domain.ConcurrencyPolicy{MaxConcurrent: 4, MaxWaitSeconds: 5})
	m.RecordFailure(id, &domain.Failure{
		Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Nanosecond,
	}, time.Now())

	first, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatalf("recovering admit failed: %+v", failure)
	}
	// Still recovering: the second request must wait even though the
	// configured limit is 4.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, blocked := m.Admit(ctx, id); blocked == nil {
		t.Fatal("second request bypassed serialized recovery")
	}
	first()

	m.RecordSuccess(id, 5, 0, domain.Usage{}, 0, 0, time.Now())
	a, failureA := m.Admit(context.Background(), id)
	b, failureB := m.Admit(context.Background(), id)
	if failureA != nil || failureB != nil {
		t.Fatalf("configured concurrency not restored after success: %+v %+v", failureA, failureB)
	}
	a()
	b()
}

func TestAdmissionRejectsWhenQueueIsFull(t *testing.T) {
	m, id := admissionManager(t, domain.ConcurrencyPolicy{MaxConcurrent: 1, MaxQueue: 1, MaxWaitSeconds: 5})
	release, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatalf("first admit failed: %+v", failure)
	}
	defer release()

	go func() {
		next, f := m.Admit(context.Background(), id)
		if f == nil {
			next()
		}
	}()
	waitForQueueDepth(t, m, "conn-a", 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, overflow := m.Admit(ctx, id)
	if overflow == nil {
		t.Fatal("queue overflow was admitted")
	}
	if overflow.Type != domain.FailRateLimit || overflow.Scope != domain.FailureScopeConnection {
		t.Fatalf("unexpected overflow failure: %+v", overflow)
	}
}

func TestAdmissionCanFailImmediatelyWhenConcurrencyIsFull(t *testing.T) {
	wait := false
	m, id := admissionManager(t, domain.ConcurrencyPolicy{
		MaxConcurrent: 1, QueueWhenFull: &wait, MaxWaitSeconds: 5,
	})
	release, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatalf("first admit failed: %+v", failure)
	}
	defer release()

	started := time.Now()
	_, overflow := m.Admit(context.Background(), id)
	if overflow == nil {
		t.Fatal("fail-fast admission unexpectedly queued")
	}
	if overflow.Type != domain.FailRateLimit || overflow.HTTPStatus != 429 {
		t.Fatalf("unexpected fail-fast result: %+v", overflow)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("fail-fast admission took %s", elapsed)
	}
	m.mu.Lock()
	depth := len(m.connections["conn-a"].adm.queue)
	m.mu.Unlock()
	if depth != 0 {
		t.Fatalf("fail-fast admission left %d queued requests", depth)
	}
}

func TestAdmissionThrottlePolicyIsIndependentFromConcurrencyOverflowPolicy(t *testing.T) {
	t.Run("throttle still waits when local overflow is fail-fast", func(t *testing.T) {
		waitForConcurrency := false
		m, id := admissionManager(t, domain.ConcurrencyPolicy{
			MaxConcurrent: 4, QueueWhenFull: &waitForConcurrency, MaxWaitSeconds: 2,
		})
		m.RecordFailure(id, &domain.Failure{
			Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: 80 * time.Millisecond,
		}, time.Now())
		started := time.Now()
		release, failure := m.Admit(context.Background(), id)
		if failure != nil {
			t.Fatalf("queue-on-throttle was overridden by queue_when_full: %+v", failure)
		}
		release()
		if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
			t.Fatalf("throttle cooldown was not awaited: %s", elapsed)
		}
	})

	t.Run("throttle can fail immediately while local overflow queues", func(t *testing.T) {
		waitForThrottle := false
		m, id := admissionManager(t, domain.ConcurrencyPolicy{
			MaxConcurrent: 4, QueueOnThrottle: &waitForThrottle, MaxWaitSeconds: 2,
		})
		m.RecordFailure(id, &domain.Failure{
			Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Second,
		}, time.Now())
		started := time.Now()
		_, failure := m.Admit(context.Background(), id)
		if failure == nil || failure.HTTPStatus != 429 || !strings.Contains(failure.Message, "throttle cooldown") {
			t.Fatalf("fail-fast throttle result=%+v", failure)
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Fatalf("fail-fast throttle took %s", elapsed)
		}
	})
}

func TestAdmissionAbandonDoesNotLeakSlots(t *testing.T) {
	m, id := admissionManager(t, domain.ConcurrencyPolicy{MaxConcurrent: 1, MaxWaitSeconds: 5})
	release, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatalf("first admit failed: %+v", failure)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if _, canceled := m.Admit(ctx, id); canceled == nil {
		t.Fatal("canceled waiter was admitted")
	}
	cancel()
	release()

	m.mu.Lock()
	inFlight := m.connections["conn-a"].st.InFlight
	depth := len(m.connections["conn-a"].adm.queue)
	m.mu.Unlock()
	if inFlight != 0 || depth != 0 {
		t.Fatalf("gate leaked state: in_flight=%d queue=%d", inFlight, depth)
	}
}

func waitForQueueDepth(t *testing.T, m *Manager, connectionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		depth := len(m.connections[connectionID].adm.queue)
		m.mu.Unlock()
		if depth == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue depth never reached %d", want)
}
