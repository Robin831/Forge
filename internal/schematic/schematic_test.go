package schematic

import (
	"testing"

	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRun_DisabledConfig(t *testing.T) {
	cfg := Config{Enabled: false, WordThreshold: 10}
	bead := poller.Bead{Description: "a long description with many many many many many many words here"}
	assert.False(t, ShouldRun(cfg, bead))
}

func TestShouldRun_BelowThreshold(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 100}
	bead := poller.Bead{Description: "Short description"}
	assert.False(t, ShouldRun(cfg, bead))
}

func TestShouldRun_AboveThreshold(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 5}
	bead := poller.Bead{Description: "This is a description with more than five words in it"}
	assert.True(t, ShouldRun(cfg, bead))
}

func TestShouldRun_DecomposeTag(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 1000} // high threshold
	bead := poller.Bead{
		Description: "Short",
			Labels:      []string{"feature", "decompose", "urgent"},
	}
	assert.True(t, ShouldRun(cfg, bead), "decompose tag should override threshold")
}

func TestShouldRun_DecomposeTagCaseInsensitive(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 1000}
	bead := poller.Bead{
		Description: "Short",
			Labels:      []string{"Decompose"},
	}
	assert.True(t, ShouldRun(cfg, bead))
}

func TestParseVerdict_JSONFence(t *testing.T) {
	output := `Here is my analysis:

` + "```json" + `
{
  "action": "plan",
  "plan": "1. Create foo.go\n2. Add tests",
  "reason": "Single focused task"
}
` + "```" + `

That's my verdict.`

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "plan", v.Action)
	assert.Contains(t, v.Plan, "Create foo.go")
	assert.Equal(t, "Single focused task", v.Reason)
}

func TestParseVerdict_PlainFence(t *testing.T) {
	output := "```\n" + `{"action":"decompose","sub_tasks":["Task A","Task B"],"reason":"Too large"}` + "\n```"

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "decompose", v.Action)
	assert.Equal(t, []string{"Task A", "Task B"}, v.SubTasks)
}

func TestParseVerdict_RawJSON(t *testing.T) {
	output := `I think this needs decomposition.
{"action":"clarify","reason":"Missing acceptance criteria"}
That's all.`

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "clarify", v.Action)
	assert.Equal(t, "Missing acceptance criteria", v.Reason)
}

func TestParseVerdict_NoJSON(t *testing.T) {
	output := "I couldn't determine the right approach for this bead."
	_, err := parseVerdict(output)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no valid schematic verdict")
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.False(t, cfg.Enabled)
	assert.Equal(t, 100, cfg.WordThreshold)
	assert.Equal(t, 10, cfg.MaxTurns)
}

func TestBuildPrompt_ContainsBeadInfo(t *testing.T) {
	bead := poller.Bead{
		ID:          "test-123",
		Title:       "Add login feature",
		IssueType:   "feature",
		Priority:    2,
		Description: "Implement OAuth login flow",
	}

	p := buildPrompt(bead)
	assert.Contains(t, p, "test-123")
	assert.Contains(t, p, "Add login feature")
	assert.Contains(t, p, "Implement OAuth login flow")
	assert.Contains(t, p, "plan|decompose|clarify")
}
