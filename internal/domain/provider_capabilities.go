package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Scheduler-facing capability names are centralized here so discovery,
// persistence, compilation, simulation, and live routing cannot drift into
// separate vocabularies. ContextWindow and MaxOutput remain authoritative
// configuration values; the remaining boolean capabilities may be refined by
// fresh endpoint evidence.
const (
	CapabilityText             = "text"
	CapabilityImages           = "images"
	CapabilityStream           = "stream"
	CapabilityTools            = "tools"
	CapabilityParallelTools    = "parallel_tools"
	CapabilityReasoning        = "reasoning"
	CapabilityStructuredOutput = "structured_output"
	CapabilityVision           = "vision"
	CapabilityPriority         = "priority"
	CapabilityContextWindow    = "context_window"
	CapabilityMaxOutput        = "max_output"
)

// CapabilityRegistry returns every field in Capabilities in declaration
// order. A reflection test guards this list when the contract grows.
func CapabilityRegistry() []string {
	return []string{
		CapabilityText,
		CapabilityImages,
		CapabilityStream,
		CapabilityTools,
		CapabilityParallelTools,
		CapabilityReasoning,
		CapabilityStructuredOutput,
		CapabilityVision,
		CapabilityPriority,
		CapabilityContextWindow,
		CapabilityMaxOutput,
	}
}

// BooleanCapabilityRegistry is the subset for which a probe can provide a
// deterministic supported/unsupported observation. Numeric limits require an
// authoritative provider declaration instead of a potentially expensive or
// unsafe boundary search.
func BooleanCapabilityRegistry() []string {
	return []string{
		CapabilityText,
		CapabilityImages,
		CapabilityStream,
		CapabilityTools,
		CapabilityParallelTools,
		CapabilityReasoning,
		CapabilityStructuredOutput,
		CapabilityVision,
		CapabilityPriority,
	}
}

// NormalizeBooleanCapabilityName returns the canonical scheduler-facing name
// for a boolean capability. All configuration, routing-policy, simulation,
// discovery, and runtime consumers should use this function instead of
// maintaining their own allowlists.
func NormalizeBooleanCapabilityName(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, capability := range BooleanCapabilityRegistry() {
		if name == capability {
			return capability, true
		}
	}
	return "", false
}

// BooleanCapability reports one capability bit by canonical or normalized
// name. The second return value is false for numeric or unknown capabilities.
func BooleanCapability(capabilities Capabilities, name string) (bool, bool) {
	capability, ok := NormalizeBooleanCapabilityName(name)
	if !ok {
		return false, false
	}
	switch capability {
	case CapabilityText:
		return capabilities.Text, true
	case CapabilityImages:
		return capabilities.Images, true
	case CapabilityStream:
		return capabilities.Stream, true
	case CapabilityTools:
		return capabilities.Tools, true
	case CapabilityParallelTools:
		return capabilities.ParallelTools, true
	case CapabilityReasoning:
		return capabilities.Reasoning, true
	case CapabilityStructuredOutput:
		return capabilities.StructuredOutput, true
	case CapabilityVision:
		return capabilities.Vision, true
	case CapabilityPriority:
		return capabilities.Priority, true
	default:
		return false, false
	}
}

// RequirementCapability reports whether a request or policy requires one
// boolean capability. The second return value is false for unknown names.
func RequirementCapability(requirements Requirements, name string) (bool, bool) {
	capability, ok := NormalizeBooleanCapabilityName(name)
	if !ok {
		return false, false
	}
	switch capability {
	case CapabilityText:
		return requirements.Text, true
	case CapabilityImages:
		return requirements.Images, true
	case CapabilityStream:
		return requirements.Stream, true
	case CapabilityTools:
		return requirements.Tools, true
	case CapabilityParallelTools:
		return requirements.ParallelTools, true
	case CapabilityReasoning:
		return requirements.Reasoning, true
	case CapabilityStructuredOutput:
		return requirements.StructuredOutput, true
	case CapabilityVision:
		return requirements.Vision, true
	case CapabilityPriority:
		return requirements.Priority, true
	default:
		return false, false
	}
}

// SetRequirementCapability applies one named boolean requirement. It returns
// false for numeric or unknown capabilities so callers fail visibly when the
// shared contract grows.
func SetRequirementCapability(requirements *Requirements, name string, required bool) bool {
	if requirements == nil {
		return false
	}
	capability, ok := NormalizeBooleanCapabilityName(name)
	if !ok {
		return false
	}
	switch capability {
	case CapabilityText:
		requirements.Text = required
	case CapabilityImages:
		requirements.Images = required
	case CapabilityStream:
		requirements.Stream = required
	case CapabilityTools:
		requirements.Tools = required
	case CapabilityParallelTools:
		requirements.ParallelTools = required
	case CapabilityReasoning:
		requirements.Reasoning = required
	case CapabilityStructuredOutput:
		requirements.StructuredOutput = required
	case CapabilityVision:
		requirements.Vision = required
	case CapabilityPriority:
		requirements.Priority = required
	default:
		return false
	}
	return true
}

// MergeRequirements returns the union of two capability contracts. Numeric
// requirements retain the stricter (larger) minimum.
func MergeRequirements(left, right Requirements) Requirements {
	for _, capability := range BooleanCapabilityRegistry() {
		required, _ := RequirementCapability(right, capability)
		if required {
			SetRequirementCapability(&left, capability, true)
		}
	}
	if right.MinContext > left.MinContext {
		left.MinContext = right.MinContext
	}
	if right.MinOutput > left.MinOutput {
		left.MinOutput = right.MinOutput
	}
	return left
}

// MergeCapabilities returns the union of two capability views. Boolean
// capabilities are merged through the shared registry, while numeric limits
// retain the larger authoritative declaration. Keeping this in domain avoids
// compiler, inventory, and runtime views drifting when the contract grows.
func MergeCapabilities(left, right Capabilities) Capabilities {
	for _, capability := range BooleanCapabilityRegistry() {
		supported, _ := BooleanCapability(right, capability)
		if supported {
			SetBooleanCapability(&left, capability, true)
		}
	}
	if right.ContextWindow > left.ContextWindow {
		left.ContextWindow = right.ContextWindow
	}
	if right.MaxOutput > left.MaxOutput {
		left.MaxOutput = right.MaxOutput
	}
	return left
}

// EffectiveCapabilities returns the capability contract Ferry can
// guarantee to clients after applying its verified semantic mappings. The
// upstream capability evidence remains unchanged and must still be used when
// deciding whether an attempt needs a mapping.
//
// Parallel tool calls are externally supported whenever ordinary tool calls
// are supported: Ferry can preserve the request semantics by executing the
// calls serially. No other capability currently has a verified equivalent
// mapping, so every other field remains fail-closed.
func EffectiveCapabilities(upstream Capabilities) Capabilities {
	effective := upstream
	effective.ParallelTools = upstream.Tools
	return effective
}

// Contains reports whether the actual request requirements include every
// capability predicate in wanted. It is used by policy matchers, not target
// eligibility (which is Capabilities.Satisfies).
func (requirements Requirements) Contains(wanted Requirements) bool {
	for _, capability := range BooleanCapabilityRegistry() {
		wants, _ := RequirementCapability(wanted, capability)
		has, _ := RequirementCapability(requirements, capability)
		if wants && !has {
			return false
		}
	}
	return (wanted.MinContext == 0 || requirements.MinContext >= wanted.MinContext) &&
		(wanted.MinOutput == 0 || requirements.MinOutput >= wanted.MinOutput)
}

// ProbeCapabilitiesForProtocol is the smallest complete evidence set for one
// native protocol. Protocol contracts that Ferry cannot encode are omitted
// from probing and applied deterministically by ProtocolCapabilityBaseline.
// Priority is deliberately excluded: accepting a parameter (or timing a
// request) cannot prove paid priority processing. It needs an authoritative
// account catalog or an operator declaration for the exact endpoint.
func ProbeCapabilitiesForProtocol(protocol Protocol) []string {
	switch protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		return []string{
			CapabilityText, CapabilityStream, CapabilityTools, CapabilityParallelTools,
			CapabilityReasoning, CapabilityStructuredOutput, CapabilityVision,
		}
	case ProtocolAnthropic:
		return []string{
			CapabilityText, CapabilityStream, CapabilityTools, CapabilityParallelTools,
			CapabilityReasoning, CapabilityStructuredOutput, CapabilityVision,
		}
	case ProtocolOpenAIImages:
		return []string{CapabilityImages}
	default:
		return nil
	}
}

// SetBooleanCapability applies one evidence observation. It returns false for
// numeric or unknown capabilities so callers cannot silently pretend they
// handled a newly added contract field.
func SetBooleanCapability(capabilities *Capabilities, capability string, supported bool) bool {
	if capabilities == nil {
		return false
	}
	capability, ok := NormalizeBooleanCapabilityName(capability)
	if !ok {
		return false
	}
	switch capability {
	case CapabilityText:
		capabilities.Text = supported
	case CapabilityImages:
		capabilities.Images = supported
	case CapabilityStream:
		capabilities.Stream = supported
	case CapabilityTools:
		capabilities.Tools = supported
	case CapabilityParallelTools:
		capabilities.ParallelTools = supported
	case CapabilityReasoning:
		capabilities.Reasoning = supported
	case CapabilityStructuredOutput:
		capabilities.StructuredOutput = supported
	case CapabilityVision:
		capabilities.Vision = supported
	case CapabilityPriority:
		capabilities.Priority = supported
	default:
		return false
	}
	return true
}

// ProtocolCapabilityBaseline applies facts imposed by Ferry's current wire
// adapters rather than by an individual model. Structured output is an
// endpoint/model capability on all three text protocols; encoding support
// alone must never turn an unknown capability into declared support.
func ProtocolCapabilityBaseline(protocol Protocol, capabilities Capabilities) Capabilities {
	if protocol != ProtocolOpenAIChat && protocol != ProtocolOpenAIResponses {
		capabilities.Priority = false
	}
	return capabilities
}

// ProviderProtocolEndpointURL resolves the exact request URL used as part of
// an evidence key. Keeping it shared with runtime compilation prevents probe
// results from being applied to a sibling protocol endpoint accidentally.
func ProviderProtocolEndpointURL(rawBase, rawFull string, protocol Protocol) (string, error) {
	suffix := "/chat/completions"
	switch protocol {
	case ProtocolOpenAIResponses:
		suffix = "/responses"
	case ProtocolAnthropic:
		suffix = "/messages"
	case ProtocolOpenAIImages:
		suffix = "/images/generations"
	}
	return (&Endpoint{BaseURL: rawBase, FullURL: rawFull, Protocol: protocol}).RequestURL(suffix)
}

// ProviderCapabilityEndpointFingerprint deliberately excludes credentials and
// hides the raw endpoint URL from the evidence table.
func ProviderCapabilityEndpointFingerprint(providerType ProviderType, endpoint string) string {
	digest := sha256.Sum256([]byte(string(providerType) + "\n" + strings.TrimSpace(endpoint)))
	return hex.EncodeToString(digest[:])
}

// ProviderCapabilityStoreProviderID gives unsaved Provider editors a bounded
// endpoint-scoped cache without allowing that anonymous evidence to affect a
// configured Provider at runtime.
func ProviderCapabilityStoreProviderID(providerID, endpointFingerprint string) string {
	if providerID = strings.TrimSpace(providerID); providerID != "" {
		return providerID
	}
	return "endpoint:" + endpointFingerprint
}
