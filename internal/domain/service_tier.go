package domain

import (
	"fmt"
	"strings"
)

// NormalizeServiceTier validates the processing tier before capability routing.
// Fast remains an alias here; only the Codex boundary rewrites it to priority.
func NormalizeServiceTier(value string) (string, error) {
	tier := strings.ToLower(strings.TrimSpace(value))
	switch tier {
	case "", "auto", "default", "flex", "priority", "fast":
		return tier, nil
	case "ultrafast":
		return "", fmt.Errorf("service_tier ultrafast requires a separately verified capability and is not supported")
	default:
		return "", fmt.Errorf("unsupported service_tier %q", value)
	}
}
