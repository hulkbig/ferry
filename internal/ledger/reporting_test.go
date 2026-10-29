package ledger

import (
	"context"
	"ferry/internal/domain"
	"testing"
	"time"
)

func TestReportingRetainedDetailsAndHistoricalCoverage(t *testing.T) {
	l := openTemp(t)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "direct", "cancel"} {
		r := sampleRequest(id, from.Add(time.Duration(i)*time.Hour))
		if id == "cancel" {
			r.Status = domain.ReqCanceled
			r.Outcome = ""
		} else {
			r.Outcome = domain.OutcomeDirectSuccess
		}
		l.BeginRequest(r)
		l.FinishRequest(id, nil)
	}
	l = reopen(t, l)
	if n, e := l.PurgeOlderThan(from.Add(time.Hour)); e != nil || n != 1 {
		t.Fatalf("purge %d: %v", n, e)
	}
	to := from.Add(3 * time.Hour)
	report, e := l.QueryBilling(context.Background(), from, to)
	if e != nil {
		t.Fatal(e)
	}
	if report.Summary.Requests != 2 || report.HistoricalSummary.Requests != 3 {
		t.Fatalf("detail/historical: %+v / %+v", report.Summary, report.HistoricalSummary)
	}
	c := report.Coverage
	if c.DetailRequests != 2 || c.RollupRequests != 3 || c.RollupOnlyRequests != 1 || c.MismatchedHours != 0 {
		t.Fatalf("coverage %+v", c)
	}
	if c.EarliestDetail == nil || !c.EarliestDetail.Equal(from.Add(time.Hour)) || c.EarliestRollup == nil || !c.EarliestRollup.Equal(from) {
		t.Fatalf("observations %+v", c)
	}
	if report.Outcomes.DirectSuccess != 1 || report.Outcomes.Canceled != 1 || report.Outcomes.Failed != 0 {
		t.Fatalf("outcomes %+v", report.Outcomes)
	}
	var trend, models, providers int64
	for _, v := range report.Trend {
		trend += v.Requests
	}
	for _, v := range report.Models {
		models += v.Requests
	}
	for _, v := range report.Providers {
		providers += v.Requests
	}
	_, count, e := l.QueryRequests(domain.LogFilter{From: from, To: to})
	if e != nil {
		t.Fatal(e)
	}
	if trend != 2 || models != 2 || providers != 2 || count != 2 {
		t.Fatalf("report drill reconciliation %d %d %d %d", trend, models, providers, count)
	}
	if _, e = l.db.Exec("UPDATE usage_hourly SET requests=requests+1 WHERE bucket=?", from.Add(time.Hour).Unix()/3600); e != nil {
		t.Fatal(e)
	}
	c, e = l.ReportingCoverage(context.Background(), from, to)
	if e != nil || c.MismatchedHours != 1 || c.RollupOnlyRequests != 1 {
		t.Fatalf("mismatch %+v %v", c, e)
	}
	shortFrom, shortTo := from.Add(61*time.Minute), from.Add(62*time.Minute)
	c, e = l.ReportingCoverage(context.Background(), shortFrom, shortTo)
	if e != nil || c.DetailRequests != 0 || c.RollupRequests != 0 || c.ComparedFrom.Before(shortFrom) || c.ComparedTo.After(shortTo) {
		t.Fatalf("partial hour %+v %v", c, e)
	}
}

func TestReportRequestScopeMatchesParticipatingAttempt(t *testing.T) {
	l := openTemp(t)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for _, id := range []string{"m1", "m10", "fallback", "unknown", "cancel", "failed"} {
		r := sampleRequest(id, from)
		r.LogicalModelID = id
		r.ProviderID = "b"
		r.Outcome = ""
		if id == "fallback" {
			r.LogicalModelID = ""
			r.ModelRequested = "m1"
		}
		if id == "unknown" {
			r.LogicalModelID = ""
			r.ModelRequested = ""
		}
		if id == "cancel" {
			r.Status = domain.ReqCanceled
		}
		if id == "failed" {
			r.Status = domain.ReqFailed
		}
		l.BeginRequest(r)
		l.AddAttempt(domain.AttemptRecord{RequestID: id, Seq: 1, Ts: to.Add(time.Second), ProviderID: "a", Status: domain.ReqFailed})
		l.AddAttempt(domain.AttemptRecord{RequestID: id, Seq: 2, Ts: to.Add(2 * time.Second), ProviderID: "b", Status: domain.ReqSuccess})
		l.FinishRequest(id, nil)
	}
	l = reopen(t, l)
	cases := []struct {
		filter domain.LogFilter
		want   int
	}{
		{domain.LogFilter{LogicalModel: "m1", ParticipatingProvider: "a", AttemptStatus: "failed"}, 2},
		{domain.LogFilter{LogicalModel: "m1", ParticipatingProvider: "b", AttemptStatus: "failed"}, 0},
		{domain.LogFilter{LogicalModel: "m1", ParticipatingProvider: "a", AttemptFailureType: "failed"}, 2},
		{domain.LogFilter{LogicalModel: "m1", ParticipatingProvider: "b", AttemptFailureType: "failed"}, 0},
		{domain.LogFilter{LogicalModel: "__unknown__"}, 1},
		{domain.LogFilter{ReportError: "canceled"}, 1},
		{domain.LogFilter{ReportError: "failed"}, 1},
		{domain.LogFilter{ProviderID: "a"}, 0},
	}
	for _, c := range cases {
		c.filter.From = from
		c.filter.To = to
		_, n, e := l.QueryRequests(c.filter)
		if e != nil || n != c.want {
			t.Errorf("%+v: count %d want %d error %v", c.filter, n, c.want, e)
		}
	}
}

func TestAnalyticsNaturalBucketsPreserveExactBoundaries(t *testing.T) {
	l := openTemp(t)
	from := time.Date(2026, 9, 1, 0, 2, 3, 456000000, time.UTC)
	to := from.Add(time.Hour)
	for _, ts := range []time.Time{from, from.Add(3 * time.Minute), to.Add(-time.Millisecond), to} {
		id := ts.String()
		l.BeginRequest(sampleRequest(id, ts))
		l.FinishRequest(id, nil)
	}
	l = reopen(t, l)
	a, e := l.QueryAnalytics(context.Background(), from, to, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if a.BucketSeconds != 300 || len(a.Timeline) != 13 || !a.Timeline[0].From.Equal(from) || !a.Timeline[12].To.Equal(to) || !a.Timeline[0].Partial || !a.Timeline[12].Partial {
		t.Fatalf("buckets %+v", a.Timeline)
	}
	var count int64
	var samples int
	for i, b := range a.Timeline {
		count += b.Requests
		samples += b.TTFTSamples
		if i > 0 && !b.From.Equal(a.Timeline[i-1].To) {
			t.Fatal("bucket gap")
		}
		if b.Succeeded+b.Failed+b.Canceled != b.Requests {
			t.Fatal("outcome mismatch")
		}
	}
	if count != 3 || samples != 3 || a.Summary.Requests != 3 {
		t.Fatalf("cohort %d samples %d", count, samples)
	}
}
