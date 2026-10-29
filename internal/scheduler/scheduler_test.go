package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/ledger"
	"ferry/internal/policy"
	"ferry/internal/runtime"
)

func TestRequestDebugPayloadRedactsSecretsAndTruncates(t *testing.T) {
	rc := &domain.RequestContract{Native: json.RawMessage(`{"model":"smart","authorization":"Bearer secret","nested":{"api_key":"top-secret","prompt":"hello"}}`)}
	payload, truncated := requestDebugPayload(rc)
	if truncated || strings.Contains(payload, "Bearer secret") || strings.Contains(payload, "top-secret") ||
		!strings.Contains(payload, `"authorization": "[redacted]"`) || !strings.Contains(payload, `"prompt": "hello"`) {
		t.Fatalf("redacted payload=%s truncated=%v", payload, truncated)
	}
	rc.Native = json.RawMessage(`{"prompt":"` + strings.Repeat("x", requestDebugLimit+100) + `"}`)
	payload, truncated = requestDebugPayload(rc)
	if !truncated || !strings.Contains(payload, `"truncated": true`) {
		t.Fatalf("large payload was not safely truncated")
	}
}

func TestResponseDebugPayloadIncludesCompatibilityWarnings(t *testing.T) {
	payload, truncated := responseDebugPayload(domain.ReqSuccess, nil, &execution{
		responseWarnings: []string{"provider caching metadata omitted during protocol conversion"},
	})
	var decoded struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode response debug payload: %v", err)
	}
	if truncated || len(decoded.Warnings) != 1 || decoded.Warnings[0] != "provider caching metadata omitted during protocol conversion" {
		t.Fatalf("response debug payload = %s truncated=%v", payload, truncated)
	}
}

func TestContractForPreservesNativeImageBodyAcrossModelAlias(t *testing.T) {
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Model: "image-alias",
		Operation: "edit", NativeContentType: "multipart/form-data; boundary=test",
		Native: json.RawMessage("multipart-body"),
	}
	target := &domain.CompiledTarget{Model: "gpt-image-2", NativeProtocols: []domain.Protocol{domain.ProtocolOpenAIImages}}
	got := contractFor(rc, target)
	if got.Model != "gpt-image-2" || got.Protocol != domain.ProtocolOpenAIImages || string(got.Native) != "multipart-body" || got.Operation != "edit" {
		t.Fatalf("image alias contract=%+v", got)
	}
}

func TestContractForPreservesNativeTextBodyAndFeatureHeadersAcrossModelAlias(t *testing.T) {
	for _, protocol := range []domain.Protocol{
		domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic,
	} {
		rc := &domain.RequestContract{Protocol: protocol, Model: "logical-alias",
			Native:          json.RawMessage(`{"model":"logical-alias","future_field":true}`),
			ProtocolHeaders: map[string]string{"Anthropic-Beta": "feature"}}
		target := &domain.CompiledTarget{Model: "actual-upstream", NativeProtocols: []domain.Protocol{protocol}}
		got := contractFor(rc, target)
		if string(got.Native) != string(rc.Native) || got.ProtocolHeaders["Anthropic-Beta"] != "feature" {
			t.Fatalf("%s alias discarded native request: %+v", protocol, got)
		}
	}
}

func TestAffinityPruneRemovesDeletedProviderSupply(t *testing.T) {
	cache := newAffinityCache()
	evidence := domain.NewAffinityEvidence("conversation", domain.AffinityConversation, "conversation-1", domain.ConfHigh)
	target := &domain.CompiledTarget{ID: "target-1", ProviderID: "provider-a", ConnectionID: "connection-a", Model: "upstream-a"}
	cache.bind(evidence, "smart", domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-a"}}}, target, time.Now(), true)
	snapshot := &domain.CompiledSnapshot{
		LogicalModels: map[string]*domain.LogicalModel{"smart": {ID: "smart", Enabled: true}},
		Providers:     map[string]*domain.Provider{"provider-a": {ID: "provider-a", Enabled: true}},
		Connections:   map[string]*domain.Connection{"connection-a": {ID: "connection-a", ProviderID: "provider-a", Enabled: true}},
		Targets:       map[domain.ResourceTargetID]*domain.CompiledTarget{"target-1": target},
	}
	if removed := cache.prune(snapshot); removed != 0 {
		t.Fatalf("valid binding pruned: %d", removed)
	}
	delete(snapshot.Providers, "provider-a")
	if removed := cache.prune(snapshot); removed != 1 {
		t.Fatalf("deleted provider binding pruned=%d, want 1", removed)
	}
	if stats := cache.statsByModel(time.Now())["smart"]; stats.ActiveBindings != 0 {
		t.Fatalf("stale binding remains: %+v", stats)
	}
}

// ---------------------------------------------------------------------------
// local ledger harness (mirrors the ledger package's own test helpers; they
// cannot be imported from another package's tests)

type tledger struct {
	*ledger.Ledger
	dir  string
	once sync.Once
}

func (tl *tledger) close() {
	tl.once.Do(func() { tl.Ledger.Close() })
}

func openTempLedger(t *testing.T) *tledger {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(dir, 256, 0)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	tl := &tledger{Ledger: l, dir: dir}
	t.Cleanup(tl.close)
	return tl
}

// reopen flushes pending async writes and reopens the same database for
// querying. Only call after every Execute in the test has finished — the
// scheduler keeps writing to the original (closed) handle afterwards.
func (tl *tledger) reopen(t *testing.T) *ledger.Ledger {
	t.Helper()
	tl.close()
	l, err := ledger.Open(tl.dir, 256, 0)
	if err != nil {
		t.Fatalf("reopen ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// ---------------------------------------------------------------------------
// fake egress caller implementing Caller + Preparer

var (
	_ egress.Caller   = (*fakeCaller)(nil)
	_ egress.Preparer = (*fakeCaller)(nil)
)

type fakeCaller struct {
	notes            []string // Prepare output; empty => clean mapping
	prepErr          error    // Prepare error => unsupported compatibility
	prepare          func(*domain.RequestContract) ([]string, error)
	connect          *domain.Failure // Invoke / Stream fails immediately with this
	midErr           *domain.Failure // Stream: visible deltas then EvError (post-commit)
	invokeResponse   *domain.Response
	streamEvents     []domain.StreamEvent
	streamGate       <-chan struct{}
	streamStarted    chan struct{}
	nativeResponseID string
	invokeStarted    chan struct{}
	invokeBlock      <-chan struct{}
	requestsMu       sync.Mutex
	requests         []*domain.RequestContract

	invokes     atomic.Int32
	streams     atomic.Int32
	streamFails atomic.Int32
}

func (c *fakeCaller) Prepare(rc *domain.RequestContract) ([]string, error) {
	if c.prepare != nil {
		return c.prepare(rc)
	}
	if c.prepErr != nil {
		return nil, c.prepErr
	}
	return c.notes, nil
}

func (c *fakeCaller) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	c.invokes.Add(1)
	if c.invokeStarted != nil {
		close(c.invokeStarted)
	}
	if c.invokeBlock != nil {
		select {
		case <-c.invokeBlock:
		case <-ctx.Done():
			return nil, &domain.Failure{Type: domain.FailClientCancel, Scope: domain.FailureScopeRequest, Message: ctx.Err().Error()}
		}
	}
	copyRequest := *rc
	c.requestsMu.Lock()
	c.requests = append(c.requests, &copyRequest)
	c.requestsMu.Unlock()
	if c.connect != nil {
		return nil, c.connect
	}
	if c.invokeResponse != nil {
		response := *c.invokeResponse
		response.Content = append([]domain.Block(nil), c.invokeResponse.Content...)
		response.NativeBody = append([]byte(nil), c.invokeResponse.NativeBody...)
		response.ConversionIssues = append([]string(nil), c.invokeResponse.ConversionIssues...)
		response.ValidationIssues = append([]string(nil), c.invokeResponse.ValidationIssues...)
		return &response, nil
	}
	u := domain.Usage{InputTokens: 100, OutputTokens: 20}
	return &domain.Response{
		Content:          []domain.Block{{Kind: domain.BlockText, Text: "ok"}},
		FinishReason:     "stop",
		Usage:            u,
		Model:            rc.Model,
		NativeResponseID: c.nativeResponseID,
	}, nil
}

func (c *fakeCaller) lastRequest() *domain.RequestContract {
	c.requestsMu.Lock()
	defer c.requestsMu.Unlock()
	if len(c.requests) == 0 {
		return nil
	}
	copyRequest := *c.requests[len(c.requests)-1]
	return &copyRequest
}

func (c *fakeCaller) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	c.streams.Add(1)
	copyRequest := *rc
	c.requestsMu.Lock()
	c.requests = append(c.requests, &copyRequest)
	c.requestsMu.Unlock()
	if c.connect != nil {
		c.streamFails.Add(1)
		return nil, func() {}, c.connect
	}
	ch := make(chan domain.StreamEvent, 8)
	go func() {
		defer close(ch)
		if c.streamGate != nil {
			ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: 0, TextDelta: "hello"}
			if c.streamStarted != nil {
				close(c.streamStarted)
			}
			<-c.streamGate
			ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: 0, TextDelta: " after cancel"}
			return
		}
		if c.streamEvents != nil {
			for _, event := range c.streamEvents {
				ch <- event
			}
			return
		}
		ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: 0, TextDelta: "hel"}
		ch <- domain.StreamEvent{Kind: domain.EvTextDelta, Index: 0, TextDelta: "lo"}
		if c.midErr != nil {
			ch <- domain.StreamEvent{Kind: domain.EvError, Err: c.midErr}
			return
		}
		ch <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: "stop",
			Usage: &domain.Usage{InputTokens: 100, OutputTokens: 20}}
	}()
	return ch, func() {}, nil
}

func TestResponseProtocolFailureRequiresActualNativePassthrough(t *testing.T) {
	rc := &domain.RequestContract{Protocol: domain.ProtocolAnthropic}
	response := &domain.Response{
		NativeProtocol:   domain.ProtocolAnthropic,
		NativeBody:       []byte(`{"type":"message"}`),
		ConversionIssues: []string{"future field is native-only"},
	}
	if failure := responseProtocolFailure(rc, response); failure != nil {
		t.Fatalf("same-protocol native response rejected: %v", failure)
	}
	response.NativeBody = nil
	if failure := responseProtocolFailure(rc, response); failure == nil ||
		failure.Type != domain.FailProtocolConversion || failure.Scope != domain.FailureScopeTarget {
		t.Fatalf("missing native body did not fail closed: %+v", failure)
	}
	if failure := responseProtocolFailure(rc, nil); failure == nil || failure.Type != domain.FailBadResponse {
		t.Fatalf("nil upstream response did not fail closed: %+v", failure)
	}
	response.NativeBody = []byte(`{"type":"message"}`)
	response.ValidationIssues = []string{"missing terminal state"}
	if failure := responseProtocolFailure(rc, response); failure == nil || failure.Type != domain.FailBadResponse {
		t.Fatalf("native protocol violation was incorrectly bypassed: %+v", failure)
	}
}

func TestStructuredOutputIsValidatedBeforeCommit(t *testing.T) {
	rc := &domain.RequestContract{ResponseFormat: &domain.ResponseFormat{}}
	invalid := &domain.Response{Content: []domain.Block{{Kind: domain.BlockText, Text: "not json"}}, FinishReason: "stop"}
	if failure := responseProtocolFailure(rc, invalid); failure == nil || failure.Type != domain.FailBadResponse ||
		failure.Scope != domain.FailureScopeTarget {
		t.Fatalf("invalid structured response was accepted: %+v", failure)
	}
	array := &domain.Response{Content: []domain.Block{{Kind: domain.BlockText, Text: `[]`}}, FinishReason: "stop"}
	if failure := responseProtocolFailure(rc, array); failure == nil ||
		!strings.Contains(failure.Message, "JSON object") {
		t.Fatalf("json_object accepted an array: %+v", failure)
	}
	valid := &domain.Response{Content: []domain.Block{{Kind: domain.BlockText, Text: `{"ok":true}`}}, FinishReason: "stop"}
	if failure := responseProtocolFailure(rc, valid); failure != nil {
		t.Fatalf("valid structured response was rejected: %v", failure)
	}
	stream := &execution{output: []domain.Block{{Kind: domain.BlockText, Text: "not json"}}}
	if failure := completedStreamProtocolFailure(rc, stream, "stop"); failure == nil ||
		failure.Type != domain.FailBadResponse {
		t.Fatalf("invalid structured stream was accepted: %+v", failure)
	}
}

func TestExecutionCapturesParallelToolArgumentsByEventIndex(t *testing.T) {
	execution := &execution{}
	for _, event := range []domain.StreamEvent{
		{Kind: domain.EvToolStart, Index: 3, ToolID: "call-a", ToolName: "alpha"},
		{Kind: domain.EvToolStart, Index: 7, ToolID: "call-b", ToolName: "beta"},
		{Kind: domain.EvToolDelta, Index: 3, ArgsDelta: `{"a":`},
		{Kind: domain.EvToolDelta, Index: 7, ArgsDelta: `{"b":2}`},
		{Kind: domain.EvToolDelta, Index: 3, ArgsDelta: `1}`},
	} {
		execution.captureStreamOutput(event)
	}
	if len(execution.output) != 2 || string(execution.output[0].Input) != `{"a":1}` ||
		string(execution.output[1].Input) != `{"b":2}` {
		t.Fatalf("parallel tool arguments crossed streams: %+v", execution.output)
	}
}

func TestVisibleStreamProtocolFailureRejectsMissingToolArguments(t *testing.T) {
	execution := &execution{}
	start := domain.StreamEvent{Kind: domain.EvToolStart, Index: 1, ToolID: "call-1", ToolName: "lookup"}
	if failure := visibleStreamProtocolFailure(&domain.RequestContract{}, execution, start); failure != nil {
		t.Fatal(failure)
	}
	execution.captureStreamOutput(start)
	end := domain.StreamEvent{Kind: domain.EvToolEnd, Index: 1}
	if failure := visibleStreamProtocolFailure(&domain.RequestContract{}, execution, end); failure == nil ||
		failure.Type != domain.FailProtocolConversion {
		t.Fatalf("missing streamed tool arguments accepted: %+v", failure)
	}
}

func TestFinalAttemptContractRejectsLossyRehydratedHistory(t *testing.T) {
	caller := egress.NewOpenAIChat(egress.Base{Model: "upstream"})
	contract := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIChat,
		Origin:   domain.ProtocolAnthropic,
		Messages: []domain.Message{{Role: domain.RoleAssistant, Blocks: []domain.Block{{
			Kind: domain.BlockThinking, Text: "private chain", Signature: "native-signature",
		}}}},
	}
	if failure := attemptContractProtocolFailure(caller, contract); failure == nil ||
		failure.Type != domain.FailProtocolConversion || failure.Scope != domain.FailureScopeTarget {
		t.Fatalf("lossy rehydration was accepted: %+v", failure)
	}
	contract.Origin = domain.ProtocolOpenAIChat
	contract.Native = []byte(`{"model":"alias","messages":[]}`)
	if failure := attemptContractProtocolFailure(caller, contract); failure != nil {
		t.Fatalf("exact native passthrough was rejected: %v", failure)
	}
}

func TestFinalAttemptNativeResponsesToolProgressUsesActualEncoder(t *testing.T) {
	input := []any{
		map[string]any{"type": "function_call", "call_id": "call-exec", "name": "exec", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call-exec", "output": "done"},
		map[string]any{"type": "function_call_output", "call_id": "call-exec", "output": "15/65"},
		map[string]any{"type": "function_call_output", "call_id": "call-exec", "output": "30/65"},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}
	native, err := json.Marshal(map[string]any{"model": "alias", "input": input})
	if err != nil {
		t.Fatal(err)
	}
	contract := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIResponses,
		Native: native, Messages: []domain.Message{
			{Role: domain.RoleAssistant, Blocks: []domain.Block{{Kind: domain.BlockToolUse, ID: "call-exec", Name: "exec", Input: json.RawMessage("{}")}}},
			{Role: domain.RoleTool, ToolID: "call-exec", Blocks: []domain.Block{{Kind: domain.BlockToolResult, ID: "call-exec", Result: json.RawMessage("\"done\"")}}},
			{Role: domain.RoleTool, ToolID: "call-exec", Blocks: []domain.Block{{Kind: domain.BlockToolResult, ID: "call-exec", Result: json.RawMessage("\"15/65\"")}}},
			{Role: domain.RoleTool, ToolID: "call-exec", Blocks: []domain.Block{{Kind: domain.BlockToolResult, ID: "call-exec", Result: json.RawMessage("\"30/65\"")}}},
			{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "continue"}}},
		},
	}
	base := egress.Base{Model: "upstream"}
	responses := egress.NewOpenAIResponses(base)
	codex := egress.NewCodexCaller(base, "account")
	multi := egress.NewMultiCaller(base, []domain.Protocol{domain.ProtocolOpenAIResponses},
		func(*domain.RequestContract) domain.Protocol { return domain.ProtocolOpenAIResponses })
	dynamic := egress.NewDynamicCaller(base, nil, func(b egress.Base, _ string) egress.Caller {
		return egress.NewOpenAIResponses(b)
	})
	for name, caller := range map[string]egress.Caller{
		"responses": responses, "codex": codex, "multi": multi, "dynamic": dynamic,
	} {
		t.Run(name, func(t *testing.T) {
			if failure := attemptContractProtocolFailure(caller, contract); failure != nil {
				t.Fatalf("preserved native input rejected: %+v", failure)
			}
		})
	}
	if failure := attemptContractProtocolFailure(egress.NewOpenAIChat(base), contract); failure == nil ||
		failure.Type != domain.FailProtocolConversion || failure.Scope != domain.FailureScopeTarget {
		t.Fatalf("cross-protocol target did not fail as target incompatibility: %+v", failure)
	}
	rehydrated := *contract
	rehydrated.Native = nil
	if failure := attemptContractProtocolFailure(responses, &rehydrated); failure == nil ||
		failure.Type != domain.FailUnsupportedParam || failure.Scope != domain.FailureScopeRequest {
		t.Fatalf("rehydrated duplicate results accepted: %+v", failure)
	}
	fallback := *contract
	fallback.Native, err = json.Marshal(map[string]any{"input": input}) // missing model forces re-encoding
	if err != nil {
		t.Fatal(err)
	}
	if failure := attemptContractProtocolFailure(responses, &fallback); failure == nil ||
		failure.Type != domain.FailProtocolConversion || failure.Scope != domain.FailureScopeTarget {
		t.Fatalf("re-encoded duplicate results accepted: %+v", failure)
	}
	malformed := *contract
	malformed.Messages = append([]domain.Message(nil), contract.Messages...)
	malformed.Messages[0].Blocks = []domain.Block{{Kind: domain.BlockToolUse, ID: "call-exec", Name: "exec", Input: json.RawMessage("[]")}}
	if failure := attemptContractProtocolFailure(responses, &malformed); failure == nil ||
		failure.Type != domain.FailUnsupportedParam || failure.Scope != domain.FailureScopeRequest {
		t.Fatalf("malformed tool argument accepted: %+v", failure)
	}
	duplicateCall := *contract
	duplicateCall.Messages = append([]domain.Message(nil), contract.Messages...)
	duplicateCall.Messages[0].Blocks = []domain.Block{
		{Kind: domain.BlockToolUse, ID: "call-exec", Name: "exec", Input: json.RawMessage("{}")},
		{Kind: domain.BlockToolUse, ID: "call-exec", Name: "exec", Input: json.RawMessage("{}")},
	}
	if failure := attemptContractProtocolFailure(responses, &duplicateCall); failure == nil ||
		failure.Type != domain.FailUnsupportedParam || failure.Scope != domain.FailureScopeRequest {
		t.Fatalf("duplicate tool call accepted: %+v", failure)
	}
}

// ---------------------------------------------------------------------------
// fixture: 3 targets, 2 pools, 2 tiers

const testCardModel = "upstream-m1"

var testCards = []*domain.RateCard{{
	Version: "card-v1", Provider: "fake", Model: testCardModel, Plan: domain.PlanFree,
	InputPerM: 1, OutputPerM: 2,
}}

func baseSnapshot(version int64) *domain.CompiledSnapshot {
	mkEndpoint := func(id string) *domain.Endpoint {
		return &domain.Endpoint{ID: id, BaseURL: "http://127.0.0.1:1", Protocol: domain.ProtocolOpenAIChat}
	}
	mkTarget := func(id string, prio int, ref string) *domain.CompiledTarget {
		return &domain.CompiledTarget{
			ID:         domain.ResourceTargetID(id),
			Endpoint:   mkEndpoint("ep-" + id),
			Credential: domain.CredentialRef{SecretRef: ref},
			Model:      testCardModel,
			Plan:       &domain.Plan{ID: "pln-free", Kind: domain.PlanFree, Currency: "USD"},
			Capabilities: domain.Capabilities{
				Text: true, Stream: true, Tools: true, ParallelTools: true,
				Reasoning: true, StructuredOutput: true, Vision: true,
				ContextWindow: 128000, MaxOutput: 8192,
			},
			Weight: 1, Priority: prio,
		}
	}
	targets := map[domain.ResourceTargetID]*domain.CompiledTarget{
		"t1": mkTarget("t1", 0, "env:CPA_TEST_T1"),
		"t2": mkTarget("t2", 10, "env:CPA_TEST_T2"),
		"t3": mkTarget("t3", 20, "env:CPA_TEST_T3"),
	}
	return &domain.CompiledSnapshot{
		Version:        version,
		V2DomainSource: config.V2DomainSourceLegacy,
		CreatedAt:      time.Now(),
		Hash:           "fixhash",
		Targets:        targets,
		TargetOrder:    []domain.ResourceTargetID{"t1", "t2", "t3"},
		Pools: map[string][]domain.ResourceTargetID{
			"poolA": {"t1", "t2"},
			"poolB": {"t3"},
		},
		Aliases: map[string]*domain.CompiledAlias{
			"main": {Name: "main", RequiredCaps: domain.Requirements{Text: true}},
		},
		Objectives: map[string]*domain.ObjectiveProfile{
			"balanced": {Name: "balanced"},
		},
		Policies: map[string]*domain.CompiledPolicy{
			"main": {
				Name:     "main",
				Timezone: time.UTC,
				Tiers: []domain.TierDef{
					{Name: "primary", Pool: "poolA"},
					{Name: "fallback", Pool: "poolB"},
				},
				Recovery: map[domain.FailureType]domain.RecoveryAction{},
				Hash:     "polhash",
			},
		},
		DefaultPolicy: "main",
	}
}

type fixture struct {
	sch   *Scheduler
	tl    *tledger
	calls map[domain.ResourceTargetID]*fakeCaller
	keys  []config.ConsumerKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	mgr := runtime.NewManager(runtime.DefaultHealthConfig())
	tl := openTempLedger(t)
	f := &fixture{
		sch: New(mgr, tl.Ledger),
		tl:  tl,
		calls: map[domain.ResourceTargetID]*fakeCaller{
			"t1": {}, "t2": {}, "t3": {},
		},
	}
	f.publish(1, nil)
	return f
}

// publish rebuilds the base snapshot at the given version, applies optional
// mutations, and swaps the world atomically.
func (f *fixture) publish(version int64, mutate func(*domain.CompiledSnapshot)) {
	enabled := true
	f.publishKeys(version, mutate, []config.ConsumerKey{{Name: "anonymous", Key: "", Enabled: &enabled}})
}

func (f *fixture) publishKeys(version int64, mutate func(*domain.CompiledSnapshot),
	keys []config.ConsumerKey) {

	snap := baseSnapshot(version)
	if mutate != nil {
		mutate(snap)
	}
	callers := make(map[domain.ResourceTargetID]egress.Caller, len(f.calls))
	for id, c := range f.calls {
		callers[id] = c
	}
	f.keys = keys
	f.sch.ReplaceWorld(snap, callers, testCards, keys)
}

func chatReq(model string) *domain.RequestContract {
	return &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIChat,
		Model:    model,
		Messages: []domain.Message{{
			Role:   domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}},
		}},
	}
}

// rows flushes the ledger and returns every logical request row.
func (f *fixture) rows(t *testing.T) []domain.RequestRecord {
	t.Helper()
	db := f.tl.reopen(t)
	recs, _, err := db.QueryRequests(domain.LogFilter{})
	if err != nil {
		t.Fatalf("query requests: %v", err)
	}
	return recs
}

func attemptsOf(t *testing.T, db *ledger.Ledger, reqID string) []domain.AttemptRecord {
	t.Helper()
	as, err := db.QueryAttempts(reqID)
	if err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	return as
}

// ---------------------------------------------------------------------------
// tests

func TestInvokeSuccessAnonymous(t *testing.T) {
	f := newFixture(t)

	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{ClientIP: "10.0.0.1"})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	if res.Target != "t1" {
		t.Errorf("target = %q, want t1", res.Target)
	}
	if res.Model != testCardModel {
		t.Errorf("model = %q, want %q", res.Model, testCardModel)
	}
	if res.PolicyName != "main" || res.PolicyVer != 1 {
		t.Errorf("policy = %s@%d, want main@1", res.PolicyName, res.PolicyVer)
	}
	if res.CompatLevel != domain.CompatExact {
		t.Errorf("compat = %s, want exact (same protocol, no notes)", res.CompatLevel)
	}
	if res.Response == nil || res.Response.FinishReason != "stop" {
		t.Fatalf("response missing or wrong finish: %+v", res.Response)
	}

	db := f.tl.reopen(t)
	recs, total, err := db.QueryRequests(domain.LogFilter{})
	if err != nil || total != 1 || len(recs) != 1 {
		t.Fatalf("rows: n=%d total=%d err=%v", len(recs), total, err)
	}
	r := recs[0]
	if r.Status != domain.ReqSuccess || r.Attempts != 1 || r.SelectedTarget != "t1" {
		t.Errorf("record = %+v", r)
	}
	if r.ConsumerKey != "anonymous" {
		t.Errorf("consumer key = %q, want explicit test-only anonymous identity", r.ConsumerKey)
	}
	if r.ShadowCost <= 0 {
		t.Errorf("shadow cost = %v, want > 0 (rate card applied)", r.ShadowCost)
	}
	as := attemptsOf(t, db, r.ID)
	if len(as) != 1 {
		t.Fatalf("attempts = %d, want 1", len(as))
	}
	// credential reference TEXT only — the secret itself never appears
	if as[0].CredentialRef != "env:CPA_TEST_T1" {
		t.Errorf("credential ref = %q, want env:CPA_TEST_T1", as[0].CredentialRef)
	}
	if as[0].Multiplier != 1.0 {
		t.Errorf("multiplier = %v, want 1 (free plan, no windows)", as[0].Multiplier)
	}
	tr, err := db.QueryTrace(r.ID)
	if err != nil || tr == nil {
		t.Fatalf("trace: %v %v", tr, err)
	}
	if tr.Primary != "t1" || len(tr.Tiers) != 2 || tr.Tiers[0] != "primary" {
		t.Errorf("trace = %+v", tr)
	}
	if len(tr.RecoveryEvents) != 0 {
		t.Errorf("unexpected recovery events: %+v", tr.RecoveryEvents)
	}
}

func TestLegacyRoutingUsesEgressProtocolCapabilityEvidence(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities = domain.Capabilities{Text: true, Tools: true}
			target.ProtocolCapabilities = map[domain.Protocol]domain.Capabilities{
				domain.ProtocolOpenAIChat: {Text: true, Tools: true},
			}
		}
		chat := snapshot.Targets["t1"].ProtocolCapabilities[domain.ProtocolOpenAIChat]
		chat.Tools = false
		snapshot.Targets["t1"].ProtocolCapabilities[domain.ProtocolOpenAIChat] = chat
	})
	request := chatReq("main")
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Target != "t2" || f.calls["t1"].invokes.Load() != 0 {
		t.Fatalf("legacy routing used aggregate instead of protocol evidence: result=%+v first=%d", result, f.calls["t1"].invokes.Load())
	}
}

func TestLegacyExplicitAliasParallelToolsRequirementUsesSerialMapping(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snapshot *domain.CompiledSnapshot) {
		snapshot.Aliases["main"].RequiredCaps = domain.Requirements{Text: true, Tools: true, ParallelTools: true}
		snapshot.Targets["t1"].Capabilities.ParallelTools = false
	})
	parallel := true
	request := chatReq("main")
	request.Native = json.RawMessage(`{"model":"main","parallel_tool_calls":true}`)
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Target != "t1" || result.CompatLevel != domain.CompatMapped || f.calls["t1"].invokes.Load() != 1 {
		t.Fatalf("explicit alias contract was not mapped: result=%+v first=%d", result, f.calls["t1"].invokes.Load())
	}
	attempt := f.calls["t1"].lastRequest()
	if attempt == nil || attempt.ParallelTools == nil || *attempt.ParallelTools || attempt.Native != nil {
		t.Fatalf("mapped alias attempt did not force serial tools: %+v", attempt)
	}
}

func TestLegacyAliasCapabilityContractParticipatesInRoutingPolicyMatch(t *testing.T) {
	f := newFixture(t)
	f.publish(7, func(snapshot *domain.CompiledSnapshot) {
		snapshot.Aliases["main"].RequiredCaps.Reasoning = true
		snapshot.Policies["reasoning-route"] = &domain.CompiledPolicy{
			Name: "reasoning-route", Timezone: time.UTC,
			Tiers:    []domain.TierDef{{Name: "reasoning", Pool: "poolB"}},
			Recovery: map[domain.FailureType]domain.RecoveryAction{},
		}
		snapshot.RoutingPolicies = []domain.RoutingPolicySpec{
			{
				ID: "reasoning-route", Priority: 10, Enabled: true,
				Match: domain.RoutingPolicyMatch{RequiredCapabilities: []string{"reasoning"}},
				Route: domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: "poolB"}}},
			},
			{
				ID: "main", Priority: 20, Enabled: true,
				Route: domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: "poolA"}}},
			},
		}
	})

	result, failure := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.PolicyName != "reasoning-route" || result.Target != "t3" {
		t.Fatalf("alias capability contract was absent from policy matching: %+v", result)
	}
}

func TestLegacyParallelPreferenceMatchesPolicyThenSerializesForTarget(t *testing.T) {
	f := newFixture(t)
	f.publish(7, func(snapshot *domain.CompiledSnapshot) {
		for _, target := range snapshot.Targets {
			target.Capabilities.ParallelTools = false
		}
		snapshot.Policies["parallel-route"] = &domain.CompiledPolicy{
			Name: "parallel-route", Timezone: time.UTC,
			Tiers:    []domain.TierDef{{Name: "parallel-preference", Pool: "poolB"}},
			Recovery: map[domain.FailureType]domain.RecoveryAction{},
		}
		snapshot.RoutingPolicies = []domain.RoutingPolicySpec{
			{
				ID: "parallel-route", Priority: 10, Enabled: true,
				Match: domain.RoutingPolicyMatch{RequiredCapabilities: []string{"parallel_tools"}},
				Route: domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: "poolB"}}},
			},
			{
				ID: "main", Priority: 20, Enabled: true,
				Route: domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: "poolA"}}},
			},
		}
	})

	parallel := true
	request := chatReq("main")
	request.Native = json.RawMessage(`{"model":"main","parallel_tool_calls":true}`)
	request.Tools = []domain.ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}
	request.ParallelTools = &parallel
	result, failure := f.sch.Execute(context.Background(), request, RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.PolicyName != "parallel-route" || result.Target != "t3" {
		t.Fatalf("parallel preference did not participate in policy matching: %+v", result)
	}
	attempt := f.calls["t3"].lastRequest()
	if attempt == nil || attempt.ParallelTools == nil || *attempt.ParallelTools || attempt.Native != nil {
		t.Fatalf("selected tools-only target did not receive explicit serial mapping: %+v", attempt)
	}
}

func TestSchedulerUsesFirstMatchingRoutingPolicyInsteadOfDefaultPolicy(t *testing.T) {
	f := newFixture(t)
	f.publish(7, func(snap *domain.CompiledSnapshot) {
		snap.Policies["glm-afternoon"] = &domain.CompiledPolicy{
			Name: "glm-afternoon", Timezone: time.UTC,
			Tiers:    []domain.TierDef{{Name: "non-glm", Pool: "poolB"}},
			Recovery: map[domain.FailureType]domain.RecoveryAction{},
		}
		snap.RoutingPolicies = []domain.RoutingPolicySpec{
			{
				ID: "glm-afternoon", Priority: 10, Enabled: true,
				Match: domain.RoutingPolicyMatch{Models: []string{"main"}},
				Route: domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: "poolB"}}},
			},
			{
				ID: "main", Priority: 20, Enabled: true,
				Route: domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: "poolA"}}},
			},
		}
	})

	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatal(ferr)
	}
	if res.PolicyName != "glm-afternoon" || res.Target != "t3" {
		t.Fatalf("selected %s via %s, want t3 via glm-afternoon", res.Target, res.PolicyName)
	}
	if res.PolicyHash == "" || res.PolicyIndexHash == "" {
		t.Fatalf("missing policy observability hashes: %+v", res)
	}
	second, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatal(ferr)
	}
	if !second.PolicySceneCacheHit {
		t.Fatal("second identical scene did not hit safe routing cache")
	}
}

func TestRoutingSceneKeyIncludesImageCapability(t *testing.T) {
	context := policy.RoutingContext{
		Consumer: "caller", Model: "smart", Protocol: domain.ProtocolOpenAIChat,
		Capabilities: domain.Requirements{Text: true},
		Now:          time.Date(2026, 8, 30, 12, 34, 0, 0, time.UTC),
	}
	textKey := routingSceneKey("index", context)
	context.Capabilities.Images = true
	imageKey := routingSceneKey("index", context)
	if textKey == imageKey {
		t.Fatalf("text and image routing scenes collided: %q", textKey)
	}
	context.Capabilities.MinOutput = 8192
	outputKey := routingSceneKey("index", context)
	if imageKey == outputKey {
		t.Fatalf("minimum-output routing scenes collided: %q", imageKey)
	}
}

func TestBlockingLegacyProjectionKeepsLegacyPlannerAuthoritative(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snapshot *domain.CompiledSnapshot) {
		snapshot.LogicalModels = map[string]*domain.LogicalModel{
			"main": {
				ID: "main", Enabled: true,
				DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "would-be-v2", UpstreamModel: "wrong"}}},
			},
		}
		snapshot.V2MigrationIssues = []domain.LegacyMappingIssue{{
			Kind: "legacy_rule", ObjectID: "conditional-tier", Blocking: true,
		}}
	})
	result, failure := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Target != "t1" {
		t.Fatalf("blocking projection bypassed legacy planner: %+v", result)
	}
}

func TestBlockingIssueForAnotherModelDoesNotDisableSafeProjection(t *testing.T) {
	snapshot := &domain.CompiledSnapshot{
		V2DomainSource: config.V2DomainSourceLegacy,
		V2MigrationIssues: []domain.LegacyMappingIssue{{
			LogicalModelID: "other", Kind: "route_step", ObjectID: "other:step-1", Blocking: true,
		}},
	}
	if v2ProjectionBlockedForModel(snapshot, "smart") {
		t.Fatal("a model-scoped migration issue blocked an unrelated safe projection")
	}
}

func TestInvokeSameTierFailover(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailConnect, Message: "dial refused"}

	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	if res.Target != "t2" {
		t.Errorf("target = %q, want t2 after same-tier failover", res.Target)
	}

	db := f.tl.reopen(t)
	recs, _, _ := db.QueryRequests(domain.LogFilter{})
	if len(recs) != 1 {
		t.Fatalf("rows = %d", len(recs))
	}
	r := recs[0]
	if r.Status != domain.ReqSuccess || r.Attempts != 2 || r.SelectedTarget != "t2" {
		t.Errorf("record = %+v", r)
	}
	tr, _ := db.QueryTrace(r.ID)
	if len(tr.RecoveryEvents) != 1 {
		t.Fatalf("recovery events = %+v, want 1", tr.RecoveryEvents)
	}
	ev := tr.RecoveryEvents[0]
	if ev.FailureType != domain.FailConnect ||
		ev.Action != "next_target_same_tier" || ev.NextTarget != "t2" {
		t.Errorf("recovery event = %+v", ev)
	}
}

func TestPolicySameTierRecoveryUsesNextNeighbor(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snap *domain.CompiledSnapshot) {
		snap.Policies["main"].Recovery[domain.FailRateLimit] = domain.RecoveryAction{
			NextTarget: "same_tier",
		}
	})
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailRateLimit, Message: "slow down"}

	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	if res.Target != "t2" {
		t.Fatalf("target = %q, want same-tier neighbor t2", res.Target)
	}

	r := f.rows(t)[0]
	if r.Attempts != 2 || r.SelectedTarget != "t2" {
		t.Fatalf("record = %+v, want two attempts ending at t2", r)
	}
}

func TestNextTierRecovery(t *testing.T) {
	f := newFixture(t)
	// quota exhaustion recovers by jumping straight to the NEXT TIER,
	// skipping remaining same-tier neighbors (t2 is never tried)
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailQuotaExhausted, Message: "quota spent"}

	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	if res.Target != "t3" {
		t.Errorf("target = %q, want t3 (next tier)", res.Target)
	}
	if f.calls["t2"].invokes.Load() != 0 {
		t.Error("next_tier recovery must skip same-tier neighbors")
	}

	db := f.tl.reopen(t)
	r := f.rows(t)[0]
	if r.Status != domain.ReqSuccess || r.Attempts != 2 || r.SelectedTarget != "t3" {
		t.Errorf("record = %+v", r)
	}
	tr, _ := db.QueryTrace(r.ID)
	var sawTier bool
	for _, ev := range tr.RecoveryEvents {
		if ev.Action == "next_tier" && ev.NextTarget == "t3" {
			sawTier = true
		}
	}
	if !sawTier {
		t.Errorf("no next_tier recovery event in %+v", tr.RecoveryEvents)
	}
}

func TestFullExhaustionReturnsTypedFailure(t *testing.T) {
	f := newFixture(t)
	// quota exhaustion skips same-tier neighbors, so t1 (primary) then
	// t3 (fallback) are the only targets tried
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailQuotaExhausted, Message: "quota spent"}
	f.calls["t3"].connect = &domain.Failure{Type: domain.FailQuotaExhausted, Message: "quota spent"}

	_, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr == nil || ferr.Type != domain.FailQuotaExhausted {
		t.Fatalf("failure = %v, want quota_exhausted", ferr)
	}
	db := f.tl.reopen(t)
	r := f.rows(t)[0]
	if r.Status != domain.ReqFailed || r.FailureType != domain.FailQuotaExhausted || r.Attempts != 2 {
		t.Errorf("record = %+v", r)
	}
	if len(attemptsOf(t, db, r.ID)) != 2 {
		t.Error("attempt rows missing")
	}
}

func TestAuthenticationFailureAlwaysFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailAuth, Message: "bad upstream key"}
	_, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr == nil || ferr.Type != domain.FailAuth {
		t.Fatalf("default auth failure = %v, want fail-closed", ferr)
	}

	// Even a malformed legacy override may not turn upstream credential
	// failures into traffic sent to another provider.
	f.publish(2, func(snap *domain.CompiledSnapshot) {
		snap.Policies["main"].Recovery[domain.FailAuth] = domain.RecoveryAction{NextTarget: "same_tier"}
	})
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailAuth, Message: "bad upstream key"}
	_, ferr = f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr == nil || ferr.Type != domain.FailAuth {
		t.Fatalf("malformed override failure = %v, want auth", ferr)
	}
	r := f.rows(t)[0]
	if r.Attempts != 1 || r.SelectedTarget != "t1" {
		t.Errorf("fail-closed record = %+v, want single attempt on t1", r)
	}
}

func TestOrderedRouteFallbackUsesCurrentStepFailureAllowlist(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snap *domain.CompiledSnapshot) {
		snap.Policies["main"].StepRecovery = []map[domain.FailureType]bool{
			{domain.FailRateLimit: true},
			{},
		}
	})
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailProvider5xx, Message: "overloaded"}
	_, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr == nil || ferr.Type != domain.FailProvider5xx {
		t.Fatalf("unlisted failure recovered: %v", ferr)
	}
	if got := f.rows(t)[0].Attempts; got != 1 {
		t.Fatalf("unlisted failure attempts = %d, want 1", got)
	}

	g := newFixture(t)
	g.publish(2, func(snap *domain.CompiledSnapshot) {
		snap.Policies["main"].StepRecovery = []map[domain.FailureType]bool{
			{domain.FailRateLimit: true},
			{},
		}
	})
	g.calls["t1"].connect = &domain.Failure{Type: domain.FailRateLimit, Message: "busy"}
	res, ferr := g.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil || res.Target != "t2" {
		t.Fatalf("listed failure did not recover within route step: res=%v err=%v", res, ferr)
	}
}

func TestAbortClassDoesNotRetry(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailUnsupportedParam, Message: "param rejected"}

	_, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr == nil || ferr.Type != domain.FailUnsupportedParam {
		t.Fatalf("failure = %v, want unsupported_param", ferr)
	}
	if f.calls["t2"].invokes.Load() != 0 || f.calls["t3"].invokes.Load() != 0 {
		t.Error("abort-class failure must not retry other targets")
	}
}

func TestCompatDegradedAllowedByDefaultAndRecorded(t *testing.T) {
	f := newFixture(t)
	for _, c := range f.calls {
		c.notes = []string{"thinking dropped"}
	}

	result, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("best-effort request failed: %v", ferr)
	}
	if result.CompatLevel != domain.CompatDegraded || !strings.Contains(strings.Join(result.CompatNotes, "; "), "thinking dropped") {
		t.Fatalf("degradation was not recorded: %+v", result)
	}
	if f.calls[result.Target].invokes.Load() != 1 {
		t.Fatalf("degraded request did not reach selected upstream: target=%s", result.Target)
	}
	r := f.rows(t)[0]
	if r.Status != domain.ReqSuccess || r.CompatLevel != domain.CompatDegraded || len(r.CompatNotes) != 1 {
		t.Fatalf("ledger did not record degraded success: %+v", r)
	}
	if len(r.Exclusions) != 0 {
		t.Errorf("successful degraded request unexpectedly excluded targets: %v", r.Exclusions)
	}
}

func TestCompatibilityNotesFailClosedWithoutKeywordHeuristics(t *testing.T) {
	if got := worstNoteLevel([]string{"reasoning effort mapped to a provider bucket"}); got != domain.CompatDegraded {
		t.Fatalf("approximate compatibility note classified as %s", got)
	}
	if got := worstNoteLevel(nil); got != domain.CompatMapped {
		t.Fatalf("lossless conversion classified as %s", got)
	}
}

func TestCompatDegradedExactOnlyRejectsBeforeAttempt(t *testing.T) {
	f := newFixture(t)
	for _, c := range f.calls {
		c.notes = []string{"thinking dropped"}
	}
	w := f.sch.world.Load()
	f.sch.ReplaceWorldWithAPIKeys(w.snap, w.callers, nil, []domain.APIKeyAuthenticator{{
		Identity:     domain.APIKeyIdentity{ID: "strict", Name: "strict", Enabled: true, ExactOnly: true},
		SecretHashes: [][32]byte{domain.HashAPIKeySecret("sk")},
	}})

	if _, failure := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{BearerToken: "sk"}); failure == nil ||
		!strings.Contains(failure.Message, "exact_only") {
		t.Fatalf("exact_only did not reject degraded request: %+v", failure)
	}
	for _, caller := range f.calls {
		if caller.invokes.Load() != 0 {
			t.Fatal("degraded request reached an upstream")
		}
	}
}

func TestConsumerKeyResolution(t *testing.T) {
	f := newFixture(t)

	// Empty consumer configuration is a locked-down state, never an implicit
	// anonymous mode on an exposed data plane.
	f.publishKeys(1, nil, nil)
	_, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr == nil || ferr.Type != domain.FailAuth {
		t.Fatalf("empty key set must fail closed: %v", ferr)
	}

	// unknown token against a configured key set
	f.publishKeys(1, nil, []config.ConsumerKey{{Name: "alice", Key: "sk-alice"}})
	_, ferr = f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{BearerToken: "wrong"})
	if ferr == nil || ferr.Type != domain.FailAuth || !strings.Contains(ferr.Message, "invalid or disabled") {
		t.Fatalf("wrong token: %v", ferr)
	}

	// explicitly disabled key
	disabled := false
	f.publishKeys(1, nil, []config.ConsumerKey{{Name: "bob", Key: "sk-bob", Enabled: &disabled}})
	_, ferr = f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{BearerToken: "sk-bob"})
	if ferr == nil || ferr.Type != domain.FailAuth {
		t.Fatalf("disabled key: %v", ferr)
	}

	// valid key resolves identity into the ledger
	f.publishKeys(1, nil, []config.ConsumerKey{{Name: "alice", Key: "sk-alice"}})
	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{BearerToken: "sk-alice"})
	if ferr != nil {
		t.Fatalf("valid key: %v", ferr)
	}
	rows := f.rows(t)
	if len(rows) != 4 {
		t.Fatalf("all accepted and rejected requests must be recorded: %d", len(rows))
	}
	for _, r := range rows {
		if r.ID == res.LedgerRequestID {
			if r.ConsumerKey != "alice" {
				t.Errorf("consumer key = %q, want alice", r.ConsumerKey)
			}
			return
		}
	}
	t.Fatal("successful request not recorded")
}

func TestActiveAndGraceSecretsResolveOneCallerIdentity(t *testing.T) {
	f := newFixture(t)
	snap := baseSnapshot(2)
	callers := make(map[domain.ResourceTargetID]egress.Caller, len(f.calls))
	for id, caller := range f.calls {
		callers[id] = caller
	}
	f.sch.ReplaceWorldWithAPIKeys(snap, callers, testCards, []domain.APIKeyAuthenticator{{
		Identity: domain.APIKeyIdentity{ID: "key_personal", Name: "personal", Enabled: true},
		SecretHashes: [][32]byte{
			domain.HashAPIKeySecret("sk-active"),
			domain.HashAPIKeySecret("sk-grace"),
		},
	}})
	for _, secret := range []string{"sk-active", "sk-grace"} {
		if _, failure := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{BearerToken: secret}); failure != nil {
			t.Fatalf("secret %s did not authenticate: %v", secret, failure)
		}
	}
	rows := f.rows(t)
	if len(rows) != 2 || rows[0].ConsumerKey != "personal" || rows[1].ConsumerKey != "personal" {
		t.Fatalf("rotated secrets split caller identity: %+v", rows)
	}
	if _, failure := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{BearerToken: "sk-retired"}); failure == nil || failure.Type != domain.FailAuth || failure.Scope != domain.FailureScopeRequest {
		t.Fatalf("retired/unknown secret failure = %+v", failure)
	}
}

func TestUnknownAliasFails(t *testing.T) {
	f := newFixture(t)
	_, ferr := f.sch.Execute(context.Background(), chatReq("nope"), RequestMeta{})
	if ferr == nil || ferr.Type != domain.FailUnsupportedParam ||
		!strings.Contains(ferr.Message, "unknown model/alias") {
		t.Fatalf("failure = %v, want unknown model/alias", ferr)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)

	plan, pol, ferr := f.sch.DryRun(chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("dry-run: %v", ferr)
	}
	if plan.Primary != "t1" || pol.Name != "main" {
		t.Errorf("plan primary=%s policy=%s, want t1/main", plan.Primary, pol.Name)
	}
	cands, ferr := f.sch.DryRunExclusions(chatReq("main"), RequestMeta{})
	if ferr != nil || len(cands) != 3 {
		t.Fatalf("candidates = %d (err %v), want 3 eligible", len(cands), ferr)
	}

	if _, _, ferr := f.sch.DryRun(chatReq("ghost"), RequestMeta{}); ferr == nil {
		t.Fatal("unknown alias dry-run must fail")
	}
	if f.rows(t) != nil && len(f.rows(t)) != 0 {
		t.Errorf("dry-run wrote %d ledger rows, want 0", len(f.rows(t)))
	}
}

func collectEvents(t *testing.T, ch <-chan domain.StreamEvent) []domain.StreamEvent {
	t.Helper()
	var out []domain.StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestStreamPreCommitFailover(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].connect = &domain.Failure{Type: domain.FailConnect, Message: "refused"}

	res, ferr := f.sch.Execute(context.Background(), func() *domain.RequestContract {
		rc := chatReq("main")
		rc.Stream = true
		return rc
	}(), RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	evs := collectEvents(t, res.Events)
	var kinds []domain.EventKind
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	if len(evs) < 2 || evs[len(evs)-1].Kind != domain.EvEnd {
		t.Fatalf("events = %v, want deltas ending in EvEnd", kinds)
	}
	if res.Target != "t2" {
		t.Errorf("committed target = %q, want t2", res.Target)
	}

	db := f.tl.reopen(t)
	r := f.rows(t)[0]
	if r.Status != domain.ReqSuccess || r.Attempts != 2 || r.SelectedTarget != "t2" {
		t.Errorf("record = %+v", r)
	}
	if r.TTFTMS <= 0 {
		t.Logf("note: ttft=%dms", r.TTFTMS)
	}
	_ = db
}

func TestStreamBuffersNativeLifecycleFramesUntilMeaningfulCommit(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].streamEvents = []domain.StreamEvent{
		{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat,
			NativeFrame: &domain.NativeSSEFrame{Data: `{"id":"upstream","choices":[]}`}},
		{Kind: domain.EvError, Err: &domain.Failure{
			Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
			Message: "upstream ended before content"}},
	}

	request := chatReq("main")
	request.Stream = true
	res, failure := f.sch.Execute(context.Background(), request, RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	events := collectEvents(t, res.Events)
	if res.Target != "t2" || f.calls["t2"].streams.Load() != 1 {
		t.Fatalf("native lifecycle frame committed the broken target: result=%+v fallback_streams=%d",
			res, f.calls["t2"].streams.Load())
	}
	for _, event := range events {
		if event.Kind == domain.EvNative {
			t.Fatalf("discarded target leaked a native frame: %+v", event)
		}
	}
}

func TestStreamReleasesBufferedNativeFramesAfterValidatedTerminal(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].streamEvents = []domain.StreamEvent{
		{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat,
			NativeFrame: &domain.NativeSSEFrame{Data: `{"id":"upstream","choices":[]}`}},
		{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat,
			NativeFrame: &domain.NativeSSEFrame{Data: "[DONE]", Terminal: true}},
		{Kind: domain.EvEnd, FinishReason: "stop"},
	}

	request := chatReq("main")
	request.Stream = true
	res, failure := f.sch.Execute(context.Background(), request, RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	events := collectEvents(t, res.Events)
	if res.Target != "t1" || f.calls["t2"].streams.Load() != 0 {
		t.Fatalf("validated native terminal stream switched target: result=%+v fallback_streams=%d",
			res, f.calls["t2"].streams.Load())
	}
	if len(events) != 3 || events[0].Kind != domain.EvNative || events[1].Kind != domain.EvNative ||
		events[1].NativeFrame == nil || !events[1].NativeFrame.Terminal || events[2].Kind != domain.EvEnd {
		t.Fatalf("buffered native frames were not released in order: %+v", events)
	}
}

func TestStreamWithholdsNativeTerminalFrameWhenFinalValidationFails(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].streamEvents = []domain.StreamEvent{
		{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat,
			NativeFrame: &domain.NativeSSEFrame{Data: `{"choices":[{"delta":{"content":"not json"}}]}`}},
		{Kind: domain.EvTextDelta, TextDelta: "not json"},
		{Kind: domain.EvNative, NativeProtocol: domain.ProtocolOpenAIChat,
			NativeFrame: &domain.NativeSSEFrame{Data: "[DONE]", Terminal: true}},
		{Kind: domain.EvEnd, FinishReason: "stop"},
	}

	request := chatReq("main")
	request.Stream = true
	request.ResponseFormat = &domain.ResponseFormat{}
	res, failure := f.sch.Execute(context.Background(), request, RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	events := collectEvents(t, res.Events)
	if len(events) == 0 || events[len(events)-1].Kind != domain.EvError ||
		events[len(events)-1].Err == nil || events[len(events)-1].Err.Type != domain.FailBadResponse {
		t.Fatalf("invalid structured stream did not end with a protocol error: %+v", events)
	}
	for _, event := range events {
		if event.Kind == domain.EvNative && event.NativeFrame != nil && event.NativeFrame.Terminal {
			t.Fatalf("successful native terminal leaked before final validation: %+v", events)
		}
	}
	if f.calls["t2"].streams.Load() != 0 {
		t.Fatal("post-commit structured output failure switched targets")
	}
}

func TestStreamPostCommitErrorPassesThrough(t *testing.T) {
	f := newFixture(t)
	f.calls["t1"].midErr = &domain.Failure{Type: domain.FailProvider5xx, Message: "upstream blew up"}

	res, ferr := f.sch.Execute(context.Background(), func() *domain.RequestContract {
		rc := chatReq("main")
		rc.Stream = true
		return rc
	}(), RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v (post-commit errors are in-band)", ferr)
	}
	evs := collectEvents(t, res.Events)
	last := evs[len(evs)-1]
	if last.Kind != domain.EvError || last.Err == nil || last.Err.Type != domain.FailProvider5xx {
		t.Fatalf("last event = %+v, want in-band EvError(provider_5xx)", last)
	}
	if f.calls["t2"].streams.Load() != 0 {
		t.Error("post-commit error must not switch targets")
	}

	r := f.rows(t)[0]
	if r.Status != domain.ReqFailed || r.FailureType != domain.FailProvider5xx || r.Attempts != 1 {
		t.Errorf("record = %+v", r)
	}
}

func TestHotReplaceWorld(t *testing.T) {
	f := newFixture(t)

	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil || res.Target != "t1" || res.PolicyVer != 1 {
		t.Fatalf("v1: res=%v err=%v", res, ferr)
	}

	// v2 promotes t2 ahead of t1
	f.publish(2, func(snap *domain.CompiledSnapshot) {
		snap.Targets["t2"].Priority = -10
	})
	if got := f.sch.Snapshot(); got == nil || got.Version != 2 {
		t.Fatalf("snapshot version = %v, want 2", got)
	}
	res2, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("v2 execute: %v", ferr)
	}
	if res2.Target != "t2" || res2.PolicyVer != 2 {
		t.Errorf("v2 routed to %s@%d, want t2@2", res2.Target, res2.PolicyVer)
	}
}

// ---------------------------------------------------------------------------
// Synthesized-alias routing: channel-model names are concrete model names, so
// pools mixing several models of one channel must route strictly by model.

func TestSynthesizedAliasRoutesByModel(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snap *domain.CompiledSnapshot) {
		snap.Targets["t1"].Model = "opus"
		snap.Targets["t2"].Model = "deepseek-v4"
		snap.Aliases["opus"] = &domain.CompiledAlias{
			Name:         "opus",
			Synthesized:  true,
			RequiredCaps: domain.Requirements{Text: true},
		}
		snap.Aliases["deepseek-v4"] = &domain.CompiledAlias{
			Name:         "deepseek-v4",
			Synthesized:  true,
			RequiredCaps: domain.Requirements{Text: true},
		}
	})

	// opus request must land on the opus target even though t2 outranks it
	// lexicographically within the same pool.
	res, ferr := f.sch.Execute(context.Background(), chatReq("opus"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	if res.Target != "t1" {
		t.Fatalf("opus request routed to %s (model %q), want t1 (opus)", res.Target, res.Model)
	}

	// deepseek request: same pool — t1 is excluded by model name, not capability.
	cands, _ := f.sch.DryRunExclusions(chatReq("deepseek-v4"), RequestMeta{})
	seenT1 := false
	for _, c := range cands {
		if c.Target == "t1" {
			seenT1 = true
			if c.Eligible || !strings.Contains(c.ExclusionReason, "model mismatch") ||
				!strings.Contains(c.ExclusionReason, `"deepseek-v4"`) {
				t.Errorf("t1 candidate wrong: eligible=%v reason=%q", c.Eligible, c.ExclusionReason)
			}
		}
	}
	if !seenT1 {
		t.Error("t1 missing from dry-run candidates")
	}

	// the exclusion trail persists for the decision trace
	res2, ferr := f.sch.Execute(context.Background(), chatReq("deepseek-v4"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("deepseek execute: %v", ferr)
	}
	if res2.Target != "t2" {
		t.Fatalf("deepseek request routed to %s, want t2", res2.Target)
	}
	var row *domain.RequestRecord
	for i := range f.rows(t) {
		if f.rows(t)[i].ModelRequested == "deepseek-v4" {
			row = &f.rows(t)[i]
		}
	}
	if row == nil {
		t.Fatal("deepseek ledger row not found")
	}
	found := false
	for _, ex := range row.Exclusions {
		if ex.Target == "t1" && strings.Contains(ex.Reason, "model mismatch") {
			found = true
		}
	}
	if !found {
		t.Errorf("ledger exclusions lack t1 model-mismatch entry: %+v", row.Exclusions)
	}
}

func TestDisabledChannelTargetExcludedWithReason(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snap *domain.CompiledSnapshot) {
		snap.Targets["t1"].Disabled = true
	})

	res, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{})
	if ferr != nil {
		t.Fatalf("disabled t1 must fail over to t2, got %v", ferr)
	}
	if res.Target != "t2" {
		t.Fatalf("routed to %s, want t2 after disabling t1", res.Target)
	}
	var row *domain.RequestRecord
	for i := range f.rows(t) {
		if f.rows(t)[i].SelectedTarget == "t2" {
			row = &f.rows(t)[i]
		}
	}
	if row == nil {
		t.Fatal("ledger row not found")
	}
	found := false
	for _, ex := range row.Exclusions {
		if ex.Target == "t1" && strings.Contains(ex.Reason, "channel disabled") {
			found = true
		}
	}
	if !found {
		t.Errorf("ledger exclusions lack t1 'channel disabled' entry: %+v", row.Exclusions)
	}
}

// ---------------------------------------------------------------------------
// Phase 2+3: channel-expanded multi-protocol targets

// multiProtoSnapshot turns every fixture target into a channel-expanded model
// speaking all three convertible text wire formats natively, so the per-attempt egress protocol
// equals the ingress protocol and the attempt rows must record it.
func multiProtoSnapshot(snap *domain.CompiledSnapshot) {
	for _, t := range snap.Targets {
		t.NativeProtocols = []domain.Protocol{
			domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic,
		}
	}
}

func TestMultiProtocolNativePassthrough(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snap *domain.CompiledSnapshot) {
		multiProtoSnapshot(snap)
	})

	// anthropic-ingress request against a native-anthropic target:
	// exact compatibility, no conversion notes.
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolAnthropic,
		Model:    "main",
		Messages: []domain.Message{{
			Role:   domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}},
		}},
	}
	res, ferr := f.sch.Execute(context.Background(), rc, RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	if res.CompatLevel != domain.CompatExact {
		t.Errorf("compat = %s, want exact for native passthrough", res.CompatLevel)
	}
	// same world, chat-ingress: also native (all targets speak chat). Run it
	// before reopening the ledger (reopen closes the live handle).
	if _, ferr := f.sch.Execute(context.Background(), chatReq("main"), RequestMeta{}); ferr != nil {
		t.Fatalf("chat execute: %v", ferr)
	}

	db := f.tl.reopen(t)
	recs, _, _ := db.QueryRequests(domain.LogFilter{})
	if len(recs) != 2 {
		t.Fatalf("rows = %d, want 2", len(recs))
	}
	var anthropicRow, chatRow domain.RequestRecord
	for _, r := range recs {
		if r.Protocol == domain.ProtocolAnthropic {
			anthropicRow = r
		} else {
			chatRow = r
		}
	}
	as := attemptsOf(t, db, anthropicRow.ID)
	if len(as) != 1 {
		t.Fatalf("attempts = %d", len(as))
	}
	if as[0].Protocol != domain.ProtocolAnthropic {
		t.Errorf("attempt Protocol = %q, want anthropic (actual egress wire)", as[0].Protocol)
	}
	as = attemptsOf(t, db, chatRow.ID)
	if len(as) != 1 || as[0].Protocol != domain.ProtocolOpenAIChat {
		t.Errorf("chat attempt = %+v, want 1 attempt on openai-chat", as)
	}
}

// recordingCaller captures the contracts it receives; it wraps fakeCaller's
// response behavior while exposing per-attempt protocol/model/native state.
type recordingCaller struct {
	inner egress.Caller

	mu    sync.Mutex
	calls []domain.RequestContract
	prep  func(rc *domain.RequestContract) ([]string, error)
}

var (
	_ egress.Caller = (*recordingCaller)(nil)
)

func (c *recordingCaller) Prepare(rc *domain.RequestContract) ([]string, error) {
	if c.prep != nil {
		return c.prep(rc)
	}
	if p, ok := c.inner.(egress.Preparer); ok {
		return p.Prepare(rc)
	}
	return nil, nil
}

func (c *recordingCaller) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	c.mu.Lock()
	c.calls = append(c.calls, *rc)
	c.mu.Unlock()
	return c.inner.Invoke(ctx, rc)
}

func (c *recordingCaller) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	return c.inner.Stream(ctx, rc)
}

func TestMultiProtocolConvertedAttemptDropsNativeBody(t *testing.T) {
	f := newFixture(t)

	// t1 speaks ONLY responses natively: a chat request converts to responses
	// there, while classic chat targets would be exact — so give the fake
	// Prepare a responses-adapter view of a chat contract (no notes: clean
	// mapping) and pin both tiers to t1 for determinism.
	rec := &recordingCaller{inner: &fakeCaller{}}
	snap := baseSnapshot(2)
	snap.Targets["t1"].NativeProtocols = []domain.Protocol{domain.ProtocolOpenAIResponses}
	snap.Pools["poolA"] = []domain.ResourceTargetID{"t1"}
	snap.Pools["poolB"] = []domain.ResourceTargetID{"t1"}
	f.sch.ReplaceWorld(
		snap,
		map[domain.ResourceTargetID]egress.Caller{
			"t1": rec, "t2": f.calls["t2"], "t3": f.calls["t3"],
		},
		testCards, []config.ConsumerKey{{Name: "anonymous", Key: ""}},
	)

	rc := chatReq("main")
	rc.Native = json.RawMessage(`{"model":"main","messages":[]}`)

	res, ferr := f.sch.Execute(context.Background(), rc, RequestMeta{})
	if ferr != nil {
		t.Fatalf("execute: %v", ferr)
	}
	if res.Target != "t1" {
		t.Fatalf("target = %q, want converted target t1", res.Target)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("recording caller invoked %d times, want 1", len(rec.calls))
	}
	got := rec.calls[0]
	// chat ingress → responses-only target: converted, so the raw body must
	// be dropped and the wire protocol rewritten.
	if got.Protocol != domain.ProtocolOpenAIResponses {
		t.Errorf("egress protocol = %q, want openai-responses (converted)", got.Protocol)
	}
	if got.Origin != domain.ProtocolOpenAIChat {
		t.Errorf("origin = %q, want openai-chat (client wire)", got.Origin)
	}
	if len(got.Native) != 0 {
		t.Error("converted attempts must drop the preserved raw body")
	}
	if got.Model != testCardModel {
		t.Errorf("model = %q, want upstream name %q", got.Model, testCardModel)
	}
}

func TestMultiProtocolStreamAttemptLogsActualProtocol(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(snap *domain.CompiledSnapshot) { multiProtoSnapshot(snap) })

	rc := &domain.RequestContract{
		Protocol: domain.ProtocolAnthropic,
		Model:    "main",
		Stream:   true,
		Messages: []domain.Message{{
			Role:   domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}},
		}},
	}
	res, ferr := f.sch.Execute(context.Background(), rc, RequestMeta{})
	if ferr != nil {
		t.Fatalf("stream execute: %v", ferr)
	}
	for ev := range res.Events {
		if ev.Kind == domain.EvEnd || ev.Kind == domain.EvError {
			break
		}
	}
	if res.Cancel != nil {
		res.Cancel()
	}

	db := f.tl.reopen(t)
	recs, _, _ := db.QueryRequests(domain.LogFilter{})
	if len(recs) != 1 {
		t.Fatalf("rows = %d", len(recs))
	}
	as := attemptsOf(t, db, recs[0].ID)
	if len(as) != 1 {
		t.Fatalf("attempts = %d", len(as))
	}
	if as[0].Protocol != domain.ProtocolAnthropic {
		t.Errorf("stream attempt Protocol = %q, want anthropic", as[0].Protocol)
	}
}

// Auxiliary mapping notes (cache_control, context_management, verbosity and
// other optimization hints) are intentional translations: they must stay
// visible in the request trail while never degrading the compatibility level.
// Regression guard for the live failure where aux notes were classified as
// degraded and rejected by the zero-silent-degradation policy.
func TestAuxiliaryMappingNotesStayMappedAndReachTheTrail(t *testing.T) {
	f := newFixture(t)
	req := chatReq("main")
	req.MappingNotes = []string{
		"cache_control omitted (target applies automatic prompt caching)",
		"context_management edits omitted (full conversation history preserved)",
	}
	res, failure := f.sch.Execute(context.Background(), req, RequestMeta{})
	if failure != nil {
		t.Fatalf("auxiliary mapping notes must not block execution: %v", failure)
	}
	if res.CompatLevel != domain.CompatExact {
		t.Fatalf("compat level = %s, want exact (aux notes never affect level)", res.CompatLevel)
	}
	joined := strings.Join(res.CompatNotes, "; ")
	for _, want := range []string{"cache_control omitted", "context_management edits omitted"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("compat notes %q missing %q", joined, want)
		}
	}
}
