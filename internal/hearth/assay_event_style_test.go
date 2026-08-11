package hearth

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestEventTypeColorAssayTerminals: the three terminal Assay events are what
// close out a review in the feed, so each has to read as its own outcome at a
// glance. assay_completed matched none of the success substrings before and sat
// in the neutral default; assay_partial still would, and it is precisely the
// outcome that must not read like an ordinary informational row.
func TestEventTypeColorAssayTerminals(t *testing.T) {
	tests := []struct {
		typ  string
		want lipgloss.AdaptiveColor
	}{
		{"assay_completed", colorSuccess},
		{"assay_partial", colorWarning},
		{"assay_failed", colorDanger},
		// Matching "complete" rather than "completed" is what brings this one
		// in; toast.go has always treated it as a success.
		{"crucible_complete", colorSuccess},
		// Unchanged neighbours — the new rules must not recolour them.
		{"warden_pass", colorSuccess},
		{"pr_merged", colorSuccess},
		{"smith_failed", colorDanger},
		{"bead_claimed", colorInfo},
	}
	for _, tt := range tests {
		if got := eventTypeColor(tt.typ); got != tt.want {
			t.Errorf("eventTypeColor(%q) = %v; want %v", tt.typ, got, tt.want)
		}
	}
}
