package scheduler

import (
	"context"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

func (ex *execution) observeServiceTier(ctx context.Context, t *domain.CompiledTarget) context.Context {
	var providerType domain.ProviderType
	if provider := ex.d.w.snap.Providers[t.ProviderID]; provider != nil {
		providerType = provider.Type
	}
	ctx, ex.serviceTierObservation = egress.ObserveServiceTier(ctx, providerType)
	return ctx
}

func (ex *execution) serviceTierEvidence(success bool) *domain.ServiceTierEvidence {
	return ex.serviceTierObservation.Snapshot(ex.d.rec.ServiceTier, ex.terminalServiceTier, success)
}
