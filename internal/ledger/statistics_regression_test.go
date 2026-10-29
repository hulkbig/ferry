package ledger

import (
	"fmt"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestLatencyPercentilesUseNearestRank(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3, 20, 21, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			l := openTemp(t)
			base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
			for i := 1; i <= count; i++ {
				r := sampleRequest(fmt.Sprint(i), base.Add(time.Duration(i)*time.Second))
				r.DurationMS = int64(i * 100)
				l.BeginRequest(r)
				l.FinishRequest(r.ID, nil)
			}
			l2 := reopen(t, l)
			summary, err := l2.Summarize(base, base.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			want := int64((count*95 + 99) / 100 * 100)
			if summary.P95LatencyMS != want {
				t.Fatalf("P95 of %d durations = %d, want %d", count, summary.P95LatencyMS, want)
			}
			if summary.MedianLatencyMS != nil && summary.P95LatencyMS < *summary.MedianLatencyMS {
				t.Fatal("P95 must not be below the median")
			}
		})
	}
}

func TestStatisticsReconcileAcrossHourBoundaries(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 9, 5, 0, 30, 0, 0, time.UTC)
	statuses := []domain.RequestStatus{domain.ReqSuccess, domain.ReqCanceled, domain.ReqFailed, domain.ReqSuccess}
	for i, status := range statuses {
		r := sampleRequest(fmt.Sprint(i), base.Add(time.Duration(i)*time.Hour))
		r.Status = status
		r.Outcome = domain.OutcomeFailed
		if status == domain.ReqSuccess {
			r.Outcome = domain.OutcomeDirectSuccess
		}
		r.AllocatedSubCost, r.ShadowCost = 0.5, 1
		l.BeginRequest(r)
		l.FinishRequest(r.ID, nil)
	}
	l2 := reopen(t, l)
	end := base.Add(4 * time.Hour)
	summary, err := l2.Summarize(base, end)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 4 || summary.Succeeded != 2 || summary.Failed != 1 || summary.Canceled != 1 {
		t.Errorf("success/failed/canceled must partition requests: %+v", summary)
	}
	if summary.AllocatedSub != 2 || summary.ShadowCost != 4 {
		t.Errorf("full hours must retain subscription/shadow dimensions: %+v", summary)
	}
	outcomes, err := l2.OutcomeSummary(base, end)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Succeeded != outcomes.DirectSuccess+outcomes.RecoveredSuccess || summary.Failed+summary.Canceled != outcomes.Failed {
		t.Errorf("summary and client outcomes disagree: summary=%+v outcomes=%+v", summary, outcomes)
	}
	targets, err := l2.ByTarget(base, end)
	if err != nil || len(targets) != 1 || targets[0].Failures != 2 {
		t.Errorf("target failures must include canceled requests in full buckets: %+v, %v", targets, err)
	}
}

func TestTargetUsageIncludesAdjacentHourTail(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 9, 5, 0, 59, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		r := sampleRequest(fmt.Sprint(i), base.Add(time.Duration(i)*time.Minute))
		l.BeginRequest(r)
		l.FinishRequest(r.ID, nil)
	}
	l2 := reopen(t, l)
	targets, err := l2.ByTarget(base, base.Add(2*time.Minute))
	if err != nil || len(targets) != 1 || targets[0].Requests != 2 {
		t.Fatalf("adjacent-hour requests lost: %+v, %v", targets, err)
	}
}

func TestBillingRetainsUnassignedUsageAndLegacyCosts(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		r := sampleRequest(fmt.Sprint(i), base.Add(time.Duration(i)*time.Second))
		if i == 0 {
			r.ProviderID, r.LogicalModelID = "", ""
			r.EstCashCost, r.ServingCost, r.RecoveryOverheadCost = 0.5, 0, 0
		} else {
			r.EstCashCost, r.ServingCost, r.RecoveryOverheadCost = 0.3, 0.2, 0.1
		}
		l.BeginRequest(r)
		l.FinishRequest(r.ID, nil)
	}
	l2 := reopen(t, l)
	end := base.Add(time.Minute)
	summary, err := l2.Summarize(base, end)
	if err != nil {
		t.Fatal(err)
	}
	if abs64(summary.TotalCost-0.8) > 1e-9 {
		t.Errorf("mixed legacy/current costs must add per request: %+v", summary)
	}
	for _, dimension := range []string{"provider_id", "logical_model_id"} {
		groups, err := l2.UsageByDimension(dimension, base, end)
		if err != nil {
			t.Fatal(err)
		}
		var requests, upstreamTokens int64
		var cost float64
		for _, group := range groups {
			requests += group.Requests
			upstreamTokens += group.UpstreamInputTokens
			cost += group.TotalCost
		}
		if groups[""] == nil || requests != summary.Requests || upstreamTokens != summary.UpstreamInputTokens || abs64(cost-summary.TotalCost) > 1e-9 {
			t.Errorf("%s must reconcile including unassigned rows: %+v", dimension, groups)
		}
	}
	trend, err := l2.UsageTrend(base, end, 2)
	if err != nil {
		t.Fatal(err)
	}
	var cost float64
	for _, point := range trend {
		cost += point.TotalCost
	}
	if abs64(cost-summary.TotalCost) > 1e-9 {
		t.Errorf("cost trend=%v summary=%v", cost, summary.TotalCost)
	}
}

func TestTrendsCoverShortWindowsAndTailMilliseconds(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for index, offset := range []time.Duration{0, 500 * time.Millisecond, 1000 * time.Millisecond} {
		r := sampleRequest(fmt.Sprintf("tail-%d", index), base.Add(offset))
		l.BeginRequest(r)
		l.FinishRequest(r.ID, nil)
	}
	l2 := reopen(t, l)
	to := base.Add(1001 * time.Millisecond)

	usage, err := l2.UsageTrend(base, to, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 || usage[0].Requests+usage[1].Requests != 3 || usage[1].Requests != 1 {
		t.Fatalf("usage trend lost the non-divisible window tail: %+v", usage)
	}
	if !usage[0].From.Equal(base) || !usage[len(usage)-1].To.Equal(to) || usage[0].To.After(usage[1].From) {
		t.Fatalf("usage trend boundaries do not cover the requested window: %+v", usage)
	}

	traffic, err := l2.TrafficTrend(base, to, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 1 || traffic[0].Requests != 3 || traffic[0].Points[1].Requests != 1 {
		t.Fatalf("traffic trend lost the non-divisible window tail: %+v", traffic)
	}

	short, err := l2.UsageTrend(base, base.Add(10*time.Millisecond), 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) != 24 || short[0].Requests != 1 || !short[len(short)-1].To.Equal(base.Add(10*time.Millisecond)) {
		t.Fatalf("short-window trend is malformed: %+v", short)
	}
}
