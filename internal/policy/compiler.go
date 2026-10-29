// Package policy implements the Policy DSL compiler, linter, and the
// deterministic scheduling engine (F-P0-11..15, F-P0-23/24).
//
// Determinism contract (G2): given the same RequestContract requirements,
// CompiledSnapshot version, and domain.Snapshot, Plan() always returns the
// same execution order. All iteration is over sorted keys; tie-breaks are
// target ID lexicographic.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
)

// CapabilityEvidenceLookup returns one fresh provider/model/protocol-scoped
// capability observation. A missing, expired, or inconclusive observation is
// reported with found=false and leaves the authoritative configuration intact.
type CapabilityEvidenceLookup func(providerID, endpointFingerprint, upstreamModel string,
	protocol domain.Protocol, capability string, now time.Time) (supported bool, found bool, err error)

// Compiler turns a validated config.File into immutable snapshots.
type Compiler struct {
	capabilityEvidence CapabilityEvidenceLookup
}

// NewCompiler creates a compiler.
func NewCompiler() *Compiler { return &Compiler{} }

// NewCompilerWithCapabilityEvidence creates a compiler whose live capability
// views are refined by fresh endpoint evidence. The configuration remains the
// fallback for absent evidence and the source of numeric model limits.
func NewCompilerWithCapabilityEvidence(lookup CapabilityEvidenceLookup) *Compiler {
	return &Compiler{capabilityEvidence: lookup}
}

// Compile builds the initial snapshot from config. Structural validation has
// already run in config.Validate; this pass resolves references and lints
// cross-object semantics that config cannot see (empty candidate sets,
// unreachable tiers).
// attachQuotaBuckets resolves every bucket referenced by the plan (legacy
// single Quota first, then stacked Quotas) onto the compiled target. Bucket
// stays the primary entry for single-bucket consumers.
func attachQuotaBuckets(ct *domain.CompiledTarget, plan *domain.Plan, buckets []domain.QuotaBucket) {
	refs := plan.QuotaRefs()
	if len(refs) == 0 {
		return
	}
	for i := range buckets {
		for _, ref := range refs {
			if buckets[i].ID == ref.BucketID {
				if ct.Bucket == nil {
					ct.Bucket = &buckets[i]
				}
				ct.Buckets = append(ct.Buckets, &buckets[i])
				break
			}
		}
	}
}

func (c *Compiler) Compile(f *config.File) (*domain.CompiledSnapshot, error) {
	compiledAt := time.Now()
	timezoneName := f.Server.Timezone
	if timezoneName == "" {
		timezoneName = "Asia/Shanghai"
	}
	runtimeTimezone, err := time.LoadLocation(timezoneName)
	if err != nil {
		return nil, fmt.Errorf("runtime timezone %q: %w", timezoneName, err)
	}
	snap := &domain.CompiledSnapshot{
		Concurrency:     f.Concurrency,
		RuntimeTimezone: runtimeTimezone,
		V2DomainSource:  f.V2DomainSource,
		Targets:         map[domain.ResourceTargetID]*domain.CompiledTarget{},
		Pools:           map[string][]domain.ResourceTargetID{},
		Aliases:         map[string]*domain.CompiledAlias{},
		Objectives:      map[string]*domain.ObjectiveProfile{},
		Policies:        map[string]*domain.CompiledPolicy{},
		LogicalModels:   map[string]*domain.LogicalModel{},
		Providers:       map[string]*domain.Provider{},
		Connections:     map[string]*domain.Connection{},
		ProviderModels:  map[string]*domain.ProviderModel{},
		CreatedAt:       compiledAt,
	}
	for i := range f.LogicalModels {
		model := f.LogicalModels[i]
		snap.LogicalModels[model.ID] = &model
	}
	for i := range f.Providers {
		provider := &f.Providers[i]
		snap.Providers[provider.ID] = provider
	}
	type compiledConnectionModelCapabilities struct {
		aggregate domain.Capabilities
		protocols map[domain.Protocol]domain.Capabilities
		statuses  map[domain.Protocol]domain.CapabilityStatuses
	}
	connectionModelCapabilities := map[string]compiledConnectionModelCapabilities{}
	for i := range f.Connections {
		connection := &f.Connections[i]
		snap.Connections[connection.ID] = connection
		provider := snap.Providers[connection.ProviderID]
		for j := range connection.Models {
			model := &connection.Models[j]
			capabilities, protocolCapabilities, statuses, err := c.resolveConnectionModelCapabilities(provider, connection, model, compiledAt)
			if err != nil {
				return nil, fmt.Errorf("connection %s model %s capabilities: %w", connection.ID, model.UpstreamModel, err)
			}
			connectionModelCapabilities[domain.ProviderModelKey(connection.ID, model.UpstreamModel)] = compiledConnectionModelCapabilities{
				aggregate: capabilities, protocols: protocolCapabilities, statuses: statuses,
			}
			key := domain.ProviderModelKey(connection.ProviderID, model.UpstreamModel)
			view := snap.ProviderModels[key]
			if view == nil {
				view = &domain.ProviderModel{ProviderID: connection.ProviderID, UpstreamModel: model.UpstreamModel}
				snap.ProviderModels[key] = view
			}
			view.SupportedConnections = append(view.SupportedConnections, connection.ID)
			if connection.Enabled && provider != nil && provider.Enabled {
				view.EligibleConnections = append(view.EligibleConnections, connection.ID)
			}
			view.Capabilities = domain.MergeCapabilities(view.Capabilities, domain.EffectiveCapabilities(capabilities))
		}
	}
	for _, view := range snap.ProviderModels {
		slices.Sort(view.SupportedConnections)
		slices.Sort(view.EligibleConnections)
	}
	snap.V2MigrationIssues = append([]domain.LegacyMappingIssue(nil), f.V2MigrationIssues...)
	targetOwners := map[domain.ResourceTargetID][2]string{}
	modelTargetIDs := map[string]domain.ResourceTargetID{}
	for i := range f.Connections {
		connection := &f.Connections[i]
		for _, model := range connection.Models {
			targetID := model.ExecutionTargetID
			if targetID == "" {
				targetID = connection.RuntimeTargetID(model.UpstreamModel)
			}
			targetOwners[targetID] = [2]string{connection.ProviderID, connection.ID}
			modelTargetIDs[domain.ProviderModelKey(connection.ID, model.UpstreamModel)] = targetID
		}
	}

	for i := range f.Targets {
		t := &f.Targets[i]
		protocols := []domain.Protocol{}
		if t.Endpoint != nil {
			protocols = append(protocols, t.Endpoint.Protocol)
		}
		capabilities := domain.NormalizeProviderModelCapabilities(
			domain.ProviderCustom, t.Model, protocols, t.Capabilities)
		protocolCapabilities := map[domain.Protocol]domain.Capabilities{}
		if t.Endpoint != nil && t.Endpoint.Protocol != "" {
			capabilities = domain.ProtocolCapabilityBaseline(t.Endpoint.Protocol, capabilities)
			protocolCapabilities[t.Endpoint.Protocol] = capabilities
		}
		ct := &domain.CompiledTarget{
			ID:                   t.ID,
			Endpoint:             t.Endpoint,
			Credential:           t.Credential,
			Model:                t.Model,
			ModelAlias:           t.ModelAlias,
			Plan:                 t.Plan,
			Capabilities:         capabilities,
			ProtocolCapabilities: protocolCapabilities,
			Weight:               t.Weight,
			Priority:             t.Priority,
		}
		if owner, ok := targetOwners[t.ID]; ok {
			ct.ProviderID, ct.ConnectionID = owner[0], owner[1]
		}
		if ct.Weight == 0 {
			ct.Weight = 1
		}
		attachQuotaBuckets(ct, t.Plan, f.QuotaBuckets)
		snap.Targets[t.ID] = ct
	}

	// Pool memberships are collected from both sources — explicit pool target
	// lists and channel/model auto-join — then merged once below, so a channel
	// model inheriting a pool that also lists classic targets lands in that
	// pool exactly once instead of being clobbered by the explicit list.
	poolMembers := map[string][]domain.ResourceTargetID{}
	poolTargets := map[string]bool{}
	for i := range f.Pools {
		poolMembers[f.Pools[i].ID] = append(poolMembers[f.Pools[i].ID], f.Pools[i].Targets...)
	}

	// Channel expansion: each channel×model pair becomes an ordinary target
	// with id "<channel>.<model>" so scheduling, ranking, quota, health, and
	// recovery reuse the existing machinery untouched. Disabled channels keep
	// their targets in the snapshot flagged Disabled (pools and lints hold
	// their shape; the scheduler excludes them with an actionable reason).
	for i := range f.Channels {
		ch := &f.Channels[i]
		channelPool := "__channel_" + ch.ID
		for j := range ch.Models {
			m := &ch.Models[j]
			providerType := domain.ProviderCustom
			protocols := m.Protocols
			if ch.Type == domain.ChannelCliproxyCodex {
				providerType = domain.ProviderCliproxyCodex
				protocols = []domain.Protocol{domain.CodexModelNativeProtocol(m.Model)}
			}
			protocolCapabilities := make(map[domain.Protocol]domain.Capabilities, len(protocols))
			capabilities := domain.NormalizeProviderModelCapabilities(providerType, m.Model, nil, m.Capabilities)
			if len(protocols) > 0 {
				capabilities = domain.Capabilities{}
				for _, protocol := range protocols {
					view := domain.NormalizeProviderModelCapabilities(
						providerType, m.Model, []domain.Protocol{protocol}, m.Capabilities)
					view = domain.ProtocolCapabilityBaseline(protocol, view)
					protocolCapabilities[protocol] = view
					capabilities = domain.MergeCapabilities(capabilities, view)
				}
			}
			tid := domain.ResourceTargetID(domain.ChannelTargetID(ch.ID, m.Model))
			pid := m.PlanRef(ch)
			var plan *domain.Plan
			if pid != "" {
				for k := range f.Plans {
					if f.Plans[k].ID == pid {
						plan = &f.Plans[k]
						break
					}
				}
			}
			ct := &domain.CompiledTarget{
				ID:         tid,
				Model:      m.Model,
				ModelAlias: m.Alias,
				// Per-model virtual endpoint: the default wire format is its
				// least-lossy native protocol. Request planning may select a
				// different declared native format through EgressProtocol.
				Endpoint: &domain.Endpoint{
					ID:       "__channel_" + ch.ID,
					BaseURL:  ch.BaseURL,
					Protocol: domain.PreferredProtocol(protocols),
				},
				Credential:           ch.Credential,
				Plan:                 plan,
				Capabilities:         capabilities,
				ProtocolCapabilities: protocolCapabilities,
				Weight:               m.Weight,
				Priority:             m.Priority,
				NativeProtocols:      protocols,
				Disabled:             !ch.EnabledOrDefault(),
			}
			if owner, ok := targetOwners[tid]; ok {
				ct.ProviderID, ct.ConnectionID = owner[0], owner[1]
			}
			if ch.Type == domain.ChannelCliproxyCodex {
				// Built-in OAuth account: the credential is the channel's own
				// login (one channel = one account), resolved through the
				// oauth manager at publish time — never from config secrets.
				ct.Credential = domain.CredentialRef{SecretRef: domain.OAuthRef(ch.ID)}
			}
			if ct.Weight == 0 {
				ct.Weight = 1
			}
			attachQuotaBuckets(ct, plan, f.QuotaBuckets)
			snap.Targets[tid] = ct
			poolMembers[channelPool] = append(poolMembers[channelPool], tid)

			pools := m.Pools
			if len(pools) == 0 {
				pools = ch.Pools
			}
			for _, pn := range pools {
				poolMembers[pn] = append(poolMembers[pn], tid)
				ct.PoolIDs = append(ct.PoolIDs, pn)
			}
		}
	}

	// Native V2 configurations persist product objects, not legacy Targets.
	// Materialize the private Connection×UpstreamModel leaves at compile time
	// so the proven adapters, health accounting and attempt ledger remain the
	// execution substrate without leaking Target into the product contract.
	for i := range f.Connections {
		connection := &f.Connections[i]
		provider := snap.Providers[connection.ProviderID]
		if provider == nil {
			continue
		}
		for j := range connection.Models {
			model := &connection.Models[j]
			targetID := modelTargetIDs[domain.ProviderModelKey(connection.ID, model.UpstreamModel)]
			if snap.Targets[targetID] != nil {
				continue
			}
			var plan *domain.Plan
			// Per-model plan wins; otherwise the connection-level default bills
			// every model of the account (legacy channel inheritance in V2).
			planID := model.PlanID
			if planID == "" {
				planID = connection.PlanID
			}
			if planID != "" {
				for k := range f.Plans {
					if f.Plans[k].ID == planID {
						plan = &f.Plans[k]
						break
					}
				}
			}
			protocols := model.Protocols
			if provider.Type == domain.ProviderCliproxyCodex {
				protocols = []domain.Protocol{domain.CodexModelNativeProtocol(model.UpstreamModel)}
			}
			protocol := domain.PreferredProtocol(protocols)
			compiledCapabilities := connectionModelCapabilities[domain.ProviderModelKey(connection.ID, model.UpstreamModel)]
			capabilities := compiledCapabilities.aggregate
			baseURL := provider.BaseURL
			if connection.BaseURL != "" {
				baseURL = connection.BaseURL
			}
			ct := &domain.CompiledTarget{
				ID: targetID, Endpoint: &domain.Endpoint{
					ID: "__provider_" + provider.ID, BaseURL: baseURL, FullURL: connection.FullURL, Protocol: protocol,
				},
				Credential: connection.Credential, Model: model.UpstreamModel, Plan: plan,
				Capabilities: capabilities, Weight: 1,
				ProviderID: provider.ID, ConnectionID: connection.ID,
				NativeProtocols:    append([]domain.Protocol(nil), protocols...),
				ProtocolConversion: provider.EffectiveProtocolConversion(),
				Disabled:           !provider.Enabled || !connection.Enabled,
			}
			attachQuotaBuckets(ct, plan, f.QuotaBuckets)
			snap.Targets[targetID] = ct
		}
	}
	// Legacy projections may have materialized a target before the native V2
	// loop above. Apply the same protocol-isolated connection-model evidence to
	// every owned target so both paths make identical live decisions.
	for i := range f.Connections {
		connection := &f.Connections[i]
		for j := range connection.Models {
			model := &connection.Models[j]
			targetID := modelTargetIDs[domain.ProviderModelKey(connection.ID, model.UpstreamModel)]
			compiledCapabilities := connectionModelCapabilities[domain.ProviderModelKey(connection.ID, model.UpstreamModel)]
			if target := snap.Targets[targetID]; target != nil {
				target.Capabilities = compiledCapabilities.aggregate
				target.ProtocolCapabilities = cloneProtocolCapabilities(compiledCapabilities.protocols)
				target.ProtocolCapabilityStatuses = compiledCapabilities.statuses
			}
		}
	}
	snap.TargetOrder = domain.SortIDs(mapKeys(snap.Targets))

	for pn, ids := range poolMembers {
		// One target can join a pool twice — listed explicitly AND auto-joined
		// through its channel's pools. Collapse to one entry: duplicate members
		// would surface as duplicate candidates and retry the same upstream.
		seen := make(map[domain.ResourceTargetID]bool, len(ids))
		uniq := make([]domain.ResourceTargetID, 0, len(ids))
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				uniq = append(uniq, id)
			}
		}
		snap.Pools[pn] = domain.SortIDs(uniq)
		for _, id := range uniq {
			poolTargets[string(id)] = true
			if ct, ok := snap.Targets[id]; ok && !strings.HasPrefix(pn, "__channel_") && !slices.Contains(ct.PoolIDs, string(pn)) {
				ct.PoolIDs = append(ct.PoolIDs, pn)
			}
		}
	}

	for name, spec := range f.Aliases {
		a := &domain.CompiledAlias{
			Name:             name,
			MinContext:       spec.MinimumContext,
			DefaultObjective: spec.DefaultObjective,
		}
		if spec.AllowDegradation != nil {
			a.AllowDegradation = *spec.AllowDegradation
		}
		for capability, required := range spec.RequiredCapabilities {
			if !domain.SetRequirementCapability(&a.RequiredCaps, capability, required) {
				return nil, fmt.Errorf("alias %s: unknown capability %q", name, capability)
			}
		}
		if a.MinContext > 0 {
			a.RequiredCaps.MinContext = a.MinContext
		}
		snap.Aliases[name] = a
	}

	// Synthesize aliases for channel-model client-facing names that no
	// user-defined alias already covers, so requests naming those models
	// resolve (decide() fails "unknown model/alias" otherwise) and the lint's
	// alias-satisfiability check sees them. Disabled channels are skipped:
	// their models must stay unaddressable until the channel is re-enabled.
	for i := range f.Channels {
		ch := &f.Channels[i]
		if !ch.EnabledOrDefault() {
			continue
		}
		for j := range ch.Models {
			m := &ch.Models[j]
			name := m.ClientFacing()
			if _, exists := snap.Aliases[name]; !exists {
				a := &domain.CompiledAlias{
					Name:        name,
					Synthesized: true,
				}
				snap.Aliases[name] = a
			}
		}
	}

	for name, spec := range f.Objectives {
		o := &domain.ObjectiveProfile{
			Name:        name,
			RankKeys:    spec.Rank,
			MaxCashCost: spec.MaxCashCost,
		}
		if o.MaxCashCost != nil && *o.MaxCashCost == 0 {
			o.NoCashSpend = true
		}
		snap.Objectives[name] = o
	}
	// built-in fallback objective when none specified anywhere
	if _, ok := snap.Objectives["balanced"]; !ok {
		snap.Objectives["balanced"] = &domain.ObjectiveProfile{
			Name:     "balanced",
			RankKeys: []string{"capability_fit", "min_effective_cost", "max_reliability", "min_latency"},
		}
	}

	for pname, py := range f.Policies {
		cp, err := compilePolicy(pname, py, snap)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", pname, err)
		}
		snap.Policies[pname] = cp
	}
	snap.DefaultPolicy = f.DefaultPolicy
	for _, spec := range f.RoutingPolicies {
		cp, err := compileRoutingPolicy(spec, snap)
		if err != nil {
			return nil, err
		}
		snap.Policies[spec.ID] = cp
		snap.RoutingPolicies = append(snap.RoutingPolicies, spec)
	}

	// Lint across the whole snapshot.
	if err := c.lint(snap); err != nil {
		return nil, err
	}

	snap.Hash, err = c.hashConfiguration(f, snap.Version, snap.RuntimeTimezone)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// CompileAtRevision compiles one immutable runtime snapshot at the exact
// durable Configuration Revision selected by the control store. Runtime
// world replacement is not itself a configuration revision boundary.
func (c *Compiler) CompileAtRevision(f *config.File, revision int64) (*domain.CompiledSnapshot, error) {
	snap, err := c.Compile(f)
	if err != nil {
		return nil, err
	}
	snap.Version = revision
	snap.Hash, err = c.hashConfiguration(f, revision, snap.RuntimeTimezone)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func (c *Compiler) resolveConnectionModelCapabilities(provider *domain.Provider, connection *domain.Connection,
	model *domain.ConnectionModel, now time.Time) (domain.Capabilities, map[domain.Protocol]domain.Capabilities, map[domain.Protocol]domain.CapabilityStatuses, error) {
	if connection == nil || model == nil {
		return domain.Capabilities{}, nil, nil, nil
	}
	providerType := domain.ProviderType("")
	protocols := append([]domain.Protocol(nil), model.Protocols...)
	if provider != nil {
		providerType = provider.Type
		if provider.Type == domain.ProviderCliproxyCodex {
			protocols = []domain.Protocol{domain.CodexModelNativeProtocol(model.UpstreamModel)}
		}
	}
	if len(protocols) == 0 {
		return domain.NormalizeProviderModelCapabilities(providerType, model.UpstreamModel, nil, model.Capabilities), nil, nil, nil
	}

	protocolCapabilities := make(map[domain.Protocol]domain.Capabilities, len(protocols))
	statuses := make(map[domain.Protocol]domain.CapabilityStatuses, len(protocols))
	aggregate := domain.Capabilities{}
	for _, protocol := range protocols {
		capabilities := domain.NormalizeProviderModelCapabilities(
			providerType, model.UpstreamModel, []domain.Protocol{protocol}, model.Capabilities)
		capabilities = domain.ProtocolCapabilityBaseline(protocol, capabilities)
		states := domain.DeclaredCapabilityStatuses(capabilities, protocol)
		if c.capabilityEvidence != nil && provider != nil {
			baseURL := provider.BaseURL
			if connection.BaseURL != "" {
				baseURL = connection.BaseURL
			}
			endpoint, err := domain.ProviderProtocolEndpointURL(baseURL, connection.FullURL, protocol)
			if err != nil {
				return domain.Capabilities{}, nil, nil, err
			}
			fingerprint := domain.ProviderCapabilityEndpointFingerprint(provider.Type, endpoint)
			for _, capability := range domain.ProbeCapabilitiesForProtocol(protocol) {
				supported, found, err := c.capabilityEvidence(
					provider.ID, domain.ConnectionCapabilityFingerprint(fingerprint, connection), model.UpstreamModel, protocol, capability, now)
				if err == nil && !found {
					supported, found, err = c.capabilityEvidence(provider.ID, fingerprint, model.UpstreamModel, protocol, capability, now)
				}
				if err != nil {
					return domain.Capabilities{}, nil, nil, err
				}
				if found {
					states[capability] = domain.CapabilityUnsupported
					if supported {
						states[capability] = domain.CapabilitySupported
					}
				}
				if found && !domain.SetBooleanCapability(&capabilities, capability, supported) {
					return domain.Capabilities{}, nil, nil, fmt.Errorf("unsupported evidence capability %q", capability)
				}
			}
		}
		protocolCapabilities[protocol] = capabilities
		statuses[protocol] = states
		aggregate = domain.MergeCapabilities(aggregate, capabilities)
	}
	return aggregate, protocolCapabilities, statuses, nil
}

func cloneProtocolCapabilities(source map[domain.Protocol]domain.Capabilities) map[domain.Protocol]domain.Capabilities {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[domain.Protocol]domain.Capabilities, len(source))
	for protocol, capabilities := range source {
		cloned[protocol] = capabilities
	}
	return cloned
}

func compileRoutingPolicy(spec domain.RoutingPolicySpec, snap *domain.CompiledSnapshot) (*domain.CompiledPolicy, error) {
	contentHash, err := domain.RoutingPolicyContentHash(spec)
	if err != nil {
		return nil, fmt.Errorf("routing policy %s: %w", spec.ID, err)
	}
	cp := &domain.CompiledPolicy{
		Name:     spec.ID,
		Timezone: time.UTC,
		Recovery: map[domain.FailureType]domain.RecoveryAction{},
		Hash:     contentHash,
	}
	if spec.Match.Timezone != "" {
		cp.Timezone, err = time.LoadLocation(spec.Match.Timezone)
		if err != nil {
			return nil, fmt.Errorf("routing policy %s: invalid timezone %q: %w", spec.ID, spec.Match.Timezone, err)
		}
	}
	for i, step := range spec.Route.Steps {
		poolName := step.ChannelGroup
		if step.Channel != "" {
			poolName = "__channel_" + step.Channel
		}
		if len(snap.Pools[poolName]) == 0 {
			return nil, fmt.Errorf("routing policy %s step %d: route %q has no targets", spec.ID, i+1, poolName)
		}
		cp.Tiers = append(cp.Tiers, domain.TierDef{
			Name: fmt.Sprintf("route-%02d", i+1), Pool: poolName, Objective: step.Objective,
		})
		allowed := make(map[domain.FailureType]bool, len(step.RecoverOn))
		for _, failure := range step.RecoverOn {
			allowed[failure] = true
			cp.Recovery[failure] = domain.RecoveryAction{NextTier: true}
		}
		cp.StepRecovery = append(cp.StepRecovery, allowed)
	}
	// New policy rules are fail-closed unless fallback is explicitly allowed.
	for _, failure := range []domain.FailureType{
		domain.FailAuth, domain.FailUnsupportedParam, domain.FailContextOverflow,
		domain.FailClientCancel,
	} {
		cp.Recovery[failure] = domain.RecoveryAction{Abort: true}
	}
	return cp, nil
}

func compilePolicy(name string, py config.PolicyYAML, snap *domain.CompiledSnapshot) (*domain.CompiledPolicy, error) {
	cp := &domain.CompiledPolicy{
		Name:     name,
		Tiers:    py.Tiers,
		Recovery: map[domain.FailureType]domain.RecoveryAction{},
	}
	tz := py.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	cp.Timezone = loc

	for _, r := range py.Rules {
		cr := domain.CompiledRule{Name: r.Name, Action: r.Action}
		w := r.When
		cr.When.HasTarget = w.Target != ""
		cr.When.Target = domain.ResourceTargetID(w.Target)
		cr.When.HasPool = w.Pool != ""
		cr.When.Pool = w.Pool
		cr.When.Window = w.TimeWindow
		cr.When.Weekdays = map[string]bool{}
		for _, d := range w.Weekdays {
			dn := strings.ToLower(d)[:3]
			switch dn {
			case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
				cr.When.Weekdays[dn] = true
			default:
				return nil, fmt.Errorf("rule %s: invalid weekday %q", r.Name, d)
			}
		}
		cr.When.Health = strings.ToLower(w.Health)
		cr.When.WantCooldown = w.Cooldown
		cr.When.HasCooldown = w.Cooldown // presence means predicate applies
		cr.When.QuotaRemaining = w.QuotaRemaining
		cr.When.QuotaConfidence = strings.ToLower(w.QuotaConfidence)
		cr.When.QuotaMult = w.QuotaMultiplier
		cr.When.ConsumerKey = w.ConsumerKey

		// validate time windows parse
		for _, tw := range []*domain.TimeWindow{cr.When.Window} {
			if tw == nil {
				continue
			}
			if !validHHMM(tw.Start) || !validHHMM(tw.End) {
				return nil, fmt.Errorf("rule %s: invalid time_window %q-%q", r.Name, tw.Start, tw.End)
			}
		}
		cp.Rules = append(cp.Rules, cr)
	}

	for fk, ra := range py.Recovery {
		ft := domain.FailureType(fk)
		if ft.RecoveryClass() == "" && ft != domain.FailNone {
			return nil, fmt.Errorf("unknown recovery failure type %q", fk)
		}
		cp.Recovery[ft] = ra
	}
	return cp, nil
}

func validHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	var h, m int
	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil {
		return false
	}
	return h >= 0 && h <= 23 && m >= 0 && m <= 59
}

func mapKeys[M ~map[K]V, K comparable, V any](m M) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lint enforces cross-object invariants that must block publish (F-P0-23):
//   - every tier pool exists and is non-empty
//   - no target appears twice within one tier chain via pools overlap? (allowed:
//     same target may serve multiple policies, but duplicate membership inside
//     one tier's pool is a config error caught earlier)
//   - at least one target in the whole policy can satisfy each alias's
//     minimum capability set (static part only — runtime state may still
//     exclude everything, which is a runtime condition, not a lint error)
func (c *Compiler) lint(snap *domain.CompiledSnapshot) error {
	for pname, pol := range snap.Policies {
		total := 0
		seen := map[domain.ResourceTargetID]string{}
		for _, tier := range pol.Tiers {
			ids, ok := snap.Pools[tier.Pool]
			if !ok || len(ids) == 0 {
				return fmt.Errorf("policy %s: tier %s pool %s empty or missing", pname, tier.Name, tier.Pool)
			}
			total += len(ids)
			for _, id := range ids {
				if prev, dup := seen[id]; dup {
					return fmt.Errorf("policy %s: target %s in tiers %s and %s (duplicate membership)",
						pname, id, prev, tier.Name)
				}
				seen[id] = tier.Name
			}
		}
		if total == 0 {
			return fmt.Errorf("policy %s: no candidates", pname)
		}
	}
	// Alias satisfiability is intentionally protocol-agnostic: compilation only
	// proves that the aggregate inventory has a possible target. Live routing
	// performs the strict check against the selected egress protocol.
	for aname, alias := range snap.Aliases {
		ok := false
		for _, t := range snap.Targets {
			if domain.EffectiveCapabilities(t.Capabilities).Satisfies(alias.RequiredCaps) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("alias %s: no configured target satisfies its capability contract", aname)
		}
	}
	return nil
}

type configurationHashCredential struct {
	SecretRef  string `json:"secret_ref"`
	Header     string `json:"header,omitempty"`
	QueryParam string `json:"query_param,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
}

type configurationHashTarget struct {
	ID           domain.ResourceTargetID     `json:"id"`
	EndpointID   string                      `json:"endpoint_id"`
	Credential   configurationHashCredential `json:"credential"`
	Model        string                      `json:"model"`
	ModelAlias   string                      `json:"model_alias,omitempty"`
	PlanID       string                      `json:"plan_id"`
	Capabilities domain.Capabilities         `json:"capabilities"`
	Weight       int                         `json:"weight"`
	Priority     int                         `json:"priority"`
}

type configurationHashChannelModel struct {
	Model        string              `json:"model"`
	Alias        string              `json:"alias,omitempty"`
	Protocols    []domain.Protocol   `json:"protocols"`
	PlanID       string              `json:"plan_id,omitempty"`
	Capabilities domain.Capabilities `json:"capabilities"`
	Weight       int                 `json:"weight"`
	Priority     int                 `json:"priority"`
	Pools        []string            `json:"pools,omitempty"`
}

type configurationHashChannel struct {
	ID         string                          `json:"id"`
	Type       domain.ChannelType              `json:"type"`
	BaseURL    string                          `json:"base_url"`
	Credential configurationHashCredential     `json:"credential"`
	Enabled    bool                            `json:"enabled"`
	Pools      []string                        `json:"pools,omitempty"`
	PlanID     string                          `json:"plan_id,omitempty"`
	Models     []configurationHashChannelModel `json:"models"`
}

type configurationHashOverride struct {
	ID        string              `json:"id"`
	Enabled   bool                `json:"enabled"`
	Callers   []string            `json:"callers,omitempty"`
	Protocols []domain.Protocol   `json:"protocols,omitempty"`
	Weekdays  []string            `json:"weekdays,omitempty"`
	Timezone  string              `json:"timezone,omitempty"`
	Window    *domain.TimeWindow  `json:"time_window,omitempty"`
	Required  domain.Requirements `json:"required_capabilities,omitempty"`
	Mode      domain.OverrideMode `json:"mode"`
	Prefer    []domain.RouteStep  `json:"prefer,omitempty"`
	Avoid     []domain.RouteStep  `json:"avoid,omitempty"`
	Route     *domain.RouteSpec   `json:"route,omitempty"`
}

type configurationHashLogicalModel struct {
	ID                           string                              `json:"id"`
	Enabled                      bool                                `json:"enabled"`
	ExactOnly                    bool                                `json:"exact_only,omitempty"`
	FirstOutputTimeoutSeconds    int                                 `json:"first_output_timeout_seconds,omitempty"`
	ExistingConversationStrategy domain.ExistingConversationStrategy `json:"existing_conversation_strategy,omitempty"`
	DefaultRoute                 domain.RouteSpec                    `json:"default_route"`
	Overrides                    []configurationHashOverride         `json:"overrides,omitempty"`
}

type configurationHashProvider struct {
	ID                 string                           `json:"id"`
	Type               domain.ProviderType              `json:"type"`
	BaseURL            string                           `json:"base_url,omitempty"`
	Enabled            bool                             `json:"enabled"`
	ProtocolConversion map[domain.Protocol]bool         `json:"protocol_conversion,omitempty"`
	ScheduledMessage   *domain.ProviderScheduledMessage `json:"scheduled_message,omitempty"`
	Concurrency        *domain.ConcurrencyPolicy        `json:"concurrency,omitempty"`
	Connections        []domain.ConnectionRef           `json:"connections,omitempty"`
}

type configurationHashConnectionModel struct {
	UpstreamModel     string                  `json:"upstream_model"`
	Protocols         []domain.Protocol       `json:"protocols"`
	Capabilities      domain.Capabilities     `json:"capabilities"`
	PlanID            string                  `json:"plan_id,omitempty"`
	ExecutionTargetID domain.ResourceTargetID `json:"execution_target_id,omitempty"`
}

type configurationHashConnection struct {
	ID          string                             `json:"id"`
	ProviderID  string                             `json:"provider_id"`
	BaseURL     string                             `json:"base_url,omitempty"`
	FullURL     string                             `json:"full_url,omitempty"`
	Credential  configurationHashCredential        `json:"credential"`
	Enabled     bool                               `json:"enabled"`
	Concurrency *domain.ConcurrencyPolicy          `json:"concurrency,omitempty"`
	Models      []configurationHashConnectionModel `json:"models"`
}

type configurationHashRoutingPolicy struct {
	ID          string `json:"id"`
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
}

type configurationHashPayload struct {
	Revision        int64                            `json:"revision"`
	RuntimeTimezone string                           `json:"runtime_timezone"`
	V2DomainSource  string                           `json:"v2_domain_source"`
	Endpoints       []domain.Endpoint                `json:"endpoints,omitempty"`
	Plans           []domain.Plan                    `json:"plans,omitempty"`
	QuotaBuckets    []domain.QuotaBucket             `json:"quota_buckets,omitempty"`
	RateCards       []config.RateCardEntry           `json:"rate_cards,omitempty"`
	Targets         []configurationHashTarget        `json:"targets,omitempty"`
	Channels        []configurationHashChannel       `json:"channels,omitempty"`
	Pools           []domain.Pool                    `json:"pools,omitempty"`
	Aliases         map[string]config.AliasSpec      `json:"aliases,omitempty"`
	Objectives      map[string]config.ObjectiveSpec  `json:"objectives,omitempty"`
	Policies        map[string]config.PolicyYAML     `json:"policies,omitempty"`
	DefaultPolicy   string                           `json:"default_policy,omitempty"`
	RoutingPolicies []configurationHashRoutingPolicy `json:"routing_policies,omitempty"`
	LogicalModels   []configurationHashLogicalModel  `json:"logical_models,omitempty"`
	Providers       []configurationHashProvider      `json:"providers,omitempty"`
	Connections     []configurationHashConnection    `json:"connections,omitempty"`
	MigrationIssues []domain.LegacyMappingIssue      `json:"v2_migration_issues,omitempty"`
}

func canonicalHashCredential(credential domain.CredentialRef) configurationHashCredential {
	secretRef := credential.SecretRef
	if strings.HasPrefix(secretRef, "plain:") {
		secretRef = "plain:<redacted>" // #nosec G101 -- fixed redaction marker, not a credential
	}
	return configurationHashCredential{
		SecretRef: secretRef, Header: credential.Header,
		QueryParam: credential.QueryParam, Prefix: credential.Prefix,
	}
}

func canonicalHashRoute(route domain.RouteSpec) domain.RouteSpec {
	out := domain.RouteSpec{Steps: append([]domain.RouteStep(nil), route.Steps...)}
	for i := range out.Steps {
		out.Steps[i].RecoverOn = append([]domain.FailureType(nil), out.Steps[i].RecoverOn...)
		slices.Sort(out.Steps[i].RecoverOn)
		out.Steps[i].RecoverOn = slices.Compact(out.Steps[i].RecoverOn)
	}
	return out
}

func canonicalHashRouteSet(steps []domain.RouteStep) []domain.RouteStep {
	out := canonicalHashRoute(domain.RouteSpec{Steps: steps}).Steps
	slices.SortFunc(out, func(left, right domain.RouteStep) int {
		if byProvider := strings.Compare(left.ProviderID, right.ProviderID); byProvider != 0 {
			return byProvider
		}
		return strings.Compare(left.UpstreamModel, right.UpstreamModel)
	})
	return out
}

func (c *Compiler) hashConfiguration(f *config.File, revision int64, timezone *time.Location) (string, error) {
	payload := configurationHashPayload{
		Revision: revision, V2DomainSource: f.V2DomainSource,
		Endpoints:    append([]domain.Endpoint(nil), f.Endpoints...),
		Plans:        append([]domain.Plan(nil), f.Plans...),
		QuotaBuckets: append([]domain.QuotaBucket(nil), f.QuotaBuckets...),
		RateCards:    append([]config.RateCardEntry(nil), f.RateCards...),
		Aliases:      f.Aliases, Objectives: f.Objectives, Policies: f.Policies,
		DefaultPolicy:   f.DefaultPolicy,
		MigrationIssues: append([]domain.LegacyMappingIssue(nil), f.V2MigrationIssues...),
	}
	if timezone != nil {
		payload.RuntimeTimezone = timezone.String()
	}
	slices.SortFunc(payload.Endpoints, func(a, b domain.Endpoint) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(payload.Plans, func(a, b domain.Plan) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(payload.QuotaBuckets, func(a, b domain.QuotaBucket) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(payload.RateCards, func(a, b config.RateCardEntry) int {
		left := a.Provider + "\x00" + a.Model + "\x00" + string(a.Plan) + "\x00" + a.Version
		right := b.Provider + "\x00" + b.Model + "\x00" + string(b.Plan) + "\x00" + b.Version
		return strings.Compare(left, right)
	})
	slices.SortFunc(payload.MigrationIssues, func(a, b domain.LegacyMappingIssue) int {
		left := a.LogicalModelID + "\x00" + a.Kind + "\x00" + a.ObjectID + "\x00" + a.Message
		right := b.LogicalModelID + "\x00" + b.Kind + "\x00" + b.ObjectID + "\x00" + b.Message
		if compared := strings.Compare(left, right); compared != 0 {
			return compared
		}
		if a.Blocking == b.Blocking {
			return 0
		}
		if !a.Blocking {
			return -1
		}
		return 1
	})

	for i := range f.Targets {
		target := &f.Targets[i]
		payload.Targets = append(payload.Targets, configurationHashTarget{
			ID: target.ID, EndpointID: target.EndpointID,
			Credential: canonicalHashCredential(target.Credential),
			Model:      target.Model, ModelAlias: target.ModelAlias, PlanID: target.PlanID,
			Capabilities: target.Capabilities, Weight: target.Weight, Priority: target.Priority,
		})
	}
	slices.SortFunc(payload.Targets, func(a, b configurationHashTarget) int { return strings.Compare(string(a.ID), string(b.ID)) })

	for i := range f.Channels {
		channel := &f.Channels[i]
		item := configurationHashChannel{
			ID: channel.ID, Type: channel.Type, BaseURL: channel.BaseURL,
			Credential: canonicalHashCredential(channel.Credential), Enabled: channel.EnabledOrDefault(),
			Pools: append([]string(nil), channel.Pools...), PlanID: channel.PlanID,
		}
		slices.Sort(item.Pools)
		item.Pools = slices.Compact(item.Pools)
		for j := range channel.Models {
			model := &channel.Models[j]
			protocols := append([]domain.Protocol(nil), model.Protocols...)
			slices.Sort(protocols)
			protocols = slices.Compact(protocols)
			pools := append([]string(nil), model.Pools...)
			slices.Sort(pools)
			pools = slices.Compact(pools)
			item.Models = append(item.Models, configurationHashChannelModel{
				Model: model.Model, Alias: model.Alias, Protocols: protocols, PlanID: model.PlanID,
				Capabilities: model.Capabilities, Weight: model.Weight, Priority: model.Priority, Pools: pools,
			})
		}
		slices.SortFunc(item.Models, func(a, b configurationHashChannelModel) int {
			return strings.Compare(a.Model+"\x00"+a.Alias+"\x00"+a.PlanID, b.Model+"\x00"+b.Alias+"\x00"+b.PlanID)
		})
		payload.Channels = append(payload.Channels, item)
	}
	slices.SortFunc(payload.Channels, func(a, b configurationHashChannel) int { return strings.Compare(a.ID, b.ID) })

	payload.Pools = append([]domain.Pool(nil), f.Pools...)
	for i := range payload.Pools {
		payload.Pools[i].Targets = append([]domain.ResourceTargetID(nil), payload.Pools[i].Targets...)
		slices.Sort(payload.Pools[i].Targets)
		payload.Pools[i].Targets = slices.Compact(payload.Pools[i].Targets)
	}
	slices.SortFunc(payload.Pools, func(a, b domain.Pool) int { return strings.Compare(a.ID, b.ID) })

	for _, spec := range f.RoutingPolicies {
		contentHash, err := domain.RoutingPolicyContentHash(spec)
		if err != nil {
			return "", fmt.Errorf("hash routing policy %s: %w", spec.ID, err)
		}
		payload.RoutingPolicies = append(payload.RoutingPolicies, configurationHashRoutingPolicy{
			ID: spec.ID, Priority: spec.Priority, Enabled: spec.Enabled,
			Revision: spec.Revision, ContentHash: contentHash,
		})
	}
	slices.SortFunc(payload.RoutingPolicies, func(a, b configurationHashRoutingPolicy) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		return strings.Compare(a.ID, b.ID)
	})

	for i := range f.LogicalModels {
		model := &f.LogicalModels[i]
		item := configurationHashLogicalModel{
			ID: model.ID, Enabled: model.Enabled, ExactOnly: model.ExactOnly,
			FirstOutputTimeoutSeconds:    model.FirstOutputTimeoutSeconds,
			ExistingConversationStrategy: model.ExistingConversationStrategy,
			DefaultRoute:                 canonicalHashRoute(model.DefaultRoute),
		}
		for j := range model.Overrides {
			override := &model.Overrides[j]
			canonical := configurationHashOverride{
				ID: override.ID, Enabled: override.Enabled,
				Callers:   append([]string(nil), override.Callers...),
				Protocols: append([]domain.Protocol(nil), override.Protocols...),
				Weekdays:  append([]string(nil), override.Weekdays...), Timezone: override.Timezone, Window: override.Window,
				Required: override.Required, Mode: override.Mode,
				Prefer: canonicalHashRouteSet(override.Prefer), Avoid: canonicalHashRouteSet(override.Avoid),
			}
			slices.Sort(canonical.Callers)
			canonical.Callers = slices.Compact(canonical.Callers)
			slices.Sort(canonical.Protocols)
			canonical.Protocols = slices.Compact(canonical.Protocols)
			for index := range canonical.Weekdays {
				canonical.Weekdays[index] = strings.ToLower(strings.TrimSpace(canonical.Weekdays[index]))
			}
			slices.Sort(canonical.Weekdays)
			canonical.Weekdays = slices.Compact(canonical.Weekdays)
			if override.Route != nil {
				route := canonicalHashRoute(*override.Route)
				canonical.Route = &route
			}
			item.Overrides = append(item.Overrides, canonical)
		}
		payload.LogicalModels = append(payload.LogicalModels, item)
	}
	slices.SortFunc(payload.LogicalModels, func(a, b configurationHashLogicalModel) int { return strings.Compare(a.ID, b.ID) })

	for i := range f.Providers {
		provider := &f.Providers[i]
		connections := append([]domain.ConnectionRef(nil), provider.Connections...)
		slices.SortFunc(connections, func(a, b domain.ConnectionRef) int { return strings.Compare(a.ID, b.ID) })
		connections = slices.CompactFunc(connections, func(a, b domain.ConnectionRef) bool { return a.ID == b.ID })
		payload.Providers = append(payload.Providers, configurationHashProvider{
			ID: provider.ID, Type: provider.Type, BaseURL: provider.BaseURL,
			Enabled: provider.Enabled, ProtocolConversion: provider.ProtocolConversion,
			ScheduledMessage: provider.ScheduledMessage,
			Concurrency:      provider.Concurrency, Connections: connections,
		})
	}
	slices.SortFunc(payload.Providers, func(a, b configurationHashProvider) int { return strings.Compare(a.ID, b.ID) })

	for i := range f.Connections {
		connection := &f.Connections[i]
		item := configurationHashConnection{
			ID: connection.ID, ProviderID: connection.ProviderID, BaseURL: connection.BaseURL, FullURL: connection.FullURL,
			Credential: canonicalHashCredential(connection.Credential), Enabled: connection.Enabled,
			Concurrency: connection.Concurrency,
		}
		for j := range connection.Models {
			model := &connection.Models[j]
			protocols := append([]domain.Protocol(nil), model.Protocols...)
			slices.Sort(protocols)
			protocols = slices.Compact(protocols)
			item.Models = append(item.Models, configurationHashConnectionModel{
				UpstreamModel: model.UpstreamModel, Protocols: protocols,
				Capabilities: model.Capabilities, PlanID: model.PlanID,
				ExecutionTargetID: model.ExecutionTargetID,
			})
		}
		slices.SortFunc(item.Models, func(a, b configurationHashConnectionModel) int {
			left := a.UpstreamModel + "\x00" + a.PlanID + "\x00" + string(a.ExecutionTargetID)
			right := b.UpstreamModel + "\x00" + b.PlanID + "\x00" + string(b.ExecutionTargetID)
			return strings.Compare(left, right)
		})
		payload.Connections = append(payload.Connections, item)
	}
	slices.SortFunc(payload.Connections, func(a, b configurationHashConnection) int { return strings.Compare(a.ID, b.ID) })

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode runtime configuration hash: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}
