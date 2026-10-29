package management

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/domain"
)

type requiredProbeCaller struct {
	calls         atomic.Int32
	failure       *domain.Failure
	omitReasoning bool
	omitTools     bool
	started       chan struct{}
	unblock       chan struct{}
	once          sync.Once
}

func (c *requiredProbeCaller) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	c.calls.Add(1)
	if c.started != nil {
		c.once.Do(func() { close(c.started) })
	}
	if c.unblock != nil {
		select {
		case <-c.unblock:
		case <-ctx.Done():
			return nil, &domain.Failure{Type: domain.FailConnect}
		}
	}
	if c.failure != nil {
		return nil, c.failure
	}
	response := &domain.Response{Content: []domain.Block{{Kind: domain.BlockText, Text: `{"ok":true}`}}}
	if len(rc.Tools) > 0 && !c.omitTools {
		response.Content = []domain.Block{{Kind: domain.BlockToolUse, ID: "probe", Name: rc.Tools[0].Name, Input: json.RawMessage(`{"ok":true}`)}}
	}
	if rc.Reasoning != nil && !c.omitReasoning {
		response.Usage.ReasoningTokens = 8
	}
	return response, nil
}
func (c *requiredProbeCaller) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	response, failure := c.Invoke(ctx, rc)
	if failure != nil {
		return nil, nil, failure
	}
	ch := make(chan domain.StreamEvent, 5)
	for _, block := range response.Content {
		if block.Kind == domain.BlockToolUse {
			ch <- domain.StreamEvent{Kind: domain.EvToolStart, ToolName: block.Name, ToolID: block.ID}
			ch <- domain.StreamEvent{Kind: domain.EvToolDelta, ArgsDelta: string(block.Input)}
			ch <- domain.StreamEvent{Kind: domain.EvToolEnd}
		} else {
			ch <- domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: block.Text}
		}
	}
	ch <- domain.StreamEvent{Kind: domain.EvEnd, Usage: &response.Usage}
	close(ch)
	return ch, func() {}, nil
}

func verificationFixture(t *testing.T) (*v2APIFixture, *domain.Provider, *domain.Connection, *domain.CompiledTarget) {
	t.Helper()
	f := newV2APIFixture(t)
	cfg, err := cloneConfig(f.server.currentConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Connections[0].Models[0].Capabilities = domain.Capabilities{Text: true}
	if err = f.server.publishConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snap := f.server.deps.Scheduler.Snapshot()
	for _, target := range snap.Targets {
		if target.Model == "upstream-model" {
			return f, snap.Providers[target.ProviderID], snap.Connections[target.ConnectionID], target
		}
	}
	t.Fatal("target missing")
	return nil, nil, nil, nil
}
func TestRequiredCapabilitiesVerifyCacheAndCredentialIsolation(t *testing.T) {
	f, p, c, target := verificationFixture(t)
	required := domain.Requirements{Text: true, Stream: true, Tools: true, ParallelTools: true, Reasoning: true}
	caller := &requiredProbeCaller{}
	verify := func(connection *domain.Connection, call *requiredProbeCaller) domain.CapabilityVerification {
		return f.server.VerifyRequiredCapabilities(context.Background(), p, connection, target, call, domain.ProtocolOpenAIChat, required)
	}
	first := verify(c, caller)
	if first.Error != "" || !first.Capabilities.Stream || !first.Capabilities.Tools || !first.Capabilities.Reasoning || first.Probes != 3 {
		t.Fatalf("first=%+v", first)
	}
	// A fresh caller bypasses memory backoff and must reuse durable evidence.
	fresh := &requiredProbeCaller{}
	cached := verify(c, fresh)
	if cached.Error != "" || fresh.calls.Load() != 0 || cached.Probes != 0 || !cached.Capabilities.Reasoning {
		t.Fatalf("cached=%+v calls=%d", cached, fresh.calls.Load())
	}
	sibling := *c
	sibling.ID = "different-account"
	if result := verify(&sibling, fresh); result.Probes != 3 {
		t.Fatalf("sibling inherited account evidence: %+v", result)
	}
	if err := f.server.markProviderModelSyncDueForSecret("FERRY_V2_TEST_KEY"); err != nil {
		t.Fatal(err)
	}
	replaced := &requiredProbeCaller{}
	if result := verify(c, replaced); result.Probes != 3 {
		t.Fatalf("credential replacement reused evidence: %+v", result)
	}
	if inFlight := f.server.deps.Manager.ConnectionStates(time.Now())[c.ID].InFlight; inFlight != 0 {
		t.Fatalf("probe leaked %d slots", inFlight)
	}
	if target.Capabilities.Reasoning {
		t.Fatal("probe mutated shared snapshot")
	}
}

func TestRequiredCapabilitiesOnlyExplicitRejectionIsNegative(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		failure                  *domain.Failure
		omitReasoning, omitTools bool
		capability               string
		want                     domain.CapabilityStatus
	}{
		{name: "rate limit", failure: &domain.Failure{Type: domain.FailRateLimit, HTTPStatus: 429}, capability: domain.CapabilityReasoning, want: domain.CapabilityUnknown},
		{name: "network", failure: &domain.Failure{Type: domain.FailConnect}, capability: domain.CapabilityReasoning, want: domain.CapabilityUnknown},
		{name: "ignored reasoning", omitReasoning: true, capability: domain.CapabilityReasoning, want: domain.CapabilityUnknown},
		{name: "ignored tools", omitTools: true, capability: domain.CapabilityTools, want: domain.CapabilityUnknown},
		{name: "explicit rejection", failure: &domain.Failure{Type: domain.FailUnsupportedParam, HTTPStatus: 400}, capability: domain.CapabilityReasoning, want: domain.CapabilityUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, c, target := verificationFixture(t)
			required := domain.Requirements{Text: true}
			if tc.capability == domain.CapabilityReasoning {
				required.Reasoning = true
			} else {
				required.Tools = true
			}
			caller := &requiredProbeCaller{failure: tc.failure, omitReasoning: tc.omitReasoning, omitTools: tc.omitTools}
			first := f.server.VerifyRequiredCapabilities(context.Background(), p, c, target, caller, domain.ProtocolOpenAIChat, required)
			if first.Statuses[tc.capability] != tc.want {
				t.Fatalf("result=%+v", first)
			}
			again := f.server.VerifyRequiredCapabilities(context.Background(), p, c, target, caller, domain.ProtocolOpenAIChat, required)
			if again.Probes != 0 || caller.calls.Load() != 1 {
				t.Fatalf("transient backoff failed: %+v", again)
			}
			fresh := &requiredProbeCaller{}
			// Simulate connection recovery separately from evidence expiry.
			if err := f.server.deps.Manager.ResetCooldowns([]domain.ResourceTargetID{target.ID}); err != nil {
				t.Fatal(err)
			}
			second := f.server.VerifyRequiredCapabilities(context.Background(), p, c, target, fresh, domain.ProtocolOpenAIChat, required)
			if tc.want == domain.CapabilityUnsupported {
				if second.Probes != 0 || second.Statuses[tc.capability] != tc.want {
					t.Fatalf("negative cache=%+v", second)
				}
			} else if second.Statuses[tc.capability] != domain.CapabilitySupported || second.Probes != 1 {
				t.Fatalf("transient became permanent denial: %+v", second)
			}
		})
	}
}

func TestRequiredCapabilitiesSingleFlightAndRotationDuringProbe(t *testing.T) {
	f, p, c, target := verificationFixture(t)
	caller := &requiredProbeCaller{started: make(chan struct{}), unblock: make(chan struct{})}
	required := domain.Requirements{Reasoning: true}
	results := make(chan domain.CapabilityVerification, 8)
	for range 8 {
		go func() {
			results <- f.server.VerifyRequiredCapabilities(context.Background(), p, c, target, caller, domain.ProtocolOpenAIChat, required)
		}()
	}
	<-caller.started
	close(caller.unblock)
	probes := 0
	for range 8 {
		r := <-results
		if r.Error != "" {
			t.Fatal(r.Error)
		}
		probes += r.Probes
	}
	if probes != 1 || caller.calls.Load() != 1 {
		t.Fatalf("singleflight calls=%d probes=%d", caller.calls.Load(), probes)
	}
	// Rotate while a new probe is active; its old-account observation cannot
	// repopulate the durable cache after invalidation.
	if err := f.server.invalidateCapabilityEvidence(map[string]bool{p.ID: true}); err != nil {
		t.Fatal(err)
	}
	blocked := &requiredProbeCaller{started: make(chan struct{}), unblock: make(chan struct{})}
	go func() {
		results <- f.server.VerifyRequiredCapabilities(context.Background(), p, c, target, blocked, domain.ProtocolOpenAIChat, required)
	}()
	<-blocked.started
	if err := f.server.invalidateCapabilityEvidence(map[string]bool{p.ID: true}); err != nil {
		t.Fatal(err)
	}
	close(blocked.unblock)
	stale := <-results
	if !strings.Contains(stale.Error, "credential changed") || stale.Capabilities.Reasoning {
		t.Fatalf("stale=%+v", stale)
	}
	fresh := &requiredProbeCaller{}
	r := f.server.VerifyRequiredCapabilities(context.Background(), p, c, target, fresh, domain.ProtocolOpenAIChat, required)
	if r.Probes != 1 || r.Error != "" {
		t.Fatalf("new credential not probed: %+v", r)
	}
}
