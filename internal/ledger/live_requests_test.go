package ledger

import (
	"context"
	"ferry/internal/domain"
	"fmt"
	"testing"
	"time"
)

// A committed marker establishes that earlier writer jobs are queryable.
func flushLiveTest(t *testing.T, l *Ledger) {
	t.Helper()
	marker := fmt.Sprintf("test-flush-%d", time.Now().UnixNano())
	l.enqueue(func(db execer) error {
		_, err := db.Exec("INSERT INTO runtime_settings(key,value,updated_at) VALUES (?, 'done', 0)", marker)
		return err
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var value string
		if l.read.QueryRow("SELECT value FROM runtime_settings WHERE key=?", marker).Scan(&value) == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ledger writes did not commit")
}

func TestLiveRequestLifecycleAndAnalyticsIsolation(t *testing.T) {
	l := openTemp(t)
	now := time.Now()
	l.BeginRequest(domain.RequestRecord{ID: "live", Ts: now, Status: "received"})
	flushLiveTest(t, l.Ledger)
	r, err := l.QueryRequest("live")
	if err != nil || r == nil || r.Status != "received" {
		t.Fatalf("received: %+v %v", r, err)
	}
	l.UpdateRequest("live", func(r *domain.RequestRecord) { r.Status = "waiting"; r.ProviderID = "p" })
	flushLiveTest(t, l.Ledger)
	rows, total, err := l.QueryRequests(domain.LogFilter{Status: "waiting"})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ProviderID != "p" {
		t.Fatalf("waiting: %+v %d %v", rows, total, err)
	}
	a, err := l.QueryAnalytics(context.Background(), now.Add(-time.Minute), now.Add(time.Minute), "", "")
	if err != nil || a.Summary.Requests != 0 {
		t.Fatalf("live polluted analytics: %+v %v", a, err)
	}
	_, total, err = l.QueryRequests(domain.LogFilter{Outcome: domain.OutcomeFailed})
	if err != nil || total != 0 {
		t.Fatalf("live counted failed: %d %v", total, err)
	}
	l.FinishRequest("live", func(r *domain.RequestRecord) { r.Status = domain.ReqSuccess; r.Outcome = domain.OutcomeDirectSuccess })
	l.FinishRequest("live", nil)
	flushLiveTest(t, l.Ledger)
	rows, total, err = l.QueryRequests(domain.LogFilter{})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Status != domain.ReqSuccess {
		t.Fatalf("final: %+v %d %v", rows, total, err)
	}
	var count int
	if err := l.read.QueryRow("SELECT count(*) FROM live_requests").Scan(&count); err != nil || count != 0 {
		t.Fatalf("live cleanup: %d %v", count, err)
	}
	a, err = l.QueryAnalytics(context.Background(), now.Add(-time.Minute), now.Add(time.Minute), "", "")
	if err != nil || a.Summary.Requests != 1 {
		t.Fatalf("final analytics: %+v %v", a, err)
	}
}

func TestRestartFinalizesLiveRequestsOnce(t *testing.T) {
	l := openTemp(t)
	l.BeginRequest(domain.RequestRecord{ID: "stranded", Ts: time.Now(), Status: "waiting"})
	l = reopen(t, l)
	r, err := l.QueryRequest("stranded")
	if err != nil || r == nil || r.Status != domain.ReqCanceled {
		t.Fatalf("restart: %+v %v", r, err)
	}
	l = reopen(t, l)
	var count int
	if err := l.read.QueryRow("SELECT sum(requests) FROM usage_hourly").Scan(&count); err != nil || count != 1 {
		t.Fatalf("double rollup: %d %v", count, err)
	}
}

func TestLiveBurstDoesNotDropLifecycleWrites(t *testing.T) {
	l, err := Open(t.TempDir(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 300; i++ {
		id := fmt.Sprint(i)
		l.BeginRequest(domain.RequestRecord{ID: id, Ts: time.Now(), Status: "received"})
		l.FinishRequest(id, func(r *domain.RequestRecord) { r.Status = domain.ReqSuccess })
	}
	flushLiveTest(t, l)
	_, total, err := l.QueryRequests(domain.LogFilter{})
	if err != nil || total != 300 {
		t.Fatalf("lost writes: %d %v", total, err)
	}
}

func TestIngressDeliveryRetainsLiveRequestAndClientTTFT(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			l := openTemp(t)
			start := time.Now().Add(-time.Second)
			l.BeginIngressRequest(domain.RequestRecord{ID: "delivery", Ts: start, Stream: stream, Status: "active"})
			l.AddAttempt(domain.AttemptRecord{RequestID: "delivery", Seq: 1, Ts: start, TTFTMS: 25})
			l.FinishRequest("delivery", func(r *domain.RequestRecord) {
				r.Status, r.Outcome, r.TTFTMS = domain.ReqSuccess, domain.OutcomeDirectSuccess, 25
			})
			phase := domain.RequestStatus("delivering")
			if stream {
				phase = "streaming"
			}
			rows := l.LiveRequests()
			if len(rows) != 1 || rows[0].Status != phase || rows[0].TTFTMS != 0 || rows[0].Outcome != "" {
				t.Fatalf("upstream finish ended client lifetime: %+v", rows)
			}
			l.RecordFirstOutput("delivery", start.Add(180*time.Millisecond))
			l.RecordFirstOutput("delivery", start.Add(250*time.Millisecond))
			flushLiveTest(t, l.Ledger)
			r, err := l.QueryRequest("delivery")
			if err != nil || r == nil || r.Status != phase || r.TTFTMS != 180 {
				t.Fatalf("live delivery TTFT: %+v %v", r, err)
			}
			a, err := l.QueryAnalytics(context.Background(), start.Add(-time.Minute), time.Now(), "", "")
			if err != nil || a.Summary.Requests != 0 {
				t.Fatalf("delivery counted as completed: %+v %v", a, err)
			}
			l.EndIngressRequest("delivery")
			l.EndIngressRequest("delivery")
			l.FinishRequest("delivery", nil)
			l.RecordFirstOutput("delivery", time.Now())
			flushLiveTest(t, l.Ledger)
			r, err = l.QueryRequest("delivery")
			if err != nil || r == nil || r.Status != domain.ReqSuccess || r.TTFTMS != 180 || r.DurationMS < 1000 {
				t.Fatalf("final client timing: %+v %v", r, err)
			}
			attempts, err := l.QueryAttempts("delivery")
			if err != nil || len(attempts) != 1 || attempts[0].TTFTMS != 25 {
				t.Fatalf("attempt TTFT changed: %+v %v", attempts, err)
			}
			if len(l.LiveRequests()) != 0 {
				t.Fatal("completed request is still live")
			}
			var count int
			if err := l.read.QueryRow("SELECT count(*) FROM live_requests").Scan(&count); err != nil || count != 0 {
				t.Fatalf("late update resurrected live row: %d %v", count, err)
			}
		})
	}
}

func TestIngressFirstOutputSurvivesSchedulerCompletionInEitherOrder(t *testing.T) {
	for _, handlerFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("handler-first=", handlerFirst), func(t *testing.T) {
			l := openTemp(t)
			start := time.Now().Add(-time.Second)
			l.BeginIngressRequest(domain.RequestRecord{ID: "stream", Ts: start, Status: "streaming", Stream: true})
			l.RecordFirstOutput("stream", start.Add(200*time.Millisecond))
			flushLiveTest(t, l.Ledger)
			live, err := l.QueryRequest("stream")
			if err != nil || live == nil || live.TTFTMS != 200 || live.Status != "streaming" {
				t.Fatalf("TTFT missing during execution: %+v %v", live, err)
			}
			if handlerFirst {
				l.EndIngressRequest("stream")
			}
			l.FinishRequest("stream", func(r *domain.RequestRecord) { r.Status = domain.ReqSuccess; r.TTFTMS = 10 })
			l.EndIngressRequest("stream")
			flushLiveTest(t, l.Ledger)
			final, err := l.QueryRequest("stream")
			if err != nil || final == nil || final.TTFTMS != 200 || final.Status != domain.ReqSuccess {
				t.Fatalf("client TTFT overwritten: %+v %v", final, err)
			}
		})
	}
}
