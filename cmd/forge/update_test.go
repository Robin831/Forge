package main

import (
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
	name := platformAssetName()
	if !strings.HasPrefix(name, "forge-") {
		t.Errorf("platformAssetName() = %q, want prefix forge-", name)
	}
	// Should contain OS and arch
	if !strings.Contains(name, "-") {
		t.Errorf("platformAssetName() = %q, expected OS-arch separator", name)
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
