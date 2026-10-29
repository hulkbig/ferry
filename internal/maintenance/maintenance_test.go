package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ferry/internal/controlstore"
	"ferry/internal/secretstore"
)

func writeBootstrap(t *testing.T, path string) {
	t.Helper()
	const body = `server:
  data_dir: ./ignored
  management_addr: 0.0.0.0:7900
  proxy_addr: 0.0.0.0:7890
  timezone: Asia/Shanghai
  log_retention_days: 30
consumer_keys:
  - name: smoke
    key: sk-ferry-test
    enabled: true
    default_alias: smart
    default_objective: balanced
plans:
  - id: upstream
    kind: payg
    currency: USD
channels:
  - id: upstream
    type: custom
    base_url: https://example.invalid/v1
    credential: {secret_ref: store:upstream}
    plan_id: upstream
    pools: [primary]
    models:
      - model: model
        alias: smart
        protocols: [openai-chat]
        capabilities: {text: true, context_window: 32768}
pools:
  - id: primary
aliases:
  smart:
    required_capabilities: {text: true}
    allow_degradation: true
    default_objective: balanced
objectives:
  balanced:
    rank: [capability_fit, max_reliability]
policies:
  main:
    timezone: Asia/Shanghai
    tiers:
      - {name: primary, pool: primary, objective: balanced}
default_policy: main
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateInitializesSQLiteDatabasesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)

	first, err := Migrate(dataDir, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if first.ControlVersion != 1 || !first.Imported {
		t.Fatalf("first migration = %#v, want imported control version 1", first)
	}
	second, err := Migrate(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ControlVersion != 1 || second.Imported {
		t.Fatalf("second migration = %#v, want idempotent version 1", second)
	}
	for _, name := range databaseNames {
		info, err := os.Stat(filepath.Join(dataDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestQuickstartStartsWithoutSharedCredentialsOrUpstreams(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	result, err := Migrate(dataDir, filepath.Join("..", "..", "config.example.yaml"))
	if err != nil || !result.Imported {
		t.Fatalf("minimal first-run config: %+v, %v", result, err)
	}
	store, err := controlstore.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	active, _, err := store.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if active.Server.ManagementAddr != "127.0.0.1:7900" || active.Server.ProxyAddr != "127.0.0.1:7890" {
		t.Fatal("quickstart must only listen on loopback")
	}
	if len(active.ConsumerKeys) != 0 || len(active.Connections) != 0 || len(active.Targets) != 0 || len(active.Channels) != 0 {
		t.Fatal("quickstart must not seed shared API keys or callable upstreams")
	}
	second, err := Migrate(dataDir, "missing-file-must-not-be-needed.yaml")
	if err != nil || second.Imported || second.ControlVersion != result.ControlVersion {
		t.Fatalf("restart must use SQLite without reimport: %+v, %v", second, err)
	}
}

func TestMigrateFailsClosedWithoutAnInitialConfig(t *testing.T) {
	_, err := Migrate(filepath.Join(t.TempDir(), "data"), "")
	if err == nil || !strings.Contains(err.Error(), "control store is empty") {
		t.Fatalf("Migrate error = %v, want empty-control failure", err)
	}
}

func TestBackupVerifyAndRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(dataDir, bootstrap); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(root, "backup")
	manifest, err := Backup(dataDir, backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != len(databaseNames) {
		t.Fatalf("backup files = %#v, want %v", manifest.Files, databaseNames)
	}
	verified, err := Verify(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if verified.ControlVersion != 1 {
		t.Fatalf("verified control version = %d, want 1", verified.ControlVersion)
	}

	if err := os.WriteFile(filepath.Join(dataDir, "control.db"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(backupDir, dataDir); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockRestore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := ReadRestoreRecords(dataDir)
	unlock()
	if err != nil || len(records) != 1 || records[0].Phase != "completed" || records[0].Displaced == "" || records[0].Source != backupDir {
		t.Fatalf("restore journal: %+v %v", records, err)
	}
	if old, err := os.ReadFile(filepath.Join(records[0].Displaced, "control.db")); err != nil || string(old) != "corrupt" {
		t.Fatalf("compensation copy changed: %v", err)
	}
	if _, err := Migrate(dataDir, ""); err != nil {
		t.Fatalf("restored databases are not bootable: %v", err)
	}
}

func TestBackupFromPreConversationReleaseCreatesEmptyPayloadOnlyInBackup(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(dataDir, bootstrap); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dataDir, "conversation.db")); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backup")
	if _, err := Backup(dataDir, backupDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "conversation.db")); !os.IsNotExist(err) {
		t.Fatalf("backup mutated pre-V2 source: %v", err)
	}
	if _, err := Verify(backupDir); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsTamperedBackup(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(dataDir, bootstrap); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backup")
	if _, err := Backup(dataDir, backupDir); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(backupDir, "control.db"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("tamper"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := Verify(backupDir); err == nil {
		t.Fatal("tampered backup must fail verification")
	}
}

func TestVerifyRejectsSymlinkedManifest(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(dataDir, bootstrap); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backup")
	if _, err := Backup(dataDir, backupDir); err != nil {
		t.Fatal(err)
	}
	realManifest := filepath.Join(backupDir, "manifest.json")
	outside := filepath.Join(root, "outside-manifest.json")
	if err := os.Rename(realManifest, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, realManifest); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(backupDir); err == nil {
		t.Fatal("symlinked backup manifest must fail verification")
	}
}

func TestProvisionSecretFromFileAndReadConsumerKey(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	writeBootstrap(t, bootstrap)
	if _, err := Migrate(dataDir, bootstrap); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(root, "upstream-key")
	if err := os.WriteFile(secretPath, []byte("  upstream-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ProvisionSecretFromFile(dataDir, "new_api", secretPath); err != nil {
		t.Fatal(err)
	}
	store, err := secretstore.OpenSQLite(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got, ok := store.Get("new_api"); !ok || got != "upstream-secret" {
		t.Fatalf("stored secret = %q, %v", got, ok)
	}
	if key, err := ConsumerKey(dataDir, "smoke"); err == nil || key != "" ||
		!strings.Contains(err.Error(), "hash-only") {
		t.Fatalf("consumer key retrieval = (%q,%v), want hash-only rejection", key, err)
	}
}
