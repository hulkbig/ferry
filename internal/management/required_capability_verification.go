package management

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

type capabilityFlight struct {
	done    chan struct{}
	result  domain.CapabilityVerification
	expires time.Time
}

func (s *Server) invalidateCapabilityEvidence(providerIDs map[string]bool) error {
	if len(providerIDs) == 0 {
		return nil
	}
	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	s.capabilityEpoch++
	s.capabilityFlights = nil
	for id := range providerIDs {
		if err := s.deps.Control.DeleteProviderCapabilityEvidence(id); err != nil {
			return err
		}
	}
	return nil
}

// VerifyRequiredCapabilities uses the already-resolved live caller, including
// Codex OAuth. It never reconstructs authentication or forwards user content.
func (s *Server) VerifyRequiredCapabilities(ctx context.Context, provider *domain.Provider, connection *domain.Connection, target *domain.CompiledTarget, caller egress.Caller, protocol domain.Protocol, required domain.Requirements) domain.CapabilityVerification {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	base := target.UpstreamCapabilitiesFor(protocol)
	for _, model := range connection.Models {
		if model.UpstreamModel == target.Model {
			base = domain.ProtocolCapabilityBaseline(protocol, domain.NormalizeProviderModelCapabilities(provider.Type, model.UpstreamModel, []domain.Protocol{protocol}, model.Capabilities))
			break
		}
	}
	result := domain.CapabilityVerification{Capabilities: base, Statuses: domain.DeclaredCapabilityStatuses(base, protocol)}
	endpoint, err := domain.ProviderProtocolEndpointURL(target.Endpoint.BaseURL, target.Endpoint.FullURL, protocol)
	if err != nil {
		result.Error = "capability endpoint is invalid"
		return result
	}
	fingerprint := domain.ProviderCapabilityEndpointFingerprint(provider.Type, endpoint)
	scoped := domain.ConnectionCapabilityFingerprint(fingerprint, connection)
	// A new live caller must not inherit transient/backoff state from a
	// replaced credential. Durable evidence is scoped to its binding below.
	s.capabilityMu.Lock()
	epoch := s.capabilityEpoch
	key := fmt.Sprintf("%s/%s/%s/%p/%d/%+v/%+v", scoped, target.Model, protocol, caller, epoch, base, required)
	if s.capabilityFlights == nil {
		s.capabilityFlights = map[string]*capabilityFlight{}
	}
	if s.capabilitySlots == nil {
		s.capabilitySlots = make(chan struct{}, 2)
	}
	now := time.Now()
	for id, flight := range s.capabilityFlights {
		if !flight.expires.IsZero() && !flight.expires.After(now) {
			delete(s.capabilityFlights, id)
		}
	}
	if flight := s.capabilityFlights[key]; flight != nil {
		s.capabilityMu.Unlock()
		select {
		case <-ctx.Done():
			result.Error = "capability verification cancelled"
			return result
		case <-flight.done:
			copy := flight.result
			copy.Statuses = maps.Clone(copy.Statuses)
			copy.Probes = 0
			return copy
		}
	}
	flight := &capabilityFlight{done: make(chan struct{})}
	s.capabilityFlights[key] = flight
	s.capabilityMu.Unlock()
	result = s.verifyRequiredCapabilities(ctx, provider, target, caller, protocol, required, result, fingerprint, scoped, epoch)
	s.capabilityMu.Lock()
	if epoch != s.capabilityEpoch {
		result.Capabilities = base
		result.Statuses = domain.DeclaredCapabilityStatuses(base, protocol)
		result.Error = "credential changed during capability verification"
	}
	flight.result = result
	// Short memory caching also backs off inconclusive probes. It is not
	// negative capability evidence; the durable cache has its own expiry.
	flight.expires = time.Now().Add(15 * time.Second)
	close(flight.done)
	s.capabilityMu.Unlock()
	return result
}

func (s *Server) verifyRequiredCapabilities(ctx context.Context, provider *domain.Provider, target *domain.CompiledTarget, caller egress.Caller, protocol domain.Protocol, required domain.Requirements, result domain.CapabilityVerification, legacy, scoped string, epoch uint64) domain.CapabilityVerification {
	if s.deps.Control == nil || s.deps.Manager == nil {
		result.Error = "capability verification is unavailable"
		return result
	}
	wanted := []string{}
	for _, name := range domain.ProbeCapabilitiesForProtocol(protocol) {
		if name == domain.CapabilityParallelTools {
			continue // The serial compatibility path requires tools evidence.
		}
		need, _ := domain.RequirementCapability(required, name)
		if name == domain.CapabilityTools {
			need = need || required.ParallelTools
		}
		if !need {
			continue
		}
		item, err := s.deps.Control.ProviderCapabilityEvidence(provider.ID, scoped, target.Model, protocol, name, time.Now())
		if err == nil && item == nil {
			item, err = s.deps.Control.ProviderCapabilityEvidence(provider.ID, legacy, target.Model, protocol, name, time.Now())
		}
		if err != nil {
			result.Error = "capability evidence store is unavailable"
			return result
		}
		if item != nil {
			domain.SetBooleanCapability(&result.Capabilities, name, item.Supported)
			result.Statuses[name] = domain.CapabilityUnsupported
			if item.Supported {
				result.Statuses[name] = domain.CapabilitySupported
			}
		}
		if result.Statuses[name] == domain.CapabilityUnknown {
			wanted = append(wanted, name)
		}
		if result.Statuses[name] == domain.CapabilityUnsupported {
			return result
		}
	}
	// Fast requires account evidence; image generation is never triggered as
	// an automatic side effect. Vision needs a dedicated semantic test.
	pending := []string{}
	for _, name := range wanted {
		if name != domain.CapabilityImages && name != domain.CapabilityVision && name != domain.CapabilityParallelTools {
			pending = append(pending, name)
		}
	}
	if len(pending) == 0 {
		return result
	}
	select {
	case s.capabilitySlots <- struct{}{}:
		defer func() { <-s.capabilitySlots }()
	case <-ctx.Done():
		result.Error = "capability verification budget exhausted"
		return result
	}
	release, failure := s.deps.Manager.Admit(ctx, target.ID)
	if failure != nil {
		result.Error = "capability verification admission unavailable"
		return result
	}
	defer release()
	for _, name := range pending {
		if ctx.Err() != nil {
			result.Error = "capability verification budget exhausted"
			break
		}
		mode := providerProbeInvoke
		if required.Stream || protocol == domain.ProtocolOpenAIResponses && provider.Type == domain.ProviderCliproxyCodex {
			mode = providerProbeStream
		}
		var probeFailure *domain.Failure
		probes := 1
		switch name {
		case domain.CapabilityTools:
			probeFailure = probeProviderTools(ctx, caller, protocol, target.Model, false, mode)
		default:
			features := providerTextProbeFeatures{Reasoning: name == domain.CapabilityReasoning, StructuredOutput: name == domain.CapabilityStructuredOutput, RequireReasoningEvidence: name == domain.CapabilityReasoning}
			if name == domain.CapabilityStream {
				mode = providerProbeStream
			}
			probeFailure, probes = probeProviderTextFeatures(ctx, caller, protocol, target.Model, features, mode)
		}
		result.Probes += probes
		// A model ignoring a tool/schema instruction once is inconclusive.
		// Only explicit endpoint rejection or a known adapter limitation is
		// durable negative evidence, never a timeout, 429 or malformed answer.
		explicitRejection := probeFailure != nil && deterministicProviderCapabilityFailure(probeFailure) &&
			(probeFailure.Type == domain.FailProtocolConversion || probeFailure.HTTPStatus == 400 || probeFailure.HTTPStatus == 422)
		if probeFailure != nil && !explicitRejection {
			result.Error = "capability verification inconclusive: " + string(probeFailure.Type)
			switch probeFailure.Type {
			case domain.FailRateLimit, domain.FailQuotaExhausted, domain.FailAuth, domain.FailProvider5xx, domain.FailConnect:
				s.deps.Manager.RecordFailure(target.ID, probeFailure, time.Now())
			}
			// Do not fan out more requests after rate limiting/network failure.
			break
		}
		supported := probeFailure == nil
		s.capabilityMu.Lock()
		if epoch != s.capabilityEpoch {
			s.capabilityMu.Unlock()
			result.Error = "credential changed during capability verification"
			break
		}
		_, _, err := s.putProviderCapabilityEvidence(provider.ID, scoped, target.Model, protocol, name, supported, probeFailure, time.Now().UTC())
		s.capabilityMu.Unlock()
		if err != nil {
			result.Error = "capability evidence could not be saved"
			break
		}
		domain.SetBooleanCapability(&result.Capabilities, name, supported)
		result.Statuses[name] = domain.CapabilityUnsupported
		if supported {
			result.Statuses[name] = domain.CapabilitySupported
		}
		if !supported {
			break
		}
	}
	if result.Error == "" {
		unknown := []string{}
		for _, name := range domain.BooleanCapabilityRegistry() {
			need, _ := domain.RequirementCapability(required, name)
			if name == domain.CapabilityParallelTools && result.Capabilities.Tools {
				continue
			}
			if need && result.Statuses[name] == domain.CapabilityUnknown {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			result.Error = "capability evidence still required: " + strings.Join(unknown, ", ")
		}
	}
	return result
}
