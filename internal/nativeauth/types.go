// Package nativeauth owns native provider login contracts. Secrets remain in
// the existing secret store; no external proxy process or management key is used.
package nativeauth

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var ErrPending = errors.New("authorization pending")
var ErrSlowDown = errors.New("authorization polling must slow down")
var ErrLoginRequired = errors.New("a new provider login is required")

type Info struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mode     string `json:"mode"` // browser, device, or import
	BaseURL  string `json:"base_url"`
	Protocol string `json:"protocol"` // Ferry wire contract used by caller
}
type Token struct {
	BindingID    string            `json:"-"` // local login generation, never sent upstream
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time         `json:"expires_at,omitempty"`
	Identity     string            `json:"identity,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}
type LoginOptions struct {
	State    string
	Verifier string
	// Optional provider-specific setup (e.g. Google project/client configuration).
	Fields map[string]string
}
type Session struct {
	State           string            `json:"state"`
	Verifier        string            `json:"verifier,omitempty"`
	DeviceCode      string            `json:"device_code,omitempty"`
	UserCode        string            `json:"user_code,omitempty"`
	URL             string            `json:"url,omitempty"`
	ExpiresAt       time.Time         `json:"expires_at"`
	IntervalSeconds int               `json:"interval_seconds,omitempty"`
	Extra           map[string]string `json:"extra,omitempty"`
}
type Driver interface {
	Info() Info
	Begin(context.Context, *http.Client, LoginOptions) (Session, error)
	Complete(context.Context, *http.Client, Session, string) (Token, error)
	Refresh(context.Context, *http.Client, Token) (Token, error)
}

// EndpointBinding is an optional driver contract for account-specific endpoints.
// The manager resolves it from stored credentials; transports recheck the same
// binding after refresh, before sending credentials. It must not perform I/O.
type EndpointBinding interface {
	Binding(Token) (baseURL string, protocol string, err error)
}

// EndpointValidator checks configuration without opening the credential store.
// Passing this check does not establish account access or model capabilities.
type EndpointValidator interface {
	ValidateEndpoint(baseURL, protocol string) error
}

// NonRotatingRefresher explicitly guarantees that retrying a refresh after an
// unknown result cannot consume or rotate the credential. Absence means false.
type NonRotatingRefresher interface {
	RefreshIsNonRotating() bool
}
