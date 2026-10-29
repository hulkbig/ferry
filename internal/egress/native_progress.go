package egress

import (
	"encoding/json"
	"ferry/internal/domain"
)

// Native-only output has no canonical delta to commit the stream. Recognize
// protocol fields, never a provider name. Tools and ordinary text still commit
// through their validated canonical events, after adapter lifecycle checks.
func nativeOnlyOutput(protocol domain.Protocol, ev sseEvent) bool {
	if protocol != domain.ProtocolOpenAIResponses {
		return false
	}
	var body responseStreamEnvelope
	if json.Unmarshal([]byte(ev.data), &body) != nil {
		return false
	}
	switch body.Type {
	case "response.reasoning_summary_text.delta", "response.refusal.delta":
		return body.Delta != ""
	case "response.output_item.added", "response.output_item.done":
		item := body.Item
		if item == nil || (body.Type == "response.output_item.done" && item.Status != "" && item.Status != "completed") {
			return false
		}
		switch item.Type {
		case "reasoning":
			if item.EncryptedContent != "" {
				return true
			}
			for _, part := range item.Summary {
				if part.Type == "summary_text" && part.Text != "" {
					return true
				}
			}
		case "message":
			if item.Role != "" && item.Role != "assistant" {
				return false
			}
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					return true
				}
			}
		}
	}
	return false
}

func nativeKeepAlive(protocol domain.Protocol, ev sseEvent) bool {
	if protocol != domain.ProtocolAnthropic {
		return false
	}
	var body struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(ev.data), &body) == nil && body.Type == "ping"
}
