package management

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestOpenRouterCatalogBaseURLRequiresExactHTTPSOrigin(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want bool
	}{
		{"https://openrouter.ai/api/v1", true},
		{"https://openrouter.ai", true},
		{"http://openrouter.ai/api/v1", false},
		{"https://openrouter.ai.evil.test/api/v1", false},
		{"https://evil.test/openrouter.ai", false},
		{"https://user@openrouter.ai/api/v1", false},
		{"https://openrouter.ai:8443/api/v1", false},
		{"", false},
	} {
		if got := openRouterCatalogBaseURL(test.raw); got != test.want {
			t.Fatalf("openRouterCatalogBaseURL(%q)=%v want=%v", test.raw, got, test.want)
		}
	}
}

func TestCanonicalOpenRouterCatalogModelIDOnlyStripsAnthropicTransportDecoration(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{"anthropic/meta/muse-spark-1.3-contributor[1m]", "meta/muse-spark-1.3-contributor"},
		{"anthropic/meta/muse-spark-1.3-contributor [1m]", "meta/muse-spark-1.3-contributor"},
		{"anthropic/meta/muse-spark-1.3-contributor", "meta/muse-spark-1.3-contributor"},
		{"anthropic/claude-sonnet-4", "anthropic/claude-sonnet-4"},
		{"meta/muse-spark-1.3-contributor[1m]", "meta/muse-spark-1.3-contributor[1m]"},
		{"  vendor/model  ", "vendor/model"},
	} {
		if got := canonicalOpenRouterCatalogModelID(test.raw); got != test.want {
			t.Fatalf("canonicalOpenRouterCatalogModelID(%q)=%q want=%q", test.raw, got, test.want)
		}
	}
}

func TestOpenRouterCatalogEntriesMapModelMetadata(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"id":"anthropic/meta/muse-spark-1.3-contributor [1m]","context_length":1048576,
		"architecture":{"input_modalities":["text","image","video"]},
		"top_provider":{"context_length":1048576,"max_completion_tokens":943718},
		"supported_parameters":["tools","tool_choice","reasoning","reasoning_effort","include_reasoning","structured_outputs","response_format"]}`),
		json.RawMessage(`{"id":"vendor/plain-model","supported_parameters":[]}`),
		json.RawMessage(`{"id":"vendor/opaque-model"}`),
		json.RawMessage(`{"architecture":{"input_modalities":["text"]}}`),
		json.RawMessage(`"bare-string-model"`),
	}
	entries := openRouterCatalogEntries(items)
	if len(entries) != 2 {
		t.Fatalf("entries=%v", entries)
	}
	spark := entries["meta/muse-spark-1.3-contributor"]
	if !spark.Authoritative {
		t.Fatalf("muse-spark entry not authoritative: %+v", spark)
	}
	want := domain.Capabilities{
		Text: true, Stream: true, Vision: true, Tools: true, ParallelTools: true,
		Reasoning: true, StructuredOutput: true, ContextWindow: 1048576, MaxOutput: 943718,
	}
	if !reflect.DeepEqual(spark.Capabilities, want) {
		t.Fatalf("muse-spark capabilities=%+v want=%+v", spark.Capabilities, want)
	}
	plain := entries["vendor/plain-model"]
	if !plain.Authoritative || plain.Capabilities != (domain.Capabilities{Text: true, Stream: true}) {
		t.Fatalf("plain entry=%+v", plain)
	}
	if _, ok := entries["vendor/opaque-model"]; ok {
		t.Fatalf("model without supported_parameters must stay non-authoritative: %+v", entries)
	}
}

func TestParseModelsDevAggregateUnifiesProviders(t *testing.T) {
	body := []byte(`{
		"prov-one": {"models": {"shared/model": {
			"id": "shared/model", "tool_call": true, "reasoning": true,
			"structured_output": true, "attachment": false,
			"modalities": {"input": ["text"], "output": ["text"]},
			"limit": {"context": 200000, "output": 8192}
		}}},
		"prov-two": {"models": {"shared/model": {
			"id": "shared/model", "tool_call": false, "reasoning": false,
			"modalities": {"input": ["text", "image"], "output": ["text"]},
			"limit": {"context": 131072, "output": 4096}
		}, "solo/model": {"id": "solo/model", "tool_call": false, "reasoning": false}}}
	}`)
	entries, err := parseModelsDevAggregate(body)
	if err != nil {
		t.Fatal(err)
	}
	shared, ok := entries["shared/model"]
	if !ok {
		t.Fatalf("entries=%v", entries)
	}
	want := domain.Capabilities{
		Text: true, Stream: true, Tools: true, Reasoning: true, StructuredOutput: true,
		Vision: true, ContextWindow: 200000, MaxOutput: 8192,
	}
	if !reflect.DeepEqual(shared, want) {
		t.Fatalf("shared/model=%+v want=%+v", shared, want)
	}
	solo := entries["solo/model"]
	if solo.Tools || solo.Reasoning || solo.Vision {
		t.Fatalf("solo/model=%+v", solo)
	}
	if _, err := parseModelsDevAggregate([]byte(`{}`)); err == nil {
		t.Fatal("empty aggregate must fail")
	}
}

const openRouterCatalogTestResponse = `{"data":[
	{"id":"meta/muse-spark-1.3-contributor","context_length":1048576,
	 "architecture":{"input_modalities":["text","image"]},
	 "top_provider":{"context_length":1048576,"max_completion_tokens":943718},
	 "supported_parameters":["tools","tool_choice","reasoning","reasoning_effort","include_reasoning","structured_outputs","response_format"]},
	{"id":"vendor/blank-model","supported_parameters":[]},
	{"id":"vendor/opaque-model"}
]}`

// The production failure this guards against: an OpenRouter model synced with
// only the conservative Text capability stays ineligible for stream, tools,
// and reasoning requests forever, because catalog growth never revisits
// existing models. Upstream catalog metadata is authoritative and must both
// seed new models and heal the stored contract of already-synchronized ones.
func TestProviderModelSyncHealsExistingModelsFromOpenRouterCatalogMetadata(t *testing.T) {
	f := newV2APIFixture(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	f.server.configMu.Lock()
	next, err := cloneConfig(f.server.deps.Config)
	if err == nil {
		next.Providers[0].BaseURL = "https://openrouter.ai/api/v1"
		next.Connections[0].Models = append(next.Connections[0].Models, domain.ConnectionModel{
			UpstreamModel: "meta/muse-spark-1.3-contributor",
			Protocols:     []domain.Protocol{domain.ProtocolOpenAIChat},
			Capabilities:  domain.Capabilities{Text: true},
		})
		err = f.server.publishConfig(next)
	}
	f.server.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var catalogRequests atomic.Int64
	configureCustomModelSyncFixture(f, modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		catalogRequests.Add(1)
		if request.Method != http.MethodGet || request.URL.String() != "https://openrouter.ai/api/v1/models" ||
			request.Header.Get("Authorization") != "Bearer catalog-secret" {
			t.Fatalf("catalog request=%s %s auth=%q", request.Method, request.URL, request.Header.Get("Authorization"))
		}
		return modelCatalogResponse(http.StatusOK, openRouterCatalogTestResponse), nil
	}), now)

	result := f.server.syncProviderModels(context.Background(), "provider-a")
	if result.HTTPStatus != http.StatusOK || result.State.Status != "succeeded" {
		t.Fatalf("result=%+v", result)
	}
	if result.State.DiscoveredModels != 3 || result.State.AddedModels != 2 ||
		result.State.UpdatedModels != 1 || result.State.CreatedLogicalModels != 2 {
		t.Fatalf("state=%+v added=%v updated=%v", result.State, result.AddedModelIDs, result.UpdatedModelIDs)
	}
	if !reflect.DeepEqual(result.UpdatedModelIDs, []string{"meta/muse-spark-1.3-contributor"}) {
		t.Fatalf("updated=%v", result.UpdatedModelIDs)
	}

	active, revision, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	connection := providerConnection(active, "provider-a", "connection-a")
	if connection == nil {
		t.Fatal("connection missing")
	}
	models := map[string]domain.ConnectionModel{}
	for _, model := range connection.Models {
		models[model.UpstreamModel] = model
	}
	healed := models["meta/muse-spark-1.3-contributor"]
	wantHealed := domain.Capabilities{
		Text: true, Stream: true, Vision: true, Tools: true, ParallelTools: true,
		Reasoning: true, StructuredOutput: true, ContextWindow: 1048576, MaxOutput: 943718,
	}
	if healed.Protocols[0] != domain.ProtocolOpenAIChat || healed.Capabilities != wantHealed {
		t.Fatalf("healed model=%+v want capabilities=%+v", healed, wantHealed)
	}
	if blank := models["vendor/blank-model"]; blank.Capabilities != (domain.Capabilities{Text: true, Stream: true}) {
		t.Fatalf("blank-model=%+v", blank)
	}
	if opaque := models["vendor/opaque-model"]; opaque.Capabilities != (domain.Capabilities{Text: true}) {
		t.Fatalf("opaque-model=%+v", opaque)
	}

	repeat := f.server.syncProviderModels(context.Background(), "provider-a")
	if repeat.HTTPStatus != http.StatusOK || repeat.State.AddedModels != 0 ||
		repeat.State.UpdatedModels != 0 || repeat.State.CreatedLogicalModels != 0 {
		t.Fatalf("repeat=%+v", repeat.State)
	}
	_, repeatedRevision, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if repeatedRevision != revision || catalogRequests.Load() != 2 {
		t.Fatalf("repeat revision=%d want=%d catalog requests=%d", repeatedRevision, revision, catalogRequests.Load())
	}
}

// Catalogs that disclose only model identifiers get community defaults from
// the models.dev aggregate. Existing models are refined additively: fallback
// data fills missing capability bits but never subtracts an operator- or
// probe-declared one.
func TestProviderModelSyncFallsBackToCommunityAggregateAdditively(t *testing.T) {
	f := newV2APIFixture(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	f.server.configMu.Lock()
	next, err := cloneConfig(f.server.deps.Config)
	if err == nil {
		next.Connections[0].Models = append(next.Connections[0].Models,
			domain.ConnectionModel{
				UpstreamModel: "shared/model",
				Protocols:     []domain.Protocol{domain.ProtocolOpenAIChat},
				// Operator-declared tool support and a tuned context limit must
				// survive the fallback merge; reasoning/vision get filled in.
				Capabilities: domain.Capabilities{Text: true, Stream: true, Tools: true, ContextWindow: 250000},
			},
			domain.ConnectionModel{
				UpstreamModel: "solo/model",
				Protocols:     []domain.Protocol{domain.ProtocolOpenAIChat},
				Capabilities:  domain.Capabilities{Text: true, Stream: true, Tools: true},
			})
		err = f.server.publishConfig(next)
	}
	f.server.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var aggregateRequests atomic.Int64
	configureCustomModelSyncFixture(f, modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "https://example.test/v1/models" {
			t.Fatalf("catalog request=%s %s", request.Method, request.URL)
		}
		return modelCatalogResponse(http.StatusOK, `{"data":[{"id":"shared/model"},{"id":"solo/model"},{"id":"unknown/model"}]}`), nil
	}), now)
	f.server.deps.ModelsDevHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		aggregateRequests.Add(1)
		if request.Method != http.MethodGet || request.URL.String() != modelsDevCatalogURL ||
			request.Header.Get("Accept") != "application/json" {
			t.Fatalf("aggregate request=%s %s", request.Method, request.URL)
		}
		return modelCatalogResponse(http.StatusOK, `{
			"prov-one": {"models": {"shared/model": {
				"id": "shared/model", "tool_call": true, "reasoning": true,
				"structured_output": true, "attachment": false,
				"modalities": {"input": ["text"], "output": ["text"]},
				"limit": {"context": 200000, "output": 8192}
			}}},
			"prov-two": {"models": {"shared/model": {
				"id": "shared/model", "tool_call": false, "reasoning": false,
				"modalities": {"input": ["text", "image"], "output": ["text"]},
				"limit": {"context": 131072, "output": 4096}
			}, "solo/model": {"id": "solo/model", "tool_call": false, "reasoning": false}}}
		}`), nil
	})}

	result := f.server.syncProviderModels(context.Background(), "provider-a")
	if result.HTTPStatus != http.StatusOK || result.State.Status != "succeeded" {
		t.Fatalf("result=%+v", result)
	}
	if result.State.DiscoveredModels != 3 || result.State.AddedModels != 1 || result.State.UpdatedModels != 1 {
		t.Fatalf("state=%+v added=%v updated=%v", result.State, result.AddedModelIDs, result.UpdatedModelIDs)
	}
	if !reflect.DeepEqual(result.UpdatedModelIDs, []string{"shared/model"}) {
		t.Fatalf("updated=%v", result.UpdatedModelIDs)
	}

	active, _, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	connection := providerConnection(active, "provider-a", "connection-a")
	models := map[string]domain.ConnectionModel{}
	for _, model := range connection.Models {
		models[model.UpstreamModel] = model
	}
	shared := models["shared/model"]
	wantShared := domain.Capabilities{
		Text: true, Stream: true, Tools: true, Reasoning: true, StructuredOutput: true,
		Vision: true, ContextWindow: 250000, MaxOutput: 8192,
	}
	if shared.Capabilities != wantShared {
		t.Fatalf("shared/model=%+v want=%+v", shared.Capabilities, wantShared)
	}
	// The aggregate claims solo/model has no tool support; the operator's
	// declaration must not be subtracted and the model must not be reported as
	// updated.
	if solo := models["solo/model"]; solo.Capabilities != (domain.Capabilities{Text: true, Stream: true, Tools: true}) {
		t.Fatalf("solo/model=%+v", solo)
	}
	if unknown := models["unknown/model"]; unknown.Capabilities != (domain.Capabilities{Text: true}) {
		t.Fatalf("unknown/model=%+v", unknown)
	}

	repeat := f.server.syncProviderModels(context.Background(), "provider-a")
	if repeat.State.AddedModels != 0 || repeat.State.UpdatedModels != 0 || aggregateRequests.Load() != 1 {
		t.Fatalf("repeat=%+v aggregate requests=%d", repeat.State, aggregateRequests.Load())
	}
}

func TestRefreshModelsDevCatalogDegradesGracefully(t *testing.T) {
	f := newV2APIFixture(t)
	var attempts atomic.Int64
	f.server.deps.ModelsDevHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return modelCatalogResponse(http.StatusServiceUnavailable, "unavailable"), nil
	})}
	if err := f.server.refreshModelsDevCatalog(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err=%v", err)
	}
	if fallback := f.server.modelsDevFallbackCapabilities("any/model"); fallback != nil {
		t.Fatalf("failed fetch must not seed the cache: %+v", fallback)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
}

func TestCatalogCapabilityEntryPrefersUpstreamMetadata(t *testing.T) {
	authoritative := providerCatalogModelEntry{
		Authoritative: true,
		Capabilities:  domain.Capabilities{Text: true, Stream: true, Tools: true},
	}
	catalog := providerModelCatalog{
		protocol: domain.ProtocolOpenAIChat,
		declared: map[string]providerCatalogModelEntry{"declared/model": authoritative},
	}
	fallback := func(modelID string) *domain.Capabilities {
		if modelID == "fallback/model" || modelID == "declared/model" {
			capabilities := domain.Capabilities{Text: true}
			return &capabilities
		}
		return nil
	}
	if entry := catalogCapabilityEntry(catalog, fallback, "declared/model"); entry == nil || !entry.Authoritative {
		t.Fatalf("declared entry=%+v", entry)
	}
	if entry := catalogCapabilityEntry(catalog, fallback, "fallback/model"); entry == nil || entry.Authoritative || !entry.Capabilities.Text {
		t.Fatalf("fallback entry=%+v", entry)
	}
	if entry := catalogCapabilityEntry(catalog, fallback, "missing/model"); entry != nil {
		t.Fatalf("missing entry=%+v", entry)
	}
	images := providerModelCatalog{protocol: domain.ProtocolOpenAIImages}
	if entry := catalogCapabilityEntry(images, fallback, "declared/model"); entry != nil {
		t.Fatalf("image catalog entry=%+v", entry)
	}
}

// Providers whose upstream exposes no catalog at all still get their
// operator-configured models refined by the community aggregate; the sync
// status keeps reporting the catalog honestly as unsupported.
func TestProviderModelSyncHealsConfiguredModelsWhenCatalogUnsupported(t *testing.T) {
	f := newV2APIFixture(t)
	now := time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC)
	f.server.configMu.Lock()
	next, err := cloneConfig(f.server.deps.Config)
	if err == nil {
		next.Connections[0].Models = append(next.Connections[0].Models, domain.ConnectionModel{
			UpstreamModel: "shared/model",
			Protocols:     []domain.Protocol{domain.ProtocolOpenAIChat},
			Capabilities:  domain.Capabilities{Text: true},
		})
		err = f.server.publishConfig(next)
	}
	f.server.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	configureCustomModelSyncFixture(f, modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return modelCatalogResponse(http.StatusNotFound, `{}`), nil
	}), now)
	var aggregateRequests atomic.Int64
	f.server.deps.ModelsDevHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		aggregateRequests.Add(1)
		return modelCatalogResponse(http.StatusOK, `{"prov-one":{"models":{"shared/model":{
			"id":"shared/model","tool_call":true,"reasoning":true,
			"modalities":{"input":["text"],"output":["text"]},
			"limit":{"context":262144,"output":131072}
		}}}}`), nil
	})}

	result := f.server.syncProviderModels(context.Background(), "provider-a")
	if result.HTTPStatus != http.StatusOK || result.State.Status != "unsupported" ||
		result.State.LastErrorCode != "catalog_unsupported" {
		t.Fatalf("result=%+v", result.State)
	}
	if result.State.DiscoveredModels != 0 || result.State.AddedModels != 0 || result.State.UpdatedModels != 1 {
		t.Fatalf("state=%+v", result.State)
	}
	active, revision, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	connection := providerConnection(active, "provider-a", "connection-a")
	healed := false
	for _, model := range connection.Models {
		if model.UpstreamModel == "shared/model" {
			want := domain.Capabilities{Text: true, Stream: true, Tools: true, Reasoning: true, ContextWindow: 262144, MaxOutput: 131072}
			if model.Capabilities != want {
				t.Fatalf("healed shared/model=%+v want=%+v", model.Capabilities, want)
			}
			healed = true
		}
	}
	if !healed {
		t.Fatalf("shared/model missing: %+v", connection.Models)
	}

	repeat := f.server.syncProviderModels(context.Background(), "provider-a")
	if repeat.State.Status != "unsupported" || repeat.State.UpdatedModels != 0 || aggregateRequests.Load() != 1 {
		t.Fatalf("repeat=%+v aggregate requests=%d", repeat.State, aggregateRequests.Load())
	}
	_, repeatedRevision, err := f.control.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if repeatedRevision != revision {
		t.Fatalf("repeat revision=%d want=%d", repeatedRevision, revision)
	}
}
