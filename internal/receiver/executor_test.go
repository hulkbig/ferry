package receiver

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestRunCommandRejectsExecutableOutsideProductionAllowlist(t *testing.T) {
	output, err := (&Environment{}).runCommand(context.Background(), nil, "/bin/sh", "-c", "exit 0")
	if err == nil || !strings.Contains(err.Error(), "executable is not allowed") {
		t.Fatalf("error = %v, want executable allowlist rejection", err)
	}
	if len(output) != 0 {
		t.Fatalf("rejected command returned output: %q", output)
	}
}

func TestServingReadinessUsesPublicAuthBoundary(t *testing.T) {
	requests := []string{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		status, body := http.StatusOK, "{}"
		switch r.URL.Path {
		case "/version":
			body = `{"commit":"` + testSHA + `"}`
		case "/v1/models":
			status = http.StatusUnauthorized
		case "/api/overview":
			status = http.StatusPreconditionRequired
		}
		return &http.Response{
			StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})}
	env := &Environment{HTTPClient: client}
	if err := env.waitForServingBoundaries(context.Background(), testSHA); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /version", "GET /v1/models", "GET /api/auth/status", "GET /health", "GET /api/overview"}
	if strings.Join(requests, "|") != strings.Join(want, "|") {
		t.Fatalf("requests=%v want=%v", requests, want)
	}
}

func TestVerifiedCommitSkipsBackupAndRestoreDrill(t *testing.T) {
	for _, phase := range []string{"verified", "restore-drilled"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "receiver-state"), 0o700); err != nil {
				t.Fatal(err)
			}
			env := &Environment{ServiceDir: root, run: func(_ context.Context, _ io.Reader, name string, args ...string) ([]byte, error) {
				t.Fatalf("verified commit unexpectedly ran %s %v", name, args)
				return nil, nil
			}}
			request := Request{ReleaseSHA: testSHA, OperationID: testOp}
			state := &operationState{Schema: "ferry/receiver-operation/v1", OperationID: testOp, ReleaseSHA: testSHA, Phase: phase, HadPrevious: true, PreviousSHA: strings.Repeat("b", 40)}
			if err := env.saveOperation(state); err != nil {
				t.Fatal(err)
			}
			result, err := env.commitRelease(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result["phase"] != "candidate-committed" {
				t.Fatalf("commit result = %v", result)
			}
			if _, err := os.Stat(filepath.Join(root, "backups")); !os.IsNotExist(err) {
				t.Fatalf("commit created backup directory: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "restore-drills")); !os.IsNotExist(err) {
				t.Fatalf("commit created restore drill directory: %v", err)
			}
		})
	}
}

func TestPreparedCommitReachesImageLoadWithoutOpeningDatabase(t *testing.T) {
	// The control.db fixture is deliberately not SQLite. The receiver must get
	// as far as the absent image artifact without trying to snapshot/migrate it.
	env, _, _ := abortFixture(t)
	env.run = func(_ context.Context, _ io.Reader, name string, args ...string) ([]byte, error) {
		if name != dockerPath || len(args) == 0 || args[0] != "compose" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return nil, nil
	}
	_, err := env.commitRelease(context.Background(), Request{ReleaseSHA: testSHA, OperationID: testOp})
	if err == nil || !strings.Contains(err.Error(), "image.tar.gz") {
		t.Fatalf("want image-load failure without database access, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.ServiceDir, "backups", testOp)); !os.IsNotExist(err) {
		t.Fatalf("release created a backup: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(env.ServiceDir, "data", "control.db"))
	if err != nil || string(raw) != "unchanged" {
		t.Fatal("receiver changed database")
	}
}

func TestRollbackWithoutReleaseSnapshotKeepsDatabaseAndReportsScope(t *testing.T) {
	root := t.TempDir()
	previousSHA := strings.Repeat("b", 40)
	for _, path := range []string{"receiver-state", filepath.Join("releases", previousSHA), "data"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(root, "data", "marker")
	if err := os.WriteFile(marker, []byte("migrated"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := Request{ReleaseSHA: testSHA, OperationID: testOp}
	state := &operationState{Schema: "ferry/receiver-operation/v1", OperationID: testOp, ReleaseSHA: testSHA, Phase: "deployed", HadPrevious: true, PreviousSHA: previousSHA}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, "{}"
		switch r.URL.Path {
		case "/version":
			body = "{\"commit\":\"" + previousSHA + "\"}"
		case "/v1/models":
			status = http.StatusUnauthorized
		case "/api/overview":
			status = http.StatusPreconditionRequired
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	env := &Environment{ServiceDir: root, HTTPClient: client, run: func(_ context.Context, _ io.Reader, name string, args ...string) ([]byte, error) {
		if name != dockerPath || len(args) == 0 || args[0] != "compose" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return nil, nil
	}}
	if err := env.saveOperation(state); err != nil {
		t.Fatal(err)
	}
	result, err := env.rollback(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result["phase"] != "rolled-back" || result["database_restored"] != false {
		t.Fatalf("rollback result = %v", result)
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "migrated" {
		t.Fatalf("database marker = %q, %v", raw, err)
	}
	if release, err := env.loadRelease(); err != nil || release.ReleaseSHA != previousSHA {
		t.Fatalf("release = %v, %v", release, err)
	}
}
