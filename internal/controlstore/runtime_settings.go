package controlstore

import (
	"database/sql"
	"fmt"
	"time"
)

// RuntimeSetting returns the persisted value of one control-plane runtime
// setting. Missing settings report found=false so callers keep their
// documented default instead of failing closed.
func (s *Store) RuntimeSetting(key string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("control store is not configured")
	}
	var value string
	err := s.db.QueryRow(`SELECT value FROM runtime_settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query runtime setting: %w", err)
	}
	return value, true, nil
}

// SetRuntimeSetting atomically persists one control-plane runtime setting.
func (s *Store) SetRuntimeSetting(key, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("control store is not configured")
	}
	if key == "" {
		return fmt.Errorf("runtime setting key is required")
	}
	if _, err := s.db.Exec(`INSERT INTO runtime_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("save runtime setting: %w", err)
	}
	return nil
}
