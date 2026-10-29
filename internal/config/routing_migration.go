package config

import (
	"fmt"
	"sort"

	"ferry/internal/domain"
)

// MigrateLegacyRoutingPolicies materializes a rule-free legacy DefaultPolicy
// as one ordered catch-all rule. A policy with conditional rules must remain on
// the legacy engine until the V2 projection proves that every rule can be
// represented; flattening it to a catch-all would silently discard behavior.
// Repeated calls are no-ops. Configurations written by older releases are also
// repaired when their generated catch-all can be identified exactly.
func MigrateLegacyRoutingPolicies(f *File) (bool, error) {
	if f == nil || len(f.Policies) == 0 {
		return false, nil
	}
	if len(f.RoutingPolicies) > 0 {
		name, legacy, ok := legacyDefaultPolicy(f)
		if !ok || len(legacy.Rules) == 0 {
			return false, nil
		}
		generated, err := materializeLegacyRoutingPolicy(name, legacy)
		if err != nil || len(f.RoutingPolicies) != 1 || !sameGeneratedLegacyRoutingPolicy(f.RoutingPolicies[0], generated) {
			return false, nil
		}
		// Older versions generated this catch-all even when legacy rules were
		// present. Remove only the exact generated shape so the rule-bearing
		// legacy policy remains the runtime source of truth.
		f.RoutingPolicies = nil
		return true, nil
	}
	name, legacy, ok := legacyDefaultPolicy(f)
	if !ok {
		return false, fmt.Errorf("legacy default policy %q not found", f.DefaultPolicy)
	}
	if len(legacy.Rules) > 0 {
		return false, nil
	}
	spec, err := materializeLegacyRoutingPolicy(name, legacy)
	if err != nil {
		return false, err
	}
	f.RoutingPolicies = []domain.RoutingPolicySpec{spec}
	return true, nil
}

func legacyDefaultPolicy(f *File) (string, PolicyYAML, bool) {
	name := f.DefaultPolicy
	if name == "" && len(f.Policies) == 1 {
		for candidate := range f.Policies {
			name = candidate
		}
	}
	legacy, ok := f.Policies[name]
	return name, legacy, ok
}

func materializeLegacyRoutingPolicy(name string, legacy PolicyYAML) (domain.RoutingPolicySpec, error) {
	spec := domain.RoutingPolicySpec{
		ID:       name,
		Name:     name,
		Priority: 10,
		Enabled:  true,
		Revision: 1,
		Match:    domain.RoutingPolicyMatch{Timezone: legacy.Timezone},
	}
	for _, tier := range legacy.Tiers {
		step := domain.RoutingPolicyStep{ChannelGroup: tier.Pool, Objective: tier.Objective}
		for failure, action := range legacy.Recovery {
			ft := domain.FailureType(failure)
			if action.NextTier && domain.FallbackAllowed(ft) {
				step.RecoverOn = append(step.RecoverOn, ft)
			}
		}
		sort.Slice(step.RecoverOn, func(i, j int) bool { return step.RecoverOn[i] < step.RecoverOn[j] })
		spec.Route.Steps = append(spec.Route.Steps, step)
	}
	if len(spec.Route.Steps) == 0 {
		return domain.RoutingPolicySpec{}, fmt.Errorf("legacy policy %q has no route tiers", name)
	}
	hash, err := domain.RoutingPolicyContentHash(spec)
	if err != nil {
		return domain.RoutingPolicySpec{}, err
	}
	spec.ContentHash = hash
	return spec, nil
}

func sameGeneratedLegacyRoutingPolicy(actual, generated domain.RoutingPolicySpec) bool {
	actualHash, actualErr := domain.RoutingPolicyContentHash(actual)
	generatedHash, generatedErr := domain.RoutingPolicyContentHash(generated)
	return actualErr == nil && generatedErr == nil && actualHash == generatedHash &&
		actual.ID == generated.ID && actual.Name == generated.Name && actual.Description == "" &&
		actual.Priority == generated.Priority && actual.Enabled == generated.Enabled && actual.Revision == generated.Revision
}
