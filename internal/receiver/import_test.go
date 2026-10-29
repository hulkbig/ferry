package receiver

import (
	"os"
	"path/filepath"
	"testing"

	"ferry/internal/maintenance"
)

func TestInitialImportIsVerifiedAndNeverOverwritesTarget(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("server:\n  management_addr: 127.0.0.1:7900\n  proxy_addr: 127.0.0.1:7890\n  timezone: UTC\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source, backup := filepath.Join(root, "source"), filepath.Join(root, "backup")
	if _, err := maintenance.Migrate(source, config); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Backup(source, backup); err != nil {
		t.Fatal(err)
	}
	data, receipt := filepath.Join(root, "target"), filepath.Join(root, "receipt")
	for range 2 {
		ok, err := restoreInitialCopy(backup, data, receipt)
		if err != nil || !ok {
			t.Fatalf("verified import/retry: %v", err)
		}
	}
	before, err := os.ReadFile(filepath.Join(data, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Removing the receipt simulates a crash between atomic restore and receipt.
	if err := os.Remove(receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreInitialCopy(backup, data, receipt); err == nil {
		t.Fatal("unreceipted existing target was overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(data, "control.db"))
	if string(before) != string(after) {
		t.Fatal("rejected import altered target")
	}
	if err := os.WriteFile(filepath.Join(backup, "control.db"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreInitialCopy(backup, filepath.Join(root, "other"), receipt); err == nil {
		t.Fatal("corrupt snapshot was imported")
	}
	if _, err := os.Stat(filepath.Join(root, "other")); !os.IsNotExist(err) {
		t.Fatal("failed verification created target")
	}
}

func TestReceiverUnboundRepositoryRejectsCommands(t *testing.T) {
	previous := RepositoryIDValue
	RepositoryIDValue = "0"
	defer func() { RepositoryIDValue = previous }()
	if _, err := ParseOriginalCommand("ferry/v1 0 " + testSHA + " " + testOp + " status"); err == nil {
		t.Fatal("unbound receiver accepted a release")
	}
}
