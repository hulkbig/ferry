package domain

import "testing"

func TestCapabilityStatusesDistinguishUnknownFromRejection(t *testing.T) {
	caps := Capabilities{Text: true, Tools: true}
	target := &CompiledTarget{Capabilities: caps, ProtocolCapabilityStatuses: map[Protocol]CapabilityStatuses{
		ProtocolOpenAIChat: {CapabilityReasoning: CapabilityUnsupported},
	}}
	chat := target.CapabilityStatusesFor(ProtocolOpenAIChat)
	if chat[CapabilityStream] != CapabilityUnknown || chat[CapabilityReasoning] != CapabilityUnsupported || chat[CapabilityParallelTools] != CapabilitySupported || chat[CapabilityImages] != CapabilityUnsupported {
		t.Fatalf("chat states=%v", chat)
	}
	responses := target.CapabilityStatusesFor(ProtocolOpenAIResponses)
	if responses[CapabilityReasoning] != CapabilityUnknown {
		t.Fatal("negative evidence leaked across protocols")
	}
	images := DeclaredCapabilityStatuses(Capabilities{Images: true}, ProtocolOpenAIImages)
	if images[CapabilityTools] != CapabilityUnsupported || images[CapabilityImages] != CapabilitySupported {
		t.Fatalf("images states=%v", images)
	}
}

func TestAnthropicStructuredCapabilityRequiresDeclarationOrEvidence(t *testing.T) {
	unknown := DeclaredCapabilityStatuses(ProtocolCapabilityBaseline(ProtocolAnthropic, Capabilities{}), ProtocolAnthropic)
	if unknown[CapabilityStructuredOutput] != CapabilityUnknown {
		t.Fatalf("unknown target became %s", unknown[CapabilityStructuredOutput])
	}
	declared := DeclaredCapabilityStatuses(ProtocolCapabilityBaseline(ProtocolAnthropic, Capabilities{StructuredOutput: true}), ProtocolAnthropic)
	if declared[CapabilityStructuredOutput] != CapabilitySupported {
		t.Fatal("explicit declaration discarded")
	}
	target := &CompiledTarget{Capabilities: Capabilities{StructuredOutput: true}, ProtocolCapabilityStatuses: map[Protocol]CapabilityStatuses{ProtocolAnthropic: {CapabilityStructuredOutput: CapabilityUnsupported}}}
	if target.CapabilityStatusesFor(ProtocolAnthropic)[CapabilityStructuredOutput] != CapabilityUnsupported {
		t.Fatal("negative endpoint evidence lost")
	}
	if target.CapabilityStatusesFor(ProtocolOpenAIChat)[CapabilityStructuredOutput] != CapabilitySupported {
		t.Fatal("negative evidence crossed protocols")
	}
}
