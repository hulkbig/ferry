package management

import "ferry/internal/domain"

const connectionTestTextMaxTokens int64 = 32

// connectionTestContract selects a minimal real operation for the model's
// native capability. Image-only Codex accounts must be validated with an
// image generation request rather than a text prompt.
func connectionTestContract(protocol domain.Protocol, model string) *domain.RequestContract {
	contract := &domain.RequestContract{
		Protocol: protocol, Origin: protocol, Model: model, MaxTokens: connectionTestTextMaxTokens,
		Messages: []domain.Message{{
			Role:   domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: "Reply with OK."}},
		}},
	}
	if protocol == domain.ProtocolOpenAIImages {
		contract.Operation = "generation"
		contract.MaxTokens = 0
		contract.Messages[0].Blocks[0].Text = "A simple neutral test image."
	}
	return contract
}
