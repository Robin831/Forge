package executil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBdReportsNoSuchBead separates bd's "no such bead" answer from every other
// way a bd show can come back empty, which is what makes a recorded bead id safe
// to drop. bd exits non-zero for all of them.
func TestBdReportsNoSuchBead(t *testing.T) {
	assert.True(t, BdReportsNoSuchBead([]byte(`{"error":"no issues found matching the provided IDs","schema_version":1}`)))
	assert.True(t, BdReportsNoSuchBead([]byte(`Error fetching x: no issue found matching "x"`+"\n"+`{"error":"no issue found matching \"x\""}`)))
	assert.False(t, BdReportsNoSuchBead([]byte(`{"error":"dial tcp 127.0.0.1:3306: connection refused"}`)))
	assert.False(t, BdReportsNoSuchBead([]byte(`{"error":"context deadline exceeded"}`)))
	assert.False(t, BdReportsNoSuchBead(nil))
	assert.False(t, BdReportsNoSuchBead([]byte("bd: command not found")))
}

func TestBdCreatedBeadID(t *testing.T) {
	assert.Equal(t, "bd-42", BdCreatedBeadID([]byte(`{"id":"bd-42","title":"x"}`)))
	// bd may append diagnostics after the JSON object.
	assert.Equal(t, "bd-42", BdCreatedBeadID([]byte(`{"id":"bd-42"}`+"\nwarning: orphan check skipped")))
	assert.Empty(t, BdCreatedBeadID([]byte(`not json`)))
	assert.Empty(t, BdCreatedBeadID(nil))
}

type testBead struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func beadID(b testBead) string { return b.ID }

// bd answers with an array on some versions and a bare object on others, so both
// have to decode — and neither may produce a zero-valued struct read as a bead.
func TestDecodeOneBead(t *testing.T) {
	if got := DecodeOneBead([]byte(`[{"id":"bd-1","title":"a"},{"id":"bd-2"}]`), beadID); assert.NotNil(t, got) {
		assert.Equal(t, "bd-1", got.ID)
	}
	if got := DecodeOneBead([]byte(`{"id":"bd-9","title":"b"}`), beadID); assert.NotNil(t, got) {
		assert.Equal(t, "bd-9", got.ID)
	}

	assert.Nil(t, DecodeOneBead([]byte(`[]`), beadID))
	assert.Nil(t, DecodeOneBead([]byte(`{"error":"no issues found"}`), beadID),
		"an object with no id is bd's error shape, not a bead")
	assert.Nil(t, DecodeOneBead(nil, beadID))
	assert.Nil(t, DecodeOneBead[testBead]([]byte(`{"id":"bd-1"}`), nil))
}
