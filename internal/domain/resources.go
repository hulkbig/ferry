package domain

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Resource model (PRD §6.7, §7)

type PlanKind string

const (
	PlanFree         PlanKind = "free"
	PlanSubscription PlanKind = "subscription"
	PlanCredits      PlanKind = "credits"
	PlanPAYG         PlanKind = "payg"
)

// QuotaUnit is the consumption unit of a quota bucket.
type QuotaUnit string

const (
	QuotaUnitPrompts QuotaUnit = "prompts" // GLM-style: per-request cost
	QuotaUnitTokens  QuotaUnit = "tokens"
	QuotaUnitUSD     QuotaUnit = "usd"
	// QuotaUnitCredits meters OpenAI Codex-style plan credits
	// (1 credit = USDPerCredit).
	QuotaUnitCredits QuotaUnit = "credits"
)

// MultiplierWindow is one time-of-day quota multiplier window (F-P0-14).
// Times are wall-clock in the plan's timezone; overnight windows wrap.
type MultiplierWindow struct {
	Start      string  `yaml:"start"` // "14:00"
	End        string  `yaml:"end"`   // "18:00"
	Multiplier float64 `yaml:"multiplier"`
}

// QuotaRef links a plan to its quota bucket.
type QuotaRef struct {
	BucketID string `yaml:"bucket_id"`
}

// Plan is the economic identity of a resource (PRD §7).
type Plan struct {
	ID       string    `yaml:"id"`
	Kind     PlanKind  `yaml:"kind"`
	Currency string    `yaml:"currency"`
	Fee      float64   `yaml:"fee"`   // fixed subscription fee per period
	Quota    *QuotaRef `yaml:"quota"` // optional linked bucket (primary)
	// Quotas links additional stacked buckets (e.g. 5h rolling + weekly for
	// Claude/Codex-style plans). Quota stays the primary/compat entry.
	Quotas   []QuotaRef         `yaml:"quotas"`
	Timezone string             `yaml:"timezone"` // for multiplier windows
	Windows  []MultiplierWindow `yaml:"windows"`  // quota multiplier windows
	// Template references a built-in platform-aligned preset
	// ("claude-pro", "codex-plus", "glm-lite", ...); unset fields of this plan
	// are filled from it at config load, and its quota buckets are
	// materialized with "<template>:<bucket>" ids. Explicit fields win.
	Template string `yaml:"template,omitempty"`
}

// QuotaRefs returns every bucket reference of the plan: the legacy single
// Quota first, then any stacked Quotas entries, duplicates removed.
func (p *Plan) QuotaRefs() []QuotaRef {
	if p == nil {
		return nil
	}
	var out []QuotaRef
	seen := map[string]bool{}
	add := func(r *QuotaRef) {
		if r == nil || r.BucketID == "" || seen[r.BucketID] {
			return
		}
		seen[r.BucketID] = true
		out = append(out, *r)
	}
	add(p.Quota)
	for i := range p.Quotas {
		add(&p.Quotas[i])
	}
	return out
}

// QuotaBucket is a refreshable allowance (PRD §2.2).
type QuotaBucket struct {
	ID            string      `yaml:"id"`
	Unit          QuotaUnit   `yaml:"unit"`
	LimitPerCycle float64     `yaml:"limit_per_cycle"`
	Refresh       RefreshSpec `yaml:"refresh"`
}

// RefreshSpec describes the refresh cadence.
type RefreshSpec struct {
	Period   string `yaml:"period"`   // "daily", "weekly", "monthly", or duration like "24h"
	At       string `yaml:"at"`       // reset wall-clock "04:00" in bucket tz
	Timezone string `yaml:"timezone"` // default UTC
	// Rolling marks a duration-period bucket whose window starts at first use
	// (Claude/Codex-style "5-hour rolling window"), not at a fixed wall-clock
	// time. Period must parse as a duration when Rolling is set.
	Rolling bool `yaml:"rolling"`
}

// IsRolling reports whether the bucket refreshes on a use-anchored rolling
// window with the given duration.
func (r *RefreshSpec) IsRolling() (time.Duration, bool) {
	if r == nil || !r.Rolling {
		return 0, false
	}
	d, err := time.ParseDuration(r.Period)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// Endpoint is an upstream API address + protocol.
type Endpoint struct {
	ID       string   `yaml:"id"`
	BaseURL  string   `yaml:"base_url"`
	FullURL  string   `yaml:"full_url,omitempty"`
	Protocol Protocol `yaml:"protocol"`
}

// RequestURL resolves the exact upstream request address. FullURL is used as
// supplied (apart from removing a client-side fragment); otherwise suffix is
// appended to BaseURL's path. Management probes, Connection Test, and routed
// traffic all use this resolver so they cannot silently target different URLs.
func (e *Endpoint) RequestURL(suffix string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("upstream endpoint is required")
	}
	if strings.TrimSpace(e.FullURL) != "" {
		parsed, err := url.Parse(strings.TrimSpace(e.FullURL))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", fmt.Errorf("an absolute HTTP(S) Full URL is required")
		}
		parsed.Fragment = ""
		return parsed.String(), nil
	}
	parsed, err := url.Parse(strings.TrimSpace(e.BaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("an absolute HTTP(S) Base URL is required")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + suffix
	parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
	return parsed.String(), nil
}

// CredentialRef points at a secret without embedding it (F-P0-25).
type CredentialRef struct {
	SecretRef  string `yaml:"secret_ref"`  // env:NAME | file:/path
	Header     string `yaml:"header"`      // default protocol-appropriate
	QueryParam string `yaml:"query_param"` // alternative: ?key=...
	Prefix     string `yaml:"prefix"`      // e.g. "Bearer "
}

// ResourceTargetID identifies a target uniquely.
type ResourceTargetID string

// ResourceTarget = Endpoint + Credential + Model + Plan (PRD §6.7).
type ResourceTarget struct {
	ID           ResourceTargetID `yaml:"id"`
	EndpointID   string           `yaml:"endpoint_id"`
	Credential   CredentialRef    `yaml:"credential"`
	Model        string           `yaml:"model"`       // upstream model name
	ModelAlias   string           `yaml:"model_alias"` // optional client-facing name override
	PlanID       string           `yaml:"plan_id"`
	Capabilities Capabilities     `yaml:"capabilities"`
	Weight       int              `yaml:"weight"`   // for weighted rank
	Priority     int              `yaml:"priority"` // lower = better, for priority rank

	// Resolved at compile time:
	Endpoint *Endpoint `yaml:"-"`
	Plan     *Plan     `yaml:"-"`
}

// Pool groups interchangeable targets (PRD §7).
type Pool struct {
	ID      string             `yaml:"id"`
	Targets []ResourceTargetID `yaml:"targets"`
}

// ---------------------------------------------------------------------------
// Alias & Objective (PRD §6.4/§6.5, F-P0-07/08)

type AliasContract struct {
	Name                 string       `yaml:"-"`
	RequiredCapabilities Requirements `yaml:"-"`
	MinContext           int64        `yaml:"minimum_context"`
	AllowDegradation     bool         `yaml:"allow_degradation"`
	DefaultObjective     string       `yaml:"default_objective"`
	// Raw capability flags parsed separately into RequiredCapabilities.
	rawCaps map[string]bool
}

// ObjectiveProfile expresses optimization goals (PRD §13.2).
type ObjectiveProfile struct {
	Name        string   `yaml:"-"`
	RankKeys    []string `yaml:"rank"` // lexicographic keys, see rank.go
	MaxCashCost *float64 `yaml:"max_cash_cost"`
	NoCashSpend bool     `yaml:"-"` // derived: max_cash_cost == 0
}

// ---------------------------------------------------------------------------
// Policy DSL types (PRD §13.3) — raw YAML shape before compilation.

type PolicyDoc struct {
	Timezone string                    `yaml:"timezone"`
	Rules    []PolicyRule              `yaml:"rules"`
	Tiers    []TierDef                 `yaml:"tiers"`
	Recovery map[string]RecoveryAction `yaml:"recovery"`
}

type PolicyRule struct {
	Name   string     `yaml:"name"`
	When   RuleWhen   `yaml:"when"`
	Action RuleAction `yaml:"action"`
}

type RuleWhen struct {
	Target          string      `yaml:"target"` // target id or pool id prefix match "*"
	Pool            string      `yaml:"pool"`
	TimeWindow      *TimeWindow `yaml:"time_window"`      // {"start":"14:00","end":"18:00"}
	Weekdays        []string    `yaml:"weekdays"`         // mon..sun
	Health          string      `yaml:"health"`           // "unhealthy"|"degraded"|...
	Cooldown        bool        `yaml:"cooldown"`         // currently cooling down
	QuotaRemaining  *FloatRange `yaml:"quota_remaining"`  // fraction 0..1
	QuotaConfidence string      `yaml:"quota_confidence"` // high|medium|low
	QuotaMultiplier *FloatRange `yaml:"quota_consumption_multiplier"`
	ConsumerKey     string      `yaml:"consumer_key"`
}

type TimeWindow struct {
	Start string `json:"start" yaml:"start"`
	End   string `json:"end" yaml:"end"`
}

type FloatRange struct {
	Gte *float64 `yaml:"gte"`
	Lte *float64 `yaml:"lte"`
	Gt  *float64 `yaml:"gt"`
	Lt  *float64 `yaml:"lt"`
	Eq  *float64 `yaml:"eq"`
}

type RuleAction struct {
	Exclude *bool  `yaml:"exclude"`
	Require string `yaml:"require"` // capability name that must be present
	Prefer  *bool  `yaml:"prefer"`  // boost within tier
	Pin     *bool  `yaml:"pin"`     // if eligible, must be chosen first
	Tier    string `yaml:"tier"`    // move matching targets to this tier
}

type TierDef struct {
	Name      string `yaml:"name"`
	Pool      string `yaml:"pool"`
	Objective string `yaml:"objective"`
}

type RecoveryAction struct {
	NextTarget string `yaml:"next_target"` // "same_tier" | explicit target id
	NextTier   bool   `yaml:"next_tier"`
	Abort      bool   `yaml:"abort"`
}

// ---------------------------------------------------------------------------
// Compiled runtime snapshot — immutable once published (F-P0-24).

// CompiledSnapshot is the atomic unit served to the data plane.
type CompiledSnapshot struct {
	Concurrency *ConcurrencyPolicy
	Version     int64
	CreatedAt   time.Time
	Hash        string
	// RuntimeTimezone is Ferry's explicitly configured wall-clock zone.
	// Runtime matching must not depend on the host process' local timezone.
	RuntimeTimezone *time.Location
	Targets         map[ResourceTargetID]*CompiledTarget
	TargetOrder     []ResourceTargetID // deterministic iteration order
	Pools           map[string][]ResourceTargetID
	Aliases         map[string]*CompiledAlias
	Objectives      map[string]*ObjectiveProfile
	Policies        map[string]*CompiledPolicy
	DefaultPolicy   string
	// RoutingPolicies is the ordered, independently enabled policy set used by
	// current runtimes. Policies/DefaultPolicy remain readable only for a
	// deterministic one-time migration from the legacy graph model.
	RoutingPolicies []RoutingPolicySpec
	PolicyIndexHash string
	// V2DomainSource distinguishes the authoritative native product domain
	// from the explicit legacy_projection compatibility surface.
	V2DomainSource    string
	LogicalModels     map[string]*LogicalModel
	Providers         map[string]*Provider
	Connections       map[string]*Connection
	ProviderModels    map[string]*ProviderModel
	V2MigrationIssues []LegacyMappingIssue
}

// CompiledTarget is a fully resolved, immutable target.
type CompiledTarget struct {
	ID         ResourceTargetID
	Endpoint   *Endpoint
	Credential CredentialRef
	Model      string
	ModelAlias string
	Plan       *Plan
	Bucket     *QuotaBucket
	// Buckets lists every stacked quota bucket of the plan (Bucket first).
	// Nil/empty falls back to Bucket — the classic single-bucket shape.
	Buckets      []*QuotaBucket
	Capabilities Capabilities
	// ProtocolCapabilities is the evidence-refined view for each native wire
	// protocol. Capabilities remains the aggregate union used by inventory and
	// protocol-agnostic linting; live eligibility must call CapabilitiesFor on
	// the final egress protocol.
	ProtocolCapabilities       map[Protocol]Capabilities       `yaml:"-"`
	ProtocolCapabilityStatuses map[Protocol]CapabilityStatuses `yaml:"-"`
	Weight                     int
	Priority                   int
	PoolIDs                    []string
	ProviderID                 string
	ConnectionID               string

	// Disabled marks a channel-expanded target whose channel is switched off:
	// it stays in the snapshot (pools and lints keep their shape) but the
	// scheduler excludes it with an actionable reason and publish assigns no
	// egress caller.
	Disabled bool `yaml:"-"`

	// NativeProtocols lists the wire formats the upstream speaks natively for
	// this model. Nil for classic resource_targets (single-protocol endpoints):
	// consumers fall back to comparing against Endpoint.Protocol.
	NativeProtocols []Protocol `yaml:"-"`

	// ProtocolConversion is copied from the owning V2 Provider. Nil retains
	// the historical behavior where all ingress protocols may be converted.
	ProtocolConversion map[Protocol]bool `yaml:"-"`
}

// QuotaBuckets returns every quota bucket attached to the target: the stacked
// Buckets when present, otherwise the legacy single Bucket.
func (t *CompiledTarget) QuotaBuckets() []*QuotaBucket {
	if t == nil {
		return nil
	}
	if len(t.Buckets) > 0 {
		return t.Buckets
	}
	if t.Bucket != nil {
		return []*QuotaBucket{t.Bucket}
	}
	return nil
}

// SpeaksNatively reports whether this target serves ingress protocol p without
// conversion. Classic targets (no channel expansion) speak only their
// endpoint's single protocol; channel-expanded targets speak their full set.
func (t *CompiledTarget) SpeaksNatively(p Protocol) bool {
	if len(t.NativeProtocols) == 0 {
		return t.Endpoint != nil && t.Endpoint.Protocol == p
	}
	return HasProtocol(t.NativeProtocols, p)
}

// AcceptsProtocol distinguishes native support from an explicitly enabled
// Ferry conversion path for the ingress protocol.
func (t *CompiledTarget) AcceptsProtocol(p Protocol) bool {
	if t.SpeaksNatively(p) {
		return true
	}
	// Images is an exact/native-only protocol. Its generation and edit bodies
	// are not meaningfully convertible to or from the three text protocols.
	if p == ProtocolOpenAIImages || t.EgressProtocol(p) == ProtocolOpenAIImages {
		return false
	}
	return t.ProtocolConversion == nil || t.ProtocolConversion[p]
}

// CompiledAlias carries the workload contract.
type CompiledAlias struct {
	Name             string
	RequiredCaps     Requirements
	MinContext       int64
	AllowDegradation bool
	DefaultObjective string

	// Synthesized marks aliases the compiler generated for channel-model
	// client-facing names rather than user-written contracts. Such aliases
	// ARE concrete model names, so scheduling routes strictly by
	// ClientModel() match — pools mix many models and an opus request must
	// never land on a deepseek target sharing its pool. User aliases keep
	// classic capability-contract semantics across models.
	Synthesized bool `yaml:"-"`
}

// CompiledPolicy is a validated, compiled policy.
type CompiledPolicy struct {
	Name     string
	Timezone *time.Location
	Rules    []CompiledRule
	Tiers    []TierDef
	Recovery map[FailureType]RecoveryAction
	// StepRecovery is present for ordered routing policies. Each map is the
	// explicit fallback allowlist of the corresponding route step.
	StepRecovery []map[FailureType]bool
	Hash         string
}

// CompiledRule is a pre-parsed rule with typed predicates.
type CompiledRule struct {
	Name   string
	When   CompiledWhen
	Action RuleAction
}

type CompiledWhen struct {
	HasTarget       bool
	Target          ResourceTargetID
	HasPool         bool
	Pool            string
	Window          *TimeWindow
	Weekdays        map[string]bool // empty = all days
	Health          string
	WantCooldown    bool
	HasCooldown     bool
	QuotaRemaining  *FloatRange
	QuotaConfidence string
	QuotaMult       *FloatRange
	ConsumerKey     string
}

// ResolveModel returns the model name clients should see.
func (t *CompiledTarget) ClientModel() string {
	if t.ModelAlias != "" {
		return t.ModelAlias
	}
	return t.Model
}

// EgressProtocol picks the wire format used to reach this target for a request
// that arrived as ingress p: native passthrough when possible, otherwise the
// least-lossy native format (canonical-contract conversion fills the gap).
func (t *CompiledTarget) EgressProtocol(p Protocol) Protocol {
	if t.SpeaksNatively(p) {
		return p
	}
	if len(t.NativeProtocols) > 0 {
		return PreferredProtocol(t.NativeProtocols)
	}
	return t.Endpoint.Protocol
}

// UpstreamCapabilitiesFor returns the isolated native capability evidence for
// one egress protocol. Classic targets compiled before protocol-scoped
// evidence existed retain their aggregate declaration as a compatibility
// fallback.
func (t *CompiledTarget) UpstreamCapabilitiesFor(protocol Protocol) Capabilities {
	if t == nil {
		return Capabilities{}
	}
	if capabilities, ok := t.ProtocolCapabilities[protocol]; ok {
		return capabilities
	}
	return t.Capabilities
}

// CapabilitiesFor returns the client-visible capability contract after
// Ferry's verified semantic mappings are applied. Attempt preparation must
// use UpstreamCapabilitiesFor to determine whether a mapping is required.
func (t *CompiledTarget) CapabilitiesFor(protocol Protocol) Capabilities {
	return EffectiveCapabilities(t.UpstreamCapabilitiesFor(protocol))
}

// CurrentMultiplier returns the quota consumption multiplier active at time
// now in the plan's timezone (F-P0-14). Default 1.0.
func (p *Plan) CurrentMultiplier(now time.Time) float64 {
	if p == nil || len(p.Windows) == 0 {
		return 1.0
	}
	loc := time.UTC
	if p.Timezone != "" {
		if l, err := time.LoadLocation(p.Timezone); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	minutesOfDay := local.Hour()*60 + local.Minute()
	for _, w := range p.Windows {
		sh, sm, ok1 := parseHHMM(w.Start)
		eh, em, ok2 := parseHHMM(w.End)
		if !ok1 || !ok2 {
			continue
		}
		s, e := sh*60+sm, eh*60+em
		inWindow := false
		if s <= e {
			inWindow = minutesOfDay >= s && minutesOfDay < e
		} else { // wraps midnight
			inWindow = minutesOfDay >= s || minutesOfDay < e
		}
		if inWindow {
			return w.Multiplier
		}
	}
	return 1.0
}

func parseHHMM(s string) (int, int, bool) {
	var h, m int
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// NextRefresh computes when the bucket next resets, given its cadence and
// anchor wall-clock time At in the bucket timezone. For rolling buckets this
// is now + period (the window is anchored at the observation moment).
func (q *QuotaBucket) NextRefresh(now time.Time) time.Time {
	if q == nil {
		return now
	}
	loc := time.UTC
	if q.Refresh.Timezone != "" {
		if l, err := time.LoadLocation(q.Refresh.Timezone); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	atH, atM := 0, 0
	if q.Refresh.At != "" {
		if h, m, ok := parseHHMM(q.Refresh.At); ok {
			atH, atM = h, m
		}
	}
	todayReset := time.Date(local.Year(), local.Month(), local.Day(), atH, atM, 0, 0, loc)
	switch q.Refresh.Period {
	case "daily":
		if local.Before(todayReset) {
			return todayReset
		}
		return todayReset.AddDate(0, 0, 1)
	case "weekly":
		cand := todayReset
		for i := 0; i < 8; i++ {
			if cand.After(local) && cand.Weekday() == time.Monday {
				return cand
			}
			cand = cand.AddDate(0, 0, 1)
		}
		return cand
	case "monthly":
		cand := time.Date(local.Year(), local.Month(), 1, atH, atM, 0, 0, loc)
		if !cand.After(local) {
			cand = cand.AddDate(0, 1, 0)
		}
		return cand
	default: // duration like "24h"
		if d, rolling := q.Refresh.IsRolling(); rolling {
			return now.Add(d)
		}
		d, err := time.ParseDuration(q.Refresh.Period)
		if err != nil || d <= 0 {
			return now.Add(24 * time.Hour)
		}
		anchor := time.Unix(now.Unix()/int64(d)*int64(d), 0)
		next := anchor.Add(d)
		if next.Before(now) {
			next = now.Add(d)
		}
		return next
	}
}

// SortIDs returns a deterministically sorted copy of ids.
func SortIDs[T ~string](ids []T) []T {
	out := make([]T, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
