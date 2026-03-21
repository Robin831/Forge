package ledger

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncateLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		maxLines int
		want     string
	}{
		{
			name:     "no truncation needed",
			input:    "line1\nline2\nline3",
			maxWidth: 20,
			maxLines: 5,
			want:     "line1\nline2\nline3",
		},
		{
			name:     "truncates excess lines replacing last with ellipsis",
			input:    "line1\nline2\nline3\nline4\nline5",
			maxWidth: 20,
			maxLines: 3,
			want:     "line1\nline2\n…",
		},
		{
			name:     "truncates line width",
			input:    "hello world",
			maxWidth: 5,
			maxLines: 5,
			want:     "hell…",
		},
		{
			name:     "single line exactly at limit",
			input:    "hello",
			maxWidth: 5,
			maxLines: 1,
			want:     "hello",
		},
		{
			name:     "single line over limit becomes ellipsis",
			input:    "hello\nworld",
			maxWidth: 20,
			maxLines: 1,
			want:     "…",
		},
		{
			name:     "total lines equal to maxLines",
			input:    "a\nb\nc",
			maxWidth: 20,
			maxLines: 3,
			want:     "a\nb\nc",
		},
		{
			name:     "empty input",
			input:    "",
			maxWidth: 20,
			maxLines: 4,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateLines(tt.input, tt.maxWidth, tt.maxLines)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIndentLines(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		prefix string
		want   string
	}{
		{"single line", "hello", "  ", "  hello"},
		{"multi line", "hello\nworld", "  ", "  hello\n  world"},
		{"empty prefix", "hello\nworld", "", "hello\nworld"},
		{"empty string", "", "  ", "  "},
		{"preserves blank lines", "a\n\nb", ">>", ">>a\n>>\n>>b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := indentLines(tt.input, tt.prefix)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseAIResponse(t *testing.T) {
	t.Run("parses all four sections", func(t *testing.T) {
		output := `Some preamble text.

### Title
Improved title here

### Description
A detailed description
spanning multiple lines.

### Complexity
medium

### AI Effort
~15 minutes with smith
`
		result := parseAIResponse(output)
		assert.Equal(t, "Improved title here", result.Title)
		assert.Equal(t, "A detailed description\nspanning multiple lines.", result.Description)
		assert.Equal(t, "medium", result.Complexity)
		assert.Equal(t, "~15 minutes with smith", result.AIEffort)
	})

	t.Run("empty output returns empty result", func(t *testing.T) {
		result := parseAIResponse("")
		assert.Equal(t, aiImprovementResult{}, result)
	})

	t.Run("missing sections return empty strings", func(t *testing.T) {
		output := "### Title\nOnly a title\n"
		result := parseAIResponse(output)
		assert.Equal(t, "Only a title", result.Title)
		assert.Equal(t, "", result.Description)
		assert.Equal(t, "", result.Complexity)
		assert.Equal(t, "", result.AIEffort)
	})

	t.Run("trims surrounding whitespace from sections", func(t *testing.T) {
		output := "### Title\n  \n  spaced title  \n  \n### Complexity\n  high  \n"
		result := parseAIResponse(output)
		assert.Equal(t, "spaced title", result.Title)
		assert.Equal(t, "high", result.Complexity)
	})

	t.Run("no sections returns empty result", func(t *testing.T) {
		output := "No headers here at all."
		result := parseAIResponse(output)
		assert.Equal(t, aiImprovementResult{}, result)
	})
}

func TestTruncateLinesEllipsisWithinLimit(t *testing.T) {
	// When lines are truncated, the ellipsis replacement must not exceed maxLines.
	input := strings.Repeat("line\n", 10)
	result := truncateLines(strings.TrimSuffix(input, "\n"), 20, 4)
	lines := strings.Split(result, "\n")
	assert.Len(t, lines, 4, "result must have exactly maxLines lines (not maxLines+1)")
	assert.Equal(t, "…", lines[3], "last line must be the ellipsis indicator")
}
