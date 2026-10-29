package receiver

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func abortFixture(t *testing.T) (*Environment, *operationState, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	previous := strings.Repeat("b", 40)
	for _, name := range []string{"receiver-state", "data", "backups/partial", "releases/" + previous} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"receiver-state/receiver.lock", "data/control.db", "backups/partial/evidence"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "releases", previous), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(root, "release.json"), releaseState{Schema: "ferry/release/v1", ReleaseSHA: previous}, 0o600); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	env := &Environment{ServiceDir: root, Stdout: output, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, "{}"
		switch r.URL.Path {
		case "/version":
			body = `{"commit":"` + previous + `"}`
		case "/v1/models":
			status = http.StatusUnauthorized
		case "/api/overview":
			status = http.StatusPreconditionRequired
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}, run: func(context.Context, io.Reader, string, ...string) ([]byte, error) {
		t.Fatal("abort must not run commands")
		return nil, nil
	}}
	state := &operationState{Schema: "ferry/receiver-operation/v1", OperationID: testOp, ReleaseSHA: testSHA, Phase: "prepared", HadPrevious: true, PreviousSHA: previous}
	if err := env.saveOperation(state); err != nil {
		t.Fatal(err)
	}
	return env, state, output
}

func TestAbortPreparedPreservesRuntimeAndBackupEvidence(t *testing.T) {
	env, state, output := abortFixture(t)
	for range 2 {
		if err := env.AbortPrepared(context.Background(), testSHA, testOp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.loadOperation(); !os.IsNotExist(err) {
		t.Fatalf("active operation remains: %v", err)
	}
	for _, name := range []string{"data/control.db", "backups/partial/evidence"} {
		raw, err := os.ReadFile(filepath.Join(env.ServiceDir, name))
		if err != nil || string(raw) != "unchanged" {
			t.Fatalf("abort changed %s", name)
		}
	}
	release, err := env.loadRelease()
	if err != nil || release.ReleaseSHA != state.PreviousSHA {
		t.Fatal("abort changed release")
	}
	if !strings.Contains(output.String(), `"database_restored":false`) || !strings.Contains(output.String(), `"phase":"aborted-before-deploy"`) {
		t.Fatalf("missing accurate abort receipt: %s", output)
	}
}

func TestAbortPreparedRejectsUncertainOrActiveOperation(t *testing.T) {
	for _, scenario := range []string{"wrong-sha", "wrong-operation", "deployed", "recorded-backup", "no-previous", "changed-current", "unhealthy", "lock-held"} {
		t.Run(scenario, func(t *testing.T) {
			env, state, _ := abortFixture(t)
			sha, op := testSHA, testOp
			switch scenario {
			case "wrong-sha":
				sha = strings.Repeat("c", 40)
			case "wrong-operation":
				op = strings.Repeat("d", 64)
			case "deployed":
				state.Phase = "deployed"
			case "recorded-backup":
				state.BackupDir = "/existing-backup"
			case "no-previous":
				state.HadPrevious = false
			case "changed-current":
				if err := os.Remove(filepath.Join(env.ServiceDir, "current")); err != nil {
					t.Fatal(err)
				}
			case "unhealthy":
				env.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
				})
			case "lock-held":
				file, err := os.Open(filepath.Join(env.ServiceDir, "receiver-state", "receiver.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			}
			if err := env.saveOperation(state); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := env.AbortPrepared(ctx, sha, op); err == nil {
				t.Fatal("unsafe abort accepted")
			}
			if _, err := env.loadOperation(); err != nil {
				t.Fatalf("rejected abort modified active state: %v", err)
			}
		})
	}
}
