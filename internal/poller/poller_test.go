package poller

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBead_UnmarshalJSON(t *testing.T) {
	jsonData := `[
		{
			"id": "BD-1",
			"title": "Test Bead",
			"description": "A test bead",
			"status": "ready",
			"priority": 1,
			"tags": ["forge-auto", "bug"]
		},
		{
			"id": "BD-2",
			"title": "Another Bead",
			"priority": 2
		}
	]`

	var beads []Bead
	err := json.Unmarshal([]byte(jsonData), &beads)
	assert.NoError(t, err)
	assert.Len(t, beads, 2)

	assert.Equal(t, "BD-1", beads[0].ID)
	assert.Equal(t, []string{"forge-auto", "bug"}, beads[0].Tags)

	assert.Equal(t, "BD-2", beads[1].ID)
	assert.Nil(t, beads[1].Tags)
}

func TestBead_Priority(t *testing.T) {
	jsonData := `[
		{"id": "P3", "priority": 3},
		{"id": "P0", "priority": 0},
		{"id": "P1", "priority": 1}
	]`

	var beads []Bead
	err := json.Unmarshal([]byte(jsonData), &beads)
	assert.NoError(t, err)
	assert.Len(t, beads, 3)

	assert.Equal(t, 3, beads[0].Priority)
	assert.Equal(t, 0, beads[1].Priority)
	assert.Equal(t, 1, beads[2].Priority)
}

