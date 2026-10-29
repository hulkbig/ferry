package management

import (
	"encoding/json"
	"fmt"
	"time"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

const usageProtectionKey = "codex_usage_protection"

func (s *Server) usageProtectionPolicy() (oauth.UsageProtection, error) {
	policy := oauth.DefaultUsageProtection()
	if s.deps.Control == nil {
		return policy, fmt.Errorf("control store unavailable")
	}
	raw, found, err := s.deps.Control.RuntimeSetting(usageProtectionKey)
	if err != nil {
		return policy, err
	}
	if found {
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return policy, err
		}
	}
	return policy, policy.Validate()
}

func (s *Server) saveUsageProtectionPolicy(policy oauth.UsageProtection) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if s.deps.Control == nil {
		return fmt.Errorf("control store unavailable")
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	s.usageProtectionMu.Lock()
	err = s.deps.Control.SetRuntimeSetting(usageProtectionKey, string(raw))
	s.usageProtectionMu.Unlock()
	if err != nil {
		return err
	}
	// Re-evaluate existing quota parking immediately. A later inference still
	// needs fresh upstream evidence before its HTTP request can be sent.
	if s.deps.OAuth != nil && s.deps.Scheduler != nil {
		snap := s.deps.Scheduler.Snapshot()
		if snap != nil {
			for _, c := range snap.Connections {
				account, ok := domain.OAuthAccountFromRef(c.Credential.SecretRef)
				if !ok {
					continue
				}
				usage, err := s.deps.OAuth.CachedUsage(account)
				if err == nil && usage != nil {
					s.observeProtectedUsage(account, usage, oauth.ProtectionWindows(usage, policy, time.Now()))
				}
			}
		}
	}
	return nil
}

func (s *Server) observeProtectedUsage(account string, usage *oauth.UsageSnapshot, windows []domain.QuotaWindowState) {
	if s.deps.Scheduler == nil || s.deps.OAuth == nil || usage == nil {
		return
	}
	s.usageProtectionMu.RLock()
	defer s.usageProtectionMu.RUnlock()
	// A response collected under an older policy cannot re-park a connection
	// after an operator has enabled paid overflow (or undo a stricter limit).
	policy, err := s.usageProtectionPolicy()
	if err != nil {
		return
	}
	if latest, err := s.deps.OAuth.CachedUsage(account); err == nil && latest != nil && latest.AccountID == usage.AccountID {
		usage = latest
	}
	windows = oauth.ProtectionWindows(usage, policy, time.Now())
	view, ok := s.deps.OAuth.Account(account)
	if !ok || view.AccountID != usage.AccountID {
		return
	}
	snap := s.deps.Scheduler.Snapshot()
	if snap == nil {
		return
	}
	for _, c := range snap.Connections {
		if c.Credential.SecretRef == domain.OAuthRef(account) {
			s.deps.Scheduler.ApplyProviderQuotaForIdentity(c.ID, c.RuntimeIdentity(), windows, time.Now())
		}
	}
}
