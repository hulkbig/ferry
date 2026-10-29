package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"ferry/internal/config"
)

type startupConfig struct {
	ProxyAddr      string `json:"proxy_addr"`
	ManagementAddr string `json:"management_addr"`
}

// readStartupConfig only reads the original configuration envelope. It must not
// use controlstore.Open/ActiveConfig: those migrate or require newer normalized
// tables that the serving process has not created yet. Do not use immutable=1;
// it would ignore committed configuration still present in an existing WAL.
func readStartupConfig(dataDir, bootstrapPath string) (startupConfig, error) {
	path, err := filepath.Abs(filepath.Join(dataDir, "control.db"))
	if err != nil {
		return startupConfig{}, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return readStartupBootstrap(bootstrapPath)
	}
	if err != nil {
		return startupConfig{}, fmt.Errorf("inspect control store: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return startupConfig{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='control_config_versions'`).Scan(&exists); err != nil {
		return startupConfig{}, fmt.Errorf("read control schema: %w", err)
	}
	if exists == 0 {
		return readStartupBootstrap(bootstrapPath)
	}
	var raw []byte
	err = db.QueryRowContext(ctx, `SELECT config_json FROM control_config_versions WHERE active=1 LIMIT 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return readStartupBootstrap(bootstrapPath)
	}
	if err != nil {
		return startupConfig{}, fmt.Errorf("read active listener configuration: %w", err)
	}
	var envelope struct{ Server config.ServerConfig }
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return startupConfig{}, fmt.Errorf("decode active listener configuration: %w", err)
	}
	return startupAddresses(envelope.Server)
}

func readStartupBootstrap(path string) (startupConfig, error) {
	if path == "" {
		return startupConfig{}, fmt.Errorf("control store is empty: import an existing config once with -config <path>")
	}
	f, err := config.Load(path)
	if err != nil {
		return startupConfig{}, fmt.Errorf("read bootstrap configuration: %w", err)
	}
	return startupAddresses(f.Server)
}

func startupAddresses(server config.ServerConfig) (startupConfig, error) {
	for _, addr := range []string{server.ProxyAddr, server.ManagementAddr} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return startupConfig{}, fmt.Errorf("invalid listener address %q: %w", addr, err)
		}
	}
	return startupConfig{ProxyAddr: server.ProxyAddr, ManagementAddr: server.ManagementAddr}, nil
}
