package controlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ferry/internal/domain"
)

// ProviderCapabilityEvidence is a bounded observation about one capability on
// one Provider endpoint, native protocol and upstream model. The endpoint
// fingerprint is deliberately opaque: credentials and raw endpoint URLs never
// enter the evidence table.
type ProviderCapabilityEvidence struct {
	ProviderID          string
	EndpointFingerprint string
	UpstreamModel       string
	Protocol            domain.Protocol
	Capability          string
	Supported           bool
	FailureType         domain.FailureType
	ObservedAt          time.Time
	ExpiresAt           time.Time
}

// DeleteProviderCapabilityEvidence invalidates observations after credential
// replacement, including legacy observations shared by provider endpoint.
func (s *Store) DeleteProviderCapabilityEvidence(providerID string) error {
	_, err := s.db.Exec(`DELETE FROM provider_capability_evidence WHERE provider_id=?`, providerID)
	return err
}

func validateProviderCapabilityEvidence(item ProviderCapabilityEvidence) error {
	if strings.TrimSpace(item.ProviderID) == "" || strings.TrimSpace(item.EndpointFingerprint) == "" ||
		strings.TrimSpace(item.UpstreamModel) == "" || !domain.ValidProtocol(item.Protocol) ||
		strings.TrimSpace(item.Capability) == "" {
		return fmt.Errorf("provider capability evidence key is incomplete")
	}
	if item.ObservedAt.IsZero() || !item.ExpiresAt.After(item.ObservedAt) {
		return fmt.Errorf("provider capability evidence expiry must follow observation")
	}
	return nil
}

// PutProviderCapabilityEvidence atomically replaces the observation for an
// exact evidence key. Expired rows are pruned opportunistically so repeated
// Provider edits cannot grow the cache without bound.
func (s *Store) PutProviderCapabilityEvidence(item ProviderCapabilityEvidence) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("control store is unavailable")
	}
	if err := validateProviderCapabilityEvidence(item); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin provider capability evidence update: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM provider_capability_evidence WHERE expires_at<=?`, item.ObservedAt.UnixMilli()); err != nil {
		return fmt.Errorf("prune provider capability evidence: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO provider_capability_evidence(
		provider_id,endpoint_fingerprint,upstream_model,protocol,capability,supported,failure_type,observed_at,expires_at
	) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(provider_id,endpoint_fingerprint,upstream_model,protocol,capability)
	DO UPDATE SET supported=excluded.supported,failure_type=excluded.failure_type,
		observed_at=excluded.observed_at,expires_at=excluded.expires_at`,
		item.ProviderID, item.EndpointFingerprint, item.UpstreamModel, string(item.Protocol), item.Capability,
		boolInt(item.Supported), string(item.FailureType), item.ObservedAt.UnixMilli(), item.ExpiresAt.UnixMilli()); err != nil {
		return fmt.Errorf("write provider capability evidence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider capability evidence: %w", err)
	}
	return nil
}

// ProviderCapabilityEvidence returns only fresh evidence. Expired or absent
// evidence is reported as a cache miss so the caller can perform one new probe.
func (s *Store) ProviderCapabilityEvidence(providerID, endpointFingerprint, upstreamModel string,
	protocol domain.Protocol, capability string, now time.Time) (*ProviderCapabilityEvidence, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	var supported int
	var failureType string
	var observedAt, expiresAt int64
	err := s.db.QueryRow(`SELECT supported,failure_type,observed_at,expires_at
		FROM provider_capability_evidence
		WHERE provider_id=? AND endpoint_fingerprint=? AND upstream_model=? AND protocol=? AND capability=? AND expires_at>?`,
		providerID, endpointFingerprint, upstreamModel, string(protocol), capability, now.UnixMilli()).
		Scan(&supported, &failureType, &observedAt, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read provider capability evidence: %w", err)
	}
	return &ProviderCapabilityEvidence{
		ProviderID: providerID, EndpointFingerprint: endpointFingerprint, UpstreamModel: upstreamModel,
		Protocol: protocol, Capability: capability, Supported: supported != 0,
		FailureType: domain.FailureType(failureType), ObservedAt: time.UnixMilli(observedAt), ExpiresAt: time.UnixMilli(expiresAt),
	}, nil
}
