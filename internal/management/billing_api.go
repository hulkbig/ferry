package management

import (
	"net/http"

	"ferry/internal/billing/pricingsync"
	"ferry/internal/domain"
)

// Billing management API: platform plan template catalog and pricing sync.
// Pricing sync only proposes rate cards (new/changed diff against the active
// set); applying them stays an explicit config/draft operation.

// handleBillingPlanTemplates lists the built-in platform-aligned plan
// templates with their materialized buckets.
func (s *Server) handleBillingPlanTemplates(w http.ResponseWriter, _ *http.Request) {
	type bucketView struct {
		ID            string             `json:"id"`
		Unit          domain.QuotaUnit   `json:"unit"`
		LimitPerCycle float64            `json:"limit_per_cycle"`
		Refresh       domain.RefreshSpec `json:"refresh"`
	}
	type templateView struct {
		ID             string                    `json:"id"`
		ProviderFamily string                    `json:"provider_family"`
		Kind           domain.PlanKind           `json:"kind"`
		Currency       string                    `json:"currency"`
		Fee            float64                   `json:"fee"`
		Timezone       string                    `json:"timezone,omitempty"`
		Windows        []domain.MultiplierWindow `json:"windows,omitempty"`
		Buckets        []bucketView              `json:"buckets"`
		Note           string                    `json:"note,omitempty"`
	}
	out := make([]templateView, 0)
	for _, id := range domain.PlanTemplateIDs() {
		t, _ := domain.LookupPlanTemplate(id)
		view := templateView{
			ID: t.ID, ProviderFamily: t.ProviderFamily, Kind: t.Kind,
			Currency: t.Currency, Fee: t.Fee, Timezone: t.Timezone,
			Windows: t.Windows, Note: t.Note,
		}
		for _, b := range t.Buckets {
			view.Buckets = append(view.Buckets, bucketView{
				ID: domain.TemplateBucketID(t.ID, b.ID), Unit: b.Unit,
				LimitPerCycle: b.LimitPerCycle, Refresh: b.Refresh,
			})
		}
		out = append(out, view)
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// handleBillingPricingSync fetches a public pricing catalog and returns the
// proposed rate cards plus a diff against the currently active cards.
// GET /api/billing/pricing-sync?source=models.dev
func (s *Server) handleBillingPricingSync(w http.ResponseWriter, r *http.Request) {
	sourceName := r.URL.Query().Get("source")
	if sourceName == "" {
		sourceName = pricingsync.DefaultSources[0].Name
	}
	source, err := pricingsync.LookupSource(sourceName)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := pricingsync.Fetch(r.Context(), source)
	if err != nil {
		writeMgmtErr(w, http.StatusBadGateway, "pricing sync: "+err.Error())
		return
	}
	var active []domain.RateCard
	if s.deps.Scheduler != nil {
		for _, c := range s.deps.Scheduler.RateCards() {
			if c != nil {
				active = append(active, *c)
			}
		}
	}
	newCards, changed := pricingsync.Diff(result.Cards, active)
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"source": result.Source, "url": result.URL,
		"fetched_at": result.FetchedAt, "version": result.Version,
		"total": len(result.Cards), "cards": result.Cards,
		"diff":     map[string]any{"new": newCards, "changed": changed},
		"warnings": result.Warnings,
		"note":     "proposal only — apply via config/draft, nothing is mutated by this call",
	})
}
