package ghpr

import (
	"testing"
)

func TestExtractPRNumber(t *testing.T) {
	tests := []struct {
		url      string
		expected int
	}{
		{"https://github.com/Robin831/Forge/pull/123", 123},
		{"https://github.com/owner/repo/pull/1", 1},
		{"invalid", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := extractPRNumber(tt.url)
		if got != tt.expected {
			t.Errorf("extractPRNumber(%q) = %d; want %d", tt.url, got, tt.expected)
		}
	}
}
