// Package secretstore persists locally-provisioned secret values (F-P0-25):
// upstream API keys entered through the console land here instead of shell
// environment variables. Values exist only in memory and in a 0600 JSON file
// under the data dir; they are never logged, never embedded in published
// configs, and never reach the ledger (which stores CredentialRef text only).
package secretstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Store is a concurrency-safe named-string secret store backed by a JSON
// file with 0600 permissions.
type Store struct {
	mu     sync.Mutex
	path   string
	values map[string]string
	db     *sql.DB
}

// Open loads the store file at path, starting empty when it does not exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path, values: map[string]string{}}
	b, err := os.ReadFile(path) // #nosec G304 -- path is the data-dir secret store file fixed by the operator at startup.
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read secret store: %w", err)
		}
		return s, nil // created lazily on first Set
	}
	if err := json.Unmarshal(b, &s.values); err != nil {
		return nil, fmt.Errorf("parse secret store %s: %w", path, err)
	}
	return s, nil
}

// OpenSQLite opens the production secret store. It can share a SQLite file
// with other control-plane tables; values never leave the explicit Get path.
func OpenSQLite(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create secret store dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous=FULL&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite secret store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite secret store: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure sqlite secret store: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS local_secrets (
		name TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at INTEGER NOT NULL DEFAULT (unixepoch()*1000)
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite secret store: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Get returns the value stored under name.
func (s *Store) Get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		var value string
		if err := s.db.QueryRow(`SELECT value FROM local_secrets WHERE name=?`, name).Scan(&value); err != nil {
			return "", false
		}
		return value, true
	}
	v, ok := s.values[name]
	return v, ok
}

// Set stores value under name and persists immediately (write temp + rename,
// 0600). On a persistence failure the in-memory change is rolled back so
// memory and disk never diverge.
func (s *Store) Set(name, value string) error {
	if name == "" {
		return fmt.Errorf("secret name must be non-empty")
	}
	if value == "" {
		return fmt.Errorf("secret value must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_, err := s.db.Exec(`INSERT INTO local_secrets(name,value,updated_at) VALUES(?,?,unixepoch()*1000)
			ON CONFLICT(name) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, name, value)
		return err
	}
	prev, had := s.values[name]
	s.values[name] = value
	if err := s.persistLocked(); err != nil {
		if had {
			s.values[name] = prev
		} else {
			delete(s.values, name)
		}
		return fmt.Errorf("persist secret store: %w", err)
	}
	return nil
}

// Delete removes name and persists; deleting an unknown name is a no-op.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_, err := s.db.Exec(`DELETE FROM local_secrets WHERE name=?`, name)
		return err
	}
	prev, had := s.values[name]
	if !had {
		return nil
	}
	delete(s.values, name)
	if err := s.persistLocked(); err != nil {
		s.values[name] = prev
		return fmt.Errorf("persist secret store: %w", err)
	}
	return nil
}

// Names lists stored names in sorted order — never values.
func (s *Store) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		rows, err := s.db.Query(`SELECT name FROM local_secrets ORDER BY name`)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				out = append(out, name)
			}
		}
		return out
	}
	out := make([]string, 0, len(s.values))
	for n := range s.values {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// EntriesWithPrefix atomically reads a reserved namespace and reports storage
// errors. Money-affecting journals must not mistake a failed read for no history.
func (s *Store) EntriesWithPrefix(prefix string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	if s.db == nil {
		for name, value := range s.values {
			if strings.HasPrefix(name, prefix) {
				out[name] = value
			}
		}
		return out, nil
	}
	rows, err := s.db.Query(`SELECT name,value FROM local_secrets WHERE substr(name,1,length(?))=?`, prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, rows.Err()
}

func (s *Store) persistLocked() error {
	b, err := json.Marshal(s.values)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path)
}

// SetIfAbsent is a create-only persistence boundary for imported credentials.
// A conflict never overwrites another account, including after partial failures.
func (s *Store) SetIfAbsent(name, value string) error {
	if name == "" || value == "" {
		return fmt.Errorf("secret name and value must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		result, err := s.db.Exec(`INSERT INTO local_secrets(name,value,updated_at) VALUES(?,?,unixepoch()*1000) ON CONFLICT(name) DO NOTHING`, name, value)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("credential already exists")
		}
		return nil
	}
	if _, exists := s.values[name]; exists {
		return fmt.Errorf("credential already exists")
	}
	s.values[name] = value
	if err := s.persistLocked(); err != nil {
		delete(s.values, name)
		return err
	}
	return nil
}
