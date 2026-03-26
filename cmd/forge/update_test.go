package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
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

func TestPlatformBinaryInArchive(t *testing.T) {
	name := platformBinaryInArchive()
	if runtime.GOOS == "windows" {
		if name != "forge.exe" {
			t.Errorf("platformBinaryInArchive() = %q, want forge.exe on windows", name)
		}
	} else {
		if name != "forge" {
			t.Errorf("platformBinaryInArchive() = %q, want forge on non-windows", name)
		}
	}
}

func TestExtractFromZip(t *testing.T) {
	content := []byte("fake forge binary content")

	// Build a zip archive in memory containing the binary.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.Create("forge")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	// Write to a temp file.
	archiveFile, err := os.CreateTemp("", "forge-test-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archiveFile.Name())
	if _, err := archiveFile.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	archiveFile.Close()

	destFile, err := os.CreateTemp("", "forge-test-dest-*")
	if err != nil {
		t.Fatal(err)
	}
	destFile.Close()
	defer os.Remove(destFile.Name())

	if err := extractFromZip(archiveFile.Name(), "forge", destFile.Name()); err != nil {
		t.Fatalf("extractFromZip() unexpected error: %v", err)
	}

	got, err := os.ReadFile(destFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("extractFromZip() content = %q, want %q", got, content)
	}

	// Missing binary should return error.
	if err := extractFromZip(archiveFile.Name(), "notfound", destFile.Name()); err == nil {
		t.Error("extractFromZip() with missing binary: want error, got nil")
	}
}

func TestExtractFromTarGz(t *testing.T) {
	content := []byte("fake forge binary content")

	// Build a tar.gz archive in memory containing the binary.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	hdr := &tar.Header{
		Name: "forge",
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}

	// Write to a temp file.
	archiveFile, err := os.CreateTemp("", "forge-test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archiveFile.Name())
	if _, err := archiveFile.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	archiveFile.Close()

	destFile, err := os.CreateTemp("", "forge-test-dest-*")
	if err != nil {
		t.Fatal(err)
	}
	destFile.Close()
	defer os.Remove(destFile.Name())

	if err := extractFromTarGz(archiveFile.Name(), "forge", destFile.Name()); err != nil {
		t.Fatalf("extractFromTarGz() unexpected error: %v", err)
	}

	got, err := os.ReadFile(destFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("extractFromTarGz() content = %q, want %q", got, content)
	}

	// Missing binary should return error.
	if err := extractFromTarGz(archiveFile.Name(), "notfound", destFile.Name()); err == nil {
		t.Error("extractFromTarGz() with missing binary: want error, got nil")
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
