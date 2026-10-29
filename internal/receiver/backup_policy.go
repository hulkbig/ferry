package receiver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ferry/internal/backuppolicy"
)

type scheduledReceipt struct {
	DestinationID string    `json:"destination_id,omitempty"`
	CompletedAt   time.Time `json:"completed_at"`
	OperationID   string    `json:"operation_id"`
	Remote        string    `json:"remote"`
	Objects       []string  `json:"objects,omitempty"`
}

// Read through a read-only connection: the root receiver must never migrate or
// write the application's control database or its policy.
func (e *Environment) backupPolicy() (backuppolicy.Policy, error) {
	p := backuppolicy.Default()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.ServiceDir, "data", "control.db")+"?mode=ro")
	if err != nil {
		return p, err
	}
	defer db.Close()
	var raw string
	err = db.QueryRow("SELECT value FROM runtime_settings WHERE key = ?", backuppolicy.SettingKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	p.IncludeHistory = false
	return p, backuppolicy.Validate(p)
}

// Local retention runs under the receiver lock and has its own restore lock.
func (e *Environment) cleanupBackupCopies(ctx context.Context, keep int) error {
	o := DefaultCleanupOptions(keep)
	o.Apply = true
	_, err := e.cleanupLocked(ctx, o)
	return err
}

// CleanupDefaults reads only the public backup policy; no S3 credentials.
func (e *Environment) CleanupDefaults() (CleanupOptions, error) {
	p, err := e.backupPolicy()
	return DefaultCleanupOptions(p.LocalCopies), err
}

func removeCopy(root, name string) error {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid backup name")
	}
	p := filepath.Join(root, name)
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup is not a plain directory")
	}
	return os.RemoveAll(p)
}

// Retention only removes exact payload keys recorded after verified uploads by
// this receiver. It never lists or recursively deletes a user-supplied prefix.
func (e *Environment) pruneRemoteCopies(ctx context.Context, receipt *scheduledReceipt, keep int) error {
	if keep < 1 {
		return fmt.Errorf("invalid remote retention")
	}
	for len(receipt.Objects) > keep {
		remote := receipt.Objects[0]
		root := minioAlias + "/" + minioBucket + "/"
		if !strings.HasPrefix(remote, root) {
			return fmt.Errorf("invalid managed backup destination")
		}
		relative := strings.TrimPrefix(remote, root)
		parts := strings.Split(relative, "/")
		if len(parts) < 3 {
			return fmt.Errorf("invalid managed backup path")
		}
		p := backuppolicy.Default()
		p.S3Prefix = strings.Join(parts[:len(parts)-1], "/")
		if err := backuppolicy.Validate(p); err != nil {
			return err
		}
		if _, err := time.Parse("scheduled-20060102T150405Z", parts[len(parts)-1]); err != nil {
			return err
		}
		for _, name := range backupPayloadNames {
			if _, err := e.run(ctx, nil, mcPath, "--config-dir", filepath.Join(e.ServiceDir, "secrets", "mc"), "rm", "--force", remote+"/"+name); err != nil {
				return fmt.Errorf("remove expired S3 backup: %w", err)
			}
		}
		receipt.Objects = receipt.Objects[1:]
	}
	return nil
}

// Compare calendar days, not elapsed hours, so the timer's randomized delay
// cannot turn a daily policy into an every-other-day backup.
func nextScheduledBackup(completed time.Time, hours int) time.Time {
	local := completed.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	return time.Date(local.Year(), local.Month(), local.Day()+hours/24, 0, 0, 0, 0, local.Location())
}
