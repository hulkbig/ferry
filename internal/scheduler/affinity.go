package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"ferry/internal/domain"
)

type affinityCache struct {
	mu       sync.Mutex
	bindings map[string]domain.AffinityBinding
}

// AffinityStats is an aggregate-only view. It exposes no evidence hashes and
// is safe for the V2 management facade.
type AffinityStats struct {
	ActiveBindings int `json:"active_bindings"`
	Conversation   int `json:"conversation"`
	Cache          int `json:"cache"`
	Caller         int `json:"caller"`
}

func newAffinityCache() *affinityCache {
	return &affinityCache{bindings: map[string]domain.AffinityBinding{}}
}

func affinityKey(evidence domain.AffinityEvidence, logicalModel string) string {
	return string(evidence.Scope) + "\x00" + evidence.KeyHash + "\x00" + logicalModel
}

func routeFingerprint(route domain.RouteSpec) string {
	raw, _ := json.Marshal(route)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func affinityPriority(scope domain.AffinityScope) int {
	switch scope {
	case domain.AffinityConversation:
		return 0
	case domain.AffinityCache:
		return 1
	default:
		return 2
	}
}

func (c *affinityCache) resolve(evidence []domain.AffinityEvidence, logicalModel string, now time.Time) (*domain.AffinityBinding, *domain.AffinityEvidence) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bestPriority := 99
	var best *domain.AffinityBinding
	var matched *domain.AffinityEvidence
	for i := range evidence {
		item := &evidence[i]
		binding, ok := c.bindings[affinityKey(*item, logicalModel)]
		if !ok {
			continue
		}
		if !binding.ExpiresAt.IsZero() && !binding.ExpiresAt.After(now) {
			delete(c.bindings, affinityKey(*item, logicalModel))
			continue
		}
		priority := affinityPriority(item.Scope)
		if priority < bestPriority {
			copyBinding := binding
			copyEvidence := *item
			best, matched, bestPriority = &copyBinding, &copyEvidence, priority
		}
	}
	return best, matched
}

func (c *affinityCache) bind(evidence domain.AffinityEvidence, logicalModel string,
	route domain.RouteSpec, target *domain.CompiledTarget, now time.Time, overwrite bool) {
	if c == nil || target == nil || evidence.KeyHash == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := affinityKey(evidence, logicalModel)
	if existing, ok := c.bindings[key]; ok && !overwrite {
		existing.LastSeenAt = now
		c.bindings[key] = existing
		return
	}
	ttl := 24 * time.Hour
	if evidence.Scope == domain.AffinityConversation {
		ttl = 30 * 24 * time.Hour
	} else if evidence.Scope == domain.AffinityCache {
		ttl = 6 * time.Hour
	}
	c.bindings[key] = domain.AffinityBinding{
		Scope: evidence.Scope, KeyHash: evidence.KeyHash, LogicalModelID: logicalModel,
		RouteFingerprint: routeFingerprint(route), ProviderID: target.ProviderID,
		UpstreamModel: target.Model, ConnectionID: target.ConnectionID,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(ttl), Status: "ACTIVE",
	}
}

func (c *affinityCache) statsByProvider(now time.Time) map[string]AffinityStats {
	return c.statsBy(now, func(binding domain.AffinityBinding) string { return binding.ProviderID })
}

func (c *affinityCache) statsByConnection(now time.Time) map[string]AffinityStats {
	return c.statsBy(now, func(binding domain.AffinityBinding) string { return binding.ConnectionID })
}

func (c *affinityCache) statsByModel(now time.Time) map[string]AffinityStats {
	return c.statsBy(now, func(binding domain.AffinityBinding) string { return binding.LogicalModelID })
}

func (c *affinityCache) prune(snapshot *domain.CompiledSnapshot) int {
	if c == nil || snapshot == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for key, binding := range c.bindings {
		model := snapshot.LogicalModels[binding.LogicalModelID]
		provider := snapshot.Providers[binding.ProviderID]
		connection := snapshot.Connections[binding.ConnectionID]
		validSupply := false
		for _, target := range snapshot.Targets {
			if target != nil && target.ProviderID == binding.ProviderID && target.ConnectionID == binding.ConnectionID && target.Model == binding.UpstreamModel {
				validSupply = true
				break
			}
		}
		if model == nil || !model.Enabled || provider == nil || connection == nil || !validSupply {
			delete(c.bindings, key)
			removed++
		}
	}
	return removed
}

func (c *affinityCache) statsBy(now time.Time, keyOf func(domain.AffinityBinding) string) map[string]AffinityStats {
	out := map[string]AffinityStats{}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, binding := range c.bindings {
		if !binding.ExpiresAt.IsZero() && !binding.ExpiresAt.After(now) {
			delete(c.bindings, key)
			continue
		}
		key := keyOf(binding)
		if key == "" {
			continue
		}
		stats := out[key]
		stats.ActiveBindings++
		switch binding.Scope {
		case domain.AffinityConversation:
			stats.Conversation++
		case domain.AffinityCache:
			stats.Cache++
		case domain.AffinityCaller:
			stats.Caller++
		}
		out[key] = stats
	}
	return out
}

func (s *Scheduler) AffinityStatsByProvider(now time.Time) map[string]AffinityStats {
	if s == nil {
		return map[string]AffinityStats{}
	}
	return s.affinity.statsByProvider(now)
}

func (s *Scheduler) AffinityStatsByConnection(now time.Time) map[string]AffinityStats {
	if s == nil {
		return map[string]AffinityStats{}
	}
	return s.affinity.statsByConnection(now)
}

func (s *Scheduler) AffinityStatsByModel(now time.Time) map[string]AffinityStats {
	if s == nil {
		return map[string]AffinityStats{}
	}
	return s.affinity.statsByModel(now)
}

// PruneAffinityBindings removes bindings whose logical model or private
// provider execution supply no longer exists in the active snapshot. It is
// called after configuration apply so cascading deletes take effect without
// waiting for affinity TTL expiry; in-flight requests retain their old world.
func (s *Scheduler) PruneAffinityBindings() int {
	if s == nil {
		return 0
	}
	return s.affinity.prune(s.Snapshot())
}
