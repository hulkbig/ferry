package pricingsync

import (
	"context"
	"errors"
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	data map[string]string
	fail bool
}

func (s *memoryStore) RuntimeSetting(k string) (string, bool, error) {
	v, ok := s.data[k]
	return v, ok, nil
}
func (s *memoryStore) SetRuntimeSetting(k, v string) error {
	if s.fail {
		return errors.New("disk failure")
	}
	s.data[k] = v
	return nil
}
func fixture(t *testing.T, source string, now time.Time) *Result {
	t.Helper()
	file := source
	if source == "models.dev" {
		file = "models-dev"
	}
	data, err := os.ReadFile("testdata/" + file + ".json")
	if err != nil {
		t.Fatal(err)
	}
	src, _ := LookupSource(source)
	r, err := Parse(data, src, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestRealCatalogShapesAndReferenceEstimates(t *testing.T) {
	now := time.Now().UTC()
	r := fixture(t, "litellm", now)
	if len(r.Prices) != 3 {
		t.Fatalf("invalid/non-token entries accepted: %+v", r.Prices)
	}
	store := &memoryStore{data: map[string]string{}}
	c := NewCache(store)
	c.fetch = func(context.Context, Source) (*Result, error) { return r, nil }
	if err := c.Refresh(context.Background(), now, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model                      string
		input, output, read, write int64
		status                     string
		cost                       float64
		tier                       int64
	}{
		{"sample-model", 100000, 1000, 90000, 0, "available", .24, 0},
		{"sample-model", 300000, 1000, 200000, 0, "available", 2.475, 272000},
		{"sample-model", 272000, 0, 0, 0, "available", 2.72, 0},
		{"sample-model", 100000, 1000, 80000, 10000, "available", .355, 0},
		{"no-cache-price", 100, 10, 10, 0, "cache_price_missing", 0, 0},
		{"sample-model", 0, 0, 0, 0, "usage_unavailable", 0, 0},
		{"free", 100, 10, 0, 0, "available", 0, 0},
		{"invented-alias", 100, 10, 0, 0, "model_not_found", 0, 0},
	} {
		e := c.Estimate(tc.model, "", tc.input, tc.output, tc.read, tc.write, now)
		if e.Status != tc.status || e.TierAbove != tc.tier {
			t.Fatalf("%+v: %+v", tc, e)
		}
		if tc.status == "available" && (e.Cost == nil || math.Abs(*e.Cost-tc.cost) > 1e-9) {
			t.Fatalf("wrong reference estimate %+v: %+v", tc, e)
		}
		if e.Version != r.Version || e.Stale {
			t.Fatalf("provenance: %+v", e)
		}
	}
	md := fixture(t, "models.dev", now)
	c.mu.Lock()
	c.saved.Result = md
	c.indexLocked()
	c.mu.Unlock()
	if len(md.Prices) != 3 {
		t.Fatalf("models.dev providers not parsed: %+v", md)
	}
	if e := c.Estimate("model-a", "", 100, 10, 0, 0, now); e.Status != "ambiguous_model" {
		t.Fatalf("ambiguous match: %+v", e)
	}
	if e := c.Estimate("model-a", "anthropic", 300000, 0, 0, 0, now); e.Cost == nil || *e.Cost != 1.8 || e.TierAbove != 200000 {
		t.Fatalf("provider/tier match: %+v", e)
	}
}
func TestSchedulePersistsPauseFailureAndRecovery(t *testing.T) {
	now := time.Now().UTC()
	store := &memoryStore{data: map[string]string{}}
	c := NewCache(store)
	calls := 0
	fail := false
	c.fetch = func(context.Context, Source) (*Result, error) {
		calls++
		if fail {
			return nil, errors.New("upstream unavailable")
		}
		return fixture(t, "litellm", now), nil
	}
	if err := c.Refresh(context.Background(), now, true); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background(), now.Add(time.Hour), true); err != nil || calls != 1 {
		t.Fatalf("refreshed before due: %d %v", calls, err)
	}
	paused := Settings{Enabled: false, IntervalHours: 1, Source: "litellm"}
	if err := c.Configure(paused); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background(), now.Add(48*time.Hour), true); err != nil || calls != 1 {
		t.Fatalf("pause ignored: %d %v", calls, err)
	}
	restored := NewCache(store)
	if restored.Status(now).Settings != paused || restored.Status(now).Total != 3 {
		t.Fatal("settings/cache did not survive restart")
	}
	// Manual refresh works while paused, and a failed fetch preserves prices.
	fail = true
	if err := c.Refresh(context.Background(), now, false); err == nil {
		t.Fatal("failure not returned")
	}
	st := c.Status(now)
	if st.Total != 3 || st.Error == "" || !st.Stale {
		t.Fatalf("failed sync lost cache: %+v", st)
	}
	restored = NewCache(store)
	if restored.Status(now).Total != 3 || restored.Status(now).Error == "" {
		t.Fatal("failure state not durable")
	}
	if err := c.Configure(Settings{Enabled: true, IntervalHours: 1, Source: "litellm"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background(), now.Add(time.Minute), true); err != nil || calls != 2 {
		t.Fatalf("retry has no backoff: %d %v", calls, err)
	}
	fail = false
	now = now.Add(6 * time.Minute)
	if err := c.Refresh(context.Background(), now, true); err != nil {
		t.Fatal(err)
	}
	if st = c.Status(now); st.Error != "" || st.Stale {
		t.Fatalf("recovery: %+v", st)
	}
	store.fail = true
	if err := c.Refresh(context.Background(), now.Add(time.Hour), false); err == nil {
		t.Fatal("persist failure not surfaced")
	}
	if c.Status(now).Version != restored.Status(now).Version {
		t.Fatal("persist failure replaced active catalog")
	}
}
func TestConcurrentRefreshAndCancellation(t *testing.T) {
	now := time.Now().UTC()
	c := NewCache(&memoryStore{data: map[string]string{}})
	started := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	c.fetch = func(ctx context.Context, _ Source) (*Result, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() { done <- c.Refresh(ctx, now, false) }()
	<-started
	if !c.Status(now).Loading {
		t.Fatal("loading state missing")
	}
	if err := c.Refresh(ctx, now, false); err == nil {
		t.Fatal("concurrent refresh allowed")
	}
	if err := c.Configure(Settings{Enabled: false, IntervalHours: 1, Source: "litellm"}); err == nil {
		t.Fatal("source settings can race refresh")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation ignored")
	}
	if c.Status(now).Loading || calls.Load() != 1 {
		t.Fatal("canceled refresh did not finish")
	}
}
func TestInvalidSettingsDoNotPersist(t *testing.T) {
	c := NewCache(&memoryStore{data: map[string]string{}})
	for _, s := range []Settings{{true, 0, "litellm"}, {true, 169, "litellm"}, {true, 24, "https://untrusted.test"}} {
		if err := c.Configure(s); err == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
	if c.Status(time.Now()).Settings.IntervalHours != 24 {
		t.Fatal("invalid settings applied")
	}
}

func TestReportSnapshotKeepsPricesAndProvenanceDuringRefresh(t *testing.T) {
	now := time.Now().UTC()
	c := NewCache(&memoryStore{data: map[string]string{}})
	c.fetch = func(context.Context, Source) (*Result, error) { return fixture(t, "litellm", now), nil }
	if err := c.Refresh(context.Background(), now, false); err != nil {
		t.Fatal(err)
	}
	snapshot := c.Snapshot(now)
	before := snapshot.Estimate("sample-model", "", 100000, 1000, 90000, 0)
	c.fetch = func(context.Context, Source) (*Result, error) {
		r := fixture(t, "litellm", now.Add(time.Hour))
		r.Version = "fixture:v2"
		for i := range r.Prices {
			r.Prices[i].Input *= 2
		}
		return r, nil
	}
	if err := c.Refresh(context.Background(), now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	after := snapshot.Estimate("sample-model", "", 100000, 1000, 90000, 0)
	latest := c.Estimate("sample-model", "", 100000, 1000, 90000, 0, now.Add(time.Hour))
	if before.Cost == nil || after.Cost == nil || latest.Cost == nil || *before.Cost != *after.Cost || before.Version != after.Version || !before.UpdatedAt.Equal(after.UpdatedAt) || after.Stale {
		t.Fatalf("report changed price snapshot: before=%+v after=%+v", before, after)
	}
	if latest.Version != "fixture:v2" || *latest.Cost <= *after.Cost || !latest.UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("new readers did not receive refreshed prices: %+v", latest)
	}
}
