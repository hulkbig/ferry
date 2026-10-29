package nativeauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const ckBodyLimit = 1 << 20
const ckTimeout = 30 * time.Second

var ckInvalid = errors.New("native authentication: invalid input or response")
var ckRequestFailed = errors.New("native authentication: upstream request failed")

// ckPost preserves the supplied transport (including egress policy), but neither
// follows redirects nor shares cookies. Never expose transport/body errors.
func ckPost(ctx context.Context, client *http.Client, endpoint, contentType, body string, headers map[string]string, out any) error {
	if client == nil || ctx == nil {
		return ckInvalid
	}
	switch endpoint {
	case claudeExchangeEndpoint, "https://auth.kimi.com/api/oauth/device_authorization", "https://auth.kimi.ai/api/oauth/device_authorization", "https://auth.kimi.com/api/oauth/token", "https://auth.kimi.ai/api/oauth/token":
	default:
		return ckInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, ckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return ckInvalid
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "Ferry/nativeauth")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	c := *client
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.Timeout <= 0 || c.Timeout > ckTimeout {
		c.Timeout = ckTimeout
	}
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ckRequestFailed
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, ckBodyLimit+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ckRequestFailed
	}
	if len(data) > ckBodyLimit {
		return ckInvalid
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrLoginRequired
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return ErrSlowDown
	}
	// Only OAuth success/client-error statuses may carry recognized OAuth errors.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		return ckRequestFailed
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return ckInvalid
	}
	switch envelope.Error {
	case "":
	case "authorization_pending":
		return ErrPending
	case "slow_down":
		return ErrSlowDown
	case "expired_token", "access_denied", "invalid_grant", "invalid_token":
		return ErrLoginRequired
	default:
		return ckRequestFailed
	}
	if resp.StatusCode != http.StatusOK {
		return ckRequestFailed
	}
	if json.Unmarshal(data, out) != nil {
		return ckInvalid
	}
	return nil
}

func ckText(s string) bool {
	if s == "" || len(s) > 16384 || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c <= 32 || c == 127 {
			return false
		}
	}
	return true
}

func ckExpiry(seconds float64) (time.Time, error) {
	// Bound before converting to Duration; a missing expiry stays unknown.
	if seconds < 0 || seconds > 366*24*60*60 {
		return time.Time{}, ckInvalid
	}
	if seconds == 0 {
		return time.Time{}, nil
	}
	return time.Now().Add(time.Duration(seconds * float64(time.Second))), nil
}

func ckCopyExtra(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
