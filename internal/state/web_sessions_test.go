package state

import (
	"testing"
	"time"
)

func TestDeleteAllWebSessions(t *testing.T) {
	db := openTestDB(t)

	now := time.Now().UTC()
	for _, hash := range []string{"hash-a", "hash-b", "hash-c"} {
		if err := db.CreateWebSession(WebSession{
			TokenHash: hash,
			Username:  "alice",
			CreatedAt: now,
			ExpiresAt: now.Add(time.Hour),
			LastSeen:  now,
		}); err != nil {
			t.Fatalf("create session %s: %v", hash, err)
		}
	}

	n, err := db.DeleteAllWebSessions()
	if err != nil {
		t.Fatalf("delete all: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 rows removed, got %d", n)
	}

	// Every prior session is gone.
	sess, err := db.GetWebSession("hash-a")
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if sess != nil {
		t.Fatalf("expected nil session after revoke, got %+v", sess)
	}

	// Idempotent: a second revoke removes nothing.
	n2, err := db.DeleteAllWebSessions()
	if err != nil {
		t.Fatalf("second delete all: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("expected 0 rows on second revoke, got %d", n2)
	}
}
