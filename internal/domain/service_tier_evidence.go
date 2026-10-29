package domain

import "net/url"

// ResponseTierAuthority describes the upstream contract, not Fast capability.
type ResponseTierAuthority string

const (
	TierAuthoritative ResponseTierAuthority = "authoritative"
	TierInformational ResponseTierAuthority = "informational"
	TierUnknown       ResponseTierAuthority = "unknown"
)

// ServiceTierEvidence records an observed HTTP attempt. Nil evidence means no
// wire observation (including historical rows); it must never imply a downgrade.
type ServiceTierEvidence struct {
	WireServiceTier       string                `json:"wire_service_tier,omitempty"`
	ResponseTierAuthority ResponseTierAuthority `json:"response_tier_authority"`
	Source                string                `json:"source"`
	PriorityConfirmation  string                `json:"priority_confirmation"`
}

// ClassifyResponseTier uses the adapter contract and exact destination, never
// a model name or timing. Only the public OpenAI API contract is authoritative.
func ClassifyResponseTier(provider ProviderType, requestURL string) (ResponseTierAuthority, string) {
	if provider == ProviderCliproxyCodex {
		return TierInformational, "codex"
	}
	u, err := url.Parse(requestURL)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return TierUnknown, "unknown"
	}
	if u.Hostname() == "chatgpt.com" && u.Path == "/backend-api/codex/responses" {
		return TierInformational, "codex"
	}
	if u.Hostname() == "api.openai.com" && (u.Path == "/v1/responses" || u.Path == "/v1/chat/completions") {
		return TierAuthoritative, "openai_api"
	}
	return TierUnknown, "unknown"
}

// WithResponse returns an independent snapshot. Even a priority echo from an
// informational/unknown source cannot confirm delivery of the priority tier.
func (e ServiceTierEvidence) WithResponse(requested, returned string, success bool) *ServiceTierEvidence {
	e.PriorityConfirmation = "unconfirmed"
	if requested != "fast" && requested != "priority" {
		e.PriorityConfirmation = "not_requested"
	} else if success && e.WireServiceTier == "priority" && e.ResponseTierAuthority == TierAuthoritative {
		switch returned {
		case "priority":
			e.PriorityConfirmation = "confirmed"
		case "default", "flex":
			e.PriorityConfirmation = "downgraded"
		}
	}
	return &e
}
