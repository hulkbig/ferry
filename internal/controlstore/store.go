// Package controlstore persists control-plane configuration versions in
// SQLite. The active row is the runtime source of truth; older rows provide a
// small, auditable rollback history without requiring editable YAML files.
package controlstore

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"ferry/internal/backuppolicy"
	"ferry/internal/config"
	"ferry/internal/domain"
)

var (
	ErrNoConfig         = errors.New("control store has no configuration")
	ErrNoPreviousConfig = errors.New("control store has no previous configuration")
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create control store dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open control store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping control store: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure control store: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS control_config_versions (
		version INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at INTEGER NOT NULL,
		config_json BLOB NOT NULL,
		active INTEGER NOT NULL DEFAULT 0 CHECK(active IN (0,1))
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate control store: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS policies (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			priority INTEGER NOT NULL,
			enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
			active_revision INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS policy_revisions (
			policy_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			content_json BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY(policy_id, revision),
			UNIQUE(policy_id, content_hash),
			FOREIGN KEY(policy_id) REFERENCES policies(id)
		);
		CREATE TABLE IF NOT EXISTS policy_set_versions (
			version INTEGER PRIMARY KEY AUTOINCREMENT,
			config_version INTEGER NOT NULL UNIQUE,
			index_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			active INTEGER NOT NULL DEFAULT 0 CHECK(active IN (0,1))
		);
		CREATE TABLE IF NOT EXISTS policy_set_members (
			set_version INTEGER NOT NULL,
			position INTEGER NOT NULL,
			priority INTEGER NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			policy_id TEXT NOT NULL,
			policy_revision INTEGER NOT NULL,
			enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
			PRIMARY KEY(set_version, position),
			UNIQUE(set_version, policy_id),
			FOREIGN KEY(set_version) REFERENCES policy_set_versions(version),
			FOREIGN KEY(policy_id, policy_revision) REFERENCES policy_revisions(policy_id, revision)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS one_active_policy_set
			ON policy_set_versions(active) WHERE active=1;
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate routing policies: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS configuration_drafts (
			id TEXT PRIMARY KEY,
			base_revision INTEGER NOT NULL,
			intent_json BLOB NOT NULL,
			status TEXT NOT NULL DEFAULT 'OPEN' CHECK(status IN ('OPEN','APPLIED')),
			restore_from_revision INTEGER,
			applied_revision INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY(base_revision) REFERENCES control_config_versions(version),
			FOREIGN KEY(restore_from_revision) REFERENCES control_config_versions(version),
			FOREIGN KEY(applied_revision) REFERENCES control_config_versions(version)
		);
		CREATE INDEX IF NOT EXISTS configuration_drafts_status
			ON configuration_drafts(status,updated_at);
		CREATE INDEX IF NOT EXISTS configuration_drafts_applied_revision
			ON configuration_drafts(applied_revision,restore_from_revision) WHERE status='APPLIED';
		CREATE TABLE IF NOT EXISTS configuration_revision_audit (
			revision INTEGER PRIMARY KEY,
			actor TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'console',
			summary TEXT NOT NULL DEFAULT '',
			resources_json BLOB NOT NULL DEFAULT '[]',
			FOREIGN KEY(revision) REFERENCES control_config_versions(version) ON DELETE CASCADE
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate configuration drafts: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS provider_capability_evidence (
			provider_id TEXT NOT NULL,
			endpoint_fingerprint TEXT NOT NULL,
			upstream_model TEXT NOT NULL,
			protocol TEXT NOT NULL,
			capability TEXT NOT NULL,
			supported INTEGER NOT NULL CHECK(supported IN (0,1)),
			failure_type TEXT NOT NULL DEFAULT '',
			observed_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			PRIMARY KEY(provider_id,endpoint_fingerprint,upstream_model,protocol,capability)
		);
		CREATE INDEX IF NOT EXISTS provider_capability_evidence_expiry
			ON provider_capability_evidence(expires_at);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate provider capability evidence: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS provider_model_sync_state (
			provider_id TEXT PRIMARY KEY,
			config_fingerprint TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL CHECK(status IN ('idle','running','succeeded','failed','unsupported','disabled')),
			last_started_at INTEGER,
			last_completed_at INTEGER,
			last_success_at INTEGER,
			next_run_at INTEGER,
			last_error_code TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			discovered_models INTEGER NOT NULL DEFAULT 0,
			added_models INTEGER NOT NULL DEFAULT 0,
			updated_models INTEGER NOT NULL DEFAULT 0,
			created_logical_models INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS provider_model_sync_due
			ON provider_model_sync_state(next_run_at,status);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate provider model sync state: %w", err)
	}
	if _, err := addColumnIfMissing(db, "provider_model_sync_state", "updated_models", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate provider model sync updated models: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS runtime_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate runtime settings: %w", err)
	}
	// Fix the implicit default before serving requests. An existing configuration
	// keeps its historical destination; a fresh database gets Ferry's namespace.
	// INSERT OR IGNORE also preserves explicit operator choices on every reopen.
	freshBackup, _ := json.Marshal(backuppolicy.Default())
	legacyBackup, _ := json.Marshal(backuppolicy.LegacyDefault())
	if _, err := db.Exec(`INSERT OR IGNORE INTO runtime_settings(key,value,updated_at)
	 VALUES (?, CASE WHEN EXISTS(SELECT 1 FROM control_config_versions) THEN ? ELSE ? END, ?)`,
		backuppolicy.SettingKey, string(legacyBackup), string(freshBackup), time.Now().UnixMilli()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize backup destination: %w", err)
	}
	// A process can stop after recording the start of a catalog request. Make
	// that interrupted attempt retryable on the next startup instead of
	// leaving the Provider permanently displayed as running.
	nowMillis := time.Now().UnixMilli()
	if _, err := db.Exec(`UPDATE provider_model_sync_state SET
		status='failed', last_completed_at=?, next_run_at=?,
		last_error_code='interrupted',
		last_error='model synchronization was interrupted by service restart'
		WHERE status='running'`, nowMillis, nowMillis); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("recover provider model sync state: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
			default_logical_model TEXT NOT NULL DEFAULT '',
			default_objective TEXT NOT NULL DEFAULT '',
			monthly_budget_usd REAL NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS api_key_secrets (
			id TEXT PRIMARY KEY,
			api_key_id TEXT NOT NULL,
			secret_hash BLOB NOT NULL UNIQUE,
			preview TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('ACTIVE','GRACE','RETIRED')),
			created_at INTEGER NOT NULL,
			retired_at INTEGER,
			last_used_at INTEGER,
			FOREIGN KEY(api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS api_key_secrets_identity_state
			ON api_key_secrets(api_key_id,state);
		CREATE UNIQUE INDEX IF NOT EXISTS one_active_api_key_secret
			ON api_key_secrets(api_key_id) WHERE state='ACTIVE';
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate api keys: %w", err)
	}
	for _, migration := range []struct{ column, definition string }{
		{"revoked_at", "INTEGER"},
		{"scope_models_json", "BLOB NOT NULL DEFAULT '[]'"},
		{"scope_protocols_json", "BLOB NOT NULL DEFAULT '[]'"},
		{"expires_at", "INTEGER"},
		{"rate_limit_rpm", "INTEGER NOT NULL DEFAULT 0"},
		{"exact_only", "INTEGER NOT NULL DEFAULT 0 CHECK(exact_only IN (0,1))"},
	} {
		if _, err := addColumnIfMissing(db, "api_keys", migration.column, migration.definition); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate api key %s: %w", migration.column, err)
		}
	}
	if _, err := addColumnIfMissing(db, "api_key_secrets", "recoverable_secret", "TEXT"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate recoverable api key secret: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS management_administrator (
			singleton INTEGER PRIMARY KEY CHECK(singleton=1),
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			failed_attempts INTEGER NOT NULL DEFAULT 0,
			failure_window_started_at INTEGER,
			locked_until INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS management_sessions (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			token_hash BLOB NOT NULL UNIQUE,
			csrf_hash BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			FOREIGN KEY(username) REFERENCES management_administrator(username) ON UPDATE CASCADE ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS management_sessions_expiry
			ON management_sessions(expires_at,last_seen_at);
		CREATE TABLE IF NOT EXISTS management_security_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			actor TEXT NOT NULL DEFAULT '',
			remote_addr TEXT NOT NULL DEFAULT '',
			detail_json BLOB NOT NULL DEFAULT '{}'
		);
		CREATE INDEX IF NOT EXISTS management_security_events_recent
			ON management_security_events(created_at DESC,id DESC);
		CREATE TABLE IF NOT EXISTS management_alert_signals (
			signal_key TEXT PRIMARY KEY,
			alert_type TEXT NOT NULL,
			resource_kind TEXT NOT NULL DEFAULT '',
			resource_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			severity TEXT NOT NULL DEFAULT 'warning',
			first_observed_at INTEGER NOT NULL,
			last_observed_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS management_alerts (
			id TEXT PRIMARY KEY,
			signal_key TEXT NOT NULL,
			alert_type TEXT NOT NULL,
			resource_kind TEXT NOT NULL DEFAULT '',
			resource_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			severity TEXT NOT NULL DEFAULT 'warning',
			state TEXT NOT NULL CHECK(state IN ('ACTIVE','ACKNOWLEDGED','RECOVERED')),
			unread INTEGER NOT NULL DEFAULT 1 CHECK(unread IN (0,1)),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			acknowledged_at INTEGER,
			recovered_at INTEGER
		);
		CREATE INDEX IF NOT EXISTS management_alerts_recent
			ON management_alerts(updated_at DESC,id);
		CREATE UNIQUE INDEX IF NOT EXISTS management_alerts_open_signal
			ON management_alerts(signal_key) WHERE state IN ('ACTIVE','ACKNOWLEDGED');
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate management security: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS management_tokens (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			secret_hash BLOB NOT NULL UNIQUE,
			preview TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
			allow_high_risk INTEGER NOT NULL DEFAULT 0 CHECK(allow_high_risk IN (0,1)),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			expires_at INTEGER,
			revoked_at INTEGER,
			last_used_at INTEGER
		);
		CREATE INDEX IF NOT EXISTS management_tokens_status
			ON management_tokens(enabled,revoked_at,expires_at);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate management tokens: %w", err)
	}
	addedPriority, err := addColumnIfMissing(db, "policy_set_members", "priority", "INTEGER NOT NULL DEFAULT 0")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate routing policy priority: %w", err)
	}
	if addedPriority {
		if _, err := db.Exec(`UPDATE policy_set_members SET priority=(position+1)*10`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("backfill routing policy priority: %w", err)
		}
	}
	addedName, err := addColumnIfMissing(db, "policy_set_members", "name", "TEXT NOT NULL DEFAULT ''")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate routing policy name: %w", err)
	}
	addedDescription, err := addColumnIfMissing(db, "policy_set_members", "description", "TEXT NOT NULL DEFAULT ''")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate routing policy description: %w", err)
	}
	if addedName || addedDescription {
		if _, err := db.Exec(`UPDATE policy_set_members SET
			name=COALESCE((SELECT name FROM policies WHERE policies.id=policy_set_members.policy_id),name),
			description=COALESCE((SELECT description FROM policies WHERE policies.id=policy_set_members.policy_id),description)`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("backfill routing policy display metadata: %w", err)
		}
	}
	store := &Store{db: db}
	if err := store.migrateLegacyActiveConfig(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate legacy routing policy: %w", err)
	}
	if err := store.migrateLegacyAPIKeys(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate legacy api keys: %w", err)
	}
	// Security migration updates historical rows in place. secure_delete zeros
	// superseded cells; truncating the WAL prevents an older frame from
	// remaining as a raw-secret archive after Open returns.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("checkpoint sanitized api key history: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SaveConfig(f *config.File) (int64, error) {
	if f == nil {
		return 0, fmt.Errorf("config must be non-nil")
	}
	// Every write crosses the same one-way compatibility boundary. This keeps
	// the very first imported SQLite version normalized instead of creating a
	// second version on the next process start.
	if _, err := config.MigrateLegacyRoutingPolicies(f); err != nil {
		return 0, err
	}
	if _, err := config.MigrateLegacyV2Domain(f); err != nil {
		return 0, err
	}
	config.EnsureSameNameLogicalModels(f)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := syncAPIKeysTx(tx, f); err != nil {
		return 0, err
	}
	if err := syncPolicyRowsTx(tx, f); err != nil {
		return 0, err
	}
	b, err := sanitizedConfigJSON(f)
	if err != nil {
		return 0, fmt.Errorf("encode config: %w", err)
	}
	var activeVersion int64
	var activeRaw []byte
	err = tx.QueryRow(`SELECT version,config_json FROM control_config_versions WHERE active=1 LIMIT 1`).Scan(&activeVersion, &activeRaw)
	if err == nil && bytes.Equal(activeRaw, b) {
		if err := ensurePolicySetTx(tx, activeVersion, f); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		clearConsumerSecrets(f)
		return activeVersion, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE control_config_versions SET active=0 WHERE active=1`); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO control_config_versions(created_at,config_json,active) VALUES(?,?,1)`, time.Now().UnixMilli(), b)
	if err != nil {
		return 0, err
	}
	version, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := ensurePolicySetTx(tx, version, f); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	clearConsumerSecrets(f)
	return version, nil
}

func (s *Store) ActiveConfig() (*config.File, int64, error) {
	var version int64
	var raw []byte
	err := s.db.QueryRow(`SELECT version,config_json FROM control_config_versions WHERE active=1 LIMIT 1`).Scan(&version, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNoConfig
	}
	if err != nil {
		return nil, 0, err
	}
	f, err := decodeConfig(raw)
	if err != nil {
		return nil, 0, err
	}
	if err := hydrateRoutingPolicies(s.db, version, f); err != nil {
		return nil, 0, err
	}
	if err := hydrateAPIKeyIdentities(s.db, f); err != nil {
		return nil, 0, err
	}
	return f, version, nil
}

// PolicyRevisions lists immutable content history newest first.
func (s *Store) PolicyRevisions(id string) ([]domain.RoutingPolicyRevision, error) {
	rows, err := s.db.Query(`SELECT revision,content_hash,content_json,created_at
		FROM policy_revisions WHERE policy_id=? ORDER BY revision DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RoutingPolicyRevision{}
	for rows.Next() {
		var revision domain.RoutingPolicyRevision
		var raw []byte
		var createdAt int64
		revision.PolicyID = id
		if err := rows.Scan(&revision.Revision, &revision.ContentHash, &raw, &createdAt); err != nil {
			return nil, err
		}
		var content struct {
			Match domain.RoutingPolicyMatch `json:"match"`
			Route domain.RoutingPolicyRoute `json:"route"`
		}
		if err := json.Unmarshal(raw, &content); err != nil {
			return nil, fmt.Errorf("decode policy %s revision %d: %w", id, revision.Revision, err)
		}
		revision.Match = content.Match
		revision.Route = content.Route
		revision.CreatedAt = time.UnixMilli(createdAt)
		out = append(out, revision)
	}
	return out, rows.Err()
}

func (s *Store) PolicyRevision(id string, revision int64) (*domain.RoutingPolicyRevision, error) {
	var out domain.RoutingPolicyRevision
	var raw []byte
	var createdAt int64
	out.PolicyID = id
	err := s.db.QueryRow(`SELECT revision,content_hash,content_json,created_at
		FROM policy_revisions WHERE policy_id=? AND revision=?`, id, revision).
		Scan(&out.Revision, &out.ContentHash, &raw, &createdAt)
	if err != nil {
		return nil, err
	}
	var content struct {
		Match domain.RoutingPolicyMatch `json:"match"`
		Route domain.RoutingPolicyRoute `json:"route"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, fmt.Errorf("decode policy %s revision %d: %w", id, revision, err)
	}
	out.Match = content.Match
	out.Route = content.Route
	out.CreatedAt = time.UnixMilli(createdAt)
	return &out, nil
}

func (s *Store) RollbackConfig() (*config.File, int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	var active int64
	if err := tx.QueryRow(`SELECT version FROM control_config_versions WHERE active=1 LIMIT 1`).Scan(&active); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNoPreviousConfig
	} else if err != nil {
		return nil, 0, err
	}
	var version int64
	var raw []byte
	if err := tx.QueryRow(`SELECT version,config_json FROM control_config_versions WHERE version < ? ORDER BY version DESC LIMIT 1`, active).Scan(&version, &raw); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNoPreviousConfig
	} else if err != nil {
		return nil, 0, err
	}
	f, err := decodeConfig(raw)
	if err != nil {
		return nil, 0, err
	}
	if err := hydrateRoutingPolicies(tx, version, f); err != nil {
		return nil, 0, err
	}
	if err := hydrateAPIKeyIdentities(tx, f); err != nil {
		return nil, 0, err
	}
	if _, err := tx.Exec(`UPDATE control_config_versions SET active=0 WHERE active=1`); err != nil {
		return nil, 0, err
	}
	if _, err := tx.Exec(`UPDATE control_config_versions SET active=1 WHERE version=?`, version); err != nil {
		return nil, 0, err
	}
	if _, err := tx.Exec(`UPDATE policy_set_versions SET active=0 WHERE active=1`); err != nil {
		return nil, 0, err
	}
	if _, err := tx.Exec(`UPDATE policy_set_versions SET active=1 WHERE config_version=?`, version); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return f, version, nil
}

// ActivateConfig moves the active pointer to an existing immutable version.
// It is used as transaction compensation when the durable SQLite change
// succeeded but rebuilding the in-memory scheduler world failed.
func (s *Store) ActivateConfig(version int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM control_config_versions WHERE version=?`, version).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("config version %d does not exist", version)
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE control_config_versions SET active=0 WHERE active=1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE control_config_versions SET active=1 WHERE version=?`, version); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE policy_set_versions SET active=0 WHERE active=1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE policy_set_versions SET active=1 WHERE config_version=?`, version); err != nil {
		return err
	}
	return tx.Commit()
}

// RestoreConfig atomically reactivates the last applied version and removes a
// newer candidate whose SQLite write succeeded but runtime publication failed.
// Removing that candidate prevents a later rollback from selecting a version
// that never actually served traffic.
func (s *Store) RestoreConfig(activeVersion, failedVersion int64) error {
	if activeVersion == failedVersion {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRow(`SELECT version FROM control_config_versions WHERE active=1 LIMIT 1`).Scan(&current); err != nil {
		return err
	}
	if current != failedVersion {
		return fmt.Errorf("active config version is %d, not failed candidate %d", current, failedVersion)
	}
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM control_config_versions WHERE version=?`, activeVersion).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("config version %d does not exist", activeVersion)
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE control_config_versions SET active=0 WHERE active=1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE control_config_versions SET active=1 WHERE version=?`, activeVersion); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE policy_set_versions SET active=0 WHERE active=1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE policy_set_versions SET active=1 WHERE config_version=?`, activeVersion); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM policy_set_members WHERE set_version IN (SELECT version FROM policy_set_versions WHERE config_version=? AND active=0)`, failedVersion); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM policy_set_versions WHERE config_version=? AND active=0`, failedVersion); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM control_config_versions WHERE version=? AND active=0`, failedVersion)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("discard failed config version %d: rows=%d err=%v", failedVersion, n, err)
	}
	return tx.Commit()
}

func decodeConfig(raw []byte) (*config.File, error) {
	var f config.File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return &f, nil
}

type rowQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// hydrateRoutingPolicies makes the normalized policy tables authoritative for
// routing. The configuration JSON remains the transactional envelope for the
// rest of the control plane, but policy matching and route content are always
// reconstructed from immutable revisions belonging to the selected set.
func hydrateRoutingPolicies(q rowQueryer, configVersion int64, f *config.File) error {
	rows, err := q.Query(`SELECT m.priority,m.policy_id,m.name,m.description,m.enabled,
		m.policy_revision,r.content_hash,r.content_json
		FROM policy_set_versions s
		JOIN policy_set_members m ON m.set_version=s.version
		JOIN policy_revisions r ON r.policy_id=m.policy_id AND r.revision=m.policy_revision
		WHERE s.config_version=? ORDER BY m.position`, configVersion)
	if err != nil {
		return err
	}
	defer rows.Close()

	policies := make([]domain.RoutingPolicySpec, 0)
	for rows.Next() {
		var (
			priority int
			enabled  int
			raw      []byte
			spec     domain.RoutingPolicySpec
		)
		if err := rows.Scan(&priority, &spec.ID, &spec.Name, &spec.Description, &enabled,
			&spec.Revision, &spec.ContentHash, &raw); err != nil {
			return err
		}
		var content struct {
			Match domain.RoutingPolicyMatch `json:"match"`
			Route domain.RoutingPolicyRoute `json:"route"`
		}
		if err := json.Unmarshal(raw, &content); err != nil {
			return fmt.Errorf("decode policy %s revision %d: %w", spec.ID, spec.Revision, err)
		}
		spec.Priority = priority
		spec.Enabled = enabled == 1
		spec.Match = content.Match
		spec.Route = content.Route
		policies = append(policies, spec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(policies) > 0 {
		f.RoutingPolicies = policies
	}
	return nil
}

func syncPolicyRowsTx(tx *sql.Tx, f *config.File) error {
	if len(f.RoutingPolicies) == 0 {
		return nil
	}
	if err := domain.ValidateRoutingPolicies(f.RoutingPolicies); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for i := range f.RoutingPolicies {
		spec := &f.RoutingPolicies[i]
		hash, err := domain.RoutingPolicyContentHash(*spec)
		if err != nil {
			return err
		}
		content, err := json.Marshal(struct {
			Match domain.RoutingPolicyMatch `json:"match"`
			Route domain.RoutingPolicyRoute `json:"route"`
		}{spec.Match, spec.Route})
		if err != nil {
			return err
		}

		var revision int64
		err = tx.QueryRow(`SELECT revision FROM policy_revisions WHERE policy_id=? AND content_hash=?`, spec.ID, hash).Scan(&revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRow(`SELECT COALESCE(MAX(revision),0)+1 FROM policy_revisions WHERE policy_id=?`, spec.ID).Scan(&revision); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO policies(id,name,description,priority,enabled,active_revision,created_at,updated_at)
				VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, spec.ID, spec.Name, spec.Description, spec.Priority, boolInt(spec.Enabled), revision, now, now); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO policy_revisions(policy_id,revision,content_hash,content_json,created_at) VALUES(?,?,?,?,?)`, spec.ID, revision, hash, content, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO policies(id,name,description,priority,enabled,active_revision,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,description=excluded.description,priority=excluded.priority,
			enabled=excluded.enabled,active_revision=excluded.active_revision,updated_at=excluded.updated_at`,
			spec.ID, spec.Name, spec.Description, spec.Priority, boolInt(spec.Enabled), revision, now, now); err != nil {
			return err
		}
		spec.Revision = revision
		spec.ContentHash = hash
	}
	return nil
}

func ensurePolicySetTx(tx *sql.Tx, configVersion int64, f *config.File) error {
	if len(f.RoutingPolicies) == 0 {
		return nil
	}
	var exists int
	err := tx.QueryRow(`SELECT 1 FROM policy_set_versions WHERE config_version=?`, configVersion).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	type member struct {
		ID       string `json:"id"`
		Priority int    `json:"priority"`
		Revision int64  `json:"revision"`
		Hash     string `json:"hash"`
	}
	members := make([]member, 0, len(f.RoutingPolicies))
	for _, spec := range f.RoutingPolicies {
		if spec.Enabled {
			members = append(members, member{spec.ID, spec.Priority, spec.Revision, spec.ContentHash})
		}
	}
	raw, err := json.Marshal(members)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	indexHash := hex.EncodeToString(sum[:])
	if _, err := tx.Exec(`UPDATE policy_set_versions SET active=0 WHERE active=1`); err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO policy_set_versions(config_version,index_hash,created_at,active) VALUES(?,?,?,1)`, configVersion, indexHash, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	setVersion, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for position, spec := range f.RoutingPolicies {
		if _, err := tx.Exec(`INSERT INTO policy_set_members(set_version,position,priority,name,description,policy_id,policy_revision,enabled) VALUES(?,?,?,?,?,?,?,?)`,
			setVersion, position, spec.Priority, spec.Name, spec.Description, spec.ID, spec.Revision, boolInt(spec.Enabled)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateLegacyActiveConfig() error {
	var raw []byte
	err := s.db.QueryRow(`SELECT config_json FROM control_config_versions WHERE active=1 LIMIT 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := decodeConfig(raw)
	if err != nil {
		return err
	}
	_, err = config.MigrateLegacyRoutingPolicies(f)
	if err != nil {
		return err
	}
	// SaveConfig is idempotent. Besides translating the legacy DAG once, this
	// also backfills normalized rows for databases created by older binaries.
	_, err = s.SaveConfig(f)
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func addColumnIfMissing(db *sql.DB, table, column, definition string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return false, err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if found {
		return false, nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		return false, err
	}
	return true, nil
}
