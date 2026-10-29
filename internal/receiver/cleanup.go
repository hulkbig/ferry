package receiver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"ferry/internal/maintenance"
	"github.com/google/uuid"
)

type CleanupOptions struct {
	Apply       bool
	LocalCopies int
	MinAge      time.Duration
	MaxAge      time.Duration
	MaxBytes    int64
}

func DefaultCleanupOptions(keep int) CleanupOptions {
	return CleanupOptions{LocalCopies: keep, MinAge: 24 * time.Hour, MaxAge: 7 * 24 * time.Hour, MaxBytes: 32 << 30}
}

type CleanupItem struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	Reason         string `json:"reason"`
	Action         string `json:"action"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	at             time.Time
	protected      bool
}

type CleanupReport struct {
	Schema                  string        `json:"schema"`
	ID                      string        `json:"id"`
	CreatedAt               time.Time     `json:"created_at"`
	DryRun                  bool          `json:"dry_run"`
	Status                  string        `json:"status"`
	Skipped                 string        `json:"skipped,omitempty"`
	Items                   []CleanupItem `json:"items"`
	ManagedAllocatedBytes   int64         `json:"managed_allocated_bytes"`
	RemainingAllocatedBytes int64         `json:"remaining_allocated_bytes"`
	RemovedAllocatedBytes   int64         `json:"removed_allocated_bytes"`
	ObservedFreeBytesDelta  int64         `json:"observed_free_bytes_delta"`
	OverBudget              bool          `json:"over_budget"`
	Error                   string        `json:"error,omitempty"`
	LocalCopies             int           `json:"local_copies"`
	MinAge                  string        `json:"min_age"`
	MaxAge                  string        `json:"max_age"`
	MaxBytes                int64         `json:"max_bytes"`
	// Set only after release/restore state and all retained references have
	// been checked. Copy inspection or receipt errors must not starve backups.
	backupSafe bool
}

func terminalCopyPhase(p string) bool {
	return p == "committed" || p == "rolled-back" || p == "aborted-before-deploy"
}

// Cleanup is host-local, never an SSH forced-command verb. Preview and apply
// both plan under the same release and restore locks. Apply replans; an old
// preview is never taken as deletion authority.
func (e *Environment) Cleanup(ctx context.Context, options CleanupOptions) (*CleanupReport, error) {
	lock, err := os.OpenFile(filepath.Join(e.ServiceDir, "receiver-state", "receiver.lock"), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("receiver is active: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return e.cleanupLocked(ctx, options)
}

// cleanupLocked requires receiver.lock. It does not read credentials, run
// Compose, contact S3, or open any application database.
func (e *Environment) cleanupLocked(ctx context.Context, options CleanupOptions) (report *CleanupReport, err error) {
	if options.LocalCopies < 1 || options.LocalCopies > 7 || options.MinAge < time.Hour || options.MaxAge < options.MinAge || options.MaxBytes <= 0 {
		return nil, fmt.Errorf("invalid cleanup retention")
	}
	data, err := filepath.Abs(filepath.Join(e.ServiceDir, "data"))
	if err != nil {
		return nil, err
	}
	unlock, err := maintenance.LockRestore(data)
	if err != nil {
		return nil, err
	}
	defer unlock()
	root, err := os.OpenRoot(e.ServiceDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, path := range []string{"receiver-state", "receiver-state/history", "receiver-state/active.json", "release.json", "backups", "backups/scheduled", "restore-drills", "failed"} {
		if err := plainCleanupPath(root, path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	report = &CleanupReport{Schema: "ferry/local-cleanup/v1", ID: uuid.NewString(), CreatedAt: time.Now().UTC(), DryRun: !options.Apply, Status: "preview", Items: []CleanupItem{}}
	report.LocalCopies, report.MinAge, report.MaxAge, report.MaxBytes = options.LocalCopies, options.MinAge.String(), options.MaxAge.String(), options.MaxBytes
	defer func() {
		if err != nil {
			report.Status, report.Error = "failed", err.Error()
		}
	}()
	if err = e.planCleanup(ctx, root, data, options, report); err != nil {
		report.Status, report.Error = "failed", err.Error()
		return report, err
	}
	if report.Skipped != "" {
		report.Status = "skipped"
	}
	if !options.Apply || report.Skipped != "" {
		return report, nil
	}
	journal := filepath.Join(e.ServiceDir, "receiver-state", "cleanup")
	if err = os.MkdirAll(journal, 0700); err != nil {
		return report, err
	}
	if err = plainCleanupPath(root, "receiver-state/cleanup"); err != nil {
		return report, err
	}
	before, statErr := cleanupFreeBytes(e.ServiceDir)
	if statErr != nil {
		return report, statErr
	}
	report.Status = "applying"
	if err = saveCleanupReceipt(root, report); err != nil {
		return report, err
	}
	defer func() {
		if after, e := cleanupFreeBytes(e.ServiceDir); e == nil {
			report.ObservedFreeBytesDelta = after - before
		}
		report.RemainingAllocatedBytes = report.ManagedAllocatedBytes - report.RemovedAllocatedBytes
		report.OverBudget = report.RemainingAllocatedBytes > options.MaxBytes
		report.Status = "completed"
		if err != nil {
			report.Status, report.Error = "failed", err.Error()
		}
		if saveErr := saveCleanupReceipt(root, report); saveErr != nil {
			err = fmt.Errorf("cleanup receipt: %v (operation: %v)", saveErr, err)
		}
	}()
	for i := range report.Items {
		item := &report.Items[i]
		if item.Action != "remove" {
			continue
		}
		if err = ctx.Err(); err != nil {
			return report, err
		}
		if err = removeCleanupItem(ctx, root, item); err != nil {
			return report, err
		}
		report.RemovedAllocatedBytes += item.AllocatedBytes
		item.Action = "removed"
		if err = saveCleanupReceipt(root, report); err != nil {
			return report, err
		}
	}
	err = trimCleanupReceipts(root)
	return report, err
}

func (e *Environment) planCleanup(ctx context.Context, root *os.Root, data string, o CleanupOptions, r *CleanupReport) error {
	active, err := e.loadOperation()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if active != nil && !terminalCopyPhase(active.Phase) {
		r.Skipped = "release_in_progress"
		return nil
	}
	if _, err = e.loadRelease(); err != nil {
		if os.IsNotExist(err) {
			r.Skipped = "no_committed_release"
			return nil
		}
		return err
	}
	restores, err := maintenance.ReadRestoreRecords(data)
	if err != nil {
		return err
	}
	for _, x := range restores {
		if x.Phase == "prepared" {
			r.Skipped = "restore_unresolved"
			return nil
		}
	}
	states := map[string]operationState{}
	entries, err := fs.ReadDir(root.FS(), "receiver-state/history")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var s operationState
		if err := cleanupReadJSON(root, "receiver-state/history/"+entry.Name(), &s); err != nil {
			return err
		}
		if s.Schema != "ferry/receiver-operation/v1" || !cleanupOperationID(s.OperationID) || entry.Name() != s.OperationID+".json" {
			return fmt.Errorf("invalid historical operation")
		}
		if !terminalCopyPhase(s.Phase) {
			r.Skipped = "history_unresolved"
			return nil
		}
		states[s.OperationID] = s
	}
	if active != nil {
		if !cleanupOperationID(active.OperationID) {
			return fmt.Errorf("invalid active operation")
		}
		states[active.OperationID] = *active
	}
	var ordered []operationState
	transitionTimes := make(map[string]time.Time, len(states))
	for _, s := range states {
		at, err := time.Parse(time.RFC3339Nano, s.LastTransition)
		if err != nil {
			return fmt.Errorf("invalid terminal timestamp")
		}
		transitionTimes[s.OperationID] = at
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := transitionTimes[ordered[i].OperationID], transitionTimes[ordered[j].OperationID]
		if a.Equal(b) {
			return ordered[i].OperationID < ordered[j].OperationID
		}
		return a.After(b)
	})
	protected := map[string]bool{}
	protectRef := func(path string) error {
		if path == "" {
			return nil
		}
		abs, err := filepath.Abs(e.ServiceDir)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil || !filepath.IsLocal(rel) {
			return fmt.Errorf("backup reference is outside service root")
		}
		protected[filepath.ToSlash(rel)] = true
		return nil
	}
	kept := 0
	for _, s := range ordered {
		hasCopy := false
		for _, path := range []string{"backups/" + s.OperationID, "failed/" + s.OperationID + "-data"} {
			if _, err := root.Lstat(path); err == nil {
				hasCopy = true
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if s.BackupDir != "" {
			if _, err := os.Lstat(s.BackupDir); err == nil {
				hasCopy = true
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if active != nil && active.OperationID == s.OperationID || hasCopy && s.Phase != "aborted-before-deploy" && kept < o.LocalCopies {
			if hasCopy && s.Phase != "aborted-before-deploy" {
				kept++
			}
			protected["backups/"+s.OperationID] = true
			if active != nil && active.OperationID == s.OperationID {
				protected["restore-drills/"+s.OperationID] = true
			}
			protected["failed/"+s.OperationID+"-data"] = true
			if err := protectRef(s.BackupDir); err != nil {
				return err
			}
		}
	}
	sort.Slice(restores, func(i, j int) bool { return restores[i].CompletedAt.After(restores[j].CompletedAt) })
	kept = 0
	for _, x := range restores {
		if x.Displaced == "" {
			continue
		}
		if _, err := os.Lstat(x.Displaced); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if kept < o.LocalCopies {
			protected[filepath.Base(x.Displaced)] = true
			// A kept compensation point may refer to a receiver-managed source.
			if strings.HasPrefix(x.Source, filepath.Dir(data)+string(filepath.Separator)) {
				if err := protectRef(x.Source); err != nil {
					return err
				}
			}
			kept++
		}
	}
	r.backupSafe = true
	known := map[string]bool{}
	add := func(path, kind string, at time.Time) error {
		if !cleanupCopyPath(path, kind) {
			return fmt.Errorf("unmanaged cleanup path: %s", path)
		}
		known[path] = true
		if _, err := root.Lstat(path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		pinned := protected[path]
		for ref := range protected {
			if strings.HasPrefix(ref, path+"/") || strings.HasPrefix(path, ref+"/") {
				pinned = true
			}
		}
		item := CleanupItem{Path: path, Kind: kind, at: at, protected: pinned, Action: "keep", Reason: "within_retention"}
		if item.protected {
			item.Reason = "referenced_or_retained"
		}
		bytes, fingerprint, err := measureCleanup(ctx, root, path)
		if err != nil {
			return err
		}
		item.AllocatedBytes, item.Fingerprint = bytes, fingerprint
		r.ManagedAllocatedBytes += bytes
		r.Items = append(r.Items, item)
		return nil
	}
	for _, s := range ordered {
		at := transitionTimes[s.OperationID]
		kind := "release_backup"
		if s.Phase == "aborted-before-deploy" {
			kind = "terminal_temporary"
		}
		if err := add("backups/"+s.OperationID, kind, at); err != nil {
			return err
		}
		if err := add("restore-drills/"+s.OperationID, "terminal_temporary", at); err != nil {
			return err
		}
		if s.Phase == "rolled-back" && !s.HadPrevious {
			if err := add("failed/"+s.OperationID+"-data", "compensation", at); err != nil {
				return err
			}
		}
	}
	for _, x := range restores {
		if x.Displaced != "" {
			if err := add(filepath.Base(x.Displaced), "compensation", x.CompletedAt); err != nil {
				return err
			}
		}
	}
	scheduled, err := fs.ReadDir(root.FS(), "backups/scheduled")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var verified []string
	for _, entry := range scheduled {
		if _, err := time.Parse("20060102T150405Z", entry.Name()); err != nil {
			continue
		}
		path := "backups/scheduled/" + entry.Name()
		var v struct {
			Verified bool `json:"verified_readback"`
		}
		if err := cleanupReadJSON(root, path+"/verified.json", &v); err != nil || !v.Verified {
			continue
		}
		verified = append(verified, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(verified)))
	for i, name := range verified {
		path := "backups/scheduled/" + name
		if i < o.LocalCopies {
			protected[path] = true
		}
		at, _ := time.Parse("20060102T150405Z", name)
		if err := add(path, "scheduled_backup", at); err != nil {
			return err
		}
	}
	// Report unregistered directories, but never infer ownership from a name.
	for _, dir := range []string{"backups", "backups/scheduled", "restore-drills", "failed", "."} {
		entries, err := fs.ReadDir(root.FS(), dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if dir == "." && !strings.HasPrefix(entry.Name(), "data.pre-restore-") {
				continue
			}
			path := filepath.ToSlash(filepath.Join(dir, entry.Name()))
			if known[path] || path == "backups/scheduled" {
				continue
			}
			r.Items = append(r.Items, CleanupItem{Path: path, Kind: "unregistered", Reason: "ownership_unproven", Action: "keep", protected: true})
		}
	}
	sort.SliceStable(r.Items, func(i, j int) bool {
		if r.Items[i].at.Equal(r.Items[j].at) {
			return r.Items[i].Path < r.Items[j].Path
		}
		return r.Items[i].at.Before(r.Items[j].at)
	})
	r.RemainingAllocatedBytes = r.ManagedAllocatedBytes
	for i := range r.Items {
		x := &r.Items[i]
		if x.protected {
			continue
		}
		age := r.CreatedAt.Sub(x.at)
		if age < o.MinAge {
			x.Reason = "minimum_age"
			continue
		}
		switch {
		case x.Kind == "release_backup" || x.Kind == "scheduled_backup":
			x.Reason = "copy_limit"
		case age >= o.MaxAge:
			x.Reason = "expired"
		default:
			continue
		}
		x.Action = "remove"
		r.RemainingAllocatedBytes -= x.AllocatedBytes
	}
	for i := range r.Items {
		x := &r.Items[i]
		if r.RemainingAllocatedBytes <= o.MaxBytes {
			break
		}
		if x.protected || x.Action == "remove" || r.CreatedAt.Sub(x.at) < o.MinAge {
			continue
		}
		x.Action, x.Reason = "remove", "byte_budget"
		r.RemainingAllocatedBytes -= x.AllocatedBytes
	}
	r.OverBudget = r.RemainingAllocatedBytes > o.MaxBytes
	return nil
}

func cleanupOperationID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func plainCleanupPath(root *os.Root, path string) error {
	if !filepath.IsLocal(path) || path == "." {
		return fmt.Errorf("invalid cleanup path")
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("cleanup path contains a link or non-directory: %s", path)
		}
	}
	return nil
}

func cleanupReadJSON(root *os.Root, path string, out any) error {
	return cleanupReadJSONLimit(root, path, out, 65536)
}

func cleanupReadJSONLimit(root *os.Root, path string, out any, maxBytes int64) error {
	if err := plainCleanupPath(root, path); err != nil {
		return err
	}
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return fmt.Errorf("invalid cleanup metadata")
	}
	b, err := root.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func measureCleanup(ctx context.Context, root *os.Root, path string) (int64, string, error) {
	if err := plainCleanupPath(root, path); err != nil {
		return 0, "", err
	}
	top, err := root.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !top.IsDir() {
		return 0, "", fmt.Errorf("copy must be a directory")
	}
	service, err := root.Stat(".")
	if err != nil {
		return 0, "", err
	}
	dev := service.Sys().(*syscall.Stat_t).Dev
	h := sha256.New()
	var bytes int64
	count := 0
	err = fs.WalkDir(root.FS(), path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 100000 {
			return fmt.Errorf("copy exceeds cleanup scan limit")
		}
		info, err := root.Lstat(p)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() || st.Dev != dev || !info.IsDir() && st.Nlink > 1 {
			return fmt.Errorf("copy contains a link, mount, or special file: %s", p)
		}
		bytes += st.Blocks * 512
		fmt.Fprintf(h, "%s:%d:%d:%d:%d:%d\n", p, st.Dev, st.Ino, info.Size(), info.ModTime().UnixNano(), st.Blocks)
		return nil
	})
	return bytes, hex.EncodeToString(h.Sum(nil)), err
}

func removeCleanupItem(ctx context.Context, root *os.Root, item *CleanupItem) error {
	if !cleanupCopyPath(item.Path, item.Kind) {
		return fmt.Errorf("unmanaged cleanup path: %s", item.Path)
	}
	_, fp, err := measureCleanup(ctx, root, item.Path)
	if err != nil {
		return err
	}
	if fp != item.Fingerprint {
		return fmt.Errorf("copy changed after planning: %s", item.Path)
	}
	return root.RemoveAll(item.Path)
}

func cleanupCopyPath(path, kind string) bool {
	if filepath.ToSlash(filepath.Clean(path)) != path {
		return false
	}
	parts := strings.Split(path, "/")
	switch kind {
	case "release_backup", "terminal_temporary":
		return len(parts) == 2 && (parts[0] == "backups" || parts[0] == "restore-drills") && cleanupOperationID(parts[1])
	case "scheduled_backup":
		if len(parts) != 3 || parts[0] != "backups" || parts[1] != "scheduled" {
			return false
		}
		_, err := time.Parse("20060102T150405Z", parts[2])
		return err == nil
	case "compensation":
		return len(parts) == 1 && maintenance.ValidDisplacedPath("data", path) || len(parts) == 2 && parts[0] == "failed" && strings.HasSuffix(parts[1], "-data") && cleanupOperationID(strings.TrimSuffix(parts[1], "-data"))
	}
	return false
}

func cleanupFreeBytes(path string) (int64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	blockSize := int64(s.Bsize)
	if blockSize <= 0 || s.Bavail > math.MaxInt64 {
		return 0, fmt.Errorf("filesystem free-space counter exceeds supported range")
	}
	blocks := int64(s.Bavail)
	if blocks > math.MaxInt64/blockSize {
		return 0, fmt.Errorf("filesystem free-space counter exceeds supported range")
	}
	return blocks * blockSize, nil
}

// Persist intent and progress before relying on them after a process or host
// failure. The enclosing receiver lock serializes updates to this journal.
func saveCleanupReceipt(root *os.Root, report *CleanupReport) error {
	path := "receiver-state/cleanup/" + report.ID + ".json"
	tmp := path + "." + uuid.NewString() + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(report); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = root.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := root.Open("receiver-state/cleanup")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func trimCleanupReceipts(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), "receiver-state/cleanup")
	if err != nil {
		return err
	}
	var done []CleanupReport
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		// Receipts include every copy, unlike the small operation metadata.
		// Decode only the header so large item lists do not stay in memory.
		var header struct {
			Schema    string    `json:"schema"`
			ID        string    `json:"id"`
			Status    string    `json:"status"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := cleanupReadJSONLimit(root, "receiver-state/cleanup/"+entry.Name(), &header, 32<<20); err != nil {
			return err
		}
		if _, err := uuid.Parse(header.ID); err != nil || entry.Name() != header.ID+".json" || header.Schema != "ferry/local-cleanup/v1" || header.CreatedAt.IsZero() {
			return fmt.Errorf("invalid cleanup receipt")
		}
		if header.Status == "completed" {
			done = append(done, CleanupReport{ID: header.ID, CreatedAt: header.CreatedAt})
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].CreatedAt.After(done[j].CreatedAt) })
	// Leave room for the current receipt, which is still applying.
	for _, r := range done[min(63, len(done)):] {
		if err := root.Remove("receiver-state/cleanup/" + r.ID + ".json"); err != nil {
			return err
		}
	}
	return nil
}
