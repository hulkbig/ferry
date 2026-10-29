package receiver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStartupMaterialsArePrivateIdempotentAndNeverRotated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".startup-bootstrap")
	materials := map[string][]byte{"config.yaml": []byte("version: 2\n"), "upstream-key": []byte("test-value")}
	for range 2 {
		if err := writeStartupBootstrap(dir, materials, os.Getuid(), os.Getgid()); err != nil {
			t.Fatal(err)
		}
	}
	for name, expected := range materials {
		path := filepath.Join(dir, name)
		actual, err := os.ReadFile(path)
		if err != nil || string(actual) != string(expected) {
			t.Fatalf("material %s changed", name)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("material %s is not private", name)
		}
	}
	if err := writeStartupBootstrap(dir, map[string][]byte{"upstream-key": []byte("different-value")}, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("staging rotated existing material")
	}
}

func TestStartupMaterialsRejectSymlinkAndPublicDirectory(t *testing.T) {
	for _, scenario := range []string{"directory-link", "file-link", "public-directory", "public-file"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "stage")
			outside := filepath.Join(root, "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "config.yaml"), []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario == "directory-link" {
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "file-link":
					if err := os.Symlink(filepath.Join(outside, "config.yaml"), filepath.Join(dir, "config.yaml")); err != nil {
						t.Fatal(err)
					}
				case "public-directory":
					if err := os.Chmod(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				case "public-file":
					if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("untouched"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writeStartupBootstrap(dir, map[string][]byte{"config.yaml": []byte("untouched")}, os.Getuid(), os.Getgid()); err == nil {
				t.Fatal("unsafe staging succeeded")
			}
			actual, err := os.ReadFile(filepath.Join(outside, "config.yaml"))
			if err != nil || string(actual) != "untouched" {
				t.Fatal("outside file changed")
			}
		})
	}
}
