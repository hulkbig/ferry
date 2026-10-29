// Package conversation persists canonical conversation state and virtual
// Responses lineage independently from configuration revisions.
package conversation

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"ferry/internal/domain"
)

type Store struct{ db *sql.DB }

// RetentionStore is the minimum lifecycle surface needed by a periodic or
// operator-triggered retention worker. It deliberately contains no scheduler
// or deployment policy.
type RetentionStore interface {
	PurgeInactive(before time.Time, limit int) (int64, error)
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create conversation data dir: %w", err)
	}
	path := filepath.Join(dataDir, "conversation.db")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close() // #nosec G104 -- error-path cleanup; the original error is returned
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close() // #nosec G104 -- error-path cleanup; the original error is returned
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS conversations (
			id TEXT PRIMARY KEY, logical_model_id TEXT NOT NULL, items_json TEXT NOT NULL,
			current_provider_id TEXT NOT NULL, current_upstream_model TEXT NOT NULL,
			current_connection_id TEXT NOT NULL, continuity_mode TEXT NOT NULL,
			created_at INTEGER NOT NULL, last_active_at INTEGER NOT NULL,
			last_route_fingerprint TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS native_handles (
			conversation_id TEXT NOT NULL, provider_id TEXT NOT NULL, connection_id TEXT NOT NULL,
			native_conversation_id TEXT NOT NULL DEFAULT '', native_response_id TEXT NOT NULL DEFAULT '',
			state_scope TEXT NOT NULL, created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL,
			PRIMARY KEY (conversation_id, provider_id, connection_id)
		)`,
		`CREATE TABLE IF NOT EXISTS smart_responses (
			id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, parent_response_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL, canonical_output_json TEXT NOT NULL,
			conversation_items_json TEXT NOT NULL DEFAULT '[]',
			provider_id TEXT NOT NULL, upstream_model TEXT NOT NULL, connection_id TEXT NOT NULL,
			native_response_id TEXT NOT NULL DEFAULT '', continuity_mode TEXT NOT NULL, created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_smart_responses_conversation ON smart_responses(conversation_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS conversation_evidence (
			logical_model_id TEXT NOT NULL, key_hash TEXT NOT NULL,
			conversation_id TEXT NOT NULL, last_seen_at INTEGER NOT NULL,
			PRIMARY KEY (logical_model_id, key_hash)
		)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate conversation store: %w", err)
		}
	}
	if err := addColumnIfMissing(s.db, "smart_responses", "conversation_items_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return fmt.Errorf("migrate response branch snapshots: %w", err)
	}
	return s.migrateEvidenceIsolation()
}

func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return err
		}
		found = found || name == column
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

// migrateEvidenceIsolation upgrades the original global key_hash primary key
// without discarding durable aliases. The conversation row is authoritative
// for the logical model of legacy evidence.
func (s *Store) migrateEvidenceIsolation() error {
	rows, err := s.db.Query(`PRAGMA table_info(conversation_evidence)`)
	if err != nil {
		return fmt.Errorf("inspect conversation evidence schema: %w", err)
	}
	hasLogicalModel := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return err
		}
		hasLogicalModel = hasLogicalModel || name == "logical_model_id"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasLogicalModel {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS conversation_evidence_v2`,
		`CREATE TABLE conversation_evidence_v2 (
			logical_model_id TEXT NOT NULL, key_hash TEXT NOT NULL,
			conversation_id TEXT NOT NULL, last_seen_at INTEGER NOT NULL,
			PRIMARY KEY (logical_model_id, key_hash))`,
		`INSERT INTO conversation_evidence_v2 (logical_model_id, key_hash, conversation_id, last_seen_at)
		 SELECT COALESCE(c.logical_model_id, ''), e.key_hash, e.conversation_id, e.last_seen_at
		 FROM conversation_evidence e LEFT JOIN conversations c ON c.id = e.conversation_id`,
		`DROP TABLE conversation_evidence`,
		`ALTER TABLE conversation_evidence_v2 RENAME TO conversation_evidence`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate conversation evidence isolation: %w", err)
		}
	}
	return tx.Commit()
}

func NewID(prefix string) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(value[:])
}

func (s *Store) Conversation(id string) (*domain.Conversation, error) {
	if s == nil || id == "" {
		return nil, nil
	}
	var conversation domain.Conversation
	var items string
	var createdAt, lastActiveAt int64
	err := s.db.QueryRow(`SELECT id, logical_model_id, items_json, current_provider_id,
		current_upstream_model, current_connection_id, continuity_mode, created_at,
		last_active_at, last_route_fingerprint FROM conversations WHERE id = ?`, id).
		Scan(&conversation.ID, &conversation.LogicalModelID, &items, &conversation.CurrentProviderID,
			&conversation.CurrentUpstreamModel, &conversation.CurrentConnectionID,
			&conversation.ContinuityMode, &createdAt, &lastActiveAt, &conversation.LastRouteFingerprint)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(items), &conversation.Items); err != nil {
		return nil, err
	}
	conversation.CreatedAt = time.UnixMilli(createdAt)
	conversation.LastActiveAt = time.UnixMilli(lastActiveAt)
	return &conversation, nil
}

func (s *Store) Response(id string) (*domain.SmartResponse, error) {
	if s == nil || id == "" {
		return nil, nil
	}
	var response domain.SmartResponse
	var output string
	var createdAt int64
	err := s.db.QueryRow(`SELECT id, conversation_id, parent_response_id, request_id,
		canonical_output_json, provider_id, upstream_model, connection_id,
		native_response_id, continuity_mode, created_at FROM smart_responses WHERE id = ?`, id).
		Scan(&response.ID, &response.ConversationID, &response.ParentResponseID, &response.RequestID,
			&output, &response.ProviderID, &response.UpstreamModel, &response.ConnectionID,
			&response.NativeResponseID, &response.ContinuityMode, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(output), &response.CanonicalOutput); err != nil {
		return nil, err
	}
	response.CreatedAt = time.UnixMilli(createdAt)
	return &response, nil
}

// ConversationAtResponse restores the canonical branch snapshot represented
// by one virtual response. It prevents two children of the same parent from
// accidentally inheriting each other's later conversation state.
func (s *Store) ConversationAtResponse(id string) (*domain.SmartResponse, *domain.Conversation, error) {
	response, err := s.Response(id)
	if err != nil || response == nil {
		return response, nil, err
	}
	conversation, err := s.Conversation(response.ConversationID)
	if err != nil || conversation == nil {
		return response, conversation, err
	}
	var itemsJSON string
	if err := s.db.QueryRow(`SELECT conversation_items_json FROM smart_responses WHERE id=?`, id).Scan(&itemsJSON); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(itemsJSON), &conversation.Items); err != nil {
		return nil, nil, err
	}
	conversation.CurrentProviderID = response.ProviderID
	conversation.CurrentUpstreamModel = response.UpstreamModel
	conversation.CurrentConnectionID = response.ConnectionID
	conversation.ContinuityMode = response.ContinuityMode
	conversation.LastActiveAt = response.CreatedAt
	return response, conversation, nil
}

func (s *Store) ConversationForEvidence(logicalModelID, keyHash string) (*domain.Conversation, error) {
	if s == nil || logicalModelID == "" || keyHash == "" {
		return nil, nil
	}
	var id string
	err := s.db.QueryRow(`SELECT conversation_id FROM conversation_evidence
		WHERE logical_model_id = ? AND key_hash = ?`, logicalModelID, keyHash).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Conversation(id)
}

func (s *Store) NativeHandle(conversationID, providerID, connectionID string) (*domain.NativeHandle, error) {
	if s == nil || conversationID == "" {
		return nil, nil
	}
	var handle domain.NativeHandle
	var createdAt, lastUsedAt int64
	err := s.db.QueryRow(`SELECT conversation_id, provider_id, connection_id,
		native_conversation_id, native_response_id, state_scope, created_at, last_used_at
		FROM native_handles WHERE conversation_id = ? AND provider_id = ? AND connection_id = ?`,
		conversationID, providerID, connectionID).
		Scan(&handle.ConversationID, &handle.ProviderID, &handle.ConnectionID,
			&handle.NativeConversationID, &handle.NativeResponseID, &handle.StateScope,
			&createdAt, &lastUsedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	handle.CreatedAt = time.UnixMilli(createdAt)
	handle.LastUsedAt = time.UnixMilli(lastUsedAt)
	return &handle, nil
}

// SaveCompletion atomically advances canonical state, virtual response
// lineage, native handles, and high-confidence conversation aliases.
func (s *Store) SaveCompletion(conversation domain.Conversation, response domain.SmartResponse,
	handle *domain.NativeHandle, evidenceHashes []string) error {
	if s == nil {
		return nil
	}
	items, err := json.Marshal(conversation.Items)
	if err != nil {
		return err
	}
	output, err := json.Marshal(response.CanonicalOutput)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO conversations (id, logical_model_id, items_json,
		current_provider_id, current_upstream_model, current_connection_id, continuity_mode,
		created_at, last_active_at, last_route_fingerprint) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET logical_model_id=excluded.logical_model_id,
		items_json=excluded.items_json, current_provider_id=excluded.current_provider_id,
		current_upstream_model=excluded.current_upstream_model,
		current_connection_id=excluded.current_connection_id,
		continuity_mode=excluded.continuity_mode, last_active_at=excluded.last_active_at,
		last_route_fingerprint=excluded.last_route_fingerprint`,
		conversation.ID, conversation.LogicalModelID, string(items), conversation.CurrentProviderID,
		conversation.CurrentUpstreamModel, conversation.CurrentConnectionID, conversation.ContinuityMode,
		conversation.CreatedAt.UnixMilli(), conversation.LastActiveAt.UnixMilli(), conversation.LastRouteFingerprint)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO smart_responses (id, conversation_id, parent_response_id,
		request_id, canonical_output_json, conversation_items_json, provider_id, upstream_model, connection_id,
		native_response_id, continuity_mode, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		response.ID, response.ConversationID, response.ParentResponseID, response.RequestID,
		string(output), string(items), response.ProviderID, response.UpstreamModel, response.ConnectionID,
		response.NativeResponseID, response.ContinuityMode, response.CreatedAt.UnixMilli())
	if err != nil {
		return err
	}
	if handle != nil && handle.NativeResponseID != "" {
		_, err = tx.Exec(`INSERT INTO native_handles (conversation_id, provider_id, connection_id,
			native_conversation_id, native_response_id, state_scope, created_at, last_used_at)
			VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(conversation_id, provider_id, connection_id)
			DO UPDATE SET native_conversation_id=excluded.native_conversation_id,
			native_response_id=excluded.native_response_id, state_scope=excluded.state_scope,
			last_used_at=excluded.last_used_at`, handle.ConversationID, handle.ProviderID,
			handle.ConnectionID, handle.NativeConversationID, handle.NativeResponseID,
			handle.StateScope, handle.CreatedAt.UnixMilli(), handle.LastUsedAt.UnixMilli())
		if err != nil {
			return err
		}
	}
	for _, hash := range evidenceHashes {
		if hash == "" {
			continue
		}
		if _, err = tx.Exec(`INSERT INTO conversation_evidence
			(logical_model_id, key_hash, conversation_id, last_seen_at)
			VALUES (?,?,?,?) ON CONFLICT(logical_model_id, key_hash) DO UPDATE
			SET conversation_id=excluded.conversation_id, last_seen_at=excluded.last_seen_at`,
			conversation.LogicalModelID, hash, conversation.ID, conversation.LastActiveAt.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PurgeInactive removes at most limit inactive conversation aggregates and
// every dependent handle, response and evidence row in one transaction.
func (s *Store) PurgeInactive(before time.Time, limit int) (int64, error) {
	if s == nil || before.IsZero() {
		return 0, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM conversations WHERE last_active_at < ?
		ORDER BY last_active_at, id LIMIT ?`, before.UnixMilli(), limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i := range ids {
		args[i] = ids[i]
	}
	for _, table := range []string{"conversation_evidence", "native_handles", "smart_responses", "conversations"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE `+map[string]string{ // #nosec G202 -- table and column come only from the fixed literal set above; every id stays a bound parameter.
			"conversation_evidence": "conversation_id", "native_handles": "conversation_id",
			"smart_responses": "conversation_id", "conversations": "id",
		}[table]+` IN (`+placeholders+`)`, args...); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}
