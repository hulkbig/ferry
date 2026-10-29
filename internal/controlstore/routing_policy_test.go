package controlstore

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"ferry/internal/config"
	"ferry/internal/domain"
)

func TestRoutingPolicySchemaAndIndependentRevisions(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, table := range []string{"policies", "policy_revisions", "policy_set_versions", "policy_set_members"} {
		var name string
		if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}

	cfg := routingConfig()
	if _, err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	assertPolicyCounts(t, store.db, 2, 2, 1)
	if got := cfg.RoutingPolicies[0].Revision; got != 1 {
		t.Fatalf("initial revision = %d, want 1", got)
	}
	active, _, err := store.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if active.RoutingPolicies[0].Priority != 7 || active.RoutingPolicies[1].Priority != 33 {
		t.Fatalf("normalized set lost exact priorities: %#v", active.RoutingPolicies)
	}

	// Reordering and toggling are policy-set metadata changes; neither may
	// invent a new immutable content revision.
	cfg.RoutingPolicies[0].Enabled = false
	cfg.RoutingPolicies[1].Priority = 10
	cfg.RoutingPolicies[0].Priority = 20
	cfg.RoutingPolicies[0], cfg.RoutingPolicies[1] = cfg.RoutingPolicies[1], cfg.RoutingPolicies[0]
	if _, err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	assertPolicyCounts(t, store.db, 2, 2, 2)

	// Editing semantic content creates one revision for exactly that policy.
	cfg.RoutingPolicies[0].Route.Steps[0].ChannelGroup = "changed"
	if _, err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	assertPolicyCounts(t, store.db, 2, 3, 3)
	if got := cfg.RoutingPolicies[0].Revision; got != 2 {
		t.Fatalf("edited revision = %d, want 2", got)
	}
	revisions, err := store.PolicyRevisions("catch-all")
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 || revisions[0].Revision != 2 || revisions[1].Revision != 1 {
		t.Fatalf("revision history = %#v", revisions)
	}
	first, err := store.PolicyRevision("catch-all", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Route.Steps[0].ChannelGroup != "secondary" {
		t.Fatalf("revision 1 route = %#v", first.Route)
	}

	// The JSON envelope cannot override normalized policy content.
	var activeVersion int64
	if err := store.db.QueryRow(`SELECT version FROM control_config_versions WHERE active=1`).Scan(&activeVersion); err != nil {
		t.Fatal(err)
	}
	var envelope config.File
	var envelopeRaw []byte
	if err := store.db.QueryRow(`SELECT config_json FROM control_config_versions WHERE version=?`, activeVersion).Scan(&envelopeRaw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelopeRaw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.RoutingPolicies = nil
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE control_config_versions SET config_json=? WHERE version=?`, raw, activeVersion); err != nil {
		t.Fatal(err)
	}
	active, _, err = store.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(active.RoutingPolicies) != 2 || active.RoutingPolicies[0].ID != "catch-all" || active.RoutingPolicies[0].Revision != 2 {
		t.Fatalf("normalized policy hydration = %#v", active.RoutingPolicies)
	}
}

func TestLegacyPolicyMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &config.File{
		DefaultPolicy: "main",
		Policies: map[string]config.PolicyYAML{
			"main": {
				Timezone: "Asia/Shanghai",
				Tiers:    []domain.TierDef{{Name: "primary", Pool: "primary"}, {Name: "fallback", Pool: "secondary"}},
				Recovery: map[string]domain.RecoveryAction{
					string(domain.FailConnect): {NextTier: true},
				},
			},
		},
	}
	if _, err := store.SaveConfig(legacy); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	migrated, versionA, err := store.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated.RoutingPolicies) != 1 || migrated.RoutingPolicies[0].ID != "main" || !migrated.RoutingPolicies[0].Match.IsCatchAll() {
		t.Fatalf("migration result = %#v", migrated.RoutingPolicies)
	}
	store.Close()

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, versionB, err := store.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if versionB != versionA {
		t.Fatalf("second open created another migration version: %d -> %d", versionA, versionB)
	}
}

func TestPolicyMemberPrioritySchemaUpgradeBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE policy_set_members (
		set_version INTEGER NOT NULL, position INTEGER NOT NULL,
		policy_id TEXT NOT NULL, policy_revision INTEGER NOT NULL,
		enabled INTEGER NOT NULL
	); INSERT INTO policy_set_members VALUES(1,2,'legacy',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var priority int
	if err := store.db.QueryRow(`SELECT priority FROM policy_set_members WHERE policy_id='legacy'`).Scan(&priority); err != nil {
		t.Fatal(err)
	}
	if priority != 30 {
		t.Fatalf("backfilled priority = %d, want 30", priority)
	}
}

func TestPolicySetRollbackRestoresDisplayMetadataWithoutNewRevision(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := routingConfig()
	if _, err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.RoutingPolicies[0].Name = "Renamed"
	cfg.RoutingPolicies[0].Description = "new display metadata"
	if _, err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RoutingPolicies[0].Revision != 1 {
		t.Fatalf("metadata-only edit created revision %d", cfg.RoutingPolicies[0].Revision)
	}
	rolled, _, err := store.RollbackConfig()
	if err != nil {
		t.Fatal(err)
	}
	if rolled.RoutingPolicies[0].Name != "Specific" || rolled.RoutingPolicies[0].Description != "" {
		t.Fatalf("rollback metadata = %#v", rolled.RoutingPolicies[0])
	}
}

func routingConfig() *config.File {
	return &config.File{RoutingPolicies: []domain.RoutingPolicySpec{
		{ID: "specific", Name: "Specific", Priority: 7, Enabled: true, Match: domain.RoutingPolicyMatch{Models: []string{"glm-5"}}, Route: routeGroupStore("primary")},
		{ID: "catch-all", Name: "Fallback", Priority: 33, Enabled: true, Route: routeGroupStore("secondary")},
	}}
}

func routeGroupStore(group string) domain.RoutingPolicyRoute {
	return domain.RoutingPolicyRoute{Steps: []domain.RoutingPolicyStep{{ChannelGroup: group}}}
}

func assertPolicyCounts(t *testing.T, db *sql.DB, policies, revisions, sets int) {
	t.Helper()
	for table, want := range map[string]int{"policies": policies, "policy_revisions": revisions, "policy_set_versions": sets} {
		var got int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
}
