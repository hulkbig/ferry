package controlstore

import (
	"database/sql"
	"ferry/internal/backuppolicy"
	"path/filepath"
	"testing"
)

func TestFerryBackupDestinationMigration(t *testing.T) {
	for _, tc := range []struct {
		name, stored, want string
		old                bool
	}{
		{"fresh", "", "ferry/scheduled", false},
		{"implicit legacy", "", "smart-api/scheduled", true},
		{"explicit legacy", `{"s3_prefix":"smart-api/custom"}`, "smart-api/custom", true},
		{"partial legacy", `{"remote_copies":7}`, "smart-api/scheduled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "control.db")
			if tc.old {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`CREATE TABLE control_config_versions(version INTEGER PRIMARY KEY AUTOINCREMENT,created_at INTEGER NOT NULL,config_json BLOB NOT NULL,active INTEGER NOT NULL);
    INSERT INTO control_config_versions VALUES(1,1,'{}',1);
    CREATE TABLE runtime_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at INTEGER NOT NULL);`)
				if err != nil {
					t.Fatal(err)
				}
				if tc.stored != "" {
					if _, err = db.Exec(`INSERT INTO runtime_settings VALUES('backup_policy',?,1)`, tc.stored); err != nil {
						t.Fatal(err)
					}
				}
				db.Close()
			}
			for range 2 {
				store, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				policy, err := backuppolicy.Load(store)
				if err != nil {
					t.Fatal(err)
				}
				if policy.S3Prefix != tc.want {
					t.Fatalf("destination changed: %s, want %s", policy.S3Prefix, tc.want)
				}
				if tc.stored != "" {
					raw, _, _ := store.RuntimeSetting(backuppolicy.SettingKey)
					if raw != tc.stored {
						t.Fatal("operator policy overwritten")
					}
				}
				store.Close()
			}
		})
	}
}
