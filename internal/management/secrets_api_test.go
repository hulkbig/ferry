package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/nativeauth"
	"ferry/internal/secretstore"
)

// secretsAPITest builds a fixture whose Deps.Store is wired to a real
// secretstore backed by a temp file, mirroring cmd/ferry main().
func secretsAPITest(t *testing.T) *mgmtFixture {
	t.Helper()
	f := newMgmtFixture(t)
	st, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}
	f.srv = New(Deps{
		Scheduler: f.sch,
		Manager:   f.mgr,
		Ledger:    f.led,
		Config:    f.cfg,
		Compiler:  nil, // not exercised by the secrets endpoints
		Store:     st,
		Publish:   f.srv.deps.Publish, // reuse the counting fake publisher
		Timezone:  nil,
	})
	return f
}

func TestSecretsDisabledWithoutStore(t *testing.T) {
	f := newMgmtFixture(t) // no Store wired
	rec, out := f.do(http.MethodGet, "/api/secrets", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/secrets code=%d body=%v, want 503", rec.Code, out)
	}
	rec, _ = f.do(http.MethodPost, "/api/secrets/k", map[string]string{"value": "v"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/secrets/k code=%d, want 503", rec.Code)
	}
}

func TestSecretLifecycleAndNeverEchoed(t *testing.T) {
	f := secretsAPITest(t)

	// empty list first
	rec, out := f.do(http.MethodGet, "/api/secrets", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "CPA_KEY") {
		t.Fatalf("initial list code=%d body=%s", rec.Code, rec.Body.String())
	}
	if items := out["secrets"].([]any); len(items) != 0 {
		t.Fatalf("expected empty secrets list, got %v", items)
	}

	// provision the key for t1 (env:CPA_KEY → store entry CPA_KEY); publish fires
	before := f.publishes
	rec, out = f.do(http.MethodPost, "/api/targets/t1/set-secret",
		map[string]string{"value": "sk-live-123"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-secret code=%d body=%v", rec.Code, out)
	}
	if out["stored"] != "CPA_KEY" || out["target"] != "t1" {
		t.Fatalf("set-secret resp = %v", out)
	}
	if f.publishes != before+1 {
		t.Fatalf("republish not triggered: %d -> %d", before, f.publishes)
	}

	// the value must never appear in any listing
	rec, out = f.do(http.MethodGet, "/api/secrets", nil)
	if strings.Contains(rec.Body.String(), "sk-live-123") {
		t.Fatalf("secret value leaked via /api/secrets: %s", rec.Body.String())
	}
	items := out["secrets"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 stored secret, got %v", items)
	}
	first := items[0].(map[string]any)
	if first["name"] != "CPA_KEY" {
		t.Fatalf("name = %v", first["name"])
	}
	targets := fmt.Sprint(first["targets"])
	if !strings.Contains(targets, "t1") {
		t.Fatalf("usage targets missing t1: %v", targets)
	}

	// per-target status: resolvable now, name surfaced, never the value
	rec, out = f.do(http.MethodGet, "/api/targets/t1/credential", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("credential status code=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk-live-123") {
		t.Fatalf("secret value leaked via credential status: %s", rec.Body.String())
	}
	if out["resolvable"] != true || out["secret_name"] != "CPA_KEY" ||
		out["provisionable_here"] != true {
		t.Fatalf("credential status = %v", out)
	}

	// unknown target → 404
	rec, _ = f.do(http.MethodPost, "/api/targets/nope/set-secret",
		map[string]string{"value": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target code=%d, want 404", rec.Code)
	}

	// bad name / empty value → 400
	rec, _ = f.do(http.MethodPost, "/api/secrets/9bad", map[string]string{"value": "v"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name code=%d, want 400", rec.Code)
	}
	rec, _ = f.do(http.MethodPost, "/api/secrets/good", map[string]string{"value": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty value code=%d, want 400", rec.Code)
	}

	// delete republishes and empties the store; afterwards the target reports
	// unresolvable again
	before = f.publishes
	rec, out = f.do(http.MethodDelete, "/api/secrets/CPA_KEY", nil)
	if rec.Code != http.StatusOK || out["deleted"] != "CPA_KEY" {
		t.Fatalf("delete code=%d out=%v", rec.Code, out)
	}
	if f.publishes != before+1 {
		t.Fatalf("delete did not republish (%d -> %d)", before, f.publishes)
	}
	rec, out = f.do(http.MethodGet, "/api/targets/t1/credential", nil)
	if out["resolvable"] != false {
		t.Fatalf("after delete resolvable = %v, want false", out["resolvable"])
	}

	// deleting an absent name still succeeds (idempotent)
	rec, _ = f.do(http.MethodDelete, "/api/secrets/CPA_KEY", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent delete code=%d", rec.Code)
	}
}

func TestSetSecretRepublishFailureSurfacesWarning(t *testing.T) {
	f := secretsAPITest(t)
	f.pubErr = fmt.Errorf("boom")
	rec, out := f.do(http.MethodPost, "/api/targets/t1/set-secret",
		map[string]string{"value": "sk-x"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("code=%d, want 207", rec.Code)
	}
	if w, _ := out["warning"].(string); !strings.Contains(w, "boom") {
		t.Fatalf("warning missing failure detail: %v", out)
	}
}

func TestConcurrentSecretSetDeleteRepublishConvergesToStore(t *testing.T) {
	tests := []struct {
		name          string
		seedValue     string
		firstMethod   string
		firstBody     any
		secondMethod  string
		secondBody    any
		finalValue    string
		finalResolved bool
	}{
		{
			name:        "set then delete",
			firstMethod: http.MethodPost, firstBody: map[string]string{"value": "sk-first"},
			secondMethod:  http.MethodDelete,
			finalResolved: false,
		},
		{
			name:      "delete then set",
			seedValue: "sk-old", firstMethod: http.MethodDelete,
			secondMethod: http.MethodPost, secondBody: map[string]string{"value": "sk-final"},
			finalValue: "sk-final", finalResolved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := secretsAPITest(t)
			store := f.srv.deps.Store
			if tt.seedValue != "" {
				if err := store.Set("CPA_KEY", tt.seedValue); err != nil {
					t.Fatalf("seed secret: %v", err)
				}
			}

			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			var publishCalls atomic.Int32
			var observedMu sync.Mutex
			var lastValue string
			var lastResolved bool

			// Capture the store value at publish entry, just as the production
			// publisher resolves credentials before constructing a new scheduler
			// world. Holding the first call lets the second mutation race with it
			// deterministically. Without exclusive republish serialization, the
			// older captured world can finish last and disagree with the store.
			f.srv.deps.Publish = func(_ *config.File) error {
				value, resolved := store.Get("CPA_KEY")
				if publishCalls.Add(1) == 1 {
					close(firstStarted)
					<-releaseFirst
				}

				var callers map[domain.ResourceTargetID]egress.Caller
				if resolved {
					callers = map[domain.ResourceTargetID]egress.Caller{"t1": stubCaller{}}
				}
				f.sch.ReplaceWorld(fixtureSnapshot(), callers, nil, nil)
				observedMu.Lock()
				lastValue, lastResolved = value, resolved
				observedMu.Unlock()
				return nil
			}

			firstDone := make(chan int, 1)
			go func() {
				rec, _ := f.do(tt.firstMethod, "/api/secrets/CPA_KEY", tt.firstBody)
				firstDone <- rec.Code
			}()
			<-firstStarted

			secondDone := make(chan int, 1)
			go func() {
				rec, _ := f.do(tt.secondMethod, "/api/secrets/CPA_KEY", tt.secondBody)
				secondDone <- rec.Code
			}()

			deadline := time.Now().Add(2 * time.Second)
			for {
				value, resolved := store.Get("CPA_KEY")
				if value == tt.finalValue && resolved == tt.finalResolved {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("second mutation did not reach store: value=%q resolved=%v", value, resolved)
				}
				time.Sleep(time.Millisecond)
			}

			close(releaseFirst)
			if code := <-firstDone; code != http.StatusOK {
				t.Fatalf("first mutation status = %d, want 200", code)
			}
			if code := <-secondDone; code != http.StatusOK {
				t.Fatalf("second mutation status = %d, want 200", code)
			}
			if got := publishCalls.Load(); got != 2 {
				t.Fatalf("publish calls = %d, want 2", got)
			}

			storeValue, storeResolved := store.Get("CPA_KEY")
			observedMu.Lock()
			publishedValue, publishedResolved := lastValue, lastResolved
			observedMu.Unlock()
			if publishedValue != storeValue || publishedResolved != storeResolved {
				t.Fatalf("last published credential = (%q,%v), store = (%q,%v)",
					publishedValue, publishedResolved, storeValue, storeResolved)
			}
			if live := f.sch.HasCaller("t1"); live != storeResolved {
				t.Fatalf("scheduler caller live = %v, store resolved = %v", live, storeResolved)
			}
		})
	}
}

func TestTargetCredentialNonProvisionableRefs(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Targets[0].Credential.SecretRef = "file:/etc/ferry/key"

	rec, out := f.do(http.MethodGet, "/api/targets/t1/credential", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if out["provisionable_here"] != false {
		t.Fatalf("file ref marked provisionable: %v", out)
	}
	if n, _ := out["secret_name"].(string); n != "" {
		t.Fatalf("file ref got a store name: %v", out["secret_name"])
	}

	rec, out = f.do(http.MethodPost, "/api/targets/t1/set-secret",
		map[string]string{"value": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("set-secret on file ref code=%d, want 400", rec.Code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "cannot be provisioned") {
		t.Fatalf("error = %v", out)
	}
}

func TestChannelExpandedTargetSecretLifecycle(t *testing.T) {
	f := secretsAPITest(t)
	f.cfg.Channels = []domain.Channel{customChannelFixture()}
	id := "gateway.model-a"

	rec, out := f.do(http.MethodPost, "/api/targets/"+id+"/set-secret",
		map[string]string{"value": "sk-channel-live"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-secret code=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["stored"] != "gateway-key" || out["target"] != id {
		t.Fatalf("set-secret response = %v", out)
	}

	rec, out = f.do(http.MethodGet, "/api/targets/"+id+"/credential", nil)
	if rec.Code != http.StatusOK || out["resolvable"] != true || out["secret_name"] != "gateway-key" {
		t.Fatalf("credential status code=%d response=%v", rec.Code, out)
	}
	if strings.Contains(rec.Body.String(), "sk-channel-live") {
		t.Fatalf("credential status leaked channel key: %s", rec.Body.String())
	}

	rec, out = f.do(http.MethodGet, "/api/secrets", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret list code=%d", rec.Code)
	}
	items := out["secrets"].([]any)
	if len(items) != 1 || !strings.Contains(fmt.Sprint(items[0].(map[string]any)["targets"]), id) {
		t.Fatalf("channel target missing from secret usage: %v", items)
	}
	if strings.Contains(rec.Body.String(), "sk-channel-live") {
		t.Fatalf("secret list leaked channel key: %s", rec.Body.String())
	}
}

func TestTargetNativeCredentialReadinessUsesConnectedStatus(t *testing.T) {
	for _, withStore := range []bool{false, true} {
		t.Run(fmt.Sprintf("management-store=%t", withStore), func(t *testing.T) {
			f := secretsAPITest(t)
			store := f.srv.deps.Store
			driver := &offlineNativeDriver{}
			manager := nativeauth.NewManager(store, driver)
			f.srv.deps.NativeOAuth = manager
			if !withStore {
				f.srv.deps.Store = nil
			}
			ref := nativeauth.Ref("offline", "readiness")
			f.cfg.Targets[0].Credential.SecretRef = ref
			check := func(want bool) {
				t.Helper()
				before, _ := store.Get(ref)
				begins, completes := driver.begins, driver.completes
				rec, out := f.do(http.MethodGet, "/api/targets/"+string(f.cfg.Targets[0].ID)+"/credential", nil)
				if rec.Code != http.StatusOK || out["resolvable"] != want || out["provisionable_here"] != false {
					t.Fatalf("credential readiness: status=%d body=%s", rec.Code, rec.Body)
				}
				for _, secret := range []string{"private-access", "private-refresh", "private-verifier", "native-managed:"} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Fatalf("readiness exposed %s", secret)
					}
				}
				after, _ := store.Get(ref)
				if before != after || begins != driver.begins || completes != driver.completes {
					t.Fatal("readiness mutated or polled account")
				}
			}
			check(false) // A configured resolver alone is not a connected account.
			if _, err := manager.Begin(context.Background(), "offline", "readiness", nil); err != nil {
				t.Fatal(err)
			}
			check(false) // Pending login is not ready either.
			if _, err := manager.Complete(context.Background(), "offline", "readiness", "callback"); err != nil {
				t.Fatal(err)
			}
			check(true)
			f.srv.deps.NativeOAuth = nil
			check(false)
			f.srv.deps.NativeOAuth = manager
			if err := manager.Disconnect("offline", "readiness"); err != nil {
				t.Fatal(err)
			}
			check(false)
		})
	}
}
