package receiver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ferry/internal/backuppolicy"
	"ferry/internal/maintenance"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Custom connections are used only for scheduled backups. Release rollback
// continues to use the independently provisioned host connection.
func (e *Environment) scheduledS3(c backuppolicy.Connection) (*minio.Client, error) {
	if err := backuppolicy.ValidateConnection(c); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.ServiceDir, "data", "control.db")+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var raw string
	if err = db.QueryRow("SELECT value FROM local_secrets WHERE name = ?", c.CredentialsRef).Scan(&raw); err != nil {
		return nil, fmt.Errorf("S3 credentials are unavailable")
	}
	var secret backuppolicy.Credentials
	if json.Unmarshal([]byte(raw), &secret) != nil || secret.AccessKey == "" || secret.SecretKey == "" {
		return nil, fmt.Errorf("S3 credentials are invalid")
	}
	return newScheduledS3(c, secret)
}

func newScheduledS3(c backuppolicy.Connection, secret backuppolicy.Credentials) (*minio.Client, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid S3 endpoint")
	}
	lookup := minio.BucketLookupDNS
	if c.PathStyle {
		lookup = minio.BucketLookupPath
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(secret.AccessKey, secret.SecretKey, ""), Secure: u.Scheme == "https", Region: c.Region, BucketLookup: lookup, Transport: transport, MaxRetries: 3})
}

func s3DestinationID(p backuppolicy.Policy) string {
	if p.S3.Endpoint == "" {
		return ""
	} // Preserve the original host receipt.
	raw, _ := json.Marshal([]string{p.S3.Endpoint, p.S3.Bucket, p.S3Prefix})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (e *Environment) uploadCustomS3(ctx context.Context, client *minio.Client, p backuppolicy.Policy, operationID, backupDir string) error {
	readback := filepath.Join(e.ServiceDir, "restore-drills", operationID, "readback-scheduled")
	if err := os.MkdirAll(readback, 0o700); err != nil {
		return err
	}
	// Failed verification evidence is retained for diagnosis; successful copies
	// are removed immediately, just like the host connection's readback.
	for _, name := range backupPayloadNames {
		key := p.S3Prefix + "/" + operationID + "/" + name
		if _, err := client.FPutObject(ctx, p.S3.Bucket, key, filepath.Join(backupDir, name), minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
			return safeS3Error("upload", err)
		}
		target := filepath.Join(readback, name)
		if err := client.FGetObject(ctx, p.S3.Bucket, key, target, minio.GetObjectOptions{}); err != nil {
			return safeS3Error("readback", err)
		}
		if err := os.Chmod(target, 0o600); err != nil {
			return err
		}
	}
	if _, err := maintenance.Verify(readback); err != nil {
		return fmt.Errorf("S3 readback verification failed: %w", err)
	}
	return os.RemoveAll(readback)
}

func pruneCustomS3(ctx context.Context, client *minio.Client, p backuppolicy.Policy, receipt *scheduledReceipt) error {
	if receipt.DestinationID != s3DestinationID(p) {
		return fmt.Errorf("S3 retention destination mismatch")
	}
	root := "s3://" + p.S3.Bucket + "/" + p.S3Prefix + "/"
	for len(receipt.Objects) > p.RemoteCopies {
		remote := receipt.Objects[0]
		if !strings.HasPrefix(remote, root) {
			return fmt.Errorf("invalid managed S3 destination")
		}
		operation := strings.TrimPrefix(remote, root)
		if _, err := time.Parse("scheduled-20060102T150405Z", operation); err != nil {
			return fmt.Errorf("invalid managed S3 backup")
		}
		for _, name := range backupPayloadNames {
			if err := client.RemoveObject(ctx, p.S3.Bucket, p.S3Prefix+"/"+operation+"/"+name, minio.RemoveObjectOptions{}); err != nil {
				return safeS3Error("retention", err)
			}
		}
		receipt.Objects = receipt.Objects[1:]
	}
	return nil
}

// Never propagate response bodies, Authorization headers or access key IDs.
func safeS3Error(action string, err error) error {
	response := minio.ToErrorResponse(err)
	return fmt.Errorf("S3 %s failed (HTTP %d); check connection settings and bucket permissions", action, response.StatusCode)
}
