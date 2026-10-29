package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestPreflightTracePreservesIngressFailureAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint("canceled=", canceled), func(t *testing.T) {
			led := openTempLedger(t)
			s := New(nil, led.Ledger)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, finish := s.ObserveIngress(ctx, domain.ProtocolOpenAIResponses, RequestMeta{PresentedCredential: "private-credential"})
			request := ctx.Value(ingressRequestKey{}).(*ingressRequest)
			if s.OpenIngressRequests() != 1 {
				t.Fatal("request must remain open before completion")
			}
			readStartedAt := time.Now()
			s.ObserveIngressPhase(ctx, "BODY_RECEIVE_STARTED", readStartedAt, nil)
			if canceled {
				cancel()
			}
			s.ObserveIngressPhase(ctx, "BODY_RECEIVE_FAILED", time.Now(), errors.New("private-body-and-credential"))
			// Returned snapshots must not mutate the live request's trace.
			copy := preflightTraceEvents(ctx)
			copy[0].Type = "corrupted"
			finish(400)
			finish(400)
			if s.OpenIngressRequests() != 0 {
				t.Fatal("finish must release ingress exactly once")
			}
			stored := led.reopen(t)
			trace, err := stored.QueryTrace(request.id)
			if err != nil || trace == nil || len(trace.Events) != 4 {
				t.Fatalf("trace=%+v error=%v", trace, err)
			}
			if trace.Events[0].Type != "REQUEST_RECEIVED" || !trace.Events[0].At.Equal(request.startedAt) || !trace.Events[1].At.Equal(readStartedAt) {
				t.Fatalf("observed timestamps were replaced: %+v", trace.Events)
			}
			wantFailure, wantPhase, wantStatus := domain.FailUnsupportedParam, "INGRESS_FAILED", domain.ReqFailed
			if canceled {
				wantFailure, wantPhase, wantStatus = domain.FailClientCancel, "INGRESS_CANCELED", domain.ReqCanceled
			}
			if trace.Events[2].FailureType != wantFailure || trace.Events[3].Type != wantPhase {
				t.Fatalf("incorrect terminal phases: %+v", trace.Events)
			}
			r, err := stored.QueryRequest(request.id)
			if err != nil || r == nil || r.Status != wantStatus || r.FailureType != wantFailure || r.Attempts != 0 {
				t.Fatalf("request=%+v error=%v", r, err)
			}
			raw, err := json.Marshal(trace)
			if err != nil || strings.Contains(string(raw), "private-") {
				t.Fatal("trace exposed body/error/credential content")
			}
		})
	}
}

func TestPreflightTraceIsBoundedAndDeduplicatesStages(t *testing.T) {
	led := openTempLedger(t)
	s := New(nil, led.Ledger)
	ctx, finish := s.ObserveIngress(context.Background(), domain.ProtocolOpenAIChat, RequestMeta{})
	defer finish(400)
	at := time.Now()
	s.ObserveIngressPhase(ctx, "BODY_PARSE_STARTED", at, nil)
	s.ObserveIngressPhase(ctx, "BODY_PARSE_STARTED", time.Now(), nil)
	s.ObserveIngressPhase(ctx, "arbitrary-request-content", at, errors.New("untrusted"))
	events := preflightTraceEvents(ctx)
	if len(events) != 2 || !events[1].At.Equal(at) {
		t.Fatalf("stage was duplicated or overwritten: %+v", events)
	}
	for i := 0; i < 100; i++ {
		s.observePreflightPhase(ctx, domain.ExecutionTraceEvent{At: at, Type: fmt.Sprintf("PHASE_%d", i), Message: strings.Repeat("x", 1024)})
	}
	events = preflightTraceEvents(ctx)
	if len(events) != maxPreflightTraceEvents {
		t.Fatalf("unbounded phase count: %d", len(events))
	}
	for _, event := range events {
		if len(event.Message) > 256 {
			t.Fatal("unbounded phase message")
		}
	}
}
