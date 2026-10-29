package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ferry/internal/domain"
	"ferry/internal/oauth"
)

// OAuth management API (Phase 4): browser login lifecycle for built-in Codex
// accounts. One channel of type cliproxy-codex is one account; its identity
// lives in the OAuth manager (backed by the local secret store), never in
// config or API responses. Browser login supports both a loopback callback
// listener and manual callback paste; headless environments use device code.

// requireOAuth answers 503 when the OAuth manager is absent (feature off).
func (s *Server) requireOAuth(w http.ResponseWriter) bool {
	if s.deps.OAuth == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "oauth accounts not configured")
		return false
	}
	return true
}

// oauthChannel finds the channel by id and verifies it is OAuth-managed.
func (s *Server) oauthChannel(w http.ResponseWriter, id string) *domain.Channel {
	cfg := s.currentConfig()
	for i := range cfg.Channels {
		if cfg.Channels[i].ID == id {
			ch := &cfg.Channels[i]
			if ch.Type != domain.ChannelCliproxyCodex {
				writeMgmtErr(w, http.StatusBadRequest,
					"channel "+id+" is not an oauth-managed channel")
				return nil
			}
			return ch
		}
	}
	writeMgmtErr(w, http.StatusNotFound, "unknown channel "+id)
	return nil
}

// oauthV2Connection resolves the native V2 resource and its managed OAuth
// account. New B-side integrations never need to know about legacy Channels.
func (s *Server) oauthV2Connection(w http.ResponseWriter, providerID, connectionID string) (*domain.Connection, string) {
	cfg := s.currentConfig()
	var provider *domain.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == providerID {
			provider = &cfg.Providers[i]
			break
		}
	}
	if provider == nil {
		writeMgmtErr(w, http.StatusNotFound, "unknown provider "+providerID)
		return nil, ""
	}
	if provider.Type != domain.ProviderCliproxyCodex {
		writeMgmtErr(w, http.StatusBadRequest, "provider "+providerID+" is not oauth-managed")
		return nil, ""
	}
	for i := range cfg.Connections {
		connection := &cfg.Connections[i]
		if connection.ID != connectionID {
			continue
		}
		if connection.ProviderID != providerID {
			writeMgmtErr(w, http.StatusBadRequest, "connection does not belong to provider")
			return nil, ""
		}
		account, ok := domain.OAuthAccountFromRef(connection.Credential.SecretRef)
		if !ok {
			writeMgmtErr(w, http.StatusBadRequest, "connection "+connectionID+" is not oauth-managed")
			return nil, ""
		}
		return connection, account
	}
	writeMgmtErr(w, http.StatusNotFound, "unknown connection "+connectionID)
	return nil, ""
}

func parseOAuthCallback(r *http.Request) (string, string, error) {
	var body struct {
		Code        string `json:"code"`
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		return "", "", err
	}
	code, state := body.Code, body.State
	if code == "" && body.CallbackURL != "" {
		u, err := url.Parse(strings.TrimSpace(body.CallbackURL))
		if err != nil {
			return "", "", err
		}
		code, state = u.Query().Get("code"), u.Query().Get("state")
	}
	if code == "" || state == "" {
		return "", "", &oauthCallbackError{"need code+state (or the full callback URL)"}
	}
	return code, state, nil
}

type oauthCallbackError struct{ message string }

func (e *oauthCallbackError) Error() string { return e.message }

func (s *Server) ensureOAuthRuntimeRefreshed() (string, bool) {
	// With RefreshRuntime configured, the registered change hook already ran
	// synchronously. Legacy embedders retain their explicit Publish fallback.
	if s.deps.RefreshRuntime != nil {
		return "", true
	}
	return s.refreshRuntime()
}

func (s *Server) startOAuthLogin(r *http.Request, account string) (map[string]any, error) {
	var input struct {
		Mode   string `json:"mode"`
		Listen bool   `json:"listen"`
	}
	if r.ContentLength != 0 {
		if err := decodeMgmtBody(r, &input); err != nil {
			return nil, err
		}
	}
	input.Mode = strings.ToLower(strings.TrimSpace(input.Mode))
	if input.Mode == "" {
		input.Mode = "browser"
	}
	switch input.Mode {
	case "browser":
		authorizeURL, err := s.deps.OAuth.StartLogin(account)
		if err != nil {
			return nil, err
		}
		response := map[string]any{"account": account, "mode": "browser", "status": "pending", "authorize_url": authorizeURL}
		if input.Listen {
			listening, listenErr := s.deps.OAuth.EnsureLoopback()
			response["callback_listening"] = listening
			if listenErr != nil {
				// Manual callback completion remains valid when the loopback port is
				// occupied, so surface a recoverable diagnostic with the URL.
				response["callback_error"] = listenErr.Error()
			}
		}
		return response, nil
	case "device":
		status, err := s.deps.OAuth.StartDeviceLogin(r.Context(), account)
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(status)
		response := map[string]any{}
		_ = json.Unmarshal(raw, &response)
		return response, nil
	default:
		return nil, fmt.Errorf("unsupported OAuth login mode %q", input.Mode)
	}
}

func (s *Server) handleV2ConnectionOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return
	}
	response, err := s.startOAuthLogin(r, account)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "start login: "+err.Error())
		return
	}
	response["provider_id"], response["connection_id"] = connection.ProviderID, connection.ID
	writeMgmtJSON(w, http.StatusOK, response)
}

// handleV2ConnectionOAuthStatus returns the complete redacted lifecycle state
// for one OAuth-backed connection. Active credentials and an in-progress login
// can coexist while an operator rotates the account.
func (s *Server) handleV2ConnectionOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return
	}
	response := map[string]any{
		"provider_id": connection.ProviderID, "connection_id": connection.ID,
		"account": account, "status": "disconnected",
	}
	if view, ok := s.deps.OAuth.Account(account); ok {
		response["credential"] = view
		response["status"] = "active"
	}
	if login, ok := s.deps.OAuth.LoginStatus(account); ok {
		response["login"] = login
		if response["status"] != "active" {
			response["status"] = login.Status
		}
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

func (s *Server) handleV2ConnectionOAuthCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return
	}
	if _, ok := s.deps.OAuth.LoginStatus(account); !ok {
		writeMgmtErr(w, http.StatusNotFound, "no login in progress for connection "+connection.ID)
		return
	}
	if err := s.deps.OAuth.CancelLogin(account); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "cancel login: "+err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"provider_id": connection.ProviderID, "connection_id": connection.ID,
		"account": account, "status": "cancelled",
	})
}

func (s *Server) handleV2ConnectionOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return
	}
	if err := s.deps.OAuth.Logout(account); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "logout: "+err.Error())
		return
	}
	response := map[string]any{
		"provider_id": connection.ProviderID, "connection_id": connection.ID,
		"account": account, "status": "disconnected",
	}
	if message, ok := s.ensureOAuthRuntimeRefreshed(); !ok {
		response["warning"] = message
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

func (s *Server) handleV2ConnectionOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return
	}
	code, state, err := parseOAuthCallback(r)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode callback: "+err.Error())
		return
	}
	if err := s.deps.OAuth.CompleteLoginContext(r.Context(), account, state, code); err != nil {
		s.emitFailureAlert("oauth_failed", "connection", connection.ID, "OAuth failed: "+connection.ID, err.Error())
		writeMgmtErr(w, http.StatusBadRequest, "complete login: "+err.Error())
		return
	}
	resp := map[string]any{
		"provider_id": connection.ProviderID, "connection_id": connection.ID,
		"account": account, "status": "active",
	}
	if message, ok := s.ensureOAuthRuntimeRefreshed(); !ok {
		resp["warning"] = message
	}
	writeMgmtJSON(w, http.StatusOK, resp)
}

// handleV2ConnectionOAuthImport replaces one configured Codex account's
// credential from either the official Codex auth.json shape or CLIProxyAPI's
// flattened Codex JSON. The uploaded document is bounded, normalized by the
// OAuth manager, and never retained as a raw file.
func (s *Server) handleV2ConnectionOAuthImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 160<<10)
	var input struct {
		AuthJSON json.RawMessage `json:"auth_json"`
	}
	if err := decodeMgmtBody(r, &input); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode auth.json import: "+err.Error())
		return
	}
	raw := []byte(input.AuthJSON)
	if len(raw) > 0 && raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			writeMgmtErr(w, http.StatusBadRequest, "decode auth.json import: invalid encoded document")
			return
		}
		raw = []byte(encoded)
	}
	view, err := s.deps.OAuth.ImportAuthJSON(account, raw)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "import auth.json: "+err.Error())
		return
	}
	response := map[string]any{
		"provider_id": connection.ProviderID, "connection_id": connection.ID,
		"account": view, "status": "active",
	}
	if message, ok := s.ensureOAuthRuntimeRefreshed(); !ok {
		response["warning"] = message
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

// quotaWindowsFromUsage converts a Codex usage snapshot into provider-level
// quota windows: the 5-hour primary window and the weekly secondary window,
// each as a remaining fraction with its reset time.
func quotaWindowsFromUsage(usage *oauth.UsageSnapshot, now time.Time) []domain.QuotaWindowState {
	return oauth.ProtectionWindows(usage, oauth.DefaultUsageProtection(), now)
}

// handleV2ConnectionOAuthUsage reads the upstream subscription watermark for
// one configured Codex account. The resulting windows refresh the scheduler's
// quota surface (provider_api source) so routing sees real remaining quota;
// the attempt ledger and local bucket accounting stay untouched.
func (s *Server) handleV2ConnectionOAuthUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	connection, account := s.oauthV2Connection(w, r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		return
	}
	identity := ""
	if s.deps.Scheduler != nil {
		if snap := s.deps.Scheduler.Snapshot(); snap != nil {
			if current := snap.Connections[connection.ID]; current != nil && current.Credential.SecretRef == connection.Credential.SecretRef {
				identity = current.RuntimeIdentity()
			}
		}
	}
	var usage *oauth.UsageSnapshot
	var err error
	if r.URL.Query().Get("refresh") == "false" {
		usage, err = s.deps.OAuth.CachedUsage(account)
	} else {
		usage, err = s.deps.OAuth.Usage(r.Context(), account)
	}
	if err != nil {
		writeMgmtErr(w, http.StatusBadGateway, "read Codex usage: "+err.Error())
		return
	}
	// Cached reads are display-only: replaying old usage would undo a reset
	// merely because the operator reopened the connection drawer.
	if r.URL.Query().Get("refresh") != "false" && usage != nil && s.deps.Scheduler != nil && identity != "" {
		policy, policyErr := s.usageProtectionPolicy()
		if policyErr != nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "Codex usage protection settings unavailable")
			return
		}
		if windows := oauth.ProtectionWindows(usage, policy, time.Now()); len(windows) > 0 {
			s.deps.Scheduler.ApplyProviderQuotaForIdentity(connection.ID, identity, windows, time.Now())
		}
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"provider_id": connection.ProviderID, "connection_id": connection.ID,
		"account": account, "usage": usage,
	})
}

// handleChannelOAuthLogin starts a PKCE login and returns the authorize URL
// for the operator's browser.
func (s *Server) handleChannelOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	ch := s.oauthChannel(w, r.PathValue("id"))
	if ch == nil {
		return
	}
	response, err := s.startOAuthLogin(r, ch.ID)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "start login: "+err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

// handleChannelOAuthCallback completes a started login from the pasted
// redirect URL (or bare code+state), persists the token, and republishes so
// the channel's targets activate immediately.
func (s *Server) handleChannelOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	ch := s.oauthChannel(w, r.PathValue("id"))
	if ch == nil {
		return
	}
	var body struct {
		Code        string `json:"code"`
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"` // pasted full redirect URL
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	code, state := body.Code, body.State
	if code == "" && body.CallbackURL != "" {
		u, err := url.Parse(strings.TrimSpace(body.CallbackURL))
		if err != nil {
			writeMgmtErr(w, http.StatusBadRequest, "parse callback_url: "+err.Error())
			return
		}
		q := u.Query()
		code, state = q.Get("code"), q.Get("state")
	}
	if code == "" || state == "" {
		writeMgmtErr(w, http.StatusBadRequest, "need code+state (or the full callback URL)")
		return
	}
	if err := s.deps.OAuth.CompleteLoginContext(r.Context(), ch.ID, state, code); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "complete login: "+err.Error())
		return
	}
	resp := map[string]any{"account": ch.ID, "status": "active"}
	if msg, ok := s.ensureOAuthRuntimeRefreshed(); !ok {
		resp["warning"] = msg
	}
	writeMgmtJSON(w, http.StatusOK, resp)
}

// handleOAuthAccounts lists every managed account, redacted, annotated with
// the channels that log in as it.
func (s *Server) handleOAuthAccounts(w http.ResponseWriter, _ *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	byAccount := map[string][]string{}
	addAssociation := func(account, id string) {
		for _, existing := range byAccount[account] {
			if existing == id {
				return
			}
		}
		byAccount[account] = append(byAccount[account], id)
	}
	cfg := s.currentConfig()
	for i := range cfg.Channels {
		ch := &cfg.Channels[i]
		if ch.Type == domain.ChannelCliproxyCodex {
			addAssociation(ch.ID, ch.ID)
		}
	}
	for i := range cfg.Connections {
		connection := &cfg.Connections[i]
		if account, ok := domain.OAuthAccountFromRef(connection.Credential.SecretRef); ok {
			addAssociation(account, connection.ID)
		}
	}
	for account := range byAccount {
		sort.Strings(byAccount[account])
	}
	byName := map[string]map[string]any{}
	for _, a := range s.deps.OAuth.Accounts() {
		byName[a.Name] = map[string]any{
			"name":       a.Name,
			"email":      a.Email,
			"plan_type":  a.PlanType,
			"expires_at": a.ExpiresAt,
			"expired":    a.Expired,
			"status":     "active",
			"models":     domain.CodexModelNamesForPlan(a.PlanType),
			"channels":   byAccount[a.Name],
		}
	}
	for _, pending := range s.deps.OAuth.LoginStatuses() {
		item := byName[pending.Account]
		if item == nil {
			item = map[string]any{"name": pending.Account, "expired": false, "channels": byAccount[pending.Account]}
			byName[pending.Account] = item
		}
		item["login"] = pending
		if item["status"] != "active" {
			item["status"] = pending.Status
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		items = append(items, byName[name])
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"accounts": items})
}

func (s *Server) handleOAuthLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	account := r.PathValue("account")
	status, ok := s.deps.OAuth.LoginStatus(account)
	if !ok {
		writeMgmtErr(w, http.StatusNotFound, "no login in progress for account "+account)
		return
	}
	writeMgmtJSON(w, http.StatusOK, status)
}

func (s *Server) handleOAuthLoginCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	account := r.PathValue("account")
	if _, ok := s.deps.OAuth.LoginStatus(account); !ok {
		writeMgmtErr(w, http.StatusNotFound, "no login in progress for account "+account)
		return
	}
	if err := s.deps.OAuth.CancelLogin(account); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "cancel login: "+err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"account": account, "status": "cancelled"})
}

// handleOAuthAccountDelete logs one account out (token dropped from memory
// and store) and republishes so its targets drop their callers.
func (s *Server) handleOAuthAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireOAuth(w) {
		return
	}
	account := r.PathValue("account")
	if err := s.deps.OAuth.Logout(account); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "logout: "+err.Error())
		return
	}
	resp := map[string]any{"deleted": account}
	if msg, ok := s.ensureOAuthRuntimeRefreshed(); !ok {
		resp["warning"] = msg
	}
	writeMgmtJSON(w, http.StatusOK, resp)
}
