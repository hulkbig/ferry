package ledger

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"time"
)

// Analytics is a read-only view of a closed-request cohort, including all its
// attempts. Theoretical current-price estimates never alter recorded costs.
type Analytics struct {
	From             time.Time         `json:"from"`
	To               time.Time         `json:"to"`
	Basis            string            `json:"basis"`
	Summary          AnalyticsMetric   `json:"summary"`
	Models           []AnalyticsMetric `json:"models"`
	Providers        []AnalyticsMetric `json:"providers"`
	Coverage         ReportingCoverage `json:"coverage"`
	BucketSeconds    int64             `json:"bucket_seconds"`
	GeneratedAt      time.Time         `json:"generated_at"`
	Timeline         []AnalyticsBucket `json:"timeline"`
	ReferencePricing *AnalyticsPricing `json:"reference_pricing,omitempty"`
}

type AnalyticsPricing struct {
	Source    string    `json:"source"`
	Version   string    `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Stale     bool      `json:"stale"`
}

// AnalyticsEstimator receives the exact attempted model and normalized total
// input (including cache reads/writes). Nil cost means unavailable, not free.
type AnalyticsEstimator func(provider, model string, input, output, read, write int64) *float64

type AnalyticsMetric struct {
	ID                          string           `json:"id,omitempty"`
	Basis                       string           `json:"basis"`
	Requests                    int64            `json:"requests"`
	Direct                      int64            `json:"direct"`
	Recovered                   int64            `json:"recovered"`
	Succeeded                   int64            `json:"succeeded"`
	Failed                      int64            `json:"failed"`
	Canceled                    int64            `json:"canceled"`
	Attempts                    int64            `json:"attempts"`
	AttemptFailed               int64            `json:"attempt_failed"`
	AttemptCanceled             int64            `json:"attempt_canceled"`
	AttemptSucceeded            int64            `json:"attempt_succeeded"`
	PricedAttempts              int64            `json:"priced_attempts"`
	UnpricedAttempts            int64            `json:"unpriced_attempts"`
	MissingAttempts             int64            `json:"missing_attempts"`
	CashCost                    float64          `json:"cash_cost"`
	SubscriptionCost            float64          `json:"subscription_cost"`
	Cost                        float64          `json:"cost"`
	UnclassifiedReferenceCost   float64          `json:"unclassified_reference_cost"`
	ReferenceCost               float64          `json:"reference_cost"`
	TheoreticalCost             float64          `json:"theoretical_cost"`
	TheoreticalPricedAttempts   int64            `json:"theoretical_priced_attempts"`
	TheoreticalUnpricedAttempts int64            `json:"theoretical_unpriced_attempts"`
	RetryCost                   float64          `json:"retry_cost"`
	FailedAttemptCost           float64          `json:"failed_attempt_cost"`
	FailedRequestCost           float64          `json:"failed_request_cost"`
	UncachedInput               int64            `json:"uncached_input"`
	CacheRead                   int64            `json:"cache_read"`
	CacheWrite                  int64            `json:"cache_write"`
	Output                      int64            `json:"output"`
	TTFTP50                     *int64           `json:"ttft_p50_ms"`
	TTFTP95                     *int64           `json:"ttft_p95_ms"`
	DurationP50                 *int64           `json:"duration_p50_ms"`
	DurationP95                 *int64           `json:"duration_p95_ms"`
	TTFTSamples                 int              `json:"ttft_samples"`
	DurationSamples             int              `json:"duration_samples"`
	Errors                      []AnalyticsError `json:"errors"`
}
type AnalyticsError struct {
	Cause string `json:"cause"`
	Count int64  `json:"count"`
}
type AnalyticsBucket struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Partial bool      `json:"partial"`
	// Requests is the chart count in Basis (client requests or attempts).
	Requests                    int64   `json:"requests"`
	Succeeded                   int64   `json:"succeeded"`
	Direct                      int64   `json:"direct"`
	Recovered                   int64   `json:"recovered"`
	Failed                      int64   `json:"failed"`
	Canceled                    int64   `json:"canceled"`
	Attempts                    int64   `json:"attempts"`
	PricedAttempts              int64   `json:"priced_attempts"`
	UnpricedAttempts            int64   `json:"unpriced_attempts"`
	Cost                        float64 `json:"cost"`
	TheoreticalCost             float64 `json:"theoretical_cost"`
	TheoreticalPricedAttempts   int64   `json:"theoretical_priced_attempts"`
	TheoreticalUnpricedAttempts int64   `json:"theoretical_unpriced_attempts"`
	TTFTP50                     *int64  `json:"ttft_p50_ms"`
	TTFTP95                     *int64  `json:"ttft_p95_ms"`
	DurationP95                 *int64  `json:"duration_p95_ms"`
	TTFTSamples                 int     `json:"ttft_samples"`
	DurationSamples             int     `json:"duration_samples"`
	timings                     *analyticsAccumulator
}

func analyticsInterval(span time.Duration) time.Duration {
	switch {
	case span <= time.Hour:
		return 5 * time.Minute
	case span <= 24*time.Hour:
		return time.Hour
	case span <= 7*24*time.Hour:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

type analyticsAccumulator struct {
	AnalyticsMetric
	ttft, duration []int64
	errors         map[string]int64
}

func newAnalyticsAccumulator(id, basis string) *analyticsAccumulator {
	return &analyticsAccumulator{AnalyticsMetric: AnalyticsMetric{ID: id, Basis: basis}, errors: map[string]int64{}}
}
func (a *analyticsAccumulator) observation(status, failure string, ttft, duration int64) {
	if status == "success" {
		if ttft > 0 {
			a.ttft = append(a.ttft, ttft)
		}
		if duration > 0 {
			a.duration = append(a.duration, duration)
		}
	} else {
		if failure == "" {
			failure = status
		}
		if failure == "" {
			failure = "unknown"
		}
		a.errors[failure]++
	}
}
func analyticsQuantile(values []int64, q float64) *int64 {
	if len(values) == 0 {
		return nil
	}
	v := values[int(math.Ceil(float64(len(values))*q))-1]
	return &v
}
func (a *analyticsAccumulator) finish() AnalyticsMetric {
	sort.Slice(a.ttft, func(i, j int) bool { return a.ttft[i] < a.ttft[j] })
	sort.Slice(a.duration, func(i, j int) bool { return a.duration[i] < a.duration[j] })
	a.TTFTSamples = len(a.ttft)
	a.DurationSamples = len(a.duration)
	a.TTFTP50 = analyticsQuantile(a.ttft, .5)
	a.TTFTP95 = analyticsQuantile(a.ttft, .95)
	a.DurationP50 = analyticsQuantile(a.duration, .5)
	a.DurationP95 = analyticsQuantile(a.duration, .95)
	a.Errors = make([]AnalyticsError, 0, len(a.errors))
	for cause, n := range a.errors {
		a.Errors = append(a.Errors, AnalyticsError{Cause: cause, Count: n})
	}
	sort.Slice(a.Errors, func(i, j int) bool {
		if a.Errors[i].Count == a.Errors[j].Count {
			return a.Errors[i].Cause < a.Errors[j].Cause
		}
		return a.Errors[i].Count > a.Errors[j].Count
	})
	return a.AnalyticsMetric
}
func analyticsGroups(groups map[string]*analyticsAccumulator) []AnalyticsMetric {
	out := make([]AnalyticsMetric, 0, len(groups))
	for _, v := range groups {
		out = append(out, v.finish())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost == out[j].Cost {
			if out[i].Attempts == out[j].Attempts {
				return out[i].ID < out[j].ID
			}
			return out[i].Attempts > out[j].Attempts
		}
		return out[i].Cost > out[j].Cost
	})
	return out
}

// QueryAnalytics uses one WAL snapshot for client outcomes and upstream costs.
// Provider filters apply to attempts, never just the final serving provider.
func (l *Ledger) QueryAnalytics(ctx context.Context, from, to time.Time, model, provider string) (*Analytics, error) {
	return l.QueryAnalyticsWithEstimates(ctx, from, to, model, provider, nil)
}

// QueryAnalyticsWithEstimates values each attempt independently of its plan
// kind, retaining per-attempt cache semantics and long-context price tiers.
func (l *Ledger) QueryAnalyticsWithEstimates(ctx context.Context, from, to time.Time, model, provider string, estimate AnalyticsEstimator) (*Analytics, error) {
	if to.UnixMilli() <= from.UnixMilli() {
		return nil, errors.New("analytics window must span at least one millisecond")
	}
	tx, err := l.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	basis := "requests"
	if provider != "" {
		basis = "attempts"
	}
	total := newAnalyticsAccumulator("", basis)
	models, providers := map[string]*analyticsAccumulator{}, map[string]*analyticsAccumulator{}
	group := func(groups map[string]*analyticsAccumulator, id, b string) *analyticsAccumulator {
		if id == "" {
			id = "__unknown__"
		}
		if groups[id] == nil {
			groups[id] = newAnalyticsAccumulator(id, b)
		}
		return groups[id]
	}
	interval := analyticsInterval(to.Sub(from))
	out := &Analytics{From: from, To: to, Basis: basis, GeneratedAt: time.Now().UTC(), BucketSeconds: int64(interval / time.Second)}
	start := from.Truncate(interval)
	for t := start; t.Before(to); t = t.Add(interval) {
		lo, hi := t, t.Add(interval)
		if lo.Before(from) {
			lo = from
		}
		if hi.After(to) {
			hi = to
		}
		out.Timeline = append(out.Timeline, AnalyticsBucket{From: lo, To: hi, Partial: hi.Sub(lo) != interval, timings: newAnalyticsAccumulator("", basis)})
	}
	bucket := func(ts int64) *AnalyticsBucket { return &out.Timeline[(ts-start.UnixMilli())/interval.Milliseconds()] }
	out.Coverage, err = reportingCoverage(ctx, tx, from, to)
	if err != nil {
		return nil, err
	}

	const modelID = "COALESCE(NULLIF(r.logical_model_id,''),NULLIF(r.model_requested,''),'__unknown__')"
	where := `r.ts>=? AND r.ts<? AND (?='' OR ` + modelID + `=?) AND (?='' OR EXISTS(SELECT 1 FROM attempts p WHERE p.request_id=r.id AND COALESCE(NULLIF(p.provider_id,''),'__unknown__')=?))`
	args := []any{from.UnixMilli(), to.UnixMilli(), model, model, provider, provider}
	// Historical rows may contain explicit NULLs in columns whose defaults
	// apply only to omitted values. Normalize these optional fields on read.
	rows, err := tx.QueryContext(ctx, `SELECT r.ts,`+modelID+`,COALESCE(r.status,''),COALESCE(r.outcome,''),COALESCE(r.failure_type,''),COALESCE(r.ttft_ms,0),COALESCE(r.duration_ms,0),
 NOT EXISTS(SELECT 1 FROM attempts a WHERE a.request_id=r.id),COALESCE(r.est_cash_cost,0),COALESCE(r.allocated_sub_cost,0)
 FROM logical_requests r WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ts, ttft, duration int64
		var mid, status, outcome, failure string
		var missing bool
		var cash, sub float64
		if err = rows.Scan(&ts, &mid, &status, &outcome, &failure, &ttft, &duration, &missing, &cash, &sub); err != nil {
			rows.Close()
			return nil, err
		}
		m := group(models, mid, basis)
		for _, a := range []*analyticsAccumulator{total, m} {
			a.Requests++
			if status == "success" {
				a.Succeeded++
				if outcome == "RECOVERED_SUCCESS" {
					a.Recovered++
				} else if outcome == "DIRECT_SUCCESS" {
					a.Direct++
				}
			} else if status == "canceled" {
				a.Canceled++
			} else {
				a.Failed++
			}
			if basis == "requests" {
				a.observation(status, failure, ttft, duration)
			}
			if missing {
				a.MissingAttempts++
				a.UnclassifiedReferenceCost += cash
				a.ReferenceCost += cash
				a.SubscriptionCost += sub
				a.Cost += sub
				if status != "success" {
					a.FailedRequestCost += sub
				}
			}
		}
		if missing {
			a := group(providers, "__unattributed__", "attempts")
			a.MissingAttempts++
			a.UnclassifiedReferenceCost += cash
			a.ReferenceCost += cash
			a.SubscriptionCost += sub
			a.Cost += sub
			if status != "success" {
				a.FailedRequestCost += sub
			}
			bucket(ts).Cost += sub
		}
		if basis == "requests" {
			b := bucket(ts)
			b.Requests++
			b.timings.observation(status, failure, ttft, duration)
			if status == "success" {
				b.Succeeded++
				if outcome == "RECOVERED_SUCCESS" {
					b.Recovered++
				} else if outcome == "DIRECT_SUCCESS" {
					b.Direct++
				}
			} else if status == "canceled" {
				b.Canceled++
			} else {
				b.Failed++
			}
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT r.ts,`+modelID+`,COALESCE(r.status,''),COALESCE(NULLIF(a.provider_id,''),'__unknown__'),a.seq,COALESCE(a.status,''),COALESCE(a.failure_type,''),COALESCE(a.ttft_ms,0),COALESCE(a.latency_ms,0),COALESCE(a.plan_kind,''),COALESCE(a.rate_card_version,''),COALESCE(a.est_cash_cost,0),COALESCE(a.allocated_sub_cost,0),COALESCE(a.input_tokens,0),COALESCE(a.cache_read_tokens,0),COALESCE(a.cache_write_tokens,0),COALESCE(a.output_tokens,0),COALESCE(a.protocol,''),COALESCE(a.model,''),a.est_cash_cost IS NOT NULL
 FROM attempts a JOIN logical_requests r ON r.id=a.request_id WHERE `+where+` AND (?='' OR COALESCE(NULLIF(a.provider_id,''),'__unknown__')=?)`, append(args, provider, provider)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ts, seq, ttft, duration, input, read, write, output int64
		var mid, requestStatus, pid, status, failure, plan, version, protocol, upstreamModel string
		var reference, sub float64
		var hasReference bool
		if err = rows.Scan(&ts, &mid, &requestStatus, &pid, &seq, &status, &failure, &ttft, &duration, &plan, &version, &reference, &sub, &input, &read, &write, &output, &protocol, &upstreamModel, &hasReference); err != nil {
			rows.Close()
			return nil, err
		}
		cash := 0.0
		if plan == "payg" {
			cash = reference
		}
		cost := cash + sub
		priced := plan == "free" || (plan == "payg" && hasReference && (version != "" || cash > 0)) || (plan == "subscription" && sub > 0)
		uncached := input
		if protocol != "anthropic" && input >= read+write {
			uncached = input - read - write
		}
		var theoretical *float64
		if estimate != nil {
			theoretical = estimate(pid, upstreamModel, uncached+read+write, output, read, write)
			if theoretical != nil && (*theoretical < 0 || math.IsNaN(*theoretical) || math.IsInf(*theoretical, 0)) {
				theoretical = nil
			}
		}
		for _, a := range []*analyticsAccumulator{total, group(models, mid, basis), group(providers, pid, "attempts")} {
			a.Attempts++
			if status == "success" {
				a.AttemptSucceeded++
			} else if status == "canceled" {
				a.AttemptCanceled++
			} else {
				a.AttemptFailed++
			}
			if priced {
				a.PricedAttempts++
			} else {
				a.UnpricedAttempts++
			}
			a.CashCost += cash
			a.SubscriptionCost += sub
			a.Cost += cost
			a.ReferenceCost += reference
			if theoretical != nil {
				a.TheoreticalCost += *theoretical
				a.TheoreticalPricedAttempts++
			} else {
				a.TheoreticalUnpricedAttempts++
			}
			if seq > 1 {
				a.RetryCost += cost
			}
			if status != "success" {
				a.FailedAttemptCost += cost
			}
			if requestStatus != "success" {
				a.FailedRequestCost += cost
			}
			a.UncachedInput += uncached
			a.CacheRead += read
			a.CacheWrite += write
			a.Output += output
			if a.Basis == "attempts" {
				a.observation(status, failure, ttft, duration)
			}
		}
		b := bucket(ts)
		b.Cost += cost
		if theoretical != nil {
			b.TheoreticalCost += *theoretical
			b.TheoreticalPricedAttempts++
		} else {
			b.TheoreticalUnpricedAttempts++
		}
		b.Attempts++
		if priced {
			b.PricedAttempts++
		} else {
			b.UnpricedAttempts++
		}
		if basis == "attempts" {
			b.Requests++
			b.timings.observation(status, failure, ttft, duration)
			if status == "success" {
				b.Succeeded++
			} else if status == "canceled" {
				b.Canceled++
			} else {
				b.Failed++
			}
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	for i := range out.Timeline {
		b := &out.Timeline[i]
		m := b.timings.finish()
		b.TTFTP50 = m.TTFTP50
		b.TTFTP95 = m.TTFTP95
		b.DurationP95 = m.DurationP95
		b.TTFTSamples = m.TTFTSamples
		b.DurationSamples = m.DurationSamples
		b.timings = nil
	}
	out.Summary = total.finish()
	out.Models = analyticsGroups(models)
	out.Providers = analyticsGroups(providers)
	return out, nil
}
