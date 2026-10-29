package ledger

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestUsageAndResponseMetadataRoundTrip(t *testing.T) {
	l := openTemp(t)
	now := time.Now()
	for _, subset := range []bool{false, true} {
		r := sampleRequest(fmt.Sprintf("cache-%v", subset), now)
		r.Attempts = 1
		r.ServiceTier, r.ResponseModel, r.ResponseServiceTier = "fast", "returned-model", "priority"
		r.ServiceTierEvidence = &domain.ServiceTierEvidence{WireServiceTier: "priority", ResponseTierAuthority: domain.TierInformational, Source: "codex", PriorityConfirmation: "unconfirmed"}
		r.Usage = domain.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 800, CacheWriteTokens: 150, CacheSubsetOfInput: subset}
		r.UpstreamUsage = r.Usage
		l.BeginRequest(r)
		l.AddAttempt(domain.AttemptRecord{RequestID: r.ID, Seq: 1, Ts: now, Protocol: domain.ProtocolOpenAIResponses, Model: r.UpstreamModel, ServiceTier: r.ServiceTier, ResponseModel: r.ResponseModel, ResponseServiceTier: r.ResponseServiceTier, ServiceTierEvidence: r.ServiceTierEvidence, Usage: r.Usage})
		l.FinishRequest(r.ID, nil)
	}
	l2 := reopen(t, l)
	rows, _, err := l2.QueryRequests(domain.LogFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	for _, r := range rows {
		subset := r.ID == "cache-true"
		if r.ServiceTier != "fast" || r.ResponseModel != "returned-model" || r.ResponseServiceTier != "priority" || r.UpstreamModel != "glm-4-flash" || r.Usage.CacheSubsetOfInput != subset || r.UpstreamUsage.CacheSubsetOfInput != subset {
			t.Fatalf("request=%+v", r)
		}
		attempts, err := l2.QueryAttempts(r.ID)
		if err != nil || len(attempts) != 1 {
			t.Fatalf("attempts=%v err=%v", attempts, err)
		}
		a := attempts[0]
		if r.ServiceTierEvidence == nil || a.ServiceTierEvidence == nil || *a.ServiceTierEvidence != *r.ServiceTierEvidence || r.ServiceTierEvidence.PriorityConfirmation != "unconfirmed" || r.ServiceTierEvidence.WireServiceTier != "priority" || r.ServiceTierEvidence.ResponseTierAuthority != domain.TierInformational {
			t.Fatalf("tier evidence lost: request=%+v attempt=%+v", r.ServiceTierEvidence, a.ServiceTierEvidence)
		}
		if a.Usage != r.Usage || a.ServiceTier != r.ServiceTier || a.ResponseModel != r.ResponseModel || a.ResponseServiceTier != r.ResponseServiceTier || a.Model != r.UpstreamModel {
			t.Fatalf("attempt=%+v", a)
		}
	}
	overview, err := l2.CacheHitOverview(now.Add(-time.Second), now.Add(time.Minute))
	if err != nil || overview.Overall.CacheEligibleInputTokens != 2950 {
		t.Fatalf("independent OpenAI usage guessed as subset: overview=%+v err=%v", overview, err)
	}
}

func TestMetadataMigrationRetainsLegacyCacheInterpretation(t *testing.T) {
	l := openTemp(t)
	now := time.Now()
	for _, tc := range []struct {
		id               string
		client, upstream domain.Protocol
		read             int64
	}{
		{"openai", domain.ProtocolAnthropic, domain.ProtocolOpenAIResponses, 400},
		{"anthropic", domain.ProtocolOpenAIChat, domain.ProtocolAnthropic, 400},
		{"empty-upstream", domain.ProtocolAnthropic, "", 100},
	} {
		r := sampleRequest(tc.id, now)
		r.Protocol = tc.client
		r.Attempts = 1
		r.Usage = domain.Usage{InputTokens: 500, CacheReadTokens: tc.read}
		r.UpstreamUsage = r.Usage
		l.BeginRequest(r)
		l.AddAttempt(domain.AttemptRecord{RequestID: r.ID, Seq: 1, Ts: now, Protocol: tc.upstream, Usage: r.Usage})
		l.FinishRequest(r.ID, nil)
	}
	l.close()
	db, err := sql.Open("sqlite", filepath.Join(l.dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the schema from before metadata and cache-dialect persistence.
	for table, columns := range map[string][]string{
		"logical_requests": {"service_tier", "response_model", "response_service_tier", "service_tier_evidence", "cache_subset_of_input", "upstream_cache_subset_of_input"},
		"attempts":         {"service_tier", "response_model", "response_service_tier", "service_tier_evidence", "cache_subset_of_input"},
	} {
		for _, column := range columns {
			if _, err := db.Exec("ALTER TABLE " + table + " DROP COLUMN " + column); err != nil {
				db.Close()
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	l2 := reopen(t, l)
	rows, _, err := l2.QueryRequests(domain.LogFilter{})
	if err != nil || len(rows) != 3 {
		t.Fatalf("migration rows=%v err=%v", rows, err)
	}
	for _, r := range rows {
		wantSubset := r.ID == "openai"
		if r.ResponseModel != "" || r.ServiceTier != "" || r.ResponseServiceTier != "" || r.Usage.CacheSubsetOfInput != wantSubset || r.UpstreamUsage.CacheSubsetOfInput != wantSubset {
			t.Fatalf("legacy request=%+v", r)
		}
		attempts, err := l2.QueryAttempts(r.ID)
		if err != nil || len(attempts) != 1 {
			t.Fatalf("legacy attempts=%v err=%v", attempts, err)
		}
		if attempts[0].Usage.CacheSubsetOfInput != wantSubset || attempts[0].ResponseModel != "" {
			t.Fatalf("legacy attempt=%+v", attempts[0])
		}
		if r.ServiceTierEvidence != nil || attempts[0].ServiceTierEvidence != nil {
			t.Fatal("migration invented historical tier evidence")
		}
	}
	overview, err := l2.CacheHitOverview(now.Add(-time.Second), now.Add(time.Minute))
	if err != nil || overview.Overall.CacheEligibleInputTokens != 2000 {
		t.Fatalf("legacy cache overview=%+v err=%v", overview, err)
	}
}
