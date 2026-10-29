package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"ferry/internal/domain"
)

type serviceTierContextKey struct{}

// ServiceTierObservation is scoped to one scheduler attempt. It observes the
// serialized request handed to HTTP.Do, not a capability declaration or a promise
// that the upstream received it. It never retains payloads, headers or URLs.
type ServiceTierObservation struct {
	mu       sync.Mutex
	provider domain.ProviderType
	evidence *domain.ServiceTierEvidence
}

func ObserveServiceTier(ctx context.Context, provider domain.ProviderType) (context.Context, *ServiceTierObservation) {
	observation := &ServiceTierObservation{provider: provider}
	return context.WithValue(ctx, serviceTierContextKey{}, observation), observation
}

func (o *ServiceTierObservation) Snapshot(requested, returned string, success bool) *domain.ServiceTierEvidence {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.evidence == nil {
		return nil
	}
	return o.evidence.WithResponse(requested, returned, success)
}

func observeServiceTierRequest(ctx context.Context, req *http.Request, payload []byte) {
	o, _ := ctx.Value(serviceTierContextKey{}).(*ServiceTierObservation)
	if o == nil {
		return
	}
	var body struct {
		ServiceTier string `json:"service_tier"`
	}
	_ = json.Unmarshal(payload, &body)
	// Only retain validated tier names, never arbitrary user payload data.
	var tier string
	if normalized, err := domain.NormalizeServiceTier(body.ServiceTier); err == nil && normalized != "" && len(body.ServiceTier) <= 64 {
		tier = body.ServiceTier
	}
	authority, source := domain.ClassifyResponseTier(o.provider, req.URL.String())
	o.mu.Lock()
	o.evidence = &domain.ServiceTierEvidence{WireServiceTier: tier, ResponseTierAuthority: authority, Source: source}
	o.mu.Unlock()
}

func observeServiceTierResponse(ctx context.Context, request, final *http.Request) {
	o, _ := ctx.Value(serviceTierContextKey{}).(*ServiceTierObservation)
	if o == nil || final == nil || (final.Method == request.Method && final.URL.String() == request.URL.String()) {
		return
	}
	// A redirect is no longer the exact adapter destination/contract observed
	// above. Do not promote an echo from the redirected response to proof.
	o.mu.Lock()
	if o.evidence != nil {
		o.evidence.ResponseTierAuthority = domain.TierUnknown
		o.evidence.Source = "redirect"
	}
	o.mu.Unlock()
}
