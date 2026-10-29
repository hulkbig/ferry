package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/ledger"
	"ferry/internal/policy"
	"ferry/internal/runtime"
	sched "ferry/internal/scheduler"
)

type v2APIFixture struct {
	t           *testing.T
	server      *Server
	control     *controlstore.Store
	controlPath string
	ledger      *ledger.Ledger
}

func nativeV2Config() *config.File {
	enabled := true
	return &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "provider-a", Name: "Provider A", Type: domain.ProviderCustom,
			BaseURL: "https://example.test/v1", Enabled: true,
			Connections: []domain.ConnectionRef{{ID: "connection-a"}},
		}},
		Connections: []domain.Connection{{
			ID: "connection-a", ProviderID: "provider-a", Identity: "primary",
			Credential: domain.CredentialRef{SecretRef: "env:FERRY_V2_TEST_KEY"}, Enabled: true,
			Models: []domain.ConnectionModel{{
				UpstreamModel: "upstream-model",
				Protocols:     []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses},
				Capabilities:  domain.Capabilities{Text: true, Stream: true, Tools: true},
			}, {
				UpstreamModel: "upstream-model-2",
				Protocols:     []domain.Protocol{domain.ProtocolOpenAIChat},
				Capabilities:  domain.Capabilities{Text: true, Stream: true, Tools: true},
			}},
		}},
		LogicalModels: []domain.LogicalModel{{
			ID: "smart", DisplayName: "Smart", Enabled: true,
			DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{
				ProviderID: "provider-a", UpstreamModel: "upstream-model",
				RecoverOn: []domain.FailureType{domain.FailConnect, domain.FailRateLimit, domain.FailProvider5xx},
			}}},
		}},
		ConsumerKeys: []config.ConsumerKey{{Name: "initial", Key: "sk-ferry-initial", Enabled: &enabled}},
	}
}

func newV2APIFixture(t *testing.T) *v2APIFixture {
	t.Helper()
	dir := t.TempDir()
	controlPath := filepath.Join(dir, "control.db")
	control, err := controlstore.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	led, err := ledger.Open(dir, 256, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	manager := runtime.NewManager(runtime.DefaultHealthConfig())
	scheduler := sched.New(manager, led)
	scheduler.SetAPIKeyUsageRecorder(control.RecordAPIKeySecretUse)
	compiler := policy.NewCompiler()

	initial := nativeV2Config()
	if err := initial.Validate(); err != nil {
		t.Fatalf("validate initial config: %v", err)
	}
	version, err := control.SaveConfig(initial)
	if err != nil {
		t.Fatal(err)
	}
	active, _, err := control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	publishWorld := func(candidate *config.File, persist bool) error {
		if err := candidate.Validate(); err != nil {
			return err
		}
		snapshot, err := compiler.Compile(candidate)
		if err != nil {
			return err
		}
		if persist {
			version, err = control.SaveConfig(candidate)
			if err != nil {
				return err
			}
		}
		authenticators, err := control.APIKeyAuthenticators()
		if err != nil {
			return err
		}
		callers := make(map[domain.ResourceTargetID]egress.Caller, len(snapshot.Targets))
		for id := range snapshot.Targets {
			callers[id] = stubCaller{}
		}
		snapshot.Version = version
		scheduler.ReplaceWorldWithAPIKeys(snapshot, callers, nil, authenticators)
		return nil
	}
	if err := publishWorld(active, false); err != nil {
		t.Fatal(err)
	}
	server := New(Deps{
		Scheduler: scheduler, Manager: manager, Ledger: led, Config: active,
		Compiler: compiler, APIKeys: control, Control: control,
		Publish: func(candidate *config.File) error { return publishWorld(candidate, true) },
		ReloadAPIKeys: func() error {
			authenticators, err := control.APIKeyAuthenticators()
			if err != nil {
				return err
			}
			return scheduler.ReplaceAPIKeys(authenticators)
		},
		ControlVersion: func() (int64, error) {
			_, activeVersion, err := control.ActiveConfig()
			return activeVersion, err
		},
		Timezone: time.UTC,
	})
	return &v2APIFixture{t: t, server: server, control: control, controlPath: controlPath, ledger: led}
}

func (f *v2APIFixture) do(method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	var input []byte
	if body != nil {
		input, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(input))
	rec := httptest.NewRecorder()
	f.server.Routes().ServeHTTP(rec, req)
	var response map[string]any
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &response)
	}
	return rec, response
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
	}
}

func requireNoLegacyRuntimeVocabulary(t *testing.T, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, forbidden := range []string{"policy", "channel", "pool", "target"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("V2 response leaked legacy runtime vocabulary %q: %s", forbidden, body)
		}
	}
}

func TestV2ConnectionRuntimeOverrides(t *testing.T) {
	f := newV2APIFixture(t)

	clearRec, cleared := f.do(http.MethodPost, "/api/providers/provider-a/connections/connection-a/clear-cooldown", map[string]any{})
	requireStatus(t, clearRec, http.StatusOK)
	if cleared["cleared_models"] != float64(2) {
		t.Fatalf("clear cooldown response=%v", cleared)
	}

	missingModelRec, _ := f.do(http.MethodPost, "/api/providers/provider-a/connections/connection-a/set-quota", map[string]any{
		"used": 25, "limit": 100,
	})
	requireStatus(t, missingModelRec, http.StatusBadRequest)

	quotaRec, quota := f.do(http.MethodPost, "/api/providers/provider-a/connections/connection-a/set-quota", map[string]any{
		"upstream_model": "upstream-model", "used": 25, "limit": 100,
	})
	requireStatus(t, quotaRec, http.StatusOK)
	if quota["upstream_model"] != "upstream-model" || quota["used"] != float64(25) || quota["limit"] != float64(100) {
		t.Fatalf("quota response=%v", quota)
	}
	connection := f.server.deps.Manager.ConnectionStates(time.Now())["connection-a"]
	if len(connection.QuotaWindows) != 1 || connection.QuotaWindows[0].RemainingFraction == nil ||
		*connection.QuotaWindows[0].RemainingFraction != 0.75 {
		t.Fatalf("connection quota state=%+v", connection.QuotaWindows)
	}

	unknownRec, _ := f.do(http.MethodPost, "/api/providers/provider-a/connections/connection-a/set-quota", map[string]any{
		"upstream_model": "unknown", "used": 1, "limit": 2,
	})
	requireStatus(t, unknownRec, http.StatusNotFound)
}

func TestV2ProductStateClassification(t *testing.T) {
	modelCases := []struct {
		name    string
		enabled bool
		steps   []v2RouteStepView
		want    string
	}{
		{name: "healthy", enabled: true, steps: []v2RouteStepView{{EligibleConnections: 1}, {EligibleConnections: 1}}, want: "healthy"},
		{name: "protected", enabled: true, steps: []v2RouteStepView{{EligibleConnections: 0}, {EligibleConnections: 1}}, want: "protected"},
		{name: "degraded", enabled: true, steps: []v2RouteStepView{{EligibleConnections: 1}, {EligibleConnections: 0}}, want: "degraded"},
		{name: "unavailable", enabled: true, steps: []v2RouteStepView{{EligibleConnections: 0}}, want: "unavailable"},
	}
	for _, test := range modelCases {
		t.Run("model "+test.name, func(t *testing.T) {
			got, sentence := v2ModelState(test.enabled, test.steps)
			if got != test.want || sentence == "" {
				t.Fatalf("state=%q sentence=%q want=%q", got, sentence, test.want)
			}
		})
	}

	now := time.Now()
	standby := v2ConnectionView{Enabled: true}
	cooling := v2ConnectionView{Enabled: true, Runtime: domain.ConnectionState{CoolingUntil: now.Add(time.Minute)}}
	disabled := v2ConnectionView{Enabled: false}
	offline := v2ConnectionView{Enabled: true, Runtime: domain.ConnectionState{Reachability: domain.ConnectionOffline}}
	providerCases := []struct {
		name        string
		enabled     bool
		connections []v2ConnectionView
		want        string
		available   int
	}{
		{name: "healthy", enabled: true, connections: []v2ConnectionView{standby}, want: "healthy", available: 1},
		{name: "recovering", enabled: true, connections: []v2ConnectionView{standby, cooling}, want: "recovering", available: 1},
		{name: "exhausted", enabled: true, connections: []v2ConnectionView{cooling}, want: "exhausted"},
		{name: "disabled provider", enabled: false, connections: []v2ConnectionView{standby}, want: "unavailable"},
		{name: "disabled connections", enabled: true, connections: []v2ConnectionView{disabled}, want: "unavailable"},
		{name: "offline connections", enabled: true, connections: []v2ConnectionView{offline}, want: "unavailable"},
		{name: "disabled connection ignored", enabled: true, connections: []v2ConnectionView{standby, disabled}, want: "healthy", available: 1},
	}
	for _, test := range providerCases {
		t.Run("provider "+test.name, func(t *testing.T) {
			got, available := v2ProviderState(test.enabled, test.connections, now)
			if got != test.want || available != test.available {
				t.Fatalf("state=%q available=%d want=%q/%d", got, available, test.want, test.available)
			}
		})
	}
}

func TestV2WindowRejectsSubMillisecondRanges(t *testing.T) {
	for _, test := range []struct {
		window string
		valid  bool
	}{
		{window: "999us", valid: false},
		{window: "1ms", valid: true},
		{window: "2160h", valid: true},
		{window: "2160h1ms", valid: false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/overview?window="+test.window, nil)
		from, to, err := v2Window(req)
		if test.valid && (err != nil || !to.After(from)) {
			t.Fatalf("window %s rejected: from=%v to=%v err=%v", test.window, from, to, err)
		}
		if !test.valid && (err == nil || !strings.Contains(err.Error(), "1ms")) {
			t.Fatalf("window %s accepted or returned the wrong boundary error: %v", test.window, err)
		}
	}
}

func TestV2AggregateWindowSupportsFrozenExplicitBounds(t *testing.T) {
	to := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	from := to.Add(-37 * time.Minute)
	pair := httptest.NewRequest(http.MethodGet,
		"/api/usage-billing?from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), nil)
	gotFrom, gotTo, err := v2Window(pair)
	if err != nil || !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Fatalf("explicit aggregate window=%v..%v err=%v, want %v..%v", gotFrom, gotTo, err, from, to)
	}

	toOnly := httptest.NewRequest(http.MethodGet,
		"/api/usage-billing?window=15m&to="+to.Format(time.RFC3339), nil)
	gotFrom, gotTo, err = v2Window(toOnly)
	if err != nil || !gotFrom.Equal(to.Add(-15*time.Minute)) || !gotTo.Equal(to) {
		t.Fatalf("frozen relative window=%v..%v err=%v", gotFrom, gotTo, err)
	}

	defaultWindow := httptest.NewRequest(http.MethodGet,
		"/api/usage-billing?to="+to.Format(time.RFC3339), nil)
	gotFrom, gotTo, err = v2Window(defaultWindow)
	if err != nil || !gotFrom.Equal(to.Add(-24*time.Hour)) || !gotTo.Equal(to) {
		t.Fatalf("frozen default window=%v..%v err=%v", gotFrom, gotTo, err)
	}
}

func TestV2AggregateWindowRejectsInvalidAndAmbiguousBounds(t *testing.T) {
	past := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "from only", path: "/api/overview?from=" + past.Format(time.RFC3339), want: "from requires to"},
		{name: "bad from", path: "/api/overview?from=2026-01-02&to=" + past.Format(time.RFC3339), want: "from must be RFC3339"},
		{name: "bad to", path: "/api/overview?from=" + past.Add(-time.Hour).Format(time.RFC3339) + "&to=2026-01-02", want: "to must be RFC3339"},
		{name: "triple parameters", path: "/api/overview?window=1h&from=" + past.Add(-time.Hour).Format(time.RFC3339) + "&to=" + past.Format(time.RFC3339), want: "window cannot be combined"},
		{name: "future to", path: "/api/overview?window=1h&to=" + future, want: "future"},
		{name: "equal", path: "/api/overview?from=" + past.Format(time.RFC3339) + "&to=" + past.Format(time.RFC3339), want: "span between 1ms"},
		{name: "reversed", path: "/api/overview?from=" + past.Format(time.RFC3339) + "&to=" + past.Add(-time.Second).Format(time.RFC3339), want: "span between 1ms"},
		{name: "sub millisecond", path: "/api/overview?from=" + past.Format(time.RFC3339Nano) + "&to=" + past.Add(time.Millisecond-time.Nanosecond).Format(time.RFC3339Nano), want: "span between 1ms"},
		{name: "over 90 days", path: "/api/overview?from=" + past.Add(-90*24*time.Hour-time.Millisecond).Format(time.RFC3339Nano) + "&to=" + past.Format(time.RFC3339Nano), want: "2160h"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			_, _, err := v2Window(req)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestV2RequestFiltersKeepIndependentBoundaryCompatibility(t *testing.T) {
	f := newV2APIFixture(t)
	from := "2026-01-02T00:00:00Z"
	to := "2026-01-03T00:00:00Z"
	for _, path := range []string{
		"/api/requests?from=" + from,
		"/api/requests?to=" + to,
		"/api/requests?window=15m&from=" + from + "&to=" + to,
	} {
		rec, _ := f.do(http.MethodGet, path, nil)
		requireStatus(t, rec, http.StatusOK)
	}
}

func TestV2UsageBillingUsesAndReturnsExplicitWindow(t *testing.T) {
	f := newV2APIFixture(t)
	from := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for _, record := range []domain.RequestRecord{
		{ID: "explicit-before", Ts: from.Add(-time.Millisecond), LogicalModelID: "smart", ProviderID: "provider-a", Status: domain.ReqSuccess, Outcome: domain.OutcomeDirectSuccess},
		{ID: "explicit-from", Ts: from, LogicalModelID: "smart", ProviderID: "provider-a", Status: domain.ReqSuccess, Outcome: domain.OutcomeDirectSuccess},
		{ID: "explicit-before-to", Ts: to.Add(-time.Millisecond), LogicalModelID: "smart", ProviderID: "provider-a", Status: domain.ReqSuccess, Outcome: domain.OutcomeRecoveredSuccess},
		{ID: "explicit-to", Ts: to, LogicalModelID: "smart", ProviderID: "provider-a", Status: domain.ReqFailed, Outcome: domain.OutcomeFailed},
	} {
		f.ledger.BeginRequest(record)
		f.ledger.FinishRequest(record.ID, nil)
	}

	path := "/api/usage-billing?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	var response map[string]any
	for attempt := 0; attempt < 50; attempt++ {
		rec, candidate := f.do(http.MethodGet, path, nil)
		requireStatus(t, rec, http.StatusOK)
		summary, _ := candidate["summary"].(map[string]any)
		if summary["requests"] == float64(2) {
			response = candidate
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if response == nil {
		t.Fatal("explicit aggregate window did not converge on the two in-range requests")
	}
	if response["from"] != from.Format(time.RFC3339) || response["to"] != to.Format(time.RFC3339) {
		t.Fatalf("returned window=%v..%v", response["from"], response["to"])
	}
	summary := response["summary"].(map[string]any)
	outcomes := response["outcomes"].(map[string]any)
	coverage, ok := response["coverage"].(map[string]any)
	if !ok || response["basis"] != "retained_request_details" || coverage["scope"] != "global" || coverage["detail_requests"] != float64(2) || coverage["rollup_requests"] != float64(2) {
		t.Fatalf("billing coverage=%v basis=%v", coverage, response["basis"])
	}
	if historical, ok := response["historical_summary"].(map[string]any); !ok || historical["requests"] != float64(2) {
		t.Fatalf("historical summary=%v", response["historical_summary"])
	}
	if summary["succeeded"] != float64(2) || outcomes["direct_success"] != float64(1) ||
		outcomes["recovered_success"] != float64(1) || outcomes["failed"] != float64(0) {
		t.Fatalf("filtered aggregates summary=%v outcomes=%v", summary, outcomes)
	}
}

func modelCount(t *testing.T, response map[string]any) int {
	t.Helper()
	models, ok := response["models"].([]any)
	if !ok {
		t.Fatalf("models response=%v", response)
	}
	return len(models)
}

func TestV2ViewsAndDraftRevisionLifecycle(t *testing.T) {
	f := newV2APIFixture(t)
	for _, path := range []string{"/api/overview", "/api/models", "/api/providers"} {
		rec, _ := f.do(http.MethodGet, path, nil)
		requireStatus(t, rec, http.StatusOK)
		requireNoLegacyRuntimeVocabulary(t, rec.Body.String())
	}
	overviewRec, overview := f.do(http.MethodGet, "/api/overview", nil)
	requireStatus(t, overviewRec, http.StatusOK)
	initialModelsRec, initialModels := f.do(http.MethodGet, "/api/models", nil)
	requireStatus(t, initialModelsRec, http.StatusOK)
	baselineModelCount := modelCount(t, initialModels)
	providerRec, provider := f.do(http.MethodGet, "/api/providers/provider-a", nil)
	requireStatus(t, providerRec, http.StatusOK)
	connections := provider["connections"].([]any)
	providerModels := connections[0].(map[string]any)["models"].([]any)
	providerModel := providerModels[0].(map[string]any)
	providerUpstream := providerModel["upstream_capabilities"].(map[string]any)
	providerEffective := providerModel["capabilities"].(map[string]any)
	if providerUpstream["parallel_tools"] != false || providerEffective["parallel_tools"] != true {
		t.Fatalf("provider view did not expose effective parallel tools: %s", providerRec.Body.String())
	}
	status, ok := overview["status"].(map[string]any)
	if !ok || status["runtime_api"] != "available" || status["client_traffic"] != "no_traffic" || status["configuration"] != "healthy" {
		t.Fatalf("overview status boundaries=%v", overview["status"])
	}
	if trafficTrend, ok := overview["traffic_trend"].([]any); !ok || len(trafficTrend) != 0 {
		t.Fatalf("overview traffic trend=%v", overview["traffic_trend"])
	}
	cacheHit, ok := overview["cache_hit_summary"].(map[string]any)
	if !ok {
		t.Fatalf("overview cache hit summary=%v", overview["cache_hit_summary"])
	}
	overallCache, _ := cacheHit["overall"].(map[string]any)
	apiKeyCache, apiKeysOK := cacheHit["api_keys"].([]any)
	modelCache, modelsOK := cacheHit["logical_models"].([]any)
	if overallCache["requests"] != float64(0) || overallCache["hit_rate_pct"] != nil ||
		!apiKeysOK || len(apiKeyCache) != 0 || !modelsOK || len(modelCache) != 0 {
		t.Fatalf("empty cache hit summary=%v", cacheHit)
	}
	simulateRec, simulated := f.do(http.MethodPost, "/api/models/smart/simulate", map[string]any{
		"required_capabilities": map[string]any{"parallel_tools": true},
	})
	requireStatus(t, simulateRec, http.StatusOK)
	if simulated["planning_failure"] != nil {
		t.Fatalf("simulation rejected gateway-mapped parallel tools: %s", simulateRec.Body.String())
	}
	candidates, _ := simulated["connection_candidates"].([]any)
	if len(candidates) == 0 || candidates[0].(map[string]any)["eligible"] != true {
		t.Fatalf("simulation did not expose mapped parallel tools: %s", simulateRec.Body.String())
	}
	upstream := candidates[0].(map[string]any)["upstream_capabilities"].(map[string]any)
	effective := candidates[0].(map[string]any)["capabilities"].(map[string]any)
	if upstream["parallel_tools"] != false || effective["parallel_tools"] != true {
		t.Fatalf("simulation did not distinguish upstream and effective capabilities: %s", simulateRec.Body.String())
	}

	staleRec, stale := f.do(http.MethodPost, "/api/drafts", map[string]any{})
	requireStatus(t, staleRec, http.StatusCreated)
	staleID, _ := stale["id"].(string)

	newModel := domain.LogicalModel{
		ID: "fast", DisplayName: "Fast", Enabled: true,
		DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-model"}}},
	}
	createRec, created := f.do(http.MethodPost, "/api/models", newModel)
	requireStatus(t, createRec, http.StatusAccepted)
	draftID, _ := created["draft_id"].(string)
	if draftID == "" {
		t.Fatalf("missing draft id: %v", created)
	}
	beforeRec, before := f.do(http.MethodGet, "/api/models", nil)
	requireStatus(t, beforeRec, http.StatusOK)
	if got := modelCount(t, before); got != baselineModelCount {
		t.Fatalf("draft changed active runtime before apply: models=%d", got)
	}
	validateRec, validation := f.do(http.MethodPost, "/api/drafts/"+draftID+"/validate", map[string]any{})
	requireStatus(t, validateRec, http.StatusOK)
	if valid, _ := validation["valid"].(bool); !valid {
		t.Fatalf("draft validation=%v", validation)
	}
	missingRec, missing := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{})
	requireStatus(t, missingRec, http.StatusPreconditionRequired)
	missingError, _ := missing["error"].(map[string]any)
	if missingError["code"] != "MISSING_PRECONDITION" {
		t.Fatalf("missing CAS precondition=%v", missing)
	}
	applyRec, applied := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, applyRec, http.StatusOK)
	if revision, _ := applied["revision"].(float64); revision != 2 {
		t.Fatalf("apply response=%v", applied)
	}
	idempotentRec, idempotent := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, idempotentRec, http.StatusOK)
	if revision, _ := idempotent["revision"].(float64); revision != 2 || idempotent["idempotent"] != true {
		t.Fatalf("idempotent apply response=%v", idempotent)
	}
	afterRec, after := f.do(http.MethodGet, "/api/models", nil)
	requireStatus(t, afterRec, http.StatusOK)
	if got := modelCount(t, after); got != baselineModelCount+1 {
		t.Fatalf("applied model count=%d", got)
	}

	conflictRec, _ := f.do(http.MethodPost, "/api/drafts/"+staleID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, conflictRec, http.StatusConflict)

	revisionsRec, revisions := f.do(http.MethodGet, "/api/revisions", nil)
	requireStatus(t, revisionsRec, http.StatusOK)
	if list, _ := revisions["revisions"].([]any); len(list) != 2 {
		t.Fatalf("revisions=%v", revisions)
	}
	restoreRec, restore := f.do(http.MethodPost, "/api/revisions/1/restore-draft", map[string]any{})
	requireStatus(t, restoreRec, http.StatusCreated)
	restoreID, _ := restore["id"].(string)
	stillActiveRec, stillActive := f.do(http.MethodGet, "/api/models", nil)
	requireStatus(t, stillActiveRec, http.StatusOK)
	if got := modelCount(t, stillActive); got != baselineModelCount+1 {
		t.Fatalf("restore draft directly changed active runtime: models=%d", got)
	}
	restoreApplyRec, restoreApplied := f.do(http.MethodPost, "/api/drafts/"+restoreID+"/apply", map[string]any{"expected_revision": 2})
	requireStatus(t, restoreApplyRec, http.StatusOK)
	if revision, _ := restoreApplied["revision"].(float64); revision != 3 {
		t.Fatalf("restore apply=%v", restoreApplied)
	}
	restoredRec, restored := f.do(http.MethodGet, "/api/models", nil)
	requireStatus(t, restoredRec, http.StatusOK)
	if got := modelCount(t, restored); got != baselineModelCount {
		t.Fatalf("restored model count=%d", got)
	}
}

func TestDisabledLogicalModelCanBeSimulatedWithoutEnablingLiveTraffic(t *testing.T) {
	f := newV2APIFixture(t)
	patchRec, mutation := f.do(http.MethodPatch, "/api/models/smart", map[string]any{"enabled": false})
	requireStatus(t, patchRec, http.StatusAccepted)
	draftID, _ := mutation["draft_id"].(string)
	if draftID == "" {
		t.Fatalf("disable mutation=%v", mutation)
	}
	applyRec, _ := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, applyRec, http.StatusOK)

	simulateRec, simulation := f.do(http.MethodPost, "/api/models/smart/simulate", map[string]any{
		"protocol": domain.ProtocolOpenAIChat,
	})
	requireStatus(t, simulateRec, http.StatusOK)
	if simulation["previewing_disabled_model"] != true {
		t.Fatalf("disabled preview flag missing: %v", simulation)
	}
	selection, _ := simulation["expected_selection"].(map[string]any)
	if selection["provider_id"] != "provider-a" || selection["upstream_model"] != "upstream-model" {
		t.Fatalf("disabled model simulation selection=%v", simulation)
	}
	detailRec, _ := f.do(http.MethodGet, "/api/models/smart", nil)
	requireStatus(t, detailRec, http.StatusOK)
	if !strings.Contains(detailRec.Body.String(), `"enabled":false`) {
		t.Fatalf("simulation changed live model state: %s", detailRec.Body.String())
	}

	overviewRec, overview := f.do(http.MethodGet, "/api/overview", nil)
	requireStatus(t, overviewRec, http.StatusOK)
	protection, _ := overview["protection"].(map[string]any)
	needsAttention, _ := overview["needs_attention"].([]any)
	status, _ := overview["status"].(map[string]any)
	if protection["unavailable"] != float64(0) || len(needsAttention) != 0 || status["configuration"] != "healthy" {
		t.Fatalf("disabled model was reported as an active configuration fault: %v", overview)
	}
	models, _ := overview["model_protection"].([]any)
	var disabledModel map[string]any
	for _, raw := range models {
		model, _ := raw.(map[string]any)
		if model["id"] == "smart" {
			disabledModel = model
			break
		}
	}
	if disabledModel == nil || disabledModel["enabled"] != false || disabledModel["state"] != "unavailable" {
		t.Fatalf("disabled model must remain visible in the inventory: %v", models)
	}
}

func TestV2StatisticalInventoryViewsFailClosedOnLedgerError(t *testing.T) {
	f := newV2APIFixture(t)
	if err := f.ledger.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/models", "/api/models/smart", "/api/providers", "/api/providers/provider-a", "/api/api-keys"} {
		rec, _ := f.do(http.MethodGet, path, nil)
		requireStatus(t, rec, http.StatusInternalServerError)
	}
}

func TestV2LogicalModelExactOnlyPatchIsDraftedAndApplied(t *testing.T) {
	f := newV2APIFixture(t)
	patchRec, mutation := f.do(http.MethodPatch, "/api/models/smart", map[string]any{"exact_only": true})
	requireStatus(t, patchRec, http.StatusAccepted)
	draftID, _ := mutation["draft_id"].(string)
	if draftID == "" {
		t.Fatalf("exact-only mutation=%v", mutation)
	}
	draft, err := f.control.Draft(draftID)
	if err != nil {
		t.Fatal(err)
	}
	modelAt := logicalModelIndex(draft.Intent, "smart")
	if modelAt < 0 || !draft.Intent.LogicalModels[modelAt].ExactOnly {
		t.Fatalf("draft omitted exact-only policy: %+v", draft.Intent.LogicalModels)
	}
	applyRec, _ := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, applyRec, http.StatusOK)
	detailRec, detail := f.do(http.MethodGet, "/api/models/smart", nil)
	requireStatus(t, detailRec, http.StatusOK)
	if exactOnly, _ := detail["exact_only"].(bool); !exactOnly {
		t.Fatalf("active model omitted exact-only policy: %s", detailRec.Body.String())
	}
}

func TestV2MutationRefusesSilentNativeTakeoverWhenLegacyBehaviorIsUnrepresentable(t *testing.T) {
	f := newV2APIFixture(t)
	draft, err := f.control.CreateDraft(1, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	intent := draft.Intent
	intent.V2DomainSource = config.V2DomainSourceLegacy
	intent.V2MigrationIssues = []domain.LegacyMappingIssue{{
		Kind: "legacy_rule", ObjectID: "dynamic-preference",
		Message: "cannot be transferred without changing behavior", Blocking: true,
	}}
	draft, err = f.control.UpdateDraft(draft.ID, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.updateV2Draft(draft, func(candidate *config.File) error {
		candidate.LogicalModels[0].Description = "must not apply"
		return nil
	}); err == nil || !strings.Contains(err.Error(), "legacy-to-V2 migration is blocked") {
		t.Fatalf("silent native takeover was not blocked: %v", err)
	}
	stored, err := f.control.Draft(draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Intent.V2DomainSource != config.V2DomainSourceLegacy || stored.Intent.LogicalModels[0].Description == "must not apply" {
		t.Fatalf("blocked draft was mutated: %+v", stored.Intent.LogicalModels[0])
	}
	candidate, problems := f.server.validateV2Draft(draft)
	if len(problems) != 0 {
		t.Fatalf("unchanged legacy draft did not validate: %v", problems)
	}
	if candidate.V2DomainSource != config.V2DomainSourceLegacy {
		t.Fatalf("draft validation silently changed the migration authority: source=%q issues=%+v",
			candidate.V2DomainSource, candidate.V2MigrationIssues)
	}
}

func TestLogicalModelSimulationHonorsExplicitTimeAndReportsMatchedOverride(t *testing.T) {
	f := newV2APIFixture(t)
	createRec, mutation := f.do(http.MethodPost, "/api/models/smart/overrides", map[string]any{
		"id": "afternoon", "name": "Afternoon route", "enabled": true,
		"timezone": "Asia/Shanghai", "time_window": map[string]any{"start": "12:00", "end": "18:00"},
		"mode": "custom_route", "route": map[string]any{"steps": []map[string]any{{
			"provider_id": "provider-a", "upstream_model": "upstream-model-2", "recover_on": []string{},
		}}},
	})
	requireStatus(t, createRec, http.StatusAccepted)
	draftID, _ := mutation["draft_id"].(string)
	applyRec, _ := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, applyRec, http.StatusOK)

	for _, test := range []struct {
		at       string
		override string
		model    string
	}{
		{"2026-08-30T03:59:00Z", "", "upstream-model"},
		{"2026-08-30T04:00:00Z", "afternoon", "upstream-model-2"},
	} {
		rec, response := f.do(http.MethodPost, "/api/models/smart/simulate", map[string]any{
			"protocol": domain.ProtocolOpenAIChat, "at": test.at,
		})
		requireStatus(t, rec, http.StatusOK)
		if response["matched_override"] != test.override {
			t.Fatalf("simulation at %s matched=%v, want %q", test.at, response["matched_override"], test.override)
		}
		selection, _ := response["expected_selection"].(map[string]any)
		if selection["upstream_model"] != test.model {
			t.Fatalf("simulation at %s selection=%v, want model %s", test.at, selection, test.model)
		}
		if response["simulated_at"] != test.at {
			t.Fatalf("simulation time=%v, want %s", response["simulated_at"], test.at)
		}
	}
}

func TestLogicalModelSimulationKeepsCandidateEvidenceOnPlanningFailure(t *testing.T) {
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/models/smart/simulate", map[string]any{
		"protocol":              domain.ProtocolOpenAIChat,
		"required_capabilities": map[string]any{"images": true},
	})
	requireStatus(t, rec, http.StatusOK)
	if response["planning_failure"] == nil {
		t.Fatalf("planning failure missing: %v", response)
	}
	candidates, _ := response["connection_candidates"].([]any)
	if len(candidates) == 0 || candidates[0].(map[string]any)["eligible"] != false {
		t.Fatalf("candidate exclusion evidence missing: %v", response)
	}
}

func TestLogicalModelSimulationRejectsInvalidRequestContext(t *testing.T) {
	for _, test := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"protocol", map[string]any{"protocol": "made-up"}, "unsupported simulation protocol"},
		{"negative context", map[string]any{"required_capabilities": map[string]any{"min_context": -1}}, "must be non-negative"},
		{"timestamp", map[string]any{"at": "tomorrow afternoon"}, "at must be RFC3339"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newV2APIFixture(t)
			rec, _ := f.do(http.MethodPost, "/api/models/smart/simulate", test.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.want) {
				t.Fatalf("invalid simulation input status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRoutingReviewReturnsTheSpecificValidationFailure(t *testing.T) {
	f := newV2APIFixture(t)
	reviewRec, review := f.do(http.MethodPost, "/api/models/smart/routing/review", map[string]any{
		"routes": []map[string]any{{
			"provider_id": "provider-a", "upstream_model": "upstream-model",
			"recover_on": []string{"connect"},
		}},
		"recovery_enabled": true,
	})
	requireStatus(t, reviewRec, http.StatusUnprocessableEntity)
	structured, _ := review["error"].(map[string]any)
	message, _ := structured["message"].(string)
	if !strings.Contains(message, `unknown recover_on failure "connect"`) {
		t.Fatalf("routing validation message=%v", review)
	}
	details, _ := structured["details"].([]any)
	if len(details) != 1 || !strings.Contains(details[0].(string), "route step 1") {
		t.Fatalf("routing validation details=%v", review)
	}
}

func TestRoutingReviewPreservesExplicitProgressionAndAllowsProviderReuse(t *testing.T) {
	f := newV2APIFixture(t)
	reviewRec, review := f.do(http.MethodPost, "/api/models/smart/routing/review", map[string]any{
		"routes": []map[string]any{
			{"provider_id": "provider-a", "upstream_model": "upstream-model", "recover_on": []string{"rate_limit", "provider_5xx"}},
			{"provider_id": "provider-a", "upstream_model": "upstream-model-2", "recover_on": []string{}},
		},
		"conversation_strategy": "migrate",
	})
	requireStatus(t, reviewRec, http.StatusOK)
	after, _ := review["after"].([]any)
	if len(after) != 2 {
		t.Fatalf("routing review after=%v", review)
	}
	first, _ := after[0].(map[string]any)
	second, _ := after[1].(map[string]any)
	firstRecovery, _ := first["recover_on"].([]any)
	if len(firstRecovery) != 2 || firstRecovery[0] != "rate_limit" || firstRecovery[1] != "provider_5xx" {
		t.Fatalf("first route did not preserve explicit progression failures: %v", first)
	}
	if secondRecovery, ok := second["recover_on"].([]any); ok && len(secondRecovery) != 0 {
		t.Fatalf("final route retained meaningless progression failures: %v", second)
	}
}

func TestRoutingReviewPreservesExplicitEmptyOnNonFinalStep(t *testing.T) {
	f := newV2APIFixture(t)
	reviewRec, review := f.do(http.MethodPost, "/api/models/smart/routing/review", map[string]any{
		"routes": []map[string]any{
			{"provider_id": "provider-a", "upstream_model": "upstream-model", "recover_on": []string{}},
			{"provider_id": "provider-a", "upstream_model": "upstream-model-2", "recover_on": []string{}},
		},
	})
	requireStatus(t, reviewRec, http.StatusOK)
	draftID, _ := review["draft_id"].(string)
	if draftID == "" {
		t.Fatalf("routing review did not return a draft: %v", review)
	}
	draft, err := f.control.Draft(draftID)
	if err != nil {
		t.Fatal(err)
	}
	recovery := draft.Intent.LogicalModels[0].DefaultRoute.Steps[0].RecoverOn
	if recovery == nil || len(recovery) != 0 {
		t.Fatalf("explicit empty recover_on was replaced by defaults: %v", recovery)
	}
}

func TestRoutingReviewRejectsNonRecoverableFailureOnNonFinalStep(t *testing.T) {
	f := newV2APIFixture(t)
	reviewRec, review := f.do(http.MethodPost, "/api/models/smart/routing/review", map[string]any{
		"routes": []map[string]any{
			{"provider_id": "provider-a", "upstream_model": "upstream-model", "recover_on": []string{"authentication_failure"}},
			{"provider_id": "provider-a", "upstream_model": "upstream-model-2", "recover_on": []string{}},
		},
	})
	requireStatus(t, reviewRec, http.StatusUnprocessableEntity)
	if !strings.Contains(reviewRec.Body.String(), `fallback on \"authentication_failure\" is not allowed`) {
		t.Fatalf("non-recoverable failure validation=%v", review)
	}
}

func TestV2CascadeDeletesCreateValidDependencyFreeDrafts(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		f := newV2APIFixture(t)
		guarded, _ := f.do(http.MethodDelete, "/api/providers/provider-a", nil)
		requireStatus(t, guarded, http.StatusConflict)

		rec, response := f.do(http.MethodDelete, "/api/providers/provider-a?cascade=true", nil)
		requireStatus(t, rec, http.StatusAccepted)
		draftID, _ := response["draft_id"].(string)
		draft, err := f.control.Draft(draftID)
		if err != nil {
			t.Fatal(err)
		}
		if len(draft.Intent.Providers) != 0 || len(draft.Intent.Connections) != 0 {
			t.Fatalf("provider cascade retained dependencies: providers=%+v connections=%+v", draft.Intent.Providers, draft.Intent.Connections)
		}
		for _, logical := range draft.Intent.LogicalModels {
			if logical.Enabled || len(logical.DefaultRoute.Steps) != 0 ||
				logical.ExistingConversationStrategy != domain.ConversationMigrate {
				t.Fatalf("provider cascade did not detach and disable the last route: %+v", draft.Intent.LogicalModels)
			}
		}
		validateRec, validation := f.do(http.MethodPost, "/api/drafts/"+draftID+"/validate", map[string]any{})
		requireStatus(t, validateRec, http.StatusOK)
		if validation["valid"] != true {
			t.Fatalf("provider cascade draft invalid: %v", validation)
		}
	})

	t.Run("connection", func(t *testing.T) {
		f := newV2APIFixture(t)
		guarded, _ := f.do(http.MethodDelete, "/api/providers/provider-a/connections/connection-a", nil)
		requireStatus(t, guarded, http.StatusNotFound)

		rec, response := f.do(http.MethodDelete, "/api/providers/provider-a/connections/connection-a?cascade=true", nil)
		requireStatus(t, rec, http.StatusAccepted)
		draftID, _ := response["draft_id"].(string)
		draft, err := f.control.Draft(draftID)
		if err != nil {
			t.Fatal(err)
		}
		if len(draft.Intent.Connections) != 0 || len(draft.Intent.Providers[0].Connections) != 0 {
			t.Fatalf("connection cascade retained connection references: %+v", draft.Intent)
		}
		for _, logical := range draft.Intent.LogicalModels {
			if logical.Enabled || len(logical.DefaultRoute.Steps) != 0 {
				t.Fatalf("connection cascade retained an unsupplied route: %+v", logical)
			}
		}
		validateRec, validation := f.do(http.MethodPost, "/api/drafts/"+draftID+"/validate", map[string]any{})
		requireStatus(t, validateRec, http.StatusOK)
		if validation["valid"] != true {
			t.Fatalf("connection cascade draft invalid: %v", validation)
		}
	})

	t.Run("logical model", func(t *testing.T) {
		f := newV2APIFixture(t)
		managed, _ := f.do(http.MethodDelete, "/api/models/upstream-model?cascade=true", nil)
		requireStatus(t, managed, http.StatusConflict)
		guarded, _ := f.do(http.MethodDelete, "/api/models/smart", nil)
		requireStatus(t, guarded, http.StatusConflict)

		rec, response := f.do(http.MethodDelete, "/api/models/smart?cascade=true", nil)
		requireStatus(t, rec, http.StatusAccepted)
		draftID, _ := response["draft_id"].(string)
		draft, err := f.control.Draft(draftID)
		if err != nil {
			t.Fatal(err)
		}
		if logicalModelIndex(draft.Intent, "smart") >= 0 || logicalModelIndex(draft.Intent, "upstream-model") < 0 ||
			logicalModelIndex(draft.Intent, "upstream-model-2") < 0 {
			t.Fatalf("logical model cascade removed or retained the wrong models: %+v", draft.Intent.LogicalModels)
		}
	})
}

func TestV2ProviderCreateCanAtomicallyIncludeExecutionIdentity(t *testing.T) {
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers", map[string]any{
		"id": "provider-b", "name": "Provider B", "type": "custom",
		"base_url": "https://provider-b.test/v1", "enabled": true,
		"scheduled_message": map[string]any{"enabled": true, "upstream_model": "model-b", "message": "provider-b heartbeat"},
		"concurrency":       map[string]any{"max_concurrent": 3, "max_queue": 8, "max_wait_seconds": 15, "queue_when_full": false},
		"connection": map[string]any{
			"id": "provider-b", "identity": "primary", "enabled": true,
			"credential": map[string]any{"secret_ref": "store:provider-b-credential"},
			"models": []any{map[string]any{
				"upstream_model": "model-b", "protocols": []string{"openai-chat"},
				"capabilities": map[string]any{"text": true, "stream": true},
			}},
		},
	})
	requireStatus(t, rec, http.StatusAccepted)
	draftID, _ := response["draft_id"].(string)
	draft, err := f.control.Draft(draftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Intent.Providers) != 2 || len(draft.Intent.Connections) != 2 {
		t.Fatalf("composite provider draft=%+v", draft.Intent)
	}
	provider := draft.Intent.Providers[1]
	connection := draft.Intent.Connections[1]
	if len(provider.Connections) != 1 || provider.Connections[0].ID != "provider-b" ||
		connection.ProviderID != "provider-b" || len(connection.Models) != 1 || connection.Models[0].UpstreamModel != "model-b" {
		t.Fatalf("provider=%+v connection=%+v", provider, connection)
	}
	if provider.Concurrency == nil || provider.Concurrency.MaxConcurrent != 3 || provider.Concurrency.QueuesWhenFull() {
		t.Fatalf("provider concurrency override=%+v", provider.Concurrency)
	}
	if provider.ScheduledMessage == nil || !provider.ScheduledMessage.Enabled ||
		provider.ScheduledMessage.UpstreamModel != "model-b" || provider.ScheduledMessage.Message != "provider-b heartbeat" {
		t.Fatalf("provider scheduled message=%+v", provider.ScheduledMessage)
	}
	modelAt := logicalModelIndex(draft.Intent, "model-b")
	if modelAt < 0 || !draft.Intent.LogicalModels[modelAt].Enabled ||
		len(draft.Intent.LogicalModels[modelAt].DefaultRoute.Steps) != 1 ||
		draft.Intent.LogicalModels[modelAt].DefaultRoute.Steps[0].ProviderID != "provider-b" {
		t.Fatalf("provider draft did not create its same-name logical model: %+v", draft.Intent.LogicalModels)
	}
	validateRec, validation := f.do(http.MethodPost, "/api/drafts/"+draftID+"/validate", map[string]any{})
	requireStatus(t, validateRec, http.StatusOK)
	if validation["valid"] != true {
		t.Fatalf("composite provider draft invalid: %v", validation)
	}
}

func TestV2SameNameLogicalModelSupportsSlashModelIDs(t *testing.T) {
	f := newV2APIFixture(t)
	const modelID = "google/gemini-3.7-flash"
	rec, response := f.do(http.MethodPost, "/api/providers", map[string]any{
		"id": "slash-provider", "name": "Slash Provider", "type": "custom",
		"base_url": "https://slash-provider.test/v1", "enabled": true,
		"connection": map[string]any{
			"id": "slash-provider", "enabled": true,
			"credential": map[string]any{"secret_ref": "store:slash-provider-credential"},
			"models": []any{map[string]any{
				"upstream_model": modelID, "protocols": []string{"openai-chat"},
				"capabilities": map[string]any{"text": true, "stream": true},
			}},
		},
	})
	requireStatus(t, rec, http.StatusAccepted)
	draftID := response["draft_id"].(string)
	draft, err := f.control.Draft(draftID)
	if err != nil {
		t.Fatal(err)
	}
	if logicalModelIndex(draft.Intent, modelID) < 0 {
		t.Fatalf("same-name slash model missing from draft: %+v", draft.Intent.LogicalModels)
	}
	applyRec, _ := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, applyRec, http.StatusOK)
	detailRec, detail := f.do(http.MethodGet, "/api/models/google%2Fgemini-3.7-flash", nil)
	requireStatus(t, detailRec, http.StatusOK)
	if detail["id"] != modelID {
		t.Fatalf("slash model detail=%v", detail)
	}
}

func TestV2CodexProviderCreateNormalizesBuiltInCapabilities(t *testing.T) {
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers", map[string]any{
		"id": "codex", "name": "Codex", "type": "cliproxy-codex",
		"base_url": domain.CodexBaseURL, "enabled": true,
		"connection": map[string]any{
			"id": "codex", "enabled": false,
			"credential": map[string]any{"secret_ref": domain.OAuthRef("personal")},
			"models": []any{
				map[string]any{
					"upstream_model": "gpt-5.6-sol", "protocols": []string{"openai-images"},
					"capabilities": map[string]any{"text": true},
				},
				map[string]any{
					"upstream_model": "gpt-image-2", "protocols": []string{"openai-responses"},
					"capabilities": map[string]any{"text": true, "stream": true},
				},
			},
		},
	})
	requireStatus(t, rec, http.StatusAccepted)
	draft, err := f.control.Draft(response["draft_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	models := draft.Intent.Connections[len(draft.Intent.Connections)-1].Models
	capabilities := models[0].Capabilities
	if !capabilities.Stream || !capabilities.Tools || !capabilities.ParallelTools || !capabilities.Reasoning ||
		!capabilities.StructuredOutput ||
		capabilities.ContextWindow != 372000 || len(models[0].Protocols) != 1 || models[0].Protocols[0] != domain.ProtocolOpenAIResponses {
		t.Fatalf("Codex built-in capabilities were not normalized: %+v", capabilities)
	}
	image := models[1]
	if len(image.Protocols) != 1 || image.Protocols[0] != domain.ProtocolOpenAIImages ||
		!image.Capabilities.Images || image.Capabilities.Text || image.Capabilities.Stream ||
		image.Capabilities.StructuredOutput {
		t.Fatalf("Codex image capability was not normalized: %+v", image)
	}
}

func TestV2CodexProviderPatchUpdatesAccountRoutingAndModelsAtomically(t *testing.T) {
	f := newV2APIFixture(t)
	createRec, createResponse := f.do(http.MethodPost, "/api/providers", map[string]any{
		"id": "codex", "name": "Codex", "type": "cliproxy-codex",
		"base_url": domain.CodexBaseURL, "enabled": true,
		"connection": map[string]any{
			"id": "codex-personal", "identity": "personal", "enabled": false,
			"credential": map[string]any{"secret_ref": domain.OAuthRef("personal")},
			"models": []any{map[string]any{
				"upstream_model": "gpt-5.6-sol", "protocols": []string{"openai-responses"},
				"capabilities": map[string]any{"text": true},
			}},
		},
	})
	requireStatus(t, createRec, http.StatusAccepted)
	draftID := createResponse["draft_id"].(string)

	patchRec, patchResponse := f.do(http.MethodPatch, "/api/providers/codex?draft_id="+draftID, map[string]any{
		"name": "Codex Images", "concurrency": map[string]any{"max_concurrent": 2},
		"connections": []any{map[string]any{
			"id": "codex-personal", "enabled": true,
			"models": []any{map[string]any{
				"upstream_model": "gpt-image-2", "protocols": []string{"openai-responses"},
				"capabilities": map[string]any{"text": true, "stream": true},
			}},
		}},
	})
	requireStatus(t, patchRec, http.StatusAccepted)
	draft, err := f.control.Draft(patchResponse["draft_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	provider := draft.Intent.Providers[len(draft.Intent.Providers)-1]
	connection := draft.Intent.Connections[len(draft.Intent.Connections)-1]
	if provider.Name != "Codex Images" || provider.Concurrency == nil || provider.Concurrency.MaxConcurrent != 2 {
		t.Fatalf("provider patch=%+v", provider)
	}
	if connection.Identity != "personal" || connection.Credential.SecretRef != domain.OAuthRef("personal") || !connection.Enabled {
		t.Fatalf("OAuth account metadata changed: %+v", connection)
	}
	if len(connection.Models) != 1 || connection.Models[0].UpstreamModel != "gpt-image-2" {
		t.Fatalf("OAuth account models=%+v", connection.Models)
	}
	image := connection.Models[0]
	if len(image.Protocols) != 1 || image.Protocols[0] != domain.ProtocolOpenAIImages ||
		!image.Capabilities.Images || image.Capabilities.Text || image.Capabilities.Stream {
		t.Fatalf("Codex image capability was not normalized: %+v", image)
	}
}

func TestV2CustomProviderCreatePreservesDeclaredCapabilitiesWithoutModelNameGuessing(t *testing.T) {
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPost, "/api/providers", map[string]any{
		"id": "zhipu", "name": "Zhipu", "type": "custom",
		"base_url": "https://open.bigmodel.test/v1", "enabled": true,
		"connection": map[string]any{
			"id": "zhipu-chat", "enabled": true,
			"credential": map[string]any{"secret_ref": "store:zhipu"},
			"models": []any{map[string]any{
				"upstream_model": "glm-5.3-flash", "protocols": []string{"openai-chat"},
				"capabilities": map[string]any{"text": true, "stream": true, "tools": true},
			}},
		},
	})
	requireStatus(t, rec, http.StatusAccepted)
	draft, err := f.control.Draft(response["draft_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	capabilities := draft.Intent.Connections[len(draft.Intent.Connections)-1].Models[0].Capabilities
	if !capabilities.Text || !capabilities.Stream || !capabilities.Tools || capabilities.Reasoning {
		t.Fatalf("custom API model capabilities were inferred from the model name: %+v", capabilities)
	}
}

func TestV2ProviderConcurrencyCanBeClearedToGlobalDefaults(t *testing.T) {
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{
		"concurrency": map[string]any{"max_concurrent": 5, "queue_when_full": false},
	})
	requireStatus(t, rec, http.StatusAccepted)
	draft, err := f.control.Draft(response["draft_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if draft.Intent.Providers[0].Concurrency == nil || draft.Intent.Providers[0].Concurrency.MaxConcurrent != 5 {
		t.Fatalf("provider override=%+v", draft.Intent.Providers[0].Concurrency)
	}

	clearRec, clearResponse := f.do(http.MethodPatch, "/api/providers/provider-a?draft_id="+draft.ID, map[string]any{"concurrency": nil})
	requireStatus(t, clearRec, http.StatusAccepted)
	cleared, err := f.control.Draft(clearResponse["draft_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Intent.Providers[0].Concurrency != nil {
		t.Fatalf("provider override not cleared: %+v", cleared.Intent.Providers[0].Concurrency)
	}
}

func TestV2ProviderScheduledMessageCanBeConfiguredViewedAndCleared(t *testing.T) {
	f := newV2APIFixture(t)
	rec, response := f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{
		"scheduled_message": map[string]any{
			"enabled": true, "upstream_model": "upstream-model", "message": "provider heartbeat",
		},
	})
	requireStatus(t, rec, http.StatusAccepted)
	draftID := response["draft_id"].(string)
	validateRec, validation := f.do(http.MethodPost, "/api/drafts/"+draftID+"/validate", map[string]any{})
	requireStatus(t, validateRec, http.StatusOK)
	if validation["valid"] != true {
		t.Fatalf("scheduled message draft invalid: %v", validation)
	}
	applyRec, _ := f.do(http.MethodPost, "/api/drafts/"+draftID+"/apply", map[string]any{"expected_revision": 1})
	requireStatus(t, applyRec, http.StatusOK)
	viewRec, view := f.do(http.MethodGet, "/api/providers/provider-a", nil)
	requireStatus(t, viewRec, http.StatusOK)
	scheduled, ok := view["scheduled_message"].(map[string]any)
	if !ok || scheduled["enabled"] != true || scheduled["upstream_model"] != "upstream-model" || scheduled["message"] != "provider heartbeat" {
		t.Fatalf("provider scheduled message view=%v", view["scheduled_message"])
	}

	clearRec, clearResponse := f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{"scheduled_message": nil})
	requireStatus(t, clearRec, http.StatusAccepted)
	cleared, err := f.control.Draft(clearResponse["draft_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Intent.Providers[0].ScheduledMessage != nil {
		t.Fatalf("provider scheduled message not cleared: %+v", cleared.Intent.Providers[0].ScheduledMessage)
	}
}

func TestV2ProviderModelSyncIntervalDefaultsAndPatchesThroughDraft(t *testing.T) {
	f := newV2APIFixture(t)
	controlRef := "store:ARK_CONTROL"
	sharedRef := "store:FERRY_V2_TEST_KEY"
	viewRec, view := f.do(http.MethodGet, "/api/providers/provider-a", nil)
	requireStatus(t, viewRec, http.StatusOK)
	modelSync, ok := view["model_sync"].(map[string]any)
	if view["model_sync_interval"] != "1h" || !ok || modelSync["interval"] != "1h" || modelSync["status"] != "idle" {
		t.Fatalf("default sync view=%v", view)
	}
	if view["model_sync_catalog"] != nil {
		t.Fatalf("default catalog=%v", view["model_sync_catalog"])
	}

	invalidRec, _ := f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{"model_sync_interval": "15m"})
	requireStatus(t, invalidRec, http.StatusBadRequest)
	invalidCatalogRec, _ := f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{
		"model_sync_catalog": map[string]any{"type": "volcengine-plan", "plan": "unknown", "credential_ref": controlRef},
	})
	requireStatus(t, invalidCatalogRec, http.StatusBadRequest)
	sharedCatalogRec, _ := f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{
		"model_sync_catalog": map[string]any{"type": "volcengine-plan", "plan": "coding", "credential_ref": sharedRef},
	})
	requireStatus(t, sharedCatalogRec, http.StatusUnprocessableEntity)

	patchRec, response := f.do(http.MethodPatch, "/api/providers/provider-a", map[string]any{
		"model_sync_interval": "30m",
		"model_sync_catalog":  map[string]any{"type": "volcengine-plan", "plan": "coding", "credential_ref": controlRef},
	})
	requireStatus(t, patchRec, http.StatusAccepted)
	draft, err := f.control.Draft(response["draft_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if draft.Intent.Providers[0].ModelSyncInterval != domain.ProviderModelSync30Minutes ||
		draft.Intent.Providers[0].ModelSyncCatalog == nil ||
		draft.Intent.Providers[0].ModelSyncCatalog.Plan != domain.ProviderModelSyncCatalogCodingPlan {
		t.Fatalf("draft provider=%+v", draft.Intent.Providers[0])
	}
	validateRec, validation := f.do(http.MethodPost, "/api/drafts/"+draft.ID+"/validate", map[string]any{})
	requireStatus(t, validateRec, http.StatusOK)
	if validation["valid"] != true {
		t.Fatalf("interval draft invalid=%v", validation)
	}
}

func TestV2ProviderProtocolConfigReplacesProtocolConnectionsAtomically(t *testing.T) {
	f := newV2APIFixture(t)
	controlRef := "store:ARK_CONTROL"
	rec, response := f.do(http.MethodPut, "/api/providers/provider-a/protocol-config", map[string]any{
		"name": "Provider A", "base_url": "https://default.test/v1", "enabled": true,
		"model_sync_interval": "off",
		"model_sync_catalog":  map[string]any{"type": "volcengine-plan", "plan": "agent", "credential_ref": controlRef},
		"protocol_conversion": map[string]bool{"openai-chat": true, "openai-responses": false, "anthropic": true},
		"scheduled_message":   map[string]any{"enabled": true, "upstream_model": "upstream-model", "message": "protocol heartbeat"},
		"connections": []any{
			map[string]any{
				"id": "provider-a-chat", "base_url": "https://chat.test/v1", "enabled": true,
				"credential": map[string]any{"secret_ref": "store:shared-key"},
				"models":     []any{map[string]any{"upstream_model": "upstream-model", "protocols": []string{"openai-chat"}, "capabilities": map[string]any{"text": true, "stream": true}}},
			},
			map[string]any{
				"id": "provider-a-responses", "full_url": "https://responses.test/custom/invoke", "enabled": true,
				"credential": map[string]any{"secret_ref": "store:responses-key"},
				"models":     []any{map[string]any{"upstream_model": "responses-only", "protocols": []string{"openai-responses"}, "capabilities": map[string]any{"text": true, "stream": true}}},
			},
		},
	})
	requireStatus(t, rec, http.StatusAccepted)
	draftID, _ := response["draft_id"].(string)
	draft, err := f.control.Draft(draftID)
	if err != nil {
		t.Fatal(err)
	}
	provider := draft.Intent.Providers[0]
	if provider.BaseURL != "https://default.test/v1" || provider.ModelSyncInterval != domain.ProviderModelSyncOff ||
		provider.ModelSyncCatalog == nil || provider.ModelSyncCatalog.Plan != domain.ProviderModelSyncCatalogAgentPlan ||
		len(provider.Connections) != 2 || provider.ProtocolConversion[domain.ProtocolOpenAIResponses] {
		t.Fatalf("provider protocol config=%+v", provider)
	}
	if provider.ScheduledMessage == nil || provider.ScheduledMessage.UpstreamModel != "upstream-model" ||
		provider.ScheduledMessage.Message != "protocol heartbeat" {
		t.Fatalf("provider protocol scheduled message=%+v", provider.ScheduledMessage)
	}
	if len(draft.Intent.Connections) != 2 || draft.Intent.Connections[0].BaseURL != "https://chat.test/v1" ||
		draft.Intent.Connections[1].FullURL != "https://responses.test/custom/invoke" ||
		draft.Intent.Connections[1].Credential.SecretRef != "store:responses-key" {
		t.Fatalf("protocol connections=%+v", draft.Intent.Connections)
	}
	validateRec, validation := f.do(http.MethodPost, "/api/drafts/"+draftID+"/validate", map[string]any{})
	requireStatus(t, validateRec, http.StatusOK)
	if validation["valid"] != true {
		t.Fatalf("protocol config draft invalid: %v", validation)
	}
}

func TestV2SettingsControlRequestDebugCapture(t *testing.T) {
	f := newV2APIFixture(t)
	getRec, settings := f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, getRec, http.StatusOK)
	if settings["debug_request_response"] != false {
		t.Fatalf("debug setting default=%v", settings)
	}
	patchRec, settings := f.do(http.MethodPatch, "/api/settings", map[string]any{"debug_request_response": true})
	requireStatus(t, patchRec, http.StatusOK)
	if settings["debug_request_response"] != true || !f.ledger.DebugCaptureEnabled() {
		t.Fatalf("debug setting not applied: %v", settings)
	}

	now := time.Now()
	f.ledger.BeginRequest(domain.RequestRecord{ID: "req-debug", Ts: now, LogicalModelID: "smart", Status: domain.ReqSuccess})
	f.ledger.CaptureDebugRequest("req-debug", now, `{"model":"smart"}`, false)
	f.ledger.CaptureDebugResponse("req-debug", `{"status":"success"}`, false)
	f.ledger.FinishRequest("req-debug", nil)
	var detailRec *httptest.ResponseRecorder
	var detail map[string]any
	for i := 0; i < 50; i++ {
		detailRec, detail = f.do(http.MethodGet, "/api/requests/req-debug", nil)
		if detailRec.Code == http.StatusOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	requireStatus(t, detailRec, http.StatusOK)
	debug, _ := detail["debug"].(map[string]any)
	request, _ := debug["request"].(map[string]any)
	response, _ := debug["response"].(map[string]any)
	if request["model"] != "smart" || response["status"] != "success" {
		t.Fatalf("request detail debug=%v", debug)
	}
}

func TestCascadeDeleteV2ProviderPrunesOnlyItsRouteDependencies(t *testing.T) {
	intent := nativeV2Config()
	intent.Providers = append(intent.Providers, domain.Provider{
		ID: "provider-b", Name: "Provider B", Type: domain.ProviderCustom,
		BaseURL: "https://provider-b.test/v1", Enabled: true,
		Connections: []domain.ConnectionRef{{ID: "connection-b"}},
	})
	intent.Connections = append(intent.Connections, domain.Connection{
		ID: "connection-b", ProviderID: "provider-b", Enabled: true,
		Credential: domain.CredentialRef{SecretRef: "env:FERRY_V2_TEST_B"},
		Models: []domain.ConnectionModel{{
			UpstreamModel: "upstream-model", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
			Capabilities: domain.Capabilities{Text: true},
		}},
	})
	model := &intent.LogicalModels[0]
	model.DefaultRoute.Steps = append(model.DefaultRoute.Steps,
		domain.RouteStep{ProviderID: "provider-b", UpstreamModel: "upstream-model"})
	model.Overrides = []domain.OverrideSpec{
		{ID: "only-a", Enabled: true, Mode: domain.OverrideCustomRoute, Route: &domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-model"}}}},
		{ID: "adjust", Enabled: true, Mode: domain.OverrideAdjustDefault,
			Prefer: []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-model"}},
			Avoid:  []domain.RouteStep{{ProviderID: "provider-b", UpstreamModel: "upstream-model"}}},
	}

	removedConnections, impact, err := cascadeDeleteV2Provider(intent, "provider-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(removedConnections) != 1 || removedConnections[0] != "connection-a" {
		t.Fatalf("removed connections=%v", removedConnections)
	}
	if len(impact.RemovedOverrides) != 2 || impact.RemovedOverrides[0] != "smart/only-a" || impact.RemovedOverrides[1] != "smart/adjust" {
		t.Fatalf("removed overrides=%v", impact.RemovedOverrides)
	}
	if !model.Enabled || model.ExistingConversationStrategy != domain.ConversationMigrate ||
		len(model.DefaultRoute.Steps) != 1 || model.DefaultRoute.Steps[0].ProviderID != "provider-b" {
		t.Fatalf("remaining model route=%+v", model)
	}
	if len(model.Overrides) != 0 {
		t.Fatalf("remaining overrides=%+v", model.Overrides)
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("cascade result did not validate: %v", err)
	}
}

func TestV2RequestDetailSuppressesPrivateRuntimeLeaf(t *testing.T) {
	f := newV2APIFixture(t)
	id := "req-v2-safe"
	targetID := domain.V2ExecutionTargetID("provider-a", "connection-a", "upstream-model")
	now := time.Now()
	f.ledger.BeginRequest(domain.RequestRecord{
		ID: id, Ts: now, ConsumerKey: "initial", LogicalModelID: "smart",
		ProviderID: "provider-a", ConnectionID: "connection-a", UpstreamModel: "upstream-model",
		RuntimeRevision: 1, Status: domain.ReqSuccess, Outcome: domain.OutcomeRecoveredSuccess,
		PolicyName: "legacy-private", SelectedTarget: targetID,
	})
	f.ledger.AddAttempt(domain.AttemptRecord{
		RequestID: id, Seq: 1, Ts: now, TargetID: targetID,
		ProviderID: "historical-provider", ConnectionID: "historical-connection", Model: "upstream-model",
		Protocol: domain.ProtocolOpenAIChat, Status: domain.ReqSuccess,
	})
	f.ledger.SetTrace(id, &domain.TraceRecord{
		PolicyName: "legacy-private", Primary: targetID,
		RequiredCaps: domain.Requirements{Text: true, Tools: true},
		Candidates: []domain.CandidateRecord{{
			Target: targetID, Eligible: false, ExclusionReason: "connection model capability missing: tools",
		}},
		Events: []domain.ExecutionTraceEvent{{
			At: now, Category: domain.TraceRoute, Type: "CONNECTION_SELECTED",
			ProviderID: "provider-a", ConnectionID: "connection-a", UpstreamModel: "upstream-model",
		}},
	})
	f.ledger.FinishRequest(id, nil)

	var rec *httptest.ResponseRecorder
	for i := 0; i < 50; i++ {
		rec, _ = f.do(http.MethodGet, "/api/requests/"+id, nil)
		if rec.Code == http.StatusOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	requireStatus(t, rec, http.StatusOK)
	requireNoLegacyRuntimeVocabulary(t, rec.Body.String())
	if strings.Contains(rec.Body.String(), string(targetID)) ||
		!strings.Contains(rec.Body.String(), `"provider_id":"historical-provider"`) ||
		!strings.Contains(rec.Body.String(), `"connection_id":"historical-connection"`) ||
		!strings.Contains(rec.Body.String(), `"required_capabilities":{"text":true`) ||
		!strings.Contains(rec.Body.String(), `"candidate_exclusions":[{"capabilities":`) ||
		!strings.Contains(rec.Body.String(), `"upstream_model":"upstream-model"`) {
		t.Fatalf("historical attempt depended on current snapshot: %s", rec.Body.String())
	}
}

func TestV2RequestSummaryIncludesFailureMessage(t *testing.T) {
	record := &domain.RequestRecord{
		ID: "req-failed", FailureType: domain.FailUnsupportedParam,
		FailureMsg: `unsupported content block type "redacted_thinking"`,
	}
	view := v2RequestSummary(record, "")
	if view["failure_message"] != record.FailureMsg {
		t.Fatalf("failure message missing from request evidence: %v", view)
	}
}

func TestV2APIKeyCreateRotateRevealAndRetire(t *testing.T) {
	f := newV2APIFixture(t)
	createRec, created := f.do(http.MethodPost, "/api/api-keys", map[string]any{
		"name": "smoke", "description": "ephemeral smoke identity", "exact_only": true,
	})
	requireStatus(t, createRec, http.StatusCreated)
	secret, _ := created["secret"].(string)
	identity, _ := created["identity"].(map[string]any)
	identityID, _ := identity["id"].(string)
	if secret == "" || identityID == "" {
		t.Fatalf("create response=%v", created)
	}
	if exactOnly, _ := identity["exact_only"].(bool); !exactOnly {
		t.Fatalf("create response omitted exact-only policy: %v", created)
	}
	patchRec, _ := f.do(http.MethodPatch, "/api/api-keys/"+identityID, map[string]any{"exact_only": false})
	requireStatus(t, patchRec, http.StatusOK)
	identities, _, err := f.control.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, persisted := range identities {
		if persisted.ID == identityID && persisted.ExactOnly {
			t.Fatalf("patch did not clear exact-only policy: %+v", persisted)
		}
	}
	revealRec, revealed := f.do(http.MethodGet, "/api/api-keys/"+identityID+"/secret", nil)
	requireStatus(t, revealRec, http.StatusOK)
	if revealed["secret"] != secret {
		t.Fatalf("created secret reveal=%v", revealed)
	}
	contract := &domain.RequestContract{
		Model: "smart", Protocol: domain.ProtocolOpenAIChat, Origin: domain.ProtocolOpenAIChat,
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "usage"}}}},
	}
	if _, failure := f.server.deps.Scheduler.Execute(context.Background(), contract, sched.RequestMeta{PresentedCredential: secret}); failure != nil {
		t.Fatalf("new API key data-plane use: %v", failure)
	}
	var usageRec *httptest.ResponseRecorder
	var usageDetail map[string]any
	for i := 0; i < 50; i++ {
		usageRec, usageDetail = f.do(http.MethodGet, "/api/api-keys/"+identityID, nil)
		if usageRec.Code == http.StatusOK && usageDetail["last_request_at"] != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	requireStatus(t, usageRec, http.StatusOK)
	used := false
	for _, raw := range usageDetail["secrets"].([]any) {
		item := raw.(map[string]any)
		used = used || item["last_used_at"] != nil
	}
	if !used {
		t.Fatalf("matched secret usage was not recorded: %v", usageDetail)
	}
	if usageDetail["last_request_at"] == nil {
		t.Fatalf("API key detail omitted key-level last request evidence: %v", usageDetail)
	}

	listRec, _ := f.do(http.MethodGet, "/api/api-keys", nil)
	requireStatus(t, listRec, http.StatusOK)
	if strings.Contains(listRec.Body.String(), secret) || strings.Contains(listRec.Body.String(), "secret_hash") {
		t.Fatalf("API key list exposed secret material: %s", listRec.Body.String())
	}

	rotateRec, rotated := f.do(http.MethodPost, "/api/api-keys/"+identityID+"/rotate", map[string]any{})
	requireStatus(t, rotateRec, http.StatusOK)
	newSecret, _ := rotated["secret"].(string)
	if newSecret == "" || newSecret == secret {
		t.Fatalf("rotation response=%v", rotated)
	}
	revealRec, revealed = f.do(http.MethodGet, "/api/api-keys/"+identityID+"/secret", nil)
	requireStatus(t, revealRec, http.StatusOK)
	if revealed["secret"] != newSecret {
		t.Fatalf("rotated secret reveal=%v", revealed)
	}
	detailRec, detail := f.do(http.MethodGet, "/api/api-keys/"+identityID, nil)
	requireStatus(t, detailRec, http.StatusOK)
	if strings.Contains(detailRec.Body.String(), secret) || strings.Contains(detailRec.Body.String(), newSecret) {
		t.Fatalf("API key detail exposed raw secret: %s", detailRec.Body.String())
	}
	var graceID string
	for _, raw := range detail["secrets"].([]any) {
		item := raw.(map[string]any)
		if item["state"] == string(domain.APIKeySecretGrace) {
			graceID, _ = item["id"].(string)
		}
	}
	if graceID == "" {
		t.Fatalf("rotation did not retain a grace secret: %v", detail)
	}
	retireRec, _ := f.do(http.MethodPost, "/api/api-keys/"+identityID+"/secrets/"+graceID+"/retire", map[string]any{})
	requireStatus(t, retireRec, http.StatusOK)
	afterRec, after := f.do(http.MethodGet, "/api/api-keys/"+identityID, nil)
	requireStatus(t, afterRec, http.StatusOK)
	foundRetired := false
	for _, raw := range after["secrets"].([]any) {
		item := raw.(map[string]any)
		foundRetired = foundRetired || (item["id"] == graceID && item["state"] == string(domain.APIKeySecretRetired))
	}
	if !foundRetired {
		t.Fatalf("retired secret metadata missing: %v", after)
	}
}

func TestDraftSimulationUsesRuntimePlannerStages(t *testing.T) {
	f := newV2APIFixture(t)
	draft, err := f.control.CreateDraft(1, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	identities, _, err := f.control.APIKeys()
	if err != nil || len(identities) == 0 {
		t.Fatalf("api keys=%+v err=%v", identities, err)
	}
	intent := draft.Intent
	intent.Providers = append(intent.Providers, domain.Provider{
		ID: "provider-b", Name: "Provider B", Type: domain.ProviderCustom,
		BaseURL: "https://provider-b.test/v1", Enabled: true,
		Connections: []domain.ConnectionRef{{ID: "connection-b"}},
	})
	intent.Connections = append(intent.Connections, domain.Connection{
		ID: "connection-b", ProviderID: "provider-b", Enabled: true,
		Credential: domain.CredentialRef{SecretRef: "env:FERRY_V2_TEST_B"},
		Models: []domain.ConnectionModel{{
			UpstreamModel: "upstream-model", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
			Capabilities: domain.Capabilities{Text: true},
		}},
	})
	intent.LogicalModels[0].DefaultRoute.Steps = append(intent.LogicalModels[0].DefaultRoute.Steps,
		domain.RouteStep{ProviderID: "provider-b", UpstreamModel: "upstream-model"})
	intent.LogicalModels[0].Overrides = []domain.OverrideSpec{{
		ID: "avoid-primary", Enabled: true, Callers: []string{identities[0].ID},
		Mode:  domain.OverrideAdjustDefault,
		Avoid: []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-model"}},
	}}
	if _, err := f.control.UpdateDraft(draft.ID, intent); err != nil {
		t.Fatal(err)
	}
	rec, response := f.do(http.MethodPost, "/api/drafts/"+draft.ID+"/simulate?model=smart", map[string]any{
		"caller_id": identities[0].ID, "protocol": domain.ProtocolOpenAIChat,
	})
	requireStatus(t, rec, http.StatusOK)
	models, _ := response["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("simulation=%v", response)
	}
	model := models[0].(map[string]any)
	for _, stage := range []string{"match", "transform", "effective_route", "eligibility", "expected_selection"} {
		if _, ok := model[stage]; !ok {
			t.Fatalf("missing planner stage %s: %v", stage, model)
		}
	}
	match := model["match"].(map[string]any)
	if match["override_id"] != "avoid-primary" {
		t.Fatalf("override match=%v", match)
	}
	route := model["effective_route"].(map[string]any)
	steps := route["steps"].([]any)
	if len(steps) != 1 || steps[0].(map[string]any)["provider_id"] != "provider-b" {
		t.Fatalf("avoid was not removed by shared planner: %v", route)
	}
	eligibility := model["eligibility"].([]any)
	if len(eligibility) != 1 || eligibility[0].(map[string]any)["connection_id"] != "connection-b" ||
		eligibility[0].(map[string]any)["eligible"] != false {
		t.Fatalf("runtime eligibility=%v", eligibility)
	}
	if strings.Contains(rec.Body.String(), "currently_live") {
		t.Fatalf("legacy simplified simulator field remained: %s", rec.Body.String())
	}
}

func TestProviderCatalogEligibleCountUsesLiveRuntimeState(t *testing.T) {
	f := newV2APIFixture(t)
	rec, _ := f.do(http.MethodGet, "/api/providers/provider-a", nil)
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"eligible_connections":["connection-a"]`) {
		t.Fatalf("initial live catalog=%s", rec.Body.String())
	}
	targetID := domain.V2ExecutionTargetID("provider-a", "connection-a", "upstream-model")
	f.server.deps.Manager.RecordFailure(targetID, &domain.Failure{
		Type: domain.FailConnect, Scope: domain.FailureScopeConnection,
	}, time.Now())
	rec, _ = f.do(http.MethodGet, "/api/providers/provider-a", nil)
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"eligible_connections":[]`) ||
		!strings.Contains(rec.Body.String(), `"eligible_connections":0`) {
		t.Fatalf("compiled enabled count masqueraded as live eligibility: %s", rec.Body.String())
	}
}

func TestAPIKeyRetirementGuardRollbackAndOverrideDeleteReference(t *testing.T) {
	f := newV2APIFixture(t)
	identities, secrets, err := f.control.APIKeys()
	if err != nil || len(identities) == 0 || len(secrets[identities[0].ID]) != 1 {
		t.Fatalf("initial keys=%+v secrets=%+v err=%v", identities, secrets, err)
	}
	last := secrets[identities[0].ID][0]
	rec, _ := f.do(http.MethodPost, "/api/api-keys/"+identities[0].ID+"/secrets/"+last.ID+"/retire", map[string]any{})
	requireStatus(t, rec, http.StatusConflict)

	createRec, created := f.do(http.MethodPost, "/api/api-keys", map[string]any{"name": "rotation"})
	requireStatus(t, createRec, http.StatusCreated)
	identity := created["identity"].(map[string]any)
	identityID := identity["id"].(string)
	rotateRec, _ := f.do(http.MethodPost, "/api/api-keys/"+identityID+"/rotate", map[string]any{})
	requireStatus(t, rotateRec, http.StatusOK)
	_, rotationSecrets, err := f.control.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	var graceID string
	for _, secret := range rotationSecrets[identityID] {
		if secret.State == domain.APIKeySecretGrace {
			graceID = secret.ID
		}
	}
	if graceID == "" {
		t.Fatalf("missing grace secret: %+v", rotationSecrets[identityID])
	}
	f.server.deps.ReloadAPIKeys = func() error { return errors.New("injected runtime refresh failure") }
	rec, _ = f.do(http.MethodPost, "/api/api-keys/"+identityID+"/secrets/"+graceID+"/retire", map[string]any{})
	requireStatus(t, rec, http.StatusInternalServerError)
	_, afterFailure, err := f.control.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range afterFailure[identityID] {
		if secret.ID == graceID && secret.State != domain.APIKeySecretGrace {
			t.Fatalf("durable state diverged after runtime refresh failure: %+v", secret)
		}
	}

	// Deletion is rejected before any config publish when an override refers
	// to the stable caller identity.
	f.server.deps.Config.LogicalModels[0].Overrides = []domain.OverrideSpec{{
		ID: "caller-specific", Enabled: true, Mode: domain.OverrideAdjustDefault,
		Callers: []string{identityID},
	}}
	rec, _ = f.do(http.MethodDelete, "/api/api-keys/"+identityID, nil)
	requireStatus(t, rec, http.StatusConflict)
}

func TestProductionConsoleFacadeUsesDurableReviewFiltersAndExplicitCapabilities(t *testing.T) {
	f := newV2APIFixture(t)

	root, _ := f.do(http.MethodGet, "/", nil)
	requireStatus(t, root, http.StatusOK)
	if !strings.Contains(root.Body.String(), `data-default-adapter="http"`) ||
		!strings.Contains(root.Body.String(), `src="./app-loader.js?visual=`) {
		t.Fatalf("production console did not select same-origin HTTP adapter: %s", root.Body.String())
	}
	asset, _ := f.do(http.MethodGet, "/api.js", nil)
	requireStatus(t, asset, http.StatusOK)
	if !strings.Contains(asset.Body.String(), "class HttpFerryAdapter") {
		t.Fatalf("reviewed console adapter asset missing: %s", asset.Body.String())
	}

	organizationsRec, organizations := f.do(http.MethodGet, "/api/organizations", nil)
	requireStatus(t, organizationsRec, http.StatusOK)
	if organizations["organization_scope_supported"] != false {
		t.Fatalf("single-runtime capability was not explicit: %v", organizations)
	}
	usageRec, usage := f.do(http.MethodGet, "/api/usage-billing?window=15m", nil)
	requireStatus(t, usageRec, http.StatusOK)
	if usage["summary"] == nil || usage["outcomes"] == nil || usage["by_provider"] == nil || usage["by_model"] == nil {
		t.Fatalf("usage facade is not ledger backed: %v", usage)
	}

	reviewBody := map[string]any{
		"routes":           []map[string]any{{"provider_id": "provider-a", "upstream_model": "upstream-model"}},
		"recovery_enabled": true, "conversation_strategy": "migrate",
	}
	reviewRec, review := f.do(http.MethodPost, "/api/models/smart/routing/review", reviewBody)
	requireStatus(t, reviewRec, http.StatusOK)
	draftID, _ := review["draft_id"].(string)
	if draftID == "" || review["conversation_strategy"] != "migrate" {
		t.Fatalf("routing review=%v", review)
	}
	beforeApply, _ := f.do(http.MethodGet, "/api/models/smart", nil)
	requireStatus(t, beforeApply, http.StatusOK)
	if strings.Contains(beforeApply.Body.String(), `"existing_conversation_strategy":"migrate"`) {
		t.Fatalf("review changed active runtime before apply: %s", beforeApply.Body.String())
	}

	staleBody := map[string]any{
		"routes":           []map[string]any{{"provider_id": "provider-a", "upstream_model": "upstream-model"}},
		"recovery_enabled": true, "conversation_strategy": "keep-existing",
	}
	staleReviewRec, staleReview := f.do(http.MethodPost, "/api/models/smart/routing/review", staleBody)
	requireStatus(t, staleReviewRec, http.StatusOK)
	applyRec, applied := f.do(http.MethodPost, "/api/models/smart/routing/apply", map[string]any{
		"draft_id": draftID, "expected_revision": review["expected_revision"],
	})
	requireStatus(t, applyRec, http.StatusOK)
	if applied["revision"] != float64(2) {
		t.Fatalf("routing apply=%v", applied)
	}
	afterApply, _ := f.do(http.MethodGet, "/api/models/smart", nil)
	requireStatus(t, afterApply, http.StatusOK)
	if !strings.Contains(afterApply.Body.String(), `"existing_conversation_strategy":"migrate"`) {
		t.Fatalf("applied conversation strategy missing: %s", afterApply.Body.String())
	}
	conflictRec, conflict := f.do(http.MethodPost, "/api/models/smart/routing/apply", map[string]any{
		"draft_id": staleReview["draft_id"], "expected_revision": staleReview["expected_revision"],
	})
	requireStatus(t, conflictRec, http.StatusConflict)
	errorBody, _ := conflict["error"].(map[string]any)
	if errorBody["code"] != "REVISION_CONFLICT" || errorBody["current_revision"] != float64(2) {
		t.Fatalf("structured CAS conflict=%v", conflict)
	}

	now := time.Now()
	for _, record := range []domain.RequestRecord{
		{ID: "request-recovered", Ts: now, ConsumerKey: "caller-a", LogicalModelID: "smart", ProviderID: "provider-a", Status: domain.ReqSuccess, Outcome: domain.OutcomeRecoveredSuccess},
		{ID: "request-failed", Ts: now.Add(time.Millisecond), ConsumerKey: "caller-b", LogicalModelID: "smart", ProviderID: "provider-b", Status: domain.ReqFailed, Outcome: domain.OutcomeFailed},
	} {
		f.ledger.BeginRequest(record)
		f.ledger.SetTrace(record.ID, &domain.TraceRecord{ConversationID: "conversation-" + record.ID})
		f.ledger.FinishRequest(record.ID, nil)
	}
	var filtered map[string]any
	for i := 0; i < 50; i++ {
		filteredRec, candidate := f.do(http.MethodGet, "/api/requests?window=15m&provider=provider-a&outcome=RECOVERED_SUCCESS&q=conversation-request-recovered", nil)
		requireStatus(t, filteredRec, http.StatusOK)
		if candidate["total"] == float64(1) {
			filtered = candidate
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if filtered == nil {
		t.Fatal("server-side request filters did not converge on the matching request")
	}
	requests := filtered["requests"].([]any)
	item := requests[0].(map[string]any)
	if item["id"] != "request-recovered" || item["conversation_id"] != "conversation-request-recovered" {
		t.Fatalf("filtered request=%v", item)
	}
}

func TestV2SettingsLogicalModelLimitPersists(t *testing.T) {
	f := newV2APIFixture(t)
	rec, settings := f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, rec, http.StatusOK)
	if settings["auto_logical_model_limit"] != float64(10) {
		t.Fatalf("default: %v", settings)
	}
	for _, limit := range []int{0, 12} {
		rec, settings = f.do(http.MethodPatch, "/api/settings", map[string]any{"auto_logical_model_limit": limit})
		requireStatus(t, rec, http.StatusOK)
		active, _, err := f.server.deps.Control.ActiveConfig()
		if err != nil || active.LogicalModelAutoLimit() != limit || settings["auto_logical_model_limit"] != float64(limit) {
			t.Fatalf("persisted setting: %v %v", settings, err)
		}
	}
	for _, invalid := range []any{-1, 10001, 1.5} {
		rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{"auto_logical_model_limit": invalid, "debug_request_response": true})
		requireStatus(t, rec, http.StatusBadRequest)
		if f.server.currentConfig().LogicalModelAutoLimit() != 12 || f.ledger.DebugCaptureEnabled() {
			t.Fatal("invalid patch changed settings")
		}
	}
}

func TestV2RequestSummaryCostMatchesLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record domain.RequestRecord
		total  float64
	}{
		{"serving and recovery", domain.RequestRecord{ServingCost: 2, RecoveryOverheadCost: 3, EstCashCost: 9, AllocatedSubCost: 1}, 5},
		{"legacy cash", domain.RequestRecord{EstCashCost: 4}, 4},
		{"unpriced", domain.RequestRecord{ShadowCost: 7}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := v2RequestSummary(&tc.record, "")
			if view["total_cost"] != tc.total || view["cost_observed"] != (tc.total != 0) || view["currency"] != "USD" || view["allocated_sub_cost"] != tc.record.AllocatedSubCost {
				t.Fatalf("cost: %v", view)
			}
		})
	}
}
