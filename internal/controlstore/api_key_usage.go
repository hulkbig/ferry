package controlstore

import (
	"database/sql"
	"sync"
	"time"
)

const maxAPIKeyUseBatch = 256

type apiKeyUse struct {
	secretID string
	usedAt   int64
	done     chan error
}

// RecordAPIKeySecretUse stores authentication evidence before returning. Calls
// already waiting on durable storage share a commit; timestamps are never
// sampled or acknowledged before persistence. SQLite synchronous=FULL remains.
func (s *Store) RecordAPIKeySecretUse(secretID string, usedAt time.Time) error {
	if secretID == "" {
		return nil
	}
	use := apiKeyUse{secretID: secretID, usedAt: usedAt.UnixMilli(), done: make(chan error, 1)}
	s.usageMu.Lock()
	if s.usageCond == nil {
		s.usageCond = sync.NewCond(&s.usageMu)
	}
	for len(s.usagePending) >= maxAPIKeyUseBatch && !s.usageClosed {
		s.usageCond.Wait()
	}
	if s.usageClosed {
		s.usageMu.Unlock()
		return sql.ErrConnDone
	}
	s.usagePending = append(s.usagePending, use)
	if !s.usageWriting {
		s.usageWriting = true
		s.usageWG.Add(1)
		go s.writeAPIKeyUses()
	}
	s.usageMu.Unlock()
	return <-use.done
}

func (s *Store) writeAPIKeyUses() {
	defer s.usageWG.Done()
	for {
		s.usageMu.Lock()
		if len(s.usagePending) == 0 {
			s.usageWriting = false
			s.usageMu.Unlock()
			return
		}
		batch := s.usagePending
		s.usagePending = nil
		s.usageCond.Broadcast()
		s.usageMu.Unlock()
		err := s.commitAPIKeyUses(batch)
		for _, use := range batch {
			use.done <- err
		}
	}
}

func (s *Store) commitAPIKeyUses(batch []apiKeyUse) error {
	latest := make(map[string]int64, len(batch))
	for _, use := range batch {
		if at, ok := latest[use.secretID]; !ok || use.usedAt > at {
			latest[use.secretID] = use.usedAt
		}
	}
	const query = `UPDATE api_key_secrets SET last_used_at=? WHERE id=? AND (last_used_at IS NULL OR last_used_at < ?)`
	if len(latest) == 1 {
		for id, at := range latest {
			_, err := s.db.Exec(query, at, id, at)
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, at := range latest {
		if _, err := tx.Exec(query, at, id, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}
