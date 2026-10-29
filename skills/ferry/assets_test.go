package skillassets

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompleteSelfHostedBundle(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		root := "ferry"
		handler := Handler()
		if legacy {
			root = "smartapi"
			handler = LegacyHandler()
		}
		t.Run(root, func(t *testing.T) {
			get := func(path string) *httptest.ResponseRecorder {
				t.Helper()
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				return w
			}
			bundle := get("/skills/" + root + "/" + root + ".zip")
			if bundle.Code != 200 {
				t.Fatal(bundle.Code)
			}
			checksum := get("/skills/" + root + "/" + root + ".zip.sha256")
			if checksum.Body.String() != fmt.Sprintf("%x  "+root+".zip\n", sha256.Sum256(bundle.Body.Bytes())) {
				t.Fatal("checksum mismatch")
			}
			zr, err := zip.NewReader(bytes.NewReader(bundle.Body.Bytes()), int64(bundle.Body.Len()))
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			for _, f := range zr.File {
				name := strings.TrimPrefix(f.Name, root+"/")
				if name == f.Name || strings.Contains(name, "..") || strings.HasSuffix(name, ".go") || strings.Contains(name, "test_") {
					t.Fatalf("unexpected archive entry %s", f.Name)
				}
				r, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				served := get("/skills/" + root + "/" + name)
				if served.Code != 200 || !bytes.Equal(data, served.Body.Bytes()) {
					t.Fatalf("bundle/URL drift: %s", name)
				}
				found[name] = true
			}
			for _, name := range []string{"SKILL.md", "INSTALL.md", "scripts/ferry.py", "scripts/sync_opencodex_fast.py", "references/management-api.md", "references/workflows.md", "references/data-plane.md", "agents/openai.yaml"} {
				if !found[name] {
					t.Errorf("missing %s", name)
				}
			}
			request := httptest.NewRequest("GET", "/skills/"+root+"/SKILL.md", nil)
			request.Header.Set("If-None-Match", get(request.URL.Path).Header().Get("ETag"))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotModified {
				t.Fatal(response.Code)
			}
			for _, path := range []string{"/skills/" + root + "/assets.go", "/skills/" + root + "/scripts/test_ferry.py", "/skills/" + root + "/../config.yaml", "/skills/" + root + "/"} {
				if w := get(path); w.Code != 404 {
					t.Errorf("%s: %d", path, w.Code)
				}
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/skills/"+root+"/SKILL.md", nil))
			if w.Code != 405 {
				t.Fatal(w.Code)
			}
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("HEAD", "/skills/"+root+"/SKILL.md", nil))
			if w.Code != 200 || w.Body.Len() != 0 {
				t.Fatalf("HEAD: %d %d", w.Code, w.Body.Len())
			}

		})
	}
}
