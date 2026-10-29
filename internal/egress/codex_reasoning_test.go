package egress

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func resetReasoningReplay() {
	reasoningReplayMu.Lock()
	reasoningReplayEntries = map[string]reasoningReplayEntry{}
	reasoningReplayMu.Unlock()
}

const toolContinuationPayload = `{"model":"gpt-5.5","input":[` +
	`{"type":"message","role":"user","content":[]},` +
	`{"type":"function_call","call_id":"c1","name":"read"},` +
	`{"type":"function_call_output","call_id":"c1","output":"ok"}]}`

func TestReasoningReplayInsertsBeforeTrailingToolOutputs(t *testing.T) {
	resetReasoningReplay()
	now := time.Now()
	item := json.RawMessage(`{"type":"reasoning","id":"r1","encrypted_content":"cipher"}`)
	rememberCodexReasoning("gpt-5.5", "session-1", []json.RawMessage{item}, now)

	out := applyCodexReasoningReplay([]byte(toolContinuationPayload), "gpt-5.5", "session-1", now)
	var body struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("replayed payload is invalid: %v", err)
	}
	kinds := make([]string, 0, len(body.Input))
	for _, entry := range body.Input {
		kinds = append(kinds, entry.Type)
	}
	want := "message,function_call,reasoning,function_call_output"
	if got := strings.Join(kinds, ","); got != want {
		t.Fatalf("input order = %s want %s", got, want)
	}
	if !strings.Contains(string(out), "cipher") {
		t.Fatal("encrypted reasoning was not replayed")
	}
}

func TestReasoningReplayLeavesNonContinuationTurnsAlone(t *testing.T) {
	resetReasoningReplay()
	now := time.Now()
	rememberCodexReasoning("gpt-5.5", "session-1",
		[]json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"cipher"}`)}, now)

	// A plain user turn is not a tool-result continuation.
	plain := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[]}]}`)
	if got := applyCodexReasoningReplay(plain, "gpt-5.5", "session-1", now); string(got) != string(plain) {
		t.Fatalf("plain turn was rewritten: %s", got)
	}
	// A turn that already carries reasoning must not have it duplicated.
	carried := []byte(`{"model":"gpt-5.5","input":[{"type":"reasoning","encrypted_content":"own"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)
	if got := applyCodexReasoningReplay(carried, "gpt-5.5", "session-1", now); string(got) != string(carried) {
		t.Fatalf("existing reasoning was duplicated: %s", got)
	}
	// A different account's session must not pick up these items.
	if got := applyCodexReasoningReplay([]byte(toolContinuationPayload), "gpt-5.5", "other-session", now); strings.Contains(string(got), "cipher") {
		t.Fatal("reasoning leaked across sessions")
	}
}

func TestReasoningReplayExpiresAndClears(t *testing.T) {
	resetReasoningReplay()
	now := time.Now()
	item := json.RawMessage(`{"type":"reasoning","encrypted_content":"cipher"}`)
	rememberCodexReasoning("gpt-5.5", "session-1", []json.RawMessage{item}, now)

	stale := now.Add(reasoningReplayTTL + time.Minute)
	if takeCodexReasoning("gpt-5.5", "session-1", stale) != nil {
		t.Fatal("expired reasoning was replayed")
	}

	// A later response without reasoning must clear the slot rather than
	// leaving the previous turn's items to be replayed again.
	rememberCodexReasoning("gpt-5.5", "session-1", []json.RawMessage{item}, now)
	rememberCodexReasoning("gpt-5.5", "session-1", nil, now)
	if takeCodexReasoning("gpt-5.5", "session-1", now) != nil {
		t.Fatal("stale reasoning survived a response that produced none")
	}
}

func TestCollectReasoningItemAcceptsOnlyEncryptedReasoning(t *testing.T) {
	encrypted := `{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"cipher"}}`
	if collectReasoningItem([]byte(encrypted)) == nil {
		t.Fatal("encrypted reasoning item was not collected")
	}
	for _, event := range []string{
		`{"type":"response.output_item.done","item":{"type":"reasoning"}}`,
		`{"type":"response.output_item.done","item":{"type":"message"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"c"}}`,
		`not-json`,
	} {
		if collectReasoningItem([]byte(event)) != nil {
			t.Fatalf("unexpected collection from %s", event)
		}
	}
}
