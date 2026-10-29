package maintenance

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationBackupExcludesHistoryAndRestores(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(data, bootstrap); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(data, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO logical_requests(id,ts) VALUES('private-history',1)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backup")
	manifest, err := BackupConfiguration(data, backup)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Scope != "configuration" {
		t.Fatal("scope not declared")
	}
	restored := filepath.Join(root, "restored")
	if err = Restore(backup, restored); err != nil {
		t.Fatal(err)
	}
	if _, err = Migrate(restored, ""); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		dir   string
		count int
	}{{data, 1}, {restored, 0}} {
		db, err := sql.Open("sqlite", filepath.Join(test.dir, "ledger.db"))
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = db.QueryRow("SELECT COUNT(*) FROM logical_requests").Scan(&n)
		db.Close()
		if err != nil || n != test.count {
			t.Fatalf("history %s: %d %v", test.dir, n, err)
		}
	}
}

func TestConfigurationExportSkipsOldRowsAndNeverReadsHistory(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(data, bootstrap); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(data, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately large, invalid old configurations must never be read/decoded or
	// copied. Keep the active configuration and its routing-policy references.
	for i := 0; i < 64; i++ {
		if _, err = db.Exec("INSERT INTO control_config_versions(created_at,config_json,active) VALUES(1,zeroblob(131072),0)"); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO local_secrets(name,value) VALUES('oauth:test','test-credential')`,
		`INSERT INTO management_administrator(singleton,username,password_hash,created_at,updated_at) VALUES(1,'admin','test-password-hash',1,1)`,
		`INSERT INTO management_tokens(id,name,secret_hash,preview,created_at,updated_at) VALUES('management','test-token',x'123456','test',1,1)`,
		`INSERT INTO management_sessions(id,username,token_hash,csrf_hash,created_at,last_seen_at,expires_at) VALUES('session','admin',x'123456',x'123456',1,1,2)`,
		`INSERT INTO runtime_settings(key,value,updated_at) VALUES('scheduled_model_sync_enabled','false',1)`,
		`INSERT INTO configuration_drafts(id,base_revision,intent_json,created_at,updated_at) VALUES('draft',1,zeroblob(131072),1,1)`,
	} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(data, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Keep a credential update in WAL: the exporter must see it without a source
	// checkpoint or any change to the main database file.
	db, err = sql.Open("sqlite", filepath.Join(data, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; UPDATE local_secrets SET value='test-wal-credential' WHERE name='oauth:test'"); err != nil {
		t.Fatal(err)
	}
	// A configuration backup must work without opening either source history DB.
	for _, name := range []string{"ledger.db", "conversation.db"} {
		path := filepath.Join(data, name)
		if err = os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte("not-a-database-do-not-read"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	backup := filepath.Join(root, "backup")
	manifest, err := BackupConfiguration(data, backup)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ControlVersion != 1 {
		t.Fatalf("active version changed: %d", manifest.ControlVersion)
	}
	for _, file := range manifest.Files {
		if file.Name == "control.db" && file.Size > 1024*1024 {
			t.Fatalf("configuration export copied history: %d", file.Size)
		}
	}
	after, err := os.ReadFile(filepath.Join(data, "control.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("live control store changed")
	}
	restored := filepath.Join(root, "restored")
	if err = Restore(backup, restored); err != nil {
		t.Fatal(err)
	}
	if _, err = Migrate(restored, ""); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", filepath.Join(restored, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, want := range map[string]int{"control_config_versions": 1, "configuration_drafts": 0, "management_sessions": 0, "management_tokens": 1, "local_secrets": 1, "management_administrator": 1} {
		var got int
		// #nosec G202 -- table names come only from the fixed fixture map above.
		if err = db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("table %s count=%d want=%d err=%v", table, got, want, err)
		}
	}
	var secret, password string
	if err = db.QueryRow("SELECT value FROM local_secrets WHERE name='oauth:test'").Scan(&secret); err != nil || secret != "test-wal-credential" {
		t.Fatal("credential not retained")
	}
	if err = db.QueryRow("SELECT password_hash FROM management_administrator").Scan(&password); err != nil || password != "test-password-hash" {
		t.Fatal("administrator not retained")
	}
	var sequence int
	if err = db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='control_config_versions'").Scan(&sequence); err != nil || sequence != 65 {
		t.Fatalf("revision sequence changed: %d %v", sequence, err)
	}
}

func TestConfigurationScopeRejectsPopulatedHistory(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(data, bootstrap); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(data, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO logical_requests(id,ts) VALUES('request',1)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	backup := filepath.Join(root, "backup")
	manifest, err := Backup(data, backup)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Scope = "configuration"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(backup, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(backup); err == nil {
		t.Fatal("accepted history as configuration backup")
	}
}
