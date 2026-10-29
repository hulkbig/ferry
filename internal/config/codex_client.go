package config

import (
	"fmt"

	"ferry/internal/domain"
)

// A manual version always wins. Pausing retains the last resolved version.
func (f *File) EffectiveCodexClientVersion() string {
	if f == nil {
		return domain.CodexClientVersion
	}
	if f.CodexClientVersion != "" {
		return f.CodexClientVersion
	}
	return domain.EffectiveCodexClientVersion(f.CodexAutoVersion)
}

func (f *File) EffectiveCodexReleaseChannel() string {
	if f == nil || f.CodexReleaseChannel == "" {
		return "stable"
	}
	return f.CodexReleaseChannel
}

func (f *File) validateCodexVersions() error {
	if err := domain.ValidateCodexClientVersion(f.CodexClientVersion); err != nil {
		return err
	}
	if err := domain.ValidateCodexClientVersion(f.CodexAutoVersion); err != nil {
		return fmt.Errorf("codex_auto_version: %w", err)
	}
	if channel := f.EffectiveCodexReleaseChannel(); channel != "stable" && channel != "prerelease" {
		return fmt.Errorf("codex_release_channel must be stable or prerelease")
	}
	return nil
}
