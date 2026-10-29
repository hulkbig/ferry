package management

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestReportDrillHTTPPreservesExactModelAndSameAttemptScope(t *testing.T) {
	f := newV2APIFixture(t)
	from := time.Date(2026, 9, 1, 0, 0, 0, 123000000, time.UTC)
	to := from.Add(time.Hour)
	for _, r := range []domain.RequestRecord{
		{ID: "match", Ts: from, LogicalModelID: "m1", ProviderID: "b", Status: domain.ReqSuccess, Outcome: domain.OutcomeRecoveredSuccess},
		{ID: "different-model", Ts: from, LogicalModelID: "m10", ProviderID: "b", Status: domain.ReqSuccess},
		{ID: "excluded-end", Ts: to, LogicalModelID: "m1", ProviderID: "b", Status: domain.ReqSuccess},
		{ID: "canceled", Ts: from, LogicalModelID: "other", ProviderID: "b", Status: domain.ReqCanceled},
	} {
		f.ledger.BeginRequest(r)
		f.ledger.AddAttempt(domain.AttemptRecord{RequestID: r.ID, Seq: 1, Ts: to.Add(time.Second), ProviderID: "a", Status: domain.ReqFailed})
		f.ledger.AddAttempt(domain.AttemptRecord{RequestID: r.ID, Seq: 2, Ts: to.Add(2 * time.Second), ProviderID: "b", Status: domain.ReqSuccess})
		f.ledger.FinishRequest(r.ID, nil)
	}
	bounds := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}}
	for attempt := 0; attempt < 100; attempt++ {
		rec, response := f.do(http.MethodGet, "/api/requests?"+bounds.Encode(), nil)
		requireStatus(t, rec, http.StatusOK)
		if response["total"] == float64(3) {
			break
		}
		if attempt == 99 {
			t.Fatalf("ledger did not converge: %v", response)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, test := range []struct {
		name    string
		filters map[string]string
		want    float64
	}{
		{"participant failed attempt", map[string]string{"logical_model": "m1", "participating_provider": "a", "attempt_status": "failed"}, 1},
		{"same attempt required", map[string]string{"logical_model": "m1", "participating_provider": "b", "attempt_status": "failed"}, 0},
		{"failure fallback", map[string]string{"logical_model": "m1", "participating_provider": "a", "attempt_failure_type": "failed"}, 1},
		{"final provider unchanged", map[string]string{"provider": "a"}, 0},
		{"cancellation error fallback", map[string]string{"report_error": "canceled"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := url.Values{"from": bounds["from"], "to": bounds["to"]}
			for key, value := range test.filters {
				query.Set(key, value)
			}
			rec, response := f.do(http.MethodGet, "/api/requests?"+query.Encode(), nil)
			requireStatus(t, rec, http.StatusOK)
			if response["total"] != test.want {
				t.Fatalf("response=%v want total=%v", response, test.want)
			}
		})
	}
}
