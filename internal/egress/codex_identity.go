package egress

import (
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- SHA-1 is mandated by the UUIDv5 derivation below and is not used for passwords or signatures.
	"encoding/hex"
	"encoding/json"
	"strings"

	"ferry/internal/domain"
)

// Codex identity isolation.
//
// Several OAuth accounts commonly sit behind one gateway, and the Codex
// backend correlates traffic by the session, thread and prompt-cache
// identifiers the client supplies. Forwarding one client's identifiers
// verbatim across accounts links those accounts to each other, which is how a
// shared gateway turns one account's problem into a pool-wide ban.
//
// Stable conversation and prompt-cache identifiers are therefore replaced by
// values derived from the pair (account, original value), while the per-call
// request identifier stays fresh. A conversation and cache bucket remain
// stable on the account serving them, without correlating two accounts.
const codexIdentitySalt = "smartapi:codex:identity:"

// codexIdentityNamespace is the RFC 4122 OID namespace, matching what the
// Codex ecosystem uses for derived identifiers.
var codexIdentityNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// codexIdentityValue derives a stable UUIDv5 for one (account, kind, value)
// triple. Never random: the same conversation on the same account must keep
// the same identity across requests and across process restarts.
func codexIdentityValue(account, kind, value string) string {
	sum := sha1.Sum(append(codexIdentityNamespace[:], // #nosec G401 -- RFC 4122 UUIDv5 mandates SHA-1; the result is a public conversation identifier, not a secret or authenticator.
		[]byte(codexIdentitySalt+kind+":"+account+":"+value)...))
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50 // version 5
	id[8] = (id[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(id)
}

func formatUUID(id [16]byte) string {
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], id[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], id[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], id[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], id[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], id[10:16])
	return string(buf)
}

func randomUUID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		// Identity headers are best-effort; a derived-from-nothing session is
		// still better than omitting the header the real client always sends.
		return codexIdentityValue("", "session-fallback", "")
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return formatUUID(id)
}

// codexSessionKey returns the account-scoped session identity for this
// request. Codex clients use prompt_cache_key as the stable thread anchor even
// when the gateway creates a fresh internal conversation for every Responses
// call, so it must take precedence over scheduler-owned provider state. The
// remaining conversation handles preserve continuity for clients without an
// explicit cache key; a genuinely one-shot request gets a fresh identifier.
func codexSessionKey(rc *domain.RequestContract, account string) string {
	source := ""
	if rc != nil {
		if rc.Lineage != nil {
			source = rc.Lineage.PromptCacheKey
		}
		if source == "" {
			source = rc.ProviderSessionKey
		}
		if source == "" && rc.Lineage != nil {
			source = firstNonEmpty(rc.Lineage.ConversationID, rc.Lineage.PreviousResponseID)
		}
	}
	if source == "" {
		return randomUUID()
	}
	return codexIdentityValue(account, "session", source)
}

// codexPromptCacheKey returns the account-scoped cache bucket for this
// request. Cache affinity is deliberately independent from conversation
// continuity: a Responses client may reuse one prompt_cache_key across many
// one-shot requests, while the scheduler still gives every request its own
// internal conversation for recovery and native-state bookkeeping.
func codexPromptCacheKey(rc *domain.RequestContract, account, sessionKey string) string {
	if rc != nil && rc.Lineage != nil && rc.Lineage.PromptCacheKey != "" {
		return codexIdentityValue(account, "prompt-cache", rc.Lineage.PromptCacheKey)
	}
	return sessionKey
}

// codexIdentityHeaders adds the session envelope the Codex client always
// sends. Without it the traffic is trivially distinguishable from a real
// client, and with the client's own values it would be cross-account
// correlatable; the derived values avoid both.
func codexIdentityHeaders(sessionKey string, base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+4)
	for key, value := range base {
		out[key] = value
	}
	if sessionKey == "" {
		return out
	}
	out["Session-Id"] = sessionKey
	out["Thread-Id"] = sessionKey
	// OpenAI defines this as a caller-generated identifier for one request,
	// not a thread/session handle. Reusing the session identity here makes
	// independent calls indistinguishable in upstream support traces.
	out["X-Client-Request-Id"] = randomUUID()
	out["X-Codex-Window-Id"] = sessionKey + ":0"
	return out
}

func codexRequestHeaders(rc *domain.RequestContract, sessionKey string) map[string]string {
	headers := protocolHeaders(rc, domain.ProtocolOpenAIResponses, map[string]string{"Accept": "text/event-stream"})
	if rc != nil && rc.Origin == domain.ProtocolOpenAIResponses && rc.CodexTurnStateAllowed {
		for name, value := range rc.ProtocolHeaders {
			if strings.EqualFold(name, "X-Codex-Turn-State") {
				headers["X-Codex-Turn-State"] = value
			}
		}
	}
	return codexIdentityHeaders(sessionKey, headers)
}

// applyCodexIdentityPayload rewrites the identity fields carried inside the
// request body so they agree with the headers. A client-supplied
// prompt_cache_key that leaked across accounts would defeat the header work
// on its own.
func applyCodexIdentityPayload(payload []byte, account, sessionKey, promptCacheKey string) []byte {
	if len(payload) == 0 || sessionKey == "" {
		return payload
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	changed := false
	if _, ok := body["prompt_cache_key"]; ok {
		if encoded, err := json.Marshal(promptCacheKey); err == nil {
			body["prompt_cache_key"] = encoded
			changed = true
		}
	}
	if metadata, ok := body["client_metadata"]; ok {
		if rewritten, ok := rewriteCodexClientMetadata(metadata, account, sessionKey, promptCacheKey); ok {
			body["client_metadata"] = rewritten
			changed = true
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

func rewriteCodexClientMetadata(raw json.RawMessage, account, sessionKey, promptCacheKey string) (json.RawMessage, bool) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, false
	}
	changed := false
	for key, derivation := range map[string]string{
		"x-codex-installation-id": "installation",
		"x-codex-turn-id":         "turn",
	} {
		var value string
		if raw, ok := metadata[key]; ok && json.Unmarshal(raw, &value) == nil && value != "" {
			if encoded, err := json.Marshal(codexIdentityValue(account, derivation, value)); err == nil {
				metadata[key] = encoded
				changed = true
			}
		}
	}
	if _, ok := metadata["x-codex-window-id"]; ok {
		if encoded, err := json.Marshal(sessionKey + ":0"); err == nil {
			metadata["x-codex-window-id"] = encoded
			changed = true
		}
	}
	if raw, ok := metadata["x-codex-turn-metadata"]; ok {
		if rewritten, ok := rewriteCodexTurnMetadata(raw, account, sessionKey, promptCacheKey); ok {
			metadata["x-codex-turn-metadata"] = rewritten
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(metadata)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rewriteCodexTurnMetadata handles the turn blob, which the client sends as a
// JSON string containing JSON.
func rewriteCodexTurnMetadata(raw json.RawMessage, account, sessionKey, promptCacheKey string) (json.RawMessage, bool) {
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil || strings.TrimSpace(encoded) == "" {
		return nil, false
	}
	var turn map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &turn); err != nil {
		return nil, false
	}
	changed := false
	if _, ok := turn["prompt_cache_key"]; ok {
		if value, err := json.Marshal(promptCacheKey); err == nil {
			turn["prompt_cache_key"] = value
			changed = true
		}
	}
	if _, ok := turn["window_id"]; ok {
		if value, err := json.Marshal(sessionKey + ":0"); err == nil {
			turn["window_id"] = value
			changed = true
		}
	}
	var turnID string
	if raw, ok := turn["turn_id"]; ok && json.Unmarshal(raw, &turnID) == nil && turnID != "" {
		if value, err := json.Marshal(codexIdentityValue(account, "turn", turnID)); err == nil {
			turn["turn_id"] = value
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	inner, err := json.Marshal(turn)
	if err != nil {
		return nil, false
	}
	out, err := json.Marshal(string(inner))
	if err != nil {
		return nil, false
	}
	return out, true
}
