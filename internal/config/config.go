// Package config defines and validates Ferry's structured control-plane
// document. SQLite is the runtime source of truth; Load exists only for the
// explicit one-time legacy YAML import. Credentials hold references, not
// secret values.
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"ferry/internal/domain"
	"ferry/internal/nativeauth"
)

// File is the top-level structured configuration document persisted as JSON
// in SQLite control-plane versions.
type File struct {
	Concurrency         *domain.ConcurrencyPolicy `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	CodexClientVersion  string                    `json:"codex_client_version,omitempty" yaml:"codex_client_version,omitempty"`
	CodexAutoUpdate     bool                      `json:"codex_auto_update,omitempty" yaml:"codex_auto_update,omitempty"`
	CodexReleaseChannel string                    `json:"codex_release_channel,omitempty" yaml:"codex_release_channel,omitempty"`
	CodexAutoVersion    string                    `json:"codex_auto_version,omitempty" yaml:"codex_auto_version,omitempty"`
	// Applies only to aggregators in logical_model_policy.json. Nil defaults to 10;
	// zero disables automatic creation for those aggregators, not direct providers.
	AutoLogicalModelLimit *int `json:"auto_logical_model_limit,omitempty" yaml:"auto_logical_model_limit,omitempty"`
	// startupValidation relaxes legacy-tolerated rules (an oauth account slot
	// referenced by multiple connections predating its exclusivity rule) so a
	// stored config can still boot; every write path keeps strict validation.
	startupValidation bool
	Server            ServerConfig                    `yaml:"server"`
	Secrets           map[string]string               `yaml:"secrets"` // name -> env:VAR | file:/path
	ConsumerKeys      []ConsumerKey                   `yaml:"consumer_keys"`
	Endpoints         []domain.Endpoint               `yaml:"endpoints"`
	Credentials       map[string]domain.CredentialRef `yaml:"credentials"` // name -> ref template
	Plans             []domain.Plan                   `yaml:"plans"`
	QuotaBuckets      []domain.QuotaBucket            `yaml:"quota_buckets"`
	RateCards         []RateCardEntry                 `yaml:"rate_cards"`
	Targets           []domain.ResourceTarget         `yaml:"resource_targets"`
	Channels          []domain.Channel                `yaml:"channels"`
	Pools             []domain.Pool                   `yaml:"pools"`
	Aliases           map[string]AliasSpec            `yaml:"aliases"`
	Objectives        map[string]ObjectiveSpec        `yaml:"objectives"`
	Policies          map[string]PolicyYAML           `yaml:"policies"`
	PolicyGraphs      map[string]PolicyGraphSpec      `yaml:"policy_graphs,omitempty"`
	DefaultPolicy     string                          `yaml:"default_policy"`
	RoutingPolicies   []domain.RoutingPolicySpec      `json:"routing_policies,omitempty" yaml:"-"`
	LogicalModels     []domain.LogicalModel           `json:"logical_models,omitempty" yaml:"-"`
	Providers         []domain.Provider               `json:"providers,omitempty" yaml:"-"`
	Connections       []domain.Connection             `json:"connections,omitempty" yaml:"-"`
	V2DomainSource    string                          `json:"v2_domain_source,omitempty" yaml:"-"`
	V2MigrationIssues []domain.LegacyMappingIssue     `json:"v2_migration_issues,omitempty" yaml:"-"`
}

type ServerConfig struct {
	DataDir          string `yaml:"data_dir"`        // SQLite + state; default ./data
	ManagementAddr   string `yaml:"management_addr"` // default 127.0.0.1:7900
	ProxyAddr        string `yaml:"proxy_addr"`      // data plane; default 127.0.0.1:7890
	Timezone         string `yaml:"timezone"`        // default Asia/Shanghai
	LogRetentionDays int    `yaml:"log_retention_days"`
}

// ConsumerKey identifies a caller (PRD §6.3).
type ConsumerKey struct {
	Name             string  `yaml:"name"`
	Key              string  `yaml:"key"` // the actual bearer token clients present
	Enabled          *bool   `yaml:"enabled"`
	DefaultAlias     string  `yaml:"default_alias"`
	DefaultObjective string  `yaml:"default_objective"`
	MonthlyBudgetUSD float64 `yaml:"monthly_budget_usd"` // soft budget, alert only
}

// AliasSpec is the structured shape of an alias contract (PRD §13.1).
type AliasSpec struct {
	RequiredCapabilities map[string]bool `yaml:"required_capabilities"`
	MinimumContext       int64           `yaml:"minimum_context"`
	AllowDegradation     *bool           `yaml:"allow_degradation"`
	DefaultObjective     string          `yaml:"default_objective"`
}

// ObjectiveSpec is the structured shape of an objective profile (PRD §13.2).
type ObjectiveSpec struct {
	Rank        []string `yaml:"rank"`
	MaxCashCost *float64 `yaml:"max_cash_cost"`
}

// RateCardEntry is one versioned price row.
type RateCardEntry struct {
	Version        string          `yaml:"version"`
	Provider       string          `yaml:"provider"`
	Model          string          `yaml:"model"`
	Plan           domain.PlanKind `yaml:"plan"`
	InputPerM      float64         `yaml:"input_per_m"`
	OutputPerM     float64         `yaml:"output_per_m"`
	CacheReadPerM  float64         `yaml:"cache_read_per_m"`
	CacheWritePerM float64         `yaml:"cache_write_per_m"`
}

// PolicyYAML is the legacy field name for the runtime policy document. The
// value is persisted as structured JSON in SQLite.
type PolicyYAML struct {
	Timezone string                           `yaml:"timezone"`
	Rules    []domain.PolicyRule              `yaml:"rules"`
	Tiers    []domain.TierDef                 `yaml:"tiers"`
	Recovery map[string]domain.RecoveryAction `yaml:"recovery"`
}

// Load reads and parses the config file.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator-supplied local config file argument, never request input.
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var f File
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	f.applyDefaults()
	if err := f.expandPlanTemplates(); err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// expandPlanTemplates materializes built-in platform presets referenced by
// plans via `template:` into ordinary plan fields and quota buckets, before
// validation. Explicit fields on the plan win over template defaults.
func (f *File) expandPlanTemplates() error {
	known := map[string]bool{}
	for i := range f.QuotaBuckets {
		known[f.QuotaBuckets[i].ID] = true
	}
	for i := range f.Plans {
		p := &f.Plans[i]
		if p.Template == "" {
			continue
		}
		template, ok := domain.LookupPlanTemplate(p.Template)
		if !ok {
			return fmt.Errorf("plan %s: unknown plan template %q (available: %v)",
				p.ID, p.Template, domain.PlanTemplateIDs())
		}
		buckets, err := template.Apply(p)
		if err != nil {
			return fmt.Errorf("plan %s: %w", p.ID, err)
		}
		for _, b := range buckets {
			if !known[b.ID] {
				f.QuotaBuckets = append(f.QuotaBuckets, b)
				known[b.ID] = true
			}
		}
	}
	return nil
}

func (f *File) applyDefaults() {
	if f.Server.DataDir == "" {
		f.Server.DataDir = "./data"
	}
	if f.Server.ManagementAddr == "" {
		f.Server.ManagementAddr = "127.0.0.1:7900"
	}
	if f.Server.ProxyAddr == "" {
		f.Server.ProxyAddr = "127.0.0.1:7890"
	}
	if f.Server.Timezone == "" {
		f.Server.Timezone = "Asia/Shanghai"
	}
	if f.Server.LogRetentionDays == 0 {
		f.Server.LogRetentionDays = 30
	}
	for i := range f.ConsumerKeys {
		if f.ConsumerKeys[i].Enabled == nil {
			t := true
			f.ConsumerKeys[i].Enabled = &t
		}
	}
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// templateBucketIDRe matches materialized plan-template bucket ids
// ("claude-pro:5h-prompts").
var templateBucketIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*:[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Validate enforces structural integrity. Configuration errors must block
// startup / publish (PRD §17.2 管理配置错误阻止上线).
// ValidateStartup validates like Validate but tolerates legacy violations
// that only newer write paths reject, so an existing deployment can boot and
// surface the violation instead of crashing.
func (f *File) ValidateStartup() error {
	f.startupValidation = true
	defer func() { f.startupValidation = false }()
	return f.Validate()
}

func (f *File) Validate() error {
	if err := f.Concurrency.Validate(); err != nil {
		return err
	}
	if err := f.validateCodexVersions(); err != nil {
		return err
	}
	ids := map[string]bool{}
	dup := func(kind, id string) error {
		return fmt.Errorf("duplicate %s id %q", kind, id)
	}
	checkID := func(kind, id string) error {
		if id == "" || !nameRe.MatchString(id) {
			return fmt.Errorf("invalid %s id %q", kind, id)
		}
		if ids[kind+"/"+id] {
			return dup(kind, id)
		}
		ids[kind+"/"+id] = true
		return nil
	}

	endpointIDs := map[string]*domain.Endpoint{}
	for i := range f.Endpoints {
		e := &f.Endpoints[i]
		if err := checkID("endpoint", e.ID); err != nil {
			return err
		}
		if !strings.HasPrefix(e.BaseURL, "http://") && !strings.HasPrefix(e.BaseURL, "https://") {
			return fmt.Errorf("endpoint %s: base_url must be http(s)", e.ID)
		}
		switch e.Protocol {
		case domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolOpenAIImages, domain.ProtocolAnthropic:
		default:
			return fmt.Errorf("endpoint %s: unsupported protocol %q", e.ID, e.Protocol)
		}
		endpointIDs[e.ID] = e
	}

	bucketIDs := map[string]*domain.QuotaBucket{}
	for i := range f.QuotaBuckets {
		q := &f.QuotaBuckets[i]
		// Template-materialized buckets carry a "<template>:<bucket>" id; the
		// colon namespace is reserved for them and rejected elsewhere.
		if templateBucketIDRe.MatchString(q.ID) {
			// colon-namespace ids bypass checkID; register for dedupe here
			if ids["quota_bucket/"+q.ID] {
				return fmt.Errorf("duplicate quota_bucket id %q", q.ID)
			}
			ids["quota_bucket/"+q.ID] = true
		} else if err := checkID("quota_bucket", q.ID); err != nil {
			return err
		}
		switch q.Unit {
		case domain.QuotaUnitPrompts, domain.QuotaUnitTokens, domain.QuotaUnitUSD, domain.QuotaUnitCredits:
		default:
			return fmt.Errorf("quota_bucket %s: invalid unit %q", q.ID, q.Unit)
		}
		if q.LimitPerCycle <= 0 {
			return fmt.Errorf("quota_bucket %s: limit_per_cycle must be > 0", q.ID)
		}
		switch q.Refresh.Period {
		case "daily", "weekly", "monthly":
			if q.Refresh.Rolling {
				return fmt.Errorf("quota_bucket %s: rolling requires a duration period", q.ID)
			}
		default:
			if _, err := time.ParseDuration(q.Refresh.Period); err != nil {
				return fmt.Errorf("quota_bucket %s: refresh.period %q invalid", q.ID, q.Refresh.Period)
			}
		}
		bucketIDs[q.ID] = q
	}

	planIDs := map[string]*domain.Plan{}
	for i := range f.Plans {
		p := &f.Plans[i]
		if err := checkID("plan", p.ID); err != nil {
			return err
		}
		switch p.Kind {
		case domain.PlanFree, domain.PlanSubscription, domain.PlanCredits, domain.PlanPAYG:
		default:
			return fmt.Errorf("plan %s: invalid kind %q", p.ID, p.Kind)
		}
		for _, w := range p.Windows {
			if w.Multiplier <= 0 {
				return fmt.Errorf("plan %s: window multiplier must be > 0", p.ID)
			}
		}
		if p.Quota != nil {
			b, ok := bucketIDs[p.Quota.BucketID]
			if !ok {
				return fmt.Errorf("plan %s: unknown quota bucket %q", p.ID, p.Quota.BucketID)
			}
			if b.Refresh.Timezone == "" && p.Timezone != "" {
				b.Refresh.Timezone = p.Timezone
			}
		}
		for i := range p.Quotas {
			b, ok := bucketIDs[p.Quotas[i].BucketID]
			if !ok {
				return fmt.Errorf("plan %s: unknown quota bucket %q", p.ID, p.Quotas[i].BucketID)
			}
			if b.Refresh.Timezone == "" && p.Timezone != "" {
				b.Refresh.Timezone = p.Timezone
			}
		}
		planIDs[p.ID] = p
	}

	targetIDs := map[string]*domain.ResourceTarget{}
	for i := range f.Targets {
		t := &f.Targets[i]
		if err := checkID("target", string(t.ID)); err != nil {
			return err
		}
		ep, ok := endpointIDs[t.EndpointID]
		if !ok {
			return fmt.Errorf("target %s: unknown endpoint %q", t.ID, t.EndpointID)
		}
		pl, ok := planIDs[t.PlanID]
		if !ok {
			return fmt.Errorf("target %s: unknown plan %q", t.ID, t.PlanID)
		}
		if t.Model == "" {
			return fmt.Errorf("target %s: model required", t.ID)
		}
		if t.Credential.SecretRef == "" {
			return fmt.Errorf("target %s: credential.secret_ref required", t.ID)
		}
		if err := validateSecretRef(t.Credential.SecretRef); err != nil {
			return fmt.Errorf("target %s: %w", t.ID, err)
		}
		t.Endpoint = ep
		t.Plan = pl
		targetIDs[string(t.ID)] = t
	}

	// Channels: one base URL + credential + model list, expanded by the
	// compiler into ordinary resource targets ("<channel>.<model>"). The name
	// charset of hand-written ids excludes dots so the two namespaces are
	// collision-free; still verify explicitly (F-P0-23 posture).
	poolIDs := map[string]bool{}
	for _, p := range f.Pools {
		poolIDs[p.ID] = true
	}
	explicitlyPooled := map[string]bool{}
	for _, p := range f.Pools {
		for _, tid := range p.Targets {
			explicitlyPooled[string(tid)] = true
		}
	}
	channelTargetIDs := map[string]bool{}
	for i := range f.Channels {
		ch := &f.Channels[i]
		if err := checkID("channel", ch.ID); err != nil {
			return err
		}
		switch ch.Type {
		case "", domain.ChannelCustom:
			ch.Type = domain.ChannelCustom
			if !strings.HasPrefix(ch.BaseURL, "http://") && !strings.HasPrefix(ch.BaseURL, "https://") {
				return fmt.Errorf("channel %s: base_url must be http(s)", ch.ID)
			}
			if ch.Credential.SecretRef == "" {
				return fmt.Errorf("channel %s: credential.secret_ref required", ch.ID)
			}
			if err := validateSecretRef(ch.Credential.SecretRef); err != nil {
				return fmt.Errorf("channel %s: %w", ch.ID, err)
			}
		case domain.ChannelCliproxyCodex:
			if ch.BaseURL == "" {
				ch.BaseURL = domain.CodexBaseURL // built-in default
			} else if !strings.HasPrefix(ch.BaseURL, "https://") {
				return fmt.Errorf("channel %s: cliproxy-codex base_url must be https", ch.ID)
			}
			if len(ch.Models) == 0 {
				// no explicit models: the account's plan tier decides the list,
				// synthesized from the built-in catalog at compile time. The
				// channel-level plan_id names the tier (free/team/plus/pro).
				ch.Models = domain.CodexModelsForPlan(ch.PlanID)
			}
			// credential comes from the built-in OAuth flow, not config
		default:
			return fmt.Errorf("channel %s: unsupported type %q", ch.ID, ch.Type)
		}
		if len(ch.Models) == 0 {
			return fmt.Errorf("channel %s: at least one model required", ch.ID)
		}
		modelNames := map[string]bool{}
		for j := range ch.Models {
			m := &ch.Models[j]
			if m.Model == "" || !modelRe.MatchString(m.Model) {
				return fmt.Errorf("channel %s: invalid model name %q", ch.ID, m.Model)
			}
			if modelNames[m.Model] {
				return fmt.Errorf("channel %s: duplicate model %q", ch.ID, m.Model)
			}
			modelNames[m.Model] = true
			if ch.Type == domain.ChannelCliproxyCodex {
				m.Protocols = []domain.Protocol{domain.CodexModelNativeProtocol(m.Model)}
			}
			if len(m.Protocols) == 0 {
				return fmt.Errorf("channel %s model %s: at least one native protocol required",
					ch.ID, m.Model)
			}
			for _, p := range m.Protocols {
				if !domain.ValidProtocol(p) {
					return fmt.Errorf("channel %s model %s: unsupported protocol %q",
						ch.ID, m.Model, p)
				}
			}
			tid := domain.ChannelTargetID(ch.ID, m.Model)
			if _, clash := targetIDs[tid]; clash {
				return fmt.Errorf("channel %s: expanded target id %q collides with a resource_target", ch.ID, tid)
			}
			if channelTargetIDs[tid] {
				return fmt.Errorf("duplicate channel target id %q", tid)
			}
			channelTargetIDs[tid] = true
			pid := m.PlanRef(ch)
			if pid == "" {
				return fmt.Errorf("channel %s model %s: plan_id required (on the model or the channel)",
					ch.ID, m.Model)
			}
			if _, ok := planIDs[pid]; !ok {
				return fmt.Errorf("channel %s model %s: unknown plan %q", ch.ID, m.Model, pid)
			}
			pools := m.Pools
			if len(pools) == 0 {
				pools = ch.Pools
			}
			for _, pn := range pools {
				if !poolIDs[pn] {
					return fmt.Errorf("channel %s model %s: unknown pool %q", ch.ID, m.Model, pn)
				}
			}
			if len(pools) == 0 && !explicitlyPooled[tid] {
				return fmt.Errorf("channel %s model %s: not reachable — set pools on the model or channel, or list %q in a pool",
					ch.ID, m.Model, tid)
			}
		}
	}

	poolTargets := map[string]bool{}
	// Channel auto-join: a pool that only receives members through channel
	// `pools:` has an empty explicit target list — legal, since expansion
	// populates it at compile time.
	channelPoolMembers := map[string]bool{}
	for i := range f.Channels {
		for _, pn := range f.Channels[i].Pools {
			channelPoolMembers[pn] = true
		}
		for j := range f.Channels[i].Models {
			m := &f.Channels[i].Models[j]
			pools := m.Pools
			if len(pools) == 0 {
				pools = f.Channels[i].Pools
			}
			for _, pn := range pools {
				channelPoolMembers[pn] = true
			}
		}
	}
	for _, p := range f.Pools {
		if err := checkID("pool", p.ID); err != nil {
			return err
		}
		if len(p.Targets) == 0 && !channelPoolMembers[p.ID] {
			return fmt.Errorf("pool %s: no targets", p.ID)
		}
		for _, tid := range p.Targets {
			// Pools may mix classic resource_targets with channel-expanded
			// targets ("<channel>.<model>"); both namespaces were built above.
			if _, ok := targetIDs[string(tid)]; !ok {
				if _, ok := channelTargetIDs[string(tid)]; !ok {
					return fmt.Errorf("pool %s: unknown target %q", p.ID, tid)
				}
			}
			poolTargets[string(tid)] = true
		}
	}

	// Channel-level pool memberships must name real pools; per-model pools are
	// verified in the channel loop above.
	for i := range f.Channels {
		ch := &f.Channels[i]
		if !ch.EnabledOrDefault() {
			continue
		}
		for _, pn := range ch.Pools {
			if !poolIDs[pn] {
				return fmt.Errorf("channel %s: unknown pool %q", ch.ID, pn)
			}
		}
	}

	for name, a := range f.Aliases {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("invalid alias name %q", name)
		}
		for capName := range a.RequiredCapabilities {
			if _, known := domain.NormalizeBooleanCapabilityName(capName); !known {
				return fmt.Errorf("alias %s: unknown capability %q", name, capName)
			}
		}
		if a.DefaultObjective != "" {
			if _, ok := f.Objectives[a.DefaultObjective]; !ok {
				return fmt.Errorf("alias %s: unknown default_objective %q", name, a.DefaultObjective)
			}
		}
	}

	for name, o := range f.Objectives {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("invalid objective name %q", name)
		}
		for _, k := range o.Rank {
			if !validRankKey(k) {
				return fmt.Errorf("objective %s: unknown rank key %q", name, k)
			}
		}
	}

	// PolicyGraphs are the editable source document. Canonicalize their
	// matching runtime Policies before validating policy references, so a
	// posted canvas can never execute a contradictory legacy policy payload.
	if err := f.canonicalizePolicyGraphs(); err != nil {
		return err
	}

	poolByID := map[string]*domain.Pool{}
	for i := range f.Pools {
		poolByID[f.Pools[i].ID] = &f.Pools[i]
	}
	for pname, pol := range f.Policies {
		if !nameRe.MatchString(pname) {
			return fmt.Errorf("invalid policy name %q", pname)
		}
		if len(pol.Tiers) == 0 {
			return fmt.Errorf("policy %s: at least one tier required", pname)
		}
		seenTier := map[string]bool{}
		for _, t := range pol.Tiers {
			if seenTier[t.Name] {
				return fmt.Errorf("policy %s: duplicate tier %q", pname, t.Name)
			}
			seenTier[t.Name] = true
			if _, ok := poolByID[t.Pool]; !ok {
				return fmt.Errorf("policy %s: tier %q references unknown pool %q", pname, t.Name, t.Pool)
			}
			if t.Objective != "" {
				if _, ok := f.Objectives[t.Objective]; !ok {
					return fmt.Errorf("policy %s: tier %q unknown objective %q", pname, t.Name, t.Objective)
				}
			}
		}
		for ri, r := range pol.Rules {
			if r.Name == "" {
				return fmt.Errorf("policy %s: rule[%d] missing name", pname, ri)
			}
			if r.When.Target != "" {
				if _, ok := targetIDs[r.When.Target]; !ok {
					return fmt.Errorf("policy %s: rule %q unknown target %q", pname, r.Name, r.When.Target)
				}
			}
			if r.When.Pool != "" {
				if _, ok := poolByID[r.When.Pool]; !ok {
					return fmt.Errorf("policy %s: rule %q unknown pool %q", pname, r.Name, r.When.Pool)
				}
			}
			hasAction := r.Action.Exclude != nil || r.Action.Require != "" ||
				r.Action.Prefer != nil || r.Action.Pin != nil || r.Action.Tier != ""
			if !hasAction {
				return fmt.Errorf("policy %s: rule %q has no action", pname, r.Name)
			}
			if r.Action.Tier != "" && !seenTier[r.Action.Tier] {
				return fmt.Errorf("policy %s: rule %q moves to unknown tier %q", pname, r.Name, r.Action.Tier)
			}
			if r.Action.Require != "" && !validCapabilityName(r.Action.Require) {
				return fmt.Errorf("policy %s: rule %q requires unknown capability %q", pname, r.Name, r.Action.Require)
			}
			for _, weekday := range r.When.Weekdays {
				lower := strings.ToLower(weekday)
				if len(lower) < 3 || !validLegacyWeekday(lower[:3]) {
					return fmt.Errorf("policy %s: rule %q has invalid weekday %q", pname, r.Name, weekday)
				}
			}
			if window := r.When.TimeWindow; window != nil &&
				(!validGraphHHMM(window.Start) || !validGraphHHMM(window.End)) {
				return fmt.Errorf("policy %s: rule %q has invalid time window %q-%q", pname, r.Name, window.Start, window.End)
			}
		}
		for fk := range pol.Recovery {
			if domain.FailureType(fk).RecoveryClass() == "" {
				return fmt.Errorf("policy %s: unknown recovery failure type %q", pname, fk)
			}
		}
	}

	if err := domain.ValidateRoutingPolicies(f.RoutingPolicies); err != nil {
		return err
	}
	channelByID := make(map[string]bool, len(f.Channels))
	for i := range f.Channels {
		channelByID[f.Channels[i].ID] = true
	}
	for i := range f.RoutingPolicies {
		routing := &f.RoutingPolicies[i]
		if !nameRe.MatchString(routing.ID) {
			return fmt.Errorf("invalid routing policy id %q", routing.ID)
		}
		for n, step := range routing.Route.Steps {
			if step.Channel != "" && !channelByID[step.Channel] {
				return fmt.Errorf("routing policy %s step %d: unknown channel %q", routing.ID, n+1, step.Channel)
			}
			if step.ChannelGroup != "" {
				if _, ok := poolByID[step.ChannelGroup]; !ok {
					return fmt.Errorf("routing policy %s step %d: unknown channel group %q", routing.ID, n+1, step.ChannelGroup)
				}
			}
			if step.Objective != "" {
				if _, ok := f.Objectives[step.Objective]; !ok {
					return fmt.Errorf("routing policy %s step %d: unknown objective %q", routing.ID, n+1, step.Objective)
				}
			}
		}
		hash, err := domain.RoutingPolicyContentHash(*routing)
		if err != nil {
			return fmt.Errorf("routing policy %s: %w", routing.ID, err)
		}
		routing.ContentHash = hash
		if routing.Revision == 0 {
			routing.Revision = 1
		}
	}

	ckNames := map[string]bool{}
	keyVals := map[string]bool{}
	for _, ck := range f.ConsumerKeys {
		if ck.Name == "" || !nameRe.MatchString(ck.Name) {
			return fmt.Errorf("invalid consumer key name %q", ck.Name)
		}
		if ckNames[ck.Name] {
			return dup("consumer_key", ck.Name)
		}
		ckNames[ck.Name] = true
		if ck.Key == "__CONSUMER_KEY__" {
			return fmt.Errorf("consumer key %s: bootstrap placeholder must be rendered before import", ck.Name)
		}
		// Key is a one-time legacy import/rotation input. Persisted runtime
		// configurations intentionally leave it blank because verifiers live in
		// api_key_secrets outside revision history.
		if ck.Key != "" && keyVals[ck.Key] {
			return fmt.Errorf("consumer key %s: duplicate key value", ck.Name)
		}
		if ck.Key != "" {
			keyVals[ck.Key] = true
		}
		if ck.DefaultAlias != "" {
			if _, ok := f.Aliases[ck.DefaultAlias]; !ok {
				return fmt.Errorf("consumer key %s: unknown default_alias %q", ck.Name, ck.DefaultAlias)
			}
		}
		if ck.DefaultObjective != "" {
			if _, ok := f.Objectives[ck.DefaultObjective]; !ok {
				return fmt.Errorf("consumer key %s: unknown default_objective %q", ck.Name, ck.DefaultObjective)
			}
		}
	}

	if _, err := MigrateLegacyV2Domain(f); err != nil {
		return err
	}
	if err := ValidateV2Domain(f); err != nil {
		return err
	}

	if f.DefaultPolicy != "" {
		if _, ok := f.Policies[f.DefaultPolicy]; !ok {
			return fmt.Errorf("default_policy %q not defined", f.DefaultPolicy)
		}
	} else if len(f.Policies) > 0 {
		return fmt.Errorf("default_policy required when policies are defined")
	}

	// Every target should belong to some pool referenced by a policy tier,
	// otherwise it can never be scheduled. Warn-level in spirit but keep
	// strictness: unattached targets are a common misconfiguration.
	for _, t := range f.Targets {
		if !poolTargets[string(t.ID)] {
			return fmt.Errorf("target %s: not added to any pool", t.ID)
		}
	}
	// Channel-expanded targets reach pools through the channels section
	// (channel-level or per-model `pools:`) or by explicit pool membership.
	for i := range f.Channels {
		ch := &f.Channels[i]
		if !ch.EnabledOrDefault() {
			continue // disabled channels compile to Disabled targets, no routing
		}
		for j := range ch.Models {
			m := &ch.Models[j]
			tid := domain.ChannelTargetID(ch.ID, m.Model)
			pools := m.Pools
			if len(pools) == 0 {
				pools = ch.Pools
			}
			inPool := len(pools) > 0 || explicitlyPooled[tid] || poolTargets[tid]
			if !inPool {
				return fmt.Errorf("channel %s model %s: not added to any pool", ch.ID, m.Model)
			}
		}
	}

	if err := f.validatePolicyGraphs(); err != nil {
		return err
	}
	return nil
}

// modelRe constrains channel model names to URL/path-safe characters. Upstream
// model ids occasionally contain dots (e.g. "gpt-4o-mini"), so dots are
// allowed in the middle — but never first (would collide with the
// "<channel>." prefix convention).
var modelRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`)

func validRankKey(k string) bool {
	switch k {
	case "min_cash_cost", "min_effective_cost", "max_quota_expiry_pressure",
		"min_latency", "max_reliability", "capability_fit", "priority",
		"least_used", "max_quota_remaining":
		return true
	}
	return false
}

func validCapabilityName(name string) bool {
	_, known := domain.NormalizeBooleanCapabilityName(name)
	return known
}

func validLegacyWeekday(value string) bool {
	switch value {
	case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		return true
	default:
		return false
	}
}

func validateSecretRef(ref string) error {
	if _, _, ok := nativeauth.ParseRef(ref); ok {
		return nil
	}
	switch {
	case strings.HasPrefix(ref, "store:"):
		if strings.Contains(ref[6:], ":") {
			return fmt.Errorf("reserved secret namespace cannot be used as a store reference")
		}
		if len(ref) <= 6 {
			return fmt.Errorf("empty store secret ref")
		}
		return nil
	case strings.HasPrefix(ref, "env:"):
		if len(ref) <= 4 {
			return fmt.Errorf("empty env secret ref")
		}
		return nil
	case strings.HasPrefix(ref, "file:"):
		if len(ref) <= 5 {
			return fmt.Errorf("empty file secret ref")
		}
		return nil
	case strings.HasPrefix(ref, "plain:"):
		return nil // dev convenience; documented as insecure
	default:
		return fmt.Errorf("secret_ref must start with env:, file:, or plain: (got %q)", ref)
	}
}

// Resolver materializes secret references (F-P0-25) with an optional local
// store consulted for `store:` refs (and as fallback for `env:` refs whose
// variable is unset — keys provisioned via the console then win over nothing,
// while a real environment value still takes precedence for env refs). A nil
// resolver behaves exactly like the package-level ResolveSecret.
type Resolver struct {
	NativeOAuth *nativeauth.Manager
	// Store resolves `store:NAME` refs; may be nil (store refs then fail).
	Store func(name string) (string, bool)
}

// Resolve materializes one secret reference.
func (r *Resolver) Resolve(ref string) (string, error) {
	if _, _, ok := nativeauth.ParseRef(ref); ok {
		if r == nil || r.NativeOAuth == nil {
			return "", fmt.Errorf("native authentication unavailable")
		}
		return "native-managed:" + ref, nil // Token materialization occurs only in the bound HTTP transport.
	}
	switch {
	case strings.HasPrefix(ref, "store:"):
		name := ref[6:]
		if strings.Contains(name, ":") {
			return "", fmt.Errorf("reserved secret namespace cannot be resolved as a static credential")
		}
		if r == nil || r.Store == nil {
			return "", fmt.Errorf("no local secret store configured (ref %q)", ref)
		}
		v, ok := r.Store(name)
		if !ok || v == "" {
			return "", fmt.Errorf("secret %q not found in local secret store (add it via the console)", name)
		}
		return v, nil
	case strings.HasPrefix(ref, "env:"):
		name := ref[4:]
		if strings.Contains(name, ":") {
			return "", fmt.Errorf("reserved secret namespace cannot be resolved as an environment credential")
		}
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
		// console-provisioned fallback: env:GLM_API_KEY resolves from the
		// store entry of the same name when the variable is unset.
		if r != nil && r.Store != nil {
			if v, ok := r.Store(name); ok && v != "" {
				return v, nil
			}
		}
		return "", fmt.Errorf("secret env var %s is empty or unset", name)
	default:
		return ResolveSecret(ref)
	}
}

// ResolveSecret materializes a secret reference (F-P0-25).
func ResolveSecret(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "env:"):
		name := ref[4:]
		if strings.Contains(name, ":") {
			return "", fmt.Errorf("reserved secret namespace cannot be resolved as an environment credential")
		}
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("secret env var %s is empty or unset", name)
		}
		return v, nil
	case strings.HasPrefix(ref, "file:"):
		path := ref[5:]
		b, err := os.ReadFile(path) // #nosec G304 -- file: refs are authored by the single administrator in the local config; resolved values go to upstream auth only, never to responses or logs.
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	case strings.HasPrefix(ref, "plain:"):
		return ref[6:], nil
	default:
		return "", fmt.Errorf("unsupported secret ref %q (must start with env:, file:, store:, or plain:)", ref)
	}
}
