// Package maintenance provides the fail-closed, non-serving operations used by
// the release receiver: schema/bootstrap migration plus verified online backup
// and restore of Ferry's SQLite databases.
package maintenance

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	_ "modernc.org/sqlite"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/conversation"
	"ferry/internal/ledger"
	"ferry/internal/policy"
	"ferry/internal/secretstore"
)

const manifestSchema = "smart-api/sqlite-backup/v1"

var databaseNames = []string{"control.db", "conversation.db", "ledger.db"}
var provisionedSecretName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

type MigrationResult struct {
	ControlVersion int64 `json:"control_version"`
	Imported       bool  `json:"imported"`
}

type BackupFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Schema         string       `json:"schema"`
	Scope          string       `json:"scope,omitempty"`
	CreatedAt      string       `json:"created_at"`
	ControlVersion int64        `json:"control_version"`
	Files          []BackupFile `json:"files"`
}

type VerificationResult struct {
	ControlVersion int64 `json:"control_version"`
}

// Migrate creates all SQLite schemas and imports bootstrapPath only when the
// control store has never had a configuration. Subsequent calls are strictly
// idempotent and never make YAML a runtime dependency.
func Migrate(dataDir, bootstrapPath string) (MigrationResult, error) {
	if err := secureDirectory(dataDir); err != nil {
		return MigrationResult{}, err
	}
	controlPath := filepath.Join(dataDir, "control.db")
	store, err := controlstore.Open(controlPath)
	if err != nil {
		return MigrationResult{}, err
	}
	defer store.Close()

	active, version, err := store.ActiveConfig()
	imported := false
	if errors.Is(err, controlstore.ErrNoConfig) {
		if bootstrapPath == "" {
			return MigrationResult{}, fmt.Errorf("control store is empty: an explicit bootstrap config is required")
		}
		active, err = config.Load(bootstrapPath)
		if err != nil {
			return MigrationResult{}, fmt.Errorf("load bootstrap config: %w", err)
		}
		active.Server.DataDir = dataDir
		if _, err := policy.NewCompiler().Compile(active); err != nil {
			return MigrationResult{}, fmt.Errorf("compile bootstrap config: %w", err)
		}
		version, err = store.SaveConfig(active)
		if err != nil {
			return MigrationResult{}, fmt.Errorf("save bootstrap config: %w", err)
		}
		imported = true
	} else if err != nil {
		return MigrationResult{}, fmt.Errorf("read active config: %w", err)
	} else if _, err := policy.NewCompiler().Compile(active); err != nil {
		return MigrationResult{}, fmt.Errorf("compile active config: %w", err)
	}

	secrets, err := secretstore.OpenSQLite(controlPath)
	if err != nil {
		return MigrationResult{}, err
	}
	if err := secrets.Close(); err != nil {
		return MigrationResult{}, fmt.Errorf("close secret store: %w", err)
	}
	log, err := ledger.Open(dataDir, 8, 0)
	if err != nil {
		return MigrationResult{}, err
	}
	if err := log.Close(); err != nil {
		return MigrationResult{}, fmt.Errorf("close ledger: %w", err)
	}
	conversations, err := conversation.Open(dataDir)
	if err != nil {
		return MigrationResult{}, err
	}
	if err := conversations.Close(); err != nil {
		return MigrationResult{}, fmt.Errorf("close conversation store: %w", err)
	}
	for _, name := range databaseNames {
		if err := os.Chmod(filepath.Join(dataDir, name), 0o600); err != nil {
			return MigrationResult{}, fmt.Errorf("secure %s: %w", name, err)
		}
	}
	return MigrationResult{ControlVersion: version, Imported: imported}, nil
}

// Backup creates transactionally consistent SQLite copies using VACUUM INTO,
// hashes them, and writes a self-describing manifest. The destination must not
// already exist, preventing accidental overwrite of recovery evidence.
func Backup(dataDir, backupDir string) (Manifest, error) {
	return backup(dataDir, backupDir, true)
}

// BackupConfiguration excludes request bodies and conversation history. Empty
// history schemas preserve the verified restore contract without copying payloads.
// The control store includes only current configuration and recovery credentials.
func BackupConfiguration(dataDir, backupDir string) (Manifest, error) {
	return backup(dataDir, backupDir, false)
}

func backup(dataDir, backupDir string, includeHistory bool) (Manifest, error) {
	if err := os.MkdirAll(filepath.Dir(backupDir), 0o700); err != nil {
		return Manifest{}, fmt.Errorf("create backup parent: %w", err)
	}
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("create backup directory: %w", err)
	}
	manifest := Manifest{Schema: manifestSchema, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if !includeHistory {
		manifest.Scope = "configuration"
	}
	for _, name := range databaseNames {
		source := filepath.Join(dataDir, name)
		destination := filepath.Join(backupDir, name)
		if !includeHistory && name == "ledger.db" {
			empty, err := ledger.Open(backupDir, 8, 0)
			if err != nil {
				return Manifest{}, err
			}
			if err = empty.Close(); err != nil {
				return Manifest{}, err
			}
		} else if !includeHistory && name == "conversation.db" {
			empty, err := conversation.Open(backupDir)
			if err != nil {
				return Manifest{}, err
			}
			if err = empty.Close(); err != nil {
				return Manifest{}, err
			}
		} else if !includeHistory && name == "control.db" {
			if err := exportConfiguration(source, destination); err != nil {
				return Manifest{}, fmt.Errorf("export configuration: %w", err)
			}
		} else if err := requireRegularFile(source); err != nil {
			// Pre-V2 installations have no conversation database. Materialize an
			// empty schema directly in the backup so the pre-migration safety copy
			// remains valid without mutating the live old-version data directory.
			if name != "conversation.db" || !errors.Is(err, os.ErrNotExist) {
				return Manifest{}, err
			}
			store, createErr := conversation.Open(backupDir)
			if createErr != nil {
				return Manifest{}, fmt.Errorf("create empty conversation backup: %w", createErr)
			}
			if closeErr := store.Close(); closeErr != nil {
				return Manifest{}, fmt.Errorf("close empty conversation backup: %w", closeErr)
			}
		} else if err := vacuumInto(source, destination); err != nil {
			return Manifest{}, fmt.Errorf("backup %s: %w", name, err)
		}
		if err := os.Chmod(destination, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("secure backup %s: %w", name, err)
		}
		file, err := describeFile(destination, name)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Files = append(manifest.Files, file)
	}
	version, err := controlVersion(filepath.Join(backupDir, "control.db"))
	if err != nil {
		return Manifest{}, err
	}
	manifest.ControlVersion = version
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), raw, 0o600); err != nil {
		return Manifest{}, fmt.Errorf("write backup manifest: %w", err)
	}
	if _, err := Verify(backupDir); err != nil {
		return Manifest{}, fmt.Errorf("verify new backup: %w", err)
	}
	return manifest, nil
}

// Verify rejects missing, extra, symlinked or modified database payloads and
// then runs SQLite integrity and schema checks on every copy.
func Verify(backupDir string) (VerificationResult, error) {
	manifestPath := filepath.Join(backupDir, "manifest.json")
	if err := requireRegularFile(manifestPath); err != nil {
		return VerificationResult{}, err
	}
	raw, err := os.ReadFile(manifestPath) // #nosec G304 -- backupDir is operator/receiver supplied and the manifest is required to be a regular non-symlink file first.
	if err != nil {
		return VerificationResult{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return VerificationResult{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if manifest.Schema != manifestSchema || len(manifest.Files) != len(databaseNames) || (manifest.Scope != "" && manifest.Scope != "configuration") {
		return VerificationResult{}, fmt.Errorf("unsupported or incomplete backup manifest")
	}
	wantNames := append([]string(nil), databaseNames...)
	sort.Strings(wantNames)
	gotNames := make([]string, 0, len(manifest.Files))
	for _, expected := range manifest.Files {
		if filepath.Base(expected.Name) != expected.Name {
			return VerificationResult{}, fmt.Errorf("unsafe backup file name %q", expected.Name)
		}
		path := filepath.Join(backupDir, expected.Name)
		if err := requireRegularFile(path); err != nil {
			return VerificationResult{}, err
		}
		actual, err := describeFile(path, expected.Name)
		if err != nil {
			return VerificationResult{}, err
		}
		if actual.Size != expected.Size || actual.SHA256 != expected.SHA256 {
			return VerificationResult{}, fmt.Errorf("backup checksum mismatch for %s", expected.Name)
		}
		gotNames = append(gotNames, expected.Name)
	}
	sort.Strings(gotNames)
	if strings.Join(gotNames, "\x00") != strings.Join(wantNames, "\x00") {
		return VerificationResult{}, fmt.Errorf("backup manifest has unexpected database set")
	}
	if err := verifySQLite(filepath.Join(backupDir, "control.db"), []string{"control_config_versions", "local_secrets"}); err != nil {
		return VerificationResult{}, err
	}
	if err := verifySQLite(filepath.Join(backupDir, "ledger.db"), []string{"logical_requests", "attempts", "decision_traces"}); err != nil {
		return VerificationResult{}, err
	}
	if err := verifySQLite(filepath.Join(backupDir, "conversation.db"), []string{
		"conversations", "native_handles", "smart_responses", "conversation_evidence",
	}); err != nil {
		return VerificationResult{}, err
	}
	if manifest.Scope == "configuration" {
		for _, name := range []string{"ledger.db", "conversation.db"} {
			if err := verifyEmptyHistory(filepath.Join(backupDir, name)); err != nil {
				return VerificationResult{}, err
			}
		}
	}
	version, err := controlVersion(filepath.Join(backupDir, "control.db"))
	if err != nil {
		return VerificationResult{}, err
	}
	if version != manifest.ControlVersion {
		return VerificationResult{}, fmt.Errorf("control version does not match manifest")
	}
	return VerificationResult{ControlVersion: version}, nil
}

// Restore verifies before mutating live state, stages every database in one
// sibling directory, then swaps the whole data directory. Callers restoring a
// serving instance must stop it first; the receiver enforces that boundary.
// The displaced directory is retained as local compensation evidence.
func Restore(backupDir, dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("data directory must be non-empty")
	}
	var err error
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	backupDir, err = filepath.Abs(backupDir)
	if err != nil {
		return err
	}
	unlock, err := LockRestore(dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	records, err := ReadRestoreRecords(dataDir)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Phase == "prepared" {
			return fmt.Errorf("previous restore is unresolved: %s", r.ID)
		}
	}
	if _, err := Verify(backupDir); err != nil {
		return err
	}
	parent := filepath.Dir(dataDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	hadExisting := false
	if info, err := os.Lstat(dataDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("data directory must be a real directory")
		}
		hadExisting = true
	} else if !os.IsNotExist(err) {
		return err
	}
	stageDir, err := os.MkdirTemp(parent, "."+filepath.Base(dataDir)+".restore-")
	if err != nil {
		return err
	}
	stageActive := true
	defer func() {
		if stageActive {
			_ = os.RemoveAll(stageDir)
		}
	}()
	if err := os.Chmod(stageDir, 0o700); err != nil { // #nosec G302 -- stageDir is a directory created by MkdirTemp; 0700 is the intended directory mode.
		return err
	}
	for _, name := range databaseNames {
		source := filepath.Join(backupDir, name)
		destination := filepath.Join(stageDir, name)
		if err := copyExclusive(source, destination); err != nil {
			return fmt.Errorf("stage restore %s: %w", name, err)
		}
		if err := os.Chmod(destination, 0o600); err != nil {
			return fmt.Errorf("secure restored %s: %w", name, err)
		}
	}
	staged, err := os.Open(stageDir) // #nosec G304 -- stageDir is the restore staging directory created by os.MkdirTemp above.
	if err == nil {
		err = staged.Sync()
		_ = staged.Close()
	}
	if err != nil {
		return err
	}
	displaced := dataDir + ".pre-restore-" + fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	record := RestoreRecord{Schema: "ferry/restore/v1", ID: uuid.NewString(), Data: dataDir, Source: backupDir, Stage: stageDir, Phase: "prepared"}
	if hadExisting {
		record.Displaced = displaced
	}
	if err := saveRestoreRecord(record); err != nil {
		return fmt.Errorf("record restore intent: %w", err)
	}
	if hadExisting {
		if err := os.Rename(dataDir, displaced); err != nil {
			record.Phase, record.CompletedAt = "compensated", time.Now().UTC()
			if journalErr := saveRestoreRecord(record); journalErr != nil {
				return fmt.Errorf("preserve pre-restore data: %v; record unchanged data: %w", err, journalErr)
			}
			return fmt.Errorf("preserve pre-restore data: %w", err)
		}
	}
	if err := os.Rename(stageDir, dataDir); err != nil {
		if hadExisting {
			if compensationErr := os.Rename(displaced, dataDir); compensationErr != nil {
				return fmt.Errorf("activate restored data: %v; compensation failed: %w", err, compensationErr)
			}
			// A compensated journal must not become durable before its rename.
			dir, syncErr := os.Open(parent)
			if syncErr == nil {
				syncErr = dir.Sync()
				_ = dir.Close()
			}
			if syncErr != nil {
				return fmt.Errorf("activate restored data: %v; sync compensation: %w", err, syncErr)
			}
		}
		record.Phase, record.CompletedAt = "compensated", time.Now().UTC()
		if journalErr := saveRestoreRecord(record); journalErr != nil {
			return fmt.Errorf("activate restored data: %v; record compensation: %w", err, journalErr)
		}
		return fmt.Errorf("activate restored data: %w", err)
	}
	stageActive = false
	dir, err := os.Open(parent) // #nosec G304 -- parent is the data directory parent derived from operator configuration, opened only to fsync the restore rename.
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		return err
	}
	record.Phase, record.CompletedAt = "completed", time.Now().UTC()
	return saveRestoreRecord(record)
}

// ProvisionSecretFromFile moves a locally staged upstream credential into the
// SQLite secret store without ever accepting it as a command-line argument.
func ProvisionSecretFromFile(dataDir, name, sourcePath string) error {
	if !provisionedSecretName.MatchString(name) {
		return fmt.Errorf("invalid secret name")
	}
	if err := requireRegularFile(sourcePath); err != nil {
		return err
	}
	file, err := os.Open(sourcePath) // #nosec G304 -- sourcePath is a receiver-staged bootstrap file already required to be a regular non-symlink file above.
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(raw) > 64*1024 {
		return fmt.Errorf("secret file is too large")
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return fmt.Errorf("secret file is empty")
	}
	store, err := secretstore.OpenSQLite(filepath.Join(dataDir, "control.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Set(name, value); err != nil {
		return fmt.Errorf("persist secret: %w", err)
	}
	return nil
}

// ConsumerKey is retained only as a compatibility guard for older receivers.
// API key secrets are hash-only after migration and therefore cannot be
// recovered. New smoke flows must create an ephemeral identity and use the
// one-time secret returned by that operation.
func ConsumerKey(dataDir, name string) (string, error) {
	store, err := controlstore.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		return "", err
	}
	defer store.Close()
	f, _, err := store.ActiveConfig()
	if err != nil {
		return "", err
	}
	for _, consumer := range f.ConsumerKeys {
		if consumer.Name == name && (consumer.Enabled == nil || *consumer.Enabled) {
			return "", fmt.Errorf("consumer %s secret is hash-only and cannot be retrieved; create an ephemeral API key for the smoke request", name)
		}
	}
	return "", fmt.Errorf("enabled consumer %s not found", name)
}

func secureDirectory(path string) error {
	if path == "" {
		return fmt.Errorf("data directory must be non-empty")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("data directory must not be a symlink")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	return os.Chmod(path, 0o700) // #nosec G302 -- path is the private data directory; 0700 is the intended directory mode, enforced because MkdirAll leaves existing directories untouched.
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

func vacuumInto(source, destination string) error {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", source)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	quoted := strings.ReplaceAll(destination, "'", "''")
	_, err = db.Exec("VACUUM INTO '" + quoted + "'") // #nosec G202 -- VACUUM INTO cannot bind its target; the path is an internal backup destination with every quote doubled.
	return err
}

func describeFile(path, name string) (BackupFile, error) {
	file, err := os.Open(path) // #nosec G304 -- callers pass backup files whose basenames and regular-file identity are validated by Verify.
	if err != nil {
		return BackupFile{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return BackupFile{}, copyErr
	}
	if closeErr != nil {
		return BackupFile{}, closeErr
	}
	return BackupFile{Name: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func openReadOnly(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func verifySQLite(path string, requiredTables []string) error {
	db, err := openReadOnly(path)
	if err != nil {
		return fmt.Errorf("open backup %s: %w", filepath.Base(path), err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("integrity check failed for %s: %s (%v)", filepath.Base(path), integrity, err)
	}
	for _, table := range requiredTables {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			return fmt.Errorf("required table %s is missing from %s", table, filepath.Base(path))
		}
	}
	return nil
}

func controlVersion(path string) (int64, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version int64
	var activeCount int
	if err := db.QueryRow(`SELECT COALESCE(MAX(CASE WHEN active=1 THEN version END),0), SUM(CASE WHEN active=1 THEN 1 ELSE 0 END) FROM control_config_versions`).Scan(&version, &activeCount); err != nil {
		return 0, fmt.Errorf("read control version: %w", err)
	}
	if activeCount != 1 || version <= 0 {
		return 0, fmt.Errorf("control backup must contain exactly one active version")
	}
	return version, nil
}

func copyExclusive(source, destination string) error {
	in, err := os.Open(source) // #nosec G304 -- source is a backup file already accepted by Verify; Restore only stages names from the fixed databaseNames set.
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- destination is a fresh path inside the exclusively created restore staging directory.
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
