package domain

import (
	"strings"
	"testing"
	"time"
)

func TestPlanTemplateApplyFillsUnsetFields(t *testing.T) {
	tpl, ok := LookupPlanTemplate("claude-pro")
	if !ok {
		t.Fatal("claude-pro template missing")
	}
	p := &Plan{ID: "my-claude", Template: "claude-pro"}
	buckets, err := tpl.Apply(p)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != PlanSubscription || p.Currency != "USD" || p.Fee != 20 {
		t.Fatalf("template defaults not applied: %+v", p)
	}
	if len(buckets) != 2 {
		t.Fatalf("want 5h+weekly buckets, got %+v", buckets)
	}
	if p.Quota == nil || p.Quota.BucketID != TemplateBucketID("claude-pro", "5h-prompts") {
		t.Fatalf("primary bucket = %+v", p.Quota)
	}
	if len(p.QuotaRefs()) != 2 {
		t.Fatalf("stacked refs = %+v", p.QuotaRefs())
	}
	// The 5h bucket must be a rolling window.
	if d, rolling := buckets[0].Refresh.IsRolling(); !rolling || d != 5*time.Hour {
		t.Fatalf("5h bucket refresh = %v rolling=%v", d, rolling)
	}
}

func TestPlanTemplateExplicitFieldsWin(t *testing.T) {
	tpl, _ := LookupPlanTemplate("glm-pro")
	p := &Plan{ID: "glm-custom", Fee: 400, Timezone: "UTC",
		Quota: &QuotaRef{BucketID: "my-own-bucket"}}
	if _, err := tpl.Apply(p); err != nil {
		t.Fatal(err)
	}
	if p.Fee != 400 || p.Timezone != "UTC" || p.Quota.BucketID != "my-own-bucket" {
		t.Fatalf("explicit fields must win: %+v", p)
	}
	// Template buckets stack after the custom primary bucket.
	ids := make([]string, 0)
	for _, r := range p.QuotaRefs() {
		ids = append(ids, r.BucketID)
	}
	if len(ids) != 3 || ids[0] != "my-own-bucket" || !strings.HasPrefix(ids[1], "glm-pro:") {
		t.Fatalf("bucket stack = %v", ids)
	}
}

func TestPlanTemplateRegistryCoversPlatforms(t *testing.T) {
	want := []string{"codex-go", "codex-plus", "codex-pro",
		"claude-pro", "claude-max-5x", "claude-max-20x",
		"glm-lite", "glm-pro", "glm-max", "kimi-subscription"}
	have := map[string]bool{}
	for _, id := range PlanTemplateIDs() {
		have[id] = true
	}
	for _, id := range want {
		if !have[id] {
			t.Errorf("template %q missing from registry", id)
		}
	}
}
