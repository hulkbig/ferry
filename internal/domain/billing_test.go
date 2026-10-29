package domain

import (
	"math"
	"testing"
	"time"
)

func TestRateCardCostOpenAISubsetSemantics(t *testing.T) {
	// OpenAI dialect: prompt_tokens already includes cached tokens. The cached
	// portion must be priced once, at the cache rate.
	card := &RateCard{InputPerM: 1, OutputPerM: 2, CacheReadPerM: 0.1}
	u := Usage{InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 800_000, CacheSubsetOfInput: true}
	got := card.Cost(u)
	want := 0.2*1 + 0.5*2 + 0.8*0.1 // 200k uncached input + 500k output + 800k cache read
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("subset cost = %v, want %v", got, want)
	}
}

func TestRateCardCostAnthropicIndependentSemantics(t *testing.T) {
	// Anthropic dialect: input_tokens excludes cache; each bucket priced once.
	card := &RateCard{InputPerM: 3, OutputPerM: 15, CacheReadPerM: 0.3, CacheWritePerM: 3.75}
	u := Usage{InputTokens: 100_000, OutputTokens: 50_000, CacheReadTokens: 900_000, CacheWriteTokens: 200_000}
	got := card.Cost(u)
	want := 0.1*3 + 0.05*15 + 0.9*0.3 + 0.2*3.75
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("independent cost = %v, want %v", got, want)
	}
}

func TestUsageUncachedInputClamps(t *testing.T) {
	u := Usage{InputTokens: 100, CacheReadTokens: 150, CacheSubsetOfInput: true}
	if got := u.UncachedInput(); got != 0 {
		t.Fatalf("clamped uncached = %d, want 0", got)
	}
}

func TestBillableTokens(t *testing.T) {
	subset := Usage{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 800, CacheWriteTokens: 100, CacheSubsetOfInput: true}
	if got := subset.BillableTokens(); got != 1000+100 {
		t.Fatalf("subset billable = %d, want 1100 (no double count)", got)
	}
	independent := Usage{InputTokens: 200, OutputTokens: 100, CacheReadTokens: 800}
	if got := independent.BillableTokens(); got != 1100 {
		t.Fatalf("independent billable = %d, want 1100", got)
	}
}

func TestQuotaCostCredits(t *testing.T) {
	// Codex: 1 credit = $0.04, so $1.00 of cash cost is 25 credits.
	got := QuotaCost(QuotaUnitCredits, Usage{}, 1, true, 1.0)
	if math.Abs(got-25) > 1e-9 {
		t.Fatalf("credits = %v, want 25", got)
	}
}

func TestQuotaCostPromptsFailureFree(t *testing.T) {
	if got := QuotaCost(QuotaUnitPrompts, Usage{}, 1, false, 0); got != 0 {
		t.Fatalf("non-request prompts = %v, want 0", got)
	}
}

func TestRefreshSpecRolling(t *testing.T) {
	spec := RefreshSpec{Period: "5h", Rolling: true}
	d, ok := spec.IsRolling()
	if !ok || d != 5*time.Hour {
		t.Fatalf("rolling = %v %v, want 5h true", d, ok)
	}
	bucket := QuotaBucket{ID: "b", Unit: QuotaUnitPrompts, LimitPerCycle: 10, Refresh: spec}
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	reset := bucket.NextRefresh(now)
	if !reset.Equal(now.Add(5 * time.Hour)) {
		t.Fatalf("rolling reset = %v, want now+5h", reset)
	}
	// A later observation slides the window from that moment, not the clock.
	later := now.Add(3 * time.Hour)
	if got := bucket.NextRefresh(later); !got.Equal(later.Add(5 * time.Hour)) {
		t.Fatalf("sliding reset = %v, want later+5h", got)
	}
	// Non-rolling duration buckets keep their fixed-cadence semantics.
	fixed := QuotaBucket{Refresh: RefreshSpec{Period: "24h"}}
	if _, rolling := fixed.Refresh.IsRolling(); rolling {
		t.Fatal("non-rolling bucket reported rolling")
	}
}

func TestPlanQuotaRefsStacked(t *testing.T) {
	p := &Plan{
		Quota:  &QuotaRef{BucketID: "weekly"},
		Quotas: []QuotaRef{{BucketID: "five-hour"}, {BucketID: "weekly"}},
	}
	refs := p.QuotaRefs()
	if len(refs) != 2 || refs[0].BucketID != "weekly" || refs[1].BucketID != "five-hour" {
		t.Fatalf("refs = %+v, want [weekly five-hour]", refs)
	}
}

func TestCompiledTargetQuotaBucketsFallback(t *testing.T) {
	single := &CompiledTarget{Bucket: &QuotaBucket{ID: "only"}}
	if got := single.QuotaBuckets(); len(got) != 1 || got[0].ID != "only" {
		t.Fatalf("fallback = %+v", got)
	}
	stacked := &CompiledTarget{Bucket: &QuotaBucket{ID: "a"}, Buckets: []*QuotaBucket{{ID: "a"}, {ID: "b"}}}
	if got := stacked.QuotaBuckets(); len(got) != 2 {
		t.Fatalf("stacked = %+v", got)
	}
}
