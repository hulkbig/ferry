package controlstore

import (
	"path/filepath"
	"testing"
)

func TestRuntimeSettingsRoundtripAndDefaults(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, found, err := store.RuntimeSetting("scheduled_model_sync_enabled"); err != nil || found {
		t.Fatalf("missing setting found=%v err=%v", found, err)
	}
	if err := store.SetRuntimeSetting("scheduled_model_sync_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	value, found, err := store.RuntimeSetting("scheduled_model_sync_enabled")
	if err != nil || !found || value != "false" {
		t.Fatalf("value=%q found=%v err=%v", value, found, err)
	}
	if err := store.SetRuntimeSetting("scheduled_model_sync_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	value, _, err = store.RuntimeSetting("scheduled_model_sync_enabled")
	if err != nil || value != "true" {
		t.Fatalf("updated value=%q err=%v", value, err)
	}
	if err := store.SetRuntimeSetting("", "x"); err == nil {
		t.Fatal("empty key must be rejected")
	}
}
