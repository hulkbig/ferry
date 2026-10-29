package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/conversation"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/runtime"
)

type v2Fixture struct {
	sch     *Scheduler
	ledger  *tledger
	manager *runtime.Manager
	calls   map[domain.ResourceTargetID]*fakeCaller
}

func conversationMeta(raw string) RequestMeta {
	return RequestMeta{
		BearerToken: "sk",
		AffinityEvidence: []domain.AffinityEvidence{
			domain.NewAffinityEvidence("test.session", domain.AffinityConversation, raw, domain.ConfHigh),
		},
	}
}

func newV2Fixture(t *testing.T) *v2Fixture {
	t.Helper()
	manager := runtime.NewManager(runtime.DefaultHealthConfig())
	led := openTempLedger(t)
	fixture := &v2Fixture{
		sch: New(manager, led.Ledger), ledger: led, manager: manager,
		calls: map[domain.ResourceTargetID]*fakeCaller{"p1-a": {}, "p1-b": {}, "p2-a": {}},
	}
	fixture.publish(nil)
	return fixture
}

func (f *v2Fixture) publish(mutate func(*domain.CompiledSnapshot)) {
	endpoint := func(id string) *domain.Endpoint {
		return &domain.Endpoint{ID: id, BaseURL: "http://127.0.0.1:1", Protocol: domain.ProtocolOpenAIChat}
	}
	target := func(id, provider, connection string) *domain.CompiledTarget {
		return &domain.CompiledTarget{
			ID: domain.ResourceTargetID(id), Endpoint: endpoint(id), Model: "upstream",
			Plan:         &domain.Plan{ID: "free", Kind: domain.PlanFree},
			Capabilities: domain.Capabilities{Text: true, Stream: true, Tools: true},
			ProviderID:   provider, ConnectionID: connection, Weight: 1,
			NativeProtocols: []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses},
		}
	}
	snap := &domain.CompiledSnapshot{
		Version: 2, Hash: "v2hash", CreatedAt: time.Now(), V2DomainSource: config.V2DomainSourceNative,
		Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{
			"p1-a": target("p1-a", "provider-1", "connection-a"),
			"p1-b": target("p1-b", "provider-1", "connection-b"),
			"p2-a": target("p2-a", "provider-2", "connection-c"),
		},
		TargetOrder: []domain.ResourceTargetID{"p1-a", "p1-b", "p2-a"},
		Providers: map[string]*domain.Provider{
			"provider-1": {ID: "provider-1", Enabled: true},
			"provider-2": {ID: "provider-2", Enabled: true},
		},
		Connections: map[string]*domain.Connection{
			"connection-a": {ID: "connection-a", ProviderID: "provider-1", Enabled: true},
			"connection-b": {ID: "connection-b", ProviderID: "provider-1", Enabled: true},
			"connection-c": {ID: "connection-c", ProviderID: "provider-2", Enabled: true},
		},
		LogicalModels: map[string]*domain.LogicalModel{
			"smart": {ID: "smart", Enabled: true, DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{
				{ProviderID: "provider-1", UpstreamModel: "upstream", RecoverOn: []domain.FailureType{
					domain.FailAuth, domain.FailRateLimit, domain.FailConnect, domain.FailProvider5xx,
				}},
				{ProviderID: "provider-2", UpstreamModel: "upstream"},
			}}},
		},
		Aliases:    map[string]*domain.CompiledAlias{},
		Objectives: map[string]*domain.ObjectiveProfile{},
		Policies:   map[string]*domain.CompiledPolicy{},
	}
	if mutate != nil {
		mutate(snap)
	}
	callers := map[domain.ResourceTargetID]egress.Caller{}
	for id, caller := range f.calls {
		callers[id] = caller
	}
	enabled := true
	f.sch.ReplaceWorld(snap, callers, nil, []config.ConsumerKey{{Name: "caller", Key: "sk", Enabled: &enabled}})
}

func (f *v2Fixture) records(t *testing.T) []domain.RequestRecord {
	t.Helper()
	db := f.ledger.reopen(t)
	records, _, err := db.QueryRequests(domain.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestV2DryRunHonorsSimulatedMinimumOutput(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities.MaxOutput = 4096
		}
	})
	if _, failure := f.sch.DryRunV2(chatReq("smart"), RequestMeta{
		BearerToken: "sk", RequiredCapabilities: &domain.Requirements{MinOutput: 8192},
	}); failure == nil {
		t.Fatal("minimum output above every target limit should make the dry run fail")
	}
	if _, failure := f.sch.DryRunV2(chatReq("smart"), RequestMeta{
		BearerToken: "sk", RequiredCapabilities: &domain.Requirements{MinOutput: 2048},
	}); failure != nil {
		t.Fatalf("supported minimum output rejected: %v", failure)
	}
}

func TestV2ConnectionFailureRebindsBeforeRouteFallback(t *testing.T) {
	f := newV2Fixture(t)
	f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection}
	result, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-b" || result.Outcome != domain.OutcomeRecoveredSuccess {
		t.Fatalf("result = %+v", result)
	}
	if f.calls["p1-a"].invokes.Load() != 1 || f.calls["p1-b"].invokes.Load() != 1 || f.calls["p2-a"].invokes.Load() != 0 {
		t.Fatalf("calls a=%d b=%d fallback=%d", f.calls["p1-a"].invokes.Load(), f.calls["p1-b"].invokes.Load(), f.calls["p2-a"].invokes.Load())
	}
	db := f.ledger.reopen(t)
	records, _, err := db.QueryRequests(domain.LogFilter{})
	if err != nil || len(records) != 1 {
		t.Fatalf("query records: n=%d err=%v", len(records), err)
	}
	record := records[0]
	if record.Outcome != domain.OutcomeRecoveredSuccess || record.ConnectionRebindCount != 1 || record.RouteFallbackCount != 0 {
		t.Fatalf("request record = %+v", record)
	}
	if record.LogicalModelID != "smart" || record.ProviderID != "provider-1" ||
		record.ConnectionID != "connection-b" || record.RuntimeRevision != 2 {
		t.Fatalf("V2 request identity = %+v", record)
	}
	trace, err := db.QueryTrace(record.ID)
	if err != nil || trace == nil {
		t.Fatalf("query trace: trace=%+v err=%v", trace, err)
	}
	wantOrder := []string{
		"REQUEST_RECEIVED", "MODEL_RESOLVED", "ROUTE_RESOLVED", "CONNECTION_SELECTED",
		"ATTEMPT_STARTED", "ATTEMPT_FAILED", "CONNECTION_REBOUND", "ATTEMPT_STARTED",
		"ATTEMPT_SUCCEEDED", "COMMIT", "COMPLETE",
	}
	position := 0
	for _, event := range trace.Events {
		if position < len(wantOrder) && event.Type == wantOrder[position] {
			position++
		}
		if strings.Contains(strings.ToLower(event.Type), "target") {
			t.Fatalf("trace leaked private target vocabulary: %+v", event)
		}
	}
	if position != len(wantOrder) {
		t.Fatalf("trace order matched %d/%d: %+v", position, len(wantOrder), trace.Events)
	}
}

func TestV2ProviderFailureAdvancesRouteWithoutTryingSiblingConnection(t *testing.T) {
	f := newV2Fixture(t)
	f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailProvider5xx, Scope: domain.FailureScopeProvider}
	result, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ProviderID != "provider-2" || f.calls["p1-b"].invokes.Load() != 0 || f.calls["p2-a"].invokes.Load() != 1 {
		t.Fatalf("result=%+v sibling=%d fallback=%d", result, f.calls["p1-b"].invokes.Load(), f.calls["p2-a"].invokes.Load())
	}
	if record := f.records(t)[0]; record.RouteFallbackCount != 1 || record.ConnectionRebindCount != 0 {
		t.Fatalf("request record = %+v", record)
	}
}

func TestV2ProtocolConversionFailureFallsBackBeforeCommit(t *testing.T) {
	t.Run("non-streaming response", func(t *testing.T) {
		f := newV2Fixture(t)
		f.calls["p1-a"].invokeResponse = &domain.Response{
			Content:          []domain.Block{{Kind: domain.BlockText, Text: "partial"}},
			Usage:            domain.Usage{InputTokens: 11, OutputTokens: 7},
			NativeProtocol:   domain.ProtocolOpenAIResponses,
			NativeBody:       []byte(`{"id":"native"}`),
			ConversionIssues: []string{"encrypted reasoning is native-only"},
		}
		result, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatal(failure)
		}
		if result.ProviderID != "provider-2" || result.Outcome != domain.OutcomeRecoveredSuccess ||
			f.calls["p1-b"].invokes.Load() != 0 || f.calls["p2-a"].invokes.Load() != 1 {
			t.Fatalf("conversion fallback result=%+v sibling=%d fallback=%d",
				result, f.calls["p1-b"].invokes.Load(), f.calls["p2-a"].invokes.Load())
		}
		db := f.ledger.reopen(t)
		records, _, err := db.QueryRequests(domain.LogFilter{})
		if err != nil || len(records) != 1 {
			t.Fatalf("requests=%d err=%v", len(records), err)
		}
		attempts, err := db.QueryAttempts(records[0].ID)
		if err != nil || len(attempts) != 2 {
			t.Fatalf("attempts=%d err=%v", len(attempts), err)
		}
		first := attempts[0]
		if first.Status != domain.ReqFailed || first.Failure == nil ||
			first.Failure.Type != domain.FailProtocolConversion ||
			first.Failure.Scope != domain.FailureScopeTarget || first.Usage.OutputTokens != 7 {
			t.Fatalf("conversion attempt was falsely recorded or lost billed usage: %+v", first)
		}
	})

	t.Run("stream before first visible event", func(t *testing.T) {
		f := newV2Fixture(t)
		f.calls["p1-a"].streamEvents = []domain.StreamEvent{{
			Kind: domain.EvSignatureDelta, SignatureDelta: "native-anthropic-signature",
		}}
		request := chatReq("smart")
		request.Stream = true
		result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatal(failure)
		}
		for range result.Events {
		}
		if result.ProviderID != "provider-2" || result.Outcome != domain.OutcomeRecoveredSuccess ||
			f.calls["p1-b"].streams.Load() != 0 || f.calls["p2-a"].streams.Load() != 1 {
			t.Fatalf("pre-commit stream conversion fallback result=%+v sibling=%d fallback=%d",
				result, f.calls["p1-b"].streams.Load(), f.calls["p2-a"].streams.Load())
		}
	})
}

func TestV2ThrottleAdmissionHonorsProviderWaitOrFailPolicy(t *testing.T) {
	t.Run("wait", func(t *testing.T) {
		f := newV2Fixture(t)
		f.publish(func(snapshot *domain.CompiledSnapshot) {
			snapshot.Connections["connection-b"].Enabled = false
		})
		f.manager.RecordFailure("p1-a", &domain.Failure{
			Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: 80 * time.Millisecond,
		}, time.Now())
		started := time.Now()
		result, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatal(failure)
		}
		if result.ConnectionID != "connection-a" || f.calls["p1-a"].invokes.Load() != 1 || time.Since(started) < 50*time.Millisecond {
			t.Fatalf("throttled primary did not wait: result=%+v calls=%d elapsed=%s", result, f.calls["p1-a"].invokes.Load(), time.Since(started))
		}
	})

	t.Run("fail fast and continue route", func(t *testing.T) {
		f := newV2Fixture(t)
		waitForThrottle := false
		f.publish(func(snapshot *domain.CompiledSnapshot) {
			snapshot.Connections["connection-b"].Enabled = false
			snapshot.Providers["provider-1"].Concurrency = &domain.ConcurrencyPolicy{QueueOnThrottle: &waitForThrottle}
		})
		f.manager.RecordFailure("p1-a", &domain.Failure{
			Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Second,
		}, time.Now())
		started := time.Now()
		result, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatal(failure)
		}
		if result.ProviderID != "provider-2" || f.calls["p1-a"].invokes.Load() != 0 || time.Since(started) > 200*time.Millisecond {
			t.Fatalf("fail-fast throttle did not advance route: result=%+v calls=%d elapsed=%s", result, f.calls["p1-a"].invokes.Load(), time.Since(started))
		}
	})
}

func TestV2CapabilityMismatchSkipsWithoutUpstreamAttempt(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snap *domain.CompiledSnapshot) {
		snap.Targets["p1-a"].Capabilities.Tools = false
	})
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	if _, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"}); failure != nil {
		t.Fatal(failure)
	}
	if f.calls["p1-a"].invokes.Load() != 0 || f.calls["p1-b"].invokes.Load() != 1 {
		t.Fatalf("capability skip attempts a=%d b=%d", f.calls["p1-a"].invokes.Load(), f.calls["p1-b"].invokes.Load())
	}
}

func TestV2RequestedCapabilitiesAreHardFallbackGates(t *testing.T) {
	tests := []struct {
		name    string
		missing func(*domain.Capabilities)
		request func() *domain.RequestContract
	}{
		{
			name:    "priority",
			missing: func(capabilities *domain.Capabilities) { capabilities.Priority = false },
			request: func() *domain.RequestContract {
				request := chatReq("smart")
				request.ServiceTier = "priority"
				return request
			},
		},
		{
			name:    "fast alias",
			missing: func(capabilities *domain.Capabilities) { capabilities.Priority = false },
			request: func() *domain.RequestContract {
				request := chatReq("smart")
				request.ServiceTier = " FAST "
				return request
			},
		},
		{
			name:    "reasoning",
			missing: func(capabilities *domain.Capabilities) { capabilities.Reasoning = false },
			request: func() *domain.RequestContract {
				request := chatReq("smart")
				request.Reasoning = &domain.ReasoningSpec{Effort: "high"}
				return request
			},
		},
		{
			name:    "json object",
			missing: func(capabilities *domain.Capabilities) { capabilities.StructuredOutput = false },
			request: func() *domain.RequestContract {
				request := chatReq("smart")
				request.ResponseFormat = &domain.ResponseFormat{}
				return request
			},
		},
		{
			name:    "vision",
			missing: func(capabilities *domain.Capabilities) { capabilities.Vision = false },
			request: func() *domain.RequestContract {
				request := chatReq("smart")
				request.Messages[0].Blocks = append(request.Messages[0].Blocks, domain.Block{Kind: domain.BlockImage, ImageMIME: "image/png"})
				return request
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newV2Fixture(t)
			f.publish(func(snapshot *domain.CompiledSnapshot) {
				for _, target := range snapshot.Targets {
					target.Capabilities = domain.Capabilities{
						Text: true, Stream: true, Tools: true, ParallelTools: true,
						Reasoning: true, StructuredOutput: true, Vision: true, Priority: true,
					}
				}
				test.missing(&snapshot.Targets["p1-a"].Capabilities)
			})
			request := test.request()
			if request.ResponseFormat != nil {
				f.calls["p1-b"].invokeResponse = &domain.Response{
					Content:      []domain.Block{{Kind: domain.BlockText, Text: `{"ok":true}`}},
					FinishReason: "stop",
					Model:        "upstream",
				}
			}
			result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
			if failure != nil {
				t.Fatal(failure)
			}
			if result.ConnectionID != "connection-b" || f.calls["p1-a"].invokes.Load() != 0 || f.calls["p1-b"].invokes.Load() != 1 {
				t.Fatalf("capability fallback result=%+v first=%d fallback=%d", result, f.calls["p1-a"].invokes.Load(), f.calls["p1-b"].invokes.Load())
			}
		})
	}
}

func TestV2StreamingCapabilityIsIndependentFromText(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Stream: true}
		}
		snapshot.Targets["p1-a"].Capabilities.Stream = false
	})

	nonStreamResult, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if nonStreamResult.ConnectionID != "connection-a" || f.calls["p1-a"].invokes.Load() != 1 {
		t.Fatalf("invoke-only target was excluded from a non-stream request: result=%+v calls=%d",
			nonStreamResult, f.calls["p1-a"].invokes.Load())
	}

	request := chatReq("smart")
	request.Stream = true
	streamResult, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	for range streamResult.Events {
	}
	if streamResult.ConnectionID != "connection-b" || f.calls["p1-a"].streams.Load() != 0 || f.calls["p1-b"].streams.Load() != 1 {
		t.Fatalf("stream request used invoke-only target: result=%+v first=%d fallback=%d",
			streamResult, f.calls["p1-a"].streams.Load(), f.calls["p1-b"].streams.Load())
	}
}

func TestV2ParallelToolsPreferenceMapsToSerialWithoutMutatingClientRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "invoke", true: "stream"}[stream], func(t *testing.T) {
			f := newV2Fixture(t)
			f.publish(func(snapshot *domain.CompiledSnapshot) {
				for _, target := range snapshot.Targets {
					target.Capabilities = domain.Capabilities{
						Text: true, Stream: true, Tools: true,
					}
				}
			})

			parallel := true
			native := json.RawMessage(`{"model":"smart","parallel_tool_calls":true}`)
			request := chatReq("smart")
			request.Stream = stream
			request.Origin = domain.ProtocolOpenAIChat
			request.Native = native
			request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
			request.ParallelTools = &parallel

			result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
			if failure != nil {
				t.Fatal(failure)
			}
			if result.ConnectionID != "connection-a" {
				t.Fatalf("serializable target was not selected: %+v", result)
			}
			attempt := f.calls["p1-a"].lastRequest()
			if attempt == nil || attempt.ParallelTools == nil || *attempt.ParallelTools {
				t.Fatalf("attempt did not explicitly disable parallel tools: %+v", attempt)
			}
			if attempt.Native != nil {
				t.Fatalf("same-protocol native replay could bypass serial mapping: %s", attempt.Native)
			}
			if request.ParallelTools == nil || !*request.ParallelTools || string(request.Native) != string(native) {
				t.Fatalf("client request was mutated: parallel=%v native=%s", request.ParallelTools, request.Native)
			}
		})
	}
}

func TestV2ExplicitParallelToolsRequirementUsesSerialMapping(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Stream: true, Tools: true, ParallelTools: true}
		}
		snapshot.Targets["p1-a"].Capabilities.ParallelTools = false
		snapshot.Connections["connection-b"].Enabled = false
	})
	parallel := true
	native := json.RawMessage(`{"model":"smart","parallel_tool_calls":true}`)
	request := chatReq("smart")
	request.Origin = domain.ProtocolOpenAIChat
	request.Native = native
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{
		BearerToken: "sk", RequiredCapabilities: &domain.Requirements{ParallelTools: true},
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-a" || result.CompatLevel != domain.CompatMapped || f.calls["p1-a"].invokes.Load() != 1 {
		t.Fatalf("explicit parallel-tools contract was not mapped: result=%+v first=%d", result, f.calls["p1-a"].invokes.Load())
	}
	attempt := f.calls["p1-a"].lastRequest()
	if attempt == nil || attempt.ParallelTools == nil || *attempt.ParallelTools || attempt.Native != nil {
		t.Fatalf("mapped attempt did not force serial tools: %+v", attempt)
	}
	if request.ParallelTools == nil || !*request.ParallelTools || string(request.Native) != string(native) {
		t.Fatalf("client request was mutated: parallel=%v native=%s", request.ParallelTools, request.Native)
	}
}

func TestV2LogicalModelExactOnlyRejectsSerialToolsMappingBeforeAttempt(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.LogicalModels["smart"].ExactOnly = true
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Stream: true, Tools: true}
		}
	})
	parallel := true
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel

	_, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure == nil || !strings.Contains(failure.Message, "exact_only") {
		t.Fatalf("exact-only mapping failure=%v", failure)
	}
	for id, caller := range f.calls {
		if caller.invokes.Load() != 0 || caller.streams.Load() != 0 {
			t.Fatalf("target %s was attempted despite exact-only: invokes=%d streams=%d", id, caller.invokes.Load(), caller.streams.Load())
		}
	}
}

func TestV2APIKeyExactOnlyRejectsSerialToolsMappingBeforeAttempt(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Stream: true, Tools: true}
		}
	})
	w := f.sch.world.Load()
	f.sch.ReplaceWorldWithAPIKeys(w.snap, w.callers, nil, []domain.APIKeyAuthenticator{{
		Identity: domain.APIKeyIdentity{ID: "caller-id", Name: "caller", Enabled: true, ExactOnly: true},
		SecretHashes: [][32]byte{
			domain.HashAPIKeySecret("sk"),
		},
	}})
	parallel := true
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel

	_, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure == nil || !strings.Contains(failure.Message, "exact_only") {
		t.Fatalf("exact-only mapping failure=%v", failure)
	}
	for id, caller := range f.calls {
		if caller.invokes.Load() != 0 || caller.streams.Load() != 0 {
			t.Fatalf("target %s was attempted despite exact-only: invokes=%d streams=%d", id, caller.invokes.Load(), caller.streams.Load())
		}
	}
}

func TestV2ExactOnlyAcceptsNativeExactTarget(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.LogicalModels["smart"].ExactOnly = true
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Stream: true, Tools: true, ParallelTools: true}
		}
		snapshot.Targets["p1-a"].Capabilities.ParallelTools = false
	})
	parallel := true
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel

	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-b" || result.CompatLevel != domain.CompatExact {
		t.Fatalf("strict request did not select native exact target: %+v", result)
	}
}

func TestV2ExactOnlyRejectsLosslessCrossProtocolMapping(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.LogicalModels["smart"].ExactOnly = true
		for _, target := range snapshot.Targets {
			target.NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
		}
	})

	_, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure == nil || !strings.Contains(failure.Message, "protocol conversion required") ||
		!strings.Contains(failure.Message, "exact_only") {
		t.Fatalf("cross-protocol exact-only failure=%v", failure)
	}
	for id, caller := range f.calls {
		if caller.invokes.Load() != 0 || caller.streams.Load() != 0 {
			t.Fatalf("target %s was attempted despite exact-only: invokes=%d streams=%d", id, caller.invokes.Load(), caller.streams.Load())
		}
	}
}

func TestV2NativeParallelToolsIsPreferredOverSerialMapping(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Stream: true, Tools: true, ParallelTools: true}
		}
		snapshot.Targets["p1-a"].Capabilities.ParallelTools = false
	})
	parallel := true
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{
		BearerToken: "sk", RequiredCapabilities: &domain.Requirements{ParallelTools: true},
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-b" || result.CompatLevel != domain.CompatExact || f.calls["p1-a"].invokes.Load() != 0 {
		t.Fatalf("native parallel-tools target was not preferred: result=%+v mapped=%d", result, f.calls["p1-a"].invokes.Load())
	}
}

func TestV2ConvertedNativeParallelToolsIsPreferredOverNativeProtocolSerialMapping(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Stream: true, Tools: true, ParallelTools: true}
		}
		snapshot.Targets["p1-a"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIChat}
		snapshot.Targets["p1-a"].Capabilities.ParallelTools = false
		snapshot.Targets["p1-b"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
	})
	parallel := true
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{
		BearerToken: "sk", RequiredCapabilities: &domain.Requirements{ParallelTools: true},
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-b" || result.CompatLevel != domain.CompatMapped || f.calls["p1-a"].invokes.Load() != 0 {
		t.Fatalf("converted native parallel-tools target was not preferred: result=%+v mapped=%d", result, f.calls["p1-a"].invokes.Load())
	}
}

func TestV2SimulationCapabilityRequirementSelectsMatchingOverride(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities.ParallelTools = true
		}
		model := snapshot.LogicalModels["smart"]
		model.Overrides = []domain.OverrideSpec{{
			ID: "parallel-tools-route", Enabled: true, Mode: domain.OverrideCustomRoute,
			Required: domain.Requirements{ParallelTools: true},
			Route: &domain.RouteSpec{Steps: []domain.RouteStep{{
				ProviderID: "provider-2", UpstreamModel: "upstream",
			}}},
		}}
	})

	simulation, failure := f.sch.DryRunV2(chatReq("smart"), RequestMeta{
		BearerToken: "sk", RequiredCapabilities: &domain.Requirements{ParallelTools: true},
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if simulation.OverrideID != "parallel-tools-route" || simulation.Plan.Primary != "p2-a" {
		t.Fatalf("simulation capability contract did not select override: %+v", simulation)
	}
}

func TestV2RoutingUsesEgressProtocolCapabilityEvidence(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Tools: true}
			target.ProtocolCapabilities = map[domain.Protocol]domain.Capabilities{
				domain.ProtocolOpenAIChat:      {Text: true, Tools: true},
				domain.ProtocolOpenAIResponses: {Text: true, Tools: true},
			}
		}
		chat := snapshot.Targets["p1-a"].ProtocolCapabilities[domain.ProtocolOpenAIChat]
		chat.Tools = false
		snapshot.Targets["p1-a"].ProtocolCapabilities[domain.ProtocolOpenAIChat] = chat
	})
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-b" || f.calls["p1-a"].invokes.Load() != 0 {
		t.Fatalf("V2 routing used aggregate instead of chat evidence: result=%+v first=%d", result, f.calls["p1-a"].invokes.Load())
	}
}

func TestV2ImagesCapabilityIsIndependentFromTextAndVision(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Endpoint.Protocol = domain.ProtocolOpenAIImages
			target.NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIImages}
			target.Capabilities = domain.Capabilities{Images: true}
		}
		snapshot.Targets["p1-a"].Capabilities.Images = false
	})
	request := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: "smart", Operation: "generation",
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "draw a cat"}}}},
	}
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-b" || f.calls["p1-a"].invokes.Load() != 0 || f.calls["p1-b"].invokes.Load() != 1 {
		t.Fatalf("Images capability fallback result=%+v first=%d fallback=%d", result,
			f.calls["p1-a"].invokes.Load(), f.calls["p1-b"].invokes.Load())
	}
}

func TestV2NoCandidateSurfacesRuntimeCauseInsteadOfFakeQuota(t *testing.T) {
	f := newV2Fixture(t)
	now := time.Now()
	for _, target := range []domain.ResourceTargetID{"p1-a", "p1-b", "p2-a"} {
		f.manager.RecordFailure(target, &domain.Failure{
			Type: domain.FailConnect, Scope: domain.FailureScopeConnection,
		}, now)
	}
	if _, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"}); failure == nil || failure.Type != domain.FailConnect || failure.Scope != domain.FailureScopeRequest {
		t.Fatalf("runtime no-candidate classification=%+v", failure)
	}

	f = newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities.Tools = false
		}
	})
	request := chatReq("smart")
	request.Tools = []domain.ToolDef{{Name: "tool"}}
	if _, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"}); failure == nil || failure.Type != domain.FailConnect {
		t.Fatalf("unknown capability must not claim unsupported: %+v", failure)
	}
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities.Tools = false
			target.ProtocolCapabilityStatuses = map[domain.Protocol]domain.CapabilityStatuses{
				domain.ProtocolOpenAIChat: {domain.CapabilityTools: domain.CapabilityUnsupported},
			}
		}
	})
	if _, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"}); failure == nil || failure.Type != domain.FailUnsupportedParam {
		t.Fatalf("capability no-candidate classification=%+v", failure)
	}
}

func TestV2RequestFailureFailsClosed(t *testing.T) {
	f := newV2Fixture(t)
	f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailUnsupportedParam, Scope: domain.FailureScopeRequest}
	if _, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"}); failure == nil {
		t.Fatal("request failure unexpectedly recovered")
	}
	if f.calls["p1-b"].invokes.Load() != 0 || f.calls["p2-a"].invokes.Load() != 0 {
		t.Fatal("request-scoped failure must fail closed")
	}
}

func TestV2TargetSpecificUnsupportedParameterFallsBack(t *testing.T) {
	f := newV2Fixture(t)
	f.calls["p1-a"].connect = &domain.Failure{
		Type: domain.FailUnsupportedParam, Scope: domain.FailureScopeTarget,
		Message: "temperature only supports 1",
	}
	result, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ProviderID != "provider-2" || f.calls["p1-b"].invokes.Load() != 0 ||
		f.calls["p2-a"].invokes.Load() != 1 {
		t.Fatalf("target-specific unsupported parameter did not fallback: result=%+v", result)
	}
}

func TestV2ConnectionRankingUsesFairnessWithoutConfiguredWeights(t *testing.T) {
	f := newV2Fixture(t)
	f.manager.Enter("p1-a")
	defer f.manager.Exit("p1-a")
	result, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-b" || f.calls["p1-a"].invokes.Load() != 0 {
		t.Fatalf("fairness selection result=%+v first calls=%d", result, f.calls["p1-a"].invokes.Load())
	}
}

func TestV2ConnectionRankingPrefersNativeProtocolBeforeConvertedSibling(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolAnthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			f := newV2Fixture(t)
			f.publish(func(snap *domain.CompiledSnapshot) {
				snap.Targets["p1-a"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
				snap.Targets["p1-b"].NativeProtocols = []domain.Protocol{protocol}
			})
			// Make the request-protocol-native connection less attractive on
			// fairness. Exact native compatibility still wins per request; the
			// Responses connection remains a conversion fallback.
			f.manager.Enter("p1-b")
			defer f.manager.Exit("p1-b")
			request := chatReq("smart")
			request.Protocol, request.Origin = protocol, protocol
			result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
			if failure != nil {
				t.Fatal(failure)
			}
			if result.ConnectionID != "connection-b" || f.calls["p1-a"].invokes.Load() != 0 {
				t.Fatalf("%s native protocol was not preferred: result=%+v converted calls=%d", protocol, result, f.calls["p1-a"].invokes.Load())
			}
		})
	}
}

func TestV2ConnectionRankingUsesConvertedSiblingWhenRequestProtocolNativeIsUnavailable(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snap *domain.CompiledSnapshot) {
		snap.Targets["p1-a"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
		snap.Targets["p1-b"].NativeProtocols = []domain.Protocol{domain.ProtocolAnthropic}
		snap.Connections["connection-b"].Enabled = false
	})
	request := chatReq("smart")
	request.Protocol, request.Origin = domain.ProtocolAnthropic, domain.ProtocolAnthropic
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ConnectionID != "connection-a" || f.calls["p1-b"].invokes.Load() != 0 {
		t.Fatalf("converted fallback was not used: result=%+v native calls=%d", result, f.calls["p1-b"].invokes.Load())
	}
}

func TestV2ChatReasoningReachesResponsesTargetInsteadOfFailingPreflight(t *testing.T) {
	f := newV2Fixture(t)
	codex := egress.NewCodexCaller(egress.Base{Model: "upstream"}, "")
	f.calls["p1-a"].prepare = codex.Prepare
	f.publish(func(snap *domain.CompiledSnapshot) {
		snap.Targets["p1-a"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
		snap.Targets["p1-a"].Capabilities = domain.Capabilities{
			Text: true, Stream: true, Tools: true, ParallelTools: true, Reasoning: true,
		}
		snap.Connections["connection-b"].Enabled = false
		snap.Connections["connection-c"].Enabled = false
	})

	parallel := true
	request := chatReq("smart")
	request.Origin = domain.ProtocolOpenAIChat
	request.Stream = true
	request.Reasoning = &domain.ReasoningSpec{Effort: "high"}
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
	request.ParallelTools = &parallel
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatalf("Chat reasoning request was rejected before Codex attempt: %v", failure)
	}
	for range result.Events {
	}
	if f.calls["p1-a"].streams.Load() != 1 {
		t.Fatalf("Responses target attempts=%d, want 1", f.calls["p1-a"].streams.Load())
	}
	if result.ConnectionID != "connection-a" || result.CompatLevel != domain.CompatMapped {
		t.Fatalf("converted Codex result=%+v", result)
	}
}

func TestV2ChatReasoningToolContinuationReachesCodexWithSessionEvidence(t *testing.T) {
	f := newV2Fixture(t)
	codex := egress.NewCodexCaller(egress.Base{Model: "upstream"}, "")
	f.calls["p1-a"].prepare = codex.Prepare
	f.publish(func(snap *domain.CompiledSnapshot) {
		snap.Targets["p1-a"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
		snap.Targets["p1-a"].Capabilities = domain.Capabilities{
			Text: true, Stream: true, Tools: true, ParallelTools: true, Reasoning: true,
		}
		snap.Connections["connection-b"].Enabled = false
		snap.Connections["connection-c"].Enabled = false
	})

	request := chatReq("smart")
	request.Origin = domain.ProtocolOpenAIChat
	request.Reasoning = &domain.ReasoningSpec{Effort: "high"}
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
	request.Messages = append(request.Messages,
		domain.Message{Role: domain.RoleAssistant, Blocks: []domain.Block{
			{Kind: domain.BlockThinking, Text: "brief"},
			{Kind: domain.BlockToolUse, ID: "call_1", Name: "lookup", Input: json.RawMessage(`{}`)},
		}},
		domain.Message{Role: domain.RoleTool, ToolID: "call_1", Blocks: []domain.Block{{
			Kind: domain.BlockToolResult, ID: "call_1", Result: json.RawMessage(`"ok"`),
		}}},
	)
	result, failure := f.sch.Execute(context.Background(), request, conversationMeta("hermes-tool-session"))
	if failure != nil {
		t.Fatalf("Chat Codex tool continuation was rejected before attempt: %v", failure)
	}
	if f.calls["p1-a"].invokes.Load() != 1 || result.ConnectionID != "connection-a" {
		t.Fatalf("Codex continuation result=%+v attempts=%d", result, f.calls["p1-a"].invokes.Load())
	}
	if sent := f.calls["p1-a"].lastRequest(); sent == nil || sent.ProviderSessionKey == "" {
		t.Fatalf("Codex continuation lost its stable provider session: %+v", sent)
	}
}

func TestV2ActiveTrafficLifecycleUsesProductIdentities(t *testing.T) {
	f := newV2Fixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	f.calls["p1-a"].invokeStarted = started
	f.calls["p1-a"].invokeBlock = release
	done := make(chan *domain.Failure, 1)
	go func() {
		_, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk"})
		done <- failure
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream attempt did not start")
	}
	active := f.sch.ActiveTraffic()
	if len(active) != 1 || active[0].LogicalModelID != "smart" ||
		active[0].ProviderID != "provider-1" || active[0].ConnectionID != "connection-a" ||
		active[0].Phase != TrafficActive || active[0].RuntimeRevision != 2 {
		t.Fatalf("active traffic=%+v", active)
	}
	close(release)
	if failure := <-done; failure != nil {
		t.Fatal(failure)
	}
	if active := f.sch.ActiveTraffic(); len(active) != 0 {
		t.Fatalf("completed request remained active: %+v", active)
	}
}

func TestV2StreamingClientCancellationReleasesAdmission(t *testing.T) {
	f := newV2Fixture(t)
	gate := make(chan struct{})
	streamStarted := make(chan struct{})
	f.calls["p1-a"].streamGate = gate
	f.calls["p1-a"].streamStarted = streamStarted
	request := chatReq("smart")
	request.Stream = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type execution struct {
		result  *Result
		failure *domain.Failure
	}
	done := make(chan execution, 1)
	go func() {
		result, failure := f.sch.Execute(ctx, request, RequestMeta{BearerToken: "sk"})
		done <- execution{result: result, failure: failure}
	}()
	select {
	case <-streamStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream stream did not start")
	}

	var result *Result
	select {
	case completed := <-done:
		if completed.failure != nil {
			t.Fatal(completed.failure)
		}
		result = completed.result
	case <-time.After(time.Second):
		t.Fatal("stream response was not committed")
	}
	if result == nil {
		t.Fatal("stream response is nil")
	}
	if inFlight := f.manager.ConnectionStates(time.Now())["connection-a"].InFlight; inFlight != 1 {
		t.Fatalf("in-flight before client cancellation = %d, want 1", inFlight)
	}

	cancel()
	close(gate)
	for range result.Events {
	}
	if inFlight := f.manager.ConnectionStates(time.Now())["connection-a"].InFlight; inFlight != 0 {
		t.Fatalf("in-flight after client cancellation = %d, want 0", inFlight)
	}
	if active := f.sch.ActiveTraffic(); len(active) != 0 {
		t.Fatalf("canceled request remained active: %+v", active)
	}
}

func TestV2StreamingPreCommitCanRebindButPostCommitCannotSwitch(t *testing.T) {
	t.Run("pre-commit", func(t *testing.T) {
		f := newV2Fixture(t)
		f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailConnect, Scope: domain.FailureScopeConnection}
		request := chatReq("smart")
		request.Stream = true
		result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatal(failure)
		}
		for range result.Events {
		}
		if result.ConnectionID != "connection-b" || result.Outcome != domain.OutcomeRecoveredSuccess ||
			f.calls["p1-b"].streams.Load() != 1 {
			t.Fatalf("pre-commit result=%+v streams=%d", result, f.calls["p1-b"].streams.Load())
		}
	})

	t.Run("post-commit", func(t *testing.T) {
		f := newV2Fixture(t)
		f.calls["p1-a"].midErr = &domain.Failure{Type: domain.FailProvider5xx, Scope: domain.FailureScopeProvider}
		request := chatReq("smart")
		request.Stream = true
		result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatal(failure)
		}
		gotError := false
		for event := range result.Events {
			gotError = gotError || event.Kind == domain.EvError
		}
		if !gotError || f.calls["p1-b"].streams.Load() != 0 || f.calls["p2-a"].streams.Load() != 0 {
			t.Fatalf("post-commit switched target or hid terminal error: error=%v b=%d fallback=%d",
				gotError, f.calls["p1-b"].streams.Load(), f.calls["p2-a"].streams.Load())
		}
	})

	t.Run("clean eof without terminal is broken", func(t *testing.T) {
		f := newV2Fixture(t)
		f.calls["p1-a"].streamEvents = []domain.StreamEvent{{Kind: domain.EvTextDelta, TextDelta: "partial"}}
		request := chatReq("smart")
		request.Stream = true
		result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatal(failure)
		}
		var terminal *domain.Failure
		for event := range result.Events {
			if event.Kind == domain.EvError {
				terminal = event.Err
			}
		}
		if terminal == nil || terminal.Type != domain.FailStreamBroken ||
			f.calls["p1-b"].streams.Load() != 0 || f.calls["p2-a"].streams.Load() != 0 {
			t.Fatalf("unterminated committed stream was hidden or switched: failure=%+v", terminal)
		}
	})
}

func TestV2AffinityHitAvoidsRerankingAndHardFailureRebinds(t *testing.T) {
	f := newV2Fixture(t)
	meta := conversationMeta("conversation-1")
	first, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil || first.ConnectionID != "connection-a" {
		t.Fatalf("first=%+v failure=%v", first, failure)
	}

	// Fairness would now rank B first, but a still-eligible conversation
	// binding bypasses ranking.
	f.manager.Enter("p1-a")
	second, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	f.manager.Exit("p1-a")
	if failure != nil || second.ConnectionID != "connection-a" {
		t.Fatalf("affinity was reranked: result=%+v failure=%v", second, failure)
	}

	// A hard connection failure invalidates the binding and rebinds to B.
	f.calls["p1-a"].connect = &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection}
	third, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil || third.ConnectionID != "connection-b" {
		t.Fatalf("hard failure did not rebind: result=%+v failure=%v", third, failure)
	}
	f.calls["p1-a"].connect = nil
	f.manager.RecordSuccess("p1-a", 1, 0, domain.Usage{}, 0, 0, time.Now())
	fourth, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil || fourth.ConnectionID != "connection-b" {
		t.Fatalf("conversation proactively failed back: result=%+v failure=%v", fourth, failure)
	}
}

func TestV2ConversationIdentityStabilizesProviderSessionAcrossChatTurns(t *testing.T) {
	f := newV2Fixture(t)
	meta := conversationMeta("hermes-session-1")
	first, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil {
		t.Fatal(failure)
	}
	firstRequest := f.calls[first.Target].lastRequest()
	if firstRequest == nil || firstRequest.ProviderSessionKey == "" {
		t.Fatalf("first attempt has no provider session identity: %+v", firstRequest)
	}
	second, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil {
		t.Fatal(failure)
	}
	secondRequest := f.calls[second.Target].lastRequest()
	if secondRequest == nil || secondRequest.ProviderSessionKey != firstRequest.ProviderSessionKey {
		t.Fatalf("provider session changed across Chat turns: first=%+v second=%+v", firstRequest, secondRequest)
	}
}

func TestV2CallerAffinityTraceCarriesScopeSourceAndProtocol(t *testing.T) {
	f := newV2Fixture(t)
	f.calls["p2-a"].notes = []string{"reasoning state is not lossless"}
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.LogicalModels["smart"].ExactOnly = true
		snapshot.Targets["p2-a"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
	})
	meta := RequestMeta{BearerToken: "sk"}
	if _, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta); failure != nil {
		t.Fatal(failure)
	}
	second, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil {
		t.Fatal(failure)
	}

	db := f.ledger.reopen(t)
	trace, err := db.QueryTrace(second.LedgerRequestID)
	if err != nil || trace == nil {
		t.Fatalf("query trace: trace=%+v err=%v", trace, err)
	}
	found, reused, excluded, selected := false, false, false, false
	for _, event := range trace.Events {
		switch event.Type {
		case "TARGET_SKIPPED":
			if event.ConnectionID == "connection-c" {
				excluded = true
				if event.RouteStep != 2 || event.Protocol != domain.ProtocolOpenAIResponses {
					t.Fatalf("candidate exclusion lacks route evidence: %+v", event)
				}
			}
		case "CONNECTION_SELECTED":
			selected = true
			if event.RouteStep != 1 || event.ConnectionID != "connection-a" || event.Protocol != domain.ProtocolOpenAIChat {
				t.Fatalf("connection selection lacks route evidence: %+v", event)
			}
		case "AFFINITY_FOUND", "AFFINITY_REUSED":
			if event.AffinityScope != domain.AffinityCaller || event.AffinitySource != "api_key_identity" ||
				event.ConnectionID != "connection-a" || event.Protocol != domain.ProtocolOpenAIChat {
				t.Fatalf("affinity event lacks public evidence: %+v", event)
			}
			found = found || event.Type == "AFFINITY_FOUND"
			reused = reused || event.Type == "AFFINITY_REUSED"
		}
	}
	if !found || !reused || !excluded || !selected {
		t.Fatalf("routing trace incomplete: %+v", trace.Events)
	}
}

func TestV2ConversationDoesNotProactivelyFailBackToEarlierProvider(t *testing.T) {
	f := newV2Fixture(t)
	meta := conversationMeta("conversation-provider-2")
	now := time.Now()
	for _, target := range []domain.ResourceTargetID{"p1-a", "p1-b"} {
		f.manager.RecordFailure(target, &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection}, now)
	}
	first, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil || first.ProviderID != "provider-2" {
		t.Fatalf("migration result=%+v failure=%v", first, failure)
	}
	for _, target := range []domain.ResourceTargetID{"p1-a", "p1-b"} {
		f.manager.RecordSuccess(target, 1, 0, domain.Usage{}, 0, 0, time.Now())
	}
	second, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
	if failure != nil || second.ProviderID != "provider-2" {
		t.Fatalf("conversation proactively failed back: result=%+v failure=%v", second, failure)
	}
}

func TestExistingConversationStrategyAfterRouteRevision(t *testing.T) {
	for _, test := range []struct {
		name     string
		strategy domain.ExistingConversationStrategy
		want     string
	}{
		{name: "keep existing", strategy: domain.ConversationKeepExisting, want: "provider-1"},
		{name: "migrate", strategy: domain.ConversationMigrate, want: "provider-2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newV2Fixture(t)
			meta := conversationMeta("conversation-route-revision")
			first, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
			if failure != nil || first.ProviderID != "provider-1" {
				t.Fatalf("first=%+v failure=%v", first, failure)
			}
			f.publish(func(snapshot *domain.CompiledSnapshot) {
				snapshot.Version = 3
				model := snapshot.LogicalModels["smart"]
				model.ExistingConversationStrategy = test.strategy
				model.DefaultRoute.Steps = []domain.RouteStep{
					{ProviderID: "provider-2", UpstreamModel: "upstream", RecoverOn: []domain.FailureType{domain.FailConnect}},
					{ProviderID: "provider-1", UpstreamModel: "upstream"},
				}
			})
			second, failure := f.sch.Execute(context.Background(), chatReq("smart"), meta)
			if failure != nil || second.ProviderID != test.want {
				t.Fatalf("strategy=%s result=%+v failure=%v", test.strategy, second, failure)
			}
			records := f.records(t)
			var latest domain.RequestRecord
			for _, record := range records {
				if record.ID == second.LedgerRequestID {
					latest = record
				}
			}
			if latest.RuntimeRevision != 3 {
				t.Fatalf("latest request did not capture applied revision: %+v", latest)
			}
		})
	}
}

func TestResponsesVirtualizationNativeContinuationRehydrateAndNoFailback(t *testing.T) {
	f := newV2Fixture(t)
	store, err := conversation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f.sch.SetConversationStore(store)
	f.calls["p1-a"].nativeResponseID = "resp_native_1"

	firstRequest := chatReq("smart")
	firstRequest.Protocol = domain.ProtocolOpenAIResponses
	firstRequest.Origin = domain.ProtocolOpenAIResponses
	first, failure := f.sch.Execute(context.Background(), firstRequest, RequestMeta{BearerToken: "sk"})
	if failure != nil || first.ContinuityMode != domain.ContinuityNone ||
		!strings.HasPrefix(first.RequestID, "resp_smart_") {
		t.Fatalf("first response=%+v failure=%v", first, failure)
	}
	f.calls["p1-a"].nativeResponseID = "resp_native_2"
	secondRequest := chatReq("smart")
	secondRequest.Protocol = domain.ProtocolOpenAIResponses
	secondRequest.Origin = domain.ProtocolOpenAIResponses
	secondRequest.Lineage = &domain.ResponsesLineage{PreviousResponseID: first.RequestID}
	second, failure := f.sch.Execute(context.Background(), secondRequest, RequestMeta{BearerToken: "sk"})
	if failure != nil || second.ContinuityMode != domain.ContinuityNative {
		t.Fatalf("native continuation=%+v failure=%v", second, failure)
	}
	if sent := f.calls["p1-a"].lastRequest(); sent == nil || sent.Continuation == nil ||
		sent.Continuation.NativeResponseID != "resp_native_1" {
		t.Fatalf("native handle not translated: %+v", sent)
	}
	mapped, err := store.Response(second.RequestID)
	if err != nil || mapped == nil || mapped.ParentResponseID != first.RequestID || mapped.NativeResponseID != "resp_native_2" {
		t.Fatalf("virtual response mapping=%+v err=%v", mapped, err)
	}

	// Make every connection in provider 1 hard-ineligible. The continuation
	// moves to provider 2 and must be explicitly rehydrated.
	now := time.Now()
	for _, target := range []domain.ResourceTargetID{"p1-a", "p1-b"} {
		f.manager.RecordFailure(target, &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection}, now)
	}
	f.calls["p2-a"].nativeResponseID = "resp_native_p2"
	thirdRequest := chatReq("smart")
	thirdRequest.Protocol = domain.ProtocolOpenAIResponses
	thirdRequest.Origin = domain.ProtocolOpenAIResponses
	thirdRequest.Lineage = &domain.ResponsesLineage{PreviousResponseID: second.RequestID}
	third, failure := f.sch.Execute(context.Background(), thirdRequest, RequestMeta{BearerToken: "sk"})
	if failure != nil || third.ProviderID != "provider-2" || third.ContinuityMode != domain.ContinuityRehydrated ||
		third.Outcome != domain.OutcomeRecoveredSuccess {
		t.Fatalf("rehydrated response=%+v failure=%v", third, failure)
	}
	if sent := f.calls["p2-a"].lastRequest(); sent == nil || sent.Continuation != nil || len(sent.Messages) < 3 {
		t.Fatalf("canonical history was not rehydrated: %+v", sent)
	}

	for _, target := range []domain.ResourceTargetID{"p1-a", "p1-b"} {
		f.manager.RecordSuccess(target, 1, 0, domain.Usage{}, 0, 0, time.Now())
	}
	fourthRequest := chatReq("smart")
	fourthRequest.Protocol = domain.ProtocolOpenAIResponses
	fourthRequest.Origin = domain.ProtocolOpenAIResponses
	fourthRequest.Lineage = &domain.ResponsesLineage{PreviousResponseID: third.RequestID}
	fourth, failure := f.sch.Execute(context.Background(), fourthRequest, RequestMeta{BearerToken: "sk"})
	if failure != nil || fourth.ProviderID != "provider-2" {
		t.Fatalf("rehydrated conversation failed back: result=%+v failure=%v", fourth, failure)
	}
}

func TestResponsesThreadAffinityDoesNotRehydrateClientOwnedToolReplay(t *testing.T) {
	f := newV2Fixture(t)
	store, err := conversation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f.sch.SetConversationStore(store)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.Providers["provider-1"].Type = domain.ProviderCliproxyCodex
		snapshot.Connections["connection-b"].Enabled = false
		snapshot.Connections["connection-c"].Enabled = false
		snapshot.Targets["p1-a"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
		snapshot.Targets["p1-a"].Capabilities = domain.Capabilities{
			Text: true, Stream: true, Tools: true, ParallelTools: true, Reasoning: true,
		}
	})

	baseMessages := []domain.Message{
		{Role: domain.RoleSystem, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "instructions"}}},
		{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "environment"}}},
		{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "run the probe"}}},
	}
	toolCall := domain.Block{
		Kind: domain.BlockToolUse, ID: "call-probe", Name: "exec_command",
		Input: json.RawMessage(`{"cmd":"read input.txt"}`),
	}
	f.calls["p1-a"].invokeResponse = &domain.Response{
		Content: []domain.Block{toolCall}, FinishReason: "tool_call", Model: "upstream",
	}
	firstRequest := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIResponses, Model: "smart",
		Native:   json.RawMessage(`{"model":"smart","input":"run the probe","store":false}`),
		Messages: append([]domain.Message(nil), baseMessages...),
		Tools:    []domain.ToolDef{{Name: "exec_command", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Lineage:  &domain.ResponsesLineage{PromptCacheKey: "codex-thread-cache"},
	}
	meta := conversationMeta("codex-thread")
	first, failure := f.sch.Execute(context.Background(), firstRequest, meta)
	if failure != nil || first.ConversationID == "" || first.ContinuityMode != domain.ContinuityNone {
		t.Fatalf("first=%+v failure=%v", first, failure)
	}

	replayedMessages := append([]domain.Message(nil), baseMessages...)
	replayedMessages = append(replayedMessages,
		domain.Message{Role: domain.RoleAssistant, Blocks: []domain.Block{{
			Kind: domain.BlockOpaque, OpaqueType: "reasoning", Opaque: json.RawMessage(`{"type":"reasoning"}`),
		}}},
		domain.Message{Role: domain.RoleAssistant, Blocks: []domain.Block{toolCall}},
		domain.Message{Role: domain.RoleTool, ToolID: "call-probe", Blocks: []domain.Block{{
			Kind: domain.BlockToolResult, ID: "call-probe", Result: json.RawMessage(`"ok"`),
		}}},
	)
	replayedNative := json.RawMessage(`{"model":"smart","input":[{"type":"function_call","call_id":"call-probe"},{"type":"function_call_output","call_id":"call-probe","output":"ok"}],"store":false}`)
	f.calls["p1-a"].invokeResponse = &domain.Response{
		Content: []domain.Block{{Kind: domain.BlockText, Text: "done"}}, FinishReason: "stop", Model: "upstream",
	}
	secondRequest := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIResponses, Model: "smart",
		Native: replayedNative, Messages: replayedMessages,
		Tools:   []domain.ToolDef{{Name: "exec_command", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Lineage: &domain.ResponsesLineage{PromptCacheKey: "codex-thread-cache"},
	}
	second, failure := f.sch.Execute(context.Background(), secondRequest, meta)
	if failure != nil || second.ContinuityMode != domain.ContinuityNone || f.calls["p1-a"].invokes.Load() != 2 {
		t.Fatalf("client-owned replay=%+v attempts=%d failure=%v", second, f.calls["p1-a"].invokes.Load(), failure)
	}
	sent := f.calls["p1-a"].lastRequest()
	if sent == nil || len(sent.Messages) != len(replayedMessages) || string(sent.Native) != string(replayedNative) {
		t.Fatalf("client replay was rewritten: %+v", sent)
	}
	stored, err := store.Conversation(first.ConversationID)
	if err != nil || stored == nil || len(stored.Items) != len(replayedMessages)+1 {
		t.Fatalf("stored replay duplicated history: conversation=%+v err=%v", stored, err)
	}
}

func TestFerryConversationIDDirectlyRestoresConversation(t *testing.T) {
	f := newV2Fixture(t)
	store, err := conversation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f.sch.SetConversationStore(store)
	f.calls["p1-a"].nativeResponseID = "native-root"
	firstRequest := chatReq("smart")
	firstRequest.Protocol = domain.ProtocolOpenAIResponses
	firstRequest.Origin = domain.ProtocolOpenAIResponses
	first, failure := f.sch.Execute(context.Background(), firstRequest, RequestMeta{BearerToken: "sk"})
	if failure != nil || first.ConversationID == "" {
		t.Fatalf("first=%+v failure=%v", first, failure)
	}
	f.calls["p1-a"].nativeResponseID = "native-next"
	nextRequest := chatReq("smart")
	nextRequest.Protocol = domain.ProtocolOpenAIResponses
	nextRequest.Origin = domain.ProtocolOpenAIResponses
	nextRequest.Lineage = &domain.ResponsesLineage{ConversationID: first.ConversationID}
	next, failure := f.sch.Execute(context.Background(), nextRequest, RequestMeta{BearerToken: "sk"})
	if failure != nil || next.ConversationID != first.ConversationID || next.ContinuityMode != domain.ContinuityNative {
		t.Fatalf("direct conversation restore=%+v failure=%v", next, failure)
	}
}

func TestNativeV2RejectsDisabledLogicalModelAndHiddenLegacyAlias(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.LogicalModels["disabled"] = &domain.LogicalModel{ID: "disabled", Enabled: false}
		snapshot.Aliases["disabled"] = &domain.CompiledAlias{Name: "disabled"}
		snapshot.Aliases["hidden-legacy"] = &domain.CompiledAlias{Name: "hidden-legacy"}
	})
	for _, model := range []string{"disabled", "hidden-legacy"} {
		_, failure := f.sch.Execute(context.Background(), chatReq(model), RequestMeta{BearerToken: "sk"})
		if failure == nil || failure.Type != domain.FailUnsupportedParam ||
			!strings.Contains(failure.Message, "logical model") {
			t.Fatalf("model %q escaped native LogicalModels namespace: %+v", model, failure)
		}
	}
}

func TestNativeV2RejectsArbitraryUnknownConversationID(t *testing.T) {
	f := newV2Fixture(t)
	store, err := conversation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f.sch.SetConversationStore(store)
	request := chatReq("smart")
	request.Protocol = domain.ProtocolOpenAIResponses
	request.Origin = domain.ProtocolOpenAIResponses
	request.Lineage = &domain.ResponsesLineage{ConversationID: "provider-native-conversation-123"}
	if _, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"}); failure == nil || failure.Type != domain.FailUnsupportedParam ||
		!strings.Contains(failure.Message, "unknown conversation") {
		t.Fatalf("arbitrary unknown conversation was adopted: %+v", failure)
	}
}

func TestPreviousResponseIDFailsWhenConversationStoreUnavailable(t *testing.T) {
	f := newV2Fixture(t)
	request := chatReq("smart")
	request.Protocol = domain.ProtocolOpenAIResponses
	request.Origin = domain.ProtocolOpenAIResponses
	request.Lineage = &domain.ResponsesLineage{PreviousResponseID: "resp_smart_missing"}
	if _, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"}); failure == nil || failure.Type != domain.FailUnsupportedParam ||
		!strings.Contains(failure.Message, "conversation store unavailable") {
		t.Fatalf("previous_response_id failed open without conversation store: %+v", failure)
	}
}

func TestResponseLineageBranchesReuseTheParentNativeHandle(t *testing.T) {
	f := newV2Fixture(t)
	store, err := conversation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f.sch.SetConversationStore(store)
	f.calls["p1-a"].nativeResponseID = "native-root"
	rootRequest := chatReq("smart")
	rootRequest.Protocol = domain.ProtocolOpenAIResponses
	rootRequest.Origin = domain.ProtocolOpenAIResponses
	root, failure := f.sch.Execute(context.Background(), rootRequest, RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	for _, branch := range []string{"a", "b"} {
		f.calls["p1-a"].nativeResponseID = "native-" + branch
		request := chatReq("smart")
		request.Protocol = domain.ProtocolOpenAIResponses
		request.Origin = domain.ProtocolOpenAIResponses
		request.Messages[0].Blocks[0].Text = branch
		request.Lineage = &domain.ResponsesLineage{PreviousResponseID: root.RequestID}
		result, failure := f.sch.Execute(context.Background(), request, RequestMeta{BearerToken: "sk"})
		if failure != nil {
			t.Fatalf("branch %s: %v", branch, failure)
		}
		sent := f.calls["p1-a"].lastRequest()
		if sent == nil || sent.Continuation == nil || sent.Continuation.NativeResponseID != "native-root" {
			t.Fatalf("branch %s continued from sibling/latest handle: %+v", branch, sent)
		}
		_, snapshot, err := store.ConversationAtResponse(result.RequestID)
		if err != nil || snapshot == nil {
			t.Fatalf("branch %s snapshot=%+v err=%v", branch, snapshot, err)
		}
		found := false
		for _, message := range snapshot.Items {
			for _, block := range message.Blocks {
				found = found || block.Text == branch
			}
		}
		if !found {
			t.Fatalf("branch %s canonical snapshot missing its input: %+v", branch, snapshot.Items)
		}
	}
}

func TestMatchedAPIKeySecretIsAttributedAndUsageRecorded(t *testing.T) {
	f := newV2Fixture(t)
	w := f.sch.world.Load()
	hash := domain.HashAPIKeySecret("sk-durable")
	f.sch.ReplaceWorldWithAPIKeys(w.snap, w.callers, nil, []domain.APIKeyAuthenticator{{
		Identity:     domain.APIKeyIdentity{ID: "caller-id", Name: "caller", Enabled: true},
		SecretHashes: [][32]byte{hash}, SecretIDs: []string{"sec-durable"},
	}})
	var recordedID string
	f.sch.SetAPIKeyUsageRecorder(func(secretID string, _ time.Time) error {
		recordedID = secretID
		return nil
	})
	if _, failure := f.sch.Execute(context.Background(), chatReq("smart"), RequestMeta{BearerToken: "sk-durable"}); failure != nil {
		t.Fatal(failure)
	}
	if recordedID != "sec-durable" {
		t.Fatalf("usage recorder secret id=%q", recordedID)
	}
	if record := f.records(t)[0]; record.APIKeySecretID != "sec-durable" {
		t.Fatalf("request secret attribution=%q", record.APIKeySecretID)
	}
}
