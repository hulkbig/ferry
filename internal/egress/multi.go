package egress

import (
	"context"
	"fmt"

	"ferry/internal/domain"
)

// ProtocolPicker selects the wire format used to reach a target for one
// request. Channel-expanded targets pass CompiledTarget.EgressProtocol:
// native passthrough when the model speaks the ingress format, otherwise its
// least-lossy native format (canonical-contract conversion fills the gap).
type ProtocolPicker func(rc *domain.RequestContract) domain.Protocol

// MultiCaller serves one upstream model that speaks several native wire
// formats, choosing the adapter per request instead of per target. This is
// what makes a single channel configuration answer openai-chat, responses,
// and anthropic ingress alike: each request is dispatched to the adapter for
// the protocol the picker selected, so native formats pass through and
// foreign formats convert through the canonical contract.
//
// All adapters share one Base — endpoint URL and credential are per-target,
// not per-protocol (each adapter appends its own path).
type MultiCaller struct {
	adapters map[domain.Protocol]Caller
	pick     ProtocolPicker
}

var (
	_ Caller   = (*MultiCaller)(nil)
	_ Preparer = (*MultiCaller)(nil)
)

// NewMultiCaller builds one adapter per listed protocol around a shared Base.
// The picker decides per request which of them serves the call.
func NewMultiCaller(b Base, protos []domain.Protocol, pick ProtocolPicker) *MultiCaller {
	m := &MultiCaller{
		adapters: make(map[domain.Protocol]Caller, len(protos)),
		pick:     pick,
	}
	for _, p := range protos {
		m.adapters[p] = NewCaller(b, p)
	}
	return m
}

// forRequest resolves the adapter for rc's wire format.
func (m *MultiCaller) forRequest(rc *domain.RequestContract) (Caller, error) {
	c, ok := m.adapters[m.pick(rc)]
	if !ok {
		return nil, fmt.Errorf("no egress adapter for protocol %q", m.pick(rc))
	}
	return c, nil
}

// Invoke performs a non-streaming call over the request's selected format.
func (m *MultiCaller) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	c, err := m.forRequest(rc)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: err.Error()}
	}
	return c.Invoke(ctx, rc)
}

// Stream performs a streaming call over the request's selected format.
func (m *MultiCaller) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	c, err := m.forRequest(rc)
	if err != nil {
		return nil, nil, &domain.Failure{Type: domain.FailBadResponse, Message: err.Error()}
	}
	return c.Stream(ctx, rc)
}

// Prepare previews the conversion notes of the format this request would
// actually use — the scheduler's compatibility assessment must describe the
// wire that will be hit, not an arbitrary fixed one.
func (m *MultiCaller) Prepare(rc *domain.RequestContract) ([]string, error) {
	prepared, err := m.PrepareDetailed(rc)
	return prepared.Notes, err
}

func (m *MultiCaller) PrepareDetailed(rc *domain.RequestContract) (Preparation, error) {
	c, err := m.forRequest(rc)
	if err != nil {
		return Preparation{}, err
	}
	if detailed, ok := c.(DetailedPreparer); ok {
		return detailed.PrepareDetailed(rc)
	}
	p, ok := c.(Preparer)
	if !ok {
		return Preparation{}, fmt.Errorf("adapter for %q cannot preview protocol conversion", m.pick(rc))
	}
	notes, err := p.Prepare(rc)
	return Preparation{Notes: notes}, err
}
