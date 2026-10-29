package egress

import (
	"encoding/json"
	"testing"

	"ferry/internal/domain"
)

// auxContract mirrors what each ingress adapter now produces for a request
// carrying auxiliary native features: no blocking ConversionIssues, a
// MappingNotes trail, and canonical verbosity.
func auxContract(origin domain.Protocol, stream bool) *domain.RequestContract {
	return &domain.RequestContract{
		Protocol: origin, Origin: origin, Model: "m", Stream: stream,
		MaxTokens: 128, Verbosity: "low",
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{
			{Kind: domain.BlockText, Text: "hi"}}}},
		MappingNotes: []string{"cache_control omitted (target applies automatic prompt caching)"},
	}
}

// Every cross-protocol direction must translate a request carrying auxiliary
// native features instead of rejecting it: the full-translation contract.
// Auxiliary features surface to the client through the scheduler's mapped
// compatibility notes (rc.MappingNotes), never through adapter diagnostics.
func TestAuxiliaryFeaturesTranslateAcrossAllProtocolDirections(t *testing.T) {
	targets := []struct {
		name   string
		caller func() Caller
	}{
		{"openai-chat", func() Caller {
			return NewCaller(Base{Endpoint: &domain.Endpoint{ID: "t", BaseURL: "https://up.test/v1", Protocol: domain.ProtocolOpenAIChat}}, domain.ProtocolOpenAIChat)
		}},
		{"openai-responses", func() Caller {
			return NewCaller(Base{Endpoint: &domain.Endpoint{ID: "t", BaseURL: "https://up.test/v1", Protocol: domain.ProtocolOpenAIResponses}}, domain.ProtocolOpenAIResponses)
		}},
		{"anthropic", func() Caller {
			return NewCaller(Base{Endpoint: &domain.Endpoint{ID: "t", BaseURL: "https://up.test", Protocol: domain.ProtocolAnthropic}}, domain.ProtocolAnthropic)
		}},
	}
	for _, origin := range []domain.Protocol{domain.ProtocolAnthropic, domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses} {
		for _, target := range targets {
			if target.name == string(origin) {
				continue
			}
			preparer := target.caller().(Preparer)
			contract := auxContract(origin, false)
			if _, err := preparer.Prepare(contract); err != nil {
				t.Fatalf("%s → %s: auxiliary features blocked conversion: %v", origin, target.name, err)
			}
		}
	}
}

func TestVerbosityMapsToEveryNativeKnob(t *testing.T) {
	chat := NewCaller(Base{Endpoint: &domain.Endpoint{ID: "t", BaseURL: "https://up.test/v1", Protocol: domain.ProtocolOpenAIChat}}, domain.ProtocolOpenAIChat).(*OpenAIChat)
	payload, _, err := chat.buildRequest(auxContract(domain.ProtocolAnthropic, false))
	if err != nil {
		t.Fatalf("anthropic → chat: %v", err)
	}
	var chatBody map[string]any
	if json.Unmarshal(payload, &chatBody) != nil || chatBody["verbosity"] != "low" {
		t.Fatalf("chat payload verbosity=%v body=%s", chatBody["verbosity"], payload)
	}

	responses := NewCaller(Base{Endpoint: &domain.Endpoint{ID: "t", BaseURL: "https://up.test/v1", Protocol: domain.ProtocolOpenAIResponses}}, domain.ProtocolOpenAIResponses).(*OpenAIResponses)
	payload, _, err = responses.buildRequest(auxContract(domain.ProtocolAnthropic, false))
	if err != nil {
		t.Fatalf("anthropic → responses: %v", err)
	}
	var responsesBody struct {
		Text *struct {
			Verbosity string `json:"verbosity"`
		} `json:"text"`
	}
	if json.Unmarshal(payload, &responsesBody) != nil || responsesBody.Text == nil || responsesBody.Text.Verbosity != "low" {
		t.Fatalf("responses payload verbosity=%+v body=%s", responsesBody.Text, payload)
	}

	anthropic := NewCaller(Base{Endpoint: &domain.Endpoint{ID: "t", BaseURL: "https://up.test", Protocol: domain.ProtocolAnthropic}}, domain.ProtocolAnthropic).(*Anthropic)
	payload, _, err = anthropic.buildRequest(auxContract(domain.ProtocolOpenAIChat, false))
	if err != nil {
		t.Fatalf("chat → anthropic: %v", err)
	}
	var anthropicBody struct {
		OutputConfig *struct {
			Text *struct {
				Verbosity string `json:"verbosity"`
			} `json:"text"`
		} `json:"output_config"`
	}
	if json.Unmarshal(payload, &anthropicBody) != nil || anthropicBody.OutputConfig == nil ||
		anthropicBody.OutputConfig.Text == nil || anthropicBody.OutputConfig.Text.Verbosity != "low" {
		t.Fatalf("anthropic payload output_config=%+v body=%s", anthropicBody.OutputConfig, payload)
	}
}
