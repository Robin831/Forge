package web

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestParseUsers_Empty(t *testing.T) {
	users, err := ParseUsers("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("expected empty map, got %v", users)
	}
}

func TestParseUsers_SingleEntry(t *testing.T) {
	users, err := ParseUsers("alice:$2a$10$abcdefghijklmnopqrstuv0123456789ABCDEFGHIJKLMNOPQ.uvwxyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(users) != 1 || !strings.HasPrefix(users["alice"], "$2a$") {
		t.Fatalf("expected one alice entry with bcrypt hash, got %v", users)
	}
}

func TestParseUsers_MultipleEntries(t *testing.T) {
	raw := "alice:$2a$10$abcdefghijklmnopqrstuv0123456789ABCDEFGHIJKLMNOPQ.uvwxyz, bob:$2a$10$ZYXWVUTSRQPONMLKJIHGFEdcbafedcbafedcbafedcba0123456789012"
	users, err := ParseUsers(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := users["alice"]; !ok {
		t.Errorf("missing alice")
	}
	if _, ok := users["bob"]; !ok {
		t.Errorf("missing bob")
	}
}

func TestParseUsers_Malformed(t *testing.T) {
	cases := []string{
		"alice",
		":nopassword",
		"alice:",
	}
	for _, c := range cases {
		if _, err := ParseUsers(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestParseUsers_Duplicate(t *testing.T) {
	if _, err := ParseUsers("alice:hash1,alice:hash2"); err == nil {
		t.Errorf("expected duplicate user error")
	}
}

func TestVerifyCredentials_OK(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash failed: %v", err)
	}
	users := map[string]string{"alice": string(hash)}
	if err := VerifyCredentials(users, "alice", "hunter2"); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestVerifyCredentials_WrongPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash failed: %v", err)
	}
	users := map[string]string{"alice": string(hash)}
	if err := VerifyCredentials(users, "alice", "wrong"); err != errInvalidCredentials {
		t.Errorf("expected errInvalidCredentials, got %v", err)
	}
}

func TestVerifyCredentials_UnknownUser(t *testing.T) {
	users := map[string]string{"alice": "$2a$10$abcdefghijklmnopqrstuv0123456789ABCDEFGHIJKLMNOPQ.uvwxyz"}
	if err := VerifyCredentials(users, "mallory", "anything"); err != errInvalidCredentials {
		t.Errorf("expected errInvalidCredentials, got %v", err)
	}
}

func TestHashSessionToken_Stable(t *testing.T) {
	h1 := hashSessionToken("abc")
	h2 := hashSessionToken("abc")
	if h1 != h2 {
		t.Errorf("hash should be deterministic")
	}
	if h1 == hashSessionToken("abd") {
		t.Errorf("different inputs should produce different hashes")
	}
}

func TestGenerateSessionToken_Format(t *testing.T) {
	a, err := generateSessionToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(a) != 64 { // 32 bytes -> 64 hex chars
		t.Errorf("unexpected token length %d", len(a))
	}
	b, err := generateSessionToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Errorf("two tokens should differ")
	}
}
