package egress

import (
	"ferry/internal/domain"
	"testing"
)

func TestNativeChatPreparationRejectsUnrepresentableFeatures(t *testing.T) {
	for _, driver := range []string{"devin", "antigravity"} {
		caller := NewCaller(Base{CredRef: "native-oauth:" + driver + ":account", Model: "model-one"}, domain.ProtocolOpenAIChat)
		preparer, ok := caller.(Preparer)
		if !ok {
			t.Fatal("native caller lacks compatibility preparation")
		}
		request := &domain.RequestContract{Origin: domain.ProtocolOpenAIChat, Protocol: domain.ProtocolOpenAIChat, Native: []byte(`{"model":"model-one","messages":[{"role":"user","content":"hello"}],"unsupported_native_state":true}`)}
		if _, err := preparer.Prepare(request); err == nil {
			t.Fatalf("%s admitted unsupported field before transport", driver)
		}
	}
}
