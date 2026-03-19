package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateTitle_ASCII(t *testing.T) {
	short := "Short title"
	if got := truncateTitle(short, 50); got != short {
		t.Errorf("short title should be unchanged, got %q", got)
	}

	exact := strings.Repeat("a", 50)
	if got := truncateTitle(exact, 50); got != exact {
		t.Errorf("50-char title should be unchanged, got %q", got)
	}

	long := strings.Repeat("b", 60)
	got := truncateTitle(long, 50)
	if utf8.RuneCountInString(got) != 50 {
		t.Errorf("expected 50 runes, got %d", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected trailing ellipsis, got %q", got)
	}
}

func TestTruncateTitle_MultiByte(t *testing.T) {
	// Each character is 3 bytes in UTF-8; 55 runes = 165 bytes
	title := strings.Repeat("日", 55)
	got := truncateTitle(title, 50)

	if utf8.RuneCountInString(got) != 50 {
		t.Errorf("expected 50 runes, got %d", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected trailing ellipsis, got %q", got)
	}
	// First 47 runes should be the CJK character
	runes := []rune(got)
	for i := range 47 {
		if runes[i] != '日' {
			t.Errorf("rune %d should be '日', got %c", i, runes[i])
		}
	}
}

func TestTruncateTitle_Emoji(t *testing.T) {
	// Emoji like 🔥 are 4 bytes each; 52 runes > 50 threshold
	title := strings.Repeat("🔥", 52)
	got := truncateTitle(title, 50)

	if utf8.RuneCountInString(got) != 50 {
		t.Errorf("expected 50 runes, got %d", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected trailing ellipsis, got %q", got)
	}
}

func TestTruncateTitle_Empty(t *testing.T) {
	if got := truncateTitle("", 50); got != "" {
		t.Errorf("empty string should remain empty, got %q", got)
	}
}

func TestTemperDisplay(t *testing.T) {
	// This tests the temper column logic from the list command
	tests := []struct {
		name   string
		status string
		passed bool
		want   string
	}{
		{"init status", "init", false, "--"},
		{"smith status", "smith", false, "--"},
		{"temper passed", "temper", true, "pass"},
		{"temper failed", "temper", false, "FAIL"},
		{"pr_open passed", "pr_open", true, "pass"},
		{"pr_open failed", "pr_open", false, "FAIL"},
		{"failed status not passed", "failed", false, "FAIL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := temperDisplay(tt.status, tt.passed)
			if got != tt.want {
				t.Errorf("temperDisplay(%q, %v) = %q, want %q", tt.status, tt.passed, got, tt.want)
			}
		})
	}
}

