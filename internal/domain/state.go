package domain

import (
	"fmt"
	"time"
)

// ResourceStateSnapshot is the runtime state read atomically at request time
// (PRD §12.2 "读取 Resource State Snapshot"). Each field carries Source /
// AsOf / Confidence per PRD §F-P1-02.
type Confidence string

const (
	ConfHigh   Confidence = "high"
	ConfMedium Confidence = "medium"
	ConfLow    Confidence = "low"
)

// BreakerState is the circuit breaker state (F-P0-18).
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// TargetState is the runtime state of one resource target.
type TargetState struct {
	// Health
	Breaker       BreakerState `json:"breaker"`
	OpenUntil     time.Time    `json:"open_until"` // breaker auto-recovery probe time
	ConsecFails   int          `json:"consec_fails"`
	ErrorRate1m   float64      `json:"error_rate_1m"`
	LatencyEWMAms float64      `json:"latency_ewma_ms"` // exponential weighted moving average of total latency
	TTFT_EWMAms   float64      `json:"ttft_ewma_ms"`
	InFlight      int64        `json:"in_flight"`
	LastFailure   *Failure     `json:"last_failure,omitempty"`

	// Quota (locally accounted; Source/AsOf/Confidence attached)
	QuotaUsed    float64    `json:"quota_used"`  // in bucket units
	QuotaLimit   float64    `json:"quota_limit"` // 0 = unlimited/unknown
	QuotaResetAt time.Time  `json:"quota_reset_at"`
	QuotaAsOf    time.Time  `json:"quota_as_of"`
	QuotaSource  string     `json:"quota_source"` // "local_ledger" | "manual" | "provider_api"
	QuotaConf    Confidence `json:"quota_conf"`

	// Cooldown (e.g. after 429 with Retry-After)
	CooldownUntil time.Time `json:"cooldown_until"`

	// Affinity (F-P1-08): session pinning, set externally
	AffinitySession string `json:"affinity_session"`
}

// QuotaRemainingFrac returns remaining fraction of quota, or 1 if unlimited.
func (s *TargetState) QuotaRemainingFrac() float64 {
	if s == nil || s.QuotaLimit <= 0 {
		return 1
	}
	rem := 1 - s.QuotaUsed/s.QuotaLimit
	if rem < 0 {
		rem = 0
	}
	return rem
}

// QuotaExhausted reports whether the locally-known quota is used up.
// Unknown quota (limit 0) is never "exhausted" — confidence handles that.
func (s *TargetState) QuotaExhausted() bool {
	if s == nil || s.QuotaLimit <= 0 {
		return false
	}
	return s.QuotaUsed >= s.QuotaLimit
}

// AvailableAt returns the earliest time this target could accept work.
func (s *TargetState) AvailableAt(now time.Time) time.Time {
	var t time.Time
	if s != nil {
		if s.CooldownUntil.After(now) {
			t = s.CooldownUntil
		}
		if s.Breaker == BreakerOpen && s.OpenUntil.After(now) && s.OpenUntil.After(t) {
			t = s.OpenUntil
		}
	}
	return t
}

// Snapshot is a point-in-time immutable view of all target states plus clock.
// Routing determinism (G2) is defined over (RequestContract, PolicySnapshot,
// Snapshot).
type Snapshot struct {
	Now     time.Time
	Targets map[ResourceTargetID]*TargetState
}

// NewSnapshot captures a consistent view.
func NewSnapshot(now time.Time, states map[ResourceTargetID]*TargetState) *Snapshot {
	cp := make(map[ResourceTargetID]*TargetState, len(states))
	for id, st := range states {
		cp2 := *st
		cp[id] = &cp2
	}
	return &Snapshot{Now: now, Targets: cp}
}

// Get returns the state for a target; missing targets read as fresh/healthy.
func (s *Snapshot) Get(id ResourceTargetID) *TargetState {
	if st, ok := s.Targets[id]; ok && st != nil {
		return st
	}
	return &TargetState{QuotaConf: ConfHigh}
}

// ---------------------------------------------------------------------------
// Cost computation (PRD §15)

// RateCard is one versioned price entry (F-P1-01).
type RateCard struct {
	Version        string
	Provider       string
	Model          string
	Plan           PlanKind
	InputPerM      float64 // USD per 1M input tokens
	OutputPerM     float64
	CacheReadPerM  float64
	CacheWritePerM float64
}

// Cost computes estimated cash cost for a usage under this card. Cached
// tokens are priced at their discounted rate only: subset dialects (OpenAI)
// have the cached portion subtracted from the input total first, so it is
// never billed at both InputPerM and the cache rate.
func (r *RateCard) Cost(u Usage) float64 {
	if r == nil {
		return 0
	}
	c := float64(u.UncachedInput())/1e6*r.InputPerM +
		float64(u.OutputTokens)/1e6*r.OutputPerM +
		float64(u.CacheReadTokens)/1e6*r.CacheReadPerM +
		float64(u.CacheWriteTokens)/1e6*r.CacheWritePerM
	return c
}

// USDPerCredit is the cash value of one OpenAI Codex-style plan credit
// (40/1000 USD, i.e. 25 credits per USD).
const USDPerCredit = 40.0 / 1000.0

// QuotaCost converts a usage into quota units for a bucket.
// prompts: one request = 1 unit × multiplier (GLM-style).
// tokens:  total tokens × multiplier / 1M... kept simple: raw tokens × mult.
// usd:     cash cost × multiplier.
// credits: cash cost converted at USDPerCredit (Codex-style plans).
func QuotaCost(unit QuotaUnit, u Usage, mult float64, isRequest bool, cash float64) float64 {
	switch unit {
	case QuotaUnitPrompts:
		if !isRequest {
			return 0
		}
		return mult
	case QuotaUnitTokens:
		return float64(u.BillableTokens()) * mult
	case QuotaUnitUSD:
		return cash * mult
	case QuotaUnitCredits:
		return cash / USDPerCredit * mult
	default:
		return float64(u.BillableTokens()) * mult
	}
}

// QuotaUsage is the consumption of one stacked quota bucket for an attempt.
type QuotaUsage struct {
	BucketID string
	Units    float64
}

// ErrNoCandidate is returned when no resource can serve the request.
type ErrNoCandidate struct {
	Alias    string
	Reason   string // human explanation for the client error
	Excluded []ExclusionRecord
}

func (e *ErrNoCandidate) Error() string {
	return fmt.Sprintf("no eligible resource for alias %q: %s", e.Alias, e.Reason)
}

// ErrClientGone signals the downstream client disconnected; stop work.
type ErrClientGone struct{ Wrapped error }

func (e *ErrClientGone) Error() string { return fmt.Sprintf("client disconnected: %v", e.Wrapped) }
func (e *ErrClientGone) Unwrap() error { return e.Wrapped }
