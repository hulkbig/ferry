package secretstore

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOpenMissingFileStartsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatalf("open missing store: %v", err)
	}
	if names := s.Names(); len(names) != 0 {
		t.Fatalf("expected empty store, got %v", names)
	}
}

func TestSetPersistReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("GLM_API_KEY", "sk-abc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Set("deepseek", "ds-xyz"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// file must exist with 0600 permissions and never in world/group-readable form
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store file perms = %o, want 600", perm)
	}

	// a fresh handle sees the persisted values
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := s2.Get("GLM_API_KEY"); !ok || v != "sk-abc" {
		t.Fatalf("reload GLM_API_KEY = (%q,%v)", v, ok)
	}
	got := s2.Names()
	if len(got) != 2 || got[0] != "GLM_API_KEY" || got[1] != "deepseek" {
		t.Fatalf("Names() = %v, want sorted [GLM_API_KEY deepseek]", got)
	}
}

func TestSetValidationAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, _ := Open(path)

	if err := s.Set("", "v"); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := s.Set("k", ""); err == nil {
		t.Fatal("empty value accepted")
	}
	if names := s.Names(); len(names) != 0 {
		t.Fatalf("rejected writes leaked into memory: %v", names)
	}

	// persistence failure (unwritable dir) rolls back memory so disk/memory
	// can never diverge
	bad := &Store{path: filepath.Join(t.TempDir(), "nope", "dir", "s.json"),
		values: map[string]string{}}
	if err := bad.Set("k", "v"); err == nil {
		t.Fatal("persist failure not reported")
	}
	if _, ok := bad.Get("k"); ok {
		t.Fatal("failed Set left value in memory")
	}
	prev := map[string]string{"k": "old"}
	bad2 := &Store{path: filepath.Join(t.TempDir(), "nope", "dir", "s.json"), values: prev}
	if err := bad2.Set("k", "new"); err == nil {
		t.Fatal("persist failure not reported")
	}
	if v, _ := bad2.Get("k"); v != "old" {
		t.Fatalf("overwrite rollback lost previous value: %q", v)
	}
}

func TestDeleteNoopAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	s, _ := Open(path)
	_ = s.Set("k", "v")

	if err := s.Delete("absent"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("value survived delete")
	}
	s2, _ := Open(path)
	if _, ok := s2.Get("k"); ok {
		t.Fatal("delete not persisted")
	}

	// failed delete restores the value
	bad := &Store{path: filepath.Join(t.TempDir(), "nope", "dir", "s.json"),
		values: map[string]string{"k": "v"}}
	if err := bad.Delete("k"); err == nil {
		t.Fatal("persist failure not reported")
	}
	if v, ok := bad.Get("k"); !ok || v != "v" {
		t.Fatal("failed Delete removed value from memory")
	}
}

func TestConcurrency(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "secrets.json"))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i%8))
			_ = s.Set(name, "v")
			_, _ = s.Get(name)
			_ = s.Names()
			if i%4 == 0 {
				_ = s.Delete(name)
			}
		}(i)
	}
	wg.Wait()
}

func TestSQLiteStorePersistsSecretsWithoutJSONSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("GLM_API_KEY", "sk-sqlite"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, ok := s2.Get("GLM_API_KEY"); !ok || got != "sk-sqlite" {
		t.Fatalf("reloaded secret = (%q,%v)", got, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected JSON secret sidecar: %v", err)
	}
}

func TestEntriesWithPrefixReportsUnavailableStorage(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, value := range map[string]string{"journal:a": "a", "journal:b": "b", "unrelated": "private"} {
		if err := s.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.EntriesWithPrefix("journal:")
	if err != nil || len(entries) != 2 || entries["journal:a"] != "a" || entries["journal:b"] != "b" {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EntriesWithPrefix("journal:"); err == nil {
		t.Fatal("failed database read appeared as an empty journal")
	}
}
