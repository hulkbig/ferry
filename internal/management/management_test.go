package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	sched "ferry/internal/scheduler"
)

// stubCaller satisfies egress.Caller + Preparer so compat assessment treats
// the target as reachable during dry runs (no network involved).
type stubCaller struct{}

var _ egress.Caller = (*stubCaller)(nil)
var _ egress.Preparer = (*stubCaller)(nil)

func (stubCaller) Invoke(context.Context, *domain.RequestContract) (*domain.Response, *domain.Failure) {
	return &domain.Response{FinishReason: "stop"}, nil
}

func (stubCaller) Stream(context.Context, *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	ch := make(chan domain.StreamEvent, 1)
	ch <- domain.StreamEvent{Kind: domain.EvEnd, FinishReason: "stop"}
	return ch, func() {}, nil
}

func (stubCaller) Prepare(*domain.RequestContract) ([]string, error) { return nil, nil }

// ---------------------------------------------------------------------------
// harness

type mgmtFixture struct {
	t              *testing.T
	srv            *Server
	sch            *sched.Scheduler
	mgr            *runtime.Manager
	led            *ledger.Ledger
	cfg            *config.File
	pubErr         error // when set, the fake Publish fails with this
	publishMu      sync.Mutex
	publishes      int
	published      []*config.File
	controlVersion int64
}

func newMgmtFixture(t *testing.T) *mgmtFixture {
	t.Helper()
	mgr := runtime.NewManager(runtime.DefaultHealthConfig())
	l, err := ledger.Open(t.TempDir(), 256, 0)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	f := &mgmtFixture{t: t, mgr: mgr, led: l, controlVersion: 7}
	f.sch = sched.New(mgr, l)
	f.cfg = testConfig()

	compiler := policy.NewCompiler()
	f.srv = New(Deps{
		Scheduler: f.sch,
		Manager:   mgr,
		Ledger:    l,
		Config:    f.cfg,
		Compiler:  compiler,
		Publish: func(cf *config.File) error {
			f.publishMu.Lock()
			defer f.publishMu.Unlock()
			f.publishes++
			if f.pubErr != nil {
				return f.pubErr
			}
			if err := cf.Validate(); err != nil {
				return err
			}
			s, err := compiler.Compile(cf)
			if err != nil {
				return err
			}
			f.published = append(f.published, cf)
			// Model the exact revision that a successful durable SaveConfig
			// would return before the runtime world is replaced.
			s.Version = atomic.AddInt64(&f.controlVersion, 1)
			f.sch.ReplaceWorld(s, nil, nil, nil)
			return nil
		},
		ControlVersion: func() (int64, error) {
			return atomic.LoadInt64(&f.controlVersion), nil
		},
		Timezone: time.UTC,
	})
	// initial world so /api/state has something to describe
	if err := f.cfg.Validate(); err != nil {
		t.Fatalf("validate fixture config: %v", err)
	}
	f.sch.ReplaceWorld(fixtureSnapshot(), nil, nil, nil)
	return f
}

func (f *mgmtFixture) publishCount() int {
	f.publishMu.Lock()
	defer f.publishMu.Unlock()
	return f.publishes
}

func (f *mgmtFixture) versionedPolicyPayload(cf *config.File) map[string]any {
	return map[string]any{
		"config":           cf,
		"expected_version": atomic.LoadInt64(&f.controlVersion),
	}
}

// testConfig is a minimal but valid config for compile/publish paths.
func testConfig() *config.File {
	return &config.File{
		Endpoints: []domain.Endpoint{{ID: "ep1", BaseURL: "http://127.0.0.1:9", Protocol: domain.ProtocolOpenAIChat}},
		Credentials: map[string]domain.CredentialRef{
			"c1": {SecretRef: "env:CPA_KEY"},
		},
		Plans: []domain.Plan{{ID: "free", Kind: domain.PlanFree, Currency: "USD"}},
		Targets: []domain.ResourceTarget{{
			ID: "t1", EndpointID: "ep1", Credential: domain.CredentialRef{SecretRef: "env:CPA_KEY"},
			Model: "m1", PlanID: "free",
			Capabilities: domain.Capabilities{Text: true, Stream: true},
		}},
		Pools: []domain.Pool{{ID: "p1", Targets: []domain.ResourceTargetID{"t1"}}},
		Policies: map[string]config.PolicyYAML{
			"main": {Tiers: []domain.TierDef{{Name: "primary", Pool: "p1"}}},
		},
		DefaultPolicy: "main",
	}
}

func fixtureSnapshot() *domain.CompiledSnapshot {
	return &domain.CompiledSnapshot{
		Version:        7,
		V2DomainSource: config.V2DomainSourceLegacy,
		CreatedAt:      time.Now(),
		Hash:           "abc123",
		Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{
			"t1": {
				ID:       "t1",
				Endpoint: &domain.Endpoint{ID: "ep1", BaseURL: "http://127.0.0.1:9", Protocol: domain.ProtocolOpenAIChat},
				Model:    "m1", Plan: &domain.Plan{ID: "free", Kind: domain.PlanFree},
				Capabilities: domain.Capabilities{Text: true, Stream: true},
				PoolIDs:      []string{"p1"},
			},
		},
		TargetOrder:   []domain.ResourceTargetID{"t1"},
		Pools:         map[string][]domain.ResourceTargetID{"p1": {"t1"}},
		Aliases:       map[string]*domain.CompiledAlias{},
		Objectives:    map[string]*domain.ObjectiveProfile{"balanced": {Name: "balanced"}},
		Policies:      map[string]*domain.CompiledPolicy{},
		DefaultPolicy: "main",
	}
}

func (f *mgmtFixture) do(method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	f.srv.legacyRoutesForTest().ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

// ---------------------------------------------------------------------------
// tests

func TestStateEndpoint(t *testing.T) {
	f := newMgmtFixture(t)
	snapshot := fixtureSnapshot()
	snapshot.Targets["t1"].Capabilities.Tools = true
	f.sch.ReplaceWorld(snapshot, nil, nil, nil)
	rec, out := f.do(http.MethodGet, "/api/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if v, _ := out["version"].(float64); int64(v) != 7 {
		t.Errorf("version = %v, want 7", out["version"])
	}
	targets, _ := out["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	tv := targets[0].(map[string]any)
	if tv["id"] != "t1" || tv["client_model"] != "m1" {
		t.Errorf("target view = %v", tv)
	}
	upstream := tv["upstream_capabilities"].(map[string]any)
	effective := tv["capabilities"].(map[string]any)
	if upstream["parallel_tools"] != false || effective["parallel_tools"] != true {
		t.Fatalf("state did not distinguish upstream and effective capabilities: %v", tv)
	}
}

func TestDashboardAndRequestsEmpty(t *testing.T) {
	f := newMgmtFixture(t)
	rec, out := f.do(http.MethodGet, "/api/dashboard?hours=24", nil)
	if rec.Code != http.StatusOK || out["summary"] == nil {
		t.Fatalf("dashboard = %d %v", rec.Code, out)
	}
	rec, out = f.do(http.MethodGet, "/api/requests", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("requests status = %d", rec.Code)
	}
	if total, _ := out["total"].(float64); total != 0 {
		t.Errorf("total = %v, want 0", out["total"])
	}
}

func TestRequestDetailShape(t *testing.T) {
	f := newMgmtFixture(t)
	id := "req-detail-1"
	f.led.BeginRequest(domain.RequestRecord{ID: id})
	f.led.AddAttempt(domain.AttemptRecord{RequestID: id, Seq: 1, TargetID: "t1", Status: domain.ReqSuccess})
	f.led.SetTrace(id, &domain.TraceRecord{PolicyName: "main", Primary: "t1"})
	f.led.FinishRequest(id, func(r *domain.RequestRecord) {
		r.Status = domain.ReqSuccess
		r.SelectedTarget = "t1"
	})
	time.Sleep(50 * time.Millisecond) // async writer

	rec, out := f.do(http.MethodGet, "/api/requests/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	attempts, _ := out["attempts"].([]any)
	if len(attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(attempts))
	}
	trace, _ := out["trace"].(map[string]any)
	if trace == nil || trace["primary"] != "t1" {
		t.Errorf("trace = %v", trace)
	}
}

func TestStructuredConfigRedactsConsumerKeysAndRetiresYAML(t *testing.T) {
	f := newMgmtFixture(t)
	on := true
	f.cfg.ConsumerKeys = []config.ConsumerKey{{Name: "alice", Key: "sk-never-list-me", Enabled: &on}}
	rec, out := f.do(http.MethodGet, "/api/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	raw, _ := json.Marshal(out)
	body := string(raw)
	for _, secret := range []string{"sk-live-", "BEGIN PRIVATE KEY", "sk-never-list-me"} {
		if strings.Contains(body, secret) {
			t.Errorf("config endpoint leaked %q", secret)
		}
	}
	if !strings.Contains(body, "main") {
		t.Errorf("policy names missing from config view: %s", body)
	}

	// YAML is no longer a control-plane format.
	rec, _ = f.do(http.MethodGet, "/api/config/raw", nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("raw YAML status = %d, want 410", rec.Code)
	}

	// Structured JSON remains available, but consumer bearer keys are redacted.
	rec, _ = f.do(http.MethodGet, "/api/config/raw?format=json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("json raw status = %d", rec.Code)
	}
	var jf config.File
	if err := json.Unmarshal(rec.Body.Bytes(), &jf); err != nil {
		t.Fatalf("json raw not parseable: %v", err)
	}
	if jf.DefaultPolicy != "main" {
		t.Errorf("json raw default_policy = %q", jf.DefaultPolicy)
	}
	if len(jf.ConsumerKeys) != 1 || jf.ConsumerKeys[0].Key != "" {
		t.Fatalf("structured config leaked consumer key: %+v", jf.ConsumerKeys)
	}
}

func TestVersionedStructuredConfigExposesMatchingControlVersion(t *testing.T) {
	f := newMgmtFixture(t)
	on := true
	f.srv.deps.Config.ConsumerKeys = []config.ConsumerKey{{Name: "alice", Key: "sk-secret", Enabled: &on}}

	rec, out := f.do(http.MethodGet, "/api/config/raw?format=json&versioned=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	wantVersion := atomic.LoadInt64(&f.controlVersion)
	version, ok := out["control_version"].(float64)
	if !ok || int64(version) != wantVersion {
		t.Fatalf("control_version = %v, want %d", out["control_version"], wantVersion)
	}
	if got := rec.Header().Get("X-SmartAPI-Control-Version"); got != fmt.Sprint(wantVersion) {
		t.Fatalf("X-SmartAPI-Control-Version = %q, want %d", got, wantVersion)
	}
	if got := rec.Header().Get("X-Ferry-Control-Version"); got != fmt.Sprint(wantVersion) {
		t.Fatalf("X-Ferry-Control-Version = %q, want %d", got, wantVersion)
	}
	if got := rec.Header().Get("ETag"); got != fmt.Sprintf(`"control-%d"`, wantVersion) {
		t.Fatalf("ETag = %q, want control version ETag", got)
	}
	gotConfig, ok := out["config"].(map[string]any)
	if !ok {
		t.Fatalf("versioned response missing config envelope: %v", out)
	}
	keys, _ := gotConfig["ConsumerKeys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["Key"] != "" {
		t.Fatalf("versioned structured config leaked consumer key: %v", gotConfig)
	}
}

func TestExplicitFullConfigPublishRequiresExpectedVersion(t *testing.T) {
	f := newMgmtFixture(t)
	before := f.srv.currentConfig()

	rec, out := f.do(http.MethodPost, "/api/policy/publish", map[string]any{"config": testConfig()})
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d body=%s, want 428", rec.Code, rec.Body.String())
	}
	if f.publishes != 0 || f.srv.currentConfig() != before {
		t.Fatal("unversioned full config reached publish pipeline or changed live config")
	}
	if out["error"] == "" {
		t.Fatal("missing expected-version error")
	}
}

func TestStaleFullConfigPublishCannotOverwriteConcurrentChannelMutation(t *testing.T) {
	f := newMgmtFixture(t)
	staleVersion := atomic.LoadInt64(&f.controlVersion)
	stale := testConfig()
	stale.Objectives = map[string]config.ObjectiveSpec{"cheap": {Rank: []string{"min_cash_cost"}}}

	rec, _ := f.do(http.MethodPost, "/api/channels", channelCreateBody())
	if rec.Code != http.StatusCreated {
		t.Fatalf("concurrent channel create status = %d body=%s", rec.Code, rec.Body.String())
	}
	currentVersion := atomic.LoadInt64(&f.controlVersion)
	if currentVersion == staleVersion {
		t.Fatal("test setup did not advance persistent control version")
	}

	rec, out := f.do(http.MethodPost, "/api/policy/publish", map[string]any{
		"config": stale, "expected_version": staleVersion,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale publish status = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	gotVersion, ok := out["current_version"].(float64)
	if !ok || int64(gotVersion) != currentVersion {
		t.Fatalf("current_version = %v, want %d", out["current_version"], currentVersion)
	}
	if f.publishes != 1 {
		t.Fatalf("stale full config reached publish pipeline: publishes=%d", f.publishes)
	}
	live := f.srv.currentConfig()
	if len(live.Channels) != 1 || live.Channels[0].ID != "gateway" {
		t.Fatalf("stale publish reverted concurrent channel: %+v", live.Channels)
	}
}

func TestStaleFullConfigValidationIsRejected(t *testing.T) {
	f := newMgmtFixture(t)
	staleVersion := atomic.LoadInt64(&f.controlVersion)
	rec, _ := f.do(http.MethodPost, "/api/channels", channelCreateBody())
	if rec.Code != http.StatusCreated {
		t.Fatalf("channel create status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec, out := f.do(http.MethodPost, "/api/policy/validate", map[string]any{
		"config": testConfig(), "expected_version": staleVersion,
	})
	if rec.Code != http.StatusConflict || out["valid"] != false {
		t.Fatalf("stale validation = %d %v, want 409 valid=false", rec.Code, out)
	}
}

func TestVersionedFullConfigValidationUsesCASWithoutPublishing(t *testing.T) {
	f := newMgmtFixture(t)
	version := atomic.LoadInt64(&f.controlVersion)

	rec, out := f.do(http.MethodPost, "/api/policy/validate", map[string]any{
		"config": testConfig(), "expected_version": version,
	})
	if rec.Code != http.StatusOK || out["valid"] != true {
		t.Fatalf("versioned validation = %d %v, want 200 valid=true", rec.Code, out)
	}
	if got, ok := out["control_version"].(float64); !ok || int64(got) != version {
		t.Fatalf("control_version = %v, want %d", out["control_version"], version)
	}
	if got := rec.Header().Get("X-SmartAPI-Control-Version"); got != fmt.Sprint(version) {
		t.Fatalf("X-SmartAPI-Control-Version = %q, want %d", got, version)
	}
	if got := rec.Header().Get("X-Ferry-Control-Version"); got != fmt.Sprint(version) {
		t.Fatalf("X-Ferry-Control-Version = %q, want %d", got, version)
	}
	if f.publishes != 0 {
		t.Fatalf("read-only validation published %d times", f.publishes)
	}
}

func TestValidateEndpoint(t *testing.T) {
	f := newMgmtFixture(t)

	// Historical clients may submit an explicit candidate without a version.
	// That path remains a read-only validation and never reaches Publish.
	rec, out := f.do(http.MethodPost, "/api/policy/validate", policyPayload{Config: testConfig()})
	if rec.Code != http.StatusOK || out["valid"] != true {
		t.Fatalf("validate valid: %d %v", rec.Code, out)
	}
	if f.publishes != 0 {
		t.Fatalf("unversioned read-only validation published %d times", f.publishes)
	}

	// broken candidate: tier references an unknown pool
	bad := testConfig()
	bad.Policies = map[string]config.PolicyYAML{
		"main": {Tiers: []domain.TierDef{{Name: "x", Pool: "ghost-pool"}}},
	}
	rec, out = f.do(http.MethodPost, "/api/policy/validate", policyPayload{Config: bad})
	if rec.Code != http.StatusOK || out["valid"] != false {
		t.Fatalf("validate invalid: %d %v", rec.Code, out)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Error("invalid config must carry an error message")
	}

	// malformed JSON
	req := httptest.NewRequest(http.MethodPost, "/api/policy/validate", strings.NewReader("{nope"))
	w := httptest.NewRecorder()
	f.srv.legacyRoutesForTest().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", w.Code)
	}
}

func TestValidateLiveConfigDoesNotCanonicalizeSharedSnapshotInPlace(t *testing.T) {
	f := newMgmtFixture(t)
	f.srv.deps.Config.PolicyGraphs = map[string]config.PolicyGraphSpec{
		"main": {
			Version: 1,
			Nodes: []config.PolicyGraphNode{
				{ID: "entry", Type: "input"},
				{ID: "channel", Type: "channel", Data: map[string]any{"internalPool": "p1"}},
				{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
			},
			Edges: []config.PolicyGraphEdge{
				{ID: "entry-channel", Source: "entry", SourcePort: "next", Target: "channel"},
				{ID: "channel-done", Source: "channel", SourcePort: "success", Target: "done"},
			},
		},
	}

	rec, out := f.do(http.MethodPost, "/api/policy/validate", map[string]any{})
	if rec.Code != http.StatusOK || out["valid"] != true {
		t.Fatalf("validate live config: %d %v", rec.Code, out)
	}
	if got := f.srv.deps.Config.Policies["main"].Tiers[0].Name; got != "primary" {
		t.Fatalf("read-only validation mutated live policy tier to %q", got)
	}
}

func TestDryRunEndpoint(t *testing.T) {
	f := newMgmtFixture(t)

	// unknown model -> ok:false with typed failure + exclusions
	rec, out := f.do(http.MethodPost, "/api/policy/dry-run", map[string]any{"model": "ghost"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ok, _ := out["ok"].(bool); ok {
		t.Fatalf("dry-run ghost should fail: %v", out)
	}
	if out["failure"] == nil {
		t.Error("failure payload missing")
	}

	// a resolvable alias plans successfully without executing anything
	f.publishAliasWorld()
	rec, out = f.do(http.MethodPost, "/api/policy/dry-run", map[string]any{"model": "main"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("dry-run main failed: %v", out)
	}
	if pol, _ := out["policy"].(string); pol != "main" {
		t.Errorf("policy = %v", out["policy"])
	}
}

// publishAliasWorld swaps in a snapshot whose aliases make DryRun resolvable.
func (f *mgmtFixture) publishAliasWorld() {
	enabled := true
	f.srv.deps.Config.ConsumerKeys = []config.ConsumerKey{{Name: "dry-run", Key: "sk-dry-run", Enabled: &enabled}}
	snap := fixtureSnapshot()
	snap.Aliases["main"] = &domain.CompiledAlias{Name: "main",
		RequiredCaps: domain.Requirements{Text: true}}
	snap.Policies["main"] = &domain.CompiledPolicy{Name: "main", Timezone: time.UTC,
		Tiers:    []domain.TierDef{{Name: "primary", Pool: "p1"}},
		Recovery: map[domain.FailureType]domain.RecoveryAction{}, Hash: "h"}
	snap.DefaultPolicy = "main"
	f.sch.ReplaceWorld(snap, map[domain.ResourceTargetID]egress.Caller{
		"t1": stubCaller{},
	}, nil, f.srv.deps.Config.ConsumerKeys)
}

func TestPublishRollbackLifecycle(t *testing.T) {
	f := newMgmtFixture(t)

	// rollback before any publish -> conflict
	rec, _ := f.do(http.MethodPost, "/api/policy/rollback", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("early rollback status = %d, want 409", rec.Code)
	}

	// Publish a changed config at the next durable configuration revision.
	next := testConfig()
	next.Objectives = map[string]config.ObjectiveSpec{"cheap": {Rank: []string{"min_cash_cost"}}}
	next.Policies["main"].Tiers[0].Objective = "cheap"
	rec, out := f.do(http.MethodPost, "/api/policy/publish", f.versionedPolicyPayload(next))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish status = %d %s", rec.Code, rec.Body.String())
	}
	if pub, _ := out["published"].(bool); !pub {
		t.Fatalf("publish rejected: %v", out)
	}
	ver, _ := out["version"].(float64)
	if int64(ver) <= 7 {
		t.Errorf("version = %v, want bump above fixture 7", ver)
	}
	if f.publishes != 1 {
		t.Errorf("publishes = %d, want 1", f.publishes)
	}

	// failing publish leaves everything untouched and reports 422. The handler
	// structurally validates before calling Publish, so the payload must be a
	// valid config; the injected pubErr simulates the compile/publish failure.
	f.pubErr = &compileFailure{}
	rec, out = f.do(http.MethodPost, "/api/policy/publish", f.versionedPolicyPayload(testConfig()))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad publish status = %d, want 422", rec.Code)
	}
	if pub, _ := out["published"].(bool); pub {
		t.Error("published must be false on error")
	}
	if f.publishes != 2 { // attempted, then failed
		t.Errorf("publishes = %d, want 2 (attempt counted)", f.publishes)
	}
	f.pubErr = nil

	// rollback restores the previous config and swaps again
	rec, out = f.do(http.MethodPost, "/api/policy/rollback", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d %s", rec.Code, rec.Body.String())
	}
	if rb, _ := out["rolled_back"].(bool); !rb {
		t.Fatalf("rollback rejected: %v", out)
	}
}

func TestPolicyPublishRollbackPublishesExactConfigs(t *testing.T) {
	f := newMgmtFixture(t)

	next := testConfig()
	next.Objectives = map[string]config.ObjectiveSpec{"cheap": {Rank: []string{"min_cash_cost"}}}
	next.Policies["main"].Tiers[0].Objective = "cheap"
	rec, _ := f.do(http.MethodPost, "/api/policy/publish", f.versionedPolicyPayload(next))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish status = %d %s", rec.Code, rec.Body.String())
	}
	if len(f.published) != 1 {
		t.Fatalf("published candidates = %d, want 1", len(f.published))
	}
	published := f.published[0]
	if _, ok := published.Objectives["cheap"]; !ok || published.Policies["main"].Tiers[0].Objective != "cheap" {
		t.Fatalf("wrong published config: objectives=%v policy=%+v", published.Objectives, published.Policies["main"])
	}

	rec, _ = f.do(http.MethodPost, "/api/policy/rollback", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d %s", rec.Code, rec.Body.String())
	}
	if len(f.published) != 2 {
		t.Fatalf("published candidates = %d, want publish plus rollback", len(f.published))
	}
	rolledBack := f.published[1]
	if _, ok := rolledBack.Objectives["cheap"]; ok || rolledBack.Policies["main"].Tiers[0].Objective != "" {
		t.Fatalf("wrong rolled-back config: objectives=%v policy=%+v", rolledBack.Objectives, rolledBack.Policies["main"])
	}
}

type compileFailure struct{}

func (*compileFailure) Error() string { return "simulated compile failure" }

func TestSetQuotaAndClearCooldown(t *testing.T) {
	f := newMgmtFixture(t)

	// unknown target -> 404
	rec, _ := f.do(http.MethodPost, "/api/targets/nope/set-quota", map[string]any{"used": 1, "limit": 10})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target quota status = %d, want 404", rec.Code)
	}
	rec, _ = f.do(http.MethodPost, "/api/targets/nope/clear-cooldown", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target cooldown status = %d, want 404", rec.Code)
	}

	rec, _ = f.do(http.MethodPost, "/api/targets/t1/set-quota", map[string]any{"used": 80, "limit": 100})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-quota status = %d %s", rec.Code, rec.Body.String())
	}
	st := f.mgr.States(time.Now())["t1"]
	if st.QuotaUsed != 80 || st.QuotaLimit != 100 || st.QuotaSource != "manual" {
		t.Errorf("quota state = used:%v limit:%v src:%s", st.QuotaUsed, st.QuotaLimit, st.QuotaSource)
	}

	rec, _ = f.do(http.MethodPost, "/api/targets/t1/clear-cooldown", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear-cooldown status = %d", rec.Code)
	}
}

func TestConsoleServedAtRoot(t *testing.T) {
	f := newMgmtFixture(t)
	rec, _ := f.do(http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("console status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content type = %s", ct)
	}
	if !strings.Contains(rec.Body.String(), "Ferry") {
		t.Error("console shell missing Ferry branding")
	}
	for _, name := range []string{"brand.svg", "brand-reverse.svg", "favicon.svg"} {
		t.Run(name, func(t *testing.T) {
			asset, _ := f.do(http.MethodGet, "/"+name+"?brand=interlock-v1", nil)
			if asset.Code != http.StatusOK {
				t.Fatalf("brand asset status = %d", asset.Code)
			}
			if ct := asset.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
				t.Errorf("brand asset content type = %s", ct)
			}
			if !strings.Contains(asset.Body.String(), "<svg") {
				t.Error("brand asset is not SVG")
			}
		})
	}

	// unknown paths fall through to 404
	rec, _ = f.do(http.MethodGet, "/nowhere", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", rec.Code)
	}
}

func TestRequestsFromToFilter(t *testing.T) {
	f := newMgmtFixture(t)
	seed := []struct {
		id string
		ts time.Time
	}{
		{"ft-old", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)},
		{"ft-mid", time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)},
		{"ft-new", time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)},
	}
	for _, s := range seed {
		f.led.BeginRequest(domain.RequestRecord{ID: s.id, Ts: s.ts})
		f.led.FinishRequest(s.id, func(r *domain.RequestRecord) { r.Status = domain.ReqSuccess })
	}
	time.Sleep(100 * time.Millisecond) // async writer flush

	// RFC3339 window keeps only ft-mid
	from := "2026-08-10T00:00:00Z"
	to := "2026-08-18T00:00:00Z"
	rec, out := f.do(http.MethodGet,
		"/api/requests?from="+from+"&to="+to, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	total, _ := out["total"].(float64)
	if int64(total) != 1 {
		t.Fatalf("RFC3339 total = %v, want 1", out["total"])
	}
	reqs, _ := out["requests"].([]any)
	first := reqs[0].(map[string]any)
	if first["id"] != "ft-mid" {
		t.Errorf("row = %v, want ft-mid", first["id"])
	}

	// epoch-millis form selects the same row
	msFrom := fmt.Sprintf("%d", time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC).UnixMilli())
	msTo := fmt.Sprintf("%d", time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC).UnixMilli())
	rec, out = f.do(http.MethodGet, "/api/requests?from="+msFrom+"&to="+msTo, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("millis status = %d", rec.Code)
	}
	if total, _ = out["total"].(float64); int64(total) != 1 {
		t.Errorf("epoch-millis total = %v, want 1", out["total"])
	}

	// malformed timestamps must be a 400, never silently ignored
	for _, bad := range []string{"yesterday", "2026/08/10"} {
		rec, _ = f.do(http.MethodGet, "/api/requests?from="+bad, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("from=%q status = %d, want 400", bad, rec.Code)
		}
	}
}

// TestStructuredPublishLifecycle drives the visual policy editor path:
// structured JSON config in, validated world out, with rollback preserved.
func TestStructuredPublishLifecycle(t *testing.T) {
	f := newMgmtFixture(t)
	enabled := true
	f.srv.deps.Config.ConsumerKeys = []config.ConsumerKey{{Name: "personal", Key: "sk-original-secret", Enabled: &enabled}}

	rec, _ := f.do(http.MethodGet, "/api/config/raw?format=json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw status = %d", rec.Code)
	}
	var cf config.File
	if err := json.Unmarshal(rec.Body.Bytes(), &cf); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cf.Objectives == nil {
		cf.Objectives = map[string]config.ObjectiveSpec{}
	}
	cf.Objectives["cheap"] = config.ObjectiveSpec{Rank: []string{"min_cash_cost"}}
	cf.Policies["main"].Tiers[0].Objective = "cheap"

	rec, out := f.do(http.MethodPost, "/api/policy/publish", f.versionedPolicyPayload(&cf))
	if rec.Code != http.StatusOK {
		t.Fatalf("structured publish status = %d %s", rec.Code, rec.Body.String())
	}
	if pub, _ := out["published"].(bool); !pub {
		t.Fatalf("structured publish rejected: %v", out)
	}
	if ver, _ := out["version"].(float64); int64(ver) != 8 {
		t.Errorf("version = %v, want 8", out["version"])
	}
	if got := f.srv.deps.Config.ConsumerKeys[0].Key; got != "sk-original-secret" {
		t.Fatalf("structured publish erased compatibility key: %q", got)
	}

	bad := cf
	bad.Policies = map[string]config.PolicyYAML{}
	rec, out = f.do(http.MethodPost, "/api/policy/publish", f.versionedPolicyPayload(&bad))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad structured publish status = %d, want 422", rec.Code)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Error("error message missing")
	}

	// rollback restores the previous config
	rec, out = f.do(http.MethodPost, "/api/policy/rollback", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d %s", rec.Code, rec.Body.String())
	}
	if rb, _ := out["rolled_back"].(bool); !rb {
		t.Fatalf("rollback rejected: %v", out)
	}
}

func TestPublishRejectsRetiredYAMLPayload(t *testing.T) {
	f := newMgmtFixture(t)
	rec, out := f.do(http.MethodPost, "/api/policy/publish", map[string]any{"yaml": "server: {}"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%v", rec.Code, out)
	}
}

// seedWindowTraffic writes two finished requests covering every extended
// dashboard dimension: per-consumer spend, policy version traffic, compat
// mix, and typed failures.
func seedWindowTraffic(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	now := time.Now()
	l.BeginRequest(domain.RequestRecord{
		ID: "ext-ok", Ts: now, ConsumerKey: "alice", Alias: "smart",
		PolicyName: "main", PolicyVersion: 7,
		ModelRequested: "smart", ModelSelected: "m1",
	})
	l.FinishRequest("ext-ok", func(r *domain.RequestRecord) {
		r.Status = domain.ReqSuccess
		r.SelectedTarget = "t1"
		r.CompatLevel = domain.CompatExact
		r.EstCashCost = 1.25
		r.Usage = domain.Usage{InputTokens: 100, OutputTokens: 50}
	})
	l.BeginRequest(domain.RequestRecord{
		ID: "ext-fail", Ts: now, ConsumerKey: "alice", Alias: "smart",
		PolicyName: "main", PolicyVersion: 7,
		ModelRequested: "smart",
	})
	l.AddAttempt(domain.AttemptRecord{
		RequestID: "ext-fail", Seq: 1, TargetID: "t1",
		Status:  domain.ReqFailed,
		Failure: &domain.Failure{Type: domain.FailRateLimit, Message: "429"},
	})
	l.FinishRequest("ext-fail", func(r *domain.RequestRecord) {
		r.Status = domain.ReqFailed
		r.FailureType = domain.FailRateLimit
		r.Attempts = 1
		r.CompatLevel = domain.CompatDegraded
	})
	time.Sleep(100 * time.Millisecond) // async writer flush
}

func TestDashboardExtensionBlocks(t *testing.T) {
	f := newMgmtFixture(t)
	seedWindowTraffic(t, f.led)

	rec, out := f.do(http.MethodGet, "/api/dashboard?hours=24", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d %s", rec.Code, rec.Body.String())
	}

	// per-consumer block carries alice's spend
	bc, _ := out["by_consumer"].(map[string]any)
	if bc == nil || bc["alice"] == nil {
		t.Fatalf("by_consumer missing alice: %v", out)
	}
	alice := bc["alice"].(map[string]any)
	if cost, _ := alice["est_cash_cost"].(float64); cost != 1.25 {
		t.Errorf("alice est_cash_cost = %v, want 1.25", alice["est_cash_cost"])
	}

	// per policy+version traffic
	pv, _ := out["policy_versions"].([]any)
	if len(pv) != 1 {
		t.Fatalf("policy_versions = %v, want 1 row", out["policy_versions"])
	}
	row := pv[0].(map[string]any)
	if row["policy_name"] != "main" {
		t.Errorf("policy_name = %v, want main", row["policy_name"])
	}
	if v, _ := row["policy_version"].(float64); int64(v) != 7 {
		t.Errorf("policy_version = %v, want 7", row["policy_version"])
	}
	sum := row["summary"].(map[string]any)
	if reqs, _ := sum["requests"].(float64); int64(reqs) != 2 {
		t.Errorf("summary.requests = %v, want 2", sum["requests"])
	}

	// compat mix counts both levels
	cb, _ := out["compat"].(map[string]any)
	if cb == nil || cb["exact"] == nil || cb["degraded"] == nil {
		t.Fatalf("compat breakdown incomplete: %v", out["compat"])
	}
	if v, _ := cb["exact"].(float64); int64(v) != 1 {
		t.Errorf("compat[exact] = %v, want 1", cb["exact"])
	}

	// typed failures bucket the rate limit
	fb, _ := out["failures"].(map[string]any)
	if fb == nil || fb["rate_limit"] == nil {
		t.Fatalf("failures breakdown incomplete: %v", out["failures"])
	}
	if v, _ := fb["rate_limit"].(float64); int64(v) != 1 {
		t.Errorf("failures[rate_limit] = %v, want 1", fb["rate_limit"])
	}
}

// TestConsumersEndpointNeverLeaksKeys pins the privacy invariant: the
// consumers view exposes identity, defaults, budgets, and window usage —
// never the bearer key value itself.
func TestConsumersEndpointNeverLeaksKeys(t *testing.T) {
	f := newMgmtFixture(t)
	const aliceKey = "sk-local-alice-DO-NOT-SERVE"
	on := true
	f.cfg.ConsumerKeys = []config.ConsumerKey{{
		Name:             "alice",
		Key:              aliceKey,
		Enabled:          &on,
		MonthlyBudgetUSD: 50,
	}}

	seedWindowTraffic(t, f.led)

	rec, out := f.do(http.MethodGet, "/api/consumers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, aliceKey) {
		t.Fatal("consumers endpoint leaked the consumer key value")
	}
	consumers, _ := out["consumers"].([]any)
	if len(consumers) != 1 {
		t.Fatalf("consumers = %d, want 1", len(consumers))
	}
	c := consumers[0].(map[string]any)
	if c["name"] != "alice" {
		t.Errorf("name = %v, want alice", c["name"])
	}
	if enabled, _ := c["enabled"].(bool); !enabled {
		t.Errorf("enabled = %v, want true", c["enabled"])
	}
	if b, _ := c["monthly_budget_usd"].(float64); b != 50 {
		t.Errorf("monthly_budget_usd = %v, want 50", c["monthly_budget_usd"])
	}
	wu, _ := c["window_usage"].(map[string]any)
	if wu == nil {
		t.Fatal("window_usage missing")
	}
	if reqs, _ := wu["requests"].(float64); int64(reqs) != 2 {
		t.Errorf("window_usage.requests = %v, want 2", wu["requests"])
	}
	if cost, _ := wu["est_cash_cost"].(float64); cost != 1.25 {
		t.Errorf("window_usage.est_cash_cost = %v, want 1.25", wu["est_cash_cost"])
	}
	if preview, _ := c["key_preview"].(string); !strings.HasPrefix(preview, "sk-") || !strings.Contains(preview, "••••") {
		t.Errorf("key_preview = %q, want masked preview", preview)
	}
}

func TestConsumerKeyCannotBeRevealedAfterCreation(t *testing.T) {
	f := newMgmtFixture(t)
	on := true
	f.cfg.ConsumerKeys = []config.ConsumerKey{{Name: "alice", Key: "sk-reveal-only", Enabled: &on}}

	rec, out := f.do(http.MethodGet, "/api/consumers/alice/key", nil)
	if rec.Code != http.StatusGone || out["key"] != nil {
		t.Fatalf("secret reveal = %d %v, want 410 without key", rec.Code, out)
	}
	rec, _ = f.do(http.MethodGet, "/api/consumers/missing/key", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing reveal status = %d, want 404", rec.Code)
	}
}

func TestConsumerLifecycle(t *testing.T) {
	f := newMgmtFixture(t)
	rec, out := f.do(http.MethodPost, "/api/consumers", map[string]any{
		"name": "personal", "monthly_budget_usd": 25,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	createdKey, _ := out["key"].(string)
	if !strings.HasPrefix(createdKey, "sk-ferry-") {
		t.Fatalf("generated key = %q", createdKey)
	}

	rec, out = f.do(http.MethodPost, "/api/consumers/personal/rotate", nil)
	rotated, _ := out["key"].(string)
	if rec.Code != http.StatusOK || rotated == "" || rotated == createdKey {
		t.Fatalf("rotate = %d %v", rec.Code, out)
	}

	rec, _ = f.do(http.MethodPatch, "/api/consumers/personal", map[string]any{"enabled": false})
	if rec.Code != http.StatusOK || f.srv.deps.Config.ConsumerKeys[0].Enabled == nil || *f.srv.deps.Config.ConsumerKeys[0].Enabled {
		t.Fatalf("disable = %d %+v", rec.Code, f.srv.deps.Config.ConsumerKeys)
	}

	rec, _ = f.do(http.MethodDelete, "/api/consumers/personal", nil)
	if rec.Code != http.StatusConflict || len(f.srv.deps.Config.ConsumerKeys) != 1 {
		t.Fatalf("delete last consumer = %d %+v, want fail-closed conflict", rec.Code, f.srv.deps.Config.ConsumerKeys)
	}
	rec, _ = f.do(http.MethodPost, "/api/consumers", map[string]any{"name": "replacement"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create replacement = %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = f.do(http.MethodDelete, "/api/consumers/personal", nil)
	if rec.Code != http.StatusOK || len(f.srv.deps.Config.ConsumerKeys) != 1 || f.srv.deps.Config.ConsumerKeys[0].Name != "replacement" {
		t.Fatalf("delete with replacement = %d %+v", rec.Code, f.srv.deps.Config.ConsumerKeys)
	}
}

func TestConsumerAnalyticsMatchesGlobalDimensions(t *testing.T) {
	f := newMgmtFixture(t)
	seedWindowTraffic(t, f.led)
	rec, out := f.do(http.MethodGet, "/api/consumers/alice/analytics?hours=24", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("analytics = %d %s", rec.Code, rec.Body.String())
	}
	summary, _ := out["summary"].(map[string]any)
	if reqs, _ := summary["requests"].(float64); int64(reqs) != 2 {
		t.Fatalf("summary.requests = %v", summary["requests"])
	}
	for _, key := range []string{"by_target", "by_model", "failures", "recent_requests"} {
		if out[key] == nil {
			t.Errorf("analytics missing %s: %v", key, out)
		}
	}
}

func TestPolicyVersionsEndpoint(t *testing.T) {
	f := newMgmtFixture(t)
	seedWindowTraffic(t, f.led)

	rec, out := f.do(http.MethodGet, "/api/policy/versions?hours=24", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	live, _ := out["live_policies"].([]any)
	if len(live) != 1 || live[0].(map[string]any)["id"] != "main" {
		t.Errorf("live_policies = %v, want one main rule", out["live_policies"])
	}
	versions, _ := out["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	v := versions[0].(map[string]any)
	if v["policy_name"] != "main" {
		t.Errorf("policy_name = %v, want main", v["policy_name"])
	}
	if ver, _ := v["policy_version"].(float64); int64(ver) != 7 {
		t.Errorf("policy_version = %v, want 7", v["policy_version"])
	}
	sum, _ := v["summary"].(map[string]any)
	if succ, _ := sum["succeeded"].(float64); int64(succ) != 1 {
		t.Errorf("summary.succeeded = %v, want 1", sum["succeeded"])
	}
	if failed, _ := sum["failed"].(float64); int64(failed) != 1 {
		t.Errorf("summary.failed = %v, want 1", sum["failed"])
	}
}

func TestRequestDetailNotFound(t *testing.T) {
	f := newMgmtFixture(t)
	rec, out := f.do(http.MethodGet, "/api/requests/no-such-request", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Error("404 body missing error message")
	}
}
