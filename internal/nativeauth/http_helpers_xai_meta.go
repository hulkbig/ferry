package nativeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const xmBodyLimit = 1 << 20
const xmDeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// xmURL uses exact hosts, never suffix matches. Endpoint URLs cannot carry
// credentials, query parameters, fragments, alternate ports, or escaped paths.
func xmURL(raw string, query bool, hosts ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawPath != "" || (!query && (u.RawQuery != "" || u.ForceQuery)) {
		return false
	}
	for _, host := range hosts {
		if u.Host == host {
			return true
		}
	}
	return false
}

// xmJSON retains the caller's transport and timeout, but never follows redirects
// or uses its cookie jar. Neither transport errors nor response bodies escape.
func xmJSON(ctx context.Context, client *http.Client, endpoint, contentType, body, bearer string, out any) error {
	if client == nil {
		return errors.New("native auth requires an HTTP client")
	}
	if ctx == nil {
		return errors.New("native auth requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if body != "" {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	}
	if err != nil {
		return errors.New("invalid native auth request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Ferry/nativeauth")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	safe := *client
	safe.Jar = nil
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safe.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("native auth transport failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, xmBodyLimit+1))
	if err != nil {
		return errors.New("native auth response read failed")
	}
	if len(raw) > xmBodyLimit {
		return errors.New("native auth response too large")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrLoginRequired
	}
	// Only known OAuth codes are exposed; arbitrary provider text may contain secrets.
	var failure struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &failure)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		switch failure.Error {
		case "authorization_pending":
			return ErrPending
		case "slow_down":
			return ErrSlowDown
		case "expired_token", "access_denied", "invalid_grant":
			return ErrLoginRequired
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("native auth HTTP status %d", resp.StatusCode)
	}
	if failure.Error != "" {
		return errors.New("native auth provider rejected request")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("invalid native auth JSON response")
	}
	return nil
}

func xmForm(ctx context.Context, c *http.Client, endpoint string, form url.Values, out any) error {
	return xmJSON(ctx, c, endpoint, "application/x-www-form-urlencoded", form.Encode(), "", out)
}

type xmDevice struct {
	DeviceCode  string `json:"device_code"`
	UserCode    string `json:"user_code"`
	URI         string `json:"verification_uri"`
	CompleteURI string `json:"verification_uri_complete"`
	ExpiresIn   int64  `json:"expires_in"`
	Interval    int    `json:"interval"`
}

func xmSession(o LoginOptions, d xmDevice, maxSeconds int64, hosts ...string) (Session, error) {
	if strings.TrimSpace(d.DeviceCode) == "" || strings.TrimSpace(d.UserCode) == "" || d.ExpiresIn <= 0 || d.Interval < 0 {
		return Session{}, errors.New("invalid device authorization response")
	}
	if d.URI != "" && !xmURL(d.URI, true, hosts...) {
		return Session{}, errors.New("invalid verification URL")
	}
	if d.CompleteURI != "" && !xmURL(d.CompleteURI, true, hosts...) {
		return Session{}, errors.New("invalid verification URL")
	}
	target := d.CompleteURI
	if target == "" {
		target = d.URI
	}
	if target == "" {
		return Session{}, errors.New("missing verification URL")
	}
	if d.ExpiresIn > maxSeconds {
		d.ExpiresIn = maxSeconds
	}
	if d.Interval < 5 {
		d.Interval = 5
	}
	return Session{State: o.State, Verifier: o.Verifier, DeviceCode: d.DeviceCode, UserCode: d.UserCode, URL: target, ExpiresAt: time.Now().Add(time.Duration(d.ExpiresIn) * time.Second), IntervalSeconds: d.Interval}, nil
}

func xmCheckSession(s Session) error {
	if strings.TrimSpace(s.DeviceCode) == "" || s.ExpiresAt.IsZero() || !time.Now().Before(s.ExpiresAt) {
		return ErrLoginRequired
	}
	return nil
}

func xmExpiry(seconds int64) (time.Time, error) {
	if seconds < 0 || seconds > int64((1<<63-1)/int64(time.Second)) {
		return time.Time{}, errors.New("invalid token lifetime")
	}
	if seconds == 0 {
		return time.Time{}, nil
	}
	return time.Now().Add(time.Duration(seconds) * time.Second), nil
}
