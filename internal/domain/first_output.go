package domain

import (
	"context"
	"encoding/json"
	"sync"
)

type firstOutputObserverKey struct{}

type firstOutputObserver struct {
	once    sync.Once
	observe func()
}

// WithFirstOutputObserver binds a new, concurrency-safe, one-shot observer to
// ctx. Bind a fresh observer for each attempt. A nil callback disables observation
// in the returned context, including an observer inherited from its parent.
func WithFirstOutputObserver(ctx context.Context, observe func()) context.Context {
	return context.WithValue(ctx, firstOutputObserverKey{}, &firstOutputObserver{observe: observe})
}

// ObserveFirstOutput reports meaningful upstream output, including output read
// while reassembling a non-streaming response. It does not report client delivery.
// With no observer bound to ctx, this is a no-op. The callback runs synchronously
// at most once and should return promptly.
func ObserveFirstOutput(ctx context.Context) {
	observer, _ := ctx.Value(firstOutputObserverKey{}).(*firstOutputObserver)
	if observer != nil && observer.observe != nil {
		observer.once.Do(observer.observe)
	}
}

// MeaningfulStreamEvent distinguishes generated output from lifecycle, usage,
// empty deltas and terminal-only events. Native events use the same wire-level
// rules as ingress; canonical events inspect only the fields relevant to Kind.
func MeaningfulStreamEvent(event StreamEvent) bool {
	switch event.Kind {
	case EvTextDelta:
		return event.TextDelta != ""
	case EvThinkingDelta:
		return event.ThinkingDelta != ""
	case EvSignatureDelta:
		return event.SignatureDelta != ""
	case EvRedactedThinking:
		return event.RedactedThinkingData != ""
	case EvToolStart, EvToolDelta:
		return event.ToolID != "" || event.ToolName != "" || event.ArgsDelta != ""
	case EvNative:
		return event.NativeFrame != nil && ((!event.NativeFrame.Terminal && event.NativeFrame.Meaningful) || MeaningfulSSEOutput(event.NativeFrame.Event, []byte(event.NativeFrame.Data)))
	default:
		return false
	}
}

// MeaningfulSSEOutput inspects a Chat, Responses or Anthropic wire frame.
// Lifecycle, usage and terminal-only frames are not output. Whitespace in an
// otherwise non-empty text delta is output, as are tool identity/arguments and
// opaque thinking; response/message identity alone is not.
func MeaningfulSSEOutput(event string, payload []byte) bool {
	var object map[string]any
	if json.Unmarshal(payload, &object) != nil {
		return false
	}
	if choices, ok := object["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if role, _ := delta["role"].(string); role != "" && role != "assistant" {
				continue
			}
			reasoning, _ := delta["reasoning"].(string)
			details, _ := json.Marshal(delta["reasoning_details"])
			if MeaningfulChatReasoning(reasoning, details) {
				return true
			}
			if outputStrings(delta, "content", "reasoning_content", "refusal") {
				return true
			}
			tools, _ := delta["tool_calls"].([]any)
			for _, rawTool := range tools {
				tool, _ := rawTool.(map[string]any)
				function, _ := tool["function"].(map[string]any)
				if outputStrings(tool, "id") || outputStrings(function, "name", "arguments") {
					return true
				}
			}
			function, _ := delta["function_call"].(map[string]any)
			if outputStrings(function, "name", "arguments") {
				return true
			}
		}
		return false
	}
	if typ, ok := object["type"].(string); ok {
		event = typ
	}
	switch event {
	case "response.output_text.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.refusal.delta":
		return outputStrings(object, "delta")
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done",
		"response.function_call_arguments.done", "response.refusal.done":
		return outputStrings(object, "text", "arguments", "refusal")
	case "response.output_item.added", "response.output_item.done":
		return meaningfulOutputBlock(object["item"])
	case "response.content_part.added", "response.content_part.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return meaningfulOutputBlock(object["part"])
	case "response.completed", "response.done", "response.incomplete":
		response, _ := object["response"].(map[string]any)
		output, _ := response["output"].([]any)
		for _, block := range output {
			if meaningfulOutputBlock(block) {
				return true
			}
		}
	case "content_block_delta":
		delta, _ := object["delta"].(map[string]any)
		return outputStrings(delta, "text", "thinking", "signature", "partial_json")
	case "content_block_start":
		return meaningfulOutputBlock(object["content_block"])
	}
	return false
}

func outputStrings(object map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := object[key].(string); ok && value != "" {
			return true
		}
	}
	return false
}

func meaningfulOutputBlock(raw any) bool {
	block, _ := raw.(map[string]any)
	if outputStrings(block, "text", "thinking", "signature", "refusal") {
		return true
	}
	switch block["type"] {
	case "tool_use", "function_call":
		return outputStrings(block, "id", "call_id", "name", "arguments")
	case "redacted_thinking":
		return outputStrings(block, "data")
	case "reasoning":
		if outputStrings(block, "encrypted_content") {
			return true
		}
	}
	for _, key := range []string{"content", "summary"} {
		items, _ := block[key].([]any)
		for _, item := range items {
			if meaningfulOutputBlock(item) {
				return true
			}
		}
	}
	return false
}
