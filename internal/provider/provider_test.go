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
		{"copilot is StreamJSON", Provider{Kind: Copilot}, StreamJSON},
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

func TestProvider_BuildArgs_Claude_WithModel(t *testing.T) {
	p := Provider{Kind: Claude, Model: "claude-opus-4-6"}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "claude-opus-4-6")
}

func TestProvider_BuildArgs_Claude_NoModelByDefault(t *testing.T) {
	p := Provider{Kind: Claude}
	args := p.BuildArgs(nil)
	assert.NotContains(t, args, "--model")
}

func TestProvider_BuildArgs_Claude_ClaudeFlagModelOverrides(t *testing.T) {
	// --model in claude_flags takes precedence over Provider.Model
	p := Provider{Kind: Claude, Model: "claude-opus-4-6"}
	args := p.BuildArgs([]string{"--model", "claude-sonnet-4-6"})
	idx := -1
	for i, a := range args {
		if a == "--model" {
			idx = i
			break
		}
	}
	assert.NotEqual(t, -1, idx, "--model flag should be present")
	if idx >= 0 && idx+1 < len(args) {
		assert.Equal(t, "claude-sonnet-4-6", args[idx+1])
	}
	// Should appear exactly once
	count := 0
	for _, a := range args {
		if a == "--model" {
			count++
		}
	}
	assert.Equal(t, 1, count, "--model should appear exactly once")
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

func TestProvider_BuildArgs_Gemini_WithModel(t *testing.T) {
	p := Provider{Kind: Gemini, Model: "gemini-2.5-pro"}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--model")
	modelIdx := -1
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			modelIdx = i + 1
			break
		}
	}
	assert.NotEqual(t, -1, modelIdx, "expected --model flag")
	assert.Equal(t, "gemini-2.5-pro", args[modelIdx])
}

func TestProvider_BuildArgs_Gemini_NoModelByDefault(t *testing.T) {
	p := Provider{Kind: Gemini}
	args := p.BuildArgs(nil)
	assert.NotContains(t, args, "--model")
}

func TestProvider_BuildArgs_Gemini_ClaudeFlagModelOverrides(t *testing.T) {
	// A --model in claude_flags overrides the Provider.Model field.
	p := Provider{Kind: Gemini, Model: "gemini-2.5-flash-lite"}
	args := p.BuildArgs([]string{"--model", "gemini-2.5-pro"})
	modelIdx := -1
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			modelIdx = i + 1
			break
		}
	}
	assert.NotEqual(t, -1, modelIdx)
	assert.Equal(t, "gemini-2.5-pro", args[modelIdx])
}

func TestProvider_BuildArgs_Copilot(t *testing.T) {
	p := Provider{Kind: Copilot}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--yolo")
	assert.Contains(t, args, "--output-format")
	assert.Contains(t, args, "json")
	assert.NotContains(t, args, "--silent")
	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "--no-auto-update")
	// Copilot omits -p so it reads from piped stdin.
	assert.NotContains(t, args, "-p")
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

func TestProvider_BuildArgs_Copilot_ProviderModelField(t *testing.T) {
	// Provider.Model is honored for Copilot (config-driven model selection).
	// Dashes in version are translated to dots for Copilot CLI compatibility.
	p := Provider{Kind: Copilot, Model: "claude-opus-4-6"}
	args := p.BuildArgs(nil)
	modelIdx := -1
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			modelIdx = i + 1
			break
		}
	}
	assert.NotEqual(t, -1, modelIdx, "expected --model flag")
	assert.Equal(t, "claude-opus-4.6", args[modelIdx])
}

func TestProvider_BuildArgs_Copilot_ClaudeFlagModelOverridesProviderModel(t *testing.T) {
	// --model in claude_flags takes precedence over Provider.Model for Copilot.
	p := Provider{Kind: Copilot, Model: "claude-opus-4-6"}
	args := p.BuildArgs([]string{"--model", "claude-sonnet-4.6"})
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

func TestProvider_Cmd_OpenAI(t *testing.T) {
	p := Provider{Kind: OpenAI}
	assert.Equal(t, "codex", p.Cmd())
}

func TestProvider_Cmd_OpenAI_CustomCommand(t *testing.T) {
	p := Provider{Kind: OpenAI, Command: "my-codex"}
	assert.Equal(t, "my-codex", p.Cmd())
}

func TestProvider_Format_OpenAI(t *testing.T) {
	p := Provider{Kind: OpenAI}
	assert.Equal(t, StreamJSON, p.Format())
}

func TestProvider_BuildArgs_OpenAI(t *testing.T) {
	p := Provider{Kind: OpenAI}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--full-auto")
	assert.Contains(t, args, "--output-format")
	assert.Contains(t, args, "stream-json")
	// Should not contain claude-specific flags
	assert.NotContains(t, args, "--dangerously-skip-permissions")
	assert.NotContains(t, args, "-p")
}

func TestProvider_BuildArgs_OpenAI_WithModel(t *testing.T) {
	p := Provider{Kind: OpenAI, Model: "gpt-5.1-codex"}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--model")
	modelIdx := -1
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			modelIdx = i + 1
			break
		}
	}
	assert.NotEqual(t, -1, modelIdx, "expected --model flag")
	assert.Equal(t, "gpt-5.1-codex", args[modelIdx])
}

func TestProvider_BuildArgs_OpenAI_NoModelByDefault(t *testing.T) {
	p := Provider{Kind: OpenAI}
	args := p.BuildArgs(nil)
	assert.NotContains(t, args, "--model")
}

func TestProvider_BuildArgs_OpenAI_ClaudeFlagModelOverrides(t *testing.T) {
	p := Provider{Kind: OpenAI, Model: "gpt-5.1-codex"}
	args := p.BuildArgs([]string{"--model", "o3"})
	modelIdx := -1
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			modelIdx = i + 1
			break
		}
	}
	assert.NotEqual(t, -1, modelIdx)
	assert.Equal(t, "o3", args[modelIdx])
}

func TestProvider_BuildArgs_OpenAI_MaxTurnsPassedThrough(t *testing.T) {
	p := Provider{Kind: OpenAI}
	args := p.BuildArgs([]string{"--max-turns", "10"})
	assert.Contains(t, args, "--max-turns")
	assert.Contains(t, args, "10")
}

func TestProvider_BuildArgs_OpenAI_DropsToolsFlag(t *testing.T) {
	p := Provider{Kind: OpenAI}
	args := p.BuildArgs([]string{"--tools", "bash"})
	assert.NotContains(t, args, "--tools")
	assert.NotContains(t, args, "bash")
}

func TestDefaults(t *testing.T) {
	providers := Defaults()
	assert.Len(t, providers, 2)
	assert.Equal(t, Claude, providers[0].Kind)
	assert.Equal(t, Gemini, providers[1].Kind)
}

func TestFromConfig(t *testing.T) {
	tests := []struct {
		name       string
		specs      []string
		wantKinds  []Kind
		wantCmds   []string
		wantModels []string
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
			name:       "kind/model format",
			specs:      []string{"gemini/gemini-2.5-pro"},
			wantKinds:  []Kind{Gemini},
			wantModels: []string{"gemini-2.5-pro"},
		},
		{
			name:       "kind:command/model format",
			specs:      []string{"gemini:mybin/gemini-2.5-flash"},
			wantKinds:  []Kind{Gemini},
			wantCmds:   []string{"mybin"},
			wantModels: []string{"gemini-2.5-flash"},
		},
		{
			name:       "multiple gemini models as fallback chain",
			specs:      []string{"claude", "gemini/gemini-2.5-pro", "gemini/gemini-3-flash-preview", "gemini/gemini-2.5-flash-lite"},
			wantKinds:  []Kind{Claude, Gemini, Gemini, Gemini},
			wantModels: []string{"", "gemini-2.5-pro", "gemini-3-flash-preview", "gemini-2.5-flash-lite"},
		},
		{
			name:      "multiple providers",
			specs:     []string{"claude", "gemini"},
			wantKinds: []Kind{Claude, Gemini},
		},
		{
			name:       "openai with model",
			specs:      []string{"openai/gpt-5.1-codex"},
			wantKinds:  []Kind{OpenAI},
			wantModels: []string{"gpt-5.1-codex"},
		},
		{
			name:      "full chain with openai",
			specs:     []string{"claude", "gemini", "openai/o3"},
			wantKinds: []Kind{Claude, Gemini, OpenAI},
		},
		{
			name:       "openai kind:command/model format",
			specs:      []string{"openai:codex/o3"},
			wantKinds:  []Kind{OpenAI},
			wantCmds:   []string{"codex"},
			wantModels: []string{"o3"},
		},
		{
			name:      "uppercase is normalized",
			specs:     []string{"CLAUDE"},
			wantKinds: []Kind{Claude},
		},
		{
			name:      "empty strings are skipped",
			specs:     []string{"claude", "", "gemini"},
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
			if tt.wantModels != nil {
				for i, model := range tt.wantModels {
					assert.Equal(t, model, got[i].Model)
				}
			}
		})
	}
}

func TestFromConfig_OllamaBackend(t *testing.T) {
	got := FromConfig([]string{"claude:ollama"})
	assert.Len(t, got, 1)
	assert.Equal(t, Claude, got[0].Kind)
	assert.Empty(t, got[0].Command, "ollama is a backend, not a command override")
	assert.Equal(t, "ollama", got[0].Backend)
	assert.Equal(t, "http://localhost:11434", got[0].Env["ANTHROPIC_BASE_URL"])
	assert.Equal(t, "ollama", got[0].Env["ANTHROPIC_AUTH_TOKEN"])
	assert.Equal(t, "1", got[0].Env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"])
}

func TestFromConfig_OllamaBackendWithModel(t *testing.T) {
	got := FromConfig([]string{"claude:ollama/qwen2.5-coder:32b"})
	assert.Len(t, got, 1)
	assert.Equal(t, Claude, got[0].Kind)
	assert.Equal(t, "ollama", got[0].Backend)
	assert.Equal(t, "qwen2.5-coder:32b", got[0].Model)
	assert.Equal(t, "http://localhost:11434", got[0].Env["ANTHROPIC_BASE_URL"])
}

func TestFromConfig_OllamaBackendCaseInsensitive(t *testing.T) {
	got := FromConfig([]string{"claude:OLLAMA/mymodel"})
	assert.Len(t, got, 1)
	assert.Equal(t, "ollama", got[0].Backend)
	assert.NotNil(t, got[0].Env)
}

func TestFromConfig_OllamaInFallbackChain(t *testing.T) {
	got := FromConfig([]string{"claude", "claude:ollama/qwen2.5-coder:32b", "gemini"})
	assert.Len(t, got, 3)
	// First: regular claude
	assert.Equal(t, Claude, got[0].Kind)
	assert.Empty(t, got[0].Backend)
	assert.Nil(t, got[0].Env)
	// Second: ollama-backed claude
	assert.Equal(t, Claude, got[1].Kind)
	assert.Equal(t, "ollama", got[1].Backend)
	assert.Equal(t, "qwen2.5-coder:32b", got[1].Model)
	// Third: gemini
	assert.Equal(t, Gemini, got[2].Kind)
}

func TestProvider_Label_WithBackend(t *testing.T) {
	p := Provider{Kind: Claude, Backend: "ollama", Model: "qwen2.5-coder:32b"}
	assert.Equal(t, "claude:ollama/qwen2.5-coder:32b", p.Label())
}

func TestProvider_Label_BackendNoModel(t *testing.T) {
	p := Provider{Kind: Claude, Backend: "ollama"}
	assert.Equal(t, "claude:ollama", p.Label())
}

func TestProvider_OllamaUsesClaudeCmd(t *testing.T) {
	p := Provider{Kind: Claude, Backend: "ollama"}
	assert.Equal(t, "claude", p.Cmd())
}

func TestProvider_OllamaBuildsClaudeArgs(t *testing.T) {
	p := Provider{Kind: Claude, Backend: "ollama", Model: "qwen2.5-coder:32b"}
	args := p.BuildArgs(nil)
	assert.Contains(t, args, "--dangerously-skip-permissions")
	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "qwen2.5-coder:32b")
}

func TestFromConfig_UnknownCommandNotBackend(t *testing.T) {
	// A non-backend name in the command position should still work as a command override
	got := FromConfig([]string{"gemini:mybin"})
	assert.Len(t, got, 1)
	assert.Equal(t, "mybin", got[0].Command)
	assert.Empty(t, got[0].Backend)
	assert.Nil(t, got[0].Env)
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
