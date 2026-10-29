package management

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

func (s *Server) usageResetTarget(w http.ResponseWriter, r *http.Request) (oauth.UsageResetBinding, bool) {
	if !s.requireOAuth(w) {
		return oauth.UsageResetBinding{}, false
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return oauth.UsageResetBinding{}, false
	}
	return oauth.UsageResetBinding{ProviderID: connection.ProviderID, ConnectionID: connection.ID, Account: account}, true
}

func (s *Server) handleUsageResetRequests(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	requests, err := s.deps.OAuth.UsageResetRequests()
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

func (s *Server) handleUsageResetPrepare(w http.ResponseWriter, r *http.Request) {
	binding, ok := s.usageResetTarget(w, r)
	if !ok {
		return
	}
	var body struct {
		CreditID  string `json:"credit_id"`
		AccountID string `json:"account_id"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	request, err := s.deps.OAuth.PrepareUsageReset(r.Context(), binding, body.CreditID, body.AccountID, managementIdentityFrom(r).Actor())
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	s.auditUsageReset(r, "codex_usage_reset_prepared", request)
	writeMgmtJSON(w, http.StatusOK, map[string]any{"request": request, "console_path": "/#/providers/" + url.PathEscape(binding.ProviderID) + "?usage_reset=" + url.QueryEscape(request.ID), "confirmation_required": true})
}

func (s *Server) handleUsageResetCancel(w http.ResponseWriter, r *http.Request) {
	binding, ok := s.usageResetTarget(w, r)
	if !ok {
		return
	}
	request, err := s.deps.OAuth.CancelUsageReset(binding, r.PathValue("request_id"))
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	s.auditUsageReset(r, "codex_usage_reset_canceled", request)
	writeMgmtJSON(w, http.StatusOK, map[string]any{"request": request})
}

// Both middleware and handler enforce the session boundary. Tokens can prepare
// intents, but cannot confirm, even when AllowHighRisk is enabled.
func (s *Server) handleUsageResetConfirm(w http.ResponseWriter, r *http.Request) {
	identity := managementIdentityFrom(r)
	if s.deps.Security == nil || identity.Session == nil || identity.Token != nil {
		writeMgmtErr(w, http.StatusForbidden, "administrator session and explicit confirmation required")
		return
	}
	binding, ok := s.usageResetTarget(w, r)
	if !ok {
		return
	}
	var body struct {
		CreditID     string `json:"credit_id"`
		AccountID    string `json:"account_id"`
		Confirmation string `json:"confirmation"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Confirmation != "consume_one_codex_usage_reset" || body.AccountID == "" || body.CreditID == "" {
		writeMgmtErr(w, http.StatusBadRequest, "confirm the exact account and credit shown in the dialog")
		return
	}
	if err := s.deps.Security.RecordManagementSecurityEvent("codex_usage_reset_confirmation", identity.Actor(), managementRemoteAddr(r), map[string]any{"request_id": r.PathValue("request_id"), "provider_id": binding.ProviderID, "connection_id": binding.ConnectionID, "account_id": body.AccountID, "credit_id": body.CreditID}, time.Now()); err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "cannot record usage-reset confirmation")
		return
	}
	request, err := s.deps.OAuth.ConfirmUsageReset(r.Context(), binding, r.PathValue("request_id"), body.AccountID, body.CreditID, func() (func(), error) { return s.lockUsageResetTarget(binding) })
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	s.auditUsageReset(r, "codex_usage_reset_outcome", request)
	// The UI explicitly refreshes usage after a known successful result. Returning
	// the durable outcome first prevents a failed refresh from replaying consume.
	writeMgmtJSON(w, http.StatusOK, map[string]any{"request": request})
}

// Resolve under the same lock used by config publication, then retain it until
// dispatch ends. No OAuth refresh runs while this lock is held.
func (s *Server) lockUsageResetTarget(binding oauth.UsageResetBinding) (func(), error) {
	s.configMu.RLock()
	cfg := s.deps.Config
	validProvider := false
	for _, provider := range cfg.Providers {
		if provider.ID == binding.ProviderID && provider.Type == domain.ProviderCliproxyCodex {
			validProvider = true
			break
		}
	}
	if validProvider {
		for _, connection := range cfg.Connections {
			account, ok := domain.OAuthAccountFromRef(connection.Credential.SecretRef)
			if connection.ID == binding.ConnectionID && connection.ProviderID == binding.ProviderID && ok && account == binding.Account {
				return s.configMu.RUnlock, nil
			}
		}
	}
	s.configMu.RUnlock()
	return nil, fmt.Errorf("provider or connection identity changed; usage reset blocked")
}

func (s *Server) auditUsageReset(r *http.Request, event string, request *oauth.UsageResetRequest) {
	if s.deps.Security == nil {
		return
	}
	_ = s.deps.Security.RecordManagementSecurityEvent(event, managementIdentityFrom(r).Actor(), managementRemoteAddr(r), map[string]any{"request_id": request.ID, "provider_id": request.ProviderID, "connection_id": request.ConnectionID, "account_id": request.AccountID, "credit_id": request.Credit.ID, "status": request.Status}, time.Now())
}
