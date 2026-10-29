package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func startupCheckFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bootstrap.yaml")
	if err := os.WriteFile(path, []byte("server:\n  proxy_addr: 127.0.0.1:17890\n  management_addr: 127.0.0.1:17900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartupCheckDoesNotCreateOrMigrateStore(t *testing.T) {
	bootstrap := startupCheckFixture(t)
	data := filepath.Join(t.TempDir(), "absent")
	got, err := readStartupConfig(data, bootstrap)
	if err != nil || got.ProxyAddr != "127.0.0.1:17890" || got.ManagementAddr != "127.0.0.1:17900" {
		t.Fatalf("bootstrap read=%+v err=%v", got, err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("read-only check created data directory: %v", err)
	}
	for _, empty := range []bool{true, false} {
		data = t.TempDir()
		path := filepath.Join(data, "control.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if !empty {
			_, err = db.Exec(`CREATE TABLE control_config_versions (version INTEGER PRIMARY KEY, config_json BLOB NOT NULL, active INTEGER NOT NULL)`)
		} else {
			_, err = db.Exec(`CREATE TABLE legacy_marker (id INTEGER PRIMARY KEY)`)
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		before, _ := os.ReadFile(path)
		if _, err := readStartupConfig(data, bootstrap); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("preflight changed a schema-less or empty control store")
		}
		files, _ := os.ReadDir(data)
		if len(files) != 1 {
			t.Fatalf("preflight created store files: %+v", files)
		}
	}
}

func TestStartupCheckReadsLegacyEnvelopeAndCommittedWAL(t *testing.T) {
	data := t.TempDir()
	path := filepath.Join(data, "control.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Deliberately omit all normalized policy/API-key tables. ActiveConfig
	// cannot read this old schema until the serving process opens the store.
	if _, err := db.Exec(`CREATE TABLE control_config_versions (version INTEGER PRIMARY KEY, config_json BLOB NOT NULL, active INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	raw := `{"Server":{"ProxyAddr":"127.0.0.1:27890","ManagementAddr":"127.0.0.1:27900"},"unrelated_secret":"must-not-be-output"}`
	if _, err := db.Exec(`INSERT INTO control_config_versions VALUES(7, ?, 1)`, raw); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	got, err := readStartupConfig(data, "missing-bootstrap-is-ignored.yaml")
	if err != nil || got.ProxyAddr != "127.0.0.1:27890" {
		t.Fatalf("legacy envelope=%+v err=%v", got, err)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "unrelated_secret") || strings.Contains(string(encoded), "must-not-be-output") {
		t.Fatal("preflight exposed configuration beyond listener addresses")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("legacy preflight modified the database")
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	raw = strings.ReplaceAll(raw, "27890", "37890")
	if _, err := db.Exec(`UPDATE control_config_versions SET config_json=? WHERE active=1`, raw); err != nil {
		t.Fatal(err)
	}
	got, err = readStartupConfig(data, startupCheckFixture(t))
	if err != nil || got.ProxyAddr != "127.0.0.1:37890" {
		t.Fatalf("committed WAL ignored: %+v err=%v", got, err)
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("preflight ran schema migration: tables=%d err=%v", tables, err)
	}
}

func TestStartupCheckRejectsCorruptionWithoutBootstrapFallback(t *testing.T) {
	data := t.TempDir()
	path := filepath.Join(data, "control.db")
	raw := []byte("not a SQLite database")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readStartupConfig(data, startupCheckFixture(t)); err == nil {
		t.Fatal("corrupt store fell back to bootstrap")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(raw, after) {
		t.Fatal("preflight changed corrupt store")
	}
}
