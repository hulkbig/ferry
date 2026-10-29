package domain

import (
	"crypto/sha256"
	"crypto/subtle"
	"time"
)

// APIKeySecretState is the lifecycle state of one credential belonging to an
// API-key identity. ACTIVE and GRACE authenticate; RETIRED never does.
type APIKeySecretState string

const (
	APIKeySecretActive  APIKeySecretState = "ACTIVE"
	APIKeySecretGrace   APIKeySecretState = "GRACE"
	APIKeySecretRetired APIKeySecretState = "RETIRED"
)

// APIKeyIdentity is the stable caller identity used by routing, affinity and
// accounting. Secret rotation never changes this ID.
type APIKeyIdentity struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Description      string     `json:"description,omitempty"`
	Enabled          bool       `json:"enabled"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	ScopeModels      []string   `json:"scope_models"`
	ScopeProtocols   []Protocol `json:"scope_protocols"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	RateLimitRPM     int        `json:"rate_limit_rpm,omitempty"`
	MonthlyBudgetUSD float64    `json:"monthly_budget_usd,omitempty"`
	// ExactOnly composes with the selected LogicalModel using strictest-wins.
	ExactOnly bool `json:"exact_only,omitempty"`
}

func (i APIKeyIdentity) Allows(model string, protocol Protocol, now time.Time) bool {
	if !i.Enabled || i.RevokedAt != nil || (i.ExpiresAt != nil && !now.Before(*i.ExpiresAt)) {
		return false
	}
	if len(i.ScopeModels) > 0 {
		allowed := false
		for _, item := range i.ScopeModels {
			if item == model {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if len(i.ScopeProtocols) > 0 {
		allowed := false
		for _, item := range i.ScopeProtocols {
			if item == protocol {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

// APIKeySecret contains safe metadata only. Revealable reports whether the
// active value can be fetched through the explicit administrator-only reveal
// endpoint; regular list/detail reads never include the raw value.
type APIKeySecret struct {
	ID         string            `json:"id"`
	APIKeyID   string            `json:"api_key_id"`
	Preview    string            `json:"preview"`
	Revealable bool              `json:"revealable"`
	State      APIKeySecretState `json:"state"`
	CreatedAt  time.Time         `json:"created_at"`
	RetiredAt  *time.Time        `json:"retired_at,omitempty"`
	LastUsedAt *time.Time        `json:"last_used_at,omitempty"`
}

// APIKeyAuthenticator is the immutable, hash-only data-plane representation
// of an API key identity. SecretHashes contains only ACTIVE/GRACE credentials.
type APIKeyAuthenticator struct {
	Identity     APIKeyIdentity
	SecretHashes [][sha256.Size]byte
	// SecretIDs is aligned with SecretHashes for durable usage attribution.
	// It contains opaque metadata IDs only, never raw key material.
	SecretIDs []string
	// Legacy fields preserve existing Consumer behavior during migration. They
	// are not part of APIKeyIdentity and must never enter the V2 API contract.
	LegacyDefaultModel     string `json:"-"`
	LegacyDefaultObjective string `json:"-"`
}

// HashAPIKeySecret hashes high-entropy API key material for storage and
// constant-time authentication. Ferry-generated keys contain 192 random
// bits, so a direct SHA-256 verifier does not expose a feasible offline search.
func HashAPIKeySecret(secret string) [sha256.Size]byte {
	return sha256.Sum256([]byte(secret))
}

// Matches reports whether the presented raw secret belongs to this identity.
// It intentionally evaluates every hash so match position is not observable.
func (a APIKeyAuthenticator) Matches(presented string) bool {
	_, matched := a.MatchSecret(presented)
	return matched
}

// MatchSecret authenticates the presented value while evaluating every
// verifier, and returns the matched durable secret metadata ID when known.
func (a APIKeyAuthenticator) MatchSecret(presented string) (string, bool) {
	presentedHash := HashAPIKeySecret(presented)
	matched := 0
	matchedID := ""
	for i := range a.SecretHashes {
		equal := subtle.ConstantTimeCompare(presentedHash[:], a.SecretHashes[i][:])
		matched |= equal
		if equal == 1 && i < len(a.SecretIDs) {
			matchedID = a.SecretIDs[i]
		}
	}
	return matchedID, matched == 1
}
