package receiver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	RepositoryIDValue = "123456789"
	os.Exit(m.Run())
}

const (
	RepositoryID int64 = 123456789
	testSHA            = "0123456789abcdef0123456789abcdef01234567"
	testOp             = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestParseOriginalCommandAcceptsOnlyBoundFerryRequests(t *testing.T) {
	digest := strings.Repeat("a", 64)
	req, err := ParseOriginalCommand("ferry/v1 123456789 " + testSHA + " " + testOp + " prepare " + digest)
	if err != nil {
		t.Fatal(err)
	}
	if req.RepositoryID != 123456789 || req.ReleaseSHA != testSHA || req.OperationID != testOp || req.Verb != "prepare" {
		t.Fatalf("parsed request = %#v", req)
	}

	bad := []string{
		"ferry/v1 1 " + testSHA + " " + testOp + " prepare",
		"ferry/v1 123456789 " + testSHA + " " + testOp + " shell",
		"ferry/v1 123456789 " + testSHA + " " + testOp + " prepare",
		"ferry/v1 123456789 " + testSHA + " " + testOp + " prepare; id",
		"ferry/v1 123456789 " + testSHA + " " + testOp + " deploy",
		"ferry/v1 123456789 " + testSHA + " " + testOp + " verify",
		"ferry/v1 123456789 " + strings.ToUpper(testSHA) + " " + testOp + " prepare",
		"ferry/v1 123456789 " + testSHA + " " + testOp + "  prepare",
	}
	for _, command := range bad {
		if _, err := ParseOriginalCommand(command); err == nil {
			t.Fatalf("unsafe command accepted: %q", command)
		}
	}
}

func TestParseOriginalCommandBindsPayloadDigestOnlyToPrepare(t *testing.T) {
	digest := strings.Repeat("a", 64)
	req, err := ParseOriginalCommand("ferry/v1 123456789 " + testSHA + " " + testOp + " prepare " + digest)
	if err != nil {
		t.Fatal(err)
	}
	if req.Argument != digest {
		t.Fatalf("bundle digest = %q, want %q", req.Argument, digest)
	}
	if _, err := ParseOriginalCommand("ferry/v1 123456789 " + testSHA + " " + testOp + " status " + digest); err == nil {
		t.Fatal("status must reject an unrelated trailing argument")
	}
}

func TestRenderAuthorizedKeyIsRepositoryScopedAndHasNoShell(t *testing.T) {
	publicKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEj7tHc0VknsMZJ3wqwuTT9sYcH+vbiAiq24GihrNqZt ferry-test"
	line, err := RenderAuthorizedKey(publicKey, "192.0.2.8")
	if err != nil {
		t.Fatal(err)
	}
	for _, clause := range []string{
		`restrict`, `from="192.0.2.8"`, `command="/usr/local/libexec/ferry-receiver forced"`,
		`ferry-receiver`,
	} {
		if !strings.Contains(line, clause) {
			t.Fatalf("authorized key line is missing %q: %s", clause, line)
		}
	}
	if strings.Contains(line, "no-restrict") {
		t.Fatalf("authorized key line enables a shell: %s", line)
	}
}

func bundleBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func TestExtractReleaseBundleAllowsOnlyReviewedArtifacts(t *testing.T) {
	raw := bundleBytes(t, map[string]string{
		"compose.yml":                    "services: {}\n",
		"worker-README.md":               "worker\n",
		"bootstrap-config.template.yaml": "server: {}\n",
	})
	sum := sha256.Sum256(raw)
	target := filepath.Join(t.TempDir(), "release")
	if err := ExtractReleaseBundle(bytes.NewReader(raw), target, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	for _, name := range releaseBundleFiles {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Fatalf("release artifact %s: %v", name, err)
		}
	}

	unsafe := bundleBytes(t, map[string]string{"../escape": "bad"})
	unsafeSum := sha256.Sum256(unsafe)
	if err := ExtractReleaseBundle(bytes.NewReader(unsafe), filepath.Join(t.TempDir(), "bad"), hex.EncodeToString(unsafeSum[:])); err == nil {
		t.Fatal("path traversal bundle must be rejected")
	}
}

func TestSafeArchiveEntryPathAllowsOnlyOneBasenameBelowRoot(t *testing.T) {
	root := t.TempDir()
	got, err := safeArchiveEntryPath(root, "manifest.json")
	if err != nil || got != filepath.Join(root, "manifest.json") {
		t.Fatalf("safe path = %q, %v", got, err)
	}
	for _, name := range []string{"", ".", "..", "../escape", "nested/file", `/absolute`, `nested\\file`} {
		if got, err := safeArchiveEntryPath(root, name); err == nil {
			t.Errorf("unsafe name %q produced %q", name, got)
		}
	}
}

func releasePayloadBytes(t *testing.T, sha string) []byte {
	t.Helper()
	bundle := bundleBytes(t, map[string]string{
		"compose.yml":                    "services: {}\n",
		"worker-README.md":               "worker\n",
		"bootstrap-config.template.yaml": "server: {}\n",
	})
	image := []byte("gzip-image-placeholder")
	files := []payloadFile{}
	for _, item := range []struct {
		name string
		raw  []byte
	}{{"image.tar.gz", image}, {"release-bundle.tar.gz", bundle}} {
		sum := sha256.Sum256(item.raw)
		files = append(files, payloadFile{Name: item.name, Size: int64(len(item.raw)), SHA256: hex.EncodeToString(sum[:])})
	}
	manifest, err := json.Marshal(payloadManifest{Schema: "ferry/release-payload/v1", ReleaseSHA: sha, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, item := range []struct {
		name string
		raw  []byte
	}{{"manifest.json", manifest}, {"release-bundle.tar.gz", bundle}, {"image.tar.gz", image}} {
		if err := tw.WriteHeader(&tar.Header{Name: item.name, Typeflag: tar.TypeReg, Mode: 0o444, Size: int64(len(item.raw))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(item.raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func TestPrepareStoresContentAddressedPayloadAndRejectsCrossOperation(t *testing.T) {
	payload := releasePayloadBytes(t, testSHA)
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	root := t.TempDir()
	newEnvironment := func() (*Environment, *bytes.Buffer) {
		output := &bytes.Buffer{}
		env := &Environment{
			ServiceDir: root,
			Stdin:      bytes.NewReader(payload), Stdout: output,
			HTTPClient: &http.Client{},
			run:        func(context.Context, io.Reader, string, ...string) ([]byte, error) { return nil, nil },
		}
		return env, output
	}
	request := Request{RepositoryID: RepositoryID, ReleaseSHA: testSHA, OperationID: testOp, Verb: "prepare", Argument: digest}
	env, _ := newEnvironment()
	if err := env.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "payloads", digest+".tar"),
		filepath.Join(root, "receiver-state", "staging", testOp, "manifest.json"),
		filepath.Join(root, "receiver-state", "staging", testOp, "image.tar.gz"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o400 {
			t.Fatalf("%s mode = %o, want 0400", path, info.Mode().Perm())
		}
	}

	// An exact replay is idempotent and still consumes/verifies its stream.
	env, output := newEnvironment()
	if err := env.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"idempotent":true`) {
		t.Fatalf("prepare replay output = %s", output.String())
	}

	cross := request
	cross.OperationID = strings.Repeat("b", 64)
	env, _ = newEnvironment()
	if err := env.Execute(context.Background(), cross); err == nil {
		t.Fatal("cross-operation prepare must be rejected")
	}
}
