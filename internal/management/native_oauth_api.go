package management

import (
	"errors"
	"io"
	"net/http"
	"time"

	"ferry/internal/nativeauth"
)

func (s *Server) nativeOAuthAvailable(w http.ResponseWriter) bool {
	if s.deps.NativeOAuth == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "native OAuth is unavailable")
		return false
	}
	return true
}

func (s *Server) nativeOAuthAccount(w http.ResponseWriter, r *http.Request) bool {
	if !s.nativeOAuthAvailable(w) {
		return false
	}
	driver, account := r.PathValue("driver"), r.PathValue("account")
	_, _, valid := nativeauth.ParseRef(nativeauth.Ref(driver, account))
	_, supported := s.deps.NativeOAuth.Driver(driver)
	if !valid || !supported {
		writeMgmtErr(w, http.StatusBadRequest, "unsupported driver or invalid account")
		return false
	}
	return true
}

func (s *Server) handleNativeOAuthDrivers(w http.ResponseWriter, r *http.Request) {
	if !s.nativeOAuthAvailable(w) {
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"drivers": s.deps.NativeOAuth.Drivers()})
}

func (s *Server) handleNativeOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.nativeOAuthAccount(w, r) {
		return
	}
	view, err := s.deps.NativeOAuth.Status(r.PathValue("driver"), r.PathValue("account"))
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "native OAuth status unavailable")
		return
	}
	writeMgmtJSON(w, http.StatusOK, view)
}

func (s *Server) auditNativeOAuth(r *http.Request, action, outcome string) {
	if s.deps.Security != nil {
		_ = s.deps.Security.RecordManagementSecurityEvent("native_oauth_"+action,
			managementIdentityFrom(r).Actor(), managementRemoteAddr(r), map[string]any{
				"driver": r.PathValue("driver"), "account": r.PathValue("account"), "outcome": outcome,
			}, time.Now())
	}
}

func (s *Server) handleNativeOAuthBegin(w http.ResponseWriter, r *http.Request) {
	if !s.nativeOAuthAccount(w, r) {
		return
	}
	var body struct {
		Fields map[string]string `json:"fields"`
	}
	if err := decodeMgmtBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeMgmtErr(w, http.StatusBadRequest, "invalid native OAuth login body")
		return
	}
	view, err := s.deps.NativeOAuth.Begin(r.Context(), r.PathValue("driver"), r.PathValue("account"), body.Fields)
	if err != nil {
		s.auditNativeOAuth(r, "begin", "failed")
		writeMgmtErr(w, http.StatusBadGateway, "native OAuth login could not start")
		return
	}
	s.auditNativeOAuth(r, "begin", view.Status)
	writeMgmtJSON(w, http.StatusOK, view)
}

// Require explicit runtime reconciliation; never fall back to publishing a
// configuration revision when credentials change.
func (s *Server) nativeOAuthRuntimeAvailable(w http.ResponseWriter) bool {
	if s.deps.RefreshRuntime == nil || s.currentConfig() == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "native OAuth runtime reconciliation unavailable")
		return false
	}
	return true
}

func (s *Server) handleNativeOAuthComplete(w http.ResponseWriter, r *http.Request) {
	if !s.nativeOAuthAccount(w, r) || !s.nativeOAuthRuntimeAvailable(w) {
		return
	}
	var body struct {
		Callback string `json:"callback"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "invalid native OAuth completion body")
		return
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	view, err := s.deps.NativeOAuth.Complete(r.Context(), r.PathValue("driver"), r.PathValue("account"), body.Callback)
	if err != nil {
		s.auditNativeOAuth(r, "complete", "failed")
		writeMgmtErr(w, http.StatusBadGateway, "native OAuth completion failed")
		return
	}
	if view.Status == "connected" {
		if _, ok := s.refreshRuntime(); !ok {
			s.auditNativeOAuth(r, "complete", "runtime_refresh_failed")
			writeMgmtErr(w, http.StatusServiceUnavailable, "native OAuth credentials saved but runtime refresh failed")
			return
		}
	}
	s.auditNativeOAuth(r, "complete", view.Status)
	writeMgmtJSON(w, http.StatusOK, view)
}

// DELETE login disconnects the account entirely, clearing both credentials and
// a pending login. It is not a cancel-only operation preserving older tokens.
func (s *Server) handleNativeOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.nativeOAuthAccount(w, r) || !s.nativeOAuthRuntimeAvailable(w) {
		return
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	if err := s.deps.NativeOAuth.Disconnect(r.PathValue("driver"), r.PathValue("account")); err != nil {
		s.auditNativeOAuth(r, "disconnect", "failed")
		writeMgmtErr(w, http.StatusServiceUnavailable, "native OAuth disconnect failed")
		return
	}
	if _, ok := s.refreshRuntime(); !ok {
		s.auditNativeOAuth(r, "disconnect", "runtime_refresh_failed")
		writeMgmtErr(w, http.StatusServiceUnavailable, "native OAuth disconnected but runtime refresh failed")
		return
	}
	s.auditNativeOAuth(r, "disconnect", "disconnected")
	writeMgmtJSON(w, http.StatusOK, nativeauth.View{Driver: r.PathValue("driver"), Account: r.PathValue("account"), CredentialRef: nativeauth.Ref(r.PathValue("driver"), r.PathValue("account")), Status: "disconnected"})
}
