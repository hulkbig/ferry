package management

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/ledger"
	sched "ferry/internal/scheduler"
)

type v2CredentialSpec struct {
	SecretRef  string `json:"secret_ref"`
	Header     string `json:"header,omitempty"`
	QueryParam string `json:"query_param,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
}

// v2ConnectionModelSpec is the persisted, upstream-native model contract used
// by create, patch, and draft round-trips. Runtime views use a separate DTO so
// Ferry's effective compatibility layer is never written back as probe or
// operator evidence.
type v2ConnectionModelSpec struct {
	UpstreamModel string              `json:"upstream_model"`
	Protocols     []domain.Protocol   `json:"protocols"`
	Capabilities  domain.Capabilities `json:"capabilities"`
	PlanID        string              `json:"plan_id,omitempty"`
}

type v2ConnectionSpec struct {
	ID         string                  `json:"id"`
	ProviderID string                  `json:"provider_id"`
	Identity   string                  `json:"identity,omitempty"`
	BaseURL    string                  `json:"base_url,omitempty"`
	FullURL    string                  `json:"full_url,omitempty"`
	Credential v2CredentialSpec        `json:"credential"`
	Enabled    bool                    `json:"enabled"`
	Models     []v2ConnectionModelSpec `json:"models"`
	// Concurrency is the account's admission contract: how many attempts may
	// run at once, and how a throttled upstream is absorbed.
	Concurrency *domain.ConcurrencyPolicy `json:"concurrency,omitempty"`
}

// v2ProviderConnectionPatch lets the Codex provider editor atomically update
// the routing gate and model catalog of an existing OAuth account without
// replacing its identity, credential, or account-specific admission policy.
type v2ProviderConnectionPatch struct {
	ID      string                  `json:"id"`
	Enabled *bool                   `json:"enabled,omitempty"`
	Models  []v2ConnectionModelSpec `json:"models"`
}

// cloneConcurrencyPolicy detaches the admission settings so a management
// round-trip never aliases the live configuration.
func cloneConcurrencyPolicy(policy *domain.ConcurrencyPolicy) *domain.ConcurrencyPolicy {
	if policy == nil {
		return nil
	}
	out := *policy
	if policy.QueueOnThrottle != nil {
		queue := *policy.QueueOnThrottle
		out.QueueOnThrottle = &queue
	}
	if policy.QueueWhenFull != nil {
		queue := *policy.QueueWhenFull
		out.QueueWhenFull = &queue
	}
	return &out
}

func cloneProviderScheduledMessage(message *domain.ProviderScheduledMessage) *domain.ProviderScheduledMessage {
	if message == nil {
		return nil
	}
	out := *message
	return &out
}

func cloneProviderModelSyncCatalog(catalog *domain.ProviderModelSyncCatalog) *domain.ProviderModelSyncCatalog {
	if catalog == nil {
		return nil
	}
	out := *catalog
	return &out
}

func validateProviderModelSyncCatalog(providerType domain.ProviderType, catalog *domain.ProviderModelSyncCatalog) error {
	if catalog == nil {
		return nil
	}
	if providerType != domain.ProviderCustom {
		return fmt.Errorf("model_sync_catalog is supported only for custom providers")
	}
	if !catalog.Type.Valid() {
		return fmt.Errorf("unsupported model_sync_catalog type %q", catalog.Type)
	}
	if !catalog.Plan.Valid() {
		return fmt.Errorf("model_sync_catalog plan must be agent or coding")
	}
	if _, err := storeNameForRef(catalog.CredentialRef); err != nil || !strings.HasPrefix(catalog.CredentialRef, "store:") {
		return fmt.Errorf("model_sync_catalog credential_ref must be a valid store: reference")
	}
	return nil
}

func validateProviderModelSyncCatalogConnections(providerID string, catalog *domain.ProviderModelSyncCatalog, connections []domain.Connection) error {
	if catalog == nil {
		return nil
	}
	catalogName := secretNameForModelSync(catalog.CredentialRef)
	for _, connection := range connections {
		if connection.ProviderID == providerID && secretNameForModelSync(connection.Credential.SecretRef) == catalogName {
			return fmt.Errorf("model_sync_catalog credential_ref must be separate from connection %s", connection.ID)
		}
	}
	return nil
}

func optionalProviderModelSyncCatalog(raw json.RawMessage) (*domain.ProviderModelSyncCatalog, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var catalog domain.ProviderModelSyncCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, true, fmt.Errorf("decode model_sync_catalog: %w", err)
	}
	return cloneProviderModelSyncCatalog(&catalog), true, nil
}

// optionalProviderScheduledMessage distinguishes an omitted patch from an
// explicit null, which clears the Provider-scoped background message.
func optionalProviderScheduledMessage(raw json.RawMessage) (*domain.ProviderScheduledMessage, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var message domain.ProviderScheduledMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, true, fmt.Errorf("decode scheduled_message: %w", err)
	}
	if len(message.Message) > domain.ProviderScheduledMessageMaxBytes {
		return nil, true, fmt.Errorf("scheduled_message message exceeds %d bytes", domain.ProviderScheduledMessageMaxBytes)
	}
	return cloneProviderScheduledMessage(&message), true, nil
}

func optionalConcurrencyPolicy(raw json.RawMessage) (*domain.ConcurrencyPolicy, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var policy domain.ConcurrencyPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, true, fmt.Errorf("decode concurrency: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return nil, true, err
	}
	return cloneConcurrencyPolicy(&policy), true, nil
}

func v2ConnectionSpecFromDomain(connection domain.Connection) v2ConnectionSpec {
	out := v2ConnectionSpec{
		ID: connection.ID, ProviderID: connection.ProviderID, Identity: connection.Identity,
		BaseURL: connection.BaseURL, FullURL: connection.FullURL, Enabled: connection.Enabled,
		Credential: v2CredentialSpec{
			SecretRef: connection.Credential.SecretRef, Header: connection.Credential.Header,
			QueryParam: connection.Credential.QueryParam, Prefix: connection.Credential.Prefix,
		},
		Concurrency: cloneConcurrencyPolicy(connection.Concurrency),
	}
	for _, model := range connection.Models {
		out.Models = append(out.Models, v2ConnectionModelSpec{
			UpstreamModel: model.UpstreamModel, Protocols: append([]domain.Protocol(nil), model.Protocols...),
			Capabilities: model.Capabilities, PlanID: model.PlanID,
		})
	}
	if out.Models == nil {
		out.Models = []v2ConnectionModelSpec{}
	}
	return out
}

func (connection v2ConnectionSpec) domain() domain.Connection {
	out := domain.Connection{
		ID: connection.ID, ProviderID: connection.ProviderID, Identity: connection.Identity,
		BaseURL: connection.BaseURL, FullURL: connection.FullURL, Enabled: connection.Enabled,
		Credential: domain.CredentialRef{
			SecretRef: connection.Credential.SecretRef, Header: connection.Credential.Header,
			QueryParam: connection.Credential.QueryParam, Prefix: connection.Credential.Prefix,
		},
		Concurrency: cloneConcurrencyPolicy(connection.Concurrency),
	}
	for _, model := range connection.Models {
		out.Models = append(out.Models, domain.ConnectionModel{
			UpstreamModel: model.UpstreamModel, Protocols: append([]domain.Protocol(nil), model.Protocols...),
			Capabilities: model.Capabilities, PlanID: model.PlanID,
		})
	}
	return out
}

func applyProviderModelCapabilities(providerType domain.ProviderType, models []v2ConnectionModelSpec) {
	for index := range models {
		if providerType == domain.ProviderCliproxyCodex {
			models[index].Protocols = []domain.Protocol{domain.CodexModelNativeProtocol(models[index].UpstreamModel)}
		}
		models[index].Capabilities = domain.NormalizeProviderModelCapabilities(
			providerType, models[index].UpstreamModel, models[index].Protocols, models[index].Capabilities)
	}
}

type v2IntentPayload struct {
	CodexAutoUpdate     *bool                  `json:"codex_auto_update,omitempty"`
	CodexReleaseChannel *string                `json:"codex_release_channel,omitempty"`
	CodexClientVersion  *string                `json:"codex_client_version,omitempty"`
	LogicalModels       *[]domain.LogicalModel `json:"logical_models,omitempty"`
	Providers           *[]domain.Provider     `json:"providers,omitempty"`
	Connections         *[]v2ConnectionSpec    `json:"connections,omitempty"`
}

// Keep the disabled value explicit on management readback while persisted
// LogicalModel JSON remains compatible with configurations that omit it.
type v2LogicalModelIntent struct {
	domain.LogicalModel
	FirstOutputTimeoutSeconds int `json:"first_output_timeout_seconds"`
}

func v2LogicalModelIntentOf(model domain.LogicalModel) v2LogicalModelIntent {
	return v2LogicalModelIntent{LogicalModel: model, FirstOutputTimeoutSeconds: model.FirstOutputTimeoutSeconds}
}

type v2IntentView struct {
	CodexAutoUpdate     bool                        `json:"codex_auto_update"`
	CodexReleaseChannel string                      `json:"codex_release_channel"`
	CodexClientVersion  string                      `json:"codex_client_version"`
	LogicalModels       []v2LogicalModelIntent      `json:"logical_models"`
	Providers           []domain.Provider           `json:"providers"`
	Connections         []v2ConnectionSpec          `json:"connections"`
	MigrationIssues     []domain.LegacyMappingIssue `json:"migration_issues,omitempty"`
}

func v2IntentOf(file *config.File) v2IntentView {
	view := v2IntentView{}
	if file == nil {
		view.LogicalModels = []v2LogicalModelIntent{}
		view.Providers = []domain.Provider{}
		view.Connections = []v2ConnectionSpec{}
		return view
	}
	view.CodexClientVersion = file.CodexClientVersion
	view.CodexAutoUpdate = file.CodexAutoUpdate
	view.CodexReleaseChannel = file.EffectiveCodexReleaseChannel()
	for _, model := range file.LogicalModels {
		view.LogicalModels = append(view.LogicalModels, v2LogicalModelIntentOf(model))
	}
	view.Providers = append([]domain.Provider(nil), file.Providers...)
	for _, connection := range file.Connections {
		view.Connections = append(view.Connections, v2ConnectionSpecFromDomain(connection))
	}
	view.MigrationIssues = append([]domain.LegacyMappingIssue(nil), file.V2MigrationIssues...)
	if view.LogicalModels == nil {
		view.LogicalModels = []v2LogicalModelIntent{}
	}
	if view.Providers == nil {
		view.Providers = []domain.Provider{}
	}
	if view.Connections == nil {
		view.Connections = []v2ConnectionSpec{}
	}
	return view
}

func applyV2IntentPayload(file *config.File, payload v2IntentPayload) {
	previousVersion := file.EffectiveCodexClientVersion()
	wasManual := file.CodexClientVersion != ""
	if payload.CodexAutoUpdate != nil {
		file.CodexAutoUpdate = *payload.CodexAutoUpdate
	}
	if payload.CodexReleaseChannel != nil {
		file.CodexReleaseChannel = *payload.CodexReleaseChannel
	}
	if payload.CodexClientVersion != nil {
		file.CodexClientVersion = *payload.CodexClientVersion
	}
	// Handing a manual pin back to automation must retain the working version
	// while the first GitHub check is pending. Never downgrade on this transition.
	if wasManual && file.CodexClientVersion == "" && file.CodexAutoUpdate &&
		domain.ValidateCodexClientVersion(previousVersion) == nil &&
		domain.ValidateCodexClientVersion(file.EffectiveCodexClientVersion()) == nil &&
		compareCodexVersions(previousVersion, file.EffectiveCodexClientVersion()) > 0 {
		file.CodexAutoVersion = previousVersion
	}
	if payload.LogicalModels != nil {
		file.LogicalModels = append([]domain.LogicalModel(nil), (*payload.LogicalModels)...)
	}
	if payload.Providers != nil {
		file.Providers = append([]domain.Provider(nil), (*payload.Providers)...)
	}
	if payload.Connections != nil {
		file.Connections = nil
		for _, item := range *payload.Connections {
			file.Connections = append(file.Connections, item.domain())
		}
	}
	file.V2DomainSource = config.V2DomainSourceNative
	file.V2MigrationIssues = nil
}

func v2DraftView(draft *controlstore.ConfigurationDraft) map[string]any {
	return map[string]any{
		"id": draft.ID, "base_revision": draft.BaseRevision, "status": draft.Status,
		"restore_from_revision": draft.RestoreFromRevision, "applied_revision": draft.AppliedRevision,
		"created_at": draft.CreatedAt, "updated_at": draft.UpdatedAt,
		"intent": v2IntentOf(draft.Intent),
	}
}

func (s *Server) requireV2Control(w http.ResponseWriter) bool {
	if s.deps.Control == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "V2 control store is not configured")
		return false
	}
	return true
}

func (s *Server) editableV2Draft(r *http.Request) (*controlstore.ConfigurationDraft, error) {
	if id := strings.TrimSpace(r.URL.Query().Get("draft_id")); id != "" {
		draft, err := s.deps.Control.Draft(id)
		if err != nil {
			return nil, err
		}
		if draft.Status != "OPEN" {
			return nil, controlstore.ErrDraftApplied
		}
		return draft, nil
	}
	return s.deps.Control.CreateDraft(0, nil, 0)
}

func (s *Server) updateV2Draft(draft *controlstore.ConfigurationDraft, mutate func(*config.File) error) (*controlstore.ConfigurationDraft, error) {
	intent, err := cloneConfig(draft.Intent)
	if err != nil {
		return nil, err
	}
	if issue := blockingV2MigrationIssue(intent); issue != nil {
		return nil, fmt.Errorf("legacy-to-V2 migration is blocked by %s %q: %s",
			issue.Kind, issue.ObjectID, issue.Message)
	}
	intent.V2DomainSource = config.V2DomainSourceNative
	intent.V2MigrationIssues = nil
	if err := mutate(intent); err != nil {
		return nil, err
	}
	if err := s.enrichCodexDraftCapabilities(draft.Intent, intent); err != nil {
		return nil, err
	}
	// Preserve ownership through full DTO replacements. Removed/recreated IDs
	// receive a fresh incarnation because they are absent from this draft.
	for i := range intent.Connections {
		c := &intent.Connections[i]
		c.RuntimeID = ""
		for _, old := range draft.Intent.Connections {
			if old.ID == c.ID && old.ProviderID == c.ProviderID {
				c.RuntimeID = old.RuntimeID
				break
			}
		}
		if c.RuntimeID == "" {
			c.RuntimeID = "conn_" + rand.Text()
		}
	}
	return s.deps.Control.UpdateDraft(draft.ID, intent)
}

func blockingV2MigrationIssue(intent *config.File) *domain.LegacyMappingIssue {
	if intent == nil || intent.V2DomainSource == config.V2DomainSourceNative {
		return nil
	}
	for index := range intent.V2MigrationIssues {
		if intent.V2MigrationIssues[index].Blocking {
			return &intent.V2MigrationIssues[index]
		}
	}
	return nil
}

func (s *Server) handleV2DraftCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var body struct {
		BaseRevision int64            `json:"base_revision"`
		Intent       *v2IntentPayload `json:"intent,omitempty"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var intent *config.File
	if body.Intent != nil {
		var err error
		base := body.BaseRevision
		if base <= 0 {
			_, base, err = s.deps.Control.ActiveConfig()
			if err != nil {
				writeMgmtErr(w, http.StatusServiceUnavailable, err.Error())
				return
			}
		}
		intent, err = s.deps.Control.ConfigRevision(base)
		if err != nil {
			writeMgmtErr(w, http.StatusNotFound, err.Error())
			return
		}
		if issue := blockingV2MigrationIssue(intent); issue != nil {
			writeMgmtErr(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"legacy-to-V2 migration is blocked by %s %q: %s", issue.Kind, issue.ObjectID, issue.Message))
			return
		}
		before, err := cloneConfig(intent)
		if err != nil {
			writeMgmtErr(w, http.StatusInternalServerError, "cannot prepare draft")
			return
		}
		applyV2IntentPayload(intent, *body.Intent)
		if err := s.enrichCodexDraftCapabilities(before, intent); err != nil {
			writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	draft, err := s.deps.Control.CreateDraft(body.BaseRevision, intent, 0)
	if err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusCreated, v2DraftView(draft))
}

func (s *Server) handleV2DraftDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	draft, err := s.deps.Control.Draft(r.PathValue("id"))
	if err != nil {
		writeDraftError(w, err)
		return
	}
	writeMgmtJSON(w, http.StatusOK, v2DraftView(draft))
}

func (s *Server) handleV2DraftPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var payload v2IntentPayload
	if err := decodeMgmtBody(r, &payload); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	draft, err := s.deps.Control.Draft(r.PathValue("id"))
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error { applyV2IntentPayload(intent, payload); return nil })
	if err != nil {
		writeDraftError(w, err)
		return
	}
	writeMgmtJSON(w, http.StatusOK, v2DraftView(updated))
}

func (s *Server) handleV2DraftDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	if err := s.deps.Control.DeleteDraft(r.PathValue("id")); err != nil {
		writeDraftError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeDraftError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, controlstore.ErrDraftNotFound):
		writeMgmtErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, controlstore.ErrDraftApplied):
		writeMgmtErr(w, http.StatusConflict, err.Error())
	default:
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
	}
}

func (s *Server) validateV2Draft(draft *controlstore.ConfigurationDraft) (*config.File, []string) {
	candidate, err := cloneConfig(draft.Intent)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var problems []string
	if err := candidate.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 && s.deps.Compiler != nil {
		if _, err := s.deps.Compiler.Compile(candidate); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return candidate, problems
}

func (s *Server) handleV2DraftValidate(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	draft, err := s.deps.Control.Draft(r.PathValue("id"))
	if err != nil {
		writeDraftError(w, err)
		return
	}
	candidate, problems := s.validateV2Draft(draft)
	impact := s.v2DraftImpact(candidate)
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"draft_id": draft.ID, "valid": len(problems) == 0, "errors": problems,
		"impact": impact,
	})
}

func (s *Server) v2DraftImpact(candidate *config.File) map[string]any {
	active := s.currentConfig()
	activeModels, candidateModels := map[string]bool{}, map[string]bool{}
	activeProviders, candidateProviders := map[string]bool{}, map[string]bool{}
	for _, item := range active.LogicalModels {
		activeModels[item.ID] = true
	}
	for _, item := range active.Providers {
		activeProviders[item.ID] = true
	}
	if candidate != nil {
		for _, item := range candidate.LogicalModels {
			candidateModels[item.ID] = true
		}
		for _, item := range candidate.Providers {
			candidateProviders[item.ID] = true
		}
	}
	removedModels, removedProviders := []string{}, []string{}
	for id := range activeModels {
		if !candidateModels[id] {
			removedModels = append(removedModels, id)
		}
	}
	for id := range activeProviders {
		if !candidateProviders[id] {
			removedProviders = append(removedProviders, id)
		}
	}
	sort.Strings(removedModels)
	sort.Strings(removedProviders)
	return map[string]any{
		"removed_models": removedModels, "removed_providers": removedProviders,
		"active_conversations": map[string]any{
			"behavior": "keep current binding while it remains in-route, eligible, and capability-compatible; otherwise migrate on next request",
		},
	}
}

func (s *Server) handleV2DraftSimulate(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	draft, err := s.deps.Control.Draft(r.PathValue("id"))
	if err != nil {
		writeDraftError(w, err)
		return
	}
	candidate, problems := s.validateV2Draft(draft)
	if len(problems) > 0 {
		writeMgmtJSON(w, http.StatusOK, map[string]any{"valid": false, "errors": problems})
		return
	}
	snapshot, err := s.deps.Compiler.Compile(candidate)
	if err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var body struct {
		CallerID string              `json:"caller_id"`
		Protocol domain.Protocol     `json:"protocol"`
		Stream   bool                `json:"stream"`
		Required domain.Requirements `json:"required_capabilities"`
		At       string              `json:"at"`
	}
	if r.ContentLength != 0 {
		if err := decodeMgmtBody(r, &body); err != nil {
			writeMgmtErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Protocol == "" {
		body.Protocol = domain.ProtocolOpenAIChat
	}
	if err := validateV2SimulationInput(body.Protocol, body.Required); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	effectiveAt := time.Now()
	if strings.TrimSpace(body.At) != "" {
		parsed, err := time.Parse(time.RFC3339, body.At)
		if err != nil {
			writeMgmtErr(w, http.StatusBadRequest, "at must be RFC3339")
			return
		}
		effectiveAt = parsed
	}
	simulatedAt := &effectiveAt
	if body.CallerID == "" && s.deps.APIKeys != nil {
		identities, _, _ := s.deps.APIKeys.APIKeys()
		for _, identity := range identities {
			if identity.Enabled {
				body.CallerID = identity.ID
				break
			}
		}
	}
	modelID := r.URL.Query().Get("model")
	models := []map[string]any{}
	for _, model := range candidate.LogicalModels {
		if modelID != "" && model.ID != modelID {
			continue
		}
		contract := &domain.RequestContract{
			Model: model.ID, Protocol: body.Protocol, Origin: body.Protocol, Stream: body.Stream,
			Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "simulation"}}}},
		}
		if body.Required.Tools || body.Required.ParallelTools {
			contract.Tools = []domain.ToolDef{{Name: "simulation", Parameters: json.RawMessage(`{}`)}}
		}
		if body.Required.ParallelTools {
			parallel := true
			contract.ParallelTools = &parallel
		}
		if body.Required.Reasoning {
			contract.Reasoning = &domain.ReasoningSpec{Effort: "medium"}
		}
		if body.Required.StructuredOutput {
			contract.ResponseFormat = &domain.ResponseFormat{Name: "simulation", Schema: []byte(`{"type":"object"}`)}
		}
		simulation, planningFailure := s.deps.Scheduler.DryRunV2Snapshot(snapshot, contract, sched.RequestMeta{
			CallerID: body.CallerID, RequiredCapabilities: &body.Required, SimulatedAt: simulatedAt,
		})
		if simulation == nil {
			models = append(models, map[string]any{
				"logical_model_id": model.ID,
				"match":            map[string]any{"caller_id": body.CallerID},
				"transform":        map[string]any{"required_capabilities": body.Required},
				"effective_route":  model.DefaultRoute,
				"eligibility":      []any{}, "expected_selection": nil,
				"planning_failure": planningFailure,
			})
			continue
		}
		eligibility := make([]map[string]any, 0, len(simulation.Plan.Candidates))
		for _, planned := range simulation.Plan.Candidates {
			item := map[string]any{
				"eligible": planned.Eligible, "rank": planned.Rank,
				"exclusion_reason": planned.ExclusionReason,
			}
			if target := simulation.Snapshot.Targets[planned.Target]; target != nil {
				item["provider_id"] = target.ProviderID
				item["connection_id"] = target.ConnectionID
				item["upstream_model"] = target.Model
				egress := target.EgressProtocol(body.Protocol)
				item["upstream_capabilities"] = target.UpstreamCapabilitiesFor(egress)
				item["capabilities"] = target.CapabilitiesFor(egress)
			}
			eligibility = append(eligibility, item)
		}
		var expected any
		if target := simulation.Snapshot.Targets[simulation.Plan.Primary]; target != nil {
			expected = map[string]any{"provider_id": target.ProviderID, "connection_id": target.ConnectionID, "upstream_model": target.Model}
		}
		transform := map[string]any{"mode": "DEFAULT", "required_capabilities": body.Required}
		for _, override := range model.Overrides {
			if override.ID == simulation.OverrideID {
				transform = map[string]any{
					"mode": override.Mode, "prefer": override.Prefer, "avoid": override.Avoid,
					"required_capabilities": body.Required,
				}
				break
			}
		}
		models = append(models, map[string]any{
			"logical_model_id": simulation.LogicalModelID,
			"match":            map[string]any{"caller_id": body.CallerID, "override_id": simulation.OverrideID},
			"transform":        transform, "effective_route": simulation.EffectiveRoute,
			"eligibility": eligibility, "expected_selection": expected,
			"planning_failure": planningFailure,
		})
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"valid": true, "models": models, "impact": s.v2DraftImpact(candidate), "simulated_at": effectiveAt,
	})
}

func (s *Server) handleV2DraftApply(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.applyV2Draft(w, r.PathValue("id"), body.ExpectedRevision, managementIdentityFrom(r).Actor())
}

func (s *Server) applyV2Draft(w http.ResponseWriter, draftID string, expectedRevision int64, actor string) {
	draft, err := s.deps.Control.Draft(draftID)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	if expectedRevision <= 0 {
		writeMgmtJSON(w, http.StatusPreconditionRequired, map[string]any{
			"error": map[string]any{
				"code": "MISSING_PRECONDITION", "message": "expected_revision is required",
				"current_revision": draft.BaseRevision, "retryable": true,
			},
		})
		return
	}
	if expectedRevision != draft.BaseRevision {
		writeMgmtJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code": "REVISION_CONFLICT", "message": "expected_revision must equal the review base revision",
				"current_revision": draft.BaseRevision, "retryable": true,
			},
		})
		return
	}
	if draft.Status == "APPLIED" {
		writeMgmtJSON(w, http.StatusOK, map[string]any{
			"applied": true, "idempotent": true, "revision": draft.AppliedRevision, "draft_id": draft.ID,
		})
		return
	}
	candidate, problems := s.validateV2Draft(draft)
	if len(problems) > 0 {
		writeMgmtJSON(w, http.StatusUnprocessableEntity, map[string]any{"applied": false, "errors": problems})
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	current, err := s.controlVersionLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if current != draft.BaseRevision {
		// A successful publish followed by a metadata-write interruption is
		// reconciled against the only revision this draft could have created.
		publishedRevision := draft.BaseRevision + 1
		if current >= publishedRevision {
			published, lookupErr := s.deps.Control.ConfigRevision(publishedRevision)
			if lookupErr == nil && sameV2Intent(published, candidate) {
				if markErr := s.deps.Control.MarkDraftApplied(draft.ID, publishedRevision); markErr == nil {
					s.recordV2RevisionAudit(draft, publishedRevision, actor, candidate)
					writeMgmtJSON(w, http.StatusOK, map[string]any{
						"applied": true, "idempotent": true, "revision": publishedRevision, "draft_id": draft.ID,
					})
					return
				}
			}
		}
		writeMgmtJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code": "REVISION_CONFLICT", "message": "Routing changed since this review was created.",
				"current_revision": current, "retryable": true,
			},
		})
		return
	}
	if err := s.publishConfig(candidate); err != nil {
		s.emitFailureAlert("revision_apply_failed", "revision", draft.ID, "Revision apply failed", err.Error())
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// The new immutable snapshot is active now. Remove stale affinity entries
	// for resources deleted by this revision; in-flight requests still retain
	// the previous snapshot and finish normally.
	if s.deps.Scheduler != nil {
		s.deps.Scheduler.PruneAffinityBindings()
	}
	revision, err := s.controlVersionLocked()
	if err != nil {
		// Publish already succeeded. Never misreport it as a failed apply; a
		// retry reconciles the OPEN draft against base_revision+1 above.
		writeMgmtJSON(w, http.StatusAccepted, map[string]any{
			"applied": true, "draft_id": draft.ID, "metadata_reconciliation_pending": true,
		})
		return
	}
	if err := s.deps.Control.MarkDraftApplied(draft.ID, revision); err != nil {
		writeMgmtJSON(w, http.StatusAccepted, map[string]any{
			"applied": true, "revision": revision, "draft_id": draft.ID,
			"metadata_reconciliation_pending": true,
		})
		return
	}
	s.recordV2RevisionAudit(draft, revision, actor, candidate)
	writeMgmtJSON(w, http.StatusOK, map[string]any{"applied": true, "revision": revision, "draft_id": draft.ID})
}

func (s *Server) recordV2RevisionAudit(draft *controlstore.ConfigurationDraft, revision int64, actor string, candidate *config.File) {
	before, _ := s.deps.Control.ConfigRevision(draft.BaseRevision)
	diff := summarizeV2IntentDiff(before, candidate)
	source := "console"
	summary := diff.Summary
	if draft.RestoreFromRevision > 0 {
		source = "restore"
		summary = fmt.Sprintf("Restored configuration from revision %d", draft.RestoreFromRevision)
	}
	_ = s.deps.Control.RecordRevisionAudit(revision, actor, source, summary, diff.Resources)
}

type v2RoutingReviewInput struct {
	FirstOutputTimeoutSeconds *int                                `json:"first_output_timeout_seconds,omitempty"`
	Routes                    []domain.RouteStep                  `json:"routes"`
	RecoveryEnabled           *bool                               `json:"recovery_enabled,omitempty"` // Deprecated compatibility flag; recover_on controls configurable upstream failures.
	ConversationStrategy      domain.ExistingConversationStrategy `json:"conversation_strategy,omitempty"`
}

func defaultV2RecoveryFailures() []domain.FailureType {
	return []domain.FailureType{
		domain.FailConnect, domain.FailRateLimit, domain.FailProvider5xx,
		domain.FailQuotaExhausted, domain.FailTTFTTimeout, domain.FailStreamBroken,
	}
}

func cloneV2RecoveryFailures(failures []domain.FailureType) []domain.FailureType {
	if failures == nil {
		return nil
	}
	return append(make([]domain.FailureType, 0, len(failures)), failures...)
}

func (s *Server) handleV2ModelRoutingReview(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var input v2RoutingReviewInput
	if err := decodeMgmtBody(r, &input); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Routes) == 0 {
		writeMgmtErr(w, http.StatusBadRequest, "at least one provider route is required")
		return
	}
	if input.ConversationStrategy == "" {
		input.ConversationStrategy = domain.ConversationKeepExisting
	}
	if !input.ConversationStrategy.Valid() {
		writeMgmtErr(w, http.StatusUnprocessableEntity, "unsupported existing conversation strategy")
		return
	}
	draft, err := s.deps.Control.CreateDraft(0, nil, 0)
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	modelID := r.PathValue("id")
	var before domain.RouteSpec
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		index := logicalModelIndex(intent, modelID)
		if index < 0 {
			return fmt.Errorf("logical model not found")
		}
		model := &intent.LogicalModels[index]
		model.AutoGenerated = false
		before = cloneRouteSpec(model.DefaultRoute)
		if input.FirstOutputTimeoutSeconds != nil {
			model.FirstOutputTimeoutSeconds = *input.FirstOutputTimeoutSeconds
		}
		model.ExistingConversationStrategy = input.ConversationStrategy
		model.DefaultRoute = domain.RouteSpec{Steps: append([]domain.RouteStep(nil), input.Routes...)}
		for index := range model.DefaultRoute.Steps {
			step := &model.DefaultRoute.Steps[index]
			// Keep nil (field omitted) distinct from a non-nil empty slice until
			// compatibility defaults have been applied. append(nil, empty...)
			// collapses both representations and would turn an explicit [] into
			// the legacy default progression set.
			step.RecoverOn = cloneV2RecoveryFailures(step.RecoverOn)
			validRecoveryTypes := true
			for _, failure := range step.RecoverOn {
				if !failure.Valid() {
					validRecoveryTypes = false
					break
				}
			}
			// The final step has nowhere to advance. Valid but meaningless legacy
			// values are normalized away; invalid values stay visible to validation.
			if index == len(model.DefaultRoute.Steps)-1 && validRecoveryTypes {
				step.RecoverOn = nil
				continue
			}
			if input.RecoveryEnabled != nil && !*input.RecoveryEnabled {
				step.RecoverOn = nil
				continue
			}
			// Omitted recover_on preserves the safe standard progression set for
			// backwards-compatible clients. An explicit list, including [], is
			// authoritative for operational upstream failures and is validated
			// without being overwritten. Pre-commit protocol-fidelity failures
			// still advance automatically because they are compatibility facts,
			// not an upstream retry policy.
			if index < len(model.DefaultRoute.Steps)-1 && step.RecoverOn == nil {
				step.RecoverOn = defaultV2RecoveryFailures()
			}
		}
		return nil
	})
	if err != nil {
		_ = s.deps.Control.DeleteDraft(draft.ID)
		status := http.StatusUnprocessableEntity
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	candidate, problems := s.validateV2Draft(updated)
	if len(problems) > 0 {
		_ = s.deps.Control.DeleteDraft(updated.ID)
		message := "Routing configuration is invalid."
		if first := strings.TrimSpace(problems[0]); first != "" {
			message = "Routing configuration is invalid: " + first
		}
		writeMgmtJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{
				"code": "VALIDATION_FAILURE", "message": message,
				"details": problems, "retryable": true,
			},
		})
		return
	}
	after := domain.RouteSpec{}
	firstOutputTimeoutSeconds := 0
	if index := logicalModelIndex(candidate, modelID); index >= 0 {
		after = cloneRouteSpec(candidate.LogicalModels[index].DefaultRoute)
		firstOutputTimeoutSeconds = candidate.LogicalModels[index].FirstOutputTimeoutSeconds
	}
	from, to := time.Now().Add(-15*time.Minute), time.Now()
	traffic, _ := s.deps.Ledger.TrafficByLogicalModel(from, to)
	expectedTraffic := float64(0)
	for _, item := range traffic {
		if item.ID == modelID {
			expectedTraffic = float64(item.Requests) / 15
			break
		}
	}
	activeConversations := s.deps.Scheduler.AffinityStatsByModel(to)[modelID].Conversation
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"model_id": modelID, "draft_id": updated.ID,
		"expected_revision": updated.BaseRevision, "expected_next_revision": updated.BaseRevision + 1,
		"before": before.Steps, "after": after.Steps,
		"first_output_timeout_seconds": firstOutputTimeoutSeconds,
		"expected_traffic_per_minute":  expectedTraffic, "active_conversations": activeConversations,
		"conversation_strategy":           input.ConversationStrategy,
		"existing_conversations_behavior": input.ConversationStrategy,
		"new_conversations_behavior":      "use-new-revision",
		"warnings":                        []string{}, "valid": true,
	})
}

func cloneRouteSpec(route domain.RouteSpec) domain.RouteSpec {
	out := domain.RouteSpec{Steps: append([]domain.RouteStep(nil), route.Steps...)}
	for index := range out.Steps {
		out.Steps[index].RecoverOn = cloneV2RecoveryFailures(out.Steps[index].RecoverOn)
	}
	return out
}

func (s *Server) handleV2ModelRoutingApply(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var body struct {
		DraftID          string `json:"draft_id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.DraftID) == "" {
		writeMgmtErr(w, http.StatusBadRequest, "draft_id is required")
		return
	}
	s.applyV2Draft(w, body.DraftID, body.ExpectedRevision, managementIdentityFrom(r).Actor())
}

func sameV2Intent(left, right *config.File) bool {
	if left == nil || right == nil {
		return false
	}
	return reflect.DeepEqual(v2IntentOf(left), v2IntentOf(right))
}

func (s *Server) handleV2Revisions(w http.ResponseWriter, _ *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	revisions, err := s.deps.Control.ConfigRevisions()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"revisions": revisions})
}

func parseRevision(r *http.Request) (int64, error) {
	revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || revision <= 0 {
		return 0, fmt.Errorf("revision must be a positive integer")
	}
	return revision, nil
}

func (s *Server) handleV2RevisionDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	revision, err := parseRevision(r)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	intent, err := s.deps.Control.ConfigRevision(revision)
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	var before *config.File
	if revision > 1 {
		before, _ = s.deps.Control.ConfigRevision(revision - 1)
	}
	diff := summarizeV2IntentDiff(before, intent)
	var metadata any
	if revisions, lookupErr := s.deps.Control.ConfigRevisions(); lookupErr == nil {
		for index := range revisions {
			if revisions[index].Revision == revision {
				metadata = revisions[index]
				break
			}
		}
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"revision": revision, "metadata": metadata, "intent": v2IntentOf(intent),
		"previous_revision": revision - 1, "before": v2IntentOf(before), "diff": diff,
	})
}

type v2IntentDiff struct {
	Summary   string         `json:"summary"`
	Resources []string       `json:"resources"`
	Added     map[string]any `json:"added"`
	Changed   map[string]any `json:"changed"`
	Removed   map[string]any `json:"removed"`
}

func summarizeV2IntentDiff(before, after *config.File) v2IntentDiff {
	result := v2IntentDiff{Resources: []string{}, Added: map[string]any{}, Changed: map[string]any{}, Removed: map[string]any{}}
	compare := func(kind string, oldItems, newItems map[string]any) {
		added, changed, removed := []string{}, []string{}, []string{}
		for id, value := range newItems {
			old, exists := oldItems[id]
			if !exists {
				added = append(added, id)
			} else if !reflect.DeepEqual(old, value) {
				changed = append(changed, id)
			}
		}
		for id := range oldItems {
			if _, exists := newItems[id]; !exists {
				removed = append(removed, id)
			}
		}
		sort.Strings(added)
		sort.Strings(changed)
		sort.Strings(removed)
		if len(added) > 0 {
			result.Added[kind] = added
		}
		if len(changed) > 0 {
			result.Changed[kind] = changed
		}
		if len(removed) > 0 {
			result.Removed[kind] = removed
		}
		for _, id := range append(append(added, changed...), removed...) {
			result.Resources = append(result.Resources, kind+":"+id)
		}
	}
	collect := func(file *config.File) (map[string]any, map[string]any, map[string]any) {
		models, providers, connections := map[string]any{}, map[string]any{}, map[string]any{}
		if file != nil {
			for _, item := range file.LogicalModels {
				models[item.ID] = item
			}
			for _, item := range file.Providers {
				providers[item.ID] = item
			}
			for _, item := range file.Connections {
				connections[item.ID] = item
			}
		}
		return models, providers, connections
	}
	oldModels, oldProviders, oldConnections := collect(before)
	newModels, newProviders, newConnections := collect(after)
	compare("model", oldModels, newModels)
	compare("provider", oldProviders, newProviders)
	compare("connection", oldConnections, newConnections)
	versionSettings := func(file *config.File) map[string]any {
		if file == nil {
			file = &config.File{}
		}
		return map[string]any{
			"codex_client_version":  file.CodexClientVersion,
			"codex_auto_update":     file.CodexAutoUpdate,
			"codex_release_channel": file.EffectiveCodexReleaseChannel(),
			"codex_auto_version":    file.CodexAutoVersion,
		}
	}
	compare("settings", versionSettings(before), versionSettings(after))
	if len(result.Resources) == 0 {
		result.Summary = "Configuration metadata updated"
	} else {
		result.Summary = fmt.Sprintf("Updated %d configuration resource(s)", len(result.Resources))
	}
	return result
}

func (s *Server) handleV2RevisionRestoreDraft(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	revision, err := parseRevision(r)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	intent, err := s.deps.Control.ConfigRevision(revision)
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	_, current, err := s.deps.Control.ActiveConfig()
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	draft, err := s.deps.Control.CreateDraft(current, intent, revision)
	if err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusCreated, v2DraftView(draft))
}

func logicalModelIndex(file *config.File, id string) int {
	for i := range file.LogicalModels {
		if file.LogicalModels[i].ID == id {
			return i
		}
	}
	return -1
}

func providerIndex(file *config.File, id string) int {
	for i := range file.Providers {
		if file.Providers[i].ID == id {
			return i
		}
	}
	return -1
}

func connectionIndexV2(file *config.File, id string) int {
	for i := range file.Connections {
		if file.Connections[i].ID == id {
			return i
		}
	}
	return -1
}

func hasSameNameUpstreamModel(file *config.File, id string) bool {
	if file == nil {
		return false
	}
	for _, connection := range file.Connections {
		for _, model := range connection.Models {
			if model.UpstreamModel == id {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleV2ModelCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var model domain.LogicalModel
	if err := decodeMgmtBody(r, &model); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(model.ID) == "" {
		writeMgmtErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if model.FirstOutputTimeoutSeconds < 0 || model.FirstOutputTimeoutSeconds > 7200 {
		writeMgmtErr(w, http.StatusUnprocessableEntity, "first_output_timeout_seconds must be between 0 and 7200 (0 disables the timeout)")
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		if logicalModelIndex(intent, model.ID) >= 0 {
			return fmt.Errorf("logical model already exists")
		}
		intent.LogicalModels = append(intent.LogicalModels, model)
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "model": v2LogicalModelIntentOf(model)})
}

func (s *Server) handleV2ModelPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var patch struct {
		DisplayName               *string                `json:"display_name"`
		Description               *string                `json:"description"`
		Enabled                   *bool                  `json:"enabled"`
		ExactOnly                 *bool                  `json:"exact_only"`
		FirstOutputTimeoutSeconds *int                   `json:"first_output_timeout_seconds"`
		DefaultRoute              *domain.RouteSpec      `json:"default_route"`
		Overrides                 *[]domain.OverrideSpec `json:"overrides"`
	}
	if err := decodeMgmtBody(r, &patch); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if patch.FirstOutputTimeoutSeconds != nil && (*patch.FirstOutputTimeoutSeconds < 0 || *patch.FirstOutputTimeoutSeconds > 7200) {
		writeMgmtErr(w, http.StatusUnprocessableEntity, "first_output_timeout_seconds must be between 0 and 7200 (0 disables the timeout)")
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		index := logicalModelIndex(intent, r.PathValue("id"))
		if index < 0 {
			return fmt.Errorf("logical model not found")
		}
		model := &intent.LogicalModels[index]
		model.AutoGenerated = false
		if patch.DisplayName != nil {
			model.DisplayName = *patch.DisplayName
		}
		if patch.Description != nil {
			model.Description = *patch.Description
		}
		if patch.Enabled != nil {
			model.Enabled = *patch.Enabled
		}
		if patch.ExactOnly != nil {
			model.ExactOnly = *patch.ExactOnly
		}
		if patch.FirstOutputTimeoutSeconds != nil {
			model.FirstOutputTimeoutSeconds = *patch.FirstOutputTimeoutSeconds
		}
		if patch.DefaultRoute != nil {
			model.DefaultRoute = *patch.DefaultRoute
		}
		if patch.Overrides != nil {
			model.Overrides = append([]domain.OverrideSpec(nil), (*patch.Overrides)...)
		}
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "model_id": r.PathValue("id")})
}

func (s *Server) handleV2ModelDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	id := r.PathValue("id")
	cascade := r.URL.Query().Get("cascade") == "true"
	if hasSameNameUpstreamModel(s.currentConfig(), id) {
		writeMgmtErr(w, http.StatusConflict,
			"same-name logical model is required while upstream model "+id+" is configured; remove the upstream model first")
		return
	}
	stats := s.deps.Scheduler.AffinityStatsByModel(time.Now())[id]
	if !cascade && stats.Conversation > 0 {
		writeMgmtErr(w, http.StatusConflict, "disable the model and wait for active conversations before deleting it")
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		index := logicalModelIndex(intent, id)
		if index < 0 {
			return fmt.Errorf("logical model not found")
		}
		if !cascade && intent.LogicalModels[index].Enabled {
			return fmt.Errorf("logical model must be disabled before deletion")
		}
		intent.LogicalModels = append(intent.LogicalModels[:index], intent.LogicalModels[index+1:]...)
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{
		"draft_id": updated.ID, "deleted_model_id": id, "cascade": cascade,
		"affected_conversation_bindings": stats.Conversation,
	})
}

func overrideIndex(model *domain.LogicalModel, id string) int {
	for index := range model.Overrides {
		if model.Overrides[index].ID == id {
			return index
		}
	}
	return -1
}

func (s *Server) handleV2OverrideCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var override domain.OverrideSpec
	if err := decodeMgmtBody(r, &override); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(override.ID) == "" {
		writeMgmtErr(w, http.StatusBadRequest, "override id is required")
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		modelAt := logicalModelIndex(intent, r.PathValue("id"))
		if modelAt < 0 {
			return fmt.Errorf("logical model not found")
		}
		if overrideIndex(&intent.LogicalModels[modelAt], override.ID) >= 0 {
			return fmt.Errorf("override already exists")
		}
		intent.LogicalModels[modelAt].Overrides = append(intent.LogicalModels[modelAt].Overrides, override)
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "override": override})
}

func (s *Server) handleV2OverridePatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var replacement domain.OverrideSpec
	if err := decodeMgmtBody(r, &replacement); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	replacement.ID = r.PathValue("override_id")
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		modelAt := logicalModelIndex(intent, r.PathValue("id"))
		if modelAt < 0 {
			return fmt.Errorf("logical model not found")
		}
		overrideAt := overrideIndex(&intent.LogicalModels[modelAt], replacement.ID)
		if overrideAt < 0 {
			return fmt.Errorf("override not found")
		}
		intent.LogicalModels[modelAt].Overrides[overrideAt] = replacement
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "override": replacement})
}

func (s *Server) handleV2OverrideDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		modelAt := logicalModelIndex(intent, r.PathValue("id"))
		if modelAt < 0 {
			return fmt.Errorf("logical model not found")
		}
		overrideAt := overrideIndex(&intent.LogicalModels[modelAt], r.PathValue("override_id"))
		if overrideAt < 0 {
			return fmt.Errorf("override not found")
		}
		items := intent.LogicalModels[modelAt].Overrides
		intent.LogicalModels[modelAt].Overrides = append(items[:overrideAt], items[overrideAt+1:]...)
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "deleted_override_id": r.PathValue("override_id")})
}

func (s *Server) handleV2ProviderCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var input struct {
		ID                 string                           `json:"id"`
		Name               string                           `json:"name"`
		Type               domain.ProviderType              `json:"type"`
		BaseURL            string                           `json:"base_url,omitempty"`
		Enabled            bool                             `json:"enabled"`
		ModelSyncInterval  domain.ProviderModelSyncInterval `json:"model_sync_interval,omitempty"`
		ModelSyncCatalog   *domain.ProviderModelSyncCatalog `json:"model_sync_catalog,omitempty"`
		ProtocolConversion map[domain.Protocol]bool         `json:"protocol_conversion,omitempty"`
		ScheduledMessage   *domain.ProviderScheduledMessage `json:"scheduled_message,omitempty"`
		Concurrency        *domain.ConcurrencyPolicy        `json:"concurrency,omitempty"`
		Connection         *v2ConnectionSpec                `json:"connection,omitempty"`
		Connections        []v2ConnectionSpec               `json:"connections,omitempty"`
	}
	if err := decodeMgmtBody(r, &input); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !input.ModelSyncInterval.Valid() {
		writeMgmtErr(w, http.StatusBadRequest, "model_sync_interval must be one of off, 30m, or 1h")
		return
	}
	if err := validateProviderModelSyncCatalog(input.Type, input.ModelSyncCatalog); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	provider := domain.Provider{
		ID: input.ID, Name: input.Name, Type: input.Type, BaseURL: input.BaseURL,
		Enabled: input.Enabled, ModelSyncInterval: input.ModelSyncInterval.Effective(), ModelSyncCatalog: cloneProviderModelSyncCatalog(input.ModelSyncCatalog), ProtocolConversion: input.ProtocolConversion,
		ScheduledMessage: cloneProviderScheduledMessage(input.ScheduledMessage),
		Concurrency:      cloneConcurrencyPolicy(input.Concurrency),
	}
	if strings.TrimSpace(provider.ID) == "" {
		writeMgmtErr(w, http.StatusBadRequest, "id is required")
		return
	}
	connections := append([]v2ConnectionSpec(nil), input.Connections...)
	if input.Connection != nil {
		connections = append(connections, *input.Connection)
	}
	createdConnections := make([]domain.Connection, 0, len(connections))
	seenConnectionIDs := map[string]bool{}
	for index := range connections {
		spec := connections[index]
		spec.ProviderID = provider.ID
		applyProviderModelCapabilities(provider.Type, spec.Models)
		if strings.TrimSpace(spec.ID) == "" {
			if len(connections) == 1 {
				spec.ID = provider.ID
			} else {
				writeMgmtErr(w, http.StatusBadRequest, "connection id is required")
				return
			}
		}
		if seenConnectionIDs[spec.ID] {
			writeMgmtErr(w, http.StatusBadRequest, "connection ids must be unique")
			return
		}
		seenConnectionIDs[spec.ID] = true
		if strings.TrimSpace(spec.Identity) == "" {
			spec.Identity = provider.Name
		}
		createdConnections = append(createdConnections, spec.domain())
		connections[index] = spec
		provider.Connections = append(provider.Connections, domain.ConnectionRef{ID: spec.ID})
	}
	if err := validateProviderModelSyncCatalogConnections(provider.ID, provider.ModelSyncCatalog, createdConnections); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		if providerIndex(intent, provider.ID) >= 0 {
			return fmt.Errorf("provider already exists")
		}
		for _, connection := range createdConnections {
			if connectionIndexV2(intent, connection.ID) >= 0 {
				return fmt.Errorf("provider execution identity already exists")
			}
		}
		intent.Providers = append(intent.Providers, provider)
		intent.Connections = append(intent.Connections, createdConnections...)
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "provider": provider, "connections": connections})
}

// handleV2ProviderProtocolConfig replaces one API endpoint provider's
// protocol-scoped execution identities in a single draft. This is the
// management transaction used by the combined Base URL/Key/Models editor:
// there is no intermediate revision where a routed model temporarily loses
// its native supply.
func (s *Server) handleV2ProviderProtocolConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var input struct {
		Name               string                            `json:"name"`
		BaseURL            string                            `json:"base_url"`
		Enabled            bool                              `json:"enabled"`
		ModelSyncInterval  *domain.ProviderModelSyncInterval `json:"model_sync_interval,omitempty"`
		ModelSyncCatalog   json.RawMessage                   `json:"model_sync_catalog"`
		ProtocolConversion map[domain.Protocol]bool          `json:"protocol_conversion"`
		ScheduledMessage   json.RawMessage                   `json:"scheduled_message"`
		Concurrency        *domain.ConcurrencyPolicy         `json:"concurrency"`
		Connections        []v2ConnectionSpec                `json:"connections"`
	}
	if err := decodeMgmtBody(r, &input); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.ModelSyncInterval != nil && !input.ModelSyncInterval.Valid() {
		writeMgmtErr(w, http.StatusBadRequest, "model_sync_interval must be one of off, 30m, or 1h")
		return
	}
	modelSyncCatalog, modelSyncCatalogSet, err := optionalProviderModelSyncCatalog(input.ModelSyncCatalog)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if modelSyncCatalogSet {
		if err := validateProviderModelSyncCatalog(domain.ProviderCustom, modelSyncCatalog); err != nil {
			writeMgmtErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	scheduledMessage, scheduledMessageSet, err := optionalProviderScheduledMessage(input.ScheduledMessage)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	providerID := r.PathValue("id")
	seen := map[string]bool{}
	for index := range input.Connections {
		connection := &input.Connections[index]
		connection.ProviderID = providerID
		applyProviderModelCapabilities(domain.ProviderCustom, connection.Models)
		if strings.TrimSpace(connection.ID) == "" || seen[connection.ID] {
			writeMgmtErr(w, http.StatusBadRequest, "connection ids must be present and unique")
			return
		}
		seen[connection.ID] = true
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		providerAt := providerIndex(intent, providerID)
		if providerAt < 0 {
			return fmt.Errorf("provider not found")
		}
		provider := &intent.Providers[providerAt]
		if provider.Type == domain.ProviderCliproxyCodex {
			return fmt.Errorf("Codex OAuth providers use account connections")
		}
		provider.Name, provider.BaseURL, provider.Enabled = input.Name, input.BaseURL, input.Enabled
		if input.ModelSyncInterval != nil {
			provider.ModelSyncInterval = *input.ModelSyncInterval
		}
		if modelSyncCatalogSet {
			provider.ModelSyncCatalog = cloneProviderModelSyncCatalog(modelSyncCatalog)
		}
		provider.ProtocolConversion = input.ProtocolConversion
		if scheduledMessageSet {
			provider.ScheduledMessage = scheduledMessage
		}
		provider.Concurrency = cloneConcurrencyPolicy(input.Concurrency)
		provider.Connections = nil
		for _, connection := range input.Connections {
			provider.Connections = append(provider.Connections, domain.ConnectionRef{ID: connection.ID})
		}
		kept := intent.Connections[:0]
		for _, connection := range intent.Connections {
			if connection.ProviderID != providerID {
				kept = append(kept, connection)
			}
		}
		intent.Connections = kept
		for _, spec := range input.Connections {
			intent.Connections = append(intent.Connections, spec.domain())
		}
		if err := validateProviderModelSyncCatalog(provider.Type, provider.ModelSyncCatalog); err != nil {
			return err
		}
		if err := validateProviderModelSyncCatalogConnections(provider.ID, provider.ModelSyncCatalog, intent.Connections); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "provider_id": providerID})
}

func (s *Server) handleV2ProviderPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var patch struct {
		Name               *string                           `json:"name"`
		BaseURL            *string                           `json:"base_url"`
		Enabled            *bool                             `json:"enabled"`
		ModelSyncInterval  *domain.ProviderModelSyncInterval `json:"model_sync_interval"`
		ModelSyncCatalog   json.RawMessage                   `json:"model_sync_catalog"`
		ProtocolConversion *map[domain.Protocol]bool         `json:"protocol_conversion"`
		ScheduledMessage   json.RawMessage                   `json:"scheduled_message"`
		Concurrency        json.RawMessage                   `json:"concurrency"`
		Connections        *[]v2ProviderConnectionPatch      `json:"connections"`
	}
	if err := decodeMgmtBody(r, &patch); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if patch.ModelSyncInterval != nil && !patch.ModelSyncInterval.Valid() {
		writeMgmtErr(w, http.StatusBadRequest, "model_sync_interval must be one of off, 30m, or 1h")
		return
	}
	modelSyncCatalog, modelSyncCatalogSet, err := optionalProviderModelSyncCatalog(patch.ModelSyncCatalog)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if modelSyncCatalogSet && modelSyncCatalog != nil {
		if err := validateProviderModelSyncCatalog(domain.ProviderCustom, modelSyncCatalog); err != nil {
			writeMgmtErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	concurrency, concurrencySet, err := optionalConcurrencyPolicy(patch.Concurrency)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	scheduledMessage, scheduledMessageSet, err := optionalProviderScheduledMessage(patch.ScheduledMessage)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		index := providerIndex(intent, r.PathValue("id"))
		if index < 0 {
			return fmt.Errorf("provider not found")
		}
		provider := &intent.Providers[index]
		if patch.Name != nil {
			provider.Name = *patch.Name
		}
		if patch.BaseURL != nil {
			provider.BaseURL = *patch.BaseURL
		}
		if patch.Enabled != nil {
			provider.Enabled = *patch.Enabled
		}
		if patch.ModelSyncInterval != nil {
			provider.ModelSyncInterval = *patch.ModelSyncInterval
		}
		if modelSyncCatalogSet {
			if err := validateProviderModelSyncCatalog(provider.Type, modelSyncCatalog); err != nil {
				return err
			}
			provider.ModelSyncCatalog = cloneProviderModelSyncCatalog(modelSyncCatalog)
		}
		if err := validateProviderModelSyncCatalogConnections(provider.ID, provider.ModelSyncCatalog, intent.Connections); err != nil {
			return err
		}
		if patch.ProtocolConversion != nil {
			provider.ProtocolConversion = *patch.ProtocolConversion
		}
		if scheduledMessageSet {
			provider.ScheduledMessage = scheduledMessage
		}
		if concurrencySet {
			provider.Concurrency = concurrency
		}
		if patch.Connections != nil {
			if provider.Type != domain.ProviderCliproxyCodex {
				return fmt.Errorf("provider connection model patches require a Codex OAuth provider")
			}
			seen := make(map[string]struct{}, len(*patch.Connections))
			for _, connectionPatch := range *patch.Connections {
				connectionID := strings.TrimSpace(connectionPatch.ID)
				if connectionID == "" {
					return fmt.Errorf("connection id is required")
				}
				if _, duplicate := seen[connectionID]; duplicate {
					return fmt.Errorf("connection ids must be unique")
				}
				seen[connectionID] = struct{}{}
				connectionAt := connectionIndexV2(intent, connectionID)
				if connectionAt < 0 || intent.Connections[connectionAt].ProviderID != provider.ID {
					return fmt.Errorf("connection does not belong to provider")
				}
				if len(connectionPatch.Models) == 0 {
					return fmt.Errorf("each OAuth account requires at least one upstream model")
				}
				applyProviderModelCapabilities(provider.Type, connectionPatch.Models)
				connection := &intent.Connections[connectionAt]
				if connectionPatch.Enabled != nil {
					connection.Enabled = *connectionPatch.Enabled
				}
				connection.Models = make([]domain.ConnectionModel, 0, len(connectionPatch.Models))
				for _, model := range connectionPatch.Models {
					connection.Models = append(connection.Models, domain.ConnectionModel{
						UpstreamModel: model.UpstreamModel,
						Protocols:     model.Protocols,
						Capabilities:  model.Capabilities,
						PlanID:        model.PlanID,
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		status := http.StatusUnprocessableEntity
		if err.Error() == "provider not found" {
			status = http.StatusNotFound
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "provider_id": r.PathValue("id")})
}

func routeUsesProvider(route domain.RouteSpec, providerID string) bool {
	for _, step := range route.Steps {
		if step.ProviderID == providerID {
			return true
		}
	}
	return false
}

type v2RouteDetachImpact struct {
	Models           []string
	DisabledModels   []string
	RemovedOverrides []string
}

func removeV2RouteSteps(steps []domain.RouteStep, remove func(domain.RouteStep) bool) ([]domain.RouteStep, int) {
	kept := steps[:0]
	removed := 0
	for _, step := range steps {
		if remove(step) {
			removed++
			continue
		}
		kept = append(kept, step)
	}
	return kept, removed
}

func v2AdjustmentLeavesRoute(model *domain.LogicalModel, override domain.OverrideSpec) bool {
	if override.Mode != domain.OverrideAdjustDefault {
		return true
	}
	if len(override.Prefer) == 0 && len(override.Avoid) == 0 {
		return false
	}
	avoided := make(map[string]bool, len(override.Avoid))
	for _, step := range override.Avoid {
		avoided[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)] = true
	}
	for _, step := range model.DefaultRoute.Steps {
		if !avoided[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)] {
			return true
		}
	}
	return false
}

func detachV2RouteSteps(intent *config.File, remove func(domain.RouteStep) bool) v2RouteDetachImpact {
	impact := v2RouteDetachImpact{}
	for modelIndex := range intent.LogicalModels {
		model := &intent.LogicalModels[modelIndex]
		affected := false
		var removed int
		model.DefaultRoute.Steps, removed = removeV2RouteSteps(model.DefaultRoute.Steps, remove)
		affected = removed > 0

		overrides := model.Overrides[:0]
		for overrideIndex := range model.Overrides {
			override := model.Overrides[overrideIndex]
			overrideAffected := false
			if override.Route != nil {
				override.Route.Steps, removed = removeV2RouteSteps(override.Route.Steps, remove)
				overrideAffected = removed > 0
				if override.Mode == domain.OverrideCustomRoute && len(override.Route.Steps) == 0 {
					impact.RemovedOverrides = append(impact.RemovedOverrides, model.ID+"/"+override.ID)
					affected = true
					continue
				}
			}
			override.Prefer, removed = removeV2RouteSteps(override.Prefer, remove)
			overrideAffected = overrideAffected || removed > 0
			override.Avoid, removed = removeV2RouteSteps(override.Avoid, remove)
			overrideAffected = overrideAffected || removed > 0
			// An adjustment can become either a no-op or an all-route exclusion
			// after its Provider+Model dependencies are detached. Neither state is
			// valid in the V2 contract, so surface it as a removed Override just as
			// we do for an emptied custom route.
			if !v2AdjustmentLeavesRoute(model, override) {
				impact.RemovedOverrides = append(impact.RemovedOverrides, model.ID+"/"+override.ID)
				affected = true
				continue
			}
			affected = affected || overrideAffected
			overrides = append(overrides, override)
		}
		model.Overrides = overrides
		if !affected {
			continue
		}
		impact.Models = append(impact.Models, model.ID)
		model.ExistingConversationStrategy = domain.ConversationMigrate
		if model.Enabled && len(model.DefaultRoute.Steps) == 0 {
			model.Enabled = false
			impact.DisabledModels = append(impact.DisabledModels, model.ID)
		}
	}
	return impact
}

func cascadeDeleteV2Provider(intent *config.File, id string) ([]string, v2RouteDetachImpact, error) {
	index := providerIndex(intent, id)
	if index < 0 {
		return nil, v2RouteDetachImpact{}, fmt.Errorf("provider not found")
	}
	connections := intent.Connections[:0]
	removedConnections := []string{}
	for _, connection := range intent.Connections {
		if connection.ProviderID == id {
			removedConnections = append(removedConnections, connection.ID)
			continue
		}
		connections = append(connections, connection)
	}
	intent.Connections = connections
	impact := detachV2RouteSteps(intent, func(step domain.RouteStep) bool {
		return step.ProviderID == id
	})
	intent.Providers = append(intent.Providers[:index], intent.Providers[index+1:]...)
	return removedConnections, impact, nil
}

func (s *Server) handleV2ProviderDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	id := r.PathValue("id")
	cascade := r.URL.Query().Get("cascade") == "true"
	if !cascade {
		for _, record := range s.deps.Scheduler.ActiveTraffic() {
			if record.ProviderID == id {
				writeMgmtErr(w, http.StatusConflict, "provider has active traffic")
				return
			}
		}
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	var removedConnections []string
	var impact v2RouteDetachImpact
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		if cascade {
			var deleteErr error
			removedConnections, impact, deleteErr = cascadeDeleteV2Provider(intent, id)
			return deleteErr
		}
		index := providerIndex(intent, id)
		if index < 0 {
			return fmt.Errorf("provider not found")
		}
		provider := intent.Providers[index]
		if provider.Enabled {
			return fmt.Errorf("provider must be disabled before deletion")
		}
		for _, connection := range intent.Connections {
			if connection.ProviderID == id {
				return fmt.Errorf("delete all provider connections first")
			}
		}
		for _, model := range intent.LogicalModels {
			if routeUsesProvider(model.DefaultRoute, id) {
				return fmt.Errorf("provider is referenced by logical model %s", model.ID)
			}
			for _, override := range model.Overrides {
				if override.Route != nil && routeUsesProvider(*override.Route, id) {
					return fmt.Errorf("provider is referenced by logical model %s override %s", model.ID, override.ID)
				}
				if routeUsesProvider(domain.RouteSpec{Steps: override.Prefer}, id) || routeUsesProvider(domain.RouteSpec{Steps: override.Avoid}, id) {
					return fmt.Errorf("provider is referenced by logical model %s override %s", model.ID, override.ID)
				}
			}
		}
		intent.Providers = append(intent.Providers[:index], intent.Providers[index+1:]...)
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{
		"draft_id": updated.ID, "deleted_provider_id": id, "cascade": cascade,
		"removed_connection_ids": removedConnections, "detached_model_ids": impact.Models,
		"disabled_model_ids": impact.DisabledModels, "removed_override_ids": impact.RemovedOverrides,
	})
}

func (s *Server) handleV2ConnectionCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var input v2ConnectionSpec
	if err := decodeMgmtBody(r, &input); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	input.ProviderID = r.PathValue("id")
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		providerAt := providerIndex(intent, input.ProviderID)
		if providerAt < 0 {
			return fmt.Errorf("provider not found")
		}
		if connectionIndexV2(intent, input.ID) >= 0 {
			return fmt.Errorf("connection already exists")
		}
		applyProviderModelCapabilities(intent.Providers[providerAt].Type, input.Models)
		intent.Connections = append(intent.Connections, input.domain())
		intent.Providers[providerAt].Connections = append(intent.Providers[providerAt].Connections, domain.ConnectionRef{ID: input.ID})
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "connection": input})
}

func (s *Server) handleV2ConnectionPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	var patch struct {
		Identity    *string                  `json:"identity"`
		BaseURL     *string                  `json:"base_url"`
		FullURL     *string                  `json:"full_url"`
		Credential  *v2CredentialSpec        `json:"credential"`
		Enabled     *bool                    `json:"enabled"`
		Models      *[]v2ConnectionModelSpec `json:"models"`
		Concurrency json.RawMessage          `json:"concurrency"`
	}
	if err := decodeMgmtBody(r, &patch); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	concurrency, concurrencySet, err := optionalConcurrencyPolicy(patch.Concurrency)
	if err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		index := connectionIndexV2(intent, r.PathValue("connection_id"))
		if index < 0 {
			return fmt.Errorf("connection not found")
		}
		connection := &intent.Connections[index]
		if connection.ProviderID != r.PathValue("id") {
			return fmt.Errorf("connection does not belong to provider")
		}
		if patch.Identity != nil {
			connection.Identity = *patch.Identity
		}
		if patch.BaseURL != nil {
			connection.BaseURL = *patch.BaseURL
		}
		if patch.FullURL != nil {
			connection.FullURL = *patch.FullURL
		}
		if patch.Enabled != nil {
			connection.Enabled = *patch.Enabled
		}
		if patch.Credential != nil {
			connection.Credential = domain.CredentialRef{SecretRef: patch.Credential.SecretRef, Header: patch.Credential.Header, QueryParam: patch.Credential.QueryParam, Prefix: patch.Credential.Prefix}
		}
		if patch.Models != nil {
			providerAt := providerIndex(intent, connection.ProviderID)
			if providerAt >= 0 {
				applyProviderModelCapabilities(intent.Providers[providerAt].Type, *patch.Models)
			}
			connection.Models = nil
			for _, model := range *patch.Models {
				connection.Models = append(connection.Models, domain.ConnectionModel{UpstreamModel: model.UpstreamModel, Protocols: model.Protocols, Capabilities: model.Capabilities, PlanID: model.PlanID})
			}
		}
		if concurrencySet {
			connection.Concurrency = concurrency
		}
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{"draft_id": updated.ID, "connection_id": r.PathValue("connection_id")})
}

func (s *Server) handleV2ConnectionDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireV2Control(w) {
		return
	}
	connectionID := r.PathValue("connection_id")
	cascade := r.URL.Query().Get("cascade") == "true"
	if !cascade {
		for _, record := range s.deps.Scheduler.ActiveTraffic() {
			if record.ConnectionID == connectionID {
				writeMgmtErr(w, http.StatusConflict, "connection has active traffic")
				return
			}
		}
		if stats := s.deps.Scheduler.AffinityStatsByConnection(time.Now())[connectionID]; stats.Conversation > 0 {
			writeMgmtErr(w, http.StatusConflict, "connection has active conversation bindings")
			return
		}
	}
	draft, err := s.editableV2Draft(r)
	if err != nil {
		writeDraftError(w, err)
		return
	}
	var impact v2RouteDetachImpact
	updated, err := s.updateV2Draft(draft, func(intent *config.File) error {
		index := connectionIndexV2(intent, connectionID)
		if index < 0 {
			return fmt.Errorf("connection not found")
		}
		if intent.Connections[index].ProviderID != r.PathValue("id") {
			return fmt.Errorf("connection does not belong to provider")
		}
		if !cascade && intent.Connections[index].Enabled {
			return fmt.Errorf("connection must be disabled before deletion")
		}
		removedConnection := intent.Connections[index]
		intent.Connections = append(intent.Connections[:index], intent.Connections[index+1:]...)
		providerAt := providerIndex(intent, r.PathValue("id"))
		if providerAt >= 0 {
			refs := intent.Providers[providerAt].Connections[:0]
			for _, ref := range intent.Providers[providerAt].Connections {
				if ref.ID != connectionID {
					refs = append(refs, ref)
				}
			}
			intent.Providers[providerAt].Connections = refs
		}
		if cascade {
			remainingSupply := map[string]bool{}
			for _, connection := range intent.Connections {
				if connection.ProviderID != removedConnection.ProviderID {
					continue
				}
				for _, model := range connection.Models {
					remainingSupply[domain.ProviderModelKey(connection.ProviderID, model.UpstreamModel)] = true
				}
			}
			removedSupply := map[string]bool{}
			for _, model := range removedConnection.Models {
				key := domain.ProviderModelKey(removedConnection.ProviderID, model.UpstreamModel)
				if !remainingSupply[key] {
					removedSupply[key] = true
				}
			}
			impact = detachV2RouteSteps(intent, func(step domain.RouteStep) bool {
				return removedSupply[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)]
			})
		}
		return nil
	})
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusAccepted, map[string]any{
		"draft_id": updated.ID, "deleted_connection_id": connectionID, "cascade": cascade,
		"detached_model_ids": impact.Models, "disabled_model_ids": impact.DisabledModels,
		"removed_override_ids": impact.RemovedOverrides,
	})
}

func (s *Server) v2ProviderConnection(providerID, connectionID string) (*domain.Provider, *domain.Connection) {
	cfg := s.currentConfig()
	var provider *domain.Provider
	var connection *domain.Connection
	for index := range cfg.Providers {
		if cfg.Providers[index].ID == providerID {
			provider = &cfg.Providers[index]
			break
		}
	}
	for index := range cfg.Connections {
		if cfg.Connections[index].ID == connectionID && cfg.Connections[index].ProviderID == providerID {
			connection = &cfg.Connections[index]
			break
		}
	}
	return provider, connection
}

func connectionBaseURL(provider *domain.Provider, connection *domain.Connection) string {
	if connection != nil && strings.TrimSpace(connection.BaseURL) != "" {
		return connection.BaseURL
	}
	if provider != nil {
		return provider.BaseURL
	}
	return ""
}

func connectionFullURL(connection *domain.Connection) string {
	if connection == nil {
		return ""
	}
	return connection.FullURL
}

func (s *Server) handleV2ConnectionTest(w http.ResponseWriter, r *http.Request) {
	provider, connection := s.v2ProviderConnection(r.PathValue("id"), r.PathValue("connection_id"))
	if provider == nil || connection == nil {
		writeMgmtErr(w, http.StatusNotFound, "provider connection not found")
		return
	}
	if len(connection.Models) == 0 {
		writeMgmtErr(w, http.StatusUnprocessableEntity, "connection has no upstream models")
		return
	}
	model := connection.Models[0]
	protocol := domain.PreferredProtocol(model.Protocols)
	if protocol == "" {
		writeMgmtErr(w, http.StatusUnprocessableEntity, "connection model has no native protocol")
		return
	}
	timeout := 15 * time.Second
	if protocol == domain.ProtocolOpenAIImages {
		timeout = 120 * time.Second
	}
	base := egress.Base{
		HTTP:     connectionTestHTTPClient(s.deps.InferenceHTTPClient, connection.ID, timeout),
		Endpoint: &domain.Endpoint{ID: "connection-test-" + connection.ID, BaseURL: connectionBaseURL(provider, connection), FullURL: connectionFullURL(connection), Protocol: protocol},
		CredRef:  connection.Credential.SecretRef, Credential: connection.Credential, Model: model.UpstreamModel,
	}
	var caller egress.Caller
	switch provider.Type {
	case "", domain.ProviderCustom:
		if s.deps.Resolver == nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "credential resolver not configured")
			return
		}
		secret, err := s.deps.Resolver.Resolve(connection.Credential.SecretRef)
		if err != nil {
			writeMgmtErr(w, http.StatusUnprocessableEntity, "credential unavailable")
			return
		}
		base.HTTP, err = s.nativeProviderClient(connection.Credential.SecretRef, base.Endpoint.BaseURL, base.Endpoint.FullURL, protocol, base.HTTP)
		if err != nil {
			writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		base.Secret = secret
		caller = egress.NewCaller(base, protocol)
	case domain.ProviderCliproxyCodex:
		if s.deps.OAuth == nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "oauth accounts not configured")
			return
		}
		account, ok := domain.OAuthAccountFromRef(connection.Credential.SecretRef)
		if !ok {
			writeMgmtErr(w, http.StatusUnprocessableEntity, "connection is missing its OAuth account")
			return
		}
		accessToken, accountID, err := s.deps.OAuth.Identity(r.Context(), account)
		if err != nil {
			writeMgmtErr(w, http.StatusConflict, "connection account is not logged in")
			return
		}
		base.Secret = accessToken
		base.CodexClientVersion = s.currentConfig().EffectiveCodexClientVersion()
		base.AfterResponse = func(headers http.Header) { s.deps.OAuth.ObserveUsageHeaders(account, accountID, headers) }
		base.BeforeSend = func(ctx context.Context) *domain.Failure { return s.deps.OAuth.CheckUsage(ctx, account, accountID) }
		if protocol == domain.ProtocolOpenAIImages {
			caller = egress.NewCodexImages(base, accountID)
		} else {
			caller = egress.NewCodexCaller(base, accountID)
		}
	default:
		writeMgmtErr(w, http.StatusUnprocessableEntity, "unsupported provider type")
		return
	}
	contract := connectionTestContract(protocol, model.UpstreamModel)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	started := time.Now()
	response, failure := caller.Invoke(ctx, contract)
	latency := time.Since(started).Milliseconds()
	targetID := connection.RuntimeTargetID(model.UpstreamModel)
	if failure != nil {
		if s.deps.Manager != nil {
			s.deps.Manager.RecordFailure(targetID, failure, time.Now())
		}
		s.observeConnectionTestAlert(connection.ID, failure, time.Now())
		writeMgmtJSON(w, http.StatusOK, map[string]any{"ok": false, "latency_ms": latency, "failure": failure,
			"tested_model": model.UpstreamModel, "protocol": protocol})
		return
	}
	if s.deps.Manager != nil {
		s.deps.Manager.RecordConnectionProbeSuccess(targetID, latency, time.Now())
	}
	s.recoverConnectionTestAlerts(connection.ID, time.Now())
	writeMgmtJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": latency, "finish_reason": response.FinishReason,
		"tested_model": model.UpstreamModel, "protocol": protocol})
}

func connectionTestHTTPClient(live func(string) *http.Client, connectionID string, timeout time.Duration) *http.Client {
	if live != nil {
		if client := live(connectionID); client != nil && client.Transport != nil {
			return &http.Client{Transport: client.Transport, Timeout: timeout}
		}
	}
	return &http.Client{Timeout: timeout}
}

func (s *Server) handleV2ConnectionSecretRotate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "secret store is not configured")
		return
	}
	_, connection := s.v2ProviderConnection(r.PathValue("id"), r.PathValue("connection_id"))
	if connection == nil {
		writeMgmtErr(w, http.StatusNotFound, "provider connection not found")
		return
	}
	secretName := strings.TrimPrefix(connection.Credential.SecretRef, "store:")
	if secretName == connection.Credential.SecretRef || secretName == "" {
		writeMgmtErr(w, http.StatusConflict, "only store-backed API key/token credentials can be rotated here")
		return
	}
	var body struct {
		Secret string `json:"secret"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Secret) == "" {
		writeMgmtErr(w, http.StatusBadRequest, "secret is required")
		return
	}
	previous, existed := s.deps.Store.Get(secretName)
	if err := s.deps.Store.Set(secretName, body.Secret); err != nil {
		s.emitFailureAlert("credential_rotation_failed", "connection", connection.ID, "Credential rotation failed: "+connection.ID, err.Error())
		writeMgmtErr(w, http.StatusInternalServerError, "rotate connection credential")
		return
	}
	s.advanceSecretGeneration(secretName)
	s.configMu.Lock()
	next, cloneErr := cloneConfig(s.deps.Config)
	publishErr := cloneErr
	if publishErr == nil {
		publishErr = s.publishConfig(next)
	}
	s.configMu.Unlock()
	if publishErr != nil {
		if existed {
			_ = s.deps.Store.Set(secretName, previous)
		} else {
			_ = s.deps.Store.Delete(secretName)
		}
		s.advanceSecretGeneration(secretName)
		s.emitFailureAlert("credential_rotation_failed", "connection", connection.ID, "Credential rotation failed: "+connection.ID, publishErr.Error())
		writeMgmtErr(w, http.StatusInternalServerError, "rotate connection credential and refresh runtime")
		return
	}
	response := map[string]any{"rotated": true, "connection_id": connection.ID}
	status := http.StatusOK
	if err := s.markProviderModelSyncDueForSecret(secretName); err != nil {
		response["warning"] = "provider model synchronization could not be scheduled"
		status = http.StatusMultiStatus
	}
	writeMgmtJSON(w, status, response)
}

func (s *Server) v2ConnectionTargets(providerID, connectionID, upstreamModel string) []domain.ResourceTargetID {
	if s.deps.Scheduler == nil || s.deps.Scheduler.Snapshot() == nil {
		return nil
	}
	snapshot := s.deps.Scheduler.Snapshot()
	result := []domain.ResourceTargetID{}
	for _, targetID := range snapshot.TargetOrder {
		target := snapshot.Targets[targetID]
		if target == nil || target.ProviderID != providerID || target.ConnectionID != connectionID {
			continue
		}
		if upstreamModel != "" && target.Model != upstreamModel {
			continue
		}
		result = append(result, targetID)
	}
	return result
}

func (s *Server) handleV2ConnectionClearCooldown(w http.ResponseWriter, r *http.Request) {
	s.handleRuntimeClearCooldown(w, r)
}

func (s *Server) handleV2ConnectionSetQuota(w http.ResponseWriter, r *http.Request) {
	if s.deps.Scheduler == nil || s.deps.Manager == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "runtime state manager is not configured")
		return
	}
	provider, connection := s.v2ProviderConnection(r.PathValue("id"), r.PathValue("connection_id"))
	if provider == nil || connection == nil {
		writeMgmtErr(w, http.StatusNotFound, "provider connection not found")
		return
	}
	var body struct {
		UpstreamModel string  `json:"upstream_model"`
		Used          float64 `json:"used"`
		Limit         float64 `json:"limit"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Used < 0 || body.Limit < 0 {
		writeMgmtErr(w, http.StatusBadRequest, "used and limit must not be negative")
		return
	}
	if body.UpstreamModel == "" && len(connection.Models) == 1 {
		body.UpstreamModel = connection.Models[0].UpstreamModel
	}
	if body.UpstreamModel == "" {
		writeMgmtErr(w, http.StatusBadRequest, "upstream_model is required for a multi-model connection")
		return
	}
	targets := s.v2ConnectionTargets(provider.ID, connection.ID, body.UpstreamModel)
	if len(targets) != 1 {
		writeMgmtErr(w, http.StatusNotFound, "connection upstream model has no runtime target")
		return
	}
	s.deps.Manager.SetQuota(targets[0], body.Used, body.Limit, time.Now(), "manual", domain.ConfHigh)
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"provider_id": provider.ID, "connection_id": connection.ID, "upstream_model": body.UpstreamModel,
		"used": body.Used, "limit": body.Limit,
	})
}

func (s *Server) v2APIKey(id string) (*domain.APIKeyIdentity, []domain.APIKeySecret, error) {
	if s.deps.APIKeys == nil {
		return nil, nil, fmt.Errorf("API key store is not configured")
	}
	identities, secrets, err := s.deps.APIKeys.APIKeys()
	if err != nil {
		return nil, nil, err
	}
	for i := range identities {
		if identities[i].ID == id {
			return &identities[i], secrets[id], nil
		}
	}
	return nil, nil, nil
}

func (s *Server) handleV2APIKeys(w http.ResponseWriter, r *http.Request) {
	if s.deps.APIKeys == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "API key store is not configured")
		return
	}
	identities, secrets, err := s.deps.APIKeys.APIKeys()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	from, to, windowErr := v2Window(r)
	if windowErr != nil {
		writeMgmtErr(w, http.StatusBadRequest, windowErr.Error())
		return
	}
	if s.deps.Ledger == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "usage ledger is not configured")
		return
	}
	usage, err := s.deps.Ledger.ByConsumer(from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(identities))
	for _, identity := range identities {
		keyUsage := usage[identity.Name]
		if keyUsage == nil {
			keyUsage = &ledger.UsageSummary{}
		}
		items = append(items, map[string]any{
			"identity": identity, "secrets": secrets[identity.ID], "usage": keyUsage,
		})
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"api_keys": items, "from": from, "to": to})
}

func (s *Server) handleV2APIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	identity, _, err := s.v2APIKey(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	if identity.RevokedAt != nil {
		writeMgmtJSON(w, http.StatusOK, map[string]any{"revoked": identity.ID, "idempotent": true})
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := findConsumerIndex(next, identity.Name)
	if index < 0 {
		writeMgmtErr(w, http.StatusConflict, "API key compatibility identity is missing")
		return
	}
	disabled := false
	next.ConsumerKeys[index].Enabled = &disabled
	if err := s.deps.Control.RevokeAPIKey(identity.ID); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Revocation is a security-forward, irreversible transition. A config
	// publish failure must never resurrect retired credentials.
	if err := s.publishConfig(next); err != nil {
		if s.deps.ReloadAPIKeys != nil {
			_ = s.deps.ReloadAPIKeys()
		}
		writeMgmtJSON(w, http.StatusAccepted, map[string]any{"revoked": identity.ID, "runtime_refresh_pending": true})
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"revoked": identity.ID})
}

func (s *Server) handleV2APIKeyDetail(w http.ResponseWriter, r *http.Request) {
	identity, secrets, err := s.v2APIKey(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	from, to, windowErr := v2Window(r)
	if windowErr != nil {
		writeMgmtErr(w, http.StatusBadRequest, windowErr.Error())
		return
	}
	if s.deps.Ledger == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "usage ledger is not configured")
		return
	}
	byConsumer, usageErr := s.deps.Ledger.ByConsumer(from, to)
	if usageErr != nil {
		writeMgmtErr(w, http.StatusInternalServerError, usageErr.Error())
		return
	}
	keyUsage := byConsumer[identity.Name]
	if keyUsage == nil {
		keyUsage = &ledger.UsageSummary{}
	}
	recent, _, recentErr := s.deps.Ledger.QueryRequests(domain.LogFilter{
		From: from, To: to, ConsumerKey: identity.Name, Limit: 10,
	})
	if recentErr != nil {
		writeMgmtErr(w, http.StatusInternalServerError, recentErr.Error())
		return
	}
	recentViews := make([]map[string]any, 0, len(recent))
	for i := range recent {
		recentViews = append(recentViews, v2RequestSummary(&recent[i], ""))
	}
	var lastRequestAt *time.Time
	if len(recent) > 0 {
		observed := recent[0].Ts
		lastRequestAt = &observed
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"identity": identity, "secrets": secrets, "usage": keyUsage,
		"recent_requests": recentViews, "last_request_at": lastRequestAt, "from": from, "to": to,
	})
}

func (s *Server) handleV2APIKeyReveal(w http.ResponseWriter, r *http.Request) {
	if s.deps.Control == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "API key store is not configured")
		return
	}
	identity, _, err := s.v2APIKey(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	secret, available, err := s.deps.Control.APIKeyActiveSecret(identity.ID)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "read API key secret")
		return
	}
	if !available {
		writeMgmtJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":      "SECRET_NOT_RECOVERABLE",
				"message":   "This legacy API key predates persistent reveal. Change the key once to enable reveal and copy.",
				"retryable": false,
			},
		})
		return
	}
	if s.deps.Security != nil {
		administrator := managementIdentityFrom(r)
		_ = s.deps.Security.RecordManagementSecurityEvent("api_key_revealed", administrator.Actor(),
			r.RemoteAddr, map[string]any{"api_key_id": identity.ID}, time.Now())
	}
	w.Header().Set("Cache-Control", "no-store")
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"api_key_id": identity.ID, "name": identity.Name, "secret": secret,
	})
}

func (s *Server) handleV2APIKeyCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Control == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "API key store is not configured")
		return
	}
	var body struct {
		Name             string            `json:"name"`
		Description      string            `json:"description"`
		ScopeModels      []string          `json:"scope_models"`
		ScopeProtocols   []domain.Protocol `json:"scope_protocols"`
		ExpiresAt        *time.Time        `json:"expires_at"`
		RateLimitRPM     int               `json:"rate_limit_rpm"`
		MonthlyBudgetUSD float64           `json:"monthly_budget_usd"`
		ExactOnly        bool              `json:"exact_only"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeMgmtErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if body.RateLimitRPM < 0 || body.MonthlyBudgetUSD < 0 {
		writeMgmtErr(w, http.StatusBadRequest, "rate_limit_rpm and monthly_budget_usd cannot be negative")
		return
	}
	for _, protocol := range body.ScopeProtocols {
		if protocol != domain.ProtocolOpenAIChat && protocol != domain.ProtocolOpenAIResponses && protocol != domain.ProtocolOpenAIImages && protocol != domain.ProtocolAnthropic {
			writeMgmtErr(w, http.StatusBadRequest, "unsupported scope protocol")
			return
		}
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if findConsumerIndex(s.deps.Config, body.Name) >= 0 {
		writeMgmtErr(w, http.StatusConflict, "API key name already exists")
		return
	}
	secret, err := generateConsumerKey()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	enabled := true
	next.ConsumerKeys = append(next.ConsumerKeys, config.ConsumerKey{Name: body.Name, Key: secret, Enabled: &enabled, MonthlyBudgetUSD: body.MonthlyBudgetUSD})
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	identities, _, err := s.deps.APIKeys.APIKeys()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var identity *domain.APIKeyIdentity
	for i := range identities {
		if identities[i].Name == body.Name {
			identity = &identities[i]
			break
		}
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusInternalServerError, "created API key identity unavailable")
		return
	}
	if body.Description != "" || len(body.ScopeModels) > 0 || len(body.ScopeProtocols) > 0 || body.ExpiresAt != nil || body.RateLimitRPM > 0 || body.MonthlyBudgetUSD > 0 || body.ExactOnly {
		if err := s.deps.Control.UpdateAPIKeyPolicy(identity.ID, body.Description, nil, body.ScopeModels,
			body.ScopeProtocols, body.ExpiresAt, body.RateLimitRPM, body.MonthlyBudgetUSD, body.ExactOnly); err != nil {
			writeMgmtErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		identity.Description = body.Description
		identity.ScopeModels = body.ScopeModels
		identity.ScopeProtocols = body.ScopeProtocols
		identity.ExpiresAt = body.ExpiresAt
		identity.RateLimitRPM = body.RateLimitRPM
		identity.MonthlyBudgetUSD = body.MonthlyBudgetUSD
		identity.ExactOnly = body.ExactOnly
		if s.deps.ReloadAPIKeys != nil {
			_ = s.deps.ReloadAPIKeys()
		}
	}
	writeMgmtJSON(w, http.StatusCreated, map[string]any{"identity": identity, "secret": secret, "secret_preview": maskConsumerKey(secret)})
}

func (s *Server) handleV2APIKeyPatch(w http.ResponseWriter, r *http.Request) {
	if s.deps.Control == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "API key store is not configured")
		return
	}
	var body struct {
		Description      *string            `json:"description"`
		Enabled          *bool              `json:"enabled"`
		ScopeModels      *[]string          `json:"scope_models"`
		ScopeProtocols   *[]domain.Protocol `json:"scope_protocols"`
		ExpiresAt        *time.Time         `json:"expires_at"`
		ClearExpiry      bool               `json:"clear_expiry"`
		RateLimitRPM     *int               `json:"rate_limit_rpm"`
		MonthlyBudgetUSD *float64           `json:"monthly_budget_usd"`
		ExactOnly        *bool              `json:"exact_only"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	identity, _, err := s.v2APIKey(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	if body.Enabled != nil && *body.Enabled && identity.RevokedAt != nil {
		writeMgmtErr(w, http.StatusConflict, controlstore.ErrAPIKeyRevoked.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if body.Enabled != nil {
		next, err := cloneConfig(s.deps.Config)
		if err != nil {
			writeMgmtErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		index := findConsumerIndex(next, identity.Name)
		if index < 0 {
			writeMgmtErr(w, http.StatusConflict, "API key compatibility identity is missing")
			return
		}
		next.ConsumerKeys[index].Enabled = body.Enabled
		if err := s.publishConfig(next); err != nil {
			writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	description := identity.Description
	if body.Description != nil {
		description = *body.Description
	}
	models := identity.ScopeModels
	if body.ScopeModels != nil {
		models = *body.ScopeModels
	}
	protocols := identity.ScopeProtocols
	if body.ScopeProtocols != nil {
		protocols = *body.ScopeProtocols
	}
	expires := identity.ExpiresAt
	if body.ClearExpiry {
		expires = nil
	} else if body.ExpiresAt != nil {
		expires = body.ExpiresAt
	}
	rate := identity.RateLimitRPM
	if body.RateLimitRPM != nil {
		rate = *body.RateLimitRPM
	}
	budget := identity.MonthlyBudgetUSD
	if body.MonthlyBudgetUSD != nil {
		budget = *body.MonthlyBudgetUSD
	}
	exactOnly := identity.ExactOnly
	if body.ExactOnly != nil {
		exactOnly = *body.ExactOnly
	}
	if rate < 0 || budget < 0 {
		writeMgmtErr(w, http.StatusBadRequest, "rate_limit_rpm and monthly_budget_usd cannot be negative")
		return
	}
	if body.Description != nil || body.ScopeModels != nil || body.ScopeProtocols != nil || body.ExpiresAt != nil || body.ClearExpiry || body.RateLimitRPM != nil || body.MonthlyBudgetUSD != nil || body.ExactOnly != nil {
		if err := s.deps.Control.UpdateAPIKeyPolicy(identity.ID, description, body.Enabled, models, protocols, expires, rate, budget, exactOnly); err != nil {
			writeMgmtErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.deps.ReloadAPIKeys != nil {
			if err := s.deps.ReloadAPIKeys(); err != nil {
				writeMgmtErr(w, http.StatusInternalServerError, "refresh API key runtime")
				return
			}
		}
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"updated": identity.ID})
}

func (s *Server) handleV2APIKeyDelete(w http.ResponseWriter, r *http.Request) {
	identity, _, err := s.v2APIKey(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := findConsumerIndex(next, identity.Name)
	if index < 0 {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	if len(next.ConsumerKeys) == 1 {
		writeMgmtErr(w, http.StatusConflict, "cannot delete the last API key")
		return
	}
	for _, logical := range next.LogicalModels {
		for _, override := range logical.Overrides {
			for _, caller := range override.Callers {
				if caller == identity.ID || caller == identity.Name {
					writeMgmtErr(w, http.StatusConflict, fmt.Sprintf(
						"API key is referenced by logical model %s override %s", logical.ID, override.ID))
					return
				}
			}
		}
	}
	next.ConsumerKeys = append(next.ConsumerKeys[:index], next.ConsumerKeys[index+1:]...)
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleV2APIKeyRotate(w http.ResponseWriter, r *http.Request) {
	identity, _, err := s.v2APIKey(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := findConsumerIndex(next, identity.Name)
	if index < 0 {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	secret, err := generateConsumerKey()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	next.ConsumerKeys[index].Key = secret
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"api_key_id": identity.ID, "secret": secret, "secret_preview": maskConsumerKey(secret)})
}

func (s *Server) handleV2APIKeySecretRetire(w http.ResponseWriter, r *http.Request) {
	if s.deps.Control == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "API key store is not configured")
		return
	}
	identity, _, err := s.v2APIKey(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identity == nil {
		writeMgmtErr(w, http.StatusNotFound, "API key not found")
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	secretID := r.PathValue("secret_id")
	previousState, err := s.deps.Control.APIKeySecretState(identity.ID, secretID)
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.deps.Control.RetireAPIKeySecret(identity.ID, secretID); err != nil {
		status := http.StatusNotFound
		if errors.Is(err, controlstore.ErrLastAuthenticatingSecret) {
			status = http.StatusConflict
		}
		writeMgmtErr(w, status, err.Error())
		return
	}
	reload := s.deps.ReloadAPIKeys
	if reload == nil {
		reload = func() error {
			next, cloneErr := cloneConfig(s.deps.Config)
			if cloneErr != nil {
				return cloneErr
			}
			return s.publishConfig(next)
		}
	}
	if err := reload(); err != nil {
		restoreErr := s.deps.Control.RestoreAPIKeySecretState(identity.ID, secretID, previousState)
		if restoreErr == nil {
			_ = reload()
		}
		writeMgmtErr(w, http.StatusInternalServerError, fmt.Sprintf(
			"refresh API key runtime: %v (durable rollback: %v)", err, restoreErr))
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"retired": secretID, "retired_at": time.Now()})
}
