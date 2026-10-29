package egress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ferry/internal/domain"
)

type codexErrorDetail struct {
	Type    string
	Code    string
	Message string
	Fields  map[string]any
}

// classifyCodexHTTPFailure preserves Codex account/quota/capacity semantics
// that the generic OpenAI classifier cannot infer from status alone.
func classifyCodexHTTPFailure(status int, body string, _ http.Header) *domain.Failure {
	return codexFailure(status, []byte(body))
}

// codexFailureFromEvent decodes terminal Responses events. A nil result means
// the event is not an error terminal.
func codexFailureFromEvent(event []byte) *domain.Failure {
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(event))
	decoder.UseNumber()
	if decoder.Decode(&envelope) != nil {
		return nil
	}
	eventType := stringValue(envelope["type"])
	switch eventType {
	case "response.failed", "response.error", "error":
		return codexFailure(0, event)
	case "response.canceled", "response.cancelled":
		return &domain.Failure{Type: domain.FailStreamBroken, Scope: domain.FailureScopeStream,
			HTTPStatus: http.StatusBadGateway, Message: "codex upstream canceled the response"}
	default:
		return nil
	}
}

func codexFailure(status int, raw []byte) *domain.Failure {
	detail := extractCodexError(raw)
	if status == 0 {
		for _, key := range []string{"status_code", "status"} {
			if embedded := int(positiveFloat(detail.Fields[key])); embedded >= 400 && embedded <= 599 {
				status = embedded
				break
			}
		}
	}
	failure := &domain.Failure{HTTPStatus: status, Message: firstNonEmpty(detail.Message, detail.Code,
		truncate(strings.TrimSpace(string(raw)), 512), "codex upstream request failed")}

	kind := strings.ToLower(strings.TrimSpace(detail.Type))
	code := strings.ToLower(strings.TrimSpace(detail.Code))
	// hint deliberately excludes the raw body. Codex echoes request content
	// into invalid_request_error messages, so matching keywords against the
	// whole payload lets a prompt that merely mentions "unauthorized" or
	// "rate limit" masquerade as a provider verdict — which would force an
	// unnecessary token refresh and account rotation on a pure request error.
	hint := strings.ToLower(strings.TrimSpace(detail.Type + " " + detail.Code + " " + detail.Message))
	if hint == "" {
		hint = strings.ToLower(truncate(strings.TrimSpace(string(raw)), 512))
	}
	// An explicit request-error verdict from the provider suppresses the
	// keyword fallbacks that would otherwise escalate to connection scope.
	// A transport-level 401/403/429 still wins: that is the provider's own
	// channel-level answer, not text it copied out of the request.
	requestError := kind == "invalid_request_error" || kind == "bad_request_error"

	switch {
	case code == "context_length_exceeded" || code == "context_too_large" ||
		containsAnyFold(hint, "context_length_exceeded", "context_too_large", "context window",
			"maximum context", "too many tokens", "prompt is too long"):
		failure.Type, failure.Scope = domain.FailContextOverflow, domain.FailureScopeRequest
		defaultFailureStatus(failure, http.StatusBadRequest)
	case kind == "usage_limit_reached" || code == "usage_limit_reached" ||
		(!requestError && containsAnyFold(hint, "usage_limit_reached", "usage limit reached")):
		// Plan exhaustion: the connection stays cooled until the plan resets.
		failure.Type, failure.Scope = domain.FailQuotaExhausted, domain.FailureScopeConnection
		failure.HTTPStatus = http.StatusTooManyRequests
		failure.RetryAfter = codexResetDelay(detail.Fields, time.Now())
	case code == "cyber_policy":
		failure.Type, failure.Scope = domain.FailBadResponse, domain.FailureScopeRequest
		failure.HTTPStatus = http.StatusBadRequest
	case !requestError && containsAnyFold(hint, "selected model is at capacity", "model is at capacity"):
		// Transient supply pressure rather than plan exhaustion: a short cool
		// and a sibling identity, never the quota reset ladder.
		failure.Type, failure.Scope = domain.FailRateLimit, domain.FailureScopeConnection
		failure.HTTPStatus = http.StatusTooManyRequests
		failure.RetryAfter = codexResetDelay(detail.Fields, time.Now())
	case kind == "authentication_error" || status == http.StatusUnauthorized ||
		(!requestError && containsAnyFold(hint, "invalid_api_key", "unauthorized",
			"invalid or expired token", "refresh_token_reused")):
		failure.Type, failure.Scope = domain.FailAuth, domain.FailureScopeConnection
		failure.HTTPStatus = http.StatusUnauthorized
	case kind == "permission_error" || status == http.StatusForbidden ||
		(!requestError && containsAnyFold(hint, "permission_denied", "forbidden")):
		failure.Type, failure.Scope = domain.FailAuth, domain.FailureScopeConnection
		failure.HTTPStatus = http.StatusForbidden
	case kind == "rate_limit_error" || status == http.StatusTooManyRequests ||
		(!requestError && containsAnyFold(hint, "rate_limit_exceeded", "rate limit")):
		failure.Type, failure.Scope = domain.FailRateLimit, domain.FailureScopeConnection
		failure.HTTPStatus = http.StatusTooManyRequests
		failure.RetryAfter = codexResetDelay(detail.Fields, time.Now())
	case kind == "upstream_error" || kind == "server_error" || code == "server_error" || status >= 500:
		// A Codex provider may have several independent OAuth identities. Try a
		// sibling identity before crossing the explicitly allowlisted route.
		failure.Type, failure.Scope = domain.FailProvider5xx, domain.FailureScopeConnection
		defaultFailureStatus(failure, http.StatusBadGateway)
	case status == http.StatusRequestTimeout:
		failure.Type, failure.Scope = domain.FailConnect, domain.FailureScopeConnection
	case (requestError || status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) &&
		strings.HasPrefix(strings.ToLower(detail.Message), "invalid value") &&
		containsAnyFold(detail.Message, "supported values are"):
		// An invalid enum value (for example reasoning.effort=ultra) is a
		// request error. Do not rotate targets or silently lower the effort.
		failure.Type, failure.Scope = domain.FailUnsupportedParam, domain.FailureScopeRequest
		defaultFailureStatus(failure, http.StatusBadRequest)
	case containsAnyFold(hint, "unsupported_parameter", "unsupported parameter", "unknown parameter"):
		// This is an upstream model/endpoint capability verdict. A sibling
		// target may support the same canonical request, so keep it recoverable
		// instead of blaming the client request globally.
		failure.Type, failure.Scope = domain.FailUnsupportedParam, domain.FailureScopeTarget
		defaultFailureStatus(failure, http.StatusBadRequest)
	case requestError || (status >= 400 && status < 500):
		failure.Type, failure.Scope = domain.FailBadResponse, domain.FailureScopeRequest
		defaultFailureStatus(failure, http.StatusBadRequest)
	default:
		failure.Type, failure.Scope = domain.FailBadResponse, domain.FailureScopeTarget
		defaultFailureStatus(failure, http.StatusBadGateway)
	}
	return failure
}

func defaultFailureStatus(failure *domain.Failure, status int) {
	if failure.HTTPStatus == 0 {
		failure.HTTPStatus = status
	}
}

func extractCodexError(raw []byte) codexErrorDetail {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return codexErrorDetail{Message: truncate(strings.TrimSpace(string(raw)), 512)}
	}
	candidate := root
	selected := false
	if response, ok := root["response"].(map[string]any); ok {
		if nested, ok := response["error"].(map[string]any); ok {
			candidate = nested
			selected = true
		} else if response["error"] != nil {
			candidate = map[string]any{"message": response["error"]}
			selected = true
		}
	}
	if !selected {
		if nested, ok := root["error"].(map[string]any); ok {
			candidate = nested
		} else if root["error"] != nil {
			candidate = map[string]any{"message": root["error"]}
		}
	}
	fields := make(map[string]any, len(root)+len(candidate))
	for key, value := range root {
		fields[key] = value
	}
	for key, value := range candidate {
		fields[key] = value
	}
	return codexErrorDetail{
		Type: stringValue(candidate["type"]), Code: stringValue(candidate["code"]),
		Message: firstNonEmpty(stringValue(candidate["message"]), stringValue(root["message"])), Fields: fields,
	}
}

func codexResetDelay(fields map[string]any, now time.Time) time.Duration {
	if fields == nil {
		return 0
	}
	for _, key := range []string{"retry_after", "retry_after_seconds", "resets_in_seconds"} {
		if seconds := positiveFloat(fields[key]); seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	if raw := fields["resets_at"]; raw != nil {
		if unix := positiveFloat(raw); unix > 0 {
			if unix > 1e12 {
				unix /= 1000
			}
			reset := time.Unix(int64(unix), 0)
			if reset.After(now) {
				return reset.Sub(now)
			}
		}
		if text := stringValue(raw); text != "" {
			if parsed, err := time.Parse(time.RFC3339, text); err == nil && parsed.After(now) {
				return parsed.Sub(now)
			}
		}
	}
	return 0
}

func positiveFloat(value any) float64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case float64:
		return typed
	case int:
		return float64(typed)
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed
	default:
		return 0
	}
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func containsAnyFold(value string, needles ...string) bool {
	value = strings.ToLower(value)
	for _, needle := range needles {
		if strings.Contains(value, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}
