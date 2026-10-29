package egress

import (
	"context"
	"encoding/json"
	"ferry/internal/domain"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatExtendedReasoningNativeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, delta string
		meaningful  bool
	}{
		{"alias", `{"reasoning":"thought"}`, true},
		{"both_aliases", `{"reasoning":"thought","reasoning_content":"thought"}`, true},
		{"detail_text", `{"reasoning_details":[{"type":"reasoning.text","text":"thought","signature":"sig","index":0}]}`, true},
		{"encrypted", `{"reasoning_details":[{"type":"reasoning.encrypted","data":"ciphertext","id":"a"}]}`, true},
		{"signature_only", `{"reasoning_details":[{"type":"reasoning.text","signature":"sig"}]}`, false},
		{"whitespace", `{"reasoning":"  ","reasoning_details":[{"type":"reasoning.text","text":"  "}]}`, false},
		{"wrong_type", `{"reasoning_details":[{"type":"reasoning.text","text":3}]}`, false},
		{"tool_requires_validation", `{"reasoning":"thought","tool_calls":[{"index":0}]}`, false},
		{"invalid_role", `{"role":"user","reasoning":"thought"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"choices":[{"index":0,"delta":` + tc.delta + `}]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sse(w, data(frame), data(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`), data(`[DONE]`))
			}))
			defer srv.Close()
			rc := chatRC()
			rc.Origin = domain.ProtocolOpenAIChat
			rc.Native = json.RawMessage(`{"model":"alias","messages":[],"stream":true}`)
			ch, cancel, fail := NewOpenAIChat(testBase(srv, "/v1")).Stream(context.Background(), rc)
			mustNoFail(t, fail)
			defer cancel()
			events := drain(t, ch)
			found := false
			canonical := 0
			for _, ev := range events {
				if ev.Kind == domain.EvNative && ev.NativeFrame.Data == frame {
					found = true
					if ev.NativeFrame.Meaningful != tc.meaningful {
						t.Fatalf("meaningful=%v want %v", ev.NativeFrame.Meaningful, tc.meaningful)
					}
				}
				if ev.Kind == domain.EvThinkingDelta {
					canonical++
				}
			}
			if !found {
				t.Fatal("original native reasoning frame not preserved")
			}
			want := 0
			if tc.name == "both_aliases" {
				want = 1
			}
			if canonical != want {
				t.Fatalf("duplicate/fabricated canonical reasoning: %d", canonical)
			}
		})
	}
}

func TestChatExtendedReasoningConversionFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, data(`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"cipher"}]}}]}`), data(`[DONE]`))
	}))
	defer srv.Close()
	rc := chatRC()
	rc.Origin = domain.ProtocolOpenAIResponses
	ch, cancel, fail := NewOpenAIChat(testBase(srv, "/v1")).Stream(context.Background(), rc)
	mustNoFail(t, fail)
	defer cancel()
	events := drain(t, ch)
	if len(events) != 1 || events[0].Kind != domain.EvError || events[0].Err.Type != domain.FailProtocolConversion {
		t.Fatalf("opaque reasoning was silently converted: %+v", events)
	}
}

func TestNativeProgressProtocolBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, data            string
		protocol              domain.Protocol
		meaningful, heartbeat bool
	}{
		{"summary", `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, domain.ProtocolOpenAIResponses, true, false},
		{"empty summary", `{"type":"response.reasoning_summary_text.delta","delta":""}`, domain.ProtocolOpenAIResponses, false, false},
		{"failed item", `{"type":"response.output_item.done","item":{"type":"reasoning","status":"failed","encrypted_content":"opaque"}}`, domain.ProtocolOpenAIResponses, false, false},
		{"unknown item", `{"type":"response.output_item.added","item":{"type":"future","text":"opaque"}}`, domain.ProtocolOpenAIResponses, false, false},
		{"terminal", `{"type":"response.completed","response":{"output":[{"type":"reasoning","encrypted_content":"opaque"}]}}`, domain.ProtocolOpenAIResponses, false, false},
		{"ping", `{"type":"ping"}`, domain.ProtocolAnthropic, false, true},
		{"wrong protocol ping", `{"type":"ping"}`, domain.ProtocolOpenAIChat, false, false},
		{"unknown anthropic", `{"type":"future","text":"opaque"}`, domain.ProtocolAnthropic, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := nativeStreamEvent(tc.protocol, sseEvent{data: tc.data}, false)
			if ev.NativeFrame.Meaningful != tc.meaningful || ev.NativeFrame.KeepAlive != tc.heartbeat {
				t.Fatalf("unexpected flags %+v", ev.NativeFrame)
			}
		})
	}
}
