package scheduler

import (
	"fmt"
	"strings"
	"time"

	"ferry/internal/conversation"
	"ferry/internal/domain"
)

func (s *Scheduler) resolveConversation(rc *domain.RequestContract, logicalModel string,
	evidence []domain.AffinityEvidence) (*domain.Conversation, []domain.AffinityEvidence, *domain.NativeContinuation, *domain.Failure) {
	if rc.Lineage != nil && rc.Lineage.ConversationID != "" {
		if s.conversations == nil {
			return nil, evidence, nil, conversationFailure("unknown conversation %q", rc.Lineage.ConversationID)
		}
		// Ferry conversation IDs are durable public handles. Resolve the ID
		// directly before evidence aliases so a client can round-trip it even
		// when the original evidence came from a different adapter/header.
		current, err := s.conversations.Conversation(rc.Lineage.ConversationID)
		if err != nil {
			return nil, evidence, nil, conversationFailure("resolve conversation: %v", err)
		}
		if current != nil {
			if current.LogicalModelID != logicalModel {
				return nil, evidence, nil, conversationFailure("conversation belongs to logical model %q", current.LogicalModelID)
			}
			evidence = append([]domain.AffinityEvidence{domain.NewAffinityEvidence(
				"smartapi.conversation", domain.AffinityConversation, current.ID, domain.ConfHigh)}, evidence...)
			return current, evidence, nil, nil
		}
		return nil, evidence, nil, conversationFailure("unknown conversation %q", rc.Lineage.ConversationID)
	}
	if s.conversations == nil {
		if rc.Lineage != nil && rc.Lineage.PreviousResponseID != "" {
			return nil, evidence, nil, conversationFailure(
				"conversation store unavailable for previous_response_id %q", rc.Lineage.PreviousResponseID)
		}
		return nil, evidence, nil, nil
	}
	if rc.Lineage != nil && rc.Lineage.PreviousResponseID != "" {
		previous, current, err := s.conversations.ConversationAtResponse(rc.Lineage.PreviousResponseID)
		if err != nil {
			return nil, evidence, nil, conversationFailure("resolve previous_response_id: %v", err)
		}
		if previous == nil {
			return nil, evidence, nil, conversationFailure("unknown previous_response_id %q", rc.Lineage.PreviousResponseID)
		}
		if current == nil {
			return nil, evidence, nil, conversationFailure("previous response conversation unavailable")
		}
		if current.LogicalModelID != logicalModel {
			return nil, evidence, nil, conversationFailure("previous response belongs to logical model %q", current.LogicalModelID)
		}
		evidence = append([]domain.AffinityEvidence{domain.NewAffinityEvidence(
			"smartapi.response_lineage", domain.AffinityConversation, current.ID, domain.ConfHigh)}, evidence...)
		var continuation *domain.NativeContinuation
		if previous.NativeResponseID != "" {
			continuation = &domain.NativeContinuation{NativeResponseID: previous.NativeResponseID}
		}
		return current, evidence, continuation, nil
	}
	for _, item := range evidence {
		if item.Scope != domain.AffinityConversation {
			continue
		}
		current, err := s.conversations.ConversationForEvidence(logicalModel, item.KeyHash)
		if err != nil {
			return nil, evidence, nil, conversationFailure("resolve conversation evidence: %v", err)
		}
		if current == nil {
			continue
		}
		if current.LogicalModelID != logicalModel {
			return nil, evidence, nil, conversationFailure("conversation belongs to logical model %q", current.LogicalModelID)
		}
		return current, evidence, nil, nil
	}
	return nil, evidence, nil, nil
}

func conversationFailure(format string, args ...any) *domain.Failure {
	return &domain.Failure{
		Type: domain.FailUnsupportedParam, Scope: domain.FailureScopeRequest,
		Message: fmt.Sprintf(format, args...),
	}
}

func (d *decision) publicResponseID() string {
	if d.responseID != "" {
		return d.responseID
	}
	return d.rec.ID
}

func conversationIDOf(current *domain.Conversation) string {
	if current == nil {
		return ""
	}
	return current.ID
}

func providerSessionKeyOf(evidence []domain.AffinityEvidence, current *domain.Conversation) string {
	for _, item := range evidence {
		if item.Scope == domain.AffinityConversation && item.KeyHash != "" {
			return item.KeyHash
		}
	}
	if current != nil {
		return current.ID
	}
	return ""
}

func (d *decision) contractForAttempt(rc *domain.RequestContract, target *domain.CompiledTarget) (*domain.RequestContract, domain.ContinuityMode) {
	contract, _ := contractForTarget(rc, target)
	contract.ProviderSessionKey = providerSessionKeyOf(d.evidence, d.conversation)
	contract.CodexTurnStateAllowed = false
	if rc.Protocol != domain.ProtocolOpenAIResponses {
		return contract, domain.ContinuityNone
	}
	provider := d.w.snap.Providers[target.ProviderID]
	contract.CodexTurnStateAllowed = provider != nil && provider.Type == domain.ProviderCliproxyCodex &&
		d.conversation != nil && d.conversation.CurrentProviderID == target.ProviderID &&
		d.conversation.CurrentConnectionID == target.ConnectionID && target.ConnectionID != ""
	supportsNative := provider != nil && provider.Type != domain.ProviderCliproxyCodex &&
		target.EgressProtocol(rc.Protocol) == domain.ProtocolOpenAIResponses
	contract.PersistNativeState = supportsNative
	if d.conversation == nil || !hasExplicitResponsesContinuation(rc) {
		// Thread/session headers are routing-affinity evidence, not a request
		// for Ferry to prepend stored history. Stateless Responses clients
		// such as Codex already replay their complete input (including tool
		// calls and outputs); prepending the affinity snapshot duplicates that
		// transcript and can put ordinary messages between a call and result.
		return contract, domain.ContinuityNone
	}
	if supportsNative && d.conversation.CurrentProviderID == target.ProviderID &&
		d.conversation.CurrentConnectionID == target.ConnectionID && d.w != nil {
		if d.parentContinuation != nil && d.parentContinuation.NativeResponseID != "" {
			copy := *d.parentContinuation
			contract.Continuation = &copy
			return contract, domain.ContinuityNative
		}
		// Native handles are connection-scoped conservatively. Providers may
		// later declare a broader scope without weakening current correctness.
		if dStore := d.conversationStore; dStore != nil {
			handle, err := dStore.NativeHandle(d.conversation.ID, target.ProviderID, target.ConnectionID)
			if err == nil && handle != nil && handle.NativeResponseID != "" {
				contract.Continuation = &domain.NativeContinuation{
					NativeConversationID: handle.NativeConversationID,
					NativeResponseID:     handle.NativeResponseID,
				}
				return contract, domain.ContinuityNative
			}
		}
	}
	if len(d.conversation.Items) > 0 {
		messages := make([]domain.Message, 0, len(d.conversation.Items)+len(contract.Messages))
		messages = append(messages, d.conversation.Items...)
		messages = append(messages, contract.Messages...)
		contract.Messages = messages
		contract.Native = nil
		contract.Continuation = nil
		return contract, domain.ContinuityRehydrated
	}
	return contract, domain.ContinuityNone
}

func hasExplicitResponsesContinuation(rc *domain.RequestContract) bool {
	return rc != nil && rc.Lineage != nil &&
		(strings.TrimSpace(rc.Lineage.ConversationID) != "" ||
			strings.TrimSpace(rc.Lineage.PreviousResponseID) != "")
}

func (d *decision) persistConversation(rc *domain.RequestContract, target *domain.CompiledTarget,
	output []domain.Block, nativeResponseID, nativeConversationID string, continuity domain.ContinuityMode) error {
	store := d.conversationStore
	if store == nil || target == nil || (!d.v2 && rc.Protocol != domain.ProtocolOpenAIResponses) {
		return nil
	}
	now := time.Now()
	current := domain.Conversation{}
	if d.conversation != nil {
		current = *d.conversation
		current.Items = append([]domain.Message(nil), d.conversation.Items...)
	}
	if current.ID == "" {
		current.ID = conversation.NewID("conv_smart_")
		current.CreatedAt = now
		current.LogicalModelID = d.aliasName
	}
	internalEvidence := domain.NewAffinityEvidence(
		"smartapi.conversation", domain.AffinityConversation, current.ID, domain.ConfHigh)
	hasInternal := false
	for _, item := range d.evidence {
		hasInternal = hasInternal || item.Scope == internalEvidence.Scope && item.KeyHash == internalEvidence.KeyHash
	}
	if !hasInternal {
		d.evidence = append([]domain.AffinityEvidence{internalEvidence}, d.evidence...)
	}
	if rc.Protocol == domain.ProtocolOpenAIResponses && !hasExplicitResponsesContinuation(rc) {
		// An affinity-only Responses request owns and carries its full replay.
		// Replace the prior snapshot so a later explicit continuation cannot
		// inherit duplicate client history accumulated across tool rounds.
		current.Items = append([]domain.Message(nil), rc.Messages...)
	} else {
		current.Items = append(current.Items, rc.Messages...)
	}
	if len(output) > 0 {
		current.Items = append(current.Items, domain.Message{Role: domain.RoleAssistant, Blocks: output})
	}
	current.CurrentProviderID = target.ProviderID
	current.CurrentUpstreamModel = target.Model
	current.CurrentConnectionID = target.ConnectionID
	current.ContinuityMode = continuity
	current.LastActiveAt = now
	current.LastRouteFingerprint = routeFingerprint(d.route)

	responseID := d.responseID
	if responseID == "" {
		responseID = conversation.NewID("resp_smart_internal_")
	}
	parentID := ""
	if rc.Lineage != nil {
		parentID = rc.Lineage.PreviousResponseID
	}
	response := domain.SmartResponse{
		ID: responseID, ConversationID: current.ID, ParentResponseID: parentID,
		RequestID: d.rec.ID, CanonicalOutput: append([]domain.Block(nil), output...),
		ProviderID: target.ProviderID, UpstreamModel: target.Model, ConnectionID: target.ConnectionID,
		NativeResponseID: nativeResponseID, ContinuityMode: continuity, CreatedAt: now,
	}
	var handle *domain.NativeHandle
	if nativeResponseID != "" {
		handle = &domain.NativeHandle{
			ConversationID: current.ID, ProviderID: target.ProviderID, ConnectionID: target.ConnectionID,
			NativeConversationID: nativeConversationID, NativeResponseID: nativeResponseID,
			StateScope: "CONNECTION", CreatedAt: now, LastUsedAt: now,
		}
	}
	var conversationHashes []string
	for _, item := range d.evidence {
		if item.Scope == domain.AffinityConversation {
			conversationHashes = append(conversationHashes, item.KeyHash)
		}
	}
	if err := store.SaveCompletion(current, response, handle, conversationHashes); err != nil {
		return err
	}
	d.conversation = &current
	return nil
}

func (d *decision) bindSuccessfulAffinity(cache *affinityCache, target *domain.CompiledTarget, attempts int) {
	if !d.v2 || cache == nil || target == nil {
		return
	}
	now := time.Now()
	for _, evidence := range d.evidence {
		switch evidence.Scope {
		case domain.AffinityConversation, domain.AffinityCache:
			cache.bind(evidence, d.aliasName, d.route, target, now, true)
		case domain.AffinityCaller:
			// A one-off fallback must not retarget the caller globally. Direct
			// successes may seed a caller binding; existing bindings are touched
			// but never overwritten here.
			if attempts == 1 {
				cache.bind(evidence, d.aliasName, d.route, target, now, false)
			}
		}
	}
}

func shouldPersistConversation(evidence []domain.AffinityEvidence, protocol domain.Protocol) bool {
	if protocol == domain.ProtocolOpenAIResponses {
		return true
	}
	for _, item := range evidence {
		if item.Scope == domain.AffinityConversation && strings.TrimSpace(item.KeyHash) != "" {
			return true
		}
	}
	return false
}
