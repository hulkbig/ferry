package ledger

import (
	"context"
	"database/sql"
	"time"
)

// Shared query implementations accept a transaction for a consistent report.
type reportReader interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

type BillingReport struct {
	From              time.Time                `json:"from"`
	To                time.Time                `json:"to"`
	Basis             string                   `json:"basis"`
	Coverage          ReportingCoverage        `json:"coverage"`
	Summary           *UsageSummary            `json:"summary"`
	HistoricalSummary *UsageSummary            `json:"historical_summary"`
	Outcomes          BillingOutcomes          `json:"outcomes"`
	Providers         map[string]*UsageSummary `json:"by_provider"`
	Models            map[string]*UsageSummary `json:"by_model"`
	Trend             []UsageTrendPoint        `json:"trend"`
}
type BillingOutcomes struct {
	DirectSuccess     int64 `json:"direct_success"`
	RecoveredSuccess  int64 `json:"recovered_success"`
	HistoricalSuccess int64 `json:"historical_success"`
	Failed            int64 `json:"failed"`
	Canceled          int64 `json:"canceled"`
}

// QueryBilling keeps the legacy final-request monetary dimensions, with a
// detail-only cohort. Historical rollups are a separate, explicitly named view.
// All components share one WAL snapshot, including coverage.
func (l *Ledger) QueryBilling(ctx context.Context, from, to time.Time) (*BillingReport, error) {
	tx, err := l.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &BillingReport{From: from, To: to, Basis: "retained_request_details"}
	if out.Summary, err = summarizeReport(tx, from, to, true); err != nil {
		return nil, err
	}
	if out.HistoricalSummary, err = summarizeReport(tx, from, to, false); err != nil {
		return nil, err
	}
	if out.Coverage, err = reportingCoverage(ctx, tx, from, to); err != nil {
		return nil, err
	}
	if out.Providers, err = reportGroups(tx, "provider_id", from.UnixMilli(), to.UnixMilli()); err != nil {
		return nil, err
	}
	if out.Models, err = reportGroups(tx, "logical_model_id", from.UnixMilli(), to.UnixMilli()); err != nil {
		return nil, err
	}
	if out.Trend, err = reportTrend(tx, from, to, 30); err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `SELECT
 COALESCE(SUM(status='success' AND UPPER(outcome)='DIRECT_SUCCESS'),0),
 COALESCE(SUM(status='success' AND UPPER(outcome)='RECOVERED_SUCCESS'),0),
 COALESCE(SUM(status='success' AND UPPER(outcome) NOT IN ('DIRECT_SUCCESS','RECOVERED_SUCCESS')),0),
 COALESCE(SUM(status NOT IN ('success','canceled')),0),COALESCE(SUM(status='canceled'),0)
 FROM logical_requests WHERE ts>=? AND ts<?`, from.UnixMilli(), to.UnixMilli()).Scan(&out.Outcomes.DirectSuccess, &out.Outcomes.RecoveredSuccess, &out.Outcomes.HistoricalSuccess, &out.Outcomes.Failed, &out.Outcomes.Canceled)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
