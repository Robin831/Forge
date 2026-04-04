package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"testing"
)

// encryptForTest encrypts plaintext using AES-256-GCM with the given key and
// returns an enc:-prefixed base64 string matching the format Hytte produces.
func encryptForTest(t *testing.T, key []byte, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("create GCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ciphertext)
}

// testKey returns a deterministic 32-byte AES-256 key derived from a fixed
// passphrase for use in tests.
func testKey() []byte {
	h := sha256.Sum256([]byte("forge-test-passphrase"))
	return h[:]
}

func TestDecryptField_Passthrough(t *testing.T) {
	plain := "https://example.com/webhook"
	got, err := decryptField(plain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != plain {
		t.Errorf("got %q, want %q", got, plain)
	}
}

func TestDecryptField_RoundTrip(t *testing.T) {
	key := testKey()
	t.Setenv("ENCRYPTION_KEY", "forge-test-passphrase")

	want := "https://hooks.example.com/secret"
	encrypted := encryptForTest(t, key, want)

	got, err := decryptField(encrypted)
	if err != nil {
		t.Fatalf("decryptField error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDecryptField_InvalidBase64(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "forge-test-passphrase")

	_, err := decryptField("enc:!!!not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64, got nil")
	}
}

func TestDecryptField_CiphertextTooShort(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "forge-test-passphrase")

	// A valid base64 string that decodes to fewer bytes than the GCM nonce size.
	short := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02})
	_, err := decryptField(encPrefix + short)
	if err == nil {
		t.Fatal("expected error for short ciphertext, got nil")
	}
}

func TestDecryptField_WrongKey(t *testing.T) {
	// Encrypt with one key, attempt decryption with a different key.
	rightKey := testKey()
	encrypted := encryptForTest(t, rightKey, "secret-url")

	t.Setenv("ENCRYPTION_KEY", "completely-different-passphrase")

	_, err := decryptField(encrypted)
	if err == nil {
		t.Fatal("expected authentication error when using wrong key, got nil")
	}
}

func TestDecryptURL_Passthrough(t *testing.T) {
	plain := "https://example.com/webhook"
	got := decryptURL(plain)
	if got != plain {
		t.Errorf("got %q, want %q", got, plain)
	}
}

func TestDecryptURL_Success(t *testing.T) {
	key := testKey()
	t.Setenv("ENCRYPTION_KEY", "forge-test-passphrase")

	want := "https://hooks.example.com/secret"
	encrypted := encryptForTest(t, key, want)

	got := decryptURL(encrypted)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDecryptURL_FailureReturnsEmpty(t *testing.T) {
	// No ENCRYPTION_KEY set and no key file — decryption must fail gracefully.
	// Redirect the user config dir to an empty temp directory so any real
	// ~/.config/hytte/.encryption_key on the developer/CI machine is not found.
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	got := decryptURL("enc:bm90dmFsaWQ=")
	if got != "" {
		t.Errorf("expected empty string on failure, got %q", got)
	}
}

func TestDecryptWebhookURLs(t *testing.T) {
	key := testKey()
	t.Setenv("ENCRYPTION_KEY", "forge-test-passphrase")

	wantTeams := "https://teams.example.com/webhook"
	wantRelease := "https://release.example.com/webhook"
	wantPRReady := "https://prready.example.com/webhook"
	wantGeneric := "https://generic.example.com/webhook"
	plainURL := "https://plain.example.com/webhook"

	cfg := &Config{}
	cfg.Notifications.TeamsWebhookURL = encryptForTest(t, key, wantTeams)
	cfg.Notifications.Teams.WebhookURL = plainURL
	cfg.Notifications.ReleaseWebhookURLs = []string{encryptForTest(t, key, wantRelease)}
	cfg.Notifications.PRReadyWebhookURLs = []string{encryptForTest(t, key, wantPRReady)}
	cfg.Notifications.Webhooks = []WebhookTargetConfig{{URL: encryptForTest(t, key, wantGeneric)}}

	decryptWebhookURLs(cfg)

	if cfg.Notifications.TeamsWebhookURL != wantTeams {
		t.Errorf("TeamsWebhookURL: got %q, want %q", cfg.Notifications.TeamsWebhookURL, wantTeams)
	}
	if cfg.Notifications.Teams.WebhookURL != plainURL {
		t.Errorf("Teams.WebhookURL: got %q, want %q", cfg.Notifications.Teams.WebhookURL, plainURL)
	}
	if cfg.Notifications.ReleaseWebhookURLs[0] != wantRelease {
		t.Errorf("ReleaseWebhookURLs[0]: got %q, want %q", cfg.Notifications.ReleaseWebhookURLs[0], wantRelease)
	}
	if cfg.Notifications.PRReadyWebhookURLs[0] != wantPRReady {
		t.Errorf("PRReadyWebhookURLs[0]: got %q, want %q", cfg.Notifications.PRReadyWebhookURLs[0], wantPRReady)
	}
	if cfg.Notifications.Webhooks[0].URL != wantGeneric {
		t.Errorf("Webhooks[0].URL: got %q, want %q", cfg.Notifications.Webhooks[0].URL, wantGeneric)
	}
}
