package daemon

import (
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
)

func TestShouldDispatch(t *testing.T) {
	tests := []struct {
		name     string
		bead     poller.Bead
		anvilCfg config.AnvilConfig
		expected bool
	}{
		{
			name: "mode all: dispatch everything",
			bead: poller.Bead{ID: "B1"},
			anvilCfg: config.AnvilConfig{
				AutoDispatch: "all",
			},
			expected: true,
		},
		{
			name: "mode empty: defaults to all",
			bead: poller.Bead{ID: "B1"},
			anvilCfg: config.AnvilConfig{
				AutoDispatch: "",
			},
			expected: true,
		},
		{
			name: "mode off: dispatch nothing",
			bead: poller.Bead{ID: "B1"},
			anvilCfg: config.AnvilConfig{
				AutoDispatch: "off",
			},
			expected: false,
		},
		{
			name: "mode tagged: match found",
			bead: poller.Bead{
				ID:   "B1",
				Tags: []string{"forge-auto", "other"},
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:    "tagged",
				AutoDispatchTag: "forge-auto",
			},
			expected: true,
		},
		{
			name: "mode tagged: case-insensitive match",
			bead: poller.Bead{
				ID:   "B1",
				Tags: []string{"FORGE-AUTO"},
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:    "tagged",
				AutoDispatchTag: "forge-auto",
			},
			expected: true,
		},
		{
			name: "mode tagged: no match",
			bead: poller.Bead{
				ID:   "B1",
				Tags: []string{"manual"},
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:    "tagged",
				AutoDispatchTag: "forge-auto",
			},
			expected: false,
		},
		{
			name: "mode tagged: empty tags",
			bead: poller.Bead{
				ID:   "B1",
				Tags: []string{},
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:    "tagged",
				AutoDispatchTag: "forge-auto",
			},
			expected: false,
		},
		{
			name: "mode tagged: empty config tag",
			bead: poller.Bead{
				ID:   "B1",
				Tags: []string{"forge-auto"},
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:    "tagged",
				AutoDispatchTag: "",
			},
			expected: false,
		},
		{
			name: "mode priority: P1 (priority 1) with min-priority P2 (2)",
			bead: poller.Bead{
				ID:       "B1",
				Priority: 1,
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:               "priority",
				AutoDispatchMinPriority: 2,
			},
			expected: true,
		},
		{
			name: "mode priority: P1 (priority 1) with min-priority P1 (1)",
			bead: poller.Bead{
				ID:       "B1",
				Priority: 1,
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:               "priority",
				AutoDispatchMinPriority: 1,
			},
			expected: true,
		},
		{
			name: "mode priority: P3 (priority 3) with min-priority P1 (1)",
			bead: poller.Bead{
				ID:       "B1",
				Priority: 3,
			},
			anvilCfg: config.AnvilConfig{
				AutoDispatch:               "priority",
				AutoDispatchMinPriority: 1,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldDispatch(tt.bead, tt.anvilCfg)
			assert.Equal(t, tt.expected, result)
		})
	}
}
