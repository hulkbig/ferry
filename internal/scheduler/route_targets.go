package scheduler

import "ferry/internal/domain"

type routeTargetKey struct {
	provider string
	model    string
}

// indexRouteTargets caches only immutable catalog membership. It is rebuilt
// with each published or simulated snapshot; health, quota, credentials and
// payload compatibility are still checked for every request.
func indexRouteTargets(snapshot *domain.CompiledSnapshot) map[routeTargetKey][]domain.ResourceTargetID {
	index := make(map[routeTargetKey][]domain.ResourceTargetID)
	for _, id := range snapshot.TargetOrder {
		if target := snapshot.Targets[id]; target != nil {
			key := routeTargetKey{provider: target.ProviderID, model: target.Model}
			index[key] = append(index[key], id)
		}
	}
	return index
}
