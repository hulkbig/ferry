package management

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/nativeauth"
)

// Secrets API (F-P0-25): the console provisions upstream API keys into the
// local secret store over the 127.0.0.1-only management plane. Values are
// accepted in request bodies, stored with 0600 file permissions, and never
// echoed back, logged, or persisted anywhere else — configs keep only
// CredentialRef reference text.

// secretNameOK restricts names to a safe charset (they end up in refs like
// `store:glm` and in file-backed storage).
var secretNameOK = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// setSecretBody is shared by POST /api/secrets/{name} and target set-secret.
type setSecretBody struct {
	Value string `json:"value"` // the API key itself; never persisted outside the store
}

// handleSecretsList returns stored secret NAMES only — never values.
func (s *Server) handleSecretsList(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Store == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "secret store not configured")
		return
	}
	names := s.deps.Store.Names()
	// annotate each name with how many live targets currently resolve from it
	usage := map[string][]string{}
	cfg := s.currentConfig()
	for _, t := range cfg.Targets {
		addSecretUsage(usage, string(t.ID), t.Credential.SecretRef)
	}
	for i := range cfg.Channels {
		ch := &cfg.Channels[i]
		ref := ch.Credential.SecretRef
		if ch.Type == domain.ChannelCliproxyCodex {
			ref = domain.OAuthRef(ch.ID)
		}
		for j := range ch.Models {
			addSecretUsage(usage, domain.ChannelTargetID(ch.ID, ch.Models[j].Model), ref)
		}
	}
	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		if provider.ModelSyncCatalog != nil {
			addSecretUsage(usage, "provider:"+provider.ID+":model-catalog", provider.ModelSyncCatalog.CredentialRef)
		}
	}
	items := make([]map[string]any, 0, len(names))
	for _, n := range names {
		if strings.Contains(n, ":") {
			// reserved namespaces (oauth:codex:<account> tokens) are managed by
			// their own flows, never operator-editable keys
			continue
		}
		items = append(items, map[string]any{
			"name": n, "targets": usage[n],
		})
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"secrets": items})
}

func addSecretUsage(usage map[string][]string, targetID, ref string) {
	name := ""
	switch {
	case strings.HasPrefix(ref, "store:"):
		name = ref[len("store:"):]
	case strings.HasPrefix(ref, "env:"):
		name = ref[len("env:"):] // env refs fall back to same-named store entry
	default:
		return
	}
	usage[name] = append(usage[name], targetID)
}

// handleSecretSet stores one named secret, then republishes so targets that
// resolve from it go live immediately.
func (s *Server) handleSecretSet(w http.ResponseWriter, r *http.Request) {
	s.setSecret(w, r, domain.ResourceTargetID(""), r.PathValue("name"))
}

// handleSecretDelete removes a stored secret and republishes.
func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "secret store not configured")
		return
	}
	name := r.PathValue("name")
	if !secretNameOK.MatchString(name) {
		writeMgmtErr(w, http.StatusBadRequest, "invalid secret name")
		return
	}
	if err := s.deps.Store.Delete(name); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "delete secret: "+err.Error())
		return
	}
	s.advanceSecretGeneration(name)
	if msg, ok := s.republish(); !ok {
		if err := s.markProviderModelSyncDueForSecret(name); err != nil {
			msg += "; provider model synchronization could not be scheduled"
		}
		writeMgmtJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"deleted": name, "warning": msg,
		})
		return
	}
	resp := map[string]any{"deleted": name}
	status := http.StatusOK
	if err := s.markProviderModelSyncDueForSecret(name); err != nil {
		resp["warning"] = "provider model synchronization could not be scheduled"
		status = http.StatusMultiStatus
	}
	writeMgmtJSON(w, status, resp)
}

// handleTargetSetSecret stores a value under the name implied by the target's
// credential ref (store:NAME uses NAME directly; env:VAR writes VAR), then
// republishes so the target activates without touching YAML.
func (s *Server) handleTargetSetSecret(w http.ResponseWriter, r *http.Request) {
	id := domain.ResourceTargetID(r.PathValue("id"))
	cfg := s.currentConfig()
	ref, ok := configuredTargetCredential(cfg, id)
	if !ok {
		writeMgmtErr(w, http.StatusNotFound, "unknown target "+string(id))
		return
	}
	name, err := storeNameForRef(ref.SecretRef)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest,
			"target credential "+ref.SecretRef+" cannot be provisioned here: "+err.Error())
		return
	}
	s.setSecret(w, r, id, name)
}

// handleTargetCredential reports one target's credential provisioning state:
// which ref it needs, whether it resolves today, never the value itself.
func (s *Server) handleTargetCredential(w http.ResponseWriter, r *http.Request) {
	id := domain.ResourceTargetID(r.PathValue("id"))
	cfg := s.currentConfig()
	ref, ok := configuredTargetCredential(cfg, id)
	if !ok {
		writeMgmtErr(w, http.StatusNotFound, "unknown target "+string(id))
		return
	}
	name := ""
	settable := true
	if n, err := storeNameForRef(ref.SecretRef); err != nil {
		settable = false
	} else {
		name = n
	}
	resolved := false
	if v, err := s.resolveCredentialRef(ref.SecretRef); err == nil && v != "" {
		resolved = true
	}
	live := s.targetHasCaller(id)
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"target": string(id), "secret_ref": ref.SecretRef,
		"secret_name": name, "resolvable": resolved,
		"provisionable_here": settable, "live": live,
	})
}

func configuredTargetCredential(cfg *config.File, id domain.ResourceTargetID) (domain.CredentialRef, bool) {
	for i := range cfg.Targets {
		if cfg.Targets[i].ID == id {
			return cfg.Targets[i].Credential, true
		}
	}
	for i := range cfg.Channels {
		ch := &cfg.Channels[i]
		for j := range ch.Models {
			if domain.ChannelTargetID(ch.ID, ch.Models[j].Model) != string(id) {
				continue
			}
			if ch.Type == domain.ChannelCliproxyCodex {
				return domain.CredentialRef{SecretRef: domain.OAuthRef(ch.ID)}, true
			}
			return ch.Credential, true
		}
	}
	return domain.CredentialRef{}, false
}

// setSecret validates + persists the body value under name, then republishes.
// targetID rides along purely for the response payload.
func (s *Server) setSecret(w http.ResponseWriter, r *http.Request,
	targetID domain.ResourceTargetID, name string) {
	if s.deps.Store == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "secret store not configured")
		return
	}
	if !secretNameOK.MatchString(name) {
		writeMgmtErr(w, http.StatusBadRequest, "invalid secret name")
		return
	}
	var body setSecretBody
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	body.Value = strings.TrimSpace(body.Value)
	if body.Value == "" {
		writeMgmtErr(w, http.StatusBadRequest, "value must be non-empty")
		return
	}
	if err := s.deps.Store.Set(name, body.Value); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "set secret: "+err.Error())
		return
	}
	s.advanceSecretGeneration(name)
	resp := map[string]any{"stored": name}
	out := http.StatusOK
	if msg, ok := s.republish(); !ok {
		// key saved but the world didn't refresh — surface loudly, don't hide it
		resp["warning"] = msg
		out = http.StatusMultiStatus
	}
	if err := s.markProviderModelSyncDueForSecret(name); err != nil {
		if warning, ok := resp["warning"].(string); ok && warning != "" {
			resp["warning"] = warning + "; provider model synchronization could not be scheduled"
		} else {
			resp["warning"] = "provider model synchronization could not be scheduled"
		}
		out = http.StatusMultiStatus
	}
	if targetID != "" {
		resp["target"] = string(targetID)
	}
	writeMgmtJSON(w, out, resp)
}

// republish re-runs the publish pipeline against the live config so targets
// whose credential just became resolvable gain egress callers. Returns
// (message, false) when the refresh failed.
func (s *Server) republish() (string, bool) {
	if s.deps.Publish == nil {
		return "no publish pipeline configured", false
	}
	// Secret and OAuth mutations can arrive concurrently. Their publishers must
	// not overlap: an older publish may have already resolved the previous store
	// value and otherwise finish after a newer publish, reverting the scheduler
	// to a world that no longer matches the store. The exclusive config lock is
	// the single transaction boundary for both config and credential refreshes.
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := s.deps.Publish(s.deps.Config); err != nil {
		return "republish failed: " + err.Error(), false
	}
	return "", true
}

// refreshRuntime updates live callers/eligibility after credential-only
// changes without manufacturing a durable configuration revision.
func (s *Server) refreshRuntime() (string, bool) {
	if s.deps.RefreshRuntime == nil {
		return s.republish()
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := s.deps.RefreshRuntime(s.deps.Config); err != nil {
		return "runtime refresh failed: " + err.Error(), false
	}
	return "", true
}

// resolveCredential materializes the target's current secret through the
// store-backed resolver (env refs fall back to same-named store entries) to
// report whether the target could go live right now. oauth: refs resolve
// through the OAuth manager instead — resolvable means "logged in". Native
// OAuth readiness uses read-only account status, never the resolver placeholder.
func (s *Server) resolveCredentialRef(ref string) (string, error) {
	if account, isOAuth := domain.OAuthAccountFromRef(ref); isOAuth {
		if s.deps.OAuth == nil {
			return "", errStatic("oauth accounts not configured")
		}
		tok, _, err := s.deps.OAuth.Identity(context.Background(), account)
		return tok, err
	}
	r := &config.Resolver{NativeOAuth: s.deps.NativeOAuth}
	if s.deps.Store != nil {
		r.Store = s.deps.Store.Get
	}
	if driver, account, native := nativeauth.ParseRef(ref); native {
		if r.NativeOAuth == nil {
			return "", errStatic("native authentication unavailable")
		}
		view, err := r.NativeOAuth.Status(driver, account)
		if err != nil {
			return "", err
		}
		if view.Status != "connected" {
			return "", errStatic("native account is not connected")
		}
	}
	return r.Resolve(ref)
}

// targetHasCaller reports whether the live world has an egress caller for the
// target, i.e. its credential resolved and the target can actually serve.
func (s *Server) targetHasCaller(id domain.ResourceTargetID) bool {
	if s.deps.Scheduler == nil {
		return false
	}
	return s.deps.Scheduler.HasCaller(id)
}

// storeNameForRef maps a target's SecretRef to the store entry name it can be
// provisioned under: store:NAME → NAME, env:VAR → VAR (env fallback rule).
// oauth: refs are managed by the login flow, not by secret entry.
func storeNameForRef(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "store:"):
		n := ref[len("store:"):]
		if !secretNameOK.MatchString(n) {
			return "", errBadName(n)
		}
		return n, nil
	case strings.HasPrefix(ref, "env:"):
		n := ref[len("env:"):]
		if !secretNameOK.MatchString(n) {
			return "", errBadName(n)
		}
		return n, nil
	case strings.HasPrefix(ref, domain.OAuthRefPrefix):
		return "", errStatic("oauth accounts are provisioned by browser login, not by entering a key")
	case ref == "":
		return "", errStatic("target has no credential configured")
	default:
		return "", errStatic("only store:/env: refs can be provisioned via console")
	}
}

type errBadName string

func (e errBadName) Error() string { return "invalid secret name " + string(e) }

type errStatic string

func (e errStatic) Error() string { return string(e) }
