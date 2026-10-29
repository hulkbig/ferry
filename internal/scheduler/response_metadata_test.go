package scheduler

import (
	"context"
	"fmt"
	"testing"

	"ferry/internal/domain"
)

func TestUsageAggregationPreservesCacheSemantics(t *testing.T) {
	openai := domain.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 800, CacheWriteTokens: 150, CacheSubsetOfInput: true}
	anthropic := domain.Usage{InputTokens: 50, OutputTokens: 200, CacheReadTokens: 800, CacheWriteTokens: 150}
	for _, usages := range [][]domain.Usage{{openai, openai}, {anthropic, anthropic}, {openai, anthropic}, {anthropic, openai}} {
		var total domain.Usage
		for _, u := range usages {
			addUsage(&total, u)
		}
		if total.UncachedInput() != 100 || total.BillableTokens() != 2400 || total.CacheReadTokens != 1600 || total.CacheWriteTokens != 300 {
			t.Fatalf("aggregate=%+v", total)
		}
		if total.CacheSubsetOfInput != (usages[0].CacheSubsetOfInput && usages[1].CacheSubsetOfInput) {
			t.Fatalf("dialect lost: %+v", total)
		}
	}
}

func TestTurnStateRequiresExistingCodexConnectionBinding(t *testing.T) {
	for _, tc := range []struct {
		name, provider, connection string
		protocol                   domain.Protocol
		codex, bound, want         bool
	}{
		{"same", "p", "a", domain.ProtocolOpenAIResponses, true, true, true},
		{"rebound", "p", "b", domain.ProtocolOpenAIResponses, true, true, false},
		{"other provider", "q", "a", domain.ProtocolOpenAIResponses, true, true, false},
		{"unbound", "p", "a", domain.ProtocolOpenAIResponses, true, false, false},
		{"empty connection", "p", "", domain.ProtocolOpenAIResponses, true, true, false},
		{"generic provider", "p", "a", domain.ProtocolOpenAIResponses, false, true, false},
		{"chat origin", "p", "a", domain.ProtocolOpenAIChat, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &domain.Provider{ID: tc.provider}
			if tc.codex {
				provider.Type = domain.ProviderCliproxyCodex
			}
			d := &decision{w: &world{snap: &domain.CompiledSnapshot{Providers: map[string]*domain.Provider{tc.provider: provider}}}}
			if tc.bound {
				d.conversation = &domain.Conversation{ID: "conversation", CurrentProviderID: "p", CurrentConnectionID: "a"}
			}
			rc := &domain.RequestContract{Protocol: tc.protocol, Origin: tc.protocol, CodexTurnStateAllowed: true}
			target := &domain.CompiledTarget{ProviderID: tc.provider, ConnectionID: tc.connection, NativeProtocols: []domain.Protocol{tc.protocol}}
			contract, _ := d.contractForAttempt(rc, target)
			if contract.CodexTurnStateAllowed != tc.want {
				t.Fatalf("allowed=%v want=%v", contract.CodexTurnStateAllowed, tc.want)
			}
		})
	}
}

func TestResponseMetadataIsPersistedPerAttempt(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, omitFinal := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/omit=%v", stream, omitFinal), func(t *testing.T) {
				f := newV2Fixture(t)
				f.publish(func(s *domain.CompiledSnapshot) {
					for _, target := range s.Targets {
						target.Capabilities.Priority = true
					}
				})
				actual, tier := "served-final", "priority"
				if omitFinal {
					actual, tier = "", ""
				}
				u := domain.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 800, CacheWriteTokens: 150, CacheSubsetOfInput: true}
				if stream {
					f.calls["p1-a"].streamEvents = []domain.StreamEvent{
						{Kind: domain.EvStart, Model: "failed-model", ServiceTier: "flex"},
						{Kind: domain.EvError, Err: &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection}},
					}
					f.calls["p1-b"].streamEvents = []domain.StreamEvent{
						{Kind: domain.EvStart, Model: actual, ServiceTier: tier},
						{Kind: domain.EvTextDelta, TextDelta: "ok"},
						{Kind: domain.EvEnd, FinishReason: "stop", Usage: &u},
					}
				} else {
					f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection}
					f.calls["p1-b"].invokeResponse = &domain.Response{Model: actual, ServiceTier: tier, Content: []domain.Block{{Kind: domain.BlockText, Text: "ok"}}, FinishReason: "stop", Usage: u}
				}
				rc := chatReq("smart")
				rc.ServiceTier = " FAST "
				rc.Stream = stream
				result, failure := f.sch.Execute(context.Background(), rc, RequestMeta{BearerToken: "sk"})
				if failure != nil {
					t.Fatal(failure)
				}
				if stream {
					var end domain.StreamEvent
					for ev := range result.Events {
						if ev.Err != nil {
							t.Fatal(ev.Err)
						}
						if ev.Kind == domain.EvEnd {
							end = ev
						}
					}
					if end.Model != actual || end.ServiceTier != tier {
						t.Fatalf("client terminal=%+v", end)
					}
				}
				if rc.ServiceTier != " FAST " {
					t.Fatal("scheduler mutated caller contract")
				}
				db := f.ledger.reopen(t)
				records, _, err := db.QueryRequests(domain.LogFilter{})
				if err != nil || len(records) != 1 {
					t.Fatalf("records=%v err=%v", records, err)
				}
				record := records[0]
				if record.ResponseModel != actual || record.ResponseServiceTier != tier || record.ServiceTier != "fast" || record.UpstreamModel != "upstream" || !record.Usage.CacheSubsetOfInput || record.Usage.UncachedInput() != 50 {
					t.Fatalf("request=%+v", record)
				}
				attempts, err := db.QueryAttempts(record.ID)
				if err != nil || len(attempts) != 2 {
					t.Fatalf("attempts=%v err=%v", attempts, err)
				}
				if attempts[1].ResponseModel != actual || attempts[1].ResponseServiceTier != tier || attempts[1].ServiceTier != "fast" || attempts[1].Model != "upstream" || attempts[1].Usage != u {
					t.Fatalf("final attempt=%+v", attempts[1])
				}
				if stream && (attempts[0].ResponseModel != "failed-model" || attempts[0].ResponseServiceTier != "flex") {
					t.Fatalf("failed metadata lost: %+v", attempts[0])
				}
			})
		}
	}
}

func TestInvalidServiceTierNeverReachesScheduling(t *testing.T) {
	f := newV2Fixture(t)
	rc := chatReq("smart")
	rc.ServiceTier = "ultrafast"
	if _, failure := f.sch.Execute(context.Background(), rc, RequestMeta{BearerToken: "sk"}); failure == nil || failure.Type != domain.FailUnsupportedParam {
		t.Fatalf("execute failure=%v", failure)
	}
	if _, failure := f.sch.DryRunV2(rc, RequestMeta{BearerToken: "sk"}); failure == nil || failure.Type != domain.FailUnsupportedParam {
		t.Fatalf("dry run failure=%v", failure)
	}
	for id, caller := range f.calls {
		if caller.invokes.Load() != 0 || caller.streams.Load() != 0 {
			t.Fatalf("invalid tier called %s", id)
		}
	}
}
