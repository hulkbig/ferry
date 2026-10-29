package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandbyStartupServesAndDeclaresPausedJobs(t *testing.T) {
	proxy, management := startupAddressesForTest(t)
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte(startupYAML(proxy, management)), 0600); err != nil {
		t.Fatal(err)
	}
	p := startStartupProcess(t, "-data-dir", filepath.Join(root, "data"), "-config", config, "-standby")
	p.ready(t, proxy, management)
	p.stop(t)
	if !strings.Contains(p.output.String(), "standby mode: upstream account changes and background jobs disabled") {
		t.Fatal("standby startup did not declare the effective migration boundary")
	}
}
