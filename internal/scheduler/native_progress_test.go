package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

// Exercise real Chat SSE decoding followed by the scheduler's precommit gate.
// This needs no upstream account or billable request.
func TestChatNativeReasoningPrecommit(t *testing.T) {
	for _, tc := range []struct {
		name, delta string
		commit      bool
	}{
		{"reasoning", `{"reasoning":"thinking"}`, true},
		{"text_details", `{"reasoning_details":[{"type":"reasoning.text","text":"thinking","index":0}]}`, true},
		{"summary_details", `{"reasoning_details":[{"type":"reasoning.summary","summary":"thinking"}]}`, true},
		{"encrypted_details", `{"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]}`, true},
		{"empty_reasoning", `{"reasoning":"","reasoning_details":[]}`, false},
		{"metadata_only", `{"role":"assistant"}`, false},
		{"empty_details", `{"reasoning_details":[{"type":"reasoning.text","text":""}]}`, false},
		{"unknown_details", `{"reasoning_details":[{"type":"future","text":"not recognized"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for i := 0; i < 300; i++ {
					fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", tc.delta)
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			rc := chatReq("main")
			rc.Stream = true
			rc.Origin = domain.ProtocolOpenAIChat
			rc.Native = json.RawMessage(`{"model":"main","messages":[{"role":"user","content":"fixture"}],"stream":true}`)
			adapter := egress.NewOpenAIChat(egress.Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL}, Model: "upstream"})
			ch, cancel, err := adapter.Stream(context.Background(), rc)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			decoded := collectEvents(t, ch)
			f := newFixture(t)
			f.calls["t1"].streamEvents = decoded
			res, failure := f.sch.Execute(context.Background(), rc, RequestMeta{})
			if failure != nil {
				t.Fatal(failure)
			}
			events := collectEvents(t, res.Events)
			want := "t2"
			if tc.commit {
				want = "t1"
			}
			if res.Target != domain.ResourceTargetID(want) {
				t.Fatalf("target=%s, want %s: native reasoning was not recognized or empty stream bypassed guard", res.Target, want)
			}
			if tc.commit {
				natives := 0
				for _, ev := range events {
					if ev.Kind == domain.EvError {
						t.Fatalf("stream failed: %+v", ev.Err)
					}
					if ev.Kind == domain.EvNative {
						natives++
						if natives <= 300 && !strings.Contains(ev.NativeFrame.Data, tc.delta) {
							t.Fatalf("native reasoning altered: %s", ev.NativeFrame.Data)
						}
					}
				}
				if natives != 303 {
					t.Fatalf("native count=%d, want 303", natives)
				}
				if events[len(events)-1].Kind != domain.EvEnd {
					t.Fatal("missing validated end")
				}
			}
		})
	}
}

func TestNativeReasoningCommitDoesNotReplayOnFailure(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].streamEvents = []domain.StreamEvent{
		{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat, NativeFrame: &domain.NativeSSEFrame{Meaningful: true, Data: `{"choices":[{"delta":{"reasoning":"thought"}}]}`}},
		{Kind: domain.EvError, Err: &domain.Failure{Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream, Message: "fixture disconnect"}},
	}
	rc := chatReq("main")
	rc.Stream = true
	res, fail := f.sch.Execute(context.Background(), rc, RequestMeta{})
	if fail != nil {
		t.Fatal(fail)
	}
	events := collectEvents(t, res.Events)
	if res.Target != "t1" || f.calls["t2"].streams.Load() != 0 {
		t.Fatal("replayed request after client-visible reasoning")
	}
	if len(events) != 2 || events[0].Kind != domain.EvNative || events[1].Kind != domain.EvError {
		t.Fatalf("events=%+v", events)
	}
}

func TestNativeReasoningFixPreservesByteGuard(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].streamEvents = []domain.StreamEvent{
		{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat, NativeFrame: &domain.NativeSSEFrame{Data: strings.Repeat("x", (1<<20)+1)}},
		{Kind: domain.EvEnd, FinishReason: "stop"},
	}
	rc := chatReq("main")
	rc.Stream = true
	res, fail := f.sch.Execute(context.Background(), rc, RequestMeta{})
	if fail != nil {
		t.Fatal(fail)
	}
	events := collectEvents(t, res.Events)
	if res.Target != "t2" {
		t.Fatal("oversized unrecognized frame bypassed byte guard")
	}
	for _, event := range events {
		if event.Kind == domain.EvNative {
			t.Fatal("uncommitted failed upstream frame leaked")
		}
	}
}

func TestResponsesNativeProgressPrecommit(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		commit      bool
	}{
		{"summary", `{"type":"response.reasoning_summary_text.delta","delta":"thought"}`, true},
		{"refusal", `{"type":"response.refusal.delta","delta":"refused"}`, true},
		{"encrypted", `{"type":"response.output_item.done","item":{"id":"r","type":"reasoning","status":"completed","encrypted_content":"opaque"}}`, true},
		{"empty", `{"type":"response.reasoning_summary_text.delta","delta":""}`, false},
		{"unknown", `{"type":"response.future.delta","delta":"unknown"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for range 300 {
					fmt.Fprintf(w, "data: %s\n\n", tc.frame)
				}
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{}}}\n\n")
			}))
			defer srv.Close()
			rc := chatReq("main")
			rc.Stream = true
			rc.Protocol = domain.ProtocolOpenAIResponses
			rc.Origin = rc.Protocol
			rc.Native = json.RawMessage(`{"model":"main","input":"fixture","stream":true}`)
			a := egress.NewOpenAIResponses(egress.Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL}, Model: "upstream"})
			ch, cancel, err := a.Stream(context.Background(), rc)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			f := newFixture(t)
			f.calls["t1"].streamEvents = collectEvents(t, ch)
			res, fail := f.sch.Execute(context.Background(), rc, RequestMeta{})
			if fail != nil {
				t.Fatal(fail)
			}
			events := collectEvents(t, res.Events)
			want := domain.ResourceTargetID("t2")
			if tc.commit {
				want = "t1"
			}
			if res.Target != want {
				t.Fatalf("target %s want %s", res.Target, want)
			}
			if tc.commit {
				n := 0
				for _, ev := range events {
					if ev.Kind == domain.EvError {
						t.Fatal(ev.Err)
					}
					if ev.Kind == domain.EvNative && !ev.NativeFrame.Terminal {
						n++
						if ev.NativeFrame.Data != tc.frame {
							t.Fatal("altered native data")
						}
					}
				}
				if n != 300 {
					t.Fatalf("frames=%d", n)
				}
			}
		})
	}
}

func TestNativeHeartbeatDoesNotFillPrecommitBuffer(t *testing.T) {
	f := newFixture(t)
	for range 300 {
		f.calls["t1"].streamEvents = append(f.calls["t1"].streamEvents, domain.StreamEvent{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat, NativeFrame: &domain.NativeSSEFrame{KeepAlive: true, Data: `{"type":"ping"}`}})
	}
	f.calls["t1"].streamEvents = append(f.calls["t1"].streamEvents, domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: "answer"}, domain.StreamEvent{Kind: domain.EvEnd, FinishReason: "stop"})
	rc := chatReq("main")
	rc.Stream = true
	res, fail := f.sch.Execute(context.Background(), rc, RequestMeta{})
	if fail != nil {
		t.Fatal(fail)
	}
	for _, ev := range collectEvents(t, res.Events) {
		if ev.Kind == domain.EvNative || ev.Kind == domain.EvError {
			t.Fatalf("unexpected %+v", ev)
		}
	}
	if res.Target != "t1" {
		t.Fatal(res.Target)
	}
}

func TestAnthropicNativeThinkingAndHeartbeatPrecommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 300 {
			fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		}
		frames := []string{
			`{"type":"message_start","message":{"usage":{}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		}
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		for range 300 {
			fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thought\"}}\n\n")
		}
		for _, frame := range []string{
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signature"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":301}}`,
			`{"type":"message_stop"}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
	}))
	defer srv.Close()
	rc := chatReq("main")
	rc.Stream = true
	rc.Protocol = domain.ProtocolAnthropic
	rc.Origin = rc.Protocol
	rc.Native = json.RawMessage(`{"model":"main","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"fixture"}]}`)
	a := egress.NewAnthropic(egress.Base{HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL}, Model: "upstream"})
	ch, cancel, err := a.Stream(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	f := newFixture(t)
	f.calls["t1"].streamEvents = collectEvents(t, ch)
	res, fail := f.sch.Execute(context.Background(), rc, RequestMeta{})
	if fail != nil {
		t.Fatal(fail)
	}
	n := 0
	signature, opaque := false, false
	for _, ev := range collectEvents(t, res.Events) {
		switch ev.Kind {
		case domain.EvError:
			t.Fatal(ev.Err)
		case domain.EvThinkingDelta:
			n++
		case domain.EvSignatureDelta:
			signature = ev.SignatureDelta == "signature"
		case domain.EvRedactedThinking:
			opaque = ev.RedactedThinkingData == "opaque"
		case domain.EvNative:
			if ev.NativeFrame.KeepAlive {
				t.Fatal("precommit heartbeat leaked")
			}
		}
	}
	if res.Target != "t1" || n != 300 || !signature || !opaque {
		t.Fatalf("target=%s thoughts=%d signature=%v opaque=%v", res.Target, n, signature, opaque)
	}
}
