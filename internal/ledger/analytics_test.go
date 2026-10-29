package ledger

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestAnalyticsAttributionAndCohort(t *testing.T) {
	l := openTemp(t)
	from := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	add := func(id, model string, ts time.Time, status domain.RequestStatus, outcome domain.RequestOutcome, latency int64, attempts ...domain.AttemptRecord) {
		l.BeginRequest(domain.RequestRecord{ID: id, Ts: ts, LogicalModelID: model, Status: status, Outcome: outcome, TTFTMS: latency, DurationMS: latency * 10, EstCashCost: 99, AllocatedSubCost: .5})
		for i, a := range attempts {
			a.RequestID = id
			a.Seq = i + 1
			a.Ts = to.Add(time.Second)
			l.AddAttempt(a)
		}
		l.FinishRequest(id, nil)
	}
	pay := domain.AttemptRecord{ProviderID: "a", PlanKind: domain.PlanPAYG, Status: domain.ReqSuccess, EstCashCost: 2, RateCardVersion: "historical", TTFTMS: 100, LatencyMS: 800, Usage: domain.Usage{InputTokens: 100, CacheReadTokens: 30, CacheWriteTokens: 10, OutputTokens: 20}, Protocol: domain.ProtocolOpenAIChat}
	failed := pay
	failed.Status = domain.ReqFailed
	failed.EstCashCost = 1
	failed.TTFTMS = 99999
	sub := pay
	sub.ProviderID = "b"
	sub.PlanKind = domain.PlanSubscription
	sub.EstCashCost = 20
	sub.AllocatedSubCost = .25
	sub.Protocol = domain.ProtocolAnthropic
	sub.TTFTMS = 300
	unknown := pay
	unknown.ProviderID = "c"
	unknown.RateCardVersion = ""
	unknown.EstCashCost = 0
	free := unknown
	free.PlanKind = domain.PlanFree
	add("recovered", "m1", from, domain.ReqSuccess, domain.OutcomeRecoveredSuccess, 500, failed, sub)
	add("direct", "m1", from.Add(time.Minute), domain.ReqSuccess, domain.OutcomeDirectSuccess, 100, pay)
	add("failed", "m2", from.Add(2*time.Minute), domain.ReqFailed, "", 99999, failed)
	add("unknown-price", "m2", from.Add(3*time.Minute), domain.ReqSuccess, "", 200, unknown)
	add("free", "m2", from.Add(4*time.Minute), domain.ReqSuccess, domain.OutcomeDirectSuccess, 0, free)
	add("missing", "m2", from.Add(5*time.Minute), domain.ReqCanceled, "", 10)
	add("excluded-end", "m1", to, domain.ReqSuccess, domain.OutcomeDirectSuccess, 10, pay)
	add("excluded-start", "m1", from.Add(-time.Millisecond), domain.ReqSuccess, domain.OutcomeDirectSuccess, 10, pay)
	l.BeginRequest(domain.RequestRecord{ID: "ongoing", Ts: from, LogicalModelID: "m1"})
	l.AddAttempt(domain.AttemptRecord{RequestID: "ongoing", Seq: 1, Ts: from, ProviderID: "a", PlanKind: domain.PlanPAYG, EstCashCost: 100})
	flushLiveTest(t, l.Ledger)
	query := func(model, provider string) *Analytics {
		t.Helper()
		a, e := l.QueryAnalytics(context.Background(), from, to, model, provider)
		if e != nil {
			t.Fatal(e)
		}
		return a
	}
	closeCost := func(got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("cost=%v want=%v", got, want)
		}
	}
	all := query("", "")
	s := all.Summary
	if s.Requests != 6 || s.Attempts != 6 || s.Succeeded != 4 || s.Direct != 2 || s.Recovered != 1 || s.Failed != 1 || s.Canceled != 1 {
		t.Fatalf("outcomes: %+v", s)
	}
	closeCost(s.Cost, 4.75)
	closeCost(s.CashCost, 4)
	closeCost(s.SubscriptionCost, .75)
	closeCost(s.RetryCost, .25)
	closeCost(s.FailedAttemptCost, 2)
	closeCost(s.FailedRequestCost, 1.5)
	closeCost(s.UnclassifiedReferenceCost, 99)
	if s.PricedAttempts != 5 || s.UnpricedAttempts != 1 || s.MissingAttempts != 1 {
		t.Fatalf("coverage: %+v", s)
	}
	if s.TTFTSamples != 3 || *s.TTFTP50 != 200 || *s.TTFTP95 != 500 {
		t.Fatalf("quantiles: %+v", s)
	}
	if s.UncachedInput != 400 || s.CacheRead != 180 || s.CacheWrite != 60 || s.Output != 120 {
		t.Fatalf("tokens: %+v", s)
	}
	for _, groups := range [][]AnalyticsMetric{all.Models, all.Providers} {
		var sum float64
		for _, g := range groups {
			sum += g.Cost
		}
		closeCost(sum, s.Cost)
	}
	var timelineCost float64
	var timelineRequests int64
	for _, b := range all.Timeline {
		timelineCost += b.Cost
		timelineRequests += b.Requests
	}
	closeCost(timelineCost, s.Cost)
	if timelineRequests != s.Requests || len(all.Timeline) != 12 {
		t.Fatal("timeline does not reconcile")
	}
	a := query("", "a").Summary
	closeCost(a.Cost, 4)
	if a.Basis != "attempts" || a.Requests != 3 || a.Attempts != 3 || a.Succeeded != 2 || a.AttemptSucceeded != 1 || *a.TTFTP95 != 100 {
		t.Fatalf("provider quality: %+v", a)
	}
	b := query("m1", "b").Summary
	closeCost(b.Cost, .25)
	if b.Requests != 1 || b.Attempts != 1 || *b.TTFTP95 != 300 {
		t.Fatalf("intersection: %+v", b)
	}
	closeCost(query("m1", "").Summary.Cost, 3.25)
	empty := query("absent", "").Summary
	if empty.Requests != 0 || empty.TTFTP95 != nil || empty.Errors == nil {
		t.Fatalf("empty: %+v", empty)
	}
	if _, e := l.QueryAnalytics(context.Background(), to, to, "", ""); e == nil {
		t.Fatal("accepted zero window")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := l.QueryAnalytics(ctx, from, to, "", ""); e == nil {
		t.Fatal("ignored canceled context")
	}
}

func TestAnalyticsTheoreticalEstimatesArePerAttempt(t *testing.T) {
	l := openTemp(t)
	from := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	type observation struct {
		provider                   string
		input, output, read, write int64
	}
	want := map[string]observation{
		"failed":        {"api", 100, 20, 30, 10},
		"retry":         {"subscription", 140, 20, 30, 10},
		"legacy":        {"other", 100, 20, 30, 10},
		"complimentary": {"free-plan", 100, 20, 30, 10},
		"missing-price": {"other", 100, 20, 30, 10},
		"zero-price":    {"other", 100, 20, 30, 10},
	}
	values := map[string]float64{"failed": 1, "retry": 2, "legacy": 3, "complimentary": 4, "zero-price": 0}
	for i, row := range []struct {
		model string
		plan  domain.PlanKind
	}{
		{"failed", domain.PlanPAYG}, {"retry", domain.PlanSubscription}, {"legacy", ""},
		{"complimentary", domain.PlanFree}, {"missing-price", ""}, {"zero-price", ""},
	} {
		id, seq := row.model, 1
		if i < 2 {
			id, seq = "recovered", i+1
		}
		if i != 1 {
			l.BeginRequest(domain.RequestRecord{ID: id, Ts: from, LogicalModelID: "logical-alias", UpstreamModel: "final-model", Status: domain.ReqSuccess})
		}
		status, protocol := domain.ReqSuccess, domain.ProtocolOpenAIResponses
		if i == 0 {
			status = domain.ReqFailed
		}
		if i == 1 {
			protocol = domain.ProtocolAnthropic
		}
		l.AddAttempt(domain.AttemptRecord{RequestID: id, Seq: seq, Ts: from.Add(2 * time.Hour), ProviderID: want[row.model].provider, Model: row.model, PlanKind: row.plan, Protocol: protocol, Status: status, EstCashCost: 7, AllocatedSubCost: .25, RateCardVersion: "historical", Usage: domain.Usage{InputTokens: 100, CacheReadTokens: 30, CacheWriteTokens: 10, OutputTokens: 20}})
		if i != 0 {
			l.FinishRequest(id, nil)
		}
	}
	l.BeginRequest(domain.RequestRecord{ID: "no-attempt", Ts: from, LogicalModelID: "logical-alias", Status: domain.ReqFailed})
	l.FinishRequest("no-attempt", nil)
	l = reopen(t, l)
	seen := map[string]int{}
	estimate := func(provider, model string, input, output, read, write int64) *float64 {
		seen[model]++
		if got := (observation{provider, input, output, read, write}); got != want[model] {
			t.Fatalf("estimate %q uses wrong upstream model/usage: %+v want %+v", model, got, want[model])
		}
		if value, ok := values[model]; ok {
			return &value
		}
		return nil
	}
	all, err := l.QueryAnalyticsWithEstimates(context.Background(), from, from.Add(time.Hour), "", "", estimate)
	if err != nil {
		t.Fatal(err)
	}
	s := all.Summary
	if s.TheoreticalCost != 10 || s.TheoreticalPricedAttempts != 5 || s.TheoreticalUnpricedAttempts != 1 || s.MissingAttempts != 1 {
		t.Fatalf("theoretical totals: %+v", s)
	}
	for model := range want {
		if seen[model] != 1 {
			t.Fatalf("%s estimated %d times", model, seen[model])
		}
	}
	for _, groups := range [][]AnalyticsMetric{all.Models, all.Providers} {
		var cost float64
		var priced, unpriced int64
		for _, g := range groups {
			cost += g.TheoreticalCost
			priced += g.TheoreticalPricedAttempts
			unpriced += g.TheoreticalUnpricedAttempts
		}
		if cost != s.TheoreticalCost || priced != s.TheoreticalPricedAttempts || unpriced != s.TheoreticalUnpricedAttempts {
			t.Fatal("group estimates do not reconcile")
		}
	}
	var cost float64
	var priced, unpriced int64
	for _, b := range all.Timeline {
		cost += b.TheoreticalCost
		priced += b.TheoreticalPricedAttempts
		unpriced += b.TheoreticalUnpricedAttempts
	}
	if cost != s.TheoreticalCost || priced != s.TheoreticalPricedAttempts || unpriced != s.TheoreticalUnpricedAttempts {
		t.Fatal("timeline estimates do not reconcile")
	}
	filtered, err := l.QueryAnalyticsWithEstimates(context.Background(), from, from.Add(time.Hour), "logical-alias", "subscription", estimate)
	if err != nil || filtered.Summary.TheoreticalCost != 2 || filtered.Summary.Attempts != 1 {
		t.Fatalf("filtered estimate: %+v %v", filtered, err)
	}
	baseline, err := l.QueryAnalytics(context.Background(), from, from.Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.TheoreticalCost, s.TheoreticalPricedAttempts, s.TheoreticalUnpricedAttempts = baseline.Summary.TheoreticalCost, baseline.Summary.TheoreticalPricedAttempts, baseline.Summary.TheoreticalUnpricedAttempts
	if !reflect.DeepEqual(s, baseline.Summary) {
		t.Fatal("current pricing changed historical accounting")
	}
}

// A wider window includes historical rows with explicit NULLs. SQLite DEFAULT
// does not replace NULL, and scanning these values into Go scalars used to fail
// the entire report even though a recent 24h report succeeded.
func TestAnalyticsHistoricalNullsAcrossWindows(t *testing.T) {
	l := openTemp(t)
	to := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id  string
		age time.Duration
	}{{"recent", time.Hour}, {"week", 48 * time.Hour}, {"month", 10 * 24 * time.Hour}} {
		_, err := l.db.Exec(`INSERT INTO logical_requests
 (id,ts,logical_model_id,status,outcome,failure_type,ttft_ms,duration_ms,est_cash_cost,allocated_sub_cost)
 VALUES (?,?,NULL,'success',NULL,NULL,NULL,NULL,NULL,NULL)`, row.id, to.Add(-row.age).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		_, err = l.db.Exec(`INSERT INTO attempts
 (request_id,seq,ts,target_id,provider_id,status,failure_type,ttft_ms,latency_ms,plan_kind,rate_card_version,est_cash_cost,allocated_sub_cost,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,protocol,model)
 VALUES (?,1,?,'',NULL,'success',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)`, row.id, to.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	// Keep the 24h cohort fully populated to isolate the wider-window failure.
	if _, err := l.db.Exec(`UPDATE logical_requests SET outcome='',failure_type='',ttft_ms=0,duration_ms=0,est_cash_cost=0,allocated_sub_cost=0 WHERE id='recent'`); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec(`UPDATE attempts SET failure_type='',ttft_ms=0,latency_ms=0,plan_kind='',rate_card_version='',est_cash_cost=0,allocated_sub_cost=0,input_tokens=0,cache_read_tokens=0,cache_write_tokens=0,output_tokens=0,protocol='',model='' WHERE request_id='recent'`); err != nil {
		t.Fatal(err)
	}
	// A known rate card without a recorded amount must not become known free.
	if _, err := l.db.Exec(`UPDATE attempts SET plan_kind='payg',rate_card_version='historical' WHERE request_id='week'`); err != nil {
		t.Fatal(err)
	}
	for i, window := range []time.Duration{24 * time.Hour, 168 * time.Hour, 720 * time.Hour} {
		for _, provider := range []string{"", "__unknown__"} {
			a, err := l.QueryAnalytics(context.Background(), to.Add(-window), to, "", provider)
			if err != nil {
				t.Fatalf("window %s provider %q: %v", window, provider, err)
			}
			if a.Summary.Requests != int64(i+1) || a.Summary.Attempts != int64(i+1) || a.Summary.UnpricedAttempts != int64(i+1) || a.Summary.PricedAttempts != 0 || a.Summary.TTFTP95 != nil {
				t.Fatalf("window %s: %+v", window, a.Summary)
			}
			var attempts int64
			for _, bucket := range a.Timeline {
				attempts += bucket.Attempts
			}
			if attempts != a.Summary.Attempts {
				t.Fatal("timeline does not reconcile")
			}
		}
	}
}
