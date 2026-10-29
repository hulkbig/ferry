package egress

import (
	"ferry/internal/domain"
)

// NewCaller builds the upstream adapter matching an endpoint's protocol.
func NewCaller(b Base, proto domain.Protocol) Caller {
	if driver := nativeChatDriver(b.CredRef); driver != "" && proto == domain.ProtocolOpenAIChat {
		return &NativeChat{OpenAIChat: NewOpenAIChat(b), driver: driver}
	}
	switch proto {
	case domain.ProtocolAnthropic:
		return NewAnthropic(b)
	case domain.ProtocolOpenAIResponses:
		return NewOpenAIResponses(b)
	case domain.ProtocolOpenAIImages:
		return NewOpenAIImages(b)
	default:
		return NewOpenAIChat(b)
	}
}
