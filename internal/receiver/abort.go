package receiver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// AbortPrepared is an operator-only reconciliation for a canceled Actions run
// that never reached deployment. It never runs Compose, opens SQLite, removes
// backups, or becomes a public forced-command verb. The stopped receiver must
// have released its lock, and the previous release must still be serving.
func (e *Environment) AbortPrepared(ctx context.Context, releaseSHA, operationID string) error {
	if !regexp40Hex.MatchString(releaseSHA) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(operationID) {
		return fmt.Errorf("invalid abort identity")
	}
	lock, err := os.OpenInRoot(filepath.Join(e.ServiceDir, "receiver-state"), "receiver.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("receiver is still active: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	request := Request{ReleaseSHA: releaseSHA, OperationID: operationID}
	state, err := e.loadOperation()
	if os.IsNotExist(err) {
		var archived operationState
		if err := readJSON(filepath.Join(e.ServiceDir, "receiver-state", "history", operationID+".json"), &archived); err != nil {
			return err
		}
		if archived.matches(request) != nil || archived.Phase != "aborted-before-deploy" {
			return fmt.Errorf("no matching aborted operation")
		}
		return json.NewEncoder(e.Stdout).Encode(archived.result(true))
	}
	if err != nil {
		return err
	}
	if state.matches(request) != nil || state.Phase != "prepared" || state.BackupDir != "" || !state.HadPrevious {
		return fmt.Errorf("abort requires the exact prepared operation with no recorded backup and an unchanged previous release")
	}
	release, err := e.loadRelease()
	if err != nil || release.ReleaseSHA != state.PreviousSHA {
		return fmt.Errorf("previous release record changed")
	}
	current, err := os.Readlink(filepath.Join(e.ServiceDir, "current"))
	if err != nil || current != filepath.Join(e.ServiceDir, "releases", state.PreviousSHA) {
		return fmt.Errorf("current release pointer changed")
	}
	if err := e.waitForServingBoundaries(ctx, state.PreviousSHA); err != nil {
		return fmt.Errorf("previous release is not healthy: %w", err)
	}
	state.Phase = "aborted-before-deploy"
	state.LastTransition = time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.archiveOperation(state); err != nil {
		return err
	}
	return json.NewEncoder(e.Stdout).Encode(state.result(false))
}
