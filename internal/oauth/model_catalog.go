package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"ferry/internal/domain"
)

const (
	// ModelCatalogAuthUnavailable means the account has no usable OAuth
	// credential, including when a single refresh after HTTP 401 fails.
	ModelCatalogAuthUnavailable = "credential_unavailable"
	// ModelCatalogAccountIdentityUnavailable means the OAuth identity lacks the
	// ChatGPT account id needed to scope the catalog request.
	ModelCatalogAccountIdentityUnavailable = "account_identity_unavailable"
	// ModelCatalogEntitlementDenied means the authenticated account was denied
	// access to the Codex catalog.
	ModelCatalogEntitlementDenied = "entitlement_denied"
	// ModelCatalogRateLimited means the catalog endpoint returned HTTP 429.
	ModelCatalogRateLimited = "rate_limited"
	// ModelCatalogUpstreamUnavailable covers transport, timeout, cancellation,
	// redirect, and unexpected upstream status failures.
	ModelCatalogUpstreamUnavailable = "upstream_unavailable"
	// ModelCatalogInvalidCatalog means a successful response was malformed,
	// unbounded, or contained no non-empty model slug.
	ModelCatalogInvalidCatalog = "invalid_catalog"
)

const (
	defaultCodexModelsURL     = "https://chatgpt.com/backend-api/codex/models"
	codexModelsTimeout        = 15 * time.Second
	codexModelsMaxBodyBytes   = 2 << 20
	codexModelsMaxModelCount  = 2000
	codexModelsErrorDrainSize = 4 << 10
)

// codexModelsURL is a var so tests can point discovery at a fake catalog.
var codexModelsURL = defaultCodexModelsURL

// SetModelCatalogURL overrides the model catalog endpoint (test seam).
func SetModelCatalogURL(u string) { codexModelsURL = u }

// CurrentModelCatalogURL returns the active model catalog endpoint.
func CurrentModelCatalogURL() string { return codexModelsURL }

// ModelCatalogError is a credential-safe classification of a Codex catalog
// failure. Error never includes an access token, account id, or response body.
type ModelCatalogError struct {
	Code       string
	HTTPStatus int
	cause      error
}

func (e *ModelCatalogError) Error() string {
	if e == nil {
		return "Codex model catalog failed"
	}
	message := "Codex model catalog failed"
	switch e.Code {
	case ModelCatalogAuthUnavailable:
		message = "Codex model catalog credential is unavailable"
	case ModelCatalogAccountIdentityUnavailable:
		message = "Codex model catalog account identity is unavailable"
	case ModelCatalogEntitlementDenied:
		message = "Codex model catalog access was denied"
	case ModelCatalogRateLimited:
		message = "Codex model catalog request was rate limited"
	case ModelCatalogUpstreamUnavailable:
		if errors.Is(e.cause, context.DeadlineExceeded) {
			message = "Codex model catalog request timed out"
		} else if errors.Is(e.cause, context.Canceled) {
			message = "Codex model catalog request was canceled"
		} else {
			message = "Codex model catalog is temporarily unavailable"
		}
	case ModelCatalogInvalidCatalog:
		message = "Codex model catalog response is invalid"
	}
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("%s (HTTP %d)", message, e.HTTPStatus)
	}
	return message
}

// Unwrap preserves context cancellation/deadline inspection without exposing
// the underlying error text through Error.
func (e *ModelCatalogError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type modelCatalogFlightKey struct {
	manager *Manager
	account string
	version string
}

type modelCatalogFlight struct {
	done   chan struct{}
	models []CatalogModel
	err    error
}

var modelCatalogFlights = struct {
	sync.Mutex
	calls map[modelCatalogFlightKey]*modelCatalogFlight
}{calls: make(map[modelCatalogFlightKey]*modelCatalogFlight)}

// CatalogModel is one entry of the account-scoped Codex model
// catalog. The metadata fields are optional upstream hints: a zero value means
// the catalog did not disclose the fact, never that the capability is
// unsupported.
type CatalogModel struct {
	Slug string
	// Visibility controls discovery, not whether a configured model's
	// capability metadata can be reconciled. Only list permits auto-addition.
	Visibility string
	// ContextWindow is max(context_window, max_context_window); 0 = undisclosed.
	ContextWindow int64
	// TextInput/ImageInput reflect input_modalities. An absent list keeps the
	// upstream text+image default, so TextInput is false only when the catalog
	// explicitly lists modalities without text.
	TextInput  bool
	ImageInput bool
	// Priority is nil for an undisclosed tier list, false for an explicit
	// list without priority, and true only for an advertised priority tier.
	Priority *bool
}

// DiscoverModels reads the account-scoped Codex model catalog and returns the
// sorted, unique entries that the service marks visibility=list, together
// with the capability metadata each entry discloses. The endpoint is a
// metadata GET: it sends no inference request and returns no credential or
// account material. Concurrent calls for the same Manager and account share
// one upstream request.
func (m *Manager) DiscoverModels(ctx context.Context, account string) ([]CatalogModel, error) {
	return m.DiscoverModelsWithVersion(ctx, account, "")
}

func (m *Manager) DiscoverModelsWithVersion(ctx context.Context, account, version string) ([]CatalogModel, error) {
	models, err := m.ReadModelCatalogWithVersion(ctx, account, version)
	if err != nil {
		return nil, err
	}
	visible := make([]CatalogModel, 0, len(models))
	for _, model := range models {
		if model.Visibility == "list" {
			visible = append(visible, model)
		}
	}
	return visible, nil
}

// ReadModelCatalog retains hidden entries so synchronization can refresh
// capabilities of already-configured models. Hidden entries do not authorize
// discovering or adding new models. Discovery and synchronization share the
// same bounded, credential-safe read and in-flight request deduplication.
func (m *Manager) ReadModelCatalog(ctx context.Context, account string) ([]CatalogModel, error) {
	return m.ReadModelCatalogWithVersion(ctx, account, "")
}

// ReadModelCatalogWithVersion pins the request and deduplication to one configuration revision.
func (m *Manager) ReadModelCatalogWithVersion(ctx context.Context, account, version string) ([]CatalogModel, error) {
	if err := domain.ValidateCodexClientVersion(version); err != nil {
		return nil, err
	}
	version = domain.EffectiveCodexClientVersion(version)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validAccount(account); err != nil {
		return nil, newModelCatalogError(ModelCatalogAuthUnavailable, 0, err)
	}

	key := modelCatalogFlightKey{manager: m, account: account, version: version}
	modelCatalogFlights.Lock()
	if call := modelCatalogFlights.calls[key]; call != nil {
		modelCatalogFlights.Unlock()
		select {
		case <-ctx.Done():
			return nil, newModelCatalogError(ModelCatalogUpstreamUnavailable, 0, ctx.Err())
		case <-call.done:
			return append([]CatalogModel(nil), call.models...), call.err
		}
	}
	call := &modelCatalogFlight{done: make(chan struct{})}
	modelCatalogFlights.calls[key] = call
	modelCatalogFlights.Unlock()

	models, err := m.discoverModels(ctx, account, version)
	call.models, call.err = append([]CatalogModel(nil), models...), err
	modelCatalogFlights.Lock()
	delete(modelCatalogFlights.calls, key)
	close(call.done)
	modelCatalogFlights.Unlock()
	return models, err
}

func (m *Manager) discoverModels(ctx context.Context, account, version string) ([]CatalogModel, error) {
	ctx, cancel := context.WithTimeout(ctx, codexModelsTimeout)
	defer cancel()

	accessToken, accountID, err := m.Identity(ctx, account)
	if err != nil {
		return nil, newModelCatalogError(ModelCatalogAuthUnavailable, 0, err)
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, newModelCatalogError(ModelCatalogAccountIdentityUnavailable, 0, nil)
	}

	models, status, err := m.readModelCatalog(ctx, accessToken, accountID, version)
	if status == http.StatusUnauthorized {
		accessToken, accountID, err = m.RefreshIdentity(ctx, account, accessToken)
		if err != nil {
			return nil, newModelCatalogError(ModelCatalogAuthUnavailable, http.StatusUnauthorized, err)
		}
		if strings.TrimSpace(accountID) == "" {
			return nil, newModelCatalogError(ModelCatalogAccountIdentityUnavailable, 0, nil)
		}
		models, status, err = m.readModelCatalog(ctx, accessToken, accountID, version)
	}
	if err != nil {
		var catalogErr *ModelCatalogError
		if errors.As(err, &catalogErr) {
			return nil, catalogErr
		}
		return nil, newModelCatalogError(ModelCatalogUpstreamUnavailable, status, err)
	}
	if status != http.StatusOK {
		return nil, modelCatalogStatusError(status)
	}
	return models, nil
}

type codexModelsResponse struct {
	Models []codexModelEntry `json:"models"`
}

// codexModelEntry mirrors only the catalog fields Ferry depends on. The
// upstream document carries more capability fields (reasoning levels, tool
// lists, truncation policy); they are deliberately not parsed so an upstream
// reshuffle cannot break discovery, and absent fields keep their zero values.
type codexModelEntry struct {
	Slug             string   `json:"slug"`
	Visibility       string   `json:"visibility"`
	ContextWindow    *int64   `json:"context_window"`
	MaxContextWindow *int64   `json:"max_context_window"`
	InputModalities  []string `json:"input_modalities"`
	ServiceTiers     []struct {
		ID string `json:"id"`
	} `json:"service_tiers"`
}

func (m *Manager) readModelCatalog(ctx context.Context, accessToken, accountID, version string) ([]CatalogModel, int, error) {
	query := url.Values{"client_version": {version}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexModelsURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, 0, newModelCatalogError(ModelCatalogUpstreamUnavailable, 0, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("Originator", domain.CodexOriginator)
	req.Header.Set("User-Agent", domain.CodexUserAgentForVersion(version))
	req.Header.Set("Version", version)

	if m.hc == nil {
		return nil, 0, newModelCatalogError(ModelCatalogUpstreamUnavailable, 0, nil)
	}
	client := *m.hc
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, newModelCatalogError(ModelCatalogUpstreamUnavailable, 0, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, codexModelsErrorDrainSize))
		return nil, resp.StatusCode, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, codexModelsMaxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, newModelCatalogError(ModelCatalogUpstreamUnavailable, resp.StatusCode, err)
	}
	if len(body) > codexModelsMaxBodyBytes {
		return nil, resp.StatusCode, newModelCatalogError(ModelCatalogInvalidCatalog, resp.StatusCode, nil)
	}
	var catalog codexModelsResponse
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, resp.StatusCode, newModelCatalogError(ModelCatalogInvalidCatalog, resp.StatusCode, err)
	}
	if len(catalog.Models) > codexModelsMaxModelCount {
		return nil, resp.StatusCode, newModelCatalogError(ModelCatalogInvalidCatalog, resp.StatusCode, nil)
	}

	seen := make(map[string]int, len(catalog.Models))
	models := make([]CatalogModel, 0, len(catalog.Models))
	for _, entry := range catalog.Models {
		model := catalogModelFromEntry(entry)
		if model.Slug == "" {
			continue
		}
		if index, duplicate := seen[model.Slug]; duplicate {
			// Preserve discovery's preference for a list-visible row even if
			// the upstream catalog sends a hidden duplicate first.
			if model.Visibility == "list" && models[index].Visibility != "list" {
				models[index] = model
			}
			continue
		}
		seen[model.Slug] = len(models)
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, resp.StatusCode, newModelCatalogError(ModelCatalogInvalidCatalog, resp.StatusCode, nil)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Slug < models[j].Slug })
	return models, resp.StatusCode, nil
}

// catalogModelFromEntry converts one upstream entry into the metadata Ferry
// consumes. Every field is optional; modalities keep the upstream text+image
// default when the catalog omits them.
func catalogModelFromEntry(entry codexModelEntry) CatalogModel {
	model := CatalogModel{
		Slug: strings.TrimSpace(entry.Slug), Visibility: strings.ToLower(strings.TrimSpace(entry.Visibility)),
	}
	if entry.ServiceTiers != nil {
		priority := false
		for _, tier := range entry.ServiceTiers {
			priority = priority || tier.ID == "priority"
		}
		model.Priority = &priority
	}
	if entry.ContextWindow != nil && *entry.ContextWindow > model.ContextWindow {
		model.ContextWindow = *entry.ContextWindow
	}
	if entry.MaxContextWindow != nil && *entry.MaxContextWindow > model.ContextWindow {
		model.ContextWindow = *entry.MaxContextWindow
	}
	if entry.InputModalities == nil {
		model.TextInput, model.ImageInput = true, true
	} else {
		for _, modality := range entry.InputModalities {
			switch strings.ToLower(strings.TrimSpace(modality)) {
			case "text":
				model.TextInput = true
			case "image":
				model.ImageInput = true
			}
		}
	}
	return model
}

func modelCatalogStatusError(status int) *ModelCatalogError {
	switch status {
	case http.StatusUnauthorized:
		return newModelCatalogError(ModelCatalogAuthUnavailable, status, nil)
	case http.StatusForbidden:
		return newModelCatalogError(ModelCatalogEntitlementDenied, status, nil)
	case http.StatusTooManyRequests:
		return newModelCatalogError(ModelCatalogRateLimited, status, nil)
	default:
		return newModelCatalogError(ModelCatalogUpstreamUnavailable, status, nil)
	}
}

func newModelCatalogError(code string, status int, cause error) *ModelCatalogError {
	return &ModelCatalogError{Code: code, HTTPStatus: status, cause: cause}
}
