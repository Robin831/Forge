package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProvider_Cmd(t *testing.T) {
	tests := []struct {
		name    string
		p       Provider
		wantCmd string
	}{
		{"claude default", Provider{Kind: Claude}, "claude"},
		{"gemini default", Provider{Kind: Gemini}, "gemini"},
		{"copilot default", Provider{Kind: Copilot}, "copilot"},
		{"claude custom command", Provider{Kind: Claude, Command: "claude2"}, "claude2"},
		{"gemini custom command", Provider{Kind: Gemini, Command: "gemini-cli"}, "gemini-cli"},
		{"unknown kind defaults to claude", Provider{Kind: "unknown"}, "claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantCmd, tt.p.Cmd())
		})
	}
}

func TestProvider_Format(t *testing.T) {
	tests := []struct {
		name       string
		p          Provider
		wantFormat OutputFormat
	}{
		{"claude is StreamJSON", Provider{Kind: Claude}, StreamJSON},
		{"gemini is StreamJSON", Provider{Kind: Gemini}, StreamJSON},
		{"copilot is PlainText", Provider{Kind: Copilot}, PlainText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantFormat, tt.p.Format())
		})
	}
}

func TestProvider_BuildArgs_Claude(t *testing.T) {
	p := Provider{Kind: Claude}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--dangerously-skip-permissions")
	assert.Contains(t, args, "--output-format")
	assert.Contains(t, args, "stream-json")
	assert.Contains(t, args, "-p")
	assert.Contains(t, args, "-")
}

func TestProvider_BuildArgs_Claude_ExtraFlags(t *testing.T) {
	p := Provider{Kind: Claude}
	args := p.BuildArgs([]string{"--max-turns", "50"})
	assert.Contains(t, args, "--max-turns")
	assert.Contains(t, args, "50")
}

func TestProvider_BuildArgs_Gemini(t *testing.T) {
	p := Provider{Kind: Gemini}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--yolo")
	assert.Contains(t, args, "-o")
	assert.Contains(t, args, "stream-json")
	// Gemini args should not contain claude-specific flags
	assert.NotContains(t, args, "--dangerously-skip-permissions")
}

func TestProvider_BuildArgs_Gemini_DropsMaxTurns(t *testing.T) {
	p := Provider{Kind: Gemini}
	// --max-turns has no Gemini equivalent and should be silently dropped
	args := p.BuildArgs([]string{"--max-turns", "50", "--tools", "bash"})
	assert.NotContains(t, args, "--max-turns")
	assert.NotContains(t, args, "50")
	assert.NotContains(t, args, "--tools")
}

func TestProvider_BuildArgs_Copilot(t *testing.T) {
	p := Provider{Kind: Copilot}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--yolo")
	assert.Contains(t, args, "--silent")
	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "--no-auto-update")
	assert.Contains(t, args, "-p")
	assert.Contains(t, args, "-")
}

func TestProvider_BuildArgs_Copilot_DefaultModel(t *testing.T) {
	p := Provider{Kind: Copilot}
	args := p.BuildArgs(nil)
	// Default model should be present
	modelIdx := -1
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			modelIdx = i + 1
			break
		}
	}
	assert.NotEqual(t, -1, modelIdx, "expected --model flag")
	assert.Equal(t, "claude-sonnet-4.6", args[modelIdx])
}

func TestProvider_BuildArgs_Copilot_CustomModel(t *testing.T) {
	p := Provider{Kind: Copilot}
	args := p.BuildArgs([]string{"--model", "claude-opus-4"})
	modelIdx := -1
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			modelIdx = i + 1
			break
		}
	}
	assert.NotEqual(t, -1, modelIdx)
	assert.Equal(t, "claude-opus-4", args[modelIdx])
}

func TestDefaults(t *testing.T) {
	providers := Defaults()
	assert.Len(t, providers, 2)
	assert.Equal(t, Claude, providers[0].Kind)
	assert.Equal(t, Gemini, providers[1].Kind)
}

func TestFromConfig(t *testing.T) {
	tests := []struct {
		name      string
		specs     []string
		wantKinds []Kind
		wantCmds  []string
	}{
		{
			name:      "empty returns defaults",
			specs:     nil,
			wantKinds: []Kind{Claude, Gemini},
		},
		{
			name:      "single provider",
			specs:     []string{"claude"},
			wantKinds: []Kind{Claude},
		},
		{
			name:      "kind:command format",
			specs:     []string{"gemini:gemini2"},
			wantKinds: []Kind{Gemini},
			wantCmds:  []string{"gemini2"},
		},
		{
			name:      "multiple providers",
			specs:     []string{"claude", "gemini"},
			wantKinds: []Kind{Claude, Gemini},
		},
		{
			name:      "uppercase is normalized",
			specs:     []string{"CLAUDE"},
			wantKinds: []Kind{Claude},
		},
		{
			name:  "empty strings are skipped",
			specs: []string{"claude", "", "gemini"},
			wantKinds: []Kind{Claude, Gemini},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromConfig(tt.specs)
			assert.Len(t, got, len(tt.wantKinds))
			for i, kind := range tt.wantKinds {
				assert.Equal(t, kind, got[i].Kind)
			}
			if tt.wantCmds != nil {
				for i, cmd := range tt.wantCmds {
					assert.Equal(t, cmd, got[i].Command)
				}
			}
		})
	}
}

func TestIsRateLimitError(t *testing.T) {
	tests := []struct {
		name          string
		exitCode      int
		stderr        string
		resultSubtype string
		want          bool
	}{
		{"exit code 2 is rate limit", 2, "", "", true},
		{"exit code 1 is not rate limit", 1, "", "", false},
		{"exit code 0 is not rate limit", 0, "", "", false},
		{"subtype rate_limit", 0, "", "error_rate_limit_exceeded", true},
		{"subtype overloaded", 0, "", "overloaded", true},
		{"stderr rate limit phrase", 0, "you have run out of requests", "", true},
		{"stderr too many requests", 0, "Too Many Requests", "", true},
		{"stderr quota exceeded", 0, "quota exceeded", "", true},
		{"stderr usage limit", 0, "Usage Limit reached", "", true},
		{"stderr plan limit", 0, "plan limit reached", "", true},
		{"stderr capacity", 0, "capacity", "", true},
		{"stderr overloaded", 0, "overloaded", "", true},
		{"stderr unrelated", 0, "some other error", "", false},
		{"empty all", 0, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRateLimitError(tt.exitCode, tt.stderr, tt.resultSubtype)
			assert.Equal(t, tt.want, got)
		})
	}
}
