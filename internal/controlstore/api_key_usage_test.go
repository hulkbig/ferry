package controlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ferry/internal/config"
)

func usageTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	on := true
	if _, err := s.SaveConfig(&config.File{ConsumerKeys: []config.ConsumerKey{{Name: "bench", Key: "synthetic-usage-test", Enabled: &on}}}); err != nil {
		t.Fatal(err)
	}
	auth, err := s.APIKeyAuthenticators()
	if err != nil || len(auth) != 1 {
		t.Fatalf("auth=%v err=%v", auth, err)
	}
	id, ok := auth[0].MatchSecret("synthetic-usage-test")
	if !ok {
		t.Fatal("missing key")
	}
	return s, id
}

func TestAPIKeyUseBatchIsDurableAndMonotonic(t *testing.T) {
	s, id := usageTestStore(t)
	if _, err := s.db.Exec(`CREATE TABLE usage_writes(n INTEGER); INSERT INTO usage_writes VALUES(0); CREATE TRIGGER count_usage AFTER UPDATE OF last_used_at ON api_key_secrets BEGIN UPDATE usage_writes SET n=n+1; END;`); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 64)
	base := time.Now().Truncate(time.Millisecond)
	waitCount := s.db.Stats().WaitCount
	launch := func(i int) {
		go func(i int) {
			at := base.Add(time.Duration(i) * time.Millisecond)
			err := s.RecordAPIKeySecretUse(id, at)
			if err == nil {
				var stored int64
				err = s.db.QueryRow("SELECT last_used_at FROM api_key_secrets WHERE id=?", id).Scan(&stored)
				if err == nil && stored < at.UnixMilli() {
					err = fmt.Errorf("returned before commit: %d < %d", stored, at.UnixMilli())
				}
			}
			done <- err
		}(i)
	}
	launch(0)
	waitDeadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount == waitCount {
		if time.Now().After(waitDeadline) {
			blocker.Rollback()
			t.Fatal("first writer not waiting")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 1; i < 64; i++ {
		launch(i)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.usageMu.Lock()
		n := len(s.usagePending)
		s.usageMu.Unlock()
		if n >= 63 {
			break
		}
		if time.Now().After(deadline) {
			blocker.Rollback()
			t.Fatal("writers did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		blocker.Rollback()
		t.Fatalf("returned before durable storage unblocked: %v", err)
	default:
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("writer stuck")
		}
	}
	var stored, writes int64
	s.db.QueryRow("SELECT last_used_at FROM api_key_secrets WHERE id=?", id).Scan(&stored)
	s.db.QueryRow("SELECT n FROM usage_writes").Scan(&writes)
	if stored != base.Add(63*time.Millisecond).UnixMilli() {
		t.Fatal("latest timestamp lost")
	}
	if writes > 2 {
		t.Fatalf("expected shared commits, got %d updates", writes)
	}
	if err := s.RecordAPIKeySecretUse(id, base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow("SELECT last_used_at FROM api_key_secrets WHERE id=?", id).Scan(&stored)
	if stored != base.Add(63*time.Millisecond).UnixMilli() {
		t.Fatal("timestamp regressed")
	}
}

func TestAPIKeyUseCloseDrainsAndRejectsNewCalls(t *testing.T) {
	s, id := usageTestStore(t)
	blocker, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.RecordAPIKeySecretUse(id, time.Now()) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.usageMu.Lock()
		busy := s.usageWriting
		s.usageMu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			blocker.Rollback()
			t.Fatal("not started")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	for {
		s.usageMu.Lock()
		closing := s.usageClosed
		s.usageMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			blocker.Rollback()
			t.Fatal("not closing")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		blocker.Rollback()
		t.Fatal("closed before flush")
	default:
	}
	if err := s.RecordAPIKeySecretUse(id, time.Now()); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("new call=%v", err)
	}
	blocker.Commit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestAPIKeyUsePropagatesStorageFailure(t *testing.T) {
	s, id := usageTestStore(t)
	s.db.Close()
	done := make(chan error, 1)
	go func() { done <- s.RecordAPIKeySecretUse(id, time.Now()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("storage failure hidden")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer stuck")
	}
}

func TestAPIKeyUseMultiSecretCommitRollsBackOnError(t *testing.T) {
	s, id := usageTestStore(t)
	on := true
	if _, err := s.SaveConfig(&config.File{ConsumerKeys: []config.ConsumerKey{{Name: "bench", Key: "synthetic-usage-test", Enabled: &on}, {Name: "other", Key: "synthetic-usage-other", Enabled: &on}}}); err != nil {
		t.Fatal(err)
	}
	auth, err := s.APIKeyAuthenticators()
	if err != nil {
		t.Fatal(err)
	}
	var other string
	for _, a := range auth {
		if got, ok := a.MatchSecret("synthetic-usage-other"); ok {
			other = got
		}
	}
	if other == "" {
		t.Fatal("missing second key")
	}
	at := time.Now().UnixMilli()
	batch := []apiKeyUse{{secretID: id, usedAt: at}, {secretID: other, usedAt: at + 1}, {secretID: id, usedAt: at - 1}}
	if err := s.commitAPIKeyUses(batch); err != nil {
		t.Fatal(err)
	}
	var got int64
	s.db.QueryRow("SELECT last_used_at FROM api_key_secrets WHERE id=?", id).Scan(&got)
	if got != at {
		t.Fatal("wrong coalesced timestamp")
	}
	// A storage failure in one row must not acknowledge a partial batch.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_usage BEFORE UPDATE OF last_used_at ON api_key_secrets WHEN NEW.last_used_at=9223372036854775000 BEGIN SELECT RAISE(ABORT,'injected storage failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.commitAPIKeyUses([]apiKeyUse{{secretID: id, usedAt: at + 10}, {secretID: other, usedAt: 9223372036854775000}}); err == nil {
		t.Fatal("expected failure")
	}
	s.db.QueryRow("SELECT last_used_at FROM api_key_secrets WHERE id=?", id).Scan(&got)
	if got != at {
		t.Fatal("partial commit")
	}
}
