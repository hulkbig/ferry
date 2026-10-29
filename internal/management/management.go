// Package management serves the local admin plane (PRD §11, §16): resource
// live state, ledger queries, policy validate/dry-run/publish/rollback, and a
// single-page console. Bound to 127.0.0.1 only.
package management

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"ferry/internal/billing/pricingsync"
	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/ledger"
	"ferry/internal/nativeauth"
	"ferry/internal/oauth"
	"ferry/internal/policy"
	"ferry/internal/runtime"
	sched "ferry/internal/scheduler"
	"ferry/internal/secretstore"
	skillassets "ferry/skills/ferry"
)

// Deps wires the management plane to live components.
type Deps struct {
	Scheduler *sched.Scheduler
	Manager   *runtime.Manager
	Ledger    *ledger.Ledger
	Config    *config.File
	Compiler  *policy.Compiler
	// Resolver materializes custom-channel credential references for the
	// ledger-free channel connectivity test.
	Resolver *config.Resolver
	// Store holds console-provisioned secrets; nil disables the secrets API.
	Store *secretstore.Store
	// APIKeys exposes identity and safe secret metadata. Raw API key material is
	// available only through explicit create/rotation/reveal responses.
	APIKeys APIKeyStore
	// Control is the draft/revision source. Draft mutations fail closed when it
	// is unavailable; retired legacy-only APIs stay offline.
	Control *controlstore.Store
	// Security persists the single administrator, Argon2id verifier, sessions,
	// CSRF digests and security audit events. Production must configure it;
	// small embedders may leave it nil to omit the management login surface.
	Security ManagementSecurityStore
	// NativeOAuth manages native provider accounts; nil disables native OAuth.
	NativeOAuth *nativeauth.Manager
	// OAuth manages built-in Codex accounts; nil disables the OAuth routes.
	OAuth   *oauth.Manager
	Publish func(f *config.File) error
	// RefreshRuntime recompiles the active immutable config against current
	// secret/OAuth state without creating a configuration revision.
	RefreshRuntime func(f *config.File) error
	// ReloadAPIKeys atomically refreshes data-plane API key authenticators
	// without creating an unrelated configuration revision.
	ReloadAPIKeys func() error
	// ControlVersion reads the active immutable SQLite config version. Visual
	// editors round-trip this value as expected_version so an older full config
	// cannot overwrite newer channel or consumer mutations.
	ControlVersion func() (int64, error)
	// Rollback restores the previous persisted control-plane version. When nil,
	// tests and embedders fall back to the last in-memory config.
	Rollback func() (*config.File, error)
	// PolicyHistory exposes immutable SQLite policy revisions for per-policy
	// inspection and rollback.
	PolicyHistory PolicyHistory
	Timezone      *time.Location
	// InferenceHTTPClient returns the live data-plane client for one Connection.
	// Connection tests wrap its Transport with a short total timeout so they
	// exercise the same pooled path as real traffic.
	InferenceHTTPClient func(connectionID string) *http.Client
	// CodexReleaseHTTPClient fetches public GitHub releases with a bounded default.
	CodexReleaseHTTPClient *http.Client
	// ProviderModelHTTPClient performs catalog-only, non-inference upstream GETs.
	// Production uses a bounded default; tests can inject a controlled client.
	ProviderModelHTTPClient *http.Client
	// ModelsDevHTTPClient fetches the community cross-provider capability
	// aggregate (models.dev) that fills defaults for catalogs disclosing only
	// model identifiers. Nil disables the fallback layer entirely; production
	// wires a bounded client so tests never observe background fetches.
	ModelsDevHTTPClient *http.Client
	Now                 func() time.Time
}

type APIKeyStore interface {
	APIKeys() ([]domain.APIKeyIdentity, map[string][]domain.APIKeySecret, error)
}

type PolicyHistory interface {
	PolicyRevisions(id string) ([]domain.RoutingPolicyRevision, error)
	PolicyRevision(id string, revision int64) (*domain.RoutingPolicyRevision, error)
}

// Server is the management HTTP server.
type Server struct {
	pricing    *pricingsync.Cache
	deps       Deps
	prevConfig *config.File // last-but-one config for rollback
	configMu   sync.RWMutex
	importMu   sync.Mutex
	// credentialMu linearizes OAuth identity replacement with account-backed
	// catalog reads. Stored API keys use the per-name generation map below so
	// their existing concurrent set/delete runtime convergence is preserved.
	credentialMu       sync.RWMutex
	secretGenerationMu sync.Mutex
	secretGenerations  map[string]uint64
	modelSyncMu        sync.Mutex
	modelSyncRunning   map[string]bool
	modelSyncSlots     chan struct{}
	modelSyncStart     sync.Once
	usageResetStart    sync.Once
	usageResetPolicyMu sync.RWMutex
	usageProtectionMu  sync.RWMutex
	codexUpdateMu      sync.Mutex
	codexUpdateStart   sync.Once
	modelsDev          modelsDevCatalogCache
	capabilityMu       sync.Mutex
	capabilityFlights  map[string]*capabilityFlight
	capabilitySlots    chan struct{}
	capabilityEpoch    uint64
}

// New builds the management server.
func New(d Deps) *Server {
	s := &Server{
		deps: d, secretGenerations: map[string]uint64{},
		modelSyncRunning: map[string]bool{}, modelSyncSlots: make(chan struct{}, 4),
	}
	s.pricing = pricingsync.NewCache(d.Control)
	if d.OAuth != nil {
		d.OAuth.SetUsageProtection(s.usageProtectionPolicy, s.observeProtectedUsage)
		d.OAuth.SetChangeHandler(func(account string) {
			s.credentialMu.Lock()
			defer s.credentialMu.Unlock()
			if d.RefreshRuntime != nil {
				if message, ok := s.refreshRuntime(); !ok {
					log.Printf("oauth account %s: %s", account, message)
				}
			}
			if err := s.markProviderModelSyncDueForOAuthAccount(account); err != nil {
				log.Printf("oauth account %s: provider model synchronization could not be scheduled", account)
			}
		})
	}
	return s
}

// currentConfig returns the immutable live config pointer under the same lock
// used by channel CRUD and policy publish/rollback. Published configs are
// never mutated after the pointer swap, so callers can safely retain the
// returned snapshot after the read lock is released.
func (s *Server) currentConfig() *config.File {
	s.configMu.RLock()
	f := s.deps.Config
	s.configMu.RUnlock()
	return f
}

// controlVersionLocked reads the durable version while the caller holds
// configMu. The management lock serializes all in-process control mutations;
// the SQLite version remains the authoritative compare value across reloads.
func (s *Server) controlVersionLocked() (int64, error) {
	if s.deps.ControlVersion == nil {
		return 0, fmt.Errorf("control version provider not configured")
	}
	version, err := s.deps.ControlVersion()
	if err != nil {
		return 0, fmt.Errorf("read control version: %w", err)
	}
	if version <= 0 {
		return 0, fmt.Errorf("invalid control version %d", version)
	}
	return version, nil
}

// Routes returns the management mux.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleManagementHealth)
	mux.HandleFunc("GET /version", s.handleManagementVersion)
	mux.HandleFunc("GET /api/auth/status", s.handleManagementAuthStatus)
	mux.HandleFunc("POST /api/auth/initialize", s.handleManagementInitialize)
	mux.HandleFunc("POST /api/auth/login", s.handleManagementLogin)
	mux.HandleFunc("GET /api/auth/session", s.handleManagementSession)
	mux.HandleFunc("POST /api/auth/logout", s.handleManagementLogout)
	mux.HandleFunc("POST /api/auth/password", s.handleManagementPasswordChange)
	mux.HandleFunc("GET /api/access", s.handleV2Access)
	mux.HandleFunc("GET /api/security-events", s.handleManagementSecurityEvents)
	mux.HandleFunc("GET /api/management-tokens", s.handleV2ManagementTokens)
	mux.HandleFunc("POST /api/management-tokens", s.handleV2ManagementTokenCreate)
	mux.HandleFunc("GET /api/management-tokens/{id}", s.handleV2ManagementTokenDetail)
	mux.HandleFunc("PATCH /api/management-tokens/{id}", s.handleV2ManagementTokenPatch)
	mux.HandleFunc("DELETE /api/management-tokens/{id}", s.handleV2ManagementTokenDelete)
	mux.HandleFunc("POST /api/management-tokens/{id}/rotate", s.handleV2ManagementTokenRotate)
	mux.HandleFunc("POST /api/management-tokens/{id}/revoke", s.handleV2ManagementTokenRevoke)
	mux.HandleFunc("GET /api/secrets", s.handleSecretsList)
	mux.HandleFunc("POST /api/secrets/{name}", s.handleSecretSet)
	mux.HandleFunc("DELETE /api/secrets/{name}", s.handleSecretDelete)
	mux.HandleFunc("POST /api/imports/preview", s.handleImportPreview)
	mux.HandleFunc("POST /api/imports/apply", s.handleImportApply)
	mux.HandleFunc("GET /api/overview", s.handleV2Overview)
	mux.HandleFunc("GET /api/analytics", s.handleAnalytics)
	mux.HandleFunc("GET /api/organizations", s.handleV2Organizations)
	mux.HandleFunc("GET /api/usage-billing", s.handleV2UsageBilling)
	mux.HandleFunc("GET /api/usage-billing/pricing", s.handleReferencePricingGet)
	mux.HandleFunc("POST /api/usage-billing/pricing/refresh", s.handleReferencePricingRefresh)
	mux.HandleFunc("GET /api/alerts", s.handleV2Alerts)
	mux.HandleFunc("POST /api/alerts/{id}/acknowledge", s.handleV2AlertAcknowledge)
	mux.HandleFunc("GET /api/models", s.handleV2Models)
	mux.HandleFunc("POST /api/models", s.handleV2ModelCreate)
	mux.HandleFunc("GET /api/models/{id}", s.handleV2ModelDetail)
	mux.HandleFunc("PATCH /api/models/{id}", s.handleV2ModelPatch)
	mux.HandleFunc("DELETE /api/models/{id}", s.handleV2ModelDelete)
	mux.HandleFunc("POST /api/models/{id}/overrides", s.handleV2OverrideCreate)
	mux.HandleFunc("PATCH /api/models/{id}/overrides/{override_id}", s.handleV2OverridePatch)
	mux.HandleFunc("DELETE /api/models/{id}/overrides/{override_id}", s.handleV2OverrideDelete)
	mux.HandleFunc("POST /api/models/{id}/simulate", s.handleV2ModelSimulate)
	mux.HandleFunc("POST /api/models/{id}/routing/review", s.handleV2ModelRoutingReview)
	mux.HandleFunc("POST /api/models/{id}/routing/apply", s.handleV2ModelRoutingApply)
	mux.HandleFunc("GET /api/billing/plan-templates", s.handleBillingPlanTemplates)
	mux.HandleFunc("GET /api/billing/pricing-sync", s.handleBillingPricingSync)
	mux.HandleFunc("GET /api/providers", s.handleV2Providers)
	mux.HandleFunc("POST /api/providers/discover-models", s.handleV2ProviderModelDiscovery)
	mux.HandleFunc("POST /api/providers/discover-protocols", s.handleV2ProviderProtocolDiscovery)
	mux.HandleFunc("POST /api/providers", s.handleV2ProviderCreate)
	mux.HandleFunc("GET /api/providers/{id}", s.handleV2ProviderDetail)
	mux.HandleFunc("PATCH /api/providers/{id}", s.handleV2ProviderPatch)
	mux.HandleFunc("POST /api/providers/{id}/models/sync", s.handleV2ProviderModelSync)
	mux.HandleFunc("PUT /api/providers/{id}/protocol-config", s.handleV2ProviderProtocolConfig)
	mux.HandleFunc("DELETE /api/providers/{id}", s.handleV2ProviderDelete)
	mux.HandleFunc("POST /api/providers/{id}/connections", s.handleV2ConnectionCreate)
	mux.HandleFunc("PATCH /api/providers/{id}/connections/{connection_id}", s.handleV2ConnectionPatch)
	mux.HandleFunc("DELETE /api/providers/{id}/connections/{connection_id}", s.handleV2ConnectionDelete)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/test", s.handleV2ConnectionTest)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/rotate-secret", s.handleV2ConnectionSecretRotate)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/clear-cooldown", s.handleV2ConnectionClearCooldown)
	mux.HandleFunc("POST /api/runtime/clear-cooldown", s.handleRuntimeClearCooldown)
	mux.HandleFunc("POST /api/providers/{id}/clear-cooldown", s.handleRuntimeClearCooldown)
	mux.HandleFunc("POST /api/models/{id}/clear-cooldown", s.handleRuntimeClearCooldown)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/set-quota", s.handleV2ConnectionSetQuota)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/oauth/login", s.handleV2ConnectionOAuthLogin)
	mux.HandleFunc("GET /api/providers/{id}/connections/{connection_id}/oauth/status", s.handleV2ConnectionOAuthStatus)
	mux.HandleFunc("DELETE /api/providers/{id}/connections/{connection_id}/oauth/login", s.handleV2ConnectionOAuthCancel)
	mux.HandleFunc("DELETE /api/providers/{id}/connections/{connection_id}/oauth", s.handleV2ConnectionOAuthDisconnect)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/oauth/callback", s.handleV2ConnectionOAuthCallback)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/oauth/import", s.handleV2ConnectionOAuthImport)
	mux.HandleFunc("GET /api/providers/{id}/connections/{connection_id}/oauth/usage", s.handleV2ConnectionOAuthUsage)
	mux.HandleFunc("GET /api/oauth/native/drivers", s.handleNativeOAuthDrivers)
	mux.HandleFunc("POST /api/oauth/native/{driver}/accounts/{account}/login", s.handleNativeOAuthBegin)
	mux.HandleFunc("POST /api/oauth/native/{driver}/accounts/{account}/complete", s.handleNativeOAuthComplete)
	mux.HandleFunc("GET /api/oauth/native/{driver}/accounts/{account}/status", s.handleNativeOAuthStatus)
	mux.HandleFunc("DELETE /api/oauth/native/{driver}/accounts/{account}/login", s.handleNativeOAuthDisconnect)
	mux.HandleFunc("GET /api/oauth/usage-reset-requests", s.handleUsageResetRequests)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/oauth/usage-reset-requests", s.handleUsageResetPrepare)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/oauth/usage-reset-requests/{request_id}/confirm", s.handleUsageResetConfirm)
	mux.HandleFunc("POST /api/providers/{id}/connections/{connection_id}/oauth/usage-reset-requests/{request_id}/cancel", s.handleUsageResetCancel)
	mux.HandleFunc("GET /api/requests", s.handleV2Requests)
	mux.HandleFunc("GET /api/queue", s.handleQueue)
	mux.HandleFunc("GET /api/requests/{id}", s.handleV2RequestDetail)
	mux.HandleFunc("GET /api/settings", s.handleV2SettingsGet)
	mux.HandleFunc("PATCH /api/settings", s.handleV2SettingsPatch)
	mux.HandleFunc("GET /api/api-keys", s.handleV2APIKeys)
	mux.HandleFunc("POST /api/api-keys", s.handleV2APIKeyCreate)
	mux.HandleFunc("GET /api/api-keys/{id}", s.handleV2APIKeyDetail)
	mux.HandleFunc("GET /api/api-keys/{id}/secret", s.handleV2APIKeyReveal)
	mux.HandleFunc("PATCH /api/api-keys/{id}", s.handleV2APIKeyPatch)
	mux.HandleFunc("DELETE /api/api-keys/{id}", s.handleV2APIKeyDelete)
	mux.HandleFunc("POST /api/api-keys/{id}/rotate", s.handleV2APIKeyRotate)
	mux.HandleFunc("POST /api/api-keys/{id}/revoke", s.handleV2APIKeyRevoke)
	mux.HandleFunc("POST /api/api-keys/{id}/secrets/{secret_id}/retire", s.handleV2APIKeySecretRetire)
	mux.HandleFunc("POST /api/drafts", s.handleV2DraftCreate)
	mux.HandleFunc("GET /api/drafts/{id}", s.handleV2DraftDetail)
	mux.HandleFunc("PATCH /api/drafts/{id}", s.handleV2DraftPatch)
	mux.HandleFunc("DELETE /api/drafts/{id}", s.handleV2DraftDelete)
	mux.HandleFunc("POST /api/drafts/{id}/validate", s.handleV2DraftValidate)
	mux.HandleFunc("POST /api/drafts/{id}/simulate", s.handleV2DraftSimulate)
	mux.HandleFunc("POST /api/drafts/{id}/apply", s.handleV2DraftApply)
	mux.HandleFunc("GET /api/revisions", s.handleV2Revisions)
	mux.HandleFunc("GET /api/revisions/{revision}", s.handleV2RevisionDetail)
	mux.HandleFunc("POST /api/revisions/{revision}/restore-draft", s.handleV2RevisionRestoreDraft)
	// Unknown and retired API paths stay offline instead of falling through to
	// the console's static-asset handler or exposing method-based route hints.
	mux.HandleFunc("/api/", http.NotFound)
	mux.Handle("/skills/ferry/", skillassets.Handler())
	mux.Handle("/skills/smartapi/", skillassets.LegacyHandler())
	for _, alias := range []string{"/SKILL.md", "/skill.md"} {
		mux.HandleFunc("GET "+alias, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/skills/ferry/SKILL.md", http.StatusTemporaryRedirect)
		})
	}
	mux.HandleFunc("GET /install.md", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/skills/ferry/INSTALL.md", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/", s.handleConsole)
	return s.managementAuthMiddleware(mux)
}

func writeMgmtJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMgmtErr(w http.ResponseWriter, status int, msg string) {
	writeMgmtJSON(w, status, map[string]string{"error": msg})
}
