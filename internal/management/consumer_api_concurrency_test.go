package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/policy"
	"ferry/internal/runtime"
	sched "ferry/internal/scheduler"
)

func TestConsumerMutationCannotResurrectConcurrentlyDeletedChannel(t *testing.T) {
	cfg := consumerConcurrencyConfig()
	compiler := policy.NewCompiler()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	initial, err := compiler.Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	initial.Version = 1
	manager := runtime.NewManager(runtime.DefaultHealthConfig())
	scheduler := sched.New(manager, nil)
	scheduler.ReplaceWorld(initial, nil, nil, cfg.ConsumerKeys)

	firstPublishStarted := make(chan struct{})
	releaseFirstPublish := make(chan struct{})
	var publishMu sync.Mutex
	publishCalls := 0
	server := New(Deps{
		Scheduler: scheduler,
		Manager:   manager,
		Config:    cfg,
		Compiler:  compiler,
		Publish: func(next *config.File) error {
			publishMu.Lock()
			publishCalls++
			call := publishCalls
			publishMu.Unlock()
			if call == 1 {
				close(firstPublishStarted)
				<-releaseFirstPublish
			}
			snapshot, err := compiler.Compile(next)
			if err != nil {
				return err
			}
			snapshot.Version = int64(call + 1)
			scheduler.ReplaceWorld(snapshot, nil, nil, next.ConsumerKeys)
			return nil
		},
		Timezone: time.UTC,
	})
	handler := server.legacyRoutesForTest()

	consumerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		consumerDone <- managementRequest(handler, http.MethodPost, "/api/consumers", map[string]any{
			"name": "personal",
		})
	}()
	<-firstPublishStarted

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		deleteDone <- managementRequest(handler, http.MethodDelete, "/api/channels/gateway", nil)
	}()

	var deleteResponse *httptest.ResponseRecorder
	select {
	case deleteResponse = <-deleteDone:
		// The buggy implementation lets the channel delete publish first because
		// consumer mutations do not participate in the config transaction lock.
		close(releaseFirstPublish)
	case <-time.After(100 * time.Millisecond):
		// A serialized consumer transaction owns configMu until its publish and
		// pointer swap complete, so the channel delete correctly waits here.
		close(releaseFirstPublish)
		deleteResponse = <-deleteDone
	}
	consumerResponse := <-consumerDone

	if consumerResponse.Code != http.StatusCreated {
		t.Fatalf("consumer create status = %d body=%s", consumerResponse.Code, consumerResponse.Body.String())
	}
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("channel delete status = %d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	final := server.currentConfig()
	if findConsumerIndex(final, "personal") < 0 {
		t.Fatalf("consumer mutation was lost: %+v", final.ConsumerKeys)
	}
	if channelIndex(final, "gateway") >= 0 {
		t.Fatalf("concurrent consumer mutation resurrected deleted channel: %+v", final.Channels)
	}
}

func managementRequest(handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func consumerConcurrencyConfig() *config.File {
	return &config.File{
		Endpoints: []domain.Endpoint{{
			ID: "ep1", BaseURL: "http://127.0.0.1:9", Protocol: domain.ProtocolOpenAIChat,
		}},
		Plans: []domain.Plan{{ID: "free", Kind: domain.PlanFree, Currency: "USD"}},
		Targets: []domain.ResourceTarget{{
			ID: "t1", EndpointID: "ep1", Credential: domain.CredentialRef{SecretRef: "env:CPA_KEY"},
			Model: "m1", PlanID: "free", Capabilities: domain.Capabilities{Text: true, Stream: true},
		}},
		Channels: []domain.Channel{{
			ID: "gateway", Type: domain.ChannelCustom,
			BaseURL:    "https://gateway.example.com/v1",
			Credential: domain.CredentialRef{SecretRef: "env:GATEWAY_KEY"},
			PlanID:     "free", Pools: []string{"p1"},
			Models: []domain.ChannelModel{{
				Model: "model-a", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
				Capabilities: domain.Capabilities{Text: true, Stream: true},
			}},
		}},
		Pools: []domain.Pool{{ID: "p1", Targets: []domain.ResourceTargetID{"t1"}}},
		Policies: map[string]config.PolicyYAML{
			"main": {Tiers: []domain.TierDef{{Name: "primary", Pool: "p1"}}},
		},
		DefaultPolicy: "main",
	}
}
