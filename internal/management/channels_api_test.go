package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/secretstore"
)

func customChannelFixture() domain.Channel {
	return domain.Channel{
		ID:         "gateway",
		Type:       domain.ChannelCustom,
		BaseURL:    "https://gateway.example.com/v1",
		Credential: domain.CredentialRef{SecretRef: "store:gateway-key"},
		PlanID:     "free",
		Pools:      []string{"p1"},
		Models: []domain.ChannelModel{{
			Model:        "model-a",
			Alias:        "fast",
			Protocols:    []domain.Protocol{domain.ProtocolOpenAIResponses, domain.ProtocolOpenAIChat},
			Capabilities: domain.Capabilities{Text: true, Stream: true},
		}},
	}
}

func TestChannelsListReturnsSanitizedConfiguration(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Channels = []domain.Channel{customChannelFixture()}

	rec, out := f.do(http.MethodGet, "/api/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	items, _ := out["channels"].([]any)
	if len(items) != 1 {
		t.Fatalf("channels = %v, want one", out["channels"])
	}
	ch := items[0].(map[string]any)
	if ch["id"] != "gateway" || ch["type"] != "custom" || ch["credential_ref"] != "store:gateway-key" {
		t.Fatalf("channel view = %v", ch)
	}
	credential, _ := ch["credential"].(map[string]any)
	if credential["secret_ref"] != "store:gateway-key" {
		t.Fatalf("sanitized credential placement missing: %v", ch)
	}
	for _, forbidden := range []string{"secret", "token", "authorization"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), `"`+forbidden+`":"`) {
			t.Fatalf("response exposed sensitive field %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestChannelDetailReturnsOneSanitizedChannel(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Channels = []domain.Channel{customChannelFixture()}

	rec, out := f.do(http.MethodGet, "/api/channels/gateway", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	ch, _ := out["channel"].(map[string]any)
	if ch["id"] != "gateway" || ch["credential_ref"] != "store:gateway-key" {
		t.Fatalf("channel detail = %v", out)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), `"secret":"`) {
		t.Fatalf("detail response leaked credential: %s", rec.Body.String())
	}

	rec, _ = f.do(http.MethodGet, "/api/channels/missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing detail status = %d", rec.Code)
	}
}

func channelCreateBody() map[string]any {
	return map[string]any{
		"id": "gateway", "type": "custom",
		"base_url":   "https://gateway.example.com/v1",
		"credential": map[string]any{"secret_ref": "store:gateway-key"},
		"plan_id":    "free", "pools": []string{"p1"},
		"models": []map[string]any{{
			"model": "model-a", "alias": "fast",
			"protocols":    []string{"openai-responses", "openai-chat"},
			"capabilities": map[string]any{"text": true, "stream": true},
		}},
	}
}

func TestChannelCreatePublishesCandidate(t *testing.T) {
	f := newMgmtFixture(t)

	rec, out := f.do(http.MethodPost, "/api/channels", channelCreateBody())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if f.publishes != 1 || out["published"] != true {
		t.Fatalf("publishes=%d response=%v", f.publishes, out)
	}
	if len(f.srv.deps.Config.Channels) != 1 || f.srv.deps.Config.Channels[0].ID != "gateway" {
		t.Fatalf("live channels = %+v", f.srv.deps.Config.Channels)
	}
	created := f.srv.deps.Config.Channels[0]
	if created.Enabled == nil || *created.Enabled {
		t.Fatalf("new channel must be saved disabled until its toggle is enabled: %+v", created.Enabled)
	}
	if len(f.published) != 1 || len(f.published[0].Channels) != 1 || f.published[0].Channels[0].ID != "gateway" {
		t.Fatalf("published candidate = %+v", f.published)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), `"secret":"`) {
		t.Fatalf("create response leaked secret material: %s", rec.Body.String())
	}
}

func TestChannelCreateRejectsInvalidCandidateWithoutMutation(t *testing.T) {
	f := newMgmtFixture(t)
	before := f.srv.deps.Config
	body := channelCreateBody()
	body["pools"] = []string{"missing-pool"}

	rec, _ := f.do(http.MethodPost, "/api/channels", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if f.publishes != 0 || len(f.srv.deps.Config.Channels) != 0 {
		t.Fatalf("invalid candidate mutated live config: publishes=%d channels=%v", f.publishes, f.srv.deps.Config.Channels)
	}
	if f.srv.deps.Config != before || len(f.published) != 0 {
		t.Fatal("invalid candidate changed the live or published config")
	}
}

func TestChannelCreateKeepsLiveConfigWhenPublishFails(t *testing.T) {
	f := newMgmtFixture(t)
	before := f.srv.deps.Config
	f.pubErr = errors.New("publisher unavailable")

	rec, _ := f.do(http.MethodPost, "/api/channels", channelCreateBody())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.srv.deps.Config.Channels) != 0 {
		t.Fatalf("failed publish changed live config: %+v", f.srv.deps.Config.Channels)
	}
	if f.srv.deps.Config != before || len(f.published) != 0 {
		t.Fatal("failed publish advanced the live or accepted config")
	}
}

func TestChannelPatchIsPartialAndPublishes(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Channels = []domain.Channel{customChannelFixture()}

	rec, out := f.do(http.MethodPatch, "/api/channels/gateway", map[string]any{
		"enabled":  false,
		"base_url": "https://gateway-2.example.com/v1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	ch := f.srv.deps.Config.Channels[0]
	if ch.Enabled == nil || *ch.Enabled || ch.BaseURL != "https://gateway-2.example.com/v1" {
		t.Fatalf("patched channel = %+v", ch)
	}
	if ch.Credential.SecretRef != "store:gateway-key" || len(ch.Models) != 1 {
		t.Fatalf("partial patch discarded existing fields: %+v", ch)
	}
	view := out["channel"].(map[string]any)
	if view["enabled"] != false {
		t.Fatalf("response = %v", out)
	}
	if len(f.published) != 1 || f.published[0].Channels[0].BaseURL != "https://gateway-2.example.com/v1" {
		t.Fatalf("patch not published: %+v", f.published)
	}
}

func TestChannelDeletePublishesCandidate(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Channels = []domain.Channel{customChannelFixture()}

	rec, out := f.do(http.MethodDelete, "/api/channels/gateway", nil)
	if rec.Code != http.StatusOK || out["deleted"] != "gateway" {
		t.Fatalf("status = %d response=%v body=%s", rec.Code, out, rec.Body.String())
	}
	if len(f.srv.deps.Config.Channels) != 0 || f.publishes != 1 {
		t.Fatalf("live config not updated: publishes=%d channels=%v", f.publishes, f.srv.deps.Config.Channels)
	}
	if len(f.published) != 1 || len(f.published[0].Channels) != 0 {
		t.Fatalf("deleted channel remains in published candidate: %+v", f.published)
	}
}

func TestChannelMutationReturnsNotFound(t *testing.T) {
	f := newMgmtFixture(t)
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		rec, _ := f.do(method, "/api/channels/missing", map[string]any{"enabled": false})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d body=%s", method, rec.Code, rec.Body.String())
		}
	}
}

func testableChannel(baseURL string) domain.Channel {
	ch := customChannelFixture()
	ch.BaseURL = baseURL
	ch.Models[0].Protocols = []domain.Protocol{domain.ProtocolOpenAIChat}
	return ch
}

func TestChannelConnectionTestCallsUpstreamWithoutLedgerEntry(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer channel-secret" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["model"] != "model-a" {
			t.Errorf("upstream model = %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	f := newMgmtFixture(t)
	store, err := secretstore.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("gateway-key", "channel-secret"); err != nil {
		t.Fatal(err)
	}
	f.srv.deps.Resolver = &config.Resolver{Store: store.Get}
	f.cfg.Channels = []domain.Channel{testableChannel(upstream.URL + "/v1")}

	rec, out := f.do(http.MethodPost, "/api/channels/gateway/test", map[string]any{
		"model": "model-a", "protocol": "openai-chat",
	})
	if rec.Code != http.StatusOK || out["ok"] != true {
		t.Fatalf("status=%d response=%v body=%s", rec.Code, out, rec.Body.String())
	}
	if calls != 1 || out["latency_ms"] == nil || out["protocol"] != "openai-chat" {
		t.Fatalf("calls=%d response=%v", calls, out)
	}
	if strings.Contains(rec.Body.String(), "channel-secret") {
		t.Fatalf("test response leaked credential: %s", rec.Body.String())
	}
	_, logs := f.do(http.MethodGet, "/api/requests", nil)
	if logs["total"] != float64(0) {
		t.Fatalf("connection test wrote ledger request: %v", logs)
	}
}

func TestChannelConnectionTestClassifiesUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	f := newMgmtFixture(t)
	store, err := secretstore.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("gateway-key", "channel-secret"); err != nil {
		t.Fatal(err)
	}
	f.srv.deps.Resolver = &config.Resolver{Store: store.Get}
	f.cfg.Channels = []domain.Channel{testableChannel(upstream.URL + "/v1")}

	rec, out := f.do(http.MethodPost, "/api/channels/gateway/test", map[string]any{"model": "model-a"})
	if rec.Code != http.StatusOK || out["ok"] != false {
		t.Fatalf("status=%d response=%v body=%s", rec.Code, out, rec.Body.String())
	}
	failure := out["failure"].(map[string]any)
	if failure["type"] != "rate_limit" || failure["http_status"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("failure = %v", failure)
	}
}

func TestChannelMutationIsSafeWithConcurrentConfigReads(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Channels = []domain.Channel{customChannelFixture()}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec, _ := f.do(http.MethodGet, "/api/config/raw?format=json", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("config read status = %d", rec.Code)
			}
		}()
		go func(enabled bool) {
			defer wg.Done()
			rec, _ := f.do(http.MethodPatch, "/api/channels/gateway", map[string]any{"enabled": enabled})
			if rec.Code != http.StatusOK {
				t.Errorf("patch status = %d body=%s", rec.Code, rec.Body.String())
			}
		}(i%2 == 0)
	}
	wg.Wait()
}

func TestSecretRepublishCannotRevertConcurrentConfigMutation(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Channels = []domain.Channel{customChannelFixture()}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	lastChannelCount := -1
	f.srv.deps.Publish = func(cf *config.File) error {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		mu.Lock()
		lastChannelCount = len(cf.Channels)
		mu.Unlock()
		return nil
	}

	republishDone := make(chan struct{})
	go func() {
		defer close(republishDone)
		if msg, ok := f.srv.republish(); !ok {
			t.Errorf("republish failed: %s", msg)
		}
	}()
	<-firstStarted

	deleteDone := make(chan struct{})
	go func() {
		defer close(deleteDone)
		rec, _ := f.do(http.MethodDelete, "/api/channels/gateway", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("delete status = %d body=%s", rec.Code, rec.Body.String())
		}
	}()

	select {
	case <-deleteDone:
		// An unlocked republish lets the newer delete publish first; releasing
		// the old republish afterward exposes the stale-world regression.
		close(releaseFirst)
	case <-time.After(100 * time.Millisecond):
		// A serialized implementation correctly holds the config read lock
		// across republish, so the delete waits and publishes last.
		close(releaseFirst)
		<-deleteDone
	}
	<-republishDone

	mu.Lock()
	got := lastChannelCount
	mu.Unlock()
	if got != 0 {
		t.Fatalf("last published world has %d channels, want deleted channel to stay deleted", got)
	}
}
