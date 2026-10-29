package domain

import "testing"

func TestNormalizeProviderModelCapabilitiesDoesNotGuessCustomModelFeatures(t *testing.T) {
	declared := Capabilities{Text: true, Stream: true, Tools: true, StructuredOutput: true, MaxOutput: 8192}
	got := NormalizeProviderModelCapabilities(
		ProviderCustom, "glm-5.3-flash", []Protocol{ProtocolOpenAIChat}, declared)
	if got.Reasoning || !got.StructuredOutput || got.MaxOutput != 8192 {
		t.Fatalf("normalized GLM capabilities = %+v", got)
	}
	kimi := NormalizeProviderModelCapabilities(
		ProviderCustom, "kimi-for-coding", []Protocol{ProtocolOpenAIResponses}, Capabilities{})
	if kimi.Text || kimi.Stream || kimi.Tools || kimi.ParallelTools || kimi.Reasoning ||
		kimi.StructuredOutput || kimi.Vision || kimi.ContextWindow != 0 || kimi.MaxOutput != 0 {
		t.Fatalf("custom model name inferred unsupported facts: %+v", kimi)
	}

	image := NormalizeProviderModelCapabilities(
		ProviderCustom, "gpt-image-2", []Protocol{ProtocolOpenAIImages}, Capabilities{})
	if !image.Images || image.Text || image.Stream || image.Tools || image.Reasoning {
		t.Fatalf("image protocol inherited text-model capabilities: %+v", image)
	}

	codex := NormalizeProviderModelCapabilities(
		ProviderCliproxyCodex, "gpt-5.6-sol", []Protocol{ProtocolOpenAIResponses}, Capabilities{Text: true})
	if !codex.Stream || !codex.Tools || !codex.ParallelTools || !codex.Reasoning ||
		!codex.StructuredOutput || codex.ContextWindow != 372000 {
		t.Fatalf("Codex authoritative capabilities changed: %+v", codex)
	}
}
