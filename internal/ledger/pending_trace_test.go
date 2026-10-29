package ledger

import (
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"ferry/internal/domain"
)

func pendingTraceFixture(version int64) *domain.TraceRecord {
	label := fmt.Sprintf("phase-%d", version)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(version) * time.Millisecond)
	return &domain.TraceRecord{
		PolicyName: label, PolicyVersion: version, PolicyHash: label, PolicyIndexHash: label,
		PolicyCompileCacheHit: true, PolicySceneCacheHit: true, Timezone: "UTC",
		Objective: label, Tiers: []string{label},
		Candidates: []domain.CandidateRecord{
			{Target: domain.ResourceTargetID(label), Tier: label, Eligible: true, Rank: 1, Scores: map[string]float64{"cost": float64(version)}},
			{Target: "other", Scores: map[string]float64{"cost": float64(version + 1)}},
		},
		ExecutionOrder: []domain.ResourceTargetID{domain.ResourceTargetID(label)},
		Primary:        domain.ResourceTargetID(label),
		RecoveryEvents: []domain.RecoveryEvent{{AfterAttempt: 1, Action: label}},
		LogicalModelID: label, OverrideID: label,
		AffinityEvidence: []domain.AffinityEvidence{{Source: label, Scope: domain.AffinityConversation, KeyHash: label, ObservedAt: at}},
		AffinityBinding: &domain.AffinityBinding{
			Scope: domain.AffinityConversation, KeyHash: label, LogicalModelID: label,
			RouteFingerprint: label, ProviderID: label, UpstreamModel: label, ConnectionID: label,
			CreatedAt: at, LastSeenAt: at, ExpiresAt: at.Add(time.Hour), Status: label,
		},
		AffinityReused: true, ConversationID: label, ContinuityMode: domain.ContinuityNative,
		Events: []domain.ExecutionTraceEvent{{At: at, Category: domain.TraceRoute, Type: label, Message: label}},
	}
}

// Change every mutable descendant as well as the root to exercise ownership
// through the public methods, independently of the production clone helper.
func mutatePendingTrace(tr *domain.TraceRecord) {
	tr.PolicyVersion = -1
	tr.Tiers[0] = "changed"
	for i := range tr.Candidates {
		tr.Candidates[i].Target = "changed"
		tr.Candidates[i].Scores["cost"] = -1
		tr.Candidates[i].Scores["added"] = -1
	}
	tr.ExecutionOrder[0] = "changed"
	tr.RecoveryEvents[0].Action = "changed"
	tr.AffinityEvidence[0].Source = "changed"
	tr.AffinityBinding.ProviderID = "changed"
	tr.Events[0].Type = "changed"
}

func assertPendingTrace(t *testing.T, l *Ledger, id string, want *domain.TraceRecord) *domain.TraceRecord {
	t.Helper()
	got, err := l.QueryTrace(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace = %+v, want %+v", got, want)
	}
	return got
}

func TestPendingTraceLiveUpdates(t *testing.T) {
	l := openTemp(t)
	const id = "pending-trace"
	assertPendingTrace(t, l.Ledger, id, nil)
	l.BeginRequest(domain.RequestRecord{ID: id, Ts: time.Now(), Status: "received"})
	assertPendingTrace(t, l.Ledger, id, nil)
	for version := int64(1); version <= 3; version++ {
		l.SetTrace(id, pendingTraceFixture(version))
		assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(version))
	}
	// Pending phase publication must be queryable without writing traces to
	// SQLite or waiting for the async writer.
	var count int
	if err := l.read.QueryRow("SELECT count(*) FROM decision_traces WHERE request_id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pending trace persisted: count=%d err=%v", count, err)
	}
	l.SetTrace(id, nil)
	assertPendingTrace(t, l.Ledger, id, nil)
	for _, empty := range []*domain.TraceRecord{
		{},
		{Tiers: []string{}, Candidates: []domain.CandidateRecord{{Scores: nil}, {Scores: map[string]float64{}}}, Events: []domain.ExecutionTraceEvent{}},
	} {
		l.SetTrace(id, empty)
		assertPendingTrace(t, l.Ledger, id, empty)
	}
}

func TestPendingTraceSnapshotIsolation(t *testing.T) {
	l := openTemp(t)
	const id = "isolated-trace"
	l.BeginRequest(domain.RequestRecord{ID: id, Ts: time.Now(), Status: "received"})
	source := pendingTraceFixture(1)
	l.SetTrace(id, source)
	mutatePendingTrace(source)
	first := assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(1))
	mutatePendingTrace(first)
	previous := assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(1))
	l.SetTrace(id, pendingTraceFixture(2))
	assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(2))
	if !reflect.DeepEqual(previous, pendingTraceFixture(1)) {
		t.Fatal("publishing a new trace mutated an earlier query result")
	}
}

func TestPendingTraceDeliveryAndPersistence(t *testing.T) {
	l := openTemp(t)
	const id = "delivery-trace"
	l.BeginIngressRequest(domain.RequestRecord{ID: id, Ts: time.Now(), Status: "received"})
	l.SetTrace(id, pendingTraceFixture(1))
	l.FinishRequest(id, func(r *domain.RequestRecord) { r.Status = domain.ReqSuccess })
	assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(1))
	final := pendingTraceFixture(2)
	l.SetTrace(id, final)
	returned := assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(2))
	l.EndIngressRequest(id)
	// The writer may still be serializing the final snapshot here. Neither
	// caller-owned nor query-owned mutations may change its persisted data.
	mutatePendingTrace(final)
	mutatePendingTrace(returned)
	flushLiveTest(t, l.Ledger)
	stored := assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(2))
	mutatePendingTrace(stored)
	assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(2))
	l = reopen(t, l)
	assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(2))
}

func TestPendingTraceConcurrentReadersAndPublishers(t *testing.T) {
	l := openTemp(t)
	const id = "concurrent-trace"
	l.BeginRequest(domain.RequestRecord{ID: id, Ts: time.Now(), Status: "received"})
	l.SetTrace(id, pendingTraceFixture(1))
	const publishers, readers, iterations = 4, 4, 200
	start := make(chan struct{})
	errors := make(chan error, readers)
	var workers sync.WaitGroup
	for publisher := range publishers {
		workers.Go(func() {
			<-start
			for i := range iterations {
				version := int64(publisher*iterations + i + 2)
				source := pendingTraceFixture(version)
				l.SetTrace(id, source)
				runtime.Gosched()
				mutatePendingTrace(source)
			}
		})
	}
	for range readers {
		workers.Go(func() {
			<-start
			for range iterations {
				tr, err := l.QueryTrace(id)
				if err != nil || tr == nil {
					errors <- fmt.Errorf("pending trace missing: trace=%+v err=%v", tr, err)
					return
				}
				runtime.Gosched()
				if tr.PolicyVersion < 1 || !reflect.DeepEqual(tr, pendingTraceFixture(tr.PolicyVersion)) {
					errors <- fmt.Errorf("inconsistent snapshot: %+v", tr)
					return
				}
				mutatePendingTrace(tr)
			}
		})
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	l.SetTrace(id, pendingTraceFixture(9999))
	l.FinishRequest(id, func(r *domain.RequestRecord) { r.Status = domain.ReqSuccess })
	flushLiveTest(t, l.Ledger)
	assertPendingTrace(t, l.Ledger, id, pendingTraceFixture(9999))
}
