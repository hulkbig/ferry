package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ferry/internal/domain"
)

// providerCatalogModelEntry is one catalog-sourced capability declaration for
// a discovered model. Authoritative entries come from the upstream catalog
// itself (OpenRouter model metadata) and may replace a connection model's
// declared capabilities during synchronization. Non-authoritative entries come
// from the community aggregate and may only refine a connection model
// additively, never subtracting an operator- or probe-declared capability.
type providerCatalogModelEntry struct {
	Capabilities  domain.Capabilities
	Authoritative bool
}

// openRouterCatalogBaseURL reports whether a catalog Base URL is the exact
// HTTPS OpenRouter origin whose /models response carries per-model capability
// metadata (architecture modalities, supported_parameters, token limits).
func openRouterCatalogBaseURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return exactHTTPSOrigin(parsed, "openrouter.ai")
}

// canonicalOpenRouterCatalogModelID collapses the protocol-shaped identifiers
// emitted by OpenRouter's Anthropic compatibility catalog back to the model
// slug used by its canonical provider catalog and inference APIs. Restrict the
// rewrite to an "anthropic/" transport prefix followed by another vendor
// segment: a real OpenRouter model such as "anthropic/claude-*" has no second
// slash and must remain untouched. The optional [1m] suffix is a catalog
// presentation badge, not part of the callable model id.
func canonicalOpenRouterCatalogModelID(raw string) string {
	id := strings.TrimSpace(raw)
	const anthropicTransportPrefix = "anthropic/"
	if !strings.HasPrefix(id, anthropicTransportPrefix) {
		return id
	}
	candidate := strings.TrimPrefix(id, anthropicTransportPrefix)
	if !strings.Contains(candidate, "/") {
		return id
	}
	candidate = strings.TrimSpace(candidate)
	if strings.HasSuffix(candidate, "[1m]") {
		candidate = strings.TrimSpace(strings.TrimSuffix(candidate, "[1m]"))
	}
	if candidate == "" {
		return id
	}
	return candidate
}

// openRouterCatalogEntries maps one OpenRouter /models page of raw items to
// capability entries. Items without a usable id or without the
// supported_parameters declaration are skipped: a missing declaration is not
// authoritative and must not silently clear configured capabilities.
func openRouterCatalogEntries(items []json.RawMessage) map[string]providerCatalogModelEntry {
	entries := map[string]providerCatalogModelEntry{}
	for _, raw := range items {
		var item struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			Architecture  struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
			TopProvider *struct {
				ContextLength       int64 `json:"context_length"`
				MaxCompletionTokens int64 `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters *[]string `json:"supported_parameters"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		id := canonicalOpenRouterCatalogModelID(item.ID)
		if id == "" || item.SupportedParameters == nil {
			continue
		}
		hasParameter := func(names ...string) bool {
			for _, parameter := range *item.SupportedParameters {
				for _, name := range names {
					if parameter == name {
						return true
					}
				}
			}
			return false
		}
		contextWindow := item.ContextLength
		if contextWindow <= 0 && item.TopProvider != nil {
			contextWindow = item.TopProvider.ContextLength
		}
		maxOutput := int64(0)
		if item.TopProvider != nil {
			maxOutput = item.TopProvider.MaxCompletionTokens
		}
		entries[id] = providerCatalogModelEntry{
			Authoritative: true,
			Capabilities: domain.Capabilities{
				// OpenRouter is a chat-completions platform: every listed
				// model speaks text and SSE streaming there, independent of
				// what the backing provider supports natively.
				Text:   true,
				Stream: true,
				Vision: catalogModalityList(item.Architecture.InputModalities).contains("image"),
				Tools:  hasParameter("tools", "tool_choice"),
				// Ferry serializes parallel tool calls when ordinary tool
				// calls exist, so parallel_tool_calls upgrades tool support.
				ParallelTools:    hasParameter("tools", "tool_choice", "parallel_tool_calls"),
				Reasoning:        hasParameter("reasoning", "include_reasoning", "reasoning_effort"),
				StructuredOutput: hasParameter("structured_outputs", "response_format"),
				ContextWindow:    contextWindow,
				MaxOutput:        maxOutput,
			},
		}
	}
	return entries
}

type catalogModalityList []string

func (list catalogModalityList) contains(modality string) bool {
	for _, candidate := range list {
		if strings.EqualFold(strings.TrimSpace(candidate), modality) {
			return true
		}
	}
	return false
}

// modelsDevCatalogURL is the community-maintained cross-provider model
// capability aggregate (github.com/sst/models). It fills capability defaults
// for catalogs that disclose only model identifiers.
const modelsDevCatalogURL = "https://models.dev/api.json"

const (
	modelsDevCatalogTTL     = 24 * time.Hour
	modelsDevCatalogTimeout = 20 * time.Second
	modelsDevCatalogBytes   = 16 << 20
)

// parseModelsDevAggregate indexes the community aggregate by exact upstream
// model id. The same model id can appear under several providers; capability
// bits are unioned and numeric limits keep the maximum so one model resolves
// to one unified capability view across platforms.
func parseModelsDevAggregate(body []byte) (map[string]domain.Capabilities, error) {
	var providers map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, fmt.Errorf("community model catalog is not valid JSON")
	}
	entries := map[string]domain.Capabilities{}
	for _, provider := range providers {
		for key, raw := range provider.Models {
			var model struct {
				ID               string `json:"id"`
				ToolCall         bool   `json:"tool_call"`
				Reasoning        bool   `json:"reasoning"`
				StructuredOutput bool   `json:"structured_output"`
				Attachment       bool   `json:"attachment"`
				Modalities       *struct {
					Input  []string `json:"input"`
					Output []string `json:"output"`
				} `json:"modalities"`
				Limit *struct {
					Context int64 `json:"context"`
					Output  int64 `json:"output"`
				} `json:"limit"`
			}
			if json.Unmarshal(raw, &model) != nil {
				continue
			}
			id := strings.TrimSpace(model.ID)
			if id == "" {
				id = strings.TrimSpace(key)
			}
			if id == "" {
				continue
			}
			vision := model.Attachment
			if model.Modalities != nil {
				vision = catalogModalityList(model.Modalities.Input).contains("image")
			}
			var contextWindow, maxOutput int64
			if model.Limit != nil {
				contextWindow, maxOutput = model.Limit.Context, model.Limit.Output
			}
			capabilities := domain.Capabilities{
				// The aggregate tracks OpenAI-compatible chat APIs; streaming
				// is the platform contract for every entry it lists.
				Text: true, Stream: true,
				Tools: model.ToolCall, Reasoning: model.Reasoning,
				StructuredOutput: model.StructuredOutput, Vision: vision,
				ContextWindow: contextWindow, MaxOutput: maxOutput,
			}
			entries[id] = domain.MergeCapabilities(entries[id], capabilities)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("community model catalog contains no model entries")
	}
	return entries, nil
}

// modelsDevCatalogCache holds the last successful community-aggregate fetch.
// A nil entries map means the fallback layer is disabled or unavailable; sync
// then keeps the conservative protocol defaults.
type modelsDevCatalogCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	entries   map[string]domain.Capabilities
}

// refreshModelsDevCatalog re-fetches the community aggregate when its TTL
// expired. Failures degrade to the previous snapshot and never fail the
// provider model synchronization itself.
func (s *Server) refreshModelsDevCatalog(ctx context.Context) error {
	client := s.deps.ModelsDevHTTPClient
	if client == nil {
		return nil
	}
	s.modelsDev.mu.Lock()
	if time.Since(s.modelsDev.fetchedAt) < modelsDevCatalogTTL && s.modelsDev.entries != nil {
		s.modelsDev.mu.Unlock()
		return nil
	}
	started := s.modelsDev.fetchedAt
	s.modelsDev.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevCatalogURL, nil)
	if err != nil {
		return fmt.Errorf("build community model catalog request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch community model catalog")
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, modelsDevCatalogBytes+1))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("community model catalog returned HTTP %d", resp.StatusCode)
	}
	if readErr != nil {
		return fmt.Errorf("read community model catalog")
	}
	if len(body) > modelsDevCatalogBytes {
		return fmt.Errorf("community model catalog exceeds %d bytes", modelsDevCatalogBytes)
	}
	entries, err := parseModelsDevAggregate(body)
	if err != nil {
		return err
	}
	s.modelsDev.mu.Lock()
	// A concurrent refresh may have completed while this fetch was running;
	// keep whichever snapshot is newer instead of overwriting it with stale
	// bytes.
	if !s.modelsDev.fetchedAt.After(started) {
		s.modelsDev.fetchedAt = time.Now().UTC()
		s.modelsDev.entries = entries
	}
	s.modelsDev.mu.Unlock()
	return nil
}

// modelsDevFallbackCapabilities returns the unified community capabilities
// for one exact upstream model id, or nil when the aggregate has no entry.
func (s *Server) modelsDevFallbackCapabilities(modelID string) *domain.Capabilities {
	s.modelsDev.mu.Lock()
	defer s.modelsDev.mu.Unlock()
	if s.modelsDev.entries == nil {
		return nil
	}
	if capabilities, ok := s.modelsDev.entries[modelID]; ok {
		return &capabilities
	}
	return nil
}

// refreshModelsDevCatalogIfNeeded fetches the community aggregate only when a
// synchronized catalog has models without upstream capability metadata.
func (s *Server) refreshModelsDevCatalogIfNeeded(ctx context.Context, catalogs []providerModelCatalog) {
	if s.deps.ModelsDevHTTPClient == nil {
		return
	}
	needsFallback := false
	for _, catalog := range catalogs {
		if len(catalog.declared) == 0 {
			needsFallback = true
			break
		}
		for _, modelID := range catalog.models {
			if _, ok := catalog.declared[modelID]; !ok {
				needsFallback = true
				break
			}
		}
	}
	if !needsFallback {
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, modelsDevCatalogTimeout)
	defer cancel()
	if err := s.refreshModelsDevCatalog(fetchCtx); err != nil {
		log.Printf("community model capability catalog unavailable: %v", err)
	}
}

// catalogCapabilityEntry resolves the best available capability declaration
// for one discovered model: upstream catalog metadata is authoritative and
// wins; the community aggregate is a non-authoritative default. Image-family
// catalogs never take chat-shaped defaults.
func catalogCapabilityEntry(catalog providerModelCatalog, fallback func(string) *domain.Capabilities, modelID string) *providerCatalogModelEntry {
	if catalog.protocol == domain.ProtocolOpenAIImages {
		return nil
	}
	if entry, ok := catalog.declared[modelID]; ok {
		return &entry
	}
	if fallback != nil {
		if capabilities := fallback(modelID); capabilities != nil {
			return &providerCatalogModelEntry{Capabilities: *capabilities}
		}
	}
	return nil
}
