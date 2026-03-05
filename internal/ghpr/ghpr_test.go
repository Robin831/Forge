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

func TestParseRepoURL(t *testing.T) {
	tests := []struct {
		url         string
		wantOwner   string
		wantRepo    string
		wantErr     bool
	}{
		{"https://github.com/Robin831/Forge", "Robin831", "Forge", false},
		{"https://github.com/Robin831/Forge.git", "Robin831", "Forge", false},
		{"git@github.com:Robin831/Forge.git", "Robin831", "Forge", false},
		{"git@github.com:Robin831/Forge", "Robin831", "Forge", false},
		{"https://github.com/owner/repo/extra", "owner", "repo", false},
		{"invalid", "", "", true},
		{"", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, err := ParseRepoURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRepoURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			continue
		}
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("ParseRepoURL(%q) = (%q, %q); want (%q, %q)", tt.url, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}
}

