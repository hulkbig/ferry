package main

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"ferry/internal/config"
	"ferry/internal/policy"
)

const (
	startupConfigLimit = 4 << 20
	startupKeyLimit    = 64 << 10
)

type startupBootstrapFile struct {
	name string
	info os.FileInfo
	hash [32]byte
}

type startupBootstrap struct {
	dir    *os.File
	fd     int
	config *config.File
	key    string
	files  []startupBootstrapFile
}

// openStartupBootstrap accepts only the receiver's private staging directory.
// A missing directory is normal on upgrades and after first-run cleanup.
func openStartupBootstrap(path string) (*startupBootstrap, error) {
	b := &startupBootstrap{}
	if path == "" {
		return b, nil
	}
	fd, err := unix.Open(filepath.Clean(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return b, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open bootstrap directory: %w", err)
	}
	if fd < 0 {
		return nil, fmt.Errorf("invalid bootstrap directory descriptor")
	}
	b.dir = os.NewFile(uintptr(fd), path)
	b.fd = fd
	ok := false
	defer func() {
		if !ok {
			b.close()
		}
	}()
	info, err := b.dir.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect bootstrap directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !startupOwnedByProcess(info) {
		return nil, fmt.Errorf("bootstrap directory must be owned by the application user with mode 0700")
	}
	for _, item := range []struct {
		name  string
		limit int64
	}{{"config.yaml", startupConfigLimit}, {"upstream-key", startupKeyLimit}} {
		raw, staged, err := b.read(item.name, item.limit)
		if err != nil {
			return nil, err
		}
		if staged == nil {
			continue
		}
		b.files = append(b.files, *staged)
		if item.name == "config.yaml" {
			b.config, err = config.ParseStartupBootstrap(raw)
			if err != nil {
				return nil, err
			}
			if _, err := time.LoadLocation(b.config.Server.Timezone); err != nil {
				return nil, fmt.Errorf("invalid bootstrap timezone")
			}
			if _, err := policy.NewCompiler().Compile(b.config); err != nil {
				return nil, fmt.Errorf("invalid bootstrap policy configuration")
			}
		} else {
			b.key = strings.TrimSpace(string(raw))
			if b.key == "" || !utf8.ValidString(b.key) || strings.IndexFunc(b.key, func(r rune) bool {
				return unicode.IsControl(r) || unicode.IsSpace(r)
			}) >= 0 {
				return nil, fmt.Errorf("invalid bootstrap upstream-key")
			}
		}
	}
	ok = true
	return b, nil
}

func startupOwnedByProcess(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Geteuid())
}

func (b *startupBootstrap) read(name string, limit int64) ([]byte, *startupBootstrapFile, error) {
	// The directory descriptor pins the receiver-staged directory even if its
	// pathname changes. NOFOLLOW rejects links; NONBLOCK avoids hanging on a FIFO.
	fd, err := unix.Openat(b.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open bootstrap %s: %w", name, err)
	}
	if fd < 0 {
		return nil, nil, fmt.Errorf("invalid bootstrap file descriptor")
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect bootstrap %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !startupOwnedByProcess(info) {
		return nil, nil, fmt.Errorf("bootstrap %s must be an application-owned regular file with mode 0600", name)
	}
	if info.Size() > limit {
		return nil, nil, fmt.Errorf("bootstrap %s exceeds its size limit", name)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read bootstrap %s: %w", name, err)
	}
	if int64(len(raw)) > limit {
		return nil, nil, fmt.Errorf("bootstrap %s exceeds its size limit", name)
	}
	return raw, &startupBootstrapFile{name: name, info: info, hash: sha256.Sum256(raw)}, nil
}

// seed runs after secretstore.OpenSQLite created its schema, before any serving
// or background work. The SQL condition prevents concurrent bootstraps from
// rotating an already provisioned secret. Same-value retries do not update it.
func (b *startupBootstrap) seed(controlPath string) error {
	if b.key == "" {
		return nil
	}
	path, err := filepath.Abs(controlPath)
	if err != nil {
		return fmt.Errorf("resolve bootstrap secret store: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open bootstrap secret store: %w", err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin bootstrap secret initialization: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO local_secrets(name,value,updated_at) VALUES('new_api',?,unixepoch()*1000)
		ON CONFLICT(name) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at
		WHERE local_secrets.value=''`, b.key); err != nil {
		return fmt.Errorf("persist bootstrap new_api secret: %w", err)
	}
	var persisted string
	if err := tx.QueryRow(`SELECT value FROM local_secrets WHERE name='new_api'`).Scan(&persisted); err != nil {
		return fmt.Errorf("verify bootstrap new_api secret: %w", err)
	}
	if persisted != b.key {
		return fmt.Errorf("bootstrap new_api conflicts with an existing secret; refusing to rotate it")
	}
	return tx.Commit()
}

// cleanup removes only the exact staged files validated by this process, after
// all startup persistence and runtime validation succeeded. An explicit -config
// always remains operator-owned, even when it names a staged file.
func (b *startupBootstrap) cleanup(explicitConfig string) error {
	if b.dir == nil {
		return nil
	}
	var explicitInfo os.FileInfo
	if explicitConfig != "" {
		explicitInfo, _ = os.Stat(explicitConfig)
	}
	for _, staged := range b.files {
		if explicitInfo != nil && os.SameFile(explicitInfo, staged.info) {
			continue
		}
		limit := int64(startupConfigLimit)
		if staged.name == "upstream-key" {
			limit = startupKeyLimit
		}
		_, current, err := b.read(staged.name, limit)
		if err != nil {
			return err
		}
		if current == nil {
			continue // A successful concurrent startup may already have removed it.
		}
		if !os.SameFile(staged.info, current.info) || current.hash != staged.hash {
			return fmt.Errorf("bootstrap %s changed during startup; preserving it", staged.name)
		}
		if err := unix.Unlinkat(b.fd, staged.name, 0); err != nil && err != unix.ENOENT {
			return fmt.Errorf("remove consumed bootstrap %s: %w", staged.name, err)
		}
	}
	if err := b.dir.Sync(); err != nil {
		return fmt.Errorf("persist bootstrap cleanup: %w", err)
	}
	return nil
}

func (b *startupBootstrap) close() {
	if b.dir != nil {
		_ = b.dir.Close()
	}
}
