package domain

import (
	"fmt"
	"regexp"
	"strings"
)

var codexVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)

// Empty configurations retain the bundled compatible version.
func EffectiveCodexClientVersion(version string) string {
	if version == "" {
		return CodexClientVersion
	}
	return version
}

func ValidateCodexClientVersion(version string) error {
	if version != "" && (len(version) > 64 || !codexVersionPattern.MatchString(version)) {
		return fmt.Errorf("codex_client_version must be a semantic version (for example 0.153.0), or empty for the bundled default")
	}
	return nil
}

func CodexUserAgentForVersion(version string) string {
	return strings.ReplaceAll(CodexUserAgent, CodexClientVersion, EffectiveCodexClientVersion(version))
}
