package egress

import (
	"encoding/json"
	"testing"

	"ferry/internal/domain"
)

func TestOpenAIServiceTierSurvivesNativeAndConvertedRequests(t *testing.T) {
	base := testBase(nil, "")
	adapters := map[string]func(*domain.RequestContract) ([]byte, []string, error){
		"chat":      NewOpenAIChat(base).buildRequest,
		"responses": NewOpenAIResponses(base).buildRequest,
		"codex":     NewCodexCaller(base, "test-account").buildRequest,
	}
	for _, origin := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses} {
		for name, build := range adapters {
			for _, tier := range []string{"priority", "fast", "auto", "default", "flex"} {
				t.Run(string(origin)+"/"+name+"/"+tier, func(t *testing.T) {
					rc := chatRC()
					rc.Protocol, rc.Origin, rc.ServiceTier = origin, origin, tier
					raw := map[string]any{"model": "m", "service_tier": tier}
					if origin == domain.ProtocolOpenAIChat {
						raw["messages"] = []map[string]string{{"role": "user", "content": "hi"}}
					} else {
						raw["input"] = "hi"
					}
					rc.Native, _ = json.Marshal(raw)
					body, _, err := build(rc)
					if err != nil {
						t.Fatal(err)
					}
					var request map[string]any
					if err := json.Unmarshal(body, &request); err != nil {
						t.Fatal(err)
					}
					want := tier
					if name == "codex" && tier == "fast" {
						want = "priority"
					}
					if request["service_tier"] != want {
						t.Fatalf("lost tier: %s", body)
					}
				})
			}
		}
	}
}

func TestOpenAIServiceTierCannotBeDroppedByAnthropicConversion(t *testing.T) {
	rc := chatRC()
	rc.ServiceTier = "priority"
	if _, _, err := NewAnthropic(testBase(nil, "")).buildRequest(rc); err == nil {
		t.Fatal("priority must not silently become an ordinary Anthropic request")
	}
}

func TestNormalizedBlankServiceTierIsOmittedFromNativeRequests(t *testing.T) {
	base := testBase(nil, "")
	for name, adapter := range map[string]struct {
		origin domain.Protocol
		build  func(*domain.RequestContract) ([]byte, []string, error)
	}{
		"chat":      {domain.ProtocolOpenAIChat, NewOpenAIChat(base).buildRequest},
		"responses": {domain.ProtocolOpenAIResponses, NewOpenAIResponses(base).buildRequest},
		"codex":     {domain.ProtocolOpenAIResponses, NewCodexCaller(base, "test-account").buildRequest},
	} {
		t.Run(name, func(t *testing.T) {
			rc := chatRC()
			rc.Protocol, rc.Origin = adapter.origin, adapter.origin
			rc.ServiceTier = ""
			rc.Native = json.RawMessage(`{"model":"m","service_tier":"  ","messages":[{"role":"user","content":"hi"}],"input":"hi"}`)
			body, _, err := adapter.build(rc)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			if _, ok := raw["service_tier"]; ok {
				t.Fatalf("blank tier survived normalization: %s", body)
			}
		})
	}
}
