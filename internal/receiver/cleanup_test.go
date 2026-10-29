package receiver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"ferry/internal/backuppolicy"
	"ferry/internal/maintenance"
	"github.com/google/uuid"
)

func cleanupFixture(t *testing.T) *Environment {
	t.Helper()
	e := &Environment{ServiceDir: t.TempDir()}
	if err := e.ensureLayout(); err != nil {
		t.Fatal(err)
	}
	cleanupJSON(t, filepath.Join(e.ServiceDir, "release.json"), releaseState{Schema: "ferry/release/v1", ReleaseSHA: strings.Repeat("f", 40)})
	if err := os.WriteFile(filepath.Join(e.ServiceDir, "receiver-state/receiver.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	return e
}
func cleanupJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func cleanupCopy(t *testing.T, e *Environment, path string) {
	t.Helper()
	p := filepath.Join(e.ServiceDir, path)
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "payload"), []byte(strings.Repeat("x", 32768)), 0600); err != nil {
		t.Fatal(err)
	}
}
func cleanupState(t *testing.T, e *Environment, char, phase string, age time.Duration) operationState {
	t.Helper()
	s := operationState{Schema: "ferry/receiver-operation/v1", OperationID: strings.Repeat(char, 64), ReleaseSHA: strings.Repeat(char, 40), Phase: phase, LastTransition: time.Now().Add(-age).UTC().Format(time.RFC3339Nano)}
	cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/history", s.OperationID+".json"), s)
	return s
}
func cleanupExists(t *testing.T, e *Environment, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(e.ServiceDir, path))
	if (err == nil) != want {
		t.Fatalf("exists(%s)=%v want %v", path, err, want)
	}
}
func cleanupItem(t *testing.T, r *CleanupReport, path string) CleanupItem {
	t.Helper()
	for _, x := range r.Items {
		if x.Path == path {
			return x
		}
	}
	t.Fatalf("missing item %s in %+v", path, r)
	return CleanupItem{}
}

func TestCleanupAbortedPreviewApplyAndReceipt(t *testing.T) {
	e := cleanupFixture(t)
	s := cleanupState(t, e, "a", "aborted-before-deploy", 10*24*time.Hour)
	p := "backups/" + s.OperationID
	cleanupCopy(t, e, p)
	cleanupCopy(t, e, "data")
	cleanupCopy(t, e, "data.pre-restore-unknown")
	cleanupCopy(t, e, "backups/unregistered")
	o := DefaultCleanupOptions(2)
	r, err := e.Cleanup(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if x := cleanupItem(t, r, p); x.Action != "remove" || x.Reason != "expired" {
		t.Fatal(x)
	}
	cleanupExists(t, e, p, true)
	cleanupExists(t, e, "receiver-state/cleanup", false)
	o.Apply = true
	r, err = e.Cleanup(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExists(t, e, p, false)
	for _, p := range []string{"data", "data.pre-restore-unknown", "backups/unregistered"} {
		cleanupExists(t, e, p, true)
	}
	if r.Status != "completed" || r.RemovedAllocatedBytes == 0 || r.RemainingAllocatedBytes != 0 {
		t.Fatalf("receipt %+v", r)
	}
	var saved CleanupReport
	root, err := os.OpenRoot(e.ServiceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := cleanupReadJSON(root, "receiver-state/cleanup/"+r.ID+".json", &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "completed" || saved.RemovedAllocatedBytes != r.RemovedAllocatedBytes {
		t.Fatal(saved)
	}
	r, err = e.Cleanup(context.Background(), o)
	if err != nil || r.RemovedAllocatedBytes != 0 {
		t.Fatalf("repeated cleanup %+v %v", r, err)
	}
}

func TestCleanupUnresolvedAndUnknownPhasesNeverDelete(t *testing.T) {
	for _, mode := range []string{"active", "history", "restore", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			e := cleanupFixture(t)
			s := cleanupState(t, e, "a", "aborted-before-deploy", 10*24*time.Hour)
			p := "backups/" + s.OperationID
			cleanupCopy(t, e, p)
			switch mode {
			case "active":
				s.Phase = "prepared"
				cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/active.json"), s)
			case "history":
				cleanupState(t, e, "b", "rollback-failed", 20*24*time.Hour)
			case "restore":
				restoreRecordFixture(t, e, "prepared", 20*24*time.Hour)
			case "corrupt":
				s.LastTransition = "not-a-time"
				cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/history", s.OperationID+".json"), s)
			}
			o := DefaultCleanupOptions(1)
			o.Apply = true
			r, err := e.Cleanup(context.Background(), o)
			if mode == "corrupt" {
				if err == nil {
					t.Fatal("corrupt evidence accepted")
				}
			} else if err != nil || r.Skipped == "" {
				t.Fatalf("%+v %v", r, err)
			}
			cleanupExists(t, e, p, true)
		})
	}
}

func TestCleanupHonorsReceiverAndRestoreLocks(t *testing.T) {
	for _, mode := range []string{"receiver", "restore"} {
		t.Run(mode, func(t *testing.T) {
			e := cleanupFixture(t)
			var unlock func()
			if mode == "restore" {
				var err error
				unlock, err = maintenance.LockRestore(filepath.Join(e.ServiceDir, "data"))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := os.OpenFile(filepath.Join(e.ServiceDir, "receiver-state/receiver.lock"), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				unlock = func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
			}
			if _, err := e.Cleanup(context.Background(), DefaultCleanupOptions(1)); err == nil {
				unlock()
				t.Fatal("concurrent cleanup admitted")
			}
			unlock()
			if _, err := e.Cleanup(context.Background(), DefaultCleanupOptions(1)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupProtectsActiveReferenceAndExistingRollbackCopies(t *testing.T) {
	e := cleanupFixture(t)
	for i, c := range []string{"a", "b", "c"} {
		s := cleanupState(t, e, c, "committed", time.Duration(20+i)*24*time.Hour)
		cleanupCopy(t, e, "backups/"+s.OperationID)
	}
	active := cleanupState(t, e, "d", "committed", 24*time.Hour)
	active.BackupDir = filepath.Join(e.ServiceDir, "backups", strings.Repeat("c", 64), "original")
	cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/active.json"), active)
	o := DefaultCleanupOptions(1)
	o.Apply = true
	if _, err := e.Cleanup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	cleanupExists(t, e, "backups/"+strings.Repeat("a", 64), true)
	cleanupExists(t, e, "backups/"+strings.Repeat("b", 64), false)
	cleanupExists(t, e, "backups/"+strings.Repeat("c", 64), true)
}

func TestCleanupRetainsNewestCopyAcrossTimestampFormats(t *testing.T) {
	e := cleanupFixture(t)
	// A string comparison would put Z before .5 and retain the older copy.
	for i, stamp := range []string{"2025-01-01T00:00:00Z", "2025-01-01T00:00:00.5Z"} {
		s := cleanupState(t, e, string(rune('a'+i)), "committed", 0)
		s.LastTransition = stamp
		cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/history", s.OperationID+".json"), s)
		cleanupCopy(t, e, "backups/"+s.OperationID)
	}
	o := DefaultCleanupOptions(1)
	o.Apply = true
	if _, err := e.Cleanup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	cleanupExists(t, e, "backups/"+strings.Repeat("a", 64), false)
	cleanupExists(t, e, "backups/"+strings.Repeat("b", 64), true)
}

func restoreRecordFixture(t *testing.T, e *Environment, phase string, age time.Duration) maintenance.RestoreRecord {
	t.Helper()
	id := uuid.NewString()
	data := filepath.Join(e.ServiceDir, "data")
	r := maintenance.RestoreRecord{Schema: "ferry/restore/v1", ID: id, Data: data, Source: filepath.Join(e.ServiceDir, "backups/source"), Stage: filepath.Join(e.ServiceDir, ".data.restore-"+id), Displaced: data + ".pre-restore-" + strconv.FormatInt(time.Now().UnixNano(), 10), Phase: phase, CompletedAt: time.Now().Add(-age)}
	cleanupJSON(t, filepath.Join(maintenance.RestoreJournalDir(data), id+".json"), r)
	cleanupCopy(t, e, filepath.Base(r.Displaced))
	return r
}

func TestCleanupRestoreCopiesRequireJournalAndRetainCompensation(t *testing.T) {
	e := cleanupFixture(t)
	old := restoreRecordFixture(t, e, "completed", 20*24*time.Hour)
	recent := restoreRecordFixture(t, e, "completed", 10*24*time.Hour)
	cleanupCopy(t, e, "data.pre-restore-legacy")
	cleanupCopy(t, e, "data")
	o := DefaultCleanupOptions(1)
	o.Apply = true
	if _, err := e.Cleanup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	cleanupExists(t, e, filepath.Base(old.Displaced), false)
	cleanupExists(t, e, filepath.Base(recent.Displaced), true)
	cleanupExists(t, e, "data.pre-restore-legacy", true)
	cleanupExists(t, e, "data", true)
}

func TestCleanupAgeBudgetAndProtectedExcess(t *testing.T) {
	e := cleanupFixture(t)
	recent := cleanupState(t, e, "a", "aborted-before-deploy", time.Hour)
	old := cleanupState(t, e, "b", "aborted-before-deploy", 48*time.Hour)
	kept := cleanupState(t, e, "c", "committed", 72*time.Hour)
	for _, s := range []operationState{recent, old, kept} {
		cleanupCopy(t, e, "backups/"+s.OperationID)
	}
	o := DefaultCleanupOptions(1)
	o.MaxBytes = 1
	o.Apply = true
	r, err := e.Cleanup(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExists(t, e, "backups/"+old.OperationID, false)
	cleanupExists(t, e, "backups/"+recent.OperationID, true)
	cleanupExists(t, e, "backups/"+kept.OperationID, true)
	if !r.OverBudget || cleanupItem(t, r, "backups/"+recent.OperationID).Reason != "minimum_age" {
		t.Fatal(r)
	}
}

func TestCleanupLinksAndChangedTreeFailClosed(t *testing.T) {
	for _, mode := range []string{"symlink", "parent-link", "hardlink", "changed"} {
		t.Run(mode, func(t *testing.T) {
			e := cleanupFixture(t)
			s := cleanupState(t, e, "a", "aborted-before-deploy", 20*24*time.Hour)
			p := "backups/" + s.OperationID
			cleanupCopy(t, e, p)
			outside := t.TempDir()
			file := filepath.Join(outside, "live.db")
			if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "changed" {
				r, err := e.Cleanup(context.Background(), DefaultCleanupOptions(1))
				if err != nil {
					t.Fatal(err)
				}
				x := cleanupItem(t, r, p)
				if err := os.WriteFile(filepath.Join(e.ServiceDir, p, "new"), []byte("new"), 0600); err != nil {
					t.Fatal(err)
				}
				root, err := os.OpenRoot(e.ServiceDir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if err := removeCleanupItem(context.Background(), root, &x); err == nil {
					t.Fatal("changed tree removed")
				}
			} else {
				if mode == "parent-link" {
					if err := os.Rename(filepath.Join(e.ServiceDir, "backups"), filepath.Join(outside, "backups")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(outside, "backups"), filepath.Join(e.ServiceDir, "backups")); err != nil {
						t.Fatal(err)
					}
				} else {
					link := os.Symlink
					if mode == "hardlink" {
						link = os.Link
					}
					if err := link(file, filepath.Join(e.ServiceDir, p, "live")); err != nil {
						t.Fatal(err)
					}
				}
				o := DefaultCleanupOptions(1)
				o.Apply = true
				if _, err := e.Cleanup(context.Background(), o); err == nil {
					t.Fatal("link accepted")
				}
			}
			cleanupExists(t, e, p, true)
			if b, err := os.ReadFile(file); err != nil || string(b) != "keep" {
				t.Fatal("live data changed")
			}
		})
	}
}

func TestCleanupScheduledCopiesStayUnderBackupPolicy(t *testing.T) {
	e := cleanupFixture(t)
	for i, name := range []string{"20260901T000000Z", "20260902T000000Z", "20260903T000000Z"} {
		p := "backups/scheduled/" + name
		cleanupCopy(t, e, p)
		if i < 2 {
			cleanupJSON(t, filepath.Join(e.ServiceDir, p, "verified.json"), map[string]bool{"verified_readback": true})
		}
	}
	o := DefaultCleanupOptions(1)
	o.Apply = true
	if _, err := e.Cleanup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	cleanupExists(t, e, "backups/scheduled/20260901T000000Z", false)
	cleanupExists(t, e, "backups/scheduled/20260902T000000Z", true)
	cleanupExists(t, e, "backups/scheduled/20260903T000000Z", true)
}

func TestCleanupCancellationAndForeignRestoreIdentity(t *testing.T) {
	e := cleanupFixture(t)
	s := cleanupState(t, e, "a", "aborted-before-deploy", 20*24*time.Hour)
	p := "backups/" + s.OperationID
	cleanupCopy(t, e, p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := DefaultCleanupOptions(1)
	o.Apply = true
	if _, err := e.Cleanup(ctx, o); err == nil {
		t.Fatal("canceled cleanup ran")
	}
	cleanupExists(t, e, p, true)
	r := restoreRecordFixture(t, e, "completed", 20*24*time.Hour)
	r.Displaced = filepath.Join(e.ServiceDir, "data")
	cleanupJSON(t, filepath.Join(maintenance.RestoreJournalDir(r.Data), r.ID+".json"), r)
	if _, err := e.Cleanup(context.Background(), o); err == nil {
		t.Fatal("live data allowed as compensation")
	}
	cleanupExists(t, e, p, true)
}

func TestScheduledBackupCleansEvenWhenDisabled(t *testing.T) {
	e := cleanupFixture(t)
	s := cleanupState(t, e, "a", "aborted-before-deploy", 20*24*time.Hour)
	p := "backups/" + s.OperationID
	cleanupCopy(t, e, p)
	if err := os.MkdirAll(filepath.Join(e.ServiceDir, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(e.ServiceDir, "data/control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE runtime_settings(key TEXT PRIMARY KEY,value TEXT)"); err != nil {
		t.Fatal(err)
	}
	policy := backuppolicy.Default()
	policy.Enabled = false
	b, _ := json.Marshal(policy)
	if _, err = db.Exec("INSERT INTO runtime_settings VALUES(?,?)", backuppolicy.SettingKey, string(b)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var out strings.Builder
	e.Stdout = &out
	if err := e.ScheduledBackup(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanupExists(t, e, p, false)
	if !strings.Contains(out.String(), "disabled") {
		t.Fatal(out.String())
	}
}

func TestCleanupReceiptHistoryIsBounded(t *testing.T) {
	e := cleanupFixture(t)
	for i := 0; i < 66; i++ {
		r := CleanupReport{Schema: "ferry/local-cleanup/v1", ID: uuid.NewString(), Status: "completed", CreatedAt: time.Now().Add(-time.Duration(i+1) * time.Hour)}
		if i == 0 {
			// A real plan with hundreds of copies exceeds operation JSON limits.
			r.Items = []CleanupItem{{Path: strings.Repeat("x", 70000)}}
		}
		cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/cleanup", r.ID+".json"), r)
	}
	pending := CleanupReport{Schema: "ferry/local-cleanup/v1", ID: uuid.NewString(), Status: "applying", CreatedAt: time.Now().Add(-100 * time.Hour)}
	cleanupJSON(t, filepath.Join(e.ServiceDir, "receiver-state/cleanup", pending.ID+".json"), pending)
	o := DefaultCleanupOptions(1)
	o.Apply = true
	if _, err := e.Cleanup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(e.ServiceDir, "receiver-state/cleanup"))
	if err != nil || len(entries) != 65 {
		t.Fatalf("receipts %d %v", len(entries), err)
	}
	cleanupExists(t, e, "receiver-state/cleanup/"+pending.ID+".json", true)
}

func TestScheduledBackupContinuesAfterRetentionErrorButBlocksUnresolvedState(t *testing.T) {
	for _, mode := range []string{"hardlink", "restore_unresolved", "restore_locked", "history_unresolved"} {
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
			policy.Enabled = true
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
			old := cleanupState(t, e, "a", "aborted-before-deploy", 20*24*time.Hour)
			oldPath := "backups/" + old.OperationID
			cleanupCopy(t, e, oldPath)
			switch mode {
			case "hardlink":
				p := filepath.Join(e.ServiceDir, oldPath, "payload")
				if err := os.Link(p, p+"-linked"); err != nil {
					t.Fatal(err)
				}
			case "restore_unresolved":
				restoreRecordFixture(t, e, "prepared", time.Hour)
			case "restore_locked":
				unlock, err := maintenance.LockRestore(data)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "history_unresolved":
				cleanupState(t, e, "b", "rollback-failed", time.Hour)
			}
			if err := os.MkdirAll(filepath.Join(e.ServiceDir, "secrets/mc"), 0700); err != nil {
				t.Fatal(err)
			}
			// Exercise the real SQLite snapshot and readback verifier, replacing
			// only the remote transfer. No external executable or network runs.
			remote := map[string][]byte{}
			transfers := 0
			e.run = func(_ context.Context, _ io.Reader, name string, args ...string) ([]byte, error) {
				if name != mcPath || len(args) != 5 || args[2] != "cp" {
					return nil, fmt.Errorf("unexpected backup command")
				}
				transfers++
				if strings.HasPrefix(args[3], minioAlias+"/") {
					b, ok := remote[args[3]]
					if !ok {
						return nil, fmt.Errorf("missing remote object")
					}
					return nil, os.WriteFile(args[4], b, 0600)
				}
				b, err := os.ReadFile(args[3])
				remote[args[4]] = b
				return nil, err
			}
			var output strings.Builder
			e.Stdout = &output
			err = e.ScheduledBackup(context.Background())
			if mode == "restore_locked" {
				if err == nil {
					t.Fatal("backup crossed restore lock")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "hardlink" {
				var result map[string]any
				if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
					t.Fatal(err)
				}
				if transfers != 8 || result["verified_readback"] != true || result["cleanup_error"] == nil {
					t.Fatalf("backup did not complete with retention warning: %d %+v", transfers, result)
				}
			} else if transfers != 0 {
				t.Fatal("backup ran across unresolved state")
			}
			cleanupExists(t, e, oldPath, true)
		})
	}
}
