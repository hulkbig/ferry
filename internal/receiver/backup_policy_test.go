package receiver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ferry/internal/backuppolicy"
	"ferry/internal/maintenance"
)

func TestRetentionProtectsActiveRollbackAndIncompleteCopies(t *testing.T) {
	root := t.TempDir()
	e := &Environment{ServiceDir: root}
	if err := e.ensureLayout(); err != nil {
		t.Fatal(err)
	}
	put := func(path string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	active := strings.Repeat("a", 64)
	put(filepath.Join(root, "release.json"), releaseState{Schema: "ferry/release/v1", ReleaseSHA: strings.Repeat("f", 40)})
	recent := strings.Repeat("b", 64)
	old := strings.Repeat("c", 64)
	unknown := strings.Repeat("d", 64)
	put(filepath.Join(root, "receiver-state", "active.json"), operationState{Schema: "ferry/receiver-operation/v1", OperationID: active, Phase: "committed"})
	for i, id := range []string{active, recent, old} {
		put(filepath.Join(root, "receiver-state", "history", id+".json"), operationState{Schema: "ferry/receiver-operation/v1", OperationID: id, Phase: "committed", LastTransition: []string{"2026-09-09T00:00:00Z", "2026-09-08T00:00:00Z", "2026-09-07T00:00:00Z"}[i]})
	}
	put(filepath.Join(root, "receiver-state", "active.json"), operationState{Schema: "ferry/receiver-operation/v1", OperationID: active, Phase: "committed", LastTransition: "2026-09-09T00:00:00Z"})
	for _, id := range []string{active, recent, old, unknown} {
		for _, kind := range []string{"backups", "restore-drills"} {
			if err := os.MkdirAll(filepath.Join(root, kind, id), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.cleanupBackupCopies(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{active, recent, unknown} {
		if _, err := os.Stat(filepath.Join(root, "backups", id)); err != nil {
			t.Fatalf("protected copy removed: %s", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "backups", old)); !os.IsNotExist(err) {
		t.Fatal("old copy retained")
	}
	if _, err := os.Stat(filepath.Join(root, "restore-drills", recent)); !os.IsNotExist(err) {
		t.Fatal("terminal drill retained")
	}
	put(filepath.Join(root, "receiver-state", "active.json"), operationState{Schema: "ferry/receiver-operation/v1", OperationID: active, Phase: "prepared"})
	if err := e.cleanupBackupCopies(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "backups", recent)); err != nil {
		t.Fatal("cleanup ran during release")
	}
}

func TestRemoveCopyRejectsSymlinkAndTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "..", "../outside"} {
		if removeCopy(root, name) == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestRemoteRetentionOnlyDeletesRecordedPayloadKeys(t *testing.T) {
	var calls []string
	e := &Environment{ServiceDir: t.TempDir(), run: func(_ context.Context, _ io.Reader, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	r := &scheduledReceipt{Objects: []string{"ferry-minio/app-backups/ferry/scheduled/scheduled-20260901T000000Z", "ferry-minio/app-backups/ferry/scheduled/scheduled-20260902T000000Z"}}
	if err := e.pruneRemoteCopies(context.Background(), r, 1); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 || len(r.Objects) != 1 {
		t.Fatalf("calls=%v receipt=%v", calls, r)
	}
	for _, call := range calls {
		if strings.Contains(call, "--recursive") || !strings.Contains(call, "20260901T000000Z/") {
			t.Fatal(call)
		}
	}
	r.Objects = append([]string{"ferry-minio/app-backups/other/scheduled-20260901T000000Z"}, r.Objects...)
	if e.pruneRemoteCopies(context.Background(), r, 1) == nil {
		t.Fatal("accepted foreign destination")
	}
}

func TestCalendarBackupDueIgnoresTimerJitter(t *testing.T) {
	last := time.Date(2026, 9, 8, 19, 30, 0, 0, time.UTC)
	next := time.Date(2026, 9, 9, 19, 15, 0, 0, time.UTC)
	if next.Before(nextScheduledBackup(last, 24)) {
		t.Fatal("daily backup skipped due to jitter")
	}
	if !next.Before(nextScheduledBackup(last, 168)) {
		t.Fatal("weekly backup ran early")
	}
}

func TestBackupNowKeepsScopeAndSafetyGates(t *testing.T) {
	for _, mode := range []string{"not_due", "now", "disabled", "release_in_progress"} {
		t.Run(mode, func(t *testing.T) {
			e := cleanupFixture(t)
			config := filepath.Join(e.ServiceDir, "test-config.yaml")
			if err := os.WriteFile(config, []byte("server:\n  management_addr: 127.0.0.1:7900\n  proxy_addr: 127.0.0.1:7890\n  timezone: UTC\n"), 0600); err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(e.ServiceDir, "data")
			if _, err := maintenance.Migrate(data, config); err != nil {
				t.Fatal(err)
			}
			policy := backuppolicy.Default()
			policy.Enabled = mode != "disabled"
			// An older saved opt-in must never expand scheduled backup scope.
			policy.IncludeHistory = true
			db, err := sql.Open("sqlite", filepath.Join(data, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(policy)
			_, err = db.Exec("INSERT OR REPLACE INTO runtime_settings(key,value,updated_at) VALUES(?,?,?)", backuppolicy.SettingKey, string(body), time.Now().UTC().Format(time.RFC3339Nano))
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/scheduled-backup.json"), scheduledReceipt{DestinationID: s3DestinationID(policy), CompletedAt: time.Now()})
			if mode == "release_in_progress" {
				s := cleanupState(t, e, "a", "prepared", time.Minute)
				cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/active.json"), s)
			}
			if err := os.MkdirAll(filepath.Join(e.ServiceDir, "secrets/mc"), 0700); err != nil {
				t.Fatal(err)
			}
			remote := map[string][]byte{}
			e.run = func(_ context.Context, _ io.Reader, name string, args ...string) ([]byte, error) {
				if name != mcPath || len(args) != 5 || args[2] != "cp" {
					return nil, fmt.Errorf("unexpected backup command")
				}
				if strings.HasPrefix(args[3], minioAlias+"/") {
					return nil, os.WriteFile(args[4], remote[args[3]], 0600)
				}
				body, err := os.ReadFile(args[3])
				remote[args[4]] = body
				return nil, err
			}
			var out strings.Builder
			e.Stdout = &out
			if mode == "not_due" {
				err = e.ScheduledBackup(context.Background())
			} else {
				err = e.BackupNow(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
				t.Fatal(err)
			}
			if mode != "now" {
				if len(remote) != 0 || result["skipped"] != mode {
					t.Fatalf("backup crossed safety gate: %s", out.String())
				}
				return
			}
			if len(remote) != 4 || result["verified_readback"] != true || result["include_history"] != false {
				t.Fatalf("backup failed: %s", out.String())
			}
			for path, body := range remote {
				if strings.HasSuffix(path, "/manifest.json") {
					var manifest maintenance.Manifest
					if err := json.Unmarshal(body, &manifest); err != nil || manifest.Scope != "configuration" {
						t.Fatal("scheduled backup contains history")
					}
				}
			}
		})
	}
}
