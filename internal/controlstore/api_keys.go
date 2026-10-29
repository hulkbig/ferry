package controlstore

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
)

var ErrLastAuthenticatingSecret = errors.New("cannot retire the last authenticating API key secret")

var ErrAPIKeyRevoked = errors.New("revoked API key cannot be re-enabled")

func (s *Store) UpdateAPIKeyPolicy(apiKeyID, description string, enabled *bool, scopeModels []string,
	scopeProtocols []domain.Protocol, expiresAt *time.Time, rateLimitRPM int, monthlyBudgetUSD float64,
	exactOnly bool) error {
	modelsRaw, err := json.Marshal(scopeModels)
	if err != nil {
		return err
	}
	protocolsRaw, err := json.Marshal(scopeProtocols)
	if err != nil {
		return err
	}
	var expires any
	if expiresAt != nil {
		expires = expiresAt.UnixMilli()
	}
	var currentEnabled int
	var revoked sql.NullInt64
	if err := s.db.QueryRow(`SELECT enabled,revoked_at FROM api_keys WHERE id=?`, apiKeyID).Scan(&currentEnabled, &revoked); err != nil {
		return err
	}
	if enabled != nil && *enabled && revoked.Valid {
		return ErrAPIKeyRevoked
	}
	value := currentEnabled
	if enabled != nil {
		value = boolInt(*enabled)
	}
	_, err = s.db.Exec(`UPDATE api_keys SET description=?,enabled=?,scope_models_json=?,scope_protocols_json=?,
		expires_at=?,rate_limit_rpm=?,monthly_budget_usd=?,exact_only=?,updated_at=? WHERE id=?`, description, value,
		modelsRaw, protocolsRaw, expires, rateLimitRPM, monthlyBudgetUSD, boolInt(exactOnly), time.Now().UnixMilli(), apiKeyID)
	return err
}

func (s *Store) RevokeAPIKey(apiKeyID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	res, err := tx.Exec(`UPDATE api_keys SET enabled=0,revoked_at=COALESCE(revoked_at,?),updated_at=? WHERE id=?`, now, now, apiKeyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("api key %s not found", apiKeyID)
	}
	if _, err := tx.Exec(`UPDATE api_key_secrets SET state='RETIRED',retired_at=COALESCE(retired_at,?)
		WHERE api_key_id=? AND state IN ('ACTIVE','GRACE')`, now, apiKeyID); err != nil {
		return err
	}
	return tx.Commit()
}

// APIKeys returns identities and safe secret metadata. It never returns a raw
// secret or a verifier hash; Revealable only describes whether an explicit
// administrator reveal is available.
func (s *Store) APIKeys() ([]domain.APIKeyIdentity, map[string][]domain.APIKeySecret, error) {
	identities, err := readAPIKeyIdentities(s.db)
	if err != nil {
		return nil, nil, err
	}
	secrets := make(map[string][]domain.APIKeySecret, len(identities))
	rows, err := s.db.Query(`SELECT id,api_key_id,preview,(recoverable_secret IS NOT NULL AND state='ACTIVE'),state,created_at,retired_at,last_used_at
		FROM api_key_secrets ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.APIKeySecret
		var createdAt int64
		var retiredAt, lastUsedAt sql.NullInt64
		if err := rows.Scan(&item.ID, &item.APIKeyID, &item.Preview, &item.Revealable, &item.State,
			&createdAt, &retiredAt, &lastUsedAt); err != nil {
			return nil, nil, err
		}
		item.CreatedAt = time.UnixMilli(createdAt)
		if retiredAt.Valid {
			value := time.UnixMilli(retiredAt.Int64)
			item.RetiredAt = &value
		}
		if lastUsedAt.Valid {
			value := time.UnixMilli(lastUsedAt.Int64)
			item.LastUsedAt = &value
		}
		secrets[item.APIKeyID] = append(secrets[item.APIKeyID], item)
	}
	return identities, secrets, rows.Err()
}

// APIKeyActiveSecret returns the recoverable value for the current ACTIVE
// secret. Legacy hash-only rows deliberately return available=false: changing
// such a key requires an explicit rotation so existing clients are not broken.
func (s *Store) APIKeyActiveSecret(apiKeyID string) (value string, available bool, err error) {
	var secret sql.NullString
	err = s.db.QueryRow(`SELECT recoverable_secret FROM api_key_secrets
		WHERE api_key_id=? AND state='ACTIVE'`, apiKeyID).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !secret.Valid || secret.String == "" {
		return "", false, nil
	}
	return secret.String, true, nil
}

// APIKeyAuthenticators builds the immutable hash-only representation consumed
// by a scheduler world. RETIRED secrets are intentionally excluded.
func (s *Store) APIKeyAuthenticators() ([]domain.APIKeyAuthenticator, error) {
	identities, err := readAPIKeyIdentities(s.db)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*domain.APIKeyAuthenticator, len(identities))
	out := make([]domain.APIKeyAuthenticator, len(identities))
	for i := range identities {
		out[i].Identity = identities[i]
		byID[identities[i].ID] = &out[i]
	}
	profileRows, err := s.db.Query(`SELECT id,default_logical_model,default_objective FROM api_keys`)
	if err != nil {
		return nil, err
	}
	for profileRows.Next() {
		var id, defaultModel, defaultObjective string
		if err := profileRows.Scan(&id, &defaultModel, &defaultObjective); err != nil {
			profileRows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return nil, err
		}
		if auth := byID[id]; auth != nil {
			auth.LegacyDefaultModel = defaultModel
			auth.LegacyDefaultObjective = defaultObjective
		}
	}
	if err := profileRows.Close(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,api_key_id,secret_hash FROM api_key_secrets
		WHERE state IN ('ACTIVE','GRACE') ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var secretID, id string
		var raw []byte
		if err := rows.Scan(&secretID, &id, &raw); err != nil {
			return nil, err
		}
		if len(raw) != sha256.Size {
			return nil, fmt.Errorf("api key %s has invalid secret hash length %d", id, len(raw))
		}
		auth := byID[id]
		if auth == nil {
			return nil, fmt.Errorf("api key secret references missing identity %s", id)
		}
		var hash [sha256.Size]byte
		copy(hash[:], raw)
		auth.SecretHashes = append(auth.SecretHashes, hash)
		auth.SecretIDs = append(auth.SecretIDs, secretID)
	}
	return out, rows.Err()
}

// RetireAPIKeySecret makes a secret permanently non-authenticating.
func (s *Store) RetireAPIKeySecret(apiKeyID, secretID string) error {
	now := time.Now().UnixMilli()
	// The correlated count is evaluated inside the same serialized SQLite
	// write statement, so concurrent retire calls cannot both remove the last
	// two authenticating secrets.
	res, err := s.db.Exec(`UPDATE api_key_secrets SET state='RETIRED',retired_at=?
		WHERE id=? AND api_key_id=? AND state IN ('ACTIVE','GRACE')
		AND (SELECT COUNT(*) FROM api_key_secrets
			WHERE api_key_id=? AND state IN ('ACTIVE','GRACE')) > 1`,
		now, secretID, apiKeyID, apiKeyID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}
	var state domain.APIKeySecretState
	if err := s.db.QueryRow(`SELECT state FROM api_key_secrets WHERE id=? AND api_key_id=?`,
		secretID, apiKeyID).Scan(&state); err != nil {
		return fmt.Errorf("active or grace api key secret %s not found", secretID)
	}
	if state == domain.APIKeySecretActive || state == domain.APIKeySecretGrace {
		return ErrLastAuthenticatingSecret
	}
	return fmt.Errorf("active or grace api key secret %s not found", secretID)
}

// APIKeySecretState returns the current lifecycle state for compensation of a
// failed runtime credential refresh.
func (s *Store) APIKeySecretState(apiKeyID, secretID string) (domain.APIKeySecretState, error) {
	var state domain.APIKeySecretState
	err := s.db.QueryRow(`SELECT state FROM api_key_secrets WHERE id=? AND api_key_id=?`,
		secretID, apiKeyID).Scan(&state)
	return state, err
}

// RestoreAPIKeySecretState compensates only management operations whose
// corresponding atomic runtime refresh failed.
func (s *Store) RestoreAPIKeySecretState(apiKeyID, secretID string, state domain.APIKeySecretState) error {
	if state != domain.APIKeySecretActive && state != domain.APIKeySecretGrace {
		return fmt.Errorf("cannot restore API key secret to state %q", state)
	}
	result, err := s.db.Exec(`UPDATE api_key_secrets SET state=?,retired_at=NULL
		WHERE id=? AND api_key_id=?`, state, secretID, apiKeyID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("restore API key secret %s: rows=%d err=%v", secretID, rows, err)
	}
	return nil
}

// RecordAPIKeySecretUse stores real authentication evidence for graceful
// rotation decisions. Timestamps advance monotonically across concurrent use.
func (s *Store) RecordAPIKeySecretUse(secretID string, usedAt time.Time) error {
	if secretID == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE api_key_secrets SET last_used_at=? WHERE id=?
		AND (last_used_at IS NULL OR last_used_at < ?)`, usedAt.UnixMilli(), secretID, usedAt.UnixMilli())
	return err
}

func (s *Store) UpdateAPIKeyDescription(apiKeyID, description string) error {
	res, err := s.db.Exec(`UPDATE api_keys SET description=?,updated_at=? WHERE id=?`,
		description, time.Now().UnixMilli(), apiKeyID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("api key %s not found", apiKeyID)
	}
	return nil
}

func (s *Store) migrateLegacyAPIKeys() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var activeRaw []byte
	err = tx.QueryRow(`SELECT config_json FROM control_config_versions WHERE active=1 LIMIT 1`).
		Scan(&activeRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	active, err := decodeConfig(activeRaw)
	if err != nil {
		return err
	}
	if err := syncAPIKeysTx(tx, active); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT version,config_json FROM control_config_versions ORDER BY version`)
	if err != nil {
		return err
	}
	type versionRow struct {
		version int64
		raw     []byte
	}
	versions := []versionRow{}
	for rows.Next() {
		var row versionRow
		if err := rows.Scan(&row.version, &row.raw); err != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return err
		}
		versions = append(versions, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range versions {
		var candidate config.File
		if err := json.Unmarshal(row.raw, &candidate); err != nil {
			return fmt.Errorf("decode config version %d for secret sanitization: %w", row.version, err)
		}
		sanitized, err := sanitizedConfigJSON(&candidate)
		if err != nil {
			return err
		}
		if string(sanitized) == string(row.raw) {
			continue
		}
		if _, err := tx.Exec(`UPDATE control_config_versions SET config_json=? WHERE version=?`, sanitized, row.version); err != nil {
			return err
		}
	}
	clearConsumerSecrets(active)
	return tx.Commit()
}

// syncAPIKeysTx treats ConsumerKeys only as a legacy compatibility facade.
// Raw Key values are one-way migration/rotation inputs and are never encoded
// back into configuration history.
func syncAPIKeysTx(tx *sql.Tx, f *config.File) error {
	if f == nil {
		return fmt.Errorf("config must be non-nil")
	}
	now := time.Now().UnixMilli()
	present := make(map[string]bool, len(f.ConsumerKeys))
	for i := range f.ConsumerKeys {
		consumer := &f.ConsumerKeys[i]
		name := strings.TrimSpace(consumer.Name)
		if name == "" {
			return fmt.Errorf("consumer key name must be non-empty")
		}
		if present[name] {
			return fmt.Errorf("duplicate consumer key name %q", name)
		}
		present[name] = true
		enabled := consumer.Enabled == nil || *consumer.Enabled
		id := legacyAPIKeyID(name)
		var existingID string
		err := tx.QueryRow(`SELECT id FROM api_keys WHERE name=?`, name).Scan(&existingID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(`INSERT INTO api_keys(
				id,name,description,enabled,default_logical_model,default_objective,monthly_budget_usd,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?)`, id, name, "", boolInt(enabled), consumer.DefaultAlias,
				consumer.DefaultObjective, consumer.MonthlyBudgetUSD, now, now); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			id = existingID
			if _, err := tx.Exec(`UPDATE api_keys SET enabled=CASE WHEN revoked_at IS NULL THEN ? ELSE 0 END,default_logical_model=?,
				default_objective=?,monthly_budget_usd=?,updated_at=? WHERE id=?`,
				boolInt(enabled), consumer.DefaultAlias, consumer.DefaultObjective,
				consumer.MonthlyBudgetUSD, now, id); err != nil {
				return err
			}
		}
		if consumer.Key != "" {
			if err := activateSecretTx(tx, id, consumer.Key, now); err != nil {
				return fmt.Errorf("api key %s: %w", name, err)
			}
		}
	}
	rows, err := tx.Query(`SELECT id,name FROM api_keys`)
	if err != nil {
		return err
	}
	type staleIdentity struct{ id, name string }
	stale := []staleIdentity{}
	for rows.Next() {
		var item staleIdentity
		if err := rows.Scan(&item.id, &item.name); err != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return err
		}
		if !present[item.name] {
			stale = append(stale, item)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range stale {
		if _, err := tx.Exec(`DELETE FROM api_keys WHERE id=?`, item.id); err != nil {
			return err
		}
	}
	return nil
}

func activateSecretTx(tx *sql.Tx, apiKeyID, rawSecret string, now int64) error {
	hash := domain.HashAPIKeySecret(rawSecret)
	var existingID, existingOwner string
	var existingState domain.APIKeySecretState
	err := tx.QueryRow(`SELECT id,api_key_id,state FROM api_key_secrets WHERE secret_hash=?`, hash[:]).
		Scan(&existingID, &existingOwner, &existingState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if existingOwner != apiKeyID {
			return fmt.Errorf("secret already belongs to another api key")
		}
		if existingState == domain.APIKeySecretRetired {
			return fmt.Errorf("retired secret cannot be reused")
		}
		if existingState == domain.APIKeySecretActive {
			_, err = tx.Exec(`UPDATE api_key_secrets SET recoverable_secret=COALESCE(recoverable_secret,?) WHERE id=?`, rawSecret, existingID)
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE api_key_secrets SET state='GRACE'
		WHERE api_key_id=? AND state='ACTIVE'`, apiKeyID); err != nil {
		return err
	}
	if existingID != "" {
		_, err = tx.Exec(`UPDATE api_key_secrets SET state='ACTIVE',retired_at=NULL,
			recoverable_secret=COALESCE(recoverable_secret,?) WHERE id=?`, rawSecret, existingID)
		return err
	}
	secretID, err := randomID("sec_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO api_key_secrets(
		id,api_key_id,secret_hash,preview,recoverable_secret,state,created_at
	) VALUES(?,?,?,?,?,?,?)`, secretID, apiKeyID, hash[:], previewSecret(rawSecret), rawSecret, domain.APIKeySecretActive, now)
	return err
}

func readAPIKeyIdentities(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}) ([]domain.APIKeyIdentity, error) {
	rows, err := q.Query(`SELECT id,name,description,enabled,default_logical_model,
		default_objective,monthly_budget_usd,created_at,updated_at,revoked_at,
		scope_models_json,scope_protocols_json,expires_at,rate_limit_rpm,exact_only FROM api_keys ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.APIKeyIdentity{}
	for rows.Next() {
		var item domain.APIKeyIdentity
		var legacyDefaultModel, legacyDefaultObjective string
		var legacyMonthlyBudget float64
		var enabled, exactOnly int
		var createdAt, updatedAt int64
		var revokedAt, expiresAt sql.NullInt64
		var modelsRaw, protocolsRaw []byte
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &enabled,
			&legacyDefaultModel, &legacyDefaultObjective, &legacyMonthlyBudget,
			&createdAt, &updatedAt, &revokedAt, &modelsRaw, &protocolsRaw, &expiresAt, &item.RateLimitRPM, &exactOnly); err != nil {
			return nil, err
		}
		item.MonthlyBudgetUSD = legacyMonthlyBudget
		_ = json.Unmarshal(modelsRaw, &item.ScopeModels)
		_ = json.Unmarshal(protocolsRaw, &item.ScopeProtocols)
		if item.ScopeModels == nil {
			item.ScopeModels = []string{}
		}
		if item.ScopeProtocols == nil {
			item.ScopeProtocols = []domain.Protocol{}
		}
		if revokedAt.Valid {
			value := time.UnixMilli(revokedAt.Int64)
			item.RevokedAt = &value
		}
		if expiresAt.Valid {
			value := time.UnixMilli(expiresAt.Int64)
			item.ExpiresAt = &value
		}
		item.Enabled = enabled != 0
		item.ExactOnly = exactOnly != 0
		item.CreatedAt = time.UnixMilli(createdAt)
		item.UpdatedAt = time.UnixMilli(updatedAt)
		out = append(out, item)
	}
	return out, rows.Err()
}

func hydrateAPIKeyIdentities(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, f *config.File) error {
	rows, err := q.Query(`SELECT name,enabled,default_logical_model,default_objective,
		monthly_budget_usd FROM api_keys ORDER BY name,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	f.ConsumerKeys = nil
	for rows.Next() {
		var name, defaultModel, defaultObjective string
		var enabledInt int
		var monthlyBudget float64
		if err := rows.Scan(&name, &enabledInt, &defaultModel, &defaultObjective, &monthlyBudget); err != nil {
			return err
		}
		enabled := enabledInt != 0
		f.ConsumerKeys = append(f.ConsumerKeys, config.ConsumerKey{
			Name: name, Enabled: &enabled, DefaultAlias: defaultModel,
			DefaultObjective: defaultObjective, MonthlyBudgetUSD: monthlyBudget,
		})
	}
	return rows.Err()
}

func sanitizedConfigJSON(f *config.File) ([]byte, error) {
	if f == nil {
		return nil, fmt.Errorf("config must be non-nil")
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	var clone config.File
	if err := json.Unmarshal(raw, &clone); err != nil {
		return nil, fmt.Errorf("clone config for secret sanitization: %w", err)
	}
	clearConsumerSecrets(&clone)
	return json.Marshal(&clone)
}

func clearConsumerSecrets(f *config.File) {
	if f == nil {
		return
	}
	for i := range f.ConsumerKeys {
		f.ConsumerKeys[i].Key = ""
	}
}

func previewSecret(secret string) string {
	if len(secret) <= 7 {
		return "••••"
	}
	return secret[:3] + "••••••••" + secret[len(secret)-4:]
}

func legacyAPIKeyID(name string) string {
	hash := sha256.Sum256([]byte("smartapi-api-key:" + name))
	return "key_" + hex.EncodeToString(hash[:12])
}

func randomID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}
