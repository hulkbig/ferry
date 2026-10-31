package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeadlessProcessDoesNotBindManagement(t *testing.T) {
	for _, standby := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "standby"}[standby], func(t *testing.T) {
			proxy, _ := startupAddressesForTest(t)
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer occupied.Close()
			dir := t.TempDir()
			cfg := filepath.Join(dir, "bootstrap.yaml")
			if err := os.WriteFile(cfg, []byte(startupYAML(proxy, occupied.Addr().String())), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-data-dir", filepath.Join(dir, "data"), "-config", cfg, "-headless"}
			if standby {
				args = append(args, "-standby")
			}
			p := startStartupProcess(t, args...)
			p.ready(t, proxy)
			client := http.Client{Timeout: time.Second}
			res, err := client.Get("http://" + proxy + "/v1/models")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("headless auth status=%d", res.StatusCode)
			}
			p.stop(t)
			logs := p.output.String()
			if strings.Contains(logs, "management listening") || strings.Contains(logs, "address already in use") {
				t.Fatal(logs)
			}
			if !strings.Contains(logs, "headless mode:") {
				t.Fatal("missing mode diagnostic")
			}
		})
	}
}

func TestHeadlessStartupCheckOmitsManagement(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bootstrap.yaml")
	if err := os.WriteFile(cfg, []byte(startupYAML("127.0.0.1:7890", "127.0.0.1:7900")), 0600); err != nil {
		t.Fatal(err)
	}
	p := startStartupProcess(t, "-data-dir", filepath.Join(dir, "data"), "-config", cfg, "-headless", "-startup-check")
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("startup check stuck")
	}
	if p.err != nil || !strings.Contains(p.output.String(), `"management_addr":""`) {
		t.Fatalf("startup check %v %s", p.err, p.output.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatal("startup check wrote state")
	}
}
