package scheduler

import (
	"context"
	"maps"
	"time"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

type CapabilityVerifier func(context.Context, *domain.Provider, *domain.Connection, *domain.CompiledTarget, egress.Caller, domain.Protocol, domain.Requirements) domain.CapabilityVerification

// SetCapabilityVerifier is wired once before serving traffic. Dry runs never
// invoke it; the callback may send bounded synthetic validation requests.
func (s *Scheduler) SetCapabilityVerifier(verifier CapabilityVerifier) {
	s.capabilityVerifier = verifier
}

func (s *Scheduler) verifyRouteCapabilities(ctx context.Context, w *world, route domain.RouteSpec, rc *domain.RequestContract, required domain.Requirements, allowDegradation, exactOnly bool) *world {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	snapshot := *w.snap
	snapshot.Targets = maps.Clone(w.snap.Targets)
	local := &world{snap: &snapshot, callers: w.callers, cards: w.cards, keys: w.keys, routing: w.routing,
		routingErr: w.routingErr, routingCacheHit: w.routingCacheHit, routeTargets: w.routeTargets,
		capabilityVerifications: map[domain.ResourceTargetID]domain.CapabilityVerification{}}
	compat := newCompatibilityAssessor(ctx, w, rc, allowDegradation, exactOnly)
	checked := 0
	for _, step := range route.Steps {
		// Known capable candidates are considered before models needing probes.
		for _, supportedFirst := range []bool{true, false} {
			for _, id := range w.routeTargets[routeTargetKey{provider: step.ProviderID, model: step.UpstreamModel}] {
				target := snapshot.Targets[id]
				if target == nil || target.ProviderID != step.ProviderID || target.Model != step.UpstreamModel {
					continue
				}
				protocol := target.EgressProtocol(rc.Protocol)
				if target.CapabilitiesFor(protocol).Satisfies(required) != supportedFirst {
					continue
				}
				if _, done := local.capabilityVerifications[id]; done {
					continue
				}
				now := time.Now()
				state := s.manager.Snapshot(now)
				connections := s.manager.ConnectionStates(now)
				if v2EligibilityReason(w, target, rc.Protocol, domain.Requirements{}, state.Get(id), connections[target.ConnectionID], now) != "" {
					continue
				}
				if compat.assess(id).blocked {
					continue
				}
				if checked >= 3 || ctx.Err() != nil {
					return local
				}
				checked++
				result := s.capabilityVerifier(ctx, snapshot.Providers[target.ProviderID], snapshot.Connections[target.ConnectionID], target, w.callers[id], protocol, required)
				local.capabilityVerifications[id] = result
				copyTarget := *target
				copyTarget.ProtocolCapabilities = maps.Clone(target.ProtocolCapabilities)
				if copyTarget.ProtocolCapabilities == nil {
					copyTarget.ProtocolCapabilities = map[domain.Protocol]domain.Capabilities{}
				}
				copyTarget.ProtocolCapabilities[protocol] = result.Capabilities
				copyTarget.ProtocolCapabilityStatuses = maps.Clone(target.ProtocolCapabilityStatuses)
				if copyTarget.ProtocolCapabilityStatuses == nil {
					copyTarget.ProtocolCapabilityStatuses = map[domain.Protocol]domain.CapabilityStatuses{}
				}
				copyTarget.ProtocolCapabilityStatuses[protocol] = result.Statuses
				snapshot.Targets[id] = &copyTarget
				if copyTarget.CapabilitiesFor(protocol).Satisfies(required) {
					// Do not probe speculative fallback routes once this request has
					// a usable route. Other already-capable fallbacks remain intact.
					return local
				}
			}
		}
	}
	return local
}
