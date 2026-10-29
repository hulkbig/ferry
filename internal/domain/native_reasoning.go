package domain

import (
	"encoding/json"
	"strings"
)

// MeaningfulChatReasoning recognizes known, nonempty reasoning extensions. IDs, signatures,
// unknown detail types and empty arrays alone must not defeat precommit limits.
// The payload stays opaque and byte-preserved in the native SSE frame.
func MeaningfulChatReasoning(reasoning string, raw json.RawMessage) bool {
	if strings.TrimSpace(reasoning) != "" {
		return true
	}
	var details []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Summary string `json:"summary"`
		Data    string `json:"data"`
	}
	if json.Unmarshal(raw, &details) != nil {
		return false
	}
	for _, detail := range details {
		var value string
		switch detail.Type {
		case "reasoning.text":
			value = detail.Text
		case "reasoning.summary":
			value = detail.Summary
		case "reasoning.encrypted":
			value = detail.Data
		}
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}
