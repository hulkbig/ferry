package egress

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"ferry/internal/domain"
)

// CredentialValue is materialized immediately before an attempt. Identity is
// optional provider-specific account metadata (for Codex it is the ChatGPT
// account id), never secret material persisted in a compiled snapshot.
type CredentialValue struct {
	Secret   string
	Identity string
}

type CredentialProvider func(context.Context) (CredentialValue, error)
type CredentialRefresher func(context.Context, CredentialValue) (CredentialValue, error)
type DynamicCallerFactory func(Base, string) Caller

// CredentialError lets a dynamic provider retain target/request scope instead
// of every resolution rejection being mistaken for an account-wide auth loss.
type CredentialError struct{ Failure *domain.Failure }

func (e *CredentialError) Error() string {
	if e == nil || e.Failure == nil {
		return "dynamic credential unavailable"
	}
	return e.Failure.Error()
}

// DynamicCaller pins the credential reference/identity in the immutable
// world while resolving the current access token for every attempt.
type DynamicCaller struct {
	template  Base
	provider  CredentialProvider
	refresher CredentialRefresher
	factory   DynamicCallerFactory
	preview   Caller
}

var (
	_ Caller   = (*DynamicCaller)(nil)
	_ Preparer = (*DynamicCaller)(nil)
)

func NewDynamicCaller(template Base, provider CredentialProvider, factory DynamicCallerFactory) *DynamicCaller {
	previewBase := template
	previewBase.Secret = ""
	return &DynamicCaller{
		template: template, provider: provider, factory: factory,
		preview: factory(previewBase, ""),
	}
}

// NewRefreshingDynamicCaller adds one same-account credential refresh/retry
// after an upstream 401. The retry happens before scheduler fallback so an
// expired access token does not unnecessarily move the conversation.
func NewRefreshingDynamicCaller(template Base, provider CredentialProvider,
	refresher CredentialRefresher, factory DynamicCallerFactory) *DynamicCaller {
	d := NewDynamicCaller(template, provider, factory)
	d.refresher = refresher
	return d
}

func (d *DynamicCaller) caller(ctx context.Context) (Caller, CredentialValue, *domain.Failure) {
	value, err := d.provider(ctx)
	if err != nil {
		var typed *CredentialError
		if errors.As(err, &typed) && typed.Failure != nil {
			snapshot := *typed.Failure
			return nil, CredentialValue{}, &snapshot
		}
		return nil, CredentialValue{}, &domain.Failure{
			Type: domain.FailAuth, Scope: domain.FailureScopeConnection,
			Message: fmt.Sprintf("resolve current credential %s: %v", d.template.CredRef, err),
		}
	}
	base := d.template
	base.Secret = value.Secret
	return d.factory(base, value.Identity), value, nil
}

func (d *DynamicCaller) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	caller, value, failure := d.caller(ctx)
	if failure != nil {
		return nil, failure
	}
	response, failure := caller.Invoke(ctx, rc)
	if !d.canRefresh(failure) {
		return response, failure
	}
	refreshed, err := d.refresher(ctx, value)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection,
			HTTPStatus: failure.HTTPStatus,
			Message:    fmt.Sprintf("refresh current credential %s after 401: %v", d.template.CredRef, err)}
	}
	base := d.template
	base.Secret = refreshed.Secret
	return d.factory(base, refreshed.Identity).Invoke(ctx, rc)
}

func (d *DynamicCaller) Stream(ctx context.Context, rc *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	caller, value, failure := d.caller(ctx)
	if failure != nil {
		return nil, nil, failure
	}
	stream, cancel, failure := caller.Stream(ctx, rc)
	if d.canRefresh(failure) {
		refreshed, err := d.refresher(ctx, value)
		if err != nil {
			return nil, nil, &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection,
				HTTPStatus: failure.HTTPStatus,
				Message:    fmt.Sprintf("refresh current credential %s after 401: %v", d.template.CredRef, err)}
		}
		base := d.template
		base.Secret = refreshed.Secret
		return d.factory(base, refreshed.Identity).Stream(ctx, rc)
	}
	if failure != nil || d.refresher == nil {
		return stream, cancel, failure
	}
	out, stop := d.retryPreEventUnauthorized(ctx, rc, value, stream, cancel)
	return out, stop, nil
}

func (d *DynamicCaller) canRefresh(failure *domain.Failure) bool {
	return d.refresher != nil && failure != nil && failure.Type == domain.FailAuth &&
		failure.HTTPStatus == 401
}

// retryPreEventUnauthorized transparently restarts a stream only when its
// very first event is an upstream 401. Once any native/canonical frame has
// been exposed, ownership remains with the original attempt.
func (d *DynamicCaller) retryPreEventUnauthorized(ctx context.Context, rc *domain.RequestContract,
	value CredentialValue, stream <-chan domain.StreamEvent, cancel func()) (<-chan domain.StreamEvent, func()) {
	out := make(chan domain.StreamEvent, 64)
	streamCtx, cancelAll := context.WithCancel(ctx)
	var cancelMu sync.Mutex
	currentCancel := cancel
	setCancel := func(next func()) {
		cancelMu.Lock()
		currentCancel = next
		cancelMu.Unlock()
	}
	cancelCurrent := func() {
		cancelMu.Lock()
		cancel := currentCancel
		cancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	stop := func() {
		cancelAll()
		cancelCurrent()
	}
	go func() {
		defer close(out)
		current := stream
		currentValue := value
		for attempt := 0; attempt < 2; attempt++ {
			sawEvent := false
			retry := false
			for event := range current {
				if !sawEvent && attempt == 0 && event.Kind == domain.EvError && d.canRefresh(event.Err) {
					cancelCurrent()
					refreshed, err := d.refresher(streamCtx, currentValue)
					if err != nil {
						out <- domain.StreamEvent{Kind: domain.EvError, Err: &domain.Failure{
							Type: domain.FailAuth, Scope: domain.FailureScopeConnection, HTTPStatus: 401,
							Message: fmt.Sprintf("refresh current credential %s after 401: %v", d.template.CredRef, err)}}
						return
					}
					base := d.template
					base.Secret = refreshed.Secret
					next, nextCancel, nextFailure := d.factory(base, refreshed.Identity).Stream(streamCtx, rc)
					if nextFailure != nil {
						out <- domain.StreamEvent{Kind: domain.EvError, Err: nextFailure}
						return
					}
					current, currentValue = next, refreshed
					setCancel(nextCancel)
					retry = true
					break
				}
				sawEvent = true
				select {
				case <-streamCtx.Done():
					return
				case out <- event:
				}
			}
			if !retry {
				return
			}
		}
	}()
	return out, stop
}

func (d *DynamicCaller) Prepare(rc *domain.RequestContract) ([]string, error) {
	prepared, err := d.PrepareDetailed(rc)
	return prepared.Notes, err
}

func (d *DynamicCaller) PrepareDetailed(rc *domain.RequestContract) (Preparation, error) {
	if detailed, ok := d.preview.(DetailedPreparer); ok {
		return detailed.PrepareDetailed(rc)
	}
	preparer, ok := d.preview.(Preparer)
	if !ok {
		return Preparation{}, fmt.Errorf("dynamic adapter cannot preview protocol conversion")
	}
	notes, err := preparer.Prepare(rc)
	return Preparation{Notes: notes}, err
}
