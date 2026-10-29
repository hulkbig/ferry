package pricingsync

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type Store interface {
	RuntimeSetting(string) (string, bool, error)
	SetRuntimeSetting(string, string) error
}
type Settings struct {
	Enabled       bool   `json:"enabled"`
	IntervalHours int    `json:"interval_hours"`
	Source        string `json:"source"`
}
type Status struct {
	Settings    Settings  `json:"settings"`
	Loading     bool      `json:"loading"`
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success"`
	NextUpdate  time.Time `json:"next_update"`
	Error       string    `json:"error,omitempty"`
	Source      string    `json:"source"`
	Version     string    `json:"version"`
	URL         string    `json:"url"`
	Total       int       `json:"total"`
	Stale       bool      `json:"stale"`
}
type savedSnapshot struct {
	Result      *Result   `json:"result"`
	LastAttempt time.Time `json:"last_attempt"`
	Error       string    `json:"error,omitempty"`
}

// Cache serializes scheduled/manual updates; readers use the last successful
// immutable snapshot while a new catalog is downloading. It never changes bills.
type Cache struct {
	mu       sync.RWMutex
	once     sync.Once
	store    Store
	settings Settings
	saved    savedSnapshot
	loading  bool
	fetch    func(context.Context, Source) (*Result, error)
	byModel  map[string][]Price
}

const settingsKey = "reference_pricing_settings"
const snapshotKey = "reference_pricing_snapshot"

func NewCache(store Store) *Cache {
	c := &Cache{store: store, settings: Settings{Enabled: true, IntervalHours: 24, Source: "litellm"}, fetch: Fetch}
	if store != nil {
		if data, found, err := store.RuntimeSetting(settingsKey); err != nil {
			c.settings.Enabled = false
			c.saved.Error = "Unable to load pricing settings"
		} else if found {
			var settings Settings
			if json.Unmarshal([]byte(data), &settings) == nil && ValidateSettings(settings) == nil {
				c.settings = settings
			} else {
				c.settings.Enabled = false
				c.saved.Error = "Invalid persisted pricing settings"
			}
		}
		if data, found, err := store.RuntimeSetting(snapshotKey); err != nil {
			c.saved.Error = "Unable to load pricing cache"
		} else if found {
			var saved savedSnapshot
			if json.Unmarshal([]byte(data), &saved) == nil {
				c.saved = saved
			} else {
				c.saved.Error = "Invalid persisted pricing cache"
			}
		}
	}
	c.indexLocked()
	return c
}
func ValidateSettings(s Settings) error {
	if s.IntervalHours < 1 || s.IntervalHours > 168 {
		return fmt.Errorf("pricing interval must be between 1 and 168 hours")
	}
	_, err := LookupSource(s.Source)
	return err
}
func (c *Cache) Configure(settings Settings) error {
	if err := ValidateSettings(settings); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loading {
		return fmt.Errorf("pricing refresh in progress; retry after completion")
	}
	if c.store == nil {
		return fmt.Errorf("pricing persistence unavailable")
	}
	data, _ := json.Marshal(settings)
	if err := c.store.SetRuntimeSetting(settingsKey, string(data)); err != nil {
		return err
	}
	c.settings = settings
	return nil
}
func (c *Cache) nextLocked() time.Time {
	if !c.settings.Enabled {
		return time.Time{}
	}
	// Retry failed fetches after five minutes without hammering a failing source.
	if c.saved.Error != "" {
		return c.saved.LastAttempt.Add(5 * time.Minute)
	}
	if c.saved.Result == nil || c.saved.Result.Source != c.settings.Source {
		return time.Time{}
	}
	return c.saved.Result.FetchedAt.Add(time.Duration(c.settings.IntervalHours) * time.Hour)
}
func (c *Cache) Status(now time.Time) Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.statusLocked(now)
}
func (c *Cache) statusLocked(now time.Time) Status {
	st := Status{Settings: c.settings, Loading: c.loading, LastAttempt: c.saved.LastAttempt, Error: c.saved.Error, NextUpdate: c.nextLocked(), Stale: true}
	if r := c.saved.Result; r != nil {
		st.LastSuccess = r.FetchedAt
		st.Source = r.Source
		st.URL = r.URL
		st.Version = r.Version
		st.Total = len(r.Prices)
		st.Stale = c.saved.Error != "" || r.Source != c.settings.Source || !now.Before(r.FetchedAt.Add(time.Duration(c.settings.IntervalHours)*time.Hour))
	}
	return st
}
func (c *Cache) indexLocked() {
	c.byModel = map[string][]Price{}
	if c.saved.Result != nil {
		for _, p := range c.saved.Result.Prices {
			c.byModel[p.Model] = append(c.byModel[p.Model], p)
		}
	}
}
func (c *Cache) Refresh(ctx context.Context, now time.Time, onlyDue bool) error {
	c.mu.Lock()
	if c.loading {
		c.mu.Unlock()
		return fmt.Errorf("pricing refresh already in progress")
	}
	if onlyDue && (!c.settings.Enabled || now.Before(c.nextLocked())) {
		c.mu.Unlock()
		return nil
	}
	if c.store == nil {
		c.mu.Unlock()
		return fmt.Errorf("pricing persistence unavailable")
	}
	source, err := LookupSource(c.settings.Source)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.loading = true
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	result, fetchErr := c.fetch(ctx, source)
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.loading = false }()
	next := c.saved
	next.LastAttempt = now
	if fetchErr != nil {
		next.Error = fetchErr.Error()
	} else if result == nil || len(result.Prices) == 0 {
		fetchErr = fmt.Errorf("empty pricing catalog")
		next.Error = fetchErr.Error()
	} else {
		next.Result = result
		next.Error = ""
	}
	data, err := json.Marshal(next)
	if err == nil {
		err = c.store.SetRuntimeSetting(snapshotKey, string(data))
	}
	if err != nil {
		c.saved.LastAttempt = now
		c.saved.Error = "Unable to persist pricing cache"
		return err
	}
	c.saved = next
	c.indexLocked()
	return fetchErr
}
func (c *Cache) Start(ctx context.Context) {
	c.once.Do(func() {
		go func() {
			_ = c.Refresh(ctx, time.Now().UTC(), true)
			timer := time.NewTicker(time.Minute)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-timer.C:
					_ = c.Refresh(ctx, now.UTC(), true)
				}
			}
		}()
	})
}

type Estimate struct {
	Status    string    `json:"status"`
	Source    string    `json:"source,omitempty"`
	URL       string    `json:"url,omitempty"`
	Version   string    `json:"version,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Model     string    `json:"model"`
	Provider  string    `json:"provider,omitempty"`
	Rates     *Rates    `json:"rates,omitempty"`
	Cost      *float64  `json:"cost,omitempty"`
	TierAbove int64     `json:"tier_above_input_tokens,omitempty"`
	Stale     bool      `json:"stale"`
}

// Snapshot pins an immutable price index and its provenance for a whole report.
// Refresh replaces the index rather than changing a snapshot already in use.
type Snapshot struct {
	Status  Status
	byModel map[string][]Price
}

func (c *Cache) Snapshot(now time.Time) Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{Status: c.statusLocked(now), byModel: c.byModel}
}

func (c *Cache) Estimate(model, provider string, input, output, read, write int64, now time.Time) Estimate {
	return c.Snapshot(now).Estimate(model, provider, input, output, read, write)
}

// Estimate uses an exact catalog model. Ambiguous providers are never picked
// using map iteration order. This is standard text-token reference value only.
func (s Snapshot) Estimate(model, provider string, input, output, read, write int64) Estimate {
	e := Estimate{Status: "catalog_unavailable", Model: model, Source: s.Status.Source,
		URL: s.Status.URL, Version: s.Status.Version, UpdatedAt: s.Status.LastSuccess, Stale: s.Status.Stale}
	if len(s.byModel) == 0 {
		return e
	}
	candidates := s.byModel[model]
	if len(candidates) == 0 {
		e.Status = "model_not_found"
		return e
	}
	var selected *Price
	if provider != "" {
		for i := range candidates {
			if candidates[i].Provider == provider {
				selected = &candidates[i]
				break
			}
		}
	}
	if selected == nil {
		if len(candidates) != 1 {
			e.Status = "ambiguous_model"
			return e
		}
		selected = &candidates[0]
	}
	rates := selected.Rates
	for _, tier := range selected.Tiers {
		if input > tier.Above {
			rates = tier.Rates
			e.TierAbove = tier.Above
		}
	}
	e.Provider = selected.Provider
	e.Rates = &rates
	if input <= 0 && output <= 0 {
		e.Status = "usage_unavailable"
		return e
	}
	if input < 0 || output < 0 || read < 0 || write < 0 || input < read+write {
		e.Status = "usage_unavailable"
		return e
	}
	if (read > 0 && rates.Read == nil) || (write > 0 && rates.Write == nil) {
		e.Status = "cache_price_missing"
		return e
	}
	value := (float64(input-read-write)*rates.Input + float64(output)*rates.Output) / 1e6
	if read > 0 {
		value += float64(read) * (*rates.Read) / 1e6
	}
	if write > 0 {
		value += float64(write) * (*rates.Write) / 1e6
	}
	e.Status = "available"
	e.Cost = &value
	return e
}
