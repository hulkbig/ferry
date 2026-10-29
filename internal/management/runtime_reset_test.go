package management

import (
	"net/http"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
)

func TestRuntimeResetScopes(t *testing.T) {
	for _, tc := range []struct {
		name, path    string
		body          any
		status, count int
	}{
		{"global", "/api/runtime/clear-cooldown", nil, 200, 2},
		{"provider", "/api/providers/provider-a/clear-cooldown", nil, 200, 2},
		{"connection", "/api/providers/provider-a/connections/connection-a/clear-cooldown", nil, 200, 2},
		{"upstream", "/api/runtime/clear-cooldown", map[string]any{"connection_id": "connection-a", "upstream_model": "upstream-model"}, 200, 1},
		{"logical", "/api/models/smart/clear-cooldown", nil, 200, 1},
		{"missing", "/api/runtime/clear-cooldown", map[string]any{"provider_id": "typo"}, 404, 0},
		{"conflicting", "/api/providers/provider-a/clear-cooldown", map[string]any{"provider_id": "other"}, 400, 0},
		{"unknown-field", "/api/runtime/clear-cooldown", map[string]any{"connection": "typo"}, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newV2APIFixture(t)
			now := time.Now()
			for id := range f.server.deps.Scheduler.Snapshot().Targets {
				f.server.deps.Manager.RecordFailure(id, &domain.Failure{Type: domain.FailRateLimit, RetryAfter: time.Hour}, now)
			}
			rec, body := f.do(http.MethodPost, tc.path, tc.body)
			requireStatus(t, rec, tc.status)
			if tc.status == 200 && body["cleared_models"] != float64(tc.count) {
				t.Fatalf("response=%v", body)
			}
			cleared := 0
			for _, st := range f.server.deps.Manager.States(now) {
				if !st.CooldownUntil.After(now) {
					cleared++
				}
			}
			if cleared != tc.count {
				t.Fatalf("cleared=%d want=%d", cleared, tc.count)
			}
		})
	}
}

func TestConnectionIncarnationSurvivesEditsButNotRecreation(t *testing.T) {
	f := newV2APIFixture(t)
	draft, err := f.control.CreateDraft(1, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	draft, err = f.server.updateV2Draft(draft, func(c *config.File) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	original := draft.Intent.Connections[0]
	if original.RuntimeID == "" {
		t.Fatal("missing internal identity")
	}
	draft, err = f.server.updateV2Draft(draft, func(c *config.File) error {
		c.Connections[0].Identity = "renamed"
		c.Connections[0].RuntimeID = "caller-must-not-choose-this"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Intent.Connections[0].RuntimeID != original.RuntimeID {
		t.Fatal("edit changed incarnation")
	}
	draft, err = f.server.updateV2Draft(draft, func(c *config.File) error { c.Connections = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	draft, err = f.server.updateV2Draft(draft, func(c *config.File) error { c.Connections = []domain.Connection{original}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.control.Draft(draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Intent.Connections[0].RuntimeID == original.RuntimeID {
		t.Fatal("recreated same name reused identity")
	}
}
