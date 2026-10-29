package management

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/config"
)

func editCodexUpdateConfig(t *testing.T, f *v2APIFixture, edit func(*config.File)) {
	t.Helper()
	f.server.configMu.Lock()
	defer f.server.configMu.Unlock()
	next, err := cloneConfig(f.server.deps.Config)
	if err != nil {
		t.Fatal(err)
	}
	edit(next)
	if err = f.server.publishConfig(next); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAutoUpdateOptInAndManualPriority(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		manual  string
	}{{false, ""}, {false, "0.155.0"}, {true, "0.155.0"}} {
		t.Run(fmt.Sprintf("%t_%s", tc.enabled, tc.manual), func(t *testing.T) {
			f := newV2APIFixture(t)
			editCodexUpdateConfig(t, f, func(c *config.File) { c.CodexAutoUpdate = tc.enabled; c.CodexClientVersion = tc.manual })
			f.server.deps.CodexReleaseHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("disabled/manual config must not contact GitHub")
				return nil, fmt.Errorf("unexpected fetch")
			})}
			before, revision, _ := f.control.ActiveConfig()
			f.server.syncCodexClientVersion(context.Background())
			after, nextRevision, _ := f.control.ActiveConfig()
			if !reflect.DeepEqual(before, after) || revision != nextRevision {
				t.Fatal("manual or disabled config changed")
			}
			state := f.server.codexClientUpdateStatus(after)
			expected := "disabled"
			if tc.manual != "" {
				expected = "manual"
			}
			if state.Status != expected || state.NextRunAt != nil {
				t.Fatalf("status=%+v", state)
			}
		})
	}
}

func TestCodexAutoUpdatePersistsAndSchedulesCatalog(t *testing.T) {
	f := newCodexPrioritySyncFixture(t)
	editCodexUpdateConfig(t, f, func(c *config.File) { c.CodexAutoUpdate = true })
	before, revision, _ := f.control.ActiveConfig()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	f.server.deps.Now = func() time.Time { return now }
	var requests atomic.Int32
	f.server.deps.CodexReleaseHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.String() != codexReleasesURL+"/latest" || r.Header.Get("Authorization") != "" {
			t.Error("wrong public GitHub endpoint or credential leak")
		}
		return modelCatalogResponse(200, `{"tag_name":"rust-v0.161.0","draft":false,"prerelease":false}`), nil
	})}
	f.server.syncCodexClientVersion(context.Background())
	after, nextRevision, _ := f.control.ActiveConfig()
	if after.CodexClientVersion != "" || after.CodexAutoVersion != "0.161.0" || after.EffectiveCodexClientVersion() != "0.161.0" || nextRevision != revision+1 {
		t.Fatalf("after=%+v revision=%d", after, nextRevision)
	}
	if !reflect.DeepEqual(before.LogicalModels, after.LogicalModels) || !reflect.DeepEqual(before.Connections, after.Connections) {
		t.Fatal("catalog or custom routes changed during version publication")
	}
	if providerModelSyncFingerprint(before, "provider-a") == providerModelSyncFingerprint(after, "provider-a") {
		t.Fatal("new version did not invalidate catalog schedule")
	}
	status := f.server.codexClientUpdateStatus(after)
	if status.Status != "succeeded" || status.NextRunAt == nil || !status.NextRunAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("state=%+v", status)
	}
	// A new server instance honors the persisted schedule after restart.
	restarted := New(f.server.deps)
	restarted.syncCodexClientVersion(context.Background())
	if requests.Load() != 1 {
		t.Fatal("restart lost schedule")
	}
	now = now.Add(time.Hour)
	f.server.syncCodexClientVersion(context.Background())
	_, repeatedRevision, _ := f.control.ActiveConfig()
	if requests.Load() != 2 || repeatedRevision != nextRevision {
		t.Fatal("unchanged release created a revision or due run skipped")
	}
	editCodexUpdateConfig(t, f, func(c *config.File) { c.CodexAutoUpdate = false })
	if f.server.currentConfig().EffectiveCodexClientVersion() != "0.161.0" {
		t.Fatal("pausing downgraded version")
	}
}

func TestCodexReleaseSelection(t *testing.T) {
	cases := []struct {
		name, channel, body, want string
		status                    int
	}{
		{"stable", "stable", `{"tag_name":"rust-v0.161.0"}`, "0.161.0", 200},
		{"prerelease", "prerelease", `[{"tag_name":"rust-v0.162.0-alpha.9","prerelease":true},{"tag_name":"rust-v0.162.0-alpha.10","prerelease":true},{"tag_name":"rust-v0.161.9"},{"tag_name":"rust-v9.0.0","draft":true},{"tag_name":"other-v9.0.0"},{"tag_name":"rust-vbogus"}]`, "0.162.0-alpha.10", 200},
		{"stable_promotion", "prerelease", `[{"tag_name":"rust-v0.162.0-alpha.10"},{"tag_name":"rust-v0.162.0"}]`, "0.162.0", 200},
		{"nested_alpha", "prerelease", `[{"tag_name":"rust-v0.162.0-alpha.6"},{"tag_name":"rust-v0.162.0-alpha.6.1"}]`, "0.162.0-alpha.6.1", 200},
		{"wrong_stable", "stable", `{"tag_name":"rust-v0.162.0-alpha.10","prerelease":true}`, "", 200},
		{"malformed", "stable", `{"tag_name":`, "", 200},
		{"empty", "prerelease", `[]`, "", 200},
		{"rate_limit", "stable", `{"message":"rate limited"}`, "", 403},
		{"unavailable", "stable", `{}`, "", 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Deps{CodexReleaseHTTPClient: &http.Client{Transport: modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.channel == "prerelease" && r.URL.Query().Get("per_page") != "100" {
					t.Error("missing release list")
				}
				return modelCatalogResponse(tc.status, tc.body), nil
			})}})
			got, err := s.latestCodexRelease(context.Background(), tc.channel)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
	for _, pair := range [][2]string{{"0.9.0", "0.10.0"}, {"0.160.0-alpha.9", "0.160.0-alpha.10"}, {"0.160.0-alpha.6", "0.160.0-alpha.6.1"}, {"0.160.0-1", "0.160.0-alpha"}, {"0.160.0-alpha", "0.160.0-beta"}, {"0.160.0-alpha.1", "0.160.0"}, {"0.160.0", "1.0.0"}} {
		if compareCodexVersions(pair[0], pair[1]) >= 0 || compareCodexVersions(pair[1], pair[0]) <= 0 || compareCodexVersions(pair[0], pair[0]) != 0 {
			t.Errorf("bad order: %v", pair)
		}
	}
}

func TestCodexAutoUpdateFailureAndNoDowngrade(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		failPublish bool
	}{
		{"rate_limit", 429, `{}`, false},
		{"malformed", 200, `not-json`, false},
		{"older_stable", 200, `{"tag_name":"rust-v0.160.0"}`, false},
		{"publish_failed", 200, `{"tag_name":"rust-v0.162.0"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newV2APIFixture(t)
			editCodexUpdateConfig(t, f, func(c *config.File) { c.CodexAutoUpdate = true; c.CodexAutoVersion = "0.161.0-alpha.3" })
			if tc.failPublish {
				f.server.deps.Publish = func(*config.File) error { return fmt.Errorf("test publish failure") }
			}
			f.server.deps.CodexReleaseHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) { return modelCatalogResponse(tc.status, tc.body), nil })}
			before, revision, _ := f.control.ActiveConfig()
			f.server.syncCodexClientVersion(context.Background())
			after, nextRevision, _ := f.control.ActiveConfig()
			if !reflect.DeepEqual(before, after) || revision != nextRevision {
				t.Fatal("failure/older release mutated config")
			}
			state := f.server.codexClientUpdateStatus(after)
			expected := "failed"
			if tc.name == "older_stable" {
				expected = "succeeded"
			}
			if state.Status != expected || state.NextRunAt == nil {
				t.Fatalf("status=%+v", state)
			}
		})
	}
}

func TestCodexAutoUpdateConcurrentEditsWin(t *testing.T) {
	for _, edit := range []string{"manual", "pause", "channel", "unrelated"} {
		t.Run(edit, func(t *testing.T) {
			f := newV2APIFixture(t)
			editCodexUpdateConfig(t, f, func(c *config.File) { c.CodexAutoUpdate = true })
			started, release := make(chan struct{}), make(chan struct{})
			done := make(chan struct{})
			var requests atomic.Int32
			f.server.deps.CodexReleaseHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				close(started)
				<-release
				return modelCatalogResponse(200, `{"tag_name":"rust-v0.162.0"}`), nil
			})}
			go func() { defer close(done); f.server.syncCodexClientVersion(context.Background()) }()
			<-started
			f.server.syncCodexClientVersion(context.Background()) // overlapping poll must not fetch
			editCodexUpdateConfig(t, f, func(c *config.File) {
				switch edit {
				case "manual":
					c.CodexClientVersion = "0.155.0"
				case "pause":
					c.CodexAutoUpdate = false
				case "channel":
					c.CodexReleaseChannel = "prerelease"
				case "unrelated":
					c.Providers[0].Name = "Renamed during fetch"
				}
			})
			before, revision, _ := f.control.ActiveConfig()
			close(release)
			<-done
			after, nextRevision, _ := f.control.ActiveConfig()
			if !reflect.DeepEqual(before, after) || revision != nextRevision || requests.Load() != 1 {
				t.Fatal("fetch overwrote concurrent edit or duplicate poll ran")
			}
		})
	}
}

func TestCodexAutoUpdateDraftContractAndRestore(t *testing.T) {
	f := newV2APIFixture(t)
	_, revision, _ := f.control.ActiveConfig()
	rec, draft := f.do(http.MethodPost, "/api/drafts", map[string]any{"base_revision": revision, "intent": map[string]any{"codex_auto_update": true, "codex_release_channel": "prerelease", "codex_client_version": "0.155.0"}})
	requireStatus(t, rec, http.StatusCreated)
	id := draft["id"].(string)
	rec, validation := f.do(http.MethodPost, "/api/drafts/"+id+"/validate", map[string]any{})
	requireStatus(t, rec, http.StatusOK)
	if validation["valid"] != true {
		t.Fatalf("validation=%v", validation)
	}
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+id+"/apply", map[string]any{"expected_revision": revision + 1})
	requireStatus(t, rec, http.StatusConflict)
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+id+"/apply", map[string]any{"expected_revision": revision})
	requireStatus(t, rec, http.StatusOK)
	after, nextRevision, _ := f.control.ActiveConfig()
	if !after.CodexAutoUpdate || after.CodexReleaseChannel != "prerelease" || after.CodexClientVersion != "0.155.0" {
		t.Fatal("draft fields not persisted")
	}
	rec, settings := f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, rec, http.StatusOK)
	if settings["codex_auto_update"] != true || settings["codex_release_channel"] != "prerelease" || settings["codex_update_status"].(map[string]any)["status"] != "manual" {
		t.Fatalf("settings=%v", settings)
	}
	rec, restore := f.do(http.MethodPost, fmt.Sprintf("/api/revisions/%d/restore-draft", revision), map[string]any{})
	requireStatus(t, rec, http.StatusCreated)
	rec, _ = f.do(http.MethodPost, "/api/drafts/"+restore["id"].(string)+"/apply", map[string]any{"expected_revision": nextRevision})
	requireStatus(t, rec, http.StatusOK)
	after, _, _ = f.control.ActiveConfig()
	if after.CodexAutoUpdate || after.CodexClientVersion != "" || after.EffectiveCodexReleaseChannel() != "stable" {
		t.Fatal("restore lost original version settings")
	}
	next, _ := cloneConfig(after)
	next.CodexReleaseChannel = "nightly"
	if next.Validate() == nil {
		t.Fatal("invalid release channel accepted")
	}
}

func TestCodexAutoUpdateHandoverKeepsWorkingVersion(t *testing.T) {
	empty := ""
	enabled := true
	for _, auto := range []string{"", "0.154.0", "0.162.0"} {
		c := &config.File{CodexClientVersion: "0.161.0-alpha.3", CodexAutoVersion: auto}
		applyV2IntentPayload(c, v2IntentPayload{CodexClientVersion: &empty, CodexAutoUpdate: &enabled})
		want := "0.161.0-alpha.3"
		if auto == "0.162.0" {
			want = auto
		}
		if c.EffectiveCodexClientVersion() != want || c.CodexClientVersion != "" {
			t.Fatalf("handover downgraded or retained manual pin: %+v", c)
		}
	}
}

func TestCodexAutoUpdateCanClearInvalidDraftPin(t *testing.T) {
	c := &config.File{CodexClientVersion: "invalid-draft", CodexAutoUpdate: true}
	empty := ""
	applyV2IntentPayload(c, v2IntentPayload{CodexClientVersion: &empty})
	if c.CodexClientVersion != "" || c.CodexAutoVersion != "" {
		t.Fatal("invalid draft pin was promoted")
	}
}

func TestCodexAutoUpdateStartAndCancellation(t *testing.T) {
	f := newV2APIFixture(t)
	editCodexUpdateConfig(t, f, func(c *config.File) { c.CodexAutoUpdate = true })
	started := make(chan struct{}, 1)
	completed := make(chan struct{})
	f.server.deps.CodexReleaseHTTPClient = &http.Client{Transport: modelCatalogRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("unbounded release request")
		}
		started <- struct{}{}
		<-r.Context().Done()
		close(completed)
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.server.StartCodexClientUpdate(ctx)
	f.server.StartCodexClientUpdate(ctx)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not check releases")
	}
	cancel()
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel fetch")
	}
	// Wait for the updater's final persistence before fixture cleanup.
	f.server.codexUpdateMu.Lock()
	defer f.server.codexUpdateMu.Unlock()
	if f.server.currentConfig().CodexAutoVersion != "" {
		t.Fatal("cancelled request changed version")
	}
}
