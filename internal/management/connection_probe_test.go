package management

import (
	"net/http"
	"testing"
	"time"

	"ferry/internal/domain"
)

type connectionProbeRoundTripper struct{}

func (*connectionProbeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

func TestConnectionTestContractUsesProviderCompatibleTextBudget(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic} {
		contract := connectionTestContract(protocol, "text-model")
		if contract.MaxTokens != connectionTestTextMaxTokens || contract.MaxTokens < 16 {
			t.Fatalf("%s connection test max_tokens=%d", protocol, contract.MaxTokens)
		}
	}
}

func TestConnectionTestContractUsesImageGenerationForImageModels(t *testing.T) {
	contract := connectionTestContract(domain.ProtocolOpenAIImages, "gpt-image-2")
	if contract.Protocol != domain.ProtocolOpenAIImages || contract.Operation != "generation" ||
		contract.Stream || contract.MaxTokens != 0 || len(contract.Messages) != 1 ||
		len(contract.Messages[0].Blocks) != 1 || contract.Messages[0].Blocks[0].Text == "" {
		t.Fatalf("image connection test contract=%+v", contract)
	}
}

func TestConnectionTestHTTPClientUsesLiveInferenceTransport(t *testing.T) {
	transport := &connectionProbeRoundTripper{}
	live := &http.Client{Transport: transport}
	client := connectionTestHTTPClient(func(connectionID string) *http.Client {
		if connectionID != "connection-a" {
			t.Fatalf("connection id=%q", connectionID)
		}
		return live
	}, "connection-a", 15*time.Second)
	if client == live {
		t.Fatal("probe must use its own bounded client")
	}
	if client.Transport != live.Transport {
		t.Fatal("probe did not reuse the live inference transport")
	}
	if client.Timeout != 15*time.Second {
		t.Fatalf("probe timeout=%s", client.Timeout)
	}
}
