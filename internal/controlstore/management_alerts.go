package controlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ManagementAlertSpec struct {
	Key          string
	Type         string
	ResourceKind string
	ResourceID   string
	Title        string
	Detail       string
	Severity     string
}

type ManagementAlert struct {
	ID             string     `json:"id"`
	SignalKey      string     `json:"signal_key"`
	Type           string     `json:"type"`
	ResourceKind   string     `json:"resource_kind,omitempty"`
	ResourceID     string     `json:"resource_id,omitempty"`
	Title          string     `json:"title"`
	Detail         string     `json:"detail,omitempty"`
	Severity       string     `json:"severity"`
	State          string     `json:"state"`
	Unread         bool       `json:"unread"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	RecoveredAt    *time.Time `json:"recovered_at,omitempty"`
}

func (s *Store) ObserveManagementAlert(spec ManagementAlertSpec, active bool, threshold time.Duration, now time.Time) error {
	if spec.Key == "" || spec.Type == "" || spec.Title == "" {
		return fmt.Errorf("alert key, type, and title are required")
	}
	if spec.Severity == "" {
		spec.Severity = "warning"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !active {
		if _, err := tx.Exec(`DELETE FROM management_alert_signals WHERE signal_key=?`, spec.Key); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE management_alerts SET state='RECOVERED',unread=1,updated_at=?,recovered_at=?
			WHERE signal_key=? AND state IN ('ACTIVE','ACKNOWLEDGED')`, now.UnixMilli(), now.UnixMilli(), spec.Key)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.Exec(`INSERT INTO management_alert_signals(signal_key,alert_type,resource_kind,resource_id,title,detail,severity,first_observed_at,last_observed_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(signal_key) DO UPDATE SET alert_type=excluded.alert_type,
		resource_kind=excluded.resource_kind,resource_id=excluded.resource_id,title=excluded.title,
		detail=excluded.detail,severity=excluded.severity,last_observed_at=excluded.last_observed_at`,
		spec.Key, spec.Type, spec.ResourceKind, spec.ResourceID, spec.Title, spec.Detail, spec.Severity, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	var first int64
	if err := tx.QueryRow(`SELECT first_observed_at FROM management_alert_signals WHERE signal_key=?`, spec.Key).Scan(&first); err != nil {
		return err
	}
	if now.Sub(time.UnixMilli(first)) < threshold {
		return tx.Commit()
	}
	var exists int
	err = tx.QueryRow(`SELECT 1 FROM management_alerts WHERE signal_key=? AND state IN ('ACTIVE','ACKNOWLEDGED')`, spec.Key).Scan(&exists)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	id, err := randomID("alert_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO management_alerts(id,signal_key,alert_type,resource_kind,resource_id,title,detail,severity,state,unread,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?, 'ACTIVE',1,?,?)`, id, spec.Key, spec.Type, spec.ResourceKind, spec.ResourceID,
		spec.Title, spec.Detail, spec.Severity, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ManagementAlerts(limit int) ([]ManagementAlert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,signal_key,alert_type,resource_kind,resource_id,title,detail,severity,state,
		unread,created_at,updated_at,acknowledged_at,recovered_at FROM management_alerts
		ORDER BY updated_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagementAlert{}
	for rows.Next() {
		var item ManagementAlert
		var unread int
		var created, updated int64
		var ack, recovered sql.NullInt64
		if err := rows.Scan(&item.ID, &item.SignalKey, &item.Type, &item.ResourceKind, &item.ResourceID,
			&item.Title, &item.Detail, &item.Severity, &item.State, &unread, &created, &updated, &ack, &recovered); err != nil {
			return nil, err
		}
		item.Unread = unread != 0
		item.CreatedAt, item.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		item.AcknowledgedAt, item.RecoveredAt = nullableMillis(ack), nullableMillis(recovered)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) AcknowledgeManagementAlert(id string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE management_alerts SET state=CASE WHEN state='ACTIVE' THEN 'ACKNOWLEDGED' ELSE state END,
		unread=0,updated_at=?,acknowledged_at=CASE WHEN state='ACTIVE' THEN ? ELSE acknowledged_at END WHERE id=?`,
		now.UnixMilli(), now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("alert %s not found", id)
	}
	return nil
}

func (s *Store) CleanupManagementAlerts(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM management_alerts WHERE updated_at<?`, before.UnixMilli())
	return err
}

func (s *Store) ManagementAlertSignalKeys(prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT signal_key FROM management_alert_signals WHERE signal_key LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}
