package management

import (
	"context"
	"net/http"
	"time"

	"ferry/internal/domain"
	"ferry/internal/ledger"
)

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	from, to, err := v2Window(r)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	pricing := s.pricing.Snapshot(time.Now().UTC())
	catalogProviders := map[string]string{}
	if cfg := s.currentConfig(); cfg != nil {
		for _, p := range cfg.Providers {
			if p.Type == domain.ProviderCliproxyCodex {
				catalogProviders[p.ID] = "openai"
			}
		}
	}
	estimate := func(provider, model string, input, output, read, write int64) *float64 {
		return pricing.Estimate(model, catalogProviders[provider], input, output, read, write).Cost
	}
	result, err := s.deps.Ledger.QueryAnalyticsWithEstimates(ctx, from, to, r.URL.Query().Get("model"), r.URL.Query().Get("provider"), estimate)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "analytics query unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	result.ReferencePricing = &ledger.AnalyticsPricing{Source: pricing.Status.Source, Version: pricing.Status.Version, UpdatedAt: pricing.Status.LastSuccess, Stale: pricing.Status.Stale}
	writeMgmtJSON(w, http.StatusOK, result)
}
