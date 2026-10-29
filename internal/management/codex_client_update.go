package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
)

const codexUpdateStateKey = "codex_client_update_state"
const codexReleasesURL = "https://api.github.com/repos/openai/codex/releases"

type codexClientUpdateState struct {
	Status        string     `json:"status"`
	Fingerprint   string     `json:"fingerprint,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
	LatestVersion string     `json:"latest_version,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

func codexUpdateFingerprint(f *config.File) string {
	return fmt.Sprintf("%t/%s/%s/%s", f.CodexAutoUpdate, f.EffectiveCodexReleaseChannel(), f.CodexClientVersion, f.CodexAutoVersion)
}

func (s *Server) readCodexUpdateState() (codexClientUpdateState, error) {
	state := codexClientUpdateState{Status: "pending"}
	if s.deps.Control == nil {
		return state, fmt.Errorf("control store unavailable")
	}
	raw, found, err := s.deps.Control.RuntimeSetting(codexUpdateStateKey)
	if err != nil {
		return state, err
	}
	if found {
		err = json.Unmarshal([]byte(raw), &state)
	}
	return state, err
}

func (s *Server) codexClientUpdateStatus(f *config.File) codexClientUpdateState {
	state, err := s.readCodexUpdateState()
	if err != nil {
		state = codexClientUpdateState{Status: "unavailable", LastError: "Update state unavailable"}
	}
	if f.CodexClientVersion != "" {
		state.Status, state.NextRunAt = "manual", nil
	} else if !f.CodexAutoUpdate {
		state.Status, state.NextRunAt = "disabled", nil
	} else if err == nil && state.Fingerprint != codexUpdateFingerprint(f) {
		state.Status, state.NextRunAt = "pending", nil
	}
	return state
}

// StartCodexClientUpdate checks the durable hourly schedule. Network work never
// holds configMu; the original revision is checked again before publication.
func (s *Server) StartCodexClientUpdate(ctx context.Context) {
	s.codexUpdateStart.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				s.syncCodexClientVersion(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

func (s *Server) syncCodexClientVersion(ctx context.Context) {
	if ctx.Err() != nil || s.deps.Control == nil || !s.codexUpdateMu.TryLock() {
		return
	}
	defer s.codexUpdateMu.Unlock()
	s.configMu.RLock()
	file := s.deps.Config
	if file == nil || !file.CodexAutoUpdate || file.CodexClientVersion != "" {
		s.configMu.RUnlock()
		return
	}
	revision, err := s.controlVersionLocked()
	s.configMu.RUnlock()
	if err != nil {
		return
	}
	state, err := s.readCodexUpdateState()
	if err != nil {
		log.Printf("Codex client update state unavailable")
		return
	}
	now := s.modelSyncNow()
	fingerprint := codexUpdateFingerprint(file)
	if state.Fingerprint == fingerprint && state.NextRunAt != nil && state.NextRunAt.After(now) {
		return
	}
	state = codexClientUpdateState{Status: "succeeded", Fingerprint: fingerprint, LastCheckedAt: &now}
	nextRun := now.Add(time.Hour)
	state.NextRunAt = &nextRun
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	version, fetchErr := s.latestCodexRelease(requestCtx, file.EffectiveCodexReleaseChannel())
	cancel()
	if fetchErr != nil {
		state.Status, state.LastError = "failed", fetchErr.Error()
	} else {
		state.LatestVersion = version
		s.configMu.Lock()
		currentRevision, readErr := s.controlVersionLocked()
		// Any concurrent revision (including manual pins, pause, restore, and
		// unrelated edits) wins. The next poll retries from the fresh snapshot.
		if readErr != nil || currentRevision != revision || codexUpdateFingerprint(s.deps.Config) != fingerprint {
			state.Status, state.NextRunAt = "pending", nil
		} else if compareCodexVersions(version, file.EffectiveCodexClientVersion()) > 0 {
			next, publishErr := cloneConfig(s.deps.Config)
			if publishErr == nil {
				next.CodexAutoVersion = version
				publishErr = s.publishConfig(next)
			}
			if publishErr != nil {
				state.Status, state.LastError = "failed", "Configuration publish failed"
			} else {
				state.Fingerprint = codexUpdateFingerprint(next)
			}
		}
		s.configMu.Unlock()
	}
	raw, _ := json.Marshal(state)
	if err := s.deps.Control.SetRuntimeSetting(codexUpdateStateKey, string(raw)); err != nil {
		log.Printf("Codex client update status could not be saved")
	}
}

type codexGitHubRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Stable uses GitHub's latest stable release. The prerelease channel considers
// the most recent 100 releases, including stable releases, by semantic version.
func (s *Server) latestCodexRelease(ctx context.Context, channel string) (string, error) {
	endpoint := codexReleasesURL + "/latest"
	if channel == "prerelease" {
		endpoint = codexReleasesURL + "?per_page=100"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Ferry-Codex-Version-Sync")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := s.deps.CodexReleaseHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("GitHub release request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub releases returned HTTP %d", response.StatusCode)
	}
	const limit = 8 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		return "", fmt.Errorf("GitHub release response unavailable or too large")
	}
	var releases []codexGitHubRelease
	if channel == "prerelease" {
		err = json.Unmarshal(body, &releases)
	} else {
		var release codexGitHubRelease
		err = json.Unmarshal(body, &release)
		releases = []codexGitHubRelease{release}
	}
	if err != nil {
		return "", fmt.Errorf("Invalid GitHub release response")
	}
	best := ""
	for _, release := range releases {
		if release.Draft || !strings.HasPrefix(release.Tag, "rust-v") {
			continue
		}
		version := strings.TrimPrefix(release.Tag, "rust-v")
		if version == "" || domain.ValidateCodexClientVersion(version) != nil {
			continue
		}
		if channel == "stable" && (release.Prerelease || strings.Contains(version, "-")) {
			continue
		}
		if best == "" || compareCodexVersions(version, best) > 0 {
			best = version
		}
	}
	if best == "" {
		return "", fmt.Errorf("No compatible Codex release found")
	}
	return best, nil
}

// Compare numeric identifiers as strings to avoid overflow. Callers supply
// versions validated by the config/release boundary (including alpha.6.1).
func compareCodexVersions(a, b string) int {
	aCore, aPre, aHasPre := strings.Cut(a, "-")
	bCore, bPre, bHasPre := strings.Cut(b, "-")
	ac, bc := strings.Split(aCore, "."), strings.Split(bCore, ".")
	for i := 0; i < 3; i++ {
		if cmp := compareCodexNumeric(ac[i], bc[i]); cmp != 0 {
			return cmp
		}
	}
	if !aHasPre && !bHasPre {
		return 0
	}
	if !aHasPre {
		return 1
	}
	if !bHasPre {
		return -1
	}
	ap, bp := strings.Split(aPre, "."), strings.Split(bPre, ".")
	numeric := func(s string) bool { return strings.Trim(s, "0123456789") == "" }
	for i := 0; i < len(ap) && i < len(bp); i++ {
		an, bn := numeric(ap[i]), numeric(bp[i])
		cmp := 0
		switch {
		case an && bn:
			cmp = compareCodexNumeric(ap[i], bp[i])
		case an:
			cmp = -1
		case bn:
			cmp = 1
		default:
			cmp = strings.Compare(ap[i], bp[i])
		}
		if cmp != 0 {
			return cmp
		}
	}
	if len(ap) < len(bp) {
		return -1
	}
	if len(ap) > len(bp) {
		return 1
	}
	return 0
}

func compareCodexNumeric(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}
