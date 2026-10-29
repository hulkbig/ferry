package ingress

import (
	"net/http"

	sched "ferry/internal/scheduler"
)

// These headers describe the committed attempt's echo contract. They do not
// confirm priority execution (the terminal response may not have arrived yet).
func writeServiceTierHeaders(w http.ResponseWriter, res *sched.Result) {
	if res == nil || res.ServiceTierEvidence == nil {
		return
	}
	w.Header().Set("X-SmartAPI-Response-Tier-Authority", string(res.ServiceTierEvidence.ResponseTierAuthority))
	w.Header().Set("X-Ferry-Response-Tier-Authority", string(res.ServiceTierEvidence.ResponseTierAuthority))
	w.Header().Set("X-SmartAPI-Response-Tier-Source", res.ServiceTierEvidence.Source)
	w.Header().Set("X-Ferry-Response-Tier-Source", res.ServiceTierEvidence.Source)
}
