package scheduler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"ferry/internal/domain"
)

// ProviderScheduledMessageInterval is intentionally fixed: configuration is
// Provider-scoped, while cadence stays predictable across every Provider.
const ProviderScheduledMessageInterval = 5 * time.Hour

const (
	providerScheduledMessageTimeout   = 2 * time.Minute
	providerScheduledMessageMaxTokens = 16
)

// StartScheduledProviderMessages starts the fixed five-hour background loop.
// The first delivery happens after one full interval so process restarts do
// not create an extra burst of upstream traffic.
func (s *Scheduler) StartScheduledProviderMessages(ctx context.Context) {
	s.startScheduledProviderMessages(ctx, ProviderScheduledMessageInterval)
}

func (s *Scheduler) startScheduledProviderMessages(ctx context.Context, interval time.Duration) {
	if s == nil || interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.dispatchScheduledProviderMessages(ctx)
			}
		}
	}()
}

// dispatchScheduledProviderMessages captures one immutable world and sends
// every enabled Provider's independently configured message. Providers run in
// parallel so a slow account cannot delay an unrelated Provider's cadence.
func (s *Scheduler) dispatchScheduledProviderMessages(ctx context.Context) {
	if s == nil {
		return
	}
	w := s.world.Load()
	if w == nil || w.snap == nil {
		return
	}
	providerIDs := make([]string, 0, len(w.snap.Providers))
	for providerID, provider := range w.snap.Providers {
		if provider != nil && provider.Enabled && provider.ScheduledMessage != nil && provider.ScheduledMessage.Enabled {
			providerIDs = append(providerIDs, providerID)
		}
	}
	sort.Strings(providerIDs)

	var workers sync.WaitGroup
	for _, providerID := range providerIDs {
		provider := w.snap.Providers[providerID]
		message := *provider.ScheduledMessage
		workers.Add(1)
		go func() {
			defer workers.Done()
			deliveryCtx, cancel := context.WithTimeout(ctx, providerScheduledMessageTimeout)
			defer cancel()
			target, failure := s.sendScheduledProviderMessage(deliveryCtx, w, provider, &message)
			if failure != nil {
				// Do not log Failure.Message: an upstream may echo request content
				// into an error, and the configured message is operator-owned data.
				log.Printf("provider scheduled message failed provider=%q model=%q failure_type=%q failure_scope=%q http_status=%d",
					provider.ID, message.UpstreamModel, failure.Type, failure.EffectiveScope(), failure.HTTPStatus)
				return
			}
			log.Printf("provider scheduled message succeeded provider=%q connection=%q model=%q", provider.ID, target.ConnectionID, message.UpstreamModel)
		}()
	}
	workers.Wait()
}

func (s *Scheduler) sendScheduledProviderMessage(ctx context.Context, w *world, provider *domain.Provider,
	message *domain.ProviderScheduledMessage) (*domain.CompiledTarget, *domain.Failure) {
	if provider == nil || message == nil || !provider.Enabled || !message.Enabled {
		return nil, scheduledMessageFailure("scheduled message is disabled")
	}
	if message.UpstreamModel == "" || message.Message == "" {
		return nil, scheduledMessageFailure("scheduled message configuration is incomplete")
	}
	protocols := scheduledMessageProtocols(w, provider.ID, message.UpstreamModel)
	if len(protocols) == 0 {
		return nil, scheduledMessageFailure("configured upstream model has no text protocol")
	}

	attempted := map[domain.ResourceTargetID]bool{}
	var lastFailure *domain.Failure
	for _, protocol := range protocols {
		rc := &domain.RequestContract{
			Protocol: protocol,
			Model:    message.UpstreamModel,
			Messages: []domain.Message{{
				Role: domain.RoleUser,
				Blocks: []domain.Block{{
					Kind: domain.BlockText,
					Text: message.Message,
				}},
			}},
			MaxTokens: providerScheduledMessageMaxTokens,
		}
		required := rc.ExtractRequirements()
		compatibility := newCompatibilityAssessor(ctx, w, rc, false, false)
		now := time.Now()
		logical := &domain.LogicalModel{ID: "scheduled-message:" + provider.ID, Enabled: true}
		plan, _, err := planV2(w, logical, domain.RouteSpec{Steps: []domain.RouteStep{{
			ProviderID: provider.ID, UpstreamModel: message.UpstreamModel,
		}}}, protocol, required, compatibility, s.manager.Snapshot(now), s.manager.ConnectionStates(now), nil)
		if ctx.Err() != nil {
			return nil, planningCancellation(ctx)
		}
		if err != nil {
			lastFailure = scheduledMessageFailure(err.Error())
			continue
		}

		started := false
		for _, targetID := range plan.Order {
			if attempted[targetID] {
				continue
			}
			attempted[targetID] = true
			started = true
			target := w.snap.Targets[targetID]
			caller := w.callers[targetID]
			if target == nil || caller == nil {
				lastFailure = scheduledMessageFailure(fmt.Sprintf("planned target %s is unavailable", targetID))
				continue
			}
			contract := contractFor(rc, target)
			if failure := attemptContractProtocolFailure(caller, contract); failure != nil {
				lastFailure = failure
				break
			}
			release, failure := s.manager.Admit(ctx, targetID)
			if failure != nil {
				lastFailure = failure
				s.manager.RecordFailure(targetID, failure, time.Now())
				if failure.EffectiveScope() != domain.FailureScopeConnection && failure.EffectiveScope() != domain.FailureScopeStream {
					return nil, failure
				}
				continue
			}
			startedAt := time.Now()
			response, failure := caller.Invoke(ctx, contract)
			release()
			if failure == nil {
				failure = responseProtocolFailure(rc, response)
			}
			if failure != nil {
				lastFailure = failure
				s.manager.RecordFailure(targetID, failure, time.Now())
				if failure.EffectiveScope() != domain.FailureScopeConnection && failure.EffectiveScope() != domain.FailureScopeStream {
					return nil, failure
				}
				continue
			}
			completedAt := time.Now()
			cash := w.rateCardFor(target).Cost(response.Usage)
			multiplier := 1.0
			if target.Plan != nil {
				multiplier = target.Plan.CurrentMultiplier(completedAt)
			}
			quota := 0.0
			if target.Bucket != nil {
				quota = domain.QuotaCost(target.Bucket.Unit, response.Usage, multiplier, true, cash)
			}
			s.manager.RecordSuccess(targetID, completedAt.Sub(startedAt).Milliseconds(), 0,
				response.Usage, cash, quota, completedAt)
			return target, nil
		}
		if started && lastFailure != nil &&
			lastFailure.EffectiveScope() != domain.FailureScopeConnection && lastFailure.EffectiveScope() != domain.FailureScopeStream {
			return nil, lastFailure
		}
	}
	if lastFailure == nil {
		lastFailure = scheduledMessageFailure("no eligible provider connection")
	}
	return nil, lastFailure
}

func scheduledMessageProtocols(w *world, providerID, upstreamModel string) []domain.Protocol {
	available := map[domain.Protocol]bool{}
	for _, targetID := range w.snap.TargetOrder {
		target := w.snap.Targets[targetID]
		if target == nil || target.ProviderID != providerID || target.Model != upstreamModel {
			continue
		}
		protocols := target.NativeProtocols
		if len(protocols) == 0 && target.Endpoint != nil {
			protocols = []domain.Protocol{target.Endpoint.Protocol}
		}
		for _, protocol := range protocols {
			switch protocol {
			case domain.ProtocolOpenAIResponses, domain.ProtocolOpenAIChat, domain.ProtocolAnthropic:
				available[protocol] = true
			}
		}
	}
	preference := []domain.Protocol{
		domain.ProtocolOpenAIResponses,
		domain.ProtocolOpenAIChat,
		domain.ProtocolAnthropic,
	}
	out := make([]domain.Protocol, 0, len(available))
	for _, protocol := range preference {
		if available[protocol] {
			out = append(out, protocol)
		}
	}
	return out
}

func scheduledMessageFailure(message string) *domain.Failure {
	return &domain.Failure{
		Type: domain.FailBadResponse, Scope: domain.FailureScopeRequest, Message: message,
	}
}
