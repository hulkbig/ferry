package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/domain"
	"ferry/internal/secretstore"
)

type modelCatalogRoundTripFunc func(*http.Request) (*http.Response, error)

func (f modelCatalogRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testCatalogManager(t *testing.T, transport http.RoundTripper) *Manager {
	t.Helper()
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(store)
	m.hc.Transport = transport
	m.hc.Timeout = 0
	m.tokens["work"] = &Token{
		AccessToken:  "secret-access-token",
		RefreshToken: "secret-refresh-token",
		AccountID:    "secret-account-id",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	return m
}

func modelCatalogResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestDiscoverModelsRequestContractAndFiltering(t *testing.T) {
	var calls int
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Errorf("method = %s", req.Method)
		}
		wantURL := codexModelsURL + "?client_version=" + domain.CodexClientVersion
		if req.URL.String() != wantURL {
			t.Errorf("URL = %s, want %s", req.URL, wantURL)
		}
		wantHeaders := map[string]string{
			"Accept":             "application/json",
			"Authorization":      "Bearer secret-access-token",
			"ChatGPT-Account-ID": "secret-account-id",
			"Originator":         domain.CodexOriginator,
			"User-Agent":         domain.CodexUserAgent,
			"Version":            domain.CodexClientVersion,
		}
		for name, want := range wantHeaders {
			if got := req.Header.Get(name); got != want {
				t.Errorf("header %s = %q, want %q", name, got, want)
			}
		}
		if got := req.Header.Get("Content-Type"); got != "" {
			t.Errorf("bodyless catalog GET has Content-Type %q", got)
		}
		if req.Body != nil {
			t.Error("catalog GET unexpectedly has a body")
		}
		return modelCatalogResponse(http.StatusOK, `{"models":[
			{"slug":" zeta ","visibility":"list"},
			{"slug":"alpha","visibility":"LIST"},
			{"slug":"alpha","visibility":"list"},
			{"slug":"hidden","visibility":"hide"},
			{"slug":"none","visibility":"none"},
			{"slug":"  ","visibility":"list"}
		]}`), nil
	}))

	models, err := m.DiscoverModels(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("catalog calls = %d, want 1", calls)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(catalogSlugs(models), want) {
		t.Fatalf("models = %#v, want %#v", catalogSlugs(models), want)
	}
}

func TestDiscoverModelsRefreshesOnceAfterUnauthorized(t *testing.T) {
	var modelCalls, refreshCalls int
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case codexModelsURL + "?client_version=" + domain.CodexClientVersion:
			modelCalls++
			if modelCalls == 1 {
				if got := req.Header.Get("Authorization"); got != "Bearer secret-access-token" {
					t.Errorf("initial authorization = %q", got)
				}
				return modelCatalogResponse(http.StatusUnauthorized, `{"private":"must-not-surface"}`), nil
			}
			if got := req.Header.Get("Authorization"); got != "Bearer refreshed-access-token" {
				t.Errorf("retried authorization = %q", got)
			}
			return modelCatalogResponse(http.StatusOK, `{"models":[{"slug":"gpt-new","visibility":"list"}]}`), nil
		case TokenURL:
			refreshCalls++
			return modelCatalogResponse(http.StatusOK, `{"access_token":"refreshed-access-token","refresh_token":"rotated-refresh-token","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected outbound URL %s", req.URL)
			return nil, errors.New("unexpected URL")
		}
	}))

	models, err := m.DiscoverModels(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalogSlugs(models), []string{"gpt-new"}) {
		t.Fatalf("models = %#v", catalogSlugs(models))
	}
	if modelCalls != 2 || refreshCalls != 1 {
		t.Fatalf("model calls=%d refresh calls=%d, want 2/1", modelCalls, refreshCalls)
	}
}

func TestDiscoverModelsStopsAfterOneUnauthorizedRefresh(t *testing.T) {
	var modelCalls, refreshCalls int
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == TokenURL {
			refreshCalls++
			return modelCatalogResponse(http.StatusOK, `{"access_token":"refreshed-access-token","expires_in":3600}`), nil
		}
		modelCalls++
		return modelCatalogResponse(http.StatusUnauthorized, `{"secret":"response-body"}`), nil
	}))

	_, err := m.DiscoverModels(context.Background(), "work")
	var catalogErr *ModelCatalogError
	if !errors.As(err, &catalogErr) || catalogErr.Code != ModelCatalogAuthUnavailable || catalogErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error = %#v", err)
	}
	if modelCalls != 2 || refreshCalls != 1 {
		t.Fatalf("model calls=%d refresh calls=%d, want 2/1", modelCalls, refreshCalls)
	}
	for _, secret := range []string{"secret-access-token", "secret-account-id", "response-body"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("safe error leaked %q: %v", secret, err)
		}
	}
}

func TestDiscoverModelsClassifiesSafeFailures(t *testing.T) {
	tooManyModels := make([]string, codexModelsMaxModelCount+1)
	for i := range tooManyModels {
		tooManyModels[i] = `{"slug":"duplicate","visibility":"list"}`
	}
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
	}{
		{name: "forbidden", status: http.StatusForbidden, body: `{"secret":"entitlement-body"}`, wantCode: ModelCatalogEntitlementDenied, wantStatus: http.StatusForbidden},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"secret":"rate-body"}`, wantCode: ModelCatalogRateLimited, wantStatus: http.StatusTooManyRequests},
		{name: "upstream", status: http.StatusBadGateway, body: `{"secret":"upstream-body"}`, wantCode: ModelCatalogUpstreamUnavailable, wantStatus: http.StatusBadGateway},
		{name: "malformed", status: http.StatusOK, body: `{"models":[`, wantCode: ModelCatalogInvalidCatalog, wantStatus: http.StatusOK},
		{name: "empty", status: http.StatusOK, body: `{"models":[]}`, wantCode: ModelCatalogInvalidCatalog, wantStatus: http.StatusOK},
		{name: "empty slug", status: http.StatusOK, body: `{"models":[{"slug":"  ","visibility":"hide"}]}`, wantCode: ModelCatalogInvalidCatalog, wantStatus: http.StatusOK},
		{name: "too many models", status: http.StatusOK, body: `{"models":[` + strings.Join(tooManyModels, ",") + `]}`, wantCode: ModelCatalogInvalidCatalog, wantStatus: http.StatusOK},
		{name: "oversize", status: http.StatusOK, body: strings.Repeat("x", codexModelsMaxBodyBytes+1), wantCode: ModelCatalogInvalidCatalog, wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := testCatalogManager(t, modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return modelCatalogResponse(test.status, test.body), nil
			}))
			_, err := m.DiscoverModels(context.Background(), "work")
			var catalogErr *ModelCatalogError
			if !errors.As(err, &catalogErr) || catalogErr.Code != test.wantCode || catalogErr.HTTPStatus != test.wantStatus {
				t.Fatalf("error = %#v", err)
			}
			for _, secret := range []string{"secret-access-token", "secret-account-id", test.body} {
				if len(secret) < 256 && strings.Contains(err.Error(), secret) {
					t.Fatalf("safe error leaked response/identity data: %v", err)
				}
			}
		})
	}
}

func TestDiscoverModelsRequiresAccountIdentity(t *testing.T) {
	var calls int
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return modelCatalogResponse(http.StatusOK, `{}`), nil
	}))
	m.tokens["work"].AccountID = ""
	_, err := m.DiscoverModels(context.Background(), "work")
	var catalogErr *ModelCatalogError
	if !errors.As(err, &catalogErr) || catalogErr.Code != ModelCatalogAccountIdentityUnavailable {
		t.Fatalf("error = %#v", err)
	}
	if calls != 0 {
		t.Fatalf("catalog calls = %d, want 0", calls)
	}
}

func TestDiscoverModelsRefusesRedirect(t *testing.T) {
	var calls int
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		resp := modelCatalogResponse(http.StatusFound, "")
		resp.Header.Set("Location", "https://example.invalid/steal")
		return resp, nil
	}))
	_, err := m.DiscoverModels(context.Background(), "work")
	var catalogErr *ModelCatalogError
	if !errors.As(err, &catalogErr) || catalogErr.Code != ModelCatalogUpstreamUnavailable || catalogErr.HTTPStatus != http.StatusFound {
		t.Fatalf("error = %#v", err)
	}
	if calls != 1 {
		t.Fatalf("redirect transport calls = %d, want 1", calls)
	}
}

func TestDiscoverModelsHonorsCancellationAndDeadline(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{name: "canceled", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		}, want: context.Canceled},
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Millisecond)
		}, want: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := testCatalogManager(t, modelCatalogRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}))
			ctx, cancel := test.ctx()
			defer cancel()
			_, err := m.DiscoverModels(ctx, "work")
			var catalogErr *ModelCatalogError
			if !errors.As(err, &catalogErr) || catalogErr.Code != ModelCatalogUpstreamUnavailable || !errors.Is(err, test.want) {
				t.Fatalf("error = %#v, want %v", err, test.want)
			}
		})
	}
}

func TestCatalogReadAndDiscoveryShareConcurrentAccountRequest(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		return modelCatalogResponse(http.StatusOK, `{"models":[
			{"slug":"gpt-shared","visibility":"list"},
			{"slug":"gpt-hidden","visibility":"hide"}
		]}`), nil
	}))

	const workers = 8
	begin := make(chan struct{})
	results := make(chan error, workers)
	for index := range workers {
		go func() {
			<-begin
			read := m.DiscoverModels
			want := []string{"gpt-shared"}
			if index%2 == 0 {
				read = m.ReadModelCatalog
				want = []string{"gpt-hidden", "gpt-shared"}
			}
			models, err := read(context.Background(), "work")
			if err == nil && !reflect.DeepEqual(catalogSlugs(models), want) {
				err = errors.New("unexpected models")
			}
			results <- err
		}()
	}
	close(begin)
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range workers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("catalog calls = %d, want 1", got)
	}
}
func catalogSlugs(models []CatalogModel) []string {
	slugs := make([]string, 0, len(models))
	for _, model := range models {
		slugs = append(slugs, model.Slug)
	}
	return slugs
}

func TestDiscoverModelsParsesCapabilityMetadata(t *testing.T) {
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return modelCatalogResponse(http.StatusOK, `{"models":[
			{"slug":"gpt-new-text","visibility":"list","context_window":372000,"max_context_window":400000,
			 "input_modalities":["text","image"],"supported_reasoning_levels":["low","high"]},
			{"slug":"gpt-new-visionless","visibility":"list","context_window":272000,
			 "input_modalities":["text"]},
			{"slug":"gpt-image-next","visibility":"list","input_modalities":["image"]},
			{"slug":"gpt-legacy-undisclosed","visibility":"list"}
		]}`), nil
	}))

	models, err := m.DiscoverModels(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	bySlug := map[string]CatalogModel{}
	for _, model := range models {
		bySlug[model.Slug] = model
	}
	if got := bySlug["gpt-new-text"]; got.ContextWindow != 400000 || !got.TextInput || !got.ImageInput {
		t.Fatalf("gpt-new-text metadata = %+v", got)
	}
	if got := bySlug["gpt-new-visionless"]; got.ContextWindow != 272000 || !got.TextInput || got.ImageInput {
		t.Fatalf("gpt-new-visionless metadata = %+v", got)
	}
	if got := bySlug["gpt-image-next"]; got.TextInput || !got.ImageInput || got.ContextWindow != 0 {
		t.Fatalf("gpt-image-next metadata = %+v", got)
	}
	if got := bySlug["gpt-legacy-undisclosed"]; !got.TextInput || !got.ImageInput || got.ContextWindow != 0 {
		t.Fatalf("undisclosed modalities must keep the upstream text+image default: %+v", got)
	}
}

func TestCatalogConcurrentVersionsDoNotShareResults(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		version := req.URL.Query().Get("client_version")
		if req.Header.Get("Version") != version || req.Header.Get("User-Agent") != domain.CodexUserAgentForVersion(version) {
			t.Error("version envelope mismatch")
		}
		started <- version
		<-release
		return modelCatalogResponse(200, `{"models":[{"slug":"`+version+`","visibility":"list"}]}`), nil
	}))
	var wg sync.WaitGroup
	for _, version := range []string{"0.153.0", "0.154.0"} {
		wg.Add(1)
		go func(version string) {
			defer wg.Done()
			models, err := m.DiscoverModelsWithVersion(context.Background(), "work", version)
			if err != nil || len(models) != 1 || models[0].Slug != version {
				t.Errorf("version %s: %v %v", version, models, err)
			}
		}(version)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("different versions shared a flight")
		}
	}
	close(release)
	wg.Wait()
}
