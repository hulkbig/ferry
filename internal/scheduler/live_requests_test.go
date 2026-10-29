package scheduler

import (
	"context"
	"ferry/internal/domain"
	"testing"
	"time"
)

func awaitLiveStatus(t *testing.T, f *v2Fixture, id string, status domain.RequestStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := f.ledger.QueryRequest(id)
		if err == nil && r != nil && r.Status == status {
			return
		}
		time.Sleep(time.Millisecond)
	}
	r, err := f.ledger.QueryRequest(id)
	t.Fatalf("want %s: %+v %v", status, r, err)
}

func TestLiveIngressQueueExecutionAndFinalization(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(s *domain.CompiledSnapshot) {
		s.Concurrency = &domain.ConcurrencyPolicy{MaxConcurrent: 1}
		s.Connections["connection-b"].Enabled = false
		s.Connections["connection-c"].Enabled = false
	})
	release, failure := f.manager.Admit(context.Background(), "p1-a")
	if failure != nil {
		t.Fatal(failure)
	}
	defer release()
	ctx, finish := f.sch.ObserveIngress(context.Background(), domain.ProtocolOpenAIChat, RequestMeta{})
	id := ctx.Value(ingressRequestKey{}).(*ingressRequest).id
	awaitLiveStatus(t, f, id, "received")
	started, unblock := make(chan struct{}), make(chan struct{})
	f.calls["p1-a"].invokeStarted = started
	f.calls["p1-a"].invokeBlock = unblock
	defer close(unblock)
	done := make(chan *domain.Failure, 1)
	go func() {
		_, failure := f.sch.Execute(ctx, chatReq("smart"), RequestMeta{BearerToken: "sk"})
		done <- failure
	}()
	awaitLiveStatus(t, f, id, "waiting")
	rows := f.sch.UnfinishedRequests()
	if len(rows) != 1 || rows[0].RequestID != id || rows[0].Phase != TrafficWaiting || rows[0].ConsumerKey != "caller" || rows[0].LogicalModelID != "smart" {
		t.Fatalf("waiting request omitted or lost identity: %+v", rows)
	}
	if q := f.manager.QueueStates(); len(q) == 0 {
		t.Fatal("no queue snapshot")
	}
	release()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream not started")
	}
	awaitLiveStatus(t, f, id, "active")
	rows = f.sch.UnfinishedRequests()
	if len(rows) != 1 || rows[0].Phase != TrafficActive || rows[0].ConnectionID != "connection-a" {
		t.Fatalf("active request: %+v", rows)
	}
	// The handler must not overwrite execution owned by the scheduler.
	finish(200)
	awaitLiveStatus(t, f, id, "active")
	unblock <- struct{}{}
	if failure := <-done; failure != nil {
		t.Fatal(failure)
	}
	awaitLiveStatus(t, f, id, domain.ReqSuccess)
	finish(200)
}

func TestGlobalProviderConnectionConcurrencyPrecedence(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(s *domain.CompiledSnapshot) {
		s.Concurrency = &domain.ConcurrencyPolicy{MaxConcurrent: 3}
		s.Providers["provider-1"].Concurrency = &domain.ConcurrencyPolicy{MaxConcurrent: 2}
		s.Connections["connection-a"].Concurrency = &domain.ConcurrencyPolicy{MaxConcurrent: 1}
	})
	want := map[string]int{"connection-a": 1, "connection-b": 2, "connection-c": 3}
	for _, q := range f.manager.QueueStates() {
		if q.MaxConcurrent != want[q.ConnectionID] {
			t.Fatalf("precedence: %+v", q)
		}
	}
}

func TestIngressEarlyFailureIsRecorded(t *testing.T) {
	f := newV2Fixture(t)
	ctx, finish := f.sch.ObserveIngress(context.Background(), domain.ProtocolOpenAIChat, RequestMeta{})
	id := ctx.Value(ingressRequestKey{}).(*ingressRequest).id
	finish(401)
	awaitLiveStatus(t, f, id, domain.ReqFailed)
	r, _ := f.ledger.QueryRequest(id)
	if r.FailureType != domain.FailAuth {
		t.Fatalf("auth failure: %+v", r)
	}
}

func TestUnfinishedRequestTracksRecoveryAndFinalDelivery(t *testing.T) {
	f := newV2Fixture(t)
	f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailConnect, Scope: domain.FailureScopeConnection}
	started, release := make(chan struct{}), make(chan struct{})
	f.calls["p1-b"].invokeStarted, f.calls["p1-b"].invokeBlock = started, release
	defer close(release)
	ctx, finish := f.sch.ObserveIngress(context.Background(), domain.ProtocolOpenAIChat, RequestMeta{BearerToken: "sk"})
	defer finish(200)
	id := ctx.Value(ingressRequestKey{}).(*ingressRequest).id
	done := make(chan *domain.Failure, 1)
	go func() {
		_, failure := f.sch.Execute(ctx, chatReq("smart"), RequestMeta{BearerToken: "sk"})
		done <- failure
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery attempt did not start")
	}
	rows := f.sch.UnfinishedRequests()
	if len(rows) != 1 || rows[0].RequestID != id || rows[0].Phase != TrafficRecovering || rows[0].ConnectionID != "connection-b" {
		t.Fatalf("recovery duplicated/lost logical request: %+v", rows)
	}
	release <- struct{}{}
	if failure := <-done; failure != nil {
		t.Fatal(failure)
	}
	rows = f.sch.UnfinishedRequests()
	if len(rows) != 1 || rows[0].Phase != TrafficDelivering || rows[0].TTFTMS != 0 || len(f.sch.ActiveTraffic()) != 0 {
		t.Fatalf("final upstream attempt ended client lifetime: %+v", rows)
	}
	f.sch.ObserveFirstOutput(ctx)
	first := f.sch.UnfinishedRequests()[0].TTFTMS
	if first <= 0 {
		t.Fatal("delivered output has no client TTFT")
	}
	f.sch.ObserveFirstOutput(ctx)
	finish(200)
	awaitLiveStatus(t, f, id, domain.ReqSuccess)
	final, _ := f.ledger.QueryRequest(id)
	if final.TTFTMS != first || final.Attempts != 2 || len(f.sch.UnfinishedRequests()) != 0 {
		t.Fatalf("final delivery lost timing or leaked queue row: %+v", final)
	}
}

func TestStreamingClientTTFTUpdatesBeforeFinalization(t *testing.T) {
	f := newV2Fixture(t)
	gate := make(chan struct{})
	f.calls["p1-a"].streamGate = gate
	ctx, finish := f.sch.ObserveIngress(context.Background(), domain.ProtocolOpenAIChat, RequestMeta{BearerToken: "sk"})
	defer finish(200)
	// Model time spent decoding/waiting before the first upstream attempt.
	ingress := ctx.Value(ingressRequestKey{}).(*ingressRequest)
	ingress.startedAt = time.Now().Add(-time.Minute)
	rc := chatReq("smart")
	rc.Stream = true
	res, failure := f.sch.Execute(ctx, rc, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		close(gate)
		t.Fatal(failure)
	}
	if rows := f.sch.UnfinishedRequests(); len(rows) != 1 || rows[0].TTFTMS != 0 || rows[0].Phase != TrafficStreaming {
		close(gate)
		t.Fatalf("upstream commit prematurely set downstream TTFT: %+v", rows)
	}
	f.sch.ObserveFirstOutput(ctx)
	rows := f.sch.UnfinishedRequests()
	first := rows[0].TTFTMS
	if first < 60000 {
		t.Fatalf("client TTFT excluded pre-attempt time: %d", first)
	}
	close(gate)
	for range res.Events {
	}
	if rows = f.sch.UnfinishedRequests(); len(rows) != 1 || rows[0].TTFTMS != first {
		t.Fatalf("upstream finalization changed client timing: %+v", rows)
	}
	finish(200)
	awaitLiveStatus(t, f, ingress.id, domain.ReqFailed) // fake gated stream has no terminal event
	final, _ := f.ledger.QueryRequest(ingress.id)
	attempts, err := f.ledger.QueryAttempts(ingress.id)
	if err != nil || len(attempts) != 1 || attempts[0].TTFTMS >= first || final.TTFTMS != first {
		t.Fatalf("client/attempt TTFT clocks conflated: final=%+v attempts=%+v err=%v", final, attempts, err)
	}
}
