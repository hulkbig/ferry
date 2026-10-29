package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/nativeauth"
)

const maxDiscoveredModels = 2000
const maxModelCatalogPages = 100

type providerModelDiscoveryHTTPError struct{ Status int }

func (e *providerModelDiscoveryHTTPError) Error() string {
	return fmt.Sprintf("upstream model discovery returned HTTP %d", e.Status)
}

// Protocol verification sends a real, potentially billable upstream request
// for each model. Keep this deliberately small and require models to be
// explicitly selected by the operator; the upstream /models catalog is never
// treated as authorization to fan out probes across every advertised model.
const maxProtocolProbeModels = 20

const (
	providerCapabilityText             = domain.CapabilityText
	providerCapabilityImages           = domain.CapabilityImages
	providerCapabilityStream           = domain.CapabilityStream
	providerCapabilityTools            = domain.CapabilityTools
	providerCapabilityParallelTools    = domain.CapabilityParallelTools
	providerCapabilityReasoning        = domain.CapabilityReasoning
	providerCapabilityStructuredOutput = domain.CapabilityStructuredOutput
	providerCapabilityVision           = domain.CapabilityVision
	providerCapabilityContextWindow    = domain.CapabilityContextWindow
	providerCapabilityMaxOutput        = domain.CapabilityMaxOutput

	providerCapabilityPositiveTTL = 30 * 24 * time.Hour
	providerCapabilityNegativeTTL = 7 * 24 * time.Hour

	providerProtocolProbeMaxTokens int64 = 32
	providerTextProbeMaxTokens     int64 = 64
	providerTextProbeRetryTokens   int64 = 256
)

// providerCapabilityRegistry is the complete scheduler-facing capability
// vocabulary. Keep it in lockstep with domain.Capabilities; a reflection test
// prevents new capability fields from silently bypassing evidence handling.
//
// Sampling controls, tool choice, prompt-cache accounting and conversation
// continuity are protocol/runtime semantics rather than independent routing
// capabilities, so they intentionally do not appear here.
func providerCapabilityRegistry() []string {
	return domain.CapabilityRegistry()
}

type providerModelDiscoveryInput struct {
	Type          domain.ProviderType `json:"type"`
	BaseURL       string              `json:"base_url"`
	Protocol      domain.Protocol     `json:"protocol"`
	Secret        string              `json:"secret,omitempty"`
	CredentialRef string              `json:"credential_ref,omitempty"`
}

type providerProtocolProbeInput struct {
	Protocol      domain.Protocol `json:"protocol"`
	BaseURL       string          `json:"base_url,omitempty"`
	FullURL       string          `json:"full_url,omitempty"`
	Secret        string          `json:"secret,omitempty"`
	CredentialRef string          `json:"credential_ref,omitempty"`
	Models        []string        `json:"models,omitempty"`
}

type providerProtocolDiscoveryInput struct {
	ProviderID    string                       `json:"provider_id,omitempty"`
	Type          domain.ProviderType          `json:"type"`
	BaseURL       string                       `json:"base_url"`
	Secret        string                       `json:"secret,omitempty"`
	CredentialRef string                       `json:"credential_ref,omitempty"`
	Protocols     []providerProtocolProbeInput `json:"protocols,omitempty"`
	Refresh       bool                         `json:"refresh,omitempty"`
}

type providerProtocolProbeResult struct {
	Protocol       domain.Protocol                    `json:"protocol"`
	BaseURL        string                             `json:"base_url,omitempty"`
	FullURL        string                             `json:"full_url,omitempty"`
	Endpoint       string                             `json:"endpoint"`
	Status         string                             `json:"status"`
	Models         []string                           `json:"models"`
	ModelResults   []providerProtocolModelProbeResult `json:"model_results"`
	CatalogError   string                             `json:"catalog_error,omitempty"`
	Error          string                             `json:"error,omitempty"`
	ProbeTruncated bool                               `json:"probe_truncated,omitempty"`
}

type providerProtocolModelProbeResult struct {
	Model              string                             `json:"model"`
	Status             string                             `json:"status"`
	Error              string                             `json:"error,omitempty"`
	Capabilities       domain.Capabilities                `json:"capabilities"`
	CapabilityEvidence []providerCapabilityEvidenceResult `json:"capability_evidence,omitempty"`
	CacheHit           bool                               `json:"cache_hit,omitempty"`
	ProbeRequests      int                                `json:"probe_requests,omitempty"`
	EvidenceError      string                             `json:"evidence_error,omitempty"`
	EvidenceChanged    bool                               `json:"-"`
	EvidenceStoreError bool                               `json:"-"`
}

type providerRuntimeRefreshResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type providerCapabilityEvidenceResult struct {
	Capability  string             `json:"capability"`
	Status      string             `json:"status"`
	Source      string             `json:"source"`
	FailureType domain.FailureType `json:"failure_type,omitempty"`
	ObservedAt  string             `json:"observed_at,omitempty"`
	ExpiresAt   string             `json:"expires_at,omitempty"`
}

type providerTextProbeFeatures struct {
	RequireReasoningEvidence bool
	StructuredOutput         bool
	Reasoning                bool
	Vision                   bool
}

type providerProbeMode uint8

const (
	providerProbeInvoke providerProbeMode = iota
	providerProbeStream
)

var providerCapabilityProbePNG = []byte{
	137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82,
	0, 0, 0, 1, 0, 0, 0, 1, 8, 4, 0, 0, 0, 181, 28, 12, 2,
	0, 0, 0, 11, 73, 68, 65, 84, 120, 218, 99, 100, 248, 15, 0, 1,
	5, 1, 1, 39, 24, 227, 102, 0, 0, 0, 0, 73, 69, 78, 68, 174,
	66, 96, 130,
}

func (s *Server) handleV2ProviderModelDiscovery(w http.ResponseWriter, r *http.Request) {
	var input providerModelDiscoveryInput
	if err := decodeMgmtBody(r, &input); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if input.Type == "" {
		input.Type = domain.ProviderCustom
	}
	switch input.Type {
	case domain.ProviderCliproxyCodex:
		planType := ""
		source := "built-in-pre-auth"
		models := domain.CodexModelNames()
		if account, ok := domain.OAuthAccountFromRef(strings.TrimSpace(input.CredentialRef)); ok && s.deps.OAuth != nil {
			if status, active := s.deps.OAuth.Account(account); active {
				planType = status.PlanType
				// Prefer the live account-scoped catalog so newly published
				// models appear immediately; the static plan table remains the
				// pre-auth and degraded fallback.
				discoveryCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
				discovered, err := s.deps.OAuth.DiscoverModelsWithVersion(discoveryCtx, account, s.currentConfig().EffectiveCodexClientVersion())
				cancel()
				if err == nil {
					source = "oauth-catalog"
					models = make([]string, 0, len(discovered))
					for _, model := range discovered {
						models = append(models, model.Slug)
					}
				} else {
					source = "oauth-plan"
					models = domain.CodexModelNamesForPlan(planType)
				}
			}
		}
		writeMgmtJSON(w, http.StatusOK, map[string]any{
			"models": models, "source": source, "plan_type": planType,
		})
		return
	case domain.ProviderCustom:
	default:
		writeMgmtErr(w, http.StatusBadRequest, "unsupported provider type")
		return
	}

	if !domain.ValidProtocol(input.Protocol) {
		writeMgmtErr(w, http.StatusBadRequest, "a valid protocol is required")
		return
	}
	secret := strings.TrimSpace(input.Secret)
	if secret == "" && strings.TrimSpace(input.CredentialRef) != "" {
		if s.deps.Resolver == nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "credential resolver not configured")
			return
		}
		resolved, err := s.deps.Resolver.Resolve(strings.TrimSpace(input.CredentialRef))
		if err != nil {
			writeMgmtErr(w, http.StatusUnprocessableEntity, "credential unavailable")
			return
		}
		secret = resolved
	}
	if secret == "" {
		writeMgmtErr(w, http.StatusUnprocessableEntity, "API key/token or credential reference is required")
		return
	}

	client, err := s.nativeProviderClient(input.CredentialRef, input.BaseURL, "", input.Protocol, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	models, err := discoverProviderModels(r.Context(), client, input.BaseURL, input.Protocol, secret)
	if err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"models": models, "source": "upstream"})
}

// handleV2ProviderProtocolDiscovery discovers a shared catalog and then sends
// a bounded capability probe only for each explicitly configured
// model/protocol pair. Catalog membership is never treated as proof of
// protocol or feature support. Fresh observations are reused by exact
// Provider, endpoint, upstream model and protocol key so reopening an editor
// does not repeatedly issue potentially billable requests.
func (s *Server) handleV2ProviderProtocolDiscovery(w http.ResponseWriter, r *http.Request) {
	var input providerProtocolDiscoveryInput
	if err := decodeMgmtBody(r, &input); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if input.Type == "" {
		input.Type = domain.ProviderCustom
	}
	if input.Type != domain.ProviderCustom {
		writeMgmtErr(w, http.StatusBadRequest, "protocol probing is available for API endpoint providers")
		return
	}
	if _, err := providerModelsURL(input.BaseURL, domain.ProtocolOpenAIChat); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Protocols) == 0 {
		for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic, domain.ProtocolOpenAIImages} {
			input.Protocols = append(input.Protocols, providerProtocolProbeInput{Protocol: protocol})
		}
	}
	seenProtocols := map[domain.Protocol]bool{}
	for i := range input.Protocols {
		probe := &input.Protocols[i]
		if !domain.ValidProtocol(probe.Protocol) || seenProtocols[probe.Protocol] {
			writeMgmtErr(w, http.StatusBadRequest, "each protocol must be valid and unique")
			return
		}
		seenProtocols[probe.Protocol] = true
		hasBaseOverride := strings.TrimSpace(probe.BaseURL) != ""
		if strings.TrimSpace(probe.BaseURL) == "" {
			probe.BaseURL = input.BaseURL
		}
		if strings.TrimSpace(probe.FullURL) != "" {
			if hasBaseOverride {
				writeMgmtErr(w, http.StatusBadRequest, "protocol base_url and full_url are mutually exclusive")
				return
			}
			if _, err := providerProtocolEndpointURL(probe.BaseURL, probe.FullURL, probe.Protocol); err != nil {
				writeMgmtErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if strings.TrimSpace(probe.Secret) == "" && strings.TrimSpace(probe.CredentialRef) == "" {
			probe.Secret, probe.CredentialRef = input.Secret, input.CredentialRef
		}
	}

	client := providerProbeHTTPClient(20 * time.Second)
	catalogs := make([][]string, len(input.Protocols))
	catalogErrors := make([]string, len(input.Protocols))
	allModels := []string{}
	modelSeen := map[string]bool{}
	var catalogWG sync.WaitGroup
	for i := range input.Protocols {
		catalogWG.Add(1)
		go func(index int) {
			defer catalogWG.Done()
			probe := input.Protocols[index]
			secret, err := s.resolveProviderDiscoverySecret(probe.Secret, probe.CredentialRef)
			if err != nil {
				catalogErrors[index] = err.Error()
				return
			}
			catalogBaseURL := probe.BaseURL
			if strings.TrimSpace(probe.FullURL) != "" {
				// A full request URL has no well-defined sibling /models endpoint.
				// Discovery stays on the Provider Base URL; explicitly configured
				// models are still verified against the exact Full URL below.
				catalogBaseURL = input.BaseURL
			}
			boundClient, err := s.nativeProviderClient(probe.CredentialRef, probe.BaseURL, probe.FullURL, probe.Protocol, client)
			if err != nil {
				catalogErrors[index] = err.Error()
				return
			}
			models, err := discoverProviderModels(r.Context(), boundClient, catalogBaseURL, probe.Protocol, secret)
			if err != nil {
				catalogErrors[index] = err.Error()
				return
			}
			catalogs[index] = models
		}(i)
	}
	catalogWG.Wait()
	for i, probe := range input.Protocols {
		for _, model := range append(append([]string{}, catalogs[i]...), probe.Models...) {
			model = strings.TrimSpace(model)
			if model != "" && !modelSeen[model] {
				modelSeen[model] = true
				allModels = append(allModels, model)
			}
		}
	}
	catalogTruncated := len(allModels) > maxDiscoveredModels
	if catalogTruncated {
		allModels = allModels[:maxDiscoveredModels]
	}
	results := make([]providerProtocolProbeResult, len(input.Protocols))
	var probeWG sync.WaitGroup
	for i := range input.Protocols {
		probeWG.Add(1)
		go func(index int) {
			defer probeWG.Done()
			probe := input.Protocols[index]
			endpoint, _ := providerProtocolEndpointURL(probe.BaseURL, probe.FullURL, probe.Protocol)
			result := providerProtocolProbeResult{
				Protocol: probe.Protocol, BaseURL: probe.BaseURL, FullURL: probe.FullURL, Endpoint: endpoint,
				Status: "unconfigured", Models: []string{}, ModelResults: []providerProtocolModelProbeResult{},
				CatalogError: catalogErrors[index],
			}
			secret, err := s.resolveProviderDiscoverySecret(probe.Secret, probe.CredentialRef)
			if err != nil {
				result.Status, result.Error = "error", err.Error()
				results[index] = result
				return
			}
			models := uniqueNonEmptyStrings(probe.Models)
			if len(models) == 0 {
				result.Error = "select at least one model to verify this protocol endpoint"
				results[index] = result
				return
			}
			if len(models) > maxProtocolProbeModels {
				models = models[:maxProtocolProbeModels]
				result.ProbeTruncated = true
			}
			boundClient, bindErr := s.nativeProviderClient(probe.CredentialRef, probe.BaseURL, probe.FullURL, probe.Protocol, client)
			if bindErr != nil {
				result.Status = "error"
				result.Error = bindErr.Error()
				results[index] = result
				return
			}
			result.Models, result.ModelResults = s.probeProviderProtocolCapabilities(
				r.Context(), boundClient, input.ProviderID, input.Type, probe.BaseURL, probe.FullURL,
				probe.Protocol, secret, models, input.Refresh, probe.CredentialRef)
			failed := len(result.ModelResults) - len(result.Models)
			switch {
			case failed == 0:
				result.Status = "verified"
			case len(result.Models) > 0:
				result.Status = "partial"
				result.Error = fmt.Sprintf("%d/%d configured models failed verification", failed, len(result.ModelResults))
			default:
				result.Status = "failed"
				result.Error = fmt.Sprintf("all %d configured models failed verification", len(result.ModelResults))
			}
			results[index] = result
		}(i)
	}
	probeWG.Wait()
	runtimeRefresh := providerRuntimeRefreshResult{Status: "not_needed"}
	evidenceChanged := false
	evidenceStoreFailed := false
	for _, protocol := range results {
		for _, model := range protocol.ModelResults {
			evidenceChanged = evidenceChanged || model.EvidenceChanged
			evidenceStoreFailed = evidenceStoreFailed || model.EvidenceStoreError
		}
	}
	if evidenceStoreFailed {
		runtimeRefresh.Status = "failed"
		runtimeRefresh.Error = "capability evidence persistence failed"
	} else if evidenceChanged {
		if !providerDiscoveryEvidenceAppliesToActiveConfig(s.currentConfig(), input, results) {
			runtimeRefresh.Status = "not_applicable"
		} else if message, ok := s.refreshRuntime(); ok {
			runtimeRefresh.Status = "refreshed"
		} else {
			runtimeRefresh.Status = "failed"
			runtimeRefresh.Error = message
		}
	}
	status := http.StatusOK
	if runtimeRefresh.Status == "failed" {
		status = http.StatusInternalServerError
	}
	writeMgmtJSON(w, status, map[string]any{
		"models": allModels, "protocols": results, "source": "real-probe",
		"truncated": catalogTruncated, "runtime_refresh": runtimeRefresh,
	})
}

func providerDiscoveryEvidenceAppliesToActiveConfig(active *config.File, input providerProtocolDiscoveryInput, results []providerProtocolProbeResult) bool {
	providerID := strings.TrimSpace(input.ProviderID)
	if active == nil || providerID == "" {
		return false
	}
	var provider *domain.Provider
	for i := range active.Providers {
		if active.Providers[i].ID == providerID {
			provider = &active.Providers[i]
			break
		}
	}
	if provider == nil {
		return false
	}
	for _, protocolResult := range results {
		probeFingerprint := providerCapabilityEndpointFingerprint(input.Type, protocolResult.Endpoint)
		for _, modelResult := range protocolResult.ModelResults {
			if !modelResult.EvidenceChanged {
				continue
			}
			for connectionIndex := range active.Connections {
				connection := &active.Connections[connectionIndex]
				if connection.ProviderID != providerID {
					continue
				}
				baseURL := provider.BaseURL
				if connection.BaseURL != "" {
					baseURL = connection.BaseURL
				}
				endpoint, err := providerProtocolEndpointURL(baseURL, connection.FullURL, protocolResult.Protocol)
				if err != nil || providerCapabilityEndpointFingerprint(provider.Type, endpoint) != probeFingerprint {
					continue
				}
				for modelIndex := range connection.Models {
					model := &connection.Models[modelIndex]
					if model.UpstreamModel != modelResult.Model {
						continue
					}
					for _, protocol := range model.Protocols {
						if protocol == protocolResult.Protocol {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (s *Server) resolveProviderDiscoverySecret(secret, credentialRef string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret != "" {
		return secret, nil
	}
	credentialRef = strings.TrimSpace(credentialRef)
	if credentialRef == "" {
		return "", fmt.Errorf("API key/token or credential reference is required")
	}
	if s.deps.Resolver == nil {
		return "", fmt.Errorf("credential resolver not configured")
	}
	resolved, err := s.deps.Resolver.Resolve(credentialRef)
	if err != nil {
		return "", fmt.Errorf("credential unavailable")
	}
	return resolved, nil
}

func providerProbeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) > 0 && !strings.EqualFold(next.URL.Host, via[0].URL.Host) {
			return fmt.Errorf("upstream endpoint redirected to a different host")
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many upstream redirects")
		}
		return nil
	}}
}

func providerProtocolEndpointURL(rawBase, rawFull string, protocol domain.Protocol) (string, error) {
	return domain.ProviderProtocolEndpointURL(rawBase, rawFull, protocol)
}

func providerCapabilityEndpointFingerprint(providerType domain.ProviderType, endpoint string) string {
	return domain.ProviderCapabilityEndpointFingerprint(providerType, endpoint)
}

func providerCapabilityStoreProviderID(providerID, endpointFingerprint string) string {
	return domain.ProviderCapabilityStoreProviderID(providerID, endpointFingerprint)
}

func providerProtocolCapabilities(protocol domain.Protocol) []string {
	return domain.ProbeCapabilitiesForProtocol(protocol)
}

func setProviderCapability(capabilities *domain.Capabilities, capability string, supported bool) {
	domain.SetBooleanCapability(capabilities, capability, supported)
}

func providerCapabilityEvidenceFromStore(item *controlstore.ProviderCapabilityEvidence) providerCapabilityEvidenceResult {
	status := "unsupported"
	if item.Supported {
		status = "supported"
	}
	return providerCapabilityEvidenceResult{
		Capability: item.Capability, Status: status, Source: "cache", FailureType: item.FailureType,
		ObservedAt: item.ObservedAt.UTC().Format(time.RFC3339Nano), ExpiresAt: item.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func providerCapabilityEvidenceFromProbe(capability string, supported bool, failure *domain.Failure, observedAt, expiresAt time.Time) providerCapabilityEvidenceResult {
	status := "unsupported"
	if supported {
		status = "supported"
	}
	result := providerCapabilityEvidenceResult{
		Capability: capability, Status: status, Source: "probe",
		ObservedAt: observedAt.UTC().Format(time.RFC3339Nano), ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	}
	if failure != nil {
		result.FailureType = failure.Type
	}
	return result
}

func providerCapabilityUnknownEvidence(capability string, failure *domain.Failure) providerCapabilityEvidenceResult {
	result := providerCapabilityEvidenceResult{Capability: capability, Status: "unknown", Source: "probe"}
	if failure != nil {
		result.FailureType = failure.Type
	}
	return result
}

func appendProviderNonProbeCapabilityEvidence(result *providerProtocolModelProbeResult, protocol domain.Protocol) {
	priority := providerCapabilityEvidenceResult{
		Capability: domain.CapabilityPriority, Status: "unknown", Source: "authoritative-config-required",
	}
	if protocol != domain.ProtocolOpenAIChat && protocol != domain.ProtocolOpenAIResponses {
		priority.Status, priority.Source = "unsupported", "protocol-contract"
	}
	setProviderCapabilityEvidenceResult(result, priority)
	if protocol != domain.ProtocolOpenAIImages {
		for _, capability := range []string{providerCapabilityContextWindow, providerCapabilityMaxOutput} {
			setProviderCapabilityEvidenceResult(result, providerCapabilityEvidenceResult{
				Capability: capability, Status: "unknown", Source: "authoritative-config-required",
			})
		}
	}
}

func setProviderCapabilityEvidenceResult(result *providerProtocolModelProbeResult, evidence providerCapabilityEvidenceResult) {
	for index := range result.CapabilityEvidence {
		if result.CapabilityEvidence[index].Capability == evidence.Capability {
			result.CapabilityEvidence[index] = evidence
			return
		}
	}
	result.CapabilityEvidence = append(result.CapabilityEvidence, evidence)
}

func deterministicProviderCapabilityFailure(failure *domain.Failure) bool {
	if failure == nil {
		return false
	}
	switch failure.Type {
	case domain.FailUnsupportedParam, domain.FailProtocolConversion:
		return true
	default:
		return false
	}
}

func (s *Server) readProviderCapabilityEvidence(providerID, endpointFingerprint, model string, protocol domain.Protocol, capabilities []string, now time.Time) (map[string]*controlstore.ProviderCapabilityEvidence, error) {
	items := make(map[string]*controlstore.ProviderCapabilityEvidence, len(capabilities))
	if s.deps.Control == nil {
		return items, nil
	}
	for _, capability := range capabilities {
		item, err := s.deps.Control.ProviderCapabilityEvidence(providerID, endpointFingerprint, model, protocol, capability, now)
		if err != nil {
			return items, err
		}
		if item != nil {
			items[capability] = item
		}
	}
	return items, nil
}

func (s *Server) putProviderCapabilityEvidence(providerID, endpointFingerprint, model string, protocol domain.Protocol, capability string, supported bool, failure *domain.Failure, observedAt time.Time) (*controlstore.ProviderCapabilityEvidence, bool, error) {
	ttl := providerCapabilityPositiveTTL
	failureType := domain.FailNone
	if !supported {
		ttl = providerCapabilityNegativeTTL
		if failure != nil {
			failureType = failure.Type
		}
	}
	item := &controlstore.ProviderCapabilityEvidence{
		ProviderID: providerID, EndpointFingerprint: endpointFingerprint, UpstreamModel: model, Protocol: protocol,
		Capability: capability, Supported: supported, FailureType: failureType, ObservedAt: observedAt, ExpiresAt: observedAt.Add(ttl),
	}
	if s.deps.Control == nil {
		return item, false, nil
	}
	previous, err := s.deps.Control.ProviderCapabilityEvidence(
		providerID, endpointFingerprint, model, protocol, capability, observedAt)
	if err != nil {
		return item, false, err
	}
	changed := previous == nil || previous.Supported != supported
	if err := s.deps.Control.PutProviderCapabilityEvidence(*item); err != nil {
		return item, false, err
	}
	return item, changed, nil
}

func providerCapabilityCacheComplete(protocol domain.Protocol, cached map[string]*controlstore.ProviderCapabilityEvidence) bool {
	for _, capability := range providerProtocolCapabilities(protocol) {
		if cached[capability] == nil {
			return false
		}
	}
	return true
}

func providerCapabilityProtocolVerified(protocol domain.Protocol, capabilities domain.Capabilities) bool {
	if protocol == domain.ProtocolOpenAIImages {
		return capabilities.Images
	}
	return capabilities.Text
}

func (s *Server) probeProviderProtocolCapabilities(ctx context.Context, client *http.Client, providerID string, providerType domain.ProviderType, baseURL, fullURL string, protocol domain.Protocol, secret string, models []string, refresh bool, nativeRefs ...string) ([]string, []providerProtocolModelProbeResult) {
	nativeRef := ""
	if len(nativeRefs) > 0 {
		nativeRef = nativeRefs[0]
	}
	endpoint, _ := providerProtocolEndpointURL(baseURL, fullURL, protocol)
	endpointFingerprint := providerCapabilityEndpointFingerprint(providerType, endpoint)
	storeProviderID := providerCapabilityStoreProviderID(providerID, endpointFingerprint)
	type outcome struct {
		model  string
		result providerProtocolModelProbeResult
	}
	jobs := make(chan string)
	outcomes := make(chan outcome, len(models))
	workers := min(6, len(models))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for model := range jobs {
				caller := egress.NewCaller(egress.Base{
					HTTP: client, Endpoint: &domain.Endpoint{ID: "protocol-capability-probe", BaseURL: baseURL, FullURL: fullURL, Protocol: protocol},
					Secret: secret, Model: model, CredRef: nativeRef,
				}, protocol)
				probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				result := s.probeProviderModelCapabilities(probeCtx, caller, storeProviderID, endpointFingerprint, protocol, model, refresh)
				cancel()
				outcomes <- outcome{model: model, result: result}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, model := range models {
			select {
			case jobs <- model:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(outcomes)
	}()
	resultsByModel := make(map[string]providerProtocolModelProbeResult, len(models))
	for current := range outcomes {
		resultsByModel[current.model] = current.result
	}
	verified := []string{}
	results := make([]providerProtocolModelProbeResult, 0, len(models))
	for _, model := range models {
		result, ok := resultsByModel[model]
		if !ok {
			result = providerProtocolModelProbeResult{Model: model, Status: "failed", Error: "verification was cancelled"}
		}
		if result.Status == "verified" {
			verified = append(verified, model)
		}
		results = append(results, result)
	}
	return verified, results
}

func (s *Server) probeProviderModelCapabilities(ctx context.Context, caller egress.Caller, providerID, endpointFingerprint string, protocol domain.Protocol, model string, refresh bool) providerProtocolModelProbeResult {
	result := providerProtocolModelProbeResult{Model: model, Capabilities: domain.Capabilities{}}
	now := time.Now().UTC()
	required := providerProtocolCapabilities(protocol)
	cached := map[string]*controlstore.ProviderCapabilityEvidence{}
	if !refresh {
		var err error
		cached, err = s.readProviderCapabilityEvidence(providerID, endpointFingerprint, model, protocol, required, now)
		if err != nil {
			result.EvidenceError = err.Error()
			result.EvidenceStoreError = true
			result.Status = "failed"
			result.Error = "capability evidence store is unavailable"
			return result
		}
	}
	if providerCapabilityCacheComplete(protocol, cached) {
		result.CacheHit = true
		for _, capability := range required {
			item := cached[capability]
			setProviderCapability(&result.Capabilities, capability, item.Supported)
			result.CapabilityEvidence = append(result.CapabilityEvidence, providerCapabilityEvidenceFromStore(item))
		}
		appendProviderNonProbeCapabilityEvidence(&result, protocol)
		if providerCapabilityProtocolVerified(protocol, result.Capabilities) {
			result.Status = "verified"
		} else {
			result.Status = "failed"
			result.Error = "cached capability evidence marks this model unavailable on the endpoint"
		}
		return result
	}

	resolved := map[string]bool{}
	reported := map[string]bool{}
	recordSource := func(capability string, supported bool, failure *domain.Failure, source string) {
		if result.EvidenceStoreError {
			return
		}
		item, changed, err := s.putProviderCapabilityEvidence(providerID, endpointFingerprint, model, protocol, capability, supported, failure, now)
		if err != nil && result.EvidenceError == "" {
			result.EvidenceError = err.Error()
		}
		result.EvidenceStoreError = result.EvidenceStoreError || err != nil
		result.EvidenceChanged = result.EvidenceChanged || changed
		setProviderCapability(&result.Capabilities, capability, supported)
		evidence := providerCapabilityEvidenceFromProbe(capability, supported, failure, item.ObservedAt, item.ExpiresAt)
		evidence.Source = source
		setProviderCapabilityEvidenceResult(&result, evidence)
		if err == nil {
			resolved[capability] = true
		}
		reported[capability] = true
	}
	record := func(capability string, supported bool, failure *domain.Failure) {
		recordSource(capability, supported, failure, "probe")
	}
	useCached := func(capability string) bool {
		item := cached[capability]
		if item == nil {
			return false
		}
		setProviderCapability(&result.Capabilities, capability, item.Supported)
		setProviderCapabilityEvidenceResult(&result, providerCapabilityEvidenceFromStore(item))
		resolved[capability] = true
		reported[capability] = true
		return true
	}
	reportUnknown := func(capability string, failure *domain.Failure) {
		if resolved[capability] || reported[capability] {
			return
		}
		setProviderCapabilityEvidenceResult(&result, providerCapabilityUnknownEvidence(capability, failure))
		reported[capability] = true
	}
	noteInconclusive := func(message string) {
		message = strings.TrimSpace(message)
		if message == "" {
			return
		}
		if result.EvidenceError == "" {
			result.EvidenceError = message
			return
		}
		result.EvidenceError += "; " + message
	}
	prerequisiteFailure := func(capability string) *domain.Failure {
		return &domain.Failure{
			Type:    domain.FailUnsupportedParam,
			Message: fmt.Sprintf("%s requires native text support", capability),
		}
	}
	recordMissingPrerequisites := func(capabilities ...string) {
		for _, capability := range capabilities {
			if result.EvidenceStoreError {
				return
			}
			if !resolved[capability] {
				recordSource(capability, false, prerequisiteFailure(capability), "capability-prerequisite")
			}
		}
	}
	reportMissingUnknown := func(failure *domain.Failure, capabilities ...string) {
		for _, capability := range capabilities {
			reportUnknown(capability, failure)
		}
	}
	for _, capability := range required {
		useCached(capability)
	}

	switch protocol {
	case domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses:
		probeProviderTextCapabilitySet(ctx, caller, protocol, model, &result, resolved, record, recordMissingPrerequisites, reportMissingUnknown, noteInconclusive)
	case domain.ProtocolAnthropic:
		probeProviderTextCapabilitySet(ctx, caller, protocol, model, &result, resolved, record, recordMissingPrerequisites, reportMissingUnknown, noteInconclusive)
	case domain.ProtocolOpenAIImages:
		if !resolved[providerCapabilityImages] {
			result.ProbeRequests++
			failure := probeProviderProtocolModel(ctx, caller, protocol, model)
			if failure == nil {
				record(providerCapabilityImages, true, nil)
			} else {
				if deterministicProviderCapabilityFailure(failure) {
					record(providerCapabilityImages, false, failure)
				} else {
					reportUnknown(providerCapabilityImages, failure)
					noteInconclusive("image capability probe was inconclusive: " + providerProbeFailureMessage(failure))
				}
				result.Error = providerProbeFailureMessage(failure)
			}
		}
	}
	appendProviderNonProbeCapabilityEvidence(&result, protocol)
	if result.EvidenceStoreError {
		result.Status = "failed"
		result.Error = "capability evidence store is unavailable"
		return result
	}
	if providerCapabilityProtocolVerified(protocol, result.Capabilities) {
		result.Status = "verified"
	} else {
		result.Status = "failed"
		if result.Error == "" {
			result.Error = "model capability verification failed"
		}
	}
	return result
}

func probeProviderTextCapabilitySet(
	ctx context.Context,
	caller egress.Caller,
	protocol domain.Protocol,
	model string,
	result *providerProtocolModelProbeResult,
	resolved map[string]bool,
	record func(string, bool, *domain.Failure),
	recordMissingPrerequisites func(...string),
	reportMissingUnknown func(*domain.Failure, ...string),
	noteInconclusive func(string),
) {
	optionalCapabilities := []string{providerCapabilityReasoning, providerCapabilityVision}
	features := providerTextProbeFeatures{
		Reasoning: !resolved[providerCapabilityReasoning],
		Vision:    !resolved[providerCapabilityVision],
	}
	optionalCapabilities = append([]string{providerCapabilityStructuredOutput}, optionalCapabilities...)
	features.StructuredOutput = !resolved[providerCapabilityStructuredOutput]
	toolCapabilities := []string{providerCapabilityTools, providerCapabilityParallelTools}
	textVerified := resolved[providerCapabilityText] && result.Capabilities.Text
	if resolved[providerCapabilityText] && !result.Capabilities.Text {
		recordMissingPrerequisites(append([]string{providerCapabilityStream}, append(optionalCapabilities, toolCapabilities...)...)...)
		return
	}

	hasOptionalProbe := features.StructuredOutput || features.Reasoning || features.Vision
	inconclusive := false
	mode := providerProbeStream
	if resolved[providerCapabilityStream] && !result.Capabilities.Stream {
		mode = providerProbeInvoke
	}

	recordSuccessfulProbe := func(probeMode providerProbeMode, probedFeatures providerTextProbeFeatures) {
		if !resolved[providerCapabilityText] {
			record(providerCapabilityText, true, nil)
		}
		if probeMode == providerProbeStream && !resolved[providerCapabilityStream] {
			record(providerCapabilityStream, true, nil)
		}
		if probedFeatures.StructuredOutput && !resolved[providerCapabilityStructuredOutput] {
			record(providerCapabilityStructuredOutput, true, nil)
		}
		if probedFeatures.Reasoning && !resolved[providerCapabilityReasoning] {
			record(providerCapabilityReasoning, true, nil)
		}
		if probedFeatures.Vision && !resolved[providerCapabilityVision] {
			record(providerCapabilityVision, true, nil)
		}
		textVerified = true
	}
	reportProbeInconclusive := func(label string, failure *domain.Failure) {
		reportMissingUnknown(failure, providerCapabilityText, providerCapabilityStream)
		for _, capability := range optionalCapabilities {
			reportMissingUnknown(failure, capability)
		}
		reportMissingUnknown(failure, toolCapabilities...)
		noteInconclusive(label + ": " + providerProbeFailureMessage(failure))
		inconclusive = true
	}
	probe := func(probeMode providerProbeMode, probedFeatures providerTextProbeFeatures) *domain.Failure {
		failure, requests := probeProviderTextFeatures(ctx, caller, protocol, model, probedFeatures, probeMode)
		result.ProbeRequests += requests
		return failure
	}
	probeFeaturesSeparately := func(probeMode providerProbeMode) {
		for _, feature := range []struct {
			capability string
			enabled    bool
			features   providerTextProbeFeatures
		}{
			{providerCapabilityStructuredOutput, features.StructuredOutput, providerTextProbeFeatures{StructuredOutput: true}},
			{providerCapabilityReasoning, features.Reasoning, providerTextProbeFeatures{Reasoning: true}},
			{providerCapabilityVision, features.Vision, providerTextProbeFeatures{Vision: true}},
		} {
			if result.EvidenceStoreError {
				reportMissingUnknown(nil, append(optionalCapabilities, toolCapabilities...)...)
				return
			}
			if !feature.enabled || resolved[feature.capability] {
				continue
			}
			featureFailure := probe(probeMode, feature.features)
			switch {
			case featureFailure == nil:
				record(feature.capability, true, nil)
			case deterministicProviderCapabilityFailure(featureFailure):
				record(feature.capability, false, featureFailure)
			default:
				reportMissingUnknown(featureFailure, feature.capability)
				noteInconclusive(feature.capability + " probe was inconclusive: " + providerProbeFailureMessage(featureFailure))
				inconclusive = true
			}
		}
	}

	needCombinedProbe := !resolved[providerCapabilityText] ||
		(mode == providerProbeStream && !resolved[providerCapabilityStream]) || hasOptionalProbe
	if needCombinedProbe {
		failure := probe(mode, features)
		if failure == nil {
			recordSuccessfulProbe(mode, features)
		} else if !deterministicProviderCapabilityFailure(failure) {
			reportProbeInconclusive("combined text capability probe was inconclusive", failure)
		} else {
			baseVerifiedForMode := textVerified && (mode == providerProbeInvoke ||
				(resolved[providerCapabilityStream] && result.Capabilities.Stream))
			if hasOptionalProbe && !baseVerifiedForMode {
				baseFailure := probe(mode, providerTextProbeFeatures{})
				switch {
				case baseFailure == nil:
					recordSuccessfulProbe(mode, providerTextProbeFeatures{})
					baseVerifiedForMode = true
				case !deterministicProviderCapabilityFailure(baseFailure):
					reportProbeInconclusive("base text capability probe was inconclusive", baseFailure)
				default:
					failure = baseFailure
				}
			}

			if baseVerifiedForMode {
				probeFeaturesSeparately(mode)
			} else if !inconclusive && mode == providerProbeStream {
				if !resolved[providerCapabilityStream] {
					record(providerCapabilityStream, false, failure)
				}
				mode = providerProbeInvoke
				invokeFailure := probe(mode, features)
				switch {
				case invokeFailure == nil:
					recordSuccessfulProbe(mode, features)
				case !deterministicProviderCapabilityFailure(invokeFailure):
					reportProbeInconclusive("non-stream text capability probe was inconclusive", invokeFailure)
				default:
					invokeBaseVerified := textVerified
					if hasOptionalProbe && !invokeBaseVerified {
						baseFailure := probe(mode, providerTextProbeFeatures{})
						switch {
						case baseFailure == nil:
							recordSuccessfulProbe(mode, providerTextProbeFeatures{})
							invokeBaseVerified = true
						case deterministicProviderCapabilityFailure(baseFailure):
							if !resolved[providerCapabilityText] {
								record(providerCapabilityText, false, baseFailure)
							}
							result.Error = providerProbeFailureMessage(baseFailure)
						default:
							reportProbeInconclusive("base non-stream text capability probe was inconclusive", baseFailure)
						}
					} else if !hasOptionalProbe && !invokeBaseVerified {
						record(providerCapabilityText, false, invokeFailure)
						result.Error = providerProbeFailureMessage(invokeFailure)
					}
					if invokeBaseVerified {
						probeFeaturesSeparately(mode)
					}
				}
			} else if !inconclusive && !baseVerifiedForMode {
				if !resolved[providerCapabilityText] {
					record(providerCapabilityText, false, failure)
				}
				result.Error = providerProbeFailureMessage(failure)
			}
		}
	}
	textVerified = resolved[providerCapabilityText] && result.Capabilities.Text
	if !textVerified {
		if resolved[providerCapabilityText] && !result.Capabilities.Text {
			recordMissingPrerequisites(append(optionalCapabilities, toolCapabilities...)...)
		}
		reportMissingUnknown(nil, toolCapabilities...)
		return
	}
	if inconclusive || result.EvidenceStoreError {
		reportMissingUnknown(nil, toolCapabilities...)
		return
	}
	if resolved[providerCapabilityStream] && result.Capabilities.Stream {
		mode = providerProbeStream
	} else {
		mode = providerProbeInvoke
	}
	probeProviderToolCapabilitySet(ctx, caller, protocol, model, mode, result, resolved, record, recordMissingPrerequisites, reportMissingUnknown, noteInconclusive)
}

func probeProviderToolCapabilitySet(
	ctx context.Context,
	caller egress.Caller,
	protocol domain.Protocol,
	model string,
	mode providerProbeMode,
	result *providerProtocolModelProbeResult,
	resolved map[string]bool,
	record func(string, bool, *domain.Failure),
	recordMissingPrerequisites func(...string),
	reportMissingUnknown func(*domain.Failure, ...string),
	noteInconclusive func(string),
) {
	var parallelFailure *domain.Failure
	if resolved[providerCapabilityTools] && !result.Capabilities.Tools {
		recordMissingPrerequisites(providerCapabilityParallelTools)
		return
	}
	if resolved[providerCapabilityTools] && resolved[providerCapabilityParallelTools] {
		return
	}
	if !resolved[providerCapabilityParallelTools] {
		result.ProbeRequests++
		failure := probeProviderTools(ctx, caller, protocol, model, true, mode)
		if failure == nil {
			if !resolved[providerCapabilityTools] {
				record(providerCapabilityTools, true, nil)
			}
			record(providerCapabilityParallelTools, true, nil)
			return
		}
		if !deterministicProviderCapabilityFailure(failure) {
			reportMissingUnknown(failure, providerCapabilityTools, providerCapabilityParallelTools)
			noteInconclusive("parallel tool capability probe was inconclusive: " + providerProbeFailureMessage(failure))
			return
		}
		parallelFailure = failure
		if resolved[providerCapabilityTools] && result.Capabilities.Tools {
			record(providerCapabilityParallelTools, false, failure)
			return
		}
	}
	if resolved[providerCapabilityTools] {
		return
	}
	result.ProbeRequests++
	failure := probeProviderTools(ctx, caller, protocol, model, false, mode)
	if failure == nil {
		record(providerCapabilityTools, true, nil)
		if !resolved[providerCapabilityParallelTools] && parallelFailure != nil {
			record(providerCapabilityParallelTools, false, parallelFailure)
		}
		return
	}
	if deterministicProviderCapabilityFailure(failure) {
		record(providerCapabilityTools, false, failure)
		if !resolved[providerCapabilityParallelTools] {
			record(providerCapabilityParallelTools, false, failure)
		}
		return
	}
	reportMissingUnknown(failure, providerCapabilityTools, providerCapabilityParallelTools)
	noteInconclusive("tool capability probe was inconclusive: " + providerProbeFailureMessage(failure))
}

func probeProviderTextFeatures(ctx context.Context, caller egress.Caller, protocol domain.Protocol, model string, features providerTextProbeFeatures, mode providerProbeMode) (*domain.Failure, int) {
	prompt := "Reply with OK."
	contract := &domain.RequestContract{
		Protocol: protocol, Origin: protocol, Model: model, MaxTokens: providerTextProbeMaxTokens,
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: prompt}}}},
	}
	if features.StructuredOutput {
		strict := true
		contract.ResponseFormat = &domain.ResponseFormat{
			Name:   "ferry_capability_probe",
			Strict: &strict,
			Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean","const":true}},"required":["ok"],"additionalProperties":false}`),
		}
		contract.Messages[0].Blocks[0].Text = `Return {"ok":true}.`
	}
	if features.Reasoning {
		if protocol == domain.ProtocolAnthropic {
			contract.MaxTokens = 2048
			contract.Reasoning = &domain.ReasoningSpec{BudgetTokens: 1024}
		} else {
			contract.Reasoning = &domain.ReasoningSpec{Effort: "low"}
		}
	}
	if features.Vision {
		contract.Messages[0].Blocks = append(contract.Messages[0].Blocks, domain.Block{
			Kind: domain.BlockImage, ImageMIME: "image/png", ImageData: providerCapabilityProbePNG,
		})
	}
	if mode == providerProbeInvoke {
		response, failure := caller.Invoke(ctx, contract)
		if failure != nil {
			return failure, 1
		}
		return validateProviderTextProbeResponse(response, features), 1
	}
	failure := streamProviderTextFeatures(ctx, caller, contract, features)
	if !emptyProviderTextProbeStream(failure) || contract.MaxTokens >= providerTextProbeRetryTokens {
		return failure, 1
	}
	// Some reasoning models consume a small output allowance internally and
	// then emit a clean terminal event without any canonical content. Retry
	// that precise inconclusive result once with a still-bounded allowance;
	// every transport/provider error remains single-shot.
	contract.MaxTokens = providerTextProbeRetryTokens
	return streamProviderTextFeatures(ctx, caller, contract, features), 2
}

func emptyProviderTextProbeStream(failure *domain.Failure) bool {
	return failure != nil && failure.Type == domain.FailStreamBroken &&
		failure.Message == "upstream stream ended before any content"
}

func streamProviderTextFeatures(ctx context.Context, caller egress.Caller, contract *domain.RequestContract, features providerTextProbeFeatures) *domain.Failure {
	events, release, failure := caller.Stream(ctx, contract)
	if failure != nil {
		return failure
	}
	if release != nil {
		defer release()
	}
	var content strings.Builder
	hasContent := false
	hasReasoning := false
	for event := range events {
		if event.Usage != nil && event.Usage.ReasoningTokens > 0 {
			hasReasoning = true
		}
		switch event.Kind {
		case domain.EvTextDelta:
			content.WriteString(event.TextDelta)
			hasContent = hasContent || event.TextDelta != ""
		case domain.EvThinkingDelta:
			hasReasoning = hasReasoning || event.ThinkingDelta != ""
			hasContent = hasContent || event.ThinkingDelta != ""
		case domain.EvSignatureDelta:
			hasReasoning = hasReasoning || event.SignatureDelta != ""
			hasContent = hasContent || event.SignatureDelta != ""
		case domain.EvRedactedThinking:
			hasReasoning = hasReasoning || event.RedactedThinkingData != ""
			hasContent = hasContent || event.RedactedThinkingData != ""
		case domain.EvError:
			if event.Err != nil {
				return event.Err
			}
			return &domain.Failure{Type: domain.FailStreamBroken, Message: "upstream stream emitted an empty error event"}
		case domain.EvEnd:
			if features.RequireReasoningEvidence && !hasReasoning {
				return &domain.Failure{Type: domain.FailBadResponse, Message: "reasoning probe produced no reasoning evidence"}
			}
			if !features.StructuredOutput {
				if !hasContent {
					return &domain.Failure{Type: domain.FailStreamBroken, Message: "upstream stream ended before any content"}
				}
				return nil
			}
			return validateProviderStructuredProbeContent(content.String())
		}
	}
	if ctx.Err() != nil {
		return &domain.Failure{Type: domain.FailStreamBroken, Message: "text capability stream probe: " + ctx.Err().Error()}
	}
	return &domain.Failure{Type: domain.FailStreamBroken, Message: "text capability stream closed without a terminal event"}
}

func validateProviderTextProbeResponse(response *domain.Response, features providerTextProbeFeatures) *domain.Failure {
	if response == nil {
		return &domain.Failure{Type: domain.FailBadResponse, Message: "text capability probe returned an empty response"}
	}
	if len(response.ValidationIssues) > 0 {
		return &domain.Failure{Type: domain.FailBadResponse, Message: "text capability probe returned an invalid response: " + response.ValidationIssues[0]}
	}
	var content strings.Builder
	hasContent := false
	hasReasoning := response.Usage.ReasoningTokens > 0
	for _, block := range response.Content {
		switch block.Kind {
		case domain.BlockText:
			content.WriteString(block.Text)
			hasContent = hasContent || block.Text != ""
		case domain.BlockThinking:
			hasReasoning = hasReasoning || block.Text != "" || block.Signature != ""
			hasContent = hasContent || block.Text != "" || block.Signature != ""
		case domain.BlockRedactedThinking:
			hasReasoning = hasReasoning || block.Data != ""
			hasContent = hasContent || block.Data != ""
		}
	}
	if !hasContent {
		return &domain.Failure{Type: domain.FailBadResponse, Message: "text capability probe returned no canonical content"}
	}
	if features.RequireReasoningEvidence && !hasReasoning {
		return &domain.Failure{Type: domain.FailBadResponse, Message: "reasoning probe produced no reasoning evidence"}
	}
	if !features.StructuredOutput {
		return nil
	}
	return validateProviderStructuredProbeContent(content.String())
}

func validateProviderStructuredProbeContent(content string) *domain.Failure {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(content)), &object) != nil || len(object) != 1 {
		return &domain.Failure{Type: domain.FailUnsupportedParam, Message: "structured output probe did not return the requested JSON object"}
	}
	var ok bool
	if raw, exists := object["ok"]; !exists || json.Unmarshal(raw, &ok) != nil || !ok {
		return &domain.Failure{Type: domain.FailUnsupportedParam, Message: "structured output probe did not satisfy the requested JSON schema"}
	}
	return nil
}

func probeProviderTools(ctx context.Context, caller egress.Caller, protocol domain.Protocol, model string, parallel bool, mode providerProbeMode) *domain.Failure {
	tools := []domain.ToolDef{{
		Name: "ferry_capability_probe_one", Description: "Records a Ferry capability probe.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
	}}
	toolChoice := tools[0].Name
	var parallelTools *bool
	if parallel {
		tools = append(tools, domain.ToolDef{
			Name: "ferry_capability_probe_two", Description: "Records a second Ferry capability probe.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
		})
		toolChoice = "required"
		enabled := true
		parallelTools = &enabled
	}
	contract := &domain.RequestContract{
		Protocol: protocol, Origin: protocol, Model: model, MaxTokens: 64,
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{
			Kind: domain.BlockText, Text: "Call the provided capability probe tool with ok=true. Do not answer with plain text.",
		}}}},
		Tools: tools, ToolChoice: toolChoice, ParallelTools: parallelTools,
	}
	requiredCalls := map[string]struct{}{tools[0].Name: {}}
	if parallel {
		contract.Messages[0].Blocks[0].Text = "In one assistant turn, call both provided capability probe tools with ok=true. Do not answer with plain text."
		requiredCalls[tools[1].Name] = struct{}{}
	}
	if mode == providerProbeInvoke {
		response, failure := caller.Invoke(ctx, contract)
		if failure != nil {
			return failure
		}
		return validateProviderToolProbeResponse(response, requiredCalls, parallel)
	}
	events, release, failure := caller.Stream(ctx, contract)
	if failure != nil {
		return failure
	}
	if release != nil {
		defer release()
	}
	type toolCall struct {
		name      string
		arguments strings.Builder
		complete  bool
	}
	calls := map[int]*toolCall{}
	for event := range events {
		switch event.Kind {
		case domain.EvToolStart:
			call := calls[event.Index]
			if call == nil {
				call = &toolCall{}
				calls[event.Index] = call
			}
			call.name = event.ToolName
		case domain.EvToolDelta:
			call := calls[event.Index]
			if call == nil {
				call = &toolCall{}
				calls[event.Index] = call
			}
			call.arguments.WriteString(event.ArgsDelta)
		case domain.EvToolEnd:
			call := calls[event.Index]
			if call != nil {
				call.complete = true
			}
		case domain.EvError:
			if event.Err != nil {
				return event.Err
			}
			return &domain.Failure{Type: domain.FailStreamBroken, Message: "upstream stream emitted an empty error event"}
		case domain.EvEnd:
			completedCalls := map[string]struct{}{}
			for _, call := range calls {
				if call.complete && call.name != "" && domain.ValidJSONObject(json.RawMessage(call.arguments.String())) {
					completedCalls[call.name] = struct{}{}
				}
			}
			for name := range requiredCalls {
				if _, ok := completedCalls[name]; !ok {
					message := "tool capability probe did not return the required complete function call with JSON object arguments"
					if parallel {
						message = "parallel tool capability probe did not return both required function calls in one assistant turn"
					}
					return &domain.Failure{Type: domain.FailUnsupportedParam, Message: message}
				}
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		return &domain.Failure{Type: domain.FailStreamBroken, Message: "tool capability stream probe: " + ctx.Err().Error()}
	}
	return &domain.Failure{Type: domain.FailStreamBroken, Message: "tool capability stream closed without a terminal event"}
}

func validateProviderToolProbeResponse(response *domain.Response, requiredCalls map[string]struct{}, parallel bool) *domain.Failure {
	if response == nil {
		return &domain.Failure{Type: domain.FailBadResponse, Message: "tool capability probe returned an empty response"}
	}
	if len(response.ValidationIssues) > 0 {
		return &domain.Failure{Type: domain.FailBadResponse, Message: "tool capability probe returned an invalid response: " + response.ValidationIssues[0]}
	}
	completedCalls := map[string]struct{}{}
	for _, block := range response.Content {
		if block.Kind == domain.BlockToolUse && block.Name != "" && domain.ValidJSONObject(block.Input) {
			completedCalls[block.Name] = struct{}{}
		}
	}
	for name := range requiredCalls {
		if _, ok := completedCalls[name]; !ok {
			message := "tool capability probe did not return the required complete function call with JSON object arguments"
			if parallel {
				message = "parallel tool capability probe did not return both required function calls in one assistant turn"
			}
			return &domain.Failure{Type: domain.FailUnsupportedParam, Message: message}
		}
	}
	return nil
}

func probeProviderProtocolModels(ctx context.Context, client *http.Client, baseURL, fullURL string, protocol domain.Protocol, secret string, models []string) ([]string, []providerProtocolModelProbeResult) {
	type outcome struct {
		model   string
		failure *domain.Failure
	}
	jobs := make(chan string)
	outcomes := make(chan outcome, len(models))
	workers := min(6, len(models))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for model := range jobs {
				caller := egress.NewCaller(egress.Base{
					HTTP: client, Endpoint: &domain.Endpoint{ID: "protocol-probe", BaseURL: baseURL, FullURL: fullURL, Protocol: protocol},
					Secret: secret, Model: model,
				}, protocol)
				probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				failure := probeProviderProtocolModel(probeCtx, caller, protocol, model)
				cancel()
				outcomes <- outcome{model: model, failure: failure}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, model := range models {
			select {
			case jobs <- model:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(outcomes)
	}()
	resultsByModel := make(map[string]providerProtocolModelProbeResult, len(models))
	for current := range outcomes {
		if current.failure == nil {
			resultsByModel[current.model] = providerProtocolModelProbeResult{Model: current.model, Status: "verified"}
			continue
		}
		message := providerProbeFailureMessage(current.failure)
		if message == "" {
			message = string(current.failure.Type)
		}
		resultsByModel[current.model] = providerProtocolModelProbeResult{Model: current.model, Status: "failed", Error: message}
	}
	verified := []string{}
	results := make([]providerProtocolModelProbeResult, 0, len(models))
	for _, model := range models {
		result, ok := resultsByModel[model]
		if !ok {
			result = providerProtocolModelProbeResult{Model: model, Status: "failed", Error: "verification was cancelled"}
		}
		if result.Status == "verified" {
			verified = append(verified, model)
		}
		results = append(results, result)
	}
	return verified, results
}

// probeProviderProtocolModel proves the behavior the console declares. Text
// protocols are tested as real SSE streams and must emit at least one
// canonical content event followed by a clean terminal event; a 200 response
// with an empty/invalid stream is not native support. Images remain a native
// non-streaming protocol and are tested through Invoke.
func probeProviderProtocolModel(ctx context.Context, caller egress.Caller, protocol domain.Protocol, model string) *domain.Failure {
	contract := &domain.RequestContract{
		Protocol: protocol, Origin: protocol, Model: model, MaxTokens: providerProtocolProbeMaxTokens,
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "Reply with OK."}}}},
	}
	if protocol == domain.ProtocolOpenAIImages {
		_, failure := caller.Invoke(ctx, contract)
		return failure
	}
	events, release, failure := caller.Stream(ctx, contract)
	if failure != nil {
		return failure
	}
	if release != nil {
		defer release()
	}
	content := false
	for event := range events {
		switch event.Kind {
		case domain.EvTextDelta:
			content = content || event.TextDelta != ""
		case domain.EvThinkingDelta:
			content = content || event.ThinkingDelta != ""
		case domain.EvSignatureDelta:
			content = content || event.SignatureDelta != ""
		case domain.EvRedactedThinking:
			content = content || event.RedactedThinkingData != ""
		case domain.EvToolStart:
			content = content || event.ToolID != "" || event.ToolName != ""
		case domain.EvToolDelta:
			content = content || event.ArgsDelta != ""
		case domain.EvError:
			if event.Err != nil {
				return event.Err
			}
			return &domain.Failure{Type: domain.FailStreamBroken, Message: "upstream stream emitted an empty error event"}
		case domain.EvEnd:
			if !content {
				return &domain.Failure{Type: domain.FailStreamBroken, Message: "upstream stream ended before any content"}
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		return &domain.Failure{Type: domain.FailStreamBroken, Message: "upstream stream probe: " + ctx.Err().Error()}
	}
	return &domain.Failure{Type: domain.FailStreamBroken, Message: "upstream stream closed without a terminal event"}
}

func providerProbeFailureMessage(failure *domain.Failure) string {
	if failure == nil {
		return ""
	}
	message := strings.TrimSpace(failure.Message)
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(message), &envelope) == nil {
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			message = strings.TrimSpace(envelope.Error.Message)
		} else if strings.TrimSpace(envelope.Message) != "" {
			message = strings.TrimSpace(envelope.Message)
		}
	}
	if message == "" {
		message = string(failure.Type)
	}
	return message
}

func discoverProviderModels(ctx context.Context, client *http.Client, baseURL string, protocol domain.Protocol, secret string) ([]string, error) {
	models, _, err := discoverProviderModelsWithCredential(ctx, client, baseURL, protocol, secret, domain.CredentialRef{})
	return models, err
}

// discoverProviderModelsWithCredential is the catalog-only counterpart of
// the configured egress identity. It honors custom header/query credentials
// but never sends an inference request. Anthropic catalogs are cursor-paged;
// stopping after its default first page would silently miss newly released
// models. OpenRouter origins additionally resolve per-model capability
// metadata from the same pages; every other origin returns a nil entry map
// and callers fall back to conservative protocol defaults.
func discoverProviderModelsWithCredential(ctx context.Context, client *http.Client, baseURL string, protocol domain.Protocol, secret string, credential domain.CredentialRef) ([]string, map[string]providerCatalogModelEntry, error) {
	endpoint, err := providerModelsURL(baseURL, protocol)
	if err != nil {
		return nil, nil, err
	}

	safeClient := *client
	previousRedirect := safeClient.CheckRedirect
	safeClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) > 0 && (!strings.EqualFold(next.URL.Host, via[0].URL.Host) ||
			!strings.EqualFold(next.URL.Scheme, via[0].URL.Scheme)) {
			return fmt.Errorf("upstream model endpoint redirected to a different origin")
		}
		if previousRedirect != nil {
			return previousRedirect(next, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many upstream redirects")
		}
		return nil
	}
	withCapabilities := openRouterCatalogBaseURL(baseURL)
	models := []string{}
	entries := map[string]providerCatalogModelEntry{}
	seenModels := map[string]bool{}
	afterID := ""
	seenCursors := map[string]bool{}
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber == maxModelCatalogPages {
			return nil, nil, fmt.Errorf("upstream model catalog exceeds %d pages", maxModelCatalogPages)
		}
		requestURL, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("build upstream model request")
		}
		query := requestURL.Query()
		if protocol == domain.ProtocolAnthropic {
			query.Set("limit", "1000")
			if afterID != "" {
				query.Set("after_id", afterID)
			}
		}
		if credential.QueryParam != "" {
			query.Set(credential.QueryParam, secret)
		}
		requestURL.RawQuery = query.Encode()
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
		if requestErr != nil {
			return nil, nil, fmt.Errorf("build upstream model request")
		}
		req.Header.Set("Accept", "application/json")
		if credential.Header != "" || credential.QueryParam != "" {
			if credential.Header != "" {
				req.Header.Set(credential.Header, credential.Prefix+secret)
			}
		} else if protocol == domain.ProtocolAnthropic {
			req.Header.Set("x-api-key", secret)
		} else {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		if protocol == domain.ProtocolAnthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}

		resp, requestErr := safeClient.Do(req)
		if requestErr != nil {
			return nil, nil, fmt.Errorf("upstream model discovery failed")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, nil, &providerModelDiscoveryHTTPError{Status: resp.StatusCode}
		}
		if readErr != nil {
			return nil, nil, fmt.Errorf("read upstream model catalog")
		}
		if len(body) > 2<<20 {
			return nil, nil, fmt.Errorf("upstream model catalog exceeds 2 MiB")
		}
		page, rawItems, hasMore, lastID, pageTruncated, parseErr := parseDiscoveredModelPage(body)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		if pageTruncated {
			return nil, nil, fmt.Errorf("upstream model catalog exceeds %d models", maxDiscoveredModels)
		}
		for _, model := range page {
			if withCapabilities {
				model = canonicalOpenRouterCatalogModelID(model)
			}
			if seenModels[model] {
				continue
			}
			if len(models) == maxDiscoveredModels {
				return nil, nil, fmt.Errorf("upstream model catalog exceeds %d models", maxDiscoveredModels)
			}
			seenModels[model] = true
			models = append(models, model)
		}
		if withCapabilities {
			for id, entry := range openRouterCatalogEntries(rawItems) {
				entries[id] = entry
			}
		}
		if protocol != domain.ProtocolAnthropic || !hasMore {
			break
		}
		if len(page) == 0 {
			return nil, nil, fmt.Errorf("upstream model catalog returned an empty page with has_more")
		}
		lastID = strings.TrimSpace(lastID)
		if lastID == "" || seenCursors[lastID] {
			return nil, nil, fmt.Errorf("upstream model catalog returned an invalid pagination cursor")
		}
		seenCursors[lastID] = true
		afterID = lastID
	}
	if len(models) == 0 {
		return nil, nil, fmt.Errorf("upstream returned no models")
	}
	if !withCapabilities {
		return models, nil, nil
	}
	return models, entries, nil
}

func providerModelsURL(rawBase string, protocol domain.Protocol) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawBase))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("an absolute HTTP(S) Base URL is required")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if protocol == domain.ProtocolAnthropic && !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	parsed.Path = path + "/models"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func parseDiscoveredModels(body []byte) ([]string, error) {
	models, _, _, _, _, err := parseDiscoveredModelPage(body)
	return models, err
}

func parseDiscoveredModelPage(body []byte) ([]string, []json.RawMessage, bool, string, bool, error) {
	var items []json.RawMessage
	var hasMore bool
	var lastID string
	if err := json.Unmarshal(body, &items); err != nil {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, nil, false, "", false, fmt.Errorf("upstream model catalog is not valid JSON")
		}
		for _, key := range []string{"data", "models", "items"} {
			if raw := envelope[key]; len(raw) > 0 && json.Unmarshal(raw, &items) == nil {
				break
			}
		}
		_ = json.Unmarshal(envelope["has_more"], &hasMore)
		_ = json.Unmarshal(envelope["last_id"], &lastID)
	}
	seen := make(map[string]bool)
	models := make([]string, 0, min(len(items), maxDiscoveredModels))
	rawByID := make(map[string]json.RawMessage, len(items))
	truncated := false
	for _, raw := range items {
		var name string
		if json.Unmarshal(raw, &name) != nil {
			var item struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Model string `json:"model"`
			}
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			for _, candidate := range []string{item.ID, item.Name, item.Model} {
				if strings.TrimSpace(candidate) != "" {
					name = candidate
					break
				}
			}
		}
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if len(models) == maxDiscoveredModels {
			truncated = true
			continue
		}
		seen[name] = true
		models = append(models, name)
		rawByID[name] = raw
	}
	rawItems := make([]json.RawMessage, 0, len(models))
	for _, model := range models {
		if raw, ok := rawByID[model]; ok {
			rawItems = append(rawItems, raw)
		}
	}
	return models, rawItems, hasMore, lastID, truncated, nil
}

func (s *Server) nativeProviderClient(ref, baseURL, fullURL string, protocol domain.Protocol, client *http.Client) (*http.Client, error) {
	if _, _, native := nativeauth.ParseRef(ref); !native {
		return client, nil
	}
	if s.deps.NativeOAuth == nil {
		return nil, fmt.Errorf("native authentication unavailable")
	}
	if fullURL != "" {
		return nil, fmt.Errorf("native credentials do not accept full URL overrides")
	}
	if err := s.deps.NativeOAuth.ValidateBinding(ref, baseURL, string(protocol)); err != nil {
		return nil, err
	}
	return s.deps.NativeOAuth.ClientFor(ref, client), nil
}
