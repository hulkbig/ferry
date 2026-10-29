package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"ferry/internal/domain"
)

type RoutingContext struct {
	Consumer     string
	Model        string
	Protocol     domain.Protocol
	Capabilities domain.Requirements
	Now          time.Time
}

type CompiledRoutingPolicy struct {
	domain.RoutingPolicySpec
	location     *time.Location
	consumers    map[string]struct{}
	models       map[string]struct{}
	protocols    map[domain.Protocol]struct{}
	weekdays     map[string]struct{}
	capabilities map[string]struct{}
}

type RoutingIndex struct {
	Hash     string
	Policies []*CompiledRoutingPolicy
}

func CompileRoutingIndex(specs []domain.RoutingPolicySpec) (*RoutingIndex, error) {
	enabled, hash, err := prepareRoutingIndex(specs)
	if err != nil {
		return nil, err
	}
	return compilePreparedRoutingIndex(enabled, hash)
}

func compilePreparedRoutingIndex(specs []domain.RoutingPolicySpec, hash string) (*RoutingIndex, error) {
	index := &RoutingIndex{Hash: hash}
	for _, spec := range specs {
		compiled := &CompiledRoutingPolicy{
			RoutingPolicySpec: spec,
			consumers:         stringSet(spec.Match.Consumers),
			models:            stringSet(spec.Match.Models),
			protocols:         protocolSet(spec.Match.Protocols),
			weekdays:          stringSet(lowerStrings(spec.Match.Weekdays)),
			capabilities:      stringSet(lowerStrings(spec.Match.RequiredCapabilities)),
		}
		var err error
		tz := spec.Match.Timezone
		if tz == "" {
			tz = "UTC"
		}
		compiled.location, err = time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("routing policy %s: invalid timezone %q: %w", spec.ID, tz, err)
		}
		if spec.Match.TimeWindow != nil && (!validHHMM(spec.Match.TimeWindow.Start) || !validHHMM(spec.Match.TimeWindow.End)) {
			return nil, fmt.Errorf("routing policy %s: invalid time window", spec.ID)
		}
		if strings.TrimSpace(spec.Match.Expression) != "" {
			return nil, fmt.Errorf("routing policy %s: expression is not supported by this runtime", spec.ID)
		}
		index.Policies = append(index.Policies, compiled)
	}
	return index, nil
}

// prepareRoutingIndex performs only deterministic validation and hashing. It
// deliberately runs before parsing timezones or building matcher maps, so an
// unchanged active index can return directly from the compiled cache.
func prepareRoutingIndex(specs []domain.RoutingPolicySpec) ([]domain.RoutingPolicySpec, string, error) {
	if err := domain.ValidateRoutingPolicies(specs); err != nil {
		return nil, "", err
	}
	type entry struct {
		ID       string `json:"id"`
		Priority int    `json:"priority"`
		Revision int64  `json:"revision"`
		Hash     string `json:"hash"`
	}
	enabled := make([]domain.RoutingPolicySpec, 0, len(specs))
	entries := make([]entry, 0, len(specs))
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		contentHash, err := domain.RoutingPolicyContentHash(spec)
		if err != nil {
			return nil, "", err
		}
		spec.ContentHash = contentHash
		enabled = append(enabled, spec)
		entries = append(entries, entry{ID: spec.ID, Priority: spec.Priority, Revision: spec.Revision, Hash: contentHash})
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return enabled, hex.EncodeToString(sum[:]), nil
}

func (i *RoutingIndex) Select(ctx RoutingContext) *CompiledRoutingPolicy {
	if i == nil {
		return nil
	}
	for _, candidate := range i.Policies {
		if candidate.matches(ctx) {
			return candidate
		}
	}
	return nil
}

func (p *CompiledRoutingPolicy) matches(ctx RoutingContext) bool {
	if !containsOrAny(p.consumers, ctx.Consumer) || !containsOrAny(p.models, ctx.Model) {
		return false
	}
	if len(p.protocols) > 0 {
		if _, ok := p.protocols[ctx.Protocol]; !ok {
			return false
		}
	}
	if !requirementsMatch(p.capabilities, ctx.Capabilities) {
		return false
	}
	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(p.location)
	if len(p.weekdays) > 0 {
		weekday := strings.ToLower(now.Weekday().String()[:3])
		if _, ok := p.weekdays[weekday]; !ok {
			return false
		}
	}
	if window := p.Match.TimeWindow; window != nil && !withinTimeWindow(now, window) {
		return false
	}
	return true
}

func requirementsMatch(wanted map[string]struct{}, got domain.Requirements) bool {
	for capability := range wanted {
		present, known := domain.RequirementCapability(got, capability)
		if !known {
			return false
		}
		if !present {
			return false
		}
	}
	return true
}

func withinTimeWindow(now time.Time, window *domain.TimeWindow) bool {
	minutes := now.Hour()*60 + now.Minute()
	start := parseHHMM(window.Start)
	end := parseHHMM(window.End)
	if start == end {
		return true
	}
	if start < end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}

func parseHHMM(value string) int {
	return int(value[0]-'0')*600 + int(value[1]-'0')*60 + int(value[3]-'0')*10 + int(value[4]-'0')
}

func containsOrAny(values map[string]struct{}, value string) bool {
	if len(values) == 0 {
		return true
	}
	_, ok := values[value]
	return ok
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func protocolSet(values []domain.Protocol) map[domain.Protocol]struct{} {
	out := make(map[domain.Protocol]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func lowerStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strings.ToLower(value))
	}
	return out
}

type RoutingIndexCache struct {
	mu    sync.RWMutex
	items map[string]*RoutingIndex
}

func NewRoutingIndexCache() *RoutingIndexCache {
	return &RoutingIndexCache{items: map[string]*RoutingIndex{}}
}

func (c *RoutingIndexCache) Compile(specs []domain.RoutingPolicySpec) (*RoutingIndex, error) {
	index, _, err := c.CompileWithStatus(specs)
	return index, err
}

// CompileWithStatus reports whether the immutable active index was reused.
func (c *RoutingIndexCache) CompileWithStatus(specs []domain.RoutingPolicySpec) (*RoutingIndex, bool, error) {
	enabled, hash, err := prepareRoutingIndex(specs)
	if err != nil {
		return nil, false, err
	}
	c.mu.RLock()
	cached := c.items[hash]
	c.mu.RUnlock()
	if cached != nil {
		return cached, true, nil
	}
	index, err := compilePreparedRoutingIndex(enabled, hash)
	if err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached = c.items[hash]; cached != nil {
		return cached, true, nil
	}
	c.items[hash] = index
	return index, false, nil
}
