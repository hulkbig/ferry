package controlstore

import (
	"path/filepath"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestProviderCapabilityEvidenceRoundTripAndExpiry(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Unix(1_800_000_000, 0).UTC()
	item := ProviderCapabilityEvidence{
		ProviderID: "provider-a", EndpointFingerprint: "endpoint-hash", UpstreamModel: "model-a",
		Protocol: domain.ProtocolOpenAIChat, Capability: "structured_output", Supported: true,
		ObservedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := store.PutProviderCapabilityEvidence(item); err != nil {
		t.Fatal(err)
	}
	got, err := store.ProviderCapabilityEvidence("provider-a", "endpoint-hash", "model-a",
		domain.ProtocolOpenAIChat, "structured_output", now.Add(time.Hour))
	if err != nil || got == nil || !got.Supported || !got.ObservedAt.Equal(now) {
		t.Fatalf("fresh evidence = (%+v,%v)", got, err)
	}
	got, err = store.ProviderCapabilityEvidence("provider-a", "endpoint-hash", "model-a",
		domain.ProtocolOpenAIChat, "structured_output", now.Add(25*time.Hour))
	if err != nil || got != nil {
		t.Fatalf("expired evidence = (%+v,%v), want cache miss", got, err)
	}
}

func TestProviderCapabilityEvidenceOverwritesSameKey(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Unix(1_800_000_000, 0).UTC()
	item := ProviderCapabilityEvidence{
		ProviderID: "provider-a", EndpointFingerprint: "endpoint-hash", UpstreamModel: "model-a",
		Protocol: domain.ProtocolOpenAIResponses, Capability: "structured_output", Supported: false,
		FailureType: domain.FailUnsupportedParam, ObservedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.PutProviderCapabilityEvidence(item); err != nil {
		t.Fatal(err)
	}
	item.Supported = true
	item.FailureType = ""
	item.ObservedAt = now.Add(time.Minute)
	item.ExpiresAt = now.Add(48 * time.Hour)
	if err := store.PutProviderCapabilityEvidence(item); err != nil {
		t.Fatal(err)
	}
	got, err := store.ProviderCapabilityEvidence(item.ProviderID, item.EndpointFingerprint, item.UpstreamModel,
		item.Protocol, item.Capability, now.Add(2*time.Hour))
	if err != nil || got == nil || !got.Supported || got.FailureType != "" || !got.ObservedAt.Equal(item.ObservedAt) {
		t.Fatalf("replacement evidence = (%+v,%v)", got, err)
	}
}
