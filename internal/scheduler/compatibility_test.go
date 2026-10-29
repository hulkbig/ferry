package scheduler

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"ferry/internal/domain"
)

func countPreparations(callers map[domain.ResourceTargetID]*fakeCaller) map[domain.ResourceTargetID]int {
	counts := map[domain.ResourceTargetID]int{}
	for id, caller := range callers {
		caller.prepare = func(*domain.RequestContract) ([]string, error) { counts[id]++; return nil, nil }
	}
	return counts
}

func TestCompatibilityPreparationBoundedBySelectedRoute(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			f := newV2Fixture(t)
			f.publish(func(s *domain.CompiledSnapshot) {
				for _, target := range s.Targets {
					target.NativeProtocols = []domain.Protocol{protocol}
				}
				// Repeated route steps must reuse their diagnostics, including fallbacks.
				model := s.LogicalModels["smart"]
				model.DefaultRoute.Steps = append(model.DefaultRoute.Steps, model.DefaultRoute.Steps[0])
				for i := 0; i < 3000; i++ {
					id := domain.ResourceTargetID(fmt.Sprintf("unrelated-%d", i))
					target := *s.Targets["p1-a"]
					target.ID, target.Model = id, string(id)
					if i%2 == 0 {
						target.Model, target.ProviderID = "upstream", "other-provider"
					}
					s.Targets[id] = &target
					s.TargetOrder = append(s.TargetOrder, id)
					f.calls[id] = &fakeCaller{}
				}
			})
			counts := countPreparations(f.calls)
			request := chatReq("smart")
			request.Protocol = protocol
			decision, failure := f.sch.decide(context.Background(), request, RequestMeta{BearerToken: "sk"}, true)
			if failure != nil {
				t.Fatal(failure)
			}
			want := map[domain.ResourceTargetID]int{"p1-a": 1, "p1-b": 1, "p2-a": 1}
			if !reflect.DeepEqual(counts, want) {
				t.Fatalf("prepared %d targets: %v", len(counts), counts)
			}
			if len(decision.compat) != 3 || len(decision.plan.OrderTiers) != 3 {
				t.Fatalf("lost assessments or fallback tiers: %+v", decision.plan)
			}
			// A new request has its own cache: changing request data cannot reuse a
			// compatibility result from the preceding request.
			_, failure = f.sch.decide(context.Background(), request, RequestMeta{BearerToken: "sk"}, true)
			if failure != nil || counts["p1-a"] != 2 {
				t.Fatalf("cross-request cache reuse: %v %v", counts, failure)
			}
		})
	}
}

func TestCompatibilityPreparationFollowsOverride(t *testing.T) {
	f := newV2Fixture(t)
	f.publish(func(s *domain.CompiledSnapshot) {
		s.LogicalModels["smart"].Overrides = []domain.OverrideSpec{{ID: "override", Enabled: true, Mode: domain.OverrideCustomRoute,
			Route: &domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "provider-2", UpstreamModel: "upstream"}}}}}
	})
	counts := countPreparations(f.calls)
	plan, failure := f.sch.DryRunV2(chatReq("smart"), RequestMeta{BearerToken: "sk"})
	if failure != nil {
		t.Fatal(failure)
	}
	if plan.OverrideID != "override" || !reflect.DeepEqual(counts, map[domain.ResourceTargetID]int{"p2-a": 1}) {
		t.Fatalf("override prepared default route: %v %+v", counts, plan)
	}
}

func TestCompatibilityPreparationSkipsIneligibleConnections(t *testing.T) {
	for _, reason := range []string{"disabled", "capability", "auth", "offline", "quota", "breaker", "cooldown"} {
		t.Run(reason, func(t *testing.T) {
			f := newV2Fixture(t)
			w := f.sch.world.Load()
			now := time.Now()
			state := f.manager.Snapshot(now)
			connections := f.manager.ConnectionStates(now)
			conn := connections["connection-a"]
			switch reason {
			case "disabled":
				w.snap.Connections["connection-a"].Enabled = false
			case "capability":
				w.snap.Targets["p1-a"].Capabilities.Text = false
			case "auth":
				conn.Auth = domain.ConnectionAuthError
			case "offline":
				conn.Reachability = domain.ConnectionOffline
			case "quota":
				conn.QuotaWindows = []domain.QuotaWindowState{{Availability: domain.QuotaExhausted}}
			case "breaker":
				state.Get("p1-a").Breaker = domain.BreakerOpen
			case "cooldown":
				state.Get("p1-a").CooldownUntil = now.Add(time.Hour)
			}
			connections["connection-a"] = conn
			counts := countPreparations(f.calls)
			rc := chatReq("smart")
			compat := newCompatibilityAssessor(context.Background(), w, rc, false, false)
			model := w.snap.LogicalModels["smart"]
			plan, _, err := planV2(w, model, model.DefaultRoute, rc.Protocol, rc.ExtractRequirements(), compat, state, connections, nil)
			if err != nil {
				t.Fatal(err)
			}
			if counts["p1-a"] != 0 || len(counts) != 2 || plan.Primary != "p1-b" {
				t.Fatalf("prepared excluded connection (%s): %v %+v", reason, counts, plan)
			}
		})
	}
}

func TestCompatibilityCancellationStopsPlanningWithoutAttempt(t *testing.T) {
	for _, before := range []bool{true, false} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("before_%t_stream_%t", before, stream), func(t *testing.T) {
				f := newV2Fixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				prepared := 0
				for _, caller := range f.calls {
					caller.prepare = func(*domain.RequestContract) ([]string, error) { prepared++; cancel(); return nil, nil }
				}
				if before {
					cancel()
				}
				rc := chatReq("smart")
				rc.Stream = stream
				result, failure := f.sch.Execute(ctx, rc, RequestMeta{BearerToken: "sk"})
				if result != nil || failure == nil || failure.Type != domain.FailClientCancel {
					t.Fatalf("cancellation = %v %v", result, failure)
				}
				want := 1
				if before {
					want = 0
				}
				if prepared != want {
					t.Fatalf("prepared=%d, want %d", prepared, want)
				}
				for id, caller := range f.calls {
					if caller.invokes.Load()+caller.streams.Load() != 0 {
						t.Fatalf("canceled request reached %s", id)
					}
				}
				db := f.ledger.reopen(t)
				records, _, err := db.QueryRequests(domain.LogFilter{})
				if err != nil {
					t.Fatal(err)
				}
				if len(records) != 1 || records[0].Status != domain.ReqCanceled || records[0].FailureType != domain.FailClientCancel {
					t.Fatalf("canceled planning ledger: %+v", records)
				}
				attempts, err := db.QueryAttempts(records[0].ID)
				if err != nil || len(attempts) != 0 {
					t.Fatalf("synthetic cancellation attempts: %+v %v", attempts, err)
				}
				trace, err := db.QueryTrace(records[0].ID)
				if err != nil || trace == nil || trace.Events[len(trace.Events)-1].Type != "PLANNING_CANCELED" {
					t.Fatalf("canceled planning trace: %+v %v", trace, err)
				}
			})
		}
	}
}

func TestLegacyCompatibilityPreparationSkipsUnselectedAndDisabledTargets(t *testing.T) {
	f := newFixture(t)
	f.publish(2, func(s *domain.CompiledSnapshot) {
		s.Targets["t2"].Disabled = true
		for i := 0; i < 2000; i++ {
			id := domain.ResourceTargetID(fmt.Sprintf("unselected-%d", i))
			target := *s.Targets["t1"]
			target.ID = id
			s.Targets[id] = &target
			s.TargetOrder = append(s.TargetOrder, id)
			f.calls[id] = &fakeCaller{}
		}
		s.Policies["main"].Tiers = append(s.Policies["main"].Tiers, domain.TierDef{Name: "duplicate", Pool: "poolA"})
	})
	counts := countPreparations(f.calls)
	plan, _, failure := f.sch.DryRun(chatReq("main"), RequestMeta{})
	if failure != nil {
		t.Fatal(failure)
	}
	if !reflect.DeepEqual(counts, map[domain.ResourceTargetID]int{"t1": 1, "t3": 1}) || len(plan.OrderTiers) != 3 {
		t.Fatalf("legacy preparations: %v %+v", counts, plan)
	}
}

func TestLegacyCompatibilityCancellationStopsFurtherPreparation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared := 0
	for _, caller := range f.calls {
		caller.prepare = func(*domain.RequestContract) ([]string, error) { prepared++; cancel(); return nil, nil }
	}
	_, failure := f.sch.Execute(ctx, chatReq("main"), RequestMeta{})
	if failure == nil || failure.Type != domain.FailClientCancel || prepared != 1 {
		t.Fatalf("legacy cancellation: preparations=%d failure=%v", prepared, failure)
	}
}
