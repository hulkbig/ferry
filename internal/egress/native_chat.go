package egress

import (
	"ferry/internal/domain"
	"ferry/internal/nativewire"
	"strings"
)

// NativeChat keeps the existing canonical codec, but validates the subsequent
// native wire translation during route preparation, before upstream dispatch.
type NativeChat struct {
	*OpenAIChat
	driver string
}

func (n *NativeChat) Prepare(rc *domain.RequestContract) ([]string, error) {
	body, notes, err := n.buildRequest(rc)
	if err != nil {
		return notes, err
	}
	switch n.driver {
	case "antigravity":
		err = nativewire.ValidateAntigravityChat(body)
	case "devin":
		err = nativewire.ValidateDevinChat(body)
	}
	return notes, err
}
func nativeChatDriver(ref string) string {
	parts := strings.Split(ref, ":")
	if len(parts) == 3 && parts[0] == "native-oauth" && (parts[1] == "antigravity" || parts[1] == "devin") {
		return parts[1]
	}
	return ""
}
