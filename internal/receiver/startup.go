package receiver

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DeploymentContract lets Actions reject a legacy receiver before it starts a
// release. Database migrations belong to the normal application process.
const DeploymentContract = "ferry/startup-migrations/v1"

func (e *Environment) stageStartupBootstrap() error {
	source := filepath.Join(e.ServiceDir, "bootstrap")
	materials := make(map[string][]byte)
	for _, name := range []string{"config.yaml", "upstream-key"} {
		if err := requireRootRegular(filepath.Join(source, name), 0o600); err != nil {
			if name == "upstream-key" && os.IsNotExist(err) {
				continue
			}
			return err
		}
		file, err := os.OpenInRoot(source, name)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 1024*1024+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(raw) == 0 || len(raw) > 1024*1024 {
			return fmt.Errorf("invalid startup material %s", name)
		}
		materials[name] = raw
	}
	return writeStartupBootstrap(filepath.Join(e.ServiceDir, "data", ".startup-bootstrap"), materials, appUID, appGID)
}

// Retried staging accepts the same content only. It never rotates an existing
// input, follows a symlink, or exposes a root-owned secret through a bind mount.
func writeStartupBootstrap(dir string, materials map[string][]byte, uid, gid int) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("startup staging must be a private directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for name, raw := range materials {
		if name != "config.yaml" && name != "upstream-key" {
			return fmt.Errorf("unknown startup material")
		}
		if info, err := root.Lstat(name); err == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 1024*1024 {
				return fmt.Errorf("unsafe staged startup material %s", name)
			}
			current, err := root.ReadFile(name)
			if err != nil || !bytes.Equal(current, raw) {
				return fmt.Errorf("staged startup material differs: %s", name)
			}
			if err := root.Chown(name, uid, gid); err != nil {
				return err
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(raw)
		ownErr := file.Chown(uid, gid)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || ownErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("write private startup material %s failed", name)
		}
	}
	return root.Chown(".", uid, gid)
}
