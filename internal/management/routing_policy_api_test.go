package management

import (
	"fmt"
	"net/http"
	"testing"

	"ferry/internal/domain"
)

type fakePolicyHistory struct {
	revisions map[string][]domain.RoutingPolicyRevision
}

func (f fakePolicyHistory) PolicyRevisions(id string) ([]domain.RoutingPolicyRevision, error) {
	return f.revisions[id], nil
}

func (f fakePolicyHistory) PolicyRevision(id string, revision int64) (*domain.RoutingPolicyRevision, error) {
	for _, candidate := range f.revisions[id] {
		if candidate.Revision == revision {
			copy := candidate
			return &copy, nil
		}
	}
	return nil, fmt.Errorf("revision not found")
}

func TestRoutingPolicyCRUDSeparatesSaveToggleAndReorder(t *testing.T) {
	f := newMgmtFixture(t)

	create := map[string]any{
		"id": "glm-afternoon", "name": "工作日下午避开 GLM",
		"match": map[string]any{"models": []string{"main"}},
		"route": map[string]any{"steps": []map[string]any{{"channel_group": "p1"}}},
	}
	rec, out := f.do(http.MethodPost, "/api/policies", create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	created := out["policy"].(map[string]any)
	if created["enabled"] != false {
		t.Fatalf("new policy must be saved disabled: %v", created)
	}
	if len(f.srv.deps.Config.RoutingPolicies) != 2 {
		t.Fatalf("legacy policy was not migrated before create: %#v", f.srv.deps.Config.RoutingPolicies)
	}

	// Move the specific rule before the mandatory catch-all, then enable it.
	rec, _ = f.do(http.MethodPost, "/api/policies/reorder", map[string]any{"ids": []string{"glm-afternoon", "main"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, out = f.do(http.MethodPatch, "/api/policies/glm-afternoon/enabled", map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["policy"].(map[string]any)["enabled"] != true {
		t.Fatalf("toggle response=%v", out)
	}

	// Saving content does not accept enabled/priority fields; those actions
	// have dedicated endpoints and therefore independent audit semantics.
	rec, _ = f.do(http.MethodPatch, "/api/policies/glm-afternoon", map[string]any{
		"name": "renamed", "enabled": false,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("content save accepted enabled field: %d %s", rec.Code, rec.Body.String())
	}

	rec, out = f.do(http.MethodGet, "/api/policies", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	policies := out["policies"].([]any)
	if len(policies) != 2 || policies[0].(map[string]any)["id"] != "glm-afternoon" || policies[1].(map[string]any)["id"] != "main" {
		t.Fatalf("ordered policies=%v", policies)
	}
}

func TestRoutingPolicyCannotActivateWithoutFinalCatchAll(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Policies = nil
	f.cfg.DefaultPolicy = ""
	f.cfg.RoutingPolicies = []domain.RoutingPolicySpec{{
		ID: "specific", Priority: 10, Enabled: false,
		Match: domain.RoutingPolicyMatch{Models: []string{"main"}},
		Route: domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: "p1"}}},
	}}

	rec, _ := f.do(http.MethodPatch, "/api/policies/specific/enabled", map[string]any{"enabled": true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("activation without catch-all status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRoutingPolicyCannotDisableOrDeleteTheOnlyCatchAll(t *testing.T) {
	f := newMgmtFixture(t)
	f.cfg.Policies = nil
	f.cfg.DefaultPolicy = ""
	f.cfg.RoutingPolicies = []domain.RoutingPolicySpec{{
		ID: "catch-all", Name: "Fallback", Priority: 10, Enabled: true,
		Route: routeGroupAPI("p1"),
	}}

	rec, _ := f.do(http.MethodPatch, "/api/policies/catch-all/enabled", map[string]any{"enabled": false})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disable only catch-all status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = f.do(http.MethodDelete, "/api/policies/catch-all", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delete only catch-all status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRoutingPolicyRevisionListAndRollback(t *testing.T) {
	f := newMgmtFixture(t)
	f.srv.deps.PolicyHistory = fakePolicyHistory{revisions: map[string][]domain.RoutingPolicyRevision{
		"main": {
			{PolicyID: "main", Revision: 2, ContentHash: "new", Route: routeGroupAPI("p1")},
			{PolicyID: "main", Revision: 1, ContentHash: "old", Route: routeGroupAPI("p1")},
		},
	}}

	rec, out := f.do(http.MethodGet, "/api/policies/main/revisions", nil)
	if rec.Code != http.StatusOK || len(out["revisions"].([]any)) != 2 {
		t.Fatalf("revision list status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, out = f.do(http.MethodPost, "/api/policies/main/rollback", map[string]any{"revision": 1})
	if rec.Code != http.StatusOK || out["rolled_back"] != true {
		t.Fatalf("rollback status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func routeGroupAPI(group string) domain.RoutingPolicyRoute {
	return domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: group}}}
}
