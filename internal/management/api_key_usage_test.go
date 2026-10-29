package management

import (
	"net/http"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestAPIKeyUsageZeroAndWindowedCount(t *testing.T) {
	f := newV2APIFixture(t)
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-time.Minute)
	window := "?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	rec, list := f.do(http.MethodGet, "/api/api-keys"+window, nil)
	requireStatus(t, rec, http.StatusOK)
	items := list["api_keys"].([]any)
	if len(items) != 1 {
		t.Fatalf("API key fixture: %+v", list)
	}
	item := items[0].(map[string]any)
	id := item["identity"].(map[string]any)["id"].(string)
	assertUsage := func(payload map[string]any, requests float64) {
		t.Helper()
		usage, ok := payload["usage"].(map[string]any)
		if !ok || usage["requests"] != requests {
			t.Fatalf("known usage must be numeric, want %v: %+v", requests, payload)
		}
	}
	assertUsage(item, 0)
	if list["from"] != from.Format(time.RFC3339) || list["to"] != to.Format(time.RFC3339) {
		t.Fatalf("list omitted actual usage window: %+v", list)
	}
	rec, detail := f.do(http.MethodGet, "/api/api-keys/"+id+window, nil)
	requireStatus(t, rec, http.StatusOK)
	assertUsage(detail, 0)
	if detail["last_request_at"] != nil {
		t.Fatalf("unused key has last request: %+v", detail)
	}
	for _, r := range []domain.RequestRecord{
		{ID: "inside", Ts: from.Add(time.Second), ConsumerKey: "initial"},
		{ID: "outside", Ts: to, ConsumerKey: "initial"},
		{ID: "other-key", Ts: from.Add(time.Second), ConsumerKey: "other"},
	} {
		f.ledger.BeginRequest(r)
		f.ledger.FinishRequest(r.ID, func(r *domain.RequestRecord) { r.Status = domain.ReqSuccess; r.Outcome = domain.OutcomeDirectSuccess })
	}
	// An unfinished ingress is visible in request logs, but not finalized usage.
	f.ledger.BeginIngressRequest(domain.RequestRecord{ID: "live", Ts: from.Add(time.Second), ConsumerKey: "initial", Status: "streaming"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec, list = f.do(http.MethodGet, "/api/api-keys"+window, nil)
		requireStatus(t, rec, http.StatusOK)
		item = list["api_keys"].([]any)[0].(map[string]any)
		if item["usage"].(map[string]any)["requests"] == float64(1) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	assertUsage(item, 1)
	rec, detail = f.do(http.MethodGet, "/api/api-keys/"+id+window, nil)
	requireStatus(t, rec, http.StatusOK)
	assertUsage(detail, 1)
}

func TestAPIKeyUsageUnavailableIsNotZero(t *testing.T) {
	for _, unavailable := range []string{"missing", "query-failed"} {
		t.Run(unavailable, func(t *testing.T) {
			f := newV2APIFixture(t)
			identities, _, err := f.control.APIKeys()
			if err != nil || len(identities) != 1 {
				t.Fatalf("identities: %+v %v", identities, err)
			}
			want := http.StatusServiceUnavailable
			if unavailable == "missing" {
				f.server.deps.Ledger = nil
			} else {
				if err := f.ledger.Close(); err != nil {
					t.Fatal(err)
				}
				want = http.StatusInternalServerError
			}
			for _, path := range []string{"/api/api-keys", "/api/api-keys/" + identities[0].ID} {
				rec, payload := f.do(http.MethodGet, path, nil)
				requireStatus(t, rec, want)
				if payload["usage"] != nil || payload["api_keys"] != nil {
					t.Fatalf("unavailable usage fabricated as zero: %+v", payload)
				}
			}
		})
	}
}
