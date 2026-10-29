package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"ferry/internal/adminauth"
	"ferry/internal/buildinfo"
	"ferry/internal/controlstore"
)

const (
	managementSessionCookie = "smartapi_admin_session"
	managementCSRFCookie    = "smartapi_admin_csrf"
	managementSessionMaxAge = 24 * time.Hour
	managementSessionIdle   = 2 * time.Hour
)

type ManagementSecurityStore interface {
	ManagementAdministrator() (*controlstore.ManagementAdministrator, error)
	InitializeManagementAdministrator(username, passwordHash, remoteAddr string, now time.Time) error
	RecordManagementLoginFailure(remoteAddr string, now time.Time) (int, *time.Time, error)
	RecordManagementLoginSuccess(username, remoteAddr string, now time.Time) error
	CreateManagementSession(session controlstore.ManagementSession, remoteAddr string) error
	ManagementSession(tokenHash []byte, now time.Time, idleLimit time.Duration) (*controlstore.ManagementSession, error)
	DeleteManagementSession(tokenHash []byte, remoteAddr string, now time.Time) error
	UpdateManagementAdministratorPassword(username, passwordHash, eventType, remoteAddr string, now time.Time) error
	ManagementSecurityEvents(limit int) ([]controlstore.ManagementSecurityEvent, error)
	RecordManagementSecurityEvent(eventType, actor, remoteAddr string, detail map[string]any, now time.Time) error
	AuthenticateManagementToken(secret, remoteAddr string, now time.Time) (*controlstore.ManagementToken, error)
	ManagementTokens() ([]controlstore.ManagementToken, error)
	ManagementToken(id string) (*controlstore.ManagementToken, error)
	CreateManagementToken(name, description string, expiresAt *time.Time, allowHighRisk bool,
		actor, remoteAddr string, now time.Time) (*controlstore.ManagementToken, string, error)
	UpdateManagementToken(id string, name, description *string, enabled, allowHighRisk *bool,
		expiresAt *time.Time, clearExpiry bool, actor, remoteAddr string, now time.Time) (*controlstore.ManagementToken, error)
	RotateManagementToken(id, actor, remoteAddr string, now time.Time) (*controlstore.ManagementToken, string, error)
	RevokeManagementToken(id, actor, remoteAddr string, now time.Time) (*controlstore.ManagementToken, error)
	DeleteManagementToken(id, actor, remoteAddr string, now time.Time) error
}

type managementIdentityKey struct{}

type managementIdentity struct {
	Username string
	Session  *controlstore.ManagementSession
	Token    *controlstore.ManagementToken
}

func (i managementIdentity) Actor() string {
	if i.Token != nil {
		return "token:" + i.Token.ID
	}
	return i.Username
}

func managementIdentityFrom(r *http.Request) managementIdentity {
	identity, _ := r.Context().Value(managementIdentityKey{}).(managementIdentity)
	return identity
}

func randomManagementToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func digestManagementToken(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func managementRemoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func validAdministratorUsername(username string) bool {
	if !utf8.ValidString(username) || len(username) < 1 || len(username) > 64 {
		return false
	}
	for _, value := range username {
		if value <= 0x20 || value == 0x7f {
			return false
		}
	}
	return true
}

func setManagementCookies(w http.ResponseWriter, sessionToken, csrfToken string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: managementSessionCookie, Value: sessionToken, Path: "/", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- the CSRF cookie is deliberately script-readable for the double-submit check; the session cookie stays HttpOnly and both are Secure with SameSite=Strict
		Name: managementCSRFCookie, Value: csrfToken, Path: "/", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), Secure: true, HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearManagementCookies(w http.ResponseWriter) {
	for _, name := range []string{managementSessionCookie, managementCSRFCookie} {
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- clearing an expired cookie; HttpOnly mirrors each original cookie flag so both are removed
			Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
			Secure: true, HttpOnly: name == managementSessionCookie, SameSite: http.SameSiteStrictMode,
		})
	}
}

func (s *Server) createManagementSession(w http.ResponseWriter, username, remoteAddr string, now time.Time) (string, error) {
	sessionToken, err := randomManagementToken(32)
	if err != nil {
		return "", err
	}
	csrfToken, err := randomManagementToken(32)
	if err != nil {
		return "", err
	}
	sessionID, err := randomManagementToken(18)
	if err != nil {
		return "", err
	}
	expires := now.Add(managementSessionMaxAge)
	if err := s.deps.Security.CreateManagementSession(controlstore.ManagementSession{
		ID: sessionID, Username: username, TokenHash: digestManagementToken(sessionToken),
		CSRFHash: digestManagementToken(csrfToken), CreatedAt: now, LastSeenAt: now, ExpiresAt: expires,
	}, remoteAddr); err != nil {
		return "", err
	}
	setManagementCookies(w, sessionToken, csrfToken, expires)
	return csrfToken, nil
}

func (s *Server) resolveManagementSession(r *http.Request) (*controlstore.ManagementSession, error) {
	if s.deps.Security == nil {
		return nil, controlstore.ErrManagementSessionInvalid
	}
	cookie, err := r.Cookie(managementSessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, controlstore.ErrManagementSessionInvalid
	}
	return s.deps.Security.ManagementSession(digestManagementToken(cookie.Value), time.Now(), managementSessionIdle)
}

func isManagementAuthPublic(r *http.Request) bool {
	if r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/version") {
		return true
	}
	switch r.URL.Path {
	case "/api/auth/status", "/api/auth/initialize", "/api/auth/login":
		return true
	default:
		return false
	}
}

func isSafeManagementMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func managementBearer(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if value == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) || strings.TrimSpace(value[len(prefix):]) == "" {
		return "", true
	}
	return strings.TrimSpace(value[len(prefix):]), true
}

func isSessionOnlyManagementPath(path string) bool {
	if strings.Contains(path, "/oauth/usage-reset-requests/") && strings.HasSuffix(path, "/confirm") {
		return true
	}
	return strings.HasPrefix(path, "/api/auth/") || strings.HasPrefix(path, "/api/management-tokens")
}

// isHighRiskManagementRequest is intentionally narrow and auditable. Draft
// editing and simulation are safe for ordinary Skill tokens; activation,
// credentials, key lifecycle, and manual runtime overrides require the
// administrator-controlled per-token allow_high_risk switch.
func isHighRiskManagementRequest(r *http.Request) bool {
	path := r.URL.Path
	if path == "/api/settings" && !isSafeManagementMethod(r.Method) {
		return true
	}
	if strings.HasPrefix(path, "/api/secrets") || strings.HasPrefix(path, "/api/imports/") {
		return true
	}
	if strings.HasPrefix(path, "/api/api-keys") {
		return !(r.Method == http.MethodGet && !strings.HasSuffix(path, "/secret"))
	}
	if strings.HasSuffix(path, "/routing/apply") || strings.HasSuffix(path, "/apply") ||
		strings.HasSuffix(path, "/rotate-secret") || strings.HasSuffix(path, "/clear-cooldown") ||
		strings.HasSuffix(path, "/set-quota") || strings.HasSuffix(path, "/models/sync") {
		return true
	}
	if (strings.Contains(path, "/oauth/") || strings.HasSuffix(path, "/oauth")) && r.Method != http.MethodGet {
		return true
	}
	return false
}

func (s *Server) managementAuthMiddleware(next http.Handler) http.Handler {
	if s.deps.Security == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || isManagementAuthPublic(r) {
			next.ServeHTTP(w, r)
			return
		}
		admin, err := s.deps.Security.ManagementAdministrator()
		if err != nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "management authentication unavailable")
			return
		}
		if admin == nil {
			writeMgmtJSON(w, http.StatusPreconditionRequired, map[string]any{"error": map[string]any{
				"code": "ADMIN_INITIALIZATION_REQUIRED", "message": "initialize the single administrator first",
			}})
			return
		}
		if bearer, supplied := managementBearer(r); supplied {
			token, tokenErr := s.deps.Security.AuthenticateManagementToken(bearer, managementRemoteAddr(r), time.Now())
			if tokenErr != nil {
				_ = s.deps.Security.RecordManagementSecurityEvent("management_token_rejected", "",
					managementRemoteAddr(r), map[string]any{"method": r.Method, "path": r.URL.Path}, time.Now())
				writeMgmtJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
					"code": "INVALID_MANAGEMENT_TOKEN", "message": "valid management bearer token required",
				}})
				return
			}
			if isSessionOnlyManagementPath(r.URL.Path) {
				_ = s.deps.Security.RecordManagementSecurityEvent("management_token_scope_denied", "token:"+token.ID,
					managementRemoteAddr(r), map[string]any{"token_id": token.ID, "method": r.Method, "path": r.URL.Path}, time.Now())
				writeMgmtJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
					"code": "ADMIN_SESSION_REQUIRED", "message": "this operation requires the Ferry console administrator session",
				}})
				return
			}
			if isHighRiskManagementRequest(r) && !token.AllowHighRisk {
				_ = s.deps.Security.RecordManagementSecurityEvent("management_token_high_risk_denied", "token:"+token.ID,
					managementRemoteAddr(r), map[string]any{"token_id": token.ID, "method": r.Method, "path": r.URL.Path}, time.Now())
				writeMgmtJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
					"code":     "HIGH_RISK_NOT_ALLOWED",
					"message":  "enable high-risk operations for this token in the Ferry console before retrying",
					"token_id": token.ID,
				}})
				return
			}
			ctx := context.WithValue(r.Context(), managementIdentityKey{}, managementIdentity{Token: token})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		session, err := s.resolveManagementSession(r)
		if err != nil {
			clearManagementCookies(w)
			writeMgmtJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": "AUTHENTICATION_REQUIRED", "message": "administrator login required",
			}})
			return
		}
		if !isSafeManagementMethod(r.Method) {
			headerToken := r.Header.Get("X-CSRF-Token")
			csrfCookie, cookieErr := r.Cookie(managementCSRFCookie)
			if headerToken == "" || cookieErr != nil || csrfCookie.Value == "" ||
				subtle.ConstantTimeCompare([]byte(headerToken), []byte(csrfCookie.Value)) != 1 ||
				subtle.ConstantTimeCompare(digestManagementToken(headerToken), session.CSRFHash) != 1 {
				writeMgmtJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
					"code": "CSRF_REJECTED", "message": "valid CSRF token required",
				}})
				return
			}
		}
		ctx := context.WithValue(r.Context(), managementIdentityKey{}, managementIdentity{Username: session.Username, Session: session})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) handleManagementAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Security == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "management authentication is not configured")
		return
	}
	admin, err := s.deps.Security.ManagementAdministrator()
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "management authentication unavailable")
		return
	}
	response := map[string]any{
		"initialized": admin != nil, "authenticated": false,
		"session_max_hours": 24, "session_idle_hours": 2,
		"login_failure_limit": 5, "login_lock_minutes": 15,
	}
	if admin != nil {
		response["username"] = admin.Username
		if session, sessionErr := s.resolveManagementSession(r); sessionErr == nil {
			response["authenticated"] = true
			response["username"] = session.Username
			response["expires_at"] = session.ExpiresAt
		}
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

func (s *Server) handleManagementHealth(w http.ResponseWriter, _ *http.Request) {
	identity := buildinfo.Current()
	status := "ok"
	if s.deps.Scheduler == nil || s.deps.Scheduler.Snapshot() == nil {
		status = "unavailable"
	}
	code := http.StatusOK
	if status != "ok" {
		code = http.StatusServiceUnavailable
	}
	writeMgmtJSON(w, code, map[string]any{"status": status, "version": identity.Version})
}

func (s *Server) handleManagementVersion(w http.ResponseWriter, _ *http.Request) {
	writeMgmtJSON(w, http.StatusOK, buildinfo.Current())
}

func (s *Server) handleManagementInitialize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if !validAdministratorUsername(body.Username) {
		writeMgmtErr(w, http.StatusBadRequest, "username must contain 1-64 visible characters")
		return
	}
	passwordHash, err := adminauth.HashPassword(body.Password)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	remoteAddr := managementRemoteAddr(r)
	if err := s.deps.Security.InitializeManagementAdministrator(body.Username, passwordHash, remoteAddr, now); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, controlstore.ErrAdministratorAlreadyInitialized) {
			status = http.StatusConflict
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	csrfToken, err := s.createManagementSession(w, body.Username, remoteAddr, now)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "create management session")
		return
	}
	writeMgmtJSON(w, http.StatusCreated, map[string]any{"initialized": true, "username": body.Username, "csrf_token": csrfToken})
}

func (s *Server) handleManagementLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	admin, err := s.deps.Security.ManagementAdministrator()
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "management authentication unavailable")
		return
	}
	if admin == nil {
		writeMgmtJSON(w, http.StatusPreconditionRequired, map[string]any{"error": map[string]any{
			"code": "ADMIN_INITIALIZATION_REQUIRED", "message": "initialize the single administrator first",
		}})
		return
	}
	now := time.Now()
	remoteAddr := managementRemoteAddr(r)
	if admin.LockedUntil != nil && admin.LockedUntil.After(now) {
		_, lockedUntil, _ := s.deps.Security.RecordManagementLoginFailure(remoteAddr, now)
		writeMgmtJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
			"code": "LOGIN_LOCKED", "message": "login is temporarily locked", "locked_until": lockedUntil,
		}})
		return
	}
	verified, verifyErr := adminauth.VerifyPassword(admin.PasswordHash, body.Password)
	if verifyErr != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "management password verifier unavailable")
		return
	}
	if body.Username != admin.Username || !verified {
		_, lockedUntil, recordErr := s.deps.Security.RecordManagementLoginFailure(remoteAddr, now)
		if recordErr != nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "record login failure")
			return
		}
		if lockedUntil != nil {
			writeMgmtJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
				"code": "LOGIN_LOCKED", "message": "login is temporarily locked", "locked_until": lockedUntil,
			}})
			return
		}
		writeMgmtJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
			"code": "INVALID_CREDENTIALS", "message": "invalid username or password",
		}})
		return
	}
	if err := s.deps.Security.RecordManagementLoginSuccess(admin.Username, remoteAddr, now); err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "record login success")
		return
	}
	csrfToken, err := s.createManagementSession(w, admin.Username, remoteAddr, now)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "create management session")
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": admin.Username, "csrf_token": csrfToken})
}

func (s *Server) handleManagementSession(w http.ResponseWriter, r *http.Request) {
	identity := managementIdentityFrom(r)
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "username": identity.Username, "expires_at": identity.Session.ExpiresAt,
	})
}

func (s *Server) handleManagementLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(managementSessionCookie)
	if err == nil {
		_ = s.deps.Security.DeleteManagementSession(digestManagementToken(cookie.Value), managementRemoteAddr(r), time.Now())
	}
	clearManagementCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleManagementPasswordChange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	identity := managementIdentityFrom(r)
	admin, err := s.deps.Security.ManagementAdministrator()
	if err != nil || admin == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "management authentication unavailable")
		return
	}
	verified, err := adminauth.VerifyPassword(admin.PasswordHash, body.CurrentPassword)
	if err != nil || !verified {
		_ = s.deps.Security.RecordManagementSecurityEvent("password_change_failed", identity.Username,
			managementRemoteAddr(r), map[string]any{"reason": "invalid_current_password"}, time.Now())
		writeMgmtErr(w, http.StatusUnauthorized, "current password is invalid")
		return
	}
	passwordHash, err := adminauth.HashPassword(body.NewPassword)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.deps.Security.UpdateManagementAdministratorPassword(identity.Username, passwordHash,
		"password_changed", managementRemoteAddr(r), time.Now()); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "change administrator password")
		return
	}
	clearManagementCookies(w)
	writeMgmtJSON(w, http.StatusOK, map[string]any{"changed": true, "sessions_revoked": true})
}

func (s *Server) handleManagementSecurityEvents(w http.ResponseWriter, _ *http.Request) {
	events, err := s.deps.Security.ManagementSecurityEvents(50)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "read security events")
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"events": events})
}
