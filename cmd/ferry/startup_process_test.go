package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercise the real main in a separate process, including fatal exits, startup
// ordering and HTTP readiness. Catalog workers cannot contact real upstreams.
func TestStartupProcessHelper(t *testing.T) {
	if os.Getenv("FERRY_STARTUP_PROCESS_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		os.Exit(2)
	}
	os.Args = append([]string{os.Args[0]}, os.Args[separator+1:]...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("outbound network disabled in startup regression")
	}
	http.DefaultTransport = transport
	main()
	os.Exit(0)
}

type startupProcess struct {
	cmd    *exec.Cmd
	output bytes.Buffer
	done   chan struct{}
	err    error
}

func startStartupProcess(t *testing.T, args ...string) *startupProcess {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := append([]string{"-test.run=^TestStartupProcessHelper$", "--"}, args...)
	p := &startupProcess{cmd: exec.Command(self, command...), done: make(chan struct{})}
	p.cmd.Env = append(os.Environ(), "FERRY_STARTUP_PROCESS_HELPER=1", "GORACE=atexit_sleep_ms=0")
	p.cmd.Stdout, p.cmd.Stderr = &p.output, &p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *startupProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		if p.err != nil {
			t.Errorf("startup process shutdown: %v\n%s", p.err, p.output.String())
		}
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Error("startup process did not shut down")
	}
}

func startupHealthy(addr string) bool {
	client := &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *startupProcess) ready(t *testing.T, addresses ...string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			t.Fatalf("startup exited before readiness: %v\n%s", p.err, p.output.String())
		default:
		}
		ready := true
		for _, addr := range addresses {
			ready = startupHealthy(addr) && ready
		}
		if ready {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("startup never became healthy")
}

func (p *startupProcess) failsBeforeHealth(t *testing.T, addresses ...string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, addr := range addresses {
			if startupHealthy(addr) {
				t.Fatal("failed startup became healthy")
			}
		}
		select {
		case <-p.done:
			if p.err == nil {
				t.Fatal("invalid startup succeeded")
			}
			return p.output.String()
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("invalid startup did not exit")
	return ""
}

func startupAddressesForTest(t *testing.T) (string, string) {
	t.Helper()
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	return first.Addr().String(), second.Addr().String()
}

func startupYAML(proxy, management string) string {
	return fmt.Sprintf("server:\n  data_dir: ./ignored-by-cli\n  proxy_addr: %s\n  management_addr: %s\n  timezone: UTC\n", proxy, management)
}

func startupOpenDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func startupExec(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

func startupInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func startupConfigSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT config_json FROM control_config_versions WHERE active=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestStartupProcessBootstrapsAndUpgradesOnce(t *testing.T) {
	data := startupTestDir(t)
	staging := filepath.Join(data, ".startup-bootstrap")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	proxy, management := startupAddressesForTest(t)
	value := startupFixtureValue(t)
	startupWrite(t, filepath.Join(staging, "config.yaml"), startupYAML(proxy, management))
	startupWrite(t, filepath.Join(staging, "upstream-key"), value+"\n")
	args := []string{"-data-dir", data, "-bootstrap-dir", staging}
	p := startStartupProcess(t, args...)
	p.ready(t, proxy, management)
	p.stop(t)
	if strings.Contains(p.output.String(), value) {
		t.Fatal("startup logged bootstrap material")
	}
	for _, name := range []string{"config.yaml", "upstream-key"} {
		if _, err := os.Stat(filepath.Join(staging, name)); !os.IsNotExist(err) {
			t.Fatal("consumed staging remains")
		}
	}
	control := startupOpenDB(t, filepath.Join(data, "control.db"))
	ledger := startupOpenDB(t, filepath.Join(data, "ledger.db"))
	conversation := startupOpenDB(t, filepath.Join(data, "conversation.db"))
	raw := startupConfigSnapshot(t, control)
	var envelope struct{ Server struct{ DataDir string } }
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Server.DataDir != data {
		t.Fatal("CLI data directory not persisted")
	}
	if n := startupInt(t, control, `SELECT count(*) FROM control_config_versions`); n != 1 {
		t.Fatalf("initial revision count = %d", n)
	}
	var persisted string
	if err := control.QueryRow(`SELECT value FROM local_secrets WHERE name='new_api'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != value {
		t.Fatal("bootstrap value was not persisted")
	}
	startupExec(t, control, `UPDATE local_secrets SET updated_at=17 WHERE name='new_api'`)

	// Downgrade actual on-disk schemas, preserving real rows. Only normal main
	// startup may upgrade these fixtures; assertions use raw SQL, never Open.
	startupExec(t, control, `ALTER TABLE api_keys DROP COLUMN exact_only`)
	startupExec(t, ledger, `ALTER TABLE logical_requests DROP COLUMN service_tier_evidence`)
	startupExec(t, ledger, `INSERT INTO logical_requests(id,ts) VALUES('retained-request',?)`, time.Now().UnixMilli())
	startupExec(t, conversation, `ALTER TABLE smart_responses DROP COLUMN conversation_items_json`)
	startupExec(t, conversation, `INSERT INTO conversations VALUES('retained-conversation','model','[]','','','','canonical',?,?,'')`, time.Now().UnixMilli(), time.Now().UnixMilli())
	startupExec(t, conversation, `DROP TABLE conversation_evidence`)
	startupExec(t, conversation, `CREATE TABLE conversation_evidence(key_hash TEXT PRIMARY KEY,conversation_id TEXT NOT NULL,last_seen_at INTEGER NOT NULL)`)
	startupExec(t, conversation, `INSERT INTO conversation_evidence VALUES('legacy-hash','retained-conversation',?)`, time.Now().UnixMilli())

	// A retry may restage a changed YAML, but an active DB remains authoritative.
	otherProxy, otherManagement := startupAddressesForTest(t)
	startupWrite(t, filepath.Join(staging, "config.yaml"), startupYAML(otherProxy, otherManagement))
	startupWrite(t, filepath.Join(staging, "upstream-key"), value)
	p = startStartupProcess(t, args...)
	p.ready(t, proxy, management)
	p.stop(t)
	for _, check := range []struct {
		db    *sql.DB
		query string
	}{
		{control, `SELECT count(*) FROM pragma_table_info('api_keys') WHERE name='exact_only'`},
		{ledger, `SELECT count(*) FROM pragma_table_info('logical_requests') WHERE name='service_tier_evidence'`},
		{ledger, `SELECT count(*) FROM logical_requests WHERE id='retained-request'`},
		{conversation, `SELECT count(*) FROM pragma_table_info('smart_responses') WHERE name='conversation_items_json'`},
		{conversation, `SELECT count(*) FROM conversation_evidence WHERE logical_model_id='model' AND key_hash='legacy-hash'`},
	} {
		if n := startupInt(t, check.db, check.query); n != 1 {
			t.Fatal("normal startup did not preserve and migrate schema/data")
		}
	}
	if startupConfigSnapshot(t, control) != raw {
		t.Fatal("staged YAML overwrote active configuration")
	}
	if startupInt(t, control, `SELECT updated_at FROM local_secrets WHERE name='new_api'`) != 17 {
		t.Fatal("same-value seed was rewritten")
	}
	versions := func() []int64 {
		return []int64{startupInt(t, control, `PRAGMA schema_version`), startupInt(t, ledger, `PRAGMA schema_version`), startupInt(t, conversation, `PRAGMA schema_version`), startupInt(t, control, `SELECT count(*) FROM control_config_versions`)}
	}
	before := versions()
	// Both an empty staging directory and an absent one are valid upgrades.
	for _, remove := range []bool{false, true} {
		if remove {
			if err := os.Remove(staging); err != nil {
				t.Fatal(err)
			}
		}
		p = startStartupProcess(t, args...)
		p.ready(t, proxy, management)
		p.stop(t)
		if !reflect.DeepEqual(versions(), before) {
			t.Fatal("repeat startup repeated DDL or created configuration revisions")
		}
		if startupConfigSnapshot(t, control) != raw {
			t.Fatal("upgrade reset configuration")
		}
	}

	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	startupWrite(t, filepath.Join(staging, "upstream-key"), value+"-conflict")
	p = startStartupProcess(t, args...)
	output := p.failsBeforeHealth(t, proxy, management)
	if !strings.Contains(output, "refusing to rotate") || strings.Contains(output, value) {
		t.Fatal("conflict error missing or disclosed material")
	}
	if err := control.QueryRow(`SELECT value FROM local_secrets WHERE name='new_api'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != value || startupInt(t, control, `SELECT updated_at FROM local_secrets WHERE name='new_api'`) != 17 {
		t.Fatal("conflicting seed rotated secret")
	}
	if _, err := os.Stat(filepath.Join(staging, "upstream-key")); err != nil {
		t.Fatal("failed bootstrap was deleted")
	}
}

func TestStartupProcessFailsBeforeListening(t *testing.T) {
	for _, failure := range []string{"missing-config", "invalid-config", "broken-ledger", "broken-conversation"} {
		t.Run(failure, func(t *testing.T) {
			data := startupTestDir(t)
			staging := filepath.Join(data, ".startup-bootstrap")
			if err := os.Mkdir(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			proxy, management := startupAddressesForTest(t)
			if failure != "missing-config" {
				startupWrite(t, filepath.Join(staging, "config.yaml"), startupYAML(proxy, management))
			}
			switch failure {
			case "invalid-config":
				startupWrite(t, filepath.Join(staging, "config.yaml"), "server: [invalid")
			case "broken-ledger":
				startupWrite(t, filepath.Join(data, "ledger.db"), "not a SQLite database")
			case "broken-conversation":
				startupWrite(t, filepath.Join(data, "conversation.db"), "not a SQLite database")
			}
			value := startupFixtureValue(t)
			startupWrite(t, filepath.Join(staging, "upstream-key"), value)
			p := startStartupProcess(t, "-data-dir", data, "-bootstrap-dir", staging)
			output := p.failsBeforeHealth(t, proxy, management)
			if strings.Contains(output, value) {
				t.Fatal("failed startup leaked material")
			}
			if _, err := os.Stat(filepath.Join(staging, "upstream-key")); err != nil {
				t.Fatal("failed startup removed retry material")
			}
		})
	}
}

func TestStartupProcessExplicitConfigIsPreserved(t *testing.T) {
	data := startupTestDir(t)
	staging := startupTestDir(t)
	proxy, management := startupAddressesForTest(t)
	path := filepath.Join(staging, "config.yaml")
	raw := startupYAML(proxy, management)
	startupWrite(t, path, raw)
	p := startStartupProcess(t, "-data-dir", data, "-config", path, "-bootstrap-dir", staging)
	p.ready(t, proxy, management)
	p.stop(t)
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != raw {
		t.Fatal("explicit -config changed or removed")
	}
}

func TestStartupProcessCheckFlagIsReadOnlyAndExclusive(t *testing.T) {
	data := filepath.Join(t.TempDir(), "absent")
	path := filepath.Join(t.TempDir(), "config.yaml")
	proxy, management := startupAddressesForTest(t)
	startupWrite(t, path, startupYAML(proxy, management))
	p := startStartupProcess(t, "-data-dir", data, "-config", path, "--startup-check")
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatalf("startup check failed: %v", p.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startup check did not exit")
	}
	var got startupConfig
	if err := json.Unmarshal(p.output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ProxyAddr != proxy || got.ManagementAddr != management {
		t.Fatal("check returned incorrect listener addresses")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("startup check created data directory")
	}
	p = startStartupProcess(t, "-data-dir", data, "-config", path, "--startup-check", "--migrate-only")
	if output := p.failsBeforeHealth(t, proxy, management); !strings.Contains(output, "mutually exclusive") {
		t.Fatal("check accepted another maintenance action")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("conflicting actions mutated data")
	}
}
