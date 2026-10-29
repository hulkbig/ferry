package controlstore

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrManagementTokenInvalid = errors.New("management token is invalid, disabled, expired, or revoked")
	ErrManagementTokenRevoked = errors.New("revoked management token cannot be re-enabled or rotated")
)

// ManagementToken is the safe, administrator-visible identity for one Skill
// credential. The raw secret and verifier hash never leave the control store.
type ManagementToken struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	Preview       string     `json:"preview"`
	Enabled       bool       `json:"enabled"`
	AllowHighRisk bool       `json:"allow_high_risk"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
}

type managementTokenRow struct {
	ManagementToken
	secretHash []byte
}

func validManagementTokenName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	for _, value := range name {
		if value < 0x20 || value == 0x7f {
			return false
		}
	}
	return true
}

func validManagementTokenDescription(description string) bool {
	return len(description) <= 240 && utf8.ValidString(description)
}

func generateManagementTokenSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "smt_" + hex.EncodeToString(raw), nil
}

func managementTokenHash(secret string) []byte {
	digest := sha256.Sum256([]byte(secret))
	return digest[:]
}

func scanManagementToken(scanner interface{ Scan(...any) error }) (*managementTokenRow, error) {
	var row managementTokenRow
	var enabled, allowHighRisk int
	var createdAt, updatedAt int64
	var expiresAt, revokedAt, lastUsedAt sql.NullInt64
	if err := scanner.Scan(&row.ID, &row.Name, &row.Description, &row.secretHash, &row.Preview,
		&enabled, &allowHighRisk, &createdAt, &updatedAt, &expiresAt, &revokedAt, &lastUsedAt); err != nil {
		return nil, err
	}
	row.Enabled = enabled != 0
	row.AllowHighRisk = allowHighRisk != 0
	row.CreatedAt = time.UnixMilli(createdAt)
	row.UpdatedAt = time.UnixMilli(updatedAt)
	row.ExpiresAt = nullableMillis(expiresAt)
	row.RevokedAt = nullableMillis(revokedAt)
	row.LastUsedAt = nullableMillis(lastUsedAt)
	return &row, nil
}

const managementTokenColumns = `id,name,description,secret_hash,preview,enabled,allow_high_risk,
	created_at,updated_at,expires_at,revoked_at,last_used_at` // #nosec G101 -- SQL column names, not a credential

func (s *Store) CreateManagementToken(name, description string, expiresAt *time.Time,
	allowHighRisk bool, actor, remoteAddr string, now time.Time) (*ManagementToken, string, error) {
	name = strings.TrimSpace(name)
	if !validManagementTokenName(name) {
		return nil, "", fmt.Errorf("token name must contain 1-64 visible characters")
	}
	description = strings.TrimSpace(description)
	if !validManagementTokenDescription(description) {
		return nil, "", fmt.Errorf("token description must contain at most 240 UTF-8 bytes")
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return nil, "", fmt.Errorf("token expiry must be in the future")
	}
	id, err := randomID("mtok_")
	if err != nil {
		return nil, "", err
	}
	secret, err := generateManagementTokenSecret()
	if err != nil {
		return nil, "", err
	}
	var expiry any
	if expiresAt != nil {
		expiry = expiresAt.UnixMilli()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO management_tokens(
		id,name,description,secret_hash,preview,enabled,allow_high_risk,created_at,updated_at,expires_at
	) VALUES(?,?,?,?,?,1,?,?,?,?)`, id, name, description, managementTokenHash(secret),
		previewSecret(secret), boolInt(allowHighRisk), now.UnixMilli(), now.UnixMilli(), expiry); err != nil {
		return nil, "", err
	}
	if err := insertManagementSecurityEvent(tx, now, "management_token_created", actor, remoteAddr,
		map[string]any{"token_id": id, "name": name, "allow_high_risk": allowHighRisk, "expires_at": expiresAt}); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return &ManagementToken{ID: id, Name: name, Description: description,
		Preview: previewSecret(secret), Enabled: true, AllowHighRisk: allowHighRisk,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: expiresAt}, secret, nil
}

func (s *Store) ManagementTokens() ([]ManagementToken, error) {
	rows, err := s.db.Query(`SELECT ` + managementTokenColumns + ` FROM management_tokens ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ManagementToken{}
	for rows.Next() {
		row, err := scanManagementToken(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row.ManagementToken)
	}
	return result, rows.Err()
}

func (s *Store) ManagementToken(id string) (*ManagementToken, error) {
	row, err := scanManagementToken(s.db.QueryRow(`SELECT `+managementTokenColumns+` FROM management_tokens WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row.ManagementToken, nil
}

// AuthenticateManagementToken validates one high-entropy bearer token and
// records bounded usage evidence. Token use never grants the ability to alter
// token identities themselves; that remains a browser-session-only action.
func (s *Store) AuthenticateManagementToken(secret, remoteAddr string, now time.Time) (*ManagementToken, error) {
	if !strings.HasPrefix(secret, "smt_") {
		return nil, ErrManagementTokenInvalid
	}
	want := managementTokenHash(secret)
	rows, err := s.db.Query(`SELECT ` + managementTokenColumns + ` FROM management_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var matched *managementTokenRow
	for rows.Next() {
		row, scanErr := scanManagementToken(rows)
		if scanErr != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return nil, scanErr
		}
		if constantTimeBytesEqual(want, row.secretHash) {
			matched = row
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if matched == nil || !matched.Enabled || matched.RevokedAt != nil ||
		(matched.ExpiresAt != nil && !matched.ExpiresAt.After(now)) {
		return nil, ErrManagementTokenInvalid
	}
	shouldAudit := matched.LastUsedAt == nil || now.Sub(*matched.LastUsedAt) >= time.Hour
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE management_tokens SET last_used_at=? WHERE id=?`, now.UnixMilli(), matched.ID); err != nil {
		return nil, err
	}
	if shouldAudit {
		if err := insertManagementSecurityEvent(tx, now, "management_token_used", "token:"+matched.ID,
			remoteAddr, map[string]any{"token_id": matched.ID, "name": matched.Name}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	matched.LastUsedAt = &now
	return &matched.ManagementToken, nil
}

func (s *Store) UpdateManagementToken(id string, name, description *string, enabled,
	allowHighRisk *bool, expiresAt *time.Time, clearExpiry bool, actor, remoteAddr string,
	now time.Time) (*ManagementToken, error) {
	current, err := s.ManagementToken(id)
	if err != nil || current == nil {
		return current, err
	}
	if current.RevokedAt != nil && enabled != nil && *enabled {
		return nil, ErrManagementTokenRevoked
	}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if !validManagementTokenName(trimmed) {
			return nil, fmt.Errorf("token name must contain 1-64 visible characters")
		}
		current.Name = trimmed
	}
	if description != nil {
		trimmed := strings.TrimSpace(*description)
		if !validManagementTokenDescription(trimmed) {
			return nil, fmt.Errorf("token description must contain at most 240 UTF-8 bytes")
		}
		current.Description = trimmed
	}
	if enabled != nil {
		current.Enabled = *enabled
	}
	if allowHighRisk != nil {
		current.AllowHighRisk = *allowHighRisk
	}
	if clearExpiry {
		current.ExpiresAt = nil
	} else if expiresAt != nil {
		if !expiresAt.After(now) {
			return nil, fmt.Errorf("token expiry must be in the future")
		}
		current.ExpiresAt = expiresAt
	}
	var expiry any
	if current.ExpiresAt != nil {
		expiry = current.ExpiresAt.UnixMilli()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE management_tokens SET name=?,description=?,enabled=?,allow_high_risk=?,
		expires_at=?,updated_at=? WHERE id=?`, current.Name, current.Description, boolInt(current.Enabled),
		boolInt(current.AllowHighRisk), expiry, now.UnixMilli(), id)
	if err != nil {
		return nil, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return nil, fmt.Errorf("management token %s not found", id)
	}
	if err := insertManagementSecurityEvent(tx, now, "management_token_updated", actor, remoteAddr,
		map[string]any{"token_id": id, "enabled": current.Enabled, "allow_high_risk": current.AllowHighRisk,
			"expires_at": current.ExpiresAt}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	current.UpdatedAt = now
	return current, nil
}

func (s *Store) RotateManagementToken(id, actor, remoteAddr string, now time.Time) (*ManagementToken, string, error) {
	current, err := s.ManagementToken(id)
	if err != nil || current == nil {
		return current, "", err
	}
	if current.RevokedAt != nil {
		return nil, "", ErrManagementTokenRevoked
	}
	secret, err := generateManagementTokenSecret()
	if err != nil {
		return nil, "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE management_tokens SET secret_hash=?,preview=?,updated_at=? WHERE id=?`,
		managementTokenHash(secret), previewSecret(secret), now.UnixMilli(), id); err != nil {
		return nil, "", err
	}
	if err := insertManagementSecurityEvent(tx, now, "management_token_rotated", actor, remoteAddr,
		map[string]any{"token_id": id}); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	current.Preview = previewSecret(secret)
	current.UpdatedAt = now
	return current, secret, nil
}

func (s *Store) RevokeManagementToken(id, actor, remoteAddr string, now time.Time) (*ManagementToken, error) {
	current, err := s.ManagementToken(id)
	if err != nil || current == nil {
		return current, err
	}
	if current.RevokedAt != nil {
		return current, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE management_tokens SET enabled=0,revoked_at=?,updated_at=? WHERE id=?`,
		now.UnixMilli(), now.UnixMilli(), id); err != nil {
		return nil, err
	}
	if err := insertManagementSecurityEvent(tx, now, "management_token_revoked", actor, remoteAddr,
		map[string]any{"token_id": id}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	current.Enabled = false
	current.RevokedAt = &now
	current.UpdatedAt = now
	return current, nil
}

func (s *Store) DeleteManagementToken(id, actor, remoteAddr string, now time.Time) error {
	current, err := s.ManagementToken(id)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("management token %s not found", id)
	}
	if current.RevokedAt == nil {
		return fmt.Errorf("management token must be revoked before deletion")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM management_tokens WHERE id=?`, id); err != nil {
		return err
	}
	if err := insertManagementSecurityEvent(tx, now, "management_token_deleted", actor, remoteAddr,
		map[string]any{"token_id": id, "name": current.Name}); err != nil {
		return err
	}
	return tx.Commit()
}
