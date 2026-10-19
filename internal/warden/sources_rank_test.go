package warden

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Card 4 part B (Robin, 2026-10-19): the shipped cap is 60, and a rule taught by
// more review comments wins over an otherwise equal one, in the checklist and in
// the file-ceiling eviction alike.
func TestDefaultCapIsSixty(t *testing.T) {
	assert.Equal(t, 60, DefaultReviewFilterConfig().MaxRules)
}

func TestSourcesScoreSaturates(t *testing.T) {
	assert.Equal(t, 0.0, sourcesScore(0))
	assert.Equal(t, 0.0, sourcesScore(1))
	assert.InDelta(t, 0.5, sourcesScore(2), 1e-9)
	assert.InDelta(t, 2.0/3, sourcesScore(3), 1e-9)
	assert.Less(t, sourcesScore(100), 1.0)
}

func TestARuleTaughtTwiceOutranksAnEqualOne(t *testing.T) {
	once := Rule{ID: "a-once", Category: "style", Pattern: "comment count", Check: "x", Added: "2026-09-01", Source: SourceList{"copilot:PR#1"}, Paths: []string{"api/**/*.cs"}}
	twice := once
	twice.ID, twice.Source = "z-twice", SourceList{"copilot:PR#1", "copilot:PR#2"}
	scores := scoreCandidates([]Rule{once, twice}, nil, []string{"api/Foo.cs"}, []int{1, 1}, []int{2, 2})
	assert.Equal(t, "z-twice", selectRules(scores, 1)[0].rule.ID, "the id would favour a-once; the source count decides")

	active, archived := EvictOverCap([]Rule{once, twice}, 1, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, "z-twice", active[0].ID)
	assert.Equal(t, "a-once", archived[0].ID)
}

func TestTheSourceTermDoesNotBeatANarrowerRule(t *testing.T) {
	broad := Rule{ID: "broad", Category: "style", Pattern: "comment count", Check: "x", Added: "2026-09-01", Source: SourceList{"copilot:PR#1", "copilot:PR#2", "copilot:PR#3"}, Paths: []string{"**/*"}}
	narrow := Rule{ID: "narrow", Category: "style", Pattern: "comment count", Check: "x", Added: "2026-09-01", Source: SourceList{"copilot:PR#4"}, Paths: []string{"api/Fhi.Metadata.Api/Services/**/*.cs"}}
	scores := scoreCandidates([]Rule{broad, narrow}, nil, []string{"api/Fhi.Metadata.Api/Services/Foo.cs"}, []int{1, 1}, []int{2, 2})
	assert.Equal(t, "narrow", selectRules(scores, 1)[0].rule.ID)
}

// Copilot on #950: with different dates, the newest broad rule with three
// sources must not overtake an older scoped rule with one, in the checklist or
// in the eviction. Before the secondary cap it scored 0.5 + 0.2 against 0.6.
func TestSourcesAndRecencyTogetherDoNotBeatAScopedRule(t *testing.T) {
	broad := Rule{ID: "a-broad", Category: "style", Pattern: "comment count", Check: "x", Added: "2026-10-18", Source: SourceList{"copilot:PR#1", "copilot:PR#2", "copilot:PR#3"}, Paths: []string{"**/*"}}
	scoped := Rule{ID: "z-scoped", Category: "style", Pattern: "comment count", Check: "x", Added: "2026-01-01", Source: SourceList{"copilot:PR#4"}, Paths: []string{"api/**/*.cs"}}
	scores := scoreCandidates([]Rule{broad, scoped}, nil, []string{"api/Foo.cs"}, []int{1, 1}, []int{2, 2})
	assert.Equal(t, "z-scoped", selectRules(scores, 1)[0].rule.ID)

	active, archived := EvictOverCap([]Rule{broad, scoped}, 1, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, "z-scoped", active[0].ID, "the eviction keeps the scoped rule too")
	assert.Equal(t, "a-broad", archived[0].ID)
}
