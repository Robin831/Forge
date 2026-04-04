package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const encPrefix = "enc:"

// decryptWebhookURLs decrypts any enc:-prefixed webhook URLs in the
// notifications config. Values without the prefix are returned unchanged
// (legacy plaintext). If the encryption key is unavailable or decryption
// fails, the affected URL is cleared and a warning is logged so startup
// continues without crashing — webhook notifications will simply not fire.
func decryptWebhookURLs(cfg *Config) {
	cfg.Notifications.TeamsWebhookURL = decryptURL(cfg.Notifications.TeamsWebhookURL)
	cfg.Notifications.Teams.WebhookURL = decryptURL(cfg.Notifications.Teams.WebhookURL)

	for i, u := range cfg.Notifications.ReleaseWebhookURLs {
		cfg.Notifications.ReleaseWebhookURLs[i] = decryptURL(u)
	}
	for i, u := range cfg.Notifications.PRReadyWebhookURLs {
		cfg.Notifications.PRReadyWebhookURLs[i] = decryptURL(u)
	}
	for i, t := range cfg.Notifications.Webhooks {
		cfg.Notifications.Webhooks[i].URL = decryptURL(t.URL)
	}
}

// decryptURL decrypts a single URL value. Non-enc: values are returned
// unchanged. On failure the empty string is returned and a warning is logged.
func decryptURL(value string) string {
	if !strings.HasPrefix(value, encPrefix) {
		return value
	}
	plaintext, err := decryptField(value)
	if err != nil {
		log.Printf("Warning: failed to decrypt webhook URL (%v) — webhook will be disabled", err)
		return ""
	}
	return plaintext
}

// decryptField decrypts an enc:-prefixed field value.
func decryptField(value string) (string, error) {
	if !strings.HasPrefix(value, encPrefix) {
		return value, nil
	}
	encoded := value[len(encPrefix):]
	key, err := hytteEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("load encryption key: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// hytteEncryptionKey returns the 32-byte AES-256 key used by Hytte to encrypt
// config fields. Key resolution follows the same order as Hytte itself:
//  1. ENCRYPTION_KEY environment variable (SHA-256 hashed to 32 bytes), or
//  2. ~/.config/hytte/.encryption_key (hex-encoded 32 bytes on disk)
func hytteEncryptionKey() ([]byte, error) {
	if raw := os.Getenv("ENCRYPTION_KEY"); raw != "" {
		h := sha256.Sum256([]byte(raw))
		return h[:], nil
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("determine user config directory: %w", err)
	}
	keyPath := filepath.Join(configDir, "hytte", ".encryption_key")

	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key file %s: %w", keyPath, err)
	}
	content := strings.TrimRight(string(data), "\r\n")
	if len(content) != 64 {
		return nil, fmt.Errorf("key file %s has unexpected length %d (expected 64 hex chars)", keyPath, len(content))
	}
	key, err := hex.DecodeString(content)
	if err != nil {
		return nil, fmt.Errorf("key file %s contains invalid hex data: %w", keyPath, err)
	}
	return key, nil
}
