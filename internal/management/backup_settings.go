package management

import (
	"encoding/json"
	"fmt"
	"strings"

	"ferry/internal/backuppolicy"
	"github.com/google/uuid"
)

// Write-only credentials are deliberately separate from the public policy.
type backupPolicyPatch struct {
	s3Present bool
	backuppolicy.Policy
	Credentials *backuppolicy.Credentials `json:"s3_credentials,omitempty"`
}

func (p *backupPolicyPatch) UnmarshalJSON(raw []byte) error {
	type wire backupPolicyPatch
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, (*wire)(p)); err != nil {
		return err
	}
	_, p.s3Present = fields["s3"]
	return nil
}

func (s *Server) prepareBackupPatch(p *backupPolicyPatch) error {
	old, err := backuppolicy.Load(s.deps.Control)
	if err != nil {
		return fmt.Errorf("backup policy could not be loaded")
	}
	if !p.s3Present {
		p.S3 = old.S3
	}
	// References can only be retained from the current connection, never selected
	// by the caller from another secret namespace or an old connection.
	p.S3.CredentialsRef = ""
	if p.S3.Endpoint != "" {
		if s.deps.Store == nil {
			return fmt.Errorf("secret store is not configured")
		}
		if p.Credentials != nil && (p.Credentials.AccessKey != "" || p.Credentials.SecretKey != "") {
			if strings.TrimSpace(p.Credentials.AccessKey) == "" || p.Credentials.SecretKey == "" || len(p.Credentials.AccessKey) > 256 || len(p.Credentials.SecretKey) > 4096 {
				return fmt.Errorf("provide both S3 access key and secret key")
			}
			p.S3.CredentialsRef = "backup:s3:" + uuid.NewString()
		} else {
			if old.S3.Endpoint != p.S3.Endpoint || old.S3.CredentialsRef == "" {
				return fmt.Errorf("provide S3 credentials when configuring a new endpoint")
			}
			if _, ok := s.deps.Store.Get(old.S3.CredentialsRef); !ok {
				return fmt.Errorf("saved S3 credentials are unavailable")
			}
			p.S3.CredentialsRef = old.S3.CredentialsRef
			p.Credentials = nil
		}
	} else if p.Credentials != nil && (p.Credentials.AccessKey != "" || p.Credentials.SecretKey != "") {
		return fmt.Errorf("S3 endpoint is required with credentials")
	}
	return backuppolicy.Validate(p.Policy)
}

func (s *Server) saveBackupPatch(p *backupPolicyPatch) error {
	// Create an immutable credential record before publishing its reference.
	// Concurrent updates cannot overwrite credentials used by another policy.
	if p.Credentials != nil && p.S3.CredentialsRef != "" {
		raw, err := json.Marshal(p.Credentials)
		if err != nil {
			return err
		}
		if err = s.deps.Store.Set(p.S3.CredentialsRef, string(raw)); err != nil {
			return fmt.Errorf("could not save S3 credentials")
		}
	}
	if err := backuppolicy.Save(s.deps.Control, p.Policy); err != nil {
		return fmt.Errorf("could not save backup policy")
	}
	return nil
}
