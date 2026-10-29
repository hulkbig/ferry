package config

import (
	_ "embed"
	"encoding/json"
	"net/url"
	"strings"
)

//go:embed logical_model_policy.json
var logicalModelPolicyJSON []byte

// This source-controlled allowlist scopes the configurable catalog-size limit.
// Provider names and model prefixes are not reliable provider identities.
var logicalModelLimitedHosts = func() map[string]bool {
	var policy struct {
		Hosts []string `json:"limited_provider_hosts"`
	}
	if err := json.Unmarshal(logicalModelPolicyJSON, &policy); err != nil {
		panic(err)
	}
	hosts := make(map[string]bool, len(policy.Hosts))
	for _, host := range policy.Hosts {
		if host == "" || host != strings.ToLower(host) || strings.ContainsAny(host, "/:@ \t\n") {
			panic("invalid logical model policy host")
		}
		hosts[host] = true
	}
	return hosts
}()

func logicalModelLimitedURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	return logicalModelLimitedHosts[strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")]
}

func limitedLogicalModelProviders(f *File) map[string]bool {
	limited := make(map[string]bool)
	for _, provider := range f.Providers {
		if logicalModelLimitedURL(provider.BaseURL) {
			limited[provider.ID] = true
		}
	}
	// Connection overrides can point at an aggregator even when Provider.BaseURL
	// is empty. Count all distinct models across the provider's connections.
	for _, connection := range f.Connections {
		if logicalModelLimitedURL(connection.BaseURL) || logicalModelLimitedURL(connection.FullURL) {
			limited[connection.ProviderID] = true
		}
	}
	return limited
}
