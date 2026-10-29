package management

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/controlstore"
	"ferry/internal/domain"
)

func TestScheduledProviderModelSyncPublishesCatalogGrowthEndToEnd(t *testing.T) {
	tests := []struct {
		name     string
		interval domain.ProviderModelSyncInterval
		duration time.Duration
	}{
		{name: "thirty_minutes", interval: domain.ProviderModelSync30Minutes, duration: 30 * time.Minute},
		{name: "one_hour", interval: domain.ProviderModelSync1Hour, duration: time.Hour},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newV2APIFixture(t)
			setAcceptanceModelSyncInterval(t, f, test.interval)

			startedAt := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
			var clock atomic.Value
			clock.Store(startedAt)
			var catalogGeneration atomic.Int32
			catalogRequests := make(chan int32, 4)
			transport := modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				generation := catalogGeneration.Load()
				catalogRequests <- generation
				if request.Method != http.MethodGet || request.URL.String() != "https://example.test/v1/models" ||
					request.Header.Get("Authorization") != "Bearer catalog-secret" {
					return modelCatalogResponse(http.StatusBadRequest, `{"error":"unexpected catalog request"}`), nil
				}
				if generation == 0 {
					return modelCatalogResponse(http.StatusOK, `{"data":[{"id":"upstream-model"},{"id":"upstream-model-2"}]}`), nil
				}
				return modelCatalogResponse(http.StatusOK, `{"data":[{"id":"upstream-model"},{"id":"upstream-model-2"},{"id":"scheduled-new-model"}]}`), nil
			})
			configureCustomModelSyncFixture(f, transport, startedAt)
			f.server.deps.Now = func() time.Time { return clock.Load().(time.Time) }

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			// The scheduler's startup scan must read the catalog even though no
			// durable scheduling state exists yet.
			f.server.StartProviderModelSync(ctx)
			first := awaitAcceptanceModelSync(t, f, catalogRequests, 0, startedAt)
			if first.DiscoveredModels != 2 || first.AddedModels != 0 || first.CreatedLogicalModels != 0 {
				t.Fatalf("startup synchronization state=%+v", first)
			}
			firstDue := startedAt.Add(test.duration)
			if first.NextRunAt == nil || !first.NextRunAt.Equal(firstDue) {
				t.Fatalf("startup next run=%v want=%v", first.NextRunAt, firstDue)
			}

			// Growing the upstream catalog alone does not publish anything early.
			catalogGeneration.Store(1)
			beforeDue := firstDue.Add(-time.Second)
			clock.Store(beforeDue)
			f.server.syncDueProviderModels(ctx, beforeDue)
			select {
			case generation := <-catalogRequests:
				t.Fatalf("catalog generation %d synchronized before %s elapsed", generation, test.duration)
			default:
			}

			// At the durable deadline the periodic dispatcher must publish the
			// added channel model and its same-name Logical Model atomically.
			clock.Store(firstDue)
			f.server.syncDueProviderModels(ctx, firstDue)
			second := awaitAcceptanceModelSync(t, f, catalogRequests, 1, firstDue)
			if second.DiscoveredModels != 3 || second.AddedModels != 1 || second.CreatedLogicalModels != 1 {
				t.Fatalf("scheduled synchronization state=%+v", second)
			}
			secondDue := firstDue.Add(test.duration)
			if second.NextRunAt == nil || !second.NextRunAt.Equal(secondDue) {
				t.Fatalf("scheduled next run=%v want=%v", second.NextRunAt, secondDue)
			}

			assertAcceptancePublishedModelSync(t, f, secondDue)
		})
	}
}

func setAcceptanceModelSyncInterval(t *testing.T, f *v2APIFixture, interval domain.ProviderModelSyncInterval) {
	t.Helper()
	f.server.configMu.Lock()
	defer f.server.configMu.Unlock()
	next, err := cloneConfig(f.server.deps.Config)
	if err == nil {
		next.Providers[0].ModelSyncInterval = interval
		err = f.server.publishConfig(next)
	}
	if err != nil {
		t.Fatalf("configure model sync interval: %v", err)
	}
}

func awaitAcceptanceModelSync(t *testing.T, f *v2APIFixture, requests <-chan int32, wantGeneration int32, wantStartedAt time.Time) controlstore.ProviderModelSyncState {
	t.Helper()
	select {
	case generation := <-requests:
		if generation != wantGeneration {
			t.Fatalf("catalog generation=%d want=%d", generation, wantGeneration)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("catalog generation %d was not requested", wantGeneration)
	}

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := f.control.ProviderModelSyncState("provider-a")
		if err != nil {
			t.Fatal(err)
		}
		f.server.modelSyncMu.Lock()
		running := f.server.modelSyncRunning["provider-a"]
		f.server.modelSyncMu.Unlock()
		if state != nil && state.Status == "succeeded" && !running && state.LastStartedAt != nil && state.LastStartedAt.Equal(wantStartedAt) {
			return *state
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("catalog generation %d did not finish: state=%+v running=%v", wantGeneration, state, running)
		}
	}
}

func assertAcceptancePublishedModelSync(t *testing.T, f *v2APIFixture, wantNextRun time.Time) {
	t.Helper()
	snapshot := f.server.deps.Scheduler.Snapshot()
	if snapshot == nil {
		t.Fatal("scheduler snapshot is nil after model synchronization")
	}
	connection := snapshot.Connections["connection-a"]
	if connection == nil || !acceptanceConnectionHasModel(connection.Models, "scheduled-new-model") {
		t.Fatalf("live connection catalog=%+v", connection)
	}
	if catalog := snapshot.ProviderModels[domain.ProviderModelKey("provider-a", "scheduled-new-model")]; catalog == nil || catalog.UpstreamModel != "scheduled-new-model" {
		t.Fatalf("live Provider model=%+v", catalog)
	}
	automatic := snapshot.LogicalModels["scheduled-new-model"]
	if automatic == nil || !automatic.AutoGenerated || len(automatic.DefaultRoute.Steps) != 1 ||
		automatic.DefaultRoute.Steps[0].ProviderID != "provider-a" || automatic.DefaultRoute.Steps[0].UpstreamModel != "scheduled-new-model" {
		t.Fatalf("live automatic Logical Model=%+v", automatic)
	}
	smart := snapshot.LogicalModels["smart"]
	if smart == nil || smart.AutoGenerated || len(smart.DefaultRoute.Steps) != 1 ||
		smart.DefaultRoute.Steps[0].ProviderID != "provider-a" || smart.DefaultRoute.Steps[0].UpstreamModel != "upstream-model" {
		t.Fatalf("custom smart route changed=%+v", smart)
	}

	modelsRec, modelsResponse := f.do(http.MethodGet, "/api/models", nil)
	requireStatus(t, modelsRec, http.StatusOK)
	automaticView := acceptanceModelView(modelsResponse, "scheduled-new-model")
	if automaticView == nil || automaticView["auto_generated"] != true {
		t.Fatalf("management model list missing automatic model: %s", modelsRec.Body.String())
	}
	automaticRoute, _ := automaticView["default_route"].([]any)
	if len(automaticRoute) != 1 || automaticRoute[0].(map[string]any)["provider_id"] != "provider-a" ||
		automaticRoute[0].(map[string]any)["upstream_model"] != "scheduled-new-model" {
		t.Fatalf("management automatic route=%v", automaticView["default_route"])
	}
	smartView := acceptanceModelView(modelsResponse, "smart")
	smartRoute, _ := smartView["default_route"].([]any)
	if smartView == nil || smartView["auto_generated"] == true || len(smartRoute) != 1 ||
		smartRoute[0].(map[string]any)["upstream_model"] != "upstream-model" {
		t.Fatalf("management smart route changed=%v", smartView)
	}

	providerRec, providerResponse := f.do(http.MethodGet, "/api/providers/provider-a", nil)
	requireStatus(t, providerRec, http.StatusOK)
	modelSync, _ := providerResponse["model_sync"].(map[string]any)
	if modelSync == nil || modelSync["status"] != "succeeded" || modelSync["added_models"] != float64(1) ||
		modelSync["created_logical_models"] != float64(1) || modelSync["next_run_at"] != wantNextRun.Format(time.RFC3339) {
		t.Fatalf("management Provider sync view=%v", modelSync)
	}
}

func acceptanceConnectionHasModel(models []domain.ConnectionModel, want string) bool {
	for _, model := range models {
		if model.UpstreamModel == want {
			return true
		}
	}
	return false
}

func acceptanceModelView(response map[string]any, id string) map[string]any {
	models, _ := response["models"].([]any)
	for _, raw := range models {
		model, _ := raw.(map[string]any)
		if model["id"] == id {
			return model
		}
	}
	return nil
}
