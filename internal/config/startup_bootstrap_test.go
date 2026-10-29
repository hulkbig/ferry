package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseStartupBootstrapMatchesLegacyDefaultsAndTemplates(t *testing.T) {
	raw := []byte("server: {}\nplans:\n  - id: subscriber\n    template: claude-pro\n")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := ParseStartupBootstrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy, bootstrap) {
		t.Fatal("bootstrap changed legacy defaults or expansion")
	}
}

func TestParseStartupBootstrapRejectsInvalidMaterialWithoutEcho(t *testing.T) {
	for _, raw := range []string{
		"", "server: [private-fixture", "unknown: private-fixture\n",
		"server: {}\n---\nserver: {}\n",
		"plans:\n  - id: example\n    template: private-fixture\n",
	} {
		if _, err := ParseStartupBootstrap([]byte(raw)); err == nil {
			t.Fatal("invalid bootstrap accepted")
		} else if strings.Contains(err.Error(), "private-fixture") {
			t.Fatal("bootstrap error echoed input")
		}
	}
}
