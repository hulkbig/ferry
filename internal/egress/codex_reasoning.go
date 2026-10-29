package egress

import (
	"encoding/json"
	"sync"
	"time"
)

// Codex reasoning replay.
//
// The Codex backend is driven statelessly here: store is forced off and
// previous_response_id is stripped, so the upstream keeps nothing between
// turns. That is the right call for a gateway that may move a conversation
// between accounts, but it breaks reasoning models, whose next turn is
// expected to carry back the encrypted reasoning items the model produced
// alongside its tool calls. Without them the model loses its own chain of
// thought across a tool round trip, and the upstream can reject the turn
// outright.
//
// The gateway therefore remembers the reasoning items from each completed
// response and splices them back into the following turn. Replay happens only
// for a tool-result continuation, and only at the one position where those
// items belong: immediately before the trailing run of function_call_output
// entries they were produced with. Anything else is passed through untouched.
const (
	reasoningReplayTTL            = time.Hour
	reasoningReplayMaxEntries     = 4096
	reasoningReplayMaxItems       = 64
	reasoningReplayMaxEntryBytes  = 4 << 20
	reasoningReplayEvictBatchSize = 128
)

type reasoningReplayEntry struct {
	items     []json.RawMessage
	storedAt  time.Time
	sizeBytes int
}

var (
	reasoningReplayMu      sync.Mutex
	reasoningReplayEntries = map[string]reasoningReplayEntry{}
)

func reasoningReplayKey(model, session string) string {
	if model == "" || session == "" {
		return ""
	}
	return model + "\x00" + session
}

// rememberCodexReasoning stores the reasoning items from one completed
// response. Passing no items clears the slot: a response that carried no
// reasoning must not leave a previous turn's items behind to be replayed.
func rememberCodexReasoning(model, session string, items []json.RawMessage, now time.Time) {
	key := reasoningReplayKey(model, session)
	if key == "" {
		return
	}
	reasoningReplayMu.Lock()
	defer reasoningReplayMu.Unlock()
	if len(items) == 0 {
		delete(reasoningReplayEntries, key)
		return
	}
	if len(items) > reasoningReplayMaxItems {
		items = items[len(items)-reasoningReplayMaxItems:]
	}
	size := 0
	stored := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if size+len(item) > reasoningReplayMaxEntryBytes {
			break
		}
		size += len(item)
		stored = append(stored, append(json.RawMessage(nil), item...))
	}
	if len(stored) == 0 {
		delete(reasoningReplayEntries, key)
		return
	}
	evictReasoningReplayLocked(now)
	reasoningReplayEntries[key] = reasoningReplayEntry{items: stored, storedAt: now, sizeBytes: size}
}

// takeCodexReasoning returns the remembered items for one conversation.
func takeCodexReasoning(model, session string, now time.Time) []json.RawMessage {
	key := reasoningReplayKey(model, session)
	if key == "" {
		return nil
	}
	reasoningReplayMu.Lock()
	defer reasoningReplayMu.Unlock()
	entry, ok := reasoningReplayEntries[key]
	if !ok {
		return nil
	}
	if now.Sub(entry.storedAt) > reasoningReplayTTL {
		delete(reasoningReplayEntries, key)
		return nil
	}
	return entry.items
}

// evictReasoningReplayLocked drops expired entries, then the oldest remaining
// ones if the cache is still at capacity.
func evictReasoningReplayLocked(now time.Time) {
	for key, entry := range reasoningReplayEntries {
		if now.Sub(entry.storedAt) > reasoningReplayTTL {
			delete(reasoningReplayEntries, key)
		}
	}
	if len(reasoningReplayEntries) < reasoningReplayMaxEntries {
		return
	}
	// Leave headroom so a busy gateway does not rescan the map every turn.
	target := reasoningReplayMaxEntries - reasoningReplayEvictBatchSize
	for len(reasoningReplayEntries) > target {
		oldestKey, oldest := "", time.Time{}
		for key, entry := range reasoningReplayEntries {
			if oldestKey == "" || entry.storedAt.Before(oldest) {
				oldestKey, oldest = key, entry.storedAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(reasoningReplayEntries, oldestKey)
	}
}

// collectReasoningItems picks the reasoning items out of a Responses event
// stream's completed output items.
func collectReasoningItem(event []byte) json.RawMessage {
	var envelope struct {
		Type string          `json:"type"`
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(event, &envelope) != nil || envelope.Type != "response.output_item.done" {
		return nil
	}
	var item struct {
		Type             string `json:"type"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if json.Unmarshal(envelope.Item, &item) != nil || item.Type != "reasoning" {
		return nil
	}
	if item.EncryptedContent == "" {
		// Plain reasoning summaries are already carried in the canonical
		// history; only the encrypted form has to be replayed verbatim.
		return nil
	}
	return envelope.Item
}

// applyCodexReasoningReplay splices remembered reasoning items into a request
// body. It returns the payload unchanged unless this is a tool-result
// continuation that is missing its reasoning.
func applyCodexReasoningReplay(payload []byte, model, session string, now time.Time) []byte {
	items := takeCodexReasoning(model, session, now)
	if len(items) == 0 || len(payload) == 0 {
		return payload
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(payload, &body) != nil {
		return payload
	}
	rawInput, ok := body["input"]
	if !ok {
		return payload
	}
	var input []json.RawMessage
	if json.Unmarshal(rawInput, &input) != nil || len(input) == 0 {
		return payload
	}
	start, hasReasoning := trailingToolOutputRun(input)
	if start < 0 || hasReasoning {
		return payload
	}
	merged := make([]json.RawMessage, 0, len(input)+len(items))
	merged = append(merged, input[:start]...)
	merged = append(merged, items...)
	merged = append(merged, input[start:]...)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return payload
	}
	body["input"] = encoded
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

// trailingToolOutputRun locates the run of function_call_output entries that
// closes the input, which is where the previous turn's reasoning belongs. It
// returns -1 when the request is not a tool-result continuation, and reports
// whether the input already carries reasoning of its own.
func trailingToolOutputRun(input []json.RawMessage) (start int, hasReasoning bool) {
	for _, raw := range input {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Type == "reasoning" {
			hasReasoning = true
			break
		}
	}
	start = -1
	for i := len(input) - 1; i >= 0; i-- {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(input[i], &probe) != nil || probe.Type != "function_call_output" {
			break
		}
		start = i
	}
	return start, hasReasoning
}
