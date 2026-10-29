package domain

import (
	"strings"
	"testing"
)

func TestRoutingPolicySemanticHashIgnoresMetadataAndSetOrdering(t *testing.T) {
	base := RoutingPolicySpec{
		ID:          "weekday-glm",
		Name:        "工作日下午避开 GLM",
		Description: "first revision",
		Priority:    10,
		Enabled:     true,
		Revision:    1,
		Match: RoutingPolicyMatch{
			Consumers:  []string{"internal", "batch"},
			Models:     []string{"glm-5", "glm-4.7"},
			Protocols:  []Protocol{ProtocolAnthropic, ProtocolOpenAIChat},
			Weekdays:   []string{"fri", "mon"},
			TimeWindow: &TimeWindow{Start: "12:00", End: "18:00"},
		},
		Route: RoutingPolicyRoute{Steps: []RoutingPolicyStep{{
			ChannelGroup: "primary",
			RecoverOn:    []FailureType{FailProvider5xx, FailConnect, FailRateLimit},
		}}},
	}
	variant := base
	variant.Name = "renamed in the console"
	variant.Description = "second revision"
	variant.Priority = 900
	variant.Enabled = false
	variant.Revision = 42
	variant.Match.Consumers = []string{"batch", "internal"}
	variant.Match.Models = []string{"glm-4.7", "glm-5"}
	variant.Match.Protocols = []Protocol{ProtocolOpenAIChat, ProtocolAnthropic}
	variant.Match.Weekdays = []string{"mon", "fri"}
	variant.Route.Steps[0].RecoverOn = []FailureType{FailRateLimit, FailConnect, FailProvider5xx}

	gotA, err := RoutingPolicyContentHash(base)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := RoutingPolicyContentHash(variant)
	if err != nil {
		t.Fatal(err)
	}
	if gotA != gotB {
		t.Fatalf("metadata and set ordering changed semantic hash: %s != %s", gotA, gotB)
	}
	if len(gotA) != 64 {
		t.Fatalf("hash length = %d, want full SHA-256", len(gotA))
	}

	variant.Route.Steps[0].ChannelGroup = "fallback"
	gotC, err := RoutingPolicyContentHash(variant)
	if err != nil {
		t.Fatal(err)
	}
	if gotC == gotA {
		t.Fatal("route change must change semantic hash")
	}
}

func TestValidateRoutingPoliciesRequiresUniquePriorityAndFinalCatchAll(t *testing.T) {
	valid := []RoutingPolicySpec{
		{ID: "specific", Priority: 10, Enabled: true, Match: RoutingPolicyMatch{Models: []string{"glm-5"}}, Route: routeToGroup("primary")},
		{ID: "fallback", Priority: 20, Enabled: true, Route: routeToGroup("fallback")},
	}
	if err := ValidateRoutingPolicies(valid); err != nil {
		t.Fatalf("valid ordered policy set rejected: %v", err)
	}

	duplicate := append([]RoutingPolicySpec(nil), valid...)
	duplicate[1].Priority = 10
	if err := ValidateRoutingPolicies(duplicate); err == nil {
		t.Fatal("duplicate priority accepted")
	}

	missingCatchAll := valid[:1]
	if err := ValidateRoutingPolicies(missingCatchAll); err == nil {
		t.Fatal("enabled policy set without final catch-all accepted")
	}

	notFinal := []RoutingPolicySpec{valid[1], valid[0]}
	if err := ValidateRoutingPolicies(notFinal); err == nil {
		t.Fatal("catch-all before a specific policy accepted")
	}

	disabledOnly := []RoutingPolicySpec{{
		ID: "fallback", Priority: 10, Enabled: false, Route: routeToGroup("fallback"),
	}}
	if err := ValidateRoutingPolicies(disabledOnly); err == nil {
		t.Fatal("disabled-only ordered policy set accepted")
	}
}

func TestValidateRoutingPoliciesRejectsInvalidMatchConditions(t *testing.T) {
	valid := []RoutingPolicySpec{
		{
			ID: "specific", Priority: 10, Enabled: true,
			Match: RoutingPolicyMatch{
				Consumers: []string{"internal"}, Models: []string{"smart"},
				Protocols:            []Protocol{ProtocolOpenAIChat},
				RequiredCapabilities: []string{"tools"}, Weekdays: []string{"mon"},
				Timezone: "Asia/Shanghai", TimeWindow: &TimeWindow{Start: "12:00", End: "18:00"},
			},
			Route: routeToGroup("primary"),
		},
		{ID: "fallback", Priority: 20, Enabled: true, Route: routeToGroup("fallback")},
	}
	if err := ValidateRoutingPolicies(valid); err != nil {
		t.Fatalf("valid match rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*RoutingPolicyMatch)
		want string
	}{
		{"empty consumer", func(m *RoutingPolicyMatch) { m.Consumers = []string{""} }, "consumer"},
		{"duplicate model", func(m *RoutingPolicyMatch) { m.Models = []string{"smart", "smart"} }, "model"},
		{"unsupported protocol", func(m *RoutingPolicyMatch) { m.Protocols = []Protocol{"unknown"} }, "protocol"},
		{"duplicate protocol", func(m *RoutingPolicyMatch) { m.Protocols = []Protocol{ProtocolOpenAIChat, ProtocolOpenAIChat} }, "protocols"},
		{"unknown capability", func(m *RoutingPolicyMatch) { m.RequiredCapabilities = []string{"audio"} }, "capability"},
		{"noncanonical capability", func(m *RoutingPolicyMatch) { m.RequiredCapabilities = []string{"Tools"} }, "canonical"},
		{"invalid weekday", func(m *RoutingPolicyMatch) { m.Weekdays = []string{"monday"} }, "weekday"},
		{"duplicate weekday", func(m *RoutingPolicyMatch) { m.Weekdays = []string{"mon", "mon"} }, "weekday"},
		{"invalid timezone", func(m *RoutingPolicyMatch) { m.Timezone = "Mars/Olympus" }, "timezone"},
		{"invalid time", func(m *RoutingPolicyMatch) { m.TimeWindow = &TimeWindow{Start: "24:00", End: "18:00"} }, "time window"},
		{"unsupported expression", func(m *RoutingPolicyMatch) { m.Expression = "context.foo == 'bar'" }, "expression"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policies := append([]RoutingPolicySpec(nil), valid...)
			policies[0].Match = valid[0].Match
			tc.edit(&policies[0].Match)
			err := ValidateRoutingPolicies(policies)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}

	allDay := append([]RoutingPolicySpec(nil), valid...)
	allDay[0].Match = valid[0].Match
	allDay[0].Match.TimeWindow = &TimeWindow{Start: "12:00", End: "12:00"}
	if err := ValidateRoutingPolicies(allDay); err != nil {
		t.Fatalf("equal ordered-policy window should retain all-day semantics: %v", err)
	}
}

func TestValidateRoutingPoliciesRejectsDuplicateRecoveryFailure(t *testing.T) {
	policies := []RoutingPolicySpec{{
		ID: "fallback", Priority: 10, Enabled: true,
		Route: RoutingPolicyRoute{Steps: []RoutingPolicyStep{{
			ChannelGroup: "primary", RecoverOn: []FailureType{FailConnect, FailConnect},
		}}},
	}}
	err := ValidateRoutingPolicies(policies)
	if err == nil || !strings.Contains(err.Error(), "recover_on") {
		t.Fatalf("error = %v, want duplicate recover_on rejection", err)
	}
}

func routeToGroup(group string) RoutingPolicyRoute {
	return RoutingPolicyRoute{Steps: []RoutingPolicyStep{{ChannelGroup: group}}}
}
