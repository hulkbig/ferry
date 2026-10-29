package controlstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ferry/internal/config"
)

func TestRevisionHistoryUsesIndexedRestoreLookupAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Represent an existing installation before the performance migration.
	if _, err := store.db.Exec(`DROP INDEX configuration_drafts_applied_revision`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.db.Query("EXPLAIN QUERY PLAN " + configurationRevisionsQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "configuration_drafts_applied_revision") && strings.Contains(detail, "applied_revision=?") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("revision history would scan applied draft payload rows for every revision")
	}
}

func TestConfigurationDraftLifecycleAndRestoreDoesNotMoveActivePointer(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	v1, err := store.SaveConfig(&config.File{DefaultPolicy: "one"})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.CreateDraft(v1, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	draft.Intent.DefaultPolicy = "two"
	draft, err = store.UpdateDraft(draft.ID, draft.Intent)
	if err != nil || draft.Intent.DefaultPolicy != "two" {
		t.Fatalf("update = %+v %v", draft, err)
	}
	v2, err := store.SaveConfig(draft.Intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDraftApplied(draft.ID, v2); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDraftApplied(draft.ID, v2); err != nil {
		t.Fatalf("idempotent mark applied: %v", err)
	}
	if _, err := store.UpdateDraft(draft.ID, draft.Intent); !errors.Is(err, ErrDraftApplied) {
		t.Fatalf("applied draft remained editable: %v", err)
	}

	historical, err := store.ConfigRevision(v1)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := store.CreateDraft(v2, historical, v1)
	if err != nil {
		t.Fatal(err)
	}
	if restore.RestoreFromRevision != v1 || restore.BaseRevision != v2 {
		t.Fatalf("restore draft = %+v", restore)
	}
	active, activeRevision, err := store.ActiveConfig()
	if err != nil || activeRevision != v2 || active.DefaultPolicy != "two" {
		t.Fatalf("restore draft moved active pointer: revision=%d config=%+v err=%v", activeRevision, active, err)
	}

	revisions, err := store.ConfigRevisions()
	if err != nil || len(revisions) != 2 || !revisions[0].Active {
		t.Fatalf("revisions = %+v err=%v", revisions, err)
	}
}

func TestOpenDraftCanBeDeletedButAppliedDraftCannot(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	revision, err := store.SaveConfig(&config.File{})
	if err != nil {
		t.Fatal(err)
	}
	open, err := store.CreateDraft(revision, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDraft(open.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Draft(open.ID); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("deleted draft lookup = %v", err)
	}
	applied, err := store.CreateDraft(revision, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDraftApplied(applied.ID, revision); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDraft(applied.ID); !errors.Is(err, ErrDraftApplied) {
		t.Fatalf("delete applied = %v", err)
	}
}
