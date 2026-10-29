package domain

import (
	"context"
	"sync"
	"testing"
)

func TestFirstOutputObserverIsOnceAndAttemptScoped(t *testing.T) {
	parentCalls, childCalls := 0, 0
	parent := WithFirstOutputObserver(context.Background(), func() { parentCalls++ })
	child := WithFirstOutputObserver(parent, func() { childCalls++ })
	inherited, cancel := context.WithCancel(parent)
	defer cancel()

	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			ObserveFirstOutput(inherited)
			ObserveFirstOutput(parent)
			ObserveFirstOutput(child)
		})
	}
	wg.Wait()
	if parentCalls != 1 || childCalls != 1 {
		t.Fatalf("parent observations=%d, child observations=%d; want one per binding", parentCalls, childCalls)
	}
	cancel()
	if inherited.Err() != context.Canceled || parent.Err() != nil {
		t.Fatal("observer changed parent/child cancellation semantics")
	}
}

func TestFirstOutputObserverWithoutCallbackIsNoop(t *testing.T) {
	ObserveFirstOutput(context.Background())
	parent := WithFirstOutputObserver(context.Background(), func() { t.Error("disabled parent observer called") })
	ObserveFirstOutput(WithFirstOutputObserver(parent, nil))
}

func TestMeaningfulStreamEvent(t *testing.T) {
	tests := []struct {
		name  string
		event StreamEvent
		want  bool
	}{
		{"start metadata", StreamEvent{Kind: EvStart, Model: "model", NativeResponseID: "resp"}, false},
		{"text", StreamEvent{Kind: EvTextDelta, TextDelta: "answer"}, true},
		{"whitespace text", StreamEvent{Kind: EvTextDelta, TextDelta: " "}, true},
		{"empty text", StreamEvent{Kind: EvTextDelta}, false},
		{"reasoning", StreamEvent{Kind: EvThinkingDelta, ThinkingDelta: "thinking"}, true},
		{"empty reasoning", StreamEvent{Kind: EvThinkingDelta}, false},
		{"signature", StreamEvent{Kind: EvSignatureDelta, SignatureDelta: "signature"}, true},
		{"empty signature", StreamEvent{Kind: EvSignatureDelta}, false},
		{"opaque thinking", StreamEvent{Kind: EvRedactedThinking, RedactedThinkingData: "opaque"}, true},
		{"empty opaque thinking", StreamEvent{Kind: EvRedactedThinking}, false},
		{"tool id", StreamEvent{Kind: EvToolStart, ToolID: "call"}, true},
		{"tool name", StreamEvent{Kind: EvToolStart, ToolName: "search"}, true},
		{"tool arguments", StreamEvent{Kind: EvToolDelta, ArgsDelta: "{"}, true},
		{"tool index only", StreamEvent{Kind: EvToolStart, Index: 1}, false},
		{"empty tool delta", StreamEvent{Kind: EvToolDelta, Index: 1}, false},
		{"tool end", StreamEvent{Kind: EvToolEnd, Index: 1}, false},
		{"usage and finish", StreamEvent{Kind: EvEnd, Usage: &Usage{OutputTokens: 1}, FinishReason: "stop"}, false},
		{"error", StreamEvent{Kind: EvError, Err: &Failure{Message: "failed"}}, false},
		{"nil native frame", StreamEvent{Kind: EvNative}, false},
		{"unrelated fields", StreamEvent{Kind: EvStart, TextDelta: "not a delta event"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MeaningfulStreamEvent(tt.event); got != tt.want {
				t.Fatalf("meaningful=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestMeaningfulSSEOutputAndNativeFrames(t *testing.T) {
	tests := []struct {
		name, event, payload string
		want                 bool
	}{
		{"invalid JSON", "response.output_text.delta", `not-json`, false},
		{"null", "", `null`, false},
		{"array", "", `[]`, false},
		{"done marker", "", `[DONE]`, false},
		{"chat role", "", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{"chat usage", "", `{"choices":[],"usage":{"output_tokens":1}}`, false},
		{"chat finish", "", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, false},
		{"chat tool index", "", `{"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":""}}]}}]}`, false},
		{"chat text", "", `{"choices":[{"delta":{"content":"answer"}}]}`, true},
		{"chat whitespace", "", `{"choices":[{"delta":{"content":" "}}]}`, true},
		{"chat reasoning", "", `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, true},
		{"chat extended reasoning", "", `{"choices":[{"delta":{"reasoning":"thinking"}}]}`, true},
		{"chat encrypted details", "", `{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]}}]}`, true},
		{"chat metadata details", "", `{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","id":"r"}]}}]}`, false},
		{"chat unknown details", "", `{"choices":[{"delta":{"reasoning_details":[{"type":"future","text":"opaque"}]}}]}`, false},
		{"chat refusal", "", `{"choices":[{"delta":{"refusal":"refused"}}]}`, true},
		{"chat tool id", "", `{"choices":[{"delta":{"tool_calls":[{"id":"call"}]}}]}`, true},
		{"chat tool name", "", `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"search"}}]}}]}`, true},
		{"chat tool arguments", "", `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{"}}]}}]}`, true},
		{"chat legacy function", "", `{"choices":[{"delta":{"function_call":{"name":"search"}}}]}`, true},
		{"chat legacy arguments", "", `{"choices":[{"delta":{"function_call":{"arguments":"{}"}}}]}`, true},
		{"chat empty legacy function", "", `{"choices":[{"delta":{"role":"assistant","function_call":{"name":"","arguments":""}}}]}`, false},
		{"responses created", "", `{"type":"response.created","response":{"id":"r","output":[]}}`, false},
		{"responses created with opaque state", "", `{"type":"response.created","response":{"output":[{"type":"reasoning","encrypted_content":"opaque"}]}}`, false},
		{"responses progress", "", `{"type":"response.in_progress","response":{"id":"r","usage":{"output_tokens":1}}}`, false},
		{"responses queued", "", `{"type":"response.queued","response":{"id":"r"}}`, false},
		{"responses message identity", "", `{"type":"response.output_item.added","item":{"type":"message","id":"m","role":"assistant","content":[]}}`, false},
		{"responses reasoning identity", "", `{"type":"response.output_item.added","item":{"type":"reasoning","id":"rs","summary":[],"encrypted_content":""}}`, false},
		{"responses empty text", "", `{"type":"response.output_text.delta","delta":""}`, false},
		{"responses null delta", "", `{"type":"response.output_text.delta","delta":null}`, false},
		{"responses invalid delta type", "", `{"type":"response.output_text.delta","delta":{"text":"answer"}}`, false},
		{"responses empty part", "", `{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}`, false},
		{"responses empty tool", "", `{"type":"response.output_item.added","item":{"type":"function_call","id":"","name":"","arguments":""}}`, false},
		{"responses terminal only", "", `{"type":"response.completed","response":{"output":[],"usage":{"output_tokens":1}}}`, false},
		{"responses done only", "", `{"type":"response.done","response":{"output":[],"usage":{"output_tokens":1}}}`, false},
		{"responses text", "", `{"type":"response.output_text.delta","delta":"answer"}`, true},
		{"responses event field", "response.output_text.delta", `{"delta":"answer"}`, true},
		{"responses JSON type wins", "response.output_text.delta", `{"type":"response.created","delta":"not output"}`, false},
		{"responses reasoning", "", `{"type":"response.reasoning_text.delta","delta":"thinking"}`, true},
		{"responses summary", "", `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, true},
		{"responses refusal", "", `{"type":"response.refusal.delta","delta":"refused"}`, true},
		{"responses arguments", "", `{"type":"response.function_call_arguments.delta","delta":"{"}`, true},
		{"responses text done", "", `{"type":"response.output_text.done","text":"answer"}`, true},
		{"responses reasoning done", "", `{"type":"response.reasoning_summary_text.done","text":"thinking"}`, true},
		{"responses refusal done", "", `{"type":"response.refusal.done","refusal":"refused"}`, true},
		{"responses arguments done", "", `{"type":"response.function_call_arguments.done","arguments":"{}"}`, true},
		{"responses tool id", "", `{"type":"response.output_item.added","item":{"type":"function_call","id":"fc"}}`, true},
		{"responses call id", "", `{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call"}}`, true},
		{"responses tool name", "", `{"type":"response.output_item.added","item":{"type":"function_call","name":"search"}}`, true},
		{"responses tool arguments", "", `{"type":"response.output_item.done","item":{"type":"function_call","arguments":"{}"}}`, true},
		{"responses part", "", `{"type":"response.content_part.added","part":{"type":"output_text","text":"answer"}}`, true},
		{"responses summary part", "", `{"type":"response.reasoning_summary_part.done","part":{"type":"summary_text","text":"thinking"}}`, true},
		{"responses item summary", "", `{"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}}`, true},
		{"responses opaque thinking", "", `{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"opaque"}}`, true},
		{"responses terminal content", "", `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}}`, true},
		{"responses done content", "", `{"type":"response.done","response":{"output":[{"type":"message","content":[{"type":"refusal","refusal":"refused"}]}]}}`, true},
		{"responses incomplete content", "", `{"type":"response.incomplete","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}}`, true},
		{"responses error", "", `{"type":"response.failed","response":{"error":{"message":"failed"}}}`, false},
		{"anthropic start", "", `{"type":"message_start","message":{"id":"m","role":"assistant","content":[]}}`, false},
		{"anthropic ping", "", `{"type":"ping"}`, false},
		{"anthropic usage", "", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`, false},
		{"anthropic end", "", `{"type":"message_stop"}`, false},
		{"anthropic empty text", "", `{"type":"content_block_start","content_block":{"type":"text","text":""}}`, false},
		{"anthropic empty thinking", "", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":""}}`, false},
		{"anthropic empty signature", "", `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":""}}`, false},
		{"anthropic empty opaque thinking", "", `{"type":"content_block_start","content_block":{"type":"redacted_thinking","data":""}}`, false},
		{"anthropic text", "", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}`, true},
		{"anthropic thinking", "", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`, true},
		{"anthropic signature", "", `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"signature"}}`, true},
		{"anthropic opaque thinking", "", `{"type":"content_block_start","content_block":{"type":"redacted_thinking","data":"opaque"}}`, true},
		{"anthropic tool id", "", `{"type":"content_block_start","content_block":{"type":"tool_use","id":"call","input":{}}}`, true},
		{"anthropic tool name", "", `{"type":"content_block_start","content_block":{"type":"tool_use","name":"search","input":{}}}`, true},
		{"anthropic tool arguments", "", `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MeaningfulSSEOutput(tt.event, []byte(tt.payload)); got != tt.want {
				t.Fatalf("wire meaningful=%v, want %v", got, tt.want)
			}
			for _, terminal := range []bool{false, true} {
				event := StreamEvent{Kind: EvNative, NativeFrame: &NativeSSEFrame{
					Event: tt.event, Data: tt.payload, Terminal: terminal,
				}}
				if got := MeaningfulStreamEvent(event); got != tt.want {
					t.Fatalf("native meaningful=%v (terminal=%v), want %v", got, terminal, tt.want)
				}
			}
		})
	}
}
