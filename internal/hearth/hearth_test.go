package hearth

import (
	"reflect"
	"testing"
)

func TestWordWrapCount(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     int
	}{
		{"short text", "hello world", 20, 1},
		{"simple wrap", "hello world", 5, 2},
		{"wrap at space", "this is a test", 7, 2},
		{"force wrap no spaces", "alongwordthatmustwrap", 10, 3},
		{"newlines respected", "line one\nline two", 20, 2},
		{"empty input", "", 10, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wordWrapCount(tt.input, tt.maxWidth)
			want := len(wordWrap(tt.input, tt.maxWidth))
			if got != want {
				t.Errorf("wordWrapCount() = %d, wordWrap produced %d lines (expected %d)", got, want, tt.want)
			}
			if got != tt.want {
				t.Errorf("wordWrapCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEventLineCountMatchesRender(t *testing.T) {
	m := Model{eventScroll: 0}
	events := []EventItem{
		{Timestamp: "12:00:00", Type: "bead_claimed", Message: "short message", BeadID: "bd-1"},
		{Timestamp: "12:00:01", Type: "smith_done", Message: "a longer message that might wrap depending on the panel width", BeadID: ""},
		{Timestamp: "12:00:02", Type: "warden_pass", Message: "", BeadID: "bd-2"},
	}

	for _, width := range []int{40, 80, 120} {
		for _, ev := range events {
			rendered := m.renderEventLines(ev, false, width)
			counted := m.eventLineCount(ev, width)
			if len(rendered) != counted {
				t.Errorf("width=%d event=%q: renderEventLines=%d lines, eventLineCount=%d",
					width, ev.Message, len(rendered), counted)
			}
		}
	}
}

func TestWordWrap(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     []string
	}{
		{
			name:     "short text",
			input:    "hello world",
			maxWidth: 20,
			want:     []string{"hello world"},
		},
		{
			name:     "simple wrap",
			input:    "hello world",
			maxWidth: 5,
			want:     []string{"hello", "world"},
		},
		{
			name:     "wrap at space",
			input:    "this is a test",
			maxWidth: 7,
			want:     []string{"this is", "a test"},
		},
		{
			name:     "force wrap no spaces",
			input:    "alongwordthatmustwrap",
			maxWidth: 10,
			want:     []string{"alongwordt", "hatmustwra", "p"},
		},
		{
			name:     "newlines respected",
			input:    "line one\nline two",
			maxWidth: 20,
			want:     []string{"line one", "line two"},
		},
		{
			name:     "empty input",
			input:    "",
			maxWidth: 10,
			want:     []string{""},
		},
		{
			name:     "UTF-8 safe wrap",
			input:    "hallo verden 🌍",
			maxWidth: 13,
			want:     []string{"hallo verden", "🌍"},
		},
		{
			name:     "UTF-8 multi-byte characters",
			input:    "日本語のテキスト",
			maxWidth: 5,
			want:     []string{"日本語のテ", "キスト"},
		},
		{
			name:     "UTF-8 multi-byte with spaces",
			input:    "こんにちは 世界",
			maxWidth: 5,
			want:     []string{"こんにちは", "世界"},
		},
		{
			name:     "trim leading spaces on new lines",
			input:    "word1 word2 word3",
			maxWidth: 5,
			want:     []string{"word1", "word2", "word3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wordWrap(tt.input, tt.maxWidth)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("wordWrap() = %q, want %q", got, tt.want)
			}
		})
	}
}
