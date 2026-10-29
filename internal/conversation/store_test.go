package conversation

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestConversationResponseNativeHandleRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	evidence := domain.NewAffinityEvidence("test", domain.AffinityConversation, "session-1", domain.ConfHigh)
	conversation := domain.Conversation{
		ID: "conv_smart_1", LogicalModelID: "smart",
		Items:             []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hi"}}}},
		CurrentProviderID: "provider", CurrentUpstreamModel: "model",
		CurrentConnectionID: "connection", ContinuityMode: domain.ContinuityNative,
		CreatedAt: now, LastActiveAt: now, LastRouteFingerprint: "route",
	}
	response := domain.SmartResponse{
		ID: "resp_smart_1", ConversationID: conversation.ID, RequestID: "req-1",
		CanonicalOutput: []domain.Block{{Kind: domain.BlockText, Text: "hello"}},
		ProviderID:      "provider", UpstreamModel: "model", ConnectionID: "connection",
		NativeResponseID: "resp_native_1", ContinuityMode: domain.ContinuityNative, CreatedAt: now,
	}
	handle := &domain.NativeHandle{
		ConversationID: conversation.ID, ProviderID: "provider", ConnectionID: "connection",
		NativeResponseID: "resp_native_1", StateScope: "CONNECTION", CreatedAt: now, LastUsedAt: now,
	}
	if err := store.SaveCompletion(conversation, response, handle, []string{evidence.KeyHash}); err != nil {
		t.Fatal(err)
	}
	gotConversation, err := store.ConversationForEvidence(conversation.LogicalModelID, evidence.KeyHash)
	if err != nil || gotConversation == nil || gotConversation.ID != conversation.ID || len(gotConversation.Items) != 1 {
		t.Fatalf("conversation = %+v err=%v", gotConversation, err)
	}
	gotResponse, err := store.Response(response.ID)
	if err != nil || gotResponse == nil || gotResponse.NativeResponseID != "resp_native_1" || len(gotResponse.CanonicalOutput) != 1 {
		t.Fatalf("response = %+v err=%v", gotResponse, err)
	}
	gotHandle, err := store.NativeHandle(conversation.ID, "provider", "connection")
	if err != nil || gotHandle == nil || gotHandle.NativeResponseID != "resp_native_1" {
		t.Fatalf("handle = %+v err=%v", gotHandle, err)
	}
}

func TestConversationEvidenceIsIsolatedByLogicalModel(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	evidence := domain.NewAffinityEvidence("shared-client", domain.AffinityConversation, "same-session", domain.ConfHigh)
	for _, model := range []string{"smart-a", "smart-b"} {
		conversation := domain.Conversation{ID: "conv_" + model, LogicalModelID: model, CreatedAt: now, LastActiveAt: now}
		response := domain.SmartResponse{ID: "resp_" + model, ConversationID: conversation.ID, RequestID: "req_" + model, CreatedAt: now}
		if err := store.SaveCompletion(conversation, response, nil, []string{evidence.KeyHash}); err != nil {
			t.Fatal(err)
		}
	}
	for _, model := range []string{"smart-a", "smart-b"} {
		got, err := store.ConversationForEvidence(model, evidence.KeyHash)
		if err != nil || got == nil || got.ID != "conv_"+model {
			t.Fatalf("model %s evidence resolved %+v err=%v", model, got, err)
		}
	}
}

func TestResponseLineageSupportsSiblingBranches(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	root := domain.Conversation{
		ID: "conv_branch", LogicalModelID: "smart", CreatedAt: now, LastActiveAt: now,
		Items: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "root"}}}},
	}
	rootResponse := domain.SmartResponse{ID: "resp_root", ConversationID: root.ID, RequestID: "req_root", CreatedAt: now}
	if err := store.SaveCompletion(root, rootResponse, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"a", "b"} {
		_, atParent, err := store.ConversationAtResponse(rootResponse.ID)
		if err != nil || atParent == nil || len(atParent.Items) != 1 {
			t.Fatalf("parent snapshot before branch %s = %+v err=%v", branch, atParent, err)
		}
		atParent.Items = append(atParent.Items, domain.Message{
			Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: branch}},
		})
		atParent.LastActiveAt = now.Add(time.Duration(len(branch)) * time.Second)
		child := domain.SmartResponse{
			ID: "resp_" + branch, ConversationID: root.ID, ParentResponseID: rootResponse.ID,
			RequestID: "req_" + branch, CreatedAt: atParent.LastActiveAt,
		}
		if err := store.SaveCompletion(*atParent, child, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, branch := range []string{"a", "b"} {
		child, snapshot, err := store.ConversationAtResponse("resp_" + branch)
		if err != nil || child == nil || child.ParentResponseID != rootResponse.ID || snapshot == nil || len(snapshot.Items) != 2 {
			t.Fatalf("branch %s response=%+v snapshot=%+v err=%v", branch, child, snapshot, err)
		}
		if got := snapshot.Items[1].Blocks[0].Text; got != branch {
			t.Fatalf("branch %s inherited sibling state %q", branch, got)
		}
	}
}

func TestConversationRetentionPurgesAggregateInBoundedBatch(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	for _, item := range []struct {
		id   string
		when time.Time
	}{{"old", now.Add(-48 * time.Hour)}, {"fresh", now}} {
		conversation := domain.Conversation{ID: "conv_" + item.id, LogicalModelID: "smart", CreatedAt: item.when, LastActiveAt: item.when}
		response := domain.SmartResponse{ID: "resp_" + item.id, ConversationID: conversation.ID, RequestID: "req_" + item.id, CreatedAt: item.when}
		if err := store.SaveCompletion(conversation, response, nil, []string{"hash_" + item.id}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.PurgeInactive(now.Add(-24*time.Hour), 1)
	if err != nil || removed != 1 {
		t.Fatalf("purge removed=%d err=%v", removed, err)
	}
	if old, _ := store.Conversation("conv_old"); old != nil {
		t.Fatalf("old conversation remained: %+v", old)
	}
	if oldResponse, _ := store.Response("resp_old"); oldResponse != nil {
		t.Fatalf("old response remained: %+v", oldResponse)
	}
	if fresh, _ := store.Conversation("conv_fresh"); fresh == nil {
		t.Fatal("fresh conversation was purged")
	}
}

func TestLegacyEvidenceSchemaMigratesWithoutLosingAliases(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	conversation := domain.Conversation{ID: "conv_legacy", LogicalModelID: "smart", CreatedAt: now, LastActiveAt: now}
	response := domain.SmartResponse{ID: "resp_legacy", ConversationID: conversation.ID, RequestID: "req", CreatedAt: now}
	if err := store.SaveCompletion(conversation, response, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "conversation.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE conversation_evidence;
		CREATE TABLE conversation_evidence (
			key_hash TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, last_seen_at INTEGER NOT NULL);
		INSERT INTO conversation_evidence(key_hash,conversation_id,last_seen_at) VALUES('legacy-hash','conv_legacy',?)`, now.UnixMilli()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.ConversationForEvidence("smart", "legacy-hash")
	if err != nil || got == nil || got.ID != conversation.ID {
		t.Fatalf("migrated alias=%+v err=%v", got, err)
	}
}
