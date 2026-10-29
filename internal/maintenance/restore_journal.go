package maintenance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// RestoreRecord proves ownership of a displaced directory. An unfinished
// record is recovery evidence, never permission to delete anything.
type RestoreRecord struct {
	Schema      string    `json:"schema"`
	ID          string    `json:"id"`
	Data        string    `json:"data"`
	Source      string    `json:"source"`
	Displaced   string    `json:"displaced,omitempty"`
	Stage       string    `json:"stage"`
	Phase       string    `json:"phase"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

func RestoreJournalDir(data string) string {
	return filepath.Join(filepath.Dir(data), "."+filepath.Base(data)+".restore-state")
}

// LockRestore serializes offline restore with receiver GC. The receiver takes
// receiver.lock first, then this lock; Restore never takes receiver.lock.
// The lock lives beside data so swapping the data directory cannot replace it.
func LockRestore(data string) (func(), error) {
	dir := RestoreJournalDir(data)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("restore journal must be a plain directory")
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("restore or cleanup is active: %w", err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func saveRestoreRecord(r RestoreRecord) error {
	dir := RestoreJournalDir(r.Data)
	f, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(r); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, r.ID+".json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ReadRestoreRecords must be called while holding LockRestore. Unknown or
// malformed records fail closed rather than turning a pathname into authority.
func ReadRestoreRecords(data string) ([]RestoreRecord, error) {
	dir := RestoreJournalDir(data)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RestoreRecord
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 65536 {
			return nil, fmt.Errorf("invalid restore record")
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var r RestoreRecord
		if json.Unmarshal(b, &r) != nil {
			return nil, fmt.Errorf("invalid restore record JSON")
		}
		_, idErr := uuid.Parse(r.ID)
		if idErr != nil || entry.Name() != r.ID+".json" || r.Schema != "ferry/restore/v1" || r.Data != data || !filepath.IsAbs(r.Source) || filepath.Clean(r.Source) != r.Source ||
			filepath.Clean(r.Stage) != r.Stage || filepath.Dir(r.Stage) != filepath.Dir(data) || !strings.HasPrefix(filepath.Base(r.Stage), "."+filepath.Base(data)+".restore-") ||
			(r.Displaced != "" && !ValidDisplacedPath(data, r.Displaced)) {
			return nil, fmt.Errorf("invalid restore record identity")
		}
		if r.Phase != "prepared" && r.Phase != "completed" && r.Phase != "compensated" {
			return nil, fmt.Errorf("invalid restore phase")
		}
		if r.Phase != "prepared" && r.CompletedAt.IsZero() {
			return nil, fmt.Errorf("restore completion time missing")
		}
		out = append(out, r)
	}
	return out, nil
}

func ValidDisplacedPath(data, path string) bool {
	if filepath.Clean(path) != path || filepath.Dir(path) != filepath.Dir(data) || !strings.HasPrefix(path, data+".pre-restore-") {
		return false
	}
	suffix := strings.TrimPrefix(path, data+".pre-restore-")
	n, err := strconv.ParseInt(suffix, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == suffix
}
