package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/nativeauth"
	"ferry/internal/oauth"
)

const (
	providerModelSyncTimeout      = 15 * time.Second
	providerModelSyncPollInterval = 30 * time.Second
	providerModelSyncErrorLimit   = 240
)

type v2ProviderModelSyncView struct {
	Interval             domain.ProviderModelSyncInterval `json:"interval"`
	Status               string                           `json:"status"`
	LastStartedAt        *time.Time                       `json:"last_started_at"`
	LastCompletedAt      *time.Time                       `json:"last_completed_at"`
	LastSuccessAt        *time.Time                       `json:"last_success_at"`
	NextRunAt            *time.Time                       `json:"next_run_at"`
	LastErrorCode        string                           `json:"last_error_code"`
	LastError            string                           `json:"last_error"`
	DiscoveredModels     int                              `json:"discovered_models"`
	AddedModels          int                              `json:"added_models"`
	UpdatedModels        int                              `json:"updated_models"`
	CreatedLogicalModels int                              `json:"created_logical_models"`
	Revision             int64                            `json:"revision"`
}

type providerModelSyncResult struct {
	State                  controlstore.ProviderModelSyncState
	AddedModelIDs          []string
	UpdatedModelIDs        []string
	CreatedLogicalModelIDs []string
	CodexCatalog           *codexCatalogSyncEvidence
	HTTPStatus             int
}

type providerModelSyncFailure struct {
	code       string
	message    string
	httpStatus int
}

func (f *providerModelSyncFailure) Error() string { return f.message }

type providerModelCatalog struct {
	connectionID string
	protocol     domain.Protocol
	models       []string
	// entries holds the per-model capability metadata for catalog sources that
	// disclose it (Codex OAuth). A missing key means "no metadata; use the
	// conservative protocol default".
	entries map[string]oauth.CatalogModel
	// declared holds authoritative per-model capability declarations parsed
	// from the upstream catalog itself (OpenRouter model metadata). A missing
	// key falls back to the community aggregate, then to the conservative
	// protocol default.
	declared map[string]providerCatalogModelEntry
}

type providerModelCatalogReadKey struct {
	baseURL      string
	protocol     domain.Protocol
	header       string
	queryParam   string
	prefix       string
	secretDigest [sha256.Size]byte
}

type providerModelCatalogRead struct {
	models  []string
	entries map[string]providerCatalogModelEntry
}

// codexSyncedModelPlan resolves the protocol and declared capabilities for a
// Codex model adopted through provider model sync. Remote catalog metadata
// upgrades the declared contract from the conservative default; the built-in
// catalog stays authoritative for known models via normalization.
func codexSyncedModelPlan(modelID string, entry oauth.CatalogModel, hasEntry bool, fallback domain.Protocol) (domain.Protocol, domain.Capabilities) {
	if !hasEntry {
		return domain.CodexModelNativeProtocol(modelID), conservativeCatalogCapabilities(domain.CodexModelNativeProtocol(modelID))
	}
	imageOnly := entry.ImageInput && !entry.TextInput
	protocol := domain.CodexCatalogModelProtocol(imageOnly)
	capabilities := domain.CodexCatalogModelCapabilities(imageOnly, entry.ImageInput, entry.ContextWindow)
	capabilities.Priority = !imageOnly && entry.Priority != nil && *entry.Priority
	return protocol, capabilities
}

// Refresh account-scoped tier metadata even for models already in the preset.
// An absent upstream field is unknown and must not erase an existing declaration.
func applyCodexPriority(connection *domain.Connection, positions []int, modelID string, entry oauth.CatalogModel, updated map[string]bool) {
	if entry.Priority == nil {
		return
	}
	for _, index := range positions {
		model := &connection.Models[index]
		priority := *entry.Priority && domain.HasProtocol(model.Protocols, domain.ProtocolOpenAIResponses)
		if model.Capabilities.Priority != priority {
			model.Capabilities.Priority = priority
			updated[modelID] = true
		}
	}
}

// Reconcile existing models as well as new additions. Catalog visibility only
// controls discovery; a configured hidden model still has account evidence.
// Refine the contract additively so omitted metadata cannot erase operator
// declarations. Priority retains its separate explicit-revocation semantics.
func applyCodexCatalogCapabilities(connection *domain.Connection, positions []int, modelID string, entry oauth.CatalogModel, updated map[string]bool) {
	protocol, declared := codexSyncedModelPlan(modelID, entry, true, domain.ProtocolOpenAIResponses)
	for _, index := range positions {
		model := &connection.Models[index]
		if !domain.HasProtocol(model.Protocols, protocol) {
			continue
		}
		capabilities := domain.NormalizeProviderModelCapabilities(domain.ProviderCliproxyCodex, modelID, model.Protocols, declared)
		capabilities.Priority = model.Capabilities.Priority
		if merged := domain.MergeCapabilities(model.Capabilities, capabilities); merged != model.Capabilities {
			model.Capabilities = merged
			updated[modelID] = true
		}
	}
	applyCodexPriority(connection, positions, modelID, entry, updated)
}

func (s *Server) modelSyncNow() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now().UTC()
	}
	return time.Now().UTC()
}

// applySynchronizedModelCapabilities reconciles the declared capabilities of
// already-synchronized models with catalog evidence. Authoritative upstream
// declarations replace the stored contract; community-aggregate fallback
// declarations only refine it additively so an operator- or probe-backed
// capability is never silently subtracted. A nil entry keeps the stored
// contract untouched, preserving pure operator configuration.
func applySynchronizedModelCapabilities(connection *domain.Connection, positions []int, providerType domain.ProviderType,
	modelID string, entry *providerCatalogModelEntry, declared domain.Capabilities, updated map[string]bool) {
	if entry == nil {
		return
	}
	for _, index := range positions {
		if index < 0 || index >= len(connection.Models) {
			continue
		}
		model := &connection.Models[index]
		capabilities := domain.NormalizeProviderModelCapabilities(providerType, modelID, model.Protocols, declared)
		if entry.Authoritative {
			// Generic catalogs do not disclose account-scoped priority support.
			capabilities.Priority = model.Capabilities.Priority
			if model.Capabilities != capabilities {
				model.Capabilities = capabilities
				updated[modelID] = true
			}
			continue
		}
		if merged := domain.MergeCapabilities(model.Capabilities, capabilities); merged != model.Capabilities {
			model.Capabilities = merged
			updated[modelID] = true
		}
	}
}

// healUnsupportedCatalogModelCapabilities applies the community-aggregate
// fallback to a provider whose upstream catalog is unavailable. Only the
// already-configured models are refined additively: no discovery, no adds,
// and no authoritative replacement are possible without a catalog. The
// provider's sync status keeps reporting catalog_unsupported.
func (s *Server) healUnsupportedCatalogModelCapabilities(ctx context.Context, providerID, fingerprint string, state *controlstore.ProviderModelSyncState) {
	if s.deps.ModelsDevHTTPClient == nil || state == nil {
		return
	}
	file := s.currentConfig()
	provider := findProvider(file, providerID)
	if provider == nil || !provider.Enabled || provider.Type != domain.ProviderCustom || provider.ModelSyncCatalog != nil {
		return
	}
	if err := s.refreshModelsDevCatalog(ctx); err != nil {
		log.Printf("community model capability catalog unavailable for provider %s: %v", providerID, err)
		return
	}

	s.configMu.Lock()
	defer s.configMu.Unlock()
	live := s.deps.Config
	liveProvider := findProvider(live, providerID)
	if liveProvider == nil || !liveProvider.Enabled ||
		providerModelSyncFingerprint(live, providerID) != fingerprint ||
		liveProvider.Type != domain.ProviderCustom || liveProvider.ModelSyncCatalog != nil {
		return
	}
	next, err := cloneConfig(live)
	if err != nil {
		return
	}
	updated := map[string]bool{}
	for index := range next.Connections {
		connection := &next.Connections[index]
		if connection.ProviderID != providerID {
			continue
		}
		positions := map[string][]int{}
		for modelIndex, model := range connection.Models {
			positions[model.UpstreamModel] = append(positions[model.UpstreamModel], modelIndex)
		}
		for _, model := range connection.Models {
			capabilities := s.modelsDevFallbackCapabilities(model.UpstreamModel)
			if capabilities == nil {
				continue
			}
			entry := providerCatalogModelEntry{Capabilities: *capabilities}
			applySynchronizedModelCapabilities(connection, positions[model.UpstreamModel], liveProvider.Type,
				model.UpstreamModel, &entry, entry.Capabilities, updated)
		}
	}
	if len(updated) == 0 {
		return
	}
	if err := s.publishConfig(next); err != nil {
		log.Printf("provider %s capability healing publish failed", providerID)
		return
	}
	state.UpdatedModels = len(updated)
	log.Printf("provider %s catalog unsupported; refined %d configured models from the community aggregate", providerID, len(updated))
}

func providerModelSyncFingerprint(file *config.File, providerID string) string {
	if file == nil {
		return ""
	}
	var provider *domain.Provider
	for index := range file.Providers {
		if file.Providers[index].ID == providerID {
			copy := file.Providers[index]
			provider = &copy
			break
		}
	}
	if provider == nil {
		return ""
	}
	connections := make([]domain.Connection, 0, len(provider.Connections))
	for _, ref := range provider.Connections {
		for _, connection := range file.Connections {
			if connection.ID == ref.ID && connection.ProviderID == providerID {
				connections = append(connections, connection)
				break
			}
		}
	}
	version := ""
	if provider.Type == domain.ProviderCliproxyCodex {
		version = file.EffectiveCodexClientVersion()
	}
	payload, _ := json.Marshal(struct {
		CodexClientVersion string              `json:"codex_client_version,omitempty"`
		Source             string              `json:"source"`
		Provider           *domain.Provider    `json:"provider"`
		Connections        []domain.Connection `json:"connections"`
	}{version, file.V2DomainSource, provider, connections})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func findProvider(file *config.File, providerID string) *domain.Provider {
	if file == nil {
		return nil
	}
	for index := range file.Providers {
		if file.Providers[index].ID == providerID {
			return &file.Providers[index]
		}
	}
	return nil
}

func providerConnection(file *config.File, providerID, connectionID string) *domain.Connection {
	if file == nil {
		return nil
	}
	for index := range file.Connections {
		connection := &file.Connections[index]
		if connection.ID == connectionID && connection.ProviderID == providerID {
			return connection
		}
	}
	return nil
}

func (s *Server) modelSyncState(provider *domain.Provider, file *config.File, stored *controlstore.ProviderModelSyncState) v2ProviderModelSyncView {
	interval := provider.ModelSyncInterval.Effective()
	view := v2ProviderModelSyncView{Interval: interval, Status: "idle"}
	fingerprint := providerModelSyncFingerprint(file, provider.ID)
	if stored != nil {
		view = v2ProviderModelSyncView{
			Interval: interval, Status: stored.Status,
			LastStartedAt: stored.LastStartedAt, LastCompletedAt: stored.LastCompletedAt,
			LastSuccessAt: stored.LastSuccessAt, NextRunAt: stored.NextRunAt,
			LastErrorCode: stored.LastErrorCode, LastError: stored.LastError,
			DiscoveredModels: stored.DiscoveredModels, AddedModels: stored.AddedModels,
			UpdatedModels: stored.UpdatedModels, CreatedLogicalModels: stored.CreatedLogicalModels, Revision: stored.Revision,
		}
		if stored.ConfigFingerprint != fingerprint {
			view.Status, view.NextRunAt = "idle", nil
			view.LastErrorCode, view.LastError = "", ""
		}
	}
	if !provider.Enabled {
		view.Status, view.NextRunAt = "disabled", nil
	} else if interval == domain.ProviderModelSyncOff {
		view.NextRunAt = nil
		if stored == nil || stored.ConfigFingerprint != fingerprint || view.Status == "idle" {
			view.Status = "disabled"
		}
	} else if file == nil || file.V2DomainSource != config.V2DomainSourceNative {
		view.Status, view.NextRunAt = "unsupported", nil
		view.LastErrorCode = "catalog_unsupported"
		view.LastError = "automatic model synchronization requires native provider configuration"
	}
	return view
}

func (s *Server) loadProviderModelSyncState(providerID, fingerprint string) controlstore.ProviderModelSyncState {
	state := controlstore.ProviderModelSyncState{ProviderID: providerID, ConfigFingerprint: fingerprint, Status: "idle"}
	if s.deps.Control == nil {
		return state
	}
	stored, err := s.deps.Control.ProviderModelSyncState(providerID)
	if err == nil && stored != nil {
		if stored.ConfigFingerprint == fingerprint {
			return *stored
		}
		state.LastStartedAt = stored.LastStartedAt
		state.LastCompletedAt = stored.LastCompletedAt
		state.LastSuccessAt = stored.LastSuccessAt
		state.Revision = stored.Revision
	}
	return state
}

func (s *Server) saveProviderModelSyncState(state controlstore.ProviderModelSyncState) error {
	if s.deps.Control == nil {
		return fmt.Errorf("control store is not configured")
	}
	if len(state.LastError) > providerModelSyncErrorLimit {
		state.LastError = state.LastError[:providerModelSyncErrorLimit]
	}
	return s.deps.Control.SaveProviderModelSyncState(state)
}

func (s *Server) beginProviderModelSync(providerID string) bool {
	s.modelSyncMu.Lock()
	defer s.modelSyncMu.Unlock()
	if s.modelSyncRunning[providerID] {
		return false
	}
	s.modelSyncRunning[providerID] = true
	return true
}

func (s *Server) endProviderModelSync(providerID string) {
	s.modelSyncMu.Lock()
	delete(s.modelSyncRunning, providerID)
	s.modelSyncMu.Unlock()
}

func (s *Server) syncProviderModels(ctx context.Context, providerID string) providerModelSyncResult {
	if !s.beginProviderModelSync(providerID) {
		return providerModelSyncResult{HTTPStatus: http.StatusConflict}
	}
	defer s.endProviderModelSync(providerID)
	select {
	case s.modelSyncSlots <- struct{}{}:
		defer func() { <-s.modelSyncSlots }()
	case <-ctx.Done():
		return providerModelSyncResult{HTTPStatus: http.StatusRequestTimeout}
	}
	s.credentialMu.RLock()
	defer s.credentialMu.RUnlock()

	s.configMu.RLock()
	file, cloneErr := cloneConfig(s.deps.Config)
	var revision int64
	if cloneErr == nil {
		revision, cloneErr = s.controlVersionLocked()
	}
	s.configMu.RUnlock()
	if cloneErr != nil || file == nil {
		return providerModelSyncResult{HTTPStatus: http.StatusServiceUnavailable}
	}
	provider := findProvider(file, providerID)
	if provider == nil {
		return providerModelSyncResult{HTTPStatus: http.StatusNotFound}
	}
	secretGenerations := s.providerSecretGenerationSnapshot(file, provider)
	fingerprint := providerModelSyncFingerprint(file, providerID)
	state := s.loadProviderModelSyncState(providerID, fingerprint)
	state.Revision = revision
	now := s.modelSyncNow()
	interval := provider.ModelSyncInterval.Effective()
	if !provider.Enabled {
		state.Status, state.NextRunAt = "disabled", nil
		state.LastErrorCode, state.LastError = "", ""
		if err := s.saveProviderModelSyncState(state); err != nil {
			return providerModelSyncResult{State: state, HTTPStatus: http.StatusServiceUnavailable}
		}
		return providerModelSyncResult{State: state, HTTPStatus: http.StatusOK}
	}
	if file.V2DomainSource != config.V2DomainSourceNative {
		state.Status, state.NextRunAt = "unsupported", nil
		state.LastErrorCode = "catalog_unsupported"
		state.LastError = "automatic model synchronization requires native provider configuration"
		if err := s.saveProviderModelSyncState(state); err != nil {
			return providerModelSyncResult{State: state, HTTPStatus: http.StatusServiceUnavailable}
		}
		return providerModelSyncResult{State: state, HTTPStatus: http.StatusOK}
	}

	state.Status = "running"
	state.LastStartedAt = &now
	state.NextRunAt = nil
	state.LastErrorCode, state.LastError = "", ""
	state.DiscoveredModels, state.AddedModels, state.UpdatedModels, state.CreatedLogicalModels = 0, 0, 0, 0
	if err := s.saveProviderModelSyncState(state); err != nil {
		return providerModelSyncResult{State: state, HTTPStatus: http.StatusServiceUnavailable}
	}

	requestCtx, cancel := context.WithTimeout(ctx, providerModelSyncTimeout)
	catalogs, failure := s.readProviderModelCatalogs(requestCtx, file, provider)
	cancel()
	s.secretGenerationMu.Lock()
	defer s.secretGenerationMu.Unlock()
	if !s.providerSecretGenerationsMatchLocked(secretGenerations) {
		failure = &providerModelSyncFailure{
			code: "credential_changed", message: "provider credential changed during model synchronization", httpStatus: http.StatusConflict,
		}
	}
	if failure != nil {
		// A provider whose upstream exposes no model catalog can still have
		// its operator-configured models refined by the community aggregate:
		// the sync status stays honestly "unsupported" while capability
		// defaults keep updating automatically.
		if failure.code == "catalog_unsupported" {
			s.healUnsupportedCatalogModelCapabilities(ctx, providerID, fingerprint, &state)
		}
		return s.finishProviderModelSyncFailure(state, interval, failure)
	}
	// Codex owns an authoritative OAuth catalog and derives capabilities from
	// it directly; the community aggregate would only waste a fetch.
	if provider.Type != domain.ProviderCliproxyCodex {
		s.refreshModelsDevCatalogIfNeeded(ctx, catalogs)
	}

	result := providerModelSyncResult{HTTPStatus: http.StatusOK}
	s.configMu.Lock()
	live := s.deps.Config
	liveProvider := findProvider(live, providerID)
	liveFingerprint := providerModelSyncFingerprint(live, providerID)
	if liveProvider == nil || liveFingerprint != fingerprint || !liveProvider.Enabled {
		s.configMu.Unlock()
		failure = &providerModelSyncFailure{code: "configuration_changed", message: "provider configuration changed during model synchronization", httpStatus: http.StatusConflict}
		state.ConfigFingerprint = liveFingerprint
		return s.finishProviderModelSyncFailure(state, interval, failure)
	}
	next, err := cloneConfig(live)
	if err != nil {
		s.configMu.Unlock()
		return s.finishProviderModelSyncFailure(state, interval, &providerModelSyncFailure{
			code: "publish_failed", message: "configuration publish failed", httpStatus: http.StatusServiceUnavailable,
		})
	}
	discovered := map[string]bool{}
	added := map[string]bool{}
	updated := map[string]bool{}
	for _, catalog := range catalogs {
		connection := providerConnection(next, providerID, catalog.connectionID)
		if connection == nil {
			continue
		}
		existing := make(map[string]bool, len(connection.Models))
		positions := make(map[string][]int, len(connection.Models))
		for index, model := range connection.Models {
			existing[model.UpstreamModel] = true
			positions[model.UpstreamModel] = append(positions[model.UpstreamModel], index)
		}
		if liveProvider.Type == domain.ProviderCliproxyCodex {
			// Account capability evidence applies to configured models even
			// when they are hidden from discovery. Missing entries stay unknown.
			for modelID, indexes := range positions {
				if entry, ok := catalog.entries[modelID]; ok {
					applyCodexCatalogCapabilities(connection, indexes, modelID, entry, updated)
				}
			}
		}
		for _, modelID := range catalog.models {
			discovered[modelID] = true
			if !validSynchronizedModelID(modelID) {
				continue
			}
			protocol := catalog.protocol
			declared := conservativeCatalogCapabilities(protocol)
			var entry *providerCatalogModelEntry
			if liveProvider.Type != domain.ProviderCliproxyCodex {
				entry = catalogCapabilityEntry(catalog, s.modelsDevFallbackCapabilities, modelID)
			}
			if entry != nil {
				declared = entry.Capabilities
			}
			if liveProvider.Type == domain.ProviderCliproxyCodex {
				codexEntry, hasEntry := catalog.entries[modelID]
				protocol, declared = codexSyncedModelPlan(modelID, codexEntry, hasEntry, protocol)
			}
			if existing[modelID] {
				applySynchronizedModelCapabilities(connection, positions[modelID], liveProvider.Type, modelID, entry, declared, updated)
				continue
			}
			capabilities := domain.NormalizeProviderModelCapabilities(liveProvider.Type, modelID, []domain.Protocol{protocol}, declared)
			connection.Models = append(connection.Models, domain.ConnectionModel{
				UpstreamModel: modelID, Protocols: []domain.Protocol{protocol}, Capabilities: capabilities,
			})
			existing[modelID], added[modelID] = true, true
		}
	}
	created := config.EnsureSameNameLogicalModels(next)
	result.AddedModelIDs = sortedStringSet(added)
	result.UpdatedModelIDs = sortedStringSet(updated)
	result.CreatedLogicalModelIDs = append([]string(nil), created...)
	sort.Strings(result.CreatedLogicalModelIDs)
	if len(result.AddedModelIDs) > 0 || len(result.UpdatedModelIDs) > 0 || len(result.CreatedLogicalModelIDs) > 0 {
		if err := s.publishConfig(next); err != nil {
			s.configMu.Unlock()
			log.Printf("provider %s model synchronization publish failed", providerID)
			return s.finishProviderModelSyncFailure(state, interval, &providerModelSyncFailure{
				code: "publish_failed", message: "configuration publish failed", httpStatus: http.StatusServiceUnavailable,
			})
		}
		live = s.deps.Config
	}
	revision, err = s.controlVersionLocked()
	if err != nil {
		s.configMu.Unlock()
		return s.finishProviderModelSyncFailure(state, interval, &providerModelSyncFailure{
			code: "state_unavailable", message: "model synchronization state unavailable", httpStatus: http.StatusServiceUnavailable,
		})
	}
	state.ConfigFingerprint = providerModelSyncFingerprint(live, providerID)
	if liveProvider.Type == domain.ProviderCliproxyCodex {
		result.CodexCatalog = codexSyncEvidence(live, providerID, catalogs)
	}
	s.configMu.Unlock()

	completed := s.modelSyncNow()
	var nextRun *time.Time
	if delay := interval.Duration(); delay > 0 {
		value := completed.Add(delay)
		nextRun = &value
	}
	state.Status = "succeeded"
	state.LastCompletedAt, state.LastSuccessAt, state.NextRunAt = &completed, &completed, nextRun
	state.LastErrorCode, state.LastError = "", ""
	state.DiscoveredModels = len(discovered)
	state.AddedModels = len(result.AddedModelIDs)
	state.UpdatedModels = len(result.UpdatedModelIDs)
	state.CreatedLogicalModels = len(result.CreatedLogicalModelIDs)
	state.Revision = revision
	result.State = state
	if err := s.saveProviderModelSyncState(state); err != nil {
		result.HTTPStatus = http.StatusServiceUnavailable
	}
	return result
}

func (s *Server) finishProviderModelSyncFailure(state controlstore.ProviderModelSyncState, interval domain.ProviderModelSyncInterval, failure *providerModelSyncFailure) providerModelSyncResult {
	completed := s.modelSyncNow()
	var nextRun *time.Time
	if delay := interval.Duration(); delay > 0 {
		value := completed.Add(delay)
		nextRun = &value
	}
	if failure.code == "configuration_changed" || failure.code == "credential_changed" || failure.code == "interrupted" {
		nextRun = &completed
	}
	state.Status = "failed"
	if failure.code == "catalog_unsupported" {
		state.Status = "unsupported"
	}
	state.LastCompletedAt, state.NextRunAt = &completed, nextRun
	state.LastErrorCode, state.LastError = failure.code, failure.message
	if err := s.saveProviderModelSyncState(state); err != nil {
		return providerModelSyncResult{State: state, HTTPStatus: http.StatusServiceUnavailable}
	}
	return providerModelSyncResult{State: state, HTTPStatus: failure.httpStatus}
}

func (s *Server) readProviderModelCatalogs(ctx context.Context, file *config.File, provider *domain.Provider) ([]providerModelCatalog, *providerModelSyncFailure) {
	if provider.ModelSyncCatalog != nil {
		return s.readConfiguredProviderModelCatalog(ctx, file, provider)
	}
	catalogs := []providerModelCatalog{}
	oauthCatalogs := map[string][]oauth.CatalogModel{}
	// Identical sibling connections share one catalog read. OpenRouter's
	// provider-level rewrite makes its Chat, Responses, and Anthropic requests
	// identical here. Cache by effective request identity so this never
	// conflates connections with different credentials or authentication shapes.
	sharedCatalogs := map[providerModelCatalogReadKey]providerModelCatalogRead{}
	for _, ref := range provider.Connections {
		connection := providerConnection(file, provider.ID, ref.ID)
		if connection == nil || !connection.Enabled {
			continue
		}
		if provider.Type == domain.ProviderCliproxyCodex {
			account, ok := domain.OAuthAccountFromRef(connection.Credential.SecretRef)
			if !ok || s.deps.OAuth == nil {
				return nil, &providerModelSyncFailure{code: "credential_unavailable", message: "OAuth credential unavailable", httpStatus: http.StatusUnprocessableEntity}
			}
			models := oauthCatalogs[account]
			if models == nil {
				var err error
				models, err = s.deps.OAuth.ReadModelCatalogWithVersion(ctx, account, file.EffectiveCodexClientVersion())
				if err != nil {
					return nil, classifyOAuthModelCatalogFailure(err)
				}
				models = filterSynchronizedCatalogModels(models)
				if len(models) == 0 {
					return nil, &providerModelSyncFailure{code: "invalid_catalog", message: "OAuth model catalog contains no valid model identifiers", httpStatus: http.StatusBadGateway}
				}
				oauthCatalogs[account] = models
			}
			entries := make(map[string]oauth.CatalogModel, len(models))
			names := make([]string, 0, len(models))
			for _, model := range models {
				entries[model.Slug] = model
				if model.Visibility == "list" {
					names = append(names, model.Slug)
				}
			}
			catalogs = append(catalogs, providerModelCatalog{connectionID: connection.ID, protocol: domain.ProtocolOpenAIResponses, models: names, entries: entries})
			continue
		}
		if provider.Type != domain.ProviderCustom {
			return nil, &providerModelSyncFailure{code: "catalog_unsupported", message: "provider does not expose a supported catalog-only model listing", httpStatus: http.StatusOK}
		}
		if driver, _, ok := nativeauth.ParseRef(connection.Credential.SecretRef); ok && (driver == "antigravity" || driver == "devin" || driver == "vertex") {
			return nil, &providerModelSyncFailure{code: "catalog_unsupported", message: "this native channel requires explicit upstream model IDs; catalog discovery is not implemented", httpStatus: http.StatusOK}
		}
		protocol := connectionModelCatalogProtocol(connection)
		if protocol == "" {
			return nil, &providerModelSyncFailure{code: "catalog_unconfigured", message: "connection has no configured model protocol for catalog discovery", httpStatus: http.StatusUnprocessableEntity}
		}
		if s.deps.Resolver == nil {
			return nil, &providerModelSyncFailure{code: "credential_unavailable", message: "credential resolver not configured", httpStatus: http.StatusServiceUnavailable}
		}
		secret, err := s.deps.Resolver.Resolve(connection.Credential.SecretRef)
		if err != nil || strings.TrimSpace(secret) == "" {
			return nil, &providerModelSyncFailure{code: "credential_unavailable", message: "credential unavailable", httpStatus: http.StatusUnprocessableEntity}
		}
		catalogBaseURL, catalogProtocol, catalogCredential := providerModelCatalogRequest(file, provider, connection, protocol)
		client := s.deps.ProviderModelHTTPClient
		if client == nil {
			client = &http.Client{Timeout: providerModelSyncTimeout}
		}
		client, err = s.nativeProviderClient(connection.Credential.SecretRef, catalogBaseURL, connection.FullURL, catalogProtocol, client)
		if err != nil {
			return nil, &providerModelSyncFailure{code: "native_binding_invalid", message: err.Error(), httpStatus: http.StatusUnprocessableEntity}
		}
		cacheKey := providerModelCatalogReadKey{
			baseURL: catalogBaseURL, protocol: catalogProtocol,
			header: catalogCredential.Header, queryParam: catalogCredential.QueryParam, prefix: catalogCredential.Prefix,
			secretDigest: sha256.Sum256([]byte(secret)),
		}
		cached, cacheHit := sharedCatalogs[cacheKey]
		models, entries := cached.models, cached.entries
		if !cacheHit {
			models, entries, err = discoverProviderModelsWithCredential(ctx, client, catalogBaseURL, catalogProtocol, secret, catalogCredential)
		}
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, &providerModelSyncFailure{code: "timeout", message: "upstream model catalog timed out", httpStatus: http.StatusGatewayTimeout}
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, &providerModelSyncFailure{code: "interrupted", message: "model synchronization was interrupted", httpStatus: http.StatusServiceUnavailable}
			}
			var statusErr *providerModelDiscoveryHTTPError
			if errors.As(err, &statusErr) && (statusErr.Status == http.StatusNotFound || statusErr.Status == http.StatusMethodNotAllowed || statusErr.Status == http.StatusNotImplemented) {
				return nil, &providerModelSyncFailure{code: "catalog_unsupported", message: "provider does not expose a supported catalog-only model listing", httpStatus: http.StatusOK}
			}
			return nil, &providerModelSyncFailure{code: "upstream_error", message: err.Error(), httpStatus: http.StatusBadGateway}
		}
		models = filterSynchronizedModelIDs(models)
		if len(models) == 0 {
			return nil, &providerModelSyncFailure{code: "invalid_catalog", message: "upstream model catalog contains no valid model identifiers", httpStatus: http.StatusBadGateway}
		}
		if !cacheHit {
			sharedCatalogs[cacheKey] = providerModelCatalogRead{models: models, entries: entries}
		}
		catalogs = append(catalogs, providerModelCatalog{connectionID: connection.ID, protocol: protocol, models: models, declared: entries})
	}
	if len(catalogs) == 0 {
		return nil, &providerModelSyncFailure{code: "catalog_unconfigured", message: "provider has no enabled connection with a model catalog", httpStatus: http.StatusUnprocessableEntity}
	}
	return catalogs, nil
}

func (s *Server) readConfiguredProviderModelCatalog(ctx context.Context, file *config.File, provider *domain.Provider) ([]providerModelCatalog, *providerModelSyncFailure) {
	catalog := provider.ModelSyncCatalog
	if catalog == nil || catalog.Type != domain.ProviderModelSyncCatalogVolcenginePlan {
		return nil, &providerModelSyncFailure{code: "catalog_unsupported", message: "configured provider model catalog is unsupported", httpStatus: http.StatusOK}
	}
	catalogs := make([]providerModelCatalog, 0, len(provider.Connections))
	for _, ref := range provider.Connections {
		connection := providerConnection(file, provider.ID, ref.ID)
		if connection == nil || !connection.Enabled {
			continue
		}
		if driver, _, ok := nativeauth.ParseRef(connection.Credential.SecretRef); ok && (driver == "antigravity" || driver == "devin" || driver == "vertex") {
			return nil, &providerModelSyncFailure{code: "catalog_unsupported", message: "this native channel requires explicit upstream model IDs; catalog discovery is not implemented", httpStatus: http.StatusOK}
		}
		protocol := connectionModelCatalogProtocol(connection)
		if protocol == "" {
			return nil, &providerModelSyncFailure{code: "catalog_unconfigured", message: "connection has no configured model protocol for catalog discovery", httpStatus: http.StatusUnprocessableEntity}
		}
		catalogs = append(catalogs, providerModelCatalog{connectionID: connection.ID, protocol: protocol})
	}
	if len(catalogs) == 0 {
		return nil, &providerModelSyncFailure{code: "catalog_unconfigured", message: "provider has no enabled connection for its configured model catalog", httpStatus: http.StatusUnprocessableEntity}
	}
	if s.deps.Resolver == nil {
		return nil, &providerModelSyncFailure{code: "catalog_credential_unavailable", message: "model catalog credential resolver is not configured", httpStatus: http.StatusServiceUnavailable}
	}
	rawCredential, err := s.deps.Resolver.Resolve(catalog.CredentialRef)
	if err != nil || strings.TrimSpace(rawCredential) == "" {
		return nil, &providerModelSyncFailure{code: "catalog_credential_unavailable", message: "model catalog credential is unavailable", httpStatus: http.StatusUnprocessableEntity}
	}
	identity, err := parseVolcengineSigningIdentity(rawCredential)
	if err != nil {
		return nil, &providerModelSyncFailure{code: "catalog_credential_invalid", message: "model catalog credential is invalid", httpStatus: http.StatusUnprocessableEntity}
	}
	client := s.deps.ProviderModelHTTPClient
	if client == nil {
		client = &http.Client{Timeout: providerModelSyncTimeout}
	}
	models, err := discoverVolcenginePlanModels(ctx, client, catalog.Plan, identity, s.modelSyncNow())
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &providerModelSyncFailure{code: "timeout", message: "provider control-plane model catalog timed out", httpStatus: http.StatusGatewayTimeout}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, &providerModelSyncFailure{code: "interrupted", message: "model synchronization was interrupted", httpStatus: http.StatusServiceUnavailable}
		}
		var statusErr *providerModelDiscoveryHTTPError
		if errors.As(err, &statusErr) {
			switch statusErr.Status {
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, &providerModelSyncFailure{code: "catalog_credential_rejected", message: "provider control-plane rejected the model catalog credential", httpStatus: http.StatusFailedDependency}
			case http.StatusTooManyRequests:
				return nil, &providerModelSyncFailure{code: "rate_limited", message: "provider control-plane model catalog is rate limited", httpStatus: http.StatusTooManyRequests}
			}
		}
		return nil, &providerModelSyncFailure{code: "upstream_error", message: "provider control-plane model catalog request failed", httpStatus: http.StatusBadGateway}
	}
	for index := range catalogs {
		catalogs[index].models = models
	}
	return catalogs, nil
}

// providerModelCatalogRequest keeps inference endpoint paths independent from
// catalog paths. DeepSeek's official Anthropic compatibility endpoint lives
// under /anthropic, while the account catalog remains the OpenAI-shaped
// same-origin GET /models authenticated with Bearer. Restricting this rewrite
// to the exact official HTTPS origin and known paths preserves arbitrary
// custom proxy path and credential semantics.
func providerModelCatalogRequest(file *config.File, provider *domain.Provider, connection *domain.Connection, protocol domain.Protocol) (string, domain.Protocol, domain.CredentialRef) {
	baseURL := connectionBaseURL(provider, connection)
	credential := connection.Credential
	if provider == nil {
		return baseURL, protocol, credential
	}
	providerURL, providerErr := url.Parse(strings.TrimSpace(provider.BaseURL))
	connectionURL, connectionErr := url.Parse(strings.TrimSpace(baseURL))
	if providerErr != nil || connectionErr != nil {
		return baseURL, protocol, credential
	}
	if exactHTTPSOrigin(providerURL, "openrouter.ai") && exactHTTPSOrigin(connectionURL, "openrouter.ai") {
		catalogURL := *providerURL
		catalogURL.Path, catalogURL.RawPath, catalogURL.RawQuery, catalogURL.Fragment = "/api/v1", "", "", ""
		// The official provider-level catalog uses OpenAI-compatible Bearer
		// authentication. Do not let an Anthropic sibling's x-api-key transport
		// shape select a different, protocol-decorated catalog representation.
		credential.Header, credential.QueryParam, credential.Prefix = "", "", ""
		return catalogURL.String(), domain.ProtocolOpenAIChat, credential
	}
	if protocol == domain.ProtocolAnthropic &&
		exactHTTPSOrigin(providerURL, "api.deepseek.com") &&
		exactHTTPSOrigin(connectionURL, "api.deepseek.com") &&
		cleanCatalogBasePath(providerURL.Path) == "" &&
		cleanCatalogBasePath(connectionURL.Path) == "/anthropic" {
		credential.Header, credential.QueryParam, credential.Prefix = "", "", ""
		return provider.BaseURL, domain.ProtocolOpenAIChat, credential
	}
	if exactHTTPSOrigin(providerURL, "open.bigmodel.cn") &&
		exactHTTPSOrigin(connectionURL, "open.bigmodel.cn") &&
		zhipuCodingPlanCatalogProvider(file, provider, connection, cleanCatalogBasePath(providerURL.Path)) &&
		zhipuCodingPlanCatalogConnection(protocol, cleanCatalogBasePath(connectionURL.Path)) {
		catalogURL := *providerURL
		catalogURL.Path, catalogURL.RawPath, catalogURL.RawQuery, catalogURL.Fragment = "/api/coding/paas/v4", "", "", ""
		credential.Header, credential.QueryParam, credential.Prefix = "", "", ""
		return catalogURL.String(), domain.ProtocolOpenAIChat, credential
	}
	return baseURL, protocol, credential
}

func zhipuCodingPlanCatalogProvider(file *config.File, provider *domain.Provider, connection *domain.Connection, providerPath string) bool {
	if providerPath == "/api/coding/paas/v4" {
		return true
	}
	if providerPath != "/api/v1" || file == nil || connection == nil {
		return false
	}
	for _, ref := range provider.Connections {
		sibling := providerConnection(file, provider.ID, ref.ID)
		if sibling == nil || !sibling.Enabled || sibling.Credential.SecretRef != connection.Credential.SecretRef {
			continue
		}
		siblingURL, err := url.Parse(strings.TrimSpace(connectionBaseURL(provider, sibling)))
		if err == nil && exactHTTPSOrigin(siblingURL, "open.bigmodel.cn") &&
			cleanCatalogBasePath(siblingURL.Path) == "/api/coding/paas/v4" {
			return true
		}
	}
	return false
}

func zhipuCodingPlanCatalogConnection(protocol domain.Protocol, path string) bool {
	switch protocol {
	case domain.ProtocolOpenAIChat:
		return path == "/api/coding/paas/v4"
	case domain.ProtocolOpenAIResponses:
		return path == "/api/v1"
	case domain.ProtocolAnthropic:
		return path == "/api/anthropic" || path == "/api/anthropic/v1"
	default:
		return false
	}
}

func exactHTTPSOrigin(parsed *url.URL, host string) bool {
	return parsed != nil && strings.EqualFold(parsed.Scheme, "https") && parsed.User == nil &&
		parsed.Port() == "" && strings.EqualFold(parsed.Hostname(), host)
}

func cleanCatalogBasePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "/" {
		return ""
	}
	return strings.TrimSuffix(path, "/")
}

func classifyOAuthModelCatalogFailure(err error) *providerModelSyncFailure {
	if errors.Is(err, context.DeadlineExceeded) {
		return &providerModelSyncFailure{code: "timeout", message: "upstream model catalog timed out", httpStatus: http.StatusGatewayTimeout}
	}
	if errors.Is(err, context.Canceled) {
		return &providerModelSyncFailure{code: "interrupted", message: "model synchronization was interrupted", httpStatus: http.StatusServiceUnavailable}
	}
	var catalogErr *oauth.ModelCatalogError
	if errors.As(err, &catalogErr) {
		status := http.StatusBadGateway
		switch catalogErr.Code {
		case oauth.ModelCatalogAuthUnavailable, oauth.ModelCatalogAccountIdentityUnavailable:
			status = http.StatusUnprocessableEntity
		case oauth.ModelCatalogEntitlementDenied:
			status = http.StatusForbidden
		case oauth.ModelCatalogRateLimited:
			status = http.StatusTooManyRequests
		}
		return &providerModelSyncFailure{code: catalogErr.Code, message: catalogErr.Error(), httpStatus: status}
	}
	return &providerModelSyncFailure{code: "upstream_error", message: "OAuth model catalog request failed", httpStatus: http.StatusBadGateway}
}

func (s *Server) markProviderModelSyncDueForSecret(name string) error {
	if s.deps.Control == nil || name == "" {
		return nil
	}
	file := s.currentConfig()
	if file == nil {
		return nil
	}
	providerIDs := map[string]bool{}
	for index := range file.Providers {
		provider := &file.Providers[index]
		if provider.ModelSyncCatalog != nil && secretNameForModelSync(provider.ModelSyncCatalog.CredentialRef) == name {
			providerIDs[provider.ID] = true
		}
	}
	for _, connection := range file.Connections {
		ref := connection.Credential.SecretRef
		if ref == "store:"+name || ref == "env:"+name {
			providerIDs[connection.ProviderID] = true
		}
	}
	if err := s.invalidateCapabilityEvidence(providerIDs); err != nil {
		return err
	}
	return s.markProviderModelSyncProvidersDue(file, providerIDs)
}

func (s *Server) markProviderModelSyncDueForOAuthAccount(account string) error {
	if s.deps.Control == nil || account == "" {
		return nil
	}
	file := s.currentConfig()
	if file == nil {
		return nil
	}
	providerIDs := map[string]bool{}
	for _, connection := range file.Connections {
		configuredAccount, ok := domain.OAuthAccountFromRef(connection.Credential.SecretRef)
		if ok && configuredAccount == account {
			providerIDs[connection.ProviderID] = true
		}
	}
	if err := s.invalidateCapabilityEvidence(providerIDs); err != nil {
		return err
	}
	return s.markProviderModelSyncProvidersDue(file, providerIDs)
}

func (s *Server) markProviderModelSyncProvidersDue(file *config.File, providerIDs map[string]bool) error {
	states, err := s.deps.Control.ProviderModelSyncStates()
	if err != nil {
		return err
	}
	now := s.modelSyncNow()
	for providerID := range providerIDs {
		provider := findProvider(file, providerID)
		state, ok := states[providerID]
		if !ok || provider == nil || !provider.Enabled || provider.ModelSyncInterval.Effective() == domain.ProviderModelSyncOff {
			continue
		}
		state.ConfigFingerprint = providerModelSyncFingerprint(file, providerID)
		state.Status = "idle"
		state.LastErrorCode, state.LastError = "", ""
		state.NextRunAt = &now
		if err := s.saveProviderModelSyncState(state); err != nil {
			return err
		}
	}
	return nil
}

func secretNameForModelSync(ref string) string {
	for _, prefix := range []string{"store:", "env:"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	return ""
}

func (s *Server) providerSecretGenerationSnapshot(file *config.File, provider *domain.Provider) map[string]uint64 {
	names := map[string]bool{}
	if provider.ModelSyncCatalog != nil {
		if name := secretNameForModelSync(provider.ModelSyncCatalog.CredentialRef); name != "" {
			names[name] = true
		}
	}
	for _, ref := range provider.Connections {
		if connection := providerConnection(file, provider.ID, ref.ID); connection != nil {
			if name := secretNameForModelSync(connection.Credential.SecretRef); name != "" {
				names[name] = true
			}
		}
	}
	s.secretGenerationMu.Lock()
	defer s.secretGenerationMu.Unlock()
	snapshot := make(map[string]uint64, len(names))
	for name := range names {
		snapshot[name] = s.secretGenerations[name]
	}
	return snapshot
}

func (s *Server) providerSecretGenerationsMatchLocked(snapshot map[string]uint64) bool {
	for name, generation := range snapshot {
		if s.secretGenerations[name] != generation {
			return false
		}
	}
	return true
}

func (s *Server) advanceSecretGeneration(name string) {
	if name == "" {
		return
	}
	s.secretGenerationMu.Lock()
	s.secretGenerations[name]++
	s.secretGenerationMu.Unlock()
}

func connectionModelCatalogProtocol(connection *domain.Connection) domain.Protocol {
	protocols := []domain.Protocol{}
	seen := map[domain.Protocol]bool{}
	for _, model := range connection.Models {
		for _, protocol := range model.Protocols {
			if domain.ValidProtocol(protocol) && !seen[protocol] {
				seen[protocol] = true
				protocols = append(protocols, protocol)
			}
		}
	}
	return domain.PreferredProtocol(protocols)
}

func conservativeCatalogCapabilities(protocol domain.Protocol) domain.Capabilities {
	if protocol == domain.ProtocolOpenAIImages {
		return domain.Capabilities{Images: true}
	}
	return domain.Capabilities{Text: true}
}

func validSynchronizedModelID(id string) bool {
	if id == "" || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func filterSynchronizedModelIDs(models []string) []string {
	filtered := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if !validSynchronizedModelID(model) || seen[model] {
			continue
		}
		seen[model] = true
		filtered = append(filtered, model)
	}
	sort.Strings(filtered)
	return filtered
}

// filterSynchronizedCatalogModels is filterSynchronizedModelIDs for catalog
// entries that carry capability metadata. Entry order stays slug-sorted.
func filterSynchronizedCatalogModels(models []oauth.CatalogModel) []oauth.CatalogModel {
	filtered := make([]oauth.CatalogModel, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if !validSynchronizedModelID(model.Slug) || seen[model.Slug] {
			continue
		}
		seen[model.Slug] = true
		filtered = append(filtered, model)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Slug < filtered[j].Slug })
	return filtered
}

func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleV2ProviderModelSync(w http.ResponseWriter, r *http.Request) {
	result := s.syncProviderModels(r.Context(), r.PathValue("id"))
	if result.HTTPStatus == http.StatusConflict && result.State.ProviderID == "" {
		writeMgmtErr(w, result.HTTPStatus, "provider model synchronization is already running")
		return
	}
	if result.HTTPStatus == http.StatusNotFound {
		writeMgmtErr(w, result.HTTPStatus, "provider not found")
		return
	}
	if result.State.ProviderID == "" {
		writeMgmtErr(w, result.HTTPStatus, "provider model synchronization unavailable")
		return
	}
	file := s.currentConfig()
	provider := findProvider(file, result.State.ProviderID)
	view := v2ProviderModelSyncView{}
	if provider != nil {
		view = s.modelSyncState(provider, file, &result.State)
	}
	if result.AddedModelIDs == nil {
		result.AddedModelIDs = []string{}
	}
	if result.UpdatedModelIDs == nil {
		result.UpdatedModelIDs = []string{}
	}
	if result.CreatedLogicalModelIDs == nil {
		result.CreatedLogicalModelIDs = []string{}
	}
	response := map[string]any{
		"provider_id": result.State.ProviderID,
		"interval":    view.Interval, "status": view.Status,
		"last_started_at": view.LastStartedAt, "last_completed_at": view.LastCompletedAt,
		"last_success_at": view.LastSuccessAt, "next_run_at": view.NextRunAt,
		"last_error_code": view.LastErrorCode, "last_error": view.LastError,
		"discovered_models": view.DiscoveredModels, "added_models": view.AddedModels,
		"updated_models":         view.UpdatedModels,
		"created_logical_models": view.CreatedLogicalModels, "revision": view.Revision,
		"added_model_ids":             result.AddedModelIDs,
		"updated_model_ids":           result.UpdatedModelIDs,
		"created_logical_model_ids":   result.CreatedLogicalModelIDs,
		"discovered_model_count":      view.DiscoveredModels,
		"added_model_count":           view.AddedModels,
		"updated_model_count":         view.UpdatedModels,
		"created_logical_model_count": view.CreatedLogicalModels,
	}
	if result.CodexCatalog != nil {
		response["codex_catalog"] = result.CodexCatalog
	}
	writeMgmtJSON(w, result.HTTPStatus, response)
}

// StartProviderModelSync runs due catalog reads immediately after startup and
// then checks the durable schedule frequently enough to honor both supported
// intervals. It is safe to call more than once.
func (s *Server) StartProviderModelSync(ctx context.Context) {
	s.modelSyncStart.Do(func() {
		go func() {
			s.syncDueProviderModels(ctx, s.modelSyncNow())
			ticker := time.NewTicker(providerModelSyncPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.syncDueProviderModels(ctx, s.modelSyncNow())
				}
			}
		}()
	})
}

// scheduledModelSyncSettingKey is the control-plane runtime setting that
// globally gates the durable provider model sync schedule. It defaults to
// enabled; per-provider intervals keep their own finer-grained controls.
const scheduledModelSyncSettingKey = "scheduled_model_sync_enabled"

func (s *Server) scheduledModelSyncEnabled() bool {
	if s.deps.Control == nil {
		return true
	}
	value, found, err := s.deps.Control.RuntimeSetting(scheduledModelSyncSettingKey)
	if err != nil || !found {
		return true
	}
	return value != "false"
}

func (s *Server) setScheduledModelSyncEnabled(enabled bool) error {
	if s.deps.Control == nil {
		return fmt.Errorf("control store is not configured")
	}
	value := "true"
	if !enabled {
		value = "false"
	}
	return s.deps.Control.SetRuntimeSetting(scheduledModelSyncSettingKey, value)
}

func (s *Server) syncDueProviderModels(ctx context.Context, now time.Time) {
	if s.deps.Control == nil || s.currentConfig() == nil {
		return
	}
	// The Settings switch pauses the schedule only; manual Sync now runs stay
	// available, mirroring the per-provider interval semantics.
	if !s.scheduledModelSyncEnabled() {
		return
	}
	states, err := s.deps.Control.ProviderModelSyncStates()
	if err != nil {
		log.Printf("provider model synchronization state unavailable")
		return
	}
	file := s.currentConfig()
	if file.V2DomainSource != config.V2DomainSourceNative {
		return
	}
	for index := range file.Providers {
		provider := &file.Providers[index]
		if provider.ModelSyncInterval.Effective() == domain.ProviderModelSyncOff {
			continue
		}
		fingerprint := providerModelSyncFingerprint(file, provider.ID)
		state, matching := states[provider.ID]
		if matching {
			matching = state.ConfigFingerprint == fingerprint
		}
		due := !matching || state.NextRunAt == nil || !state.NextRunAt.After(now)
		if matching && state.Status == "disabled" {
			due = false
		}
		if !due {
			continue
		}
		providerID := provider.ID
		go func() {
			result := s.syncProviderModels(ctx, providerID)
			if result.State.Status == "failed" {
				log.Printf("provider %s model synchronization failed: %s", providerID, result.State.LastErrorCode)
			}
		}()
	}
}
