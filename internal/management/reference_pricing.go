package management

import (
	"context"
	"ferry/internal/domain"
	"net/http"
	"time"
)

func (s *Server) StartReferencePricingSync(ctx context.Context) { s.pricing.Start(ctx) }
func (s *Server) handleReferencePricingGet(w http.ResponseWriter, r *http.Request) {
	writeMgmtJSON(w, http.StatusOK, s.pricing.Status(time.Now().UTC()))
}
func (s *Server) handleReferencePricingRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.pricing.Refresh(r.Context(), time.Now().UTC(), false); err != nil {
		writeMgmtJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "pricing_sync": s.pricing.Status(time.Now().UTC())})
		return
	}
	s.handleReferencePricingGet(w, r)
}

// Ledger reads restore historical semantics and retain explicit dialect flags.
func requestEligibleInput(r *domain.RequestRecord) int64 {
	return max(r.Usage.UncachedInput(), 0) + max(r.Usage.CacheReadTokens, 0) + max(r.Usage.CacheWriteTokens, 0)
}
func (s *Server) addReferencePricing(view map[string]any, r *domain.RequestRecord) {
	provider := ""
	// Codex uses OpenAI's public standard token value as reference, not the
	// account's subscription bill. Never infer a model alias from its label.
	if f := s.currentConfig(); f != nil {
		for _, p := range f.Providers {
			if p.ID == r.ProviderID && p.Type == domain.ProviderCliproxyCodex {
				provider = "openai"
				break
			}
		}
	}
	view["reference_pricing"] = s.pricing.Estimate(r.UpstreamModel, provider, requestEligibleInput(r), r.Usage.OutputTokens, r.Usage.CacheReadTokens, r.Usage.CacheWriteTokens, time.Now().UTC())
}
