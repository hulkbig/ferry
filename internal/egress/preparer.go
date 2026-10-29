package egress

import (
	"bytes"
	"encoding/json"

	"ferry/internal/domain"
)

// Preparer is implemented by adapters that can translate a canonical request
// into their wire format without sending it. The scheduler uses it to assess
// protocol compatibility (PRD §6.10): the returned notes enumerate every
// lossy, approximate, or defaulted mapping. Any note is degraded and rejected
// by the default policy; an empty note list means exact/mapped fidelity.
type Preparer interface {
	// Prepare builds the wire payload for rc and returns compatibility
	// notes. The payload is discarded; only diagnostics survive.
	Prepare(rc *domain.RequestContract) ([]string, error)
}

// Preparation describes the wire payload selected by the actual adapter.
// ToolTranscriptPreserved is true only when the emitted Responses input is
// identical to the ingress input; it is never inferred from a route label.
type Preparation struct {
	Notes                   []string
	ToolTranscriptPreserved bool
}

type DetailedPreparer interface {
	PrepareDetailed(rc *domain.RequestContract) (Preparation, error)
}

func nativeResponsesInputPreserved(rc *domain.RequestContract, payload []byte) bool {
	if rc == nil || rc.Origin != domain.ProtocolOpenAIResponses || len(rc.Native) == 0 {
		return false
	}
	var original, outgoing map[string]json.RawMessage
	if json.Unmarshal(rc.Native, &original) != nil || json.Unmarshal(payload, &outgoing) != nil {
		return false
	}
	input := original["input"]
	encoded := outgoing["input"]
	if len(input) == 0 || len(encoded) == 0 {
		return false
	}
	var source, target bytes.Buffer
	return json.Compact(&source, input) == nil && json.Compact(&target, encoded) == nil &&
		bytes.Equal(source.Bytes(), target.Bytes())
}

// Prepare implements Preparer for the OpenAI Chat adapter.
func (o *OpenAIChat) Prepare(rc *domain.RequestContract) ([]string, error) {
	_, notes, err := o.buildRequest(rc)
	return notes, err
}

// Prepare implements Preparer for the OpenAI Responses adapter.
func (r *OpenAIResponses) Prepare(rc *domain.RequestContract) ([]string, error) {
	prepared, err := r.PrepareDetailed(rc)
	return prepared.Notes, err
}

func (r *OpenAIResponses) PrepareDetailed(rc *domain.RequestContract) (Preparation, error) {
	payload, notes, err := r.buildRequest(rc)
	return Preparation{Notes: notes, ToolTranscriptPreserved: err == nil &&
		nativeResponsesInputPreserved(rc, payload)}, err
}

// Prepare implements Preparer for the exact/native-only OpenAI Images adapter.
func (o *OpenAIImages) Prepare(rc *domain.RequestContract) ([]string, error) {
	_, _, err := o.buildRequest(rc)
	return nil, err
}

// Prepare implements Preparer for the Anthropic adapter.
func (a *Anthropic) Prepare(rc *domain.RequestContract) ([]string, error) {
	_, notes, err := a.buildRequest(rc)
	return notes, err
}
