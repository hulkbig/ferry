// Package backuppolicy defines the operator-controlled scheduled backup contract.
package backuppolicy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const SettingKey = "backup_policy"

type Policy struct {
	S3             Connection `json:"s3"`
	Enabled        bool       `json:"enabled"`
	IntervalHours  int        `json:"interval_hours"`
	IncludeHistory bool       `json:"include_history"`
	S3Prefix       string     `json:"s3_prefix"`
	LocalCopies    int        `json:"local_copies"`
	RemoteCopies   int        `json:"remote_copies"`
}

// Connection contains public settings and an opaque reference, never credentials.
// An empty Endpoint uses the host's existing project S3 connection.
type Connection struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	PathStyle      bool   `json:"path_style"`
	CredentialsRef string `json:"credentials_ref,omitempty"`
}

type Credentials struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

func ValidateConnection(c Connection) error {
	if c.Endpoint == "" {
		if c.Region != "" || c.Bucket != "" || c.CredentialsRef != "" || c.PathStyle {
			return fmt.Errorf("S3 endpoint is required for a custom connection")
		}
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return fmt.Errorf("S3 endpoint must be an HTTP(S) origin without credentials or a path")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`).MatchString(c.Bucket) || strings.Contains(c.Bucket, "..") {
		return fmt.Errorf("S3 bucket must be a valid bucket name")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(c.Region) {
		return fmt.Errorf("S3 region is required and must contain only letters, numbers, hyphens or underscores")
	}
	if c.CredentialsRef != "" && !regexp.MustCompile(`^backup:s3:[a-f0-9-]{36}$`).MatchString(c.CredentialsRef) {
		return fmt.Errorf("invalid S3 credential reference")
	}
	return nil
}

func Default() Policy {
	return Policy{Enabled: true, IntervalHours: 24, S3Prefix: "ferry/scheduled", LocalCopies: 2, RemoteCopies: 30}
}

// LegacyDefault preserves destinations for databases created before the Ferry rename.
func LegacyDefault() Policy {
	p := Default()
	p.S3Prefix = "smart-api/scheduled"
	return p
}

var prefixPart = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func Validate(p Policy) error {
	if p.IncludeHistory {
		return fmt.Errorf("scheduled backups are configuration-only; use an explicit manual history backup")
	}
	if err := ValidateConnection(p.S3); err != nil {
		return err
	}
	if p.IntervalHours < 24 || p.IntervalHours > 720 || p.IntervalHours%24 != 0 {
		return fmt.Errorf("backup interval_hours must be a whole number of days between 24 and 720")
	}
	if p.RemoteCopies < 1 || p.RemoteCopies > 90 {
		return fmt.Errorf("backup remote_copies must be between 1 and 90")
	}
	if p.LocalCopies < 1 || p.LocalCopies > 7 {
		return fmt.Errorf("backup local_copies must be between 1 and 7")
	}
	if (!strings.HasPrefix(p.S3Prefix, "ferry/") && !strings.HasPrefix(p.S3Prefix, "smart-api/")) || len(p.S3Prefix) > 200 {
		return fmt.Errorf("backup s3_prefix must be inside ferry/ or smart-api/")
	}
	for _, part := range strings.Split(p.S3Prefix, "/") {
		if !prefixPart.MatchString(part) {
			return fmt.Errorf("backup s3_prefix contains an invalid directory")
		}
	}
	return nil
}

type Store interface {
	RuntimeSetting(string) (string, bool, error)
	SetRuntimeSetting(string, string) error
}

func Load(s Store) (Policy, error) {
	p := LegacyDefault()
	raw, found, err := s.RuntimeSetting(SettingKey)
	if err != nil {
		return p, err
	}
	if !found {
		return p, nil
	}
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	p.IncludeHistory = false
	return p, Validate(p)
}
func Save(s Store, p Policy) error {
	if err := Validate(p); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.SetRuntimeSetting(SettingKey, string(raw))
}
