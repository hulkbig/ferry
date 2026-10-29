package management

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"ferry/internal/controlstore"
	"ferry/internal/domain"
)

func requireFirstOutputTimeout(t *testing.T, model map[string]any, want int) {
	t.Helper()
	if got, present := model["first_output_timeout_seconds"]; !present || got != float64(want) {
		t.Fatalf("model timeout readback=%v present=%v want=%d", got, present, want)
	}
}

func timeoutIntentModel(t *testing.T, response map[string]any, id string) map[string]any {
	t.Helper()
	intent, _ := response["intent"].(map[string]any)
	models, _ := intent["logical_models"].([]any)
	for _, raw := range models {
		model, _ := raw.(map[string]any)
		if model["id"] == id {
			return model
		}
	}
	t.Fatalf("intent omitted model %s: %v", id, response)
	return nil
}

func applyTimeoutDraft(t *testing.T, f *v2APIFixture, id string, revision int64) {
	t.Helper()
	rec, validation := f.do(http.MethodPost, "/api/drafts/"+id+"/validate", nil)
	requireStatus(t, rec, http.StatusOK)
	if validation["valid"] != true {
		t.Fatalf("invalid draft: %v", validation)
	}
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+id+"/apply", map[string]any{"expected_revision": revision})
	requireStatus(t, rec, http.StatusOK)
}

func TestFirstOutputTimeoutCreateDraftPersistenceAndZeroReadback(t *testing.T) {
	for _, seconds := range []int{0, 1, 7200} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			f := newV2APIFixture(t)
			rec, detail := f.do(http.MethodGet, "/api/models/smart", nil)
			requireStatus(t, rec, http.StatusOK)
			requireFirstOutputTimeout(t, detail, 0)
			rec, created := f.do(http.MethodPost, "/api/models", map[string]any{
				"id": "bounded", "display_name": "Bounded", "enabled": false,
				"first_output_timeout_seconds": seconds, "default_route": map[string]any{"steps": []any{}},
			})
			requireStatus(t, rec, http.StatusAccepted)
			requireFirstOutputTimeout(t, created["model"].(map[string]any), seconds)
			draftID := created["draft_id"].(string)
			rec, draft := f.do(http.MethodGet, "/api/drafts/"+draftID, nil)
			requireStatus(t, rec, http.StatusOK)
			requireFirstOutputTimeout(t, timeoutIntentModel(t, draft, "smart"), 0)
			requireFirstOutputTimeout(t, timeoutIntentModel(t, draft, "bounded"), seconds)
			// Readback is a usable intent payload, including explicit zero values.
			rec, roundtrip := f.do(http.MethodPatch, "/api/drafts/"+draftID, draft["intent"])
			requireStatus(t, rec, http.StatusOK)
			requireFirstOutputTimeout(t, timeoutIntentModel(t, roundtrip, "bounded"), seconds)
			applyTimeoutDraft(t, f, draftID, 1)
			rec, detail = f.do(http.MethodGet, "/api/models/bounded", nil)
			requireStatus(t, rec, http.StatusOK)
			requireFirstOutputTimeout(t, detail, seconds)
			rec, revision := f.do(http.MethodGet, "/api/revisions/2", nil)
			requireStatus(t, rec, http.StatusOK)
			requireFirstOutputTimeout(t, timeoutIntentModel(t, revision, "bounded"), seconds)

			// Open an independent SQLite connection to verify durable state.
			reopened, err := controlstore.Open(f.controlPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			persisted, version, err := reopened.ActiveConfig()
			if err != nil {
				t.Fatal(err)
			}
			at := logicalModelIndex(persisted, "bounded")
			if at < 0 || version != 2 || persisted.LogicalModels[at].FirstOutputTimeoutSeconds != seconds {
				t.Fatalf("durable model missing: revision=%d models=%+v", version, persisted.LogicalModels)
			}
		})
	}
}

func TestFirstOutputTimeoutPatchRoutingCASAndUnrelatedFields(t *testing.T) {
	f := newV2APIFixture(t)
	patch := map[string]any{
		"first_output_timeout_seconds": 180, "display_name": "Keep name", "description": "Keep description", "exact_only": true,
		"overrides": []domain.OverrideSpec{{ID: "keep-rule", Mode: domain.OverrideCustomRoute,
			Route: &domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-model-2"}}}}},
	}
	rec, mutation := f.do(http.MethodPatch, "/api/models/smart", patch)
	requireStatus(t, rec, http.StatusAccepted)
	applyTimeoutDraft(t, f, mutation["draft_id"].(string), 1)
	route := map[string]any{
		"routes":                []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-model-2"}},
		"conversation_strategy": "migrate",
	}
	rec, staleReview := f.do(http.MethodPost, "/api/models/smart/routing/review", route)
	requireStatus(t, rec, http.StatusOK)
	requireFirstOutputTimeout(t, staleReview, 180)
	rec, stalePatch := f.do(http.MethodPatch, "/api/models/smart", map[string]any{"first_output_timeout_seconds": 90})
	requireStatus(t, rec, http.StatusAccepted)
	// A metadata-only edit must neither reset the timeout nor be overwritten by
	// a concurrent timeout/routing draft based on the preceding revision.
	rec, rename := f.do(http.MethodPatch, "/api/models/smart", map[string]any{"display_name": "Renamed"})
	requireStatus(t, rec, http.StatusAccepted)
	applyTimeoutDraft(t, f, rename["draft_id"].(string), 2)
	rec, _ = f.do(http.MethodPost, "/api/models/smart/routing/apply", map[string]any{
		"draft_id": staleReview["draft_id"], "expected_revision": 2,
	})
	requireStatus(t, rec, http.StatusConflict)
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+stalePatch["draft_id"].(string)+"/apply", map[string]any{"expected_revision": 2})
	requireStatus(t, rec, http.StatusConflict)
	rec, detail := f.do(http.MethodGet, "/api/models/smart", nil)
	requireStatus(t, rec, http.StatusOK)
	requireFirstOutputTimeout(t, detail, 180)
	if detail["display_name"] != "Renamed" || detail["description"] != "Keep description" || detail["exact_only"] != true {
		t.Fatalf("CAS conflict overwrote model metadata: %v", detail)
	}
	rec, review := f.do(http.MethodPost, "/api/models/smart/routing/review", route)
	requireStatus(t, rec, http.StatusOK)
	requireFirstOutputTimeout(t, review, 180)
	rec, _ = f.do(http.MethodPost, "/api/models/smart/routing/apply", map[string]any{
		"draft_id": review["draft_id"], "expected_revision": 3,
	})
	requireStatus(t, rec, http.StatusOK)
	before, _, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	at := logicalModelIndex(before, "smart")
	if before.LogicalModels[at].FirstOutputTimeoutSeconds != 180 || before.LogicalModels[at].DefaultRoute.Steps[0].UpstreamModel != "upstream-model-2" {
		t.Fatalf("routing roundtrip lost timeout/route: %+v", before.LogicalModels[at])
	}
	rec, clear := f.do(http.MethodPatch, "/api/models/smart", map[string]any{"first_output_timeout_seconds": 0})
	requireStatus(t, rec, http.StatusAccepted)
	rec, draft := f.do(http.MethodGet, "/api/drafts/"+clear["draft_id"].(string), nil)
	requireStatus(t, rec, http.StatusOK)
	requireFirstOutputTimeout(t, timeoutIntentModel(t, draft, "smart"), 0)
	applyTimeoutDraft(t, f, clear["draft_id"].(string), 4)
	after, version, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	before.LogicalModels[at].FirstOutputTimeoutSeconds = 0
	if version != 5 || !reflect.DeepEqual(before.LogicalModels, after.LogicalModels) ||
		!reflect.DeepEqual(before.Providers, after.Providers) || !reflect.DeepEqual(before.Connections, after.Connections) {
		t.Fatalf("clearing timeout changed unrelated configuration: before=%+v after=%+v", before.LogicalModels, after.LogicalModels)
	}
	rec, detail = f.do(http.MethodGet, "/api/models/smart", nil)
	requireStatus(t, rec, http.StatusOK)
	requireFirstOutputTimeout(t, detail, 0)

	// The optional routing field follows the same explicit-zero semantics.
	for i, seconds := range []int{1, 0} {
		route["first_output_timeout_seconds"] = seconds
		rec, review = f.do(http.MethodPost, "/api/models/smart/routing/review", route)
		requireStatus(t, rec, http.StatusOK)
		requireFirstOutputTimeout(t, review, seconds)
		applyTimeoutDraft(t, f, review["draft_id"].(string), int64(5+i))
	}
}

func TestFirstOutputTimeoutInvalidInputsCannotPublish(t *testing.T) {
	f := newV2APIFixture(t)
	for _, value := range []any{-1, 7201, 1.5, "180", true} {
		status := http.StatusBadRequest
		if _, ok := value.(int); ok {
			status = http.StatusUnprocessableEntity
		}
		for _, surface := range []struct{ method, path string }{
			{http.MethodPost, "/api/models"}, {http.MethodPatch, "/api/models/smart"},
			{http.MethodPost, "/api/models/smart/routing/review"},
		} {
			body := map[string]any{"first_output_timeout_seconds": value}
			if surface.path == "/api/models" {
				body["id"] = "invalid"
			}
			if strings.HasSuffix(surface.path, "/review") {
				body["routes"] = []domain.RouteStep{{ProviderID: "provider-a", UpstreamModel: "upstream-model"}}
			}
			rec, _ := f.do(surface.method, surface.path, body)
			requireStatus(t, rec, status)
		}
	}
	// Raw intent drafts can be edited while invalid, but validation and Apply
	// must reject their out-of-range value without changing the active revision.
	for _, seconds := range []int{-1, 7201} {
		rec, draft := f.do(http.MethodPost, "/api/drafts", map[string]any{
			"base_revision": 1, "intent": map[string]any{"logical_models": []domain.LogicalModel{{ID: "invalid", FirstOutputTimeoutSeconds: seconds}}},
		})
		requireStatus(t, rec, http.StatusCreated)
		id := draft["id"].(string)
		rec, validation := f.do(http.MethodPost, "/api/drafts/"+id+"/validate", nil)
		requireStatus(t, rec, http.StatusOK)
		if validation["valid"] != false || !strings.Contains(rec.Body.String(), "first_output_timeout_seconds") {
			t.Fatalf("invalid draft timeout passed validation: %s", rec.Body.String())
		}
		rec, _ = f.do(http.MethodPost, "/api/drafts/"+id+"/apply", map[string]any{"expected_revision": 1})
		requireStatus(t, rec, http.StatusUnprocessableEntity)
	}
	_, revision, err := f.control.ActiveConfig()
	if err != nil || revision != 1 {
		t.Fatalf("invalid input changed revision: %d err=%v", revision, err)
	}
}

func TestFirstOutputTimeoutSurvivesModelSyncAndClone(t *testing.T) {
	f := newV2APIFixture(t)
	next, err := cloneConfig(f.server.deps.Config)
	if err != nil {
		t.Fatal(err)
	}
	for i := range next.LogicalModels {
		next.LogicalModels[i].FirstOutputTimeoutSeconds = 180
	}
	if err := f.server.publishConfig(next); err != nil {
		t.Fatal(err)
	}
	configureCustomModelSyncFixture(f, modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelCatalogResponse(http.StatusOK, `{"data":[{"id":"upstream-model"},{"id":"new-model"}]}`), nil
	}), time.Now())
	rec, result := f.do(http.MethodPost, "/api/providers/provider-a/models/sync", nil)
	requireStatus(t, rec, http.StatusOK)
	if result["added_models"] != float64(1) {
		t.Fatalf("catalog fixture did not add a model: %v", result)
	}
	active, _, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := cloneConfig(active)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"smart", "upstream-model", "upstream-model-2", "new-model"} {
		want := 180
		if id == "new-model" {
			want = 0
		}
		at := logicalModelIndex(cloned, id)
		if at < 0 || cloned.LogicalModels[at].FirstOutputTimeoutSeconds != want {
			t.Fatalf("sync/clone changed timeout for %s: %+v", id, cloned.LogicalModels)
		}
	}
}
