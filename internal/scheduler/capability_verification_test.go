package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

func TestCapabilityVerificationRecoversUnknownWithoutChangingRequest(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(s *domain.CompiledSnapshot) {
		for _, target := range s.Targets {
			target.Capabilities = domain.Capabilities{Text: true}
		}
	})
	calls := 0
	f.sch.SetCapabilityVerifier(func(ctx context.Context, p *domain.Provider, c *domain.Connection, target *domain.CompiledTarget, caller egress.Caller, protocol domain.Protocol, required domain.Requirements) domain.CapabilityVerification {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 20*time.Second {
			t.Error("unbounded verification")
		}
		if caller != f.calls[target.ID] || p.ID != target.ProviderID || c.ID != target.ConnectionID || !required.Tools || !required.Reasoning {
			t.Error("lost live caller or requirements")
		}
		caps := domain.Capabilities{Text: true, Stream: true, Tools: true, Reasoning: true}
		return domain.CapabilityVerification{Capabilities: caps, Statuses: domain.DeclaredCapabilityStatuses(caps, protocol), Probes: 3}
	})
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "user_tool", Parameters: json.RawMessage(`{"type":"object"}`)}}
	request.Reasoning = &domain.ReasoningSpec{Effort: "high"}
	if _, failure := f.sch.DryRunV2(request, RequestMeta{BearerToken: "sk"}); failure == nil {
		t.Fatal("dry run invented support")
	}
	if calls != 0 {
		t.Fatal("simulation sent probes")
	}
	if _, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "invalid"}); failure == nil || calls != 0 {
		t.Fatal("unauthenticated request probed upstream")
	}
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure != nil || result.ConnectionID != "connection-a" || calls != 1 {
		t.Fatalf("result=%+v failure=%v calls=%d", result, failure, calls)
	}
	sent := f.calls["p1-a"].lastRequest()
	if sent == nil || sent.Tools[0].Name != "user_tool" || sent.Reasoning.Effort != "high" {
		t.Fatalf("request degraded: %+v", sent)
	}
	if f.sch.Snapshot().Targets["p1-a"].Capabilities.Tools {
		t.Fatal("verification changed shared config")
	}
	db := f.ledger.reopen(t)
	records, _, err := db.QueryRequests(domain.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		trace, err := db.QueryTrace(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if trace == nil {
			continue
		}
		for _, candidate := range trace.Candidates {
			if candidate.CapabilityProbeRequests == 3 && candidate.CapabilityStatuses[domain.CapabilityReasoning] == domain.CapabilitySupported {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("verification evidence missing from request trace")
	}
}

func TestCapabilityVerificationSkipsDisabledAndBacksOffInconclusive(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(s *domain.CompiledSnapshot) {
		s.Providers["provider-1"].Enabled = false
		for _, target := range s.Targets {
			target.Capabilities = domain.Capabilities{Text: true}
		}
	})
	calls := 0
	f.sch.SetCapabilityVerifier(func(ctx context.Context, p *domain.Provider, c *domain.Connection, target *domain.CompiledTarget, caller egress.Caller, protocol domain.Protocol, required domain.Requirements) domain.CapabilityVerification {
		calls++
		if p.ID != "provider-2" {
			t.Error("disabled provider was probed")
		}
		caps := domain.Capabilities{Text: true}
		return domain.CapabilityVerification{Capabilities: caps, Statuses: domain.DeclaredCapabilityStatuses(caps, protocol), Error: "capability verification inconclusive: rate_limit", Probes: 1}
	})
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "tool"}}
	_, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure == nil || failure.Type != domain.FailConnect || calls != 1 {
		t.Fatalf("failure=%v calls=%d", failure, calls)
	}
	for _, caller := range f.calls {
		if caller.invokes.Load() != 0 {
			t.Fatal("user request sent to unverified endpoint")
		}
	}
	db := f.ledger.reopen(t)
	records, _, err := db.QueryRequests(domain.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	trace, err := db.QueryTrace(records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range trace.Candidates {
		if candidate.Target == "p2-a" {
			found = strings.Contains(candidate.ExclusionReason, "unverified: tools") && candidate.CapabilityVerificationError != ""
		}
	}
	if !found {
		t.Fatal("inconclusive state was not recorded")
	}
}
