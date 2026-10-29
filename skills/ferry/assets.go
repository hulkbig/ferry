// Package skillassets serves the exact Skill shipped with this binary.
package skillassets

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Only reviewed distributable files are embedded; tests and local files are excluded.
//
//go:embed SKILL.md INSTALL.md agents/openai.yaml scripts/ferry.py scripts/sync_opencodex_fast.py references/*.md
var assets embed.FS

// Handler exposes public documentation only. It never reads runtime configuration.
func Handler() http.Handler { return bundleHandler(false) }

// LegacyHandler keeps old install URLs usable without maintaining a second client.
func LegacyHandler() http.Handler { return bundleHandler(true) }

func bundleHandler(legacy bool) http.Handler {
	root := "ferry"
	if legacy {
		root = "smartapi"
	}
	files := map[string][]byte{}
	if err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := assets.ReadFile(name)
		if err != nil {
			return err
		}
		if legacy && (strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".yaml")) {
			data = []byte(strings.ReplaceAll(string(data), "ferry", "smartapi"))
		}
		files[name] = data
		return nil
	}); err != nil {
		panic(err)
	}
	if legacy {
		files["scripts/smartapi.py"] = []byte("#!/usr/bin/env python3\nfrom ferry import main\nif __name__ == '__main__':\n    raise SystemExit(main())\n")
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for _, name := range names {
		h := &zip.FileHeader{Name: root + "/" + name, Method: zip.Deflate}
		h.SetMode(0644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			panic(err)
		}
		if _, err := w.Write(files[name]); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	bundle := archive.Bytes()
	checksum := fmt.Sprintf("%x  %s.zip\n", sha256.Sum256(bundle), root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/skills/"+root+"/")
		var data []byte
		contentType := "text/plain; charset=utf-8"
		switch name {
		case root + ".zip":
			data = bundle
			contentType = "application/zip"
			w.Header().Set("Content-Disposition", `attachment; filename="`+root+`.zip"`)
		case root + ".zip.sha256":
			data = []byte(checksum)
		default:
			if !fs.ValidPath(name) {
				http.NotFound(w, r)
				return
			}
			var ok bool
			data, ok = files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if strings.HasSuffix(name, ".md") {
				contentType = "text/markdown; charset=utf-8"
			}
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(data)))
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}
