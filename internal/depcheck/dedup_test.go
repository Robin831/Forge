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
	// Verify the time comparison logic: a bead closed 3 days ago is within the 7-day window.
	cutoff := time.Now().AddDate(0, 0, -7)
	threeDaysAgo := time.Now().AddDate(0, 0, -3)
	tenDaysAgo := time.Now().AddDate(0, 0, -10)

	assert.True(t, threeDaysAgo.After(cutoff), "3 days ago should be after the 7-day cutoff")
	assert.False(t, tenDaysAgo.After(cutoff), "10 days ago should be before the 7-day cutoff")
}
