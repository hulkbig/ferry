package scheduler

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ferry/internal/domain"
	"ferry/internal/policy"
)

type v2RouteSelection struct {
	Route      domain.RouteSpec
	OverrideID string
}

func selectV2Route(model *domain.LogicalModel, callerID, callerName string, protocol domain.Protocol, required domain.Requirements, now time.Time, location *time.Location) v2RouteSelection {
	selected := v2RouteSelection{Route: cloneRoute(model.DefaultRoute)}
	for _, override := range model.Overrides {
		if !override.Enabled || !overrideMatches(override, callerID, callerName, protocol, required, now, location) {
			continue
		}
		selected.OverrideID = override.ID
		switch override.Mode {
		case domain.OverrideCustomRoute:
			if override.Route != nil {
				selected.Route = cloneRoute(*override.Route)
			}
		case domain.OverrideAdjustDefault:
			selected.Route.Steps = adjustRoute(selected.Route.Steps, override.Prefer, override.Avoid)
		}
		return selected
	}
	return selected
}

func cloneRoute(route domain.RouteSpec) domain.RouteSpec {
	out := domain.RouteSpec{Steps: append([]domain.RouteStep(nil), route.Steps...)}
	for i := range out.Steps {
		if out.Steps[i].RecoverOn != nil {
			out.Steps[i].RecoverOn = append(make([]domain.FailureType, 0, len(out.Steps[i].RecoverOn)), out.Steps[i].RecoverOn...)
		}
	}
	return out
}

func overrideMatches(override domain.OverrideSpec, callerID, callerName string, protocol domain.Protocol, required domain.Requirements, now time.Time, location *time.Location) bool {
	if len(override.Callers) > 0 {
		found := false
		for _, candidate := range override.Callers {
			found = found || candidate == callerID || candidate == callerName
		}
		if !found {
			return false
		}
	}
	if len(override.Protocols) > 0 {
		found := false
		for _, candidate := range override.Protocols {
			found = found || candidate == protocol
		}
		if !found {
			return false
		}
	}
	overrideLocation := location
	if override.Timezone != "" {
		configured, err := time.LoadLocation(override.Timezone)
		if err != nil {
			return false
		}
		overrideLocation = configured
	}
	if overrideLocation == nil {
		overrideLocation = time.UTC
	}
	if len(override.Weekdays) > 0 {
		weekday := strings.ToLower(now.In(overrideLocation).Weekday().String()[:3])
		found := false
		for _, candidate := range override.Weekdays {
			found = found || strings.ToLower(strings.TrimSpace(candidate)) == weekday
		}
		if !found {
			return false
		}
	}
	if override.Window != nil && !matchesWindow(*override.Window, now, overrideLocation) {
		return false
	}
	return required.Contains(override.Required)
}

func matchesWindow(window domain.TimeWindow, now time.Time, location *time.Location) bool {
	parse := func(value string) (int, bool) {
		if len(value) != 5 || value[2] != ':' {
			return 0, false
		}
		var hour, minute int
		if _, err := fmt.Sscanf(value, "%02d:%02d", &hour, &minute); err != nil ||
			hour > 23 || minute > 59 {
			return 0, false
		}
		return hour*60 + minute, true
	}
	start, okStart := parse(window.Start)
	end, okEnd := parse(window.End)
	if !okStart || !okEnd {
		return false
	}
	if location == nil {
		location = time.UTC
	}
	configuredNow := now.In(location)
	current := configuredNow.Hour()*60 + configuredNow.Minute()
	if start <= end {
		return current >= start && current < end
	}
	return current >= start || current < end
}

func adjustRoute(base, prefer, avoid []domain.RouteStep) []domain.RouteStep {
	preferred := map[string]bool{}
	for _, step := range prefer {
		preferred[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)] = true
	}
	avoided := map[string]bool{}
	for _, step := range avoid {
		avoided[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)] = true
	}
	out := make([]domain.RouteStep, 0, len(base))
	for _, wantPreferred := range []bool{true, false} {
		for _, step := range base {
			key := domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)
			if avoided[key] || preferred[key] != wantPreferred {
				continue
			}
			out = append(out, step)
		}
	}
	return out
}

type connectionCandidate struct {
	id       domain.ResourceTargetID
	semantic int
	compat   int
	quota    float64
	throttle float64
	health   float64
	fairness int64
}

func semanticCompatibilityPriority(assessment compatAssessment) int {
	if len(assessment.notes) == 0 {
		return 0
	}
	switch worstNoteLevel(assessment.notes) {
	case domain.CompatMapped:
		return 1
	case domain.CompatDegraded:
		return 2
	default:
		return 3
	}
}

func protocolCompatibilityPriority(level domain.CompatLevel) int {
	switch level {
	case domain.CompatExact:
		return 0
	case domain.CompatMapped:
		return 1
	case domain.CompatDegraded:
		return 2
	default:
		return 3
	}
}

func planV2(w *world, logical *domain.LogicalModel, route domain.RouteSpec, ingress domain.Protocol,
	required domain.Requirements, compat *compatibilityAssessor,
	state *domain.Snapshot, connectionStates map[string]domain.ConnectionState,
	binding *domain.AffinityBinding) (*policy.PlanResult, bool, error) {

	result := &policy.PlanResult{}
	if err := compat.ctx.Err(); err != nil {
		return result, false, err
	}
	for routeIndex, step := range route.Steps {
		tier := fmt.Sprintf("route-%02d", routeIndex+1)
		result.Tiers = append(result.Tiers, tier)
		eligible := []connectionCandidate{}
		for _, id := range w.routeTargets[routeTargetKey{provider: step.ProviderID, model: step.UpstreamModel}] {
			if err := compat.ctx.Err(); err != nil {
				return result, false, err
			}
			target := w.snap.Targets[id]
			if target == nil || target.ProviderID != step.ProviderID || target.Model != step.UpstreamModel {
				continue
			}
			record := domain.CandidateRecord{Target: id, Tier: tier}
			record.CapabilityStatuses = target.CapabilityStatusesFor(target.EgressProtocol(ingress))
			if verification, ok := w.capabilityVerifications[id]; ok {
				record.CapabilityVerificationError = verification.Error
				record.CapabilityProbeRequests = verification.Probes
			}
			reason := v2EligibilityReason(w, target, ingress, required, state.Get(id), connectionStates[target.ConnectionID], state.Now)
			var assessment compatAssessment
			if reason == "" {
				assessment = compat.assess(id)
				if err := compat.ctx.Err(); err != nil {
					return result, false, err
				}
				if assessment.blocked {
					reason = assessment.reason
				}
			}
			if reason != "" {
				record.ExclusionReason = reason
				result.Candidates = append(result.Candidates, record)
				continue
			}
			connection := connectionStates[target.ConnectionID]
			targetState := state.Get(id)
			candidate := connectionCandidate{
				id: id, semantic: semanticCompatibilityPriority(assessment),
				compat: protocolCompatibilityPriority(assessment.level), quota: connectionQuotaScore(connection),
				throttle: connection.RecentThrottleRate,
				health:   targetState.ErrorRate1m + float64(targetState.ConsecFails),
				fairness: connection.InFlight,
			}
			record.Eligible = true
			record.Scores = map[string]float64{
				"semantic_compatibility": float64(candidate.semantic),
				"protocol_compatibility": float64(candidate.compat),
				"quota_pressure":         candidate.quota, "recent_throttling": candidate.throttle,
				"health": candidate.health, "fairness": float64(candidate.fairness),
			}
			eligible = append(eligible, candidate)
			result.Candidates = append(result.Candidates, record)
		}
		sort.SliceStable(eligible, func(i, j int) bool {
			left, right := eligible[i], eligible[j]
			switch {
			case left.semantic != right.semantic:
				return left.semantic < right.semantic
			case left.compat != right.compat:
				return left.compat < right.compat
			case left.quota != right.quota:
				return left.quota < right.quota
			case left.throttle != right.throttle:
				return left.throttle < right.throttle
			case left.health != right.health:
				return left.health < right.health
			case left.fairness != right.fairness:
				return left.fairness < right.fairness
			default:
				return left.id < right.id
			}
		})
		segment := make([]domain.ResourceTargetID, 0, len(eligible))
		for rank, candidate := range eligible {
			segment = append(segment, candidate.id)
			for i := range result.Candidates {
				if result.Candidates[i].Target == candidate.id && result.Candidates[i].Tier == tier {
					result.Candidates[i].Rank = rank + 1
				}
			}
		}
		if len(segment) > 0 {
			result.OrderTiers = append(result.OrderTiers, segment)
			result.OrderTierPolicyIndexes = append(result.OrderTierPolicyIndexes, routeIndex)
			result.Order = append(result.Order, segment...)
		}
	}
	if len(result.Order) == 0 {
		excluded := make([]domain.ExclusionRecord, 0, len(result.Candidates))
		for _, candidate := range result.Candidates {
			excluded = append(excluded, domain.ExclusionRecord{
				Target: candidate.Target, Tier: candidate.Tier, Reason: candidate.ExclusionReason,
			})
		}
		return result, false, &domain.ErrNoCandidate{
			Alias: logical.ID, Reason: "all provider connections are ineligible", Excluded: excluded,
		}
	}
	affinityReused := applyAffinity(result, w.snap, binding)
	result.Primary = result.Order[0]
	return result, affinityReused, nil
}

func applyAffinity(result *policy.PlanResult, snap *domain.CompiledSnapshot, binding *domain.AffinityBinding) bool {
	if result == nil || binding == nil {
		return false
	}
	segmentIndex, position := -1, -1
	for i, segment := range result.OrderTiers {
		for j, id := range segment {
			target := snap.Targets[id]
			if target != nil && target.ProviderID == binding.ProviderID && target.Model == binding.UpstreamModel &&
				target.ConnectionID == binding.ConnectionID {
				segmentIndex, position = i, j
				break
			}
		}
	}
	if segmentIndex < 0 || (binding.Scope != domain.AffinityConversation && segmentIndex != 0) {
		return false
	}
	// Conversation affinity resumes at its current route step. Earlier
	// default steps are deliberately not retained as fallback candidates: that
	// would be proactive failback for an existing conversation.
	if binding.Scope == domain.AffinityConversation && segmentIndex > 0 {
		result.OrderTiers = result.OrderTiers[segmentIndex:]
		result.OrderTierPolicyIndexes = result.OrderTierPolicyIndexes[segmentIndex:]
		segmentIndex = 0
	}
	segment := result.OrderTiers[segmentIndex]
	chosen := segment[position]
	if position > 0 {
		copy(segment[1:position+1], segment[:position])
		segment[0] = chosen
	}
	result.Order = result.Order[:0]
	for _, current := range result.OrderTiers {
		result.Order = append(result.Order, current...)
	}
	ranks := make(map[domain.ResourceTargetID]int, len(result.Order))
	for _, current := range result.OrderTiers {
		for rank, id := range current {
			ranks[id] = rank + 1
		}
	}
	for i := range result.Candidates {
		if rank, ok := ranks[result.Candidates[i].Target]; ok {
			result.Candidates[i].Rank = rank
		} else if result.Candidates[i].Eligible {
			// Conversation affinity can intentionally resume at a later route
			// step, so earlier eligible steps are not in the effective order.
			result.Candidates[i].Rank = 0
		}
		if result.Candidates[i].Target == chosen {
			if result.Candidates[i].Scores == nil {
				result.Candidates[i].Scores = map[string]float64{}
			}
			result.Candidates[i].Scores["affinity_reused"] = 1
		}
	}
	return true
}

func v2EligibilityReason(w *world, target *domain.CompiledTarget, ingress domain.Protocol, required domain.Requirements,
	targetState *domain.TargetState, connection domain.ConnectionState, now time.Time) string {
	provider := w.snap.Providers[target.ProviderID]
	if provider == nil || !provider.Enabled {
		return "provider disabled"
	}
	configured := w.snap.Connections[target.ConnectionID]
	if configured == nil || !configured.Enabled || !connection.Enabled || target.Disabled {
		return "connection disabled"
	}
	if w.callers[target.ID] == nil {
		return "connection credential unavailable"
	}
	if connection.Auth == domain.ConnectionAuthError {
		return "connection authentication error"
	}
	if connection.Reachability == domain.ConnectionOffline {
		return "connection offline"
	}
	concurrency := configured.Concurrency
	if concurrency == nil {
		concurrency = provider.Concurrency
	}
	if concurrency == nil {
		concurrency = w.snap.Concurrency
	}
	// A queue-on-throttle policy keeps the connection eligible so admission
	// can wait out its cooldown. Fail-fast policies exclude it here and let
	// route fallback proceed without starting an upstream attempt.
	if connection.CoolingUntil.After(now) && !concurrency.QueuesOnThrottle() {
		return fmt.Sprintf("connection cooling until %s", connection.CoolingUntil.Format(time.RFC3339))
	}
	for _, window := range connection.QuotaWindows {
		if window.Availability == domain.QuotaExhausted && (window.ResetAt.IsZero() || window.ResetAt.After(now)) {
			return "connection quota exhausted"
		}
	}
	capabilities := target.CapabilitiesFor(target.EgressProtocol(ingress))
	if !capabilities.Satisfies(required) {
		reason := v2CapabilityMismatchReason(capabilities, required)
		states := target.CapabilityStatusesFor(target.EgressProtocol(ingress))
		unverified := []string{}
		for _, name := range domain.BooleanCapabilityRegistry() {
			need, _ := domain.RequirementCapability(required, name)
			if need && states[name] == domain.CapabilityUnknown {
				unverified = append(unverified, name)
			}
		}
		if len(unverified) > 0 {
			reason += "; unverified: " + strings.Join(unverified, ", ")
		}
		return reason
	}
	if targetState.Breaker == domain.BreakerOpen {
		return "connection model circuit breaker open"
	}
	if targetState.CooldownUntil.After(now) {
		queueableThrottle := targetState.LastFailure != nil &&
			targetState.LastFailure.Type == domain.FailRateLimit &&
			targetState.LastFailure.EffectiveScope() == domain.FailureScopeConnection &&
			concurrency.QueuesOnThrottle()
		if !queueableThrottle {
			return fmt.Sprintf("connection model cooling until %s", targetState.CooldownUntil.Format(time.RFC3339))
		}
	}
	return ""
}

func v2CapabilityMismatchReason(capabilities domain.Capabilities, required domain.Requirements) string {
	missing := []string{}
	for _, capability := range domain.BooleanCapabilityRegistry() {
		wants, _ := domain.RequirementCapability(required, capability)
		has, _ := domain.BooleanCapability(capabilities, capability)
		if wants && !has {
			missing = append(missing, capability)
		}
	}
	if required.MinContext > 0 && capabilities.ContextWindow > 0 && capabilities.ContextWindow < required.MinContext {
		missing = append(missing, fmt.Sprintf("context_window(%d<%d)", capabilities.ContextWindow, required.MinContext))
	}
	if required.MinOutput > 0 && capabilities.MaxOutput > 0 && capabilities.MaxOutput < required.MinOutput {
		missing = append(missing, fmt.Sprintf("max_output(%d<%d)", capabilities.MaxOutput, required.MinOutput))
	}
	if len(missing) == 0 {
		return "connection model capability mismatch"
	}
	return "connection model capability missing: " + strings.Join(missing, ", ")
}

func connectionQuotaScore(connection domain.ConnectionState) float64 {
	score := 0.5 // unknown is neutral rather than optimistically unlimited
	found := false
	for _, window := range connection.QuotaWindows {
		if window.RemainingFraction == nil {
			continue
		}
		pressure := 1 - *window.RemainingFraction
		if !found || pressure > score {
			score = pressure
		}
		found = true
	}
	return score
}
