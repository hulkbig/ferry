package domain

import (
	"fmt"
	"time"
)

// FailureType is the typed failure taxonomy (PRD §6.8, F-P0-16).
// Each type maps to a distinct recovery action; never a blanket retry.
type FailureType string

const (
	FailNone             FailureType = ""
	FailConnect          FailureType = "connect_failure"
	FailAuth             FailureType = "authentication_failure"
	FailRateLimit        FailureType = "rate_limit"      // 429
	FailProvider5xx      FailureType = "provider_5xx"    // upstream 5xx
	FailQuotaExhausted   FailureType = "quota_exhausted" // local bucket empty
	FailTTFTTimeout      FailureType = "ttft_timeout"    // no first byte in time
	FailUnsupportedParam FailureType = "unsupported_parameter"
	FailContextOverflow  FailureType = "context_overflow" // request too large
	FailStreamBroken     FailureType = "stream_broken"    // SSE died mid-stream
	// FailProtocolConversion means an attempt's request, response, or stream
	// cannot be represented across the selected protocol boundary without
	// dropping or fabricating data. It is recoverable only before stream commit.
	FailProtocolConversion FailureType = "protocol_conversion"
	FailBadResponse        FailureType = "bad_response" // unparseable / protocol violation
	FailClientCancel       FailureType = "client_cancel"
)

// FailureScope identifies the resource boundary affected by a failure. V2
// recovery combines type + scope + commit state + remaining budget; type alone
// is no longer sufficient to choose a recovery action.
type FailureScope string

const (
	FailureScopeRequest    FailureScope = "request"
	FailureScopeTarget     FailureScope = "target"
	FailureScopeConnection FailureScope = "connection"
	FailureScopeProvider   FailureScope = "provider"
	FailureScopeStream     FailureScope = "stream"
)

// RecoveryClass groups failure types that share the same recovery shape.
type RecoveryClass string

const (
	RecoverNextTargetSameTier RecoveryClass = "next_target_same_tier"
	RecoverNextTier           RecoveryClass = "next_tier"
	RecoverAbort              RecoveryClass = "abort" // do not retry anywhere
)

// RecoveryClass maps each failure to its default recovery class (F-P0-16).
func (f FailureType) RecoveryClass() RecoveryClass {
	switch f {
	case FailQuotaExhausted:
		return RecoverNextTier
	case FailAuth, FailContextOverflow, FailClientCancel, FailUnsupportedParam:
		return RecoverAbort
	case FailConnect, FailRateLimit, FailProvider5xx,
		FailTTFTTimeout, FailStreamBroken, FailProtocolConversion, FailBadResponse:
		return RecoverNextTargetSameTier
	default:
		return RecoverNextTargetSameTier
	}
}

// CircuitAction says how a failure should feed passive health.
type CircuitAction string

const (
	CircuitCountFailure CircuitAction = "failure" // counts toward breaker
	CircuitCountSuccess CircuitAction = "success"
	CircuitNeutral      CircuitAction = "neutral" // e.g. client cancel, quota
)

// CircuitAction classifies failures for health accounting.
func (f FailureType) CircuitAction() CircuitAction {
	switch f {
	case FailQuotaExhausted, FailClientCancel, FailContextOverflow,
		FailUnsupportedParam, FailProtocolConversion:
		return CircuitNeutral
	default:
		return CircuitCountFailure
	}
}

// Failure is a typed upstream failure with provider detail.
type Failure struct {
	// LocalQuotaProtection distinguishes a pre-send refusal from upstream 429s.
	LocalQuotaProtection bool          `json:"local_quota_protection,omitempty"`
	Type                 FailureType   `json:"type"`
	Scope                FailureScope  `json:"scope,omitempty"`
	HTTPStatus           int           `json:"http_status"`
	Message              string        `json:"message"`
	RetryAfter           time.Duration `json:"retry_after,omitempty"` // honored hint from 429 responses
}

func (f FailureType) Valid() bool {
	switch f {
	case FailConnect, FailAuth, FailRateLimit, FailProvider5xx, FailQuotaExhausted,
		FailTTFTTimeout, FailUnsupportedParam, FailContextOverflow, FailStreamBroken,
		FailProtocolConversion, FailBadResponse, FailClientCancel:
		return true
	default:
		return false
	}
}

// EffectiveScope supplies a conservative scope for legacy failure producers
// until every adapter can classify with provider-specific evidence.
func (f *Failure) EffectiveScope() FailureScope {
	if f == nil {
		return FailureScopeRequest
	}
	if f.Scope != "" {
		return f.Scope
	}
	switch f.Type {
	case FailUnsupportedParam, FailContextOverflow, FailClientCancel:
		return FailureScopeRequest
	case FailAuth, FailRateLimit, FailQuotaExhausted, FailConnect:
		return FailureScopeConnection
	case FailProvider5xx:
		return FailureScopeProvider
	case FailProtocolConversion:
		return FailureScopeTarget
	case FailStreamBroken, FailTTFTTimeout:
		return FailureScopeStream
	default:
		return FailureScopeTarget
	}
}

func (f *Failure) Error() string {
	if f == nil {
		return "<nil>"
	}
	if f.HTTPStatus != 0 {
		return fmt.Sprintf("%s (http %d): %s", f.Type, f.HTTPStatus, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.Type, f.Message)
}

// ClassifyHTTP maps an HTTP status + body hint to a FailureType.
// Body hints let providers that signal quota exhaustion via 429/403 bodies
// be distinguished from plain rate limits.
func ClassifyHTTP(status int, bodyHint string) FailureType {
	switch status {
	case 401, 403:
		return FailAuth
	case 404:
		// wrong path/model naming is a configuration-class failure;
		// treat as bad response so it surfaces loudly.
		return FailBadResponse
	case 429:
		if containsAny(bodyHint, "usage_limit_reached", "quota", "insufficient", "balance", "credit") {
			return FailQuotaExhausted
		}
		return FailRateLimit
	}
	if status == 0 || status == 400 || status == 422 {
		unsupported := containsAny(bodyHint,
			"not supported", "unsupported", "does not support", "only supports", "only support",
			"unknown parameter", "unrecognized parameter")
		// Several OpenAI-compatible providers express their fixed-temperature
		// contract as "temperature must be 1". Keep that recovery signal
		// narrow: matching "must be 1" by itself also catches unrelated input
		// validation such as "max_tokens must be 128".
		fixedTemperature := containsAny(bodyHint, "temperature") &&
			containsAny(bodyHint, "must be 1", "must equal 1", "only value of 1", "supports only 1")
		if unsupported || fixedTemperature {
			return FailUnsupportedParam
		}
	}
	if status >= 500 {
		return FailProvider5xx
	}
	if status >= 400 {
		// context overflow often arrives as 400 with a recognizable body
		if containsAny(bodyHint,
			"context length", "context_length", "maximum context",
			"too many tokens", "input tokens exceed", "prompt is too long") {
			return FailContextOverflow
		}
		if containsAny(bodyHint, "quota", "insufficient", "balance", "credit") {
			return FailQuotaExhausted
		}
		return FailBadResponse
	}
	return FailBadResponse
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && containsFold(s, sub) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	n := len(sub)
	if n == 0 || len(s) < n {
		return false
	}
	for i := 0; i+n <= len(s); i++ {
		if equalFoldByteRange(s[i:i+n], sub) {
			return true
		}
	}
	return false
}

func equalFoldByteRange(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
