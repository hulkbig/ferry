package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/ledger"
	"ferry/internal/policy"
	"ferry/internal/runtime"
	sched "ferry/internal/scheduler"
)

func testInferenceHTTPClient(string) *http.Client {
	return newInferenceHTTPClient(time.Second)
}

func TestNewHTTPServerBoundsHeaderReadsWithoutBoundingStreams(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := newHTTPServer("127.0.0.1:0", handler)
	if server.Addr != "127.0.0.1:0" {
		t.Fatalf("address = %q", server.Addr)
	}
	if server.Handler == nil {
		t.Fatal("handler must be preserved")
	}
	if server.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("read header timeout = %s", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 0 || server.WriteTimeout != 0 {
		t.Fatalf("streaming deadlines changed: read=%s write=%s", server.ReadTimeout, server.WriteTimeout)
	}
}

func TestInferenceHTTPClientBoundsResponseHeadersWithoutBoundingStreams(t *testing.T) {
	const headerTimeout = 40 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, chunk := range []string{"com", "ple", "ted"} {
			time.Sleep(headerTimeout / 2)
			_, _ = w.Write([]byte(chunk))
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	client := newInferenceHTTPClient(headerTimeout)
	defer client.CloseIdleConnections()
	if client.Timeout != 0 {
		t.Fatalf("total request timeout = %s, want none for streaming responses", client.Timeout)
	}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request failed after response headers: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream body was cut off by response-header timeout: %v", err)
	}
	if string(body) != "completed" {
		t.Fatalf("body = %q", body)
	}
}

func TestProductionInferenceTimeoutSupportsLongRunningResponses(t *testing.T) {
	if upstreamResponseHeaderTimeout != 2*time.Hour {
		t.Fatalf("upstream response timeout = %s, want 2h", upstreamResponseHeaderTimeout)
	}
}

func TestInferenceHTTPClientTimesOutIdleResponseBody(t *testing.T) {
	const idleTimeout = 40 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	client := newInferenceHTTPClient(idleTimeout)
	defer client.CloseIdleConnections()
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("response headers failed: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil || !strings.Contains(err.Error(), "response body idle") {
		t.Fatalf("idle stream error = %v", err)
	}
}

func TestInferenceHTTPClientTimesOutWaitingForResponseHeaders(t *testing.T) {
	const headerTimeout = 40 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * headerTimeout):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()

	client := newInferenceHTTPClient(headerTimeout)
	defer client.CloseIdleConnections()
	if _, err := client.Get(upstream.URL); err == nil {
		t.Fatal("request waiting past the response-header timeout unexpectedly succeeded")
	}
}

func TestInferenceHTTPClientRegistryIsolatesConnections(t *testing.T) {
	registry := newInferenceHTTPClientRegistry(time.Second, 20*time.Millisecond)
	defer registry.CloseIdleConnections()

	first := registry.Client("connection-a")
	if first != registry.Client("connection-a") {
		t.Fatal("one connection did not retain its HTTP client")
	}
	if first == registry.Client("connection-b") || first.Transport == registry.Client("connection-b").Transport {
		t.Fatal("different connections unexpectedly shared an inference transport")
	}
}

func TestInferenceHTTPClientRotatesOnlyCanceledConnectionBeforeHeaders(t *testing.T) {
	const resetAfter = 20 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stuck" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	registry := newInferenceHTTPClientRegistry(time.Second, resetAfter)
	defer registry.CloseIdleConnections()
	clientA := registry.Client("connection-a")
	clientB := registry.Client("connection-b")
	transportA := clientA.Transport.(*inferenceRoundTripper)
	transportB := clientB.Transport.(*inferenceRoundTripper)

	ctx, cancel := context.WithTimeout(context.Background(), 2*resetAfter)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/stuck", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientA.Do(req); err == nil {
		t.Fatal("stuck request unexpectedly succeeded")
	}
	if transportA.Generation() != 2 {
		t.Fatalf("connection-a transport generation=%d want=2", transportA.Generation())
	}
	if transportB.Generation() != 1 {
		t.Fatalf("connection-b transport generation=%d want=1", transportB.Generation())
	}
	resp, err := clientA.Get(upstream.URL + "/healthy")
	if err != nil {
		t.Fatalf("request through rotated transport failed: %v", err)
	}
	_ = resp.Body.Close()
}

func TestInferenceHTTPClientRotatesAfterUnexpectedResponseBodyEOF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
	}))
	defer upstream.Close()

	registry := newInferenceHTTPClientRegistry(time.Second, 20*time.Millisecond)
	defer registry.CloseIdleConnections()
	client := registry.Client("connection-a")
	transport := client.Transport.(*inferenceRoundTripper)
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("response headers failed: %v", err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("body error=%v", err)
	}
	if transport.Generation() != 2 {
		t.Fatalf("transport generation=%d want=2", transport.Generation())
	}
}

func TestPublishInvalidatesCooldownWhenSecretChangesAtSameReference(t *testing.T) {
	led, err := ledger.Open(t.TempDir(), 256, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	manager := runtime.NewManager(runtime.DefaultHealthConfig())
	scheduler := sched.New(manager, led)
	compiler := policy.NewCompiler()
	secret := "old-test-key"
	resolver := &config.Resolver{Store: func(string) (string, bool) { return secret, true }}
	f := &config.File{V2DomainSource: config.V2DomainSourceNative,
		Providers:     []domain.Provider{{ID: "p", Type: domain.ProviderCustom, BaseURL: "https://example.test/v1", Enabled: true, Connections: []domain.ConnectionRef{{ID: "c"}}}},
		Connections:   []domain.Connection{{ID: "c", ProviderID: "p", RuntimeID: "incarnation", Enabled: true, Credential: domain.CredentialRef{SecretRef: "store:key"}, Models: []domain.ConnectionModel{{UpstreamModel: "model", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat}, Capabilities: domain.Capabilities{Text: true}}}}},
		LogicalModels: []domain.LogicalModel{{ID: "model", Enabled: true, DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "p", UpstreamModel: "model"}}}}},
	}
	republish := func() domain.ResourceTargetID {
		t.Helper()
		if err := publish(scheduler, compiler, f, resolver, nil, testInferenceHTTPClient, nil, 1); err != nil {
			t.Fatal(err)
		}
		return scheduler.Snapshot().TargetOrder[0]
	}
	old := republish()
	manager.RecordFailure(old, &domain.Failure{Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Hour}, time.Now())
	if republish() != old || !manager.States(time.Now())[old].CooldownUntil.After(time.Now()) {
		t.Fatal("unchanged publication lost cooldown")
	}
	secret = "new-test-key"
	fresh := republish()
	if fresh == old || manager.States(time.Now())[fresh].CooldownUntil.After(time.Now()) {
		t.Fatal("new key inherited cooldown")
	}
	if f.Connections[0].RuntimeCredential != "" {
		t.Fatal("publication mutated persisted config")
	}
}

func TestPrepareLoadedConfigRebuildsDerivedAndGraphState(t *testing.T) {
	f := &config.File{
		Endpoints: []domain.Endpoint{{ID: "ep", BaseURL: "https://example.com/v1", Protocol: domain.ProtocolOpenAIChat}},
		Plans:     []domain.Plan{{ID: "free", Kind: domain.PlanFree, Currency: "USD"}},
		Targets: []domain.ResourceTarget{{
			ID: "target", EndpointID: "ep", PlanID: "free", Model: "model",
			Credential:   domain.CredentialRef{SecretRef: "env:UPSTREAM_KEY"},
			Capabilities: domain.Capabilities{Text: true},
		}},
		Pools: []domain.Pool{{ID: "pool", Targets: []domain.ResourceTargetID{"target"}}},
		Policies: map[string]config.PolicyYAML{
			"main": {Tiers: []domain.TierDef{{Name: "stale-tier", Pool: "pool"}}},
		},
		PolicyGraphs: map[string]config.PolicyGraphSpec{
			"main": {
				Version: 1,
				Nodes: []config.PolicyGraphNode{
					{ID: "entry", Type: "input"},
					{ID: "channel", Type: "channel", Data: map[string]any{"internalPool": "pool", "tierName": "canonical-tier"}},
					{ID: "done", Type: "end", Data: map[string]any{"outcome": "success"}},
				},
				Edges: []config.PolicyGraphEdge{
					{ID: "entry-channel", Source: "entry", SourcePort: "next", Target: "channel"},
					{ID: "channel-done", Source: "channel", SourcePort: "success", Target: "done"},
				},
			},
		},
		DefaultPolicy: "main",
	}

	dataDir := t.TempDir()
	if err := prepareLoadedConfig(f, dataDir); err != nil {
		t.Fatal(err)
	}
	if f.Server.DataDir != dataDir {
		t.Fatalf("data dir = %q, want %q", f.Server.DataDir, dataDir)
	}
	if f.Targets[0].Endpoint != &f.Endpoints[0] || f.Targets[0].Plan != &f.Plans[0] {
		t.Fatalf("decoded target references were not rebuilt: endpoint=%p plan=%p", f.Targets[0].Endpoint, f.Targets[0].Plan)
	}
	if got := f.Policies["main"].Tiers[0].Name; got != "canonical-tier" {
		t.Fatalf("runtime policy was not canonicalized from graph: tier=%q", got)
	}
}

func TestPrepareLoadedConfigFailsClosedOnInvalidHistory(t *testing.T) {
	f := &config.File{
		Endpoints: []domain.Endpoint{{ID: "ep", BaseURL: "https://example.com/v1", Protocol: domain.ProtocolOpenAIChat}},
		Plans:     []domain.Plan{{ID: "free", Kind: domain.PlanFree, Currency: "USD"}},
		Targets: []domain.ResourceTarget{{
			ID: "target", EndpointID: "missing", PlanID: "free", Model: "model",
			Credential:   domain.CredentialRef{SecretRef: "env:UPSTREAM_KEY"},
			Capabilities: domain.Capabilities{Text: true},
		}},
	}
	if err := prepareLoadedConfig(f, t.TempDir()); err == nil {
		t.Fatal("invalid persisted config must fail validation")
	}
}

func runtimeRevisionConfig(dataDir, upstreamURL string, marker int) *config.File {
	return &config.File{
		Server: config.ServerConfig{DataDir: dataDir, Timezone: "UTC", LogRetentionDays: marker},
		Endpoints: []domain.Endpoint{{
			ID: "upstream", BaseURL: upstreamURL + "/v1", Protocol: domain.ProtocolOpenAIChat,
		}},
		Plans: []domain.Plan{{ID: "free", Kind: domain.PlanFree, Currency: "USD"}},
		Targets: []domain.ResourceTarget{{
			ID: "target", EndpointID: "upstream", Model: "upstream-model", PlanID: "free",
			Credential:   domain.CredentialRef{SecretRef: "env:UPSTREAM_KEY"},
			Capabilities: domain.Capabilities{Text: true},
		}},
		Pools:   []domain.Pool{{ID: "primary", Targets: []domain.ResourceTargetID{"target"}}},
		Aliases: map[string]config.AliasSpec{"public-model": {RequiredCapabilities: map[string]bool{"text": true}}},
		Policies: map[string]config.PolicyYAML{
			"main": {Tiers: []domain.TierDef{{Name: "primary", Pool: "primary"}}},
		},
		DefaultPolicy: "main",
	}
}

func TestRequestRuntimeRevisionTracksDurableConfigurationRevision(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"upstream-model","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer upstream.Close()

	dataDir := t.TempDir()
	controlPath := filepath.Join(dataDir, "control.db")
	control, err := controlstore.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	var revisionN int64
	for marker := 1; marker <= 3; marker++ {
		candidate := runtimeRevisionConfig(dataDir, upstream.URL, marker)
		if err := prepareLoadedConfig(candidate, dataDir); err != nil {
			t.Fatal(err)
		}
		revisionN, err = control.SaveConfig(candidate)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the durable store to model a process restart at revision N.
	control, err = controlstore.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	active, activeRevision, err := control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if activeRevision != revisionN {
		t.Fatalf("restart revision = %d, want %d", activeRevision, revisionN)
	}
	if err := prepareLoadedConfig(active, dataDir); err != nil {
		t.Fatal(err)
	}

	led, err := ledger.Open(dataDir, 256, 0)
	if err != nil {
		t.Fatal(err)
	}
	manager := runtime.NewManager(runtime.DefaultHealthConfig())
	scheduler := sched.New(manager, led)
	compiler := policy.NewCompiler()
	resolver := &config.Resolver{Store: func(name string) (string, bool) {
		return "upstream-secret", name == "UPSTREAM_KEY"
	}}
	authenticators := []domain.APIKeyAuthenticator{{
		Identity:     domain.APIKeyIdentity{ID: "caller", Name: "caller", Enabled: true},
		SecretHashes: [][32]byte{domain.HashAPIKeySecret("client-secret")},
	}}
	issue := func(wantRevision int64) string {
		t.Helper()
		result, failure := scheduler.Execute(context.Background(), &domain.RequestContract{
			Protocol: domain.ProtocolOpenAIChat, Model: "public-model",
			Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}}}},
		}, sched.RequestMeta{PresentedCredential: "client-secret"})
		if failure != nil {
			t.Fatalf("request at revision %d: %v", wantRevision, failure)
		}
		return result.RequestID
	}

	if err := publish(scheduler, compiler, active, resolver, nil, testInferenceHTTPClient, authenticators, activeRevision); err != nil {
		t.Fatal(err)
	}
	restartRequestID := issue(revisionN)

	// OAuth/secret-only publication rebuilds callers but keeps the same
	// durable configuration revision.
	if err := publish(scheduler, compiler, active, resolver, nil, testInferenceHTTPClient, authenticators, activeRevision); err != nil {
		t.Fatal(err)
	}
	republishRequestID := issue(revisionN)

	next := runtimeRevisionConfig(dataDir, upstream.URL, 4)
	if err := prepareLoadedConfig(next, dataDir); err != nil {
		t.Fatal(err)
	}
	persistedRevision, err := control.SaveConfig(next)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRevision != revisionN+1 {
		t.Fatalf("config apply revision = %d, want %d", persistedRevision, revisionN+1)
	}
	if err := publish(scheduler, compiler, next, resolver, nil, testInferenceHTTPClient, authenticators, persistedRevision); err != nil {
		t.Fatal(err)
	}
	applyRequestID := issue(persistedRevision)
	if err := led.Close(); err != nil {
		t.Fatal(err)
	}

	history, err := ledger.Open(dataDir, 256, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	records, _, err := history.QueryRequests(domain.LogFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		restartRequestID: revisionN, republishRequestID: revisionN, applyRequestID: persistedRevision,
	}
	for _, record := range records {
		if expected, ok := want[record.ID]; ok {
			if record.RuntimeRevision != expected {
				t.Fatalf("request %s runtime_revision=%d, want %d", record.ID, record.RuntimeRevision, expected)
			}
			delete(want, record.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing revision request records: %v", want)
	}
}
