package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "forge-state-fs-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestForgeSessions_CreateAndGet(t *testing.T) {
	db := openTestDB(t)

	s, err := db.CreateForgeSession(ForgeSession{
		Title:     "Refactor poller",
		Anvil:     "anvil-1",
		CreatedBy: "alice",
	})
	if err != nil {
		t.Fatalf("CreateForgeSession: %v", err)
	}
	if s.ID == 0 {
		t.Fatal("expected non-zero ID")
	}
	if s.Status != ForgeSessionStatusDraft {
		t.Errorf("expected default status %q, got %q", ForgeSessionStatusDraft, s.Status)
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		t.Error("expected timestamps to be set")
	}

	got, err := db.GetForgeSession(s.ID)
	if err != nil {
		t.Fatalf("GetForgeSession: %v", err)
	}
	if got == nil {
		t.Fatal("session not found after insert")
	}
	if got.Title != "Refactor poller" || got.Anvil != "anvil-1" || got.CreatedBy != "alice" {
		t.Errorf("unexpected fields: %+v", got)
	}
}

func TestForgeSessions_GetMissingReturnsNil(t *testing.T) {
	db := openTestDB(t)
	got, err := db.GetForgeSession(9999)
	if err != nil {
		t.Fatalf("GetForgeSession: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing session, got %+v", got)
	}
}

func TestForgeSessions_ListOrderAndScope(t *testing.T) {
	db := openTestDB(t)

	// Insert sessions for two different users; the older one should
	// come last in the list, and the bob session should be filtered
	// out when we scope to alice.
	a1, err := db.CreateForgeSession(ForgeSession{Title: "Older alice", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	// Force a small clock skew so updated_at differs.
	time.Sleep(2 * time.Millisecond)
	_, err = db.CreateForgeSession(ForgeSession{Title: "Bob's session", CreatedBy: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	a2, err := db.CreateForgeSession(ForgeSession{Title: "Newer alice", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	all, err := db.ListForgeSessions("", 50)
	if err != nil {
		t.Fatalf("ListForgeSessions: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(all))
	}
	if all[0].ID != a2.ID {
		t.Errorf("expected newest session first, got id=%d", all[0].ID)
	}

	scoped, err := db.ListForgeSessions("alice", 50)
	if err != nil {
		t.Fatalf("ListForgeSessions(alice): %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("expected 2 alice sessions, got %d", len(scoped))
	}
	if scoped[0].ID != a2.ID || scoped[1].ID != a1.ID {
		t.Errorf("unexpected order: %d then %d", scoped[0].ID, scoped[1].ID)
	}
}

func TestForgeSessions_AppendBumpsUpdatedAt(t *testing.T) {
	db := openTestDB(t)

	s, err := db.CreateForgeSession(ForgeSession{Title: "Talk", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	original := s.UpdatedAt
	// Sleep so the updated_at delta is observable in the dbTimeLayout
	// resolution (nanoseconds, but on some platforms the system clock
	// only advances on millisecond ticks).
	time.Sleep(2 * time.Millisecond)

	m, err := db.AppendForgeSessionMessage(ForgeSessionMessage{
		SessionID: s.ID,
		Role:      ForgeMessageRoleUser,
		Content:   "Hello",
	})
	if err != nil {
		t.Fatalf("AppendForgeSessionMessage: %v", err)
	}
	if m.ID == 0 {
		t.Error("expected non-zero message ID")
	}
	if m.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}

	got, err := db.GetForgeSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.After(original) {
		t.Errorf("expected updated_at to advance after appending; was %v, now %v", original, got.UpdatedAt)
	}
}

func TestForgeSessions_ListMessagesInOrder(t *testing.T) {
	db := openTestDB(t)

	s, err := db.CreateForgeSession(ForgeSession{Title: "Conv", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"first", "second", "third"} {
		if _, err := db.AppendForgeSessionMessage(ForgeSessionMessage{
			SessionID: s.ID,
			Role:      ForgeMessageRoleUser,
			Content:   content,
		}); err != nil {
			t.Fatalf("append %s: %v", content, err)
		}
	}

	msgs, err := db.ListForgeSessionMessages(s.ID)
	if err != nil {
		t.Fatalf("ListForgeSessionMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	for i, want := range []string{"first", "second", "third"} {
		if msgs[i].Content != want {
			t.Errorf("msg %d: want %q, got %q", i, want, msgs[i].Content)
		}
	}

	count, err := db.CountForgeSessionMessages(s.ID)
	if err != nil {
		t.Fatalf("CountForgeSessionMessages: %v", err)
	}
	if count != 3 {
		t.Errorf("expected count 3, got %d", count)
	}
}

func TestForgeSessions_UpdateTitleAndStatus(t *testing.T) {
	db := openTestDB(t)
	s, err := db.CreateForgeSession(ForgeSession{Title: "Old", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	newTitle := "Renamed"
	if err := db.UpdateForgeSession(s.ID, &newTitle, nil); err != nil {
		t.Fatalf("UpdateForgeSession (title): %v", err)
	}
	got, err := db.GetForgeSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Renamed" {
		t.Errorf("expected title Renamed, got %q", got.Title)
	}
	if got.Status != ForgeSessionStatusDraft {
		t.Errorf("status should remain draft when not updated, got %q", got.Status)
	}

	archived := ForgeSessionStatusArchived
	if err := db.UpdateForgeSession(s.ID, nil, &archived); err != nil {
		t.Fatalf("UpdateForgeSession (status): %v", err)
	}
	got, err = db.GetForgeSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ForgeSessionStatusArchived {
		t.Errorf("expected archived status, got %q", got.Status)
	}
	if got.Title != "Renamed" {
		t.Errorf("title should remain Renamed when only status updated, got %q", got.Title)
	}
}

func TestForgeSessions_DeleteCascades(t *testing.T) {
	db := openTestDB(t)
	s, err := db.CreateForgeSession(ForgeSession{Title: "Doomed", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendForgeSessionMessage(ForgeSessionMessage{
		SessionID: s.ID,
		Role:      ForgeMessageRoleUser,
		Content:   "hi",
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteForgeSession(s.ID); err != nil {
		t.Fatalf("DeleteForgeSession: %v", err)
	}

	got, err := db.GetForgeSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected session to be deleted, got %+v", got)
	}
	count, err := db.CountForgeSessionMessages(s.ID)
	if err != nil {
		t.Fatalf("CountForgeSessionMessages: %v", err)
	}
	if count != 0 {
		t.Errorf("expected messages to be cascade-deleted, got %d", count)
	}
}

func TestForgeSessions_AppendValidation(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.AppendForgeSessionMessage(ForgeSessionMessage{
		Role:    ForgeMessageRoleUser,
		Content: "no session",
	}); err == nil {
		t.Error("expected error for missing session_id")
	}
	s, err := db.CreateForgeSession(ForgeSession{Title: "S", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendForgeSessionMessage(ForgeSessionMessage{
		SessionID: s.ID,
		Content:   "no role",
	}); err == nil {
		t.Error("expected error for missing role")
	}
}
