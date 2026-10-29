package ingress

import (
	"net/http"
	"sort"
	"time"

	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
)

type logicalModelSource interface {
	Snapshot() *domain.CompiledSnapshot
}

type dataPlaneAuthenticator interface {
	Authenticate(meta sched.RequestMeta) bool
}

// ModelsHandler implements the standard OpenAI model discovery envelope. It
// exposes only enabled Logical Models, never Provider catalogs or private
// execution leaves.
type ModelsHandler struct{ Source any }

func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "GET only")
		return
	}
	authenticator, ok := h.Source.(dataPlaneAuthenticator)
	if !ok || !authenticator.Authenticate(requestMeta(r, nil)) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid or disabled consumer key")
		return
	}
	source, ok := h.Source.(logicalModelSource)
	if !ok || source.Snapshot() == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	snapshot := source.Snapshot()
	ids := make([]string, 0, len(snapshot.LogicalModels))
	for id, model := range snapshot.LogicalModels {
		if model != nil && model.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	created := snapshot.CreatedAt.Unix()
	if snapshot.CreatedAt.IsZero() {
		created = time.Now().Unix()
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id": id, "object": "model", "created": created, "owned_by": "ferry",
			"supported_service_tiers": modelServiceTiers(snapshot, snapshot.LogicalModels[id]),
			"response_tier_authority": modelResponseTierAuthority(snapshot, snapshot.LogicalModels[id]),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// Discovery describes configured capabilities, not momentary quota/health or
// a promise of latency. Require a priority-capable route for every enabled
// override that can affect this protocol, because discovery has no request
// body, time window, or conversation with which to select an override.
func modelServiceTiers(snapshot *domain.CompiledSnapshot, model *domain.LogicalModel) map[domain.Protocol][]string {
	tiers := make(map[domain.Protocol][]string)
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses} {
		tiers[protocol] = []string{}
		supported := true
		for _, route := range modelRoutes(model, protocol) {
			if !routeSupportsPriority(snapshot, model, route, protocol) {
				supported = false
				break
			}
		}
		if supported {
			tiers[protocol] = []string{"priority"}
		}
	}
	return tiers
}

func routeSupportsPriority(snapshot *domain.CompiledSnapshot, model *domain.LogicalModel, route domain.RouteSpec, protocol domain.Protocol) bool {
	for _, target := range routeTierTargets(snapshot, model, route, protocol) {
		egress := target.EgressProtocol(protocol)
		if (egress == domain.ProtocolOpenAIChat || egress == domain.ProtocolOpenAIResponses) && target.CapabilitiesFor(egress).Priority {
			return true
		}
	}
	return false
}

func routeTierTargets(snapshot *domain.CompiledSnapshot, model *domain.LogicalModel, route domain.RouteSpec, protocol domain.Protocol) []*domain.CompiledTarget {
	var targets []*domain.CompiledTarget
	for _, step := range route.Steps {
		provider := snapshot.Providers[step.ProviderID]
		if provider == nil || !provider.Enabled {
			continue
		}
		for _, target := range snapshot.Targets {
			if target == nil || target.Disabled || target.ProviderID != step.ProviderID || target.Model != step.UpstreamModel ||
				(target.Endpoint == nil && len(target.NativeProtocols) == 0) {
				continue
			}
			connection := snapshot.Connections[target.ConnectionID]
			if connection == nil || !connection.Enabled || !target.AcceptsProtocol(protocol) || (model.ExactOnly && !target.SpeaksNatively(protocol)) {
				continue
			}
			egress := target.EgressProtocol(protocol)
			capabilities := target.CapabilitiesFor(egress)
			if capabilities.Text {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

func modelRoutes(model *domain.LogicalModel, protocol domain.Protocol) []domain.RouteSpec {
	routes := []domain.RouteSpec{model.DefaultRoute}
	for _, override := range model.Overrides {
		if !override.Enabled || (len(override.Protocols) > 0 && !domain.HasProtocol(override.Protocols, protocol)) {
			continue
		}
		if override.Mode == domain.OverrideCustomRoute && override.Route != nil {
			routes = append(routes, *override.Route)
		} else if override.Mode == domain.OverrideAdjustDefault {
			avoided := map[string]bool{}
			for _, step := range override.Avoid {
				avoided[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)] = true
			}
			var route domain.RouteSpec
			for _, step := range model.DefaultRoute.Steps {
				if !avoided[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)] {
					route.Steps = append(route.Steps, step)
				}
			}
			routes = append(routes, route)
		}
	}
	return routes
}

// This is a conservative discovery contract across all configured routes, not
// the evidence of a particular request. Mixed/empty routes remain unknown.
func modelResponseTierAuthority(snapshot *domain.CompiledSnapshot, model *domain.LogicalModel) map[domain.Protocol]domain.ResponseTierAuthority {
	result := make(map[domain.Protocol]domain.ResponseTierAuthority)
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses} {
		var combined domain.ResponseTierAuthority
		for _, route := range modelRoutes(model, protocol) {
			targets := routeTierTargets(snapshot, model, route, protocol)
			if len(targets) == 0 {
				combined = domain.TierUnknown
			}
			for _, target := range targets {
				egress := target.EgressProtocol(protocol)
				if egress != domain.ProtocolOpenAIChat && egress != domain.ProtocolOpenAIResponses {
					combined = domain.TierUnknown
					continue
				}
				path := "/responses"
				if target.EgressProtocol(protocol) == domain.ProtocolOpenAIChat {
					path = "/chat/completions"
				}
				requestURL, _ := target.Endpoint.RequestURL(path)
				authority, _ := domain.ClassifyResponseTier(snapshot.Providers[target.ProviderID].Type, requestURL)
				if combined == "" {
					combined = authority
				} else if combined != authority {
					combined = domain.TierUnknown
				}
			}
		}
		if combined == "" {
			combined = domain.TierUnknown
		}
		result[protocol] = combined
	}
	return result
}
