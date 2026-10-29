package management

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

const automaticUsageResetKey = "codex_usage_reset_auto"

type automaticUsageResetPolicy struct {
	Enabled          bool `json:"enabled"`
	ThresholdMinutes int  `json:"threshold_minutes"`
}

func (p automaticUsageResetPolicy) validate() error {
	if p.ThresholdMinutes < 1 || p.ThresholdMinutes > 1440 {
		return fmt.Errorf("codex_usage_reset_auto.threshold_minutes must be between 1 and 1440")
	}
	return nil
}

func (s *Server) automaticUsageResetPolicy() (automaticUsageResetPolicy, error) {
	policy := automaticUsageResetPolicy{Enabled: true, ThresholdMinutes: 20}
	if s.deps.Control == nil {
		return policy, fmt.Errorf("control store is not configured")
	}
	raw, found, err := s.deps.Control.RuntimeSetting(automaticUsageResetKey)
	if err != nil {
		return policy, err
	}
	if found {
		policy = automaticUsageResetPolicy{}
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return policy, err
		}
	}
	return policy, policy.validate()
}

func (s *Server) saveAutomaticUsageResetPolicy(policy automaticUsageResetPolicy) error {
	if err := policy.validate(); err != nil {
		return err
	}
	if s.deps.Control == nil {
		return fmt.Errorf("control store is not configured")
	}
	s.usageResetPolicyMu.Lock()
	defer s.usageResetPolicyMu.Unlock()
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return s.deps.Control.SetRuntimeSetting(automaticUsageResetKey, string(raw))
}

// Hold policy and target locks through dispatch so disabling the automation or
// replacing its connection cannot race an already validated consume operation.
func (s *Server) lockAutomaticUsageResetTarget(binding oauth.UsageResetBinding, expected automaticUsageResetPolicy) (func(), error) {
	s.usageResetPolicyMu.RLock()
	policy, err := s.automaticUsageResetPolicy()
	if err != nil || !policy.Enabled || policy != expected {
		s.usageResetPolicyMu.RUnlock()
		return nil, fmt.Errorf("automatic usage-reset policy changed or unavailable")
	}
	release, err := s.lockUsageResetTarget(binding)
	if err != nil {
		s.usageResetPolicyMu.RUnlock()
		return nil, err
	}
	enabled := false
	for _, p := range s.deps.Config.Providers {
		if p.ID == binding.ProviderID {
			enabled = p.Enabled
		}
	}
	for _, c := range s.deps.Config.Connections {
		if c.ID == binding.ConnectionID {
			enabled = enabled && c.Enabled
		}
	}
	if !enabled {
		release()
		s.usageResetPolicyMu.RUnlock()
		return nil, fmt.Errorf("automatic usage-reset target is disabled")
	}
	return func() { release(); s.usageResetPolicyMu.RUnlock() }, nil
}

func (s *Server) runAutomaticUsageReset(ctx context.Context) {
	if s.deps.OAuth == nil {
		return
	}
	policy, err := s.automaticUsageResetPolicy()
	if err != nil || !policy.Enabled {
		return
	}
	s.configMu.RLock()
	providers := map[string]bool{}
	for _, p := range s.deps.Config.Providers {
		providers[p.ID] = p.Enabled && p.Type == domain.ProviderCliproxyCodex
	}
	bindings := []oauth.UsageResetBinding{}
	for _, c := range s.deps.Config.Connections {
		account, ok := domain.OAuthAccountFromRef(c.Credential.SecretRef)
		if providers[c.ProviderID] && c.Enabled && ok {
			bindings = append(bindings, oauth.UsageResetBinding{ProviderID: c.ProviderID, ConnectionID: c.ID, Account: account})
		}
	}
	s.configMu.RUnlock()
	seen := map[string]bool{}
	for _, binding := range bindings {
		if ctx.Err() != nil {
			return
		}
		scanCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, accountID, err := s.deps.OAuth.Identity(scanCtx, binding.Account)
		if err != nil || accountID == "" || seen[accountID] {
			cancel()
			continue
		}
		seen[accountID] = true
		request, err := s.deps.OAuth.AutoUsageReset(scanCtx, binding, time.Duration(policy.ThresholdMinutes)*time.Minute, func() (func(), error) { return s.lockAutomaticUsageResetTarget(binding, policy) })
		cancel()
		if err != nil {
			log.Printf("automatic Codex usage reset provider=%s connection=%s: %v", binding.ProviderID, binding.ConnectionID, err)
			continue
		}
		if request != nil && s.deps.Security != nil {
			_ = s.deps.Security.RecordManagementSecurityEvent("codex_usage_reset_automatic_outcome", request.CreatedBy, "", map[string]any{"request_id": request.ID, "provider_id": binding.ProviderID, "connection_id": binding.ConnectionID, "status": request.Status}, time.Now())
		}
	}
}

func (s *Server) StartAutomaticUsageReset(ctx context.Context) {
	s.usageResetStart.Do(func() {
		go func() {
			s.runAutomaticUsageReset(ctx)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.runAutomaticUsageReset(ctx)
				}
			}
		}()
	})
}
