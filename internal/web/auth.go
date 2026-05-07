package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ParseUsersFromEnv parses the FORGE_USERS environment variable. The format
// is a comma-separated list of user:bcrypt-hash entries:
//
//	FORGE_USERS=alice:$2a$10$abcd...,bob:$2a$10$wxyz...
//
// Whitespace around entries is trimmed. Bcrypt hashes contain ':' inside
// their cost/salt section, so ParseUsersFromEnv splits on the FIRST ':' per
// entry and treats everything after it as the hash.
//
// Returns an empty map when the env var is unset or empty. An entry with a
// blank username or hash returns an error.
func ParseUsersFromEnv() (map[string]string, error) {
	return ParseUsers(os.Getenv("FORGE_USERS"))
}

// ParseUsers parses a FORGE_USERS-formatted string. Exposed separately from
// ParseUsersFromEnv to make testing simpler.
func ParseUsers(raw string) (map[string]string, error) {
	out := make(map[string]string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.Index(entry, ":")
		if idx <= 0 {
			return nil, fmt.Errorf("web: malformed FORGE_USERS entry %q (expected user:hash)", entry)
		}
		user := strings.TrimSpace(entry[:idx])
		hash := strings.TrimSpace(entry[idx+1:])
		if user == "" || hash == "" {
			return nil, fmt.Errorf("web: empty user or hash in FORGE_USERS entry %q", entry)
		}
		if _, dup := out[user]; dup {
			return nil, fmt.Errorf("web: duplicate user %q in FORGE_USERS", user)
		}
		out[user] = hash
	}
	return out, nil
}

// errInvalidCredentials is returned by VerifyCredentials when the username
// is unknown or the password does not match.
var errInvalidCredentials = errors.New("invalid credentials")

// VerifyCredentials returns nil when the password matches the bcrypt hash
// stored for the user. Returns errInvalidCredentials for unknown users or
// password mismatches; this avoids leaking which half of the pair was wrong.
func VerifyCredentials(users map[string]string, username, password string) error {
	hash, ok := users[username]
	if !ok {
		// Run a dummy bcrypt comparison against a known hash so unknown
		// usernames take the same time as wrong passwords. Without this
		// an attacker can enumerate valid usernames by timing.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$abcdefghijklmnopqrstuv0123456789ABCDEFGHIJKLMNOPQ.uvwxyz"), []byte(password))
		return errInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return errInvalidCredentials
	}
	return nil
}

// generateSessionToken returns a fresh, hex-encoded 32-byte random token.
func generateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashSessionToken returns the SHA-256 hex digest of the raw session token.
// Only the hash is stored in the database so a leaked DB does not expose
// usable cookies.
func hashSessionToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
