package main

import (
	"reflect"
	"testing"
)

func TestMatchAnvil(t *testing.T) {
	anvils := []string{"forge", "heimdall", "metadata"}

	cases := []struct {
		name   string
		title  string
		anvils []string
		want   []string
	}{
		{
			name:   "single exact match",
			title:  "Refactor poller in Forge",
			anvils: anvils,
			want:   []string{"forge"},
		},
		{
			name:   "case insensitive",
			title:  "HEIMDALL crash on startup",
			anvils: anvils,
			want:   []string{"heimdall"},
		},
		{
			name:   "substring match",
			title:  "Improve metadata-sync error handling",
			anvils: anvils,
			want:   []string{"metadata"},
		},
		{
			name:   "multiple matches",
			title:  "Wire forge into heimdall pipeline",
			anvils: anvils,
			want:   []string{"forge", "heimdall"},
		},
		{
			name:   "no match",
			title:  "Cleanup unrelated TODOs",
			anvils: anvils,
			want:   []string{},
		},
		{
			name:   "empty title",
			title:  "",
			anvils: anvils,
			want:   []string{},
		},
		{
			name:   "empty anvil names are skipped",
			title:  "Anything goes",
			anvils: []string{"", "forge"},
			want:   []string{},
		},
		{
			name:   "preserves input order",
			title:  "metadata then forge",
			anvils: []string{"forge", "heimdall", "metadata"},
			want:   []string{"forge", "metadata"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchAnvil(tc.title, tc.anvils)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("matchAnvil(%q, %v) = %v, want %v", tc.title, tc.anvils, got, tc.want)
			}
		})
	}
}
