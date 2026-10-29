package receiver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"ferry/internal/maintenance"
	"github.com/minio/minio-go/v7"
)

// ServiceDir is bound at build time for the administrator-installed receiver.
var ServiceDir = "/var/lib/ferry"

// These values are fixed in the administrator-installed binary, never supplied
// by a release request. Separate instances must not share ports or a project.
var ProjectName = "ferry"
var DataAddress = "127.0.0.1:17810"
var ManagementAddress = "127.0.0.1:17910"

const (
	dockerPath  = "/usr/bin/docker"
	mcPath      = "/usr/local/bin/mc"
	minioAlias  = "ferry-minio"
	minioBucket = "app-backups"
	appUID      = 10001
	appGID      = 10001
)

var phaseRank = map[string]int{
	"prepared": 1, "image-loaded": 2, "files-installed": 3, "deployed": 4,
	"verified": 5, "restore-drilled": 6, "candidate-committed": 7,
	"committed": 8, "rolled-back": 8, "aborted-before-deploy": 8,
}

var backupPayloadNames = []string{"control.db", "conversation.db", "ledger.db", "manifest.json"}

type operationState struct {
	Schema         string `json:"schema"`
	OperationID    string `json:"operation_id"`
	ReleaseSHA     string `json:"release_sha"`
	Phase          string `json:"phase"`
	PreviousSHA    string `json:"previous_sha,omitempty"`
	HadPrevious    bool   `json:"had_previous"`
	BackupDir      string `json:"backup_dir,omitempty"`
	PayloadDigest  string `json:"payload_digest"`
	ImageDigest    string `json:"image_digest"`
	BundleDigest   string `json:"bundle_digest,omitempty"`
	StartedAt      string `json:"started_at"`
	LastTransition string `json:"last_transition"`
}

type releaseState struct {
	Schema      string `json:"schema"`
	ReleaseSHA  string `json:"release_sha"`
	CommittedAt string `json:"committed_at"`
}

// Environment fixes all authority-bearing paths. Tests may override only the
// service root and process runner; production uses DefaultEnvironment.
type Environment struct {
	ServiceDir string
	Stdin      io.Reader
	Stdout     io.Writer
	HTTPClient *http.Client
	run        func(context.Context, io.Reader, string, ...string) ([]byte, error)
}

func DefaultEnvironment(stdin io.Reader, stdout io.Writer) *Environment {
	env := &Environment{
		ServiceDir: ServiceDir,
		Stdin:      stdin,
		Stdout:     stdout,
		HTTPClient: &http.Client{Timeout: 150 * time.Second},
	}
	env.run = env.runCommand
	return env
}

// Execute runs one already-parsed forced operation under a cross-process lock.
func (e *Environment) Execute(ctx context.Context, request Request) error {
	if e.ServiceDir == "" || e.Stdin == nil || e.Stdout == nil || e.HTTPClient == nil || e.run == nil {
		return fmt.Errorf("receiver environment is incomplete")
	}
	if err := e.ensureLayout(); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(e.ServiceDir, "receiver-state", "receiver.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open receiver lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock receiver: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	var result map[string]any
	switch request.Verb {
	case "prepare":
		result, err = e.prepare(ctx, request)
	case "commit":
		result, err = e.commitRelease(ctx, request)
	case "rollback":
		result, err = e.rollback(ctx, request)
	case "status":
		result, err = e.status(request)
	default:
		err = fmt.Errorf("unsupported receiver verb")
	}
	if err != nil {
		return err
	}
	result["deployment_contract"] = DeploymentContract
	return json.NewEncoder(e.Stdout).Encode(result)
}

// ScheduledBackup is a host-local mode for the root systemd timer. It is not
// reachable through the forced SSH protocol and follows the configured interval
// independently of application releases.
func (e *Environment) ScheduledBackup(ctx context.Context) error {
	return e.scheduledBackup(ctx, false)
}

// BackupNow uses the scheduled destination and retention, bypassing only the due
// time. Disabled backups, release/restore locks and scope restrictions still apply.
func (e *Environment) BackupNow(ctx context.Context) error {
	return e.scheduledBackup(ctx, true)
}

func (e *Environment) scheduledBackup(ctx context.Context, nowRequested bool) error {
	if err := e.ensureLayout(); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(e.ServiceDir, "receiver-state", "receiver.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	release, err := e.loadRelease()
	if err != nil {
		return fmt.Errorf("no committed release to back up: %w", err)
	}
	active, activeErr := e.loadOperation()
	if activeErr != nil && !os.IsNotExist(activeErr) {
		return activeErr
	}
	if active != nil && active.Phase != "committed" && active.Phase != "rolled-back" {
		return json.NewEncoder(e.Stdout).Encode(map[string]any{"skipped": "release_in_progress"})
	}
	policy, err := e.backupPolicy()
	if err != nil {
		return err
	}
	// Local GC is independent of upload availability, due time, and whether
	// new scheduled backups are enabled. Both operations hold receiver.lock.
	cleanupOptions := DefaultCleanupOptions(policy.LocalCopies)
	cleanupOptions.Apply = true
	cleanup, cleanupErr := e.cleanupLocked(ctx, cleanupOptions)
	cleanupWarning := ""
	if cleanupErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cleanup == nil || !cleanup.backupSafe {
			return cleanupErr
		}
		cleanupWarning = cleanupErr.Error()
		log.Printf("local copy retention failed; scheduled backup continues: %v", cleanupErr)
	} else if cleanup.Skipped != "" {
		return json.NewEncoder(e.Stdout).Encode(map[string]any{"skipped": cleanup.Skipped})
	}
	writeResult := func(result map[string]any) error {
		if cleanupWarning != "" {
			result["cleanup_error"] = cleanupWarning
		}
		return json.NewEncoder(e.Stdout).Encode(result)
	}
	if !policy.Enabled {
		return writeResult(map[string]any{"skipped": "disabled"})
	}
	receiptPath := filepath.Join(e.ServiceDir, "receiver-state", "scheduled-backup.json")
	var receipt scheduledReceipt
	if raw, readErr := os.ReadFile(receiptPath); readErr == nil {
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	// Start a separate retention journal on destination changes. Old objects
	// remain untouched; never replay their deletion records against a new server.
	if receipt.DestinationID != s3DestinationID(policy) {
		if raw, err := json.Marshal(receipt); err == nil {
			if err = os.WriteFile(receiptPath+".previous-"+time.Now().UTC().Format("20060102T150405Z"), raw, 0o600); err != nil {
				return err
			}
		} else {
			return err
		}
		receipt = scheduledReceipt{DestinationID: s3DestinationID(policy)}
	}
	var s3Client *minio.Client
	if policy.S3.Endpoint != "" {
		s3Client, err = e.scheduledS3(policy.S3)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	now := time.Now().UTC()
	nextBackup := nextScheduledBackup(receipt.CompletedAt, policy.IntervalHours)
	if !nowRequested && !receipt.CompletedAt.IsZero() && now.Before(nextBackup) {
		return writeResult(map[string]any{"skipped": "not_due", "next_backup": nextBackup})
	}
	stamp := now.Format("20060102T150405Z")
	operationID := "scheduled-" + stamp
	backupDir := filepath.Join(e.ServiceDir, "backups", "scheduled", stamp)
	var manifest maintenance.Manifest
	manifest, err = maintenance.BackupConfiguration(filepath.Join(e.ServiceDir, "data"), backupDir)
	if err != nil {
		return err
	}
	remote := minioAlias + "/" + minioBucket + "/" + policy.S3Prefix + "/" + operationID
	if s3Client != nil {
		remote = "s3://" + policy.S3.Bucket + "/" + policy.S3Prefix + "/" + operationID
		err = e.uploadCustomS3(ctx, s3Client, policy, operationID, backupDir)
	} else {
		err = e.uploadReadbackTo(ctx, operationID, "scheduled", backupDir, remote)
	}
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(backupDir, "verified.json"), []byte(`{"verified_readback":true}`), 0o600); err != nil {
		return err
	}
	receipt = scheduledReceipt{DestinationID: s3DestinationID(policy), CompletedAt: now, OperationID: operationID, Remote: remote, Objects: append(receipt.Objects, remote)}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err = os.WriteFile(receiptPath+".tmp", raw, 0o600); err != nil {
		return err
	}
	if err = os.Rename(receiptPath+".tmp", receiptPath); err != nil {
		return err
	}
	if s3Client != nil {
		err = pruneCustomS3(ctx, s3Client, policy, &receipt)
	} else {
		err = e.pruneRemoteCopies(ctx, &receipt, policy.RemoteCopies)
	}
	if err != nil {
		return err
	}
	raw, err = json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err = os.WriteFile(receiptPath+".tmp", raw, 0o600); err != nil {
		return err
	}
	if err = os.Rename(receiptPath+".tmp", receiptPath); err != nil {
		return err
	}
	return writeResult(map[string]any{
		"release_sha": release.ReleaseSHA, "control_version": manifest.ControlVersion,
		"backup": operationID, "verified_readback": true, "s3_directory": strings.Replace(remote, minioAlias+"/", "s3://", 1), "include_history": false,
	})
}

func (e *Environment) ensureLayout() error {
	for _, item := range []struct {
		name string
		mode os.FileMode
	}{
		{"receiver-state", 0o700}, {"releases", 0o755}, {"backups", 0o700},
		{"restore-drills", 0o700}, {"failed", 0o700}, {"bootstrap", 0o700},
		{"secrets", 0o700}, {"payloads", 0o700},
	} {
		path := filepath.Join(e.ServiceDir, item.name)
		if err := os.MkdirAll(path, item.mode); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Chmod(path, item.mode); err != nil {
			return err
		}
	}
	return nil
}

func (e *Environment) prepare(ctx context.Context, request Request) (map[string]any, error) {
	if state, err := e.loadOperation(); err == nil {
		if state.Phase == "committed" {
			if err := e.archiveOperation(state); err != nil {
				return nil, err
			}
		} else {
			if err := state.matches(request); err != nil {
				return nil, err
			}
			if state.PayloadDigest != request.Argument {
				return nil, fmt.Errorf("payload digest differs from active operation")
			}
			return state.result(true), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	payloadPath, err := e.storePayload(request.Argument)
	if err != nil {
		return nil, err
	}
	manifest, err := e.stagePayload(payloadPath, request)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state := &operationState{
		Schema: "ferry/receiver-operation/v1", OperationID: request.OperationID,
		ReleaseSHA: request.ReleaseSHA, Phase: "prepared", StartedAt: now, LastTransition: now,
		PayloadDigest: request.Argument, ImageDigest: manifest.digestFor("image.tar.gz"),
		BundleDigest: manifest.digestFor("release-bundle.tar.gz"),
	}
	if current, err := e.loadRelease(); err == nil {
		state.HadPrevious = true
		state.PreviousSHA = current.ReleaseSHA
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := e.saveOperation(state); err != nil {
		return nil, err
	}
	return state.result(false), nil
}

type payloadFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type payloadManifest struct {
	Schema     string        `json:"schema"`
	ReleaseSHA string        `json:"release_sha"`
	Files      []payloadFile `json:"files"`
}

func (m payloadManifest) digestFor(name string) string {
	for _, file := range m.Files {
		if file.Name == name {
			return file.SHA256
		}
	}
	return ""
}

func (e *Environment) storePayload(expectedDigest string) (string, error) {
	payloadDir := filepath.Join(e.ServiceDir, "payloads")
	finalPath := filepath.Join(payloadDir, expectedDigest+".tar")
	temporary, err := os.CreateTemp(payloadDir, ".incoming-")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		_ = temporary.Close()
		if !complete {
			_ = os.Remove(temporaryPath)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(e.Stdin, 8*1024*1024*1024+1))
	if err != nil {
		return "", fmt.Errorf("receive payload: %w", err)
	}
	if written > 8*1024*1024*1024 {
		return "", fmt.Errorf("release payload is too large")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return "", fmt.Errorf("release payload digest mismatch")
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(temporaryPath, 0o400); err != nil {
		return "", err
	}
	if _, err := os.Stat(finalPath); err == nil {
		actual, describeErr := describeSHA(finalPath)
		if describeErr != nil || actual != expectedDigest {
			return "", fmt.Errorf("existing content-addressed payload is invalid")
		}
		if err := os.Remove(temporaryPath); err != nil {
			return "", err
		}
		complete = true
		return finalPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return "", err
	}
	complete = true
	return finalPath, nil
}

func (e *Environment) stagePayload(payloadPath string, request Request) (payloadManifest, error) {
	stageDir := filepath.Join(e.ServiceDir, "receiver-state", "staging", request.OperationID)
	if err := os.MkdirAll(filepath.Dir(stageDir), 0o700); err != nil {
		return payloadManifest{}, err
	}
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return payloadManifest{}, fmt.Errorf("create operation stage: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(stageDir)
		}
	}()
	file, err := os.Open(payloadPath) // #nosec G304 -- payloadPath is the receiver-stored upload written by storePayload under ServiceDir, not client-controlled input.
	if err != nil {
		return payloadManifest{}, err
	}
	defer file.Close()
	wanted := map[string]int64{
		"manifest.json":         64 * 1024,
		"release-bundle.tar.gz": 4 * 1024 * 1024,
		"image.tar.gz":          8 * 1024 * 1024 * 1024,
	}
	seen := map[string]bool{}
	reader := tar.NewReader(file)
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return payloadManifest{}, nextErr
		}
		limit, ok := wanted[header.Name]
		if !ok || seen[header.Name] || header.Typeflag != tar.TypeReg ||
			filepath.Base(header.Name) != header.Name || header.Size < 0 || header.Size > limit {
			return payloadManifest{}, fmt.Errorf("payload contains forbidden entry %q", header.Name)
		}
		destination, pathErr := safeArchiveEntryPath(stageDir, header.Name)
		if pathErr != nil {
			return payloadManifest{}, pathErr
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400) // #nosec G304 -- destination comes from safeArchiveEntryPath, which rejects any entry escaping the staging root.
		if err != nil {
			return payloadManifest{}, err
		}
		written, copyErr := io.Copy(out, io.LimitReader(reader, header.Size+1))
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil || written != header.Size || syncErr != nil || closeErr != nil {
			return payloadManifest{}, fmt.Errorf("write payload entry %s", header.Name)
		}
		if err := os.Chmod(destination, 0o400); err != nil {
			return payloadManifest{}, err
		}
		seen[header.Name] = true
	}
	if len(seen) != len(wanted) {
		return payloadManifest{}, fmt.Errorf("release payload is incomplete")
	}
	var manifest payloadManifest
	if err := readJSON(filepath.Join(stageDir, "manifest.json"), &manifest); err != nil {
		return payloadManifest{}, fmt.Errorf("decode payload manifest: %w", err)
	}
	if manifest.Schema != "ferry/release-payload/v1" || manifest.ReleaseSHA != request.ReleaseSHA || len(manifest.Files) != 2 {
		return payloadManifest{}, fmt.Errorf("payload manifest identity is invalid")
	}
	manifestFiles := map[string]bool{}
	for _, described := range manifest.Files {
		if described.Name != "image.tar.gz" && described.Name != "release-bundle.tar.gz" {
			return payloadManifest{}, fmt.Errorf("payload manifest names an unexpected file")
		}
		if manifestFiles[described.Name] || described.Size <= 0 {
			return payloadManifest{}, fmt.Errorf("payload manifest has a duplicate or empty file")
		}
		path := filepath.Join(stageDir, described.Name)
		info, err := os.Stat(path)
		if err != nil || info.Size() != described.Size {
			return payloadManifest{}, fmt.Errorf("payload file size mismatch for %s", described.Name)
		}
		actual, err := describeSHA(path)
		if err != nil || actual != described.SHA256 {
			return payloadManifest{}, fmt.Errorf("payload file checksum mismatch for %s", described.Name)
		}
		manifestFiles[described.Name] = true
	}
	complete = true
	return manifest, nil
}

func describeSHA(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- path is a staged payload artifact whose hash is checked against the signed manifest by every caller.
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (e *Environment) loadImage(ctx context.Context, request Request) (map[string]any, error) {
	state, err := e.requirePhase(request, "prepared")
	if err != nil {
		return nil, err
	}
	if phaseRank[state.Phase] >= phaseRank["image-loaded"] {
		return state.result(true), nil
	}
	imagePath := filepath.Join(e.ServiceDir, "receiver-state", "staging", request.OperationID, "image.tar.gz")
	imageFile, err := os.Open(imagePath) // #nosec G304 -- imagePath is the staged image.tar.gz produced and checksum-verified by stagePayload.
	if err != nil {
		return nil, err
	}
	defer imageFile.Close()
	if err := verifyDockerArchive(imagePath, request.ReleaseSHA); err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(imageFile)
	if err != nil {
		return nil, fmt.Errorf("open image stream: %w", err)
	}
	if _, err := e.run(ctx, gz, dockerPath, "load"); err != nil {
		_ = gz.Close()
		return nil, fmt.Errorf("load image: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close image stream: %w", err)
	}
	if err := e.verifyImage(ctx, request.ReleaseSHA); err != nil {
		return nil, err
	}
	return e.advance(state, "image-loaded")
}

func (e *Environment) installFiles(request Request) (map[string]any, error) {
	state, err := e.requirePhase(request, "image-loaded")
	if err != nil {
		return nil, err
	}
	if phaseRank[state.Phase] >= phaseRank["files-installed"] {
		return state.result(true), nil
	}
	releaseDir := filepath.Join(e.ServiceDir, "releases", request.ReleaseSHA)
	bundlePath := filepath.Join(e.ServiceDir, "receiver-state", "staging", request.OperationID, "release-bundle.tar.gz")
	bundle, err := os.Open(bundlePath) // #nosec G304 -- bundlePath is the staged release-bundle.tar.gz checksum-verified by stagePayload and digest-checked on extraction.
	if err != nil {
		return nil, err
	}
	defer bundle.Close()
	if err := ExtractReleaseBundle(bundle, releaseDir, state.BundleDigest); err != nil {
		return nil, err
	}
	envBody := []byte("SERVICE_IMAGE=ferry:" + request.ReleaseSHA + "\n")
	if err := os.WriteFile(filepath.Join(releaseDir, ".env"), envBody, 0o600); err != nil {
		return nil, fmt.Errorf("write release environment: %w", err)
	}
	return e.advance(state, "files-installed")
}

// commitRelease is the only public mutation verb. On its first call it drives
// every internal, restartable phase through a locally verified candidate and
// stops at candidate-committed. After Actions verifies the canonical HTTPS
// paths, a second identical commit call seals the operation and erases the
// one-time bootstrap material.
func (e *Environment) commitRelease(ctx context.Context, request Request) (result map[string]any, err error) {
	state, err := e.loadOperation()
	if err != nil {
		return nil, err
	}
	if err := state.matches(request); err != nil {
		return nil, err
	}
	switch state.Phase {
	case "candidate-committed":
		return e.finalizeCommit(ctx, state, request)
	case "committed":
		return state.result(true), nil
	case "rolled-back", "aborted-before-deploy":
		return nil, fmt.Errorf("terminal operation cannot commit")
	}
	defer func() {
		if err == nil {
			return
		}
		if _, rollbackErr := e.rollback(ctx, request); rollbackErr != nil {
			err = fmt.Errorf("commit failed: %v; automatic rollback failed: %w", err, rollbackErr)
		} else {
			err = fmt.Errorf("commit failed and was rolled back: %w", err)
		}
	}()

	state, err = e.loadOperation()
	if err != nil {
		return nil, err
	}
	if state.Phase == "prepared" {
		if _, err = e.loadImage(ctx, request); err != nil {
			return nil, err
		}
	}
	state, err = e.loadOperation()
	if err != nil {
		return nil, err
	}
	if state.Phase == "image-loaded" {
		if _, err = e.installFiles(request); err != nil {
			return nil, err
		}
	}
	state, err = e.loadOperation()
	if err != nil {
		return nil, err
	}
	if state.Phase == "files-installed" {
		if _, err = e.deploy(ctx, request); err != nil {
			return nil, err
		}
	}
	state, err = e.loadOperation()
	if err != nil {
		return nil, err
	}
	if state.Phase == "deployed" {
		if _, err = e.verify(ctx, request); err != nil {
			return nil, err
		}
	}
	state, err = e.loadOperation()
	if err != nil {
		return nil, err
	}
	// Older receivers may have stopped after a restore drill. Accept that
	// persisted phase, but new releases go directly from readiness to candidate.
	if state.Phase != "verified" && state.Phase != "restore-drilled" {
		return nil, fmt.Errorf("commit stopped in unexpected phase %s", state.Phase)
	}
	return e.markCandidateCommitted(state, request)
}

func (e *Environment) deploy(ctx context.Context, request Request) (map[string]any, error) {
	state, err := e.requirePhase(request, "files-installed")
	if err != nil {
		return nil, err
	}
	if phaseRank[state.Phase] >= phaseRank["deployed"] {
		return state.result(true), nil
	}
	if err := e.verifyImage(ctx, request.ReleaseSHA); err != nil {
		return nil, err
	}
	dataDir := filepath.Join(e.ServiceDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chown(dataDir, appUID, appGID); err != nil {
		return nil, fmt.Errorf("own data directory: %w", err)
	}
	if !state.HadPrevious {
		imported, importErr := restoreInitialCopy(filepath.Join(e.ServiceDir, "bootstrap", "import"), dataDir,
			filepath.Join(e.ServiceDir, "receiver-state", "initial-import.sha256"))
		if importErr != nil {
			return nil, fmt.Errorf("restore initial copy: %w", importErr)
		}
		if !imported {
			if err := e.stageStartupBootstrap(); err != nil {
				return nil, fmt.Errorf("stage first-start materials: %w", err)
			}
		}
	}
	// The application opens and migrates its stores before binding listeners.
	// The receiver must never open the databases with migration authority.
	if err := chownDatabaseFiles(dataDir); err != nil {
		return nil, err
	}
	if err := e.activateRelease(request.ReleaseSHA); err != nil {
		return nil, err
	}
	if _, err := e.compose(ctx, request.ReleaseSHA, "up", "-d", "--remove-orphans"); err != nil {
		return nil, fmt.Errorf("start candidate: %w", err)
	}
	return e.advance(state, "deployed")
}

func (e *Environment) verify(ctx context.Context, request Request) (map[string]any, error) {
	state, err := e.requirePhase(request, "deployed")
	if err != nil {
		return nil, err
	}
	if phaseRank[state.Phase] >= phaseRank["verified"] {
		return state.result(true), nil
	}
	if err := e.verifyServing(ctx, request.ReleaseSHA); err != nil {
		return nil, err
	}
	return e.advance(state, "verified")
}

func (e *Environment) markCandidateCommitted(state *operationState, request Request) (map[string]any, error) {
	release := releaseState{
		Schema: "ferry/release/v1", ReleaseSHA: request.ReleaseSHA,
		CommittedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeJSONAtomic(filepath.Join(e.ServiceDir, "release.json"), release, 0o600); err != nil {
		return nil, err
	}
	state.Phase = "candidate-committed"
	state.LastTransition = release.CommittedAt
	if err := e.saveOperation(state); err != nil {
		return nil, err
	}
	return state.result(false), nil
}

func (e *Environment) finalizeCommit(ctx context.Context, state *operationState, request Request) (map[string]any, error) {
	for _, path := range []string{
		filepath.Join(e.ServiceDir, "bootstrap", "config.yaml"),
		filepath.Join(e.ServiceDir, "bootstrap", "upstream-key"),
	} {
		if err := eraseSmallFile(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	state.Phase = "committed"
	state.LastTransition = time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.saveOperation(state); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(filepath.Join(e.ServiceDir, "restore-drills", request.OperationID))
	// Cache cleanup is non-authoritative; named current/previous images and all
	// backup evidence are retained even if this best-effort step fails.
	_, _ = e.run(ctx, nil, dockerPath, "image", "prune", "-f", "--filter", "dangling=true")
	return state.result(false), nil
}

func (e *Environment) rollback(ctx context.Context, request Request) (map[string]any, error) {
	state, err := e.loadOperation()
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"phase": "no-active-operation", "idempotent": true}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := state.matches(request); err != nil {
		return nil, err
	}
	_, _ = e.compose(ctx, request.ReleaseSHA, "down", "--remove-orphans")
	if state.HadPrevious {
		if state.BackupDir != "" {
			if err := maintenance.Restore(state.BackupDir, filepath.Join(e.ServiceDir, "data")); err != nil {
				return nil, fmt.Errorf("restore previous databases: %w", err)
			}
			if err := chownDatabaseFiles(filepath.Join(e.ServiceDir, "data")); err != nil {
				return nil, err
			}
		}
		if err := e.activateRelease(state.PreviousSHA); err != nil {
			return nil, err
		}
		if _, err := e.compose(ctx, state.PreviousSHA, "up", "-d", "--remove-orphans"); err != nil {
			return nil, fmt.Errorf("restart previous release: %w", err)
		}
		if err := e.verifyServing(ctx, state.PreviousSHA); err != nil {
			return nil, fmt.Errorf("verify rollback: %w", err)
		}
		previousRelease := releaseState{
			Schema: "ferry/release/v1", ReleaseSHA: state.PreviousSHA,
			CommittedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := writeJSONAtomic(filepath.Join(e.ServiceDir, "release.json"), previousRelease, 0o600); err != nil {
			return nil, err
		}
	} else {
		dataDir := filepath.Join(e.ServiceDir, "data")
		if _, statErr := os.Stat(dataDir); statErr == nil {
			failedData := filepath.Join(e.ServiceDir, "failed", request.OperationID+"-data")
			if err := os.Rename(dataDir, failedData); err != nil {
				return nil, fmt.Errorf("preserve failed first-release data: %w", err)
			}
		}
		if err := removeCurrentSymlink(filepath.Join(e.ServiceDir, "current")); err != nil {
			return nil, err
		}
		if err := os.Remove(filepath.Join(e.ServiceDir, "release.json")); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	state.Phase = "rolled-back"
	state.LastTransition = time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.archiveOperation(state); err != nil {
		return nil, err
	}
	return state.result(false), nil
}

func (e *Environment) status(request Request) (map[string]any, error) {
	state, err := e.loadOperation()
	if err == nil {
		if err := state.matches(request); err != nil {
			return nil, err
		}
		return state.result(false), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	release, releaseErr := e.loadRelease()
	if releaseErr == nil {
		return map[string]any{"phase": "idle", "release_sha": release.ReleaseSHA}, nil
	}
	if errors.Is(releaseErr, os.ErrNotExist) {
		return map[string]any{"phase": "idle", "release_sha": ""}, nil
	}
	return nil, releaseErr
}

func (e *Environment) prune(ctx context.Context, request Request) (map[string]any, error) {
	release, err := e.loadRelease()
	if err != nil {
		return nil, err
	}
	if release.ReleaseSHA != request.ReleaseSHA {
		return nil, fmt.Errorf("only the active release may prune")
	}
	// Deliberately retain the previous immutable image and all backup evidence.
	// Docker's scoped dangling cleanup is safe and does not affect named images.
	_, err = e.run(ctx, nil, dockerPath, "image", "prune", "-f", "--filter", "dangling=true")
	if err != nil {
		return nil, err
	}
	return map[string]any{"phase": "pruned", "release_sha": release.ReleaseSHA}, nil
}

func (e *Environment) requirePhase(request Request, minimum string) (*operationState, error) {
	state, err := e.loadOperation()
	if err != nil {
		return nil, err
	}
	if err := state.matches(request); err != nil {
		return nil, err
	}
	if phaseRank[state.Phase] < phaseRank[minimum] {
		return nil, fmt.Errorf("operation phase %s is before required %s", state.Phase, minimum)
	}
	return state, nil
}

func (s *operationState) matches(request Request) error {
	if s.OperationID != request.OperationID || s.ReleaseSHA != request.ReleaseSHA {
		return fmt.Errorf("another release operation is active")
	}
	return nil
}

func (s *operationState) result(idempotent bool) map[string]any {
	result := map[string]any{
		"operation_id": s.OperationID, "release_sha": s.ReleaseSHA,
		"phase": s.Phase, "previous_sha": s.PreviousSHA, "idempotent": idempotent,
	}
	if s.Phase == "rolled-back" || s.Phase == "aborted-before-deploy" {
		result["database_restored"] = s.Phase == "rolled-back" && s.BackupDir != ""
	}
	return result
}

func (e *Environment) advance(state *operationState, phase string) (map[string]any, error) {
	if phaseRank[phase] != phaseRank[state.Phase]+1 {
		return nil, fmt.Errorf("invalid phase transition %s -> %s", state.Phase, phase)
	}
	state.Phase = phase
	state.LastTransition = time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.saveOperation(state); err != nil {
		return nil, err
	}
	return state.result(false), nil
}

func (e *Environment) loadOperation() (*operationState, error) {
	var state operationState
	if err := readJSON(filepath.Join(e.ServiceDir, "receiver-state", "active.json"), &state); err != nil {
		return nil, err
	}
	if state.Schema != "ferry/receiver-operation/v1" || phaseRank[state.Phase] == 0 {
		return nil, fmt.Errorf("invalid receiver operation state")
	}
	return &state, nil
}

func (e *Environment) saveOperation(state *operationState) error {
	return writeJSONAtomic(filepath.Join(e.ServiceDir, "receiver-state", "active.json"), state, 0o600)
}

func (e *Environment) archiveOperation(state *operationState) error {
	history := filepath.Join(e.ServiceDir, "receiver-state", "history")
	if err := os.MkdirAll(history, 0o700); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(history, state.OperationID+".json"), state, 0o600); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(e.ServiceDir, "receiver-state", "active.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (e *Environment) loadRelease() (*releaseState, error) {
	var state releaseState
	if err := readJSON(filepath.Join(e.ServiceDir, "release.json"), &state); err != nil {
		return nil, err
	}
	if state.Schema != "ferry/release/v1" || !regexp40Hex.MatchString(state.ReleaseSHA) {
		return nil, fmt.Errorf("invalid release state")
	}
	return &state, nil
}

var regexp40Hex = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (e *Environment) activateRelease(sha string) error {
	target := filepath.Join(e.ServiceDir, "releases", sha)
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		return fmt.Errorf("release directory %s is unavailable", sha)
	}
	current := filepath.Join(e.ServiceDir, "current")
	if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("current release pointer is not a symlink")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	temporary := filepath.Join(e.ServiceDir, ".current-"+sha)
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, current); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func removeCurrentSymlink(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refuse to remove non-symlink current path")
	}
	return os.Remove(path)
}

func (e *Environment) compose(ctx context.Context, sha string, args ...string) ([]byte, error) {
	releaseDir := filepath.Join(e.ServiceDir, "releases", sha)
	base := []string{"compose", "--project-name", ProjectName, "--project-directory", e.ServiceDir,
		"--env-file", filepath.Join(releaseDir, ".env"), "-f", filepath.Join(releaseDir, "compose.yml")}
	base = append(base, args...)
	return e.run(ctx, nil, dockerPath, base...)
}

func (e *Environment) verifyImage(ctx context.Context, sha string) error {
	image := "ferry:" + sha
	out, err := e.run(ctx, nil, dockerPath, "image", "inspect", image,
		"--format", `{{index .Config.Labels "org.opencontainers.image.revision"}}`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != sha {
		return fmt.Errorf("image revision label does not match release SHA")
	}
	architecture, err := e.run(ctx, nil, dockerPath, "image", "inspect", image, "--format", `{{.Architecture}}`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(architecture)) != "amd64" {
		return fmt.Errorf("release image architecture is not amd64")
	}
	return nil
}

func verifyDockerArchive(path, sha string) error {
	file, err := os.Open(path) // #nosec G304 -- path is the staged image archive from stagePayload; the embedded RepoTag is verified against the release SHA here.
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open Docker image archive: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			return fmt.Errorf("Docker image archive has no manifest.json")
		}
		if nextErr != nil {
			return fmt.Errorf("read Docker image archive: %w", nextErr)
		}
		if header.Name != "manifest.json" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 1024*1024 {
			return fmt.Errorf("Docker image manifest is invalid")
		}
		var manifest []struct {
			RepoTags []string `json:"RepoTags"`
		}
		decoder := json.NewDecoder(io.LimitReader(reader, header.Size))
		if err := decoder.Decode(&manifest); err != nil {
			return fmt.Errorf("decode Docker image manifest: %w", err)
		}
		want := "ferry:" + sha
		if len(manifest) != 1 || len(manifest[0].RepoTags) != 1 || manifest[0].RepoTags[0] != want {
			return fmt.Errorf("Docker image archive must contain only %s", want)
		}
		return nil
	}
}

func (e *Environment) verifyServing(ctx context.Context, sha string) error {
	return e.waitForServingBoundaries(ctx, sha)
}

func (e *Environment) waitForServingBoundaries(ctx context.Context, sha string) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("service readiness: %v: %w", lastErr, err)
		}
		lastErr = e.verifyVersion(ctx, "http://"+DataAddress+"/version", sha)
		if lastErr == nil {
			lastErr = e.expectStatus(ctx, http.MethodGet,
				"http://"+DataAddress+"/v1/models", "", nil, http.StatusUnauthorized)
		}
		if lastErr == nil {
			var authStatus int
			authStatus, lastErr = e.expectAnyStatus(ctx, http.MethodGet,
				"http://"+ManagementAddress+"/api/auth/status", "", nil, http.StatusOK, http.StatusNotFound)
			if lastErr == nil && authStatus == http.StatusNotFound {
				lastErr = e.expectStatus(ctx, http.MethodGet, "http://"+ManagementAddress+"/api/state", "", nil, http.StatusOK)
			} else if lastErr == nil {
				lastErr = e.expectStatus(ctx, http.MethodGet, "http://"+ManagementAddress+"/health", "", nil, http.StatusOK)
				if lastErr == nil {
					_, lastErr = e.expectAnyStatus(ctx, http.MethodGet,
						"http://"+ManagementAddress+"/api/overview", "", nil,
						http.StatusUnauthorized, http.StatusPreconditionRequired)
				}
			}
		}
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service readiness: %v: %w", lastErr, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func (e *Environment) verifyVersion(ctx context.Context, url, sha string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("version endpoint returned HTTP %d", resp.StatusCode)
	}
	var version struct {
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&version); err != nil {
		return err
	}
	if version.Commit != sha {
		return fmt.Errorf("served commit %s does not match expected commit", version.Commit)
	}
	return nil
}

func (e *Environment) expectStatus(ctx context.Context, method, url, bearer string, body []byte, want int) error {
	_, err := e.expectAnyStatus(ctx, method, url, bearer, body, want)
	return err
}

func (e *Environment) expectAnyStatus(ctx context.Context, method, url, bearer string, body []byte, wants ...int) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 256*1024))
	if readErr != nil {
		return resp.StatusCode, readErr
	}
	for _, want := range wants {
		if resp.StatusCode == want {
			return resp.StatusCode, nil
		}
	}
	return resp.StatusCode, fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
}

func (e *Environment) uploadReadbackTo(ctx context.Context, operationID, kind, backupDir, remote string) error {
	configDir := filepath.Join(e.ServiceDir, "secrets", "mc")
	if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
		return fmt.Errorf("MinIO client configuration is unavailable")
	}
	for _, name := range backupPayloadNames {
		if _, err := e.run(ctx, nil, mcPath, "--config-dir", configDir, "cp", filepath.Join(backupDir, name), remote+"/"+name); err != nil {
			return fmt.Errorf("upload %s backup: %w", kind, err)
		}
	}
	readback := filepath.Join(e.ServiceDir, "restore-drills", operationID, "readback-"+kind)
	if err := os.MkdirAll(readback, 0o700); err != nil {
		return err
	}
	for _, name := range backupPayloadNames {
		if _, err := e.run(ctx, nil, mcPath, "--config-dir", configDir, "cp", remote+"/"+name, filepath.Join(readback, name)); err != nil {
			return fmt.Errorf("read back %s backup: %w", kind, err)
		}
		if err := os.Chmod(filepath.Join(readback, name), 0o600); err != nil {
			return err
		}
	}
	if _, err := maintenance.Verify(readback); err != nil {
		return fmt.Errorf("verify MinIO readback: %w", err)
	}
	return os.RemoveAll(readback)
}

func (e *Environment) runCommand(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	switch name {
	case dockerPath, mcPath:
	default:
		return nil, fmt.Errorf("executable is not allowed: %s", filepath.Base(name))
	}
	command := exec.CommandContext(ctx, name, args...) // #nosec G204 -- name is restricted to the two absolute production executable paths above.
	command.Stdin = stdin
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if err != nil {
		return output.Bytes(), fmt.Errorf("%s failed: %w: %s", filepath.Base(name), err, strings.TrimSpace(output.String()))
	}
	return output.Bytes(), nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(payload []byte) (int, error) {
	const limit = 1024 * 1024
	remaining := limit - b.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			_, _ = b.Buffer.Write(payload[:remaining])
		} else {
			_, _ = b.Buffer.Write(payload)
		}
	}
	return len(payload), nil
}

func chownDatabaseFiles(dataDir string) error {
	if err := os.Chown(dataDir, appUID, appGID); err != nil {
		return err
	}
	for _, name := range []string{
		"control.db", "control.db-wal", "control.db-shm",
		"conversation.db", "conversation.db-wal", "conversation.db-shm",
		"ledger.db", "ledger.db-wal", "ledger.db-shm",
	} {
		path := filepath.Join(dataDir, name)
		if err := os.Chown(path, appUID, appGID); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("own %s: %w", name, err)
		}
	}
	return nil
}

func requireRootRegular(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return fmt.Errorf("%s must be a root-owned regular file with mode %o", path, mode)
	}
	return nil
}

func eraseSmallFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return fmt.Errorf("refuse to erase unexpected bootstrap file")
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- path is a bootstrap secret file already Lstat-checked as a small regular file above.
	if err != nil {
		return err
	}
	zero := make([]byte, info.Size())
	_, writeErr := file.WriteAt(zero, 0)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Remove(path)
}

func readJSON(path string, target any) error {
	file, err := os.Open(path) // #nosec G304 -- path is a staged receiver-state file written by this executor under ServiceDir.
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}
