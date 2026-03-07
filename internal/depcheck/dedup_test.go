package depcheck

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBeadTitle(t *testing.T) {
	got := BeadTitle("Go", "github.com/foo/bar", "v1.2.3", "v1.3.0")
	assert.Equal(t, "Deps(Go): update github.com/foo/bar v1.2.3 → v1.3.0", got)
}

func TestContainsPackageRef_Array(t *testing.T) {
	data := `[{"id":"bd-1","title":"Deps(Go): update github.com/foo/bar v1.0.0 → v1.1.0","description":"","status":"open"}]`
	assert.True(t, containsPackageRef([]byte(data), "github.com/foo/bar"))
	assert.False(t, containsPackageRef([]byte(data), "github.com/baz/qux"))
}

func TestContainsPackageRef_SingleObject(t *testing.T) {
	data := `{"id":"bd-1","title":"Deps(Go): update github.com/foo/bar v1.0.0 → v1.1.0","description":"","status":"open"}`
	assert.True(t, containsPackageRef([]byte(data), "github.com/foo/bar"))
	assert.False(t, containsPackageRef([]byte(data), "github.com/other/pkg"))
}

func TestContainsPackageRef_InDescription(t *testing.T) {
	data := `[{"id":"bd-1","title":"Some update bead","description":"Update github.com/foo/bar to latest","status":"open"}]`
	assert.True(t, containsPackageRef([]byte(data), "github.com/foo/bar"))
}

func TestContainsPackageRef_InvalidJSON(t *testing.T) {
	// Falls back to raw string search.
	data := `not valid json but contains github.com/foo/bar`
	assert.True(t, containsPackageRef([]byte(data), "github.com/foo/bar"))
	assert.False(t, containsPackageRef([]byte(data), "github.com/baz/qux"))
}

func TestMentionsPackage(t *testing.T) {
	b := bdBead{Title: "Deps(Go): update github.com/foo/bar v1.0.0 → v1.1.0"}
	assert.True(t, mentionsPackage(b, "github.com/foo/bar"))
	assert.False(t, mentionsPackage(b, "github.com/other/pkg"))

	b2 := bdBead{Description: "Updates github.com/baz/qux to v2.0.0"}
	assert.True(t, mentionsPackage(b2, "github.com/baz/qux"))
}

func TestParseBeadTime(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"2026-03-07T10:00:00Z", true},
		{"2026-03-07T10:00:00+02:00", true},
		{"2026-03-07T10:00:00", true},
		{"2026-03-07 10:00:00", true},
		{"not-a-time", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			_, err := parseBeadTime(tt.input)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestRecentlyClosedFiltering(t *testing.T) {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -7)

	recentBead := bdBead{
		Title:    "Deps(Go): update github.com/foo/bar v1.0.0 → v1.1.0",
		ClosedAt: now.AddDate(0, 0, -3).Format(time.RFC3339),
	}
	oldBead := bdBead{
		Title:    "Deps(Go): update github.com/foo/bar v1.0.0 → v1.1.0",
		ClosedAt: now.AddDate(0, 0, -10).Format(time.RFC3339),
	}
	otherPkgBead := bdBead{
		Title:    "Deps(Go): update github.com/baz/qux v2.0.0 → v2.1.0",
		ClosedAt: now.AddDate(0, 0, -1).Format(time.RFC3339),
	}
	noTimeBead := bdBead{
		Title: "Deps(Go): update github.com/foo/bar v1.0.0 → v1.1.0",
		// no ClosedAt or UpdatedAt
	}

	// Bead closed 3 days ago — within the 7-day window.
	assert.True(t, isRecentlyClosedBeadAt([]bdBead{recentBead}, "github.com/foo/bar", cutoff))

	// Bead closed 10 days ago — outside the window.
	assert.False(t, isRecentlyClosedBeadAt([]bdBead{oldBead}, "github.com/foo/bar", cutoff))

	// Bead for a different package — should not match.
	assert.False(t, isRecentlyClosedBeadAt([]bdBead{otherPkgBead}, "github.com/foo/bar", cutoff))

	// Mix of old and recent for the same package — recent one matches.
	assert.True(t, isRecentlyClosedBeadAt([]bdBead{oldBead, recentBead}, "github.com/foo/bar", cutoff))

	// Bead with no timestamp — should not match (skip safely).
	assert.False(t, isRecentlyClosedBeadAt([]bdBead{noTimeBead}, "github.com/foo/bar", cutoff))

	// Empty slice — no match.
	assert.False(t, isRecentlyClosedBeadAt(nil, "github.com/foo/bar", cutoff))
}
