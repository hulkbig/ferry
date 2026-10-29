package controlstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ferry/internal/config"
)

var (
	ErrDraftNotFound = errors.New("configuration draft not found")
	ErrDraftApplied  = errors.New("configuration draft is already applied")
)

type ConfigurationDraft struct {
	ID                  string       `json:"id"`
	BaseRevision        int64        `json:"base_revision"`
	Intent              *config.File `json:"-"`
	Status              string       `json:"status"`
	RestoreFromRevision int64        `json:"restore_from_revision,omitempty"`
	AppliedRevision     int64        `json:"applied_revision,omitempty"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
}

type ConfigurationRevision struct {
	Revision             int64     `json:"revision"`
	CreatedAt            time.Time `json:"created_at"`
	Active               bool      `json:"active"`
	RestoredFromRevision int64     `json:"restored_from_revision,omitempty"`
	Actor                string    `json:"actor"`
	Source               string    `json:"source"`
	Summary              string    `json:"summary"`
	Resources            []string  `json:"resources"`
}

func (s *Store) RecordRevisionAudit(revision int64, actor, source, summary string, resources []string) error {
	if source == "" {
		source = "console"
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO configuration_revision_audit(revision,actor,source,summary,resources_json)
		VALUES(?,?,?,?,?) ON CONFLICT(revision) DO UPDATE SET actor=excluded.actor,source=excluded.source,
		summary=excluded.summary,resources_json=excluded.resources_json`, revision, actor, source, summary, raw)
	return err
}

func (s *Store) CreateDraft(baseRevision int64, intent *config.File, restoreFromRevision int64) (*ConfigurationDraft, error) {
	if baseRevision <= 0 {
		_, active, err := s.ActiveConfig()
		if err != nil {
			return nil, err
		}
		baseRevision = active
	}
	if intent == nil {
		var err error
		intent, err = s.ConfigRevision(baseRevision)
		if err != nil {
			return nil, err
		}
	}
	config.EnsureSameNameLogicalModels(intent)
	raw, err := sanitizedConfigJSON(intent)
	if err != nil {
		return nil, err
	}
	id, err := randomID("draft_")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var restore any
	if restoreFromRevision > 0 {
		restore = restoreFromRevision
	}
	_, err = s.db.Exec(`INSERT INTO configuration_drafts(
		id,base_revision,intent_json,status,restore_from_revision,created_at,updated_at
	) VALUES(?,?,?,'OPEN',?,?,?)`, id, baseRevision, raw, restore, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	return &ConfigurationDraft{
		ID: id, BaseRevision: baseRevision, Intent: intent, Status: "OPEN",
		RestoreFromRevision: restoreFromRevision, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Store) Draft(id string) (*ConfigurationDraft, error) {
	var (
		draft                ConfigurationDraft
		raw                  []byte
		createdAt, updatedAt int64
		restoreFrom, applied sql.NullInt64
	)
	err := s.db.QueryRow(`SELECT id,base_revision,intent_json,status,
		restore_from_revision,applied_revision,created_at,updated_at
		FROM configuration_drafts WHERE id=?`, id).
		Scan(&draft.ID, &draft.BaseRevision, &raw, &draft.Status,
			&restoreFrom, &applied, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, err
	}
	intent, err := decodeConfig(raw)
	if err != nil {
		return nil, err
	}
	draft.Intent = intent
	draft.CreatedAt = time.UnixMilli(createdAt)
	draft.UpdatedAt = time.UnixMilli(updatedAt)
	if restoreFrom.Valid {
		draft.RestoreFromRevision = restoreFrom.Int64
	}
	if applied.Valid {
		draft.AppliedRevision = applied.Int64
	}
	return &draft, nil
}

func (s *Store) UpdateDraft(id string, intent *config.File) (*ConfigurationDraft, error) {
	if intent == nil {
		return nil, fmt.Errorf("draft intent must be non-nil")
	}
	config.EnsureSameNameLogicalModels(intent)
	raw, err := sanitizedConfigJSON(intent)
	if err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`UPDATE configuration_drafts SET intent_json=?,updated_at=?
		WHERE id=? AND status='OPEN'`, raw, time.Now().UnixMilli(), id)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		if existing, lookupErr := s.Draft(id); lookupErr == nil && existing.Status == "APPLIED" {
			return nil, ErrDraftApplied
		}
		return nil, ErrDraftNotFound
	}
	return s.Draft(id)
}

func (s *Store) DeleteDraft(id string) error {
	res, err := s.db.Exec(`DELETE FROM configuration_drafts WHERE id=? AND status='OPEN'`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		if existing, lookupErr := s.Draft(id); lookupErr == nil && existing.Status == "APPLIED" {
			return ErrDraftApplied
		}
		return ErrDraftNotFound
	}
	return nil
}

func (s *Store) MarkDraftApplied(id string, revision int64) error {
	res, err := s.db.Exec(`UPDATE configuration_drafts
		SET status='APPLIED',applied_revision=?,updated_at=? WHERE id=? AND status='OPEN'`,
		revision, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("mark draft %s applied: rows=%d err=%v", id, n, err)
		}
		existing, lookupErr := s.Draft(id)
		if lookupErr == nil && existing.Status == "APPLIED" && existing.AppliedRevision == revision {
			return nil
		}
		return fmt.Errorf("mark draft %s applied: rows=%d", id, n)
	}
	return nil
}

func (s *Store) ConfigRevision(revision int64) (*config.File, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT config_json FROM control_config_versions WHERE version=?`, revision).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("configuration revision %d not found", revision)
	}
	if err != nil {
		return nil, err
	}
	intent, err := decodeConfig(raw)
	if err != nil {
		return nil, err
	}
	if err := hydrateRoutingPolicies(s.db, revision, intent); err != nil {
		return nil, err
	}
	if err := hydrateAPIKeyIdentities(s.db, intent); err != nil {
		return nil, err
	}
	return intent, nil
}

const configurationRevisionsQuery = `SELECT v.version,v.created_at,v.active,
		COALESCE((SELECT d.restore_from_revision FROM configuration_drafts d
			WHERE d.applied_revision=v.version AND d.status='APPLIED' LIMIT 1),0),
		COALESCE(a.actor,''),COALESCE(a.source,'system'),COALESCE(a.summary,'Configuration revision'),
		COALESCE(a.resources_json,'[]')
		FROM control_config_versions v LEFT JOIN configuration_revision_audit a ON a.revision=v.version
		ORDER BY v.version DESC`

func (s *Store) ConfigRevisions() ([]ConfigurationRevision, error) {
	rows, err := s.db.Query(configurationRevisionsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConfigurationRevision{}
	for rows.Next() {
		var item ConfigurationRevision
		var createdAt int64
		var active int
		var resourcesRaw []byte
		if err := rows.Scan(&item.Revision, &createdAt, &active, &item.RestoredFromRevision,
			&item.Actor, &item.Source, &item.Summary, &resourcesRaw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(resourcesRaw, &item.Resources)
		if item.Resources == nil {
			item.Resources = []string{}
		}
		item.CreatedAt = time.UnixMilli(createdAt)
		item.Active = active != 0
		out = append(out, item)
	}
	return out, rows.Err()
}
