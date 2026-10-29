package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExpandsPlanTemplates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server: {}
plans:
  - id: my-claude
    template: claude-pro
endpoints:
  - id: ep
    base_url: https://example.com
    protocol: openai-chat
resource_targets:
  - id: t1
    endpoint_id: ep
    credential: {secret_ref: env:X}
    model: m
    plan_id: my-claude
pools:
  - id: p
    targets: [t1]
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Plans) != 1 {
		t.Fatalf("plans = %+v", f.Plans)
	}
	p := f.Plans[0]
	if p.Kind != "subscription" || p.Fee != 20 || p.Quota == nil {
		t.Fatalf("template not expanded: %+v", p)
	}
	bucketIDs := map[string]bool{}
	for _, b := range f.QuotaBuckets {
		bucketIDs[b.ID] = true
	}
	if !bucketIDs["claude-pro:5h-prompts"] || !bucketIDs["claude-pro:weekly-prompts"] {
		t.Fatalf("materialized buckets missing: %+v", f.QuotaBuckets)
	}
}

func TestLoadRejectsUnknownTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server: {}
plans:
  - id: bad
    template: no-such-template
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown template must fail config load")
	}
}
