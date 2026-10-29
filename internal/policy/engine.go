package policy

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ferry/internal/domain"
)

// PlanInput is everything the engine needs for one deterministic decision.
type PlanInput struct {
	Snapshot        *domain.CompiledSnapshot
	Policy          *domain.CompiledPolicy
	Alias           *domain.CompiledAlias
	Objective       *domain.ObjectiveProfile
	IngressProtocol domain.Protocol
	Required        domain.Requirements
	ConsumerKey     string
	State           *domain.Snapshot // runtime resource state at the same instant

	// ExtraCheck optionally excludes targets for reasons outside the policy
	// DSL (e.g. protocol compatibility). Pure function of (id, request) —
	// must stay deterministic for the same inputs.
	ExtraCheck func(id domain.ResourceTargetID) (reason string, blocked bool)
}

// PlanResult is the deterministic execution plan plus its explanation.
type PlanResult struct {
	Tiers      []string
	Candidates []domain.CandidateRecord
	Order      []domain.ResourceTargetID // full fallback order across tiers
	// OrderTiers segments Order by tier: element i lists tier i's execution
	// order. Typed recovery navigates these segments (same-tier vs next-tier).
	OrderTiers [][]domain.ResourceTargetID
	// OrderTierPolicyIndexes maps each non-empty execution segment back to its
	// original route-step index, preserving per-step recovery semantics when a
	// preceding group has no eligible candidates.
	OrderTierPolicyIndexes []int
	Primary                domain.ResourceTargetID
}

// Plan computes the execution order: Capability Gate → Policy Filter →
// Tier walk → Lexicographic Rank → stable tie-break (PRD §12.2, F-P0-13).
func Plan(in *PlanInput) (*PlanResult, error) {
	res := &PlanResult{}
	excludedByCap := map[domain.ResourceTargetID]string{}

	// ---- Stage 1: capability gate (PRD §6.6 — before any policy) ----
	capOK := map[domain.ResourceTargetID]bool{}
	for id, t := range in.Snapshot.Targets {
		capabilities := targetCapabilities(in, t)
		if capabilities.Satisfies(in.Required) {
			capOK[id] = true
		} else {
			capOK[id] = false
			excludedByCap[id] = capExclusionReason(capabilities, in.Required)
		}
	}

	// ---- Stage 2: per-tier evaluation ----
	type tierOut struct {
		name  string
		order []ranked
	}
	var tiers []tierOut

	effectiveTargets := effectiveTierTargets(in)
	for _, tier := range in.Policy.Tiers {
		poolIDs := effectiveTargets[tier.Name]
		objName := tier.Objective
		if objName == "" {
			if in.Objective != nil {
				objName = in.Objective.Name
			}
		}
		obj := in.Objective
		if objName != "" {
			if o, ok := in.Snapshot.Objectives[objName]; ok {
				obj = o
			}
		}

		var rankedList []ranked
		for _, id := range poolIDs {
			t := in.Snapshot.Targets[id]
			if t == nil {
				continue
			}
			rec := domain.CandidateRecord{Target: id, Tier: tier.Name}

			// capability gate
			if !capOK[id] {
				rec.Eligible = false
				rec.ExclusionReason = excludedByCap[id]
				res.Candidates = append(res.Candidates, rec)
				continue
			}

			// policy rules
			reason, blocked := applyRules(in, id, t)
			if blocked {
				rec.Eligible = false
				rec.ExclusionReason = reason
				res.Candidates = append(res.Candidates, rec)
				continue
			}

			// objective hard constraint (e.g. max_cash_cost = 0)
			if obj != nil && obj.NoCashSpend && cashCostOf(t) > 0 {
				rec.Eligible = false
				rec.ExclusionReason = fmt.Sprintf("objective %s forbids cash spend", obj.Name)
				res.Candidates = append(res.Candidates, rec)
				continue
			}

			st := in.State.Get(id)
			// runtime availability: breaker / cooldown / quota
			if availReason := availabilityExclusion(st, in.State.Now); availReason != "" {
				rec.Eligible = false
				rec.ExclusionReason = availReason
				res.Candidates = append(res.Candidates, rec)
				continue
			}

			// extra scheduler-side gate (protocol compatibility), after cheap exclusions
			if in.ExtraCheck != nil {
				if reason, blocked := in.ExtraCheck(id); blocked {
					rec.Eligible = false
					rec.ExclusionReason = reason
					res.Candidates = append(res.Candidates, rec)
					continue
				}
			}

			rk := rankTarget(in, id, t, st, obj)
			rk.pinned = pinnedByRules(in, id, t, st)
			rk.preference = preferenceByRules(in, id, t, st)
			rec.Eligible = true
			rec.Scores = rk.scores
			rankedList = append(rankedList, rk)
			res.Candidates = append(res.Candidates, rec)
		}

		sortRanked(rankedList)
		for i, rk := range rankedList {
			for j := range res.Candidates {
				if res.Candidates[j].Target == rk.id && res.Candidates[j].Tier == tier.Name {
					res.Candidates[j].Rank = i + 1
				}
			}
		}
		out := tierOut{name: tier.Name}
		for _, rk := range rankedList {
			out.order = append(out.order, rk)
		}
		tiers = append(tiers, out)
	}

	for tierIndex, t := range tiers {
		res.Tiers = append(res.Tiers, t.name)
		var seg []domain.ResourceTargetID
		for _, rk := range t.order {
			seg = append(seg, rk.id)
			res.Order = append(res.Order, rk.id)
		}
		if len(seg) > 0 {
			res.OrderTiers = append(res.OrderTiers, seg)
			res.OrderTierPolicyIndexes = append(res.OrderTierPolicyIndexes, tierIndex)
		}
	}
	if len(res.Order) == 0 {
		exc := make([]domain.ExclusionRecord, 0, len(res.Candidates))
		for _, c := range res.Candidates {
			exc = append(exc, domain.ExclusionRecord{
				Target: c.Target, Tier: c.Tier, Reason: c.ExclusionReason,
			})
		}
		return res, &domain.ErrNoCandidate{
			Alias:    aliasName(in),
			Reason:   "all candidates excluded by capability gate, policy rules, or runtime state",
			Excluded: exc,
		}
	}
	res.Primary = res.Order[0]
	return res, nil
}

// effectiveTierTargets applies conditional tier moves before evaluating each
// tier. The first matching tier action moves a membership to the named
// destination. Pool predicates continue to describe the target's configured
// pool, even after a move, which keeps rules such as "when pool=free, move to
// paid" meaningful. A target may legitimately belong to multiple source tiers;
// preserve those memberships unless they converge on the same destination.
func effectiveTierTargets(in *PlanInput) map[string][]domain.ResourceTargetID {
	out := make(map[string][]domain.ResourceTargetID, len(in.Policy.Tiers))
	seenByDestination := map[string]map[domain.ResourceTargetID]bool{}
	for _, tier := range in.Policy.Tiers {
		for _, id := range in.Snapshot.Pools[tier.Pool] {
			target := in.Snapshot.Targets[id]
			if target == nil {
				continue
			}
			state := in.State.Get(id)
			multiplier := 1.0
			if target.Plan != nil {
				multiplier = target.Plan.CurrentMultiplier(in.State.Now)
			}
			destination := tier.Name
			for i := range in.Policy.Rules {
				rule := &in.Policy.Rules[i]
				if rule.Action.Tier != "" && ruleMatches(rule, in, id, target, state, multiplier) {
					destination = rule.Action.Tier
					break
				}
			}
			if seenByDestination[destination] == nil {
				seenByDestination[destination] = map[domain.ResourceTargetID]bool{}
			}
			if seenByDestination[destination][id] {
				continue
			}
			seenByDestination[destination][id] = true
			out[destination] = append(out[destination], id)
		}
	}
	return out
}

func aliasName(in *PlanInput) string {
	if in.Alias != nil {
		return in.Alias.Name
	}
	return ""
}

func targetCapabilities(in *PlanInput, target *domain.CompiledTarget) domain.Capabilities {
	if target == nil {
		return domain.Capabilities{}
	}
	return target.CapabilitiesFor(target.EgressProtocol(in.IngressProtocol))
}

func capExclusionReason(capabilities domain.Capabilities, req domain.Requirements) string {
	var missing []string
	for _, capability := range domain.BooleanCapabilityRegistry() {
		required, _ := domain.RequirementCapability(req, capability)
		supported, _ := domain.BooleanCapability(capabilities, capability)
		if required && !supported {
			missing = append(missing, capability)
		}
	}
	if req.MinContext > 0 && capabilities.ContextWindow > 0 &&
		capabilities.ContextWindow < req.MinContext {
		missing = append(missing,
			fmt.Sprintf("context %dK < required %dK",
				capabilities.ContextWindow/1024, req.MinContext/1024))
	}
	if req.MinOutput > 0 && capabilities.MaxOutput > 0 &&
		capabilities.MaxOutput < req.MinOutput {
		missing = append(missing,
			fmt.Sprintf("max output %d < required %d", capabilities.MaxOutput, req.MinOutput))
	}
	if len(missing) == 0 {
		return "capability mismatch"
	}
	return "missing capabilities: " + strings.Join(missing, ",")
}

// applyRules evaluates compiled policy rules against one target.
// First matching exclude wins; pin/prefer accumulate.
func applyRules(in *PlanInput, id domain.ResourceTargetID, t *domain.CompiledTarget) (string, bool) {
	st := in.State.Get(id)
	mult := 1.0
	if t.Plan != nil {
		mult = t.Plan.CurrentMultiplier(in.State.Now)
	}
	for i := range in.Policy.Rules {
		r := &in.Policy.Rules[i]
		if !ruleMatches(r, in, id, t, st, mult) {
			continue
		}
		if r.Action.Exclude != nil && *r.Action.Exclude {
			return fmt.Sprintf("policy rule %q", r.Name), true
		}
		if r.Action.Require != "" && !targetHasCapability(targetCapabilities(in, t), r.Action.Require) {
			return fmt.Sprintf("policy rule %q requires capability %s", r.Name, r.Action.Require), true
		}
	}
	return "", false
}

func pinnedByRules(in *PlanInput, id domain.ResourceTargetID, target *domain.CompiledTarget, state *domain.TargetState) bool {
	multiplier := 1.0
	if target.Plan != nil {
		multiplier = target.Plan.CurrentMultiplier(in.State.Now)
	}
	pinned := false
	for i := range in.Policy.Rules {
		r := &in.Policy.Rules[i]
		if r.Action.Pin != nil && ruleMatches(r, in, id, target, state, multiplier) {
			pinned = *r.Action.Pin
		}
	}
	return pinned
}

// preferenceByRules returns -1 for an explicit de-prioritization, 0 for no
// preference, and 1 for a preference. Later matching rules intentionally win,
// mirroring the ordered policy document while allowing a specific rule to
// refine a broader one.
func preferenceByRules(in *PlanInput, id domain.ResourceTargetID, target *domain.CompiledTarget, state *domain.TargetState) int {
	multiplier := 1.0
	if target.Plan != nil {
		multiplier = target.Plan.CurrentMultiplier(in.State.Now)
	}
	preference := 0
	for i := range in.Policy.Rules {
		r := &in.Policy.Rules[i]
		if r.Action.Prefer != nil && ruleMatches(r, in, id, target, state, multiplier) {
			if *r.Action.Prefer {
				preference = 1
			} else {
				preference = -1
			}
		}
	}
	return preference
}

func targetHasCapability(capabilities domain.Capabilities, capability string) bool {
	supported, known := domain.BooleanCapability(capabilities, capability)
	return known && supported
}

func ruleMatches(r *domain.CompiledRule, in *PlanInput, id domain.ResourceTargetID,
	t *domain.CompiledTarget, st *domain.TargetState, mult float64) bool {

	w := &r.When
	if w.HasTarget && w.Target != id {
		return false
	}
	if w.HasPool && !containsID(in.Snapshot.Pools[w.Pool], id) {
		return false
	}
	if w.Window != nil && !inTimeWindow(w.Window, in.State.Now, in.Policy.Timezone) {
		return false
	}
	if len(w.Weekdays) > 0 {
		dn := weekdayName(in.State.Now.In(in.Policy.Timezone).Weekday())
		if !w.Weekdays[dn] {
			return false
		}
	}
	switch w.Health {
	case "":
	case "unhealthy":
		if st.Breaker != domain.BreakerOpen || st.ConsecFails < 1 {
			if st.Breaker != domain.BreakerOpen {
				return false
			}
		}
	case "healthy":
		if st.Breaker == domain.BreakerOpen {
			return false
		}
	}
	if w.HasCooldown {
		if w.WantCooldown && st.CooldownUntil.IsZero() {
			return false
		}
	}
	if w.QuotaRemaining != nil {
		frac := st.QuotaRemainingFrac()
		if !rangeMatch(w.QuotaRemaining, frac) {
			return false
		}
	}
	if w.QuotaConfidence != "" {
		if string(st.QuotaConf) != w.QuotaConfidence {
			return false
		}
	}
	if w.QuotaMult != nil {
		if !rangeMatch(w.QuotaMult, mult) {
			return false
		}
	}
	if w.ConsumerKey != "" && w.ConsumerKey != in.ConsumerKey {
		return false
	}
	return true
}

func inTimeWindow(tw *domain.TimeWindow, now interface {
	In(*time.Location) time.Time
}, loc *time.Location) bool {
	sh, sm, ok1 := parseHHMM2(tw.Start)
	eh, em, ok2 := parseHHMM2(tw.End)
	if !ok1 || !ok2 {
		return false
	}
	local := now.In(loc)
	cur := local.Hour()*60 + local.Minute()
	s, e := sh*60+sm, eh*60+em
	if s <= e {
		return cur >= s && cur < e
	}
	return cur >= s || cur < e
}

func parseHHMM2(s string) (int, int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil {
		return 0, 0, false
	}
	return h, m, true
}

func weekdayName(d time.Weekday) string {
	names := []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	return names[int(d)]
}

func rangeMatch(r *domain.FloatRange, v float64) bool {
	if r.Gte != nil && !(v >= *r.Gte) {
		return false
	}
	if r.Lte != nil && !(v <= *r.Lte) {
		return false
	}
	if r.Gt != nil && !(v > *r.Gt) {
		return false
	}
	if r.Lt != nil && !(v < *r.Lt) {
		return false
	}
	if r.Eq != nil && v != *r.Eq {
		return false
	}
	return true
}

func containsID(ids []domain.ResourceTargetID, id domain.ResourceTargetID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func availabilityExclusion(st *domain.TargetState, now time.Time) string {
	if st.Breaker == domain.BreakerOpen {
		return "circuit_breaker = open"
	}
	if !st.CooldownUntil.IsZero() && st.CooldownUntil.After(now) {
		return fmt.Sprintf("cooldown until %s", st.CooldownUntil.Format("15:04:05"))
	}
	if st.QuotaExhausted() {
		return "quota exhausted"
	}
	return ""
}

func cashCostOf(t *domain.CompiledTarget) float64 {
	// A target costs cash when its plan is PAYG or credits without a free
	// allowance. Subscription plans are pre-paid (no marginal cash).
	switch t.Plan.Kind {
	case domain.PlanPAYG:
		return 1 // nonzero marker; exact pricing lives in ledger layer
	case domain.PlanCredits:
		return 0.5
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Lexicographic ranking (F-P0-13): keys applied in declared order; each key
// produces a scalar where LOWER is better; ties fall through; final tie-break
// is target ID ascending.

type ranked struct {
	id         domain.ResourceTargetID
	keys       []float64
	scores     map[string]float64
	pinned     bool
	preference int
	priority   int
	targetStr  string
}

func rankTarget(in *PlanInput, id domain.ResourceTargetID, t *domain.CompiledTarget,
	st *domain.TargetState, obj *domain.ObjectiveProfile) ranked {

	mult := 1.0
	if t.Plan != nil {
		mult = t.Plan.CurrentMultiplier(in.State.Now)
	}
	scores := map[string]float64{}
	get := func(key string) float64 {
		if v, ok := scores[key]; ok {
			return v
		}
		v := computeScore(key, in, id, t, st, mult)
		scores[key] = v
		return v
	}

	keys := defaultRankKeys
	if obj != nil && len(obj.RankKeys) > 0 {
		keys = obj.RankKeys
	}
	rk := ranked{id: id, scores: scores, priority: t.Priority, targetStr: string(id)}
	for _, k := range keys {
		rk.keys = append(rk.keys, get(k))
	}
	return rk
}

var defaultRankKeys = []string{"priority", "min_effective_cost", "max_reliability", "min_latency"}

func computeScore(key string, in *PlanInput, id domain.ResourceTargetID,
	t *domain.CompiledTarget, st *domain.TargetState, mult float64) float64 {

	switch key {
	case "priority":
		return float64(t.Priority)
	case "min_cash_cost":
		return cashCostOf(t)
	case "min_effective_cost":
		// effective cost blends cash and quota pressure × multiplier
		cash := cashCostOf(t)
		quotaPressure := 1 - st.QuotaRemainingFrac()
		return cash*10 + quotaPressure*mult
	case "max_quota_expiry_pressure":
		// higher pressure (sooner reset with quota left) should be used first;
		// lower score = better, so invert.
		if st.QuotaResetAt.IsZero() {
			return 1
		}
		untilReset := st.QuotaResetAt.Sub(in.State.Now).Hours()
		if untilReset < 0 {
			untilReset = 0
		}
		rem := st.QuotaRemainingFrac()
		// burn-down urgency: lots of quota + little time => very negative
		return rem - 24/(untilReset+1)
	case "max_quota_remaining":
		return -st.QuotaRemainingFrac()
	case "min_latency":
		lat := st.LatencyEWMAms
		if lat <= 0 {
			lat = 1000 // neutral prior for unobserved targets
		}
		return lat
	case "max_reliability":
		errRate := st.ErrorRate1m
		penalty := 0.0
		switch st.Breaker {
		case domain.BreakerOpen:
			penalty = 10
		case domain.BreakerHalfOpen:
			penalty = 2
		}
		return errRate + penalty + float64(st.ConsecFails)*0.1
	case "capability_fit":
		// prefer targets whose context window is closest above requirement
		need := in.Required.MinContext
		have := targetCapabilities(in, t).ContextWindow
		if have <= 0 {
			return 0.5
		}
		ratio := float64(need) / float64(have)
		if ratio > 1 {
			ratio = 1
		}
		return ratio // closer to 1 = tighter fit = better (lower is better so invert)
	case "least_used":
		return float64(st.InFlight)
	default:
		return math.MaxFloat64 / 4 // unknown keys sort last but stable
	}
}

func sortRanked(list []ranked) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		// pinned targets always first within their tier
		if a.pinned != b.pinned {
			return a.pinned
		}
		// Preferred targets sort first and explicitly de-prioritized targets
		// sort last; zero retains objective ordering.
		if a.preference != b.preference {
			return a.preference > b.preference
		}
		for k := 0; k < len(a.keys) && k < len(b.keys); k++ {
			if a.keys[k] != b.keys[k] {
				return a.keys[k] < b.keys[k]
			}
		}
		return a.targetStr < b.targetStr // stable tie-break (G2)
	})
}
