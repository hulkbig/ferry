package ledger

import (
	"context"
	"database/sql"
	"time"
)

// Coverage compares whole hours only. It describes the global ledger even when
// analytics filters a model/provider: legacy rollups lack those dimensions.
// EarliestDetail is an observation, not a guaranteed retention cutoff.
type ReportingCoverage struct {
	Basis              string     `json:"basis"`
	Scope              string     `json:"scope"`
	EarliestDetail     *time.Time `json:"earliest_detail"`
	EarliestRollup     *time.Time `json:"earliest_rollup"`
	ComparedFrom       time.Time  `json:"compared_from"`
	ComparedTo         time.Time  `json:"compared_to"`
	DetailRequests     int64      `json:"detail_requests"`
	RollupRequests     int64      `json:"rollup_requests"`
	RollupOnlyRequests int64      `json:"rollup_only_requests"`
	MismatchedHours    int64      `json:"mismatched_hours"`
}

func (l *Ledger) ReportingCoverage(ctx context.Context, from, to time.Time) (ReportingCoverage, error) {
	tx, err := l.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReportingCoverage{}, err
	}
	defer tx.Rollback()
	return reportingCoverage(ctx, tx, from, to)
}
func reportingCoverage(ctx context.Context, tx *sql.Tx, from, to time.Time) (ReportingCoverage, error) {
	start := from.Truncate(time.Hour)
	if start.Before(from) {
		start = start.Add(time.Hour)
	}
	end := to.Truncate(time.Hour)
	if end.Before(start) {
		start = to
		end = to
	}
	c := ReportingCoverage{Basis: "retained_request_details", Scope: "global", ComparedFrom: start, ComparedTo: end}
	var detail, rollup sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT MIN(ts) FROM logical_requests),(SELECT MIN(bucket)*3600000 FROM usage_hourly)").Scan(&detail, &rollup); err != nil {
		return c, err
	}
	if detail.Valid {
		v := time.UnixMilli(detail.Int64)
		c.EarliestDetail = &v
	}
	if rollup.Valid {
		v := time.UnixMilli(rollup.Int64)
		c.EarliestRollup = &v
	}
	err := tx.QueryRowContext(ctx, `WITH d AS (SELECT ts/3600000 h,COUNT(*) n FROM logical_requests WHERE ts>=? AND ts<? GROUP BY h),
 r AS (SELECT bucket h,SUM(requests) n FROM usage_hourly WHERE bucket>=? AND bucket<? GROUP BY h),
 hours AS (SELECT h FROM d UNION SELECT h FROM r)
 SELECT COALESCE(SUM(d.n),0),COALESCE(SUM(r.n),0),
 COALESCE(SUM(CASE WHEN d.h IS NULL THEN r.n ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN d.h IS NOT NULL AND d.n!=COALESCE(r.n,0) THEN 1 ELSE 0 END),0)
 FROM hours LEFT JOIN d USING(h) LEFT JOIN r USING(h)`,
		start.UnixMilli(), end.UnixMilli(), start.UnixMilli()/3600000, end.UnixMilli()/3600000).Scan(&c.DetailRequests, &c.RollupRequests, &c.RollupOnlyRequests, &c.MismatchedHours)
	return c, err
}
