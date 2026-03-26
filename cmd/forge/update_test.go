package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"0.3.1", "0.3.0", 1},
		{"0.3.0", "0.3.1", -1},
		{"1.2.3", "1.2.3", 0},
	}

	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestStripV(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"v1.2.3", "1.2.3"},
		{"1.2.3", "1.2.3"},
		{"dev", "dev"},
		{"v0.0.1", "0.0.1"},
	}
	for _, tt := range tests {
		got := stripV(tt.in)
		if got != tt.want {
			t.Errorf("stripV(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPlatformAssetName(t *testing.T) {
	name := platformAssetName("1.2.3")
	if !strings.HasPrefix(name, "forge_") {
		t.Errorf("platformAssetName() = %q, want prefix forge_", name)
	}
	// Should contain version, OS, and arch separated by underscores
	if !strings.Contains(name, "_1.2.3_") {
		t.Errorf("platformAssetName() = %q, expected version in name", name)
	}
	// Should end in a known archive extension
	if !strings.HasSuffix(name, ".zip") && !strings.HasSuffix(name, ".tar.gz") {
		t.Errorf("platformAssetName() = %q, expected .zip or .tar.gz suffix", name)
	}
}

func TestVerifyChecksum(t *testing.T) {
	// Write a temp file with known content
	content := []byte("forge binary content")
	tmp, err := os.CreateTemp("", "forge-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	// Compute expected checksum
	h := sha256.Sum256(content)
	hash := hex.EncodeToString(h[:])
	assetName := "forge-linux-amd64"
	checksumBody := fmt.Sprintf("%s  %s\n", hash, assetName)

	// Serve checksums via httptest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checksumBody)
	}))
	defer srv.Close()

	ctx := t.Context()
	if err := verifyChecksum(ctx, tmp.Name(), assetName, srv.URL); err != nil {
		t.Errorf("verifyChecksum() unexpected error: %v", err)
	}

	// Wrong hash should fail
	badBody := strings.Replace(checksumBody, hash[:4], "dead", 1)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, badBody)
	}))
	defer srv2.Close()

	if err := verifyChecksum(ctx, tmp.Name(), assetName, srv2.URL); err == nil {
		t.Error("verifyChecksum() with wrong hash: want error, got nil")
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		in   string
		want [3]int
	}{
		{"1.2.3", [3]int{1, 2, 3}},
		{"0.0.1", [3]int{0, 0, 1}},
		{"10.20.30", [3]int{10, 20, 30}},
		{"dev", [3]int{0, 0, 0}},
	}
	for _, tt := range tests {
		got := parseSemver(tt.in)
		if got != tt.want {
			t.Errorf("parseSemver(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
