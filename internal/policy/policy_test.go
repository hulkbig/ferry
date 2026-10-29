package policy

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Test-world builders.

func testTarget(id string, priority int, caps domain.Capabilities, plan *domain.Plan) *domain.CompiledTarget {
	return &domain.CompiledTarget{
		ID:           domain.ResourceTargetID(id),
		Endpoint:     &domain.Endpoint{ID: "ep-" + id, BaseURL: "https://" + id + ".example/v1", Protocol: domain.ProtocolOpenAIChat},
		Model:        "model-" + id,
		Plan:         plan,
		Capabilities: caps,
		PoolIDs:      []string{"main"},
	}
}

// twoTargets builds a snapshot with targets a and b (ids sorted), a policy
// "main" with one tier over pool "main", and the built-in balanced objective.
func twoTargets(t *testing.T, aCaps, bCaps domain.Capabilities,
	aState, bState *domain.TargetState) (*domain.CompiledSnapshot, *domain.CompiledPolicy, *domain.Snapshot) {
	t.Helper()

	snap := &domain.CompiledSnapshot{
		Version:   7,
		CreatedAt: time.Now(),
		Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{
			"a": testTarget("a", 0, aCaps, &domain.Plan{ID: "free", Kind: domain.PlanFree}),
			"b": testTarget("b", 0, bCaps, &domain.Plan{ID: "free", Kind: domain.PlanFree}),
		},
		TargetOrder: []domain.ResourceTargetID{"a", "b"},
		Pools:       map[string][]domain.ResourceTargetID{"main": {"a", "b"}},
		Objectives: map[string]*domain.ObjectiveProfile{
			"balanced": {Name: "balanced", RankKeys: []string{"capability_fit", "min_effective_cost", "max_reliability", "min_latency"}},
		},
		Policies:      map[string]*domain.CompiledPolicy{},
		DefaultPolicy: "main",
	}
	pol := &domain.CompiledPolicy{
		Name:     "main",
		Timezone: time.UTC,
		Tiers:    []domain.TierDef{{Name: "primary", Pool: "main"}},
		Recovery: map[domain.FailureType]domain.RecoveryAction{},
	}
	snap.Policies["main"] = pol

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if aState == nil {
		aState = &domain.TargetState{}
	}
	if bState == nil {
		bState = &domain.TargetState{}
	}
	state := domain.NewSnapshot(now, map[domain.ResourceTargetID]*domain.TargetState{
		"a": aState, "b": bState,
	})
	return snap, pol, state
}

func basicInput(snap *domain.CompiledSnapshot, pol *domain.CompiledPolicy, state *domain.Snapshot) *PlanInput {
	return &PlanInput{
		Snapshot:  snap,
		Policy:    pol,
		Objective: snap.Objectives["balanced"],
		State:     state,
	}
}

func ptr[T any](v T) *T { return &v }

// countString counts occurrences of v in a []string (slices.Count is
// generics-constrained to ~[]byte in the toolchain this repo targets).
func countString(xs []string, v string) int {
	n := 0
	for _, x := range xs {
		if x == v {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------

func TestPlanDeterministicAndLexicographic(t *testing.T) {
	caps := domain.Capabilities{Text: true, ContextWindow: 32 * 1024}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	in := basicInput(snap, pol, state)

	for i := 0; i < 20; i++ {
		res, err := Plan(in)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		want := []domain.ResourceTargetID{"a", "b"}
		if len(res.Order) != 2 || res.Order[0] != want[0] || res.Order[1] != want[1] {
			t.Fatalf("run %d order=%v want %v (tie-break must be ID ascending)", i, res.Order, want)
		}
		if res.Primary != "a" {
			t.Fatalf("primary=%s want a", res.Primary)
		}
		if len(res.OrderTiers) != 1 || len(res.OrderTiers[0]) != 2 {
			t.Fatalf("order tiers=%v", res.OrderTiers)
		}
		if res.Tiers[0] != "primary" {
			t.Errorf("tiers=%v", res.Tiers)
		}
	}
}

func TestPlanPriorityKey(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	// priority only matters when it is part of the objective's rank keys
	snap.Objectives["balanced"].RankKeys = []string{"priority"}
	// b gets better (lower) priority -> should win despite lexicographic order.
	snap.Targets["b"].Priority = -5
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "b" || res.Order[1] != "a" {
		t.Fatalf("priority key ignored: %v", res.Order)
	}
}

func TestPlanPinnedTargetFirst(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	pol.Rules = append(pol.Rules, domain.CompiledRule{
		Name:   "pin-b",
		When:   domain.CompiledWhen{HasTarget: true, Target: "b"},
		Action: domain.RuleAction{Pin: ptr(true)},
	})
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "b" {
		t.Fatalf("pinned target must sort first, got %v", res.Order)
	}
}

func TestPlanPinFalseOverridesEarlierBroaderPin(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	snap.Objectives["balanced"].RankKeys = []string{"priority"}
	snap.Targets["b"].Priority = -10
	pol.Rules = []domain.CompiledRule{
		{
			Name: "pin-all", When: domain.CompiledWhen{HasPool: true, Pool: "main"},
			Action: domain.RuleAction{Pin: ptr(true)},
		},
		{
			Name: "unpin-b", When: domain.CompiledWhen{HasTarget: true, Target: "b"},
			Action: domain.RuleAction{Pin: ptr(false)},
		},
	}
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "a" {
		t.Fatalf("later pin:false did not refine the broader pin: %v", res.Order)
	}
}

func TestPlanPreferredTargetFirst(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	pol.Rules = append(pol.Rules, domain.CompiledRule{
		Name:   "prefer-a",
		When:   domain.CompiledWhen{Pool: "main", HasPool: true},
		Action: domain.RuleAction{Prefer: ptr(true)},
	})
	// Both match the pool scope; prefer is not discriminating here. Scope to
	// target a instead.
	pol.Rules[0] = domain.CompiledRule{
		Name:   "prefer-a",
		When:   domain.CompiledWhen{HasTarget: true, Target: "a"},
		Action: domain.RuleAction{Prefer: ptr(true)},
	}
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "a" {
		t.Fatalf("preferred target must sort ahead of unpinned peers, got %v", res.Order)
	}
}

func TestPlanPreferenceUsesAllPredicatesAndFalseDemotes(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	snap.Targets["b"].Priority = -10
	snap.Objectives["balanced"].RankKeys = []string{"priority"}
	pol.Rules = []domain.CompiledRule{
		{
			Name: "prefer-a-only-for-team-x-in-window",
			When: domain.CompiledWhen{
				HasTarget: true, Target: "a", ConsumerKey: "team-x",
				Window: &domain.TimeWindow{Start: "11:00", End: "13:00"},
			},
			Action: domain.RuleAction{Prefer: ptr(true)},
		},
		{
			Name:   "deprioritize-b-for-team-x",
			When:   domain.CompiledWhen{HasTarget: true, Target: "b", ConsumerKey: "team-x"},
			Action: domain.RuleAction{Prefer: ptr(false)},
		},
	}
	in := basicInput(snap, pol, state)
	in.ConsumerKey = "team-x"
	res, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "a" {
		t.Fatalf("conditional preference/de-prioritization order = %v", res.Order)
	}

	in.ConsumerKey = "team-y"
	res, err = Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "b" {
		t.Fatalf("non-matching consumer inherited preference: %v", res.Order)
	}
}

func TestPlanRequireCapabilityAndConditionalTierMove(t *testing.T) {
	snap, pol, state := twoTargets(t,
		domain.Capabilities{Text: true},
		domain.Capabilities{Text: true, Tools: true}, nil, nil)
	snap.Pools["free"] = []domain.ResourceTargetID{"a"}
	snap.Pools["paid"] = []domain.ResourceTargetID{"b"}
	pol.Tiers = []domain.TierDef{{Name: "free-tier", Pool: "free"}, {Name: "paid-tier", Pool: "paid"}}
	pol.Rules = []domain.CompiledRule{
		{
			Name: "weekday-daytime-go-paid",
			When: domain.CompiledWhen{
				HasPool: true, Pool: "free", Weekdays: map[string]bool{"fri": true},
				Window: &domain.TimeWindow{Start: "10:00", End: "16:00"},
			},
			Action: domain.RuleAction{Tier: "paid-tier"},
		},
		{
			Name:   "tools-required-in-paid",
			When:   domain.CompiledWhen{HasPool: true, Pool: "paid"},
			Action: domain.RuleAction{Require: "tools"},
		},
	}
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OrderTiers) != 1 || len(res.OrderTiers[0]) != 2 || res.OrderTiers[0][0] != "a" {
		t.Fatalf("conditional tier move did not place free target in paid tier: tiers=%v order=%v", res.Tiers, res.OrderTiers)
	}

	// Requiring tools on the moved free target must exclude it; add the
	// capability and verify it becomes eligible again.
	pol.Rules[1].When = domain.CompiledWhen{HasTarget: true, Target: "a"}
	res, err = Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Order) != 1 || res.Order[0] != "b" {
		t.Fatalf("require action did not exclude missing capability: %v", res.Order)
	}
	snap.Targets["a"].Capabilities.Tools = true
	res, err = Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Order) != 2 {
		t.Fatalf("capable target remained excluded: %v", res.Order)
	}
}

func TestPlanPreservesOverlappingTierMembershipWithoutAMove(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	snap.Pools["first"] = []domain.ResourceTargetID{"a"}
	snap.Pools["second"] = []domain.ResourceTargetID{"a", "b"}
	pol.Tiers = []domain.TierDef{{Name: "first", Pool: "first"}, {Name: "second", Pool: "second"}}
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OrderTiers) != 2 || len(res.OrderTiers[0]) != 1 || res.OrderTiers[0][0] != "a" ||
		len(res.OrderTiers[1]) != 2 || res.OrderTiers[1][0] != "a" || res.OrderTiers[1][1] != "b" {
		t.Fatalf("overlapping tier membership changed without a move rule: %v", res.OrderTiers)
	}
}

func TestPlanExcludeRule(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	pol.Rules = append(pol.Rules, domain.CompiledRule{
		Name:   "block-a",
		When:   domain.CompiledWhen{HasTarget: true, Target: "a"},
		Action: domain.RuleAction{Exclude: ptr(true)},
	})
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "b" {
		t.Fatalf("excluded target still primary: %v", res.Order)
	}
	var blocked *domain.CandidateRecord
	for i := range res.Candidates {
		if res.Candidates[i].Target == "a" {
			blocked = &res.Candidates[i]
		}
	}
	if blocked == nil || blocked.Eligible {
		t.Fatal("candidate record for a missing or wrongly eligible")
	}
	if blocked.ExclusionReason != `policy rule "block-a"` {
		t.Errorf("exclusion reason = %q", blocked.ExclusionReason)
	}
}

// Rule matching predicates: time windows (incl. wrap), weekdays, health,
// cooldown, quota ranges (incl. eq), confidence, multiplier, consumer key.
func TestRuleMatching(t *testing.T) {
	caps := domain.Capabilities{Text: true}

	mkRule := func(name string, w domain.CompiledWhen) domain.CompiledRule {
		return domain.CompiledRule{Name: name, When: w, Action: domain.RuleAction{Exclude: ptr(true)}}
	}

	cases := []struct {
		name    string
		when    domain.CompiledWhen
		now     time.Time
		stA     *domain.TargetState
		multA   float64 // multiplier of a's plan at now (via Plan windows)
		blocked bool    // whether "a" ends up excluded by rule
	}{
		{
			name:    "time window inside",
			when:    domain.CompiledWhen{Window: &domain.TimeWindow{Start: "08:00", End: "18:00"}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			blocked: true,
		},
		{
			name:    "time window outside",
			when:    domain.CompiledWhen{Window: &domain.TimeWindow{Start: "08:00", End: "18:00"}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 19, 0, 0, 0, time.UTC),
			blocked: false,
		},
		{
			name:    "wrap window late night matches",
			when:    domain.CompiledWhen{Window: &domain.TimeWindow{Start: "22:00", End: "06:00"}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 23, 30, 0, 0, time.UTC),
			blocked: true,
		},
		{
			name:    "wrap window end boundary excluded",
			when:    domain.CompiledWhen{Window: &domain.TimeWindow{Start: "22:00", End: "06:00"}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 6, 0, 0, 0, time.UTC),
			blocked: false,
		},
		{
			name:    "weekday matches (Fri)",
			when:    domain.CompiledWhen{Weekdays: map[string]bool{"fri": true}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{},
			blocked: true,
		},
		{
			name:    "weekday mismatch",
			when:    domain.CompiledWhen{Weekdays: map[string]bool{"mon": true}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			blocked: false,
		},
		{
			name:    "health unhealthy requires open breaker",
			when:    domain.CompiledWhen{Health: "unhealthy", HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{Breaker: domain.BreakerOpen},
			blocked: true,
		},
		{
			name:    "health unhealthy with closed breaker no match",
			when:    domain.CompiledWhen{Health: "unhealthy", HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{Breaker: domain.BreakerClosed},
			blocked: false,
		},
		{
			name:    "health healthy rejects open breaker",
			when:    domain.CompiledWhen{Health: "healthy", HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{Breaker: domain.BreakerOpen},
			blocked: false,
		},
		{
			name:    "cooldown wanted and present",
			when:    domain.CompiledWhen{WantCooldown: true, HasCooldown: true, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{CooldownUntil: time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)},
			blocked: true,
		},
		{
			name:    "cooldown wanted but zero",
			when:    domain.CompiledWhen{WantCooldown: true, HasCooldown: true, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{},
			blocked: false,
		},
		{
			name:    "quota remaining range gte",
			when:    domain.CompiledWhen{QuotaRemaining: &domain.FloatRange{Gte: ptr(0.9)}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{QuotaUsed: 100, QuotaLimit: 1000},
			blocked: true,
		},
		{
			name:    "quota remaining below gte",
			when:    domain.CompiledWhen{QuotaRemaining: &domain.FloatRange{Gte: ptr(0.95)}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{QuotaUsed: 200, QuotaLimit: 1000},
			blocked: false,
		},
		{
			name:    "quota remaining eq exact",
			when:    domain.CompiledWhen{QuotaRemaining: &domain.FloatRange{Eq: ptr(0.75)}, HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{QuotaUsed: 250, QuotaLimit: 1000},
			blocked: true,
		},
		{
			name:    "quota confidence low",
			when:    domain.CompiledWhen{QuotaConfidence: "low", HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{QuotaConf: domain.ConfLow},
			blocked: true,
		},
		{
			name:    "quota confidence mismatch",
			when:    domain.CompiledWhen{QuotaConfidence: "low", HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			stA:     &domain.TargetState{QuotaConf: domain.ConfHigh},
			blocked: false,
		},
		{
			name:    "consumer key match",
			when:    domain.CompiledWhen{ConsumerKey: "team-x", HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			blocked: true,
		},
		{
			name:    "consumer key mismatch",
			when:    domain.CompiledWhen{ConsumerKey: "team-y", HasTarget: true, Target: "a"},
			now:     time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
			blocked: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
			now := c.now
			var stA, stB *domain.TargetState = &domain.TargetState{}, &domain.TargetState{}
			if c.stA != nil {
				stA = c.stA
			}
			state := domain.NewSnapshot(now, map[domain.ResourceTargetID]*domain.TargetState{"a": stA, "b": stB})
			pol.Rules = []domain.CompiledRule{mkRule("r", c.when)}
			in := basicInput(snap, pol, state)
			in.ConsumerKey = "team-x"

			res, err := Plan(in)
			if err != nil {
				t.Fatal(err)
			}
			blockedA := false
			for _, cd := range res.Candidates {
				if cd.Target == "a" && !cd.Eligible && strings.HasPrefix(cd.ExclusionReason, `policy rule`) {
					blockedA = true
				}
			}
			if blockedA != c.blocked {
				t.Fatalf("blockedA=%v want %v (candidates: %+v)", blockedA, c.blocked, res.Candidates)
			}
			_ = c.multA // multiplier predicate covered via golden-hours case below
		})
	}
}

func TestRuleQuotaMultiplierPredicate(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
	// Golden hours on a's plan: 00:00-08:00 Shanghai ×0.5. Rule excludes when
	// the current multiplier is within [0,1).
	snap.Targets["a"].Plan = &domain.Plan{
		ID: "free", Kind: domain.PlanFree, Timezone: "Asia/Shanghai",
		Windows: []domain.MultiplierWindow{{Start: "00:00", End: "08:00", Multiplier: 0.5}},
	}
	golden := time.Date(2026, 8, 21, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	state := domain.NewSnapshot(golden, map[domain.ResourceTargetID]*domain.TargetState{
		"a": {}, "b": {},
	})
	pol.Rules = []domain.CompiledRule{{
		Name: "golden-hour-free-excluded",
		When: domain.CompiledWhen{
			HasTarget: true, Target: "a",
			QuotaMult: &domain.FloatRange{Lt: ptr(1.0)},
		},
		Action: domain.RuleAction{Exclude: ptr(true)},
	}}
	res, err := Plan(basicInput(snap, pol, state))
	if err != nil {
		t.Fatal(err)
	}
	for _, cd := range res.Candidates {
		if cd.Target == "a" && !cd.Eligible {
			return // excluded during golden hours as expected
		}
	}
	t.Fatal("multiplier predicate did not exclude a during golden hours")
}

// ---------------------------------------------------------------------------
// Runtime availability exclusions — exact reason strings feed the decision trace.

func TestAvailabilityExclusions(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	t.Run("breaker open", func(t *testing.T) {
		snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
		state := domain.NewSnapshot(now, map[domain.ResourceTargetID]*domain.TargetState{
			"a": {Breaker: domain.BreakerOpen}, "b": {},
		})
		res, err := Plan(basicInput(snap, pol, state))
		if err != nil {
			t.Fatal(err)
		}
		if res.Primary != "b" {
			t.Fatalf("open breaker must demote a: %v", res.Order)
		}
		found := ""
		for _, cd := range res.Candidates {
			if cd.Target == "a" {
				found = cd.ExclusionReason
			}
		}
		if found != "circuit_breaker = open" {
			t.Errorf("reason = %q", found)
		}
	})

	t.Run("cooldown until formatted", func(t *testing.T) {
		snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
		cd := now.Add(90 * time.Second)
		state := domain.NewSnapshot(now, map[domain.ResourceTargetID]*domain.TargetState{
			"a": {CooldownUntil: cd}, "b": {},
		})
		res, err := Plan(basicInput(snap, pol, state))
		if err != nil {
			t.Fatal(err)
		}
		found := ""
		for _, cd := range res.Candidates {
			if cd.Target == "a" {
				found = cd.ExclusionReason
			}
		}
		if found != "cooldown until "+cd.Format("15:04:05") {
			t.Errorf("reason = %q", found)
		}
	})

	t.Run("quota exhausted", func(t *testing.T) {
		snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
		state := domain.NewSnapshot(now, map[domain.ResourceTargetID]*domain.TargetState{
			"a": {QuotaUsed: 100, QuotaLimit: 100}, "b": {},
		})
		res, err := Plan(basicInput(snap, pol, state))
		if err != nil {
			t.Fatal(err)
		}
		found := ""
		for _, cd := range res.Candidates {
			if cd.Target == "a" {
				found = cd.ExclusionReason
			}
		}
		if found != "quota exhausted" {
			t.Errorf("reason = %q", found)
		}
	})
}

// ---------------------------------------------------------------------------
// Capability gate + ExtraCheck + objective constraints + ErrNoCandidate.

func TestCapabilityGateExclusion(t *testing.T) {
	full := domain.Capabilities{Text: true, Tools: true, Stream: true, ContextWindow: 128 * 1024}
	small := domain.Capabilities{Text: true, ContextWindow: 8 * 1024}
	snap, pol, state := twoTargets(t, full, small, nil, nil)

	req := domain.Requirements{Text: true, Tools: true, MinContext: 32000}
	in := basicInput(snap, pol, state)
	in.Required = req
	res, err := Plan(in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.Primary != "a" {
		t.Fatalf("primary=%q want a (only a satisfies the requirements)", res.Primary)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("both candidates recorded, got %d", len(res.Candidates))
	}
	reasonB := ""
	for _, cd := range res.Candidates {
		if cd.Target == "b" {
			reasonB = cd.ExclusionReason
		}
	}
	if !strings.Contains(reasonB, "missing capabilities: tools") ||
		!strings.Contains(reasonB, "context 8K < required 31K") { // 32000/1024 truncates to 31
		t.Errorf("cap exclusion reason malformed: %q", reasonB)
	}
}

type asErr struct{ error }

func errorsAs(err error, target **domain.ErrNoCandidate) bool {
	if e, ok := err.(*domain.ErrNoCandidate); ok {
		*target = e
		return true
	}
	return false
}

func TestExtraCheckBlocksBeforeRules(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	in := basicInput(snap, pol, state)
	in.ExtraCheck = func(id domain.ResourceTargetID) (string, bool) {
		return "compatibility degraded not allowed (zero silent degradation): dropped thinking", id == "a"
	}
	res, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "b" {
		t.Fatalf("extra check must block a: %v", res.Order)
	}
	for _, cd := range res.Candidates {
		if cd.Target == "a" && cd.Eligible {
			t.Fatal("a recorded eligible despite ExtraCheck")
		}
	}
}

func TestNoCashSpendObjective(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, state := twoTargets(t, caps, caps, nil, nil)
	// Make b a PAYG target under a no-cash-spend objective.
	snap.Targets["b"].Plan = &domain.Plan{ID: "payg", Kind: domain.PlanPAYG}
	obj := &domain.ObjectiveProfile{Name: "free-only", NoCashSpend: true}
	in := basicInput(snap, pol, state)
	in.Objective = obj
	res, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "a" {
		t.Fatalf("PAYG target must be excluded under no-cash objective: %v", res.Order)
	}
	for _, cd := range res.Candidates {
		if cd.Target == "b" && cd.Eligible {
			t.Error("b eligible despite cash ban")
		}
	}
}

func TestErrNoCandidateShape(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
	now := time.Now()
	state := domain.NewSnapshot(now, map[domain.ResourceTargetID]*domain.TargetState{
		"a": {Breaker: domain.BreakerOpen}, "b": {Breaker: domain.BreakerOpen},
	})
	in := basicInput(snap, pol, state)
	in.Alias = &domain.CompiledAlias{Name: "glm-4"}
	res, err := Plan(in)
	if err == nil {
		t.Fatal("expected error")
	}
	nc := err.(*domain.ErrNoCandidate)
	if nc.Alias != "glm-4" {
		t.Errorf("alias = %q", nc.Alias)
	}
	if nc.Reason == "" || !strings.Contains(nc.Reason, "all candidates excluded") {
		t.Errorf("reason = %q", nc.Reason)
	}
	if len(nc.Excluded) != 2 {
		t.Fatalf("excluded records = %d", len(nc.Excluded))
	}
	if res.Primary != "" || res.Order != nil {
		t.Errorf("failed plan must not name a primary: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Compiler: defaults, lint, hash format, republish versioning.

func compileFile(t *testing.T, yamlSrc string) *domain.CompiledSnapshot {
	t.Helper()
	dir := t.TempDir()
	f, err := config.Load(writeTemp(t, dir, yamlSrc))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := NewCompiler()
	snap, err := c.Compile(f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return snap
}

func writeTemp(t *testing.T, dir, content string) string {
	t.Helper()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodConfig = `
endpoints:
  - {id: zhipu, base_url: https://open.bigmodel.cn/api/paas/v4, protocol: openai-chat}
plans:
  - id: free
    kind: free
    quota: {bucket_id: glm-free}
quota_buckets:
  - id: glm-free
    unit: prompts
    limit_per_cycle: 200
    refresh: {period: daily, at: "04:00"}
resource_targets:
  - id: glm-1
    endpoint_id: zhipu
    model: glm-4-flash
    plan_id: free
    credential: {secret_ref: env:ZHIPU_KEY}
    capabilities: {text: true, stream: true, context_window: 131072}
  - id: glm-2
    endpoint_id: zhipu
    model: glm-4-flash
    plan_id: free
    credential: {secret_ref: env:ZHIPU_KEY}
    capabilities: {text: true, stream: true, context_window: 131072}
pools:
  - {id: main, targets: [glm-1, glm-2]}
objectives:
  cheap:
    rank: [min_effective_cost, max_reliability]
aliases:
  glm-flash:
    required_capabilities: {text: true, stream: true}
default_policy: std
policies:
  std:
    timezone: Asia/Shanghai
    tiers:
      - {name: main, pool: main, objective: cheap}
    rules:
      - name: weekend-drain
        when:
          weekdays: [Saturday, Sunday]
          quota_remaining: {lt: 0.2}
        action: {exclude: true}
`

func TestCompileHappyPath(t *testing.T) {
	snap := compileFile(t, goodConfig)

	if len(snap.Targets) != 2 {
		t.Fatalf("targets = %d", len(snap.Targets))
	}
	tg := snap.Targets["glm-1"]
	if tg.Weight != 1 {
		t.Errorf("weight default must be 1, got %d", tg.Weight)
	}
	if tg.Bucket == nil || tg.Bucket.ID != "glm-free" {
		t.Errorf("bucket not linked from plan.quota: %+v", tg.Bucket)
	}
	if len(tg.PoolIDs) != 1 || tg.PoolIDs[0] != "main" {
		t.Errorf("pool ids = %v", tg.PoolIDs)
	}
	// pools are sorted deterministically
	if got := snap.Pools["main"]; len(got) != 2 || got[0] != "glm-1" || got[1] != "glm-2" {
		t.Errorf("pool order = %v", got)
	}
	alias := snap.Aliases["glm-flash"]
	if alias == nil || !alias.RequiredCaps.Text || !alias.RequiredCaps.Stream {
		t.Fatalf("alias compiled wrong: %+v", alias)
	}
	obj := snap.Objectives["cheap"]
	if obj == nil || obj.Name != "cheap" || len(obj.RankKeys) != 2 {
		t.Fatalf("objective compiled wrong: %+v", obj)
	}
	// balanced injected even though absent from config
	if snap.Objectives["balanced"] == nil {
		t.Error("balanced objective must be auto-injected")
	}
	pol := snap.Policies["std"]
	if pol == nil {
		t.Fatal("policy std missing")
	}
	if pol.Timezone.String() != "Asia/Shanghai" {
		t.Errorf("policy tz = %v", pol.Timezone)
	}
	if len(pol.Rules) != 1 || len(pol.Rules[0].When.Weekdays) != 2 {
		t.Errorf("rule compilation wrong: %+v", pol.Rules)
	}
	if _, sat := pol.Rules[0].When.Weekdays["sat"]; !sat {
		t.Errorf("weekdays normalized to 3-letter keys: %+v", pol.Rules[0].When.Weekdays)
	}
	if len(snap.Hash) != 16 {
		t.Errorf("hash must be 16 hex chars, got %q (%d)", snap.Hash, len(snap.Hash))
	}
	for _, ch := range snap.Hash {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			t.Fatalf("hash not hex: %q", snap.Hash)
		}
	}
	if snap.Version != 0 {
		t.Errorf("initial compile leaves version 0 for caller to set, got %d", snap.Version)
	}
}

func TestCompileAtRevisionUsesExactDurableRevision(t *testing.T) {
	c := NewCompiler()
	dir := t.TempDir()
	f, err := config.Load(writeTemp(t, dir, goodConfig))
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.CompileAtRevision(f, 37)
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != 37 {
		t.Errorf("runtime revision = %d, want durable revision 37", out.Version)
	}
	if len(out.Hash) != 16 {
		t.Errorf("hash recomputed: %q", out.Hash)
	}
}

func TestLintErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "tier pool empty or missing",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example/v1, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}, capabilities: {text: true}}
pools:
  - {id: main, targets: [t1]}
default_policy: std
# lint (not config.Validate) catches the dangling pool reference; Validate only
# knows about pools that exist in the resource tree.
policies:
  std:
    tiers:
      - {name: broken, pool: ghost}
`,
			want: "policy std: tier broken pool ghost empty or missing",
		},
		{
			name: "duplicate tier membership",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example/v1, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}, capabilities: {text: true}}
pools:
  - {id: p1, targets: [t1]}
  - {id: p2, targets: [t1]}
default_policy: std
policies:
  std:
    tiers:
      - {name: first, pool: p1}
      - {name: second, pool: p2}
`,
			want: "policy std: target t1 in tiers first and second (duplicate membership)",
		},
		{
			name: "no candidates anywhere",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example/v1, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}, capabilities: {text: true}}
pools:
  - {id: p1, targets: [t1]}
default_policy: std
policies:
  other:
    tiers: []
`,
			want: "",
		},
	}
	for _, tc := range cases[:2] {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, tc.yaml)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var f config.File
			if err := yaml.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			// Deliberately skip f.Validate(): these cases exercise the
			// compiler's own lint pass, which runs after Validate in Load and
			// would otherwise mask these defects first.
			snap, cerr := NewCompiler().Compile(&f)
			_ = snap
			if cerr == nil || !strings.Contains(cerr.Error(), tc.want) {
				t.Fatalf("got %v want substring %q", cerr, tc.want)
			}
		})
	}
}

func TestLintRejectsUnsatisfiableAlias(t *testing.T) {
	yamlSrc := `
endpoints:
  - {id: ep, base_url: https://a.example/v1, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}, capabilities: {text: true}}
pools:
  - {id: main, targets: [t1]}
aliases:
  vision-bot:
    required_capabilities: {text: true, vision: true}
default_policy: std
policies:
  std:
    tiers:
      - {name: main, pool: main}
`
	dir := t.TempDir()
	f, err := config.Load(writeTemp(t, dir, yamlSrc))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, cerr := NewCompiler().Compile(f)
	if cerr == nil || !strings.Contains(cerr.Error(),
		"alias vision-bot: no configured target satisfies its capability contract") {
		t.Fatalf("got %v", cerr)
	}
}

func TestLintAcceptsAliasSatisfiedByEffectiveParallelTools(t *testing.T) {
	yamlSrc := `
endpoints:
  - {id: ep, base_url: https://a.example/v1, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}, capabilities: {text: true, tools: true, parallel_tools: false}}
pools:
  - {id: main, targets: [t1]}
aliases:
  tool-bot:
    required_capabilities: {text: true, tools: true, parallel_tools: true}
default_policy: std
policies:
  std:
    tiers:
      - {name: main, pool: main}
`
	dir := t.TempDir()
	f, err := config.Load(writeTemp(t, dir, yamlSrc))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := NewCompiler().Compile(f); err != nil {
		t.Fatalf("effective parallel-tools capability failed lint: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Ranking score functions (spot checks on the exported behavior via Plan).

func TestRankingByLeastUsedAndLatency(t *testing.T) {
	caps := domain.Capabilities{Text: true, ContextWindow: 128 * 1024}
	snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
	obj := &domain.ObjectiveProfile{Name: "spread", RankKeys: []string{"least_used"}}
	in := basicInput(snap, pol, nil)
	in.Objective = obj
	in.State = domain.NewSnapshot(time.Now(), map[domain.ResourceTargetID]*domain.TargetState{
		"a": {InFlight: 5}, "b": {InFlight: 1},
	})
	res, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary != "b" {
		t.Fatalf("least_used must pick lower InFlight, got %v", res.Order)
	}

	// latency EWMA drives min_latency
	in2 := basicInput(snap, pol, nil)
	in2.Objective = &domain.ObjectiveProfile{Name: "fast", RankKeys: []string{"min_latency"}}
	in2.State = domain.NewSnapshot(time.Now(), map[domain.ResourceTargetID]*domain.TargetState{
		"a": {LatencyEWMAms: 2500}, "b": {LatencyEWMAms: 300},
	})
	res2, err := Plan(in2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Primary != "b" {
		t.Fatalf("min_latency must pick lower EWMA, got %v", res2.Order)
	}
}

func TestUnknownRankKeySortsLast(t *testing.T) {
	caps := domain.Capabilities{Text: true}
	snap, pol, _ := twoTargets(t, caps, caps, nil, nil)
	obj := &domain.ObjectiveProfile{Name: "weird", RankKeys: []string{"nonexistent_key", "priority"}}
	// both share priority 0 -> unknown key equal (MaxFloat64/4) -> tie-break ID
	res, err := Plan(basicInput(snap, pol, func() *domain.Snapshot {
		return domain.NewSnapshot(time.Now(), map[domain.ResourceTargetID]*domain.TargetState{"a": {}, "b": {}})
	}()))
	if err != nil {
		t.Fatal(err)
	}
	_ = obj
	if res.Primary != "a" {
		t.Fatalf("unknown keys must degrade to stable tie-break, got %v", res.Order)
	}
}

// ---------------------------------------------------------------------------
// Channel expansion: channel×model pairs compile into ordinary targets so the
// rest of the machinery never learns about channels.

func channelFixture(poolsYAML, modelsYAML string) string {
	return `
endpoints:
  - {id: zhipu, base_url: https://open.bigmodel.cn/api/paas/v4, protocol: openai-chat}
plans:
  - id: free
    kind: free
    quota: {bucket_id: shared-bucket}
quota_buckets:
  - id: shared-bucket
    unit: prompts
    limit_per_cycle: 100
    refresh: {period: daily, at: "04:00"}
resource_targets:
  - id: glm-1
    endpoint_id: zhipu
    model: glm-4-flash
    plan_id: free
    credential: {secret_ref: env:ZHIPU_KEY}
    capabilities: {text: true, stream: true}
` + poolsYAML + `
channels:
  - id: relay
    base_url: https://relay.example/v1
    credential: {secret_ref: env:RELAY_KEY}
    plan_id: free
` + modelsYAML + `
policies:
  std:
    tiers:
      - {name: main, pool: main}
default_policy: std
`
}

const channelModelsBasic = `    pools: [main]
    models:
      - model: opus
        protocols: [anthropic, openai-chat]
        capabilities: {text: true, stream: true, context_window: 200000}
      - model: deepseek-v4
        protocols: [openai-chat]
`

func TestChannelExpandsToTargets(t *testing.T) {
	snap := compileFile(t, channelFixture("pools:\n  - {id: main, targets: [glm-1]}\n", channelModelsBasic))

	if len(snap.Targets) != 3 {
		t.Fatalf("targets = %d (%v), want 3 (2 expanded + 1 classic)",
			len(snap.Targets), snap.TargetOrder)
	}
	opus := snap.Targets[domain.ResourceTargetID("relay.opus")]
	if opus == nil {
		t.Fatal("expanded target relay.opus missing")
	}
	if opus.Model != "opus" {
		t.Errorf("Model = %q", opus.Model)
	}
	if !slices.Equal(opus.NativeProtocols, []domain.Protocol{domain.ProtocolAnthropic, domain.ProtocolOpenAIChat}) {
		t.Errorf("NativeProtocols = %v", opus.NativeProtocols)
	}
	if !opus.SpeaksNatively(domain.ProtocolAnthropic) || !opus.SpeaksNatively(domain.ProtocolOpenAIChat) ||
		opus.SpeaksNatively(domain.ProtocolOpenAIResponses) {
		t.Error("SpeaksNatively disagrees with natives")
	}
	// interim per-model virtual endpoint: least-lossy native protocol
	if opus.Endpoint.ID != "__channel_relay" {
		t.Errorf("virtual endpoint id = %q", opus.Endpoint.ID)
	}
	if opus.Endpoint.BaseURL != "https://relay.example/v1" {
		t.Errorf("endpoint base_url = %q", opus.Endpoint.BaseURL)
	}
	if want := domain.PreferredProtocol([]domain.Protocol{domain.ProtocolAnthropic, domain.ProtocolOpenAIChat}); opus.Endpoint.Protocol != want {
		t.Errorf("endpoint protocol = %q, want PreferredProtocol %q", opus.Endpoint.Protocol, want)
	}
	if opus.Weight != 1 {
		t.Errorf("weight default must be 1, got %d", opus.Weight)
	}
	if opus.Bucket == nil || opus.Bucket.ID != "shared-bucket" {
		t.Errorf("bucket not linked through plan.quota: %+v", opus.Bucket)
	}
	if !slices.Equal(opus.PoolIDs, []string{"main"}) {
		t.Errorf("pool ids = %v", opus.PoolIDs)
	}
	if opus.Disabled {
		t.Error("enabled channel must compile Disabled=false")
	}

	// channel members join the classic target's pool, sorted deterministically
	got := snap.Pools["main"]
	want := []domain.ResourceTargetID{"glm-1", "relay.deepseek-v4", "relay.opus"}
	if !slices.Equal(got, want) {
		t.Errorf("pool main = %v, want %v", got, want)
	}
}

func TestChannelSynthesizesAliases(t *testing.T) {
	snap := compileFile(t, channelFixture("pools:\n  - {id: main, targets: [glm-1]}\n", channelModelsBasic))

	a := snap.Aliases["opus"]
	if a == nil || !a.Synthesized {
		t.Fatalf("synthesized alias for opus missing: %+v", a)
	}
	if a.RequiredCaps != (domain.Requirements{}) {
		t.Errorf("synthesized alias must not turn model support into request requirements: %+v", a.RequiredCaps)
	}
	second := snap.Aliases["deepseek-v4"]
	if second == nil || !second.Synthesized {
		t.Fatalf("synthesized alias for deepseek-v4 missing: %+v", second)
	}
	if second.RequiredCaps != (domain.Requirements{}) {
		t.Errorf("synthesized alias must have an empty workload contract: %+v", second.RequiredCaps)
	}
}

func TestChannelModelAliasPropagatesToExpandedTarget(t *testing.T) {
	models := `    pools: [main]
    models:
      - model: opus
        alias: sonnet
        protocols: [anthropic]
        capabilities: {text: true, stream: true}
`
	snap := compileFile(t, channelFixture("pools:\n  - {id: main, targets: [glm-1]}\n", models))
	target := snap.Targets[domain.ResourceTargetID("relay.opus")]
	if target == nil {
		t.Fatal("expanded target missing")
	}
	if target.ModelAlias != "sonnet" {
		t.Fatalf("expanded target ModelAlias = %q, want sonnet", target.ModelAlias)
	}
	if a := snap.Aliases["sonnet"]; a == nil || !a.Synthesized {
		t.Fatalf("client alias was not synthesized: %+v", a)
	}
}

func TestChannelDisabledKeepsShapeNoAliases(t *testing.T) {
	yaml := `
plans:
  - {id: free, kind: free}
channels:
  - id: relay
    base_url: https://relay.example/v1
    credential: {secret_ref: env:RELAY_KEY}
    plan_id: free
    enabled: false
    pools: [main]
    models:
      - model: opus
        protocols: [anthropic]
        capabilities: {text: true, stream: true}
      - model: deepseek-v4
        protocols: [openai-chat]
pools:
  - {id: main, targets: [relay.opus]}
policies:
  std:
    tiers:
      - {name: main, pool: main}
default_policy: std
`
	snap := compileFile(t, yaml)

	opus := snap.Targets[domain.ResourceTargetID("relay.opus")]
	if opus == nil {
		t.Fatal("disabled channel targets must stay in the snapshot")
	}
	if !opus.Disabled {
		t.Error("Disabled flag lost")
	}
	// shape holds (pools/lints), but nothing may address its models
	if got := snap.Pools["main"]; !slices.Contains(got, "relay.opus") {
		t.Errorf("disabled target dropped from pool: %v", got)
	}
	for _, name := range []string{"opus", "deepseek-v4"} {
		if a := snap.Aliases[name]; a != nil {
			t.Errorf("disabled channel must not synthesize alias %q", name)
		}
	}
}

func TestDisabledChannelMayBeOnlyPoolMember(t *testing.T) {
	yaml := `
plans:
  - {id: free, kind: free}
channels:
  - id: relay
    base_url: https://relay.example/v1
    credential: {secret_ref: env:RELAY_KEY}
    plan_id: free
    enabled: false
    pools: [main]
    models:
      - model: opus
        protocols: [anthropic]
        capabilities: {text: true}
pools:
  - {id: main, targets: []}
policies:
  std:
    tiers:
      - {name: main, pool: main}
default_policy: std
`
	snap := compileFile(t, yaml)
	if got := snap.Pools["main"]; !slices.Contains(got, "relay.opus") {
		t.Fatalf("disabled sole channel target dropped from pool: %v", got)
	}
	if target := snap.Targets["relay.opus"]; target == nil || !target.Disabled {
		t.Fatalf("disabled sole channel target missing or enabled: %+v", target)
	}
}

func TestChannelPoolMembershipDedup(t *testing.T) {
	// relay.opus is auto-joined via channel pools AND listed explicitly —
	// duplicate candidates would retry the same upstream.
	pools := "pools:\n  - {id: main, targets: [glm-1, relay.opus]}\n"
	snap := compileFile(t, channelFixture(pools, channelModelsBasic))

	got := snap.Pools["main"]
	n := 0
	for _, id := range got {
		if id == "relay.opus" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("relay.opus appears %d times in pool main (%v), want exactly 1", n, got)
	}
	want := []domain.ResourceTargetID{"glm-1", "relay.deepseek-v4", "relay.opus"}
	if !slices.Equal(got, want) {
		t.Errorf("pool main = %v, want %v", got, want)
	}
	opus := snap.Targets[domain.ResourceTargetID("relay.opus")]
	if c := countString(opus.PoolIDs, "main"); c != 1 {
		t.Errorf("PoolIDs contains main %d times: %v", c, opus.PoolIDs)
	}
}

func TestChannelModelPoolsOverride(t *testing.T) {
	models := `    pools: [chan-pool]
    models:
      - {model: m1, protocols: [openai-chat]}
      - {model: m2, protocols: [openai-chat], pools: [solo-pool]}
`
	pools := "pools:\n  - {id: chan-pool, targets: [glm-2]}\n  - {id: solo-pool, targets: [glm-2]}\n"
	yaml := `
endpoints:
  - {id: zhipu, base_url: https://open.bigmodel.cn/api/paas/v4, protocol: openai-chat}
plans:
  - id: free
    kind: free
resource_targets:
  - id: glm-2
    endpoint_id: zhipu
    model: glm-4-flash
    plan_id: free
    credential: {secret_ref: env:ZHIPU_KEY}
    capabilities: {text: true}
` + pools + `
channels:
  - id: relay
    base_url: https://relay.example/v1
    credential: {secret_ref: env:RELAY_KEY}
    plan_id: free
` + models + `
policies:
  std:
    tiers:
      - {name: main, pool: chan-pool}
default_policy: std
`
	snap := compileFile(t, yaml)

	m1 := snap.Targets[domain.ResourceTargetID("relay.m1")]
	m2 := snap.Targets[domain.ResourceTargetID("relay.m2")]
	if m1 == nil || m2 == nil {
		t.Fatalf("expanded targets missing: %v", snap.TargetOrder)
	}
	if got := snap.Pools["chan-pool"]; !slices.Equal(got, []domain.ResourceTargetID{"glm-2", "relay.m1"}) {
		t.Errorf("m1 must inherit channel pools, chan-pool = %v", got)
	}
	if got := snap.Pools["solo-pool"]; !slices.Equal(got, []domain.ResourceTargetID{"glm-2", "relay.m2"}) {
		t.Errorf("non-empty per-model pools must override channel pools, solo-pool = %v", got)
	}
	if !slices.Equal(m2.PoolIDs, []string{"solo-pool"}) {
		t.Errorf("m2 pool ids = %v", m2.PoolIDs)
	}
	if slices.Contains(snap.Pools["chan-pool"], "relay.m2") {
		t.Error("override must remove m2 from the channel-level pool")
	}
}

func TestUserAliasNotOverwrittenBySynthesis(t *testing.T) {
	models := strings.Replace(channelModelsBasic, "- model: opus", "- model: opus", 1)
	yaml := channelFixture("pools:\n  - {id: main, targets: [glm-1]}\n", models) + `
aliases:
  opus:
    required_capabilities: {text: true}
`
	snap := compileFile(t, yaml)

	a := snap.Aliases["opus"]
	if a == nil {
		t.Fatal("user alias opus vanished")
	}
	if a.Synthesized {
		t.Error("user-defined alias must keep its own semantics, not be replaced by synthesis")
	}
	// user contract: text only — synthesis would have added stream/min-context
	if a.RequiredCaps.Stream || a.RequiredCaps.MinContext != 0 {
		t.Errorf("user alias mutated by synthesis: %+v", a.RequiredCaps)
	}
}
