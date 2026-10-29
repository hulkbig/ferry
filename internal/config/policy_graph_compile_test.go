package config

import (
	"reflect"
	"strings"
	"testing"

	"ferry/internal/domain"
	"gopkg.in/yaml.v3"
)

func minimalPolicyGraph() PolicyGraphSpec {
	return PolicyGraphSpec{
		Version: 1,
		Nodes: []PolicyGraphNode{
			{ID: "entry", Type: "input"},
			{ID: "primary", Type: "channel", Data: map[string]any{
				"tierName": "Graph primary", "internalPool": "free-pool",
			}},
			{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
		},
		Edges: []PolicyGraphEdge{
			{ID: "entry-primary", Source: "entry", SourcePort: "next", Target: "primary", TargetPort: "in"},
			{ID: "primary-done", Source: "primary", SourcePort: "success", Target: "done", TargetPort: "in"},
		},
	}
}

func TestValidateCanonicalizesRuntimePolicyFromGraph(t *testing.T) {
	f := mustLoad(t, baseYAML)
	f.PolicyGraphs = map[string]PolicyGraphSpec{"basic": minimalPolicyGraph()}
	// Deliberately keep a valid but contradictory runtime policy. The graph is
	// the source of truth, so validation must not publish this stale policy.
	f.Policies["basic"] = PolicyYAML{
		Timezone: "UTC",
		Tiers:    []domain.TierDef{{Name: "stale", Pool: "free-pool"}},
		Recovery: map[string]domain.RecoveryAction{
			string(domain.FailRateLimit): {NextTier: true},
		},
	}

	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := f.Policies["basic"]
	if got.Timezone != "Asia/Shanghai" {
		t.Fatalf("canonical timezone = %q, want Asia/Shanghai", got.Timezone)
	}
	wantTiers := []domain.TierDef{{Name: "Graph primary", Pool: "free-pool"}}
	if !reflect.DeepEqual(got.Tiers, wantTiers) {
		t.Fatalf("canonical tiers = %#v, want %#v", got.Tiers, wantTiers)
	}
	if action := got.Recovery[string(domain.FailRateLimit)]; !action.Abort || action.NextTier || action.NextTarget != "" {
		t.Fatalf("unconnected rate-limit edge must canonicalize to Abort, got %#v", action)
	}
	if len(got.Recovery) != len(knownPolicyFailures) {
		t.Fatalf("canonical recovery has %d failures, want %d", len(got.Recovery), len(knownPolicyFailures))
	}
}

func TestValidateCompilesNestedDecisionGraphDeterministically(t *testing.T) {
	f := mustLoad(t, baseYAML)
	target := domain.ResourceTargetID("glm-free-1")
	f.Pools = append(f.Pools,
		domain.Pool{ID: "subscription-pool", Targets: []domain.ResourceTargetID{target}},
		domain.Pool{ID: "premium-pool", Targets: []domain.ResourceTargetID{target}},
	)
	f.PolicyGraphs = map[string]PolicyGraphSpec{"basic": {
		Version: 1,
		// Node and edge storage order is intentionally unrelated to execution
		// order. Ports and topology alone determine the canonical policy.
		Nodes: []PolicyGraphNode{
			{ID: "premium", Type: "channel", Data: map[string]any{"tierName": "Premium", "internalPool": "premium-pool"}},
			{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
			{ID: "inner", Type: "condition", Data: map[string]any{"label": "Personal routing", "expression": `consumer.name == "personal"`}},
			{ID: "entry", Type: "input"},
			{ID: "free", Type: "channel", Data: map[string]any{"tierName": "Free", "internalPool": "free-pool"}},
			{ID: "outer", Type: "schedule", Data: map[string]any{"label": "Afternoon routing", "timezone": "Asia/Shanghai", "start": "12:00", "end": "18:00"}},
			{ID: "subscription", Type: "channel", Data: map[string]any{"tierName": "Subscription", "internalPool": "subscription-pool"}},
		},
		Edges: []PolicyGraphEdge{
			{ID: "subscription-done", Source: "subscription", SourcePort: "success", Target: "done", TargetPort: "in"},
			{ID: "inner-true", Source: "inner", SourcePort: "true", Target: "subscription", TargetPort: "in"},
			{ID: "outer-match", Source: "outer", SourcePort: "match", Target: "premium", TargetPort: "in"},
			{ID: "entry-outer", Source: "entry", SourcePort: "next", Target: "outer", TargetPort: "in"},
			{ID: "free-done", Source: "free", SourcePort: "success", Target: "done", TargetPort: "in"},
			{ID: "outer-otherwise", Source: "outer", SourcePort: "otherwise", Target: "inner", TargetPort: "in"},
			{ID: "premium-done", Source: "premium", SourcePort: "success", Target: "done", TargetPort: "in"},
			{ID: "inner-false", Source: "inner", SourcePort: "false", Target: "free", TargetPort: "in"},
		},
	}}

	if err := f.Validate(); err != nil {
		t.Fatalf("Validate nested graph: %v", err)
	}
	got := f.Policies["basic"]
	wantTiers := []domain.TierDef{
		{Name: "Free", Pool: "free-pool"},
		{Name: "Subscription", Pool: "subscription-pool"},
		{Name: "Premium", Pool: "premium-pool"},
	}
	if !reflect.DeepEqual(got.Tiers, wantTiers) {
		t.Fatalf("tiers = %#v, want topology order %#v", got.Tiers, wantTiers)
	}
	if len(got.Rules) != 3 {
		t.Fatalf("rules = %#v, want two outer exclusions plus one nested exclusion", got.Rules)
	}
	if got.Rules[0].When.Pool != "free-pool" || got.Rules[0].When.TimeWindow == nil ||
		got.Rules[1].When.Pool != "subscription-pool" || got.Rules[1].When.TimeWindow == nil {
		t.Fatalf("outer decision rules = %#v, want false-branch-exclusive pool exclusions", got.Rules[:2])
	}
	if got.Rules[2].When.Pool != "free-pool" || got.Rules[2].When.ConsumerKey != "personal" {
		t.Fatalf("nested decision rule = %#v, want personal exclusion for free branch", got.Rules[2])
	}
}

func TestCompilePolicyGraphUsesUniqueExplicitTimezone(t *testing.T) {
	graph := PolicyGraphSpec{
		Version: 1,
		Nodes: []PolicyGraphNode{
			{ID: "entry", Type: "input"},
			{ID: "outer", Type: "schedule", Data: map[string]any{"start": "08:00", "end": "10:00"}},
			{ID: "inner", Type: "schedule", Data: map[string]any{"timezone": "UTC", "start": "12:00", "end": "18:00"}},
			{ID: "free", Type: "channel", Data: map[string]any{"internalPool": "free-pool"}},
			{ID: "subscription", Type: "channel", Data: map[string]any{"internalPool": "subscription-pool"}},
			{ID: "premium", Type: "channel", Data: map[string]any{"internalPool": "premium-pool"}},
			{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
		},
		Edges: []PolicyGraphEdge{
			{ID: "entry-outer", Source: "entry", SourcePort: "next", Target: "outer"},
			{ID: "outer-otherwise", Source: "outer", SourcePort: "otherwise", Target: "inner"},
			{ID: "outer-match", Source: "outer", SourcePort: "match", Target: "premium"},
			{ID: "inner-otherwise", Source: "inner", SourcePort: "otherwise", Target: "free"},
			{ID: "inner-match", Source: "inner", SourcePort: "match", Target: "subscription"},
			{ID: "free-done", Source: "free", SourcePort: "success", Target: "done"},
			{ID: "subscription-done", Source: "subscription", SourcePort: "success", Target: "done"},
			{ID: "premium-done", Source: "premium", SourcePort: "success", Target: "done"},
		},
	}
	compiled, err := compilePolicyGraph("main", graph, "Asia/Shanghai")
	if err != nil {
		t.Fatalf("compilePolicyGraph: %v", err)
	}
	if compiled.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want unique explicit UTC", compiled.Timezone)
	}
}

func recoveryPolicyGraph() PolicyGraphSpec {
	return PolicyGraphSpec{
		Version: 1,
		Nodes: []PolicyGraphNode{
			{ID: "entry", Type: "input"},
			{ID: "primary", Type: "channel", Data: map[string]any{"internalPool": "primary-pool"}},
			{ID: "backup", Type: "channel", Data: map[string]any{"internalPool": "backup-pool"}},
			{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
			{ID: "failed", Type: "end", Data: map[string]any{"outcome": "failure"}},
		},
		Edges: []PolicyGraphEdge{
			{ID: "entry-primary", Source: "entry", SourcePort: "next", Target: "primary", TargetPort: "in"},
			{ID: "primary-done", Source: "primary", SourcePort: "success", Target: "done", TargetPort: "in"},
			{ID: "backup-done", Source: "backup", SourcePort: "success", Target: "done", TargetPort: "in"},
			{ID: "rate-limit-next-tier", Source: "primary", SourcePort: "recoverable_failure", Target: "backup", TargetPort: "in", RecoveryAction: "next_tier", FailureTypes: []string{"rate_limit"}},
			{ID: "primary-connect-same-tier", Source: "primary", SourcePort: "recoverable_failure", Target: "primary", TargetPort: "in", RecoveryAction: "same_tier", FailureTypes: []string{"connect_failure"}},
			{ID: "backup-connect-same-tier", Source: "backup", SourcePort: "recoverable_failure", Target: "backup", TargetPort: "in", RecoveryAction: "same_tier", FailureTypes: []string{"connect_failure"}},
			{ID: "primary-auth-abort", Source: "primary", SourcePort: "terminal_failure", Target: "failed", TargetPort: "in", FailureTypes: []string{"authentication_failure"}},
			{ID: "backup-auth-abort", Source: "backup", SourcePort: "terminal_failure", Target: "failed", TargetPort: "in", FailureTypes: []string{"authentication_failure"}},
		},
	}
}

func TestCompilePolicyGraphPreservesExplicitRecoveryActions(t *testing.T) {
	compiled, err := compilePolicyGraph("main", recoveryPolicyGraph(), "Asia/Shanghai")
	if err != nil {
		t.Fatalf("compilePolicyGraph: %v", err)
	}
	if got := compiled.Recovery[string(domain.FailConnect)]; got.NextTarget != "same_tier" || got.NextTier || got.Abort {
		t.Fatalf("connect recovery = %#v, want next_target:same_tier", got)
	}
	if got := compiled.Recovery[string(domain.FailRateLimit)]; !got.NextTier || got.NextTarget != "" || got.Abort {
		t.Fatalf("rate-limit recovery = %#v, want next_tier", got)
	}
	if got := compiled.Recovery[string(domain.FailAuth)]; !got.Abort || got.NextTier || got.NextTarget != "" {
		t.Fatalf("auth recovery = %#v, want abort", got)
	}
	if got := compiled.Recovery[string(domain.FailProvider5xx)]; !got.Abort || got.NextTier || got.NextTarget != "" {
		t.Fatalf("unconnected provider failure = %#v, want fail-closed abort", got)
	}
}

func TestPolicyGraphRecoveryActionRoundTripsThroughYAML(t *testing.T) {
	want := recoveryPolicyGraph()
	raw, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "recovery_action: same_tier") {
		t.Fatalf("YAML omits explicit recovery action:\n%s", raw)
	}
	var got PolicyGraphSpec
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy graph changed across YAML round-trip:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestPolicyGraphRejectsRecoverySemanticsRuntimeCannotRepresent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PolicyGraphSpec)
		want   string
	}{
		{
			name: "recoverable action is explicit",
			mutate: func(g *PolicyGraphSpec) {
				g.Edges[3].RecoveryAction = ""
			},
			want: "RECOVERY_ACTION_REQUIRED",
		},
		{
			name: "same tier is a channel self-loop",
			mutate: func(g *PolicyGraphSpec) {
				g.Edges[4].Target = "backup"
			},
			want: "SAME_TIER_TARGET_INVALID",
		},
		{
			name: "same tier covers every channel",
			mutate: func(g *PolicyGraphSpec) {
				g.Edges = append(g.Edges[:5], g.Edges[6:]...)
			},
			want: "RECOVERY_COVERAGE_INCOMPLETE",
		},
		{
			name: "same and next tier cannot mix",
			mutate: func(g *PolicyGraphSpec) {
				g.Edges[5].FailureTypes = []string{"connect_failure", "rate_limit"}
			},
			want: "AMBIGUOUS_RECOVERY",
		},
		{
			name: "terminal abort covers every channel",
			mutate: func(g *PolicyGraphSpec) {
				g.Edges = g.Edges[:7]
			},
			want: "RECOVERY_COVERAGE_INCOMPLETE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graph := recoveryPolicyGraph()
			tc.mutate(&graph)
			err := validatePolicyGraph("main", graph)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestPolicyGraphRejectsTopologyThatRuntimeCannotRepresent(t *testing.T) {
	tests := []struct {
		name  string
		graph PolicyGraphSpec
		want  string
	}{
		{
			name: "entry cannot terminate",
			graph: PolicyGraphSpec{Version: 1,
				Nodes: []PolicyGraphNode{
					{ID: "entry", Type: "input"}, {ID: "primary", Type: "channel"},
					{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
				},
				Edges: []PolicyGraphEdge{
					{ID: "entry-done", Source: "entry", SourcePort: "next", Target: "done"},
					{ID: "primary-done", Source: "primary", SourcePort: "success", Target: "done"},
				}},
			want: "ENTRY_TARGET_INVALID",
		},
		{
			name: "decision branch cannot terminate before a channel",
			graph: PolicyGraphSpec{Version: 1,
				Nodes: []PolicyGraphNode{
					{ID: "entry", Type: "input"},
					{ID: "window", Type: "schedule", Data: map[string]any{"start": "12:00", "end": "18:00"}},
					{ID: "primary", Type: "channel"},
					{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
				},
				Edges: []PolicyGraphEdge{
					{ID: "entry-window", Source: "entry", SourcePort: "next", Target: "window"},
					{ID: "window-match", Source: "window", SourcePort: "match", Target: "done"},
					{ID: "window-otherwise", Source: "window", SourcePort: "otherwise", Target: "primary"},
					{ID: "primary-done", Source: "primary", SourcePort: "success", Target: "done"},
				}},
			want: "DECISION_TARGET_INVALID",
		},
		{
			name: "unreachable channel is not silently appended",
			graph: func() PolicyGraphSpec {
				g := minimalPolicyGraph()
				g.Nodes = append(g.Nodes, PolicyGraphNode{ID: "backup", Type: "channel"})
				g.Edges = append(g.Edges, PolicyGraphEdge{ID: "backup-done", Source: "backup", SourcePort: "success", Target: "done"})
				return g
			}(),
			want: "UNREACHABLE_NODE",
		},
		{
			name: "unreachable decision is not silently discarded",
			graph: func() PolicyGraphSpec {
				g := minimalPolicyGraph()
				g.Nodes = append(g.Nodes, PolicyGraphNode{ID: "orphan", Type: "condition", Data: map[string]any{"expression": `consumer.name == "personal"`}})
				g.Edges = append(g.Edges,
					PolicyGraphEdge{ID: "orphan-true", Source: "orphan", SourcePort: "true", Target: "primary"},
					PolicyGraphEdge{ID: "orphan-false", Source: "orphan", SourcePort: "false", Target: "primary"},
				)
				return g
			}(),
			want: "UNREACHABLE_NODE",
		},
		{
			name: "merged decision cannot be flattened",
			graph: PolicyGraphSpec{Version: 1,
				Nodes: []PolicyGraphNode{
					{ID: "entry", Type: "input"},
					{ID: "outer", Type: "schedule", Data: map[string]any{"start": "12:00", "end": "18:00"}},
					{ID: "inner", Type: "condition", Data: map[string]any{"expression": `consumer.name == "personal"`}},
					{ID: "free", Type: "channel"}, {ID: "premium", Type: "channel"},
					{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
				},
				Edges: []PolicyGraphEdge{
					{ID: "entry-outer", Source: "entry", SourcePort: "next", Target: "outer"},
					{ID: "outer-match", Source: "outer", SourcePort: "match", Target: "inner"},
					{ID: "outer-otherwise", Source: "outer", SourcePort: "otherwise", Target: "inner"},
					{ID: "inner-true", Source: "inner", SourcePort: "true", Target: "premium"},
					{ID: "inner-false", Source: "inner", SourcePort: "false", Target: "free"},
					{ID: "free-done", Source: "free", SourcePort: "success", Target: "done"},
					{ID: "premium-done", Source: "premium", SourcePort: "success", Target: "done"},
				}},
			want: "DECISION_MERGE_UNSUPPORTED",
		},
		{
			name: "decision needs a false-exclusive channel",
			graph: PolicyGraphSpec{Version: 1,
				Nodes: []PolicyGraphNode{
					{ID: "entry", Type: "input"},
					{ID: "condition", Type: "condition", Data: map[string]any{"expression": `consumer.name == "personal"`}},
					{ID: "primary", Type: "channel"},
					{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
				},
				Edges: []PolicyGraphEdge{
					{ID: "entry-condition", Source: "entry", SourcePort: "next", Target: "condition"},
					{ID: "condition-true", Source: "condition", SourcePort: "true", Target: "primary"},
					{ID: "condition-false", Source: "condition", SourcePort: "false", Target: "primary"},
					{ID: "primary-done", Source: "primary", SourcePort: "success", Target: "done"},
				}},
			want: "DECISION_BRANCH_NOT_DISTINCT",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicyGraph("main", tc.graph)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}
