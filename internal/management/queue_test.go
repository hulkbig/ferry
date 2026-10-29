package management

import (
	"context"
	"ferry/internal/domain"
	"ferry/internal/ingress"
	"ferry/internal/scheduler"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueueGlobalPolicyPersistenceAndCAS(t *testing.T) {
	f := newV2APIFixture(t)
	rec, settings := f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, rec, 200)
	patch := map[string]any{"expected_revision": settings["revision"], "concurrency": map[string]any{"max_concurrent": 2, "max_queue": 7, "max_wait_seconds": 30, "queue_when_full": false, "queue_on_throttle": true}}
	rec, _ = f.do(http.MethodPatch, "/api/settings", patch)
	requireStatus(t, rec, 200)
	rec, q := f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, 200)
	connections := q["connections"].([]any)
	if len(connections) != 1 {
		t.Fatalf("connections: %+v", q)
	}
	c := connections[0].(map[string]any)
	r := c["runtime"].(map[string]any)
	if c["policy_source"] != "global" || r["max_concurrent"] != float64(2) || r["max_queue"] != float64(7) || r["queue_when_full"] != false {
		t.Fatalf("effective policy: %+v", q)
	}
	saved, _, err := f.control.ActiveConfig()
	if err != nil || saved.Concurrency == nil || saved.Concurrency.MaxConcurrent != 2 {
		t.Fatalf("persisted: %+v %v", saved, err)
	}
	rec, _ = f.do(http.MethodPatch, "/api/settings", patch)
	requireStatus(t, rec, 409)
	rec, settings = f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, rec, 200)
	patch["expected_revision"] = settings["revision"]
	patch["concurrency"] = map[string]any{"max_wait_seconds": 1801}
	rec, _ = f.do(http.MethodPatch, "/api/settings", patch)
	requireStatus(t, rec, 400)
}

func TestQueueCountsActualAdmissionAndCancellation(t *testing.T) {
	f := newV2APIFixture(t)
	m := f.server.deps.Manager
	m.SetConnectionConcurrency("connection-a", &domain.ConcurrencyPolicy{MaxConcurrent: 1})
	var id domain.ResourceTargetID
	for target := range f.server.deps.Scheduler.Snapshot().Targets {
		id = target
		break
	}
	release, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatal(failure)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *domain.Failure, 1)
	go func() {
		next, failure := m.Admit(ctx, id)
		if next != nil {
			next()
		}
		done <- failure
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.QueueStates()[0].Waiting == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	rec, q := f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, 200)
	if q["running"] != float64(1) || q["waiting"] != float64(1) {
		t.Fatalf("counts: %+v", q)
	}
	cancel()
	if failure := <-done; failure == nil || failure.Type != domain.FailClientCancel {
		t.Fatalf("cancel: %+v", failure)
	}
	rec, q = f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, 200)
	if q["waiting"] != float64(0) {
		t.Fatalf("stale waiter: %+v", q)
	}
}

func TestQueueKeepsOpenClientRequestSeparateFromUpstreamCapacity(t *testing.T) {
	f := newV2APIFixture(t)
	ctx, finish := f.server.deps.Scheduler.ObserveIngress(context.Background(), domain.ProtocolOpenAIResponses, scheduler.RequestMeta{PresentedCredential: nativeV2Config().ConsumerKeys[0].Key, ClientIP: "192.0.2.1"})
	_ = ctx
	defer finish(http.StatusOK)
	var id domain.ResourceTargetID
	for target := range f.server.deps.Scheduler.Snapshot().Targets {
		id = target
		break
	}
	release, failure := f.server.deps.Manager.Admit(context.Background(), id)
	if failure != nil {
		t.Fatal(failure)
	}
	rec, q := f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, 200)
	if q["open_requests"] != float64(1) || q["running"] != float64(1) {
		t.Fatalf("active client request and upstream slot: %+v", q)
	}
	rows := q["requests"].([]any)
	if len(rows) != int(q["open_requests"].(float64)) {
		t.Fatalf("received rows do not reconcile: %+v", q)
	}
	row := rows[0].(map[string]any)
	if row["phase"] != "received" || row["request_id"] == "" || row["consumer_key"] != "initial" ||
		row["protocol"] != string(domain.ProtocolOpenAIResponses) || row["client_ip"] != "192.0.2.1" ||
		row["started_at"] == nil || row["duration_ms"] == nil || row["ttft_ms"] != float64(0) {
		t.Fatalf("received identity/timing: %+v", row)
	}
	release()
	rec, q = f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, 200)
	if q["open_requests"] != float64(1) || q["running"] != float64(0) || len(q["requests"].([]any)) != 1 {
		t.Fatalf("open client request should remain visible without an upstream slot: %+v", q)
	}
	finish(http.StatusOK)
	finish(http.StatusOK)
	rec, q = f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, 200)
	if q["open_requests"] != float64(0) || len(q["requests"].([]any)) != 0 {
		t.Fatalf("completed client request still counted: %+v", q)
	}
}

type heldDeliveryWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
}

func (w *heldDeliveryWriter) Write(b []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.ResponseRecorder.Write(b)
}

func TestQueueRetainsRequestDuringFinalDownstreamWrite(t *testing.T) {
	f := newV2APIFixture(t)
	writer := &heldDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+nativeV2Config().ConsumerKeys[0].Key)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ingress.NewServer(f.server.deps.Scheduler).Chat.ServeHTTP(writer, req)
	}()
	t.Cleanup(func() { close(writer.release); <-done })
	select {
	case <-writer.started:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream response write did not start")
	}
	rec, q := f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, http.StatusOK)
	rows := q["requests"].([]any)
	if q["open_requests"] != float64(1) || len(rows) != 1 || q["running"] != float64(0) {
		t.Fatalf("downstream lifetime omitted from queue: %+v", q)
	}
	row := rows[0].(map[string]any)
	if row["phase"] != "delivering" || row["consumer_key"] != "initial" || row["logical_model_id"] != "smart" ||
		row["provider_id"] != "provider-a" || row["connection_id"] != "connection-a" || row["ttft_ms"] != float64(0) {
		t.Fatalf("downstream row identity/timing: %+v", row)
	}
	writer.release <- struct{}{}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not finish downstream delivery")
	}
	rec, q = f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, http.StatusOK)
	if q["open_requests"] != float64(0) || len(q["requests"].([]any)) != 0 {
		t.Fatalf("finished downstream request still queued: %+v", q)
	}
}

func TestQueueIncludesConnectionsWithoutTargets(t *testing.T) {
	f := newV2APIFixture(t)
	// Publish a separate snapshot; do not mutate the active world.
	old := f.server.deps.Scheduler.Snapshot()
	snapshot := *old
	snapshot.Connections = make(map[string]*domain.Connection, len(old.Connections)+1)
	for id, connection := range old.Connections {
		snapshot.Connections[id] = connection
	}
	snapshot.Connections["connection-unused"] = &domain.Connection{ID: "connection-unused", ProviderID: "provider-a", Concurrency: &domain.ConcurrencyPolicy{MaxConcurrent: 3}}
	f.server.deps.Scheduler.ReplaceWorld(&snapshot, nil, nil, nil)
	rec, q := f.do(http.MethodGet, "/api/queue", nil)
	requireStatus(t, rec, 200)
	for _, raw := range q["connections"].([]any) {
		c := raw.(map[string]any)
		if c["connection_id"] != "connection-unused" {
			continue
		}
		runtime := c["runtime"].(map[string]any)
		if c["policy_source"] != "connection" || runtime["max_concurrent"] != float64(3) || runtime["waiting"] != float64(0) {
			t.Fatalf("unused connection policy: %+v", c)
		}
		return
	}
	t.Fatalf("configured connection omitted: %+v", q)
}
