package egress

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"ferry/internal/domain"
)

type dynamicProbe struct {
	secret string
	seen   *[]string
	mu     *sync.Mutex
}

type refreshingProbe struct {
	secret string
	seen   *[]string
	mu     *sync.Mutex
}

type committedUnauthorizedProbe struct{}

func (*committedUnauthorizedProbe) Invoke(context.Context, *domain.RequestContract) (*domain.Response, *domain.Failure) {
	return &domain.Response{}, nil
}

func (*committedUnauthorizedProbe) Stream(context.Context, *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	ch := make(chan domain.StreamEvent, 2)
	ch <- domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: "visible"}
	ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
		Type: domain.FailAuth, Scope: domain.FailureScopeConnection, HTTPStatus: 401, Message: "late auth"}}
	close(ch)
	return ch, func() {}, nil
}

func (p *refreshingProbe) Invoke(context.Context, *domain.RequestContract) (*domain.Response, *domain.Failure) {
	p.mu.Lock()
	*p.seen = append(*p.seen, p.secret)
	p.mu.Unlock()
	if p.secret == "expired" {
		return nil, &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection, HTTPStatus: 401, Message: "expired"}
	}
	return &domain.Response{}, nil
}

func (p *refreshingProbe) Stream(context.Context, *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	p.mu.Lock()
	*p.seen = append(*p.seen, p.secret)
	p.mu.Unlock()
	ch := make(chan domain.StreamEvent, 1)
	if p.secret == "expired" {
		ch <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
			Type: domain.FailAuth, Scope: domain.FailureScopeConnection, HTTPStatus: 401, Message: "expired"}}
	} else {
		ch <- domain.StreamEvent{Kind: domain.EvTextDelta, TextDelta: "ok"}
	}
	close(ch)
	return ch, func() {}, nil
}

func (p *dynamicProbe) Prepare(*domain.RequestContract) ([]string, error) { return nil, nil }
func (p *dynamicProbe) Invoke(context.Context, *domain.RequestContract) (*domain.Response, *domain.Failure) {
	p.mu.Lock()
	*p.seen = append(*p.seen, p.secret)
	p.mu.Unlock()
	return &domain.Response{}, nil
}
func (p *dynamicProbe) Stream(context.Context, *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	ch := make(chan domain.StreamEvent)
	close(ch)
	return ch, func() {}, nil
}

func TestDynamicCallerResolvesCredentialForEveryAttempt(t *testing.T) {
	var providerCalls int
	var seen []string
	var mu sync.Mutex
	caller := NewDynamicCaller(Base{CredRef: "oauth:codex:work"}, func(context.Context) (CredentialValue, error) {
		providerCalls++
		return CredentialValue{Secret: "token-" + string(rune('0'+providerCalls))}, nil
	}, func(base Base, _ string) Caller {
		return &dynamicProbe{secret: base.Secret, seen: &seen, mu: &mu}
	})
	if _, err := caller.Prepare(&domain.RequestContract{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, failure := caller.Invoke(context.Background(), &domain.RequestContract{}); failure != nil {
			t.Fatal(failure)
		}
	}
	if providerCalls != 2 || len(seen) != 2 || seen[0] != "token-1" || seen[1] != "token-2" {
		t.Fatalf("provider calls=%d secrets=%v", providerCalls, seen)
	}
}

func TestDynamicCallerCredentialFailureIsConnectionScoped(t *testing.T) {
	caller := NewDynamicCaller(Base{CredRef: "oauth:codex:work"}, func(context.Context) (CredentialValue, error) {
		return CredentialValue{}, errors.New("refresh rejected")
	}, func(base Base, _ string) Caller {
		return &dynamicProbe{secret: base.Secret, seen: &[]string{}, mu: &sync.Mutex{}}
	})
	_, failure := caller.Invoke(context.Background(), &domain.RequestContract{})
	if failure == nil || failure.Type != domain.FailAuth || failure.Scope != domain.FailureScopeConnection {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestDynamicCallerRefreshesOnceAfterUnauthorized(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	refreshes := 0
	caller := NewRefreshingDynamicCaller(Base{CredRef: "oauth:codex:work"},
		func(context.Context) (CredentialValue, error) {
			return CredentialValue{Secret: "expired", Identity: "old"}, nil
		},
		func(_ context.Context, failed CredentialValue) (CredentialValue, error) {
			refreshes++
			if failed.Secret != "expired" {
				t.Fatalf("failed credential=%+v", failed)
			}
			return CredentialValue{Secret: "fresh", Identity: "new"}, nil
		},
		func(base Base, _ string) Caller { return &refreshingProbe{secret: base.Secret, seen: &seen, mu: &mu} })
	if _, failure := caller.Invoke(context.Background(), &domain.RequestContract{}); failure != nil {
		t.Fatal(failure)
	}
	if refreshes != 1 || len(seen) != 2 || seen[0] != "expired" || seen[1] != "fresh" {
		t.Fatalf("refreshes=%d seen=%v", refreshes, seen)
	}
}

func TestDynamicCallerRefreshesCodexImageOAuthAfterUnauthorized(t *testing.T) {
	var authorizations []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer expired" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"expired"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"b64_json": "aW1hZ2U="}}})
	}))
	defer srv.Close()

	refreshes := 0
	caller := NewRefreshingDynamicCaller(Base{
		HTTP: srv.Client(), Endpoint: &domain.Endpoint{BaseURL: srv.URL, Protocol: domain.ProtocolOpenAIImages},
		CredRef: domain.OAuthRef("work"), Model: "gpt-image-2",
	}, func(context.Context) (CredentialValue, error) {
		return CredentialValue{Secret: "expired", Identity: "account-old"}, nil
	}, func(context.Context, CredentialValue) (CredentialValue, error) {
		refreshes++
		return CredentialValue{Secret: "fresh", Identity: "account-new"}, nil
	}, func(base Base, accountID string) Caller {
		return NewCodexImages(base, accountID)
	})
	response, failure := caller.Invoke(context.Background(), &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: "gpt-image-2", Operation: "generation",
		Native: json.RawMessage(`{"model":"gpt-image-2","prompt":"test"}`),
	})
	if failure != nil || response == nil || refreshes != 1 || len(authorizations) != 2 ||
		authorizations[0] != "Bearer expired" || authorizations[1] != "Bearer fresh" {
		t.Fatalf("response=%+v failure=%+v refreshes=%d authorizations=%v",
			response, failure, refreshes, authorizations)
	}
}

func TestDynamicCallerRefreshesPreEventUnauthorizedStream(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	refreshes := 0
	caller := NewRefreshingDynamicCaller(Base{CredRef: "oauth:codex:work"},
		func(context.Context) (CredentialValue, error) { return CredentialValue{Secret: "expired"}, nil },
		func(context.Context, CredentialValue) (CredentialValue, error) {
			refreshes++
			return CredentialValue{Secret: "fresh"}, nil
		},
		func(base Base, _ string) Caller {
			return &refreshingProbe{secret: base.Secret, seen: &seen, mu: &mu}
		})
	stream, cancel, failure := caller.Stream(context.Background(), &domain.RequestContract{})
	if failure != nil {
		t.Fatal(failure)
	}
	defer cancel()
	var events []domain.StreamEvent
	for event := range stream {
		events = append(events, event)
	}
	if refreshes != 1 || len(seen) != 2 || seen[0] != "expired" || seen[1] != "fresh" ||
		len(events) != 1 || events[0].Kind != domain.EvTextDelta || events[0].TextDelta != "ok" {
		t.Fatalf("refreshes=%d seen=%v events=%+v", refreshes, seen, events)
	}
}

func TestDynamicCallerNeverRefreshesAfterVisibleStreamEvent(t *testing.T) {
	refreshes := 0
	caller := NewRefreshingDynamicCaller(Base{CredRef: "oauth:codex:work"},
		func(context.Context) (CredentialValue, error) { return CredentialValue{Secret: "expired"}, nil },
		func(context.Context, CredentialValue) (CredentialValue, error) {
			refreshes++
			return CredentialValue{Secret: "fresh"}, nil
		},
		func(Base, string) Caller { return &committedUnauthorizedProbe{} })
	stream, cancel, failure := caller.Stream(context.Background(), &domain.RequestContract{})
	if failure != nil {
		t.Fatal(failure)
	}
	defer cancel()
	var events []domain.StreamEvent
	for event := range stream {
		events = append(events, event)
	}
	if refreshes != 0 || len(events) != 2 || events[0].TextDelta != "visible" || events[1].Kind != domain.EvError {
		t.Fatalf("post-commit refreshes=%d events=%+v", refreshes, events)
	}
}
