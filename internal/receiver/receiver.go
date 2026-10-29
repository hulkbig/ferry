// Package receiver implements the project-scoped forced-command boundary used
// by GitHub Actions. Parsing is intentionally separate from execution so an
// SSH key can never turn untrusted command text into a shell invocation.
package receiver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	ReceiverPath        = "/usr/local/libexec/ferry-receiver"
	AuthorizedKeyMarker = "ferry-receiver"
)

// RepositoryIDValue must be set when building the receiver; an unbound binary rejects every release.
var RepositoryIDValue = "0"

var commandPattern = regexp.MustCompile(`^ferry/v1 ([0-9]+) ([0-9a-f]{40}) ([0-9a-f]{64}) ([a-z-]+)(?: ([0-9a-f]{64}))?$`)

var allowedVerbs = map[string]bool{
	"prepare": true, "status": true, "commit": true, "rollback": true,
}

var releaseBundleFiles = []string{
	"compose.yml",
	"worker-README.md",
	"bootstrap-config.template.yaml",
}

func safeArchiveEntryPath(root, name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) ||
		strings.ContainsAny(name, `/\`) || filepath.Base(name) != name {
		return "", fmt.Errorf("archive entry path is unsafe: %q", name)
	}
	joined := filepath.Join(root, name) // #nosec G305 -- name is a separator-free basename and the root-relative identity is checked below.
	relative, err := filepath.Rel(root, joined)
	if err != nil || relative != name {
		return "", fmt.Errorf("archive entry escapes its root: %q", name)
	}
	return joined, nil
}

// Request is the fully validated command envelope bound to one repository,
// immutable release commit and idempotent operation identifier.
type Request struct {
	RepositoryID int64
	ReleaseSHA   string
	OperationID  string
	Verb         string
	Argument     string
}

// ParseOriginalCommand validates SSH_ORIGINAL_COMMAND without tokenizing it
// through a shell. It accepts one canonical representation only.
func ParseOriginalCommand(command string) (Request, error) {
	match := commandPattern.FindStringSubmatch(command)
	if match == nil {
		return Request{}, fmt.Errorf("invalid forced command")
	}
	repositoryID, err := strconv.ParseInt(match[1], 10, 64)
	expectedID, boundErr := strconv.ParseInt(RepositoryIDValue, 10, 64)
	if err != nil || boundErr != nil || expectedID <= 0 || repositoryID != expectedID {
		return Request{}, fmt.Errorf("repository is not authorized")
	}
	verb, argument := match[4], match[5]
	if !allowedVerbs[verb] {
		return Request{}, fmt.Errorf("verb is not authorized")
	}
	if (verb == "prepare") != (argument != "") {
		return Request{}, fmt.Errorf("invalid argument for %s", verb)
	}
	return Request{
		RepositoryID: repositoryID,
		ReleaseSHA:   match[2],
		OperationID:  match[3],
		Verb:         verb,
		Argument:     argument,
	}, nil
}

// RenderAuthorizedKey renders the only supported OpenSSH boundary: source-IP
// restricted, forced command, and no interactive shell or unrelated forwarding.
func RenderAuthorizedKey(publicKey, sourceIP string) (string, error) {
	if sourceIP == "" || strings.ContainsAny(sourceIP, `\" ,`) {
		return "", fmt.Errorf("invalid source IP")
	}
	fields := strings.Fields(publicKey)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", fmt.Errorf("only ssh-ed25519 public keys are supported")
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return "", fmt.Errorf("invalid public key: %w", err)
	}
	return fmt.Sprintf(`restrict,from="%s",command="%s forced" %s %s %s`,
		sourceIP, ReceiverPath, fields[0], fields[1], AuthorizedKeyMarker), nil
}

// ExtractReleaseBundle verifies the streamed artifact digest before writing
// and accepts exactly the three reviewed deployment files. It never honors
// tar paths, links, devices, ownership or mode metadata.
func ExtractReleaseBundle(reader io.Reader, targetDir, expectedDigest string) (err error) {
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(expectedDigest) {
		return fmt.Errorf("invalid bundle digest")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, 4*1024*1024+1))
	if err != nil {
		return fmt.Errorf("read release bundle: %w", err)
	}
	if len(raw) > 4*1024*1024 {
		return fmt.Errorf("release bundle is too large")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != expectedDigest {
		return fmt.Errorf("release bundle digest mismatch")
	}
	if info, statErr := os.Lstat(targetDir); statErr == nil {
		return fmt.Errorf("release directory already exists: mode=%s", info.Mode())
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.MkdirAll(filepath.Dir(targetDir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(targetDir)
		}
	}()

	wanted := make(map[string]bool, len(releaseBundleFiles))
	for _, name := range releaseBundleFiles {
		wanted[name] = true
	}
	seen := map[string]bool{}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("open release bundle: %w", err)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	for {
		header, nextErr := tarReader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("read release archive: %w", nextErr)
		}
		if header.Typeflag != tar.TypeReg || !wanted[header.Name] || seen[header.Name] ||
			filepath.Base(header.Name) != header.Name || header.Size < 0 || header.Size > 2*1024*1024 {
			return fmt.Errorf("release archive contains forbidden entry %q", header.Name)
		}
		payload, readErr := io.ReadAll(io.LimitReader(tarReader, header.Size+1))
		if readErr != nil || int64(len(payload)) != header.Size {
			return fmt.Errorf("read release artifact %s: %v", header.Name, readErr)
		}
		destination, pathErr := safeArchiveEntryPath(targetDir, header.Name)
		if pathErr != nil {
			return pathErr
		}
		if err := os.WriteFile(destination, payload, 0o600); err != nil {
			return err
		}
		seen[header.Name] = true
	}
	for name := range wanted {
		if !seen[name] {
			return fmt.Errorf("release archive is missing %s", name)
		}
	}
	complete = true
	return nil
}
