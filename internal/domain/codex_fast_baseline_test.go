package domain

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCodexClientVersionCoversPinnedCatalog(t *testing.T) {
	parseVersion := func(value string) []int {
		t.Helper()
		var major, minor, patch int
		if n, err := fmt.Sscanf(value, "%d.%d.%d", &major, &minor, &patch); err != nil || n != 3 {
			t.Fatalf("invalid pinned version %q", value)
		}
		return []int{major, minor, patch}
	}
	client := parseVersion(CodexClientVersion)
	for model, entry := range codexFastBaseline {
		if slices.Compare(client, parseVersion(entry.MinimalClientVersion)) < 0 {
			t.Errorf("client %s cannot retrieve %s (minimum %s)", CodexClientVersion, model, entry.MinimalClientVersion)
		}
	}
	if !strings.Contains(CodexUserAgent, "codex-tui/"+CodexClientVersion) {
		t.Fatalf("user agent and catalog version disagree: %s", CodexUserAgent)
	}
}

func TestCodexFastBaselineDoesNotGrantAccountPriority(t *testing.T) {
	baseline, known := CodexFastBaseline("gpt-6-astra")
	if !known || !baseline.Priority {
		t.Fatal("pinned official Astra declaration must be represented")
	}
	if _, known := CodexFastBaseline("gpt-future"); known {
		t.Fatal("unlisted models must remain unknown")
	}
	capabilities, _ := CodexModelCapabilities("gpt-6-astra")
	if capabilities.Priority {
		t.Fatal("model baseline must not grant connection priority")
	}
	for _, model := range CodexModelsForPlan("pro") {
		if model.Capabilities.Priority {
			t.Fatalf("plan fallback granted priority for %s", model.Model)
		}
	}
}
