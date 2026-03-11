package main

import "testing"

func TestRepoNameFromURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "release URL",
			input: "https://github.com/Robin831/Forge/releases/tag/v1.0.0",
			want:  "Forge",
		},
		{
			name:  "PR URL",
			input: "https://github.com/Robin831/Hytte/pull/31",
			want:  "Hytte",
		},
		{
			name:  "root repo URL",
			input: "https://github.com/org/myrepo",
			want:  "myrepo",
		},
		{
			name:  "empty URL returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "URL with only owner returns empty",
			input: "https://github.com/org",
			want:  "",
		},
		{
			name:  "invalid URL returns empty",
			input: "not-a-url",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := repoNameFromURL(tc.input)
			if got != tc.want {
				t.Errorf("repoNameFromURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
