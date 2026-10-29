package receiver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCandidateUsesBoundProjectAndPorts(t *testing.T) {
	oldProject, oldData, oldManagement := ProjectName, DataAddress, ManagementAddress
	ProjectName, DataAddress, ManagementAddress = "ferry-candidate", "127.0.0.1:17811", "127.0.0.1:17911"
	t.Cleanup(func() { ProjectName, DataAddress, ManagementAddress = oldProject, oldData, oldManagement })
	e := &Environment{ServiceDir: t.TempDir(), run: func(_ context.Context, _ io.Reader, _ string, args ...string) ([]byte, error) {
		if len(args) < 3 || args[1] != "--project-name" || args[2] != "ferry-candidate" {
			t.Fatalf("wrong instance: %v", args)
		}
		return nil, nil
	}}
	if _, err := e.compose(context.Background(), testSHA, "up", "-d"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	e.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen[r.URL.Host] = true
		status, body := http.StatusOK, `{}`
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/health" {
			if r.URL.Host != ManagementAddress {
				t.Fatal("wrong management instance")
			}
		} else if r.URL.Host != DataAddress {
			t.Fatal("wrong data instance")
		}
		switch r.URL.Path {
		case "/version":
			body = `{"commit":"` + testSHA + `"}`
		case "/v1/models", "/api/overview":
			status = http.StatusUnauthorized
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	if err := e.verifyServing(context.Background(), testSHA); err != nil {
		t.Fatal(err)
	}
	if !seen[DataAddress] || !seen[ManagementAddress] {
		t.Fatalf("incomplete readiness: %v", seen)
	}
}
