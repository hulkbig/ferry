// Package scheduler orchestrates one logical request end to end: resolve
// consumer/alias/objective/policy, assess protocol compatibility, plan
// deterministically, execute attempts with typed recovery and the stream
// commit boundary, feed runtime health, and ledger everything
// (PRD §6, §12, §14, §15, F-P0-09..21).
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ferry/internal/config"
	"ferry/internal/conversation"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/ledger"
	"ferry/internal/policy"
	"ferry/internal/runtime"
)

// world is one immutable generation of {snapshot, callers, rate cards}.
// Hot publish swaps the pointer atomically; in-flight requests keep the
// world they started with (F-P0-24: hot update never disturbs in-flight).
type world struct {
	snap                    *domain.CompiledSnapshot
	callers                 map[domain.ResourceTargetID]egress.Caller
	cards                   map[string]*domain.RateCard // "model|plankind" -> card
	keys                    []domain.APIKeyAuthenticator
	routing                 *policy.RoutingIndex
	routingErr              error
	routingCacheHit         bool
	routeTargets            map[routeTargetKey][]domain.ResourceTargetID
	scenes                  sync.Map
	capabilityVerifications map[domain.ResourceTargetID]domain.CapabilityVerification
}

// Scheduler is the data-plane core.
type Scheduler struct {
	world              atomic.Pointer[world]
	manager            *runtime.Manager
	led                *ledger.Ledger
	routingCache       *policy.RoutingIndexCache
	affinity           *affinityCache
	conversations      *conversation.Store
	keyUsage           func(string, time.Time) error
	activeMu           sync.Mutex
	active             map[string]ActiveTrafficRecord
	openIngress        atomic.Int64
	keyRateMu          sync.Mutex
	keyRates           map[string]apiKeyRateWindow
	capabilityVerifier CapabilityVerifier
}

type apiKeyRateWindow struct {
	Minute int64
	Count  int
}

// New creates a scheduler around a runtime manager and ledger.
func New(mgr *runtime.Manager, led *ledger.Ledger) *Scheduler {
	return &Scheduler{manager: mgr, led: led, routingCache: policy.NewRoutingIndexCache(), affinity: newAffinityCache(), keyRates: map[string]apiKeyRateWindow{}}
}

func (s *Scheduler) SetConversationStore(store *conversation.Store) { s.conversations = store }

func (s *Scheduler) SetAPIKeyUsageRecorder(recorder func(string, time.Time) error) {
	s.keyUsage = recorder
}

// ReplaceWorld atomically publishes a new compiled snapshot + callers.
// New targets are registered with the runtime manager on the way in.
func (s *Scheduler) ReplaceWorld(snap *domain.CompiledSnapshot,
	callers map[domain.ResourceTargetID]egress.Caller, cards []*domain.RateCard,
	keys []config.ConsumerKey) {
	authenticators := make([]domain.APIKeyAuthenticator, 0, len(keys))
	for _, key := range keys {
		enabled := key.Enabled == nil || *key.Enabled
		identity := domain.APIKeyIdentity{
			ID: key.Name, Name: key.Name, Enabled: enabled,
		}
		authenticators = append(authenticators, domain.APIKeyAuthenticator{
			Identity:               identity,
			SecretHashes:           [][32]byte{domain.HashAPIKeySecret(key.Key)},
			LegacyDefaultModel:     key.DefaultAlias,
			LegacyDefaultObjective: key.DefaultObjective,
		})
	}
	s.ReplaceWorldWithAPIKeys(snap, callers, cards, authenticators)
}

// ReplaceWorldWithAPIKeys publishes a world whose caller credentials are
// hash-only API key authenticators loaded from the control store.
func (s *Scheduler) ReplaceWorldWithAPIKeys(snap *domain.CompiledSnapshot,
	callers map[domain.ResourceTargetID]egress.Caller, cards []*domain.RateCard,
	keys []domain.APIKeyAuthenticator) {

	idx := make(map[string]*domain.RateCard, len(cards))
	for _, c := range cards {
		idx[cardKey(c.Model, c.Plan)] = c
	}
	routing, routingCacheHit, routingErr := s.routingCache.CompileWithStatus(snap.RoutingPolicies)
	w := &world{
		snap: snap, callers: callers, cards: idx, keys: keys,
		routing: routing, routingErr: routingErr, routingCacheHit: routingCacheHit,
		routeTargets: indexRouteTargets(snap),
	}
	if routing != nil {
		snap.PolicyIndexHash = routing.Hash
	}
	s.manager.ReconcileTargets(snap)
	for id, target := range snap.Targets {
		var buckets []*domain.QuotaBucket
		var plan *domain.Plan
		if target != nil {
			buckets, plan = target.QuotaBuckets(), target.Plan
		}
		connectionID := ""
		enabled := true
		var concurrency *domain.ConcurrencyPolicy
		if target != nil {
			connectionID = target.ConnectionID
			enabled = !target.Disabled
			if connection := snap.Connections[connectionID]; connection != nil {
				enabled = enabled && connection.Enabled
				concurrency = connection.Concurrency
				if concurrency == nil {
					if provider := snap.Providers[target.ProviderID]; provider != nil {
						concurrency = provider.Concurrency
					}
				}
			}
		}
		s.manager.RegisterExecutionTargetBuckets(id, connectionID, enabled, callers[id] != nil, buckets, plan)
		if concurrency == nil {
			concurrency = snap.Concurrency
		}
		s.manager.SetConnectionConcurrency(connectionID, concurrency)
	}
	s.world.Store(w)
}

// Snapshot returns the currently published compiled snapshot (for console),
// or nil before the first publish.
// ApplyProviderQuota feeds provider-reported quota windows (e.g. the Codex
// account usage snapshot) into runtime state for every target of a
// connection. It only refreshes the scheduling surface (quota remaining,
// cooldown); the attempt ledger and local bucket accounting are untouched.
func (s *Scheduler) ApplyProviderQuota(connectionID string, windows []domain.QuotaWindowState, now time.Time) {
	s.manager.ApplyProviderQuota(connectionID, windows, now)
}

func (s *Scheduler) ApplyProviderQuotaForIdentity(connectionID, identity string, windows []domain.QuotaWindowState, now time.Time) {
	s.manager.ApplyProviderQuotaForIdentity(connectionID, identity, windows, now)
}

// RateCards returns the active rate cards of the published world (pricing-sync
// diffing and console display).
func (s *Scheduler) RateCards() []*domain.RateCard {
	w := s.world.Load()
	if w == nil {
		return nil
	}
	out := make([]*domain.RateCard, 0, len(w.cards))
	for _, c := range w.cards {
		out = append(out, c)
	}
	return out
}

func (s *Scheduler) Snapshot() *domain.CompiledSnapshot {
	if w := s.world.Load(); w != nil {
		return w.snap
	}
	return nil
}

// Authenticate applies the same immutable-world API key verification used by
// request execution. It is used by authenticated data-plane discovery routes.
func (s *Scheduler) Authenticate(meta RequestMeta) bool {
	now := time.Now()
	authenticator, secretID := s.resolveKeyInWorld(s.world.Load(), meta.credential(), "")
	if authenticator == nil {
		return false
	}
	if s.keyUsage != nil {
		_ = s.keyUsage(secretID, now)
	}
	return true
}

// ReplaceAPIKeys atomically refreshes only client authenticators while
// retaining the exact published runtime snapshot, callers and rate cards.
func (s *Scheduler) ReplaceAPIKeys(keys []domain.APIKeyAuthenticator) error {
	current := s.world.Load()
	if current == nil {
		return fmt.Errorf("scheduler world is not initialized")
	}
	next := &world{
		snap: current.snap, callers: current.callers, cards: current.cards,
		keys:    append([]domain.APIKeyAuthenticator(nil), keys...),
		routing: current.routing, routingErr: current.routingErr,
		routingCacheHit: current.routingCacheHit,
		routeTargets:    current.routeTargets,
	}
	s.world.Store(next)
	return nil
}

// HasCaller reports whether the live world holds an egress caller (i.e. a
// resolved credential) for the target — the console's 凭证管理 uses this to
// show whether a target is actually live after a key is provisioned.
func (s *Scheduler) HasCaller(id domain.ResourceTargetID) bool {
	if w := s.world.Load(); w != nil {
		return w.callers[id] != nil
	}
	return false
}

func cardKey(model string, plan domain.PlanKind) string {
	return model + "|" + string(plan)
}

// routingSceneKey is safe to cache for one UTC minute because low-code time
// windows have minute precision. It includes every request field used by the
// matcher and is scoped by the immutable active-index hash.
func routingSceneKey(indexHash string, ctx policy.RoutingContext) string {
	minute := ctx.Now.UTC().Format("200601021504")
	bits := make([]byte, 0, len(domain.BooleanCapabilityRegistry()))
	for _, capability := range domain.BooleanCapabilityRegistry() {
		required, _ := domain.RequirementCapability(ctx.Capabilities, capability)
		if required {
			bits = append(bits, '1')
		} else {
			bits = append(bits, '0')
		}
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s|%d|%d|%s",
		indexHash, ctx.Consumer, ctx.Model, ctx.Protocol, string(bits),
		ctx.Capabilities.MinContext, ctx.Capabilities.MinOutput, minute)
}

// RequestMeta carries per-HTTP-request identity into the pipeline.
type RequestMeta struct {
	requestID           string
	startedAt           time.Time
	PresentedCredential string // raw credential presented by the client
	// BearerToken remains an in-process legacy alias while management/tests
	// migrate; ingress populates PresentedCredential only.
	BearerToken      string
	ClientIP         string
	AffinityEvidence []domain.AffinityEvidence
	// CallerID is a trusted in-process identity override used by management
	// simulation. Ingress adapters never populate it from client input.
	CallerID string
	// RequiredCapabilities is another trusted simulation-only override. It
	// lets management exercise capability gates that cannot be inferred from a
	// synthetic wire request (for example parallel-tools support).
	RequiredCapabilities *domain.Requirements
	// SimulatedAt is honored only by dry-run planning. It lets management
	// verify weekday and time-window boundaries without changing the process
	// clock and can never influence live client traffic.
	SimulatedAt *time.Time
}

func (m RequestMeta) credential() string {
	if m.PresentedCredential != "" {
		return m.PresentedCredential
	}
	return m.BearerToken
}

// Result is a successful plan + live execution handed back to ingress.
type Result struct {
	RequestID       string
	LedgerRequestID string

	// Exactly one of Response / Events is populated.
	Response *domain.Response          // non-streaming
	Events   <-chan domain.StreamEvent // streaming (terminated by EvEnd/EvError)
	Cancel   func()                    // aborts the upstream stream

	Target                domain.ResourceTargetID
	Model                 string // client-facing model name (alias override honored)
	PolicyName            string
	PolicyVer             int64
	PolicyHash            string
	PolicyIndexHash       string
	PolicyCompileCacheHit bool
	PolicySceneCacheHit   bool
	CompatLevel           domain.CompatLevel
	CompatNotes           []string
	Outcome               domain.RequestOutcome
	ProviderID            string
	ConnectionID          string
	ConversationID        string
	ContinuityMode        domain.ContinuityMode
	ServiceTierEvidence   *domain.ServiceTierEvidence
}

// ---------------------------------------------------------------------------
// compatibility assessment (PRD §6.10, zero silent degradation)

type compatAssessment struct {
	level   domain.CompatLevel
	notes   []string
	blocked bool
	reason  string
}

// worstNoteLevel fails closed: Preparer notes are the adapter's declaration
// that some request semantics were defaulted, approximated, or discarded.
// Lossless field-name remappings must not emit a note. This avoids a new note
// being accidentally treated as safe merely because its wording omitted one
// of a few magic keywords.
func worstNoteLevel(notes []string) domain.CompatLevel {
	for _, note := range notes {
		if note != parallelToolsSerialMappingNote {
			return domain.CompatDegraded
		}
	}
	return domain.CompatMapped
}

// ---------------------------------------------------------------------------
// resolution + planning

type decision struct {
	debugCapture          bool // captured at request start; complete existing capture even if setting changes
	w                     *world
	plan                  *policy.PlanResult
	pol                   *domain.CompiledPolicy
	polName               string
	polVersion            int64
	polHash               string
	policyCompileCacheHit bool
	policySceneCacheHit   bool
	alias                 *domain.CompiledAlias
	aliasName             string
	objective             string
	required              domain.Requirements
	compat                map[domain.ResourceTargetID]compatAssessment
	state                 *domain.Snapshot
	now                   time.Time
	rec                   domain.RequestRecord
	recoveryEvents        []domain.RecoveryEvent
	publishTrace          func()
	v2                    bool
	logical               *domain.LogicalModel
	route                 domain.RouteSpec
	overrideID            string
	budget                domain.RecoveryBudget
	evidence              []domain.AffinityEvidence
	affinityBinding       *domain.AffinityBinding
	affinityEvidence      *domain.AffinityEvidence
	affinityReused        bool
	conversation          *domain.Conversation
	parentContinuation    *domain.NativeContinuation
	conversationStore     *conversation.Store
	responseID            string
	traceEvents           []domain.ExecutionTraceEvent
}

func failf(t domain.FailureType, format string, args ...any) *domain.Failure {
	return &domain.Failure{Type: t, Message: fmt.Sprintf(format, args...)}
}

// resolveKey maps a bearer token to its configured identity. An empty key set
// is deliberately locked down: deployment or configuration mistakes must
// never turn the data plane into an anonymous proxy.
func (s *Scheduler) resolveKeyInWorld(w *world, token, callerID string) (*domain.APIKeyAuthenticator, string) {
	if w == nil {
		return nil, ""
	}
	for i := range w.keys {
		key := &w.keys[i]
		if callerID != "" && key.Identity.Enabled &&
			(key.Identity.ID == callerID || key.Identity.Name == callerID) {
			return key, ""
		}
		if key.Identity.Enabled {
			if secretID, matched := key.MatchSecret(token); matched {
				return key, secretID
			}
		}
	}
	return nil, ""
}

func (s *Scheduler) allowAPIKeyRate(identity domain.APIKeyIdentity, now time.Time) bool {
	if identity.RateLimitRPM <= 0 {
		return true
	}
	minute := now.Unix() / 60
	s.keyRateMu.Lock()
	defer s.keyRateMu.Unlock()
	window := s.keyRates[identity.ID]
	if window.Minute != minute {
		window = apiKeyRateWindow{Minute: minute}
	}
	if window.Count >= identity.RateLimitRPM {
		return false
	}
	window.Count++
	s.keyRates[identity.ID] = window
	return true
}

func (s *Scheduler) apiKeyBudgetAvailable(identity domain.APIKeyIdentity, now time.Time) bool {
	if identity.MonthlyBudgetUSD <= 0 || s.led == nil {
		return true
	}
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	usage, err := s.led.ByConsumer(from, now.Add(time.Millisecond))
	if err != nil {
		return false
	}
	item := usage[identity.Name]
	return item == nil || item.TotalCost < identity.MonthlyBudgetUSD
}

// decide runs everything before the first attempt. It returns either a
// decision ready for execution or a terminal failure (already ledgered).
func (s *Scheduler) decide(ctx context.Context, rc *domain.RequestContract, meta RequestMeta, dryRun bool) (*decision, *domain.Failure) {
	w := s.world.Load()
	return s.decideWithWorld(ctx, w, rc, meta, dryRun)
}

// decideWithWorld is the single planner entry for both live traffic and draft
// simulation. Supplying a candidate world changes data, never planner logic.
func (s *Scheduler) decideWithWorld(ctx context.Context, w *world, rc *domain.RequestContract, meta RequestMeta, dryRun bool) (_ *decision, failure *domain.Failure) {
	now := time.Now()
	if !meta.startedAt.IsZero() {
		now = meta.startedAt
	}
	reqID := meta.requestID
	if reqID == "" {
		reqID = newRequestID()
	}
	phases := preflightTraceEvents(ctx)
	recordPhase := func(event domain.ExecutionTraceEvent) {
		if len(phases) >= maxPreflightTraceEvents {
			return
		}
		if len(event.Message) > 256 {
			event.Message = strings.Clone(event.Message[:256])
		}
		phases = append(phases, event)
		if !dryRun {
			s.led.SetTrace(reqID, &domain.TraceRecord{Events: append([]domain.ExecutionTraceEvent(nil), phases...)})
		}
	}
	planningCompleted := false
	if !dryRun && rc != nil {
		initial := domain.RequestRecord{ID: reqID, Ts: now, Protocol: rc.Protocol, ModelRequested: rc.Model, LogicalModelID: rc.Model, Stream: rc.Stream, ClientIP: meta.ClientIP, Status: "received"}
		if meta.requestID == "" {
			s.led.BeginRequest(initial)
		} else {
			s.led.UpdateRequest(reqID, func(r *domain.RequestRecord) { *r = initial })
		}
		defer func() {
			if failure != nil {
				if !planningCompleted {
					phase := "PLANNING_FAILED"
					if failure.Type == domain.FailClientCancel {
						phase = "PLANNING_CANCELED"
					}
					recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: phase,
						FailureType: failure.Type, FailureScope: domain.FailureScopeRequest})
				}
				s.led.FinishRequest(reqID, func(r *domain.RequestRecord) {
					r.Status, r.Outcome = domain.ReqFailed, domain.OutcomeFailed
					r.FailureType, r.FailureMsg = failure.Type, failure.Message
					if failure.Type == domain.FailClientCancel {
						r.Status = domain.ReqCanceled
					}
					r.DurationMS = time.Since(now).Milliseconds()
				})
			}
		}()
	}
	if len(phases) == 0 {
		recordPhase(domain.ExecutionTraceEvent{At: now, Category: domain.TraceRoute, Type: "REQUEST_RECEIVED"})
	}
	recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "PLANNING_STARTED"})
	if ctx.Err() != nil {
		return nil, planningCancellation(ctx)
	}
	if w == nil || w.snap == nil {
		return nil, failf(domain.FailBadResponse, "scheduler world is not initialized")
	}
	if rc == nil {
		return nil, failf(domain.FailUnsupportedParam, "nil request contract")
	}
	if _, err := domain.NormalizeServiceTier(rc.ServiceTier); err != nil {
		return nil, failf(domain.FailUnsupportedParam, "%s", err)
	}
	if dryRun && meta.SimulatedAt != nil {
		now = *meta.SimulatedAt
	}
	if rc != nil && rc.Lineage != nil && rc.Lineage.ConversationID != "" && rc.Lineage.PreviousResponseID != "" {
		failure := failf(domain.FailUnsupportedParam, "conversation and previous_response_id are mutually exclusive")
		failure.Scope = domain.FailureScopeRequest
		return nil, failure
	}

	ck, matchedSecretID := s.resolveKeyInWorld(w, meta.credential(), meta.CallerID)
	if ck == nil {
		failure := failf(domain.FailAuth, "invalid or disabled consumer key")
		failure.Scope = domain.FailureScopeRequest
		return nil, failure
	}
	if !dryRun {
		s.led.UpdateRequest(reqID, func(r *domain.RequestRecord) { r.ConsumerKey = ck.Identity.Name; r.APIKeySecretID = matchedSecretID })
	}
	if !ck.Identity.Allows(rc.Model, rc.Protocol, now) {
		failure := failf(domain.FailAuth, "API key is expired, revoked, or outside its model/protocol scope")
		failure.Scope = domain.FailureScopeRequest
		return nil, failure
	}
	if !dryRun && !s.allowAPIKeyRate(ck.Identity, now) {
		failure := failf(domain.FailRateLimit, "API key request limit exceeded")
		failure.Scope = domain.FailureScopeRequest
		return nil, failure
	}
	if !dryRun && !s.apiKeyBudgetAvailable(ck.Identity, now) {
		failure := failf(domain.FailQuotaExhausted, "API key monthly budget exhausted")
		failure.Scope = domain.FailureScopeRequest
		return nil, failure
	}
	if matchedSecretID != "" && s.keyUsage != nil && !dryRun {
		_ = s.keyUsage(matchedSecretID, now)
	}

	logical := w.snap.LogicalModels[rc.Model]
	blockedLegacyProjection := v2ProjectionBlockedForModel(w.snap, rc.Model)
	useV2 := logical != nil && logical.Enabled && !blockedLegacyProjection
	if !useV2 && w.snap.V2DomainSource != config.V2DomainSourceLegacy {
		return nil, failf(domain.FailUnsupportedParam, "unknown or disabled logical model %q", rc.Model)
	}

	// Native V2 resolves the public model directly. The legacy default fields
	// are consulted only by the compatibility path and are not part of the V2
	// API-key contract.
	aliasName := rc.Model
	var alias *domain.CompiledAlias
	if useV2 {
		alias = &domain.CompiledAlias{Name: logical.ID, Synthesized: true}
	} else {
		// Alias/default-model fallback is an explicit legacy_projection
		// compatibility surface. Native V2 exposes LogicalModels only.
		alias = w.snap.Aliases[aliasName]
		if alias == nil && ck.LegacyDefaultModel != "" {
			aliasName = ck.LegacyDefaultModel
			alias = w.snap.Aliases[aliasName]
		}
		if alias == nil {
			return nil, failf(domain.FailUnsupportedParam, "unknown model/alias %q", rc.Model)
		}
	}
	recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "MODEL_RESOLVED", Message: aliasName})

	objName := "connection-ranking"
	if !useV2 {
		// objective: consumer → alias → built-in balanced (legacy only)
		objName = ck.LegacyDefaultObjective
		if objName == "" {
			objName = alias.DefaultObjective
		}
		if objName == "" {
			objName = "balanced"
		}
		if _, ok := w.snap.Objectives[objName]; !ok {
			objName = "balanced"
		}
	}

	// Route matching and target eligibility use the same client-visible
	// capability contract. Target-specific native-vs-mapped handling happens
	// only when preparing the upstream attempt.
	requestCapabilities := rc.ExtractRequirements()
	routingRequirements := requestCapabilities
	if simulated := meta.RequiredCapabilities; simulated != nil {
		routingRequirements = domain.MergeRequirements(routingRequirements, *simulated)
	}
	ac := alias.RequiredCaps
	routingRequirements = domain.MergeRequirements(routingRequirements, ac)
	targetRequirements := routingRequirements

	polName := ""
	polVersion := w.snap.Version
	polHash := ""
	compileCacheHit := false
	sceneCacheHit := false
	var pol *domain.CompiledPolicy
	var selectedV2 v2RouteSelection
	evidence := append([]domain.AffinityEvidence(nil), meta.AffinityEvidence...)
	var currentConversation *domain.Conversation
	var parentContinuation *domain.NativeContinuation
	var affinityBinding *domain.AffinityBinding
	var affinityEvidence *domain.AffinityEvidence
	if useV2 {
		evidence = append(evidence, domain.NewAffinityEvidence(
			"api_key_identity", domain.AffinityCaller, ck.Identity.ID, domain.ConfHigh))
		var conversationFailure *domain.Failure
		currentConversation, evidence, parentContinuation, conversationFailure = s.resolveConversation(rc, logical.ID, evidence)
		if conversationFailure != nil {
			return nil, conversationFailure
		}
		if currentConversation == nil && shouldPersistConversation(evidence, rc.Protocol) {
			currentConversation = &domain.Conversation{
				ID: conversation.NewID("conv_smart_"), LogicalModelID: logical.ID, CreatedAt: now,
			}
		}
		selectedV2 = selectV2Route(logical, ck.Identity.ID, ck.Identity.Name, rc.Protocol, routingRequirements, now, w.snap.RuntimeTimezone)
		if selectedV2.OverrideID != "" {
			recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "OVERRIDE_MATCHED", Message: selectedV2.OverrideID})
		}
		recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "ROUTE_RESOLVED", Message: routeFingerprint(selectedV2.Route)})
		affinityBinding, affinityEvidence = s.affinity.resolve(evidence, logical.ID, now)
		if affinityBinding == nil && currentConversation != nil && currentConversation.CurrentConnectionID != "" {
			for i := range evidence {
				if evidence[i].Scope == domain.AffinityConversation {
					affinityEvidence = &evidence[i]
					break
				}
			}
			affinityBinding = &domain.AffinityBinding{
				Scope: domain.AffinityConversation, LogicalModelID: logical.ID,
				ProviderID:    currentConversation.CurrentProviderID,
				UpstreamModel: currentConversation.CurrentUpstreamModel,
				ConnectionID:  currentConversation.CurrentConnectionID,
			}
			if affinityEvidence != nil {
				affinityBinding.KeyHash = affinityEvidence.KeyHash
			}
		}
		polName = "logical:" + logical.ID
		polHash = w.snap.Hash
		policyTimezone := w.snap.RuntimeTimezone
		if policyTimezone == nil {
			policyTimezone = time.UTC
		}
		pol = &domain.CompiledPolicy{Name: polName, Timezone: policyTimezone,
			Recovery: map[domain.FailureType]domain.RecoveryAction{}}
		for _, step := range selectedV2.Route.Steps {
			allowed := map[domain.FailureType]bool{}
			for _, failure := range step.RecoverOn {
				allowed[failure] = true
			}
			pol.StepRecovery = append(pol.StepRecovery, allowed)
		}
	} else if len(w.snap.RoutingPolicies) > 0 {
		if w.routingErr != nil {
			return nil, failf(domain.FailBadResponse, "routing policy index unavailable: %v", w.routingErr)
		}
		routingContext := policy.RoutingContext{
			Consumer: ck.Identity.Name, Model: aliasName, Protocol: rc.Protocol,
			Capabilities: routingRequirements, Now: now,
		}
		sceneKey := routingSceneKey(w.routing.Hash, routingContext)
		var selected *policy.CompiledRoutingPolicy
		if cached, ok := w.scenes.Load(sceneKey); ok {
			selected, _ = cached.(*policy.CompiledRoutingPolicy)
			sceneCacheHit = selected != nil
		}
		if selected == nil {
			selected = w.routing.Select(routingContext)
			if selected != nil {
				w.scenes.Store(sceneKey, selected)
			}
		}
		if selected == nil {
			return nil, failf(domain.FailBadResponse, "no enabled routing policy matched request")
		}
		polName = selected.ID
		if selected.Revision > 0 {
			polVersion = selected.Revision
		}
		polHash = selected.ContentHash
		compileCacheHit = w.routingCacheHit
	} else {
		// Legacy compatibility exists only until SQLite migration materializes
		// independent ordered rules.
		polName = w.snap.DefaultPolicy
		if polName == "" && len(w.snap.Policies) == 1 {
			for n := range w.snap.Policies {
				polName = n
			}
		}
	}
	if !useV2 {
		pol = w.snap.Policies[polName]
		if pol == nil {
			return nil, failf(domain.FailBadResponse, "no policy configured: cannot schedule")
		}
		recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "ROUTE_RESOLVED", Message: polName})
	}

	compatContract := *rc
	compatContract.ProviderSessionKey = providerSessionKeyOf(evidence, currentConversation)
	exactOnly := ck.Identity.ExactOnly
	if useV2 {
		exactOnly = exactOnly || logical.ExactOnly
	}
	if useV2 && !dryRun && s.capabilityVerifier != nil {
		w = s.verifyRouteCapabilities(ctx, w, selectedV2.Route, &compatContract, targetRequirements, alias.AllowDegradation, exactOnly)
	}
	compat := newCompatibilityAssessor(ctx, w, &compatContract, alias.AllowDegradation, exactOnly)

	st := s.manager.Snapshot(now)
	connectionStates := s.manager.ConnectionStates(now)
	recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "COMPATIBILITY_STARTED"})
	var plan *policy.PlanResult
	var err error
	affinityReused := false
	if useV2 {
		planningBinding := affinityBinding
		if logical.ExistingConversationStrategy == domain.ConversationMigrate &&
			planningBinding != nil && planningBinding.Scope == domain.AffinityConversation {
			previousFingerprint := planningBinding.RouteFingerprint
			if previousFingerprint == "" && currentConversation != nil {
				previousFingerprint = currentConversation.LastRouteFingerprint
			}
			if previousFingerprint != "" && previousFingerprint != routeFingerprint(selectedV2.Route) {
				// Keep the binding on the trace as migration evidence, but do not
				// let it override the newly applied route during planning.
				planningBinding = nil
			}
		}
		plan, affinityReused, err = planV2(w, logical, selectedV2.Route, rc.Protocol, targetRequirements, compat, st,
			connectionStates, planningBinding)
	} else {
		plan, err = policy.Plan(&policy.PlanInput{
			Snapshot:        w.snap,
			Policy:          pol,
			Alias:           alias,
			Objective:       w.snap.Objectives[objName],
			IngressProtocol: rc.Protocol,
			Required:        targetRequirements,
			ConsumerKey:     ck.Identity.Name,
			State:           st,
			ExtraCheck: func(id domain.ResourceTargetID) (string, bool) {
				// Synthesized per-model aliases ARE concrete model names: pools
				// mixing many models of one channel must not route an opus
				// request to a deepseek target that merely shares its pool.
				// Classic aliases keep contract semantics across models.
				if alias.Synthesized {
					if t := w.snap.Targets[id]; t != nil && t.ClientModel() != rc.Model &&
						t.ClientModel() != aliasName {
						return fmt.Sprintf("model mismatch: pool serves %q, request wants %q",
							t.ClientModel(), rc.Model), true
					}
				}
				// a target whose channel is disabled stays out of rotation with an
				// actionable reason; its shape is kept in pools/lints on purpose.
				if t := w.snap.Targets[id]; t != nil && t.Disabled {
					return "channel disabled: enable the channel via the console or config", true
				}
				// a target whose credential could not be resolved at publish time
				// (key not yet provisioned) is excluded with an actionable reason
				// instead of failing at call time (zero silent degradation).
				if w.callers[id] == nil {
					return "credential not configured: set its API key via the console (凭证管理)", true
				}
				a := compat.assess(id)
				return a.reason, a.blocked
			},
		})
	}

	recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "COMPATIBILITY_COMPLETED",
		Message: fmt.Sprintf("prepared=%d cache_hits=%d prepare_duration_ms=%d", compat.prepared, compat.cacheHits, compat.duration.Milliseconds())})
	if ctx.Err() != nil {
		return nil, planningCancellation(ctx)
	}
	planningCompleted = true
	recordPhase(domain.ExecutionTraceEvent{At: time.Now(), Category: domain.TraceRoute, Type: "PLANNING_COMPLETED"})
	d := &decision{
		w: w, plan: plan, pol: pol, polName: polName, polVersion: polVersion,
		polHash: polHash, policyCompileCacheHit: compileCacheHit, policySceneCacheHit: sceneCacheHit,
		alias: alias, aliasName: aliasName, objective: objName,
		required: routingRequirements, compat: compat.results, state: st, now: now,
		v2: useV2, logical: logical, route: selectedV2.Route, overrideID: selectedV2.OverrideID,
		evidence: evidence, affinityBinding: affinityBinding, affinityEvidence: affinityEvidence,
		affinityReused: affinityReused, conversation: currentConversation, conversationStore: s.conversations,
		parentContinuation: parentContinuation,
		rec: domain.RequestRecord{
			ID: reqID, Ts: now,
			ConsumerKey:           ck.Identity.Name,
			APIKeySecretID:        matchedSecretID,
			Alias:                 aliasName,
			Objective:             objName,
			PolicyName:            polName,
			PolicyVersion:         polVersion,
			PolicyHash:            polHash,
			PolicyIndexHash:       w.snap.PolicyIndexHash,
			PolicyCompileCacheHit: compileCacheHit,
			PolicySceneCacheHit:   sceneCacheHit,
			ModelRequested:        rc.Model,
			ServiceTier:           rc.ServiceTier,
			Protocol:              rc.Protocol,
			Stream:                rc.Stream,
			ClientIP:              meta.ClientIP,
			RuntimeRevision:       w.snap.Version,
		},
	}
	if useV2 {
		d.rec.LogicalModelID = logical.ID
		d.rec.RouteFingerprint = routeFingerprint(selectedV2.Route)
		if plan != nil && len(plan.OrderTierPolicyIndexes) > 0 && plan.OrderTierPolicyIndexes[0] > 0 {
			d.rec.RouteAdvanceCount = plan.OrderTierPolicyIndexes[0]
		}
	}
	d.traceEvents = initialTraceEvents(d, phases)
	if useV2 && rc.Protocol == domain.ProtocolOpenAIResponses {
		d.responseID = conversation.NewID("resp_smart_")
	}
	if !dryRun {
		s.led.UpdateRequest(d.rec.ID, func(r *domain.RequestRecord) { *r = d.rec; r.Status = "received" })
		if s.led.DebugCaptureEnabled() {
			d.debugCapture = true
			payload, truncated := requestDebugPayload(rc)
			s.led.CaptureDebugRequest(d.rec.ID, d.rec.Ts, payload, truncated)
		}
	}

	if err != nil {
		exc := make([]domain.ExclusionRecord, 0)
		var nc *domain.ErrNoCandidate
		if errors.As(err, &nc) {
			exc = nc.Excluded
		}
		d.rec.Exclusions = exc
		// Classify the aggregate decision from the actual V2 exclusion set;
		// runtime unavailability must not masquerade as local quota exhaustion.
		ft := domain.FailBadResponse
		msg := err.Error()
		compatCount := 0
		var firstCompat string
		for _, e := range exc {
			if strings.Contains(e.Reason, "compatibility") {
				compatCount++
				if firstCompat == "" {
					firstCompat = fmt.Sprintf("%s: %s", e.Target, e.Reason)
				}
			}
		}
		if useV2 {
			ft = classifyV2NoCandidate(exc)
		} else {
			ft = domain.FailQuotaExhausted
		}
		if compatCount > 0 {
			ft = domain.FailUnsupportedParam
			msg = fmt.Sprintf("%s (%d target(s) rejected by compatibility policy, e.g. %s)",
				msg, compatCount, firstCompat)
		}
		f := failf(ft, "%s", msg)
		f.Scope = domain.FailureScopeRequest
		if retryAfter, retryAt := retryDelayForExclusions(exc, w.snap.Targets, connectionStates, now); retryAfter > 0 &&
			(ft == domain.FailConnect || ft == domain.FailRateLimit || ft == domain.FailQuotaExhausted) {
			f.RetryAfter = retryAfter
			f.HTTPStatus = 503
			if ft == domain.FailRateLimit || ft == domain.FailQuotaExhausted {
				f.HTTPStatus = 429
			}
			f.Message = fmt.Sprintf("%s; retry after %s", f.Message, retryAt.Format(time.RFC3339))
		}
		if !dryRun {
			s.finish(d, domain.ReqFailed, f, nil)
		}
		if dryRun {
			return d, f
		}
		return nil, f
	}

	trace := &domain.TraceRecord{
		PolicyName:            polName,
		PolicyVersion:         polVersion,
		PolicyHash:            polHash,
		PolicyIndexHash:       w.snap.PolicyIndexHash,
		PolicyCompileCacheHit: compileCacheHit,
		PolicySceneCacheHit:   sceneCacheHit,
		Timezone:              pol.Timezone.String(),
		RequiredCaps:          routingRequirements,
		Objective:             objName,
		Tiers:                 plan.Tiers,
		Candidates:            plan.Candidates,
		ExecutionOrder:        plan.Order,
		Primary:               plan.Primary,
		LogicalModelID:        aliasName,
		OverrideID:            selectedV2.OverrideID,
		AffinityEvidence:      append([]domain.AffinityEvidence(nil), evidence...),
		AffinityBinding:       affinityBinding,
		AffinityReused:        affinityReused,
		ConversationID:        conversationIDOf(currentConversation),
		Events:                append([]domain.ExecutionTraceEvent(nil), d.traceEvents...),
	}
	if !dryRun {
		s.led.SetTrace(reqID, trace)
		d.publishTrace = func() {
			snapshot := *trace
			snapshot.Events = append([]domain.ExecutionTraceEvent(nil), d.traceEvents...)
			s.led.SetTrace(reqID, &snapshot)
		}
	}
	return d, nil
}

func retryDelayForExclusions(exclusions []domain.ExclusionRecord, targets map[domain.ResourceTargetID]*domain.CompiledTarget,
	connections map[string]domain.ConnectionState, now time.Time) (time.Duration, time.Time) {
	var earliest time.Time
	for _, exclusion := range exclusions {
		target := targets[exclusion.Target]
		if target == nil {
			continue
		}
		connection, ok := connections[target.ConnectionID]
		if !ok {
			continue
		}
		retryAt := connection.RetryAt
		if retryAt.IsZero() && connection.CoolingUntil.After(now) {
			retryAt = connection.CoolingUntil
		}
		for _, window := range connection.QuotaWindows {
			if window.Availability == domain.QuotaExhausted && window.ResetAt.After(now) &&
				(retryAt.IsZero() || window.ResetAt.Before(retryAt)) {
				retryAt = window.ResetAt
			}
		}
		if retryAt.After(now) && (earliest.IsZero() || retryAt.Before(earliest)) {
			earliest = retryAt
		}
	}
	if earliest.IsZero() {
		return 0, time.Time{}
	}
	delay := earliest.Sub(now)
	if delay < time.Second {
		delay = time.Second
	}
	return delay, earliest
}

func v2ProjectionBlockedForModel(snapshot *domain.CompiledSnapshot, modelID string) bool {
	if snapshot == nil || snapshot.V2DomainSource != config.V2DomainSourceLegacy {
		return false
	}
	for _, issue := range snapshot.V2MigrationIssues {
		if issue.Blocking && (issue.LogicalModelID == "" || issue.LogicalModelID == modelID) {
			return true
		}
	}
	return false
}

func classifyV2NoCandidate(exclusions []domain.ExclusionRecord) domain.FailureType {
	if len(exclusions) == 0 {
		return domain.FailBadResponse
	}
	unsupported, quota, auth, availability := 0, 0, 0, 0
	for _, exclusion := range exclusions {
		reason := strings.ToLower(exclusion.Reason)
		switch {
		case strings.Contains(reason, "unverified:"):
			availability++
		case strings.Contains(reason, "capability"), strings.Contains(reason, "compatibility"),
			strings.Contains(reason, "unsupported"), strings.Contains(reason, "lossless"):
			unsupported++
		case strings.Contains(reason, "quota exhausted"):
			quota++
		case strings.Contains(reason, "credential unavailable"), strings.Contains(reason, "authentication"):
			auth++
		case strings.Contains(reason, "disabled"), strings.Contains(reason, "offline"),
			strings.Contains(reason, "cooling"), strings.Contains(reason, "circuit breaker"):
			availability++
		default:
			availability++
		}
	}
	total := len(exclusions)
	switch {
	case unsupported == total:
		return domain.FailUnsupportedParam
	case quota == total:
		return domain.FailQuotaExhausted
	case auth == total:
		return domain.FailAuth
	case availability > 0 || quota > 0 || auth > 0:
		return domain.FailConnect
	default:
		return domain.FailBadResponse
	}
}

// finish finalizes the logical request row and attaches recovery events to
// the trace (idempotent per request).
func (s *Scheduler) finish(d *decision, status domain.RequestStatus, f *domain.Failure, ex *execution) {
	defer s.clearActiveTraffic(d.rec.ID)
	trace := &domain.TraceRecord{
		PolicyName:            d.polName,
		PolicyVersion:         d.polVersion,
		PolicyHash:            d.polHash,
		PolicyIndexHash:       d.w.snap.PolicyIndexHash,
		PolicyCompileCacheHit: d.policyCompileCacheHit,
		PolicySceneCacheHit:   d.policySceneCacheHit,
		Timezone:              d.pol.Timezone.String(),
		RequiredCaps:          d.required,
		Objective:             d.objective,
		Tiers:                 nil,
		Candidates:            nil,
		ExecutionOrder:        nil,
		Primary:               "",
		LogicalModelID:        d.aliasName,
		OverrideID:            d.overrideID,
		AffinityEvidence:      append([]domain.AffinityEvidence(nil), d.evidence...),
		AffinityBinding:       d.affinityBinding,
		AffinityReused:        d.affinityReused,
		ConversationID:        conversationIDOf(d.conversation),
		Events:                append([]domain.ExecutionTraceEvent(nil), d.traceEvents...),
	}
	if d.plan != nil {
		trace.Tiers = d.plan.Tiers
		trace.Candidates = d.plan.Candidates
		trace.ExecutionOrder = d.plan.Order
		trace.Primary = d.plan.Primary
	}
	if len(d.recoveryEvents) > 0 {
		trace.RecoveryEvents = d.recoveryEvents
	}
	if ex != nil {
		trace.ContinuityMode = ex.continuity
	}
	d.addTrace(domain.TraceOutput, "COMPLETE", nil, ex, nav{}, "")
	trace.Events = append([]domain.ExecutionTraceEvent(nil), d.traceEvents...)
	s.led.SetTrace(d.rec.ID, trace)
	if d.debugCapture {
		payload, truncated := responseDebugPayload(status, f, ex)
		s.led.CaptureDebugResponse(d.rec.ID, payload, truncated)
	}
	s.led.FinishRequest(d.rec.ID, func(rec *domain.RequestRecord) {
		rec.Status = status
		for _, event := range d.recoveryEvents {
			if event.NextTarget != "" {
				rec.FallbackCount++
			}
			if strings.Contains(event.Action, "connection_rebind") {
				rec.ConnectionRebindCount++
			}
			if strings.Contains(event.Action, "route_fallback") || strings.Contains(event.Action, "next_route_step") ||
				strings.Contains(event.Action, "next_tier") {
				rec.RouteFallbackCount++
				rec.RecoveryFallbackCount++
			}
		}
		rec.Outcome = domain.OutcomeFailed
		if f != nil {
			rec.FailureType = f.Type
			rec.FailureMsg = f.Message
		}
		if ex != nil {
			rec.Attempts = ex.seq
			rec.ResponseModel = ex.responseModel
			rec.ResponseServiceTier = ex.responseServiceTier
			rec.ServiceTierEvidence = ex.serviceTierEvidence(status == domain.ReqSuccess)
			rec.Usage = ex.usage
			rec.UpstreamUsage = ex.upstreamUsage
			rec.EstCashCost = ex.estCash
			rec.QuotaUnits = ex.quotaUnits
			rec.AllocatedSubCost = ex.allocSub
			rec.ShadowCost = ex.shadow
			rec.ServingCost = ex.servingCost
			rec.RecoveryOverheadCost = ex.recoveryOverheadCost
			rec.RecoveryAddedLatencyMS = ex.recoveryAddedLatencyMS
			rec.SelectedTarget = ex.lastTarget
			rec.DurationMS = time.Since(ex.start).Milliseconds()
			rec.TTFTMS = ex.ttftMS
			rec.CompatLevel = ex.compatLevel
			rec.CompatNotes = ex.compatNotes
			rec.CommitAt = ex.commitAt
			rec.ContinuityMode = ex.continuity
			if ex.continuity == domain.ContinuityRehydrated {
				rec.ConversationMigrationCount = 1
			}
			if target := d.w.snap.Targets[ex.lastTarget]; target != nil {
				rec.ProviderID = target.ProviderID
				rec.ConnectionID = target.ConnectionID
				rec.UpstreamModel = target.Model
			}
			if rec.ModelSelected == "" {
				if d.v2 {
					rec.ModelSelected = d.aliasName
				} else if t := d.w.snap.Targets[ex.lastTarget]; t != nil {
					rec.ModelSelected = t.ClientModel()
				}
			}
			if status == domain.ReqSuccess {
				rec.Outcome = domain.OutcomeDirectSuccess
				if ex.seq > 1 || len(d.recoveryEvents) > 0 || ex.continuity == domain.ContinuityRehydrated {
					rec.Outcome = domain.OutcomeRecoveredSuccess
				}
			}
		}
		rec.Exclusions = mergeExclusions(rec.Exclusions, d.planExclusions())
	})
}

func (d *decision) planExclusions() []domain.ExclusionRecord {
	if d.plan == nil {
		return nil
	}
	out := make([]domain.ExclusionRecord, 0, len(d.plan.Candidates))
	for _, c := range d.plan.Candidates {
		if !c.Eligible {
			out = append(out, domain.ExclusionRecord{Target: c.Target, Tier: c.Tier, Reason: c.ExclusionReason})
		}
	}
	return out
}

func mergeExclusions(a, b []domain.ExclusionRecord) []domain.ExclusionRecord {
	if len(a) == 0 {
		return b
	}
	return append(a, b...)
}

func initialTraceEvents(d *decision, phases []domain.ExecutionTraceEvent) []domain.ExecutionTraceEvent {
	if d == nil {
		return nil
	}
	now := time.Now()
	events := append([]domain.ExecutionTraceEvent(nil), phases...)
	if d.affinityBinding != nil {
		affinityProtocol := domain.Protocol("")
		if d.w != nil && d.w.snap != nil {
			for _, targetID := range d.w.snap.TargetOrder {
				target := d.w.snap.Targets[targetID]
				if target != nil && target.ProviderID == d.affinityBinding.ProviderID &&
					target.ConnectionID == d.affinityBinding.ConnectionID &&
					target.Model == d.affinityBinding.UpstreamModel {
					affinityProtocol = target.EgressProtocol(d.rec.Protocol)
					break
				}
			}
		}
		affinitySource := ""
		if d.affinityEvidence != nil {
			affinitySource = d.affinityEvidence.Source
		}
		typ := "AFFINITY_BROKEN"
		if d.affinityReused {
			typ = "AFFINITY_REUSED"
		}
		events = append(events,
			domain.ExecutionTraceEvent{
				At: now, Category: domain.TraceRoute, Type: "AFFINITY_FOUND",
				ProviderID: d.affinityBinding.ProviderID, ConnectionID: d.affinityBinding.ConnectionID,
				UpstreamModel: d.affinityBinding.UpstreamModel, Protocol: affinityProtocol,
				AffinityScope: d.affinityBinding.Scope, AffinitySource: affinitySource,
			},
			domain.ExecutionTraceEvent{
				At: now, Category: domain.TraceRoute, Type: typ,
				ProviderID: d.affinityBinding.ProviderID, ConnectionID: d.affinityBinding.ConnectionID,
				UpstreamModel: d.affinityBinding.UpstreamModel, Protocol: affinityProtocol,
				AffinityScope: d.affinityBinding.Scope, AffinitySource: affinitySource,
			})
	}
	if d.plan != nil {
		for _, candidate := range d.plan.Candidates {
			if candidate.Eligible {
				continue
			}
			target := d.w.snap.Targets[candidate.Target]
			event := domain.ExecutionTraceEvent{
				At: now, Category: domain.TraceRoute, Type: "TARGET_SKIPPED",
				Message: candidate.ExclusionReason,
			}
			var routeStep int
			if _, err := fmt.Sscanf(candidate.Tier, "route-%d", &routeStep); err == nil {
				event.RouteStep = routeStep
			}
			if target != nil {
				event.ProviderID = target.ProviderID
				event.ConnectionID = target.ConnectionID
				event.UpstreamModel = target.Model
				event.Protocol = target.EgressProtocol(d.rec.Protocol)
			}
			events = append(events, event)
		}
		if target := d.w.snap.Targets[d.plan.Primary]; target != nil {
			routeStep := 0
			if len(d.plan.OrderTierPolicyIndexes) > 0 {
				routeStep = d.plan.OrderTierPolicyIndexes[0] + 1
			}
			events = append(events, domain.ExecutionTraceEvent{
				At: now, Category: domain.TraceRoute, Type: "CONNECTION_SELECTED",
				ProviderID: target.ProviderID, ConnectionID: target.ConnectionID,
				UpstreamModel: target.Model, Protocol: target.EgressProtocol(d.rec.Protocol), RouteStep: routeStep,
			})
		}
	}
	return events
}

func (d *decision) addTrace(category domain.TraceCategory, typ string,
	target *domain.CompiledTarget, ex *execution, position nav, message string) {
	if d == nil {
		return
	}
	event := domain.ExecutionTraceEvent{At: time.Now(), Category: category, Type: typ, Message: message}
	if ex != nil {
		event.Attempt = ex.seq
		switch typ {
		case "ATTEMPT_STARTED", "CONVERSATION_NATIVE", "CONVERSATION_REHYDRATING", "ADMISSION_COMPLETED", "UPSTREAM_FIRST_EVENT":
			event.Attempt++ // seq counts completed attempts, not the one in flight.
		}
		event.ContinuityMode = ex.continuity
		event.Protocol = ex.protocol
	}
	if position.seg >= 0 && d.plan != nil && position.seg < len(d.plan.OrderTierPolicyIndexes) {
		event.RouteStep = d.plan.OrderTierPolicyIndexes[position.seg] + 1
	}
	if target != nil {
		event.ProviderID = target.ProviderID
		event.ConnectionID = target.ConnectionID
		event.UpstreamModel = target.Model
	}
	d.traceEvents = append(d.traceEvents, event)
	if d.publishTrace != nil {
		d.publishTrace()
	}
}

func (d *decision) traceAttemptStarted(target *domain.CompiledTarget, ex *execution, position nav) {
	d.addTrace(domain.TraceRecovery, "ATTEMPT_STARTED", target, ex, position, "")
	switch ex.continuity {
	case domain.ContinuityNative:
		d.addTrace(domain.TraceContinuity, "CONVERSATION_NATIVE", target, ex, position, "")
	case domain.ContinuityRehydrated:
		d.addTrace(domain.TraceContinuity, "CONVERSATION_REHYDRATING", target, ex, position, "")
	}
}

// ---------------------------------------------------------------------------
// execution state shared by both modes

type execution struct {
	d            *decision
	start        time.Time
	attemptStart time.Time
	seq          int
	lastTarget   domain.ResourceTargetID
	// protocol is the wire format of the attempt in flight — for channel-
	// expanded targets this varies per request (native passthrough or least-
	// lossy native), so the ledger records what actually hit upstream.
	protocol               domain.Protocol
	usage                  domain.Usage
	upstreamUsage          domain.Usage
	estCash                float64
	quotaUnits             float64
	allocSub               float64
	shadow                 float64
	servingCost            float64
	recoveryOverheadCost   float64
	recoveryAddedLatencyMS int64
	commitAt               time.Time
	ttftMS                 int64
	compatLevel            domain.CompatLevel
	compatNotes            []string
	failures               []*domain.Failure
	recoveryEvents         []domain.RecoveryEvent
	continuity             domain.ContinuityMode
	output                 []domain.Block
	nativeResponseID       string
	nativeConversationID   string
	finishReason           string
	responseModel          string
	responseServiceTier    string
	terminalServiceTier    string
	serviceTierObservation *egress.ServiceTierObservation
	responseWarnings       []string
	streamToolBlocks       map[int]int
	streamToolEnded        map[int]bool
}

const parallelToolsSerialMappingNote = "parallel tool preference mapped to serial tool execution"

// contractFor clones the request for a specific target: upstream model name,
// and the wire protocol chosen for this target — native passthrough when the
// model speaks the ingress format, otherwise its least-lossy native format
// (canonical-contract conversion fills the gap). A logical→upstream model
// rename is still same-protocol and retains the raw body; passthrough adapters
// safely rewrite model/stream while preserving every other field. Only an
// actual protocol conversion drops the native body and feature headers.
func contractFor(rc *domain.RequestContract, t *domain.CompiledTarget) *domain.RequestContract {
	contract, _ := contractForTarget(rc, t)
	return contract
}

// contractForTarget also applies target-specific, semantically safe request
// mappings. Parallel tool calls may be serialized whenever a target supports
// tools but not simultaneous calls, regardless of whether the requirement
// came from client traffic, a simulation, or an alias contract.
func contractForTarget(rc *domain.RequestContract, t *domain.CompiledTarget) (*domain.RequestContract, []string) {
	c := *rc
	c.Model = t.Model
	// Origin pins the wire format the client used before Protocol is
	// rewritten below; adapters consult it to tell a replayable raw body
	// from a cross-protocol conversion.
	c.Origin = rc.Protocol
	c.Protocol = t.EgressProtocol(rc.Protocol)
	if c.Protocol != rc.Protocol {
		c.Native = nil
		c.ProtocolHeaders = nil
	}
	notes := []string{}
	capabilities := t.UpstreamCapabilitiesFor(c.Protocol)
	if rc.ParallelTools != nil && *rc.ParallelTools &&
		capabilities.Tools && !capabilities.ParallelTools {
		serial := false
		c.ParallelTools = &serial
		// Same-protocol native replay would otherwise resend the original true
		// value and bypass this explicit canonical mapping.
		c.Native = nil
		notes = append(notes, parallelToolsSerialMappingNote)
	}
	c.Stream = rc.Stream
	return &c, notes
}

// responseProtocolFailure runs before a non-streaming response is committed
// or recorded as successful. Conversion issues are harmless only when the
// exact native body can be returned to a client speaking that same protocol.
func responseProtocolFailure(rc *domain.RequestContract, resp *domain.Response) *domain.Failure {
	if resp == nil {
		return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
			Message: "upstream returned no response"}
	}
	if len(resp.ValidationIssues) > 0 {
		return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
			Message: "upstream response violates its protocol: " + strings.Join(resp.ValidationIssues, "; ")}
	}
	if failure := structuredOutputFailure(rc, resp.Content, resp.FinishReason); failure != nil {
		return failure
	}
	if len(resp.ConversionIssues) == 0 {
		nativeExact := rc != nil && resp.NativeProtocol == rc.Protocol && len(resp.NativeBody) > 0
		if rc != nil && !nativeExact {
			for _, block := range resp.Content {
				if block.Kind == domain.BlockToolUse && !requestDeclaresTool(rc, block.Name) {
					return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "upstream response called undeclared tool " + block.Name}
				}
			}
		}
		return nil
	}
	if rc != nil && resp.NativeProtocol == rc.Protocol && len(resp.NativeBody) > 0 {
		return nil
	}
	return &domain.Failure{
		Type:  domain.FailProtocolConversion,
		Scope: domain.FailureScopeTarget,
		Message: "upstream response cannot be losslessly converted: " +
			strings.Join(resp.ConversionIssues, "; "),
	}
}

func requestDeclaresTool(rc *domain.RequestContract, name string) bool {
	if rc == nil || name == "" {
		return false
	}
	for _, tool := range rc.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func attemptContractProtocolFailure(caller egress.Caller, contract *domain.RequestContract, allowDegradation ...bool) *domain.Failure {
	if err := contract.ValidateToolShape(); err != nil {
		return &domain.Failure{Type: domain.FailUnsupportedParam, Scope: domain.FailureScopeRequest,
			Message: "invalid tool transcript: " + err.Error()}
	}
	preparer, ok := caller.(egress.Preparer)
	if !ok {
		return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
			Message: "egress adapter cannot verify the final attempt contract"}
	}
	var notes []string
	var preserved bool
	var err error
	if detailed, ok := caller.(egress.DetailedPreparer); ok {
		var preparation egress.Preparation
		preparation, err = detailed.PrepareDetailed(contract)
		notes, preserved = preparation.Notes, preparation.ToolTranscriptPreserved
	} else {
		notes, err = preparer.Prepare(contract)
	}
	if err != nil {
		return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
			Message: "final attempt contract cannot be losslessly converted: " + err.Error()}
	}
	if !preserved {
		if err := contract.ValidateToolSequence(); err != nil {
			failure := &domain.Failure{Type: domain.FailUnsupportedParam, Scope: domain.FailureScopeRequest,
				Message: "invalid tool transcript: " + err.Error()}
			// A native Responses transcript may be valid for its source endpoint
			// but impossible to encode faithfully for this selected target.
			if contract.Origin == domain.ProtocolOpenAIResponses && len(contract.Native) > 0 {
				failure.Type = domain.FailProtocolConversion
				failure.Scope = domain.FailureScopeTarget
				failure.Message = "target cannot preserve native Responses tool transcript: " + err.Error()
			}
			return failure
		}
	}
	degradedAllowed := len(allowDegradation) > 0 && allowDegradation[0]
	if len(notes) > 0 && !degradedAllowed {
		return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
			Message: "final attempt contract would degrade semantics: " + strings.Join(notes, "; ")}
	}
	// Best-effort targets may intentionally carry mapping/drop notes through to
	// the adapter. Exact-only callers are excluded during assessment before any
	// attempt can reach this point.
	return nil
}

// rateCardFor finds the price card for a target (model+plan, else model).
func (w *world) rateCardFor(t *domain.CompiledTarget) *domain.RateCard {
	if t.Plan != nil {
		if c, ok := w.cards[cardKey(t.Model, t.Plan.Kind)]; ok {
			return c
		}
	}
	for _, c := range w.cards {
		if c.Model == t.Model {
			return c
		}
	}
	return nil
}

// allocatedSubCost amortizes a subscription fee linearly over 30 days by
// wall-clock duration of the attempt (PRD §15.2 dimension 3).
func allocatedSubCost(t *domain.CompiledTarget, dur time.Duration) float64 {
	if t.Plan == nil || t.Plan.Kind != domain.PlanSubscription || t.Plan.Fee <= 0 {
		return 0
	}
	perSec := t.Plan.Fee / (30 * 86400)
	return perSec * dur.Seconds()
}

// recordAttempt writes one attempt row + feeds runtime health + accumulates
// the request-level cost rollup.
func (ex *execution) recordAttempt(target domain.ResourceTargetID, status domain.RequestStatus,
	f *domain.Failure, latencyMS, ttftMS int64, u domain.Usage, sched *Scheduler) {

	d := ex.d
	t := d.w.snap.Targets[target]
	if t == nil {
		return
	}
	ex.seq++
	now := time.Now()
	mult := 1.0
	if t.Plan != nil {
		mult = t.Plan.CurrentMultiplier(now)
	}
	card := d.w.rateCardFor(t)
	cash := card.Cost(u)
	paidCash := 0.0
	if t.Plan != nil && t.Plan.Kind == domain.PlanPAYG {
		ex.estCash += cash
		paidCash = cash
	}
	dur := time.Since(ex.attemptStart)
	quota := 0.0
	var quotas []domain.QuotaUsage
	for _, b := range t.QuotaBuckets() {
		units := domain.QuotaCost(b.Unit, u, mult, true, cash)
		quotas = append(quotas, domain.QuotaUsage{BucketID: b.ID, Units: units})
		if b == t.Bucket || t.Bucket == nil {
			quota = units
		}
	}
	sub := allocatedSubCost(t, dur)
	ex.shadow += cash
	ex.quotaUnits += quota
	ex.allocSub += sub
	ex.lastTarget = target
	addUsage(&ex.upstreamUsage, u)
	servingAttempt := status == domain.ReqSuccess || !ex.commitAt.IsZero()
	if servingAttempt {
		addUsage(&ex.usage, u)
		ex.servingCost += paidCash + sub
		if ex.compatLevel == "" {
			a := d.compat[target]
			ex.compatLevel = a.level
			ex.compatNotes = a.notes
		}
	} else {
		ex.recoveryOverheadCost += paidCash + sub
		ex.recoveryAddedLatencyMS += latencyMS
	}

	rec := domain.AttemptRecord{
		RequestID: d.rec.ID, Seq: ex.seq, Ts: now,
		TargetID: target, ProviderID: t.ProviderID, ConnectionID: t.ConnectionID,
		EndpointURL: func() string {
			if t.Endpoint != nil {
				if strings.TrimSpace(t.Endpoint.FullURL) != "" {
					return t.Endpoint.FullURL
				}
				return t.Endpoint.BaseURL
			}
			return ""
		}(),
		Protocol:            ex.protocol,
		CredentialRef:       t.Credential.SecretRef, // reference text only, never the secret
		Model:               t.Model,
		ServiceTier:         d.rec.ServiceTier,
		ResponseModel:       ex.responseModel,
		ResponseServiceTier: ex.responseServiceTier,
		ServiceTierEvidence: ex.serviceTierEvidence(status == domain.ReqSuccess),
		PlanKind: func() domain.PlanKind {
			if t.Plan != nil {
				return t.Plan.Kind
			}
			return ""
		}(),
		Status: status, Failure: f,
		LatencyMS: latencyMS, TTFTMS: ttftMS, Usage: u, Multiplier: mult,
		EstCashCost: cash, QuotaUnits: quota, AllocatedSubCost: sub, ShadowCost: cash,
		RateCardVersion: func() string {
			if card != nil {
				return card.Version
			}
			return ""
		}(),
		QuotaRuleVersion: func() string {
			if t.Bucket != nil {
				return t.Bucket.ID
			}
			return ""
		}(),
		MultiplierRuleVersion: fmt.Sprintf("%s@%d", t.ID, d.w.snap.Version),
	}
	sched.led.AddAttempt(rec)
	if status == domain.ReqSuccess {
		d.addTrace(domain.TraceRecovery, "ATTEMPT_SUCCEEDED", t, ex, nav{}, "")
	} else {
		d.addTrace(domain.TraceRecovery, "ATTEMPT_FAILED", t, ex, nav{}, failureMessage(f))
		if f != nil {
			last := &d.traceEvents[len(d.traceEvents)-1]
			last.FailureType = f.Type
			last.FailureScope = f.EffectiveScope()
		}
	}

	if status == domain.ReqSuccess {
		sched.manager.RecordSuccessMulti(target, latencyMS, ttftMS, u, cash, quotas, now)
	} else if f != nil {
		ex.failures = append(ex.failures, f)
		sched.manager.RecordFailure(target, f, now)
	}
}

func addUsage(total *domain.Usage, usage domain.Usage) {
	if *total == (domain.Usage{}) {
		*total = usage
		return
	}
	// Attempts may use different upstream dialects. Preserve raw totals when
	// their semantics agree; otherwise aggregate using independent cache counts.
	if total.CacheSubsetOfInput != usage.CacheSubsetOfInput {
		total.InputTokens = total.UncachedInput()
		total.CacheSubsetOfInput = false
		usage.InputTokens = usage.UncachedInput()
	}
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.CacheReadTokens += usage.CacheReadTokens
	total.CacheWriteTokens += usage.CacheWriteTokens
	total.ReasoningTokens += usage.ReasoningTokens
}

func failureMessage(failure *domain.Failure) string {
	if failure == nil {
		return ""
	}
	return failure.Message
}

// ---------------------------------------------------------------------------
// navigation: typed recovery over tier segments (F-P0-16)

type nav struct {
	seg int // index into plan.OrderTiers
	pos int // index within segment
}

func (d *decision) targetAt(n nav) (domain.ResourceTargetID, bool) {
	if n.seg < 0 || n.seg >= len(d.plan.OrderTiers) {
		return "", false
	}
	seg := d.plan.OrderTiers[n.seg]
	if n.pos < 0 || n.pos >= len(seg) {
		return "", false
	}
	return seg[n.pos], true
}

// advance dispatches V2 recovery by typed scope and then applies the
// request-local recovery budget. Legacy requests retain their existing
// policy semantics behind the compatibility boundary.
func (d *decision) advance(n nav, f *domain.Failure) (nav, string, bool) {
	var next nav
	var action string
	var ok bool
	if d.v2 {
		next, action, ok = d.advanceV2(n, f)
	} else {
		next, action, ok = d.advanceLegacy(n, f)
	}
	if !ok {
		return next, action, false
	}
	routeAdvance := next.seg != n.seg
	if !d.budget.AllowRecovery(time.Now(), routeAdvance) {
		return n, "abort:recovery_budget_exhausted", false
	}
	if routeAdvance {
		d.budget.ConsumeRoute()
	}
	return next, action, true
}

func (d *decision) advanceV2(n nav, f *domain.Failure) (nav, string, bool) {
	if f == nil || f.EffectiveScope() == domain.FailureScopeRequest {
		return n, "abort:request_failure", false
	}
	scope := f.EffectiveScope()
	automaticCompatibilityFallback := f.Type == domain.FailProtocolConversion ||
		(f.Type == domain.FailUnsupportedParam && scope == domain.FailureScopeTarget)
	if scope == domain.FailureScopeConnection || scope == domain.FailureScopeStream {
		next := nav{seg: n.seg, pos: n.pos + 1}
		if _, ok := d.targetAt(next); ok {
			return next, "recovery:connection_rebind", true
		}
	}
	// TARGET skips the current Provider+Model step. PROVIDER and exhausted
	// CONNECTION/STREAM failures also arrive here. Crossing the route edge is
	// fail-closed unless its recover_on allowlist contains this FailureType.
	stepIndex := n.seg
	if n.seg < len(d.plan.OrderTierPolicyIndexes) {
		stepIndex = d.plan.OrderTierPolicyIndexes[n.seg]
	}
	// Protocol-conversion incompatibility is a planning fact discovered late,
	// not a provider outage. It is always safe to try the next route before
	// commit, even for configurations created before this failure type existed.
	if stepIndex < 0 || stepIndex >= len(d.route.Steps) ||
		(!automaticCompatibilityFallback &&
			!containsFailure(d.route.Steps[stepIndex].RecoverOn, f.Type)) {
		return n, "abort:not_in_route_recover_on", false
	}
	next := nav{seg: n.seg + 1, pos: 0}
	if _, ok := d.targetAt(next); !ok {
		return n, "abort:provider_exhausted", false
	}
	return next, "recovery:route_fallback", true
}

func containsFailure(allowed []domain.FailureType, failure domain.FailureType) bool {
	for _, candidate := range allowed {
		if candidate == failure {
			return true
		}
	}
	return false
}

func (d *decision) advanceLegacy(n nav, f *domain.Failure) (nav, string, bool) {
	automaticCompatibilityFallback := f != nil && (f.Type == domain.FailProtocolConversion ||
		(f.Type == domain.FailUnsupportedParam && f.EffectiveScope() == domain.FailureScopeTarget))
	if !domain.FallbackAllowed(f.Type) && !automaticCompatibilityFallback {
		return n, "abort:fail_closed", false
	}
	if len(d.pol.StepRecovery) > 0 {
		stepIndex := n.seg
		if n.seg < len(d.plan.OrderTierPolicyIndexes) {
			stepIndex = d.plan.OrderTierPolicyIndexes[n.seg]
		}
		if stepIndex >= len(d.pol.StepRecovery) ||
			(!automaticCompatibilityFallback && !d.pol.StepRecovery[stepIndex][f.Type]) {
			return n, "abort:not_in_step_fallback_allowlist", false
		}
		// A channel group can contain several eligible targets. Exhaust the
		// current group before entering the next explicit fallback step.
		nn := nav{seg: n.seg, pos: n.pos + 1}
		if _, ok := d.targetAt(nn); ok {
			return nn, "policy:next_target:same_step", true
		}
		nn = nav{seg: n.seg + 1, pos: 0}
		if _, ok := d.targetAt(nn); ok {
			return nn, "policy:next_route_step", true
		}
		return n, "abort:fallback_exhausted", false
	}
	class := f.Type.RecoveryClass()
	if ra, ok := d.pol.Recovery[f.Type]; ok {
		switch {
		case ra.Abort:
			return n, "abort", false
		case ra.NextTarget == "same_tier":
			nn := nav{seg: n.seg, pos: n.pos + 1}
			if _, ok := d.targetAt(nn); ok {
				return nn, "policy:next_target:same_tier", true
			}
			return n, "abort", false
		case ra.NextTarget != "":
			if nn, ok := d.locate(domain.ResourceTargetID(ra.NextTarget)); ok {
				return nn, "policy:next_target:" + ra.NextTarget, true
			}
			return n, "abort", false
		case ra.NextTier:
			nn := nav{seg: n.seg + 1, pos: 0}
			if _, ok := d.targetAt(nn); ok {
				return nn, "policy:next_tier", true
			}
			return n, "abort", false
		}
	}
	switch class {
	case domain.RecoverAbort:
		return n, "abort", false
	case domain.RecoverNextTier:
		nn := nav{seg: n.seg + 1, pos: 0}
		if _, ok := d.targetAt(nn); ok {
			return nn, "next_tier", true
		}
		// no next tier: fall through to same-tier continuation
		nn = nav{seg: n.seg, pos: n.pos + 1}
		if _, ok := d.targetAt(nn); ok {
			return nn, "next_target_same_tier", true
		}
		return n, "abort", false
	default: // next_target_same_tier
		nn := nav{seg: n.seg, pos: n.pos + 1}
		if _, ok := d.targetAt(nn); ok {
			return nn, "next_target_same_tier", true
		}
		nn = nav{seg: n.seg + 1, pos: 0}
		if _, ok := d.targetAt(nn); ok {
			return nn, "next_tier", true
		}
		return n, "abort", false
	}
}

func (d *decision) locate(id domain.ResourceTargetID) (nav, bool) {
	for si, seg := range d.plan.OrderTiers {
		for pi, t := range seg {
			if t == id {
				return nav{seg: si, pos: pi}, true
			}
		}
	}
	return nav{}, false
}

// ---------------------------------------------------------------------------
// public entry points

// Execute runs the full pipeline for one request. rc.Stream selects mode.
// The returned failure is terminal (already ledgered); ingress maps it to
// the wire error shape.
func (s *Scheduler) Execute(ctx context.Context, rc *domain.RequestContract, meta RequestMeta) (*Result, *domain.Failure) {
	if ingress, ok := ctx.Value(ingressRequestKey{}).(*ingressRequest); ok {
		meta.requestID, meta.startedAt = ingress.id, ingress.startedAt
		if rc != nil {
			ingress.claimed.Store(true)
		}
	}
	if rc == nil {
		return nil, failf(domain.FailUnsupportedParam, "nil request contract")
	}
	tier, err := domain.NormalizeServiceTier(rc.ServiceTier)
	contract := *rc
	if err == nil {
		contract.ServiceTier = tier
	}
	rc = &contract
	d, f := s.decide(ctx, rc, meta, false)
	if f != nil {
		return nil, f
	}
	deadline, _ := ctx.Deadline()
	d.initializeRecoveryBudget(time.Now(), deadline)
	s.setActiveTraffic(d, TrafficWaiting, nil)
	if rc.Stream {
		return s.executeStream(ctx, rc, d)
	}
	return s.executeInvoke(ctx, rc, d)
}

// DryRun resolves and plans without executing anything or writing ledger
// rows. It powers the console dry-run (PRD §16.5).
func (s *Scheduler) DryRun(rc *domain.RequestContract, meta RequestMeta) (*policy.PlanResult, *domain.CompiledPolicy, *domain.Failure) {
	d, f := s.decide(context.Background(), rc, meta, true)
	if f != nil {
		return nil, nil, f
	}
	return d.plan, d.pol, nil
}

// DryRunExclusions returns the candidate evaluation from a dry run, for the
// console to show why each target was included/excluded.
func (s *Scheduler) DryRunExclusions(rc *domain.RequestContract, meta RequestMeta) ([]domain.CandidateRecord, *domain.Failure) {
	d, f := s.decide(context.Background(), rc, meta, true)
	if f != nil {
		return nil, f
	}
	return d.plan.Candidates, nil
}

// V2Simulation is the product-level route explanation used by the V2
// management facade. Candidate target IDs remain internal to this package's
// caller and must be projected to Provider/Connection identities before JSON.
type V2Simulation struct {
	LogicalModelID string
	OverrideID     string
	DefaultRoute   domain.RouteSpec
	EffectiveRoute domain.RouteSpec
	Plan           *policy.PlanResult
	Snapshot       *domain.CompiledSnapshot
}

func (s *Scheduler) DryRunV2(rc *domain.RequestContract, meta RequestMeta) (*V2Simulation, *domain.Failure) {
	d, failure := s.decide(context.Background(), rc, meta, true)
	if failure != nil {
		return nil, failure
	}
	if !d.v2 || d.logical == nil {
		return nil, failf(domain.FailUnsupportedParam, "model %q is not an enabled logical model", rc.Model)
	}
	return &V2Simulation{
		LogicalModelID: d.logical.ID,
		OverrideID:     d.overrideID,
		DefaultRoute:   d.logical.DefaultRoute,
		EffectiveRoute: d.route,
		Plan:           d.plan,
		Snapshot:       d.w.snap,
	}, nil
}

// DryRunV2Snapshot evaluates an unpublished compiled snapshot through the
// exact live decision path. Existing callers/runtime evidence are mapped by
// stable Provider+Connection+Model identity; new unresolved connections stay
// ineligible until their credentials and runtime state exist.
func (s *Scheduler) DryRunV2Snapshot(snapshot *domain.CompiledSnapshot, rc *domain.RequestContract, meta RequestMeta) (*V2Simulation, *domain.Failure) {
	active := s.world.Load()
	if active == nil || snapshot == nil {
		return nil, failf(domain.FailBadResponse, "scheduler world or candidate snapshot is not initialized")
	}
	activeTargets := map[string]domain.ResourceTargetID{}
	for id, target := range active.snap.Targets {
		if target != nil {
			activeTargets[targetIdentityKey(target)] = id
		}
	}
	callers := make(map[domain.ResourceTargetID]egress.Caller, len(snapshot.Targets))
	for id, target := range snapshot.Targets {
		if target == nil {
			continue
		}
		if activeID, ok := activeTargets[targetIdentityKey(target)]; ok {
			callers[id] = active.callers[activeID]
		}
	}
	candidate := &world{
		snap: snapshot, callers: callers, cards: active.cards, keys: active.keys,
		routing: active.routing, routingErr: active.routingErr,
		routingCacheHit: active.routingCacheHit,
		routeTargets:    indexRouteTargets(snapshot),
	}
	d, failure := s.decideWithWorld(context.Background(), candidate, rc, meta, true)
	if d == nil && failure != nil {
		return nil, failure
	}
	if !d.v2 || d.logical == nil {
		return nil, failf(domain.FailUnsupportedParam, "model %q is not an enabled logical model", rc.Model)
	}
	return &V2Simulation{
		LogicalModelID: d.logical.ID, OverrideID: d.overrideID,
		DefaultRoute: d.logical.DefaultRoute, EffectiveRoute: d.route,
		Plan: d.plan, Snapshot: snapshot,
	}, failure
}

func targetIdentityKey(target *domain.CompiledTarget) string {
	return target.ProviderID + "\x00" + target.ConnectionID + "\x00" + target.Model
}

// executeInvoke: non-streaming attempt loop.
func (s *Scheduler) executeInvoke(ctx context.Context, rc *domain.RequestContract, d *decision) (*Result, *domain.Failure) {
	ex := &execution{d: d, start: time.Now()}
	n := nav{}
	for {
		if ctx.Err() != nil {
			s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
			return nil, failf(domain.FailClientCancel, "client canceled")
		}
		id, ok := d.targetAt(n)
		if !ok {
			break
		}
		if !d.budget.AllowAttempt(time.Now()) {
			f := failf(domain.FailTTFTTimeout, "recovery budget exhausted before next upstream attempt")
			f.Scope = domain.FailureScopeRequest
			d.recordRecovery(ex, ex.seq, f, "abort:recovery_budget_exhausted", n)
			s.finish(d, domain.ReqFailed, f, ex)
			return nil, f
		}
		ex.responseModel, ex.responseServiceTier = "", ""
		ex.terminalServiceTier = ""
		ex.serviceTierObservation = nil
		t := d.w.snap.Targets[id]
		caller := d.w.callers[id]
		if caller == nil {
			// defense in depth: planning excludes caller-less targets; if one
			// is ever reached anyway, fail the attempt without a panic.
			f := failf(domain.FailBadResponse, "target %s has no configured credential", id)
			ex.recordAttempt(id, domain.ReqFailed, f, 0, 0, domain.Usage{}, s)
			s.finish(d, domain.ReqFailed, f, ex)
			return nil, f
		}
		c, continuity := d.contractForAttempt(rc, t)
		ex.continuity = continuity
		ex.protocol = c.Protocol
		contractFailure := attemptContractProtocolFailure(caller, c, d.compat[id].level == domain.CompatDegraded)
		if ctx.Err() != nil {
			f := failf(domain.FailClientCancel, "client canceled during attempt preparation")
			s.finish(d, domain.ReqCanceled, f, ex)
			return nil, f
		}
		if contractFailure != nil {
			d.addTrace(domain.TraceRoute, "TARGET_SKIPPED", t, ex, n, contractFailure.Message)
			nn, action, cont := d.advance(n, contractFailure)
			d.recordRecovery(ex, ex.seq, contractFailure, action, nn)
			if !cont {
				s.finish(d, domain.ReqFailed, contractFailure, ex)
				return nil, contractFailure
			}
			n = nn
			continue
		}
		d.budget.ConsumeAttempt()
		d.traceAttemptStarted(t, ex, n)
		s.setActiveTraffic(d, TrafficWaiting, t)

		release, admitFailure := s.manager.Admit(ctx, id)
		if admitFailure != nil {
			ex.recordAttempt(id, domain.ReqFailed, admitFailure, 0, 0, domain.Usage{}, s)
			if ctx.Err() != nil {
				s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
				return nil, failf(domain.FailClientCancel, "client canceled")
			}
			nn, action, cont := d.advance(n, admitFailure)
			d.recordRecovery(ex, ex.seq, admitFailure, action, nn)
			if !cont {
				s.finish(d, domain.ReqFailed, admitFailure, ex)
				return nil, admitFailure
			}
			n = nn
			continue
		}
		s.setActiveTraffic(d, executionTrafficPhase(ex), t)
		d.addTrace(domain.TraceRecovery, "ADMISSION_COMPLETED", t, ex, n, "")
		attemptStart := time.Now()
		ex.attemptStart = attemptStart
		wait := newFirstOutputWait(ex.observeServiceTier(ctx, t), d.firstOutputTimeout())
		resp, f := caller.Invoke(wait.ctx, c)
		wait.close()
		if ctx.Err() != nil {
			f = failf(domain.FailClientCancel, "client canceled")
		} else if timeoutFailure := wait.failure(); timeoutFailure != nil {
			f = timeoutFailure
			d.addTrace(domain.TraceOutput, "FIRST_OUTPUT_TIMEOUT", t, ex, n, f.Message)
		}
		if resp != nil {
			ex.captureResponseMetadata(resp.Model, resp.ServiceTier)
			ex.terminalServiceTier = resp.ServiceTier
		}
		release()
		if f == nil {
			f = responseProtocolFailure(rc, resp)
		}

		if f == nil {
			ex.output = append([]domain.Block(nil), resp.Content...)
			ex.finishReason = resp.FinishReason
			ex.responseWarnings = append([]string(nil), resp.Warnings...)
			ex.compatNotes = append(ex.compatNotes, resp.Warnings...)
			ex.nativeResponseID = resp.NativeResponseID
			ex.nativeConversationID = resp.NativeConversationID
			ex.recordAttempt(id, domain.ReqSuccess, nil,
				time.Since(attemptStart).Milliseconds(), 0, resp.Usage, s)
			if ex.continuity == domain.ContinuityRehydrated {
				d.addTrace(domain.TraceContinuity, "CONVERSATION_REHYDRATED", t, ex, n, "")
			}
			if shouldPersistConversation(d.evidence, rc.Protocol) {
				if err := d.persistConversation(rc, t, ex.output, ex.nativeResponseID, ex.nativeConversationID, ex.continuity); err != nil {
					failure := &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeRequest,
						Message: "persist response lineage: " + err.Error()}
					s.finish(d, domain.ReqFailed, failure, ex)
					return nil, failure
				}
			}
			d.bindSuccessfulAffinity(s.affinity, t, ex.seq)
			ex.commitAt = time.Now()
			for _, warning := range resp.Warnings {
				d.addTrace(domain.TraceOutput, "RESPONSE_WARNING", t, ex, n, warning)
			}
			d.addTrace(domain.TraceOutput, "COMMIT", t, ex, n, "non-streaming response")
			s.finish(d, domain.ReqSuccess, nil, ex)
			return &Result{
				RequestID: d.publicResponseID(), LedgerRequestID: d.rec.ID, Response: resp, Target: id,
				ServiceTierEvidence: ex.serviceTierEvidence(true),
				Model:               t.ClientModel(), PolicyName: d.polName, PolicyVer: d.polVersion,
				PolicyHash: d.polHash, PolicyIndexHash: d.w.snap.PolicyIndexHash,
				PolicyCompileCacheHit: d.policyCompileCacheHit, PolicySceneCacheHit: d.policySceneCacheHit,
				CompatLevel: ex.compatLevel, CompatNotes: ex.compatNotes,
				Outcome: outcomeForExecution(ex.seq, ex.continuity), ProviderID: t.ProviderID, ConnectionID: t.ConnectionID,
				ConversationID: conversationIDOf(d.conversation), ContinuityMode: ex.continuity,
			}, nil
		}
		failureUsage := domain.Usage{}
		if resp != nil {
			failureUsage = resp.Usage
		}
		ex.recordAttempt(id, domain.ReqFailed, f, time.Since(attemptStart).Milliseconds(), 0, failureUsage, s)

		if ctx.Err() != nil {
			s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
			return nil, failf(domain.FailClientCancel, "client canceled")
		}
		nn, action, cont := d.advance(n, f)
		d.recordRecovery(ex, ex.seq, f, action, nn)
		if !cont {
			break
		}
		n = nn
	}
	last := d.lastFailureOf(ex)
	s.finish(d, domain.ReqFailed, last, ex)
	return nil, last
}

// executeStream: streaming attempt loop with the Stream Commit boundary
// (F-P0-17): after the first externally-visible event reaches the client,
// no cross-target switch may happen — an upstream error from then on is
// passed through as EvError.
func (s *Scheduler) executeStream(ctx context.Context, rc *domain.RequestContract, d *decision) (*Result, *domain.Failure) {
	ex := &execution{d: d, start: time.Now()}
	out := make(chan domain.StreamEvent, 64)

	n := nav{}
	var committed bool

	// pump lifecycle owned by the active attempt goroutine; result channel
	// returned to ingress immediately after the first successful connect.
	resCh := make(chan *Result, 1)
	failCh := make(chan *domain.Failure, 1)

	go func() {
		defer close(out)
		for {
			if ctx.Err() != nil {
				s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
				failCh <- failf(domain.FailClientCancel, "client canceled")
				return
			}
			id, ok := d.targetAt(n)
			if !ok {
				break
			}
			if !d.budget.AllowAttempt(time.Now()) {
				f := failf(domain.FailTTFTTimeout, "recovery budget exhausted before next upstream attempt")
				f.Scope = domain.FailureScopeRequest
				d.recordRecovery(ex, ex.seq, f, "abort:recovery_budget_exhausted", n)
				s.finish(d, domain.ReqFailed, f, ex)
				failCh <- f
				return
			}
			ex.responseModel, ex.responseServiceTier = "", ""
			ex.terminalServiceTier = ""
			ex.serviceTierObservation = nil
			t := d.w.snap.Targets[id]
			caller := d.w.callers[id]
			if caller == nil {
				f := failf(domain.FailBadResponse, "target %s has no configured credential", id)
				ex.recordAttempt(id, domain.ReqFailed, f, 0, 0, domain.Usage{}, s)
				s.finish(d, domain.ReqFailed, f, ex)
				failCh <- f
				return
			}
			c2, continuity := d.contractForAttempt(rc, t)
			ex.continuity = continuity
			ex.output = nil
			ex.streamToolBlocks = nil
			ex.streamToolEnded = nil
			ex.nativeResponseID = ""
			ex.nativeConversationID = ""
			c2.Stream = true
			ex.protocol = c2.Protocol
			contractFailure := attemptContractProtocolFailure(caller, c2, d.compat[id].level == domain.CompatDegraded)
			if ctx.Err() != nil {
				f := failf(domain.FailClientCancel, "client canceled during attempt preparation")
				s.finish(d, domain.ReqCanceled, f, ex)
				failCh <- f
				return
			}
			if contractFailure != nil {
				d.addTrace(domain.TraceRoute, "TARGET_SKIPPED", t, ex, n, contractFailure.Message)
				nn, action, cont := d.advance(n, contractFailure)
				d.recordRecovery(ex, ex.seq, contractFailure, action, nn)
				if !cont {
					s.finish(d, domain.ReqFailed, contractFailure, ex)
					failCh <- contractFailure
					return
				}
				n = nn
				continue
			}
			d.budget.ConsumeAttempt()
			d.traceAttemptStarted(t, ex, n)
			s.setActiveTraffic(d, TrafficWaiting, t)

			release, admitFailure := s.manager.Admit(ctx, id)
			if admitFailure != nil {
				ex.recordAttempt(id, domain.ReqFailed, admitFailure, 0, 0, domain.Usage{}, s)
				if ctx.Err() != nil {
					s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
					failCh <- failf(domain.FailClientCancel, "client canceled")
					return
				}
				nn, action, cont := d.advance(n, admitFailure)
				d.recordRecovery(ex, ex.seq, admitFailure, action, nn)
				if !cont {
					s.finish(d, domain.ReqFailed, admitFailure, ex)
					failCh <- admitFailure
					return
				}
				n = nn
				continue
			}
			s.setActiveTraffic(d, executionTrafficPhase(ex), t)
			d.addTrace(domain.TraceRecovery, "ADMISSION_COMPLETED", t, ex, n, "")
			attemptStart := time.Now()
			ex.attemptStart = attemptStart
			wait := newFirstOutputWait(ex.observeServiceTier(ctx, t), d.firstOutputTimeout())
			ch, upstreamCancel, f := caller.Stream(wait.ctx, c2)
			cancel := func() {
				wait.close()
				if upstreamCancel != nil {
					upstreamCancel()
				}
			}
			if f != nil {
				cancel()
				if timeoutFailure := wait.failure(); timeoutFailure != nil {
					f = timeoutFailure
					d.addTrace(domain.TraceOutput, "FIRST_OUTPUT_TIMEOUT", t, ex, n, f.Message)
				}
				release()
				ex.recordAttempt(id, domain.ReqFailed, f, time.Since(attemptStart).Milliseconds(), 0, domain.Usage{}, s)
				if ctx.Err() != nil {
					s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
					failCh <- failf(domain.FailClientCancel, "client canceled")
					return
				}
				nn, action, cont := d.advance(n, f)
				d.recordRecovery(ex, ex.seq, f, action, nn)
				if !cont {
					last := f
					s.finish(d, domain.ReqFailed, last, ex)
					failCh <- last
					return
				}
				n = nn
				continue
			}
			d.addTrace(domain.TraceOutput, "STREAM_STARTED", t, ex, n, "")

			// consume this attempt's events
			var ttft int64
			var usage domain.Usage
			var finish string
			firstVisible := true
			terminalSeen := false
			var streamErr *domain.Failure
			consumeDone := false
			var pendingNative []domain.StreamEvent
			pendingNativeBytes := 0
			nativeTerminalPending := false
			commitStream := func(traceMessage string) bool {
				if !firstVisible {
					return true
				}
				if !wait.acceptOutput() {
					streamErr = wait.failure()
					if streamErr == nil {
						streamErr = failf(domain.FailClientCancel, "client canceled")
					}
					consumeDone = true
					cancel()
					return false
				}
				firstVisible = false
				committed = true
				ex.commitAt = time.Now()
				s.setActiveTraffic(d, TrafficStreaming, t)
				ex.ttftMS = max(1, time.Since(d.rec.Ts).Milliseconds())
				if _, observed := ctx.Value(ingressRequestKey{}).(*ingressRequest); !observed {
					// HTTP ingress reports actual downstream delivery separately.
					s.led.RecordFirstOutput(d.rec.ID, ex.commitAt)
				}
				ex.compatLevel = d.compat[id].level
				ex.compatNotes = d.compat[id].notes
				resCh <- &Result{
					RequestID: d.publicResponseID(), LedgerRequestID: d.rec.ID, Events: out,
					ServiceTierEvidence: ex.serviceTierEvidence(false),
					Cancel:              func() { cancel() },
					Target:              id, Model: t.ClientModel(),
					PolicyName: d.polName, PolicyVer: d.polVersion,
					PolicyHash: d.polHash, PolicyIndexHash: d.w.snap.PolicyIndexHash,
					PolicyCompileCacheHit: d.policyCompileCacheHit, PolicySceneCacheHit: d.policySceneCacheHit,
					CompatLevel: ex.compatLevel, CompatNotes: ex.compatNotes,
					Outcome: outcomeForExecution(ex.seq+1, ex.continuity), ProviderID: t.ProviderID, ConnectionID: t.ConnectionID,
					ConversationID: conversationIDOf(d.conversation), ContinuityMode: ex.continuity,
				}
				d.addTrace(domain.TraceOutput, "COMMIT", t, ex, n, traceMessage)
				return true
			}
			flushPendingNative := func() {
				for _, pending := range pendingNative {
					out <- pending
				}
				pendingNative = nil
				pendingNativeBytes = 0
			}
			firstUpstreamEvent := true
			for ev := range ch {
				if firstUpstreamEvent {
					d.addTrace(domain.TraceOutput, "UPSTREAM_FIRST_EVENT", t, ex, n, "")
					firstUpstreamEvent = false
				}
				ex.captureResponseMetadata(ev.Model, ev.ServiceTier)
				if ev.Kind == domain.EvEnd {
					ex.terminalServiceTier = ev.ServiceTier
				}
				if ctx.Err() != nil {
					cancel()
					// drain rest quietly
					for range ch {
					}
					release()
					ex.recordAttempt(id, domain.ReqCanceled, nil, time.Since(attemptStart).Milliseconds(), ttft, usage, s)
					s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
					if committed {
						out <- domain.StreamEvent{Kind: domain.EvError,
							Err: failf(domain.FailClientCancel, "client canceled")}
					}
					failCh <- failf(domain.FailClientCancel, "client canceled")
					return
				}
				if wait.failure() != nil {
					cancel()
					for range ch {
					}
					break
				}
				meaningful := domain.MeaningfulStreamEvent(ev)
				if meaningful {
					wait.observe()
					if wait.failure() != nil {
						cancel()
						for range ch {
						}
						break
					}
					if ttft == 0 {
						ttft = max(1, time.Since(attemptStart).Milliseconds())
					}
				}
				switch ev.Kind {
				case domain.EvTextDelta, domain.EvThinkingDelta, domain.EvSignatureDelta,
					domain.EvRedactedThinking, domain.EvToolStart, domain.EvToolDelta, domain.EvToolEnd:
					if nativeTerminalPending {
						streamErr = &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
							Message: "upstream emitted content after its native terminal frame"}
						consumeDone = true
						cancel()
						break
					}
					if failure := visibleStreamProtocolFailure(rc, ex, ev); failure != nil {
						streamErr = failure
						consumeDone = true
						cancel()
						break
					}
					ex.captureStreamOutput(ev)
					if firstVisible && !meaningful {
						break
					}
					if firstVisible {
						if !commitStream("first client-visible stream event") {
							break
						}
						flushPendingNative()
					}
					out <- ev
				case domain.EvNative:
					if ev.NativeProtocol != rc.Protocol || ev.NativeFrame == nil {
						streamErr = &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
							Message: "upstream emitted a native frame for a different client protocol"}
						consumeDone = true
						cancel()
						break
					}
					if nativeTerminalPending {
						streamErr = &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget,
							Message: "upstream emitted a frame after its native terminal frame"}
						consumeDone = true
						cancel()
						break
					}
					if ev.NativeFrame.Terminal {
						nativeTerminalPending = true
						pendingNative = append(pendingNative, ev)
						pendingNativeBytes += len(ev.NativeFrame.Event) + len(ev.NativeFrame.Data)
						break
					}
					if ev.NativeFrame.Meaningful && firstVisible {
						if !commitStream("first client-visible native output event") {
							break
						}
						flushPendingNative()
					}
					if !committed && ev.NativeFrame.KeepAlive {
						break
					}
					if !committed {
						pendingNative = append(pendingNative, ev)
						pendingNativeBytes += len(ev.NativeFrame.Event) + len(ev.NativeFrame.Data)
						if len(pendingNative) > 256 || pendingNativeBytes > 1<<20 {
							streamErr = &domain.Failure{Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
								Message: "upstream emitted excessive native frames before meaningful content"}
							consumeDone = true
							cancel()
						}
						break
					}
					out <- ev
				case domain.EvEnd:
					if failure := completedStreamProtocolFailure(rc, ex, ev.FinishReason); failure != nil {
						streamErr = failure
						consumeDone = true
						cancel()
						break
					}
					if ev.Usage != nil {
						usage = *ev.Usage
					}
					terminalSeen = true
					finish = ev.FinishReason
					ex.finishReason = finish
					ex.nativeResponseID = ev.NativeResponseID
					ex.nativeConversationID = ev.NativeConversationID
					if firstVisible && len(pendingNative) > 0 {
						if !commitStream("validated native terminal stream") {
							break
						}
					}
					flushPendingNative()
					nativeTerminalPending = false
					consumeDone = true
				case domain.EvError:
					if ev.Err != nil {
						streamErr = ev.Err
					} else {
						streamErr = failf(domain.FailStreamBroken, "upstream stream error")
					}
					consumeDone = true
				case domain.EvStart:
					// Internal lifecycle marker; it has no client-visible payload.
				default:
					streamErr = &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
						Message: "upstream emitted an unknown canonical stream event"}
					consumeDone = true
					cancel()
				}
				if consumeDone {
					// drain any trailing events (e.g. usage after finish)
					for e2 := range ch {
						ex.captureResponseMetadata(e2.Model, e2.ServiceTier)
						if e2.Kind == domain.EvEnd {
							ex.terminalServiceTier = e2.ServiceTier
						}
						if e2.Kind == domain.EvEnd && e2.Usage != nil {
							usage = *e2.Usage
						}
					}
					break
				}
			}
			cancel()
			release()
			if timeoutFailure := wait.failure(); timeoutFailure != nil && !committed {
				streamErr = timeoutFailure
				d.addTrace(domain.TraceOutput, "FIRST_OUTPUT_TIMEOUT", t, ex, n, streamErr.Message)
			}
			if streamErr == nil && !terminalSeen {
				streamErr = failf(domain.FailStreamBroken, "upstream stream closed without a terminal event")
				streamErr.Scope = domain.FailureScopeStream
			}

			if !committed {
				// never showed the client anything — recover
				if streamErr == nil {
					streamErr = failf(domain.FailStreamBroken, "upstream ended before any content (finish=%s)", finish)
				}
				ex.recordAttempt(id, domain.ReqFailed, streamErr, time.Since(attemptStart).Milliseconds(), ttft, usage, s)
				if ctx.Err() != nil {
					s.finish(d, domain.ReqCanceled, failf(domain.FailClientCancel, "client canceled"), ex)
					failCh <- failf(domain.FailClientCancel, "client canceled")
					return
				}
				nn, action, cont := d.advance(n, streamErr)
				d.recordRecovery(ex, ex.seq, streamErr, action, nn)
				if !cont {
					s.finish(d, domain.ReqFailed, streamErr, ex)
					failCh <- streamErr
					return
				}
				n = nn
				continue
			}

			// committed: this target owns the response no matter what
			if streamErr == nil {
				ex.recordAttempt(id, domain.ReqSuccess, nil, time.Since(attemptStart).Milliseconds(), ttft, usage, s)
				if ex.continuity == domain.ContinuityRehydrated {
					d.addTrace(domain.TraceContinuity, "CONVERSATION_REHYDRATED", t, ex, n, "")
				}
				if shouldPersistConversation(d.evidence, rc.Protocol) {
					if err := d.persistConversation(rc, t, ex.output, ex.nativeResponseID, ex.nativeConversationID, ex.continuity); err != nil {
						failure := &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeRequest,
							Message: "persist response lineage: " + err.Error()}
						s.finish(d, domain.ReqFailed, failure, ex)
						out <- domain.StreamEvent{Kind: domain.EvError, Err: failure}
						return
					}
				}
				d.bindSuccessfulAffinity(s.affinity, t, ex.seq)
				s.finish(d, domain.ReqSuccess, nil, ex)
				// A terminal event is the caller's completion boundary. Finalize
				// accounting first so shutdown cannot close the ledger while this
				// goroutine is still enqueueing the request record.
				out <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: finish, Usage: &usage, Model: ex.responseModel, ServiceTier: ex.responseServiceTier}
				return
			}
			d.addTrace(domain.TraceOutput, "STREAM_BROKEN", t, ex, n, failureMessage(streamErr))
			ex.recordAttempt(id, domain.ReqFailed, streamErr, time.Since(attemptStart).Milliseconds(), ttft, usage, s)
			s.finish(d, domain.ReqFailed, streamErr, ex)
			out <- domain.StreamEvent{Kind: domain.EvError, Err: streamErr}
			return
		}
		// order exhausted without any successful connect
		last := failf(domain.FailConnect, "all targets failed before stream start")
		if lf := d.lastFailureOf(ex); lf != nil {
			last = lf
		}
		s.finish(d, domain.ReqFailed, last, ex)
		failCh <- last
	}()

	select {
	case r := <-resCh:
		return r, nil
	case f := <-failCh:
		return nil, f
	}
}

// Empty lifecycle fields never erase metadata observed on the same attempt.
func (ex *execution) captureResponseMetadata(model, tier string) {
	if model != "" {
		ex.responseModel = model
	}
	if tier != "" {
		ex.responseServiceTier = tier
	}
}

func (ex *execution) captureStreamOutput(event domain.StreamEvent) {
	switch event.Kind {
	case domain.EvTextDelta:
		ex.output = appendOrMergeBlock(ex.output, domain.Block{Kind: domain.BlockText, Text: event.TextDelta})
	case domain.EvThinkingDelta:
		ex.output = appendOrMergeBlock(ex.output, domain.Block{Kind: domain.BlockThinking, Text: event.ThinkingDelta})
	case domain.EvSignatureDelta:
		for i := len(ex.output) - 1; i >= 0; i-- {
			if ex.output[i].Kind == domain.BlockThinking {
				ex.output[i].Signature += event.SignatureDelta
				return
			}
		}
		ex.output = append(ex.output, domain.Block{Kind: domain.BlockThinking, Signature: event.SignatureDelta})
	case domain.EvRedactedThinking:
		ex.output = append(ex.output, domain.Block{Kind: domain.BlockRedactedThinking, Data: event.RedactedThinkingData})
	case domain.EvToolStart:
		if ex.streamToolBlocks == nil {
			ex.streamToolBlocks = map[int]int{}
		}
		if ex.streamToolEnded == nil {
			ex.streamToolEnded = map[int]bool{}
		}
		if index, ok := ex.streamToolBlocks[event.Index]; ok && index >= 0 && index < len(ex.output) {
			ex.output[index].ID = event.ToolID
			ex.output[index].Name = event.ToolName
			break
		}
		ex.streamToolBlocks[event.Index] = len(ex.output)
		ex.output = append(ex.output, domain.Block{Kind: domain.BlockToolUse, ID: event.ToolID, Name: event.ToolName})
	case domain.EvToolDelta:
		if index, ok := ex.streamToolBlocks[event.Index]; ok && index >= 0 && index < len(ex.output) &&
			ex.output[index].Kind == domain.BlockToolUse {
			ex.output[index].Input = append(ex.output[index].Input, event.ArgsDelta...)
		}
	case domain.EvToolEnd:
		if index, ok := ex.streamToolBlocks[event.Index]; ok && index >= 0 && index < len(ex.output) &&
			ex.output[index].Kind == domain.BlockToolUse {
			if ex.streamToolEnded == nil {
				ex.streamToolEnded = map[int]bool{}
			}
			ex.streamToolEnded[event.Index] = true
		}
	}
}

func visibleStreamProtocolFailure(rc *domain.RequestContract, ex *execution, event domain.StreamEvent) *domain.Failure {
	failure := func(message string) *domain.Failure {
		return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget, Message: message}
	}
	switch event.Kind {
	case domain.EvSignatureDelta, domain.EvRedactedThinking:
		if rc == nil || rc.Protocol != domain.ProtocolAnthropic {
			return failure("native Anthropic thinking state cannot be represented in the client protocol")
		}
	case domain.EvToolStart:
		if event.ToolID == "" || event.ToolName == "" {
			return failure("streamed tool call is missing its id or name")
		}
		if ex != nil && ex.streamToolBlocks != nil {
			if _, exists := ex.streamToolBlocks[event.Index]; exists {
				return failure("streamed tool call reused an existing index")
			}
		}
		if rc != nil && ex != nil && ex.protocol != rc.Protocol && !requestDeclaresTool(rc, event.ToolName) {
			return failure("upstream stream called undeclared tool " + event.ToolName)
		}
	case domain.EvToolDelta:
		if ex == nil || ex.streamToolBlocks == nil {
			return failure("streamed tool arguments arrived before the tool call started")
		}
		if _, ok := ex.streamToolBlocks[event.Index]; !ok {
			return failure("streamed tool arguments do not match a started tool call")
		}
		if ex.streamToolEnded[event.Index] {
			return failure("streamed tool arguments arrived after the tool call ended")
		}
	case domain.EvToolEnd:
		if ex == nil || ex.streamToolBlocks == nil {
			return failure("streamed tool call ended before it started")
		}
		index, ok := ex.streamToolBlocks[event.Index]
		if !ok || index < 0 || index >= len(ex.output) {
			return failure("streamed tool call ended without a matching start")
		}
		if ex.streamToolEnded[event.Index] {
			return failure("streamed tool call ended more than once")
		}
		arguments := ex.output[index].Input
		if !domain.ValidJSONObject(arguments) {
			return failure("streamed tool call arguments are missing or not a JSON object")
		}
	}
	return nil
}

func completedStreamProtocolFailure(rc *domain.RequestContract, ex *execution, finishReason string) *domain.Failure {
	if finishReason != "stop" && finishReason != "tool_call" && finishReason != "length" {
		return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
			Message: "stream completed without a supported finish reason"}
	}
	if ex == nil {
		return structuredOutputFailure(rc, nil, finishReason)
	}
	if len(ex.streamToolBlocks) > 0 {
		for eventIndex, blockIndex := range ex.streamToolBlocks {
			if !ex.streamToolEnded[eventIndex] {
				return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
					Message: "stream completed with an unfinished tool call"}
			}
			if blockIndex < 0 || blockIndex >= len(ex.output) ||
				!domain.ValidJSONObject(ex.output[blockIndex].Input) {
				return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
					Message: "stream completed with invalid tool call arguments"}
			}
		}
		if finishReason != "tool_call" {
			return &domain.Failure{Type: domain.FailProtocolConversion, Scope: domain.FailureScopeTarget,
				Message: "stream emitted tool calls without a tool_call finish reason"}
		}
	}
	return structuredOutputFailure(rc, ex.output, finishReason)
}

func structuredOutputFailure(rc *domain.RequestContract, content []domain.Block, finishReason string) *domain.Failure {
	if rc == nil || rc.ResponseFormat == nil || finishReason == "tool_call" {
		return nil
	}
	var text strings.Builder
	for _, block := range content {
		if block.Kind == domain.BlockText {
			text.WriteString(block.Text)
		}
	}
	raw := []byte(strings.TrimSpace(text.String()))
	bad := func(message string) *domain.Failure {
		return &domain.Failure{Type: domain.FailBadResponse, Scope: domain.FailureScopeTarget, Message: message}
	}
	if len(raw) == 0 || !json.Valid(raw) {
		return bad("upstream violated structured output: response text is not valid JSON")
	}
	if len(rc.ResponseFormat.Schema) == 0 {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return bad("upstream violated json_object output: response is not a JSON object")
		}
	}
	return nil
}

func appendOrMergeBlock(blocks []domain.Block, block domain.Block) []domain.Block {
	if len(blocks) > 0 && blocks[len(blocks)-1].Kind == block.Kind {
		blocks[len(blocks)-1].Text += block.Text
		return blocks
	}
	return append(blocks, block)
}

const requestDebugLimit = 256 * 1024

func requestDebugPayload(rc *domain.RequestContract) (string, bool) {
	var value any
	if len(rc.Native) > 0 && json.Unmarshal(rc.Native, &value) == nil {
		value = redactDebugValue(value)
	} else {
		messages := make([]any, 0, len(rc.Messages))
		for _, message := range rc.Messages {
			messages = append(messages, map[string]any{
				"role": message.Role, "name": message.Name, "tool_id": message.ToolID,
				"content": debugBlocks(message.Blocks),
			})
		}
		tools := make([]any, 0, len(rc.Tools))
		for _, tool := range rc.Tools {
			tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "parameters": json.RawMessage(tool.Parameters)})
		}
		value = map[string]any{
			"protocol": rc.Protocol, "model": rc.Model, "messages": messages, "tools": tools,
			"tool_choice": rc.ToolChoice, "max_tokens": rc.MaxTokens, "temperature": rc.Temperature,
			"top_p": rc.TopP, "stop_sequences": rc.StopSequences, "stream": rc.Stream,
			"reasoning": rc.Reasoning, "response_format": rc.ResponseFormat,
		}
	}
	return marshalDebugValue(value)
}

func responseDebugPayload(status domain.RequestStatus, failure *domain.Failure, ex *execution) (string, bool) {
	value := map[string]any{"status": status}
	if failure != nil {
		value["error"] = map[string]any{
			"type": failure.Type, "scope": failure.EffectiveScope(), "http_status": failure.HTTPStatus,
			"message": failure.Message,
		}
	}
	if ex != nil {
		value["content"] = debugBlocks(ex.output)
		value["finish_reason"] = ex.finishReason
		value["usage"] = ex.usage
		value["upstream_usage"] = ex.upstreamUsage
		value["native_response_id"] = ex.nativeResponseID
		value["native_conversation_id"] = ex.nativeConversationID
		if len(ex.responseWarnings) > 0 {
			value["warnings"] = ex.responseWarnings
		}
	}
	return marshalDebugValue(value)
}

func debugBlocks(blocks []domain.Block) []any {
	out := make([]any, 0, len(blocks))
	for _, block := range blocks {
		item := map[string]any{"type": block.Kind}
		if block.Text != "" {
			item["text"] = block.Text
		}
		if block.ID != "" {
			item["id"] = block.ID
		}
		if block.Name != "" {
			item["name"] = block.Name
		}
		if len(block.Input) > 0 {
			var input any
			if json.Unmarshal(block.Input, &input) == nil {
				item["input"] = redactDebugValue(input)
			} else {
				item["input"] = "[invalid JSON omitted]"
			}
		}
		if len(block.ImageData) > 0 {
			item["image"] = map[string]any{"mime_type": block.ImageMIME, "bytes": len(block.ImageData), "data": "[binary omitted]"}
		}
		out = append(out, item)
	}
	return out
}

func redactDebugValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "authorization") || strings.Contains(lower, "api_key") ||
				strings.Contains(lower, "apikey") || strings.Contains(lower, "secret") ||
				strings.Contains(lower, "credential") || strings.Contains(lower, "cookie") ||
				(lower == "token" || strings.HasSuffix(lower, "_token")) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactDebugValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index := range typed {
			out[index] = redactDebugValue(typed[index])
		}
		return out
	default:
		return value
	}
}

func marshalDebugValue(value any) (string, bool) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		payload = []byte(`{"error":"debug payload could not be encoded"}`)
	}
	if len(payload) <= requestDebugLimit {
		return string(payload), false
	}
	wrapper, _ := json.MarshalIndent(map[string]any{
		"truncated": true, "original_bytes": len(payload),
		"preview": string(payload[:requestDebugLimit]),
	}, "", "  ")
	return string(wrapper), true
}

// recordRecovery appends one recovery transition to the trace.
func (d *decision) recordRecovery(ex *execution, afterAttempt int, f *domain.Failure, action string, next nav) {
	if f == nil {
		return
	}
	ev := domain.RecoveryEvent{AfterAttempt: afterAttempt, FailureType: f.Type, FailureScope: f.EffectiveScope(), Action: action}
	if id, ok := d.targetAt(next); ok {
		ev.NextTarget = id
	}
	d.recoveryEvents = append(d.recoveryEvents, ev)
	var typ string
	switch {
	case strings.Contains(action, "connection_rebind"):
		typ = "CONNECTION_REBOUND"
	case strings.Contains(action, "route_fallback"), strings.Contains(action, "next_route_step"), strings.Contains(action, "next_tier"):
		d.addTrace(domain.TraceRecovery, "PROVIDER_EXHAUSTED", nil, ex, next, action)
		typ = "ROUTE_FALLBACK"
	case strings.Contains(action, "provider_exhausted"):
		d.addTrace(domain.TraceRecovery, "PROVIDER_EXHAUSTED", nil, ex, next, action)
		typ = "RECOVERY_STOPPED"
	case strings.HasPrefix(action, "abort:"):
		typ = "RECOVERY_STOPPED"
	}
	if typ != "" {
		var target *domain.CompiledTarget
		if id, ok := d.targetAt(next); ok {
			target = d.w.snap.Targets[id]
		}
		d.addTrace(domain.TraceRecovery, typ, target, ex, next, action)
		last := &d.traceEvents[len(d.traceEvents)-1]
		last.FailureType = f.Type
		last.FailureScope = f.EffectiveScope()
	}
}

func outcomeForExecution(attempts int, continuity domain.ContinuityMode) domain.RequestOutcome {
	if attempts > 1 || continuity == domain.ContinuityRehydrated {
		return domain.OutcomeRecoveredSuccess
	}
	return domain.OutcomeDirectSuccess
}

func (d *decision) lastFailureOf(ex *execution) *domain.Failure {
	if len(ex.failures) == 0 {
		return failf(domain.FailConnect, "no target attempted")
	}
	return ex.failures[len(ex.failures)-1]
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(b[:])
}
