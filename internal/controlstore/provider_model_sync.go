package controlstore

import (
	"database/sql"
	"fmt"
	"time"
)

// ProviderModelSyncState is the durable, non-sensitive result of the most
// recent model-catalog synchronization attempt for one Provider.
type ProviderModelSyncState struct {
	ProviderID           string
	ConfigFingerprint    string
	Status               string
	LastStartedAt        *time.Time
	LastCompletedAt      *time.Time
	LastSuccessAt        *time.Time
	NextRunAt            *time.Time
	LastErrorCode        string
	LastError            string
	DiscoveredModels     int
	AddedModels          int
	UpdatedModels        int
	CreatedLogicalModels int
	Revision             int64
}

func timeMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}

func scanOptionalTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	parsed := time.UnixMilli(value.Int64).UTC()
	return &parsed
}

// SaveProviderModelSyncState atomically replaces one Provider's latest sync
// state. It contains only aggregate counts and bounded safe error text.
func (s *Store) SaveProviderModelSyncState(state ProviderModelSyncState) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("control store is not configured")
	}
	if state.ProviderID == "" {
		return fmt.Errorf("provider id is required")
	}
	_, err := s.db.Exec(`INSERT INTO provider_model_sync_state(
		provider_id,config_fingerprint,status,last_started_at,last_completed_at,last_success_at,next_run_at,
		last_error_code,last_error,discovered_models,added_models,updated_models,created_logical_models,revision
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(provider_id) DO UPDATE SET
		config_fingerprint=excluded.config_fingerprint,
		status=excluded.status,
		last_started_at=excluded.last_started_at,
		last_completed_at=excluded.last_completed_at,
		last_success_at=excluded.last_success_at,
		next_run_at=excluded.next_run_at,
		last_error_code=excluded.last_error_code,
		last_error=excluded.last_error,
		discovered_models=excluded.discovered_models,
		added_models=excluded.added_models,
		updated_models=excluded.updated_models,
		created_logical_models=excluded.created_logical_models,
		revision=excluded.revision`,
		state.ProviderID, state.ConfigFingerprint, state.Status,
		timeMillis(state.LastStartedAt), timeMillis(state.LastCompletedAt), timeMillis(state.LastSuccessAt), timeMillis(state.NextRunAt),
		state.LastErrorCode, state.LastError, state.DiscoveredModels, state.AddedModels, state.UpdatedModels, state.CreatedLogicalModels, state.Revision)
	if err != nil {
		return fmt.Errorf("save provider model sync state: %w", err)
	}
	return nil
}

// ProviderModelSyncStates returns the last state for every Provider keyed by
// Provider ID. Callers reconcile the stored fingerprint with live config.
func (s *Store) ProviderModelSyncStates() (map[string]ProviderModelSyncState, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is not configured")
	}
	rows, err := s.db.Query(`SELECT provider_id,config_fingerprint,status,
		last_started_at,last_completed_at,last_success_at,next_run_at,
		last_error_code,last_error,discovered_models,added_models,updated_models,created_logical_models,revision
		FROM provider_model_sync_state`)
	if err != nil {
		return nil, fmt.Errorf("query provider model sync states: %w", err)
	}
	defer rows.Close()
	states := map[string]ProviderModelSyncState{}
	for rows.Next() {
		var state ProviderModelSyncState
		var started, completed, success, next sql.NullInt64
		if err := rows.Scan(&state.ProviderID, &state.ConfigFingerprint, &state.Status,
			&started, &completed, &success, &next,
			&state.LastErrorCode, &state.LastError, &state.DiscoveredModels, &state.AddedModels,
			&state.UpdatedModels, &state.CreatedLogicalModels, &state.Revision); err != nil {
			return nil, fmt.Errorf("scan provider model sync state: %w", err)
		}
		state.LastStartedAt = scanOptionalTime(started)
		state.LastCompletedAt = scanOptionalTime(completed)
		state.LastSuccessAt = scanOptionalTime(success)
		state.NextRunAt = scanOptionalTime(next)
		states[state.ProviderID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider model sync states: %w", err)
	}
	return states, nil
}

func (s *Store) ProviderModelSyncState(providerID string) (*ProviderModelSyncState, error) {
	states, err := s.ProviderModelSyncStates()
	if err != nil {
		return nil, err
	}
	state, ok := states[providerID]
	if !ok {
		return nil, nil
	}
	return &state, nil
}
