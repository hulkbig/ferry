package scheduler

import (
	"reflect"
	"testing"

	"ferry/internal/domain"
)

func TestRouteTargetIndexFollowsPublishedAndDraftSnapshots(t *testing.T) {
	f := newV2Fixture(t)
	request, meta := chatReq("smart"), RequestMeta{BearerToken: "sk"}
	assertOrder := func(want []domain.ResourceTargetID) {
		t.Helper()
		simulation, failure := f.sch.DryRunV2(request, meta)
		if failure != nil || !reflect.DeepEqual(simulation.Plan.Order, want) {
			t.Fatalf("live order=%+v failure=%v want=%v", simulation, failure, want)
		}
	}
	assertOrder([]domain.ResourceTargetID{"p1-a", "p1-b", "p2-a"})
	original := f.sch.world.Load()
	f.calls["replacement"] = &fakeCaller{}
	f.publish(func(snapshot *domain.CompiledSnapshot) {
		snapshot.Version++
		replacement := *snapshot.Targets["p1-a"]
		replacement.ID = "replacement"
		snapshot.Targets = map[domain.ResourceTargetID]*domain.CompiledTarget{replacement.ID: &replacement}
		snapshot.TargetOrder = []domain.ResourceTargetID{replacement.ID}
	})
	assertOrder([]domain.ResourceTargetID{"replacement"})
	if got := original.routeTargets[routeTargetKey{provider: "provider-1", model: "upstream"}]; !reflect.DeepEqual(got, []domain.ResourceTargetID{"p1-a", "p1-b"}) {
		t.Fatalf("publication mutated the previous world's index: %v", got)
	}
	current := f.sch.world.Load()
	if err := f.sch.ReplaceAPIKeys(current.keys); err != nil {
		t.Fatal(err)
	}
	assertOrder([]domain.ResourceTargetID{"replacement"})

	// Draft compilation can assign new leaf IDs to the same public target.
	// Its index must use the draft IDs without publishing them to live traffic.
	draft := *current.snap
	replacement := *draft.Targets["replacement"]
	replacement.ID = "draft-only"
	draft.Targets = map[domain.ResourceTargetID]*domain.CompiledTarget{replacement.ID: &replacement}
	draft.TargetOrder = []domain.ResourceTargetID{replacement.ID}
	simulation, failure := f.sch.DryRunV2Snapshot(&draft, request, meta)
	if failure != nil || !reflect.DeepEqual(simulation.Plan.Order, []domain.ResourceTargetID{"draft-only"}) {
		t.Fatalf("draft index=%+v failure=%v", simulation, failure)
	}
	assertOrder([]domain.ResourceTargetID{"replacement"})
}
