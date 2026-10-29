package ledger

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ferry/internal/domain"
)

// QueryRequests returns logical requests matching the filter, newest first.
// Queries run on the read pool; under WAL they never contend with the
// single writer connection.
func (l *Ledger) QueryRequests(f domain.LogFilter) ([]domain.RequestRecord, int, error) {
	tx, err := l.read.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	where, args := buildWhere(f)
	var total int
	if err := tx.QueryRow(liveRequestsCTE+"SELECT COUNT(*) FROM logical_requests "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count: %w", err)
	}
	q := liveRequestsCTE + "SELECT " + requestCols + " FROM logical_requests " + where + // #nosec G202 -- identifiers are constant and filter values stay bound.
		" ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limitOf(f.Limit), f.Offset)
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []domain.RequestRecord
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

const requestCols = `id, ts, consumer_key, api_key_secret_id, alias, objective, policy_name,
	policy_version, policy_hash, policy_index_hash, policy_compile_cache_hit,
	policy_scene_cache_hit, fallback_count, connection_rebind_count, route_fallback_count,
	route_advance_count, recovery_fallback_count, conversation_migration_count,
	model_requested, model_selected, protocol, stream,
	logical_model_id, provider_id, connection_id, upstream_model, runtime_revision, route_fingerprint, commit_at, continuity_mode,
	status, outcome,
	failure_type, failure_msg, attempts, input_tokens, output_tokens,
	cache_read_tokens, cache_write_tokens, reasoning_tokens,
	upstream_input_tokens, upstream_output_tokens, upstream_cache_read_tokens,
	upstream_cache_write_tokens, upstream_reasoning_tokens,
	est_cash_cost, quota_units, allocated_sub_cost, shadow_cost, serving_cost,
	recovery_overhead_cost, recovery_added_latency_ms, selected_target, duration_ms,
	ttft_ms, compat_level, compat_notes, exclusions, client_ip,
 COALESCE((SELECT NULLIF(a.protocol,'') FROM attempts a
 WHERE a.request_id=logical_requests.id AND a.seq=logical_requests.attempts),logical_requests.protocol),
 service_tier, response_model, response_service_tier,
 COALESCE(cache_subset_of_input, COALESCE((SELECT NULLIF(a.protocol,'') FROM attempts a
 WHERE a.request_id=logical_requests.id AND a.seq=logical_requests.attempts),logical_requests.protocol) != 'anthropic' AND input_tokens>=cache_read_tokens+cache_write_tokens),
 COALESCE(upstream_cache_subset_of_input, COALESCE((SELECT NULLIF(a.protocol,'') FROM attempts a
 WHERE a.request_id=logical_requests.id AND a.seq=logical_requests.attempts),logical_requests.protocol) != 'anthropic' AND upstream_input_tokens>=upstream_cache_read_tokens+upstream_cache_write_tokens), service_tier_evidence`

func limitOf(n int) int {
	if n <= 0 {
		return 100
	}
	if n > 1000 {
		return 1000
	}
	return n
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRequest(rs rowScanner) (*domain.RequestRecord, error) {
	var r domain.RequestRecord
	var tsMilli, stream, compileCacheHit, sceneCacheHit, commitAt int64
	var notesJSON, excJSON, evidenceJSON string
	if err := rs.Scan(&r.ID, &tsMilli, &r.ConsumerKey, &r.APIKeySecretID, &r.Alias, &r.Objective,
		&r.PolicyName, &r.PolicyVersion, &r.PolicyHash, &r.PolicyIndexHash,
		&compileCacheHit, &sceneCacheHit, &r.FallbackCount, &r.ConnectionRebindCount,
		&r.RouteFallbackCount, &r.RouteAdvanceCount, &r.RecoveryFallbackCount,
		&r.ConversationMigrationCount, &r.ModelRequested, &r.ModelSelected,
		&r.Protocol, &stream, &r.LogicalModelID, &r.ProviderID, &r.ConnectionID, &r.UpstreamModel,
		&r.RuntimeRevision, &r.RouteFingerprint, &commitAt, &r.ContinuityMode,
		&r.Status, &r.Outcome, &r.FailureType, &r.FailureMsg,
		&r.Attempts, &r.Usage.InputTokens, &r.Usage.OutputTokens,
		&r.Usage.CacheReadTokens, &r.Usage.CacheWriteTokens, &r.Usage.ReasoningTokens,
		&r.UpstreamUsage.InputTokens, &r.UpstreamUsage.OutputTokens,
		&r.UpstreamUsage.CacheReadTokens, &r.UpstreamUsage.CacheWriteTokens,
		&r.UpstreamUsage.ReasoningTokens,
		&r.EstCashCost, &r.QuotaUnits, &r.AllocatedSubCost, &r.ShadowCost,
		&r.ServingCost, &r.RecoveryOverheadCost, &r.RecoveryAddedLatencyMS,
		&r.SelectedTarget, &r.DurationMS, &r.TTFTMS, &r.CompatLevel,
		&notesJSON, &excJSON, &r.ClientIP, &r.UsageProtocol,
		&r.ServiceTier, &r.ResponseModel, &r.ResponseServiceTier, &r.Usage.CacheSubsetOfInput, &r.UpstreamUsage.CacheSubsetOfInput, &evidenceJSON); err != nil {
		return nil, fmt.Errorf("scan request: %w", err)
	}
	r.Ts = time.UnixMilli(tsMilli)
	if r.Status == "received" || r.Status == "waiting" || r.Status == "active" || r.Status == "recovering" || r.Status == "streaming" || r.Status == "delivering" {
		r.DurationMS = max(0, time.Since(r.Ts).Milliseconds())
	}
	r.Stream = stream != 0
	r.PolicyCompileCacheHit = compileCacheHit != 0
	r.PolicySceneCacheHit = sceneCacheHit != 0
	if commitAt > 0 {
		r.CommitAt = time.UnixMilli(commitAt)
	}
	_ = json.Unmarshal([]byte(notesJSON), &r.CompatNotes)
	_ = json.Unmarshal([]byte(excJSON), &r.Exclusions)
	_ = json.Unmarshal([]byte(evidenceJSON), &r.ServiceTierEvidence)
	return &r, nil
}

// QueryRequest returns one logical request by id, or (nil, nil) when absent.
func (l *Ledger) QueryRequest(id string) (*domain.RequestRecord, error) {
	row := l.read.QueryRow(liveRequestsCTE+"SELECT "+requestCols+" FROM logical_requests WHERE id = ?", id)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query request: %w", err)
	}
	return r, nil
}

// QueryRequestDebug returns opt-in payload capture for one request.
func (l *Ledger) QueryRequestDebug(id string) (*domain.RequestDebugRecord, error) {
	var record domain.RequestDebugRecord
	var requestTruncated, responseTruncated int
	err := l.read.QueryRow(`SELECT request_id, request_json, response_json, request_truncated, response_truncated
		FROM request_debug WHERE request_id = ?`, id).Scan(&record.RequestID, &record.RequestJSON,
		&record.ResponseJSON, &requestTruncated, &responseTruncated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query request debug: %w", err)
	}
	record.RequestTruncated = requestTruncated != 0
	record.ResponseTruncated = responseTruncated != 0
	return &record, nil
}

// QueryAttempts returns all attempts for one logical request in seq order.
func (l *Ledger) QueryAttempts(requestID string) ([]domain.AttemptRecord, error) {
	rows, err := l.read.Query(`SELECT request_id, seq, ts, target_id, provider_id, connection_id, endpoint_url,
		protocol, credential_ref, model, plan_kind, status, failure_type, failure_scope,
		failure_status, failure_msg, latency_ms, ttft_ms, input_tokens,
		output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
		multiplier, est_cash_cost, quota_units, allocated_sub_cost, shadow_cost,
		rate_card_version, quota_rule_version, multiplier_rule_version,
		service_tier, response_model, response_service_tier,
		COALESCE(cache_subset_of_input, COALESCE(NULLIF(protocol,''),
		(SELECT lr.protocol FROM logical_requests lr WHERE lr.id=attempts.request_id),'') != 'anthropic'
		AND input_tokens>=cache_read_tokens+cache_write_tokens), service_tier_evidence
		FROM attempts WHERE request_id = ? ORDER BY seq ASC`, requestID)
	if err != nil {
		return nil, fmt.Errorf("query attempts: %w", err)
	}
	defer rows.Close()
	var out []domain.AttemptRecord
	for rows.Next() {
		var a domain.AttemptRecord
		var tsMilli int64
		var failType string
		var failScope string
		var failStatus int64
		var failMsg string
		var evidenceJSON string
		if err := rows.Scan(&a.RequestID, &a.Seq, &tsMilli, &a.TargetID,
			&a.ProviderID, &a.ConnectionID, &a.EndpointURL, &a.Protocol, &a.CredentialRef, &a.Model, &a.PlanKind,
			&a.Status, &failType, &failScope, &failStatus, &failMsg, &a.LatencyMS, &a.TTFTMS,
			&a.Usage.InputTokens, &a.Usage.OutputTokens, &a.Usage.CacheReadTokens,
			&a.Usage.CacheWriteTokens, &a.Usage.ReasoningTokens, &a.Multiplier,
			&a.EstCashCost, &a.QuotaUnits, &a.AllocatedSubCost, &a.ShadowCost,
			&a.RateCardVersion, &a.QuotaRuleVersion, &a.MultiplierRuleVersion,
			&a.ServiceTier, &a.ResponseModel, &a.ResponseServiceTier, &a.Usage.CacheSubsetOfInput, &evidenceJSON); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		a.Ts = time.UnixMilli(tsMilli)
		_ = json.Unmarshal([]byte(evidenceJSON), &a.ServiceTierEvidence)
		if failType != "" || failMsg != "" {
			a.Failure = &domain.Failure{
				Type:       domain.FailureType(failType),
				Scope:      domain.FailureScope(failScope),
				HTTPStatus: int(failStatus),
				Message:    failMsg,
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// QueryTrace returns an independent snapshot of the latest pending trace, or
// the persisted decision trace after completion, if recorded.
func (l *Ledger) QueryTrace(requestID string) (*domain.TraceRecord, error) {
	l.mu.Lock()
	var pending *domain.TraceRecord
	if p := l.requests[requestID]; p != nil {
		pending = p.trace
	}
	l.mu.Unlock()
	if pending != nil {
		// Published snapshots are immutable, so copying can happen outside the
		// request lock without blocking lifecycle updates for other requests.
		return cloneTraceRecord(pending), nil
	}

	var raw string
	err := l.read.QueryRow("SELECT trace FROM decision_traces WHERE request_id = ?", requestID).
		Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query trace: %w", err)
	}
	tr := &domain.TraceRecord{}
	if err := json.Unmarshal([]byte(raw), tr); err != nil {
		return nil, fmt.Errorf("decode trace: %w", err)
	}
	return tr, nil
}

// ---------------------------------------------------------------------------
// dashboard aggregates

// UsageSummary aggregates cost/token totals over a window.
type UsageSummary struct {
	Requests             int64    `json:"requests"`
	Succeeded            int64    `json:"succeeded"`
	Failed               int64    `json:"failed"`
	Canceled             int64    `json:"canceled"`
	InputTokens          int64    `json:"input_tokens"`
	OutputTokens         int64    `json:"output_tokens"`
	CacheRead            int64    `json:"cache_read_tokens"`
	TokensObserved       bool     `json:"tokens_observed"`
	EstCashCost          float64  `json:"est_cash_cost"`
	QuotaUnits           float64  `json:"quota_units"`
	AllocatedSub         float64  `json:"allocated_subscription_cost"`
	ShadowCost           float64  `json:"shadow_cost"`
	AvgLatencyMS         float64  `json:"avg_latency_ms"`
	MedianLatencyMS      *int64   `json:"median_latency_ms"`
	P95LatencyMS         int64    `json:"p95_latency_ms"`
	AvgRecoveryLatencyMS *float64 `json:"avg_recovery_latency_ms"`
	UpstreamInputTokens  int64    `json:"upstream_input_tokens"`
	UpstreamOutputTokens int64    `json:"upstream_output_tokens"`
	ServingCost          float64  `json:"serving_cost"`
	RecoveryOverheadCost float64  `json:"recovery_overhead_cost"`
	TotalCost            float64  `json:"total_cost"`
	CostObserved         bool     `json:"cost_observed"`
}

// OutcomeSummary keeps recovered success as a first-class product outcome.
type OutcomeSummary struct {
	DirectSuccess    int64 `json:"direct_success"`
	RecoveredSuccess int64 `json:"recovered_success"`
	Failed           int64 `json:"failed"`
}

// TrafficSummary is a stable V2 traffic slice keyed by Logical Model or
// Provider rather than the private execution target leaf.
type TrafficSummary struct {
	ID              string     `json:"id"`
	Requests        int64      `json:"requests"`
	DirectTraffic   int64      `json:"direct_traffic"`
	RecoveryTraffic int64      `json:"recovery_traffic"`
	Failed          int64      `json:"failed"`
	AvgTTFTMS       *float64   `json:"avg_ttft_ms,omitempty"`
	AvgDurationMS   *float64   `json:"avg_duration_ms,omitempty"`
	LastSuccessAt   *time.Time `json:"last_success_at,omitempty"`
}

type RouteTrafficSummary struct {
	LogicalModelID  string `json:"logical_model_id"`
	ProviderID      string `json:"provider_id"`
	UpstreamModel   string `json:"upstream_model"`
	Requests        int64  `json:"requests"`
	DirectTraffic   int64  `json:"direct_traffic"`
	RecoveryTraffic int64  `json:"recovery_traffic"`
}

type UsageTrendPoint struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Requests     int64     `json:"requests"`
	Succeeded    int64     `json:"succeeded"`
	Failed       int64     `json:"failed"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalCost    float64   `json:"total_cost"`
	AvgLatencyMS float64   `json:"avg_latency_ms"`
}

// TrafficTrendPoint is one gap-filled time bucket for a concrete final
// logical-model/provider/model route. Succeeded plus Failed equals Requests;
// canceled requests are intentionally included in Failed for success-rate
// denominators.
type TrafficTrendPoint struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Requests  int64     `json:"requests"`
	Succeeded int64     `json:"succeeded"`
	Failed    int64     `json:"failed"`
}

// RouteTrafficTrend keeps the finest useful product dimension in one ledger
// result. Consumers can aggregate the same rows into logical-model, provider,
// or provider/upstream-model series without issuing three nearly identical
// database queries.
type RouteTrafficTrend struct {
	LogicalModelID string              `json:"logical_model_id"`
	ProviderID     string              `json:"provider_id"`
	UpstreamModel  string              `json:"upstream_model"`
	Requests       int64               `json:"requests"`
	Succeeded      int64               `json:"succeeded"`
	Failed         int64               `json:"failed"`
	Points         []TrafficTrendPoint `json:"points"`
}

// CacheHitStats is a token-weighted prompt-cache summary. CacheEligibleInput
// is normalized using the protocol of the final upstream attempt so that
// OpenAI-style usage (cached tokens included in input_tokens) and Anthropic
// usage (cache read/write tokens reported beside input_tokens) share one
// denominator without producing rates above 100%.
type CacheHitStats struct {
	Requests                 int64    `json:"requests"`
	ObservedRequests         int64    `json:"observed_requests"`
	CacheEligibleInputTokens int64    `json:"cache_eligible_input_tokens"`
	CacheReadTokens          int64    `json:"cache_read_tokens"`
	CacheWriteTokens         int64    `json:"cache_write_tokens"`
	HitRatePct               *float64 `json:"hit_rate_pct,omitempty"`
}

type CacheHitDimension struct {
	ID string `json:"id"`
	CacheHitStats
}

type CacheHitOverview struct {
	Overall       CacheHitStats       `json:"overall"`
	APIKeys       []CacheHitDimension `json:"api_keys"`
	LogicalModels []CacheHitDimension `json:"logical_models"`
}

func (l *Ledger) UsageTrend(from, to time.Time, buckets int) ([]UsageTrendPoint, error) {
	return reportTrend(l.read, from, to, buckets)
}
func reportTrend(reader reportReader, from, to time.Time, buckets int) ([]UsageTrendPoint, error) {
	if !to.After(from) {
		return []UsageTrendPoint{}, nil
	}
	if buckets < 2 {
		buckets = 2
	}
	if buckets > 96 {
		buckets = 96
	}
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()
	windowMS := toMS - fromMS
	if windowMS <= 0 {
		return []UsageTrendPoint{}, nil
	}
	rows, err := reader.Query(`SELECT CAST(((ts-?)*?)/? AS INTEGER) AS bucket,COUNT(*),
		SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status!='success' THEN 1 ELSE 0 END),
		SUM(input_tokens),SUM(output_tokens),SUM(`+requestTotalCostSQL+`),
		COALESCE(AVG(NULLIF(duration_ms,0)),0)
		FROM logical_requests WHERE ts>=? AND ts<? GROUP BY bucket ORDER BY bucket`,
		fromMS, buckets, windowMS, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[int]UsageTrendPoint{}
	for rows.Next() {
		var index int
		var point UsageTrendPoint
		if err := rows.Scan(&index, &point.Requests, &point.Succeeded, &point.Failed,
			&point.InputTokens, &point.OutputTokens, &point.TotalCost, &point.AvgLatencyMS); err != nil {
			return nil, err
		}
		if index < 0 || index >= buckets {
			continue
		}
		values[index] = point
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]UsageTrendPoint, 0, buckets)
	span := to.Sub(from)
	baseWidth, remainder := span/time.Duration(buckets), span%time.Duration(buckets)
	boundary := func(index int) time.Time {
		position := time.Duration(index)
		return from.Add(baseWidth*position + remainder*position/time.Duration(buckets))
	}
	for index := 0; index < buckets; index++ {
		point := values[index]
		point.From = boundary(index)
		point.To = boundary(index + 1)
		out = append(out, point)
	}
	return out, nil
}

// TrafficTrend returns route-level, gap-filled time series for [from,to).
// ProviderID and UpstreamModel remain empty when a request failed before an
// upstream was selected; those rows still contribute to the logical-model
// series but are not presented as provider traffic.
func (l *Ledger) TrafficTrend(from, to time.Time, buckets int) ([]RouteTrafficTrend, error) {
	if !to.After(from) {
		return []RouteTrafficTrend{}, nil
	}
	if buckets < 2 {
		buckets = 2
	}
	if buckets > 96 {
		buckets = 96
	}
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()
	windowMS := toMS - fromMS
	if windowMS <= 0 {
		return []RouteTrafficTrend{}, nil
	}
	rows, err := l.read.Query(`SELECT CAST(((ts-?)*?)/? AS INTEGER) AS bucket,
		COALESCE(NULLIF(logical_model_id,''),NULLIF(alias,''),NULLIF(model_requested,''),model_selected),
		provider_id,COALESCE(NULLIF(upstream_model,''),model_selected),COUNT(*),
		SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status!='success' THEN 1 ELSE 0 END)
		FROM logical_requests WHERE ts>=? AND ts<? AND
			COALESCE(NULLIF(logical_model_id,''),NULLIF(alias,''),NULLIF(model_requested,''),model_selected)!=''
		GROUP BY bucket,
			COALESCE(NULLIF(logical_model_id,''),NULLIF(alias,''),NULLIF(model_requested,''),model_selected),
			provider_id,COALESCE(NULLIF(upstream_model,''),model_selected)
		ORDER BY 2,3,4,bucket`,
		fromMS, buckets, windowMS, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type routeKey struct {
		logicalModelID string
		providerID     string
		upstreamModel  string
	}
	series := map[routeKey]*RouteTrafficTrend{}
	span := to.Sub(from)
	baseWidth, remainder := span/time.Duration(buckets), span%time.Duration(buckets)
	boundary := func(index int) time.Time {
		position := time.Duration(index)
		return from.Add(baseWidth*position + remainder*position/time.Duration(buckets))
	}
	for rows.Next() {
		var index int
		var key routeKey
		var requests, succeeded, failed int64
		if err := rows.Scan(&index, &key.logicalModelID, &key.providerID, &key.upstreamModel,
			&requests, &succeeded, &failed); err != nil {
			return nil, err
		}
		if index < 0 || index >= buckets {
			continue
		}
		item := series[key]
		if item == nil {
			item = &RouteTrafficTrend{
				LogicalModelID: key.logicalModelID, ProviderID: key.providerID, UpstreamModel: key.upstreamModel,
				Points: make([]TrafficTrendPoint, buckets),
			}
			for bucket := range item.Points {
				item.Points[bucket].From = boundary(bucket)
				item.Points[bucket].To = boundary(bucket + 1)
			}
			series[key] = item
		}
		item.Points[index].Requests = requests
		item.Points[index].Succeeded = succeeded
		item.Points[index].Failed = failed
		item.Requests += requests
		item.Succeeded += succeeded
		item.Failed += failed
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]RouteTrafficTrend, 0, len(series))
	for _, item := range series {
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].LogicalModelID != out[j].LogicalModelID {
			return out[i].LogicalModelID < out[j].LogicalModelID
		}
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].UpstreamModel < out[j].UpstreamModel
	})
	return out, nil
}

func addCacheHitStats(total *CacheHitStats, value CacheHitStats) {
	total.Requests += value.Requests
	total.ObservedRequests += value.ObservedRequests
	total.CacheEligibleInputTokens += value.CacheEligibleInputTokens
	total.CacheReadTokens += value.CacheReadTokens
	total.CacheWriteTokens += value.CacheWriteTokens
}

func finishCacheHitStats(value CacheHitStats) CacheHitStats {
	if value.CacheEligibleInputTokens > 0 {
		rate := float64(value.CacheReadTokens) / float64(value.CacheEligibleInputTokens) * 100
		if rate < 0 {
			rate = 0
		}
		if rate > 100 {
			rate = 100
		}
		value.HitRatePct = &rate
	}
	return value
}

// CacheHitOverview calculates overall, API-key, and logical-model cache hit
// rates in one detail-table scan. Request usage is paired with the protocol of
// its final upstream attempt because the incoming client protocol may differ
// after Ferry translation.
func (l *Ledger) CacheHitOverview(from, to time.Time) (*CacheHitOverview, error) {
	result := &CacheHitOverview{APIKeys: []CacheHitDimension{}, LogicalModels: []CacheHitDimension{}}
	if !to.After(from) {
		return result, nil
	}
	rows, err := l.read.Query(`WITH cache_rows AS (
		SELECT lr.consumer_key AS consumer_key,
			COALESCE(NULLIF(lr.logical_model_id,''),NULLIF(lr.alias,''),NULLIF(lr.model_requested,''),lr.model_selected) AS logical_model,
			COALESCE(NULLIF(a.protocol,''),lr.protocol) AS final_protocol,
			lr.cache_subset_of_input AS cache_subset_of_input,
			CASE WHEN COALESCE(lr.input_tokens,0)>0 THEN lr.input_tokens ELSE 0 END AS input_tokens,
			CASE WHEN COALESCE(lr.cache_read_tokens,0)>0 THEN lr.cache_read_tokens ELSE 0 END AS cache_read_tokens,
			CASE WHEN COALESCE(lr.cache_write_tokens,0)>0 THEN lr.cache_write_tokens ELSE 0 END AS cache_write_tokens
		FROM logical_requests lr
		LEFT JOIN attempts a ON a.request_id=lr.id AND a.seq=lr.attempts
		WHERE lr.ts>=? AND lr.ts<?
	)
	SELECT consumer_key,logical_model,COUNT(*),
		COALESCE(SUM(CASE WHEN input_tokens>0 OR cache_read_tokens>0 OR cache_write_tokens>0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE
			WHEN cache_subset_of_input=1 THEN MAX(input_tokens,cache_read_tokens+cache_write_tokens)
			WHEN cache_subset_of_input=0 THEN input_tokens+cache_read_tokens+cache_write_tokens
			WHEN final_protocol=? THEN input_tokens+cache_read_tokens+cache_write_tokens
			WHEN input_tokens>=cache_read_tokens+cache_write_tokens THEN input_tokens
			ELSE input_tokens+cache_read_tokens+cache_write_tokens
		END),0),
		COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_write_tokens),0)
	FROM cache_rows
	GROUP BY consumer_key,logical_model`,
		from.UnixMilli(), to.UnixMilli(), string(domain.ProtocolAnthropic))
	if err != nil {
		return nil, fmt.Errorf("cache hit overview: %w", err)
	}
	defer rows.Close()
	byKey := map[string]CacheHitStats{}
	byModel := map[string]CacheHitStats{}
	for rows.Next() {
		var key, model string
		var value CacheHitStats
		if err := rows.Scan(&key, &model, &value.Requests, &value.ObservedRequests,
			&value.CacheEligibleInputTokens, &value.CacheReadTokens, &value.CacheWriteTokens); err != nil {
			return nil, fmt.Errorf("scan cache hit overview: %w", err)
		}
		addCacheHitStats(&result.Overall, value)
		if key != "" {
			total := byKey[key]
			addCacheHitStats(&total, value)
			byKey[key] = total
		}
		if model != "" {
			total := byModel[model]
			addCacheHitStats(&total, value)
			byModel[model] = total
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cache hit overview: %w", err)
	}
	result.Overall = finishCacheHitStats(result.Overall)
	for id, value := range byKey {
		result.APIKeys = append(result.APIKeys, CacheHitDimension{ID: id, CacheHitStats: finishCacheHitStats(value)})
	}
	for id, value := range byModel {
		result.LogicalModels = append(result.LogicalModels, CacheHitDimension{ID: id, CacheHitStats: finishCacheHitStats(value)})
	}
	sortCacheHitDimensions(result.APIKeys)
	sortCacheHitDimensions(result.LogicalModels)
	return result, nil
}

func sortCacheHitDimensions(items []CacheHitDimension) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].CacheEligibleInputTokens != items[j].CacheEligibleInputTokens {
			return items[i].CacheEligibleInputTokens > items[j].CacheEligibleInputTokens
		}
		return items[i].ID < items[j].ID
	})
}

// Summarize aggregates the window [from,to) across logical requests. Full
// hour buckets come from the hourly rollup (O(hours×targets)); the ragged
// head and tail come from detail-table passes. P95 stays a detail query —
// it needs row-level order.
func (l *Ledger) Summarize(from, to time.Time) (*UsageSummary, error) {
	return l.summarize(from, to, false)
}

// SummarizeDetails uses the queryable request cohort, matching dimensions and trends.
// Historical hourly records remain available through Summarize and coverage.
func (l *Ledger) SummarizeDetails(from, to time.Time) (*UsageSummary, error) {
	return l.summarize(from, to, true)
}
func (l *Ledger) summarize(from, to time.Time, detailOnly bool) (*UsageSummary, error) {
	return summarizeReport(l.read, from, to, detailOnly)
}
func summarizeReport(reader reportReader, from, to time.Time, detailOnly bool) (*UsageSummary, error) {
	s := &UsageSummary{}
	var latSum, latCnt int64
	// detail pass: ragged edges the rollup cannot cover
	edgeFrom := to.Truncate(time.Hour) // [edgeFrom, to) is the ragged tail
	edgeTo := from.Truncate(time.Hour)
	if edgeTo.Before(from) {
		edgeTo = edgeTo.Add(time.Hour)
	}
	if edgeTo.After(to) {
		edgeTo = to
	}
	sumDetail := func(fromMs, toMs int64) error {
		row := reader.QueryRow(`SELECT
			COUNT(*),
			COALESCE(SUM(status='success'),0),
			COALESCE(SUM(status='failed'),0),
			COALESCE(SUM(status='canceled'),0),
			COALESCE(SUM(input_tokens),0),
			COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(est_cash_cost),0),
			COALESCE(SUM(quota_units),0),
			COALESCE(SUM(allocated_sub_cost),0),
			COALESCE(SUM(shadow_cost),0),
			COALESCE(SUM(CASE WHEN duration_ms > 0 THEN duration_ms ELSE 0 END),0),
			COUNT(NULLIF(duration_ms,0))
			FROM logical_requests WHERE ts >= ? AND ts < ?`, fromMs, toMs)
		var n, succ, fail, canc, in, out, cr, dLat, dCnt int64
		var cash, quota, sub, shadow float64
		if err := row.Scan(&n, &succ, &fail, &canc, &in, &out, &cr,
			&cash, &quota, &sub, &shadow, &dLat, &dCnt); err != nil {
			return err
		}
		s.Requests += n
		s.Succeeded += succ
		s.Failed += fail
		s.Canceled += canc
		s.InputTokens += in
		s.OutputTokens += out
		s.CacheRead += cr
		s.EstCashCost += cash
		s.QuotaUnits += quota
		s.AllocatedSub += sub
		s.ShadowCost += shadow
		latSum += dLat
		latCnt += dCnt
		return nil
	}
	if detailOnly {
		edgeTo = to
		edgeFrom = to
	}
	if err := sumDetail(from.UnixMilli(), edgeTo.UnixMilli()); err != nil {
		return nil, fmt.Errorf("summarize head: %w", err)
	}
	tailStart := edgeFrom
	if edgeTo.After(tailStart) {
		tailStart = edgeTo // windows shorter than an hour are fully covered above
	}
	if tailStart.Before(to) {
		if err := sumDetail(tailStart.UnixMilli(), to.UnixMilli()); err != nil {
			return nil, fmt.Errorf("summarize tail: %w", err)
		}
	}
	// rollup pass over the full buckets inside [edgeTo, edgeFrom)
	if edgeTo.Before(edgeFrom) {
		var ru struct {
			Requests, Failures, Canceled, Input, Output, CacheRead, LatSum, LatCnt int64
			Cash, Quota                                                            float64
		}
		err := reader.QueryRow(`SELECT COALESCE(SUM(requests),0),
			COALESCE(SUM(failures),0), COALESCE(SUM(canceled),0),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(est_cash_cost),0), COALESCE(SUM(quota_units),0),
			COALESCE(SUM(latency_sum_ms),0), COALESCE(SUM(latency_count),0)
			FROM usage_hourly WHERE bucket >= ? AND bucket < ?`,
			edgeTo.UnixMilli()/3600000, edgeFrom.UnixMilli()/3600000).
			Scan(&ru.Requests, &ru.Failures, &ru.Canceled, &ru.Input, &ru.Output,
				&ru.CacheRead, &ru.Cash, &ru.Quota, &ru.LatSum, &ru.LatCnt)
		if err != nil {
			return nil, fmt.Errorf("summarize rollup: %w", err)
		}
		s.Requests += ru.Requests
		// usage_hourly stores failed and canceled as separate counters.
		s.Succeeded += ru.Requests - ru.Failures - ru.Canceled
		s.Failed += ru.Failures
		s.Canceled += ru.Canceled
		s.InputTokens += ru.Input
		s.OutputTokens += ru.Output
		s.CacheRead += ru.CacheRead
		s.EstCashCost += ru.Cash
		s.QuotaUnits += ru.Quota
		latSum += ru.LatSum
		latCnt += ru.LatCnt
	}
	if latCnt > 0 {
		s.AvgLatencyMS = float64(latSum) / float64(latCnt)
	}
	if err := reader.QueryRow(`SELECT COALESCE(MAX(duration_ms),0) FROM (
		SELECT duration_ms FROM logical_requests
		WHERE ts >= ? AND ts < ? AND duration_ms > 0
		ORDER BY duration_ms ASC LIMIT 1 OFFSET MAX(0, (
			(SELECT COUNT(*) FROM logical_requests WHERE ts >= ? AND ts < ? AND duration_ms > 0) * 95 + 99) / 100 - 1)
	)`, from.UnixMilli(), to.UnixMilli(), from.UnixMilli(), to.UnixMilli()).Scan(&s.P95LatencyMS); err != nil {
		return nil, fmt.Errorf("summarize p95 latency: %w", err)
	}
	var median sql.NullFloat64
	if err := reader.QueryRow(`WITH ranked AS (
		SELECT duration_ms,
			ROW_NUMBER() OVER (ORDER BY duration_ms) AS row_number,
			COUNT(*) OVER () AS row_count
		FROM logical_requests
		WHERE ts >= ? AND ts < ? AND duration_ms > 0
	)
	SELECT AVG(duration_ms) FROM ranked
	WHERE row_number IN ((row_count + 1) / 2, (row_count + 2) / 2)`,
		from.UnixMilli(), to.UnixMilli()).Scan(&median); err != nil {
		return nil, fmt.Errorf("summarize median latency: %w", err)
	}
	if median.Valid {
		value := int64(median.Float64 + 0.5)
		s.MedianLatencyMS = &value
	}
	var recoveryLatency sql.NullFloat64
	if err := reader.QueryRow(`SELECT AVG(recovery_added_latency_ms) FROM logical_requests
		WHERE ts >= ? AND ts < ? AND UPPER(outcome)='RECOVERED_SUCCESS'`,
		from.UnixMilli(), to.UnixMilli()).Scan(&recoveryLatency); err != nil {
		return nil, fmt.Errorf("summarize recovery latency: %w", err)
	}
	if recoveryLatency.Valid {
		value := recoveryLatency.Float64
		s.AvgRecoveryLatencyMS = &value
	}
	// The hourly rollup has no subscription/shadow or upstream dimensions.
	// Read these over the whole interval, including all complete hours.
	if err := reader.QueryRow(`SELECT
		COALESCE(SUM(upstream_input_tokens),0),
		COALESCE(SUM(upstream_output_tokens),0),
		COALESCE(SUM(serving_cost),0),
		COALESCE(SUM(recovery_overhead_cost),0),
		COALESCE(SUM(allocated_sub_cost),0),
		COALESCE(SUM(shadow_cost),0),
		COALESCE(SUM(`+requestTotalCostSQL+`),0)
		FROM logical_requests WHERE ts >= ? AND ts < ?`, from.UnixMilli(), to.UnixMilli()).
		Scan(&s.UpstreamInputTokens, &s.UpstreamOutputTokens, &s.ServingCost, &s.RecoveryOverheadCost,
			&s.AllocatedSub, &s.ShadowCost, &s.TotalCost); err != nil {
		return nil, fmt.Errorf("summarize cost dimensions: %w", err)
	}
	var tokensObserved, costObserved int
	if err := reader.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM logical_requests WHERE ts >= ? AND ts < ? AND
			(input_tokens != 0 OR output_tokens != 0 OR upstream_input_tokens != 0 OR upstream_output_tokens != 0)),
		EXISTS(SELECT 1 FROM logical_requests WHERE ts >= ? AND ts < ? AND
			(est_cash_cost != 0 OR serving_cost != 0 OR recovery_overhead_cost != 0))`,
		from.UnixMilli(), to.UnixMilli(), from.UnixMilli(), to.UnixMilli()).Scan(&tokensObserved, &costObserved); err != nil {
		return nil, fmt.Errorf("summarize observations: %w", err)
	}
	s.TokensObserved = tokensObserved != 0
	s.CostObserved = costObserved != 0
	return s, nil
}

func (l *Ledger) OutcomeSummary(from, to time.Time) (*OutcomeSummary, error) {
	out := &OutcomeSummary{}
	err := l.read.QueryRow(`SELECT
		COALESCE(SUM(UPPER(outcome)='DIRECT_SUCCESS' OR (outcome='' AND status='success')),0),
		COALESCE(SUM(UPPER(outcome)='RECOVERED_SUCCESS'),0),
		COALESCE(SUM(UPPER(outcome)='FAILED' OR (outcome='' AND status!='success')),0)
		FROM logical_requests WHERE ts >= ? AND ts < ?`, from.UnixMilli(), to.UnixMilli()).
		Scan(&out.DirectSuccess, &out.RecoveredSuccess, &out.Failed)
	if err != nil {
		return nil, fmt.Errorf("outcome summary: %w", err)
	}
	return out, nil
}

func (l *Ledger) TrafficByLogicalModel(from, to time.Time) ([]TrafficSummary, error) {
	return l.v2TrafficBy("logical_model_id", from, to)
}

func (l *Ledger) TrafficByProvider(from, to time.Time) ([]TrafficSummary, error) {
	return l.v2TrafficBy("provider_id", from, to)
}

func (l *Ledger) TrafficByRoute(from, to time.Time) ([]RouteTrafficSummary, error) {
	rows, err := l.read.Query(`SELECT logical_model_id,provider_id,upstream_model,COUNT(*),
		COALESCE(SUM(UPPER(outcome)='DIRECT_SUCCESS' OR (outcome='' AND status='success')),0),
		COALESCE(SUM(UPPER(outcome)='RECOVERED_SUCCESS'),0)
		FROM logical_requests WHERE ts >= ? AND ts < ? AND logical_model_id != '' AND provider_id != ''
		GROUP BY logical_model_id,provider_id,upstream_model`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RouteTrafficSummary{}
	for rows.Next() {
		var item RouteTrafficSummary
		if err := rows.Scan(&item.LogicalModelID, &item.ProviderID, &item.UpstreamModel,
			&item.Requests, &item.DirectTraffic, &item.RecoveryTraffic); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (l *Ledger) v2TrafficBy(column string, from, to time.Time) ([]TrafficSummary, error) {
	if column != "logical_model_id" && column != "provider_id" {
		return nil, fmt.Errorf("unsupported V2 traffic dimension %q", column)
	}
	query := fmt.Sprintf(`SELECT %s, COUNT(*),`+ // #nosec G201 -- column is rejected above unless it is one of two fixed identifiers; every value stays a bound parameter.
		`COALESCE(SUM(UPPER(outcome)='DIRECT_SUCCESS' OR (outcome='' AND status='success')),0),
		COALESCE(SUM(UPPER(outcome)='RECOVERED_SUCCESS'),0),
		COALESCE(SUM(UPPER(outcome)='FAILED' OR (outcome='' AND status!='success')),0),
		AVG(CASE WHEN status='success' AND ttft_ms>0 THEN ttft_ms END),
		AVG(CASE WHEN status='success' AND duration_ms>0 THEN duration_ms END),
		MAX(CASE WHEN status='success' THEN ts END)
		FROM logical_requests WHERE ts >= ? AND ts < ? AND %s != '' GROUP BY %s`, column, column, column)
	rows, err := l.read.Query(query, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrafficSummary{}
	for rows.Next() {
		var item TrafficSummary
		var avgTTFT, avgDuration sql.NullFloat64
		var lastSuccessMS sql.NullInt64
		if err := rows.Scan(&item.ID, &item.Requests, &item.DirectTraffic, &item.RecoveryTraffic, &item.Failed,
			&avgTTFT, &avgDuration, &lastSuccessMS); err != nil {
			return nil, err
		}
		if avgTTFT.Valid {
			item.AvgTTFTMS = &avgTTFT.Float64
		}
		if avgDuration.Valid {
			item.AvgDurationMS = &avgDuration.Float64
		}
		if lastSuccessMS.Valid {
			lastSuccess := time.UnixMilli(lastSuccessMS.Int64)
			item.LastSuccessAt = &lastSuccess
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].ID < out[j].ID
	})
	return out, rows.Err()
}

// TargetUsage is per-target rollup for the console dashboard.
type TargetUsage struct {
	Target       string  `json:"target"`
	Requests     int64   `json:"requests"`
	Failures     int64   `json:"failures"`
	Tokens       int64   `json:"tokens"`
	EstCashCost  float64 `json:"est_cash_cost"`
	QuotaUnits   float64 `json:"quota_units"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

// ByTarget rolls up usage per selected target over a window. Full hour
// buckets come from the rollup table; ragged edges from the detail table.
func (l *Ledger) ByTarget(from, to time.Time) ([]TargetUsage, error) {
	type acc struct {
		requests, failures, tokens, latSum, latCnt int64
		cash, quota, lat                           float64
	}
	byTarget := map[string]*acc{}
	add := func(t string, reqs, fails, toks, latSum, latCnt int64, cash, quota float64) {
		a := byTarget[t]
		if a == nil {
			a = &acc{}
			byTarget[t] = a
		}
		a.requests += reqs
		a.failures += fails
		a.tokens += toks
		a.cash += cash
		a.quota += quota
		a.latSum += latSum
		a.latCnt += latCnt
	}
	// detail pass: ragged edges only [from, edgeTo) ∪ [edgeFrom, to)
	edgeFrom := to.Truncate(time.Hour)
	edgeTo := from.Truncate(time.Hour).Add(time.Hour)
	if edgeTo.After(to) {
		edgeTo = to
	}
	scanDetail := func(fromMs, toMs int64) error {
		rows, err := l.read.Query(`SELECT selected_target, COUNT(*),
			COALESCE(SUM(status!='success'),0),
			COALESCE(SUM(input_tokens+output_tokens),0),
			COALESCE(SUM(est_cash_cost),0),
			COALESCE(SUM(quota_units),0),
			COALESCE(SUM(duration_ms),0),
			COUNT(NULLIF(duration_ms,0))
			FROM logical_requests
			WHERE ts >= ? AND ts < ? AND selected_target != ''
			GROUP BY selected_target`, fromMs, toMs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			var reqs, fails, toks, latSum, latCnt int64
			var cash, quota float64
			if err := rows.Scan(&t, &reqs, &fails, &toks, &cash, &quota, &latSum, &latCnt); err != nil {
				return err
			}
			add(t, reqs, fails, toks, latSum, latCnt, cash, quota)
		}
		return rows.Err()
	}
	if err := scanDetail(from.UnixMilli(), edgeTo.UnixMilli()); err != nil {
		return nil, fmt.Errorf("by target edge: %w", err)
	}
	tailStart := edgeFrom
	if edgeTo.After(tailStart) {
		tailStart = edgeTo
	}
	if tailStart.Before(to) {
		if err := scanDetail(tailStart.UnixMilli(), to.UnixMilli()); err != nil {
			return nil, fmt.Errorf("by target tail: %w", err)
		}
	}
	// rollup pass: full buckets inside [edgeTo, edgeFrom)
	if edgeTo.Before(edgeFrom) {
		rows, err := l.read.Query(`SELECT target, SUM(requests), SUM(failures+canceled),
			SUM(input_tokens+output_tokens), SUM(est_cash_cost), SUM(quota_units),
			SUM(latency_sum_ms), SUM(latency_count)
			FROM usage_hourly WHERE bucket >= ? AND bucket < ? AND target != ''
			GROUP BY target`, edgeTo.UnixMilli()/3600000, edgeFrom.UnixMilli()/3600000)
		if err != nil {
			return nil, fmt.Errorf("by target rollup: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			var reqs, fails, toks, latSum, latCnt int64
			var cash, quota float64
			if err := rows.Scan(&t, &reqs, &fails, &toks, &cash, &quota, &latSum, &latCnt); err != nil {
				return nil, err
			}
			add(t, reqs, fails, toks, latSum, latCnt, cash, quota)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]TargetUsage, 0, len(byTarget))
	for t, a := range byTarget {
		tu := TargetUsage{Target: t, Requests: a.requests, Failures: a.failures,
			Tokens: a.tokens, EstCashCost: a.cash, QuotaUnits: a.quota}
		if a.latCnt > 0 {
			tu.AvgLatencyMS = float64(a.latSum) / float64(a.latCnt)
		}
		out = append(out, tu)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out, nil
}

// ---------------------------------------------------------------------------
// console breakdowns (PRD §16) — detail-table GROUP BYs. These dimensions
// (consumer, policy version, compat level, failure type) are not in the
// usage_hourly rollup, so they scan the window's logical_requests directly;
// windows are bounded by the dashboard's retention-aware defaults.

// Apply the legacy fallback per request, so mixed old/new records reconcile
// between totals, dimensional breakdowns and time series.
const requestTotalCostSQL = `CASE WHEN serving_cost != 0 OR recovery_overhead_cost != 0
	THEN serving_cost+recovery_overhead_cost ELSE est_cash_cost END`

// groupByDimension is the shared SELECT for console breakdowns; dim is a
// trusted column name (never user input).
const groupByDimension = `SELECT %s,
	COUNT(*),
	COALESCE(SUM(status='success'),0),
	COALESCE(SUM(status='failed'),0),
	COALESCE(SUM(status='canceled'),0),
	COALESCE(SUM(input_tokens),0),
	COALESCE(SUM(output_tokens),0),
	COALESCE(SUM(cache_read_tokens),0),
	COALESCE(SUM(est_cash_cost),0),
	COALESCE(SUM(quota_units),0),
	COALESCE(SUM(allocated_sub_cost),0),
	COALESCE(SUM(shadow_cost),0),
	COALESCE(SUM(serving_cost),0),
	COALESCE(SUM(recovery_overhead_cost),0),
	COALESCE(SUM(upstream_input_tokens),0),
	COALESCE(SUM(upstream_output_tokens),0),
	COALESCE(SUM(` + requestTotalCostSQL + `),0),
	COALESCE(SUM(CASE WHEN input_tokens != 0 OR output_tokens != 0 OR upstream_input_tokens != 0 OR upstream_output_tokens != 0 THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN est_cash_cost != 0 OR serving_cost != 0 OR recovery_overhead_cost != 0 THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN duration_ms > 0 THEN duration_ms ELSE 0 END),0),
	COUNT(NULLIF(duration_ms,0))
	FROM logical_requests WHERE ts >= ? AND ts < ? GROUP BY %s`

func (l *Ledger) groupSum(dim string, fromMs, toMs int64) (map[string]*UsageSummary, error) {
	return reportGroups(l.read, dim, fromMs, toMs)
}
func reportGroups(reader reportReader, dim string, fromMs, toMs int64) (map[string]*UsageSummary, error) {
	rows, err := reader.Query(fmt.Sprintf(groupByDimension, dim, dim), fromMs, toMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*UsageSummary{}
	for rows.Next() {
		var key string
		s := &UsageSummary{}
		var latSum, latCnt, tokenRows, costRows int64
		var cash, quota, sub, shadow float64
		if err := rows.Scan(&key, &s.Requests,
			&s.Succeeded, &s.Failed, &s.Canceled,
			&s.InputTokens, &s.OutputTokens, &s.CacheRead,
			&cash, &quota, &sub, &shadow, &s.ServingCost, &s.RecoveryOverheadCost,
			&s.UpstreamInputTokens, &s.UpstreamOutputTokens, &s.TotalCost,
			&tokenRows, &costRows, &latSum, &latCnt); err != nil {
			return nil, err
		}
		s.EstCashCost, s.QuotaUnits, s.AllocatedSub, s.ShadowCost = cash, quota, sub, shadow
		s.TokensObserved = tokenRows > 0
		s.CostObserved = costRows > 0
		if latCnt > 0 {
			s.AvgLatencyMS = float64(latSum) / float64(latCnt)
		}
		out[key] = s
	}
	return out, rows.Err()
}

// ByConsumer breaks usage down per consumer key over a window.
func (l *Ledger) ByConsumer(from, to time.Time) (map[string]*UsageSummary, error) {
	m, err := l.groupSum("consumer_key", from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("by consumer: %w", err)
	}
	delete(m, "") // unauthenticated rows surface under the summary totals only
	return m, nil
}

// UsageByDimension exposes ledger-backed cost and token breakdowns for the V2
// management facade. Only product-level dimensions are accepted.
func (l *Ledger) UsageByDimension(dimension string, from, to time.Time) (map[string]*UsageSummary, error) {
	switch dimension {
	case "logical_model_id", "provider_id":
	default:
		return nil, fmt.Errorf("unsupported usage dimension %q", dimension)
	}
	out, err := l.groupSum(dimension, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("usage by %s: %w", dimension, err)
	}
	// Empty identity means the request ended before a model/provider was
	// selected. Keep it as an explicit unassigned group for reconciliation.
	return out, nil
}

// ByConsumerDimension returns the same usage dimensions as the global
// dashboard, scoped to one consumer. dim is allow-listed because SQL column
// names cannot be bound as parameters.
func (l *Ledger) ByConsumerDimension(consumer, dim string, from, to time.Time) (map[string]*UsageSummary, error) {
	if dim != "selected_target" && dim != "model_selected" {
		return nil, fmt.Errorf("unsupported consumer dimension %q", dim)
	}
	q := fmt.Sprintf(`SELECT %s,`+ // #nosec G201 -- dim is rejected above unless it is one of two fixed identifiers; every value stays a bound parameter.
		`COUNT(*),
		COALESCE(SUM(status='success'),0),
		COALESCE(SUM(status='failed'),0),
		COALESCE(SUM(status='canceled'),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(est_cash_cost),0),
		COALESCE(SUM(quota_units),0), COALESCE(SUM(allocated_sub_cost),0),
		COALESCE(SUM(shadow_cost),0),
		COALESCE(SUM(CASE WHEN duration_ms > 0 THEN duration_ms ELSE 0 END),0),
		COUNT(NULLIF(duration_ms,0))
		FROM logical_requests
		WHERE ts >= ? AND ts < ? AND consumer_key=? AND %s != ''
		GROUP BY %s`, dim, dim, dim)
	rows, err := l.read.Query(q, from.UnixMilli(), to.UnixMilli(), consumer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*UsageSummary{}
	for rows.Next() {
		var key string
		s := &UsageSummary{}
		var latSum, latCnt int64
		if err := rows.Scan(&key, &s.Requests, &s.Succeeded, &s.Failed, &s.Canceled,
			&s.InputTokens, &s.OutputTokens, &s.CacheRead, &s.EstCashCost,
			&s.QuotaUnits, &s.AllocatedSub, &s.ShadowCost, &latSum, &latCnt); err != nil {
			return nil, err
		}
		if latCnt > 0 {
			s.AvgLatencyMS = float64(latSum) / float64(latCnt)
		}
		out[key] = s
	}
	return out, rows.Err()
}

func (l *Ledger) ConsumerFailureBreakdown(consumer string, from, to time.Time) (map[string]int64, error) {
	rows, err := l.read.Query(`SELECT failure_type,COUNT(*) FROM logical_requests
		WHERE ts >= ? AND ts < ? AND consumer_key=? AND failure_type != '' GROUP BY failure_type`,
		from.UnixMilli(), to.UnixMilli(), consumer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			return nil, err
		}
		out[key] = count
	}
	return out, rows.Err()
}

func (l *Ledger) ConsumerFallbackCount(consumer string, from, to time.Time) (int64, error) {
	var count int64
	err := l.read.QueryRow(`SELECT COUNT(*) FROM logical_requests
		WHERE ts >= ? AND ts < ? AND consumer_key=? AND attempts > 1`,
		from.UnixMilli(), to.UnixMilli(), consumer).Scan(&count)
	return count, err
}

// PolicyVersionUsage is one policy name+version's share of window traffic.
type PolicyVersionUsage struct {
	PolicyName    string        `json:"policy_name"`
	PolicyVersion int64         `json:"policy_version"`
	Summary       *UsageSummary `json:"summary"`
}

// ByPolicyVersion groups window traffic by policy and published version.
func (l *Ledger) ByPolicyVersion(from, to time.Time) ([]PolicyVersionUsage, error) {
	rows, err := l.read.Query(`SELECT policy_name, policy_version,
		COUNT(*),
		COALESCE(SUM(status='success'),0),
		COALESCE(SUM(status='failed'),0),
		COALESCE(SUM(status='canceled'),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(est_cash_cost),0), COALESCE(SUM(quota_units),0),
		COALESCE(SUM(allocated_sub_cost),0), COALESCE(SUM(shadow_cost),0)
		FROM logical_requests WHERE ts >= ? AND ts < ? AND policy_name != ''
		GROUP BY policy_name, policy_version`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("by policy version: %w", err)
	}
	defer rows.Close()
	out := []PolicyVersionUsage{}
	for rows.Next() {
		u := PolicyVersionUsage{Summary: &UsageSummary{}}
		if err := rows.Scan(&u.PolicyName, &u.PolicyVersion,
			&u.Summary.Requests, &u.Summary.Succeeded, &u.Summary.Failed,
			&u.Summary.Canceled, &u.Summary.InputTokens, &u.Summary.OutputTokens,
			&u.Summary.CacheRead, &u.Summary.EstCashCost, &u.Summary.QuotaUnits,
			&u.Summary.AllocatedSub, &u.Summary.ShadowCost); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PolicyName != out[j].PolicyName {
			return out[i].PolicyName < out[j].PolicyName
		}
		return out[i].PolicyVersion < out[j].PolicyVersion
	})
	return out, rows.Err()
}

// CompatBreakdown counts requests per compatibility level over a window.
func (l *Ledger) CompatBreakdown(from, to time.Time) (map[string]int64, error) {
	rows, err := l.read.Query(`SELECT compat_level, COUNT(*) FROM logical_requests
		WHERE ts >= ? AND ts < ? GROUP BY compat_level`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("compat breakdown: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var lvl string
		var n int64
		if err := rows.Scan(&lvl, &n); err != nil {
			return nil, err
		}
		if lvl == "" {
			lvl = "exact" // pre-gating rows default to the native level
		}
		out[lvl] = n
	}
	return out, rows.Err()
}

// FailureBreakdown counts failed/canceled requests per typed failure.
func (l *Ledger) FailureBreakdown(from, to time.Time) (map[string]int64, error) {
	rows, err := l.read.Query(`SELECT failure_type, COUNT(*) FROM logical_requests
		WHERE ts >= ? AND ts < ? AND failure_type != ''
		GROUP BY failure_type ORDER BY COUNT(*) DESC`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("failure breakdown: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var ft string
		var n int64
		if err := rows.Scan(&ft, &n); err != nil {
			return nil, err
		}
		out[ft] = n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// filter builder

func buildWhere(f domain.LogFilter) (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, args2 ...any) {
		conds = append(conds, cond)
		args = append(args, args2...)
	}
	if !f.From.IsZero() {
		add("ts >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		add("ts < ?", f.To.UnixMilli())
	}
	if f.RequestID != "" {
		add("id = ?", f.RequestID)
	}
	if f.Search != "" {
		like := "%" + f.Search + "%"
		add(`(id LIKE ? OR consumer_key LIKE ? OR EXISTS (
			SELECT 1 FROM decision_traces dt
			WHERE dt.request_id = logical_requests.id
			AND COALESCE(json_extract(dt.trace, '$.conversation_id'), '') LIKE ?
		))`, like, like, like)
	}
	if f.ConsumerKey != "" {
		add("consumer_key = ?", f.ConsumerKey)
	}
	if f.ProviderID != "" {
		add("provider_id = ?", f.ProviderID)
	}
	if f.LogicalModel != "" {
		add("COALESCE(NULLIF(logical_model_id,''),NULLIF(model_requested,''),'__unknown__') = ?", f.LogicalModel)
	}
	if f.ParticipatingProvider != "" || f.AttemptStatus != "" || f.AttemptFailureType != "" {
		cond := "EXISTS (SELECT 1 FROM attempts pa WHERE pa.request_id=logical_requests.id"
		var params []any
		if f.ParticipatingProvider != "" {
			cond += " AND COALESCE(NULLIF(pa.provider_id,''),'__unknown__')=?"
			params = append(params, f.ParticipatingProvider)
		}
		if f.AttemptStatus != "" {
			cond += " AND pa.status=?"
			params = append(params, f.AttemptStatus)
		}
		if f.AttemptFailureType != "" {
			cond += " AND pa.status!='success' AND COALESCE(NULLIF(pa.failure_type,''),NULLIF(pa.status,''),'unknown')=?"
			params = append(params, f.AttemptFailureType)
		}
		add(cond+")", params...)
	}
	if f.ConnectionID != "" {
		add("connection_id = ?", f.ConnectionID)
	}
	if f.ConversationID != "" {
		add(`EXISTS (SELECT 1 FROM decision_traces dt
			WHERE dt.request_id = logical_requests.id
			AND COALESCE(json_extract(dt.trace, '$.conversation_id'), '') = ?)`, f.ConversationID)
	}
	if f.Outcome != "" {
		switch domain.RequestOutcome(strings.ToUpper(string(f.Outcome))) {
		case domain.OutcomeDirectSuccess:
			add("(UPPER(outcome) = 'DIRECT_SUCCESS' OR (outcome = '' AND status = 'success'))")
		case domain.OutcomeFailed:
			add("(UPPER(outcome) = 'FAILED' OR (outcome = '' AND status IN ('failed','canceled')))")
		default:
			add("UPPER(outcome) = ?", strings.ToUpper(string(f.Outcome)))
		}
	}
	if f.Alias != "" {
		add("alias = ?", f.Alias)
	}
	if f.Objective != "" {
		add("objective = ?", f.Objective)
	}
	if f.Target != "" {
		add("selected_target LIKE ?", "%"+f.Target+"%")
	}
	if f.Model != "" {
		like := "%" + f.Model + "%"
		add("(model_selected LIKE ? OR model_requested LIKE ?)", like, like)
	}
	if f.Status != "" {
		add("status = ?", string(f.Status))
	}
	if f.ReportError != "" {
		add("status!='success' AND COALESCE(NULLIF(failure_type,''),NULLIF(status,''),'unknown')=?", f.ReportError)
	}
	if f.FailureType != "" {
		add("failure_type = ?", string(f.FailureType))
	}
	if f.PolicyName != "" {
		add("policy_name = ?", f.PolicyName)
	}
	if f.PolicyVersion > 0 {
		add("policy_version = ?", f.PolicyVersion)
	}
	if f.RuntimeRevision > 0 {
		add("runtime_revision = ?", f.RuntimeRevision)
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}
