// Package pricingsync maintains public reference prices independently of the
// immutable billing ledger and operator-configured runtime rate cards.
package pricingsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"ferry/internal/domain"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Source struct {
	Name string
	URL  string
}

var DefaultSources = []Source{
	{Name: "litellm", URL: "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"},
	{Name: "models.dev", URL: "https://models.dev/api.json"},
}

type Rates struct {
	Input  float64  `json:"input_per_m"`
	Output float64  `json:"output_per_m"`
	Read   *float64 `json:"cache_read_per_m"`
	Write  *float64 `json:"cache_write_per_m"`
}
type Tier struct {
	Above int64 `json:"above_input_tokens"`
	Rates
}
type Price struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Rates
	Tiers []Tier `json:"tiers,omitempty"`
}
type Result struct {
	Source    string            `json:"source"`
	URL       string            `json:"url"`
	FetchedAt time.Time         `json:"fetched_at"`
	Version   string            `json:"version"`
	Cards     []domain.RateCard `json:"cards"`
	Prices    []Price           `json:"prices"`
	Warnings  []string          `json:"warnings,omitempty"`
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

func LookupSource(name string) (Source, error) {
	for _, s := range DefaultSources {
		if s.Name == name {
			return s, nil
		}
	}
	return Source{}, fmt.Errorf("unknown pricing source %q", name)
}
func Fetch(ctx context.Context, source Source) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", source.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", source.Name, resp.StatusCode)
	}
	const maxBytes = 32 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("pricing catalog exceeds 32 MiB")
	}
	return Parse(body, source, time.Now().UTC())
}

// Parse accepts provider -> models (models.dev) and the flat LiteLLM catalog.
func Parse(body []byte, source Source, now time.Time) (*Result, error) {
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode %s: %w", source.Name, err)
	}
	digest := sha256.Sum256(body)
	result := &Result{Source: source.Name, URL: source.URL, FetchedAt: now, Version: fmt.Sprintf("%s:sha256:%x", source.Name, digest)}
	for key, raw := range catalog {
		var entry struct {
			Models   map[string]json.RawMessage `json:"models"`
			Cost     json.RawMessage            `json:"cost"`
			Provider struct {
				Name string `json:"name"`
			} `json:"provider"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if entry.Models != nil {
			for model, modelRaw := range entry.Models {
				var m struct {
					Cost json.RawMessage `json:"cost"`
				}
				if json.Unmarshal(modelRaw, &m) == nil {
					if p, ok := modelsDevPrice(key, model, m.Cost); ok {
						result.Prices = append(result.Prices, p)
					}
				}
			}
		} else if source.Name == "litellm" {
			if p, ok := liteLLMPrice(key, raw); ok {
				result.Prices = append(result.Prices, p)
			}
		} else if p, ok := modelsDevPrice(slug(entry.Provider.Name), key, entry.Cost); ok {
			result.Prices = append(result.Prices, p)
		}
	}
	sort.Slice(result.Prices, func(i, j int) bool {
		a, b := result.Prices[i], result.Prices[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	for _, p := range result.Prices {
		card := domain.RateCard{Version: result.Version, Provider: p.Provider, Model: p.Model, Plan: domain.PlanPAYG, InputPerM: p.Input, OutputPerM: p.Output}
		if p.Read != nil {
			card.CacheReadPerM = *p.Read
		}
		if p.Write != nil {
			card.CacheWritePerM = *p.Write
		}
		result.Cards = append(result.Cards, card)
	}
	if len(result.Prices) == 0 {
		return nil, fmt.Errorf("source %s yielded no priced models", source.Name)
	}
	return result, nil
}
func number(m map[string]json.RawMessage, key string, scale float64) *float64 {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return nil
	}
	n *= scale
	if math.IsInf(n, 0) || math.IsNaN(n) {
		return nil
	}
	return &n
}
func modelsDevRates(raw json.RawMessage) (Rates, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return Rates{}, false
	}
	in, out := number(m, "input", 1), number(m, "output", 1)
	if in == nil || out == nil {
		return Rates{}, false
	}
	r := Rates{Input: *in, Output: *out, Read: number(m, "cache_read", 1), Write: number(m, "cache_write", 1)}
	if r.Read == nil {
		r.Read = number(m, "input_cache_read", 1)
	}
	if r.Write == nil {
		r.Write = number(m, "input_cache_write", 1)
	}
	return r, true
}
func modelsDevPrice(provider, model string, raw json.RawMessage) (Price, bool) {
	rates, ok := modelsDevRates(raw)
	if !ok {
		return Price{}, false
	}
	p := Price{Provider: provider, Model: model, Rates: rates}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if r, ok := modelsDevRates(m["context_over_200k"]); ok {
		p.Tiers = append(p.Tiers, Tier{Above: 200000, Rates: r})
	}
	return p, true
}

var tierPattern = regexp.MustCompile("^input_cost_per_token_above_([0-9]+)(k?)_tokens$")

func liteLLMPrice(model string, raw json.RawMessage) (Price, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return Price{}, false
	}
	var provider, mode string
	_ = json.Unmarshal(m["litellm_provider"], &provider)
	_ = json.Unmarshal(m["mode"], &mode)
	if provider == "" || (mode != "chat" && mode != "completion" && mode != "embedding") {
		return Price{}, false
	}
	rates := func(suffix string, base Rates) Rates {
		if n := number(m, "input_cost_per_token"+suffix, 1e6); n != nil {
			base.Input = *n
		}
		if n := number(m, "output_cost_per_token"+suffix, 1e6); n != nil {
			base.Output = *n
		}
		if n := number(m, "cache_read_input_token_cost"+suffix, 1e6); n != nil {
			base.Read = n
		}
		if n := number(m, "cache_creation_input_token_cost"+suffix, 1e6); n != nil {
			base.Write = n
		}
		return base
	}
	if number(m, "input_cost_per_token", 1e6) == nil || number(m, "output_cost_per_token", 1e6) == nil {
		return Price{}, false
	}
	p := Price{Provider: provider, Model: model, Rates: rates("", Rates{})}
	for key := range m {
		parts := tierPattern.FindStringSubmatch(key)
		if parts == nil {
			continue
		}
		n, _ := strconv.ParseInt(parts[1], 10, 64)
		if parts[2] == "k" {
			n *= 1000
		}
		if n > 0 {
			p.Tiers = append(p.Tiers, Tier{Above: n, Rates: rates(strings.TrimPrefix(key, "input_cost_per_token"), p.Rates)})
		}
	}
	sort.Slice(p.Tiers, func(i, j int) bool { return p.Tiers[i].Above < p.Tiers[j].Above })
	return p, true
}
func Diff(proposed, active []domain.RateCard) (newCards, changed []domain.RateCard) {
	current := map[string]domain.RateCard{}
	key := func(c domain.RateCard) string { return c.Provider + "|" + c.Model + "|" + string(c.Plan) }
	for _, c := range active {
		current[key(c)] = c
	}
	for _, c := range proposed {
		old, ok := current[key(c)]
		if !ok {
			newCards = append(newCards, c)
		} else if old.InputPerM != c.InputPerM || old.OutputPerM != c.OutputPerM || old.CacheReadPerM != c.CacheReadPerM || old.CacheWritePerM != c.CacheWritePerM {
			changed = append(changed, c)
		}
	}
	return
}
func slug(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
}
