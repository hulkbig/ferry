package controlstore

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrAdministratorAlreadyInitialized = errors.New("management administrator is already initialized")
	ErrAdministratorNotInitialized     = errors.New("management administrator is not initialized")
	ErrManagementSessionInvalid        = errors.New("management session is invalid or expired")
)

// ManagementAdministrator contains the verifier and lockout state for the
// single local administrator. PasswordHash is never exposed through HTTP.
type ManagementAdministrator struct {
	Username               string
	PasswordHash           string
	FailedAttempts         int
	FailureWindowStartedAt *time.Time
	LockedUntil            *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// ManagementSession is the durable server-side session projection. Only
// token and CSRF digests are stored in SQLite.
type ManagementSession struct {
	ID         string
	Username   string
	TokenHash  []byte
	CSRFHash   []byte
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

type ManagementSecurityEvent struct {
	ID         int64          `json:"id"`
	CreatedAt  time.Time      `json:"created_at"`
	EventType  string         `json:"event_type"`
	Actor      string         `json:"actor,omitempty"`
	RemoteAddr string         `json:"remote_addr,omitempty"`
	Detail     map[string]any `json:"detail"`
}

type managementExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertManagementSecurityEvent(exec managementExec, now time.Time, eventType, actor, remoteAddr string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode management security event: %w", err)
	}
	_, err = exec.Exec(`INSERT INTO management_security_events(created_at,event_type,actor,remote_addr,detail_json)
		VALUES(?,?,?,?,?)`, now.UnixMilli(), eventType, actor, remoteAddr, raw)
	return err
}

func nullableMillis(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.UnixMilli(value.Int64)
	return &result
}

func (s *Store) ManagementAdministrator() (*ManagementAdministrator, error) {
	var admin ManagementAdministrator
	var failureWindow, lockedUntil sql.NullInt64
	var createdAt, updatedAt int64
	err := s.db.QueryRow(`SELECT username,password_hash,failed_attempts,failure_window_started_at,
		locked_until,created_at,updated_at FROM management_administrator WHERE singleton=1`).Scan(
		&admin.Username, &admin.PasswordHash, &admin.FailedAttempts, &failureWindow,
		&lockedUntil, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	admin.FailureWindowStartedAt = nullableMillis(failureWindow)
	admin.LockedUntil = nullableMillis(lockedUntil)
	admin.CreatedAt = time.UnixMilli(createdAt)
	admin.UpdatedAt = time.UnixMilli(updatedAt)
	return &admin, nil
}

func (s *Store) InitializeManagementAdministrator(username, passwordHash, remoteAddr string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRow(`SELECT 1 FROM management_administrator WHERE singleton=1`).Scan(&exists)
	if err == nil {
		return ErrAdministratorAlreadyInitialized
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO management_administrator(
		singleton,username,password_hash,created_at,updated_at) VALUES(1,?,?,?,?)`,
		username, passwordHash, now.UnixMilli(), now.UnixMilli()); err != nil {
		return err
	}
	if err := insertManagementSecurityEvent(tx, now, "administrator_initialized", username, remoteAddr, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordManagementLoginFailure advances the global single-admin lockout
// counter. Five consecutive failures inside fifteen minutes lock login for
// fifteen minutes.
func (s *Store) RecordManagementLoginFailure(remoteAddr string, now time.Time) (int, *time.Time, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	var username string
	var attempts int
	var windowRaw, lockedRaw sql.NullInt64
	if err := tx.QueryRow(`SELECT username,failed_attempts,failure_window_started_at,locked_until
		FROM management_administrator WHERE singleton=1`).Scan(&username, &attempts, &windowRaw, &lockedRaw); errors.Is(err, sql.ErrNoRows) {
		return 0, nil, ErrAdministratorNotInitialized
	} else if err != nil {
		return 0, nil, err
	}
	lockedUntil := nullableMillis(lockedRaw)
	if lockedUntil != nil && lockedUntil.After(now) {
		if err := insertManagementSecurityEvent(tx, now, "login_failed", username, remoteAddr,
			map[string]any{"reason": "locked", "locked_until": lockedUntil}); err != nil {
			return 0, nil, err
		}
		if err := tx.Commit(); err != nil {
			return 0, nil, err
		}
		return attempts, lockedUntil, nil
	}
	window := nullableMillis(windowRaw)
	if window == nil || now.Sub(*window) > 15*time.Minute {
		attempts = 0
		start := now
		window = &start
	}
	attempts++
	lockedUntil = nil
	if attempts >= 5 {
		value := now.Add(15 * time.Minute)
		lockedUntil = &value
	}
	var lockedValue any
	if lockedUntil != nil {
		lockedValue = lockedUntil.UnixMilli()
	}
	if _, err := tx.Exec(`UPDATE management_administrator SET failed_attempts=?,
		failure_window_started_at=?,locked_until=?,updated_at=? WHERE singleton=1`,
		attempts, window.UnixMilli(), lockedValue, now.UnixMilli()); err != nil {
		return 0, nil, err
	}
	detail := map[string]any{"attempt": attempts, "reason": "invalid_credentials"}
	if lockedUntil != nil {
		detail["locked_until"] = lockedUntil
	}
	if err := insertManagementSecurityEvent(tx, now, "login_failed", username, remoteAddr, detail); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return attempts, lockedUntil, nil
}

func (s *Store) RecordManagementLoginSuccess(username, remoteAddr string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE management_administrator SET failed_attempts=0,
		failure_window_started_at=NULL,locked_until=NULL,updated_at=? WHERE singleton=1 AND username=?`,
		now.UnixMilli(), username)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("management administrator %q not found", username)
	}
	if err := insertManagementSecurityEvent(tx, now, "login_succeeded", username, remoteAddr, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateManagementSession(session ManagementSession, remoteAddr string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM management_sessions WHERE expires_at<=?`, session.CreatedAt.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO management_sessions(
		id,username,token_hash,csrf_hash,created_at,last_seen_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
		session.ID, session.Username, session.TokenHash, session.CSRFHash,
		session.CreatedAt.UnixMilli(), session.LastSeenAt.UnixMilli(), session.ExpiresAt.UnixMilli()); err != nil {
		return err
	}
	if err := insertManagementSecurityEvent(tx, session.CreatedAt, "session_created", session.Username, remoteAddr,
		map[string]any{"expires_at": session.ExpiresAt}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ManagementSession(tokenHash []byte, now time.Time, idleLimit time.Duration) (*ManagementSession, error) {
	var session ManagementSession
	var createdAt, lastSeenAt, expiresAt int64
	err := s.db.QueryRow(`SELECT id,username,token_hash,csrf_hash,created_at,last_seen_at,expires_at
		FROM management_sessions WHERE token_hash=?`, tokenHash).Scan(&session.ID, &session.Username,
		&session.TokenHash, &session.CSRFHash, &createdAt, &lastSeenAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrManagementSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	session.CreatedAt = time.UnixMilli(createdAt)
	session.LastSeenAt = time.UnixMilli(lastSeenAt)
	session.ExpiresAt = time.UnixMilli(expiresAt)
	if !session.ExpiresAt.After(now) || now.Sub(session.LastSeenAt) >= idleLimit {
		_, _ = s.db.Exec(`DELETE FROM management_sessions WHERE id=?`, session.ID)
		return nil, ErrManagementSessionInvalid
	}
	if _, err := s.db.Exec(`UPDATE management_sessions SET last_seen_at=? WHERE id=?`, now.UnixMilli(), session.ID); err != nil {
		return nil, err
	}
	session.LastSeenAt = now
	return &session, nil
}

func (s *Store) UpdateManagementSessionCSRF(sessionID string, csrfHash []byte) error {
	result, err := s.db.Exec(`UPDATE management_sessions SET csrf_hash=? WHERE id=?`, csrfHash, sessionID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return ErrManagementSessionInvalid
	}
	return nil
}

func (s *Store) DeleteManagementSession(tokenHash []byte, remoteAddr string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var username string
	err = tx.QueryRow(`SELECT username FROM management_sessions WHERE token_hash=?`, tokenHash).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrManagementSessionInvalid
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM management_sessions WHERE token_hash=?`, tokenHash); err != nil {
		return err
	}
	if err := insertManagementSecurityEvent(tx, now, "logout", username, remoteAddr, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateManagementAdministratorPassword(username, passwordHash, eventType, remoteAddr string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE management_administrator SET password_hash=?,failed_attempts=0,
		failure_window_started_at=NULL,locked_until=NULL,updated_at=? WHERE singleton=1 AND username=?`,
		passwordHash, now.UnixMilli(), username)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return ErrAdministratorNotInitialized
	}
	if _, err := tx.Exec(`DELETE FROM management_sessions WHERE username=?`, username); err != nil {
		return err
	}
	if eventType == "" {
		eventType = "password_changed"
	}
	if err := insertManagementSecurityEvent(tx, now, eventType, username, remoteAddr,
		map[string]any{"sessions_revoked": true}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ManagementSecurityEvents(limit int) ([]ManagementSecurityEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id,created_at,event_type,actor,remote_addr,detail_json
		FROM management_security_events ORDER BY created_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ManagementSecurityEvent{}
	for rows.Next() {
		var item ManagementSecurityEvent
		var createdAt int64
		var raw []byte
		if err := rows.Scan(&item.ID, &createdAt, &item.EventType, &item.Actor, &item.RemoteAddr, &raw); err != nil {
			return nil, err
		}
		item.CreatedAt = time.UnixMilli(createdAt)
		item.Detail = map[string]any{}
		if err := json.Unmarshal(raw, &item.Detail); err != nil {
			return nil, fmt.Errorf("decode management security event %d: %w", item.ID, err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) RecordManagementSecurityEvent(eventType, actor, remoteAddr string, detail map[string]any, now time.Time) error {
	return insertManagementSecurityEvent(s.db, now, eventType, actor, remoteAddr, detail)
}

func constantTimeBytesEqual(left, right []byte) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1
}
