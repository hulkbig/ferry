package pricingsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ferry/internal/domain"
)

func TestFetchConvertsModelsDevCatalog(t *testing.T) {
	payload := `{
  "gpt-5.6": {"provider": {"name": "OpenAI"}, "cost": {"input": 1.25, "output": 10, "cache_read": 0.125}},
  "claude-sonnet-4-5": {"provider": {"name": "Anthropic"}, "cost": {"input": 3, "output": 15, "cache_read": 0.3, "input_cache_write": 3.75}},
  "free-model": {"provider": {"name": "X"}, "cost": null}
}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), Source{Name: "test", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Cards) != 2 {
		t.Fatalf("cards = %+v", result.Cards)
	}
	byModel := map[string]domain.RateCard{}
	for _, c := range result.Cards {
		byModel[c.Model] = c
	}
	gpt := byModel["gpt-5.6"]
	if gpt.Provider != "openai" || gpt.InputPerM != 1.25 || gpt.CacheReadPerM != 0.125 {
		t.Fatalf("gpt card = %+v", gpt)
	}
	claude := byModel["claude-sonnet-4-5"]
	if claude.CacheWritePerM != 3.75 {
		t.Fatalf("cache write alias not converted: %+v", claude)
	}
	if result.Version == "" || !result.FetchedAt.IsZero() == false {
		t.Fatalf("provenance missing: %+v", result)
	}
}

func TestDiffReportsNewAndChanged(t *testing.T) {
	proposed := []domain.RateCard{
		{Version: "v2", Model: "a", Plan: domain.PlanPAYG, InputPerM: 2},
		{Version: "v2", Model: "b", Plan: domain.PlanPAYG, InputPerM: 1},
	}
	active := []domain.RateCard{
		{Version: "v1", Model: "a", Plan: domain.PlanPAYG, InputPerM: 1},
	}
	newCards, changed := Diff(proposed, active)
	if len(newCards) != 1 || newCards[0].Model != "b" {
		t.Fatalf("new = %+v", newCards)
	}
	if len(changed) != 1 || changed[0].Model != "a" {
		t.Fatalf("changed = %+v", changed)
	}
}
