package management

import (
	"net/http/httptest"
	"os"
	"testing"
)

// TestDumpConsoleForLint writes the served console HTML to /tmp/console.html so
// the external Node smoke test (/tmp/console_dom_test.js) can exercise it.
// Guarded by CPA_CONSOLE_DUMP=1 so normal `go test ./...` stays hermetic.
func TestDumpConsoleForLint(t *testing.T) {
	if os.Getenv("CPA_CONSOLE_DUMP") == "" {
		t.Skip("set CPA_CONSOLE_DUMP=1 to dump the console HTML")
	}
	f := newMgmtFixture(t)
	rec := httptest.NewRecorder()
	f.srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if err := os.WriteFile("/tmp/console.html", rec.Body.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
