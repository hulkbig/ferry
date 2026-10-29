package scheduler

import (
	"context"
	"testing"
	"time"

	"ferry/internal/domain"
	"ferry/internal/policy"
)

type outputWaitCaller struct {
	*fakeCaller
	stream func(context.Context) (<-chan domain.StreamEvent, func(), *domain.Failure)
	invoke func(context.Context) (*domain.Response, *domain.Failure)
}

func (c *outputWaitCaller) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	if c.stream != nil {
		return c.stream(ctx)
	}
	return c.fakeCaller.Stream(ctx, rc)
}

func (c *outputWaitCaller) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	if c.invoke != nil {
		return c.invoke(ctx)
	}
	return c.fakeCaller.Invoke(ctx, rc)
}

func outputTimeoutFixture(t *testing.T, recover bool) *v2Fixture {
	t.Helper()
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.LogicalModels["smart"].FirstOutputTimeoutSeconds = 1
		snapshot.Connections["connection-b"].Enabled = false
		if recover {
			snapshot.LogicalModels["smart"].DefaultRoute.Steps[0].RecoverOn = []domain.FailureType{domain.FailTTFTTimeout}
		}
	})
	return f
}

func TestFirstOutputTimeoutRecoversBeforeHeadersAndAfterLifecycleOnly(t *testing.T) {
	for _, mode := range []string{"headers", "silent", "lifecycle", "empty-delta", "invoke"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := outputTimeoutFixture(t, true)
			caller := &outputWaitCaller{fakeCaller: f.calls["p1-a"]}
			caller.invoke = func(ctx context.Context) (*domain.Response, *domain.Failure) {
				<-ctx.Done()
				return nil, failf(domain.FailClientCancel, "attempt context ended")
			}
			caller.stream = func(ctx context.Context) (<-chan domain.StreamEvent, func(), *domain.Failure) {
				if mode == "headers" {
					<-ctx.Done()
					return nil, nil, failf(domain.FailClientCancel, "attempt context ended")
				}
				ch := make(chan domain.StreamEvent)
				go func() {
					defer close(ch)
					ticker := time.NewTicker(20 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							if mode == "silent" {
								continue
							}
							event := domain.StreamEvent{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat,
								NativeFrame: &domain.NativeSSEFrame{Data: `{"choices":[{"delta":{"role":"assistant"}}]}`}}
							if mode == "empty-delta" {
								event = domain.StreamEvent{Kind: domain.EvTextDelta}
							}
							select {
							case ch <- event:
							case <-ctx.Done():
								return
							}
						}
					}
				}()
				return ch, func() {}, nil
			}
			f.sch.world.Load().callers["p1-a"] = caller
			request := chatReq("smart")
			request.Stream = mode != "invoke"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, failure := f.sch.Execute(ctx, request, RequestMeta{BearerToken: "sk"})
			if failure != nil {
				t.Fatal(failure)
			}
			if result.ProviderID != "provider-2" || result.Outcome != domain.OutcomeRecoveredSuccess {
				t.Fatalf("no route recovery: %+v", result)
			}
			if request.Stream {
				defer result.Cancel()
				for event := range result.Events {
					if event.Kind == domain.EvError {
						t.Fatal(event.Err)
					}
				}
			}
			db := f.ledger.reopen(t)
			records, _, queryErr := db.QueryRequests(domain.LogFilter{})
			if queryErr != nil {
				t.Fatal(queryErr)
			}
			if len(records) != 1 || records[0].Attempts != 2 || records[0].Status != domain.ReqSuccess {
				t.Fatalf("request accounting: %+v", records)
			}
			attempts, err := db.QueryAttempts(records[0].ID)
			if err != nil || len(attempts) != 2 || attempts[0].Failure == nil || attempts[0].Failure.Type != domain.FailTTFTTimeout || attempts[0].TTFTMS != 0 {
				t.Fatalf("first attempt must be no-output timeout, not client cancellation or lifecycle TTFT: %+v %v", attempts, err)
			}
			for _, gate := range f.manager.QueueStates() {
				if gate.InFlight != 0 {
					t.Fatalf("admission leaked: %+v", gate)
				}
			}
		})
	}
}

func TestFirstOutputTimeoutNeverRecoversAfterClientCancellation(t *testing.T) {
	t.Parallel()
	f := outputTimeoutFixture(t, true)
	f.calls["p1-a"].invokeBlock = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, failure := f.sch.Execute(ctx, chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure == nil || failure.Type != domain.FailClientCancel || f.calls["p2-a"].invokes.Load() != 0 {
		t.Fatalf("client cancellation caused fallback: %v", failure)
	}
}

func TestFirstOutputTimeoutHonorsRouteRecoveryAllowlist(t *testing.T) {
	t.Parallel()
	f := outputTimeoutFixture(t, false)
	f.calls["p1-a"].invokeBlock = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, failure := f.sch.Execute(ctx, chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure == nil || failure.Type != domain.FailTTFTTimeout || f.calls["p2-a"].invokes.Load() != 0 {
		t.Fatalf("timeout bypassed route allowlist: %v", failure)
	}
}

func TestFirstOutputAllowsLongStreamAndBufferedInvoke(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered-invoke", true: "stream"}[stream], func(t *testing.T) {
			t.Parallel()
			f := outputTimeoutFixture(t, true)
			caller := &outputWaitCaller{fakeCaller: f.calls["p1-a"]}
			caller.invoke = func(ctx context.Context) (*domain.Response, *domain.Failure) {
				domain.ObserveFirstOutput(ctx)
				select {
				case <-time.After(1200 * time.Millisecond):
					return &domain.Response{Content: []domain.Block{{Kind: domain.BlockText, Text: "ok"}}, FinishReason: "stop"}, nil
				case <-ctx.Done():
					return nil, failf(domain.FailClientCancel, "unexpected attempt cancellation")
				}
			}
			caller.stream = func(ctx context.Context) (<-chan domain.StreamEvent, func(), *domain.Failure) {
				ch := make(chan domain.StreamEvent, 2)
				ch <- domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: "ok"}
				go func() {
					defer close(ch)
					select {
					case <-time.After(1200 * time.Millisecond):
						ch <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: "stop"}
					case <-ctx.Done():
						ch <- domain.StreamEvent{Kind: domain.EvError, Err: failf(domain.FailClientCancel, "unexpected attempt cancellation")}
					}
				}()
				return ch, func() {}, nil
			}
			f.sch.world.Load().callers["p1-a"] = caller
			request := chatReq("smart")
			request.Stream = stream
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, failure := f.sch.Execute(ctx, request, RequestMeta{BearerToken: "sk"})
			if failure != nil || result.ProviderID != "provider-1" {
				t.Fatalf("long response failed: %+v %v", result, failure)
			}
			if stream {
				defer result.Cancel()
				for event := range result.Events {
					if event.Kind == domain.EvError {
						t.Fatal(event.Err)
					}
				}
			}
			if f.calls["p2-a"].invokes.Load()+f.calls["p2-a"].streams.Load() != 0 {
				t.Fatal("healthy response caused fallback")
			}
		})
	}
}

func TestFirstOutputWaitDisabledAndTimerRace(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Hour} {
		wait := newFirstOutputWait(context.Background(), timeout)
		if !wait.acceptOutput() {
			t.Fatal("output could not win against a pending/disabled timer")
		}
		if wait.ctx.Err() != nil || wait.failure() != nil {
			t.Fatal("observing output canceled the request")
		}
		wait.close()
	}
	wait := newFirstOutputWait(context.Background(), time.Millisecond)
	defer wait.close()
	<-wait.ctx.Done()
	if wait.acceptOutput() {
		t.Fatal("late output or terminal commitment won against an expired timer")
	}
	wait.observe()
	if wait.failure() == nil {
		t.Fatal("late output cleared an expired timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := newFirstOutputWait(ctx, time.Hour)
	defer canceled.close()
	cancel()
	if canceled.acceptOutput() || canceled.failure() != nil {
		t.Fatal("client cancellation must reject commitment without a timeout failure")
	}
}

func TestFirstOutputBudgetAllowsConfiguredWaitButPreservesLimits(t *testing.T) {
	now := time.Now()
	d := &decision{logical: &domain.LogicalModel{FirstOutputTimeoutSeconds: 180},
		plan: &policy.PlanResult{OrderTiers: [][]domain.ResourceTargetID{{"a"}, {"b"}}}}
	d.initializeRecoveryBudget(now, time.Time{})
	d.budget.ConsumeAttempt()
	if !d.budget.AllowRecovery(now.Add(180*time.Second), true) || !d.budget.AllowAttempt(now.Add(180*time.Second)) {
		t.Fatal("legacy recovery budget prevented configured timeout fallback")
	}
	d.budget.ConsumeRoute()
	if d.budget.AllowRecovery(now.Add(180*time.Second), true) {
		t.Fatal("route budget was bypassed")
	}
	deadline := now.Add(90 * time.Second)
	d.initializeRecoveryBudget(now, deadline)
	if !d.budget.HardDeadline.Equal(deadline) || !d.budget.SoftRecoveryDeadline.Equal(deadline) {
		t.Fatal("caller deadline was extended")
	}
	d.logical.FirstOutputTimeoutSeconds = 0
	d.initializeRecoveryBudget(now, time.Time{})
	if d.budget.AllowRecovery(now.Add(31*time.Second), false) {
		t.Fatal("disabled model changed its existing recovery budget")
	}
}
