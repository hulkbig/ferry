package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestLedgerDatabaseModeIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger.db mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestDebugSettingAndPayloadsPersist(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, 32, 0)
	if err != nil {
		t.Fatal(err)
	}
	if l.DebugCaptureEnabled() {
		t.Fatal("debug capture must default off")
	}
	if err := l.SetDebugCapture(true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.CaptureDebugRequest("req-debug", now, `{"model":"smart"}`, false)
	l.CaptureDebugResponse("req-debug", `{"status":"success"}`, true)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, 32, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.DebugCaptureEnabled() {
		t.Fatal("debug setting did not persist")
	}
	record, err := reopened.QueryRequestDebug("req-debug")
	if err != nil || record == nil {
		t.Fatalf("query debug: record=%+v err=%v", record, err)
	}
	if record.RequestJSON != `{"model":"smart"}` || record.ResponseJSON != `{"status":"success"}` ||
		record.RequestTruncated || !record.ResponseTruncated {
		t.Fatalf("debug record=%+v", record)
	}
}

// tledger wraps the ledger so Close is idempotent across cleanup and
// explicit reopen.
type tledger struct {
	*Ledger
	dir  string
	once sync.Once
}

func (tl *tledger) close() {
	tl.once.Do(func() { tl.Ledger.Close() })
}

func openTemp(t *testing.T) *tledger {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir, 256, 0)
	if err != nil {
		t.Fatal(err)
	}
	tl := &tledger{Ledger: l, dir: dir}
	t.Cleanup(tl.close)
	return tl
}

func sampleRequest(id string, ts time.Time) domain.RequestRecord {
	return domain.RequestRecord{
		ID: id, Ts: ts,
		ConsumerKey: "team-x", Alias: "glm-flash", Objective: "cheap",
		PolicyName: "std", PolicyVersion: 3,
		PolicyHash: "policy-sha", PolicyIndexHash: "index-sha",
		PolicyCompileCacheHit: true, PolicySceneCacheHit: true, FallbackCount: 2,
		ConnectionRebindCount: 1, RouteFallbackCount: 1,
		RouteAdvanceCount: 1, RecoveryFallbackCount: 1, ConversationMigrationCount: 1,
		ModelRequested: "glm-4-flash", ModelSelected: "glm-4-flash",
		Protocol: domain.ProtocolOpenAIChat, Stream: true,
		LogicalModelID: "smart", ProviderID: "provider-a", ConnectionID: "account-a",
		UpstreamModel: "glm-4-flash", RuntimeRevision: 12, RouteFingerprint: "route-sha",
		CommitAt: ts.Add(100 * time.Millisecond), ContinuityMode: domain.ContinuityRehydrated,
		Status: domain.ReqSuccess, Outcome: domain.OutcomeRecoveredSuccess,
		Usage:         domain.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10, ReasoningTokens: 5},
		UpstreamUsage: domain.Usage{InputTokens: 130, OutputTokens: 55, CacheReadTokens: 10, ReasoningTokens: 5},
		EstCashCost:   0.25, QuotaUnits: 1.5, AllocatedSubCost: 0.01, ShadowCost: 0.4,
		ServingCost: 0.2, RecoveryOverheadCost: 0.05, RecoveryAddedLatencyMS: 75,
		SelectedTarget: "glm-1", DurationMS: 800, TTFTMS: 120,
		CompatLevel: domain.CompatMapped, CompatNotes: []string{"thinking block dropped"},
		Exclusions: []domain.ExclusionRecord{
			{Target: "glm-2", Tier: "primary", Reason: "circuit_breaker = open"},
		},
		ClientIP: "127.0.0.1",
	}
}

// Writes are async: Close flushes, so round-trip tests Close and re-Open the
// same directory.
func reopen(t *testing.T, l *tledger) *tledger {
	t.Helper()
	l.close()
	nl, err := Open(l.dir, 256, 0)
	if err != nil {
		t.Fatal(err)
	}
	tl := &tledger{Ledger: nl, dir: l.dir}
	t.Cleanup(tl.close)
	return tl
}

func TestRequestRoundTrip(t *testing.T) {
	l := openTemp(t)
	ts := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	l.BeginRequest(sampleRequest("req-1", ts))
	l.FinishRequest("req-1", nil)

	// reopen() already closed l; drop its cleanup so Close isn't called twice.
	l2 := reopen(t, l)
	recs, total, err := l2.QueryRequests(domain.LogFilter{})
	if err != nil || total != 1 || len(recs) != 1 {
		t.Fatalf("query: %v total=%d n=%d", err, total, len(recs))
	}
	got := recs[0]
	want := sampleRequest("req-1", ts) // for field comparison
	if got.ID != want.ID || got.ConsumerKey != "team-x" || got.Alias != "glm-flash" ||
		got.Objective != "cheap" || got.PolicyName != "std" || got.PolicyVersion != 3 {
		t.Errorf("identity fields: %+v", got)
	}
	if got.PolicyHash != "policy-sha" || got.PolicyIndexHash != "index-sha" ||
		!got.PolicyCompileCacheHit || !got.PolicySceneCacheHit || got.FallbackCount != 2 ||
		got.ConnectionRebindCount != 1 || got.RouteFallbackCount != 1 {
		t.Errorf("policy observability fields: %+v", got)
	}
	if got.RouteAdvanceCount != 1 || got.RecoveryFallbackCount != 1 || got.ConversationMigrationCount != 1 ||
		got.LogicalModelID != "smart" || got.ProviderID != "provider-a" || got.ConnectionID != "account-a" ||
		got.UpstreamModel != "glm-4-flash" || got.RuntimeRevision != 12 || got.RouteFingerprint != "route-sha" ||
		got.ContinuityMode != domain.ContinuityRehydrated || got.CommitAt.IsZero() {
		t.Errorf("V2 execution fields: %+v", got)
	}
	if got.ModelSelected != "glm-4-flash" || got.Protocol != domain.ProtocolOpenAIChat || !got.Stream {
		t.Errorf("model/protocol/stream: %+v", got)
	}
	if got.Status != domain.ReqSuccess || got.Outcome != domain.OutcomeRecoveredSuccess || got.Attempts != 0 {
		t.Errorf("status: %+v", got)
	}
	if got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 50 ||
		got.Usage.CacheReadTokens != 10 || got.Usage.ReasoningTokens != 5 {
		t.Errorf("usage: %+v", got.Usage)
	}
	if got.UpstreamUsage.InputTokens != 130 || got.UpstreamUsage.OutputTokens != 55 {
		t.Errorf("upstream usage: %+v", got.UpstreamUsage)
	}
	if got.EstCashCost != 0.25 || got.QuotaUnits != 1.5 || got.ShadowCost != 0.4 || got.AllocatedSubCost != 0.01 {
		t.Errorf("costs: %+v", got)
	}
	if got.ServingCost != 0.2 || got.RecoveryOverheadCost != 0.05 || got.RecoveryAddedLatencyMS != 75 {
		t.Errorf("V2 costs: %+v", got)
	}
	if got.SelectedTarget != "glm-1" || got.DurationMS != 800 || got.TTFTMS != 120 {
		t.Errorf("selection/timing: %+v", got)
	}
	if got.CompatLevel != domain.CompatMapped || len(got.CompatNotes) != 1 ||
		got.CompatNotes[0] != "thinking block dropped" {
		t.Errorf("compat: %s %v", got.CompatLevel, got.CompatNotes)
	}
	if len(got.Exclusions) != 1 || got.Exclusions[0].Target != "glm-2" ||
		got.Exclusions[0].Reason != "circuit_breaker = open" {
		t.Errorf("exclusions: %+v", got.Exclusions)
	}
	if got.ClientIP != "127.0.0.1" {
		t.Errorf("client ip: %q", got.ClientIP)
	}
	_ = want
}

func TestFinishMutatesRecord(t *testing.T) {
	l := openTemp(t)
	ts := time.Now()
	rec := sampleRequest("req-m", ts)
	rec.Status = ""
	l.BeginRequest(rec)
	l.AddAttempt(domain.AttemptRecord{RequestID: "req-m", Seq: 1, Ts: ts,
		TargetID: "glm-1", Status: domain.ReqSuccess, LatencyMS: 100})
	l.FinishRequest("req-m", func(r *domain.RequestRecord) {
		r.Status = domain.ReqSuccess
		r.Attempts = 1
		r.SelectedTarget = "glm-1"
		r.DurationMS = 100
	})

	l2 := reopen(t, l)
	recs, _, _ := mustQuery(t, l2.Ledger, domain.LogFilter{})
	if len(recs) != 1 || recs[0].Status != domain.ReqSuccess || recs[0].Attempts != 1 {
		t.Fatalf("mutated record wrong: %+v", recs)
	}
}

func TestAttemptWrittenImmediatelyAndQueryableAfterClose(t *testing.T) {
	l := openTemp(t)
	ts := time.Now()
	l.BeginRequest(sampleRequest("req-a", ts))
	l.AddAttempt(domain.AttemptRecord{RequestID: "req-a", Seq: 1, Ts: ts,
		TargetID: "glm-1", ProviderID: "provider-historical", ConnectionID: "connection-historical",
		Protocol:      domain.ProtocolOpenAIChat,
		CredentialRef: "env:ZHIPU_KEY", Model: "glm-4-flash", PlanKind: domain.PlanFree,
		Status: domain.ReqFailed, LatencyMS: 55, TTFTMS: 0,
		Failure:    &domain.Failure{Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, HTTPStatus: 429, Message: "slow"},
		Multiplier: 0.5, RateCardVersion: "card@1", MultiplierRuleVersion: "glm-1@3",
	})
	// attempt persists even though the request is never finished
	l2 := reopen(t, l)
	atts, err := l2.QueryAttempts("req-a")
	if err != nil || len(atts) != 1 {
		t.Fatalf("attempts: %v n=%d", err, len(atts))
	}
	if atts[0].Failure == nil || atts[0].Failure.Scope != domain.FailureScopeConnection {
		t.Fatalf("attempt failure scope lost: %+v", atts[0].Failure)
	}
	a := atts[0]
	if a.Seq != 1 || a.TargetID != "glm-1" || a.Status != domain.ReqFailed {
		t.Errorf("attempt identity: %+v", a)
	}
	if a.ProviderID != "provider-historical" || a.ConnectionID != "connection-historical" {
		t.Errorf("attempt public identities not persisted: %+v", a)
	}
	if a.CredentialRef != "env:ZHIPU_KEY" {
		t.Errorf("credential ref must round-trip as reference text: %q", a.CredentialRef)
	}
	if a.Failure == nil || a.Failure.Type != domain.FailRateLimit ||
		a.Failure.HTTPStatus != 429 || a.Failure.Message != "slow" {
		t.Errorf("failure: %+v", a.Failure)
	}
	if a.MultiplierRuleVersion != "glm-1@3" || a.RateCardVersion != "card@1" {
		t.Errorf("rule versions: %+v", a)
	}
}

func TestTraceRoundTrip(t *testing.T) {
	l := openTemp(t)
	ts := time.Now()
	l.BeginRequest(sampleRequest("req-t", ts))
	tr := &domain.TraceRecord{
		PolicyName: "std", PolicyVersion: 3, Timezone: "Asia/Shanghai",
		RequiredCaps: domain.Requirements{Text: true, Stream: true, MinContext: 8192},
		Objective:    "cheap", Tiers: []string{"primary"},
		Candidates: []domain.CandidateRecord{
			{Target: "glm-1", Tier: "primary", Eligible: true, Rank: 1},
			{Target: "glm-2", Tier: "primary", ExclusionReason: "circuit_breaker = open"},
		},
		ExecutionOrder: []domain.ResourceTargetID{"glm-1"},
		Primary:        "glm-1",
		RecoveryEvents: []domain.RecoveryEvent{{AfterAttempt: 1, FailureType: domain.FailConnect, Action: "next_target_same_tier"}},
		Events:         []domain.ExecutionTraceEvent{{At: ts, Category: domain.TraceRecovery, Type: "CONNECTION_REBOUND", Attempt: 1, ProviderID: "provider-a"}},
	}
	l.SetTrace("req-t", tr)
	l.FinishRequest("req-t", nil)

	l2 := reopen(t, l)
	got, err := l2.QueryTrace("req-t")
	if err != nil || got == nil {
		t.Fatalf("trace: %v", err)
	}
	if got.PolicyName != "std" || got.PolicyVersion != 3 || got.Timezone != "Asia/Shanghai" {
		t.Errorf("trace header: %+v", got)
	}
	if len(got.Candidates) != 2 || got.Candidates[0].Rank != 1 ||
		got.Candidates[1].ExclusionReason != "circuit_breaker = open" {
		t.Errorf("candidates: %+v", got.Candidates)
	}
	if len(got.RecoveryEvents) != 1 || got.RecoveryEvents[0].Action != "next_target_same_tier" {
		t.Errorf("recovery events: %+v", got.RecoveryEvents)
	}
	if got.RequiredCaps.MinContext != 8192 {
		t.Errorf("required caps: %+v", got.RequiredCaps)
	}
	if len(got.Events) != 1 || got.Events[0].Type != "CONNECTION_REBOUND" || got.Events[0].ProviderID != "provider-a" {
		t.Errorf("execution trace events: %+v", got.Events)
	}
	// missing trace returns nil, not an error
	if t2, err := l2.QueryTrace("no-such-request"); err != nil || t2 != nil {
		t.Errorf("missing trace: %v %v", t2, err)
	}
}

func TestQueryFiltersAndPaging(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	mk := func(id, key, alias, status string, ts time.Time) {
		r := sampleRequest(id, ts)
		r.ConsumerKey, r.Alias, r.Status = key, alias, domain.RequestStatus(status)
		l.BeginRequest(r)
		l.FinishRequest(id, nil)
	}
	// ts ordering: newest first on query; insert with increasing ts
	mk("r1", "team-x", "glm-flash", "success", base)
	mk("r2", "team-x", "glm-flash", "failed", base.Add(time.Second))
	mk("r3", "team-y", "kimi", "success", base.Add(2*time.Second))

	l2 := reopen(t, l)

	// newest first
	recs, total, err := l2.QueryRequests(domain.LogFilter{})
	if err != nil || total != 3 || len(recs) != 3 {
		t.Fatalf("all: %v total=%d", err, total)
	}
	if recs[0].ID != "r3" || recs[2].ID != "r1" {
		t.Errorf("newest-first violated: %v %v %v", recs[0].ID, recs[1].ID, recs[2].ID)
	}

	// filter by consumer key
	recs, total, _ = mustQuery(t, l2.Ledger, domain.LogFilter{ConsumerKey: "team-y"})
	if total != 1 || len(recs) != 1 || recs[0].ID != "r3" {
		t.Errorf("consumer filter: %v %v", total, recs)
	}

	// filter by status
	recs, total, _ = mustQuery(t, l2.Ledger, domain.LogFilter{Status: domain.ReqFailed})
	if total != 1 || recs[0].ID != "r2" {
		t.Errorf("status filter: %v %v", total, recs)
	}

	// filter by alias + limit/offset
	recs, total, _ = mustQuery(t, l2.Ledger, domain.LogFilter{Alias: "glm-flash"})
	if total != 2 {
		t.Errorf("alias filter total=%d", total)
	}
	recs, _, _ = mustQuery(t, l2.Ledger, domain.LogFilter{Alias: "glm-flash", Limit: 1, Offset: 1})
	if len(recs) != 1 || recs[0].ID != "r1" {
		t.Errorf("paging: %+v", recs)
	}

	// time window [from, to)
	recs, total, _ = mustQuery(t, l2.Ledger, domain.LogFilter{From: base.Add(time.Second), To: base.Add(3 * time.Second)})
	if total != 2 || recs[0].ID != "r3" || recs[1].ID != "r2" {
		t.Errorf("window: total=%d %v", total, recs)
	}

	// target substring
	recs, total, _ = mustQuery(t, l2.Ledger, domain.LogFilter{Target: "glm"})
	if total != 3 {
		t.Errorf("target LIKE: total=%d", total)
	}
}

func TestSummarizeWindow(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	mk := func(id string, status domain.RequestStatus, dur int64, ts time.Time) {
		r := sampleRequest(id, ts)
		r.Status, r.DurationMS = status, dur
		l.BeginRequest(r)
		l.FinishRequest(id, nil)
	}
	mk("s1", domain.ReqSuccess, 100, base)
	mk("s2", domain.ReqSuccess, 300, base.Add(time.Second))
	mk("s3", domain.ReqFailed, 900, base.Add(2*time.Second))
	mk("outside", domain.ReqSuccess, 5000, base.Add(time.Hour)) // outside window

	l2 := reopen(t, l)
	s, err := l2.Summarize(base, base.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests != 3 || s.Succeeded != 2 || s.Failed != 1 || s.Canceled != 0 {
		t.Errorf("counts: %+v", s)
	}
	// 3 requests x (100 in + 50 out)
	if s.InputTokens != 300 || s.OutputTokens != 150 {
		t.Errorf("tokens: %+v", s)
	}
	if abs64(s.EstCashCost-0.75) > 1e-9 {
		t.Errorf("cash: %v", s.EstCashCost)
	}
	if s.UpstreamInputTokens != 390 || s.UpstreamOutputTokens != 165 ||
		abs64(s.ServingCost-0.6) > 1e-9 || abs64(s.RecoveryOverheadCost-0.15) > 1e-9 ||
		abs64(s.TotalCost-0.75) > 1e-9 {
		t.Errorf("V2 usage/cost: %+v", s)
	}
	if !s.TokensObserved || !s.CostObserved {
		t.Errorf("usage observation flags: %+v", s)
	}
	// avg over non-zero durations: (100+300+900)/3
	if abs64(s.AvgLatencyMS-433.333) > 0.01 {
		t.Errorf("avg latency: %v", s.AvgLatencyMS)
	}
	// Nearest-rank P95 selects ceil(0.95 * 3), the largest observation.
	if s.P95LatencyMS != 900 {
		t.Errorf("p95: %v", s.P95LatencyMS)
	}
	if s.MedianLatencyMS == nil || *s.MedianLatencyMS != 300 {
		t.Errorf("median latency: %v", s.MedianLatencyMS)
	}
	if s.AvgRecoveryLatencyMS == nil || abs64(*s.AvgRecoveryLatencyMS-75) > 1e-9 {
		t.Errorf("avg recovery latency: %v", s.AvgRecoveryLatencyMS)
	}
}

func TestSummarizeMarksUnobservedUsageUnknown(t *testing.T) {
	l := openTemp(t)
	base := time.Now().Add(-time.Minute)
	record := sampleRequest("unknown-usage", base)
	record.Usage = domain.Usage{}
	record.UpstreamUsage = domain.Usage{}
	record.EstCashCost, record.QuotaUnits = 0, 0
	record.AllocatedSubCost, record.ShadowCost = 0, 0
	record.ServingCost, record.RecoveryOverheadCost = 0, 0
	l.BeginRequest(record)
	l.FinishRequest(record.ID, nil)

	l2 := reopen(t, l)
	summary, err := l2.Summarize(base.Add(-time.Second), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if summary.TokensObserved || summary.CostObserved {
		t.Fatalf("zero-valued, unobserved usage must remain unknown: %+v", summary)
	}
	providers, err := l2.UsageByDimension("provider_id", base.Add(-time.Second), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if providers[record.ProviderID].TokensObserved || providers[record.ProviderID].CostObserved {
		t.Fatalf("breakdown must preserve unknown usage: %+v", providers[record.ProviderID])
	}
}

func TestV2RequestFiltersUsePublicIdentitiesAndConversationTrace(t *testing.T) {
	l := openTemp(t)
	base := time.Now().Add(-time.Minute)
	first := sampleRequest("request-one", base)
	first.ProviderID = "provider-a"
	first.Outcome = domain.OutcomeRecoveredSuccess
	l.BeginRequest(first)
	l.SetTrace(first.ID, &domain.TraceRecord{ConversationID: "conversation-one"})
	l.FinishRequest(first.ID, nil)
	second := sampleRequest("request-two", base.Add(time.Second))
	second.ConsumerKey = "other-key"
	second.ProviderID = "provider-b"
	second.Outcome = domain.OutcomeDirectSuccess
	l.BeginRequest(second)
	l.SetTrace(second.ID, &domain.TraceRecord{ConversationID: "conversation-two"})
	l.FinishRequest(second.ID, nil)
	legacy := sampleRequest("request-legacy", base.Add(2*time.Second))
	legacy.ProviderID = "provider-c"
	legacy.Outcome = ""
	legacy.Status = domain.ReqSuccess
	l.BeginRequest(legacy)
	l.FinishRequest(legacy.ID, nil)

	l2 := reopen(t, l)
	checks := []struct {
		name   string
		filter domain.LogFilter
		want   string
	}{
		{name: "request id", filter: domain.LogFilter{RequestID: "request-two"}, want: "request-two"},
		{name: "provider", filter: domain.LogFilter{ProviderID: "provider-a"}, want: "request-one"},
		{name: "outcome", filter: domain.LogFilter{RequestID: "request-two", Outcome: domain.OutcomeDirectSuccess}, want: "request-two"},
		{name: "legacy direct outcome", filter: domain.LogFilter{RequestID: "request-legacy", Outcome: domain.OutcomeDirectSuccess}, want: "request-legacy"},
		{name: "conversation id", filter: domain.LogFilter{ConversationID: "conversation-one"}, want: "request-one"},
		{name: "search conversation", filter: domain.LogFilter{Search: "versation-two"}, want: "request-two"},
		{name: "search caller", filter: domain.LogFilter{Search: "other-key"}, want: "request-two"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			rows, total, err := l2.QueryRequests(check.filter)
			if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != check.want {
				t.Fatalf("rows=%+v total=%d err=%v", rows, total, err)
			}
		})
	}
}

func TestV2OutcomeAndTrafficAggregates(t *testing.T) {
	l := openTemp(t)
	base := time.Now().Add(-time.Minute)
	for i, outcome := range []domain.RequestOutcome{
		domain.OutcomeDirectSuccess, domain.OutcomeRecoveredSuccess, domain.OutcomeFailed,
	} {
		record := sampleRequest(fmt.Sprintf("v2-%d", i), base.Add(time.Duration(i)*time.Millisecond))
		record.Outcome = outcome
		record.Status = domain.ReqSuccess
		if outcome == domain.OutcomeFailed {
			record.Status = domain.ReqFailed
		}
		if i == 2 {
			record.ProviderID = "provider-b"
		}
		l.BeginRequest(record)
		l.FinishRequest(record.ID, nil)
	}
	legacy := sampleRequest("v2-legacy-direct", base.Add(4*time.Millisecond))
	legacy.Outcome = ""
	legacy.Status = domain.ReqSuccess
	l.BeginRequest(legacy)
	l.FinishRequest(legacy.ID, nil)
	l2 := reopen(t, l)
	outcomes, err := l2.OutcomeSummary(base.Add(-time.Second), time.Now().Add(time.Second))
	if err != nil || outcomes.DirectSuccess != 2 || outcomes.RecoveredSuccess != 1 || outcomes.Failed != 1 {
		t.Fatalf("outcomes = %+v err=%v", outcomes, err)
	}
	providers, err := l2.TrafficByProvider(base.Add(-time.Second), time.Now().Add(time.Second))
	if err != nil || len(providers) != 2 || providers[0].ID != "provider-a" || providers[0].Requests != 3 {
		t.Fatalf("provider traffic = %+v err=%v", providers, err)
	}
	for _, provider := range providers {
		if provider.Requests != provider.DirectTraffic+provider.RecoveryTraffic+provider.Failed {
			t.Fatalf("provider outcomes must partition requests, including legacy successes: %+v", provider)
		}
	}
	if providers[0].DirectTraffic != 2 || providers[0].RecoveryTraffic != 1 {
		t.Fatalf("provider legacy direct success was not classified: %+v", providers[0])
	}
	if providers[0].AvgTTFTMS == nil || *providers[0].AvgTTFTMS != 120 ||
		providers[0].AvgDurationMS == nil || *providers[0].AvgDurationMS != 800 {
		t.Fatalf("provider response timing = %+v", providers[0])
	}
	wantLastSuccess := base.Add(4 * time.Millisecond)
	if providers[0].LastSuccessAt == nil || providers[0].LastSuccessAt.UnixMilli() != wantLastSuccess.UnixMilli() {
		t.Fatalf("provider last success = %v want %v", providers[0].LastSuccessAt, wantLastSuccess)
	}
	models, err := l2.TrafficByLogicalModel(base.Add(-time.Second), time.Now().Add(time.Second))
	if err != nil || len(models) != 1 || models[0].AvgTTFTMS == nil || models[0].AvgDurationMS == nil {
		t.Fatalf("model response timing = %+v err=%v", models, err)
	}
	routes, err := l2.TrafficByRoute(base.Add(-time.Second), time.Now().Add(time.Second))
	if err != nil || len(routes) != 2 {
		t.Fatalf("route traffic = %+v err=%v", routes, err)
	}
	var providerARoute *RouteTrafficSummary
	for index := range routes {
		if routes[index].ProviderID == "provider-a" {
			providerARoute = &routes[index]
			break
		}
	}
	if providerARoute == nil || providerARoute.DirectTraffic != 2 || providerARoute.RecoveryTraffic != 1 {
		t.Fatalf("route legacy direct success was not classified: %+v", routes)
	}
}

func TestTrafficTrendReturnsGapFilledRouteSeries(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	add := func(id, model, provider, upstream string, status domain.RequestStatus, offset time.Duration) {
		record := sampleRequest(id, base.Add(offset))
		record.LogicalModelID = model
		record.ProviderID = provider
		record.UpstreamModel = upstream
		if model == "" {
			record.Alias = "legacy-hermes"
			record.ModelSelected = "legacy-upstream-model"
		}
		record.Status = status
		switch status {
		case domain.ReqSuccess:
			record.Outcome = domain.OutcomeDirectSuccess
		case domain.ReqCanceled:
			record.Outcome = domain.OutcomeFailed
		default:
			record.Outcome = domain.OutcomeFailed
		}
		l.BeginRequest(record)
		l.FinishRequest(record.ID, nil)
	}
	add("trend-a-1", "model-a", "provider-a", "upstream-a", domain.ReqSuccess, 10*time.Second)
	add("trend-a-2", "model-a", "provider-a", "upstream-a", domain.ReqFailed, 70*time.Second)
	add("trend-b-1", "model-a", "provider-b", "upstream-b", domain.ReqSuccess, 130*time.Second)
	// A pre-selection cancellation belongs in the logical-model denominator,
	// but must not be attributed to a provider that never accepted it.
	add("trend-unassigned", "model-b", "", "", domain.ReqCanceled, 190*time.Second)
	// Legacy Hermes rows predate logical_model_id/upstream_model but still have
	// a client alias and selected model; they must remain visible in trends.
	add("trend-legacy", "", "provider-c", "", domain.ReqSuccess, 200*time.Second)

	l2 := reopen(t, l)
	series, err := l2.TrafficTrend(base, base.Add(4*time.Minute), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 4 {
		t.Fatalf("route trend series=%+v", series)
	}
	find := func(model, provider, upstream string) *RouteTrafficTrend {
		for index := range series {
			if series[index].LogicalModelID == model && series[index].ProviderID == provider && series[index].UpstreamModel == upstream {
				return &series[index]
			}
		}
		return nil
	}
	providerA := find("model-a", "provider-a", "upstream-a")
	if providerA == nil || providerA.Requests != 2 || providerA.Succeeded != 1 || providerA.Failed != 1 || len(providerA.Points) != 4 {
		t.Fatalf("provider-a trend=%+v", providerA)
	}
	if providerA.Points[0].Requests != 1 || providerA.Points[1].Requests != 1 || providerA.Points[2].Requests != 0 || providerA.Points[3].Requests != 0 {
		t.Fatalf("provider-a buckets=%+v", providerA.Points)
	}
	unassigned := find("model-b", "", "glm-4-flash")
	if unassigned == nil || unassigned.Requests != 1 || unassigned.Failed != 1 || unassigned.Points[3].Failed != 1 {
		t.Fatalf("unassigned trend=%+v", unassigned)
	}
	legacy := find("legacy-hermes", "provider-c", "legacy-upstream-model")
	if legacy == nil || legacy.Requests != 1 || legacy.Succeeded != 1 || legacy.Points[3].Succeeded != 1 {
		t.Fatalf("legacy Hermes trend=%+v", legacy)
	}
}

func TestCacheHitOverviewNormalizesProtocolsAndDimensions(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 9, 3, 1, 0, 0, 0, time.UTC)
	add := func(id, key, model string, protocol domain.Protocol, usage domain.Usage, offset time.Duration) {
		record := sampleRequest(id, base.Add(offset))
		record.ConsumerKey = key
		record.LogicalModelID = model
		if model == "" {
			record.Alias = "legacy-hermes"
			record.ModelSelected = "legacy-upstream-model"
		}
		// Deliberately keep the client protocol OpenAI-shaped: normalization
		// must follow the actual final attempt protocol after translation.
		record.Protocol = domain.ProtocolOpenAIResponses
		record.Attempts = 1
		record.Usage = usage
		l.BeginRequest(record)
		l.AddAttempt(domain.AttemptRecord{
			RequestID: id, Seq: 1, Ts: record.Ts, TargetID: "target-" + domain.ResourceTargetID(id),
			Protocol: protocol, Status: domain.ReqSuccess, Usage: usage,
		})
		l.FinishRequest(id, nil)
	}
	add("cache-openai", "key-a", "model-a", domain.ProtocolOpenAIResponses,
		domain.Usage{InputTokens: 1_000, CacheReadTokens: 800, CacheSubsetOfInput: true}, 10*time.Second)
	// A historical OpenAI-compatible adapter reported cached tokens beside,
	// rather than inside, input tokens. Keeping it in the same dimension as the
	// modern row proves normalization happens before aggregation.
	add("cache-openai-exclusive", "key-a", "model-a", domain.ProtocolOpenAIResponses,
		domain.Usage{InputTokens: 100, CacheReadTokens: 200}, 15*time.Second)
	add("cache-anthropic", "key-a", "model-b", domain.ProtocolAnthropic,
		domain.Usage{InputTokens: 100, CacheReadTokens: 800, CacheWriteTokens: 100}, 20*time.Second)
	add("cache-miss", "key-b", "model-a", domain.ProtocolOpenAIChat,
		domain.Usage{InputTokens: 400}, 30*time.Second)
	add("cache-unobserved", "key-c", "model-c", domain.ProtocolOpenAIChat,
		domain.Usage{}, 40*time.Second)
	add("cache-legacy-hermes", "key-d", "", domain.ProtocolOpenAIChat,
		domain.Usage{InputTokens: 200, CacheReadTokens: 100, CacheSubsetOfInput: true}, 50*time.Second)

	l2 := reopen(t, l)
	got, err := l2.CacheHitOverview(base, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.Overall.Requests != 6 || got.Overall.ObservedRequests != 5 ||
		got.Overall.CacheEligibleInputTokens != 2_900 || got.Overall.CacheReadTokens != 1_900 ||
		got.Overall.CacheWriteTokens != 100 || got.Overall.HitRatePct == nil ||
		*got.Overall.HitRatePct < 65.51 || *got.Overall.HitRatePct > 65.52 {
		t.Fatalf("overall cache hit=%+v", got.Overall)
	}
	find := func(items []CacheHitDimension, id string) *CacheHitDimension {
		for index := range items {
			if items[index].ID == id {
				return &items[index]
			}
		}
		return nil
	}
	keyA, keyB, keyC := find(got.APIKeys, "key-a"), find(got.APIKeys, "key-b"), find(got.APIKeys, "key-c")
	if keyA == nil || keyA.HitRatePct == nil || *keyA.HitRatePct < 78.26 || *keyA.HitRatePct > 78.27 || keyA.CacheEligibleInputTokens != 2_300 {
		t.Fatalf("key-a cache hit=%+v", keyA)
	}
	if keyB == nil || keyB.HitRatePct == nil || *keyB.HitRatePct != 0 {
		t.Fatalf("key-b cache hit=%+v", keyB)
	}
	if keyC == nil || keyC.HitRatePct != nil || keyC.ObservedRequests != 0 {
		t.Fatalf("key-c unobserved cache hit=%+v", keyC)
	}
	modelA, modelB := find(got.LogicalModels, "model-a"), find(got.LogicalModels, "model-b")
	if modelA == nil || modelA.HitRatePct == nil || *modelA.HitRatePct < 58.82 || *modelA.HitRatePct > 58.83 {
		t.Fatalf("model-a cache hit=%+v", modelA)
	}
	if modelB == nil || modelB.HitRatePct == nil || *modelB.HitRatePct != 80 {
		t.Fatalf("model-b cache hit=%+v", modelB)
	}
	legacy := find(got.LogicalModels, "legacy-hermes")
	if legacy == nil || legacy.HitRatePct == nil || *legacy.HitRatePct != 50 {
		t.Fatalf("legacy Hermes model fallback=%+v", legacy)
	}
}

func TestByTargetRollup(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	mk := func(id, target string, status domain.RequestStatus, ts time.Time) {
		r := sampleRequest(id, ts)
		r.SelectedTarget = domain.ResourceTargetID(target)
		r.Status = status
		l.BeginRequest(r)
		l.FinishRequest(id, nil)
	}
	for i := 0; i < 3; i++ {
		mk("a"+string(rune('0'+i)), "glm-1", domain.ReqSuccess, base.Add(time.Duration(i)*time.Second))
	}
	mk("b0", "glm-2", domain.ReqFailed, base)

	l2 := reopen(t, l)
	rows, err := l2.ByTarget(base, base.Add(time.Hour))
	if err != nil || len(rows) != 2 {
		t.Fatalf("by target: %v n=%d", err, len(rows))
	}
	// ordered by request count desc
	if rows[0].Target != "glm-1" || rows[0].Requests != 3 || rows[0].Failures != 0 {
		t.Errorf("glm-1 row: %+v", rows[0])
	}
	if rows[1].Target != "glm-2" || rows[1].Requests != 1 || rows[1].Failures != 1 {
		t.Errorf("glm-2 row: %+v", rows[1])
	}
	// tokens: 3x150 + 1x150
	if rows[0].Tokens != 450 || rows[1].Tokens != 150 {
		t.Errorf("tokens: %+v %+v", rows[0], rows[1])
	}
}

func TestUnknownFinishIsNoop(t *testing.T) {
	l := openTemp(t)
	l.FinishRequest("ghost", nil) // must not panic
	l.AddAttempt(domain.AttemptRecord{RequestID: "ghost2", Seq: 1})
}

func TestPurgeKeepsFreshDeletesStale(t *testing.T) {
	l := openTemp(t)
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, ts time.Time, withTrace bool) {
		l.BeginRequest(sampleRequest(id, ts))
		if withTrace {
			l.SetTrace(id, &domain.TraceRecord{PolicyName: "std"})
		}
		l.AddAttempt(domain.AttemptRecord{RequestID: id, Seq: 1, Ts: ts,
			TargetID: "glm-1", Status: domain.ReqSuccess})
		l.FinishRequest(id, nil)
	}
	mk("old-1", cutoff.Add(-24*time.Hour), true)
	mk("old-2", cutoff.Add(-time.Hour), false)
	mk("fresh-1", cutoff.Add(time.Hour), true)
	mk("fresh-2", cutoff.Add(2*time.Hour), true) // trace-bearing fresh row too

	// Ledger ingestion is intentionally asynchronous. Close/reopen is the
	// established test barrier: retention should be asserted against persisted
	// rows, not race the non-blocking data-plane queue.
	l = reopen(t, l)
	removed, err := l.PurgeOlderThan(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	// fresh rows survive intact, including children
	recs, total, err := l.QueryRequests(domain.LogFilter{})
	if err != nil || total != 2 {
		t.Fatalf("post-purge query: %v total=%d", err, total)
	}
	for _, r := range recs {
		if r.ID == "old-1" || r.ID == "old-2" {
			t.Errorf("stale row survived purge: %s", r.ID)
		}
		tr, err := l.QueryTrace(r.ID)
		if err != nil || tr == nil {
			t.Errorf("trace of fresh row lost: %s %v", r.ID, err)
		}
		atts, err := l.QueryAttempts(r.ID)
		if err != nil || len(atts) != 1 {
			t.Errorf("attempts of fresh row lost: %s %v", r.ID, err)
		}
	}
}

// TestBatchDrainAndPoisonIsolation covers task #14: a burst larger than the
// queue drains in batches, every job persists exactly once, and one poison
// job neither drops its batch siblings nor wedges the writer.
func TestBatchDrainAndPoisonIsolation(t *testing.T) {
	dir := t.TempDir()
	// Drop-on-full is by design (G5), so an exactness assertion needs a queue
	// sized for the whole burst; batching is still exercised because the
	// writer drains adjacent backlog in transactions.
	l0, err := Open(dir, 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 300
	ts := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("bulk-%03d", i)
			l0.BeginRequest(sampleRequest(id, ts))
			l0.FinishRequest(id, nil)
		}(i)
	}
	wg.Wait()
	tl := &tledger{Ledger: l0, dir: dir}
	l := reopen(t, tl)
	recs, total, err := l.QueryRequests(domain.LogFilter{Limit: n})
	if err != nil || total != n || len(recs) != n {
		t.Fatalf("after bulk: %v total=%d n=%d (want exactly %d)", err, total, len(recs), n)
	}

	// Poison isolation: a job that always fails must roll back only itself.
	poison := func(db execer) error { return fmt.Errorf("synthetic poison") }
	ok := func(db execer) error {
		_, err := db.Exec(`SELECT 1`)
		return err
	} // no-op success
	l.enqueue(poison) // first of batch
	for i := 0; i < 5; i++ {
		l.enqueue(ok) // siblings in same batch
	}
	l.enqueue(poison)
	for i := 0; i < 5; i++ {
		l.enqueue(ok)
	}
	time.Sleep(100 * time.Millisecond) // let the writer churn through both batches

	l2 := reopen(t, l) // Close flushes; writer still alive afterwards
	_, total, err = l2.QueryRequests(domain.LogFilter{})
	if err != nil || total != n {
		t.Fatalf("poison run changed request count: %v total=%d want %d", err, total, n)
	}
}

// TestRollupMatchesDetail verifies the hourly rollup aggregates agree with a
// direct detail-table scan over the same window (task #13 invariant).
func TestRollupMatchesDetail(t *testing.T) {
	l := openTemp(t)
	base := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC) // mid-hour start
	end := base.Add(3 * time.Hour).Add(20 * time.Minute)   // ragged tail
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("ru-%02d", i)
		// 7-min spacing keeps all 25 inside the 3h20m window (24*7=168min).
		r := sampleRequest(id, base.Add(time.Duration(i)*7*time.Minute))
		if i%4 == 0 {
			r.Status = domain.ReqFailed
		}
		l.BeginRequest(r)
		l.FinishRequest(id, nil)
	}
	l2 := reopen(t, l)

	s, err := l2.Summarize(base, end)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l2.ByTarget(base, end)
	if err != nil {
		t.Fatal(err)
	}
	var byTargetReq int64
	for _, row := range rows {
		byTargetReq += row.Requests
	}
	if s.Requests != 25 {
		t.Errorf("summary requests = %d, want 25", s.Requests)
	}
	if byTargetReq != 25 {
		t.Errorf("by-target requests sum = %d, want 25 (rollup/detail mismatch)", byTargetReq)
	}
	wantFail := int64(len([...]int{0, 4, 8, 12, 16, 20, 24}))
	if s.Failed != wantFail {
		t.Errorf("failed = %d, want %d", s.Failed, wantFail)
	}
}

// TestConcurrentWritersAndReaders exercises interleaved ingestion and queries
// on the split read/write pools; run under -race it validates the WAL model.
func TestConcurrentWritersAndReaders(t *testing.T) {
	dir := t.TempDir()
	led, err := Open(dir, 4096, 0) // queue fits the 8x40 burst: no design drops
	if err != nil {
		t.Fatal(err)
	}
	l := &tledger{Ledger: led, dir: dir}
	t.Cleanup(l.close)
	base := time.Now().Add(-time.Hour)
	stop := make(chan struct{})
	var readers, writers sync.WaitGroup

	readers.Add(1)
	go func() { // concurrent reader hammering dashboard paths
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = l.Summarize(base, time.Now())
				_, _, _ = l.QueryRequests(domain.LogFilter{Limit: 10})
				time.Sleep(time.Millisecond)
			}
		}
	}()
	for w := 0; w < 8; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 40; i++ {
				id := fmt.Sprintf("cc-%d-%d", w, i)
				l.BeginRequest(sampleRequest(id, base.Add(time.Duration(i)*time.Second)))
				l.FinishRequest(id, nil)
			}
		}(w)
	}
	writers.Wait() // ingestion finished; now let the reader exit
	close(stop)
	readers.Wait()
	l.close() // flush the async writer before asserting exact counts
	l2 := reopen(t, l)

	total := 0
	for _, rec := range mustQueryAll(t, l2.Ledger) {
		_ = rec
		total++
	}
	if total != 320 {
		t.Errorf("total = %d, want 320", total)
	}
}

// Close may overlap the tail of a streaming request during graceful shutdown.
// Late accounting calls must be harmless instead of racing with close(ch) or
// panicking with "send on closed channel".
func TestWritesAfterCloseDoNotPanic(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	l.AddAttempt(domain.AttemptRecord{
		RequestID: "late", Seq: 1, Ts: now, TargetID: "t1",
	})
	l.BeginRequest(sampleRequest("late", now))
	l.FinishRequest("late", nil)
}

// helpers

func mustQueryAll(t *testing.T, l *Ledger) []domain.RequestRecord {
	t.Helper()
	recs, _, err := l.QueryRequests(domain.LogFilter{Limit: 1000})
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	return recs
}

func mustQuery(t *testing.T, l *Ledger, f domain.LogFilter) ([]domain.RequestRecord, int, error) {
	t.Helper()
	return l.QueryRequests(f)
}

func abs64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
