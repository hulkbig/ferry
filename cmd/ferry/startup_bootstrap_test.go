package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"ferry/internal/secretstore"
)

func startupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func startupWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startupFixtureValue(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("fixture-%s", strings.ReplaceAll(t.Name(), "/", "-"))
}

func TestStartupBootstrapMissingMaterials(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "absent"), startupTestDir(t)} {
		b, err := openStartupBootstrap(path)
		if err != nil {
			t.Fatal(err)
		}
		if b.config != nil || b.key != "" || len(b.files) != 0 {
			t.Fatal("missing staging must not synthesize bootstrap material")
		}
		if err := b.cleanup(""); err != nil {
			t.Fatal(err)
		}
		b.close()
	}
}

func TestStartupBootstrapRejectsUnsafeMaterial(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{"directory permissions", func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"file permissions", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "config.yaml")
			startupWrite(t, path, "server: {}\n")
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"file symlink", func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "outside")
			startupWrite(t, target, "server: {}\n")
			if err := os.Symlink(target, filepath.Join(dir, "config.yaml")); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink", func(t *testing.T, dir string) {
			if err := os.Symlink("absent", filepath.Join(dir, "upstream-key")); err != nil {
				t.Fatal(err)
			}
		}},
		{"nonregular file", func(t *testing.T, dir string) {
			if err := unix.Mkfifo(filepath.Join(dir, "upstream-key"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized config", func(t *testing.T, dir string) {
			startupWrite(t, filepath.Join(dir, "config.yaml"), strings.Repeat("x", startupConfigLimit+1))
		}},
		{"oversized key", func(t *testing.T, dir string) {
			startupWrite(t, filepath.Join(dir, "upstream-key"), strings.Repeat("x", startupKeyLimit+1))
		}},
		{"empty key", func(t *testing.T, dir string) {
			startupWrite(t, filepath.Join(dir, "upstream-key"), " \n")
		}},
		{"multiline key", func(t *testing.T, dir string) {
			startupWrite(t, filepath.Join(dir, "upstream-key"), startupFixtureValue(t)+"\nsecond-line")
		}},
		{"malformed config", func(t *testing.T, dir string) {
			startupWrite(t, filepath.Join(dir, "config.yaml"), "server: ["+startupFixtureValue(t))
		}},
		{"invalid timezone", func(t *testing.T, dir string) {
			startupWrite(t, filepath.Join(dir, "config.yaml"), "server:\n  timezone: "+startupFixtureValue(t)+"\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := startupTestDir(t)
			tc.prepare(t, dir)
			b, err := openStartupBootstrap(dir)
			if b != nil {
				b.close()
			}
			if err == nil {
				t.Fatal("unsafe staging accepted")
			}
			if strings.Contains(err.Error(), startupFixtureValue(t)) {
				t.Fatal("bootstrap error disclosed material")
			}
		})
	}
	t.Run("directory symlink", func(t *testing.T) {
		target := startupTestDir(t)
		link := filepath.Join(t.TempDir(), "staged")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		b, err := openStartupBootstrap(link)
		if b != nil {
			b.close()
		}
		if err == nil {
			t.Fatal("directory symlink accepted")
		}
	})
	t.Run("directory is file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "staged")
		startupWrite(t, path, "invalid")
		if _, err := openStartupBootstrap(path); err == nil {
			t.Fatal("file accepted as directory")
		}
	})
}

func TestStartupBootstrapCleanup(t *testing.T) {
	dir := startupTestDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	keyPath := filepath.Join(dir, "upstream-key")
	otherPath := filepath.Join(dir, "operator-notes")
	startupWrite(t, configPath, "server: {}\n")
	startupWrite(t, keyPath, "\n"+startupFixtureValue(t)+"\n")
	startupWrite(t, otherPath, "preserve")
	b, err := openStartupBootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	if b.key != startupFixtureValue(t) {
		t.Fatal("key newline was not trimmed")
	}
	// Explicit configuration stays operator-owned, even inside the staging dir.
	if err := b.cleanup(configPath); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{configPath, otherPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("operator file removed", err)
		}
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("consumed key remains")
	}
	if err := b.cleanup(configPath); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
}

func TestStartupBootstrapCleanupPreservesChangedFiles(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			dir := startupTestDir(t)
			path := filepath.Join(dir, "upstream-key")
			value := startupFixtureValue(t)
			startupWrite(t, path, value)
			b, err := openStartupBootstrap(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer b.close()
			if replacement {
				other := filepath.Join(dir, "replacement")
				startupWrite(t, other, value)
				if err := os.Rename(other, path); err != nil {
					t.Fatal(err)
				}
			} else {
				startupWrite(t, path, value+"-changed")
			}
			if err := b.cleanup(""); err == nil {
				t.Fatal("changed material removed")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("changed material not preserved", err)
			}
		})
	}
}

func startupSecretDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	// URI punctuation must not change the selected database or its mode.
	path := filepath.Join(t.TempDir(), "control?#.db")
	// The existing store API uses an unescaped DSN, so initialize the fixture
	// through a direct connection; production creates the table before seeding.
	db := startupOpenDB(t, path)
	if _, err := db.Exec(`CREATE TABLE local_secrets(name TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestStartupBootstrapSeedNeverRotates(t *testing.T) {
	for _, state := range []string{"absent", "empty", "same", "different"} {
		t.Run(state, func(t *testing.T) {
			path, db := startupSecretDB(t)
			value := startupFixtureValue(t)
			initial := value
			if state == "empty" {
				initial = ""
			}
			if state == "different" {
				initial += "-original"
			}
			if state != "absent" {
				if _, err := db.Exec(`INSERT INTO local_secrets VALUES('new_api',?,17)`, initial); err != nil {
					t.Fatal(err)
				}
			}
			b := &startupBootstrap{key: value}
			err := b.seed(path)
			if (err != nil) != (state == "different") {
				t.Fatalf("unexpected seeding result: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), value) || strings.Contains(err.Error(), initial)) {
				t.Fatal("error leaked material")
			}
			var actual string
			var updated int64
			if err := db.QueryRow(`SELECT value,updated_at FROM local_secrets WHERE name='new_api'`).Scan(&actual, &updated); err != nil {
				t.Fatal(err)
			}
			want := value
			if state == "different" {
				want = initial
			}
			if actual != want {
				t.Fatal("unexpected persisted secret")
			}
			if (state == "same" || state == "different") && updated != 17 {
				t.Fatal("existing secret was rewritten")
			}
		})
	}
}

func TestStartupBootstrapConcurrentSeeding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	store, err := secretstore.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	values := []string{startupFixtureValue(t), startupFixtureValue(t) + "-other"}
	results := make([]error, len(values))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, value := range values {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = (&startupBootstrap{key: value}).seed(path)
		}()
	}
	close(start)
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) {
		t.Fatal("exactly one conflicting initializer must succeed")
	}
	actual, ok := store.Get("new_api")
	if !ok {
		t.Fatal("seed missing")
	}
	for i, err := range results {
		if err == nil && actual != values[i] {
			t.Fatal("winning initialization was rotated")
		}
	}
}
