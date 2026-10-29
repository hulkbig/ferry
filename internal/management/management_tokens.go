package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"ferry/internal/controlstore"
)

func (s *Server) requireAdministratorSession(w http.ResponseWriter, r *http.Request) (managementIdentity, bool) {
	if s.deps.Security == nil {
		writeMgmtJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{
			"code": "MANAGEMENT_SECURITY_UNAVAILABLE", "message": "management security store is not configured",
		}})
		return managementIdentity{}, false
	}
	identity := managementIdentityFrom(r)
	if identity.Session == nil || identity.Username == "" {
		writeMgmtJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
			"code": "ADMIN_SESSION_REQUIRED", "message": "this operation requires the Ferry console administrator session",
		}})
		return managementIdentity{}, false
	}
	return identity, true
}

func (s *Server) handleV2Access(w http.ResponseWriter, r *http.Request) {
	identity := managementIdentityFrom(r)
	if identity.Token != nil {
		writeMgmtJSON(w, http.StatusOK, map[string]any{
			"authenticated": true, "auth_type": "management_token", "actor": identity.Actor(),
			"token": identity.Token, "allow_high_risk": identity.Token.AllowHighRisk,
		})
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "auth_type": "administrator_session", "actor": identity.Actor(),
		"allow_high_risk": true,
	})
}

func (s *Server) handleV2ManagementTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdministratorSession(w, r); !ok {
		return
	}
	tokens, err := s.deps.Security.ManagementTokens()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "list management tokens")
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"management_tokens": tokens})
}

func (s *Server) handleV2ManagementTokenDetail(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdministratorSession(w, r); !ok {
		return
	}
	token, err := s.deps.Security.ManagementToken(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "read management token")
		return
	}
	if token == nil {
		writeMgmtErr(w, http.StatusNotFound, "management token not found")
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"management_token": token})
}

func (s *Server) handleV2ManagementTokenCreate(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdministratorSession(w, r)
	if !ok {
		return
	}
	var body struct {
		Name          string     `json:"name"`
		Description   string     `json:"description"`
		ExpiresAt     *time.Time `json:"expires_at"`
		AllowHighRisk bool       `json:"allow_high_risk"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	token, secret, err := s.deps.Security.CreateManagementToken(body.Name, body.Description,
		body.ExpiresAt, body.AllowHighRisk, identity.Actor(), managementRemoteAddr(r), time.Now())
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			status = http.StatusConflict
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeMgmtJSON(w, http.StatusCreated, map[string]any{"management_token": token, "secret": secret})
}

func (s *Server) handleV2ManagementTokenPatch(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdministratorSession(w, r)
	if !ok {
		return
	}
	var body struct {
		Name          *string    `json:"name"`
		Description   *string    `json:"description"`
		Enabled       *bool      `json:"enabled"`
		AllowHighRisk *bool      `json:"allow_high_risk"`
		ExpiresAt     *time.Time `json:"expires_at"`
		ClearExpiry   bool       `json:"clear_expiry"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ClearExpiry && body.ExpiresAt != nil {
		writeMgmtErr(w, http.StatusBadRequest, "expires_at and clear_expiry cannot be used together")
		return
	}
	token, err := s.deps.Security.UpdateManagementToken(r.PathValue("id"), body.Name, body.Description,
		body.Enabled, body.AllowHighRisk, body.ExpiresAt, body.ClearExpiry, identity.Actor(),
		managementRemoteAddr(r), time.Now())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, controlstore.ErrManagementTokenRevoked) || strings.Contains(strings.ToLower(err.Error()), "unique") {
			status = http.StatusConflict
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	if token == nil {
		writeMgmtErr(w, http.StatusNotFound, "management token not found")
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"management_token": token})
}

func (s *Server) handleV2ManagementTokenRotate(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdministratorSession(w, r)
	if !ok {
		return
	}
	token, secret, err := s.deps.Security.RotateManagementToken(r.PathValue("id"), identity.Actor(),
		managementRemoteAddr(r), time.Now())
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, controlstore.ErrManagementTokenRevoked) {
			status = http.StatusConflict
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	if token == nil {
		writeMgmtErr(w, http.StatusNotFound, "management token not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeMgmtJSON(w, http.StatusOK, map[string]any{"management_token": token, "secret": secret})
}

func (s *Server) handleV2ManagementTokenRevoke(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdministratorSession(w, r)
	if !ok {
		return
	}
	token, err := s.deps.Security.RevokeManagementToken(r.PathValue("id"), identity.Actor(),
		managementRemoteAddr(r), time.Now())
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if token == nil {
		writeMgmtErr(w, http.StatusNotFound, "management token not found")
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"management_token": token})
}

func (s *Server) handleV2ManagementTokenDelete(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdministratorSession(w, r)
	if !ok {
		return
	}
	if err := s.deps.Security.DeleteManagementToken(r.PathValue("id"), identity.Actor(),
		managementRemoteAddr(r), time.Now()); err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
