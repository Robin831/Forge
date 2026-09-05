package depcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
