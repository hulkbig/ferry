package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

// CapabilityStatus separates missing evidence from a negative observation.
// The persisted bool contract remains backward compatible: true is declared
// support, while false alone cannot prove that an endpoint rejected a feature.
type CapabilityStatus string

const (
	CapabilityUnknown     CapabilityStatus = "unknown"
	CapabilitySupported   CapabilityStatus = "supported"
	CapabilityUnsupported CapabilityStatus = "unsupported"
)

type CapabilityStatuses map[string]CapabilityStatus

func DeclaredCapabilityStatuses(caps Capabilities, protocol Protocol) CapabilityStatuses {
	states := CapabilityStatuses{}
	for _, name := range BooleanCapabilityRegistry() {
		states[name] = CapabilityUnknown
		if supported, _ := BooleanCapability(caps, name); supported {
			states[name] = CapabilitySupported
		}
	}
	if protocol == ProtocolOpenAIImages {
		for _, name := range BooleanCapabilityRegistry() {
			if name != CapabilityImages {
				states[name] = CapabilityUnsupported
			}
		}
	} else {
		states[CapabilityImages] = CapabilityUnsupported
	}
	return states
}

func (t *CompiledTarget) CapabilityStatusesFor(protocol Protocol) CapabilityStatuses {
	states := DeclaredCapabilityStatuses(t.UpstreamCapabilitiesFor(protocol), protocol)
	for name, state := range t.ProtocolCapabilityStatuses[protocol] {
		states[name] = state
	}
	// Preserve the existing, verified serial-tool compatibility mapping.
	if states[CapabilityTools] == CapabilitySupported {
		states[CapabilityParallelTools] = CapabilitySupported
	}
	return states
}

// CapabilityVerification is request-local evidence. It never grants a feature
// merely because a verifier returned no error.
type CapabilityVerification struct {
	Capabilities Capabilities
	Statuses     CapabilityStatuses
	Error        string
	Probes       int
}

// Automatic observations belong to one connection incarnation and credential
// binding. A sibling account on the same URL must not inherit them.
func ConnectionCapabilityFingerprint(endpointFingerprint string, connection *Connection) string {
	if connection == nil {
		return endpointFingerprint
	}
	sum := sha256.Sum256([]byte(endpointFingerprint + "\x00" + connection.ID + "\x00" + connection.RuntimeID + "\x00" + connection.Credential.SecretRef))
	return "connection:" + hex.EncodeToString(sum[:])
}
