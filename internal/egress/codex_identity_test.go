package egress

import (
	"encoding/json"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func TestCodexIdentityIsStablePerAccountAndDistinctAcrossAccounts(t *testing.T) {
	rc := &domain.RequestContract{Lineage: &domain.ResponsesLineage{PromptCacheKey: "client-key"}}

	first := codexSessionKey(rc, "acct-a")
	if first != codexSessionKey(rc, "acct-a") {
		t.Fatal("session identity is not stable for one account")
	}
	second := codexSessionKey(rc, "acct-b")
	if second == first {
		t.Fatal("two accounts derived the same session identity from one client key")
	}
	if strings.Contains(first, "client-key") || strings.Contains(second, "client-key") {
		t.Fatalf("client key leaked into the derived identity: %s %s", first, second)
	}
	if len(first) != 36 {
		t.Fatalf("derived identity is not a UUID: %q", first)
	}
	if first[14] != '5' {
		t.Fatalf("derived identity is not UUIDv5: %q", first)
	}
}

func TestCodexSessionKeyFallsBackToConversationThenRandom(t *testing.T) {
	providerSession := &domain.RequestContract{
		ProviderSessionKey: "conv-smart-1",
		Lineage:            &domain.ResponsesLineage{ConversationID: "client-conv-1"},
	}
	if codexSessionKey(providerSession, "acct") != codexSessionKey(providerSession, "acct") {
		t.Fatal("scheduler-owned provider session identity is not stable")
	}
	if codexSessionKey(providerSession, "acct") ==
		codexSessionKey(&domain.RequestContract{Lineage: providerSession.Lineage}, "acct") {
		t.Fatal("provider session identity did not take precedence over client lineage")
	}

	conversation := &domain.RequestContract{Lineage: &domain.ResponsesLineage{ConversationID: "conv-1"}}
	if codexSessionKey(conversation, "acct") != codexSessionKey(conversation, "acct") {
		t.Fatal("conversation-derived identity is not stable")
	}
	// Nothing stable to anchor to: a fresh session per request is correct, and
	// must not collapse to a shared constant.
	oneShot := &domain.RequestContract{}
	if codexSessionKey(oneShot, "acct") == codexSessionKey(oneShot, "acct") {
		t.Fatal("one-shot requests reused a single session identity")
	}
}

func TestCodexPromptCacheKeyAnchorsSessionAcrossProviderConversations(t *testing.T) {
	first := &domain.RequestContract{
		ProviderSessionKey: "a",
		Lineage:            &domain.ResponsesLineage{PromptCacheKey: "shared-client-cache"},
	}
	second := &domain.RequestContract{
		ProviderSessionKey: "b",
		Lineage:            &domain.ResponsesLineage{PromptCacheKey: "shared-client-cache"},
	}
	firstSession := codexSessionKey(first, "acct-a")
	secondSession := codexSessionKey(second, "acct-a")
	if firstSession != secondSession {
		t.Fatal("stable client prompt cache key was fragmented by internal conversations")
	}
	firstCache := codexPromptCacheKey(first, "acct-a", firstSession)
	secondCache := codexPromptCacheKey(second, "acct-a", secondSession)
	if firstCache != secondCache {
		t.Fatal("stable client prompt cache key fragmented by internal conversations")
	}
	if firstCache == codexPromptCacheKey(first, "acct-b", firstSession) {
		t.Fatal("prompt cache identity was reused across OAuth accounts")
	}
	if firstSession == codexSessionKey(first, "acct-b") {
		t.Fatal("session identity was reused across OAuth accounts")
	}
	legacyChat := &domain.RequestContract{ProviderSessionKey: "hermes-conversation"}
	legacySession := codexSessionKey(legacyChat, "acct-a")
	if codexPromptCacheKey(legacyChat, "acct-a", legacySession) != legacySession {
		t.Fatal("request without an explicit prompt cache key stopped using its stable session fallback")
	}
}

func TestCodexIdentityHeadersCarryTheClientEnvelope(t *testing.T) {
	headers := codexIdentityHeaders("session-value", map[string]string{"Accept": "text/event-stream"})
	for _, key := range []string{"Session-Id", "Thread-Id"} {
		if headers[key] != "session-value" {
			t.Fatalf("header %s = %q", key, headers[key])
		}
	}
	if headers["X-Client-Request-Id"] == "" || headers["X-Client-Request-Id"] == "session-value" {
		t.Fatalf("request id reused the session identity: %q", headers["X-Client-Request-Id"])
	}
	if headers["X-Client-Request-Id"] == codexIdentityHeaders("session-value", nil)["X-Client-Request-Id"] {
		t.Fatal("request id was reused across calls")
	}
	if headers["X-Codex-Window-Id"] != "session-value:0" {
		t.Fatalf("window id = %q", headers["X-Codex-Window-Id"])
	}
	if headers["Accept"] != "text/event-stream" {
		t.Fatal("base headers were dropped")
	}
	if len(codexIdentityHeaders("", map[string]string{"Accept": "x"})) != 1 {
		t.Fatal("empty session key still emitted identity headers")
	}
}

func TestCodexIdentityPayloadRewritesClientIdentifiers(t *testing.T) {
	turn, err := json.Marshal(`{"prompt_cache_key":"client-key","turn_id":"turn-1","window_id":"client-key:0"}`)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"model":"gpt-5.5","prompt_cache_key":"client-key","client_metadata":{` +
		`"x-codex-installation-id":"install-1","x-codex-window-id":"client-key:0",` +
		`"x-codex-turn-metadata":` + string(turn) + `}}`)

	out := applyCodexIdentityPayload(payload, "acct-a", "derived-session", "derived-cache")
	if strings.Contains(string(out), "client-key") {
		t.Fatalf("client prompt cache key survived: %s", out)
	}
	if strings.Contains(string(out), "install-1") || strings.Contains(string(out), "turn-1") {
		t.Fatalf("client install/turn identity survived: %s", out)
	}

	var body struct {
		Model          string `json:"model"`
		PromptCacheKey string `json:"prompt_cache_key"`
		ClientMetadata struct {
			InstallationID string `json:"x-codex-installation-id"`
			WindowID       string `json:"x-codex-window-id"`
			TurnMetadata   string `json:"x-codex-turn-metadata"`
		} `json:"client_metadata"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("rewritten payload is not valid JSON: %v", err)
	}
	if body.Model != "gpt-5.5" {
		t.Fatalf("unrelated fields were disturbed: %+v", body)
	}
	if body.PromptCacheKey != "derived-cache" || body.ClientMetadata.WindowID != "derived-session:0" {
		t.Fatalf("identity fields not rewritten: %+v", body)
	}
	var turnBody struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		TurnID         string `json:"turn_id"`
		WindowID       string `json:"window_id"`
	}
	if err := json.Unmarshal([]byte(body.ClientMetadata.TurnMetadata), &turnBody); err != nil {
		t.Fatalf("turn metadata is no longer JSON-in-JSON: %v", err)
	}
	if turnBody.PromptCacheKey != "derived-cache" || turnBody.WindowID != "derived-session:0" {
		t.Fatalf("turn metadata not rewritten: %+v", turnBody)
	}
	if turnBody.TurnID == "turn-1" || turnBody.TurnID == "" {
		t.Fatalf("turn id not derived: %q", turnBody.TurnID)
	}
}

func TestCodexIdentityPayloadLeavesUnrelatedBodiesAlone(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.5","input":[]}`)
	if got := applyCodexIdentityPayload(payload, "acct", "session", "cache"); string(got) != string(payload) {
		t.Fatalf("payload without identity fields was rewritten: %s", got)
	}
	if got := applyCodexIdentityPayload([]byte("not-json"), "acct", "session", "cache"); string(got) != "not-json" {
		t.Fatal("non-JSON payload was not passed through untouched")
	}
}

func TestCodexConfiguredVersionPinsTextAndImageHeaders(t *testing.T) {
	b := Base{CodexClientVersion: "0.154.0", ExtraHeaders: map[string]string{"User-Agent": "stale"}}
	textCaller := NewCodexCaller(b, "account")
	for _, headers := range []map[string]string{textCaller.headers(), NewCodexImages(b, "account").ExtraHeaders} {
		if headers["Version"] != "0.154.0" || headers["User-Agent"] != domain.CodexUserAgentForVersion("0.154.0") {
			t.Fatalf("version envelope=%v", headers)
		}
	}
}

func TestCodexIdentitySurvivesFerryRename(t *testing.T) {
	if got := codexSessionKey(&domain.RequestContract{Lineage: &domain.ResponsesLineage{PromptCacheKey: "client-key"}}, "acct-a"); got != "f68b2407-bb93-5d82-b9ce-94b1ef961e9a" {
		t.Fatal("existing conversation identity changed", got)
	}
}
