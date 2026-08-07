package web

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

var tokenSecret = []byte("a-32-byte-test-secret-000000000")

func TestPreviewToken_RoundTrip(t *testing.T) {
	now := time.Now()
	tok := mintPreviewToken(tokenSecret, "forge-abc1", now.Add(previewTokenTTL))
	if err := verifyPreviewToken(tokenSecret, tok, "forge-abc1", now); err != nil {
		t.Fatalf("freshly minted token must verify: %v", err)
	}
}

func TestPreviewToken_Expiry(t *testing.T) {
	// Whole seconds: the token stores a unix second, so a fractional expiry
	// would land a hair before the deadline it was minted with.
	exp := time.Unix(time.Now().Add(previewTokenTTL).Unix(), 0)
	tok := mintPreviewToken(tokenSecret, "forge-abc1", exp)

	// Still valid on the deadline itself — the check is "after", so a token
	// does not die a second early.
	if err := verifyPreviewToken(tokenSecret, tok, "forge-abc1", exp); err != nil {
		t.Fatalf("token must be valid up to its expiry: %v", err)
	}
	if err := verifyPreviewToken(tokenSecret, tok, "forge-abc1", exp.Add(time.Second)); !errors.Is(err, errTokenExpired) {
		t.Fatalf("expired token: got %v, want errTokenExpired", err)
	}
}

func TestPreviewToken_LabelMismatch(t *testing.T) {
	now := time.Now()
	tok := mintPreviewToken(tokenSecret, "forge-abc1", now.Add(previewTokenTTL))
	err := verifyPreviewToken(tokenSecret, tok, "forge-zzz9", now)
	if !errors.Is(err, errTokenLabelMismatch) {
		t.Fatalf("token for another preview: got %v, want errTokenLabelMismatch", err)
	}
}

// An expired token for the wrong label reports the expiry, not the label: the
// signature and the deadline are checked before anything the payload claims is
// compared against the request.
func TestPreviewToken_ExpiryBeatsLabelMismatch(t *testing.T) {
	issued := time.Now()
	tok := mintPreviewToken(tokenSecret, "forge-abc1", issued.Add(time.Minute))
	err := verifyPreviewToken(tokenSecret, tok, "forge-zzz9", issued.Add(time.Hour))
	if !errors.Is(err, errTokenExpired) {
		t.Fatalf("got %v, want errTokenExpired", err)
	}
}

func TestPreviewToken_TamperedAndMalformed(t *testing.T) {
	now := time.Now()
	valid := mintPreviewToken(tokenSecret, "forge-abc1", now.Add(previewTokenTTL))
	parts := strings.Split(valid, ".")
	if len(parts) != 2 {
		t.Fatalf("token wire form should be payload.mac, got %q", valid)
	}
	// A payload rewritten to a far-future expiry, keeping the original MAC.
	forged := base64.RawURLEncoding.EncodeToString([]byte("forge-abc1|"+
		"99999999999")) + "." + parts[1]

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"no separator", parts[0]},
		{"too many separators", valid + ".extra"},
		{"payload not base64", "!!!." + parts[1]},
		{"mac not base64", parts[0] + ".!!!"},
		{"mac wrong length", parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("short"))},
		{"payload rewritten under the original mac", forged},
		{"payload has no expiry field", base64.RawURLEncoding.EncodeToString([]byte("forge-abc1")) + "." + parts[1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyPreviewToken(tokenSecret, tc.token, "forge-abc1", now); !errors.Is(err, errTokenMalformed) {
				t.Fatalf("got %v, want errTokenMalformed", err)
			}
		})
	}
}

// A token minted by another daemon (or by this one before a restart) is not
// distinguishable from a forgery, and must not be treated as one that merely
// expired.
func TestPreviewToken_WrongSecret(t *testing.T) {
	now := time.Now()
	tok := mintPreviewToken([]byte("some-other-daemons-secret-value"), "forge-abc1", now.Add(previewTokenTTL))
	if err := verifyPreviewToken(tokenSecret, tok, "forge-abc1", now); !errors.Is(err, errTokenMalformed) {
		t.Fatalf("got %v, want errTokenMalformed", err)
	}
}

// The wire form has to survive a query string untouched, which is the whole
// reason for raw-URL base64 rather than the standard alphabet.
func TestPreviewToken_IsURLSafe(t *testing.T) {
	tok := mintPreviewToken(tokenSecret, "forge-abc1", time.Now().Add(previewTokenTTL))
	if strings.ContainsAny(tok, "+/=&?# ") {
		t.Fatalf("token %q contains a character that needs escaping in a URL", tok)
	}
}
