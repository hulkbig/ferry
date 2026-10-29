package ingress

import (
	"fmt"
	"net/http"
	"testing"

	"ferry/internal/domain"
)

func TestOpenAIServiceTierIsCanonicalAndGatesPriority(t *testing.T) {
	for _, tc := range []struct {
		tier     string
		priority bool
	}{{"priority", true}, {"fast", true}, {"auto", false}, {"default", false}, {"flex", false}} {
		tier := tc.tier
		for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses} {
			t.Run(string(protocol)+"/"+tier, func(t *testing.T) {
				eng := newStub()
				handler, path := eng.chat(), "/v1/chat/completions"
				input := `"messages":[{"role":"user","content":"hi"}]`
				if protocol == domain.ProtocolOpenAIResponses {
					handler, path = eng.responses(), "/v1/responses"
					input = `"input":"hi"`
				}
				rec := postJSON(t, handler, path, fmt.Sprintf(`{"model":"m",%s,"service_tier":%q}`, input, tier))
				if rec.Code != http.StatusOK {
					t.Fatalf("%d %s", rec.Code, rec.Body)
				}
				rc := eng.gotContract
				if rc.ServiceTier != tier || rc.ExtractRequirements().Priority != tc.priority || len(rc.ConversionIssues) != 0 || len(rc.MappingNotes) != 0 {
					t.Fatalf("lost or misclassified tier: %+v", rc)
				}
			})
		}
	}
}
