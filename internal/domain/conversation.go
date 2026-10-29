package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type AffinityScope string

const (
	AffinityConversation AffinityScope = "CONVERSATION"
	AffinityCache        AffinityScope = "CACHE"
	AffinityCaller       AffinityScope = "CALLER"
)

type AffinityEvidence struct {
	Source     string        `json:"source"`
	Scope      AffinityScope `json:"scope"`
	KeyHash    string        `json:"key_hash"`
	Confidence Confidence    `json:"confidence"`
	ObservedAt time.Time     `json:"observed_at"`
}

// NewAffinityEvidence hashes client-supplied identity immediately; raw
// session/cache/caller values do not enter runtime caches or ledgers.
func NewAffinityEvidence(source string, scope AffinityScope, raw string, confidence Confidence) AffinityEvidence {
	sum := sha256.Sum256([]byte(string(scope) + "\x00" + source + "\x00" + raw))
	return AffinityEvidence{
		Source: source, Scope: scope, KeyHash: hex.EncodeToString(sum[:]),
		Confidence: confidence, ObservedAt: time.Now(),
	}
}

type AffinityBinding struct {
	Scope            AffinityScope `json:"scope"`
	KeyHash          string        `json:"key_hash"`
	LogicalModelID   string        `json:"logical_model_id"`
	RouteFingerprint string        `json:"route_fingerprint"`
	ProviderID       string        `json:"provider_id"`
	UpstreamModel    string        `json:"upstream_model"`
	ConnectionID     string        `json:"connection_id"`
	CreatedAt        time.Time     `json:"created_at"`
	LastSeenAt       time.Time     `json:"last_seen_at"`
	ExpiresAt        time.Time     `json:"expires_at"`
	Status           string        `json:"status"`
}

type ContinuityMode string

const (
	ContinuityNative     ContinuityMode = "NATIVE"
	ContinuityRehydrated ContinuityMode = "REHYDRATED"
	ContinuityNone       ContinuityMode = "NONE"
)

type ResponsesLineage struct {
	ConversationID     string `json:"conversation_id,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	PromptCacheKey     string `json:"prompt_cache_key,omitempty"`
}

type NativeContinuation struct {
	NativeConversationID string `json:"native_conversation_id,omitempty"`
	NativeResponseID     string `json:"native_response_id,omitempty"`
}

type Conversation struct {
	ID                   string         `json:"id"`
	LogicalModelID       string         `json:"logical_model_id"`
	Items                []Message      `json:"items"`
	CurrentProviderID    string         `json:"current_provider_id"`
	CurrentUpstreamModel string         `json:"current_upstream_model"`
	CurrentConnectionID  string         `json:"current_connection_id"`
	ContinuityMode       ContinuityMode `json:"continuity_mode"`
	CreatedAt            time.Time      `json:"created_at"`
	LastActiveAt         time.Time      `json:"last_active_at"`
	LastRouteFingerprint string         `json:"last_route_fingerprint"`
}

type NativeHandle struct {
	ConversationID       string    `json:"conversation_id"`
	ProviderID           string    `json:"provider_id"`
	ConnectionID         string    `json:"connection_id"`
	NativeConversationID string    `json:"native_conversation_id,omitempty"`
	NativeResponseID     string    `json:"native_response_id,omitempty"`
	StateScope           string    `json:"state_scope"`
	CreatedAt            time.Time `json:"created_at"`
	LastUsedAt           time.Time `json:"last_used_at"`
}

type SmartResponse struct {
	ID               string         `json:"id"`
	ConversationID   string         `json:"conversation_id"`
	ParentResponseID string         `json:"parent_response_id,omitempty"`
	RequestID        string         `json:"request_id"`
	CanonicalOutput  []Block        `json:"canonical_output"`
	ProviderID       string         `json:"provider_id"`
	UpstreamModel    string         `json:"upstream_model"`
	ConnectionID     string         `json:"connection_id"`
	NativeResponseID string         `json:"native_response_id,omitempty"`
	ContinuityMode   ContinuityMode `json:"continuity_mode"`
	CreatedAt        time.Time      `json:"created_at"`
}
