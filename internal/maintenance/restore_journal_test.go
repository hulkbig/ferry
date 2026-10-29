package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRestoreRejectsPendingJournalAndConcurrentRestore(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	unlock, err := LockRestore(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(filepath.Join(root, "missing"), data); err == nil {
		unlock()
		t.Fatal("concurrent restore admitted")
	}
	r := RestoreRecord{Schema: "ferry/restore/v1", ID: uuid.NewString(), Data: data, Source: filepath.Join(root, "source"), Stage: filepath.Join(root, ".data.restore-test"), Phase: "prepared"}
	if err := saveRestoreRecord(r); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err := Restore(filepath.Join(root, "missing"), data); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("pending restore: %v", err)
	}
}

func TestRestoreJournalRejectsTraversalAndSymlinks(t *testing.T) {
	for _, kind := range []string{"traversal", "link"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			unlock, err := LockRestore(data)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			r := RestoreRecord{Schema: "ferry/restore/v1", ID: uuid.NewString(), Data: data, Source: filepath.Join(root, "source"), Stage: filepath.Join(root, ".data.restore-test"), Phase: "completed", CompletedAt: time.Now()}
			if kind == "traversal" {
				r.Displaced = data + ".pre-restore-123/../data"
				if err := saveRestoreRecord(r); err != nil {
					t.Fatal(err)
				}
			} else {
				outside := filepath.Join(t.TempDir(), "record.json")
				if err := os.WriteFile(outside, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(RestoreJournalDir(data), r.ID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadRestoreRecords(data); err == nil {
				t.Fatal("unsafe journal accepted")
			}
		})
	}
}
