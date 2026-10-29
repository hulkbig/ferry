package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/ledger"
	"ferry/internal/nativeauth"
	"ferry/internal/policy"
	"ferry/internal/runtime"
	sched "ferry/internal/scheduler"
	"ferry/internal/secretstore"
)

type publishNativeDriver struct{ endpoint string }

func (d *publishNativeDriver) Info() nativeauth.Info {
	return nativeauth.Info{ID: "vertex", Mode: "import", Protocol: "openai-chat"}
}
func (d *publishNativeDriver) Begin(context.Context, *http.Client, nativeauth.LoginOptions) (nativeauth.Session, error) {
	return nativeauth.Session{ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (d *publishNativeDriver) Complete(context.Context, *http.Client, nativeauth.Session, string) (nativeauth.Token, error) {
	return nativeauth.Token{AccessToken: "fixture-token", Identity: d.endpoint, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (d *publishNativeDriver) Refresh(_ context.Context, _ *http.Client, t nativeauth.Token) (nativeauth.Token, error) {
	return t, nil
}
func (d *publishNativeDriver) Binding(t nativeauth.Token) (string, string, error) {
	return t.Identity, "openai-chat", nil
}

func TestNativePublishRebindingIsolatesAvailabilityAndKeepsStartupWorking(t *testing.T) {
	led, err := ledger.Open(t.TempDir(), 256, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "https://aiplatform.googleapis.com/v1/projects/project-one/locations/global/endpoints/openapi"
	driver := &publishNativeDriver{endpoint: endpoint}
	native := nativeauth.NewManager(store, driver)
	login := func() {
		t.Helper()
		if _, err := native.Begin(context.Background(), "vertex", "account", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := native.Complete(context.Background(), "vertex", "account", ""); err != nil {
			t.Fatal(err)
		}
	}
	login()
	manager := runtime.NewManager(runtime.DefaultHealthConfig())
	scheduler := sched.New(manager, led)
	resolver := &config.Resolver{NativeOAuth: native}
	f := &config.File{V2DomainSource: config.V2DomainSourceNative,
		Providers:     []domain.Provider{{ID: "p", Type: domain.ProviderCustom, BaseURL: endpoint, Enabled: true, Connections: []domain.ConnectionRef{{ID: "c"}}}},
		Connections:   []domain.Connection{{ID: "c", ProviderID: "p", RuntimeID: "incarnation", Enabled: true, Credential: domain.CredentialRef{SecretRef: nativeauth.Ref("vertex", "account")}, Models: []domain.ConnectionModel{{UpstreamModel: "model", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat}, Capabilities: domain.Capabilities{Text: true}}}}},
		LogicalModels: []domain.LogicalModel{{ID: "model", Enabled: true, DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "p", UpstreamModel: "model"}}}}}}
	republish := func() domain.ResourceTargetID {
		t.Helper()
		if err := publish(scheduler, policy.NewCompiler(), f, resolver, nil, testInferenceHTTPClient, nil, 1); err != nil {
			t.Fatal(err)
		}
		return scheduler.Snapshot().TargetOrder[0]
	}
	old := republish()
	if !scheduler.HasCaller(old) {
		t.Fatal("connected target unavailable")
	}
	manager.RecordFailure(old, &domain.Failure{Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Hour}, time.Now())
	if republish() != old || !manager.States(time.Now())[old].CooldownUntil.After(time.Now()) {
		t.Fatal("unchanged account lost state")
	}
	if _, err := native.Identity(context.Background(), nativeauth.Ref("vertex", "account"), "fixture-token"); err != nil {
		t.Fatal(err)
	}
	if republish() != old {
		t.Fatal("token refresh changed account identity")
	}
	login()
	fresh := republish()
	if fresh == old || manager.States(time.Now())[fresh].CooldownUntil.After(time.Now()) {
		t.Fatal("new login inherited old account cooldown")
	}
	driver.endpoint = "https://aiplatform.googleapis.com/v1/projects/project-two/locations/global/endpoints/openapi"
	login()
	mismatched := republish()
	if scheduler.HasCaller(mismatched) {
		t.Fatal("mismatched account endpoint has a caller")
	}
	f.Providers[0].BaseURL = driver.endpoint
	if id := republish(); !scheduler.HasCaller(id) {
		t.Fatal("corrected endpoint remains unavailable")
	}
	if f.Connections[0].RuntimeCredential != "" {
		t.Fatal("runtime identity leaked into stored config")
	}
}
