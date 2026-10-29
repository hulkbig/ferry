package management

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"ferry/internal/config"
	"ferry/internal/credentialimport"
	"ferry/internal/domain"
	"ferry/internal/nativeauth"
)

type importResult struct {
	credentialimport.Item
	Status        string `json:"status"`
	Message       string `json:"message,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

func decodeImport(w http.ResponseWriter, r *http.Request) (credentialimport.Request, credentialimport.Plan, bool) {
	w.Header().Set("Cache-Control", "no-store")
	var request credentialimport.Request
	// JSON escaping can expand the upload; the parser separately bounds content.
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6*credentialimport.MaxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeMgmtErr(w, 400, "invalid import request or upload too large")
		return request, credentialimport.Plan{}, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeMgmtErr(w, 400, "expected one import request")
		return request, credentialimport.Plan{}, false
	}
	plan, err := credentialimport.Parse(request)
	if err != nil {
		writeMgmtErr(w, 400, err.Error())
		return request, plan, false
	}
	return request, plan, true
}
func (s *Server) handleImportPreview(w http.ResponseWriter, r *http.Request) {
	_, plan, ok := decodeImport(w, r)
	if !ok {
		return
	}
	cfg := s.currentConfig()
	results := make([]importResult, 0, len(plan.Items))
	for _, item := range plan.Items {
		status := "ready"
		if item.Error != "" {
			status = "unsupported"
		} else if importExists(cfg, item) {
			status = "exists"
		}
		results = append(results, importResult{Item: item, Status: status})
	}
	writeMgmtJSON(w, 200, map[string]any{"items": results, "warnings": plan.Warnings})
}
func importExists(cfg *config.File, item credentialimport.Item) bool {
	if item.Kind == "client_key" {
		return findConsumerIndex(cfg, item.ID) >= 0
	}
	return providerIndex(cfg, item.ID) >= 0 || connectionIndexV2(cfg, item.ID) >= 0
}
func (s *Server) handleImportApply(w http.ResponseWriter, r *http.Request) {
	request, plan, ok := decodeImport(w, r)
	if !ok {
		return
	}
	if !s.requireV2Control(w) {
		return
	}
	if len(request.Selected) == 0 {
		writeMgmtErr(w, 400, "select explicit item IDs from the preview")
		return
	}
	selected := map[string]bool{}
	known := map[string]bool{}
	for _, item := range plan.Items {
		known[item.ID] = true
	}
	for _, id := range request.Selected {
		if !known[id] || selected[id] {
			writeMgmtErr(w, 400, "unknown or duplicate selected item ID")
			return
		}
		selected[id] = true
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	results := []importResult{}
	partial := false
	for _, item := range plan.Items {
		if r.Context().Err() != nil {
			break
		}
		if !selected[item.ID] {
			continue
		}
		result := importResult{Item: item}
		if item.Error != "" {
			result.Status = "unsupported"
			partial = true
		} else {
			result = s.applyImportItem(item)
			partial = partial || result.Status == "failed"
		}
		results = append(results, result)
	}
	status := http.StatusOK
	if partial {
		status = http.StatusMultiStatus
	}
	writeMgmtJSON(w, status, map[string]any{"items": results, "warnings": plan.Warnings})
}
func (s *Server) applyImportItem(item credentialimport.Item) importResult {
	result := importResult{Item: item, Status: "failed"}
	if s.deps.Compiler == nil || s.deps.Publish == nil {
		result.Message = "Configuration publish pipeline unavailable"
		return result
	}
	if blockingV2MigrationIssue(s.currentConfig()) != nil {
		result.Message = "Resolve existing configuration migration issues before importing"
		return result
	}
	if importExists(s.currentConfig(), item) {
		result.Status = "exists"
		result.Message = "Existing object was not changed"
		return result
	}
	// Credential names are fresh per attempt. On a failed publish the response
	// identifies the retained, unbound credential; retry never replaces it.
	account := "imp-" + strings.ToLower(rand.Text())
	ref := "store:" + account
	if item.Kind != "client_key" {
		var err error
		switch item.Kind {
		case "api_key":
			if s.deps.Store == nil {
				result.Message = "Secret store unavailable"
				return result
			}
			err = s.deps.Store.SetIfAbsent(account, item.Secret)
		case "codex":
			if s.deps.OAuth == nil {
				result.Message = "Codex OAuth manager unavailable"
				return result
			}
			ref = domain.OAuthRef(account)
			_, err = s.deps.OAuth.ImportNewAuthJSON(account, item.Auth)
		case "claude":
			if s.deps.NativeOAuth == nil {
				result.Message = "Native OAuth manager unavailable"
				return result
			}
			ref = nativeauth.Ref("claude", account)
			_, err = s.deps.NativeOAuth.ImportNewClaude(account, *item.Native)
		default:
			result.Message = "Unsupported credential"
			return result
		}
		if err != nil {
			result.Message = "Credential storage failed; no provider was created"
			return result
		}
		result.CredentialRef = ref
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if importExists(s.deps.Config, item) {
		result.Status = "exists"
		result.Message = "Object appeared during import; retained credential is unbound"
		return result
	}
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		result.Message = "Configuration unavailable; any stored credential is retained but unbound"
		return result
	}
	if item.Kind == "client_key" {
		// Refuse duplicate client credentials across identities, even disabled ones.
		authenticators, err := s.deps.Control.APIKeyAuthenticators()
		if err != nil {
			result.Message = "Unable to check existing client keys"
			return result
		}
		hash := domain.HashAPIKeySecret(item.Secret)
		for _, auth := range authenticators {
			for _, existing := range auth.SecretHashes {
				if existing == hash {
					result.Status = "exists"
					result.Message = "Client credential already belongs to an existing identity; no change"
					return result
				}
			}
		}
		// Key text enters only the existing sealed API-key persistence boundary.
		disabled := false
		next.ConsumerKeys = append(next.ConsumerKeys, config.ConsumerKey{Name: item.ID, Key: item.Secret, Enabled: &disabled})
	} else {
		next.V2DomainSource = config.V2DomainSourceNative
		provider := domain.Provider{ID: item.ID, Name: item.Name, Type: item.ProviderType, BaseURL: item.BaseURL, Enabled: false, ModelSyncInterval: domain.ProviderModelSyncInterval("off"), Connections: []domain.ConnectionRef{{ID: item.ID}}}
		connection := domain.Connection{RuntimeID: "conn_" + rand.Text(), ID: item.ID, ProviderID: item.ID, Identity: item.Name, Enabled: false, Credential: domain.CredentialRef{SecretRef: ref}}
		if item.Kind == "api_key" {
			connection.Credential.Header = item.Header
			connection.Credential.Prefix = item.Prefix
		}

		for _, model := range item.Models {
			connection.Models = append(connection.Models, domain.ConnectionModel{UpstreamModel: model, Protocols: []domain.Protocol{item.Protocol}, Capabilities: domain.Capabilities{Text: true}})
		}
		next.Providers = append(next.Providers, provider)
		next.Connections = append(next.Connections, connection)
	}
	if err := s.publishConfig(next); err != nil {
		result.Message = "Configuration publish failed; read back before retrying. Any stored credential is retained but unbound"
		return result
	}
	result.Status = "imported"
	result.Message = "Imported disabled; review configuration before enabling"
	return result
}
