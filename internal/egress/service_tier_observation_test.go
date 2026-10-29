package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func TestCodexTierObservationUsesSerializedRequestAndKeepsDefaultEcho(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			capture := newCodexCapture(strings.Replace(codexDoneSSE, `"status":"completed"`, `"status":"completed","service_tier":"default"`, 1))
			defer capture.close()
			caller := NewCodexCaller(codexBase(t, capture), "account")
			ctx, observation := ObserveServiceTier(context.Background(), domain.ProviderCliproxyCodex)
			rc := chatRC()
			rc.ServiceTier = "fast"
			returned := ""
			if stream {
				ch, cancel, failure := caller.Stream(ctx, rc)
				if failure != nil {
					t.Fatal(failure)
				}
				defer cancel()
				for ev := range ch {
					if ev.Err != nil {
						t.Fatal(ev.Err)
					}
					if ev.ServiceTier != "" {
						returned = ev.ServiceTier
					}
				}
			} else {
				resp, failure := caller.Invoke(ctx, rc)
				if failure != nil {
					t.Fatal(failure)
				}
				returned = resp.ServiceTier
			}
			evidence := observation.Snapshot("fast", returned, true)
			if returned != "default" || evidence == nil || evidence.WireServiceTier != "priority" || evidence.PriorityConfirmation != "unconfirmed" || evidence.ResponseTierAuthority != domain.TierInformational || evidence.Source != "codex" {
				t.Fatalf("returned=%s evidence=%+v", returned, evidence)
			}
			if !strings.Contains(capture.bodyRaw, `"service_tier":"priority"`) || rc.ServiceTier != "fast" {
				t.Fatalf("wire=%s requested=%s", capture.bodyRaw, rc.ServiceTier)
			}
		})
	}
}

func TestTierObservationDoesNotInventUnsentEvidence(t *testing.T) {
	ctx, observation := ObserveServiceTier(context.Background(), domain.ProviderCliproxyCodex)
	caller := NewCodexCaller(Base{Model: "test"}, "")
	rc := chatRC()
	rc.ServiceTier = "ultrafast"
	if _, failure := caller.Invoke(ctx, rc); failure == nil {
		t.Fatal("ultrafast accepted")
	}
	if got := observation.Snapshot("ultrafast", "priority", true); got != nil {
		t.Fatalf("pre-send rejection invented evidence: %+v", got)
	}
}

func TestTierObservationRedirectDoesNotTrustOriginalContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	base := testBase(srv, "")
	ctx, observation := ObserveServiceTier(context.Background(), domain.ProviderCliproxyCodex)
	resp, failure := base.doSend(ctx, "/responses", []byte(`{"service_tier":"priority"}`), nil)
	if failure != nil {
		t.Fatal(failure)
	}
	resp.Body.Close()
	got := observation.Snapshot("priority", "priority", true)
	if got == nil || got.Source != "redirect" || got.ResponseTierAuthority != domain.TierUnknown || got.PriorityConfirmation != "unconfirmed" {
		t.Fatalf("redirect evidence=%+v", got)
	}
}
