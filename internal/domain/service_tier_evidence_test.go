package domain

import "testing"

func TestResponseTierAuthorityIsEndpointContractNotModelCapability(t *testing.T) {
	for _, tc := range []struct {
		url      string
		provider ProviderType
		want     ResponseTierAuthority
	}{
		{"https://api.openai.com/v1/responses", ProviderCustom, TierAuthoritative},
		{"https://api.openai.com:443/v1/chat/completions", ProviderCustom, TierAuthoritative},
		{"https://chatgpt.com/backend-api/codex/responses", ProviderCustom, TierInformational},
		{"http://local-codex/responses", ProviderCliproxyCodex, TierInformational},
		{"https://api.openai.com/v1/responses", ProviderCliproxyCodex, TierInformational},
		{"https://gateway.example/v1/responses", ProviderCustom, TierUnknown},
		{"http://api.openai.com/v1/responses", ProviderCustom, TierUnknown},
		{"https://api.openai.com:8443/v1/responses", ProviderCustom, TierUnknown},
		{"https://api.openai.com.evil.example/v1/responses", ProviderCustom, TierUnknown},
		{"https://api.openai.com@evil.example/v1/responses", ProviderCustom, TierUnknown},
		{"https://api.openai.com/proxy/v1/responses", ProviderCustom, TierUnknown},
		{"https://api.openai.com/v1/messages", ProviderCustom, TierUnknown},
		{"", ProviderCustom, TierUnknown},
	} {
		t.Run(string(tc.provider)+tc.url, func(t *testing.T) {
			got, _ := ClassifyResponseTier(tc.provider, tc.url)
			if got != tc.want {
				t.Fatalf("authority=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestPriorityConfirmationRequiresSuccessfulAuthoritativeWireEvidence(t *testing.T) {
	for _, authority := range []ResponseTierAuthority{TierAuthoritative, TierInformational, TierUnknown} {
		for _, wire := range []string{"", "fast", "priority"} {
			for _, requested := range []string{"", "fast", "priority", "default"} {
				for _, returned := range []string{"", "priority", "default", "flex", "unknown"} {
					for _, success := range []bool{false, true} {
						e := ServiceTierEvidence{WireServiceTier: wire, ResponseTierAuthority: authority}
						want := "unconfirmed"
						if requested != "fast" && requested != "priority" {
							want = "not_requested"
						} else if wire == "priority" && authority == TierAuthoritative && success {
							if returned == "priority" {
								want = "confirmed"
							} else if returned == "default" || returned == "flex" {
								want = "downgraded"
							}
						}
						got := e.WithResponse(requested, returned, success)
						if got.PriorityConfirmation != want || e.PriorityConfirmation != "" {
							t.Fatalf("evidence=%+v requested=%q returned=%q success=%v want=%q", got, requested, returned, success, want)
						}
					}
				}
			}
		}
	}
}
