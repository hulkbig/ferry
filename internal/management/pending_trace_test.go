package management

import (
	"net/http"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestV2RequestDetailPendingTraceUpdates(t *testing.T) {
	f := newV2APIFixture(t)
	const id = "req-v2-pending-trace"
	started := time.Now().UTC().Add(-time.Second)
	f.ledger.BeginRequest(domain.RequestRecord{
		ID: id, Ts: started, ConsumerKey: "initial", LogicalModelID: "smart",
		Protocol: domain.ProtocolOpenAIChat, Status: "received",
	})
	events := []domain.ExecutionTraceEvent{
		{At: started, Category: domain.TraceRoute, Type: "REQUEST_RECEIVED"},
		{At: started.Add(10 * time.Millisecond), Category: domain.TraceRoute, Type: "PLANNING_STARTED"},
		{At: started.Add(20 * time.Millisecond), Category: domain.TraceRoute, Type: "MODEL_RESOLVED", Message: "smart"},
	}

	// Never finish the request or create an attempt: both responses must come
	// from the live request and its latest in-memory trace.
	for _, count := range []int{2, 3} {
		f.ledger.SetTrace(id, &domain.TraceRecord{Events: events[:count]})
		rec, response := f.do(http.MethodGet, "/api/requests/"+id, nil)
		if count == 2 {
			// BeginRequest writes the live row asynchronously. Wait only for that
			// row's first appearance; missing or stale trace events must fail.
			deadline := time.Now().Add(5 * time.Second)
			for rec.Code == http.StatusNotFound && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
				rec, response = f.do(http.MethodGet, "/api/requests/"+id, nil)
			}
		}
		requireStatus(t, rec, http.StatusOK)

		request, ok := response["client_request"].(map[string]any)
		if !ok {
			t.Fatalf("missing V2 client_request: %s", rec.Body.String())
		}
		// These are v2RequestSummary's public fields, not the queue view's phase.
		for field, want := range map[string]any{
			"id": id, "status": "received", "outcome": "", "failure_type": "",
			"logical_model_id": "smart", "attempts": float64(0),
		} {
			if request[field] != want {
				t.Fatalf("%d phases: client_request.%s=%v, want %v", count, field, request[field], want)
			}
		}
		attempts, ok := response["attempts"].([]any)
		if !ok || len(attempts) != 0 {
			t.Fatalf("%d phases: expected empty attempts array, got %v", count, response["attempts"])
		}
		gotEvents, ok := response["execution_trace_events"].([]any)
		if !ok || len(gotEvents) != count {
			t.Fatalf("%d phases: execution_trace_events=%v", count, response["execution_trace_events"])
		}
		for i, want := range events[:count] {
			event, ok := gotEvents[i].(map[string]any)
			if !ok || event["type"] != want.Type || event["category"] != string(want.Category) ||
				event["at"] != want.At.Format(time.RFC3339Nano) {
				t.Fatalf("%d phases: event[%d]=%v, want %+v", count, i, gotEvents[i], want)
			}
		}
		if live := f.ledger.LiveRequests(); len(live) != 1 || live[0].ID != id || live[0].Status != "received" {
			t.Fatalf("trace detail read ended the live request: %+v", live)
		}
	}
}
