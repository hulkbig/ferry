package egress

import "encoding/json"

// Codex sends moderation diagnostics separately from generated output. They
// have no Chat/Anthropic equivalent and are not a completion or a refusal.
// Recognize only the observed diagnostic envelope; future payloads still go
// through the normal native-only checks. Upstream failure and refusal events
// remain authoritative and are handled independently.
func codexDiagnosticMetadata(raw []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	for key := range envelope {
		switch key {
		case "type", "response_id", "sequence_number", "metadata":
		default:
			return false
		}
	}
	if raw, exists := envelope["response_id"]; exists {
		var id string
		if json.Unmarshal(raw, &id) != nil {
			return false
		}
	}
	if raw, exists := envelope["sequence_number"]; exists {
		var sequence int64
		if json.Unmarshal(raw, &sequence) != nil || sequence < 0 {
			return false
		}
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(envelope["metadata"], &metadata) != nil || len(metadata) != 1 {
		return false
	}
	var diagnostics map[string]json.RawMessage
	return json.Unmarshal(metadata["openai_chatgpt_moderation_metadata"], &diagnostics) == nil && diagnostics != nil
}
