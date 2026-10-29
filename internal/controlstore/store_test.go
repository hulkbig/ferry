package controlstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
)

func TestConfigHistoryLivesInSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, _, err := s.ActiveConfig(); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("empty ActiveConfig error = %v, want ErrNoConfig", err)
	}

	v1 := &config.File{DefaultPolicy: "first"}
	ver1, err := s.SaveConfig(v1)
	if err != nil || ver1 != 1 {
		t.Fatalf("SaveConfig v1 = (%d,%v)", ver1, err)
	}
	v2 := &config.File{DefaultPolicy: "second"}
	ver2, err := s.SaveConfig(v2)
	if err != nil || ver2 != 2 {
		t.Fatalf("SaveConfig v2 = (%d,%v)", ver2, err)
	}

	got, activeVersion, err := s.ActiveConfig()
	if err != nil || activeVersion != 2 || got.DefaultPolicy != "second" {
		t.Fatalf("ActiveConfig = (%+v,%d,%v)", got, activeVersion, err)
	}
	rolled, rollbackVersion, err := s.RollbackConfig()
	if err != nil || rollbackVersion != 1 || rolled.DefaultPolicy != "first" {
		t.Fatalf("RollbackConfig = (%+v,%d,%v)", rolled, rollbackVersion, err)
	}

	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("control db mode = (%v,%v), want 0600", info, err)
	}
}

func TestRollbackRequiresPreviousVersion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.RollbackConfig(); !errors.Is(err, ErrNoPreviousConfig) {
		t.Fatalf("RollbackConfig error = %v, want ErrNoPreviousConfig", err)
	}
	if _, err := s.SaveConfig(&config.File{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RollbackConfig(); !errors.Is(err, ErrNoPreviousConfig) {
		t.Fatalf("single-version rollback error = %v, want ErrNoPreviousConfig", err)
	}
}

func TestSaveConfigDoesNotCreateDuplicateHistory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cfg := &config.File{DefaultPolicy: "stable"}
	v1, err := s.SaveConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.SaveConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v1 {
		t.Fatalf("duplicate config created version %d, want %d", v2, v1)
	}
	if _, _, err := s.RollbackConfig(); !errors.Is(err, ErrNoPreviousConfig) {
		t.Fatalf("duplicate config created rollback history: %v", err)
	}
}

func TestAPIKeyPolicyAndIrreversibleRevocation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	enabled := true
	if _, err := s.SaveConfig(&config.File{ConsumerKeys: []config.ConsumerKey{{Name: "scoped", Key: "sk-scoped-secret", Enabled: &enabled}}}); err != nil {
		t.Fatal(err)
	}
	identities, _, err := s.APIKeys()
	if err != nil || len(identities) != 1 {
		t.Fatalf("identities=%+v err=%v", identities, err)
	}
	if identities[0].ExactOnly {
		t.Fatal("upgraded API key unexpectedly defaulted to exact-only")
	}
	expires := time.Now().Add(time.Hour)
	if err := s.UpdateAPIKeyPolicy(identities[0].ID, "test", nil, []string{"model-a"}, []domain.Protocol{domain.ProtocolOpenAIResponses}, &expires, 12, 25, true); err != nil {
		t.Fatal(err)
	}
	auth, err := s.APIKeyAuthenticators()
	if err != nil || len(auth) != 1 {
		t.Fatalf("auth=%+v err=%v", auth, err)
	}
	identity := auth[0].Identity
	if !identity.ExactOnly {
		t.Fatalf("exact-only policy was not persisted: %+v", identity)
	}
	revealed, available, err := s.APIKeyActiveSecret(identity.ID)
	if err != nil || !available || revealed != "sk-scoped-secret" {
		t.Fatalf("recoverable active secret=%q available=%v err=%v", revealed, available, err)
	}
	if !identity.Allows("model-a", domain.ProtocolOpenAIResponses, time.Now()) || identity.Allows("model-b", domain.ProtocolOpenAIResponses, time.Now()) || identity.Allows("model-a", domain.ProtocolOpenAIChat, time.Now()) {
		t.Fatalf("scope not enforced: %+v", identity)
	}
	if identity.RateLimitRPM != 12 || identity.MonthlyBudgetUSD != 25 {
		t.Fatalf("limits=%+v", identity)
	}
	if err := s.RevokeAPIKey(identity.ID); err != nil {
		t.Fatal(err)
	}
	if revealed, available, err := s.APIKeyActiveSecret(identity.ID); err != nil || available || revealed != "" {
		t.Fatalf("revoked secret remained revealable: value=%q available=%v err=%v", revealed, available, err)
	}
	if err := s.UpdateAPIKeyPolicy(identity.ID, "test", &enabled, identity.ScopeModels, identity.ScopeProtocols, identity.ExpiresAt, 12, 25, identity.ExactOnly); !errors.Is(err, ErrAPIKeyRevoked) {
		t.Fatalf("revoked key re-enabled: %v", err)
	}
	identities, secrets, err := s.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if identities[0].RevokedAt == nil || identities[0].Enabled {
		t.Fatalf("revoked identity=%+v", identities[0])
	}
	for _, secret := range secrets[identity.ID] {
		if secret.State != domain.APIKeySecretRetired {
			t.Fatalf("secret not retired: %+v", secret)
		}
	}
}

func TestCanReactivateExactVersionAfterRuntimePublishFailure(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	v1, err := s.SaveConfig(&config.File{DefaultPolicy: "first"})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.SaveConfig(&config.File{DefaultPolicy: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RollbackConfig(); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConfig(v2); err != nil {
		t.Fatal(err)
	}
	got, active, err := s.ActiveConfig()
	if err != nil || active != v2 || got.DefaultPolicy != "second" {
		t.Fatalf("reactivated config = (%+v,%d,%v), want exact version %d", got, active, err, v2)
	}
	if v1 == v2 {
		t.Fatal("test requires distinct versions")
	}
}

func TestFailedCandidateIsNotEligibleForFutureRollback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SaveConfig(&config.File{DefaultPolicy: "first"}); err != nil {
		t.Fatal(err)
	}
	stable, err := s.SaveConfig(&config.File{DefaultPolicy: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.SaveConfig(&config.File{DefaultPolicy: "failed-runtime-publish"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreConfig(stable, failed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveConfig(&config.File{DefaultPolicy: "next-success"}); err != nil {
		t.Fatal(err)
	}
	rolled, _, err := s.RollbackConfig()
	if err != nil {
		t.Fatal(err)
	}
	if rolled.DefaultPolicy != "stable" {
		t.Fatalf("rollback selected %q, want last successfully applied config", rolled.DefaultPolicy)
	}
}

func TestPolicyGraphRoundTripsLosslesslyThroughSQLite(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	graph := config.PolicyGraphSpec{
		Version: 1,
		Nodes: []config.PolicyGraphNode{
			{ID: "entry", Type: "input", Data: map[string]any{"label": "请求入口"}, X: 12.5, Y: 28},
			{ID: "primary", Type: "channel", Data: map[string]any{"channelID": "glm", "internalPool": "glm-pool"}, X: 240, Y: 120},
			{ID: "backup", Type: "channel", Data: map[string]any{"channelID": "backup", "internalPool": "backup-pool"}, X: 420, Y: 220},
		},
		Edges: []config.PolicyGraphEdge{{
			ID: "fallback", Source: "primary", SourcePort: "recoverable_failure",
			Target: "backup", TargetPort: "in", Kind: "fallback",
			RecoveryAction: "next_tier", FailureTypes: []string{"rate_limit", "provider_5xx"}, Priority: 7,
		}},
	}
	cfg := &config.File{PolicyGraphs: map[string]config.PolicyGraphSpec{"main": graph}}
	if _, err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PolicyGraphs["main"], graph) {
		t.Fatalf("policy graph changed across SQLite round-trip:\n got: %#v\nwant: %#v", got.PolicyGraphs["main"], graph)
	}
}

func TestLegacyAPIKeyMigrationSanitizesAllHistoryAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	legacy := &config.File{ConsumerKeys: []config.ConsumerKey{{
		Name: "personal", Key: "sk-smartapi-legacy-fixture", Enabled: &on, DefaultAlias: "smart",
	}}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO control_config_versions(created_at,config_json,active) VALUES(?,?,1)`,
		time.Now().UnixMilli(), raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	authenticators, err := s.APIKeyAuthenticators()
	if err != nil {
		t.Fatal(err)
	}
	if len(authenticators) != 1 || authenticators[0].Identity.Name != "personal" ||
		!authenticators[0].Matches("sk-smartapi-legacy-fixture") {
		t.Fatalf("legacy authenticator = %+v", authenticators)
	}
	active, _, err := s.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(active.ConsumerKeys) != 1 || active.ConsumerKeys[0].Key != "" {
		t.Fatalf("active config retained raw key: %+v", active.ConsumerKeys)
	}
	rows, err := s.db.Query(`SELECT config_json FROM control_config_versions`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var history []byte
		if err := rows.Scan(&history); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(history, []byte("sk-smartapi-legacy-fixture")) {
			t.Fatalf("historical config retained raw key: %s", history)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	revealed, available, err := s.APIKeyActiveSecret(authenticators[0].Identity.ID)
	if err != nil || !available || revealed != "sk-smartapi-legacy-fixture" {
		t.Fatalf("migrated secret reveal=%q available=%v err=%v", revealed, available, err)
	}
	var versionsBefore, identitiesBefore, secretsBefore int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM control_config_versions`: &versionsBefore,
		`SELECT COUNT(*) FROM api_keys`:                &identitiesBefore,
		`SELECT COUNT(*) FROM api_key_secrets`:         &secretsBefore,
	} {
		if err := s.db.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM control_config_versions`: versionsBefore,
		`SELECT COUNT(*) FROM api_keys`:                identitiesBefore,
		`SELECT COUNT(*) FROM api_key_secrets`:         secretsBefore,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("idempotent migration count for %q = %d, want %d", query, got, want)
		}
	}
}

func TestAPIKeyRotationKeepsIdentityAndGraceUntilRetired(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	on := true
	first := &config.File{DefaultPolicy: "v1", ConsumerKeys: []config.ConsumerKey{{
		Name: "personal", Key: "sk-smartapi-first-fixture", Enabled: &on,
	}}}
	if _, err := s.SaveConfig(first); err != nil {
		t.Fatal(err)
	}
	second := &config.File{DefaultPolicy: "v2", ConsumerKeys: []config.ConsumerKey{{
		Name: "personal", Key: "sk-ferry-second-fixture", Enabled: &on,
	}}}
	if _, err := s.SaveConfig(second); err != nil {
		t.Fatal(err)
	}
	authenticators, err := s.APIKeyAuthenticators()
	if err != nil {
		t.Fatal(err)
	}
	if len(authenticators) != 1 || !authenticators[0].Matches("sk-smartapi-first-fixture") ||
		!authenticators[0].Matches("sk-ferry-second-fixture") {
		t.Fatalf("ACTIVE + GRACE did not authenticate: %+v", authenticators)
	}
	identityID := authenticators[0].Identity.ID
	if _, _, err := s.RollbackConfig(); err != nil {
		t.Fatal(err)
	}
	afterRollback, err := s.APIKeyAuthenticators()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRollback) != 1 || afterRollback[0].Identity.ID != identityID ||
		!afterRollback[0].Matches("sk-smartapi-first-fixture") || !afterRollback[0].Matches("sk-ferry-second-fixture") {
		t.Fatalf("config rollback changed key identity/secrets: %+v", afterRollback)
	}
	_, secrets, err := s.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	var graceID string
	for _, secret := range secrets[identityID] {
		if secret.State == domain.APIKeySecretGrace {
			graceID = secret.ID
		}
	}
	if graceID == "" {
		t.Fatalf("missing GRACE secret: %+v", secrets[identityID])
	}
	if err := s.RetireAPIKeySecret(identityID, graceID); err != nil {
		t.Fatal(err)
	}
	afterRetire, err := s.APIKeyAuthenticators()
	if err != nil {
		t.Fatal(err)
	}
	if afterRetire[0].Matches("sk-smartapi-first-fixture") || !afterRetire[0].Matches("sk-ferry-second-fixture") {
		t.Fatalf("retired/active authentication = %+v", afterRetire)
	}
}

func TestAPIKeyMatchedSecretUsageAndLastSecretRetirementGuard(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	on := true
	if _, err := s.SaveConfig(&config.File{ConsumerKeys: []config.ConsumerKey{{
		Name: "personal", Key: "sk-only-secret", Enabled: &on,
	}}}); err != nil {
		t.Fatal(err)
	}
	authenticators, err := s.APIKeyAuthenticators()
	if err != nil || len(authenticators) != 1 {
		t.Fatalf("authenticators=%+v err=%v", authenticators, err)
	}
	secretID, matched := authenticators[0].MatchSecret("sk-only-secret")
	if !matched || secretID == "" {
		t.Fatalf("matched secret id=%q matched=%v", secretID, matched)
	}
	usedAt := time.Date(2026, 8, 26, 12, 34, 56, 0, time.UTC)
	if err := s.RecordAPIKeySecretUse(secretID, usedAt); err != nil {
		t.Fatal(err)
	}
	identities, secrets, err := s.APIKeys()
	if err != nil || len(identities) != 1 || len(secrets[identities[0].ID]) != 1 {
		t.Fatalf("api keys=%+v secrets=%+v err=%v", identities, secrets, err)
	}
	if got := secrets[identities[0].ID][0].LastUsedAt; got == nil || !got.Equal(usedAt) {
		t.Fatalf("last_used_at=%v want=%v", got, usedAt)
	}
	if err := s.RetireAPIKeySecret(identities[0].ID, secretID); !errors.Is(err, ErrLastAuthenticatingSecret) {
		t.Fatalf("last secret retirement error=%v", err)
	}
	if after, err := s.APIKeyAuthenticators(); err != nil || !after[0].Matches("sk-only-secret") {
		t.Fatalf("guard changed durable authentication: %+v err=%v", after, err)
	}
}

func TestLegacyAPIKeyIdentityGolden(t *testing.T) {
	if got := legacyAPIKeyID("personal"); got != "key_5bb380691fb3fce2f625a6db" {
		t.Fatal("legacy identity changed", got)
	}
}
