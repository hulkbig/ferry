package config

import (
	"testing"

	"ferry/internal/domain"
)

func TestEffectiveCodexVersionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *File
		want string
	}{
		{"nil", nil, domain.CodexClientVersion},
		{"legacy_empty", &File{}, domain.CodexClientVersion},
		{"manual", &File{CodexClientVersion: "0.154.0", CodexAutoVersion: "0.161.0", CodexAutoUpdate: true}, "0.154.0"},
		{"auto", &File{CodexAutoVersion: "0.161.0", CodexAutoUpdate: true}, "0.161.0"},
		{"paused", &File{CodexAutoVersion: "0.161.0"}, "0.161.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.file.EffectiveCodexClientVersion(); got != tc.want {
				t.Fatalf("got=%s want=%s", got, tc.want)
			}
		})
	}
	for _, file := range []*File{{CodexClientVersion: "bad"}, {CodexAutoVersion: "bad"}, {CodexReleaseChannel: "unknown"}} {
		if file.validateCodexVersions() == nil {
			t.Fatal("invalid version settings accepted")
		}
	}
}
