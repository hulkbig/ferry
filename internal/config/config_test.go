package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ferry/internal/domain"
	"gopkg.in/yaml.v3"
)

// baseYAML is a minimal valid config every negative test can mutate.
const baseYAML = `
server:
  data_dir: ./data-test
endpoints:
  - id: zhipu
    base_url: https://open.bigmodel.cn/api/paas/v4
    protocol: openai-chat
plans:
  - id: free
    kind: free
quota_buckets:
  - id: glm-free
    unit: prompts
    limit_per_cycle: 100
    refresh: {period: daily, at: "04:00", timezone: Asia/Shanghai}
resource_targets:
  - id: glm-free-1
    endpoint_id: zhipu
    model: glm-4-flash
    plan_id: free
    credential: {secret_ref: env:ZHIPU_API_KEY}
    capabilities: {text: true, stream: true, context_window: 131072}
pools:
  - id: free-pool
    targets: [glm-free-1]
policies:
  basic:
    tiers:
      - {name: main, pool: free-pool}
default_policy: basic
`

func mustLoad(t *testing.T, yaml string) *File {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return f
}

func TestLoadDefaults(t *testing.T) {
	f := mustLoad(t, baseYAML)
	if f.Server.DataDir != "./data-test" {
		t.Errorf("DataDir=%q", f.Server.DataDir)
	}
	if f.Server.ManagementAddr != "127.0.0.1:7900" {
		t.Errorf("ManagementAddr default: %q", f.Server.ManagementAddr)
	}
	if f.Server.ProxyAddr != "127.0.0.1:7890" {
		t.Errorf("ProxyAddr default: %q", f.Server.ProxyAddr)
	}
	if f.Server.Timezone != "Asia/Shanghai" {
		t.Errorf("Timezone default: %q", f.Server.Timezone)
	}
	if f.Server.LogRetentionDays != 30 {
		t.Errorf("LogRetentionDays default: %d", f.Server.LogRetentionDays)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := strings.Replace(baseYAML, "  data_dir: ./data-test", "  data_dir: ./data-test\n  log_retention_dayz: 30", 1)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field log_retention_dayz not found") {
		t.Fatalf("Load error = %v, want strict unknown-field rejection", err)
	}
}

func TestApplyDefaultsEnablesConsumerKeys(t *testing.T) {
	f := &File{ConsumerKeys: []ConsumerKey{{Name: "team-a", Key: "sk-a"}}}
	f.applyDefaults()
	if f.ConsumerKeys[0].Enabled == nil || !*f.ConsumerKeys[0].Enabled {
		t.Error("Enabled nil must default to true")
	}
	disabled := false
	f2 := &File{ConsumerKeys: []ConsumerKey{{Name: "team-b", Key: "sk-b", Enabled: &disabled}}}
	f2.applyDefaults()
	if *f2.ConsumerKeys[0].Enabled {
		t.Error("explicit false must be preserved")
	}
}

// The bucket timezone backfill: a bucket without its own timezone inherits
// the referencing plan's timezone.
func TestValidateBackfillsBucketTimezone(t *testing.T) {
	f := mustLoad(t, baseYAML)
	if got := f.QuotaBuckets[0].Refresh.Timezone; got != "Asia/Shanghai" {
		t.Errorf("bucket tz backfilled from plan: %q", got)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring of the expected error
	}{
		{
			name: "duplicate endpoint id",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
  - {id: ep, base_url: https://b.example, protocol: openai-chat}
`,
			want: `duplicate endpoint id "ep"`,
		},
		{
			name: "bad base_url",
			yaml: `
endpoints:
  - {id: ep, base_url: ftp://x, protocol: openai-chat}
`,
			want: "endpoint ep: base_url must be http(s)",
		},
		{
			name: "unsupported protocol",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: gemini}
`,
			want: `endpoint ep: unsupported protocol "gemini"`,
		},
		{
			name: "invalid quota unit",
			yaml: `
quota_buckets:
  - {id: q, unit: requests, limit_per_cycle: 5, refresh: {period: daily}}
`,
			want: `quota_bucket q: invalid unit "requests"`,
		},
		{
			name: "non-positive quota limit",
			yaml: `
quota_buckets:
  - {id: q, unit: prompts, limit_per_cycle: 0, refresh: {period: daily}}
`,
			want: "quota_bucket q: limit_per_cycle must be > 0",
		},
		{
			name: "invalid refresh period",
			yaml: `
quota_buckets:
  - {id: q, unit: prompts, limit_per_cycle: 5, refresh: {period: fortnightly}}
`,
			want: `quota_bucket q: refresh.period "fortnightly" invalid`,
		},
		{
			name: "invalid plan kind",
			yaml: `
plans:
  - {id: p, kind: enterprise}
`,
			want: `plan p: invalid kind "enterprise"`,
		},
		{
			name: "non-positive multiplier window",
			yaml: `
plans:
  - id: p
    kind: free
    windows: [{start: "00:00", end: "08:00", multiplier: 0}]
`,
			want: "plan p: window multiplier must be > 0",
		},
		{
			name: "unknown quota bucket on plan",
			yaml: `
plans:
  - {id: p, kind: free, quota: {bucket_id: nope}}
`,
			want: `plan p: unknown quota bucket "nope"`,
		},
		{
			name: "unknown endpoint on target",
			yaml: `
resource_targets:
  - {id: tgt, endpoint_id: ghost, model: m, plan_id: p2, credential: {secret_ref: env:K}}
plans:
  - {id: p2, kind: free}
`,
			want: `target tgt: unknown endpoint "ghost"`,
		},
		{
			name: "unknown plan on target",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
resource_targets:
  - {id: tgt, endpoint_id: ep, model: m, plan_id: ghost, credential: {secret_ref: env:K}}
`,
			want: `target tgt: unknown plan "ghost"`,
		},
		{
			name: "missing model",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: tgt, endpoint_id: ep, plan_id: p, credential: {secret_ref: env:K}}
`,
			want: "target tgt: model required",
		},
		{
			name: "missing secret ref",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: tgt, endpoint_id: ep, model: m, plan_id: p}
`,
			want: "target tgt: credential.secret_ref required",
		},
		{
			name: "malformed secret ref",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: tgt, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: K}}
`,
			want: `target tgt: secret_ref must start with env:, file:, or plain: (got "K")`,
		},
		{
			name: "empty pool",
			yaml: `
pools:
  - {id: empty-pool, targets: []}
`,
			want: "pool empty-pool: no targets",
		},
		{
			name: "pool references unknown target",
			yaml: `
pools:
  - {id: pool, targets: [ghost]}
`,
			want: `pool pool: unknown target "ghost"`,
		},
		{
			name: "unknown capability in alias",
			yaml: `
aliases:
  chat:
    required_capabilities: {telepathy: true}
`,
			want: `alias chat: unknown capability "telepathy"`,
		},
		{
			name: "alias unknown default objective",
			yaml: `
objectives:
  cheap: {rank: [priority]}
aliases:
  chat:
    default_objective: expensive
`,
			want: `alias chat: unknown default_objective "expensive"`,
		},
		{
			name: "unknown rank key",
			yaml: `
objectives:
  weird: {rank: [vibes]}
`,
			want: `objective weird: unknown rank key "vibes"`,
		},
		{
			name: "policy without tiers",
			yaml: `
policies:
  empty: {}
`,
			want: "policy empty: at least one tier required",
		},
		{
			name: "duplicate tier",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}}
pools:
  - {id: pool, targets: [t1]}
policies:
  pol:
    tiers:
      - {name: main, pool: pool}
      - {name: main, pool: pool}
`,
			want: `policy pol: duplicate tier "main"`,
		},
		{
			name: "tier unknown pool",
			yaml: `
policies:
  pol:
    tiers:
      - {name: main, pool: ghost-pool}
`,
			want: `policy pol: tier "main" references unknown pool "ghost-pool"`,
		},
		{
			name: "rule without action",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}}
pools:
  - {id: pool, targets: [t1]}
policies:
  pol:
    rules:
      - name: noop
        when: {health: unhealthy}
    tiers:
      - {name: main, pool: pool}
`,
			want: `policy pol: rule "noop" has no action`,
		},
		{
			name: "default policy undefined",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}}
pools:
  - {id: pool, targets: [t1]}
policies:
  pol:
    tiers:
      - {name: main, pool: pool}
default_policy: other
`,
			want: `default_policy "other" not defined`,
		},
		{
			name: "default policy missing with policies present",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: t1, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}}
pools:
  - {id: pool, targets: [t1]}
policies:
  pol:
    tiers:
      - {name: main, pool: pool}
`,
			want: "default_policy required when policies are defined",
		},
		{
			name: "duplicate consumer key value",
			yaml: `
consumer_keys:
  - {name: a, key: sk-same}
  - {name: b, key: sk-same}
`,
			want: "consumer key b: duplicate key value",
		},
		{
			name: "consumer key unknown alias",
			yaml: `
consumer_keys:
  - {name: a, key: k, default_alias: ghost}
`,
			want: `consumer key a: unknown default_alias "ghost"`,
		},
		{
			name: "target not in any pool",
			yaml: `
endpoints:
  - {id: ep, base_url: https://a.example, protocol: openai-chat}
plans:
  - {id: p, kind: free}
resource_targets:
  - {id: lonely, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}}
  - {id: member, endpoint_id: ep, model: m, plan_id: p, credential: {secret_ref: env:K}}
pools:
  - {id: other-pool, targets: [member]}
`,
			want: "target lonely: not added to any pool",
		},
		// ---- channels ----
		{
			name: "channel unsupported type",
			yaml: `
channels:
  - id: oc
    type: gemini-native
    base_url: https://a.example
    credential: {secret_ref: env:K}
    models:
      - {model: m, protocols: [openai-chat]}
`,
			want: `channel oc: unsupported type "gemini-native"`,
		},
		{
			name: "channel custom missing base_url",
			yaml: `
channels:
  - id: oc
    credential: {secret_ref: env:K}
    models:
      - {model: m, protocols: [openai-chat]}
`,
			want: "channel oc: base_url must be http(s)",
		},
		{
			name: "channel custom missing credential",
			yaml: `
channels:
  - id: oc
    base_url: https://a.example
    models:
      - {model: m, protocols: [openai-chat]}
`,
			want: "channel oc: credential.secret_ref required",
		},
		{
			name: "channel no models",
			yaml: `
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
`,
			want: "channel oc: at least one model required",
		},
		{
			name: "channel model without native protocol",
			yaml: `
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
    models:
      - {model: m}
`,
			want: "channel oc model m: at least one native protocol required",
		},
		{
			name: "channel model unknown protocol",
			yaml: `
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
    models:
      - {model: m, protocols: [gemini]}
`,
			want: `channel oc model m: unsupported protocol "gemini"`,
		},
		// NOTE: no tests for "expanded target id collides/duplicates" — those
		// Validate branches are defense-in-depth only. nameRe forbids dots in
		// channel ids while expanded ids ("<channel>.<model>") always contain
		// one, and duplicate tids require duplicate channel ids, which checkID
		// rejects first; neither is reachable through valid YAML.
		{
			name: "channel model without plan",
			yaml: `
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
    models:
      - {model: m, protocols: [openai-chat], pools: [free-pool]}
`,
			want: "channel oc model m: plan_id required (on the model or the channel)",
		},
		{
			name: "channel model unknown pool",
			yaml: `
plans:
  - {id: free, kind: free}
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
    plan_id: free
    models:
      - {model: m, protocols: [openai-chat], pools: [ghost-pool]}
`,
			want: `channel oc model m: unknown pool "ghost-pool"`,
		},
		{
			name: "channel-level unknown pool",
			yaml: `
plans:
  - {id: free, kind: free}
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
    plan_id: free
    pools: [ghost-pool]
    models:
      - {model: m, protocols: [openai-chat]}
`,
			want: `unknown pool "ghost-pool"`,
		},
		{
			name: "channel model unreachable",
			yaml: `
plans:
  - {id: free, kind: free}
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
    plan_id: free
    models:
      - {model: m, protocols: [openai-chat]}
`,
			want: "not reachable — set pools on the model or channel",
		},
		{
			name: "pool referencing unknown channel target",
			yaml: `
plans:
  - {id: free, kind: free}
channels:
  - id: oc
    base_url: https://a.example
    credential: {secret_ref: env:K}
    plan_id: free
    models:
      - {model: m, protocols: [openai-chat]}
pools:
  - {id: main, targets: [oc.m]}
  - {id: extra, targets: [oc.wrong]}
`,
			want: `pool extra: unknown target "oc.wrong"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f File
			if err := yaml.Unmarshal([]byte(c.yaml), &f); err != nil {
				t.Fatalf("yaml parse: %v", err)
			}
			err := f.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestValidateHappyPath(t *testing.T) {
	mustLoad(t, baseYAML) // panics via Fatalf on any validation error
}

func TestCodexChannelNormalizesNativeProtocolByModelCapability(t *testing.T) {
	f := mustLoad(t, baseYAML)
	f.Channels = []domain.Channel{{
		ID: "codex", Type: domain.ChannelCliproxyCodex, PlanID: "free",
		Pools: []string{"free-pool"}, Models: []domain.ChannelModel{
			{Model: "gpt-5.5", Protocols: []domain.Protocol{domain.ProtocolAnthropic}},
			{Model: "gpt-image-2", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}},
		},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("Codex channel rejected: %v", err)
	}
	if got := f.Channels[0].Models[0].Protocols; len(got) != 1 || got[0] != domain.ProtocolOpenAIResponses {
		t.Fatalf("text model protocols=%v", got)
	}
	if got := f.Channels[0].Models[1].Protocols; len(got) != 1 || got[0] != domain.ProtocolOpenAIImages {
		t.Fatalf("image model protocols=%v", got)
	}
}

func TestPolicyGraphValidationRejectsBrokenTopologyAndUnsafeExpression(t *testing.T) {
	validGraph := `
policy_graphs:
  basic:
    version: 1
    nodes:
      - {id: entry, type: input}
      - {id: window, type: schedule, data: {timezone: Asia/Shanghai, start: "12:00", end: "18:00"}}
      - {id: glm, type: channel, data: {tier_name: GLM, pool: free-pool}}
      - {id: alternate, type: channel, data: {tier_name: Alternate, pool: free-pool}}
      - {id: done, type: end, data: {outcome: success}}
    edges:
      - {id: e1, source: entry, source_port: next, target: window}
      - {id: e2, source: window, source_port: match, target: alternate}
      - {id: e3, source: window, source_port: otherwise, target: glm}
      - {id: e4, source: glm, source_port: success, target: done}
      - {id: e5, source: alternate, source_port: success, target: done}
`
	cases := []struct {
		name  string
		graph string
		want  string
	}{
		{name: "valid", graph: validGraph},
		{name: "required branch", graph: strings.Replace(validGraph, "      - {id: e3, source: window, source_port: otherwise, target: glm}\n", "", 1), want: "REQUIRED_PORT_UNCONNECTED"},
		{name: "cycle", graph: strings.Replace(validGraph, "      - {id: e2, source: window, source_port: match, target: alternate}", "      - {id: e2, source: window, source_port: match, target: window}", 1), want: "GRAPH_CYCLE"},
		{name: "unsafe expression", graph: strings.Replace(validGraph, "      - {id: window, type: schedule, data: {timezone: Asia/Shanghai, start: \"12:00\", end: \"18:00\"}}", "      - {id: window, type: condition, data: {mode: expression, expression: 'fetch(\"https://example.com\")'}}", 1), want: "EXPRESSION_FORBIDDEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f File
			if err := yaml.Unmarshal([]byte(baseYAML+tc.graph), &f); err != nil {
				t.Fatal(err)
			}
			err := f.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid graph rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestPolicyGraphValidationEnforcesTypedEdgeContract(t *testing.T) {
	base := func() PolicyGraphSpec {
		return PolicyGraphSpec{
			Version: 1,
			Nodes: []PolicyGraphNode{
				{ID: "entry", Type: "input"},
				{ID: "primary", Type: "channel"},
				{ID: "backup", Type: "channel"},
				{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
				{ID: "failed", Type: "end", Data: map[string]any{"outcome": "failure"}},
			},
			Edges: []PolicyGraphEdge{
				{ID: "entry-primary", Source: "entry", SourcePort: "next", Target: "primary", TargetPort: "in"},
				{ID: "primary-done", Source: "primary", SourcePort: "success", Target: "done", TargetPort: "in"},
				{ID: "primary-backup", Source: "primary", SourcePort: "recoverable_failure", Target: "backup", TargetPort: "in", RecoveryAction: "next_tier", FailureTypes: []string{"rate_limit"}},
				{ID: "backup-done", Source: "backup", SourcePort: "success", Target: "done", TargetPort: "in"},
				{ID: "primary-failed", Source: "primary", SourcePort: "terminal_failure", Target: "failed", TargetPort: "in", FailureTypes: []string{"authentication_failure"}},
				{ID: "backup-failed", Source: "backup", SourcePort: "terminal_failure", Target: "failed", TargetPort: "in", FailureTypes: []string{"authentication_failure"}},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*PolicyGraphSpec)
		want   string
	}{
		{name: "valid typed fallback"},
		{name: "channel success is required", mutate: func(g *PolicyGraphSpec) { g.Edges = g.Edges[:3] }, want: "REQUIRED_PORT_UNCONNECTED"},
		{name: "ordinary port cannot fan out", mutate: func(g *PolicyGraphSpec) {
			g.Edges = append(g.Edges, PolicyGraphEdge{ID: "entry-backup", Source: "entry", SourcePort: "next", Target: "backup", TargetPort: "in"})
		}, want: "OUTPUT_PORT_FANOUT"},
		{name: "fallback requires failures", mutate: func(g *PolicyGraphSpec) { g.Edges[2].FailureTypes = nil }, want: "FAILURE_TYPE_REQUIRED"},
		{name: "fallback rejects unknown failure", mutate: func(g *PolicyGraphSpec) { g.Edges[2].FailureTypes = []string{"mystery"} }, want: "UNKNOWN_FAILURE_TYPE"},
		{name: "ordinary edge rejects failures", mutate: func(g *PolicyGraphSpec) { g.Edges[1].FailureTypes = []string{"rate_limit"} }, want: "FAILURE_TYPES_FORBIDDEN"},
		{name: "recoverable edge targets a channel", mutate: func(g *PolicyGraphSpec) { g.Edges[2].Target = "failed" }, want: "RECOVERABLE_TARGET_INVALID"},
		{name: "success edge targets success end", mutate: func(g *PolicyGraphSpec) { g.Edges[1].Target = "failed" }, want: "SUCCESS_TARGET_INVALID"},
		{name: "terminal edge targets failure end", mutate: func(g *PolicyGraphSpec) {
			g.Edges = append(g.Edges, PolicyGraphEdge{ID: "bad-terminal", Source: "primary", SourcePort: "terminal_failure", Target: "done", TargetPort: "in", FailureTypes: []string{"authentication_failure"}})
		}, want: "TERMINAL_TARGET_INVALID"},
		{name: "source-specific recovery cannot be flattened", mutate: func(g *PolicyGraphSpec) {
			g.Edges = append(g.Edges, PolicyGraphEdge{ID: "backup-primary", Source: "backup", SourcePort: "recoverable_failure", Target: "primary", TargetPort: "in", RecoveryAction: "next_tier", FailureTypes: []string{"rate_limit"}})
		}, want: "AMBIGUOUS_RECOVERY"},
		{name: "three tier recovery chain must be complete", mutate: func(g *PolicyGraphSpec) {
			g.Nodes = append(g.Nodes, PolicyGraphNode{ID: "payg", Type: "channel"})
			g.Edges = append(g.Edges,
				PolicyGraphEdge{ID: "payg-done", Source: "payg", SourcePort: "success", Target: "done", TargetPort: "in"},
				PolicyGraphEdge{ID: "payg-failed", Source: "payg", SourcePort: "terminal_failure", Target: "failed", TargetPort: "in", FailureTypes: []string{"authentication_failure"}},
			)
		}, want: "RECOVERY_CHAIN_INCOMPLETE"},
		{name: "three tier recovery chain is representable", mutate: func(g *PolicyGraphSpec) {
			g.Nodes = append(g.Nodes, PolicyGraphNode{ID: "payg", Type: "channel"})
			g.Edges = append(g.Edges,
				PolicyGraphEdge{ID: "backup-payg", Source: "backup", SourcePort: "recoverable_failure", Target: "payg", TargetPort: "in", RecoveryAction: "next_tier", FailureTypes: []string{"rate_limit"}},
				PolicyGraphEdge{ID: "payg-done", Source: "payg", SourcePort: "success", Target: "done", TargetPort: "in"},
				PolicyGraphEdge{ID: "payg-failed", Source: "payg", SourcePort: "terminal_failure", Target: "failed", TargetPort: "in", FailureTypes: []string{"authentication_failure"}},
			)
		}},
		{name: "input cannot be a target", mutate: func(g *PolicyGraphSpec) { g.Edges[1].Target = "entry" }, want: "TARGET_PORT_INVALID"},
		{name: "target port must be input", mutate: func(g *PolicyGraphSpec) { g.Edges[1].TargetPort = "wrong" }, want: "TARGET_PORT_INVALID"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graph := base()
			if tc.mutate != nil {
				tc.mutate(&graph)
			}
			err := validatePolicyGraph("main", graph)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid graph rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestValidatePolicyExpressionOnlyAcceptsCompilableSubset(t *testing.T) {
	accepted := []string{
		`context.local_hour >= 12 && context.local_hour < 18`,
		`consumer.name == "personal"`,
		`channel.health == "unhealthy"`,
		`channel.quota_remaining >= 0.25`,
		`channel.cooldown == true`,
		`context.weekday in ["mon", "tue"]`,
	}
	for _, expression := range accepted {
		if err := ValidatePolicyExpression(expression); err != nil {
			t.Errorf("expression %q rejected: %v", expression, err)
		}
	}
	rejected := []string{
		`request.model == "glm-4"`,
		`channel.id == "glm"`,
		`consumer.name == "personal" || channel.health == "degraded"`,
	}
	for _, expression := range rejected {
		err := ValidatePolicyExpression(expression)
		if err == nil || !strings.Contains(err.Error(), "EXPRESSION_UNSUPPORTED") {
			t.Errorf("expression %q error = %v, want EXPRESSION_UNSUPPORTED", expression, err)
		}
	}
}

func TestLoadReadError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !strings.HasPrefix(err.Error(), "read config:") {
		t.Errorf("want 'read config:' prefix, got %v", err)
	}
}

func TestLoadParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.HasPrefix(err.Error(), "parse config "+path+":") {
		t.Errorf("want 'parse config' prefix, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Secret refs (F-P0-25): configs hold references only; values resolve at
// publish time and never round-trip through the config file.

func TestResolveSecretEnv(t *testing.T) {
	t.Setenv("CPA_TEST_SECRET", "s3cr3t")
	got, err := ResolveSecret("env:CPA_TEST_SECRET")
	if err != nil || got != "s3cr3t" {
		t.Fatalf("got %q, %v", got, err)
	}
	_, err = ResolveSecret("env:CPA_TEST_UNSET_VAR")
	if err == nil || !strings.Contains(err.Error(), "secret env var CPA_TEST_UNSET_VAR is empty or unset") {
		t.Errorf("unset env: got %v", err)
	}
}

func TestResolveSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSecret("file:" + path)
	if err != nil || got != "file-secret" {
		t.Fatalf("got %q, %v (trailing newline must be trimmed)", got, err)
	}
	_, err = ResolveSecret("file:/nonexistent/ferry/secret")
	if err == nil || !strings.Contains(err.Error(), "read secret file:") {
		t.Errorf("missing file: got %v", err)
	}
}

func TestResolveSecretPlainAndInvalid(t *testing.T) {
	got, err := ResolveSecret("plain:dev-only")
	if err != nil || got != "dev-only" {
		t.Fatalf("plain: got %q, %v", got, err)
	}
	_, err = ResolveSecret("kms:arn:whatever")
	if err == nil || !strings.Contains(err.Error(), `unsupported secret ref "kms:arn:whatever"`) {
		t.Errorf("unsupported scheme: got %v", err)
	}
}

func TestValidateSecretRefMessages(t *testing.T) {
	if err := validateSecretRef("env:"); err == nil || err.Error() != "empty env secret ref" {
		t.Errorf("env: got %v", err)
	}
	if err := validateSecretRef("file:"); err == nil || err.Error() != "empty file secret ref" {
		t.Errorf("file: got %v", err)
	}
	for _, ok := range []string{"env:A", "file:/tmp/k", "plain:x"} {
		if err := validateSecretRef(ok); err != nil {
			t.Errorf("%q should be accepted, got %v", ok, err)
		}
	}
}

func TestValidateV2DomainOAuthAccountSlotsAreGloballyExclusive(t *testing.T) {
	base := func() *File {
		f := nativeV2DomainFixtureForOAuth()
		return f
	}
	f := base()
	if err := ValidateV2Domain(f); err != nil {
		t.Fatalf("distinct oauth accounts must validate: %v", err)
	}
	// Two connections on the same oauth slot: the exact incident that
	// double-burned one subscription's quota across providers.
	f = base()
	f.Connections = append(f.Connections, domain.Connection{
		ID: "connection-b", ProviderID: f.Providers[0].ID,
		Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("personal")},
		Models: []domain.ConnectionModel{{UpstreamModel: "gpt-5.6-sol",
			Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}}},
	})
	f.Providers = append(f.Providers, domain.Provider{
		ID: "provider-b", Type: domain.ProviderCliproxyCodex,
		BaseURL: "https://chatgpt.com/backend-api/codex", Enabled: true,
		Connections: []domain.ConnectionRef{{ID: "connection-b"}},
	})
	err := ValidateV2Domain(f)
	if err == nil || !strings.Contains(err.Error(), `oauth account "personal" is already referenced by connection`) {
		t.Fatalf("duplicate oauth binding must fail with the owning connection: %v", err)
	}
	// Ordinary API-key secrets stay shareable across connections.
	f = base()
	f.Connections = append(f.Connections, domain.Connection{
		ID: "connection-c", ProviderID: "provider-c",
		Credential: domain.CredentialRef{SecretRef: "store:shared-key"},
		Models: []domain.ConnectionModel{{UpstreamModel: "gpt-5.6-sol",
			Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}}},
	})
	f.Providers = append(f.Providers, domain.Provider{
		ID: "provider-c", Type: domain.ProviderCustom,
		BaseURL: "https://api.example.test/v1", Enabled: true,
		Connections: []domain.ConnectionRef{{ID: "connection-c"}},
	})
	if err := ValidateV2Domain(f); err != nil {
		t.Fatalf("shared store: secret across connections must stay valid: %v", err)
	}
}

func nativeV2DomainFixtureForOAuth() *File {
	return &File{
		V2DomainSource: V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "provider-a", Type: domain.ProviderCliproxyCodex,
			BaseURL: "https://chatgpt.com/backend-api/codex", Enabled: true,
			Connections: []domain.ConnectionRef{{ID: "connection-a"}},
		}},
		Connections: []domain.Connection{{
			ID: "connection-a", ProviderID: "provider-a",
			Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("personal")},
			Models: []domain.ConnectionModel{{UpstreamModel: "gpt-5.6-sol",
				Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}}},
		}},
	}
}

// A stored config that predates oauth-slot exclusivity must still boot: the
// runtime tolerates it with a logged warning while every write path keeps
// rejecting the duplicate binding.
func TestStartupValidationToleratesLegacyOAuthSharing(t *testing.T) {
	f := nativeV2DomainFixtureForOAuth()
	f.Connections = append(f.Connections, domain.Connection{
		ID: "connection-b", ProviderID: "provider-b",
		Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("personal")},
		Models: []domain.ConnectionModel{{UpstreamModel: "gpt-5.6-sol",
			Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}}},
	})
	f.Providers = append(f.Providers, domain.Provider{
		ID: "provider-b", Type: domain.ProviderCliproxyCodex,
		BaseURL: "https://chatgpt.com/backend-api/codex", Enabled: true,
		Connections: []domain.ConnectionRef{{ID: "connection-b"}},
	})
	if err := ValidateV2Domain(f); err == nil ||
		!strings.Contains(err.Error(), `oauth account "personal" is already referenced`) {
		t.Fatalf("strict validation must reject duplicate oauth binding: %v", err)
	}
	if err := f.ValidateStartup(); err != nil {
		t.Fatalf("startup validation must tolerate legacy oauth sharing: %v", err)
	}
	if violations := OAuthBindingViolations(f); len(violations) != 1 ||
		!strings.Contains(violations[0], "connection-b") {
		t.Fatalf("violations=%v", violations)
	}
	// The tolerated flag must not leak into later strict validations of the
	// same file.
	if err := f.Validate(); err == nil {
		t.Fatal("strict Validate must reject again after ValidateStartup reset the flag")
	}
}
