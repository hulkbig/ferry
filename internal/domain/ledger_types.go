package domain

import "time"

// RequestDebugRecord contains the optional, redacted wire-level payloads
// captured for one request. Capture is opt-in and applies only to requests
// that begin while the runtime setting is enabled.
type RequestDebugRecord struct {
	RequestID         string `json:"request_id"`
	RequestJSON       string `json:"request_json,omitempty"`
	ResponseJSON      string `json:"response_json,omitempty"`
	RequestTruncated  bool   `json:"request_truncated"`
	ResponseTruncated bool   `json:"response_truncated"`
}

// RequestStatus is the terminal state of a logical request or attempt.
type RequestStatus string

const (
	ReqSuccess  RequestStatus = "success"
	ReqFailed   RequestStatus = "failed"
	ReqCanceled RequestStatus = "canceled"
)

// AttemptRecord is one real upstream call (F-P0-20). Immutable once written.
type AttemptRecord struct {
	RequestID           string           `json:"request_id"`
	Seq                 int              `json:"seq"`
	Ts                  time.Time        `json:"ts"`
	TargetID            ResourceTargetID `json:"target_id"`
	ProviderID          string           `json:"provider_id,omitempty"`
	ConnectionID        string           `json:"connection_id,omitempty"`
	EndpointURL         string           `json:"endpoint_url"`
	Protocol            Protocol         `json:"protocol"`
	CredentialRef       string           `json:"credential_ref"` // secret reference text, never the secret
	Model               string           `json:"model"`
	ServiceTier         string           `json:"service_tier,omitempty"`
	ResponseModel       string           `json:"response_model,omitempty"`
	ResponseServiceTier string           `json:"response_service_tier,omitempty"`
	PlanKind            PlanKind         `json:"plan_kind"`

	Status     RequestStatus `json:"status"`
	Failure    *Failure      `json:"failure,omitempty"`
	LatencyMS  int64         `json:"latency_ms"`
	TTFTMS     int64         `json:"ttft_ms"`
	Usage      Usage         `json:"usage"`
	Multiplier float64       `json:"multiplier"` // quota consumption multiplier applied

	// Cost dimensions (PRD §15.2)
	EstCashCost      float64 `json:"est_cash_cost"`
	QuotaUnits       float64 `json:"quota_units"`
	AllocatedSubCost float64 `json:"allocated_sub_cost"`
	ShadowCost       float64 `json:"shadow_cost"`

	// Rule versions active at attempt time (PRD §15.3)
	RateCardVersion       string `json:"rate_card_version"`
	QuotaRuleVersion      string `json:"quota_rule_version"`
	MultiplierRuleVersion string `json:"multiplier_rule_version"`

	ServiceTierEvidence *ServiceTierEvidence `json:"service_tier_evidence,omitempty"`
}

// ExclusionRecord explains why a candidate never became eligible.
type ExclusionRecord struct {
	Target ResourceTargetID `json:"target"`
	Tier   string           `json:"tier,omitempty"`
	Reason string           `json:"reason"`
}

// CandidateRecord is one candidate's evaluation outcome in the trace.
type CandidateRecord struct {
	Target                      ResourceTargetID   `json:"target"`
	Tier                        string             `json:"tier"`
	Eligible                    bool               `json:"eligible"`
	ExclusionReason             string             `json:"exclusion_reason,omitempty"`
	Rank                        int                `json:"rank"` // 1-based within tier; 0 if excluded
	Scores                      map[string]float64 `json:"scores,omitempty"`
	CapabilityStatuses          CapabilityStatuses `json:"capability_status,omitempty"`
	CapabilityVerificationError string             `json:"capability_verification_error,omitempty"`
	CapabilityProbeRequests     int                `json:"capability_probe_requests,omitempty"`
}

// RecoveryEvent documents one typed recovery transition.
type RecoveryEvent struct {
	AfterAttempt int              `json:"after_attempt"`
	FailureType  FailureType      `json:"failure_type"`
	FailureScope FailureScope     `json:"failure_scope"`
	Action       string           `json:"action"` // next_target_same_tier | next_tier | abort
	NextTarget   ResourceTargetID `json:"next_target,omitempty"`
}

// TraceCategory is a stable product-level lane in one request's execution
// timeline. It deliberately does not expose the legacy policy graph model.
type TraceCategory string

const (
	TraceRoute      TraceCategory = "route"
	TraceRecovery   TraceCategory = "recovery"
	TraceContinuity TraceCategory = "continuity"
	TraceOutput     TraceCategory = "output"
)

// ExecutionTraceEvent is one ordered, client-comprehensible runtime fact.
// Provider/Connection fields are public V2 resource identities; the private
// ResourceTarget leaf is intentionally absent.
type ExecutionTraceEvent struct {
	At             time.Time      `json:"at"`
	Category       TraceCategory  `json:"category"`
	Type           string         `json:"type"`
	Attempt        int            `json:"attempt,omitempty"`
	RouteStep      int            `json:"route_step,omitempty"`
	ProviderID     string         `json:"provider_id,omitempty"`
	ConnectionID   string         `json:"connection_id,omitempty"`
	UpstreamModel  string         `json:"upstream_model,omitempty"`
	Protocol       Protocol       `json:"protocol,omitempty"`
	AffinityScope  AffinityScope  `json:"affinity_scope,omitempty"`
	AffinitySource string         `json:"affinity_source,omitempty"`
	FailureType    FailureType    `json:"failure_type,omitempty"`
	FailureScope   FailureScope   `json:"failure_scope,omitempty"`
	ContinuityMode ContinuityMode `json:"continuity_mode,omitempty"`
	Message        string         `json:"message,omitempty"`
}

// TraceRecord is the full Decision Trace (F-P0-21, PRD §14).
type TraceRecord struct {
	PolicyName            string                `json:"policy_name"`
	PolicyVersion         int64                 `json:"policy_version"`
	PolicyHash            string                `json:"policy_hash,omitempty"`
	PolicyIndexHash       string                `json:"policy_index_hash,omitempty"`
	PolicyCompileCacheHit bool                  `json:"policy_compile_cache_hit"`
	PolicySceneCacheHit   bool                  `json:"policy_scene_cache_hit"`
	Timezone              string                `json:"timezone,omitempty"`
	RequiredCaps          Requirements          `json:"required_caps"`
	Objective             string                `json:"objective"`
	Tiers                 []string              `json:"tiers"`
	Candidates            []CandidateRecord     `json:"candidates"`
	ExecutionOrder        []ResourceTargetID    `json:"execution_order"`
	Primary               ResourceTargetID      `json:"primary"`
	RecoveryEvents        []RecoveryEvent       `json:"recovery_events,omitempty"`
	LogicalModelID        string                `json:"logical_model_id,omitempty"`
	OverrideID            string                `json:"override_id,omitempty"`
	AffinityEvidence      []AffinityEvidence    `json:"affinity_evidence,omitempty"`
	AffinityBinding       *AffinityBinding      `json:"affinity_binding,omitempty"`
	AffinityReused        bool                  `json:"affinity_reused"`
	ConversationID        string                `json:"conversation_id,omitempty"`
	ContinuityMode        ContinuityMode        `json:"continuity_mode,omitempty"`
	Events                []ExecutionTraceEvent `json:"events,omitempty"`
}

// RequestRecord is one logical request (F-P0-19). Immutable once written.
type RequestRecord struct {
	ID                         string         `json:"id"`
	Ts                         time.Time      `json:"ts"`
	ConsumerKey                string         `json:"consumer_key"`
	APIKeySecretID             string         `json:"api_key_secret_id,omitempty"`
	Alias                      string         `json:"alias"`
	Objective                  string         `json:"objective"`
	PolicyName                 string         `json:"policy_name"`
	PolicyVersion              int64          `json:"policy_version"`
	PolicyHash                 string         `json:"policy_hash,omitempty"`
	PolicyIndexHash            string         `json:"policy_index_hash,omitempty"`
	PolicyCompileCacheHit      bool           `json:"policy_compile_cache_hit"`
	PolicySceneCacheHit        bool           `json:"policy_scene_cache_hit"`
	FallbackCount              int            `json:"fallback_count"`
	ConnectionRebindCount      int            `json:"connection_rebind_count"`
	RouteFallbackCount         int            `json:"route_fallback_count"`
	RouteAdvanceCount          int            `json:"route_advance_count"`
	RecoveryFallbackCount      int            `json:"recovery_fallback_count"`
	ConversationMigrationCount int            `json:"conversation_migration_count"`
	ServiceTier                string         `json:"service_tier,omitempty"`
	ResponseModel              string         `json:"response_model,omitempty"`
	ResponseServiceTier        string         `json:"response_service_tier,omitempty"`
	ModelRequested             string         `json:"model_requested"`
	ModelSelected              string         `json:"model_selected"`
	Protocol                   Protocol       `json:"protocol"`
	Stream                     bool           `json:"stream"`
	LogicalModelID             string         `json:"logical_model_id,omitempty"`
	ProviderID                 string         `json:"provider_id,omitempty"`
	ConnectionID               string         `json:"connection_id,omitempty"`
	UpstreamModel              string         `json:"upstream_model,omitempty"`
	RuntimeRevision            int64          `json:"runtime_revision"`
	RouteFingerprint           string         `json:"route_fingerprint,omitempty"`
	CommitAt                   time.Time      `json:"commit_at,omitempty"`
	ContinuityMode             ContinuityMode `json:"continuity_mode,omitempty"`

	Status      RequestStatus  `json:"status"`
	Outcome     RequestOutcome `json:"outcome"`
	FailureType FailureType    `json:"failure_type"`
	FailureMsg  string         `json:"failure_msg"`
	Attempts    int            `json:"attempts"`

	// UsageProtocol is derived from the final attempt when reading history.
	UsageProtocol Protocol `json:"usage_protocol,omitempty"`
	Usage         Usage    `json:"usage"`
	UpstreamUsage Usage    `json:"upstream_usage"`

	EstCashCost            float64 `json:"est_cash_cost"`
	QuotaUnits             float64 `json:"quota_units"`
	AllocatedSubCost       float64 `json:"allocated_sub_cost"`
	ShadowCost             float64 `json:"shadow_cost"`
	ServingCost            float64 `json:"serving_cost"`
	RecoveryOverheadCost   float64 `json:"recovery_overhead_cost"`
	RecoveryAddedLatencyMS int64   `json:"recovery_added_latency_ms"`

	SelectedTarget ResourceTargetID `json:"selected_target"`
	DurationMS     int64            `json:"duration_ms"`
	TTFTMS         int64            `json:"ttft_ms"`

	CompatLevel CompatLevel `json:"compat_level"`
	CompatNotes []string    `json:"compat_notes"`

	Exclusions []ExclusionRecord `json:"exclusions"`
	ClientIP   string            `json:"client_ip"`

	ServiceTierEvidence *ServiceTierEvidence `json:"service_tier_evidence,omitempty"`
}

// LogFilter is the query input for GET /v0/management/console/logs (F-P0-22).
type LogFilter struct {
	From                  time.Time
	To                    time.Time
	RequestID             string
	Search                string
	ConsumerKey           string
	ProviderID            string
	ParticipatingProvider string
	LogicalModel          string
	AttemptStatus         string
	AttemptFailureType    string
	ReportError           string
	ConnectionID          string
	ConversationID        string
	Outcome               RequestOutcome
	Alias                 string
	Objective             string
	Target                string // substring match on selected target
	Model                 string // substring match on selected model
	Status                RequestStatus
	FailureType           FailureType
	PolicyName            string
	PolicyVersion         int64
	RuntimeRevision       int64
	Limit                 int
	Offset                int
}
