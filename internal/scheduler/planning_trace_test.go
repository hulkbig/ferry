package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestPlanningTraceVisibleWhilePreparingAndCallingUpstream(t *testing.T) {
	f := newV2Fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, finish := f.sch.ObserveIngress(ctx, domain.ProtocolOpenAIChat, RequestMeta{BearerToken: "sk"})
	defer finish(500)
	request := ctx.Value(ingressRequestKey{}).(*ingressRequest)
	readAt := time.Now()
	f.sch.ObserveIngressPhase(ctx, "BODY_RECEIVE_STARTED", readAt, nil)
	f.sch.ObserveIngressPhase(ctx, "BODY_RECEIVE_COMPLETED", time.Now(), nil)
	f.sch.ObserveIngressPhase(ctx, "BODY_PARSE_STARTED", time.Now(), nil)
	f.sch.ObserveIngressPhase(ctx, "BODY_PARSE_COMPLETED", time.Now(), nil)
	preparing, releasePreparation := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var unblock sync.Once
	defer unblock.Do(func() { close(releasePreparation) })
	f.calls["p1-a"].prepare = func(*domain.RequestContract) ([]string, error) {
		once.Do(func() { close(preparing); <-releasePreparation })
		return nil, nil
	}
	invoking, releaseInvoke := make(chan struct{}), make(chan struct{})
	f.calls["p1-a"].invokeStarted = invoking
	f.calls["p1-a"].invokeBlock = releaseInvoke
	done := make(chan *domain.Failure, 1)
	go func() {
		_, failure := f.sch.Execute(ctx, chatReq("smart"), RequestMeta{BearerToken: "sk"})
		done <- failure
	}()
	await := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("phase not reached")
		}
	}
	await(preparing)
	trace, err := f.ledger.QueryTrace(request.id)
	if err != nil || trace == nil || trace.Events[len(trace.Events)-1].Type != "COMPATIBILITY_STARTED" {
		t.Fatalf("pending preparation trace=%+v err=%v", trace, err)
	}
	unblock.Do(func() { close(releasePreparation) })
	await(invoking)
	trace, err = f.ledger.QueryTrace(request.id)
	if err != nil || trace == nil || trace.Events[len(trace.Events)-1].Type != "ADMISSION_COMPLETED" {
		t.Fatalf("pending upstream trace=%+v err=%v", trace, err)
	}
	close(releaseInvoke)
	select {
	case failure := <-done:
		if failure != nil {
			t.Fatal(failure)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not finish")
	}
	finish(200)
	stored := f.ledger.reopen(t)
	trace, err = stored.QueryTrace(request.id)
	if err != nil || trace == nil {
		t.Fatalf("stored trace=%+v err=%v", trace, err)
	}
	counts := map[string]int{}
	previous := time.Time{}
	for _, event := range trace.Events {
		counts[event.Type]++
		if event.At.Before(previous) {
			t.Fatalf("out-of-order phase: %+v", event)
		}
		previous = event.At
		if event.Type == "REQUEST_RECEIVED" && !event.At.Equal(request.startedAt) {
			t.Fatal("receipt timestamp replaced")
		}
		if event.Type == "BODY_RECEIVE_STARTED" && !event.At.Equal(readAt) {
			t.Fatal("read timestamp replaced")
		}
		if event.Type == "COMPATIBILITY_COMPLETED" && !strings.Contains(event.Message, "prepared=3") {
			t.Fatalf("missing preparation count: %+v", event)
		}
		if (event.Type == "ATTEMPT_STARTED" || event.Type == "ADMISSION_COMPLETED") && event.Attempt != 1 {
			t.Fatalf("incorrect active attempt: %+v", event)
		}
	}
	for _, phase := range []string{"REQUEST_RECEIVED", "BODY_PARSE_COMPLETED", "PLANNING_STARTED", "MODEL_RESOLVED", "ROUTE_RESOLVED", "COMPATIBILITY_STARTED", "COMPATIBILITY_COMPLETED", "PLANNING_COMPLETED", "ATTEMPT_STARTED", "ADMISSION_COMPLETED", "COMPLETE"} {
		if counts[phase] != 1 {
			t.Fatalf("phase %s count=%d", phase, counts[phase])
		}
	}
}

func TestCancellationDuringFinalAttemptPreparationStartsNoAttempt(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, preparationFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t,error=%t", stream, preparationFails), func(t *testing.T) {
				f := newV2Fixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				count := 0
				f.calls["p1-a"].prepare = func(*domain.RequestContract) ([]string, error) {
					count++
					if count == 2 {
						cancel()
						if preparationFails {
							return nil, errors.New("preparation failed while canceling")
						}
					} // after route planning, during final validation
					return nil, nil
				}
				rc := chatReq("smart")
				rc.Stream = stream
				_, failure := f.sch.Execute(ctx, rc, RequestMeta{BearerToken: "sk"})
				cancel()
				if failure == nil || failure.Type != domain.FailClientCancel {
					t.Fatalf("cancellation=%v", failure)
				}
				if f.calls["p1-a"].invokes.Load()+f.calls["p1-a"].streams.Load() != 0 {
					t.Fatal("canceled request reached upstream")
				}
				stored := f.ledger.reopen(t)
				records, _, err := stored.QueryRequests(domain.LogFilter{})
				if err != nil || len(records) != 1 || records[0].Status != domain.ReqCanceled || records[0].Attempts != 0 {
					t.Fatalf("records=%+v err=%v", records, err)
				}
				trace, err := stored.QueryTrace(records[0].ID)
				if err != nil || trace == nil {
					t.Fatalf("trace=%+v err=%v", trace, err)
				}
				for _, event := range trace.Events {
					if event.Type == "ATTEMPT_STARTED" {
						t.Fatal("synthetic attempt started")
					}
				}
			})
		}
	}
}

func TestStreamingTracePairsAdmissionAndFirstEventWithRetriedAttempt(t *testing.T) {
	f := newV2Fixture(t)
	f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailConnect, Scope: domain.FailureScopeConnection}
	rc := chatReq("smart")
	rc.Stream = true
	result, failure := f.sch.Execute(context.Background(), rc, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	for range result.Events {
	}
	stored := f.ledger.reopen(t)
	records, _, err := stored.QueryRequests(domain.LogFilter{})
	if err != nil || len(records) != 1 || records[0].Status != domain.ReqSuccess {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	trace, err := stored.QueryTrace(records[0].ID)
	if err != nil || trace == nil {
		t.Fatalf("trace=%+v err=%v", trace, err)
	}
	var phases []string
	for _, event := range trace.Events {
		switch event.Type {
		case "ATTEMPT_STARTED", "ADMISSION_COMPLETED", "UPSTREAM_FIRST_EVENT":
			phases = append(phases, fmt.Sprintf("%s:%d", event.Type, event.Attempt))
		}
	}
	want := "ATTEMPT_STARTED:1 ADMISSION_COMPLETED:1 ATTEMPT_STARTED:2 ADMISSION_COMPLETED:2 UPSTREAM_FIRST_EVENT:2"
	if got := strings.Join(phases, " "); got != want {
		t.Fatalf("phases=%s want=%s", got, want)
	}
}
