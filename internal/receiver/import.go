package receiver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"ferry/internal/maintenance"
)

// restoreInitialCopy consumes only the administrator-staged, verified snapshot.
// It never replaces an initialized target and never opens a source service DB.
// Schema upgrades still belong to the application startup after this copy.
func restoreInitialCopy(source, data, receipt string) (bool, error) {
	manifest := filepath.Join(source, "manifest.json")
	raw, err := os.ReadFile(manifest)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, name := range backupPayloadNames {
		info, err := os.Lstat(filepath.Join(source, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return false, fmt.Errorf("import snapshot must contain private regular files")
		}
	}
	if _, err := maintenance.Verify(source); err != nil {
		return false, err
	}
	sum := sha256.Sum256(raw)
	identity := []byte(hex.EncodeToString(sum[:]) + "\n")
	if previous, err := os.ReadFile(receipt); err == nil {
		if !bytes.Equal(previous, identity) {
			return false, fmt.Errorf("import snapshot differs from the recorded copy")
		}
		if _, err := os.Stat(filepath.Join(data, "control.db")); err != nil {
			return false, fmt.Errorf("recorded import has no target database")
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	entries, err := os.ReadDir(data)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if len(entries) != 0 {
		return false, fmt.Errorf("initial import refuses an existing data directory; reconcile before retrying")
	}
	if err := maintenance.Restore(source, data); err != nil {
		return false, err
	}
	// An interruption before this receipt fails closed on the next attempt.
	// It must not silently replace a target that may have started serving.
	if err := os.WriteFile(receipt, identity, 0600); err != nil {
		return false, err
	}
	return true, nil
}
