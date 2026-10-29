package receiver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ferry/internal/backuppolicy"
	"ferry/internal/maintenance"
)

func TestCustomS3UploadReadbackAndScopedRetention(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(r.Header.Get("Authorization"), "/eu-west-1/s3/aws4_request") {
			t.Errorf("configured region was not used in signing")
		}
		if !strings.HasPrefix(r.URL.Path, "/custom-backups/ferry/scheduled/") {
			t.Errorf("incorrect path style: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPut:
			var body io.Reader = r.Body
			if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
				body = httputil.NewChunkedReader(r.Body)
			}
			b, err := io.ReadAll(body)
			if err != nil {
				t.Error(err)
			}
			objects[r.URL.Path] = b
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		case http.MethodGet, http.MethodHead:
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Header().Set("Last-Modified", "Wed, 09 Sep 2026 00:00:00 GMT")
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
			if r.Method == http.MethodGet {
				_, _ = w.Write(b)
			}
		case http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	p := backuppolicy.Default()
	p.S3 = backuppolicy.Connection{Endpoint: server.URL, Region: "eu-west-1", Bucket: "custom-backups", PathStyle: true}
	p.RemoteCopies = 1
	client, err := newScheduledS3(p.S3, backuppolicy.Credentials{AccessKey: "test-access", SecretKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	bootstrap := filepath.Join(root, "bootstrap.yaml")
	if err := os.WriteFile(bootstrap, []byte(`server:
  data_dir: ./ignored
  management_addr: 0.0.0.0:7900
  proxy_addr: 0.0.0.0:7890
  timezone: Asia/Shanghai
  log_retention_days: 30
consumer_keys:
  - name: smoke
    key: sk-smartapi-test
    enabled: true
    default_alias: smart
    default_objective: balanced
plans:
  - id: upstream
    kind: payg
    currency: USD
channels:
  - id: upstream
    type: custom
    base_url: https://example.invalid/v1
    credential: {secret_ref: store:upstream}
    plan_id: upstream
    pools: [primary]
    models:
      - model: model
        alias: smart
        protocols: [openai-chat]
        capabilities: {text: true, context_window: 32768}
pools:
  - id: primary
aliases:
  smart:
    required_capabilities: {text: true}
    allow_degradation: true
    default_objective: balanced
objectives:
  balanced:
    rank: [capability_fit, max_reliability]
policies:
  main:
    timezone: Asia/Shanghai
    tiers:
      - {name: primary, pool: primary, objective: balanced}
default_policy: main
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Migrate(data, bootstrap); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backup")
	if _, err := maintenance.BackupConfiguration(data, backup); err != nil {
		t.Fatal(err)
	}
	env := &Environment{ServiceDir: root}
	op := "scheduled-20260909T000000Z"
	if err := env.uploadCustomS3(context.Background(), client, p, op, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "restore-drills", op, "readback-scheduled")); !os.IsNotExist(err) {
		t.Fatal("successful readback was retained")
	}
	receipt := scheduledReceipt{DestinationID: s3DestinationID(p), Objects: []string{"s3://custom-backups/ferry/scheduled/" + op, "s3://custom-backups/ferry/scheduled/scheduled-20260910T000000Z"}}
	changed := p
	changed.S3.Endpoint = "https://another.example.test"
	if pruneCustomS3(context.Background(), client, changed, &receipt) == nil {
		t.Fatal("accepted retention for another endpoint")
	}
	if len(deleted) != 0 {
		t.Fatal("destination change deleted old objects")
	}
	if err := pruneCustomS3(context.Background(), client, p, &receipt); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 4 || len(receipt.Objects) != 1 {
		t.Fatalf("retention: deleted %d, retained %d", len(deleted), len(receipt.Objects))
	}
}
