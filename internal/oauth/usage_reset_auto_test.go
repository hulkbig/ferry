package oauth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAutomaticUsageResetExpiryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		expiry    bool
		status    string
		want      int
	}{
		{"exact threshold", 20 * time.Minute, true, "available", 1},
		{"inside", time.Second, true, "available", 1},
		{"outside", 20*time.Minute + time.Nanosecond, true, "available", 0},
		{"expired", -time.Second, true, "available", 0},
		{"expires now", 0, true, "available", 0},
		{"unknown expiry", 0, false, "available", 0},
		{"redeemed", time.Minute, true, "redeemed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f, b := resetTestManager(t)
			credit := &f.credits.Credits[0]
			credit.Status = tc.status
			credit.ExpiresAt = nil
			if tc.expiry {
				expires := m.now().Add(tc.remaining).Format(time.RFC3339Nano)
				credit.ExpiresAt = &expires
			}
			_, err := m.AutoUsageReset(context.Background(), b, 20*time.Minute, noResetGuard)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.posts) != tc.want {
				t.Fatalf("posts=%d want %d", len(f.posts), tc.want)
			}
		})
	}
}

func TestAutomaticUsageResetJournalBlocksReplay(t *testing.T) {
	for _, outcome := range []string{"reset", "already_redeemed", "nothing_to_reset", "no_credit", "uncertain", "manual"} {
		t.Run(outcome, func(t *testing.T) {
			m, f, b := resetTestManager(t)
			expires := m.now().Add(10 * time.Minute).Format(time.RFC3339Nano)
			f.credits.Credits[0].ExpiresAt = &expires
			if outcome == "manual" {
				prepareReset(t, m, b)
			} else {
				f.fail = outcome == "uncertain"
				f.response = `{"code":"` + outcome + `","windows_reset":1}`
			}
			if _, err := m.AutoUsageReset(context.Background(), b, 20*time.Minute, noResetGuard); err != nil {
				t.Fatal(err)
			}
			// Restart with the same durable journal, while upstream still reports the card available.
			restarted := NewManager(m.store)
			restarted.hc = m.hc
			restarted.now = m.now
			if _, err := restarted.AutoUsageReset(context.Background(), b, 20*time.Minute, noResetGuard); err != nil {
				t.Fatal(err)
			}
			want := 1
			if outcome == "manual" {
				want = 0
			}
			if len(f.posts) != want {
				t.Fatalf("posts=%d want %d", len(f.posts), want)
			}
		})
	}
}

func TestAutomaticUsageResetSelectsEarliestAndHonorsGuard(t *testing.T) {
	m, f, b := resetTestManager(t)
	first := m.now().Add(15 * time.Minute).Format(time.RFC3339Nano)
	second := m.now().Add(5 * time.Minute).Format(time.RFC3339Nano)
	f.credits.Credits[0].ExpiresAt = &first
	f.credits.Credits[1].ExpiresAt = &second
	guard := func() (func(), error) { return nil, errors.New("policy disabled") }
	if _, err := m.AutoUsageReset(context.Background(), b, 20*time.Minute, guard); err == nil {
		t.Fatal("guard should block")
	}
	if len(f.posts) != 0 {
		t.Fatal("guard bypassed")
	}
	// A failed pre-dispatch guard may resume the same unsubmitted automatic intent.
	r, err := m.AutoUsageReset(context.Background(), b, 20*time.Minute, noResetGuard)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Credit.ID != "credit-b" || len(f.posts) != 1 {
		t.Fatalf("request=%+v posts=%v", r, f.posts)
	}
}

func TestAutomaticUsageResetInvalidDetailsFailClosed(t *testing.T) {
	for _, kind := range []string{"invalid expiry", "duplicate id", "details unavailable", "wrong reset type", "zero threshold"} {
		t.Run(kind, func(t *testing.T) {
			m, f, b := resetTestManager(t)
			expires := m.now().Add(time.Minute).Format(time.RFC3339Nano)
			f.credits.Credits[0].ExpiresAt = &expires
			threshold := 20 * time.Minute
			switch kind {
			case "invalid expiry":
				expires = "invalid"
			case "duplicate id":
				f.credits.Credits = append(f.credits.Credits, f.credits.Credits[0])
			case "details unavailable":
				f.detailError = true
			case "wrong reset type":
				f.credits.Credits[0].ResetType = "other"
			case "zero threshold":
				threshold = 0
			}
			_, _ = m.AutoUsageReset(context.Background(), b, threshold, noResetGuard)
			if len(f.posts) != 0 {
				t.Fatal("invalid credit consumed")
			}
		})
	}
}

func TestAutomaticUsageResetConcurrentScansDoNotReplay(t *testing.T) {
	m, f, b := resetTestManager(t)
	expires := m.now().Add(time.Minute).Format(time.RFC3339Nano)
	f.credits.Credits[0].ExpiresAt = &expires
	f.response = `{"code":"nothing_to_reset"}`
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.AutoUsageReset(context.Background(), b, 20*time.Minute, noResetGuard); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(f.posts) != 1 {
		t.Fatalf("posts=%d", len(f.posts))
	}
}

func TestAutomaticUsageResetExpiresWhileWaitingForGuard(t *testing.T) {
	m, f, b := resetTestManager(t)
	now := m.now()
	expires := now.Add(time.Second).Format(time.RFC3339Nano)
	f.credits.Credits[0].ExpiresAt = &expires
	guard := func() (func(), error) {
		m.now = func() time.Time { return now.Add(2 * time.Second) }
		return func() {}, nil
	}
	if _, err := m.AutoUsageReset(context.Background(), b, 20*time.Minute, guard); err == nil {
		t.Fatal("expired dispatch accepted")
	}
	if len(f.posts) != 0 {
		t.Fatal("expired credit consumed")
	}
}
