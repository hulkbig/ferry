package domain

import "fmt"

// PlanTemplate is a built-in, platform-aligned billing preset. Referencing a
// template from a plan (`template: claude-pro`) materializes its kind, fee,
// quota buckets and multiplier windows into ordinary config objects, so every
// downstream consumer (compiler, runtime, console) sees a normal plan.
// Sources and caveats: docs/billing/platform-plan-matrix.md (2026-09-04).
type PlanTemplate struct {
	// ID names the template ("claude-pro", "codex-plus", ...).
	ID string
	// ProviderFamily is the platform the preset aligns with (documentation).
	ProviderFamily string
	Kind           PlanKind
	Currency       string
	Fee            float64
	Timezone       string
	Windows        []MultiplierWindow
	// Buckets are materialized with stable IDs "<template>:<bucket-id>".
	Buckets   []QuotaBucket
	QuotaRefs []QuotaRef
	Note      string
}

// planTemplates is the built-in registry. Limits are best-effort public
// estimates — operators should recalibrate from observed consumption.
var planTemplates = map[string]PlanTemplate{
	"codex-go": {
		ID: "codex-go", ProviderFamily: "openai", Kind: PlanSubscription,
		Currency: "USD", Fee: 8,
		Buckets: []QuotaBucket{
			{ID: "5h-credits", Unit: QuotaUnitCredits, LimitPerCycle: 30, Refresh: RefreshSpec{Period: "5h", Rolling: true}},
			{ID: "weekly-credits", Unit: QuotaUnitCredits, LimitPerCycle: 300, Refresh: RefreshSpec{Period: "weekly", At: "04:00"}},
		},
		Note: "ChatGPT Go: credits metered at $0.04; 5h rolling + weekly.",
	},
	"codex-plus": {
		ID: "codex-plus", ProviderFamily: "openai", Kind: PlanSubscription,
		Currency: "USD", Fee: 20,
		Buckets: []QuotaBucket{
			{ID: "5h-credits", Unit: QuotaUnitCredits, LimitPerCycle: 125, Refresh: RefreshSpec{Period: "5h", Rolling: true}},
			{ID: "weekly-credits", Unit: QuotaUnitCredits, LimitPerCycle: 1500, Refresh: RefreshSpec{Period: "weekly", At: "04:00"}},
		},
		Note: "ChatGPT Plus: recalibrate limits from the account usage snapshot.",
	},
	"codex-pro": {
		ID: "codex-pro", ProviderFamily: "openai", Kind: PlanSubscription,
		Currency: "USD", Fee: 100,
		Buckets: []QuotaBucket{
			{ID: "5h-credits", Unit: QuotaUnitCredits, LimitPerCycle: 750, Refresh: RefreshSpec{Period: "5h", Rolling: true}},
			{ID: "weekly-credits", Unit: QuotaUnitCredits, LimitPerCycle: 10000, Refresh: RefreshSpec{Period: "weekly", At: "04:00"}},
		},
		Note: "ChatGPT Pro: Enterprise-style accounts should switch to pure rate-card PAYG with no fixed buckets.",
	},
	"claude-pro": {
		ID: "claude-pro", ProviderFamily: "anthropic", Kind: PlanSubscription,
		Currency: "USD", Fee: 20,
		Buckets: []QuotaBucket{
			{ID: "5h-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 45, Refresh: RefreshSpec{Period: "5h", Rolling: true}},
			{ID: "weekly-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 700, Refresh: RefreshSpec{Period: "weekly", At: "04:00"}},
		},
		Note: "Claude Pro: 5-hour session window from first message + weekly; Opus drains faster.",
	},
	"claude-max-5x": {
		ID: "claude-max-5x", ProviderFamily: "anthropic", Kind: PlanSubscription,
		Currency: "USD", Fee: 100,
		Buckets: []QuotaBucket{
			{ID: "5h-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 225, Refresh: RefreshSpec{Period: "5h", Rolling: true}},
			{ID: "weekly-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 3500, Refresh: RefreshSpec{Period: "weekly", At: "04:00"}},
		},
		Note: "Claude Max 5x.",
	},
	"claude-max-20x": {
		ID: "claude-max-20x", ProviderFamily: "anthropic", Kind: PlanSubscription,
		Currency: "USD", Fee: 200,
		Buckets: []QuotaBucket{
			{ID: "5h-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 900, Refresh: RefreshSpec{Period: "5h", Rolling: true}},
			{ID: "weekly-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 14000, Refresh: RefreshSpec{Period: "weekly", At: "04:00"}},
		},
		Note: "Claude Max 20x.",
	},
	"glm-lite": {
		ID: "glm-lite", ProviderFamily: "zhipu", Kind: PlanSubscription,
		Currency: "CNY", Fee: 118, Timezone: "Asia/Shanghai",
		Windows: []MultiplierWindow{{Start: "00:00", End: "08:00", Multiplier: 0.5}},
		Buckets: []QuotaBucket{
			{ID: "5h-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 60, Refresh: RefreshSpec{Period: "5h", Rolling: true, Timezone: "Asia/Shanghai"}},
			{ID: "weekly-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 2000, Refresh: RefreshSpec{Period: "weekly", At: "00:00", Timezone: "Asia/Shanghai"}},
		},
		Note: "GLM Coding Plan Lite: quota restores on 5h cycles; off-peak (Beijing 00:00-08:00) counts half.",
	},
	"glm-pro": {
		ID: "glm-pro", ProviderFamily: "zhipu", Kind: PlanSubscription,
		Currency: "CNY", Fee: 538, Timezone: "Asia/Shanghai",
		Windows: []MultiplierWindow{{Start: "00:00", End: "08:00", Multiplier: 0.5}},
		Buckets: []QuotaBucket{
			{ID: "5h-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 360, Refresh: RefreshSpec{Period: "5h", Rolling: true, Timezone: "Asia/Shanghai"}},
			{ID: "weekly-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 12000, Refresh: RefreshSpec{Period: "weekly", At: "00:00", Timezone: "Asia/Shanghai"}},
		},
		Note: "GLM Coding Plan Pro.",
	},
	"glm-max": {
		ID: "glm-max", ProviderFamily: "zhipu", Kind: PlanSubscription,
		Currency: "CNY", Fee: 1078, Timezone: "Asia/Shanghai",
		Windows: []MultiplierWindow{{Start: "00:00", End: "08:00", Multiplier: 0.5}},
		Buckets: []QuotaBucket{
			{ID: "5h-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 840, Refresh: RefreshSpec{Period: "5h", Rolling: true, Timezone: "Asia/Shanghai"}},
			{ID: "weekly-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 28000, Refresh: RefreshSpec{Period: "weekly", At: "00:00", Timezone: "Asia/Shanghai"}},
		},
		Note: "GLM Coding Plan Max.",
	},
	"kimi-subscription": {
		ID: "kimi-subscription", ProviderFamily: "moonshot", Kind: PlanSubscription,
		Currency: "CNY", Fee: 49,
		Buckets: []QuotaBucket{
			{ID: "5h-prompts", Unit: QuotaUnitPrompts, LimitPerCycle: 60, Refresh: RefreshSpec{Period: "5h", Rolling: true}},
			{ID: "weekly-quota", Unit: QuotaUnitPrompts, LimitPerCycle: 1000, Refresh: RefreshSpec{Period: "weekly", At: "04:00"}},
		},
		Note: "Kimi Code Andante ¥49 baseline; top-up packs are payg (rate card ≈ open-platform API prices).",
	},
}

// PlanTemplateIDs lists available template ids.
func PlanTemplateIDs() []string { return SortIDs(keys(planTemplates)) }

// LookupPlanTemplate returns the built-in template by id.
func LookupPlanTemplate(id string) (PlanTemplate, bool) {
	t, ok := planTemplates[id]
	return t, ok
}

// TemplateBucketID builds the stable materialized id of a template bucket.
func TemplateBucketID(templateID, bucketID string) string {
	return templateID + ":" + bucketID
}

// Apply fills the plan's unset billing fields from the template and returns
// the materialized quota buckets (IDs namespaced under the template). Fields
// already set on the plan win, so templates are defaults, not overrides.
func (t PlanTemplate) Apply(p *Plan) ([]QuotaBucket, error) {
	if p == nil {
		return nil, fmt.Errorf("plan is required")
	}
	if p.Kind == "" {
		p.Kind = t.Kind
	}
	if p.Currency == "" {
		p.Currency = t.Currency
	}
	if p.Fee == 0 {
		p.Fee = t.Fee
	}
	if p.Timezone == "" {
		p.Timezone = t.Timezone
	}
	if len(p.Windows) == 0 {
		p.Windows = append([]MultiplierWindow(nil), t.Windows...)
	}
	var buckets []QuotaBucket
	for _, b := range t.Buckets {
		materialized := b
		materialized.ID = TemplateBucketID(t.ID, b.ID)
		if materialized.Refresh.Timezone == "" && p.Timezone != "" {
			materialized.Refresh.Timezone = p.Timezone
		}
		buckets = append(buckets, materialized)
		if p.Quota == nil {
			ref := QuotaRef{BucketID: materialized.ID}
			p.Quota = &ref
			continue
		}
		if !containsQuotaRef(p, materialized.ID) {
			p.Quotas = append(p.Quotas, QuotaRef{BucketID: materialized.ID})
		}
	}
	return buckets, nil
}

func containsQuotaRef(p *Plan, bucketID string) bool {
	if p.Quota != nil && p.Quota.BucketID == bucketID {
		return true
	}
	for _, r := range p.Quotas {
		if r.BucketID == bucketID {
			return true
		}
	}
	return false
}

func keys(m map[string]PlanTemplate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
