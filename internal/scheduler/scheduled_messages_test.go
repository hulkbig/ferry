package scheduler

import (
	"context"
	"testing"
	"time"

	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/runtime"
)

func scheduledMessageScheduler(t *testing.T) (*Scheduler, map[string]*fakeCaller) {
	t.Helper()
	manager := runtime.NewManager(runtime.DefaultHealthConfig())
	scheduler := New(manager, nil)
	callers := map[string]*fakeCaller{
		"chat":      {},
		"responses": {},
		"anthropic": {},
	}
	targets := map[domain.ResourceTargetID]*domain.CompiledTarget{}
	connections := map[string]*domain.Connection{}
	providers := map[string]*domain.Provider{}
	egressCallers := map[domain.ResourceTargetID]egress.Caller{}
	order := []domain.ResourceTargetID{}
	protocols := map[string]domain.Protocol{
		"chat": domain.ProtocolOpenAIChat, "responses": domain.ProtocolOpenAIResponses, "anthropic": domain.ProtocolAnthropic,
	}
	for _, id := range []string{"chat", "responses", "anthropic"} {
		providerID := "provider-" + id
		connectionID := "connection-" + id
		targetID := domain.ResourceTargetID("target-" + id)
		protocol := protocols[id]
		providers[providerID] = &domain.Provider{
			ID: providerID, Type: domain.ProviderCustom, Enabled: true,
			ScheduledMessage: &domain.ProviderScheduledMessage{
				Enabled: true, UpstreamModel: "model-" + id, Message: "message for " + id,
			},
		}
		connections[connectionID] = &domain.Connection{ID: connectionID, ProviderID: providerID, Enabled: true}
		targets[targetID] = &domain.CompiledTarget{
			ID: targetID, Endpoint: &domain.Endpoint{ID: string(targetID), BaseURL: "https://provider.test", Protocol: protocol},
			Model: "model-" + id, Capabilities: domain.Capabilities{Text: true},
			ProviderID: providerID, ConnectionID: connectionID, NativeProtocols: []domain.Protocol{protocol},
		}
		egressCallers[targetID] = callers[id]
		order = append(order, targetID)
	}
	scheduler.ReplaceWorldWithAPIKeys(&domain.CompiledSnapshot{
		Version: 2, CreatedAt: time.Now(), Targets: targets, TargetOrder: order,
		Providers: providers, Connections: connections,
	}, egressCallers, nil, nil)
	return scheduler, callers
}

func TestProviderScheduledMessagesSupportEveryTextProtocol(t *testing.T) {
	scheduler, callers := scheduledMessageScheduler(t)
	scheduler.dispatchScheduledProviderMessages(context.Background())

	for _, id := range []string{"chat", "responses", "anthropic"} {
		caller := callers[id]
		if got := caller.invokes.Load(); got != 1 {
			t.Fatalf("%s invokes = %d, want 1", id, got)
		}
		request := caller.lastRequest()
		if request == nil || request.Protocol != map[string]domain.Protocol{
			"chat": domain.ProtocolOpenAIChat, "responses": domain.ProtocolOpenAIResponses, "anthropic": domain.ProtocolAnthropic,
		}[id] {
			t.Fatalf("%s request = %+v", id, request)
		}
		if request.Model != "model-"+id || request.MaxTokens != providerScheduledMessageMaxTokens ||
			len(request.Messages) != 1 || len(request.Messages[0].Blocks) != 1 ||
			request.Messages[0].Blocks[0].Text != "message for "+id {
			t.Fatalf("%s canonical request = %+v", id, request)
		}
	}
}

func TestScheduledMessagePreparesOnlyItsProviderRoute(t *testing.T) {
	scheduler, callers := scheduledMessageScheduler(t)
	counts := map[string]int{}
	for id, caller := range callers {
		caller.prepare = func(*domain.RequestContract) ([]string, error) { counts[id]++; return nil, nil }
	}
	w := scheduler.world.Load()
	provider := w.snap.Providers["provider-chat"]
	_, failure := scheduler.sendScheduledProviderMessage(context.Background(), w, provider, provider.ScheduledMessage)
	// One route compatibility check and one final attempt validation.
	if failure != nil || len(counts) != 1 || counts["chat"] != 2 {
		t.Fatalf("preparations=%v failure=%v", counts, failure)
	}
}

func TestProviderScheduledMessageLoopUsesFixedFiveHourCadence(t *testing.T) {
	if ProviderScheduledMessageInterval != 5*time.Hour {
		t.Fatalf("scheduled message interval = %s, want 5h", ProviderScheduledMessageInterval)
	}
	scheduler, callers := scheduledMessageScheduler(t)
	started := make(chan struct{})
	callers["chat"].invokeStarted = started
	for id, provider := range scheduler.Snapshot().Providers {
		if id != "provider-chat" {
			provider.ScheduledMessage.Enabled = false
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler.startScheduledProviderMessages(ctx, 10*time.Millisecond)
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("scheduled message loop did not dispatch after its interval")
	}
}

func TestProviderScheduledMessagesSkipDisabledConfigurations(t *testing.T) {
	scheduler, callers := scheduledMessageScheduler(t)
	scheduler.Snapshot().Providers["provider-chat"].Enabled = false
	scheduler.Snapshot().Providers["provider-responses"].ScheduledMessage.Enabled = false
	scheduler.dispatchScheduledProviderMessages(context.Background())
	if callers["chat"].invokes.Load() != 0 || callers["responses"].invokes.Load() != 0 || callers["anthropic"].invokes.Load() != 1 {
		t.Fatalf("invokes chat=%d responses=%d anthropic=%d", callers["chat"].invokes.Load(), callers["responses"].invokes.Load(), callers["anthropic"].invokes.Load())
	}
}
